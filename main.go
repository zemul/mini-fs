package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/go-fuse/v2/fuse/nodefs"
	"github.com/hanwen/go-fuse/v2/fuse/pathfs"
)

// ============ 数据结构 ============

type BlobRef struct {
	Blob   string `json:"blob"`
	Offset uint64 `json:"offset"`
	Len    uint64 `json:"len"`
}

type Entry struct {
	Ino   uint64    `json:"ino"`
	Mode  uint32    `json:"mode"`
	Size  uint64    `json:"size"`
	Mtime int64     `json:"mtime"`
	Ctime int64     `json:"ctime"`
	Blobs []BlobRef `json:"blobs"`
}

type Metadata struct {
	NextInode uint64           `json:"next_inode"`
	NextBlob  uint64           `json:"next_blob"`
	Entries   map[string]Entry `json:"entries"`
}

// ============ MiniFS ============

type MiniFS struct {
	pathfs.FileSystem
	mu           sync.Mutex
	storeDir     string
	blobsDir     string
	metadataPath string
	meta         Metadata
	staged       map[uint64][]byte
}

func NewMiniFS(storeDir string) (*MiniFS, error) {
	blobsDir := filepath.Join(storeDir, "blobs")
	metadataPath := filepath.Join(storeDir, "metadata.json")

	if err := os.MkdirAll(blobsDir, 0755); err != nil {
		return nil, err
	}

	fs := &MiniFS{
		FileSystem:   pathfs.NewDefaultFileSystem(),
		storeDir:     storeDir,
		blobsDir:     blobsDir,
		metadataPath: metadataPath,
		staged:       make(map[uint64][]byte),
	}

	if data, err := os.ReadFile(metadataPath); err == nil {
		if err := json.Unmarshal(data, &fs.meta); err != nil {
			return nil, fmt.Errorf("read metadata: %w", err)
		}
	} else {
		fs.meta = Metadata{
			NextInode: 2,
			NextBlob:  1,
			Entries:   make(map[string]Entry),
		}
		if err := fs.bootstrap(); err != nil {
			return nil, err
		}
		if err := fs.commitMetadata(); err != nil {
			return nil, err
		}
	}

	return fs, nil
}

func (fs *MiniFS) bootstrap() error {
	hello := []byte("Hello from a tiny FUSE filesystem.\n")
	blobID, err := fs.putBlob(hello)
	if err != nil {
		return err
	}

	now := time.Now().Unix()
	ino := fs.allocateInode()
	fs.meta.Entries["hello.txt"] = Entry{
		Ino:  ino,
		Mode: 0644,
		Size: uint64(len(hello)),
		Mtime: now,
		Ctime: now,
		Blobs: []BlobRef{{
			Blob: blobID, Offset: 0, Len: uint64(len(hello)),
		}},
	}

	ino = fs.allocateInode()
	fs.meta.Entries["notes.txt"] = Entry{
		Ino:   ino,
		Mode:  0644,
		Size:  0,
		Mtime: now,
		Ctime: now,
	}
	return nil
}

func (fs *MiniFS) allocateInode() uint64 {
	ino := fs.meta.NextInode
	fs.meta.NextInode++
	return ino
}

func (fs *MiniFS) putBlob(data []byte) (string, error) {
	id := fmt.Sprintf("blob-%012d", fs.meta.NextBlob)
	fs.meta.NextBlob++

	path := filepath.Join(fs.blobsDir, id)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return "", err
	}
	return id, os.Rename(tmp, path)
}

func (fs *MiniFS) readBlobs(entry *Entry) ([]byte, error) {
	data := make([]byte, entry.Size)
	for _, ref := range entry.Blobs {
		blob, err := os.ReadFile(filepath.Join(fs.blobsDir, ref.Blob))
		if err != nil {
			return nil, err
		}
		copy(data[ref.Offset:], blob[:ref.Len])
	}
	return data, nil
}

func (fs *MiniFS) commitInode(ino uint64) error {
	data, ok := fs.staged[ino]
	if !ok {
		return nil
	}
	delete(fs.staged, ino)

	var newBlobs []BlobRef
	if len(data) > 0 {
		blobID, err := fs.putBlob(data)
		if err != nil {
			return err
		}
		newBlobs = []BlobRef{{Blob: blobID, Offset: 0, Len: uint64(len(data))}}
	}

	for name, entry := range fs.meta.Entries {
		if entry.Ino == ino {
			// GC: 删旧 blob
			for _, old := range entry.Blobs {
				os.Remove(filepath.Join(fs.blobsDir, old.Blob))
			}
			entry.Size = uint64(len(data))
			entry.Blobs = newBlobs
			entry.Mtime = time.Now().Unix()
			fs.meta.Entries[name] = entry
			log.Printf("[mini-fs] COMMIT %s ino=%d size=%d", name, ino, entry.Size)
			break
		}
	}
	return fs.commitMetadata()
}

func (fs *MiniFS) commitMetadata() error {
	data, err := json.MarshalIndent(fs.meta, "", "  ")
	if err != nil {
		return err
	}
	tmp := fs.metadataPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	log.Printf("[mini-fs] COMMIT metadata entries=%d", len(fs.meta.Entries))
	return os.Rename(tmp, fs.metadataPath)
}

func (fs *MiniFS) findEntry(name string) *Entry {
	e, ok := fs.meta.Entries[name]
	if !ok {
		return nil
	}
	return &e
}

func (fs *MiniFS) findByIno(ino uint64) (string, *Entry) {
	for name, entry := range fs.meta.Entries {
		if entry.Ino == ino {
			return name, &entry
		}
	}
	return "", nil
}

// ============ FUSE 接口实现 ============

func (fs *MiniFS) GetAttr(name string, ctx *fuse.Context) (*fuse.Attr, fuse.Status) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	if name == "" { // 根目录
		return &fuse.Attr{Mode: fuse.S_IFDIR | 0755, Nlink: 2}, fuse.OK
	}

	entry := fs.findEntry(name)
	if entry == nil {
		return nil, fuse.ENOENT
	}

	size := entry.Size
	if staged, ok := fs.staged[entry.Ino]; ok {
		size = uint64(len(staged))
	}

	mtime := uint64(entry.Mtime)
	ctime := uint64(entry.Ctime)
	return &fuse.Attr{
		Mode:      fuse.S_IFREG | entry.Mode,
		Size:      size,
		Nlink:     1,
		Mtime:     mtime,
		Ctime:     ctime,
		Atime:     mtime,
	}, fuse.OK
}

func (fs *MiniFS) OpenDir(name string, ctx *fuse.Context) ([]fuse.DirEntry, fuse.Status) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	if name != "" {
		return nil, fuse.ENOENT
	}

	entries := []fuse.DirEntry{}
	for n, e := range fs.meta.Entries {
		entries = append(entries, fuse.DirEntry{Name: n, Mode: fuse.S_IFREG | e.Mode, Ino: e.Ino})
	}
	log.Printf("[mini-fs] READDIR entries=%d", len(entries))
	return entries, fuse.OK
}

func (fs *MiniFS) Open(name string, flags uint32, ctx *fuse.Context) (nodefs.File, fuse.Status) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	entry := fs.findEntry(name)
	if entry == nil {
		return nil, fuse.ENOENT
	}
	log.Printf("[mini-fs] OPEN %s ino=%d flags=0x%x", name, entry.Ino, flags)
	return &miniFile{File: nodefs.NewDefaultFile(), fs: fs, ino: entry.Ino, name: name}, fuse.OK
}

func (fs *MiniFS) Create(name string, flags uint32, mode uint32, ctx *fuse.Context) (nodefs.File, fuse.Status) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	if fs.findEntry(name) != nil {
		return nil, fuse.Status(syscall.EEXIST)
	}

	now := time.Now().Unix()
	ino := fs.allocateInode()
	fs.meta.Entries[name] = Entry{Ino: ino, Mode: mode & 0o777, Size: 0, Mtime: now, Ctime: now}
	log.Printf("[mini-fs] CREATE %s ino=%d", name, ino)

	if err := fs.commitMetadata(); err != nil {
		return nil, fuse.EIO
	}
	return &miniFile{File: nodefs.NewDefaultFile(), fs: fs, ino: ino, name: name}, fuse.OK
}

func (fs *MiniFS) Rename(oldName, newName string, ctx *fuse.Context) fuse.Status {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	entry, ok := fs.meta.Entries[oldName]
	if !ok {
		return fuse.ENOENT
	}
	delete(fs.meta.Entries, oldName)
	fs.meta.Entries[newName] = entry
	log.Printf("[mini-fs] RENAME %s -> %s", oldName, newName)

	if err := fs.commitMetadata(); err != nil {
		return fuse.EIO
	}
	return fuse.OK
}

func (fs *MiniFS) Unlink(name string, ctx *fuse.Context) fuse.Status {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	entry, ok := fs.meta.Entries[name]
	if !ok {
		return fuse.ENOENT
	}
	// GC: 删除文件关联的 blob
	for _, ref := range entry.Blobs {
		os.Remove(filepath.Join(fs.blobsDir, ref.Blob))
	}
	delete(fs.meta.Entries, name)
	delete(fs.staged, entry.Ino)
	log.Printf("[mini-fs] UNLINK %s ino=%d", name, entry.Ino)

	if err := fs.commitMetadata(); err != nil {
		return fuse.EIO
	}
	return fuse.OK
}

func (fs *MiniFS) Chmod(name string, mode uint32, ctx *fuse.Context) fuse.Status {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	entry, ok := fs.meta.Entries[name]
	if !ok {
		return fuse.ENOENT
	}
	entry.Mode = mode & 0o777
	entry.Ctime = time.Now().Unix()
	fs.meta.Entries[name] = entry
	log.Printf("[mini-fs] CHMOD %s mode=%o", name, entry.Mode)

	if err := fs.commitMetadata(); err != nil {
		return fuse.EIO
	}
	return fuse.OK
}

func (fs *MiniFS) Truncate(name string, size uint64, ctx *fuse.Context) fuse.Status {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	entry := fs.findEntry(name)
	if entry == nil {
		return fuse.ENOENT
	}

	buf, ok := fs.staged[entry.Ino]
	if !ok {
		var err error
		buf, err = fs.readBlobs(entry)
		if err != nil {
			return fuse.EIO
		}
	}

	if uint64(len(buf)) < size {
		buf = append(buf, make([]byte, size-uint64(len(buf)))...)
	} else {
		buf = buf[:size]
	}
	fs.staged[entry.Ino] = buf
	return fuse.OK
}

// ============ File 实现（处理 Read/Write/Flush） ============

type miniFile struct {
	nodefs.File
	fs   *MiniFS
	ino  uint64
	name string
}

func (f *miniFile) Read(buf []byte, off int64) (fuse.ReadResult, fuse.Status) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()

	log.Printf("[mini-fs] READ %s ino=%d offset=%d size=%d", f.name, f.ino, off, len(buf))

	var data []byte
	if staged, ok := f.fs.staged[f.ino]; ok {
		data = staged
	} else {
		_, entry := f.fs.findByIno(f.ino)
		if entry == nil {
			return nil, fuse.ENOENT
		}
		var err error
		data, err = f.fs.readBlobs(entry)
		if err != nil {
			return nil, fuse.EIO
		}
	}

	if int(off) >= len(data) {
		return fuse.ReadResultData(nil), fuse.OK
	}
	return fuse.ReadResultData(data[off:]), fuse.OK
}

func (f *miniFile) Write(data []byte, off int64) (uint32, fuse.Status) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()

	log.Printf("[mini-fs] WRITE %s ino=%d offset=%d len=%d", f.name, f.ino, off, len(data))

	buf, ok := f.fs.staged[f.ino]
	if !ok {
		_, entry := f.fs.findByIno(f.ino)
		if entry == nil {
			return 0, fuse.ENOENT
		}
		var err error
		buf, err = f.fs.readBlobs(entry)
		if err != nil {
			return 0, fuse.EIO
		}
	}

	end := int(off) + len(data)
	if len(buf) < end {
		buf = append(buf, make([]byte, end-len(buf))...)
	}
	copy(buf[off:], data)
	f.fs.staged[f.ino] = buf
	return uint32(len(data)), fuse.OK
}

func (f *miniFile) Flush() fuse.Status {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()

	log.Printf("[mini-fs] FLUSH %s ino=%d", f.name, f.ino)
	if err := f.fs.commitInode(f.ino); err != nil {
		return fuse.EIO
	}
	return fuse.OK
}

func (f *miniFile) Fsync(flags int) fuse.Status {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()

	log.Printf("[mini-fs] FSYNC %s ino=%d", f.name, f.ino)
	if err := f.fs.commitInode(f.ino); err != nil {
		return fuse.EIO
	}
	return fuse.OK
}

func (f *miniFile) GetAttr(out *fuse.Attr) fuse.Status {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()

	_, entry := f.fs.findByIno(f.ino)
	if entry == nil {
		return fuse.ENOENT
	}
	size := entry.Size
	if staged, ok := f.fs.staged[f.ino]; ok {
		size = uint64(len(staged))
	}
	out.Mode = fuse.S_IFREG | entry.Mode
	out.Size = size
	out.Nlink = 1
	out.Mtime = uint64(entry.Mtime)
	out.Ctime = uint64(entry.Ctime)
	out.Atime = uint64(entry.Mtime)
	return fuse.OK
}

func (f *miniFile) Truncate(size uint64) fuse.Status {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()

	buf, ok := f.fs.staged[f.ino]
	if !ok {
		_, entry := f.fs.findByIno(f.ino)
		if entry == nil {
			return fuse.ENOENT
		}
		var err error
		buf, err = f.fs.readBlobs(entry)
		if err != nil {
			return fuse.EIO
		}
	}
	if uint64(len(buf)) < size {
		buf = append(buf, make([]byte, size-uint64(len(buf)))...)
	} else {
		buf = buf[:size]
	}
	f.fs.staged[f.ino] = buf
	return fuse.OK
}

// ============ main ============

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: mini-fs <mountpoint> [store-dir]\n")
		os.Exit(1)
	}

	mountpoint := os.Args[1]
	storeDir := "/tmp/minifs-store"
	if len(os.Args) > 2 {
		storeDir = os.Args[2]
	}

	miniFS, err := NewMiniFS(storeDir)
	if err != nil {
		log.Fatalf("init mini-fs: %v", err)
	}

	nfs := pathfs.NewPathNodeFs(miniFS, &pathfs.PathNodeFsOptions{ClientInodes: true})
	opts := &nodefs.Options{
		AttrTimeout:  time.Second,
		EntryTimeout: time.Second,
	}

	server, _, err := nodefs.MountRoot(mountpoint, nfs.Root(), opts)
	if err != nil {
		log.Fatalf("mount: %v", err)
	}

	log.Printf("[mini-fs] mounted at %s, backing store at %s", mountpoint, storeDir)
	server.Serve()
}

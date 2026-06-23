package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/go-fuse/v2/fuse/nodefs"
	"github.com/hanwen/go-fuse/v2/fuse/pathfs"
)

// ============ 数据结构 ============

// BlobRef 描述文件内容在 blobs/ 目录中的一个片段。
// 一个文件可以由多个 BlobRef 组成（当前实现每次写入只生成一个）。
type BlobRef struct {
	Blob   string `json:"blob"`   // blob 文件名，如 "blob-000000000001"
	Offset uint64 `json:"offset"` // 该片段在文件内容中的起始偏移
	Len    uint64 `json:"len"`    // 片段长度（字节）
}

// Entry 表示文件系统中的一个节点（文件或目录）。
// 目录通过 Children 引用子节点 inode，形成树结构。
// 文件通过 Blobs 引用磁盘上的内容片段。
type Entry struct {
	Ino      uint64            `json:"ino"`              // 唯一 inode 编号
	IsDir    bool              `json:"is_dir"`           // true=目录, false=文件
	Mode     uint32            `json:"mode"`             // 权限位，如 0755、0644
	Size     uint64            `json:"size"`             // 文件大小（目录忽略）
	Mtime    int64             `json:"mtime"`            // 最后修改时间（Unix 秒）
	Ctime    int64             `json:"ctime"`            // 最后状态变更时间
	Blobs    []BlobRef         `json:"blobs,omitempty"`  // 文件内容引用（文件专用）
	Children map[string]uint64 `json:"children,omitempty"` // 子条目名 → inode（目录专用）
}

// Metadata 是整个文件系统的持久化状态，序列化为 metadata.json。
// Entries 按 inode 号索引所有节点，RootIno 指向根目录。
type Metadata struct {
	NextInode uint64            `json:"next_inode"` // 下一个可分配的 inode 号
	NextBlob  uint64            `json:"next_blob"`  // 下一个 blob 文件的序号
	RootIno   uint64            `json:"root_ino"`   // 根目录的 inode 号（通常为 1）
	Entries   map[uint64]*Entry `json:"entries"`    // inode → Entry 全局索引
}

// ============ MiniFS 核心结构 ============

// MiniFS 实现 pathfs.FileSystem 接口，是整个文件系统的核心。
// 它持有元数据（meta）和写缓冲（staged），所有操作通过 mu 互斥锁串行化。
type MiniFS struct {
	pathfs.FileSystem                // 嵌入默认实现，未覆盖的方法返回 ENOSYS
	mu           sync.Mutex         // 保护所有共享状态
	storeDir     string             // 持久化根目录，如 /tmp/minifs-store
	blobsDir     string             // blob 文件目录
	metadataPath string             // metadata.json 路径
	meta         Metadata           // 内存中的元数据（inode 树）
	staged       map[uint64][]byte  // 写缓冲：ino → 尚未持久化的文件内容
}

// NewMiniFS 初始化文件系统。如果 metadata.json 存在则加载，否则创建空的根目录。
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
			RootIno:   1,
			Entries:   make(map[uint64]*Entry),
		}
		// 创建根目录 entry
		fs.meta.Entries[1] = &Entry{
			Ino:      1,
			IsDir:    true,
			Mode:     0755,
			Mtime:    time.Now().Unix(),
			Ctime:    time.Now().Unix(),
			Children: make(map[string]uint64),
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

// bootstrap 首次运行时创建演示文件（hello.txt 和 notes.txt）
func (fs *MiniFS) bootstrap() error {
	hello := []byte("Hello from a tiny FUSE filesystem.\n")
	blobID, err := fs.putBlob(hello)
	if err != nil {
		return err
	}

	now := time.Now().Unix()
	ino := fs.allocateInode()
	fs.meta.Entries[ino] = &Entry{
		Ino:   ino,
		Mode:  0644,
		Size:  uint64(len(hello)),
		Mtime: now,
		Ctime: now,
		Blobs: []BlobRef{{Blob: blobID, Offset: 0, Len: uint64(len(hello))}},
	}
	fs.meta.Entries[fs.meta.RootIno].Children["hello.txt"] = ino

	ino2 := fs.allocateInode()
	fs.meta.Entries[ino2] = &Entry{
		Ino:   ino2,
		Mode:  0644,
		Mtime: now,
		Ctime: now,
	}
	fs.meta.Entries[fs.meta.RootIno].Children["notes.txt"] = ino2
	return nil
}

// allocateInode 分配一个新的 inode 号（单调递增，永不复用）
func (fs *MiniFS) allocateInode() uint64 {
	ino := fs.meta.NextInode
	fs.meta.NextInode++
	return ino
}

// ============ 路径解析 ============
// FUSE 内核按 "/" 分割路径，逐级调用 Lookup。pathfs 帮我们拼好完整路径再传入。
// 例如 "docs/readme.txt" 会先找 root→"docs"→ino=3，再找 entries[3]→"readme.txt"→ino=4。

// resolve 将路径字符串解析为对应的 Entry。
// 空路径 "" 表示根目录。逐级查找 Children 直到找到目标或失败返回 nil。
func (fs *MiniFS) resolve(path string) *Entry {
	if path == "" {
		return fs.meta.Entries[fs.meta.RootIno]
	}
	parts := strings.Split(path, "/")
	cur := fs.meta.Entries[fs.meta.RootIno]
	for _, p := range parts {
		if cur == nil || !cur.IsDir {
			return nil
		}
		childIno, ok := cur.Children[p]
		if !ok {
			return nil
		}
		cur = fs.meta.Entries[childIno]
	}
	return cur
}

// resolveParent 解析路径的父目录和最后一段文件名。
// 例如 "docs/readme.txt" → (entries[docs的ino], "readme.txt")
// 用于 Create/Unlink/Mkdir/Rmdir 等需要修改父目录 Children 的操作。
func (fs *MiniFS) resolveParent(path string) (*Entry, string) {
	if path == "" {
		return nil, ""
	}
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return fs.meta.Entries[fs.meta.RootIno], path
	}
	parent := fs.resolve(path[:idx])
	return parent, path[idx+1:]
}

// ============ Blob 操作 ============
// Blob 是不可变的文件内容块。每次写入生成新 blob，旧 blob 被 GC 删除。
// 写入使用 write-tmp-then-rename 保证原子性（崩溃不会出现半写文件）。

// putBlob 将数据写入新 blob 文件，返回 blob ID（如 "blob-000000000001"）
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

// readBlobs 从磁盘读取文件的所有 blob 片段，拼成完整内容
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

// commitInode 将 staged 中的写缓冲持久化为新 blob，并删除旧 blob（GC）。
// 在 Flush/Fsync 时调用，对应用户关闭文件或显式 sync 的时刻。
func (fs *MiniFS) commitInode(ino uint64) error {
	data, ok := fs.staged[ino]
	if !ok {
		return nil
	}
	delete(fs.staged, ino)

	entry := fs.meta.Entries[ino]
	if entry == nil {
		return nil
	}

	var newBlobs []BlobRef
	if len(data) > 0 {
		blobID, err := fs.putBlob(data)
		if err != nil {
			return err
		}
		newBlobs = []BlobRef{{Blob: blobID, Offset: 0, Len: uint64(len(data))}}
	}

	// GC: 删旧 blob
	for _, old := range entry.Blobs {
		os.Remove(filepath.Join(fs.blobsDir, old.Blob))
	}
	entry.Size = uint64(len(data))
	entry.Blobs = newBlobs
	entry.Mtime = time.Now().Unix()

	log.Printf("[mini-fs] COMMIT ino=%d size=%d", ino, entry.Size)
	return fs.commitMetadata()
}

// commitMetadata 原子写入 metadata.json（先写 .tmp 再 rename）
func (fs *MiniFS) commitMetadata() error {
	data, err := json.MarshalIndent(fs.meta, "", "  ")
	if err != nil {
		return err
	}
	tmp := fs.metadataPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	log.Printf("[mini-fs] COMMIT metadata inodes=%d", len(fs.meta.Entries))
	return os.Rename(tmp, fs.metadataPath)
}

// ============ FUSE 接口实现 ============
// 以下方法由 go-fuse 在收到内核请求时调用。每个方法对应一个 FUSE 操作。
// pathfs 接口以路径字符串（如 "docs/readme.txt"）为参数，我们用 resolve() 转为 Entry。

// GetAttr 对应 stat() 系统调用，返回文件/目录的属性（大小、权限、时间等）
func (fs *MiniFS) GetAttr(name string, ctx *fuse.Context) (*fuse.Attr, fuse.Status) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	entry := fs.resolve(name)
	if entry == nil {
		return nil, fuse.ENOENT
	}

	if entry.IsDir {
		return &fuse.Attr{
			Ino:   entry.Ino,
			Mode:  fuse.S_IFDIR | entry.Mode,
			Nlink: 2 + uint32(len(entry.Children)),
			Mtime: uint64(entry.Mtime),
			Ctime: uint64(entry.Ctime),
			Atime: uint64(entry.Mtime),
		}, fuse.OK
	}

	size := entry.Size
	if staged, ok := fs.staged[entry.Ino]; ok {
		size = uint64(len(staged))
	}
	return &fuse.Attr{
		Ino:   entry.Ino,
		Mode:  fuse.S_IFREG | entry.Mode,
		Size:  size,
		Nlink: 1,
		Mtime: uint64(entry.Mtime),
		Ctime: uint64(entry.Ctime),
		Atime: uint64(entry.Mtime),
	}, fuse.OK
}

// OpenDir 对应 readdir()，返回目录下的所有直接子条目
func (fs *MiniFS) OpenDir(name string, ctx *fuse.Context) ([]fuse.DirEntry, fuse.Status) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	dir := fs.resolve(name)
	if dir == nil || !dir.IsDir {
		return nil, fuse.ENOENT
	}

	entries := make([]fuse.DirEntry, 0, len(dir.Children))
	for childName, childIno := range dir.Children {
		child := fs.meta.Entries[childIno]
		if child == nil {
			continue
		}
		mode := fuse.S_IFREG | child.Mode
		if child.IsDir {
			mode = fuse.S_IFDIR | child.Mode
		}
		entries = append(entries, fuse.DirEntry{Name: childName, Mode: mode, Ino: childIno})
	}
	log.Printf("[mini-fs] READDIR %q entries=%d", name, len(entries))
	return entries, fuse.OK
}

// Open 打开一个已有文件，返回 miniFile 文件句柄供后续 Read/Write
func (fs *MiniFS) Open(name string, flags uint32, ctx *fuse.Context) (nodefs.File, fuse.Status) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	entry := fs.resolve(name)
	if entry == nil {
		return nil, fuse.ENOENT
	}
	if entry.IsDir {
		return nil, fuse.Status(syscall.EISDIR)
	}
	log.Printf("[mini-fs] OPEN %s ino=%d flags=0x%x", name, entry.Ino, flags)
	return &miniFile{File: nodefs.NewDefaultFile(), fs: fs, ino: entry.Ino, name: name}, fuse.OK
}

// Create 创建新文件：分配 inode、加入父目录 Children、持久化 metadata
func (fs *MiniFS) Create(name string, flags uint32, mode uint32, ctx *fuse.Context) (nodefs.File, fuse.Status) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	parent, baseName := fs.resolveParent(name)
	if parent == nil || !parent.IsDir {
		return nil, fuse.ENOENT
	}
	if _, exists := parent.Children[baseName]; exists {
		return nil, fuse.Status(syscall.EEXIST)
	}

	now := time.Now().Unix()
	ino := fs.allocateInode()
	fs.meta.Entries[ino] = &Entry{Ino: ino, Mode: mode & 0o777, Mtime: now, Ctime: now}
	parent.Children[baseName] = ino
	log.Printf("[mini-fs] CREATE %s ino=%d", name, ino)

	if err := fs.commitMetadata(); err != nil {
		return nil, fuse.EIO
	}
	return &miniFile{File: nodefs.NewDefaultFile(), fs: fs, ino: ino, name: name}, fuse.OK
}

// Mkdir 创建子目录：分配 inode、设置 IsDir=true、初始化空 Children
func (fs *MiniFS) Mkdir(name string, mode uint32, ctx *fuse.Context) fuse.Status {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	parent, baseName := fs.resolveParent(name)
	if parent == nil || !parent.IsDir {
		return fuse.ENOENT
	}
	if _, exists := parent.Children[baseName]; exists {
		return fuse.Status(syscall.EEXIST)
	}

	now := time.Now().Unix()
	ino := fs.allocateInode()
	fs.meta.Entries[ino] = &Entry{
		Ino:      ino,
		IsDir:    true,
		Mode:     mode & 0o777,
		Mtime:    now,
		Ctime:    now,
		Children: make(map[string]uint64),
	}
	parent.Children[baseName] = ino
	log.Printf("[mini-fs] MKDIR %s ino=%d", name, ino)

	if err := fs.commitMetadata(); err != nil {
		return fuse.EIO
	}
	return fuse.OK
}

// Rmdir 删除空目录。非空目录返回 ENOTEMPTY（POSIX 要求）
func (fs *MiniFS) Rmdir(name string, ctx *fuse.Context) fuse.Status {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	parent, baseName := fs.resolveParent(name)
	if parent == nil || !parent.IsDir {
		return fuse.ENOENT
	}
	childIno, ok := parent.Children[baseName]
	if !ok {
		return fuse.ENOENT
	}
	child := fs.meta.Entries[childIno]
	if child == nil || !child.IsDir {
		return fuse.Status(syscall.ENOTDIR)
	}
	if len(child.Children) > 0 {
		return fuse.Status(syscall.ENOTEMPTY)
	}

	delete(parent.Children, baseName)
	delete(fs.meta.Entries, childIno)
	log.Printf("[mini-fs] RMDIR %s ino=%d", name, childIno)

	if err := fs.commitMetadata(); err != nil {
		return fuse.EIO
	}
	return fuse.OK
}

// Rename 移动/重命名文件或目录。支持跨目录移动。
// POSIX 语义：如果目标已存在且是文件，会被覆盖。
func (fs *MiniFS) Rename(oldName, newName string, ctx *fuse.Context) fuse.Status {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	oldParent, oldBase := fs.resolveParent(oldName)
	if oldParent == nil {
		return fuse.ENOENT
	}
	childIno, ok := oldParent.Children[oldBase]
	if !ok {
		return fuse.ENOENT
	}

	newParent, newBase := fs.resolveParent(newName)
	if newParent == nil || !newParent.IsDir {
		return fuse.ENOENT
	}

	// 如果目标已存在，覆盖（POSIX 语义）
	if existIno, ok := newParent.Children[newBase]; ok {
		existing := fs.meta.Entries[existIno]
		if existing != nil && !existing.IsDir {
			for _, ref := range existing.Blobs {
				os.Remove(filepath.Join(fs.blobsDir, ref.Blob))
			}
			delete(fs.meta.Entries, existIno)
			delete(fs.staged, existIno)
		}
	}

	delete(oldParent.Children, oldBase)
	newParent.Children[newBase] = childIno
	log.Printf("[mini-fs] RENAME %s -> %s", oldName, newName)

	if err := fs.commitMetadata(); err != nil {
		return fuse.EIO
	}
	return fuse.OK
}

// Unlink 删除文件（不能用于目录，目录用 Rmdir）。同步删除关联 blob（GC）。
func (fs *MiniFS) Unlink(name string, ctx *fuse.Context) fuse.Status {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	parent, baseName := fs.resolveParent(name)
	if parent == nil || !parent.IsDir {
		return fuse.ENOENT
	}
	childIno, ok := parent.Children[baseName]
	if !ok {
		return fuse.ENOENT
	}
	entry := fs.meta.Entries[childIno]
	if entry == nil {
		return fuse.ENOENT
	}
	if entry.IsDir {
		return fuse.Status(syscall.EISDIR)
	}

	// GC: 删除文件关联的 blob
	for _, ref := range entry.Blobs {
		os.Remove(filepath.Join(fs.blobsDir, ref.Blob))
	}
	delete(parent.Children, baseName)
	delete(fs.meta.Entries, childIno)
	delete(fs.staged, childIno)
	log.Printf("[mini-fs] UNLINK %s ino=%d", name, childIno)

	if err := fs.commitMetadata(); err != nil {
		return fuse.EIO
	}
	return fuse.OK
}

// Chmod 修改文件/目录权限位
func (fs *MiniFS) Chmod(name string, mode uint32, ctx *fuse.Context) fuse.Status {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	entry := fs.resolve(name)
	if entry == nil {
		return fuse.ENOENT
	}
	entry.Mode = mode & 0o777
	entry.Ctime = time.Now().Unix()
	log.Printf("[mini-fs] CHMOD %s mode=%o", name, entry.Mode)

	if err := fs.commitMetadata(); err != nil {
		return fuse.EIO
	}
	return fuse.OK
}

// Truncate 截断文件到指定大小（shell 重定向 > 会先调这个清空文件）
func (fs *MiniFS) Truncate(name string, size uint64, ctx *fuse.Context) fuse.Status {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	entry := fs.resolve(name)
	if entry == nil || entry.IsDir {
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

// ============ miniFile 文件句柄 ============
// miniFile 代表一个打开的文件描述符。内核对同一文件可能打开多个 fd，
// 每个对应一个 miniFile 实例。Read/Write 操作内存中的 staged 缓冲，
// Flush 时才真正写入磁盘。

type miniFile struct {
	nodefs.File          // 嵌入默认实现
	fs   *MiniFS        // 指回文件系统，访问 staged 和 meta
	ino  uint64         // 此文件的 inode 号
	name string         // 路径名（用于日志）
}

// Read 从 staged 缓冲或磁盘 blob 中读取文件内容。
// 优先读 staged（有未持久化的写入），否则从 blob 文件加载。
func (f *miniFile) Read(buf []byte, off int64) (fuse.ReadResult, fuse.Status) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()

	log.Printf("[mini-fs] READ %s ino=%d offset=%d size=%d", f.name, f.ino, off, len(buf))

	var data []byte
	if staged, ok := f.fs.staged[f.ino]; ok {
		data = staged
	} else {
		entry := f.fs.meta.Entries[f.ino]
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

// Write 将数据写入内存 staged 缓冲（不落盘！）。
// 真正持久化在 Flush 时发生。这就是 "write ≠ sync" 的核心体现。
func (f *miniFile) Write(data []byte, off int64) (uint32, fuse.Status) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()

	log.Printf("[mini-fs] WRITE %s ino=%d offset=%d len=%d", f.name, f.ino, off, len(data))

	buf, ok := f.fs.staged[f.ino]
	if !ok {
		entry := f.fs.meta.Entries[f.ino]
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

// Flush 在文件描述符关闭时由内核调用。此时将 staged 数据持久化为 blob。
func (f *miniFile) Flush() fuse.Status {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()

	log.Printf("[mini-fs] FLUSH %s ino=%d", f.name, f.ino)
	if err := f.fs.commitInode(f.ino); err != nil {
		return fuse.EIO
	}
	return fuse.OK
}

// Fsync 在应用程序显式调用 fsync() 时触发，强制持久化
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

	entry := f.fs.meta.Entries[f.ino]
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
		entry := f.fs.meta.Entries[f.ino]
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

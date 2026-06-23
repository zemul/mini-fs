# mini-fs

一个用 Go 实现的迷你 FUSE 文件系统，用于学习 FUSE 工作原理。灵感来自 [shayonj/magicfs](https://github.com/shayonj/magicfs) 和 [Building a Tiny FUSE Filesystem](https://www.shayon.dev/post/2026/161/building-a-tiny-fuse-filesystem/)。

## 核心思想

文件系统本质上是一个**请求-响应循环**：用户程序调 `open`/`read`/`write` 等系统调用，内核把请求转发给 FUSE 用户态进程，我们的代码回答这些请求。

```
┌─────────────────────────────────────────────────────────┐
│  用户程序 (cat, echo, ls, rm, mv, mkdir, rmdir)          │
└────────────────────────┬────────────────────────────────┘
                         │ 系统调用
┌────────────────────────▼────────────────────────────────┐
│  Linux/macOS 内核 VFS                                    │
│  → 路径解析 → 找到 FUSE 挂载点                            │
└────────────────────────┬────────────────────────────────┘
                         │ /dev/fuse 协议
┌────────────────────────▼────────────────────────────────┐
│  go-fuse 库 (解析 FUSE 协议，分发到回调方法)               │
└────────────────────────┬────────────────────────────────┘
                         │ 方法调用
┌────────────────────────▼────────────────────────────────┐
│  mini-fs (我们的代码)                                     │
│  ┌──────────────┐  ┌──────────────────────────────┐     │
│  │ staged (内存) │  │ metadata.json + blobs/ (磁盘) │     │
│  └──────────────┘  └──────────────────────────────┘     │
└─────────────────────────────────────────────────────────┘
```

## 存储结构

```
/tmp/minifs-store/
├── metadata.json          # inode 树形索引（目录 + 文件）
└── blobs/
    └── blob-000000000001  # 文件内容（不可变，每次写入生成新 blob）
```

`metadata.json` 示例：

```json
{
  "next_inode": 5,
  "next_blob": 2,
  "root_ino": 1,
  "entries": {
    "1": {
      "ino": 1,
      "is_dir": true,
      "mode": 493,
      "mtime": 1719136000,
      "ctime": 1719136000,
      "children": {
        "hello.txt": 2,
        "docs": 3
      }
    },
    "2": {
      "ino": 2,
      "mode": 420,
      "size": 35,
      "mtime": 1719136000,
      "ctime": 1719136000,
      "blobs": [{"blob": "blob-000000000001", "offset": 0, "len": 35}]
    },
    "3": {
      "ino": 3,
      "is_dir": true,
      "mode": 493,
      "mtime": 1719136000,
      "ctime": 1719136000,
      "children": {
        "readme.txt": 4
      }
    },
    "4": {
      "ino": 4,
      "mode": 420,
      "size": 7,
      "mtime": 1719136000,
      "ctime": 1719136000,
      "blobs": [{"blob": "blob-000000000002", "offset": 0, "len": 7}]
    }
  }
}
```

## 调用流程

### 读文件：`cat /tmp/minifs/docs/readme.txt`

```
cat docs/readme.txt
    │
    ▼
LOOKUP "docs"       ──→  root.Children["docs"] → ino=3
    │
    ▼
LOOKUP "readme.txt" ──→  entries[3].Children["readme.txt"] → ino=4
    │
    ▼
OPEN ino=4          ──→  创建 miniFile{ino=4} 文件句柄
    │
    ▼
READ ino=4 off=0    ──→  读 staged[4] 或 readBlobs() → 返回字节
    │
    ▼
RELEASE ino=4       ──→  关闭文件句柄
```

### 写文件：`echo "hello" > /tmp/minifs/foo.txt`

```
echo "hello" > foo.txt
    │
    ▼
CREATE "foo.txt"    ──→  分配 ino=5, 加入 root.Children, 写 metadata
    │
    ▼
OPEN ino=5          ──→  创建 miniFile{ino=5}
    │
    ▼
SETATTR size=0      ──→  截断文件（staged[5] = []byte{}）
    │
    ▼
WRITE ino=5 "hello" ──→  staged[5] = []byte("hello")  ← 纯内存！
    │
    ▼
FLUSH ino=5         ──→  putBlob("hello") 写新 blob
                         删旧 blob（GC）
                         更新 metadata.json
    │
    ▼
RELEASE ino=5       ──→  关闭
```

### 创建目录：`mkdir /tmp/minifs/docs`

```
MKDIR "docs"
    │
    ▼
分配 ino=3, IsDir=true, Children={}
加入 root.Children["docs"] = 3
    │
    ▼
commitMetadata()  ──→  原子更新 metadata.json
```

### 删除目录：`rmdir /tmp/minifs/docs`

```
RMDIR "docs"
    │
    ▼
检查 entries[3].Children 是否为空（非空返回 ENOTEMPTY）
    │
    ▼
从 root.Children 删除 "docs"
从 entries 删除 ino=3
    │
    ▼
commitMetadata()  ──→  原子更新 metadata.json
```

### 重命名：`mv docs/a.txt b.txt`

```
RENAME "docs/a.txt" → "b.txt"
    │
    ▼
找到源父目录 entries[3]，从 Children 删 "a.txt"
找到目标父目录 root，加入 Children["b.txt"]（ino 不变）
    │
    ▼
commitMetadata()  ──→  原子更新 metadata.json
```

### 删除文件：`rm bar.txt`

```
UNLINK "bar.txt"
    │
    ▼
删除关联的 blob 文件（GC）
从父目录 Children 删除 "bar.txt"
从 entries 删除对应 ino
    │
    ▼
commitMetadata()  ──→  原子更新 metadata.json
```

## 关键设计

### 树形 inode 索引

所有 entry（文件和目录）按 inode 号索引在 `Metadata.Entries` 中。目录通过 `Children map[string]uint64` 引用子条目，形成树结构。路径解析逐级查找 Children。

### Write ≠ Sync

`write` 系统调用只是"内核接受了数据"，不代表数据到了磁盘：

| FUSE 回调 | 含义 | mini-fs 做了什么 |
|-----------|------|-----------------|
| WRITE | 应用写入数据 | 存到内存 `staged` map |
| FLUSH | fd 关闭时调用 | 写 blob + 更新 metadata |
| FSYNC | 应用主动请求持久化 | 同上 |
| RELEASE | 内核释放文件句柄 | 什么都不做 |

### 原子写入

所有持久化操作都用 **write-tmp-then-rename** 模式：

```go
os.WriteFile("blob.tmp", data, 0644)  // 写临时文件
os.Rename("blob.tmp", "blob")          // 原子替换
```

崩溃安全性：任何一步失败，要么旧数据完好，要么新数据完整，不会出现半写。

### 内核缓存 (TTL)

```go
AttrTimeout:  time.Second   // 属性缓存 1s
EntryTimeout: time.Second   // 目录项缓存 1s
```

1 秒内重复的 `stat`/`lookup` 请求由内核直接回答，不走 FUSE 进程。代价是删除/修改后有短暂的不一致窗口。

### GC 策略

采用**写入时即时清理**（eager GC）：
- `commitInode` 写入新 blob 后立刻删除旧 blob
- `Unlink` 删文件时同步删除关联 blob

安全性：删旧 blob 在更新 metadata 之后，最坏情况只是多留一个无引用文件。

## 前置依赖

- Go 1.20+
- macOS: [macFUSE](https://osxfuse.github.io/)（`brew install macfuse`）
- Linux: `sudo apt install fuse3`

## 使用

```bash
go build -o mini-fs .

mkdir -p /tmp/minifs
./mini-fs /tmp/minifs /tmp/minifs-store
```

另开终端：

```bash
ls -l /tmp/minifs
cat /tmp/minifs/hello.txt
echo "remember the milk" > /tmp/minifs/notes.txt
mkdir /tmp/minifs/docs
echo "nested file" > /tmp/minifs/docs/readme.txt
cat /tmp/minifs/docs/readme.txt
ls /tmp/minifs/docs
mv /tmp/minifs/docs/readme.txt /tmp/minifs/top.txt
rmdir /tmp/minifs/docs
chmod 755 /tmp/minifs/notes.txt
rm /tmp/minifs/top.txt
cat /tmp/minifs-store/metadata.json
```

卸载：

```bash
# macOS
umount /tmp/minifs
# Linux
fusermount3 -u /tmp/minifs
```

## 代码结构

```
main.go（单文件，~500 行）
├── 数据结构        BlobRef / Entry / Metadata
├── MiniFS         核心逻辑（初始化、路径解析、blob 读写、提交）
├── FUSE 回调      GetAttr / OpenDir / Open / Create / Mkdir / Rmdir / Rename / Unlink / Chmod
├── miniFile       文件句柄（Read / Write / Flush / Fsync）
└── main()         参数解析 + 挂载
```

## 不支持

硬链接、符号链接、xattr、mmap、文件锁、权限模型（uid/gid）、远程存储、日志恢复。

## 进一步学习

- [Building a Tiny FUSE Filesystem](https://www.shayon.dev/post/2026/161/building-a-tiny-fuse-filesystem/) — 原始博客
- [Linux FUSE 内核文档](https://www.kernel.org/doc/html/latest/filesystems/fuse.html)
- [go-fuse 库](https://github.com/hanwen/go-fuse)
- [libfuse 低层接口文档](https://libfuse.github.io/doxygen/)

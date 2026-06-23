# mini-fs

一个用 Go 实现的迷你 FUSE 文件系统，用于学习 FUSE 工作原理。灵感来自 [shayonj/magicfs](https://github.com/shayonj/magicfs) 和 [Building a Tiny FUSE Filesystem](https://www.shayon.dev/post/2026/161/building-a-tiny-fuse-filesystem/)。

## 核心思想

文件系统本质上是一个**请求-响应循环**：用户程序调 `open`/`read`/`write` 等系统调用，内核把请求转发给 FUSE 用户态进程，我们的代码回答这些请求。

```
┌─────────────────────────────────────────────────────────┐
│  用户程序 (cat, echo, ls, rm, mv)                        │
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
├── metadata.json          # 文件名 → inode → blob 的索引
└── blobs/
    └── blob-000000000001  # 文件内容（不可变，每次写入生成新 blob）
```

`metadata.json` 示例：

```json
{
  "next_inode": 4,
  "next_blob": 3,
  "entries": {
    "hello.txt": {
      "ino": 2,
      "mode": 420,
      "size": 35,
      "mtime": 1719136000,
      "ctime": 1719136000,
      "blobs": [{"blob": "blob-000000000001", "offset": 0, "len": 35}]
    }
  }
}
```

## 调用流程

### 读文件：`cat /tmp/magic/hello.txt`

```
cat hello.txt
    │
    ▼
LOOKUP "hello.txt"  ──→  在 meta.Entries 查找 → 返回 ino=2
    │
    ▼
OPEN ino=2          ──→  创建 miniFile{ino=2} 文件句柄
    │
    ▼
READ ino=2 off=0    ──→  读 staged[2] 或 readBlobs() → 返回字节
    │
    ▼
RELEASE ino=2       ──→  关闭文件句柄
```

### 写文件：`echo "hello" > /tmp/magic/foo.txt`

```
echo "hello" > foo.txt
    │
    ▼
CREATE "foo.txt"    ──→  分配 ino=4, 写 metadata.json
    │
    ▼
OPEN ino=4          ──→  创建 miniFile{ino=4}
    │
    ▼
SETATTR size=0      ──→  截断文件（staged[4] = []byte{}）
    │
    ▼
WRITE ino=4 "hello" ──→  staged[4] = []byte("hello")  ← 纯内存！
    │
    ▼
FLUSH ino=4         ──→  putBlob("hello") 写新 blob
                         删旧 blob（GC）
                         更新 metadata.json
    │
    ▼
RELEASE ino=4       ──→  关闭
```

### 重命名：`mv foo.txt bar.txt`

```
RENAME "foo.txt" → "bar.txt"
    │
    ▼
meta.Entries 删 "foo.txt" key，加 "bar.txt" key（Entry 不变）
    │
    ▼
commitMetadata()  ──→  原子更新 metadata.json
```

### 删除：`rm bar.txt`

```
UNLINK "bar.txt"
    │
    ▼
删除关联的 blob 文件（GC）
删除 meta.Entries["bar.txt"]
删除 staged 缓冲
    │
    ▼
commitMetadata()  ──→  原子更新 metadata.json
```

## 关键设计

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

mkdir -p /tmp/magic
./mini-fs /tmp/magic /tmp/minifs-store
```

另开终端：

```bash
ls -l /tmp/magic
cat /tmp/magic/hello.txt
echo "remember the milk" > /tmp/magic/notes.txt
chmod 755 /tmp/magic/notes.txt
mv /tmp/magic/notes.txt /tmp/magic/todo.txt
rm /tmp/magic/todo.txt
cat /tmp/minifs-store/metadata.json
```

卸载：

```bash
# macOS
umount /tmp/magic
# Linux
fusermount3 -u /tmp/magic
```

## 代码结构

```
main.go（单文件，~490 行）
├── 数据结构        BlobRef / Entry / Metadata
├── MiniFS         核心逻辑（初始化、blob 读写、提交）
├── FUSE 回调      GetAttr / OpenDir / Open / Create / Rename / Unlink / Chmod
├── miniFile       文件句柄（Read / Write / Flush / Fsync）
└── main()         参数解析 + 挂载
```

## 不支持

子目录、硬链接、符号链接、xattr、mmap、文件锁、权限模型（uid/gid）、远程存储、日志恢复。

## 进一步学习

- [Building a Tiny FUSE Filesystem](https://www.shayon.dev/post/2026/161/building-a-tiny-fuse-filesystem/) — 原始博客
- [Linux FUSE 内核文档](https://www.kernel.org/doc/html/latest/filesystems/fuse.html)
- [go-fuse 库](https://github.com/hanwen/go-fuse)
- [libfuse 低层接口文档](https://libfuse.github.io/doxygen/)

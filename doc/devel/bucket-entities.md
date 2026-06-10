# GoBeansDB Bucket 详解

## 什么是 Bucket

Bucket 是 GoBeansDB 中 **数据分区的逻辑单元**。通过对 key 的 64-bit Hash 取前 `TreeDepth` 个 4-bit hex digit 来路由 key 到对应 bucket。

```
KeyHash = (FNV1a(key) << 32) | Murmur3(key)
KeyPath = [h0, h1, h2, ..., h15]   // 每 4 bit 一个 hex digit
BucketID = h0*16^(TreeDepth-1) + h1*16^(TreeDepth-2) + ...
```

| NumBucket | TreeDepth | BucketID 示例 | 目录结构 |
|---|---|---|---|
| 1 | 0 | 只有一个 bucket 0 | `./testdb/` |
| 16 | 1 | `0` ~ `f` | `./testdb/0/` ~ `./testdb/f/` |
| 256 | 2 | `00` ~ `ff` | `./testdb/0/0/` ~ `./testdb/f/f/` |

**本质**: Bucket 将全局 KeyHash 空间切分为 16^TreeDepth 份，每份独立管理自己的 data file、hint file、内存索引，实现隔离和并行。

---

## 单个 Bucket 的完整实体图

```
Bucket
├── [内存实体]
│   ├── Bucket struct              ← 管理结构 (锁/统计/状态)
│   ├── HTree                     ← 16叉内存索引树
│   │   ├── levels[][]Node        ← 内部节点 (count + hash)
│   │   └── leafs[]SliceHeader     ← 叶子节点 (C malloc, 存储 HTreeItem[])
│   ├── hintMgr                   ← Hint 管理器
│   │   ├── hintChunk[].hintSplit[].HintBuffer  ← 内存 hint 缓冲
│   │   └── CollisionTable        ← Hash 冲突表
│   ├── dataStore                 ← 数据存储管理器
│   │   └── dataChunk[].wbuf[]    ← 写缓冲 (WriteRecord 切片)
│   └── GCMgr.stat[bucket]       ← GC 状态 (仅 GC 运行时)
│
└── [磁盘实体] (bucket Home 目录下)
    ├── NNN.data                  ← 数据文件 (append-only)
    ├── NNN.NNN.idx.s             ← Hint split 文件
    ├── NNN.NNN.idx.m             ← Merged hint 文件
    ├── NNN.NNN.idx.hash          ← HTree 快照文件
    ├── collision.yaml             ← Hash 冲突持久化
    └── nextgc.txt                 ← GC 进度记录
```

---

## 内存实体结构

### 1. Bucket struct

[bucket.go:65](../../store/bucket.go#L65)：

```go
type Bucket struct {
    writeLock sync.Mutex       // 写互斥锁 (同一 bucket 串行写)
    BucketInfo                 // 嵌入统计/状态信息

    htree     *HTree           // 内存索引
    hints     *hintMgr         // hint 管理器
    datas     *dataStore       // 数据存储管理器
    GCHistory []GCState        // GC 历史记录
}

type BucketInfo struct {
    BucketStat                  // State, ID, Home, TreeID, NextGCChunk
    Pos             Position    // 当前写入位置 {ChunkID, Offset}
    LastGC          *GCState
    HintState       int         // hint 状态 (Idle/Dump/Merge/GC)
    MaxDumpedHintID HintID
    DU              int64       // 磁盘用量
    NumSameVhash    int64       // 相同 vhash 跳过次数
    SizeSameVhash   int64
    SizeVhashKey    string
    NumSet          int64       // Set 计数
    NumGet          int64       // Get 计数
}
```

### 2. HTree — 16 叉内存索引

[htree.go:25](../../store/htree.go#L25)：

```go
type HTree struct {
    sync.Mutex
    depth    int              // 在 HStore 总树中的层级 (0-based)
    bucketID int              // 该 htree 在同层 htree 列表中的偏移
    levels   [][]Node         // levels[0][0] 是根, levels[i] 是第 i 层所有节点
    leafs    []SliceHeader    // 叶子节点数组, 每个指向 C malloc 的内存块
    ni       NodeInfo         // 临时变量, 避免反复分配
}

type Node struct {
    count        uint32       // 该节点下的有效 key 数 (Ver > 0)
    hash         uint16       // 子树摘要 (用于快速 diff/比较)
    isHashUpdated bool        // hash 是否已更新 (懒更新)
}

type SliceHeader struct {     // 叶子节点 — C malloc 内存
    Data uintptr             // C.malloc 返回的地址
    Len  int                 // 内存块字节数
}
```

**叶子内存布局** (每条 item 11~14 字节):

```
+-------------------+----+----+----+----+----+----+----+----+---+
| KeyHash (5-8B)    |Ver |VHas|OffH|OffM|OffL|ChID|ChHi| ...|
| 64636261...       |0001|xxxx|00  |01  |00  |00  |00  |    |
+-------------------+----+----+----+----+----+----+----+----+---+
                      4B   2B   <--- Offset 3B ---> <ChunkID 2B>

KeyHash 长度 = TreeKeyHashLen (由 TreeDepth+TreeHeight 决定):
  depth+height-1: 1  2  3  4  5  6  7  8+
  KeyHashLen:     8  8  7  7  6  6  5  5
```

### 3. hintMgr — Hint 管理器

[hint.go:303](../../store/hint.go#L303)：

```go
type hintMgr struct {
    bucketID int
    home     string

    sync.Mutex                       // 保护 maxChunkID
    maxChunkID int

    chunks [MAX_NUM_CHUNK]*hintChunk  // 每个 data chunk 对应一个 hintChunk

    maxDumpedHintID   HintID
    dumpLock          sync.Mutex
    mergeLock         sync.Mutex
    maxDumpableChunkID int
    merged            *hintFileIndex   // merge 后的 hint 索引 (加速查找)
    state             int              // Idle/Dump/Merge/GC

    collisions *CollisionTable
}
```

**hintChunk**:

```go
type hintChunk struct {
    sync.Mutex
    id       int
    fileLock sync.RWMutex
    splits   []*hintSplit      // split 列表 (容量满时分片)
    lastTS   int64             // 最后写入时间戳
}

type hintSplit struct {
    buf  *HintBuffer           // 内存缓冲
    file *hintFileIndex        // 已 dump 的磁盘文件索引
}
```

**HintBuffer** (内存):

```go
type HintBuffer struct {
    maxoffset  uint32
    index      map[uint64]int              // KeyHash -> items 切片下标
    collisions map[uint64]map[string]int   // KeyHash -> Key -> 下标 (冲突)
    items      []*HintItem                 // item 数组
    num        int
}
```

**CollisionTable**:

```go
type CollisionTable struct {
    sync.Mutex
    HintID               // {Chunk, Split} — merge 进度
    Items map[uint64]map[string]HintItem // KeyHash -> Key -> HintItem
}
```

### 4. dataStore — 数据存储管理器

[datafile.go:19](../../store/datafile.go#L19)：

```go
type dataStore struct {
    bucketID int
    home     string

    sync.Mutex
    flushLock sync.Mutex

    oldHead int   // 旧 chunk 起始
    newHead int   // 当前活跃 chunk
    newTail int   // 尾部 chunk

    chunks [MAX_NUM_CHUNK]dataChunk  // 最多 998 个 chunk
    wbufSize      uint32             // 写缓冲总大小
    lastFlushTime time.Time
}
```

**dataChunk** (含写缓冲):

```go
type dataChunk struct {
    sync.Mutex
    chunkid     int
    path        string              // 如 "./testdb/0/000.data"
    size        uint32             // 磁盘文件大小

    writingHead uint32             // 写入头偏移 (含缓冲)
    wbuf        []*WriteRecord     // 写缓冲: 尚未刷盘的记录

    rewriting   bool               // GC rewrite 标志
    gcbufsize   uint32
    gcWriter    *DataStreamWriter   // GC 写入器
}
```

**WriteRecord** (写缓冲中的单条记录):

```go
type WriteRecord struct {
    rec    *Record           // Key + Payload
    crc    uint32
    ksz    uint32
    vsz    uint32
    pos    Position          // {ChunkID, Offset}
    header [24]byte          // 已编码的 header
}
```

---

## 磁盘实体结构

### 1. NNN.data — 数据文件

**命名**: `000.data`, `001.data`, ... `997.data` (最多 998 个)

**单条记录** (256 字节对齐):

```
Offset 0x000
+--------+--------+--------+--------+--------+--------+
| CRC32  |  TS    |  Flag  |  Ver   | KeySz  | ValSz  |  <- Header 24B
| 4B LE  | 4B LE  | 4B LE  | 4B LE  | 4B LE  | 4B LE  |
+--------+--------+--------+--------+--------+--------+
|  Key (ksz bytes)  |  Value (vsz bytes)  | Padding   |
|  "test_key"       |  "hello-from-host"  | \x00...   |
+--------+--------+--------+--------+--------+--------+
|                    整条记录 256B 对齐                      |
+----------------------------------------------------------+

CRC32 覆盖范围: header[4:] + Key + Value (不含 CRC 本身)
Ver: 正数=有效, 负数=tombstone(已删除), 0=自动递增
Flag: 0x00010000=服务端压缩, 0x00000204=incr计数器, 0x00000010=客户端压缩
```

**文件生命周期**:

```
创建 → 追加写入 (append-only) → 达到 DataFileMax(4GB) 切换新 chunk
                                        ↓
                              GC 回收旧 chunk → Truncate/Clear
```

### 2. NNN.NNN.idx.s — Hint Split 文件

**命名**: `000.000.idx.s` (chunkID.splitID.idx.s)

**文件格式**:

```
+---------------------+  <- 0x00  HINTFILE_HEAD_SIZE = 16
| indexOffset (8B LE) |  <- 指向尾部索引的偏移
| numKey     (4B LE)  |  <- key 总数
| datasize   (4B LE)  |  <- 对应 data 文件的大小
+---------------------+
|                     |
| Hint Items...       |  <- 逐条 hint item
|                     |
+---------------------+  <- indexOffset
| Hint Index          |  <- 稀疏索引 (加速查找)
| [KeyHash(8B)|Offset(8B)]...
+---------------------+
```

**单条 HintItem** (HINTITEM_HEAD_SIZE = 23 + KeyLen):

```
+---------+---------+---------+---------+---------+-------+-----+
| KeyHash | ChunkID | Offset  |  Ver    |  VHash  | KeySz | Key |
| 8B LE   | 4B LE   | 4B LE   | 4B LE   | 2B LE   | 1B    | var |
+---------+---------+---------+---------+---------+-------+-----+
```

**稀疏索引** (文件尾部):

```
+---------+---------+
| KeyHash | Offset  |   <- 每隔 IndexIntervalSize(32K) 字节一条
| 8B LE   | 8B LE   |
+---------+---------+
```

查找流程: 二分搜索稀疏索引 → 定位到区间 → 顺序扫描 hint items

### 3. NNN.NNN.idx.m — Merged Hint 文件

**命名**: `000.002.idx.m` (合并后的 hint, 格式与 `.idx.s` 相同)

与 split hint 的区别:
- 由多个 `.idx.s` 多路归并 (堆排序) 生成
- 所有 key 按 KeyHash 排序, 同一 KeyHash 的冲突 key 相邻
- merge 过程中检测到的冲突写入 CollisionTable
- 加载时可直接使用，不需要逐 split 查找

### 4. NNN.NNN.idx.hash — HTree 快照

**命名**: `000.000.idx.hash` (chunkID.splitID.idx.hash)

**文件格式**:

```
+----------------------------+  <- 0x00
| Leaf Nodes Summary         |
| count(4B LE) | hash(2B LE)|  <- 每个 leaf node 6 字节
| count(4B LE) | hash(2B LE)|
| ...                        |
+----------------------------+  <- size * 6
| Leaf Sizes                 |
| len(4B LE)                 |  <- 每个叶子的大小
| len(4B LE)                 |
| ...                        |
+----------------------------+
| Leaf Data                   |
| [leaf0 bytes]              |  <- 紧跟 leaf size 后的叶子原始数据
| [leaf1 bytes]              |
| ...                        |
+----------------------------+
```

**原子写入**: 先写 `.tmp` 再 `os.Rename` 覆盖，保证崩溃安全。

### 5. collision.yaml — Hash 冲突表

**格式**: YAML

```yaml
chunk: 5
split: 2
items:
  1234567890abcdef:
    other_key:
      keyhash: 1234567890abcdef
      pos:
        chunkid: 3
        offset: 1048576
      ver: 10
      vhash: 12345
```

### 6. nextgc.txt — GC 进度

**格式**: 纯文本, 存储一个整数

```
5
```

表示下次 GC 从 chunk 5 开始，避免重复扫描已 GC 过的 chunk。

---

## 内存与磁盘实体对应关系

```
[内存]                              [磁盘]

HTree.levels + leafs       <--->    NNN.NNN.idx.hash
  (16叉树 + 叶子C内存)            (HTree 快照, 启动时加载)

hintChunk.splits[].buf     ---->    NNN.NNN.idx.s
  (HintBuffer 满时 dump)          (hint split 文件)

多个 .idx.s                 ---->    NNN.NNN.idx.m
  (merge 归并)                    (merged hint, 加速查找)

CollisionTable.Items       <--->    collision.yaml
  (冲突表内存)                    (YAML 持久化, 启动时加载)

dataChunk.wbuf[]           ---->    NNN.data
  (写缓冲, flush 后落盘)         (append-only 数据文件)

Bucket.NextGCChunk         <--->    nextgc.txt
  (GC 进度)                       (纯文本, 启动时加载)
```

启动时加载顺序 ([bucket.go:166](../../store/bucket.go#L166)):

```
1. NewdataStore()           -> ListFiles() 扫描 .data 文件大小
2. newHintMgr()             -> loadCollisions() 加载 collision.yaml
3. newHTree()               -> 创建空树
4. 加载 .idx.hash           -> htree.load() 恢复 HTree
5. 逐 chunk 加载 .idx.s     -> loadHintsByChunk() -> updateHtreeFromHint() 恢复索引
6. 检查 hint/data 一致性    -> checkHintWithData() (hint 覆盖不足时从 data 重建)
7. loadGCHistroy()           -> 加载 nextgc.txt
```

---

## 单 Bucket 数据量估算

假设一个 bucket 有 100 万个 key，value 平均 1KB：

| 实体 | 内存占用 | 磁盘占用 |
|---|---|---|
| HTree (levels) | ~16MB (16^7 个 Node, 每个 12B) | — |
| HTree (leafs) | ~12MB (1M 个 item, 每个 ~12B) | ~12MB (.idx.hash) |
| HintBuffer | ~32MB (1M 个 HintItem, 每个 ~32B) | ~32MB (.idx.s + .idx.m) |
| dataChunk.wbuf | ~10MB (未刷盘缓冲) | — |
| CollisionTable | ~0 (冲突极少) | ~0 |
| data files | — | ~1GB (1M * 1KB + 256B对齐开销) |
| **合计** | **~70MB** | **~1.05GB** |

内存约为磁盘数据量的 7%，体现了 Bitcask 模型"内存存索引、磁盘存数据"的设计特点。

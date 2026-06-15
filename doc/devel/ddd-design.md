# GoBeansDB 领域驱动设计文档

## 1. 系统概述

GoBeansDB 是一个受 Bitcask 论文启发的分布式键值存储系统，由豆瓣开发。它以 append-only 日志结构存储数据，以内存中的 16 叉哈希树 (HTree) 作为主索引，对外暴露 memcache 协议和 HTTP REST API 两种访问接口。

**核心设计哲学**：
- 内存存索引，磁盘存数据 — 索引常驻内存，数据以 append-only 方式写入磁盘
- 最终一致性 — 通过 merkle tree 摘要比较实现副本间异步同步
- 顺序写优化 — 所有写入都是追加，从不原地修改，磁盘 IO 顺序化

---

## 2. 领域识别与子域划分

### 2.1 子域地图

```
┌─────────────────────────────────────────────────────────────┐
│                    GoBeansDB 领域                            │
│                                                             │
│  ┌──────────────┐  ┌──────────────┐  ┌───────────────────┐  │
│  │  核心子域     │  │  核心子域     │  │  支撑子域         │  │
│  │              │  │              │  │                   │  │
│  │  键值存储     │  │  数据生命周期 │  │  协议适配         │  │
│  │  (KV Store)  │  │  (Lifecycle) │  │  (Protocol)      │  │
│  │              │  │              │  │                   │  │
│  │  · Bucket    │  │  · GC 回收   │  │  · Memcache 协议  │  │
│  │  · HTree     │  │  · Hint 管理 │  │  · HTTP REST API  │  │
│  │  · DataStore │  │  · 重启恢复  │  │  · Observability  │  │
│  └──────────────┘  └──────────────┘  └───────────────────┘  │
│                                                             │
│  ┌──────────────┐  ┌──────────────┐                          │
│  │  核心子域     │  │  通用子域     │                          │
│  │              │  │              │                          │
│  │  路由与分片   │  │  基础设施     │                          │
│  │  (Routing)   │  │  (Infra)     │                          │
│  │              │  │              │                          │
│  │  · 静态哈希   │  │  · C 内存    │                          │
│  │  · ZK 路由   │  │  · 压缩      │                          │
│  │  · 热加载     │  │  · 日志      │                          │
│  └──────────────┘  │  · 配置      │                          │
│                    └──────────────┘                          │
└─────────────────────────────────────────────────────────────┘
```

### 2.2 子域说明

| 子域 | 类型 | 职责 | 核心包 |
|---|---|---|---|
| **键值存储** | 核心 | 管理键值数据的写入、读取、删除，维护内存索引 | `store/` |
| **数据生命周期** | 核心 | 管理 append-only 模型下的数据回收、hint 索引、崩溃恢复 | `store/` |
| **路由与分片** | 核心 | 将 key 路由到正确的 Bucket 和节点 | `store/`, `config/` |
| **协议适配** | 支撑 | 将外部协议 (memcache/HTTP) 适配为内部存储操作 | `memcache/`, `gobeansdb/` |
| **基础设施** | 通用 | C 内存管理、压缩、日志、配置 | `cmem/`, `quicklz/`, `loghub/`, `config/` |

---

## 3. 限界上下文 (Bounded Context)

### 3.1 上下文地图

```
                    ┌─────────────────────┐
                    │   协议适配上下文      │
                    │  (Protocol Context)  │
                    │                     │
                    │  Memcache Server     │
                    │  HTTP Data Server    │
                    │  HTTP Admin Server   │
                    │  Observability       │
                    └────────┬────────────┘
                             │
                    U/D      │  StorageClient 接口
                    ┌────────▼────────────┐
                    │   键值存储上下文      │
                    │  (Storage Context)    │
                    │                     │
                    │  HStore             │
                    │  Bucket             │
                    │  HTree              │
                    │  DataStore          │
                    │  HintMgr            │
                    │ CollisionTable     │
                    └────────┬────────────┘
                             │
                    U/D      │  KeyHash + BucketID
               ┌─────────────┼───────────────┐
               │             │               │
    ┌──────────▼──────┐ ┌────▼──────────┐ ┌───▼────────────┐
    │  路由上下文      │ │ 生命周期上下文 │ │  基础设施上下文  │
    │ (Routing Ctx)  │ │(Lifecycle Ctx)│ │  (Infra Ctx)   │
    │                │ │              │ │               │
    │ RouteTable     │ │ GCMgr       │ │ CArray        │
    │ ZKClient       │ │ HintDumper  │ │ ResourceLimiter│
    │ ChangeRoute    │ │ Flusher     │ │ QuickLZ       │
    │ KeyInfo        │ │ Recovery    │ │ LogHub        │
    └────────────────┘ └─────────────┘ └───────────────┘

    U/D = 上游/下游 (协议层为下游，存储层为上游)
```

### 3.2 上下文间的协作关系

| 上游上下文 | 下游上下文 | 协作模式 | 接口协议 |
|---|---|---|---|
| 键值存储 | 协议适配 | 遵奉者 (Conformist) | `StorageClient` 接口 |
| 路由 | 键值存储 | 开放主机服务 (OHS) | `KeyInfo.Prepare()` + `BucketID` |
| 生命周期 | 键值存储 | 开放主机服务 (OHS) | `GCMgr.gc()`, `HintDumper()`, `Flusher()` |
| 基础设施 | 所有上下文 | 开放主机服务 (OHS) | `CArray`, `ResourceLimiter`, `LogHub` |

---

## 4. 领域模型

### 4.1 聚合与实体

#### 聚合根: HStore

HStore 是最顶层的聚合根，管理所有 Bucket 的生命周期和全局路由。

```
HStore (Aggregate Root)
├── buckets []*Bucket          ← 受管实体集合
├── gcMgr *GCMgr               ← 领域服务 (GC)
├── htree *HTree                ← 全局路由树 (TreeDepth > 0 时)
└── htreeLock sync.Mutex       ← 并发控制
```

**职责**：
- 根据 BucketID 路由请求到正确的 Bucket
- 管理全局 HTree (仅当 NumBucket > 1 时)
- 协调 GC 操作
- 处理热加载/卸载 Bucket (`ChangeRoute`)
- 统计全局 key 数量

**不变量**：
- `len(buckets) == NumBucket`
- `buckets[i].State` 只有在 `BUCKET_STAT_READY` 时才能服务请求
- 同一 Bucket 的 GC 不能并发运行

#### 聚合根: Bucket

Bucket 是数据分区的核心单元，每个 Bucket 独立管理自己的索引、数据文件和 hint。

```
Bucket (Aggregate Root)
├── ID int                      ← 标识 (0 ~ NumBucket-1)
├── Home string                 ← 数据目录路径
├── State int                   ← 生命周期状态 (Empty/NotEmpty/Ready)
├── writeLock sync.Mutex        ← 写互斥锁
│
├── htree *HTree                ← 受管实体: 内存索引
├── hints *hintMgr              ← 受管实体: hint 管理器
├── datas *dataStore            ← 受管实体: 数据存储
└── GCHistory []GCState         ← 值对象: GC 历史记录
```

**职责**：
- 提供 `checkAndSet` / `get` / `incr` / `listDir` 操作
- 管理写入串行化 (writeLock)
- 版本号递增与校验
- 启动时从磁盘重建内存索引

**不变量**：
- 写入操作串行化 (同一 Bucket 互斥)
- 读取操作可并行 (读 HTree 共享锁)
- Set 操作必须三步一致：`dataStore.AppendRecord` → `HTree.set` → `hintMgr.set`
- `Ver` 递增规则：新建 key 从 0→1，更新 key 递增，删除 key 取负绝对值

**Bucket 生命周期状态机**：

```
        NewHStore()
            │
            v
     BUCKET_STAT_EMPTY ──scanBuckets()──► BUCKET_STAT_NOT_EMPTY
            │                                    │
       allocBucket()                        open()
            │                                    │
            v                                    v
     BUCKET_STAT_READY ◄─────────────── BUCKET_STAT_READY
            │                                    │
     ChangeRoute()                        ChangeRoute()
     (unload)                             (load)
            │                                    │
            v                                    v
     BUCKET_STAT_EMPTY                   (close+release)
```

#### 实体: HTree

HTree 是 Bucket 的内存索引，16 叉树结构，负责 key 到 Position 的映射。

```
HTree (Entity)
├── depth int                   ← 在总树中的起始层
├── bucketID int                ← 所属 Bucket
├── levels [][]Node            ← 内部节点层
├── leafs []SliceHeader        ← 叶子节点 (C malloc)
└── ni NodeInfo                ← 临时路由变量 (避免分配)
```

**关键行为**：
- `get(ki) → (Meta, Position, bool)`：按 KeyHash 查找
- `set(ki, meta, pos)`：插入/更新索引项
- `remove(ki, pos)`：删除索引项
- `ListDir(ki)`：按前缀列目录 (HTree 独有能力)
- `dump(path)` / `load(path)`：持久化/恢复
- `Update()`：懒更新内部节点 hash

**不变量**：
- 叶子节点中 item 的 KeyHash 经 `TreeKeyHashMask` 截断后唯一
- 内部节点 hash 懒更新，仅在读取时触发
- 叶子内存由 C malloc 管理，不受 Go GC 影响

#### 实体: dataStore

dataStore 管理 append-only 数据文件，负责数据持久化和读取。

```
dataStore (Entity)
├── bucketID int
├── home string
├── newHead int                 ← 当前活跃 chunk
├── chunks [998]dataChunk      ← 数据文件管理
└── wbufSize uint32            ← 写缓冲总大小
```

**关键行为**：
- `AppendRecord(rec) → Position`：追加记录到写缓冲
- `GetRecordByPos(pos) → (*Record, bool, error)`：按位置读取记录
- `flush(chunkID, force)`：刷盘指定 chunk 的写缓冲
- `GetStreamReader(chunkID)`：获取顺序读取器 (用于 GC/Recovery)

#### 实体: hintMgr

hintMgr 管理 hint 索引文件，是 data file 的"目录"，加速查找和恢复。

```
hintMgr (Entity)
├── bucketID int
├── home string
├── chunks [998]hintChunk     ← 每个 data chunk 对应一个 hintChunk
├── collisions *CollisionTable ← Hash 冲突表
├── state int                  ← 状态 (Idle/Dump/Merge/GC)
└── maxDumpedHintID HintID     ← 已 dump 到磁盘的进度
```

**关键行为**：
- `set(ki, meta, pos, recSize, reason)`：写入 hint buffer
- `getItem(keyhash, key) → (*HintItem, chunkID, error)`：查找 hint
- `dumpAndMerge(force)`：dump buffer 到磁盘 + 多路归并
- `loadHintsByChunk(chunkID)`：加载指定 chunk 的 hint 文件
- `forceRotateSplit()`：强制当前 split 旋转 (GC 前调用)

#### 实体: CollisionTable

CollisionTable 管理 Hash 冲突的 key，当不同 key 产生相同 KeyHash 时激活。

```
CollisionTable (Entity)
├── Items map[uint64]map[string]HintItem  ← KeyHash → Key → HintItem
└── HintID                                ← merge 进度
```

**关键行为**：
- `get(keyhash, key) → (*HintItem, bool)`
- `compareAndSet(item, reason)`：条件更新 (仅当新位置 ≥ 旧位置)
- `dump(path)` / `load(path)`：YAML 持久化

---

### 4.2 值对象 (Value Object)

| 值对象 | 定义位置 | 用途 |
|---|---|---|
| `KeyHash` | `uint64` | key 的 64-bit 双哈希标识，不可变 |
| `KeyPath` | `[16]uint8` | KeyHash 的 16 个 4-bit hex digit，用于树路由 |
| `Position` | `struct{ChunkID int, Offset uint32}` | 数据记录在文件中的物理位置 |
| `Meta` | `struct{TS, Flag uint32; Ver int32; ValueHash uint16; RecSize uint32}` | 记录元数据 |
| `HintID` | `struct{Chunk, Split int}` | hint/htree 快照的进度标识 |
| `BucketID` | `int` | Bucket 标识，由 KeyPath 前 TreeDepth 位决定 |
| `HTreeItem` | `struct{Keyhash, Pos, Ver, Vhash}` | HTree 叶子中存储的紧凑索引项 |
| `HintItem` | `HTreeItem + Key string` | hint 文件中存储的索引项 (含完整 key) |
| `GCState` | `struct{Begin, End, Src, Dst int; Running bool; ...}` | GC 执行状态快照 |
| `GCFileState` | `struct{NumBefore, NumReleased, ... int64}` | GC 文件级统计 |

值对象特征：
- 不可变 — 一旦创建不再修改 (Position/Meta 在 Set 时创建新实例)
- 按值比较 — 两个 Position 相等当且仅当 ChunkID 和 Offset 都相等
- 可替换 — 无需跟踪生命周期

---

### 4.3 领域事件 (Domain Event)

```
┌─────────────────────────────────────────────────────────────────┐
│                        领域事件                                  │
├─────────────────────┬───────────────────────────────────────────┤
│ 事件名              │ 触发时机与效果                              │
├─────────────────────┼───────────────────────────────────────────┤
│ RecordAppended     │ 数据记录追加到写缓冲后触发                    │
│                    │ → 更新 HTree 索引位置                        │
│                    │ → 写入 HintBuffer                           │
│                    │ → 唤醒 Flusher (条件满足时)                  │
├─────────────────────┼───────────────────────────────────────────┤
│ BufferFlushed      │ 写缓冲刷盘完成后触发                        │
│                    │ → 释放 WriteRecord 中的 CArray 内存          │
│                    │ → 更新 chunk.size 为磁盘文件大小             │
├─────────────────────┼───────────────────────────────────────────┤
│ HintDumped         │ HintBuffer dump 到 .idx.s 文件后触发        │
│                    │ → 清空内存 HintBuffer                        │
│                    │ → 可能触发 hint merge                        │
├─────────────────────┼───────────────────────────────────────────┤
│ HintMerged         │ 多个 .idx.s 归并为 .idx.m 后触发           │
│                    │ → 更新 CollisionTable                       │
│                    │ → 持久化 collision.yaml                     │
│                    │ → 可能触发 HTree dump                        │
├─────────────────────┼───────────────────────────────────────────┤
│ HTreeDumped        │ HTree 快照写入 .idx.hash 后触发             │
│                    │ → 更新 Bucket.TreeID                        │
│                    │ → 删除旧的快照文件                           │
├─────────────────────┼───────────────────────────────────────────┤
│ GCCycleStarted     │ GC 开始处理一个 chunk 范围时触发             │
│                    │ → 进入 HintStateGC 状态                     │
│                    │ → 强制 hint dump + merge                    │
│                    │ → 删除旧 HTree 快照                         │
├─────────────────────┼───────────────────────────────────────────┤
│ GCCycleCompleted   │ GC 完成后触发                               │
│                    │ → 退出 HintStateGC 状态                     │
│                    │ → 恢复 maxDumpableChunkID                   │
├─────────────────────┼───────────────────────────────────────────┤
│ CollisionDetected  │ 读取时发现 hash 冲突触发                     │
│                    │ → 注册冲突 key 到 CollisionTable             │
│                    │ → merge 时也会检测并注册                     │
├─────────────────────┼───────────────────────────────────────────┤
│ BucketLoaded       │ ChangeRoute 加载新 Bucket 时触发            │
│                    │ → State: Empty → Ready                      │
│                    │ → 开始服务该 Bucket 的请求                   │
├─────────────────────┼───────────────────────────────────────────┤
│ BucketUnloaded     │ ChangeRoute 卸载 Bucket 时触发              │
│                    │ → State: Ready → Empty                      │
│                    │ → 等待 10s 后 close + release               │
└─────────────────────┴───────────────────────────────────────────┘
```

---

## 5. 聚合设计详解

### 5.1 Bucket 聚合 — 写入事务

Bucket 聚合的核心操作是 `checkAndSet`，它保证了一个写入事务的原子性：

```
checkAndSet(ki, payload):
  ┌─── 锁外 ──────────────────────────────────────────────┐
  │ 1. CalcValueHash — 计算 value 摘要                       │
  │ 2. TryCompress — 尝试 QuickLZ 压缩                      │
  └──────────────────────────────────────────────────────┘
          │
          v
  ┌─── 锁内 ──────────────────────────────────────────────┐
  │ 3. writeLock.Lock()                                    │
  │ 4. get(ki, memOnly=true) → 读旧 Meta                    │
  │ 5. CheckVHash — 相同 vhash 则跳过 (可选去重)             │
  │ 6. checkAndUpdateVersion — 版本号递增校验                 │
  │ 7. set(ki, payload) — 三步写入:                         │
  │    a. dataStore.AppendRecord → Position                  │
  │    b. HTree.set(ki, meta, pos) → 更新内存索引           │
  │    c. hintMgr.set(ki, meta, pos) → 写入 hint buffer      │
  │ 8. writeLock.Unlock()                                   │
  └──────────────────────────────────────────────────────┘
```

**一致性保证**：
- 三步写入 (data → HTree → hint) 在写锁内串行完成
- 如果 AppendRecord 失败，set 不会被调用，HTree 和 hint 保持一致
- 如果 HTree.set 或 hint.set 发生 panic (不应发生)，数据已写入磁盘但索引未更新，下次重启时从 hint 重建会自动修复

### 5.2 Bucket 聚合 — 读取查询

读取路径为四级查找，不持有写锁：

```
get(ki, memOnly):
  Level 1: CollisionTable.get(keyhash, key)
    │ 命中 → 获得完整 Position (含 chunkID)
    │ 未命中 ↓
  Level 2: HTree.get(ki)
    │ 命中 → 获得 Position (仅 offset)
    │ 未命中 → return nil
    ↓
  Level 3: dataChunk.GetRecordByOffsetInBuffer(offset)
    │ 缓冲命中 → rec.Copy() (inbuffer=true)
    │ 未命中 ↓
  Level 4: readRecordAtPath(path, offset)
    │ 磁盘读取 → CRC32 校验 → 返回 Record (inbuffer=false)
```

### 5.3 HStore 聚合 — 路由

```
Set(ki, payload):
  1. ki.KeyHash = getKeyHash(ki.Key)    ← 双哈希: FNV1a<<32 | Murmur3
  2. ki.Prepare()                       ← 计算 KeyPath + BucketID
  3. bkt := store.buckets[ki.BucketID]  ← 定位 Bucket
  4. bkt.checkAndSet(ki, payload)       ← 委托给 Bucket
```

---

## 6. 领域服务

### 6.1 GC 服务 (GCMgr)

GC 是一个跨聚合的领域服务，它协调 Bucket、HTree、hintMgr 和 dataStore 来完成数据回收。

```
GCMgr.gc(bucket, begin, end, merge):
  ┌── 准备阶段 ───────────────────────────────────────┐
  │ 1. BeforeBucket():                                │
  │    - 设置 HintStateGC 标志                        │
  │    - 等待进行中的 merge 完成                       │
  │    - 强制 dump + merge 所有 hint                   │
  │    - 删除旧 HTree 快照                             │
  └──────────────────────────────────────────────────┘
          │
          v
  ┌── 回收阶段 (逐 chunk) ─────────────────────────────┐
  │ 2. 对每个 src chunk [begin, end]:                   │
  │    a. 清除该 chunk 的 hint 文件                     │
  │    b. 顺序扫描 data file 每条 Record                │
  │    c. 对每条 Record 判断是否是"最新版":             │
  │       - HTree.get(ki) 比较 Position                │
  │       - CollisionTable 查找冲突 key                │
  │    d. 最新版 → 写入目标 chunk (AppendRecordGC)     │
  │       → 更新 HTree 位置 (UpdateHtreePos)           │
  │       → 写入 hint buffer                           │
  │    e. 非最新版 → 跳过 (释放空间)                    │
  │    f. 扫描完成后 Clear 源 chunk 文件                │
  └──────────────────────────────────────────────────┘
          │
          v
  ┌── 收尾阶段 ───────────────────────────────────────┐
  │ 3. AfterBucket():                                  │
  │    - 清除 HintStateGC 标志                        │
  │    - 恢复 maxDumpableChunkID                      │
  └──────────────────────────────────────────────────┘
```

**GC 中的一致性**：
- GC 开始前强制 merge，确保 CollisionTable 包含所有冲突信息
- GC 期间新写入的 hint buffer 也会被检查，避免误删冲突 key 的记录
- GC 设置 `maxDumpableChunkID` 限制，确保新写入的 hint 不会被 dump 到 GC 正在清理的 chunk 范围

### 6.2 恢复服务 (Recovery)

恢复服务在 Bucket.open() 中执行，负责从磁盘重建完整的内存索引。

```
Bucket.open(bucketID, home):
  Step 0: 初始化实体
    - NewdataStore() → 扫描 .data 文件大小
    - newHintMgr() → 创建 hint 管理器
    - loadCollisions() → 加载 collision.yaml
    - newHTree() → 创建空树

  Step 1: 加载 .idx.hash 快照
    - 选择最新有效快照
    - htree.load() 恢复 HTree 内存索引

  Step 2: 增量恢复 (hint → HTree)
    - 从 TreeID 之后的 hint 分片逐个恢复
    - checkHintWithData() 确保 hint 覆盖范围
    - 不足时 buildHintFromData() 从 data 重建

  Step 3: 异步检查快照前 chunk
    - goroutine 异步检查 hint/data 一致性

  Step 4: 按需 dump 新快照
    - 无快照或快照过旧时立即 dump
```

### 6.3 Flusher 服务

后台 goroutine，定时将写缓冲中的数据刷盘。

```
Flusher():
  loop:
    wait(FlushInterval=60s 或 FlushWake=10M 字节)
    for each bucket:
      if bucket.datas != nil:
        bucket.datas.flush(-1, force=false)
```

### 6.4 HintDumper 服务

后台 goroutine，定时将 hint buffer dump 到磁盘，并触发 merge。

```
HintDumper(interval=1min):
  loop:
    for each bucket (READY):
      bucket.hints.dumpAndMerge(force=false)
    wait(interval 或 mergeChan 信号)
```

---

## 7. 核心领域规则

### 7.1 KeyHash 路由规则

```
KeyHash = (FNV1a(key) << 32) | Murmur3(key)

路由层 (TreeDepth 层):
  KeyPath = [h0, h1, ..., h15]  ← KeyHash 的每 4 bit
  BucketID = KeyPath[:TreeDepth] 组合

Bucket 内层 (TreeHeight 层):
  路由 KeyPath[TreeDepth:] 到叶子节点
```

**约束**：
- `NumBucket` 只能取 1 / 16 / 256
- `TreeDepth + TreeHeight ≤ 8` (MAX_DEPTH)
- FNV 的高位用于路由，Murmur 的低位保留给叶子内区分

### 7.2 版本号 (Ver) 规则

| 场景 | oldv | 传入 ver | 结果 ver |
|---|---|---|---|
| 新建 key | 0 | 0 | 1 |
| 更新已存在 key | 5 | 0 | 6 |
| 更新已删除 key | -3 | 0 | 4 |
| 强制删除 | 5 | -1 | -6 |
| 版本不递增 (拒绝) | 5 | 3 | 拒绝 |
| 删除不存在的 key | 0 | -1 | NOT_FOUND |

**规则**：
- `ver=0` 表示自动递增
- `ver<0` 表示删除 (tombstone)
- 新版本号的绝对值必须严格大于旧版本号的绝对值

### 7.3 去重规则 (CheckVHash)

```
if oldv > 0 && newValueHash == oldValueHash:
    if CheckVHash 开启:
        if ver != 0:   // sync script with specific ver
            仅更新 HTree 位置 (不同 chunk, 相同 value)
        return  // 跳过实际写入
```

**设计意图**：数据同步场景下，同一 value 可能被写入多次，CheckVHash 避免产生冗余记录。

### 7.4 压缩规则

```
TryCompress():
  跳过条件:  Ver < 0 / 已压缩 / body ≤ 256B
  检测:      Content-Type (前 10K), 音频不压缩
  试压缩:    QuickLZ 前 10K
  判定:      压缩比 > 0.7 → 放弃
  执行:      全量压缩, 设置 FLAG_COMPRESS
```

### 7.5 CollisionTable 注册规则

```
compareAndSet(item, reason):
  if 新 item 不存在:
    创建新 map entry
  elif reason == "gc" 或 新位置 ≥ 旧位置:
    更新 (GC 具有最高优先级)
  else:
    保持旧值 (正常写入不能覆盖更深的记录)
```

---

## 8. 上下文交互流程

### 8.1 写入流程 (协议层 → 存储层)

```
[协议适配上下文]                [键值存储上下文]              [生命周期上下文]

HTTP PUT /api/v1/object/k
  │
  ├─ Auth + 校验
  ├─ 解析 body → CArray
  ├─ 构建 Item{Flag,Exptime,CArray}
  │
  └─ StorageClient.Set(key, item)
       │
       ├─ IsValidKeyString(key)
       ├─ KeyInfo{StringKey, Key}
       ├─ Payload{Flag, CArray, Ver, TS}
       │
       └─ HStore.Set(ki, payload)
            │
            ├─ KeyHash = getKeyHash(ki.Key)
            ├─ ki.Prepare() → BucketID
            │
            └─ Bucket.checkAndSet(ki, v)
                 │
                 ├─ [锁外] CalcValueHash + TryCompress
                 ├─ writeLock.Lock()
                 ├─ get(ki, memOnly=true) → old Meta
                 ├─ CheckVHash (可选去重)
                 ├─ checkAndUpdateVersion → 新 Ver
                 │
                 ├─ Bucket.set(ki, v) ──────────────────┐
                 │   ├─ dataStore.AppendRecord → pos     │
                 │   ├─ HTree.set(ki, meta, pos)         │ 异步:
                 │   └─ hintMgr.set(ki, meta, pos)       │ Flusher 刷盘
                 ├─ writeLock.Unlock()                    │ HintDumper dump
                 │                                       │ HintDumper merge
                 └─ 返回 true                             │ HTree dump
                                                        └────────────────→
```

### 8.2 读取流程 (存储层 → 协议层)

```
HTTP GET /api/v1/object/k
  │
  └─ StorageClient.Get(key)
       │
       ├─ key[0] 分支:
       │   ├─ '@' → 列目录 / collision 查询
       │   ├─ '?' → 元信息查询 (Ver, VHash, Flag, Size, TS)
       │   └─ 其他 → 普通读取
       │
       └─ HStore.Get(ki, memOnly=false)
            │
            └─ Bucket.get(ki, memOnly=false)
                 │
                 ├─ L1: CollisionTable.get(keyhash, key)
                 │   ├─ 命中 → pos (含 chunkID)
                 │   └─ 未命中 ↓
                 │
                 ├─ L2: HTree.get(ki)
                 │   ├─ 命中 → meta + pos
                 │   └─ 未命中 → return nil
                 │
                 ├─ L3: dataChunk.GetRecordByOffsetInBuffer(offset)
                 │   ├─ 二分搜索 wbuf
                 │   ├─ 命中 → rec.Copy() (inbuffer=true)
                 │   └─ 未命中 ↓
                 │
                 ├─ L4: readRecordAtPath(path, offset)
                 │   ├─ ReadAt header (24B)
                 │   ├─ 校验 key/value size
                 │   ├─ ReadAt key+value (一次 IO)
                 │   ├─ CRC32 校验
                 │   └─ 返回 Record (inbuffer=false)
                 │
                 ├─ bytes.Compare(rec.Key, ki.Key)
                 │   ├─ 匹配 → 返回 Payload
                 │   └─ 不匹配 → Hash 冲突处理
                 │       ├─ 查 hint 找正确位置
                 │       ├─ 注册两个 key 到 CollisionTable
                 │       └─ 从正确位置读取返回
                 │
       └─ Decompress (如需)
```

### 8.3 GC 流程 (生命周期 → 存储层)

```
HTTP POST /gc/{bucket_id}?run=true

  └─ HStore.GC(bucketID, begin, end, noGCDays, merge)
       │
       ├─ gcCheckRange() → 确定 [begin, end] 范围
       │
       └─ GCMgr.gc(bucket, begin, end, merge)
            │
            ├─ BeforeBucket() ──────────────────────────┐
            │   ├─ HintStateGC 标志                       │
            │   ├─ 等待 merge 完成                        │
            │   ├─ 强制 hint dump + merge                 │
            │   └─ 删除旧 HTree 快照                     │
            │                                             │
            ├─ [逐 chunk 回收]                            │
            │   ├─ 扫描 data file 每条 Record            │
            │   ├─ 判断: HTree pos == record pos?        │
            │   │   ├─ 是 → 最新版, 复写到 dst chunk     │
            │   │ 否 → 非最新版, 跳过 (释放)        │
            │   ├─ 更新 HTree 位置 (src→dst)             │
            │   ├─ 写入 hint buffer                       │
            │   └─ Clear 源 chunk 文件                    │
            │                                             │
            └─ AfterBucket() ────────────────────────────┘
                ├─ 清除 HintStateGC 标志
                └─ 恢复 maxDumpableChunkID
```

---

## 9. 领域模型与代码映射

### 9.1 类型映射表

| 领域概念 | 代码类型 | 所在包 | 文件 |
|---|---|---|---|
| HStore | `*HStore` | `store` | [hstore.go](../../store/hstore.go) |
| Bucket | `*Bucket` | `store` | [bucket.go](../../store/bucket.go) |
| HTree | `*HTree` | `store` | [htree.go](../../store/htree.go) |
| Node (内部节点) | `Node` | `store` | [htree.go](../../store/htree.go) |
| SliceHeader (叶子) | `SliceHeader` | `store` | [leaf.go](../../store/leaf.go) |
| dataStore | `*dataStore` | `store` | [datafile.go](../../store/datafile.go) |
| dataChunk | `dataChunk` | `store` | [datachunk.go](../../store/datachunk.go) |
| hintMgr | `*hintMgr` | `store` | [hint.go](../../store/hint.go) |
| hintChunk | `*hintChunk` | `store` | [hint.go](../../store/hint.go) |
| HintBuffer | `*HintBuffer` | `store` | [hint.go](../../store/hint.go) |
| CollisionTable | `*CollisionTable` | `store` | [collision.go](../../store/collision.go) |
| GCMgr | `*GCMgr` | `store` | [gc.go](../../store/gc.go) |
| GCState | `GCState` | `store` | [gc.go](../../store/gc.go) |
| KeyInfo | `*KeyInfo` | `store` | [key.go](../../store/key.go) |
| Payload | `*Payload` | `store` | [item.go](../../store/item.go) |
| Meta | `Meta` | `store` | [item.go](../../store/item.go) |
| Position | `Position` | `store` | [item.go](../../store/item.go) |
| Record | `*Record` | `store` | [item.go](../../store/item.go) |
| HTreeItem | `HTreeItem` | `store` | [item.go](../../store/item.go) |
| HintItem | `HintItem` | `store` | [item.go](../../store/item.go) |
| CArray | `CArray` | `cmem` | [cmem.go](../../cmem/cmem.go) |
| ResourceLimiter | `*ResourceLimiter` | `cmem` | [cmem.go](../../cmem/cmem.go) |
| StorageClient | `*StorageClient` | `gobeansdb` | [store.go](../store.go) |
| DBConfig | `DBConfig` | `gobeansdb` | [config.go](../config.go) |
| RouteTable | `*RouteTable` | `config` | [route.go](../../config/route.go) |

### 9.2 接口映射表

| 领域概念 | 代码接口 | 所在包 | 文件 |
|---|---|---|---|
| 存储客户端 | `StorageClient` | `memcache` | [store.go](../../memcache/store.go) |
| 存储提供者 | `Storage` | `memcache` | [store.go](../../memcache/store.go) |
| 日志中心 | `LogHub` | `loghub` | [log.go](../../loghub/log.go) |

---

## 10. 架构分层

### 10.1 六边形架构视图

```
                        ┌──────────────────────────┐
                        │      应用层 (App)         │
                        │                          │
   Memcache TCP ◄──────│  Server / WebServer      │
   HTTP REST API ◄─────│  DataWebServer           │──────► gobeansdb/
   HTTP Admin ◄────────│  WebHandler              │
                        │                          │
                        ├──────────────────────────┤
                        │      适配层 (Adapter)    │
                        │                          │
                        │  StorageClient           │──────► gobeansdb/store.go
                        │  (mc.StorageClient 实现) │
                        │                          │
                        ├──────────────────────────┤
                        │      领域层 (Domain)      │
                        │                          │
                        │  HStore → Bucket →       │──────► store/
                        │    HTree + DataStore +   │
                        │    HintMgr + Collision   │
                        │                          │
                        │  GCMgr (领域服务)         │
                        │  Recovery (领域服务)       │
                        │  KeyInfo (路由服务)       │
                        │                          │
                        ├──────────────────────────┤
                        │      基础设施层 (Infra)   │
                        │                          │
                        │  CArray (C 内存)         │──────► cmem/
                        │  QuickLZ (压缩)          │──────► quicklz/
                        │  LogHub (日志)           │──────► loghub/
                        │  Config (配置)            │──────► config/
                        │  Utils (工具)            │──────► utils/
                        └──────────────────────────┘
```

### 10.2 依赖方向

```
gobeansdb/ ──依赖──► store/
           ──依赖──► memcache/
           ──依赖──► config/
           ──依赖──► cmem/
           ──依赖──► loghub/

store/     ──依赖──► cmem/
           ──依赖──► config/
           ──依赖──► loghub/
           ──依赖──► utils/
           ──依赖──► quicklz/  (cgo)

memcache/ ──依赖──► cmem/  (仅 Item.CArray)
           无 store 依赖    (通过接口解耦)

config/   ──依赖──► loghub/

cmem/     ──依赖──► cgo (C malloc/free)

quicklz/  ──依赖──► cgo (C QuickLZ)
```

**关键解耦点**：`memcache.StorageClient` 接口使协议层与存储层完全解耦。`memcache` 包不知道 `store.HStore` 的存在，只通过接口交互。

---

## 11. 并发模型

### 11.1 锁策略

| 资源 | 锁类型 | 粒度 | 持锁范围 |
|---|---|---|---|
| Bucket 写入 | `sync.Mutex` | Bucket 级 | checkAndSet 全程 |
| HTree 操作 | `sync.Mutex` (HTree 自身) | Tree 级 | 每次 get/set/remove |
| dataStore 操作 | `sync.Mutex` (dataStore 自身) | Store 级 | AppendRecord + chunk 切换 |
| dataChunk 操作 | `sync.Mutex` (dataChunk 自身) | Chunk 级 | AppendRecord / GetRecord / flush |
| hintChunk 操作 | `sync.Mutex` (hintChunk 自身) | Chunk 级 | setItem / dump |
| CollisionTable | `sync.Mutex` | Table 级 | get / compareAndSet |
| GC 全局 | `sync.Mutex` | 全局 | 防止同一 bucket 并发 GC |
| GC 状态 | `sync.RWMutex` | Mgr 级 | 读取/更新 GC 状态 |
| HStore 全局树 | `sync.Mutex` | Store 级 | ListUpper / ChangeRoute |
| 配置热加载 | `AllowReload` | 全局 | 运行时开关 |

### 11.2 并发写入保证

```
同一 Bucket:
  ┌────────────────────────────────────┐
  │  writeLock.Lock()                  │
  │  ┌─── get(old) ──┐                 │
  │  │   HTree读    │ ← 可与读并发     │
  │  └──────────────┘    (HTree 自身锁) │
  │  ┌─── set(new) ──┐                 │
  │  │  data write  │                  │
  │  │  HTree写     │ ← 独占          │
  │  │  hint write  │                  │
  │  └──────────────┘                  │
  │  writeLock.Unlock()                │
  └────────────────────────────────────┘

不同 Bucket:
  完全并行 (独立 writeLock)
```

---

## 12. 磁盘格式与持久化策略

### 12.1 文件格式总览

```
<Bucket Home>/
├── 000.data                    ← 数据文件 (append-only, 256B 对齐)
├── 001.data
├── 000.000.idx.s               ← Hint split 文件
├── 000.001.idx.s
├── 000.002.idx.m               ← Merged hint 文件
├── 000.000.idx.hash            ← HTree 快照文件
├── collision.yaml              ← Hash 冲突表 (YAML)
└── nextgc.txt                  ← GC 进度记录
```

### 12.2 持久化策略

| 数据 | 持久化方式 | 时机 | 原子性保证 |
|---|---|---|---|
| Data 记录 | append-only 写入 | 先入 wbuf, 后异步 flush | 单条记录 CRC32 校验 |
| Hint buffer | dump 到 .idx.s | 延迟 > 5秒 或 HintDumper 周期 | 先 .tmp 后 rename |
| Merged hint | 多路归并到 .idx.m | 每 MergeInterval 个 chunk | 先 .tmp 后 rename |
| HTree 快照 | dump 到 .idx.hash | 每 TreeDump 个 chunk | 先 .tmp 后 rename |
| CollisionTable | dump 到 collision.yaml | merge 后 / close 时 | YAML 覆盖写 |
| GC 进度 | nextgc.txt | chunk Clear 后 | 纯文本覆盖写 |

### 12.3 恢复优先级

```
.idx.hash 快照    →  1 (最快, O(leaf_count) IO)
.idx.s/.idx.m hint →  2 (增量, 快照之后的部分)
.data 文件         →  3 (兜底, hint 不足时扫描重建)
```

---

## 13. 容量规划与性能特征

### 13.1 内存模型

单 Bucket 1M key 的资源估算：

| 组件 | 内存 | 占比 |
|---|---|---|
| HTree levels (Node[]) | ~16MB | 50% |
| HTree leafs (C items) | ~12MB | 37% |
| HintBuffer | ~32MB | (可释放) |
| dataChunk wbuf | ~10MB | 3% |
| **常驻合计** | **~38MB** | |

内存/磁盘比约 3.8% (38MB / 1GB)，体现 Bitcask 模型的高索引效率。

### 13.2 IO 特征

| 操作 | IO 模式 | 延迟特征 |
|---|---|---|
| 读取 (缓冲命中) | 零 IO | < 0.1ms |
| 读取 (磁盘) | 单次 ReadAt | ~0.5-2ms (HDD), ~0.05ms (SSD) |
| 写入 | 写入内存缓冲 | < 0.1ms (同步) |
| 刷盘 | 顺序追加写 | 批量合并，IO 效率高 |
| GC | 顺序读 + 顺序写 | 后台异步，不影响前台延迟 |

### 13.3 关键约束

| 约束 | 值 | 原因 |
|---|---|---|
| 单 chunk 最大 | 4GB | uint32 offset 上限 (实际 4GB 对齐后) |
| 最大 chunk 数 | 998 | 数组预留空间 |
| KeyHash 位数 | 64 bit | FNV1a(32) + Murmur3(32) |
| 树最大深度 | 8 | 16^8 = 4G 叶子，足够 |
| Key 最大长度 | 250 字节 | memcache 协议限制 |
| Value 最大 | 50MB | 配置限制 BodyMax |
| 写入缓冲最大 | 100MB | FlushMaxStr 配置限制 |

# GoBeansDB 项目架构文档

## 概述

GoBeansDB 是一个参考 Bitcask 论文设计的 KV 存储引擎，由豆瓣(douban)开发。它采用 append-only 日志 + 内存索引的架构，支持 memcache 协议访问，具备自动 GC、数据压缩、热加载/卸载 bucket 等特性。

## 与 Bitcask 的对应关系

| Bitcask 概念 | GoBeansDB 实现 | 说明 |
|---|---|---|
| Active data file | `dataStore` + `dataChunk` | 每个 bucket 拥有多 chunk 轮转的 data file，单文件达到 `DataFileMax`(默认4GB) 后切换新 chunk |
| Data file | `NNN.data` | 以 256 字节对齐的 append-only 记录文件，每条记录含 CRC 校验 |
| Hint file | `NNN.NNN.idx.s` / `NNN.NNN.idx.m` | `.s` 为 split hint，`.m` 为合并后的 hint，含索引加速查找 |
| Key directory (in-memory) | `HTree` + `CollisionTable` | 内存中用 16 叉哈希树索引，hash 冲突由 CollisionTable 处理 |
| Merge (GC) | `GCMgr` | 扫描旧 data chunk，保留最新记录，删除过期/重复数据，释放空间 |

## 整体架构

```
┌─────────────────────────────────────────────────────┐
│                    main.go                          │
│              (入口, 信号处理, 启动流程)                │
└──────────────────┬──────────────────────────────────┘
                   │
        ┌──────────┴──────────┐
        │                     │
┌───────▼────────┐  ┌────────▼─────────┐
│   memcache/    │  │  gobeansdb/      │
│  (协议层)      │  │  (应用层)         │
│                │  │                  │
│  Server ───────►  Storage          │
│  Protocol      │  StorageClient    │
│  Request/Resp  │  (适配层)         │
└───────┬────────┘  └────────┬────────┘
        │                    │
        │      ┌────────────▼────────────┐
        │      │       store/           │
        │      │    (核心存储引擎)       │
        │      │                        │
        │      │  HStore (顶层调度)      │
        │      │    ├── Bucket[]        │
        │      │    │     ├── HTree     │
        │      │    │     ├── hintMgr   │
        │      │    │     └── dataStore │
        │      │    │           └── dataChunk[]│
        │      │    └── GCMgr           │
        │      └────────────────────────┘
        │
┌───────▼────────┐  ┌────────────────┐  ┌───────────────┐
│   config/       │  │    cmem/       │  │  loghub/      │
│  (配置管理)     │  │  (C内存管理)   │  │  (日志系统)   │
│  ServerConfig  │  │  CArray        │  │  ErrorLog     │
│  RouteTable    │  │  ResourceLimiter│  │  AccessLog    │
│  MCConfig      │  │  (大块C malloc)│  │  AnalysisLog  │
│  ZKClient      │  └────────────────┘  └───────────────┘
└────────────────┘
                                        ┌───────────────┐
                                        │  quicklz/     │
                                        │  (数据压缩)   │
                                        │  C实现压缩    │
                                        └───────────────┘
```

## 模块详解

### 1. 入口层 (`main.go` / `gobeansdb/gobeansdb.go`)

**入口流程**:
1. 解析命令行参数 (`-confdir`, `-buildhint`, `-dumpconf`, `-version`)
2. 加载配置 (`conf.Load()`)
3. 初始化日志系统 (`loghub.InitLogger()`)
4. 创建 `HStore` (`store.NewHStore()`) — 加载所有 bucket
5. 创建 memcache Server (`mc.NewServer(storage)`)
6. 启动后台 goroutine: `HintDumper`(每分钟)、`Flusher`(定时刷盘)
7. 进入 `server.Serve()` 事件循环

**gobeansdb.go** 还包含:
- `WebServer`: 提供 HTTP 管理 API (端口 7903)
- `DataWebServer`: 提供 HTTP 数据 CRUD API (可选)

### 2. 协议层 (`memcache/`)

实现了 memcache 文本协议，是 GoBeansDB 对外的网络接口。

| 文件 | 职责 |
|---|---|
| `server.go` | TCP Server，Accept 连接，每个连接一个 goroutine 处理，信号处理(SIGUSR1重开日志) |
| `protocol.go` | memcache 协议解析/响应，支持命令: `get/gets/set/add/replace/cas/delete/incr/decr/stats/version/quit` |
| `store.go` | `Storage` 接口 + `StorageClient` 接口，桥接 memcache 协议与底层存储 |
| `stats.go` | 统计信息 (cmd_get, cmd_set, get_hits, get_misses 等) |
| `token.go` | 请求令牌/限流器 |

**特殊 key 前缀**:
- `?key`: 获取 key 的元信息 (ver, vhash, flag, size, ts)
- `??key`: 获取 key 的 record 详细信息 (含 chunkID, offset)
- `@path`: 列目录 (HTree 树形浏览)
- `@@keyhash`: 通过 keyhash 获取 record

### 3. 存储引擎层 (`store/`) — 核心

这是 Bitcask 模型的核心实现。

#### 3.1 HStore (`hstore.go`)

顶层调度器，管理所有 Bucket。

- **数据结构**: `buckets []*Bucket` + `gcMgr *GCMgr`
- **启动时**: 扫描数据目录 -> 并发打开所有 bucket -> 加载 HTree
- **Get/Set/Incr**: 先计算 keyhash -> 路由到 bucket -> 委托 bucket 执行
- **HintDumper**: 后台定时 dump hint 并触发 merge
- **Flusher**: 后台定时将写缓冲刷盘
- **ChangeRoute**: 热加载/卸载 bucket (用于在线扩缩容)

#### 3.2 Bucket (`bucket.go`)

单个逻辑分区的存储管理单元，对应一个子目录 (如 `8/`, `b/`)。

- **数据结构**: `htree *HTree` + `hints *hintMgr` + `datas *dataStore`
- **open()**: 加载 HTree -> ListFiles -> 逐 chunk 加载 hint -> 从 hint 恢复 HTree -> 检查 hint 与 data 一致性
- **checkAndSet()**: 乐观写 — 读取旧值比较 vhash，相同则跳过(CheckVHash)，不同则版本号递增后 append 写入
- **get()**: 先查 CollisionTable -> 再查 HTree -> 按 Position 读取 data file

**Bucket 状态机**:
```
BUCKET_STAT_EMPTY -> BUCKET_STAT_NOT_EMPTY -> BUCKET_STAT_READY
```

#### 3.3 HTree (`htree.go` / `leaf.go`)

**16 叉哈希树** — GoBeansDB 的核心内存索引结构，替代 Bitcask 原论文中的 flat hash table。

```
              Root (level 0)
           /  |  ...  |  \
        16 children (level 1)
       / | \
     ...     (up to level 7)
     |
   Leaf (SliceHeader, C malloc 内存)
   存储: [keyhash1 | item1][keyhash2 | item2]...
```

- **TreeDepth**: 由 `NumBucket` 决定 (16^depth = NumBucket)，用于 key 路由到 bucket
- **TreeHeight**: 可配置 (默认 3-7)，决定树深度
- **Node**: `count` (key数) + `hash` (子树摘要，用于快速比较)
- **Leaf**: 使用 C `malloc` 分配的连续内存块，存储 `HTreeItem`(keyhash + pos + ver + vhash)
- **查找**: keyhash 的每 4 bit 对应树的一层，逐层路由到叶子节点
- **持久化**: `dump()`/`load()` 序列化到 `.idx.hash` 文件

**Leaf 内存布局** (`leaf.go`):
```
| keyhash (5-8 bytes) | ver(4B) | vhash(2B) | offset(3B) | chunkID(2B) |
```
使用 C 的 `malloc/realloc/free` 和 `memcmp` 实现高性能内存操作。

#### 3.4 Data Store (`datafile.go` / `datachunk.go`)

**append-only 数据文件**，对应 Bitcask 的 data file。

**文件命名**: `NNN.data` (如 `000.data`, `001.data`)

**记录格式** (`WriteRecord`):
```
| CRC32(4B) | TS(4B) | Flag(4B) | Ver(4B) | KeySize(4B) | ValueSize(4B) | Key | Value | Padding |
```
- 记录以 256 字节对齐 (PADDING)
- CRC32 校验 (C 实现)
- Flag 含压缩标志 (`FLAG_COMPRESS = 0x00010000`)
- Ver: 正数=有效, 负数=已删除, 0=自动递增

**写缓冲**: `dataChunk.wbuf` — 新写入先缓存在内存，定时/定量刷盘

**dataStore 管理**:
- `oldHead`, `newHead`, `newTail`: chunk ID 管理
- 写入时若当前 chunk 满了 (超过 `DataFileMax`)，切换到新 chunk
- 读取时先查写缓冲 (inbuffer=true)，未命中则读磁盘文件

#### 3.5 Hint 系统 (`hint.go` / `hintfile.go` / `hintindex.go` / `hintmerge.go`)

**hint 文件**是 data file 的"目录"，记录每条记录的 keyhash+位置+元信息，用于快速重建内存索引。

**文件命名**: `NNN.NNN.idx.s` (split hint) / `NNN.NNN.idx.m` (merged hint)

**hint 记录格式** (`HintItem`):
```
| KeyHash(8B) | ChunkID(4B) | Offset(4B) | Ver(4B) | VHash(2B) | KeyLen(1B) | Key |
```

**层级结构**:
```
hintMgr (per bucket)
  ├── hintChunk[] (per data chunk)
  │     └── hintSplit[] (容量满时分片)
  │           ├── HintBuffer (内存缓冲, map[KeyHash]Item)
  │           └── hintFileIndex (持久化文件+索引)
  └── CollisionTable (hash冲突表, YAML持久化)
```

**工作流**:
1. 每次 Set 写入 data 后，同步写入 hint buffer
2. hint buffer 满 (SplitCap) 后 rotate 新 split，dump 旧 split 到 `.idx.s` 文件
3. 后台 HintDumper 定时触发，检查是否有可 dump 的 split
4. 达到 MergeInterval 后触发 merge: 多路归并所有 `.idx.s` -> `.idx.m`
5. merge 过程中检测 hash 冲突 -> 更新 CollisionTable

**hint 索引** (`hintindex.go`):
- hint 文件尾部存储 keyhash->offset 的稀疏索引
- 查找时先定位到可能的区间，再线性扫描

#### 3.6 GC (Merge/Compaction) (`gc.go`)

对应 Bitcask 的 merge 操作，清理旧数据释放空间。

**GC 流程**:
1. `gcCheckRange()`: 确定要 GC 的 chunk 范围 (受 `NoGCDays` 限制)
2. `BeforeBucket()`: dump+merge hint，删除旧 HTree (GC 期间将新写入 hold 在 hint buffer)
3. 逐 chunk 扫描 data file，对每条记录:
   - 查 HTree 判断是否为最新版本
   - 查 CollisionTable 处理 hash 冲突
   - 保留最新记录，写入目标 chunk
   - 更新 HTree 中的 Position
4. 源 chunk 数据清除
5. `AfterBucket()`: 恢复 hint 状态

**GC 统计**: `GCFileState` 记录回收前后的记录数/大小/已删除数等。

**支持取消**: `CancelFlag` 可中断正在进行的 GC。

#### 3.7 CollisionTable (`collision.go`)

处理 keyhash 冲突 (不同 key 产生相同 hash)。

- **数据结构**: `map[uint64]map[string]HintItem` — keyhash -> key -> HintItem
- **持久化**: YAML 格式存储在 `collision.yaml`
- **用途**: 当 get 操作发现 HTree 中的记录 key 不匹配时，查 CollisionTable 获取正确记录

#### 3.8 Key 路由 (`key.go`)

**KeyHash 计算**:
```
KeyHash = (FNV1a(key) << 32) | Murmur3(key)
```
64-bit hash，高 32 位 FNV1a，低 32 位 Murmur3。

**路由到 Bucket**:
```
KeyHash 的前 TreeDepth 个 4-bit hex digit -> BucketID
```
例如 `NumBucket=256, TreeDepth=2`: KeyHash 前 2 个 hex 字符决定 bucket。

### 4. C 内存管理 (`cmem/`)

使用 C 的 `malloc/free` 管理大块内存，避免 Go GC 压力。

- `CArray`: 封装 C malloc 的字节数组，小对象 (<4K) 用 Go 原生分配
- `ResourceLimiter`: 跟踪内存分配/释放计数和大小 (SetData/GetData/FlushData)
- `DBRL`: 全局资源限制器实例，用于 OOM 防护和监控

### 5. 配置系统 (`config/`)

| 文件 | 职责 |
|---|---|
| `config.go` | 全局配置加载、版本号、YAML 解析工具 |
| `server_config.go` | 服务器配置 (端口、线程、日志路径、ZK地址) |
| `mc_config.go` | Memcache 协议限制 (key长度、value大小上限) |
| `route.go` | 路由表 (bucket -> server 映射)，支持本地YAML和ZK两种方式 |
| `zk.go` | ZooKeeper 客户端，用于集群路由发现和热更新 |

**配置层级**: `global.yaml` (默认) -> `local.yaml` (覆盖) -> ZK (运行时)

**关键配置项**:

| 配置 | 默认值 | 说明 |
|---|---|---|
| `NumBucket` | 16 | bucket 数量 (16/256) |
| `TreeHeight` | 7 | HTree 层数 |
| `DataFileMax` | 4000M | 单个 data file 最大大小 |
| `FlushInterval` | 60s | 数据刷盘间隔 |
| `NoGCDays` | 7 | 不 GC 最近 N 天的数据 |
| `SplitCap` | 1M | hint buffer 容量 |
| `MergeInterval` | 5 | 每 N 个 chunk 轮转触发 merge |
| `BodyMax` | 50M | 单个 value 最大大小 |

### 6. 数据压缩 (`quicklz/`)

使用 QuickLZ 算法 (C 实现) 压缩 value:
- 仅压缩 > 256 字节且非音频类型的数据
- 压缩比 > 0.7 时不压缩 (不划算)
- 支持 `FLAG_CLIENT_COMPRESS` (客户端预压缩) 和 `FLAG_COMPRESS` (服务端压缩)

### 7. 日志系统 (`loghub/`)

| Logger | 用途 |
|---|---|
| `ErrorLogger` | 错误/运行日志 |
| `AccessLogger` | 请求访问日志 (cmd, key, size, 耗时) |
| `AnalysisLogger` | 性能分析日志 (get 耗时、chunkID、记录大小) |

支持 SIGUSR1 信号重开日志文件 (配合 logrotate)。

## 数据目录结构

```
/home/beansdb/           # Home 目录
├── 0/                   # Bucket 0 (16 buckets: 0-f)
│   ├── 000.data         # Data chunk 0
│   ├── 001.data         # Data chunk 1
│   ├── 000.000.idx.s    # Hint: chunk 0, split 0
│   ├── 000.001.idx.s    # Hint: chunk 0, split 1
│   ├── 001.000.idx.s    # Hint: chunk 1, split 0
│   ├── 001.000.idx.m    # Merged hint: chunk 1, split 0
│   ├── 000.000.idx.hash # HTree snapshot
│   ├── collision.yaml   # Hash 冲突表
│   └── nextgc.txt       # GC 进度记录
├── 1/
├── ...
└── f/
```

## 读写路径

### 写路径 (Set)
```
Client -> memcache Protocol -> StorageClient.Set()
  -> KeyHash = FNV1a(key)<<32 | Murmur3(key)
  -> KeyInfo.Prepare() -> 路由到 BucketID
  -> Bucket.checkAndSet():
       1. TryCompress (QuickLZ)
       2. CalcValueHash
       3. 读取旧值，比较 vhash (CheckVHash)
       4. 版本号递增
       5. dataStore.AppendRecord() -> append 到 dataChunk.wbuf
       6. HTree.set() -> 更新内存索引
       7. hintMgr.set() -> 写入 hint buffer
```

### 读路径 (Get)
```
Client -> memcache Protocol -> StorageClient.Get()
  -> KeyHash -> BucketID -> Bucket.get():
       1. CollisionTable.get() (先查冲突表)
       2. HTree.get() -> 得到 Position(chunkID, offset)
       3. dataChunk.GetRecordByOffset():
          - 先查写缓冲 (wbuf)
          - 未命中则读磁盘文件
       4. Decompress (如果压缩了)
       5. 校验 key 匹配 (处理冲突)
```

### GC 路径
```
HStore.GC() -> GCMgr.gc():
  1. 确定范围 [startChunk, endChunk]
  2. BeforeBucket: dump+merge hint, 删除旧 HTree
  3. 逐 chunk 扫描:
     - DataStreamReader 逐条读取
     - 判断是否最新 (HTree + CollisionTable)
     - 最新记录写入目标 chunk
     - 更新 HTree position
     - 清除源 chunk
  4. AfterBucket: 恢复 hint 状态
```

## 关键设计决策

1. **16 叉哈希树 (HTree)**: 替代 flat hash table，支持按前缀列目录、快速 diff、持久化，适合分布式场景
2. **多 chunk 轮转**: 单 bucket 内多 data file，简化 GC — 只回收旧 chunk，不修改当前活跃 chunk
3. **256 字节对齐**: 简化偏移计算，支持对齐读取
4. **C 内存管理**: 大 value 使用 C malloc，避免 Go GC 扫描压力
5. **CheckVHash**: 写入时比较 value hash，相同则跳过实际写入 (去重优化)
6. **CollisionTable**: 与 HTree 分离的冲突处理，避免修改 HTree 的内部结构
7. **热加载/卸载**: `ChangeRoute()` 支持在线增减 bucket，无需重启
8. **ZK 路由**: 支持 ZooKeeper 集群发现，自动更新路由表

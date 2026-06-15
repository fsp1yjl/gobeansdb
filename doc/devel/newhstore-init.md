# GoBeansDB NewHStore 初始化流程详解

## 概述

`NewHStore()` 是 GoBeansDB 存储引擎的初始化入口，负责从磁盘状态重建完整的内存索引。整个过程分为 5 个阶段：环境准备 → 磁盘扫描 → 路由对齐 → 并行加载 → 全局树创建。

入口: [hstore.go:78](../../store/hstore.go#L78)

---

## 阶段 1: 准备环境

[hstore.go:79](../../store/hstore.go#L79)：

```go
home := Conf.Home
if err := os.MkdirAll(home, os.ModePerm); err != nil {
    logger.Fatalf("fail to init home %s", home)
}
mergeChan = nil
cmem.DBRL.ResetAll()
store = new(HStore)
store.gcMgr = &GCMgr{stat: make(map[*Bucket]*GCState)}
store.buckets = make([]*Bucket, Conf.NumBucket)
for i := 0; i < Conf.NumBucket; i++ {
    store.buckets[i] = &Bucket{}
    store.buckets[i].ID = i
}
```

执行步骤：

| 序号 | 操作 | 说明 |
|---|---|---|
| 1 | `os.MkdirAll(Conf.Home)` | 确保数据根目录存在 |
| 2 | `mergeChan = nil` | 重置 merge 通道 (启动时无 merge 任务) |
| 3 | `cmem.DBRL.ResetAll()` | 重置内存资源计数器 (Alloc/GetData/SetData/FlushData) |
| 4 | `store = new(HStore)` | 分配 HStore 结构体 |
| 5 | `store.gcMgr = &GCMgr{stat: map}` | 初始化 GC 管理器 (空 stat map) |
| 6 | `store.buckets = make([]*Bucket, NumBucket)` | 分配 Bucket 指针数组 |
| 7 | 每个 `buckets[i].ID = i` | 设置 Bucket ID，其余字段为零值 (State=EMPTY) |

此阶段全部为内存操作，无磁盘 IO。

---

## 阶段 2: 扫描磁盘

[hstore.go:32](../../store/hstore.go#L32) `scanBuckets()`：

```go
func (store *HStore) scanBuckets() (err error) {
    for id := 0; id < Conf.NumBucket; id++ {
        path := GetBucketPath(id)
        fi, err := os.Stat(path)
        if err != nil {
            if os.IsNotExist(err) {
                continue  // 目录不存在，保持 EMPTY
            }
            return err
        }
        if !fi.IsDir() {
            return fmt.Errorf("%s is not dir", path)
        }
        datas, err := filepath.Glob(filepath.Join(path, "*.data"))
        if err != nil {
            return err
        }
        if len(datas) == 0 {
            if Conf.NumBucket > 1 {
                logger.Warnf("remove empty bucket dir %s", path)
                os.RemoveAll(path)  // 多 bucket 时删除空目录
            }
        } else {
            logger.Infof("found bucket %x", id)
            store.buckets[id].State = BUCKET_STAT_NOT_EMPTY
        }
    }
    return nil
}
```

对每个 bucket 逐个检查磁盘目录：

```
对 bucket id in [0, NumBucket):
  │
  ├─ os.Stat(GetBucketPath(id))
  │   ├─ 不存在 → 跳过 (保持 State=EMPTY)
  │   ├─ 存在且是目录 ↓
  │   └─ 存在但不是目录 → 返回错误
  │
  ├─ filepath.Glob("*.data")
  │   ├─ 无数据文件 → NumBucket>1 时删除空目录
  │   └─ 有数据文件 → State = BUCKET_STAT_NOT_EMPTY
  └─ 继续下一个 bucket
```

**设计决策**：单 bucket 模式 (NumBucket=1) 保留空目录，因为数据根目录就是 bucket 目录；多 bucket 模式删除空目录，避免残留无用路径。

此阶段有磁盘 IO (Stat + Glob)，但开销较小。

---

## 阶段 3: 路由对齐

[hstore.go:99](../../store/hstore.go#L99)：

```go
for i := 0; i < Conf.NumBucket; i++ {
    need := Conf.BucketsStat[i] > 0
    found := store.buckets[i].State >= BUCKET_STAT_NOT_EMPTY
    if need {
        if !found {
            err = store.allocBucket(i)
            // ...
        }
        store.buckets[i].State = BUCKET_STAT_READY
    } else {
        if found {
            logger.Warnf("found unexpect bucket %d", i)
        }
    }
}
```

对比路由表要求与磁盘现状：

| 路由表 (need) | 磁盘 (found) | 操作 | 结果 State |
|---|---|---|---|
| 本节点负责 | 有数据 | 直接标记可用 | READY |
| 本节点负责 | 无数据 | `allocBucket(i)` 创建目录 | READY |
| 本节点不负责 | 有数据 | 警告 "unexpected bucket" | NOT_EMPTY |
| 本节点不数据 | 无操作 | EMPTY |

**`allocBucket()`** 实现：

```go
func (store *HStore) allocBucket(bucketID int) (err error) {
    dirpath := GetBucketPath(bucketID)
    if _, err = os.Stat(dirpath); err != nil {
        err = os.MkdirAll(dirpath, 0755)
    }
    return
}
```

仅创建目录，不执行数据加载。`BucketsStat` 的值由路由表 (route.yaml 或 ZK) 填充，正数表示本节点负责该 bucket。

---

## 阶段 4: 并行加载 Bucket

[hstore.go:121](../../store/hstore.go#L121) — 整个初始化中最耗时的阶段：

```go
var n int32
var wg = sync.WaitGroup{}
wg.Add(Conf.NumBucket)
errs := make(chan error, Conf.NumBucket)
for i := 0; i < Conf.NumBucket; i++ {
    go func(id int) {
        defer wg.Done()
        bkt := store.buckets[id]
        if Conf.BucketsStat[id] > 0 {
            err = bkt.open(id, GetBucketPath(id))
            if err != nil {
                logger.Errorf("Error in bkt open %s", err.Error())
                errs <- err
            } else {
                atomic.AddInt32(&n, 1)
            }
        }
    }(i)
}
wg.Wait()
close(errs)
for e := range errs {
    if e != nil {
        err = e
        return
    }
}
```

**并发模型**：
- 每个 bucket 独立 goroutine 并行加载
- 任何 bucket 加载失败 → 整体启动失败
- `atomic.AddInt32(&n, 1)` 统计成功加载数量
- 只有 `BucketsStat[id] > 0` 的 bucket 才会执行 open

### Bucket.open() 内部流程

[bucket.go:166](../../store/bucket.go#L166) — 单个 bucket 的完整恢复过程：

```
Bucket.open(bucketID, home):
  │
  ├─ Step 0: 初始化基础实体
  │   ├── bkt.ID = bucketID
  │   ├── bkt.Home = home
  │   ├── bkt.datas = NewdataStore(bucketID, home)    → 扫描 .data 文件大小
  │   ├── bkt.hints = newHintMgr(bucketID, home)      → 创建 hint 管
  │   ├── bkt.hints.loadCollisions()                  → 加载 collision.yaml
  │   ├── htree = newHTree(TreeDepth, bucketID, TreeHeight) → 创建空 16 叉树
  │   └── bkt.TreeID = HintID{0, -1}                  → 快照进度: 尚无快照
  │
  ├─ Step 1: 加载 .idx.hash 快照
  │   ├── datas.ListFiles() → 获取最大 data chunk 编号
  │   ├── getAllIndex("hash") → 获取所有 .idx.hash 文件
  │   ├── 倒序遍历 (从最新开始):
  │   │   ├── id.Chunk > maxdata → 无效快照，删除
  │   │   ├── TreeID < id → 尝试 htree.load(treepath)
  │   │   │   ├── 成功 → TreeID = id
  │   │   │   └── 失败 → 重置为空树, TreeID = {0, -1}
  │   │   └── TreeID >= id → 旧快照，删除
  │   └── bkt.htree = htree
  │
  ├─ Step 2: 从 hint 文件增量恢复
  │   ├── maxDumpedHintID = TreeID
  │   ├── 对每个 chunk i (从 TreeID.Chunk 开始):
  │   │   ├── 第一个 chunk: startsp = TreeID.Split + 1
  │   │   ├── 后续 chunk: startsp = 0
  │   │   ├── checkHintWithData(i)
  │   │   │   ├── data size == 0 → 删除该 chunk 的 hint 文件
  │   │   │   ├── hintDataSize < dataSize → buildHintFromData(i, hintDataSize)
  │   │   │   └── hintDataSize >= dataSize → 无需重建
  │   │   └── 逐 split 文件: updateHtreeFromHint(i, splitPath)
  │   │       ├── Ver > 0 → tree.set(ki, &meta, pos)
  │   │       └── Ver <= 0 → tree.remove(ki, pos)
  │   └── 更新 maxDumpedHintID
  │
  ├─ Step 3: 异步检查快照前 chunk
  │   └── go func() {
  │         for i := 0; i < TreeID.Chunk; i++ {
  │             checkHintWithData(i)    ← 不阻塞启动
  │         }
  │       }()
  │
  ├─ Step 4: 判断是否需要 dump 新快照
  │   ├── checkForDump(TreeDump)
  │   │   ├── 无 .idx.hash 或快照过旧 → dumpHtree()
  │   │   └── 快照足够新 → 跳过
  │   └── dumpHtree() → 写 .tmp → os.Rename → .idx.hash
  │
  └─ Step 5: 加载 GC 历史
      └── loadGCHistory() → 读取 nextgc.txt
```

### 三种重启场景的 open 行为差异

| 场景 | Step 1 (快照) | Step 2 (增量) | Step 3 (异步) | Step 4 (dump) | 耗时 |
|---|---|---|---|---|---|
| **正常关闭** | 加载最新快照 | 少量 hint 增量 | 跳过 (无缺口) | 跳过 (快照足够新) | 快 |
| **首次启动 / 快照损坏** | 空树 (TreeID={0,-1}) | 全量 hint/data 扫描 | 跳过 | 立即 dump | 慢 |
| **异常崩溃** | 可能加载到部分快照 | 增量 + buildHintFromData 兜底 | 检查快照前 chunk | 按需 dump | 中等 |

### 恢复数据源优先级

```
.idx.hash 快照      →  1 (最高)  O(leaf_count) IO，快速恢复大部分索引
.idx.s/.idx.m hint  →  2         增量恢复快照之后的数据
.data 文件          →  3 (兜底)  hint 不足时扫描重建，最慢但最可靠
```

---

## 阶段 5: 创建全局路由树

[hstore.go:144](../../store/hstore.go#L144)：

```go
if Conf.TreeDepth > 0 {
    store.htree = newHTree(0, 0, Conf.TreeDepth+1)
}
logger.Infof("all %d bucket loaded, ready to serve, maxrss = %d, use time %s",
    n, utils.GetMaxRSS(), time.Since(st))
```

仅当 `NumBucket > 1` (即 `TreeDepth > 0`) 时创建全局 HTree。这棵树只用于 `ListUpper()` — 在 bucket 层级之上按前缀列目录。单 bucket 模式不需要全局树。

---

## 完整初始化流程图

```
NewHStore()
  │
  ├── 阶段 1: 准备环境 (纯内存操作)
  │   ├── MkdirAll(Home)
  │   ├── ResetAll() 重置资源计数
  │   ├── new(HStore) + new(GCMgr)
  │   └── make([]*Bucket, NumBucket) + ID 赋值
  │
  ├── 阶段 2: 扫描磁盘 (轻量 IO)
  │   └── scanBuckets() → 检查目录 + Glob("*.data")
  │       → 标记 NOT_EMPTY 或删除空目录
  │
  ├── 阶段 3: 路由对齐 (少量 IO)
  │   ├── 路由表 vs 磁盘现状
  │   ├── allocBucket() 创建缺失目录
  │   └── State → READY
  │
  ├── 阶段 4: 并行加载 (主要耗时)  ◄─────────────── 核心阶段
  │   └── 并行 bkt.open():
  │       ├── NewdataStore + newHintMgr + loadCollisions
  │       ├── 加载 .idx.hash 快照
  │       ├── 增量恢复 hint → HTree
  │       ├── 异步检查快照前 chunk
  │       ├── 按需 dump 新快照
  │       └── loadGCHistory
  │
  ├── 阶段 5: 全局路由树 (条件创建)
  │   └── TreeDepth > 0 → newHTree(0,0,TreeDepth+1)
  │
  └── 返回 HStore 或 error
```

---

## 关键设计要点

### 1. 并行启动

Bucket 之间完全独立，可以并行 open。这在大集群 (NumBucket=256) 时显著加速启动过程。

### 2. 快照 + 增量恢复

避免每次启动都全量扫描 hint/data 文件。`.idx.hash` 一次性恢复到快照时刻的完整 HTree，只需增量补全快照之后的部分。

### 3. data 文件作为最终兜底

当 hint 不可用或不完整时，`buildHintFromData` 直接扫描 data 文件重建 hint。即使 hint 全部丢失，只要 data 文件完好，就能完整恢复。

### 4. 异步检查不阻塞启动

快照覆盖范围之前的 chunk 一致性检查放在独立 goroutine 中，主线程不等待，加速就绪时间。

### 5. 失败快速传播

任何 bucket 的 open 失败都会通过 channel 传递，Wait 后统一检查。一个失败则整体拒绝启动，避免部分可用的不一致状态。

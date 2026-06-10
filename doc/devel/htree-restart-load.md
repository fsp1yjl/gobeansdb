# GoBeansDB HTree 重启加载流程详解

## 概述

GoBeansDB 重启后需要从磁盘重建完整的 HTree 内存索引。由于 HTree 是纯内存结构，进程退出后索引丢失，必须通过磁盘上的持久化文件恢复。

加载策略: **快照 + 增量恢复** — 先加载 `.idx.hash` 快照恢复到某一时刻的状态，再从 hint 文件增量补全快照之后的数据，最后以 data 文件为兜底确保完整性。

---

## 完整加载流程

入口: [bucket.go:166](../../store/bucket.go#L166) `Bucket.open()`

```
Step 0: 初始化基础实体
  |
Step 1: 加载 .idx.hash 快照
  |
Step 2: 从 hint 文件增量恢复
  |
Step 3: 异步检查快照前的 chunk
  |
Step 4: 判断是否需要 dump 新快照
```

---

## Step 0: 初始化基础实体

[bucket.go:166](../../store/bucket.go#L166)：

```go
func (bkt *Bucket) open(bucketID int, home string) (err error) {
    bkt.ID = bucketID
    bkt.Home = home
    bkt.datas = NewdataStore(bucketID, home)    // 扫描 .data 文件大小
    bkt.hints = newHintMgr(bucketID, home)      // 创建 hint 管理器
    bkt.hints.loadCollisions()                  // 加载 collision.yaml
    htree := newHTree(Conf.TreeDepth, bucketID, Conf.TreeHeight)
    bkt.TreeID = HintID{0, -1}                 // 快照进度标记: 尚无快照
```

- `NewdataStore()`: 扫描 Home 目录下的 `.data` 文件，记录每个 chunk 的文件大小
- `newHintMgr()`: 初始化 hint 管理器，每个 chunk 对应一个 hintChunk
- `loadCollisions()`: 从 `collision.yaml` 加载 hash 冲突表
- `newHTree()`: 创建空的 16 叉树，levels 和 leafs 均为零值
- `TreeID = {0, -1}`: 表示 HTree 快照覆盖到的位置（chunk 0, split -1 = 尚无数据）

---

## Step 1: 加载 .idx.hash 快照

[bucket.go:178](../../store/bucket.go#L178)：

```go
maxdata, err := bkt.datas.ListFiles()
htrees, ids := bkt.getAllIndex(HTREE_SUFFIX)
for i := len(htrees) - 1; i >= 0; i-- {
    treepath := htrees[i]
    id := ids[i]
    if id.Chunk > maxdata {
        // 快照指向的 chunk 超出实际数据范围 — 无效快照，删除
        utils.Remove(treepath)
    } else {
        if bkt.TreeID.isLarger(id.Chunk, id.Split) {
            err := htree.load(treepath)
            if err != nil {
                // 加载失败（文件损坏），重置为空树
                bkt.TreeID = HintID{0, -1}
                htree = newHTree(Conf.TreeDepth, bucketID, Conf.TreeHeight)
            } else {
                bkt.TreeID = id  // 更新快照进度
            }
        } else {
            // 旧快照（ID 更小），删除
            utils.Remove(treepath)
        }
    }
}
bkt.htree = htree
```

**选择策略**: 从最新的快照文件开始尝试加载（倒序遍历），成功则停止。

**容错**:
- 快照 chunk 超出数据范围 → 删除（数据已被 GC 清除）
- 快照加载失败 → 放弃，从空树开始恢复
- 旧快照（ID < 当前已加载的） → 删除

### htree.load() 内部流程

[htree.go:107](../../store/htree.go#L107)：

```go
func (tree *HTree) load(path string) (err error) {
    f, err := os.Open(path)
    reader := bufio.NewReader(f)

    // 1. 读取叶子节点摘要 (count + hash, 每叶 6B)
    leafnodes := tree.levels[Conf.TreeHeight-1]
    size := len(leafnodes)
    for i := 0; i < size; i++ {
        reader.ReadFull(buf)
        leafnodes[i].count = binary.LittleEndian.Uint32(buf[0:4])
        leafnodes[i].hash = binary.LittleEndian.Uint16(buf[4:6])
    }

    // 2. 读取叶子数据 (size + raw bytes)
    for i := 0; i < size; i++ {
        reader.ReadFull(buf[:4])
        l := int(binary.LittleEndian.Uint32(buf[:4]))
        if l > 0 {
            tree.leafs[i].enlarge(l)      // C.malloc 分配内存
            reader.ReadFull(tree.leafs[i].ToBytes())  // 读取原始数据
        }
    }

    tree.ListTop()  // 重建内部节点的 count 和 hash
    return nil
}
```

**关键点**:
- `.idx.hash` 只存储叶子节点的 count + hash + 原始数据
- 内部节点不持久化，加载后通过 `ListTop()` 从叶子向上递推重建
- `enlarge()` 使用 C.malloc 分配叶子内存，与运行时分配方式一致
- 快照覆盖范围: 从 chunk 0 到 `TreeID.Chunk` 的 `TreeID.Split` 分片

---

## Step 2: 从 hint 文件增量恢复

[bucket.go:206](../../store/bucket.go#L206)：

```go
bkt.hints.maxDumpedHintID = bkt.TreeID
for i := bkt.TreeID.Chunk; i < MAX_NUM_CHUNK; i++ {
    startsp := 0
    if i == bkt.TreeID.Chunk {
        startsp = bkt.TreeID.Split + 1  // 从快照之后开始
    }
    e := bkt.checkHintWithData(i)       // 检查 hint/data 一致性
    ...
    splits := bkt.hints.chunks[i].splits
    numhintfile := len(splits) - 1
    if startsp >= numhintfile {
        continue
    }
    for j, sp := range splits[:numhintfile] {
        bkt.updateHtreeFromHint(i, sp.file.path)  // 从 hint 恢复 HTree
        bkt.hints.maxDumpedHintID = HintID{i, startsp + j}
    }
}
```

**遍历逻辑**:
- 从 `TreeID.Chunk` 开始（快照覆盖到的 chunk）
- 第一个 chunk 内从 `TreeID.Split + 1` 分片开始（跳过快照已覆盖的部分）
- 后续 chunk 从分片 0 开始
- 对每个 chunk，先检查 hint/data 一致性，再逐 split 文件恢复

### checkHintWithData — hint/data 一致性检查

[bucket.go:153](../../store/bucket.go#L153)：

```go
func (bkt *Bucket) checkHintWithData(chunkID int) (err error) {
    size := bkt.datas.chunks[chunkID].size
    if size == 0 {
        bkt.hints.RemoveHintfilesByChunk(chunkID)  // 无数据则删除对应 hint
        return
    }
    hintDataSize := bkt.hints.loadHintsByChunk(chunkID)  // 加载 hint 文件
    if hintDataSize < size {
        // hint 覆盖不足 — 从 data 重建 hint
        err = bkt.buildHintFromData(chunkID, hintDataSize)
    }
    return
}
```

当 hint 文件覆盖的数据范围小于实际 data 文件大小时（可能因为上次 hint dump 不完整），需要从 data 文件重建 hint。

### buildHintFromData — 从 data 重建 hint

[bucket.go:89](../../store/bucket.go#L89)：

```go
func (bkt *Bucket) buildHintFromData(chunkID int, start uint32) (err error) {
    r, _ := bkt.datas.GetStreamReader(chunkID)
    r.seek(start)          // 从 hint 覆盖不到的位置开始
    defer r.Close()
    for {
        rec, offset, _, e := r.Next()
        if rec == nil { break }
        khash := getKeyHash(rec.Key)
        p := rec.Payload
        p.Decompress()
        vhash := Getvhash(p.Body)
        p.Free()
        item := newHintItem(khash, p.Ver, vhash, Position{0, offset}, string(rec.Key))
        bkt.hints.setItem(item, chunkID, rec.Payload.RecSize)
    }
    bkt.hints.trydump(chunkID, true)  // 重建后立即 dump hint
    return
}
```

这是 **data 文件作为兜底** 的体现 — 当 hint 不完整时，直接扫描 data 文件重建。

### updateHtreeFromHint — 从 hint 恢复 HTree

[bucket.go:119](../../store/bucket.go#L119)：

```go
func (bkt *Bucket) updateHtreeFromHint(chunkID int, path string) (maxoffset uint32, err error) {
    tree := bkt.htree
    r := newHintFileReader(path, chunkID, 1<<20)
    r.open()
    defer r.close()
    for {
        item, e := r.next()
        if item == nil { return }
        ki := NewKeyInfoFromBytes([]byte(item.Key), item.Keyhash, false)
        ki.Prepare()
        meta.ValueHash = item.Vhash
        meta.Ver = item.Ver
        pos.Offset = item.Pos.Offset
        if item.Ver > 0 {
            pos.ChunkID = chunkID
            tree.set(ki, &meta, pos)       // 有效记录: set
        } else {
            pos.ChunkID = -1
            tree.remove(ki, pos)            // 已删除记录: remove
        }
    }
}
```

**Ver 驱动的 set/remove**:
- `Ver > 0`: 正常记录，调用 `tree.set()` 插入/更新 HTree
- `Ver <= 0`: tombstone 记录，调用 `tree.remove()` 从 HTree 删除

这保证了恢复后的 HTree 与运行时状态一致：已删除的 key 不会出现在索引中。

---

## Step 3: 异步检查快照前的 chunk

[bucket.go:231](../../store/bucket.go#L231)：

```go
go func() {
    for i := 0; i < bkt.TreeID.Chunk; i++ {
        bkt.checkHintWithData(i)
    }
}()
```

快照覆盖范围之前的 chunk 已经在 `.idx.hash` 中有索引，但可能存在 hint/data 不一致的情况。这段检查放在 goroutine 中异步执行，不阻塞启动。

---

## Step 4: 判断是否需要 dump 新快照

[bucket.go:237](../../store/bucket.go#L237)：

```go
if bkt.checkForDump(Conf.TreeDump) {
    bkt.dumpHtree()
}
```

[bucket.go:255](../../store/bucket.go#L255)：

```go
func (bkt *Bucket) checkForDump(dumpthreshold int) bool {
    maxdata, _ := bkt.datas.ListFiles()
    htrees, ids := bkt.getAllIndex(HTREE_SUFFIX)
    for i := len(htrees) - 1; i >= 0; i-- {
        id := ids[i]
        if maxdata > id.Chunk+dumpthreshold {
            return false  // 现有快照足够新，不需要 dump
        }
    }
    if len(ids) > 0 {
        return false  // 已有快照
    }
    return true  // 无快照或快照太旧，需要 dump
}
```

如果当前没有任何 `.idx.hash` 文件，或者所有数据 chunk 都没有快照覆盖，则在启动恢复完成后立即 dump 一个新快照，避免下次重启需要全量恢复。

---

## 三种重启场景

### 场景 A: 正常关闭后重启

```
磁盘状态: .idx.hash (完整) + .idx.s/.idx.m (完整) + .data (完整)

Step 1: 加载 .idx.hash → HTree 恢复到快照时刻
Step 2: 从 TreeID 之后的 hint 分片增量恢复
        → 恢复到关闭前的完整状态
Step 3: 异步检查快照前 chunk 的一致性
Step 4: 快照存在且较新，跳过 dump

启动耗时: 快（主要耗时在 IO 读 .idx.hash + 少量 hint 分片）
```

### 场景 B: 首次启动 / .idx.hash 损坏

```
磁盘状态: 无 .idx.hash (或损坏)，仅有 .data + 可能的 .idx.s/.idx.m

Step 1: getAllIndex 找不到 .idx.hash (或 load 失败)
        → TreeID = {0, -1}，HTree 为空树
Step 2: 从 chunk 0 开始，逐 chunk 检查 hint/data 一致性
        - 有 hint: 加载 hint → updateHtreeFromHint
        - hint 不足: buildHintFromData 从 data 重建
        - 无 hint: 全量扫描 data 重建 hint + HTree
Step 3: 无快照前 chunk (TreeID.Chunk = 0)，跳过
Step 4: 无快照，立即 dumpHtree() 创建快照

启动耗时: 慢（需要扫描所有 data/hint 文件）
```

### 场景 C: 异常崩溃后重启

```
磁盘状态: .idx.hash (可能不完整) + .idx.s (部分) + .data (完整)
         写缓冲中尚未刷盘的数据可能丢失

Step 1: 加载最新的 .idx.hash (如果存在且有效)
Step 2: 增量恢复，checkHintWithData 检查 hint 覆盖范围
        - 如果 hint 覆盖不足: buildHintFromData 从 data 重建
        - data 文件是最终兜底，保证至少能恢复已刷盘的数据
Step 3: 异步检查快照前 chunk
Step 4: 按需 dump

启动耗时: 中等 (取决于 hint 覆盖范围与 data 缺口大小)

注意: 未刷盘的写缓冲数据可能丢失 (append-only 模型下的可接受折衷)
```

---

## 恢复优先级总结

```
恢复数据源          优先级    说明
─────────────────────────────────────────────────────
.idx.hash 快照      1 (最高)  快速恢复大部分索引，O(leaf_count) IO
.idx.s/.idx.m hint  2         增量恢复快照之后的数据
.data 文件          3 (兜底)  hint 不足时扫描重建，最慢但最可靠
```

**设计哲学**: hint 是 data 的"目录"，加载更快；data 是"真相"，保证完整性。快照是 hint 的压缩形式，进一步加速启动。

---

## 关键设计要点

### 1. 快照 + 增量恢复

避免每次启动都全量扫描 hint/data 文件。`.idx.hash` 一次性恢复到快照时刻的完整 HTree，只需增量补全快照之后的部分。

### 2. hint 作为 data 的目录

hint 文件记录了每条记录的 KeyHash + Position + Ver + VHash，相当于 data 文件的索引。从 hint 恢复 HTree 只需读取 hint（比扫描 data 更快，因为 hint 更紧凑）。

### 3. data 文件作为最终兜底

当 hint 不可用或不完整时，`buildHintFromData` 直接扫描 data 文件重建 hint。这保证了即使 hint 全部丢失，只要 data 文件完好，就能完整恢复。

### 4. Ver 驱动的 set/remove

恢复时根据 `Ver` 正负决定是 `tree.set()` 还是 `tree.remove()`。这与运行时的写入逻辑一致，保证恢复后 HTree 不会包含已删除的 key。

### 5. 并行启动

快照前 chunk 的一致性检查放在独立 goroutine 中异步执行，主线程不阻塞，加速启动过程。

### 6. 内部节点懒恢复

`.idx.hash` 只存储叶子节点数据，内部节点的 count 和 hash 通过 `ListTop()` 从叶子向上递推重建。这减少了快照文件大小，也简化了持久化逻辑。

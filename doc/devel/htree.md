# GoBeansDB HTree 哈希树详解

## 什么是哈希树 (HTree)

HTree 是 GoBeansDB 中的 **核心内存索引结构**，替代了 Bitcask 原论文中的 flat hash table。它是一棵 **16 叉树**，用 key 的 64-bit Hash 的每 4 bit (一个 hex digit) 作为一层的路由键，逐层向下定位到叶子节点。

与普通哈希表的区别：

| | Flat Hash Table | HTree (GoBeansDB) |
|---|---|---|
| 结构 | 单层 map | 多层 16 叉树 |
| 查找 | O(1) 哈希取模 | O(depth) 逐层路由 |
| 空间 | 整体扩缩容 | 叶子独立 realloc |
| 列目录 | 不支持 | 按前缀浏览子树 |
| 快速 diff | 不支持 | Node.hash 子树摘要 |
| 持久化 | 整体序列化 | 叶子独立序列化 |

---

## 工作原理详解

### 1. KeyHash 生成与路由

```
key = "test_key"

KeyHash = (FNV1a("test_key") << 32) | Murmur3("test_key")
         +--------高32位---------++--------低32位---------+
         0x A3B4 C5D6           E7F8 091A

KeyPath = KeyHash 的每 4 bit 拆成一个 hex digit (0-f):
         [A, 3, B, 4, C, 5, D, 6, E, 7, F, 8, 0, 9, 1, A]
          0  1  2  3  4  5  6  7  8  9  10 11 12 13 14 15  <- index
```

**路由规则**: KeyPath 中的每个元素决定树的一层往哪个子节点走。

### 2. 两段式路由 — HStore 层 + Bucket 层

```
                HStore 总树 (depth=0 起)
              /   |   ... |   \
            h0   h1   ... hf      <- TreeDepth 层 (用于 BucketID)
            |
          h0*16+h3                 <- TreeDepth+1 层
            |
           ...
            |
       -- Bucket 边界 --
            |
          HTree (depth=TreeDepth 起, Bucket 独立管理)
            |
           ...
            |
          叶子节点                 <- TreeDepth+TreeHeight-1 层
```

- **前 TreeDepth 层**: 决定 key 路由到哪个 Bucket（由 HStore 管理）
- **后 TreeHeight 层**: 在 Bucket 内定位到叶子节点（由 Bucket 的 HTree 管理）

示例 (NumBucket=16, TreeDepth=1, TreeHeight=3):

```
HStore 总树:
          Root (level 0)
       /   |   ... |   \
      0    1   ...  f        <- 第 1 层 = BucketID (Depth=1)
      |                    <- 此处切分为独立 Bucket
     HTree (Bucket 内, 3 层):
      Root (level 0 of HTree, = level 1 of 总树)
    / | ... | \
  16 子节点       <- level 1
  / | \
 ...   叶子       <- level 2 (最底层)
```

### 3. 逐层路由的查找过程

[htree.go](../../store/htree.go#L236)：

```go
func (tree *HTree) getLeaf(ki *KeyInfo, ni *NodeInfo) {
    ni.level = len(tree.levels) - 1   // 目标: 最底层
    ni.offset = 0
    path := ki.KeyPath[tree.depth:]   // 取 Bucket 内的路由路径
    for level := 1; level < len(tree.levels); level += 1 {
        ni.offset = ni.offset*16 + path[level-1]
        // 每层: offset = 父offset * 16 + 本层 hex digit
    }
    ni.node = &tree.levels[ni.level][ni.offset]
}
```

具体计算 (TreeDepth=2, TreeHeight=3, KeyPath=[A,3,B,4,...]):

```
tree.depth = 2, path = KeyPath[2:] = [B, 4, ...]

level 1: offset = 0*16 + B(11) = 11
level 2: offset = 11*16 + 4 = 180

最终: leafs[180] 就是目标叶子
```

**数组存储，不用指针**: `levels[i]` 是一个连续的 `[]Node` 数组，用 `offset` 计算下标直接访问，避免指针追踪，缓存友好。

### 4. 叶子节点 — 数据存储

叶子是 **C malloc 的连续内存块**，紧凑存储 HTreeItem：

[leaf.go](../../store/leaf.go#L30)：

```go
type SliceHeader struct {
    Data uintptr   // C.malloc 地址
    Len  int       // 字节数
}
```

**内存布局**:

```
+-------------------+---------+---------+----+---------+----+---+
| KeyHash (5-8B)    | Ver(4B) |VHash(2B)| Offset 3B   |ChID| ...|
+-------------------+---------+---------+----+---------+----+---+
     <- 1 item ->     <- TREE_ITEM_HEAD_SIZE = 11B ->
     <- 1 item ->
     ...
```

KeyHash 长度由树深度决定 ([config.go](../../store/config.go#L12)):

```go
KHASH_LENS = [8, 8, 7, 7, 6, 6, 5, 5]
// depth+height-1: 1  2  3  4  5  6  7  8+
// KeyHashLen:     8  8  7  7  6  6  5  5
```

更深的树需要更少的 KeyHash 字节来区分叶子内的 item，每条 item 更紧凑。

### 5. 叶子内的查找

[leaf.go](../../store/leaf.go#L81)：

```go
func findInBytes(leaf []byte, keyhash uint64) int {
    lenKHash := Conf.TreeKeyHashLen
    lenItem := lenKHash + TREE_ITEM_HEAD_SIZE
    n := len(leaf) / lenItem

    if n < 100 {
        // Go 线性搜索 (小叶子)
        for i := 0; i < size; i += lenItem {
            if bytes.Compare(leaf[i:i+lenKHash], kb) == 0 {
                return i
            }
        }
    } else {
        // C.memcmp 搜索 (大叶子, 性能更好)
        i := int(C.find(...))
        return i * lenItem
    }
    return -1
}
```

- 叶子内 item **不排序**，线性搜索
- 小叶子 (< 100 items) 用 Go，大叶子用 C 的 `memcmp` 批量比较
- 因为叶子内 item 数量有限 (由树高度控制)，线性搜索仍然很快

### 6. 内部节点 — 摘要与计数

[htree.go](../../store/htree.go#L61)：

```go
type Node struct {
    count        uint32   // 有效 key 数 (Ver > 0)
    hash         uint16   // 子树摘要
    isHashUpdated bool     // 是否需要重新计算
}
```

**Node.hash 的计算** ([htree.go](../../store/htree.go#L338)):

```go
func (tree *HTree) updateNodes(level, offset int) (node *Node) {
    node = &tree.levels[level][offset]
    if node.isHashUpdated {
        return  // 懒更新: 没变就不重算
    }
    node.count = 0
    var hashs [16]uint16
    for i := 0; i < 16; i++ {
        cnode := tree.updateNodes(level+1, offset*16+i)
        node.count += cnode.count
        hashs[i] = cnode.hash
    }
    node.hash = 0
    for i := 0; i < 16; i++ {
        if node.count > ThresholdBigHash {  // > 256
            node.hash *= 97                 // 大子树: 加权混合
        }
        node.hash += hashs[i]
    }
    node.isHashUpdated = true
    return
}
```

**hash 摘要的用途**:

1. **快速 diff**: 比较两个节点 hash 是否相同，快速判断子树是否一致 (用于分布式同步)
2. **列目录**: `ListDir()` 返回子节点的 count 和 hash，支持按前缀浏览
3. **懒更新**: 只在需要时重算 (`isHashUpdated=false` 时触发)，减少写操作的开销

### 7. 写入 (Set) 操作

[htree.go](../../store/htree.go#L287)：

```go
func (tree *HTree) set(ki *KeyInfo, meta *Meta, pos Position) {
    req := HTreeReq{ki, *meta, pos, HTreeItem{ki.KeyHash, pos, meta.Ver, meta.ValueHash}}
    tree.setReq(&req)
}

func (tree *HTree) setReq(req *HTreeReq) {
    tree.Lock()
    defer tree.Unlock()
    // 1. 定位叶子 + 标记路径上 Node 为需要更新
    tree.getLeafAndInvalidNodes(req.ki, &tree.ni)
    // 2. 在叶子中 Set (C 内存操作)
    tree.setToLeaf(&tree.ni, req)
}
```

[leaf.go](../../store/leaf.go#L120)：

```go
func (sh *SliceHeader) Set(req *HTreeReq) (oldm HTreeItem, exist bool) {
    leaf := sh.ToBytes()
    idx := findInBytes(leaf, req.ki.KeyHash)
    exist = (idx >= 0)
    if exist {
        // 覆盖旧 item
        bytesToItem(leaf[idx+lenKHash:], &oldm)
        dst = leaf[idx:]
    } else {
        // C.realloc 扩大叶子, 追加新 item
        sh.enlarge(newSize)
        dst = sh.ToBytes()[len(leaf):]
    }
    khashToBytes(dst, req.ki.KeyHash)
    itemToBytes(dst[lenKHash:], &req.item)
    return
}
```

关键点: Set 只修改叶子数据 + 标记 Node 需要更新，**不立即重算 Node.hash**。hash 在下次读取时懒更新。

### 8. 删除 (Remove) 操作

[htree.go](../../store/htree.go#L305)：

```go
func (tree *HTree) remove(ki *KeyInfo, oldPos Position) {
    tree.Lock()
    defer tree.Unlock()
    tree.getLeafAndInvalidNodes(ki, &tree.ni)
    tree.remvoeFromLeaf(&tree.ni, ki, oldPos)
}
```

[leaf.go](../../store/leaf.go#L139)：

```go
func (sh *SliceHeader) Remove(ki *KeyInfo, oldPos Position) (oldm HTreeItem, removed bool) {
    leaf := sh.ToBytes()
    idx := findInBytes(leaf, ki.KeyHash)
    if idx >= 0 {
        bytesToItem(leaf[idx+lenKHash:], &oldm)
        if oldPos.ChunkID == -1 || oldm.Pos.Offset == oldPos.Offset {
            removed = true
            copy(leaf[idx:], leaf[idx+itemLen:])  // 内存移动, 覆盖被删 item
            sh.Len -= itemLen                      // 缩小长度 (不 realloc 缩容)
        }
    }
    return
}
```

删除是原地移动覆盖，缩小 Len 但 **不释放内存** (C realloc 缩容代价高)。

### 9. 持久化 (dump/load)

[htree.go](../../store/htree.go#L146)：

**dump 文件格式** (`NNN.NNN.idx.hash`):

```
+----------------------------+  <- 0x00
| Leaf Nodes Summary         |
| count(4B) | hash(2B)       |  <- 每个 leaf node 6 字节
| ...                        |
+----------------------------+  <- size * 6
| Leaf Sizes                 |
| len(4B)                    |  <- 每个叶子的字节长度
| ...                        |
+----------------------------+
| Leaf Data                   |
| [leaf0 raw bytes]          |  <- 紧跟 leaf size 后的叶子原始数据
| [leaf1 raw bytes]          |
| ...                        |
+----------------------------+
```

- 内部 Node 只存 count + hash (6B/个)
- 叶子存 size + raw data
- 写入先 `.tmp` 再 `os.Rename`，原子操作

**load**: 逆序恢复 — 读 leaf summary → 读 leaf size → 读 leaf data → `C.malloc` + `memcpy` 恢复叶子内存

### 10. 列目录 (ListDir)

[htree.go](../../store/htree.go#L386)：

```go
func (tree *HTree) listDir(ki *KeyInfo) (items []HTreeItem, nodes []*Node) {
    // 定位到目标节点
    if ni.level >= len(tree.levels)-1 || node.count < thresholdListKey {
        // 叶子层或小子树: 返回所有 item (按 KeyHash 过滤前缀)
        items = tree.collectItems(&ni, items, ki.KeyHash, filtermask)
    } else {
        // 中间层: 返回 16 个子 Node (count + hash)
        nodes = make([]*Node, 16)
        for i := 0; i < 16; i++ {
            nodes[i] = &tree.levels[ni.level+1][ni.offset*16+i]
        }
    }
}
```

这是 HTree 相对于 flat hash table 的独有能力 — 按前缀层级浏览:

```
GET @a         -> 列出前缀 "a" 下的 16 个子节点 (hash + count)
GET @a3        -> 列出前缀 "a3" 下的 16 个子节点
GET @a3b       -> 列出前缀 "a3b" 下的所有 key item
```

---

## 完整工作流程总结

```
Set("test_key", value):
  KeyHash -> KeyPath -> 逐层路由 -> 定位 leafs[offset]
  -> findInBytes(KeyHash) -> 找到旧 item / realloc 追加
  -> 写入 HTreeItem{KeyHash, Pos, Ver, VHash}
  -> 标记路径上 Node.isHashUpdated = false (懒更新)

Get("test_key"):
  KeyHash -> KeyPath -> 逐层路由 -> 定位 leafs[offset]
  -> findInBytes(KeyHash) -> 读出 HTreeItem -> 得到 Position
  -> 用 Position 从 data file 读取 value

ListDir("a3"):
  KeyPath = [A, 3] -> 路由到 Node(levels[depth+2])
  -> updateNodes() 计算子节点 hash
  -> 返回 16 个子 Node 的 {count, hash}  (中间层)
  -> 或返回过滤后的 HTreeItem[] (叶子层)

dump():
  遍历所有 leaf node -> 写 count+hash -> 写 leaf size -> 写 leaf raw data
  -> 写 .tmp -> os.Rename -> .idx.hash (原子)
```

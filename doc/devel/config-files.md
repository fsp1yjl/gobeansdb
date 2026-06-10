# GoBeansDB 配置文件详解

## 配置文件总览

GoBeansDB 有三个 YAML 配置文件，分为全局配置和本地覆盖两层：

| 文件 | 作用 | 优先级 |
|---|---|---|
| `route.yaml` | 路由表，定义 bucket 数量与节点分配 | 基础 |
| `global.yaml` | 全局服务与存储参数 | 基础 |
| `local.yaml` | 本地覆盖，覆盖 global.yaml 中的部分参数 | 覆盖 |

单节点部署时配置位于 `.single-node/conf/`，生产环境位于 `conf/`。

---

## route.yaml — 路由表配置

```yaml
numbucket: 16
backup:
- "backup-host:7901"
main:
- addr: main-host:7900
  buckets: ["0", "1", "2", "3", "4", "5", "6", "7", "8", "9", a, b, c, d, e, f]
```

| 参数 | 类型 | 说明 |
|---|---|---|
| `numbucket` | int | **bucket 总数**，决定数据分片数量和哈希树深度。只能取 **1、16 或 256** |
| `main` | []Server | 主节点列表，每个节点含 `addr`（地址）和 `buckets`（十六进制 bucket ID） |
| `backup` | []string | 备份节点地址列表，备份节点默认承载所有 bucket |

**NumBucket 约束**：代码中 `GetBucketDir()` 只处理 1/16/256 三种值，其他值会 panic。

**Bucket 分配**：`buckets` 字段用十六进制字符串表示，如 `["0", "a", "f"]` 表示该节点负责 bucket 0、10、15。不同主节点可分担不同 bucket 子集。

---

## global.yaml — 全局服务与存储配置

### server 分区

```yaml
server:
  zkserves: ["zk1:2181,zk2:2181"]
  zkpath: "/beansdb/test"
  listen: 0.0.0.0
  port: 7900
  webport: 7903
  data_http_listen: 0.0.0.0
  data_http_port: 0
  data_http_auth_token: ""
  data_http_access_log: ""
  threads: 4
  errorlog: /var/log/gobeansdb/gobeansdb.log
  accesslog: ""
  analysislog: /var/log/gobeansdb/gobeansdb_analysis.log
  hostname: 127.0.0.1
  staticdir: /var/lib/gobeansdb
```

| 参数 | 默认值 | 说明 |
|---|---|---|
| `zkserves` | `[]` | ZooKeeper 服务器列表，为空时从本地 route.yaml 加载路由 |
| `zkpath` | `""` | ZooKeeper 根路径，如 `/beansdb/test` |
| `listen` | `"0.0.0.0"` | 监听 IP |
| `port` | `7900` | memcache 协议端口 |
| `webport` | `7903` | Web 管理端口 |
| `data_http_listen` | `"0.0.0.0"` | HTTP 数据接口监听 IP |
| `data_http_port` | `0` | HTTP 数据接口端口（0 = 关闭） |
| `data_http_auth_token` | `""` | HTTP 数据接口认证 token |
| `data_http_access_log` | `""` | HTTP 数据接口访问日志路径 |
| `threads` | `4` | 工作线程数 |
| `errorlog` | `"./gobeansdb.log"` | 错误日志路径 |
| `accesslog` | `""` | 访问日志路径 |
| `analysislog` | `""` | 分析日志路径 |
| `hostname` | `"127.0.0.1"` | 本机主机名（线上必须在 local.yaml 中修改） |
| `staticdir` | `"./"` | 静态文件目录（data 文件存储根路径） |

### mc 分区 — memcache 协议参数

```yaml
mc:
  max_key_len: 250
  max_req: 16
  body_max_str: 50M
  body_big_str: 5M
  body_c_str: 4K
  flush_max_str: 100M
```

| 参数 | 默认值 | 说明 |
|---|---|---|
| `max_key_len` | `250` | key 最大长度（字节） |
| `max_req` | `16` | 最大并发请求数 |
| `body_max_str` | `"50M"` | value 最大尺寸，超过则拒绝 set |
| `body_big_str` | `"1M"` | value 大于此值时，内存紧张可能拒绝 set |
| `body_c_str` | `"4K"` | value 超过此值时使用 C 内存分配（cgo），避免 Go GC 压力 |
| `flush_max_str` | `"100M"` | flush 缓冲区大小上限，超出可能拒绝大 value 的 set |
| `timeout_ms` | `3000` | 请求超时（毫秒） |

### hstore.data 分区 — 数据文件参数

```yaml
hstore:
  data:
    flush_interval: 60
    flush_wake_str: 10M
    datafile_max_str: 4000M
    check_vhash: true
    no_gc_days: 7
    not_compress:
      "audio/mpeg": true
      "audio/wave": true
```

| 参数 | 默认值 | 说明 |
|---|---|---|
| `flush_interval` | `0` | 刷盘间隔（秒），0 = 立即刷盘（测试用） |
| `flush_wake_str` | `"0"` | 写缓冲超过此大小后唤醒刷盘协程 |
| `datafile_max_str` | `"4000M"` | 单个 data chunk 文件最大尺寸，达到后切换新 chunk |
| `check_vhash` | `false` | 是否检查 value hash，相同则跳过写入（去重优化） |
| `no_gc_days` | `0` | 最近 N 天内的 data chunk 不参与 GC（0 = 无保护） |
| `not_compress` | `{}` | 不压缩的 content-type 映射，键为 MIME 类型 |

### hstore.hint 分区 — Hint 索引参数

```yaml
    hint:
      hint_no_merged: true
      hint_split_cap_str: 1M
      hint_index_interval_str: 32K
      hint_merge_interval: 5
```

| 参数 | 默认值 | 说明 |
|---|---|---|
| `hint_no_merged` | `false` | 是否跳过 hint merge。`true` = 只用于碰撞检测，不写 merged hint 文件，省磁盘 |
| `hint_split_cap_str` | `"1M"` | 每个 hint split 的容量上限，满时 dump 到磁盘 |
| `hint_index_interval_str` | `"4K"` | hint 稀疏索引的间隔大小，用于加速查找 |
| `hint_merge_interval` | `1` | 每隔多少个 chunk 做一次 hint merge |

### hstore.htree 分区 — 哈希树参数

```yaml
    htree:
      tree_height: 7
      tree_dump: 3
```

| 参数 | 默认值 | 说明 |
|---|---|---|
| `tree_height` | `3` | **bucket 内哈希树高度**，直接影响内存占用与查找性能 |
| `tree_dump` | `3` | htree dump 阈值，每隔多少个 data chunk 做一次持久化快照 |

### hstore.local 分区 — 本地存储路径

```yaml
    local:
      homes:
      - /var/lib/beansdb
```

| 参数 | 默认值 | 说明 |
|---|---|---|
| `homes` | `"./testdb"` | 数据存储目录列表 |

---

## local.yaml — 本地覆盖配置

结构与 global.yaml 相同，但只包含需要覆盖的字段。单机部署时用于覆盖端口、日志路径、数据目录等。加载时 global.yaml 先加载，local.yaml 的字段覆盖 global.yaml 中的同名项。

---

## Bucket 个数与哈希树高度的核心关系

### 整体结构

```
整体哈希树 = 路由层 (TreeDepth) + Bucket 内树层 (TreeHeight)
总深度 = TreeDepth + TreeHeight ≤ 8 (MAX_DEPTH)
```

### TreeDepth 由 NumBucket 自动推导

```go
// store/config.go
n := c.NumBucket
c.TreeDepth = 0
for n > 1 {
    c.TreeDepth += 1
    n /= 16
}
```

| NumBucket | TreeDepth | 含义 |
|---|---|---|
| 1 | 0 | 无路由层，所有 key 在一棵树里 |
| 16 | 1 | 路由层 1 级（0~f），key 按 hash 首位分到 16 个 bucket |
| 256 | 2 | 路由层 2 级（00~ff），key 按 hash 前两位分到 256 个 bucket |

### 典型配置

| 场景 | NumBucket | TreeHeight | 总深度 | TreeKeyHashLen | 单 Bucket 内 leaf 数 |
|---|---|---|---|---|---|
| 单节点测试 | 16 | 3 | 4 | 7 | 16^2 = 256 |
| 线上生产 | 16 | 7 | 8 | 5 | 16^6 = 16M |
| 大集群 | 256 | 6 | 8 | 5 | 16^5 = 1M |

### 约束

- **`TreeDepth + TreeHeight ≤ 8`**（MAX_DEPTH），超出会 panic
- **`NumBucket` 只能取 1/16/256**，因为每层是 16 叉树
- 减小 `tree_height` 会降低单 bucket 内存占用，但 leaf 变少导致每个 leaf 容纳更多 key，list 性能下降

---

## KHASH_LENS 设计原理

```go
KHASH_LENS = [8, 8, 7, 7, 6, 6, 5, 5]
// 索引:     总深度 1  2  3  4  5  6  7  8
```

### 叶子节点的 item 内存布局

```
| KeyHash (N 字节) | Ver(4B) | VHash(2B) | Offset(3B) | ChunkID(2B) |
                   |<------- TREE_ITEM_HEAD_SIZE = 11B ------->|
```

`TreeKeyHashLen` 决定 KeyHash 存几字节。**每减 1 字节，每条 item 省 1 字节**，百万级 key 时内存节省可观。

### 为什么每 2 层减 1 字节

16 叉树的每一层消耗 KeyHash 的 **4 bit**（1 个 hex digit）做路由：

| 总深度 | 路由消耗 bit 数 | 剩余区分 bit 数 | 向上取整字节数 = KeyHashLen |
|---|---|---|---|
| 1 | 4 | 60 | 8 |
| 2 | 8 = 1 字节 | 56 | 8 |
| 3 | 12 | 52 | 7 |
| 4 | 16 = 2 字节 | 48 | 7 |
| 5 | 20 | 44 | 6 |
| 6 | 24 = 3 字节 | 40 | 6 |
| 7 | 28 | 36 | 5 |
| 8 | 32 = 4 字节 | 32 | 5 |

规律：每 2 层消耗 8 bit = 1 字节，所以每 2 深度 KeyHashLen 减 1。

### FNV + Murmur 双哈希的保障

```
KeyHash = (FNV1a(key) << 32) | Murmur3(key)
         +--- 高 32 位 (FNV) ---+ +--- 低 32 位 (Murmur) ---+
```

树路由从高位（FNV 部分）开始消耗。当总深度到 8 时，FNV 全部 32 bit 被路由用掉，但 Murmur 的 32 bit 完整保留在低位。`KHASH_LENS[7]=5` 保留低 40 bit（Murmur 32 bit + 低 8 bit FNV），确保叶子内仍有足够区分度。

**关键设计**：路由消耗 FNV 的位，叶子内靠 Murmur 区分，二者互不干扰。这就是敢递减到 5 字节的原因。

### TreeKeyHashMask 的计算

```go
// store/config.go
c.TreeKeyHashLen = KHASH_LENS[c.TreeDepth+c.TreeHeight-1]
shift := 64 - uint32(c.TreeKeyHashLen)*8
c.TreeKeyHashMask = (uint64(0xffffffffffffffff) << shift) >> shift
```

`TreeKeyHashMask` 是低位掩码，用于在叶子内存储和比较截断后的 KeyHash。存储时只保留低 `TreeKeyHashLen` 字节，查找时用 mask 截断传入的完整 KeyHash 后做匹配。

---

## 配置加载流程

1. 加载 `global.yaml` → 填充默认值
2. 加载 `local.yaml` → 覆盖 global 中的同名项
3. 加载 `route.yaml`（本地文件或 ZooKeeper）→ 设置 NumBucket 和 BucketsStat
4. 调用 `HStoreConfig.InitTree()` → 根据 NumBucket 推导 TreeDepth，再结合 TreeHeight 计算 TreeKeyHashLen 和 TreeKeyHashMask
5. 初始化每个 Bucket 的 HTree：`newHTree(Conf.TreeDepth, bucketID, Conf.TreeHeight)`

---

## 配置修改注意事项

| 参数 | 可否在线修改 | 说明 |
|---|---|---|
| `numbucket` | 否 | 需重启，影响整体数据分布 |
| `tree_height` | 否 | 需重启并重建 htree |
| `tree_dump` | 可调整 | 控制持久化频率 |
| `flush_interval` | 可调整 | 控制刷盘频率 |
| `no_gc_days` | 可调整 | 控制 GC 保护期 |
| `buckets` (route) | 支持 hot load/unload | 通过 `ChangeRoute()` 动态加载/卸载 bucket |
| `port` / `webport` | 否 | 需重启 |

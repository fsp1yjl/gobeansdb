# GoBeansDB HTTP PUT 写入处理流程分析

本文以如下请求为例，追踪从 HTTP 接收直到数据持久化的完整处理链路：

```
curl -i -X PUT --data-binary 'hello-from-host' \
  "http://192.168.220.128:7990/api/v1/object/test_key?flag=0&exptime=0"
```

---

## 1. HTTP 连接建立

```
Client (curl) ──TCP──► http.ListenAndServe(":7990")
```

`initDataWeb()` ([data_web.go](../../gobeansdb/data_web.go#L211)) 在启动时注册了 HTTP 监听：

- 监听地址: `conf.DataHTTPListen:conf.DataHTTPPort` →0.0.0:7990`
- Handler: `newDataHTTPMux()` 返回的 `http.ServeMux`，外层包装了 `withObservability` 中间件

---

## 2. Observability 中间件

请求先经过 `withObservability()` ([data_web.go](../../gobeansdb/data_web.go#L190))：

```go
st := time.Now()                           // 记录开始时间
rw := &statusRecorder{ResponseWriter: w}    // 包装 ResponseWriter 捕获状态码和字节数
next.ServeHTTP(rw, r)                      // 进入实际 Handler
latency := time.Since(st)
svc.metrics.record(status, latency)        // 更新指标 (QPS/状态码/延迟分桶)
svc.logAccess(r, status, rw.bytes, latency) // 写 access log (如配置了日志文件)
```

延迟分桶: `<10ms` / `<50ms` / `<200ms` / `>=200ms`

---

## 3. 路由分发

`newDataHTTPMux()` ([data_web.go](../../gobeansdb/data_web.go#L233)) 中注册了路由：

```go
mux.HandleFunc("/api/v1/object/", newDataObjectHandler(clientProvider))
```

请求路径 `/api/v1/object/test_key` 匹配前缀 `/api/v1/object/`，进入 `dataObjectHandler`。

其他路由:
- `/healthz` → 健康检查 (返回 `ok`)
- `/metrics` → 指标快照 (QPS/状态码/延迟/当前 item 数)

---

## 4. dataObjectHandler 入口

[data_web.go](../../gobeansdb/data_web.go#L256)：

```go
func newDataObjectHandler(clientProvider dataStoreProvider) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        // Step 4a: 鉴权
        if !requireDataAuth(w, r) { return }
        // 检查 X-Beansdb-Token header，若配置了 DataHTTPAuthToken 则必须匹配

        // Step 4b: 检查服务是否就绪
        if checkStarting(w) { return }
        // storage == nil 时返回 "starting"

        // Step 4c: 提取 key
        key := strings.TrimPrefix(r.URL.Path, "/api/v1/object/")
        // key = "test_key"

        // Step 4d: 校验 key 合法性
        if !store.IsValidKeyString(key) { ... }
        // 检查: 长度 1~250, 首字符非空格/?/@, 无控制字符
        // "test_key" 合法

        // Step 4e: 获取 StorageClient
        client := clientProvider()
        // = storage.Client() -> &StorageClient{hstore}

        // Step 4f: 按方法分发
        switch r.Method {
        case http.MethodPut, http.MethodPost:
            handleUploadObject(w, r, client, key)  // <- PUT 走这里
        case http.MethodGet:
            handleDownloadObject(w, client, key)
        case http.MethodDelete:
            handleDeleteObject(w, client, key)
        default:
            // 405 Method Not Allowed
        }
    }
}
```

---

## 5. handleUploadObject — 请求体解析

[data_web.go](../../gobeansdb/data_web.go#L296)：

```go
func handleUploadObject(w http.ResponseWriter, r *http.Request, client mc.StorageClient, key string) {
```

### 5.1 检查 Content-Length

```go
if r.ContentLength > config.MCConf.BodyMax { ... }
// BodyMax 默认 50M, "hello-from-host" 16 字节，通过
```

### 5.2 解析 flag 查询参数

```go
flag := 0
if s := r.URL.Query().Get("flag"); s != "" {
    flag, _ = strconv.Atoi(s)  // flag = 0
}
```

### 5.3 解析 exptime 查询参数

```go
exptime := 0
if s := r.URL.Query().Get("exptime"); s != "" {
    exptime, _ = strconv.Atoi(s)  // exptime = 0
}
```

> **重要**: HTTP API 的 `exptime` 参数被直接映射为 `Payload.Ver`（版本号），
> 而不是 memcache 协议中的过期时间。`exptime=0` 表示自动递增版本号。

### 5.4 读取 body

```go
limitedBody := http.MaxBytesReader(w, r.Body, config.MCConf.BodyMax)
body, _ := io.ReadAll(limitedBody)
// body = []byte("hello-from-host"), len=16
```

带大小限制读取，防止 OOM。

### 5.5 校验 value 大小

```go
if !config.IsValidValueSize(uint32(len(body))) { ... }
// 检查 <= BodyMax，通过
```

### 5.6 分配 CArray (C 内存)

```go
var arr cmem.CArray
arr.Alloc(len(body))       // 16 < Body默认4K) -> Go make([]byte, 16)
copy(arr.Body, body)       // 拷贝 "hello-from-host"
cmem.DBRL.SetData.AddSizeAndCount(arr.Cap)  // 记录资源统计
```

小对象 (<4K) 使用 Go 原生分配，大对象使用 C `malloc` 避免 GC 压力。

### 5.7 构建 memcache Item

```go
item := &mc.Item{
    ReceiveTime: time.Now(),
    Flag:        0,       // flag=0
    Exptime:     0,       // exptime=0 (将作为 version 使用)
    CArray:      arr,     // Body="hello-from-host"
}
```

### 5.8 调用 StorageClient.Set()

```go
stored, err := client.Set(key, item, false)
```

---

## 6. StorageClient.Set() — 适配层

[store.go](../../gobeansdb/store.go#L41)：

```go
func (s *StorageClient) Set(key string, item *mc.Item, noreply bool) (bool, error) {
    tofree := &item.CArray
    defer func() {
        if tofree != nil {
            cmem.DBRL.SetData.SubSizeAndCount(tofree.Cap)
            tofree.Free()      // 释放 CArray 内存 (若未转移所有权)
        }
    }()
```

### 6.1 再次校验 key

```go
if !store.IsValidKeyString(key) { return false, nil }
```

### 6.2 构建 KeyInfo

```go
ki := s.prepare(key, false)
// ki = &store.KeyInfo{
//     StringKey: "test_key",
//     Key:       []byte("test_key"),
//     KeyIsPath: false,
// }
```

### 6.3 构建 Payload

```go
payload := &store.Payload{}
payload.Flag = uint32(item.Flag)      // 0
payload.CArray = item.CArray          // "hello-from-host"
payload.Ver = int32(item.Exptime)     // 0  <- 注意: exptime 被当作 Ver 使用
payload.TS = uint32(item.ReceiveTime.Unix())  // 当前 Unix 时间戳

tofree = nil  // 标记 CArray 所有权已转移给 payload，不要在 defer 中释放
```

### 6.4 调用 HStore.Set()

```go
err := s.hstore.Set(ki, payload)
```

---

## 7. HStore.Set() — 路由到 Bucket

[hstore.go](../../gobeansdb/store/hstore.go#L370)：

```go
func (store *HStore) Set(ki *store.KeyInfo, p *store.Payload) error {
```

### 7.1 计算 KeyHash

```go
ki.KeyHash = getKeyHash(ki.Key)
// getKeyHash("test_key") = (FNV1a("test_key") << 32) | Murmur3("test_key")
// 64-bit hash，高32位 FNV1a，低32位 Murmur3
```

双重哈希降低碰撞概率，同时将 64-bit 空间均匀分布到 16 叉树。

### 7.2 准备路由信息

```go
ki.Prepare()
// 将 KeyHash 解析为 16 个 4-bit hex digit 路径
// KeyPath = [h0, h1, h2, ..., h15] (每个 0-f)
// BucketID = KeyPath[:TreeDepth] 的组合
// 例如 TreeDepth=2: BucketID = h0*16 + h1
```

`KeyInfo.Prepare()` 内部 ([key.go](../../store/key.go#L125))：

```go
func (ki *KeyInfo) Prepare() (err error) {
    ki.KeyPath = ParsePathUint64(ki.KeyHash, ki.KeyPathBuf[:16])
    // 将 64-bit KeyHash 的每 4 bit 解析为一个 hex digit
    ki.BucketID = 0
    for _, v := range ki.KeyPath[:Conf.TreeDepth] {
        ki.BucketID <<= 4
        ki.BucketID += v
    }
    return
}
```

### 7.3 获取目标 Bucket

```go
bkt := store.buckets[ki.BucketID]
atomic.AddInt64(&bkt.NumSet, 1)

if bkt.State != BUCKET_STAT_READY { ... }
// 必须是 READY 状态
```

### 7.4 委托给 Bucket.checkAndSet()

```go
return bkt.checkAndSet(ki, p)
```

---

## 8. Bucket.checkAndSet() — 版本控制 + 写入

[bucket.go](../../store/bucket.go#L342)：

### 8.1 压缩 + ValueHash (在锁外)

```go
if v.Ver >= 0 {
    rec := &Record{ki.Key, v}
    v.CalcValueHash()           // 计算 vhash = Getvhash("hello-from-host")
    // Getvhash: FNV1a(value) 或 FNV1a(value[:512])*97+FNV1a(value[-512:])
    // 返回 uint16 摘要
    oldCap := rec.Payload.CArray.Cap
    rec.TryCompress()           // 尝试 QuickLZ 压缩
    // "hello-from-host" 仅 16 字节, 小于 256 字节阈值 -> 不压缩
    cmem.DBRL.SetData.AddSize(rec.Payload.CArray.Cap - oldCap)
}
```

`TryCompress()` 内部逻辑 ([item.go](../../store/item.go#L120))：

1. 跳过: Ver < 0 (已删除)、已有压缩标志、body <= 256 字节
2. 检测 Content-Type (前 10K 字节)，音频类型不压缩
3. QuickLZ 试压缩前 10K 字节
4. 压缩比 > 0.7 则放弃 (不划算)
5. 通过则压缩整个 body，设置 `FLAG_COMPRESS`

### 8.2 加写锁

```go
bkt.writeLock.Lock()
ok := false
defer func() {
    bkt.writeLock.Unlock()
    if !ok && v.Ver >= 0 {
        cmem.DBRL.SetData.SubSizeAndCount(v.CArray.Cap)
        v.Free()   // 写入失败时释放内存
    }
}()
```

### 8.3 读取旧值

```go
oldv := int32(0)
payload, pos, err := bkt.get(ki, true)  // memOnly=true, 只查内存索引不读磁盘
if payload != nil {
    oldv = payload.Ver
    // CheckVHash: 如果 value hash 相同则跳过写入
    if oldv > 0 && v.ValueHash == payload.ValueHash {
        if Conf.CheckVHash {
            if v.Ver != 0 {
                // sync script: set_raw(k, v, rev=xxx)
                bkt.htree.set(ki, &v.Meta, pos)
            }
            return nil  // 相同值不重复写
        }
    }
}
// 对于首次写入 test_key: payload=nil, oldv=0
```

`memOnly=true` 模式下 `get()` 只查 HTree/CollisionTable 中的 Meta 信息，
不读磁盘文件，减少 IO 开销。

### 8.4 版本号处理

```go
v.Ver, valid := bkt.checkAndUpdateVerison(oldv, v.Ver)
```

版本号逻辑 ([bucket.go](../../store/bucket.go#L325))：

| 场景 | oldv | 传入 ver | 结果 ver |
|---|---|---|---|
| 新建 key | 0 | 0 (自动) | 1 |
| 更新已存在 | 5 | 0 (自动) | 6 |
| 强制删除 | 5 | -1 | -6 |
| 版本不递增 | 5 | 3 | 拒绝 (abs(3) <= abs(5)) |
| 删除不存在的 key | 0 | -1 | 返回 NOT_FOUND |

本例中: oldv=0, ver=0 -> ver=1 (新建)

### 8.5 执行写入

```go
ok = true
bkt.set(ki, v)
return nil
```

---

## 9. Bucket.set() — 三步写入

[bucket.go](../../store/bucket.go#L395)：

```go
func (bkt *Bucket) set(ki *store.KeyInfo, v *store.Payload) error {
    pos, err := bkt.datas.AppendRecord(&Record{ki.Key, v})  // Step 9a: 追加数据文件
    bkt.htree.set(ki, &v.Meta, pos)                         // Step 9b: 更新内存索引
    bkt.hints.set(ki, &v.Meta, pos, v.RecSize, "set")        // Step 9c: 写入 hint buffer
    return nil
}
```

三步必须全部成功。数据 append 到 data file (得到 Position)，
然后用 Position 更新 HTree 和 hint。

---

## 10. dataStore.AppendRecord() — 追加数据文件

[datafile.go](../../store/datafile.go#L65)：

### 10.1 再次尝试压缩

```go
rec.TryCompress()
// 在 set 路径中已压缩过，这里不会生效
```

### 10.2 封装为 WriteRecord

```go
wrec := wrapRecord(rec)
// wrec.ksz = len("test_key") = 9
// wrec.vsz = len("hello-from-host") = 16
```

### 10.3 检查当前 chunk 是否已满

```go
ds.Lock()
currOffset := ds.chunks[ds.newHead].writingHead
size := rec.Payload.RecSize
// recSize = 24(头) + 9(key) + 16(value) = 49
// 对齐到 256 字节: ((49 + 255) >> 8) << 8 = 256

if currOffset + size > DataFileMax (4GB) {
    ds.newHead++        // 切换到新 chunk
    go ds.flush(...)    // 异步刷盘旧 chunk
}
```

### 10.4 计算位置

```go
pos.ChunkID = ds.newHead     // 如 chunk 0
pos.Offset = currOffset      // 如 offset 0x100 (256)
```

### 10.5 追加到写缓冲

```go
wrec.pos = pos
ds.chunks[ds.newHead].AppendRecord(wrec)
// wrec 加入 dataChunk.wbuf 切片，writingHead += 256
```

`dataChunk.AppendRecord()` ([datachunk.go](../../store/datachunk.go#L45))：

```go
func (dc *dataChunk) AppendRecord(wrec *WriteRecord) {
    dc.Lock()
    dc.wbuf = append(dc.wbuf, wrec)
    dc.writingHead += wrec.rec.Payload.RecSize
    dc.size = dc.writingHead
    dc.Unlock()
}
```

### 10.6 更新资源统计 + 唤醒刷盘

```go
if wrec.rec.Payload.Ver > 0 {
    cmem.DBRL.FlushData.AddSizeAndCount(rec.Payload.CArray.Cap)
    cmem.DBRL.SetData.SubSizeAndCount(rec.Payload.CArray.Cap)
}

if cmem.DBRL.FlushData.Size > Conf.FlushWake (10M) {
    WakeupFlush()
}
```

### 磁盘记录格式 (256 字节对齐)

```
Offset: 0x100
+----------------+----------------+----------------+----------------+
| CRC32 (4B)     | TS (4B)        | Flag (4B)      | Ver (4B)       |
| xxxxxxxx       | 6Bxxxxxx       | 00000000       | 00000001       |
+----------------+----------------+----------------+----------------+
| KeySz (4B)     | ValSz (4B)     | Key (9B)       | Value (16B)    |
| 00000009       | 00000010       | "test_key"     | "hello-from..."|
+----------------+----------------+---------------------------------+
| Padding (207B)                                                     |
| \x00\x00...                                                       |
+--------------------------------------------------------------------+
```

- CRC32: 覆盖 header[4:] + Key + Value (C 实现)
- 所有字段 Little-Endian
- 记录大小 = 49 字节，对齐到 256 字节 (尾部 padding)

---

## 11. HTree.set() — 更新内存索引

[htree.go](../../store/htree.go#L287)：

### 11.1 构建 HTreeReq

```go
func (tree *HTree) set(ki *store.KeyInfo, meta *store.Meta, pos Position) {
    req := HTreeReq{
        ki:       ki,
        Meta:     *meta,
        Position: pos,
        item:     HTreeItem{ki.KeyHash, pos, meta.Ver, meta.ValueHash},
    }
    tree.setReq(&req)
}
```

`HTreeItem` 结构 ([item.go](../../store/item.go#L51))：

```go
type HTreeItem struct {
    Keyhash uint64    // 64-bit key hash
    Pos     Position  // {ChunkID, Offset}
    Ver     int32     // 版本号 (正=有效, 负=已删除)
    Vhash   uint16    // value hash 摘要
}
```

### 11.2 定位到叶子节点

```go
func (tree *HTree) setReq(req *HTreeReq) {
    tree.Lock()
    defer tree.Unlock()

    tree.getLeafAndInvalidNodes(req.ki, &tree.ni)
    // 按 KeyPath 逐层路由:
    //   level 0: offset = KeyPath[TreeDepth]
    //   level 1: offset = offset*16 + KeyPath[TreeDepth+1]
    //   ...
    //   最终定位到 leafs[ni.offset]
    // 同时标记路径上所有 Node 的 isHashUpdated = false
}
```

路由示意 (假设 TreeDepth=2, TreeHeight=7):

```
                Root (level 0)
             /  |  ...  |  \
         h0  h1  ...  hf      <- level 1 (TreeDepth 层, 用于 BucketID)
       / | \
     ...     ...               <- level 2-6
     |
   Leaf (level 6)
   存储: [KeyHash|Ver|VHash|Offset|ChunkID][KeyHash|...]
```

### 11.3 在叶子节点中 Set

```go
tree.setToLeaf(&tree.ni, req)
```

[leaf.go](../../store/leaf.go#L120)：

```go
func (sh *SliceHeader) Set(req *HTreeReq) (oldm HTreeItem, exist bool) {
    leaf := sh.ToBytes()
    idx := findInBytes(leaf, req.ki.KeyHash)
    // 使用 C.memcmp 或 Go bytes.Compare 在叶子中搜索 KeyHash

    exist = (idx >= 0)
    if exist {
        bytesToItem(leaf[idx+lenKHash:], &oldm)  // 读取旧 item
        // 覆盖写入新 item
    } else {
        sh.enlarge(newSize)  // C.realloc 扩大叶子内存
        // 追加新 item
    }
}
```

叶子节点内存布局:

```
Leaf (C malloc/realloc 内存):
+------------------+------------------+------------------+-----+
| KeyHash (5-8B)   | Ver (4B)         | VHash (2B)       | ... |
| xxxxxxxx         | 00000001         | xxxx             |     |
+------------------+------------------+------------------+-----+
        | Offset (3B)      | ChunkID (2B)     |
        | 00000100         | 0000             |
        +------------------+------------------+
```

- KeyHash 长度由 `TreeKeyHashLen` 决定 (5-8 字节，取决于树深度)
- Offset 占 3 字节 (最大 16MB per chunk，足够)
- ChunkID 占 2 字节 (最大 65535 chunks)
- 单条 item 共 11-14 字节，非常紧凑

---

## 12. hintMgr.set() — 写入 hint 缓冲

[hint.go](../../store/hint.go#L510)：

### 12.1 构建 HintItem

```go
it := newHintItem(ki.KeyHash, meta.Ver, meta.ValueHash, Position{0, pos.Offset}, ki.StringKey)
// HintItem 中 Pos.ChunkID = 0，因为 hint 按 chunk 存储，chunkID 由 hintChunk 隐含
```

### 12.2 更新 CollisionTable (如果存在冲突)

```go
if _, ok := h.collisions.get(ki.KeyHash, ki.StringKey); ok {
    it2 := *it
    it2.Pos.ChunkID = pos.ChunkID
    h.collisions.compareAndSet(&it2, "set")
}
// 首次写入 test_key: 不存在冲突，跳过
```

### 12.3 写入 hintChunk 的 buffer

```go
return h.setItem(it, pos.ChunkID, recSize)
```

[hint.go](../../store/hint.go#L241)：

```go
func (chunk *hintChunk) setItem(it *HintItem, recSize uint32) (rotated bool) {
    chunk.Lock()
    sp := chunk.splits[len(chunk.splits)-1]  // 当前最后一个 split
    if !sp.buf.Set(it, recSize) {
        // buffer 满 (SplitCap=1M 条) ->
        chunk.rotate().buf.Set(it, recSize)
        rotated = true
    }
    chunk.lastTS = time.Now().Unix()
    chunk.Unlock()
    return
}
```

`HintBuffer.Set()` 内部 ([hint.go](../../store/hint.go#L122))：

- 维护 `index map[KeyHash]int` 快速查找
- 维护 `collisions map[KeyHash]map[string]int` 处理同 hash 不同 key
- buffer 满 (达到 SplitCap) 时返回 false

Hint 缓冲层级结构:

```
hintMgr (per bucket)
  +-- hintChunk[] (per data chunk)
  |     +-- hintSplit[] (容量满时分片)
  |           +-- HintBuffer (内存缓冲, map[KeyHash]Item)
  |           +-- hintFileIndex (持久化文件+索引)
  +-- CollisionTable (hash 冲突表, YAML 持久化)
```

---

## 13. 响应返回

回到 `handleUploadObject()` ([data_web.go](../../gobeansdb/data_web.go#L352))：

```go
stored, err := client.Set(key, item, false)
// stored = true, err = nil

writeJSON(w, http.StatusOK, dataCRUDResult{
    OK:   true,
    Key:  "test_key",
    Size: 16,  // len("hello-from-host")
})
```

**HTTP 响应**:

```
HTTP/1.1 200 OK
Content-Type: application/json

{"ok":true,"key":"test_key","size":16}
```

---

## 14. 后台异步操作 (请求已返回)

写完数据后，以下操作在后台异步进行：

### 14a. 数据刷盘 (Flusher goroutine)

- 触发条件: 每 60 秒 (`FlushInterval`) 或缓冲超过 10M (`FlushWake`)
- `dataChunk.flush()`: 将 `wbuf` 中的 `WriteRecord` 逐条写入磁盘 `.data` 文件
- 写完后释放 payload CArray 内存
- 调用 `fsync` (通过 `bufio.Writer.Flush()` + `fd.Close()`)

### 14b. Hint dump (HintDumper goroutine)

- 每分钟检查各 chunk 的 hint buffer
- 沉默时间 > 5 秒 (`SecsBeforeDump`) 的 split -> dump 到 `NNN.NNN.idx.s` 文件
- dump 时 HintBuffer 中的 items 按 KeyHash 排序后顺序写入
- 同时生成 hint 文件尾部索引 (keyhash -> offset 的稀疏索引)

### 14c. Hint merge

- 每 `MergeInterval`(5) 个 chunk 轮转后触发 merge
- 多路归并所有 `.idx.s` -> `.idx.m` (使用堆排序)
- merge 过程中检测 hash 冲突 -> 更新 CollisionTable -> 持久化到 `collision.yaml`

### 14d. HTree dump

- 在 HintDumper 后检查 `checkForDump()`
- 如果 data chunk 数 > 已 dump 的 htree 超过 `TreeDump`(3) 个 chunk
- -> dump HTree 到 `NNN.NNN.idx.hash` 文件
- dump 原子操作: 先写 `.tmp` 再 `os.Rename` 覆盖

---

## 完整调用链路图

```
curl PUT /api/v1/object/test_key?flag=0&exptime=0
  |
  v
http.ListenAndServe(":7990")
  |
  v
withObservability middleware                    <- 计时/指标/日志
  |
  v
newDataObjectHandler()                          <- data_web.go:256
  |-- requireDataAuth()                          <- Token 鉴权
  |-- checkStarting()                            <- 服务就绪检查
  |-- key = "test_key"                           <- URL 路径解析
  |-- IsValidKeyString("test_key")               <- key 合法性
  `-- handleUploadObject()                       <- data_web.go:296
       |-- flag = 0, exptime = 0                 <- 查询参数解析
       |-- body = io.ReadAll(r.Body)             <- 读取 body
       |-- CArray.Alloc(16) + copy               <- C 内存分配
       |-- Item{Flag:0, Exptime:0, CArray:body}  <- 构建 Item
       `-- StorageClient.Set("test_key", item)   <- store.go:41
            |-- IsValidKeyString()                <- 再次校验
            |-- KeyInfo{StringKey:"test_key"}     <- 构建 KeyInfo
            |-- Payload{Flag:0, Ver:0, TS:now}    <- 构建 Payload
            `-- HStore.Set(ki, payload)           <- hstore.go:370
                 |-- KeyHash = FNV1a<<32|Murmur3  <- 计算 64-bit hash
                 |-- ki.Prepare() -> BucketID      <- 路由到 bucket
                 `-- Bucket.checkAndSet(ki, v)     <- bucket.go:342
                      |-- CalcValueHash()           <- 计算 vhash
                      |-- TryCompress()             <- QuickLZ (跳过, <256B)
                      |-- writeLock.Lock()          <- 加写锁
                      |-- get(ki, memOnly=true)     <- 读取旧值 (不存在)
                      |-- checkAndUpdateVerison()   <- Ver: 0->1 (自动递增)
                      `-- Bucket.set(ki, v)         <- bucket.go:395
                           |-- dataStore.AppendRecord()  <- datafile.go:65
                           |    |-- wrapRecord()            <- 封装 WriteRecord
                           |    |-- 检查 chunk 容量         <- 4GB 限制
                           |    |-- pos = {ChunkID, Offset} <- 记录位置
                           |    |-- dataChunk.AppendRecord()<- 写入 wbuf
                           |    `-- 更新 ResourceLimiter     <- 内存统计
                           |
                           |-- HTree.set(ki, meta, pos)    <- htree.go:287
                           |    |-- getLeafAndInvalidNodes()<- 定位叶子
        |    `-- SliceHeader.Set()        <- C内存写入/扩容
                           |
                           `-- hintMgr.set(ki, meta, pos)   <- hint.go:510
                                |-- collisions.compareAndSet() <- 冲突表
                                `-- hintChunk.setItem()         <- hint buffer
                                     `-- HintBuffer.Set()        <- 内存 buffer
```

---

## 关键数据流转

```
"hello-from-host" (HTTP body)
    | copy
    v
CArray.Body (Go/C 内存, 16 bytes)
    | TryCompress (跳过, 太小)
    | CalcValueHash
    v
Payload{Ver:1, VHash:xxxx, Flag:0, Body:"hello-from-host", TS:0x68...}
    | AppendRecord
    v
WriteRecord -> dataChunk.wbuf[] (内存写缓冲)
    | (异步 flush)
    v
000.data 磁盘文件 (256B 对齐, CRC32 校验)
    |
    v
HTreeItem{KeyHash, Pos:{0, 0x100}, Ver:1, VHash:xxxx}
    | HTree.set
    v
HTree leaf C 内存 (索引: keyhash -> position)
    |
    v
HintItem{KeyHash, Pos:{0, 0x100}, Ver:1, VHash:xxxx, Key:"test_key"}
    | hintMgr.set
    v
hintBuffer 内存 (等待 dump -> .idx.s -> merge -> .idx.m)
```

---

## 注意事项

1. **`exptime` 实际用作 `Ver` (版本号)**: HTTP API 将 URL 参数 `exptime` 映射为 memcache Item 的 Exptime，但 `StorageClient.Set()` 将其直接赋给 `Payload.Ver`。`exptime=0` 表示"自动递增版本号"，不是"永不过期"。

2. **写入不是立即落盘**: 数据先写入 `dataChunk.wbuf` 内存缓冲，由后台 `Flusher` goroutine 定时刷盘 (默认 60 秒或缓冲超 10M)。异常断电可能丢失未刷盘数据。

3. **内存索引优先**: 读取时先查 HTree (内存) -> 再查 CollisionTable -> 最后读磁盘文件。写缓冲中的数据可被直接读到 (`inbuffer=true`)。

4. **写锁粒度为 Bucket**: 同一 Bucket 内的写入串行化 (互斥锁)，不同 Bucket 可并行写入。

5. **CheckVHash 去重**: 开启后，如果新旧 value 的 vhash 相同则跳过实际写入，仅更新 HTree 位置。适用于数据同步等场景。

6. **数据对齐**: 每条记录以 256 字节对齐，简化偏移计算，GC 中损坏记录恢复也以 256 字节为单位步进。

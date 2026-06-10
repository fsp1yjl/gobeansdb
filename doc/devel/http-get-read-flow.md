# GoBeansDB HTTP GET 读取处理流程分析

本文以如下请求为例，追踪从 HTTP 接收到数据返回的完整处理链路：

```
curl -i "http://192.168.220.128:7990/api/v1/object/test_key"
```

假设 key `test_key` 已存在，值为 `hello-from-host`。

---

## 1. HTTP 连接建立

```
Client (curl) ──TCP──► http.ListenAndServe("0.0.0.0:7990")
```

与 PUT 相同，请求到达 `dataHTTPMux` 注册的 Handler。

---

## 2. Observability 中间件

与 PUT 相同，`withObservability()` ([data_web.go](../../gobeansdb/data_web.go#L190))：

```go
st := time.Now()
rw := &statusRecorder{ResponseWriter: w}
next.ServeHTTP(rw, r)                      // 进入实际 Handler
latency := time.Since(st)
svc.metrics.record(status, latency)        // 更新延迟分桶指标
svc.logAccess(r, status, rw.bytes, latency) // 写 access log
```

---

## 3. 路由分发 → dataObjectHandler

与 PUT 相同，路径 `/api/v1/object/test_key` 匹配前缀 `/api/v1/object/`，进入 `dataObjectHandler` ([data_web.go](../../gobeansdb/data_web.go#L256))。

### 入口处理

```go
// 3a: 鉴权
if !requireDataAuth(w, r) { return }

// 3b: 服务就绪检查
if checkStarting(w) { return }

// 3c: 提取 key
key := strings.TrimPrefix(r.URL.Path, "/api/v1/object/")
// key = "test_key"

// 3d: 校验 key 合法性
if !store.IsValidKeyString(key) { ... }
// IsValidKeyString: 长度 1~250, 首字符非空格/?/@, 无控制字符

// 3e: 获取 StorageClient
client := clientProvider()
// = storage.Client() -> &StorageClient{hstore}

// 3f: 按方法分发 — GET 走这里
switch r.Method {
case http.MethodGet:
    handleDownloadObject(w, client, key)   // ← GET 路径
...
}
```

---

## 4. handleDownloadObject — HTTP 响应封装

[data_web.go](../../gobeansdb/data_web.go#L364)：

```go
func handleDownloadObject(w http.ResponseWriter, client mc.StorageClient, key string) {
    item, err := client.Get(key)
    if err != nil {
        writeJSON(w, http.StatusInternalServerError, dataCRUDResult{OK: false, Key: key, Msg: err.Error()})
        return
    }
    if item == nil {
        writeJSON(w, http.StatusNotFound, dataCRUDResult{OK: false, Key: key, Msg: "not found"})
        return
    }

    defer func() {
        cmem.DBRL.GetData.SubSizeAndCount(item.CArray.Cap)
        item.CArray.Free()
    }()

    w.Header().Set("Content-Type", "application/octet-stream")
    w.Header().Set("X-Beansdb-Flag", strconv.Itoa(item.Flag))
    w.WriteHeader(http.StatusOK)
    _, _ = w.Write(item.Body)
}
```

核心只有一步：调用 `client.Get(key)`，然后根据结果返回。

三种返回情况：

| 条件 | HTTP Status | Body |
|---|---|---|
| item != nil | 200 OK | value 原始字节 + `X-Beansdb-Flag` header |
| item == nil | 404 Not Found | `{"ok":false,"key":"test_key","msg":"not found"}` |
| err != nil | 500 Internal Server Error | `{"ok":false,"key":"test_key","msg":"..."}` |

---

## 5. StorageClient.Get() — 适配层

[store.go](../../gobeansdb/store.go#L124)：

```go
func (s *StorageClient) Get(key string) (*mc.Item, error) {
```

这是一个关键分支点。`StorageClient.Get()` 根据 key 的首字符决定走哪条读取路径：

### 5.1 首字符分支

```go
if key[0] == '@' {
    // ... @ 路径: 列目录或 collision 查询
} else if key[0] == '?' {
    // ... ? 路径: 元信息查询 (ver, vhash, flag, size, ts)
} else {
    // 普通读取路径 ← "test_key" 走这里
    ki := s.prepare(key, false)
    payload, _, err := s.hstore.Get(ki, false)
    ...
}
```

| 首字符 | 路径 | 用途 |
|---|---|---|
| `@` | 列目录 / collision 查询 | 管理和调试 |
| `?key` | 元信息查询 (不含 body) | 调试、版本比较 |
| `??key` | 扩展元信息 (含 chunkID, offset) | 深度调试 |
| 其他 | **普通读取** | 正常数据访问 |

`test_key` 首字符为 `t`，走普通读取路径。

### 5.2 普通 GET 路径

```go
// 构建 KeyInfo
ki := s.prepare(key, false)
// ki = &store.KeyInfo{
//     StringKey: "test_key",
//     Key:       []byte("test_key"),
//     KeyIsPath: false,
// }

// 调用 HStore.Get()
payload, _, err := s.hstore.Get(ki, false)
// 第二个返回值 pos 被忽略 (HTTP API 不需要 position)

if err != nil {
    logger.Errorf("err to get %s: %s", key, err.Error())
    return nil, err
}
if payload == nil {
    return nil, nil   // key 不存在
}

// 检查是否已被删除 (Ver < 0)
if payload.Ver < 0 {
    cmem.DBRL.GetData.SubSizeAndCount(payload.CArray.Cap)
    payload.CArray.Free()
    return nil, nil   // 已删除，等同于不存在
}

// 构建 memcache Item 返回
item := new(mc.Item)
item.CArray = payload.CArray
item.Flag = int(payload.Flag)
return item, nil
```

**删除标记**: Bitcask 模型中删除不是物理删除，而是写入一条 `Ver < 0` 的 tombstone 记录。GET 时检查到 Ver 为负则返回 nil。

---

## 6. HStore.Get() — 路由到 Bucket

[hstore.go](../../gobeansdb/store/hstore.go#L359)：

```go
func (store *HStore) Get(ki *store.KeyInfo, memOnly bool) (payload *store.Payload, pos Position, err error) {
```

### 6.1 计算 KeyHash

```go
ki.KeyHash = getKeyHash(ki.Key)
// getKeyHash("test_key") = (FNV1a("test_key") << 32) | Murmur3("test_key")
```

### 6.2 准备路由信息

```go
ki.Prepare()
// 将 64-bit KeyHash 解析为 16 个 4-bit hex digit 路径
// 计算 BucketID = KeyPath[:TreeDepth] 的组合
```

### 6.3 获取目标 Bucket

```go
bkt := store.buckets[ki.BucketID]
atomic.AddInt64(&bkt.NumGet, 1)  // 统计计数

if bkt.State != BUCKET_STAT_READY {
    return  // Bucket 不可用，返回 nil
}
```

### 6.4 委托给 Bucket.get()

```go
return bkt.get(ki, memOnly)
// memOnly = false (需要读取实际数据)
```

---

## 7. Bucket.get() — 核心读取逻辑

[bucket.go](../../store/bucket.go#L405)：

这是整个读取路径中最复杂的一步，涉及 CollisionTable、HTree、写缓冲、磁盘文件四级查找。

### 7.1 查 CollisionTable

```go
hintit, _ := bkt.hints.collisions.get(ki.KeyHash, ki.StringKey)
```

CollisionTable 存储 hash 冲突的 key (不同 key 产生相同 KeyHash)。如果 `test_key` 存在冲突，会先在 CollisionTable 中找到其 Position。

### 7.2 查 HTree (如果不在 CollisionTable 中)

```go
var meta *Meta
var pos Position
var found bool

if hintit == nil {
    // 正常路径: 在 HTree 中查找
    meta, pos, found = bkt.htree.get(ki)
    if !found {
        return  // key 不存在
    }
} else {
    // 冲突路径: 使用 CollisionTable 中的 Position
    pos = hintit.Pos
    meta = &Meta{
        Ver:       hintit.Ver,
        ValueHash: hintit.Vhash,
        RecSize:   0,
        TS:        0,
        Flag:      0,
    }
}
```

对于 `test_key` (无冲突): `hintit == nil`，走 HTree 正常路径。

### 7.3 按 Position 读取 Record

```go
beforeGetRecord := time.Now()

rec, inbuffer, err := bkt.datas.GetRecordByPos(pos)
// pos = {ChunkID: 0, Offset: 0x100} (之前 PUT 写入时的位置)

getRecordTimeCost := time.Now().Sub(beforeGetRecord).Seconds() * 1000
```

### 7.4 校验读取结果

```go
if err != nil {
    // 读取失败: 数据文件可能损坏
    return
} else if rec == nil {
    // HTree 指向的位置没有数据 (索引与数据不一致)
    err = fmt.Errorf("bad htree item, get nothing, key %s pos %v inbuffer %v",
        ki.Key, pos, inbuffer)
    logger.Errorf("%s", err.Error())
    return
}
```

### 7.5 校验 Key 匹配

```go
else if bytes.Compare(rec.Key, ki.Key) == 0 {
    // Key 完全匹配 — 最常见的情况
    payload = rec.Payload
    payload.Ver = meta.Ver  // 使用 HTree 中记录的 Ver (而非 data file 中的)
    // 写入 analysis log (如果开启)
    return
}
```

Key 匹配时，`payload.Ver` 被替换为 HTree 中存储的 Ver。这是因为 data file 中可能存在多条同名 key 记录 (append-only)，HTree 中的 Ver 反映的是最新版本。

### 7.6 Key 不匹配 — 处理 Hash 冲突

如果从 HTree 指向的 Position 读取的 key 与期望 key 不匹配：

```go
// Key 不匹配，说明是 Hash 冲突
keyhash := getKeyHash(rec.Key)
if keyhash != ki.KeyHash {
    // 完全不同的 KeyHash: HTree 索引损坏
    // (inbuffer && 旧 chunk): GC 期间读取到过期位置，忽略
    // 否则: 报错
} else {
    // 相同 KeyHash，不同 Key — Hash 冲突

    // 在 hint 中查找正确的记录
    hintit, chunkID, err := bkt.hints.getItem(ki.KeyHash, ki.StringKey, false)

    // 将 HTree 中的记录注册到 CollisionTable
    hintit2 := newHintItem(ki.KeyHash, rec.Payload.Ver, vhash, pos, string(rec.Key))
    bkt.hints.collisions.compareAndSet(hintit2, "get1")  // HTree 中的记录

    // 将正确的记录也注册到 CollisionTable
    pos = Position{chunkID, hintit.Pos.Offset}
    bkt.hints.collisions.compareAndSet(hintit, "get2")  // hint 中的记录

    // 从正确位置读取
    rec2, _, err := bkt.datas.GetRecordByPos(pos)
    if rec2 != nil {
        payload = rec2.Payload
    }
}
```

冲突处理流程:

```
GET test_key (KeyHash=0xABCD...)
  |
  HTree.get() → pos={0, 0x200}
  |
  读取 pos={0, 0x200} → rec.Key = "other_key" (同 Hash, 不同 Key)
  |
  查 hint → 找到 test_key 的真实位置 pos={1, 0x100}
  |
  注册两个 key 到 CollisionTable
  |
  从 pos={1, 0x100} 读取 → payload 正确返回
```

---

## 8. HTree.get() — 内存索引查找

[htree.go](../../store/htree.go#L313)：

### 8.1 构建 HTreeReq

```go
func (tree *HTree) get(ki *store.KeyInfo) (meta *Meta, pos Position, found bool) {
    var req HTreeReq
    req.ki = ki
    found = tree.getReq(&req)
    meta = &Meta{0, 0, req.item.Ver, req.item.Vhash, 0}
    pos = req.item.Pos
    return
}
```

### 8.2 定位叶子节点

```go
func (tree *HTree) getReq(req *HTreeReq) (found bool) {
    tree.Lock()
    defer tree.Unlock()
    ni := &tree.ni
    tree.getLeaf(req.ki, ni)
```

`getLeaf()` 按 KeyPath 逐层路由:

```go
func (tree *HTree) getLeaf(ki *KeyInfo, ni *NodeInfo) {
    ni.level = len(tree.levels) - 1  // 最底层
    ni.offset = 0
    path := ki.KeyPath[tree.depth:]
    for level := 1; level < len(tree.levels); level += 1 {
        ni.offset = ni.offset*16 + path[level-1]
    }
    ni.node = &tree.levels[ni.level][ni.offset]
}
```

路由示意 (假设 TreeDepth=2, TreeHeight=7):

```
               Root
            /   |   \
         h0    h5   hf     ← level 1 (BucketID)
         |
       h0*16+h3              ← level 2
         |
        ...                   ← level 3-5
         |
     [Leaf] ni.offset=X      ← level 6
     +---+---+---+---+
     | A | B | C | D |       ← 逐条 item
     +---+---+---+---+
```

### 8.3 在叶子节点中搜索

```go
    found = tree.leafs[ni.offset].Get(req)
    return
}
```

[leaf.go](../../store/leaf.go#L155)：

```go
func (sh *SliceHeader) Get(req *HTreeReq) (exist bool) {
    leaf := sh.ToBytes()
    idx := findInBytes(leaf, req.ki.KeyHash)
    // 在 C malloc 内存中搜索 KeyHash

    exist = (idx >= 0)
    if exist {
        bytesToItem(leaf[idx+Conf.TreeKeyHashLen:], &req.item)
        // 解析出: Ver, VHash, Offset, ChunkID
    }
    return
}
```

`findInBytes()` 搜索策略 ([leaf.go](../../store/leaf.go#L81))：

```go
func findInBytes(leaf []byte, keyhash uint64) int {
    n := len(leaf) / lenItem
    if n < LEN_USE_C_FIND (100) {
        // Go 线性搜索 (小叶子)
        for i := 0; i < size; i += lenItem {
            if bytes.Compare(leaf[i:i+lenKHash], kb) == 0 {
                return i
            }
        }
    } else {
        // C.memcmp 搜索 (大叶子，性能更好)
        i := int(C.find(...))
        return i * lenItem
    }
    return -1
}
```

---

## 9. dataStore.GetRecordByPos() — 按 Position 读取数据

[datafile.go](../../store/datafile.go#L143)：

```go
func (ds *dataStore) GetRecordByPos(pos Position) (res *Record, inbuffer bool, err error) {
    return ds.chunks[pos.ChunkID].GetRecordByOffset(pos.Offset)
}
```

### 9.1 先查写缓冲 (wbuf)

[datachunk.go](../../store/datachunk.go#L122)：

```go
func (dc *dataChunk) GetRecordByOffsetInBuffer(offset uint32) (res *Record, err error) {
    dc.Lock()
    defer dc.Unlock()

    wbuf := dc.wbuf
    n := len(wbuf)
    // 检查 offset 是否在写缓冲范围内
    if n == 0 || offset < wbuf[0].pos.Offset || offset >= dc.writingHead {
        return  // 不在缓冲中
    }

    // 二分搜索定位到具体的 WriteRecord
    idx := sort.Search(n, func(i int) bool { return wbuf[i].pos.Offset >= offset })
    if idx < n && wbuf[idx].pos.Offset == offset {
        res = wbuf[idx].rec.Copy()   // 拷贝一份 (引用仍在缓冲中)
        cmem.DBRL.GetData.AddSizeAndCount(res.Payload.CArray.Cap)
        return
    }
}
```

### 9.2 缓冲命中 → 直接返回

```go
res, err = dc.GetRecordByOffsetInBuffer(offset)
if err != nil {
    inbuffer = true
    return
}
if res != nil {
    inbuffer = true
    cmem.DBRL.GetData.AddSize(res.Payload.DiffSizeAfterDecompressed())
    res.Payload.Decompress()   // 如果压缩了，解压
    return
}
```

### 9.3 缓冲未命中 → 读磁盘文件

```go
wrec, e := readRecordAtPath(dc.path, offset)
if e != nil {
    return nil, false, e
}
cmem.DBRL.GetData.AddSize(wrec.rec.Payload.DiffSizeAfterDecompressed())
wrec.rec.Payload.Decompress()   // 如果压缩了，解压
return wrec.rec, false, nil
// inbuffer = false
```

### 9.4 readRecordAtPath — 磁盘读取细节

[datafile.go](../../store/datafile.go#L104)：

```go
func readRecordAtPath(path string, offset uint32) (*WriteRecord, error) {
    f, err := os.Open(path)
    ...
    return readRecordAt(path, f, offset)
}
```

```go
func readRecordAt(path string, f *os.File, offset uint32) (wrec *WriteRecord, err error) {
    wrec = newWriteRecord()

    // 1. 读取 24 字节 header
    f.ReadAt(wrec.header[:], int64(offset))
    wrec.decodeHeader()
    // 解析: CRC, TS, Flag, Ver, KeySz, ValSz

    // 2. 校验 key/value 大小
    if !config.IsValidKeySize(wrec.ksz) { ... }
    if !config.IsValidValueSize(wrec.vsz) { ... }

    // 3. 分配 CArray 并一次性读取 key + value
    var kv cmem.CArray
    kv.Alloc(int(wrec.ksz + wrec.vsz))
    cmem.DBRL.GetData.AddSizeAndCount(kv.Cap)
    f.ReadAt(kv.Body, int64(offset)+recHeaderSize)

    // 4. 分离 key 和 value
    wrec.rec.Key = make([]byte, wrec.ksz)
    copy(wrec.rec.Key, kv.Body[:wrec.ksz])
    wrec.rec.Payload.CArray = kv
    wrec.rec.Payload.Body = kv.Body[wrec.ksz:]
    wrec.rec.Payload.RecSize = wrec.vsz

    // 5. CRC32 校验
    crc := wrec.getCRC()
    if wrec.crc != crc {
        err = fmt.Errorf("crc check fail ...")
        return
    }
    return wrec, nil
}
```

**磁盘读取合并**: key 和 value 一次性读取 (单次 `ReadAt` syscall)，减少 IO 次数。

---

## 10. 数据解压

[store.go](../../gobeansdb/store.go#L186) (隐含在 `GetRecordByOffset` 返回前)：

```go
res.Payload.Decompress()
```

[item.go](../../store/item.go#L163)：

```go
func (p *Payload) Decompress() (err error) {
    if p.Flag&FLAG_COMPRESS == 0 {
        return  // 未压缩，直接返回
    }
    arr, err := quicklz.CDecompressSafe(p.Body)
    if err != nil {
        logger.Errorf("decompress fail %s", err.Error())
        return
    }
    p.CArray.Free()       // 释放压缩数据
    p.CArray = arr         // 替换为解压数据
    p.Flag -= FLAG_COMPRESS
    return
}
```

`hello-from-host` 未被压缩 (写入时 < 256B 跳过压缩)，此步直接返回。

---

## 11. HTTP 响应返回

回到 `handleDownloadObject()` ([data_web.go](../../gobeansdb/data_web.go#L364))：

```go
item, err := client.Get(key)
// item != nil, err == nil

defer func() {
    cmem.DBRL.GetData.SubSizeAndCount(item.CArray.Cap)
    item.CArray.Free()  // 响应写完后释放内存
}()

w.Header().Set("Content-Type", "application/octet-stream")
w.Header().Set("X-Beansdb-Flag", strconv.Itoa(item.Flag))
// X-Beansdb-Flag: 0

w.WriteHeader(http.StatusOK)
_, _ = w.Write(item.Body)
// Body: "hello-from-host"
```

**HTTP 响应**:

```
HTTP/1.1 200 OK
Content-Type: application/octet-stream
X-Beansdb-Flag: 0

hello-from-host
```

内存释放在 `defer` 中执行，确保响应写入完成后才释放 CArray。

---

## 完整调用链路图

```
curl GET /api/v1/object/test_key
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
  `-- handleDownloadObject()                     <- data_web.go:364
       |
       `-- StorageClient.Get("test_key")        <- store.go:124
            |
            |-- key[0] != '@' && key[0] != '?'   <- 首字符分支: 普通读取
            |-- KeyInfo{StringKey:"test_key"}     <- 构建 KeyInfo
            `-- HStore.Get(ki, memOnly=false)     <- hstore.go:359
                 |
                 |-- KeyHash = FNV1a<<32|Murmur3   <- 计算 64-bit hash
                 |-- ki.Prepare() -> BucketID       <- 路由到 bucket
                 `-- Bucket.get(ki, memOnly=false)  <- bucket.go:405
                      |
                      |-- collisions.get()           <- Step 7.1: 查冲突表
                      |   (test_key 无冲突, hintit=nil)
                      |
                      |-- HTree.get(ki)             <- Step 7.2: 查内存索引
                      |   |-- getLeaf()               <- 定位叶子节点
                      |   |-- findInBytes()           <- C/Go 搜索 KeyHash
                      |   `-- 返回 meta + pos
                      |   (found=true, pos={0, 0x100})
                      |
                      |-- dataStore.GetRecordByPos()  <- Step 7.3: 按位置读取
                      |   |
                      |   `-- dataChunk.GetRecordByOffset()
                      |        |
                      |        |-- GetRecordByOffsetInBuffer()  <- Step 9.1: 查写缓冲
                      |        |   |-- 二分搜索 wbuf
                      |        |   |-- 命中 → rec.Copy()  (inbuffer=true)
                      |        |   `-- 未命中 ↓
                      |        |
                      |        `-- readRecordAtPath()            <- Step 9.3: 读磁盘
                      |             |-- ReadAt header (24B)
                      |             |-- ReadAt key+value (一次 IO)
                      |             |-- CRC32 校验
                      |             `-- 返回 Record (inbuffer=false)
                      |
                      |-- bytes.Compare(rec.Key, ki.Key)  <- Step 7.5: 校验 Key
                      |   匹配 → payload = rec.Payload
                      |
                      `-- payload.Decompress()             <- Step 10: 解压 (如需)
```

---

## 读取路径的多级查找策略

GET 请求需要经过四级查找，每一级都有特定的作用：

```
Level 1: CollisionTable (内存)
  |  作用: 处理 Hash 冲突的 key (不同 key, 相同 KeyHash)
  |  查找: map[uint64]map[string]HintItem (O(1))
  |  命中: 直接获得 Position, 跳过 HTree
  |  未命中: ↓
  |
Level 2: HTree (内存, C malloc)
  |  作用: 主索引, KeyHash → Position 的映射
  |  查找: 16 叉树路由 → 叶子节点线性搜索
  |  命中: 得到 Position{ChunkID, Offset}
  |  未命中: key 不存在, 返回 nil
  |
Level 3: dataChunk.wbuf (内存, 写缓冲)
  |  作用: 读取尚未刷盘的最新写入数据
  |  查找: 二分搜索 WriteRecord 切片 (按 Offset 有序)
  |  命中: 返回 Record 拷贝 (inbuffer=true)
  |  未命中: ↓
  |
Level 4: data file (磁盘, .data 文件)
  |  作用: 持久化数据
  |  查找: os.File.ReadAt(offset) 单次 IO
  |  步骤: 读 header → 校验大小 → 读 key+value → CRC32 校验
  |  返回: Record (inbuffer=false)
```

---

## 读取路径中的校验机制

### CRC32 校验

读取磁盘数据时，对 header + key + value 计算 CRC32，与文件中存储的 CRC 比对。不匹配则返回错误。这是 Bitcask 模型的核心数据完整性保障。

CRC32 使用 C 实现 (查表法)，性能高于 Go 标准库。

### Key 匹配校验

从 Position 读取的 Record 的 key 必须与请求 key 完全匹配。不匹配有两种情况：

1. **KeyHash 不同**: 说明 HTree 索引损坏 (严重错误)
2. **KeyHash 相同，Key 不同**: Hash 冲突，需要在 hint 中查找正确记录

### Version 校验 (Ver)

```go
if payload.Ver < 0 {
    // 已被删除 (tombstone 记录)
    return nil, nil
}
```

Bitcask 模型中删除是追加一条 `Ver < 0` 的记录，GET 时检查到 Ver 为负则返回"不存在"。

---

## 不同结果场景的完整响应

### 场景 A: key 存在 (本例)

```
HTTP/1.1 200 OK
Content-Type: application/octet-stream
X-Beansdb-Flag: 0

hello-from-host
```

### 场景 B: key 不存在

```
HTTP/1.1 404 Not Found
Content-Type: application/json

{"ok":false,"key":"test_key","msg":"not found"}
```

触发条件: HTree 中找不到该 KeyHash，或 Ver < 0 (已删除)

### 场景 C: key 存在但数据损坏

```
HTTP/1.1 500 Internal Server Error
Content-Type: application/json

{"ok":false,"key":"test_key","msg":"crc check fail ..."}
```

触发条件: CRC32 不匹配、key/value 大小异常

### 场景 D: 服务未就绪

```
HTTP/1.1 200 OK (body: "starting")
```

触发条件: `storage == nil` (正在启动)

### 场景 E: 鉴权失败

```
HTTP/1.1 401 Unauthorized (body: "unauthorized")
```

触发条件: 配置了 `DataHTTPAuthToken` 但请求缺少 `X-Beansdb-Token` header

---

## 与 PUT 写入路径的关键差异

| 维度 | PUT (写入) | GET (读取) |
|---|---|---|
| 锁 | Bucket 写锁 (互斥) | HTree 读锁 (共享) |
| IO 模式 | 写缓冲 + 异步刷盘 | 先查缓冲, 未命中同步读磁盘 |
| 数据校验 | 写入前计算 CRC32 | 读取后校验 CRC32 |
| 冲突处理 | 写入时注册 CollisionTable | 读取时查询 CollisionTable |
| 压缩 | TryCompress (写前) | Decompress (读后) |
| 版本号 | checkAndUpdateVersion (递增) | 检查 Ver < 0 (tombstone) |
| 一致性 | 三步写入 (data + HTree + hint) | 多级查找 (collision → HTree → wbuf → disk) |
| 内存分配 | CArray.Alloc | CArray.Copy (缓冲命中) 或 readRecordAtPath |
| 内存释放 | flush 后 Free | 响应写完后 defer Free |
| 写缓冲 | 数据写入 wbuf | 从 wbuf 读取最新写入 |
| Key 校验 | 写入前 IsValidKeyString | 读取后 bytes.Compare (hash 冲突检测) |

---

## 注意事项

1. **读取是同步的**: 与写入不同，GET 必须同步等待数据返回。写缓冲命中时零 IO，磁盘读取时一次 `ReadAt` syscall。

2. **inbuffer 标记**: `Bucket.get()` 中的 `inbuffer` 参数标识数据来自写缓冲还是磁盘。GC 期间如果读到旧 chunk 的 inbuffer 记录可能过期，会被忽略。

3. **HTree 中的 Ver 可能与 data file 中不同**: `Bucket.get()` 在 7.5 步用 `payload.Ver = meta.Ver` 覆盖了 data file 中的 Ver。这是因为 append-only 模型下 data file 中可能存在多条同名 key 记录，HTree 中的 Ver 反映的是最新版本。

4. **CollisionTable 的延迟注册**: 首次遇到 hash 冲突时 (Step 7.6)，两个冲突 key 才被注册到 CollisionTable。之后的 GET 请求可以直接从 CollisionTable 定位，不再需要 hint 查找。

5. **`memOnly` 参数**: HTTP GET 路径中 `memOnly=false`，需要读取实际数据。`memOnly=true` 仅用于写入时 (checkAndSet) 读取旧值的 Meta 信息，不读磁盘文件。

package gobeansdb

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/douban/gobeansdb/cmem"
	"github.com/douban/gobeansdb/config"
	mc "github.com/douban/gobeansdb/memcache"
	"github.com/douban/gobeansdb/store"
)

const dataObjectPrefix = "/api/v1/object/"

type dataStoreProvider func() mc.StorageClient
type dataItemCountProvider func() uint64

var dataClientProvider dataStoreProvider = func() mc.StorageClient {
	return storage.Client()
}

var defaultDataItemCountProvider dataItemCountProvider = func() uint64 {
	if storage == nil || storage.hstore == nil {
		return 0
	}
	return uint64(storage.hstore.NumKey())
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

type dataHTTPMetrics struct {
	start time.Time

	total uint64

	statusMu sync.Mutex
	status   map[int]uint64

	latencyLt10ms  uint64
	latencyLt50ms  uint64
	latencyLt200ms uint64
	latencyGte200  uint64

	qpsMu      sync.Mutex
	qpsTs      [60]int64
	qpsCounter [60]uint64
}

type dataHTTPService struct {
	metrics           *dataHTTPMetrics
	logger            *log.Logger
	logFd             *os.File
	itemCountProvider dataItemCountProvider
}

type dataCRUDResult struct {
	OK   bool   `json:"ok"`
	Key  string `json:"key"`
	Size int    `json:"size,omitempty"`
	Msg  string `json:"msg,omitempty"`
}

type dataHTTPMetricsSnapshot struct {
	UptimeSeconds float64           `json:"uptime_seconds"`
	TotalRequests uint64            `json:"total_requests"`
	CurrItems     uint64            `json:"curr_items"`
	QPS1m         float64           `json:"qps_1m"`
	StatusCodes   map[string]uint64 `json:"status_codes"`
	LatencyBucket map[string]uint64 `json:"latency_buckets"`
}

func newDataHTTPService(accessLogPath string, itemCountProvider dataItemCountProvider) *dataHTTPService {
	if itemCountProvider == nil {
		itemCountProvider = defaultDataItemCountProvider
	}
	svc := &dataHTTPService{
		metrics: &dataHTTPMetrics{
			start:  time.Now(),
			status: make(map[int]uint64),
		},
		itemCountProvider: itemCountProvider,
	}
	if accessLogPath != "" {
		fd, err := os.OpenFile(accessLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			logger.Errorf("open data http access log failed %s: %s", accessLogPath, err.Error())
		} else {
			svc.logFd = fd
			svc.logger = log.New(fd, "", log.Ldate|log.Ltime|log.Lmicroseconds)
		}
	}
	return svc
}

func (m *dataHTTPMetrics) record(status int, d time.Duration) {
	atomic.AddUint64(&m.total, 1)
	m.statusMu.Lock()
	m.status[status]++
	m.statusMu.Unlock()

	ms := d.Milliseconds()
	switch {
	case ms < 10:
		atomic.AddUint64(&m.latencyLt10ms, 1)
	case ms < 50:
		atomic.AddUint64(&m.latencyLt50ms, 1)
	case ms < 200:
		atomic.AddUint64(&m.latencyLt200ms, 1)
	default:
		atomic.AddUint64(&m.latencyGte200, 1)
	}

	now := time.Now().Unix()
	idx := now % 60
	m.qpsMu.Lock()
	if m.qpsTs[idx] != now {
		m.qpsTs[idx] = now
		m.qpsCounter[idx] = 0
	}
	m.qpsCounter[idx]++
	m.qpsMu.Unlock()
}

func (m *dataHTTPMetrics) snapshot() dataHTTPMetricsSnapshot {
	now := time.Now().Unix()
	qpsCnt := uint64(0)
	m.qpsMu.Lock()
	for i := 0; i < 60; i++ {
		if now-m.qpsTs[i] < 60 {
			qpsCnt += m.qpsCounter[i]
		}
	}
	m.qpsMu.Unlock()

	status := make(map[string]uint64)
	m.statusMu.Lock()
	for k, v := range m.status {
		status[strconv.Itoa(k)] = v
	}
	m.statusMu.Unlock()

	return dataHTTPMetricsSnapshot{
		UptimeSeconds: time.Since(m.start).Seconds(),
		TotalRequests: atomic.LoadUint64(&m.total),
		QPS1m:         float64(qpsCnt) / 60.0,
		StatusCodes:   status,
		LatencyBucket: map[string]uint64{
			"lt_10ms":   atomic.LoadUint64(&m.latencyLt10ms),
			"lt_50ms":   atomic.LoadUint64(&m.latencyLt50ms),
			"lt_200ms":  atomic.LoadUint64(&m.latencyLt200ms),
			"gte_200ms": atomic.LoadUint64(&m.latencyGte200),
		},
	}
}

func (svc *dataHTTPService) logAccess(r *http.Request, status, bytes int, latency time.Duration) {
	if svc.logger == nil {
		return
	}
	svc.logger.Printf("%s method=%s path=%s status=%d bytes=%d latency_us=%d ua=%q", r.RemoteAddr, r.Method, r.URL.RequestURI(), status, bytes, latency.Microseconds(), r.UserAgent())
}

func (svc *dataHTTPService) withObservability(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := time.Now()
		rw := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rw, r)
		status := rw.status
		if status == 0 {
			status = http.StatusOK
		}
		latency := time.Since(st)
		svc.metrics.record(status, latency)
		svc.logAccess(r, status, rw.bytes, latency)
	})
}

func (svc *dataHTTPService) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	snapshot := svc.metrics.snapshot()
	snapshot.CurrItems = svc.itemCountProvider()
	writeJSON(w, http.StatusOK, snapshot)
}

func initDataWeb() {
	if conf.DataHTTPPort <= 0 {
		return
	}

	listen := conf.DataHTTPListen
	if listen == "" {
		listen = conf.Listen
	}
	addr := fmt.Sprintf("%s:%d", listen, conf.DataHTTPPort)
	svc := newDataHTTPService(conf.DataHTTPAccessLog, defaultDataItemCountProvider)

	h := newDataHTTPMux(svc, dataClientProvider)
	go func() {
		logger.Infof("http data listen at %s", addr)
		err := http.ListenAndServe(addr, h)
		if err != nil {
			logger.Fatalf(err.Error())
		}
	}()
}

func newDataHTTPMux(svc *dataHTTPService, clientProvider dataStoreProvider) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(dataObjectPrefix, newDataObjectHandler(clientProvider))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/metrics", svc.handleMetrics)
	return svc.withObservability(mux)
}

func requireDataAuth(w http.ResponseWriter, r *http.Request) bool {
	token := conf.DataHTTPAuthToken
	if token == "" {
		return true
	}
	if r.Header.Get("X-Beansdb-Token") != token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func newDataObjectHandler(clientProvider dataStoreProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireDataAuth(w, r) {
			return
		}
		if checkStarting(w) {
			return
		}

		key := strings.TrimPrefix(r.URL.Path, dataObjectPrefix)
		if key == "" {
			writeJSON(w, http.StatusBadRequest, dataCRUDResult{OK: false, Msg: "missing key"})
			return
		}
		if !store.IsValidKeyString(key) {
			writeJSON(w, http.StatusBadRequest, dataCRUDResult{OK: false, Key: key, Msg: "invalid key"})
			return
		}

		client := clientProvider()
		switch r.Method {
		case http.MethodPut, http.MethodPost:
			handleUploadObject(w, r, client, key)
		case http.MethodGet:
			handleDownloadObject(w, client, key)
		case http.MethodDelete:
			handleDeleteObject(w, client, key)
		default:
			w.Header().Set("Allow", "PUT, POST, GET, DELETE")
			writeJSON(w, http.StatusMethodNotAllowed, dataCRUDResult{OK: false, Key: key, Msg: "method not allowed"})
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func handleUploadObject(w http.ResponseWriter, r *http.Request, client mc.StorageClient, key string) {
	if r.ContentLength > config.MCConf.BodyMax {
		writeJSON(w, http.StatusRequestEntityTooLarge, dataCRUDResult{OK: false, Key: key, Msg: "value too large"})
		return
	}

	flag := 0
	if s := r.URL.Query().Get("flag"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, dataCRUDResult{OK: false, Key: key, Msg: "invalid flag"})
			return
		}
		flag = v
	}

	exptime := 0
	if s := r.URL.Query().Get("exptime"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, dataCRUDResult{OK: false, Key: key, Msg: "invalid exptime"})
			return
		}
		exptime = v
	}

	limitedBody := http.MaxBytesReader(w, r.Body, config.MCConf.BodyMax)
	body, err := io.ReadAll(limitedBody)
	if err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			writeJSON(w, http.StatusRequestEntityTooLarge, dataCRUDResult{OK: false, Key: key, Msg: "value too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, dataCRUDResult{OK: false, Key: key, Msg: "invalid body"})
		return
	}
	if !config.IsValidValueSize(uint32(len(body))) {
		writeJSON(w, http.StatusRequestEntityTooLarge, dataCRUDResult{OK: false, Key: key, Msg: "value too large"})
		return
	}

	var arr cmem.CArray
	if !arr.Alloc(len(body)) {
		http.Error(w, "memory shortage", http.StatusServiceUnavailable)
		return
	}
	copy(arr.Body, body)
	cmem.DBRL.SetData.AddSizeAndCount(arr.Cap)

	item := &mc.Item{
		ReceiveTime: time.Now(),
		Flag:        flag,
		Exptime:     exptime,
		CArray:      arr,
	}

	stored, err := client.Set(key, item, false)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, dataCRUDResult{OK: false, Key: key, Msg: err.Error()})
		return
	}
	if !stored {
		writeJSON(w, http.StatusBadRequest, dataCRUDResult{OK: false, Key: key, Msg: "not stored"})
		return
	}
	writeJSON(w, http.StatusOK, dataCRUDResult{OK: true, Key: key, Size: len(body)})
}

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

func handleDeleteObject(w http.ResponseWriter, client mc.StorageClient, key string) {
	deleted, err := client.Delete(key)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, dataCRUDResult{OK: false, Key: key, Msg: err.Error()})
		return
	}
	if !deleted {
		writeJSON(w, http.StatusNotFound, dataCRUDResult{OK: false, Key: key, Msg: "not found"})
		return
	}
	writeJSON(w, http.StatusOK, dataCRUDResult{OK: true, Key: key})
}

func closeDataWeb() {
	// Keep this function for future shutdown extension.
}

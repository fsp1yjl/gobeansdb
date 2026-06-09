package gobeansdb

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/douban/gobeansdb/cmem"
	"github.com/douban/gobeansdb/config"
	mc "github.com/douban/gobeansdb/memcache"
)

type fakeStorageClient struct {
	setErr    error
	getErr    error
	deleteErr error
	data      map[string]*mc.Item
}

func newFakeStorageClient() *fakeStorageClient {
	return &fakeStorageClient{data: make(map[string]*mc.Item)}
}

func (f *fakeStorageClient) GetSuccessedTargets() []string { return []string{"localhost"} }
func (f *fakeStorageClient) Clean()                        {}
func (f *fakeStorageClient) GetMulti(keys []string) (map[string]*mc.Item, error) {
	res := make(map[string]*mc.Item)
	for _, k := range keys {
		if v, ok := f.data[k]; ok {
			res[k] = v
		}
	}
	return res, nil
}
func (f *fakeStorageClient) Append(key string, value []byte) (bool, error) { return false, nil }
func (f *fakeStorageClient) Incr(key string, value int) (int, error)       { return 0, nil }
func (f *fakeStorageClient) Len() int                                      { return len(f.data) }
func (f *fakeStorageClient) Close()                                        {}
func (f *fakeStorageClient) Process(key string, args []string) (string, string) {
	return "", ""
}

func (f *fakeStorageClient) Set(key string, item *mc.Item, noreply bool) (bool, error) {
	defer func() {
		cmem.DBRL.SetData.SubSizeAndCount(item.CArray.Cap)
		item.CArray.Free()
	}()
	if f.setErr != nil {
		return false, f.setErr
	}
	copyBody := make([]byte, len(item.Body))
	copy(copyBody, item.Body)
	f.data[key] = &mc.Item{Flag: item.Flag, CArray: cmem.CArray{Body: copyBody}}
	return true, nil
}

func (f *fakeStorageClient) Get(key string) (*mc.Item, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if v, ok := f.data[key]; ok {
		copyBody := make([]byte, len(v.Body))
		copy(copyBody, v.Body)
		return &mc.Item{Flag: v.Flag, CArray: cmem.CArray{Body: copyBody}}, nil
	}
	return nil, nil
}

func (f *fakeStorageClient) Delete(key string) (bool, error) {
	if f.deleteErr != nil {
		return false, f.deleteErr
	}
	if _, ok := f.data[key]; ok {
		delete(f.data, key)
		return true, nil
	}
	return false, nil
}

func withDataWebTestEnv(t *testing.T, token string, bodyMax int64) func() {
	t.Helper()
	oldStorage := storage
	oldToken := conf.DataHTTPAuthToken
	oldBodyMax := config.MCConf.BodyMax
	storage = &Storage{}
	conf.DataHTTPAuthToken = token
	config.MCConf.BodyMax = bodyMax
	return func() {
		storage = oldStorage
		conf.DataHTTPAuthToken = oldToken
		config.MCConf.BodyMax = oldBodyMax
	}
}

func TestDataWebAuthAndValidation(t *testing.T) {
	defer withDataWebTestEnv(t, "token", 4)()
	fake := newFakeStorageClient()
	svc := newDataHTTPService("", func() uint64 { return uint64(fake.Len()) })
	h := newDataHTTPMux(svc, func() mc.StorageClient { return fake })

	req := httptest.NewRequest(http.MethodPut, "/api/v1/object/key", bytes.NewBufferString("ok"))
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expect 401 got %d", resp.Code)
	}

	req = httptest.NewRequest(http.MethodPut, "/api/v1/object/@bad", bytes.NewBufferString("ok"))
	req.Header.Set("X-Beansdb-Token", "token")
	resp = httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expect 400 got %d", resp.Code)
	}

	req = httptest.NewRequest(http.MethodPut, "/api/v1/object/key", bytes.NewBufferString("12345"))
	req.Header.Set("X-Beansdb-Token", "token")
	resp = httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expect 413 got %d", resp.Code)
	}
}

func TestDataWebErrorMapping(t *testing.T) {
	defer withDataWebTestEnv(t, "", 1024)()
	fake := newFakeStorageClient()
	svc := newDataHTTPService("", func() uint64 { return uint64(fake.Len()) })
	h := newDataHTTPMux(svc, func() mc.StorageClient { return fake })

	fake.setErr = errors.New("set failed")
	req := httptest.NewRequest(http.MethodPut, "/api/v1/object/key", bytes.NewBufferString("ok"))
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("expect 500 got %d", resp.Code)
	}
	fake.setErr = nil

	req = httptest.NewRequest(http.MethodGet, "/api/v1/object/missing", nil)
	resp = httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("expect 404 got %d", resp.Code)
	}

	fake.deleteErr = errors.New("delete failed")
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/object/key", nil)
	resp = httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("expect 500 got %d", resp.Code)
	}
}

func TestDataWebCRUDAndMetrics(t *testing.T) {
	defer withDataWebTestEnv(t, "", 1024)()
	fake := newFakeStorageClient()
	svc := newDataHTTPService("", func() uint64 { return uint64(fake.Len()) })
	h := newDataHTTPMux(svc, func() mc.StorageClient { return fake })

	req := httptest.NewRequest(http.MethodPut, "/api/v1/object/key1?flag=9", bytes.NewBufferString("hello"))
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expect 200 got %d", resp.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/object/key1", nil)
	resp = httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expect 200 got %d", resp.Code)
	}
	if resp.Body.String() != "hello" {
		t.Fatalf("unexpected body %q", resp.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/v1/object/key1", nil)
	resp = httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expect 200 got %d", resp.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	resp = httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expect 200 got %d", resp.Code)
	}

	var m dataHTTPMetricsSnapshot
	if err := json.Unmarshal(resp.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode metrics failed: %s", err.Error())
	}
	if m.TotalRequests < 3 {
		t.Fatalf("expected metrics total >= 3 got %d", m.TotalRequests)
	}
	if m.CurrItems != 0 {
		t.Fatalf("expected curr_items=0 got %d", m.CurrItems)
	}
	if m.StatusCodes["200"] == 0 {
		t.Fatalf("expected status 200 in metrics")
	}
}

func TestDataWebAccessLog(t *testing.T) {
	defer withDataWebTestEnv(t, "", 1024)()
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "data_http_access.log")
	fake := newFakeStorageClient()
	svc := newDataHTTPService(logPath, func() uint64 { return uint64(fake.Len()) })
	h := newDataHTTPMux(svc, func() mc.StorageClient { return fake })

	req := httptest.NewRequest(http.MethodGet, "/api/v1/object/missing", nil)
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("expect 404 got %d", resp.Code)
	}

	if svc.logFd != nil {
		_ = svc.logFd.Sync()
		_ = svc.logFd.Close()
	}
	b, err := ioutil.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read access log failed: %s", err.Error())
	}
	content := string(b)
	if !bytes.Contains(b, []byte("method=GET")) || !bytes.Contains(b, []byte("status=404")) {
		t.Fatalf("unexpected access log content: %s", content)
	}
	_ = os.Remove(logPath)
}

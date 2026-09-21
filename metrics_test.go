package httpcache_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/soulteary/httpcache-kit/v4"
	"github.com/soulteary/httpcache-kit/v4/prometheusmetrics"
	metrics "github.com/soulteary/metrics-kit/v3"
)

func TestHandlerWithMetrics(t *testing.T) {
	reg := metrics.NewRegistry("test_handler_metrics")
	m := prometheusmetrics.New(reg)
	defer func() { httpcache.SetDefaultMetrics(nil) }()

	upstream := &upstreamServer{
		Body:         []byte("ok"),
		Now:          time.Date(2009, 11, 10, 23, 0, 0, 0, time.UTC),
		CacheControl: "max-age=60",
		Header:       http.Header{},
	}
	httpcache.Clock = func() time.Time { return upstream.Now }

	cache := httpcache.NewMemoryCache()
	handler := httpcache.NewHandler(cache, upstream)
	handler.SetMetrics(m)
	handler.Shared = true

	// MISS then HIT
	req1, _ := http.NewRequest("GET", "http://example.org/", nil)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	httpcache.Writes.Wait()
	if rec1.Header().Get(httpcache.CacheHeader) != "MISS" {
		t.Errorf("first request: want MISS, got %s", rec1.Header().Get(httpcache.CacheHeader))
	}

	req2, _ := http.NewRequest("GET", "http://example.org/", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Header().Get(httpcache.CacheHeader) != "HIT" {
		t.Errorf("second request: want HIT, got %s", rec2.Header().Get(httpcache.CacheHeader))
	}

	// SKIP: non-cacheable method
	req3, _ := http.NewRequest("POST", "http://example.org/", nil)
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)
	if rec3.Header().Get(httpcache.CacheHeader) != "SKIP" {
		t.Errorf("POST: want SKIP, got %s", rec3.Header().Get(httpcache.CacheHeader))
	}
}

func TestStoreFailureMetrics(t *testing.T) {
	reg := metrics.NewRegistry("test_store_fail_metrics")
	m := prometheusmetrics.New(reg)
	defer func() { httpcache.SetDefaultMetrics(nil) }()

	// Use a cache that fails on Store by using a broken body (wrong Content-Length)
	upstream := &upstreamServer{
		Body:         []byte("short"),
		Now:          time.Date(2009, 11, 10, 23, 0, 0, 0, time.UTC),
		CacheControl: "max-age=60",
		Header:       http.Header{"Content-Length": []string{"100"}},
	}
	httpcache.Clock = func() time.Time { return upstream.Now }

	cache := httpcache.NewMemoryCache()
	handler := httpcache.NewHandler(cache, upstream)
	handler.SetMetrics(m)
	handler.Shared = true

	req, _ := http.NewRequest("GET", "http://example.org/fail", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	httpcache.Writes.Wait()
	// Store fails (Content-Length 100 but body "short") -> RecordStoreOperation(false) is called
	_ = rec
}

func TestPassUpstreamInMemoryFallback(t *testing.T) {
	reg := metrics.NewRegistry("test_fallback")
	m := prometheusmetrics.New(reg)
	defer func() { httpcache.SetDefaultMetrics(nil) }()

	// Small body so we don't need temp file; trigger in-memory path in passUpstream
	body := []byte("tiny")
	upstream := &upstreamServer{
		Body:         body,
		Now:          time.Date(2009, 11, 10, 23, 0, 0, 0, time.UTC),
		CacheControl: "max-age=60",
		Header:       http.Header{},
	}
	httpcache.Clock = func() time.Time { return upstream.Now }

	cache := httpcache.NewMemoryCache()
	handler := httpcache.NewHandler(cache, upstream)
	handler.SetMetrics(m)

	req, _ := http.NewRequest("GET", "http://example.org/small", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	httpcache.Writes.Wait()

	if rec.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec.Code)
	}
	got, _ := io.ReadAll(rec.Body)
	if !bytes.Equal(got, body) {
		t.Errorf("body: got %q", got)
	}
}

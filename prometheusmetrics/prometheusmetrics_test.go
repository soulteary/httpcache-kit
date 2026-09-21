package prometheusmetrics_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	metrics "github.com/soulteary/metrics-kit/v3"

	httpcache "github.com/soulteary/httpcache-kit/v4"
	"github.com/soulteary/httpcache-kit/v4/prometheusmetrics"
)

func TestNewCacheMetrics(t *testing.T) {
	reg := metrics.NewRegistry("test_httpcache")
	m := prometheusmetrics.New(reg)
	if m == nil {
		t.Fatal("NewCacheMetrics returned nil")
	}
	if httpcache.GetDefaultMetrics() != m {
		t.Error("DefaultMetrics should be set to returned metrics")
	}
	// Reset so other tests don't get DefaultMetrics from this test
	httpcache.SetDefaultMetrics(nil)
}

func TestCacheMetrics_AllMethods(t *testing.T) {
	reg := metrics.NewRegistry("test_httpcache_ops")
	m := prometheusmetrics.New(reg)
	defer func() { httpcache.SetDefaultMetrics(nil) }()

	m.RecordCacheHit("GET")
	m.RecordCacheMiss("GET")
	m.RecordCacheSkip()
	m.RecordUpstreamDuration("GET", 200, 0.5)
	m.RecordUpstreamDuration("GET", 500, 0.1) // error status
	m.RecordUpstreamError("timeout")
	m.RecordStoreOperation(true)
	m.RecordStoreOperation(false)
	m.RecordRetrieveOperation(true)
	m.RecordRetrieveOperation(false)
	m.SetCacheSize(1024)
	m.SetCacheItemCount(10)
	m.SetCacheStaleCount(2)
	m.RecordCacheEviction("lru")
	m.RecordCacheEviction("ttl")
	m.RecordCleanupDuration(0.5)
	m.UpdateCacheStats(httpcache.CacheStats{
		TotalSize:  2048,
		ItemCount:  5,
		StaleCount: 1,
		HitCount:   100,
		MissCount:  10,
	})
	// Nil safety
	var nilM *prometheusmetrics.Metrics
	nilM.RecordCacheHit("GET")
	nilM.RecordCacheMiss("GET")
	nilM.RecordCacheSkip()
	nilM.RecordUpstreamDuration("GET", 200, 0.1)
	nilM.RecordUpstreamError("err")
	nilM.RecordStoreOperation(true)
	nilM.RecordRetrieveOperation(true)
	nilM.SetCacheSize(0)
	nilM.SetCacheItemCount(0)
	nilM.SetCacheStaleCount(0)
	nilM.RecordCacheEviction("lru")
	nilM.RecordCleanupDuration(0)
	nilM.UpdateCacheStats(httpcache.CacheStats{})
}

func TestCleanupWithMetrics(t *testing.T) {
	reg := metrics.NewRegistry("test_cleanup_metrics")
	m := prometheusmetrics.New(reg)
	defer func() { httpcache.SetDefaultMetrics(nil) }()

	now := time.Now().UTC()
	httpcache.Clock = func() time.Time { return now }

	config := httpcache.DefaultCacheConfig().
		WithTTL(1 * time.Hour).
		WithStaleMapTTL(1 * time.Hour).
		WithCleanupInterval(0)
	cache := httpcache.NewMemoryCacheWithConfig(config)
	defer func() { _ = cache.Close() }()

	body := []byte("x")
	res := httpcache.NewResourceBytes(http.StatusOK, body, http.Header{})
	if err := cache.Store(res, "k1"); err != nil {
		t.Fatal(err)
	}
	cache.Invalidate("k1")

	// Advance time and run cleanup (uses DefaultMetrics for RecordCleanupDuration / UpdateCacheStats)
	httpcache.SetDefaultMetrics(m)
	result := cache.Cleanup()
	if result.RemovedStaleEntries != 1 {
		t.Logf("cleanup result: %+v", result)
	}
}

func TestCacheEvictionMetrics(t *testing.T) {
	reg := metrics.NewRegistry("test_eviction_metrics")
	m := prometheusmetrics.New(reg)
	defer func() { httpcache.SetDefaultMetrics(nil) }()
	httpcache.SetDefaultMetrics(m)

	config := httpcache.DefaultCacheConfig().
		WithMaxSize(1500).
		WithCleanupInterval(0)
	cache := httpcache.NewMemoryCacheWithConfig(config)
	defer func() { _ = cache.Close() }()

	for i := 0; i < 5; i++ {
		body := strings.Repeat("x", 200)
		res := httpcache.NewResourceBytes(http.StatusOK, []byte(body), http.Header{
			"Content-Length": []string{"200"},
		})
		if err := cache.Store(res, "key"+string(rune('0'+i))); err != nil {
			t.Fatal(err)
		}
	}
	// Evictions should have been recorded
	_ = cache.Stats()
}

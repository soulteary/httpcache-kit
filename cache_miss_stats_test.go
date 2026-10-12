package httpcache

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type nativeMissStatsMetrics struct {
	handlerHerdMetrics
	storeFailures atomic.Int64
}

func (m *nativeMissStatsMetrics) RecordStoreOperation(success bool) {
	if !success {
		m.storeFailures.Add(1)
	}
}

type exportedNativeRecheckCache struct {
	Cache
	rechecker cacheRechecker
	calls     atomic.Int64
}

func (c *exportedNativeRecheckCache) RetrieveForRecheck(key string) (*Resource, error) {
	c.calls.Add(1)
	return c.rechecker.RetrieveForRecheck(key)
}

func TestCoordinatedRetriesCountOneNativeMissPerClient(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, failedStore := range []bool{false, true} {
			for _, wrapped := range []bool{false, true} {
				t.Run(stringBool(disk)+"/failed-store="+stringBool(failedStore)+"/wrapped="+stringBool(wrapped), func(t *testing.T) {
					const clients = 8
					c := generationBudgetCache(t, disk, 128)
					var backend Cache = c
					var wrapper *exportedNativeRecheckCache
					if wrapped {
						wrapper = &exportedNativeRecheckCache{Cache: c, rechecker: c}
						backend = wrapper
					}
					coord := newHandlerHerdCoordinator()
					metrics := &nativeMissStatsMetrics{handlerHerdMetrics: handlerHerdMetrics{seen: make(chan struct{}, clients)}}
					entered, release := make(chan struct{}), make(chan struct{})
					var releaseOnce sync.Once
					releaseFirst := func() { releaseOnce.Do(func() { close(release) }) }
					defer releaseFirst()
					var calls atomic.Int64
					payload := strings.Repeat("p", 4096)
					h := NewHandlerWithOptions(backend, nativeMissStatsOrigin(&calls, entered, release, failedStore, payload), &HandlerOptions{MissCoordinator: coord})
					h.SetMetrics(metrics)
					done := make(chan *httptest.ResponseRecorder, clients)
					for range clients {
						go func() {
							rec := httptest.NewRecorder()
							h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://origin.test/native-stats", nil))
							done <- rec
						}()
					}
					featureAwait(t, entered)
					for range clients {
						featureAwait(t, metrics.seen)
					}
					releaseFirst()
					for range clients {
						assertReviewVaryBody(t, featureAwait(t, done), payload)
					}
					h.writes.Wait()
					assertNativeMissStats(t, c, wrapper, coord, metrics, &calls, clients, failedStore)
				})
			}
		}
	}
}

func nativeMissStatsOrigin(calls *atomic.Int64, entered, release chan struct{}, failedStore bool, payload string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		policy := "no-store"
		if failedStore {
			policy = "max-age=3600"
		}
		w.Header().Set("Cache-Control", policy)
		w.Header().Set("Content-Length", "4096")
		_, _ = io.WriteString(w, payload)
	}
}

func assertNativeMissStats(t *testing.T, c *cache, wrapper *exportedNativeRecheckCache, coord *handlerHerdCoordinator, metrics *nativeMissStatsMetrics, calls *atomic.Int64, clients int64, failedStore bool) {
	t.Helper()
	if calls.Load() != clients || coord.acquired.Load() != clients || coord.released.Load() != clients {
		t.Fatalf("fills lost bounded ownership: calls=%d leases=%d/%d", calls.Load(), coord.acquired.Load(), coord.released.Load())
	}
	if wrapper != nil && wrapper.calls.Load() < clients {
		t.Fatalf("wrapper quiet lookup was bypassed: rechecks=%d", wrapper.calls.Load())
	}
	stats := c.Stats()
	if stats.MissCount != clients || stats.HitCount != 0 || stats.ItemCount != 0 || metrics.misses.Load() != clients {
		t.Fatalf("internal rechecks inflated client misses: native=%+v handler=%d, want %d misses", stats, metrics.misses.Load(), clients)
	}
	wantFailures := int64(0)
	if failedStore {
		wantFailures = clients
	}
	if got := metrics.storeFailures.Load(); got != wantFailures {
		t.Fatalf("backend failure path not exercised: %d failures, want %d", got, wantFailures)
	}
}

func TestNativeRecheckPreservesDirectLookupAndIntegrityAccounting(t *testing.T) {
	c := integrityFeatureCache(t, t.TempDir(), IntegritySHA256).(*cache)
	rechecker, ok := any(c).(interface {
		RetrieveForRecheck(string) (*Resource, error)
	})
	if !ok {
		t.Fatal("native cache does not expose non-counting rechecks")
	}
	if _, err := c.Retrieve("absent"); !errors.Is(err, ErrNotFoundInCache) {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := rechecker.RetrieveForRecheck("absent"); !errors.Is(err, ErrNotFoundInCache) {
			t.Fatal(err)
		}
	}
	if err := c.Store(integrityFeatureResource("payload"), "present"); err != nil {
		t.Fatal(err)
	}
	assertNativeRecheckPresent(t, c, rechecker.RetrieveForRecheck, 0, "quiet recheck changed public lookup accounting")
	assertNativeRecheckPresent(t, c, c.Retrieve, 1, "ordinary successful lookup lost its hit count")
	assertNativeRecheckCorruption(t, c, rechecker.RetrieveForRecheck)
}

func assertNativeRecheckPresent(t *testing.T, c *cache, retrieve func(string) (*Resource, error), wantHits int64, message string) {
	t.Helper()
	res, err := retrieve("present")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Close()
	if stats := c.Stats(); stats.MissCount != 1 || stats.HitCount != wantHits {
		t.Fatalf("%s: %+v", message, stats)
	}
}

func assertNativeRecheckCorruption(t *testing.T, c *cache, retrieveForRecheck func(string) (*Resource, error)) {
	t.Helper()
	path := integrityFeaturePath(c.diskRoot, bodyPrefix, "present")
	if err := os.WriteFile(path, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := retrieveForRecheck("present"); !errors.Is(err, ErrNotFoundInCache) {
		t.Fatalf("recheck did not discard corruption: %v", err)
	}
	if stats := c.Stats(); stats.MissCount != 1 || stats.ItemCount != 0 {
		t.Fatalf("corruption recheck counted another miss or retained entry: %+v", stats)
	}
	if err := c.Store(integrityFeatureResource("payload"), "present"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Retrieve("present"); !errors.Is(err, ErrNotFoundInCache) {
		t.Fatalf("ordinary lookup did not discard corruption: %v", err)
	}
	if stats := c.Stats(); stats.MissCount != 2 || stats.HitCount != 1 || stats.ItemCount != 0 {
		t.Fatalf("direct corruption lookup lost its miss count: %+v", stats)
	}
}

func TestRecheckKeepsLegacyCacheInterfaceCompatible(t *testing.T) {
	inner := generationBudgetCache(t, false, 1<<20)
	legacy := plainCache{inner: inner} // Implements only the original Cache interface.
	h := NewSharedHandler(legacy, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		_, _ = io.WriteString(w, "legacy body")
	}))
	defer h.writes.Wait()
	for range 2 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://origin.test/legacy-stats", nil))
		h.writes.Wait()
		assertReviewVaryBody(t, rec, "legacy body")
	}
}

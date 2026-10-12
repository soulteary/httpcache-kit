package httpcache

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func assertRecheckStats(t *testing.T, c ExtendedCache, hits, misses int64) {
	t.Helper()
	stats := c.Stats()
	if stats.HitCount != hits || stats.MissCount != misses {
		t.Fatalf("backend hits=%d misses=%d; want hits=%d misses=%d", stats.HitCount, stats.MissCount, hits, misses)
	}
}

func TestMissCoordinatorRechecksDoNotInflateBackendStats(t *testing.T) {
	for _, mode := range []IntegrityMode{IntegrityDisabled, IntegritySHA256} {
		for _, failedStore := range []bool{false, true} {
			t.Run(stringBool(failedStore)+"/"+stringBool(mode == IntegritySHA256), func(t *testing.T) {
				assertUncachedHerdBackendStats(t, mode, failedStore)
			})
		}
	}
}

func assertUncachedHerdBackendStats(t *testing.T, mode IntegrityMode, failedStore bool) {
	t.Helper()
	const clients = 8
	config := &CacheConfig{Integrity: mode}
	if failedStore {
		config.MaxSize = 1 // The real backend refuses every response's admission.
	}
	cache := NewMemoryCacheWithConfig(config)
	t.Cleanup(func() { _ = cache.Close() })
	coordinator := newFeatureCoordinator()
	coordinator.entered = make(chan string, clients)
	upstream, entered, release, calls := retryBlockedOrigin(failedStore)
	metrics := &retryMissMetrics{}
	h := NewHandlerWithOptions(cache, upstream, &HandlerOptions{MissCoordinator: coordinator})
	h.SetMetrics(metrics)
	defer h.writes.Wait()
	defer release()
	responses := make(chan retryClientResponse, clients)
	retryStartClient(h, context.Background(), 0, responses)
	featureAwait(t, entered)
	waiting := make(chan struct{}, clients-1)
	for id := 1; id < clients; id++ {
		ctx := &retryWaitingContext{Context: context.Background(), waiting: waiting}
		retryStartClient(h, ctx, id, responses)
	}
	for range clients - 1 {
		featureAwait(t, waiting)
	}
	release()
	assertRetryClientResponses(t, responses, clients, failedStore)
	h.writes.Wait()
	assertRetryCoordinationComplete(t, h, coordinator, metrics, calls.Load(), clients)
	assertRecheckStats(t, cache, 0, clients)
}

func TestInternalMissRecheckPreservesPublicRetrieveAccounting(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(stringBool(disk), func(t *testing.T) {
			cache := generationBudgetCache(t, disk, 1<<20)
			h := NewHandler(cache, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			req, err := newCacheRequest(httptest.NewRequest(http.MethodGet, "http://cache.example/recheck", nil))
			if err != nil {
				t.Fatal(err)
			}
			assertRecheckMissing(t, h, req)
			assertRecheckStats(t, cache, 0, 0)
			if _, err := cache.Retrieve(req.Key.String()); err != ErrNotFoundInCache {
				t.Fatalf("public miss: %v", err)
			}
			assertRecheckStats(t, cache, 0, 1)
			if err := cache.Store(integrityFeatureResource("cached response"), req.Key.String()); err != nil {
				t.Fatal(err)
			}
			res, err := h.recheckMiss(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Close()
			assertRecheckStats(t, cache, 0, 1)
			integrityFeatureBody(t, cache, req.Key.String(), "cached response")
			assertRecheckStats(t, cache, 1, 1)
		})
	}
}

func assertRecheckMissing(t *testing.T, h *Handler, req *cacheRequest) {
	t.Helper()
	res, err := h.recheckMiss(req)
	if res != nil || err != ErrNotFoundInCache {
		t.Fatalf("recheck returned resource=%v error=%v; want a miss", res, err)
	}
}

func TestInternalMissRecheckVerifiesAndDiscardsCorruption(t *testing.T) {
	for _, prefix := range []string{bodyPrefix, headerPrefix} {
		t.Run(prefix, func(t *testing.T) {
			dir := t.TempDir()
			cache := integrityFeatureCache(t, dir, IntegritySHA256)
			h := NewHandler(cache, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			req, err := newCacheRequest(httptest.NewRequest(http.MethodGet, "http://cache.example/corrupt-recheck", nil))
			if err != nil {
				t.Fatal(err)
			}
			key := req.Key.String()
			storeCorruptRecheckEntry(t, cache, dir, key, prefix)
			assertRecheckMissing(t, h, req)
			assertRecheckStats(t, cache, 0, 0)
			for _, entryPrefix := range []string{bodyPrefix, headerPrefix} {
				if _, err := os.Stat(integrityFeaturePath(dir, entryPrefix, key)); !os.IsNotExist(err) {
					t.Fatalf("corrupt %s survived internal recheck: %v", entryPrefix, err)
				}
			}
			integrityFeatureMiss(t, cache, key)
			assertRecheckStats(t, cache, 0, 1)
			storeCorruptRecheckEntry(t, cache, dir, key, prefix)
			integrityFeatureMiss(t, cache, key)
			assertRecheckStats(t, cache, 0, 2)
		})
	}
}

func storeCorruptRecheckEntry(t *testing.T, cache Cache, dir, key, prefix string) {
	t.Helper()
	if err := cache.Store(integrityFeatureResource("trusted response"), key); err != nil {
		t.Fatal(err)
	}
	kind := "same-length-body"
	if prefix == headerPrefix {
		kind = "same-length-header"
	}
	integrityFeatureTamperFile(t, dir, key, kind)
}

func TestInternalMissRecheckRetainsHeadVarySelection(t *testing.T) {
	cache := NewMemoryCacheWithConfig(&CacheConfig{Integrity: IntegritySHA256})
	t.Cleanup(func() { _ = cache.Close() })
	get := httptest.NewRequest(http.MethodGet, "http://cache.example/head-recheck", nil)
	get.Header.Set("Accept-Language", "fr")
	key := NewRequestKey(get)
	variant := key.Vary("Accept-Language", get).String()
	res := integrityFeatureResource("French response")
	res.Header().Set("Vary", "Accept-Language")
	if err := cache.Store(res, key.String(), variant); err != nil {
		t.Fatal(err)
	}
	head := get.Clone(get.Context())
	head.Method = http.MethodHead
	req, err := newCacheRequest(head)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(cache, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	found, err := h.recheckMiss(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = found.Close()
	if req.discoveryKey != key.String() || req.servedKey != variant {
		t.Fatalf("recheck selected discovery=%q variant=%q; want %q and %q", req.discoveryKey, req.servedKey, key.String(), variant)
	}
	assertRecheckStats(t, cache, 0, 0)
	found, err = h.lookup(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = found.Close()
	assertRecheckStats(t, cache, 2, 1)
}

func TestInternalMissRecheckHonorsOpaqueCacheRetrieve(t *testing.T) {
	failure := errors.New("opaque lookup failure")
	cache := &serveFlowLookupFailureCache{Cache: newServeFlowCache(t), failure: failure}
	h := NewHandler(cache, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req, err := newCacheRequest(httptest.NewRequest(http.MethodGet, "http://cache.example/opaque-recheck", nil))
	if err != nil {
		t.Fatal(err)
	}
	assertRecheckMissing(t, h, req)
	if _, err := h.recheckMiss(req); !errors.Is(err, failure) || cache.retrieves.Load() != 2 {
		t.Fatalf("recheck bypassed opaque Retrieve: error=%v calls=%d", err, cache.retrieves.Load())
	}
}

func TestMissRecheckCanceledFollowerKeepsOriginalAccounting(t *testing.T) {
	cache := NewMemoryCacheWithConfig(&CacheConfig{})
	t.Cleanup(func() { _ = cache.Close() })
	coordinator := newFeatureCoordinator()
	upstream, entered, release, calls := retryBlockedOrigin(false)
	metrics := &retryMissMetrics{}
	h := NewHandlerWithOptions(cache, upstream, &HandlerOptions{MissCoordinator: coordinator})
	h.SetMetrics(metrics)
	defer h.writes.Wait()
	defer release()
	responses := make(chan retryClientResponse, 2)
	retryStartClient(h, context.Background(), 0, responses)
	featureAwait(t, entered)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := make(chan struct{}, 1)
	retryStartClient(h, &retryWaitingContext{Context: ctx, waiting: waiting}, 1, responses)
	featureAwait(t, waiting)
	cancel()
	canceled := featureAwait(t, responses)
	if canceled.id != 1 || canceled.recorder.Body.Len() != 0 || calls.Load() != 1 {
		t.Fatalf("canceled follower received a response or reached origin: client=%d body=%q calls=%d", canceled.id, canceled.recorder.Body.String(), calls.Load())
	}
	assertRecheckStats(t, cache, 0, 2)
	release()
	assertRetryClientResponses(t, responses, 1, false)
	h.writes.Wait()
	assertRecheckStats(t, cache, 0, 2)
	if metrics.misses.Load() != 2 || coordinator.closed.Load() != 1 {
		t.Fatalf("cancellation accounting: handler misses=%d closed leases=%d", metrics.misses.Load(), coordinator.closed.Load())
	}
	assertNoRetryFlights(t, h)
}

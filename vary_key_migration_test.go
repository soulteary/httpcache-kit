package httpcache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

func TestVaryLegacyCacheCannotAliasVersionedVariant(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(stringBool(disk), func(t *testing.T) {
			testVaryLegacyCacheMigration(t, disk)
		})
	}
}

func testVaryLegacyCacheMigration(t *testing.T, disk bool) {
	t.Helper()
	dir := ""
	if disk {
		dir = t.TempDir()
	}
	cache := newVaryMigrationCache(t, dir)
	legacyRequest := httptest.NewRequest(http.MethodGet, "http://cache.example/migration", nil)
	legacyRequest.Header.Set("X-Variant", `["en"]`)
	baseKey := NewRequestKey(legacyRequest).String()
	// v4.0.2 serialized the first raw field value rather than its field lines.
	legacyKey := baseKey + varySeparator + strconv.Quote("X-Variant="+legacyRequest.Header.Get("X-Variant"))
	res := integrityFeatureResource("legacy JSON-header variant")
	res.Header().Set("Vary", "X-Variant")
	if err := cache.Store(res, baseKey, legacyKey); err != nil {
		t.Fatal(err)
	}
	if disk {
		if err := cache.Close(); err != nil {
			t.Fatal(err)
		}
		// Exercise persisted legacy records rather than an in-process index.
		cache = newVaryMigrationCache(t, dir)
	}
	assertVaryMigrationEntry(t, cache, baseKey, "legacy JSON-header variant")
	assertVaryMigrationEntry(t, cache, legacyKey, "legacy JSON-header variant")

	var calls atomic.Int64
	h := NewSharedHandler(cache, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("Vary", "X-Variant")
		_, _ = io.WriteString(w, "current plain-header variant")
	}))
	defer h.writes.Wait()
	request := httptest.NewRequest(http.MethodGet, "http://cache.example/migration", nil)
	request.Header.Set("X-Variant", "en")
	assertVaryMigrationLookupMiss(t, h, request)

	assertVaryMigrationResponse(t, h, request, "MISS", "current plain-header variant")
	assertVaryMigrationResponse(t, h, request, "HIT", "current plain-header variant")
	head := request.Clone(request.Context())
	head.Method = http.MethodHead
	assertVaryMigrationResponse(t, h, head, "HIT", "")
	if calls.Load() != 1 {
		t.Fatalf("origin calls = %d; want one refill followed by GET/HEAD hits", calls.Load())
	}
	assertVaryMigrationEntry(t, cache, legacyKey, "legacy JSON-header variant")
}

func newVaryMigrationCache(t *testing.T, dir string) ExtendedCache {
	t.Helper()
	config := DefaultCacheConfig().WithCleanupInterval(0)
	var cache ExtendedCache
	if dir == "" {
		cache = NewMemoryCacheWithConfig(config)
	} else {
		var err error
		cache, err = NewDiskCacheWithConfig(dir, config)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = cache.Close() })
	return cache
}

func assertVaryMigrationEntry(t *testing.T, cache Cache, key, want string) {
	t.Helper()
	res, err := cache.Retrieve(key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Close() }()
	body, err := io.ReadAll(res)
	if err != nil || string(body) != want || res.Header().Get("Vary") != "X-Variant" {
		t.Fatalf("legacy setup/retrieval = %q, %v, %v; want %q", body, err, res.Header(), want)
	}
}

func assertVaryMigrationLookupMiss(t *testing.T, h *Handler, request *http.Request) {
	t.Helper()
	req, err := newCacheRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.lookup(req)
	if res != nil {
		_ = res.Close()
	}
	if res != nil || err != ErrNotFoundInCache {
		t.Fatalf("legacy discovery entry did not yield a new-variant miss: resource=%v error=%v", res, err)
	}
}

func assertVaryMigrationResponse(t *testing.T, h *Handler, request *http.Request, cacheStatus, body string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request.Clone(request.Context()))
	h.writes.Wait()
	if rec.Code != http.StatusOK || rec.Header().Get(CacheHeader) != cacheStatus || rec.Body.String() != body {
		t.Fatalf("response = %d %q %q; want 200 %s %q", rec.Code, rec.Header().Get(CacheHeader), rec.Body.String(), cacheStatus, body)
	}
}

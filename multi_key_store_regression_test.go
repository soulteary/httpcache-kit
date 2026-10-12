package httpcache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type multiKeyCancelBody struct {
	*bytes.Reader
	cancel context.CancelFunc
	seeks  int
	stopAt int
}

func (b *multiKeyCancelBody) Seek(offset int64, whence int) (int64, error) {
	b.seeks++
	if b.seeks == b.stopAt {
		b.cancel()
	}
	return b.Reader.Seek(offset, whence)
}

func (b *multiKeyCancelBody) Close() error { return nil }

type multiKeyRejectBudget struct {
	budget *featureBudget
	body   atomic.Int64
	reject int64
}

func (b *multiKeyRejectBudget) NewLease(ctx context.Context, kind StageKind) (StagingLease, error) {
	if kind == StageCacheBody && b.body.Add(1) == b.reject {
		return nil, ErrStagingBudgetExceeded
	}
	return b.budget.NewLease(ctx, kind)
}

func multiKeyStoreResource(body, etag string) *Resource {
	res := generationBudgetResource(body, etag)
	res.Header().Set("Vary", "Accept-Language")
	res.Header().Set("Date", Clock().Format(http.TimeFormat))
	return res
}

func multiKeyStoreCache(t *testing.T, disk bool, integrity IntegrityMode) *cache {
	t.Helper()
	config := &CacheConfig{MaxSize: 1 << 20, Integrity: integrity}
	if !disk {
		c := NewMemoryCacheWithConfig(config).(*cache)
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	c, err := NewDiskCacheWithConfig(t.TempDir(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*cache)
}

func seedMultiKeyEvictedBase(t *testing.T, c *cache, req *http.Request) (string, string) {
	t.Helper()
	key := NewRequestKey(req)
	base, variant := key.String(), key.Vary("Accept-Language", req).String()
	// A large old representation leaves enough capacity for both small new
	// keys after LRU pressure evicts only the older discovery base.
	if err := c.Store(multiKeyStoreResource(strings.Repeat("old-", 1024), `"old"`), base, variant); err != nil {
		t.Fatal(err)
	}
	c.config.MaxSize = c.Stats().TotalSize
	if err := c.Store(generationBudgetResource("pressure", `"pressure"`), "pressure"); err != nil {
		t.Fatal(err)
	}
	integrityFeatureMiss(t, c, base)
	assertMultiKeyStoreEntry(t, c, variant, strings.Repeat("old-", 1024), false)
	return base, variant
}

func assertMultiKeyStoreEntry(t *testing.T, c Cache, key, body string, stale bool) {
	t.Helper()
	res, err := c.Retrieve(key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Close() }()
	got, err := io.ReadAll(res)
	if err != nil || string(got) != body || res.IsStale() != stale {
		t.Fatalf("entry %q: body=%q stale=%t err=%v; want body=%q stale=%t", key, got, res.IsStale(), err, body, stale)
	}
}

func failMultiKeyReplacement(t *testing.T, c *cache, failure, base, variant string, stopAt int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := multiKeyStoreResource("new", `"new"`)
	wantErr := error(context.Canceled)
	if failure == "cancellation" {
		res = NewResource(http.StatusOK, &multiKeyCancelBody{Reader: bytes.NewReader([]byte("new")), cancel: cancel, stopAt: stopAt}, res.Header())
	} else {
		budget := &featureBudget{limit: 1 << 20}
		c.config.StagingBudget = &multiKeyRejectBudget{budget: budget, reject: int64(stopAt)}
		t.Cleanup(func() { budget.assertReleased(t) })
		wantErr = ErrStagingBudgetExceeded
	}
	if err := c.StoreContext(ctx, res, base, variant); !errors.Is(err, wantErr) {
		t.Fatalf("replacement error=%v, want %v", err, wantErr)
	}
	c.lruMutex.Lock()
	reserved := c.reservedSize
	reservations := len(c.reservations)
	c.lruMutex.Unlock()
	if reserved != 0 || reservations != 0 {
		t.Fatalf("failed multi-key store retained reservations: bytes=%d entries=%d", reserved, reservations)
	}
}

func TestMultiKeyStoreFailureKeepsVaryDiscoveryAtomicAcrossRestart(t *testing.T) {
	for _, tc := range []struct {
		name, failure string
		disk          bool
	}{
		{"memory-cancellation", "cancellation", false},
		{"disk-cancellation", "cancellation", true},
		{"disk-budget", "budget", true},
	} {
		for _, integrity := range []IntegrityMode{IntegrityDisabled, IntegritySHA256} {
			t.Run(tc.name+"/integrity="+stringBool(integrity == IntegritySHA256), func(t *testing.T) {
				c := multiKeyStoreCache(t, tc.disk, integrity)
				req := httptest.NewRequest(http.MethodGet, "http://cache.example/multi-key", nil)
				req.Header.Set("Accept-Language", "en")
				base, variant := seedMultiKeyEvictedBase(t, c, req)
				failMultiKeyReplacement(t, c, tc.failure, base, variant, 2)
				assertMultiKeyFailedTransaction(t, c, base, variant, tc.disk)
				c = reopenMultiKeyStoreCache(t, c)
				assertMultiKeyFailedTransaction(t, c, base, variant, tc.disk)
				assertMultiKeyStoreRefetch(t, c, req, base, variant)
			})
		}
	}
}

func assertMultiKeyFailedTransaction(t *testing.T, c *cache, base, variant string, disk bool) {
	t.Helper()
	// No newly prepared discovery base can become visible after a rejected
	// transaction. Native staging leaves the old variant intact; an in-place
	// VFS transaction must discard it along with the overwritten base.
	integrityFeatureMiss(t, c, base)
	if disk {
		assertMultiKeyStoreEntry(t, c, variant, strings.Repeat("old-", 1024), false)
	} else {
		integrityFeatureMiss(t, c, variant)
	}
}

func reopenMultiKeyStoreCache(t *testing.T, previous *cache) *cache {
	t.Helper()
	if err := previous.Close(); err != nil {
		t.Fatal(err)
	}
	config := *previous.config
	config.StagingBudget = nil
	var c ExtendedCache
	if previous.diskRoot == "" {
		c = NewVFSCacheWithConfig(previous.fs, &config)
	} else {
		var err error
		c, err = NewDiskCacheWithConfig(previous.diskRoot, &config)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*cache)
}

func assertMultiKeyStoreRefetch(t *testing.T, c *cache, req *http.Request, base, variant string) {
	t.Helper()
	var calls atomic.Int64
	h := NewSharedHandler(c, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		for name, values := range multiKeyStoreResource("new", `"new"`).Header() {
			w.Header()[name] = values
		}
		_, _ = io.WriteString(w, "new")
	}))
	defer h.writes.Wait()
	// The injected failure applies only to the rejected replacement.
	c.config.StagingBudget = nil
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	h.writes.Wait()
	if rec.Code != http.StatusOK || rec.Body.String() != "new" || calls.Load() != 1 {
		t.Fatalf("failed transaction bypassed discovery refetch: status=%d body=%q calls=%d", rec.Code, rec.Body.String(), calls.Load())
	}
	assertMultiKeyStoreEntry(t, c, base, "new", false)
	assertMultiKeyStoreEntry(t, c, variant, "new", false)
}

func TestMultiKeyFirstFailurePreservesExistingFreshEntries(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(stringBool(disk), func(t *testing.T) {
			c := multiKeyStoreCache(t, disk, IntegritySHA256)
			const base, variant = "first-failure-base", "first-failure-variant"
			if err := c.Store(multiKeyStoreResource("old", `"old"`), base, variant); err != nil {
				t.Fatal(err)
			}
			failMultiKeyReplacement(t, c, "cancellation", base, variant, 1)
			assertMultiKeyStoreEntry(t, c, base, "old", false)
			assertMultiKeyStoreEntry(t, c, variant, "old", false)
		})
	}
}

package httpcache

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type replacementNativeCache struct {
	*cache
	fail atomic.Bool
}

func (c *replacementNativeCache) StoreContext(ctx context.Context, res *Resource, keys ...string) error {
	if c.fail.Load() {
		return errors.New("replacement backend write failed")
	}
	return c.cache.StoreContext(ctx, res, keys...)
}

// This backend does not expose generations or order invalidation against
// writes. The handler must supply the precise stale/fresh evidence itself.
type replacementOpaqueCache struct{ *replacementNativeCache }

func (c replacementOpaqueCache) Invalidate(...string) {}
func (c replacementOpaqueCache) Retrieve(key string) (*Resource, error) {
	res, err := c.cache.Retrieve(key)
	if res != nil {
		res.SetStoredAt(time.Time{})
	}
	return res, err
}

// Embedding only Cache hides StaleAt/StaleSnapshot even when its concrete
// backend implements them, while retaining the contextual Store extension.
type replacementOpaqueAdapter struct {
	Cache
	store ContextStore
}

func (c replacementOpaqueAdapter) StoreContext(ctx context.Context, res *Resource, keys ...string) error {
	return c.store.StoreContext(ctx, res, keys...)
}

func replacementCache(t *testing.T, disk, opaque bool, budget StagingBudget) (*replacementNativeCache, Cache) {
	t.Helper()
	config := DefaultCacheConfig().WithCleanupInterval(0)
	config.StagingBudget = budget
	var backend ExtendedCache
	if disk {
		var err error
		backend, err = NewDiskCacheWithConfig(t.TempDir(), config)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		backend = NewMemoryCacheWithConfig(config)
	}
	native := &replacementNativeCache{cache: backend.(*cache)}
	t.Cleanup(func() { _ = backend.Close() })
	if !opaque {
		return native, native
	}
	wrapped := replacementOpaqueCache{native}
	return native, replacementOpaqueAdapter{Cache: wrapped, store: wrapped}
}

func reviewGenerationRequest(h *Handler, force bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "http://origin.test/replacement", nil)
	if force {
		r.Header.Set("Cache-Control", "min-fresh=7200")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	h.writes.Wait()
	return rec
}

type replacementCancelBudget struct {
	cancel context.CancelFunc
}

func (b replacementCancelBudget) NewLease(ctx context.Context, _ StageKind) (StagingLease, error) {
	// Cancel before any temporary body can be admitted, rather than racing
	// cancellation after all client bytes have already reached valid storage.
	b.cancel()
	return nil, ctx.Err()
}

func TestValidationReplacementFencesPreviousGenerationBeforeAdmission(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, opaque := range []bool{false, true} {
			for _, validators := range []bool{false, true} {
				for _, outcome := range []string{"stored", "handler-budget", "cache-budget", "backend-failure", "client-canceled"} {
					if outcome == "cache-budget" && !disk {
						continue // Memory publication does not stage disk copies.
					}
					t.Run(stringBool(disk)+"/opaque="+strconv.FormatBool(opaque)+"/validators="+strconv.FormatBool(validators)+"/"+outcome, func(t *testing.T) {
						testReplacementAdmission(t, disk, opaque, validators, outcome)
					})
				}
			}
		}
	}
}

const replacementCurrentBody = "current representation after conditional validation"

func replacementAdmissionOrigin(t *testing.T, changed *atomic.Bool, calls *atomic.Int64, validators bool) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, etag := "original representation", `"v1"`
		if changed.Load() {
			body, etag = replacementCurrentBody, `"v2"`
			if validators && calls.Load() == 2 && r.Header.Get("If-None-Match") != `"v1"` {
				t.Errorf("replacement was not a conditional validation: %v", r.Header)
			}
		}
		w.Header().Set("Date", Clock().Format(http.TimeFormat))
		w.Header().Set("Cache-Control", "max-age=3600")
		if validators {
			w.Header().Set("ETag", etag)
		}
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = io.WriteString(w, body)
	}
}

func replacementAdmissionFault(outcome string, native *replacementNativeCache, handlerBudget, backendBudget *featureBudget) {
	switch outcome {
	case "handler-budget":
		handlerBudget.mu.Lock()
		handlerBudget.limit, handlerBudget.peak = 1, 0
		handlerBudget.mu.Unlock()
	case "cache-budget":
		backendBudget.mu.Lock()
		backendBudget.limit, backendBudget.peak = 1, 0
		backendBudget.mu.Unlock()
	case "backend-failure":
		native.fail.Store(true)
	}
}

func replacementAdmissionResponse(h *Handler, handlerBudget *featureBudget, outcome string) *httptest.ResponseRecorder {
	if outcome != "client-canceled" {
		return reviewGenerationRequest(h, true)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "http://origin.test/replacement", nil).WithContext(ctx)
	r.Header.Set("Cache-Control", "min-fresh=7200")
	rec := httptest.NewRecorder()
	h.stagingBudget = replacementCancelBudget{cancel}
	h.ServeHTTP(rec, r)
	h.writes.Wait()
	h.stagingBudget = handlerBudget
	return rec
}

func testReplacementAdmission(t *testing.T, disk, opaque, validators bool, outcome string) {
	t.Helper()
	backendBudget, handlerBudget := &featureBudget{limit: 1 << 20}, &featureBudget{limit: 1 << 20}
	native, backend := replacementCache(t, disk, opaque, backendBudget)
	var changed atomic.Bool
	var calls atomic.Int64
	h := NewHandlerWithOptions(backend, replacementAdmissionOrigin(t, &changed, &calls, validators), &HandlerOptions{StagingBudget: handlerBudget, TempDir: t.TempDir()})
	h.Shared = true
	defer h.writes.Wait()
	if rec := reviewGenerationRequest(h, false); rec.Body.String() != "original representation" {
		t.Fatal(rec.Body.String())
	}
	changed.Store(true)
	replacementAdmissionFault(outcome, native, handlerBudget, backendBudget)
	rec := replacementAdmissionResponse(h, handlerBudget, outcome)
	if rec.Code != http.StatusOK || calls.Load() != 2 || (outcome != "client-canceled" && rec.Body.String() != replacementCurrentBody) {
		t.Fatalf("validation delivery: status=%d body=%q origins=%d", rec.Code, rec.Body.String(), calls.Load())
	}
	handlerBudget.assertReleased(t)
	backendBudget.assertReleased(t)
	for _, budget := range []*featureBudget{handlerBudget, backendBudget} {
		budget.mu.Lock()
		budget.limit = 1 << 20
		budget.mu.Unlock()
	}
	native.fail.Store(false)
	assertReplacementAdmissionReuse(t, h, &calls, outcome)
}

func assertReplacementAdmissionReuse(t *testing.T, h *Handler, calls *atomic.Int64, outcome string) {
	t.Helper()
	wantCalls := int64(3)
	if outcome == "stored" {
		wantCalls = 2
	}
	rec := reviewGenerationRequest(h, false)
	if rec.Body.String() != replacementCurrentBody || calls.Load() != wantCalls {
		t.Fatalf("obsolete generation reused or successful Store not reusable: body=%q origins=%d want=%d headers=%v", rec.Body.String(), calls.Load(), wantCalls, rec.Header())
	}
	rec = reviewGenerationRequest(h, false)
	if rec.Body.String() != replacementCurrentBody || rec.Header().Get(CacheHeader) != "HIT" || calls.Load() != wantCalls {
		t.Fatalf("replacement repeats validation: body=%q origins=%d headers=%v", rec.Body.String(), calls.Load(), rec.Header())
	}
}

func TestValidationReplacementCannotCrossConcurrentUnsafeInvalidation(t *testing.T) {
	for _, opaque := range []bool{false, true} {
		t.Run("opaque="+strconv.FormatBool(opaque), func(t *testing.T) {
			_, backend := replacementCache(t, true, opaque, nil)
			var version atomic.Int64
			version.Store(1)
			var calls atomic.Int64
			entered, release := make(chan struct{}), make(chan struct{})
			h := NewSharedHandler(backend, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					version.Store(3)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				calls.Add(1)
				generation := version.Load()
				if generation == 2 {
					close(entered)
					<-release
				}
				w.Header().Set("Date", Clock().Format(http.TimeFormat))
				w.Header().Set("Cache-Control", "max-age=3600")
				w.Header().Set("ETag", `"v`+strconv.FormatInt(generation, 10)+`"`)
				_, _ = io.WriteString(w, "body-v"+strconv.FormatInt(generation, 10))
			}))
			defer h.writes.Wait()
			_ = reviewGenerationRequest(h, false)
			version.Store(2)
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- reviewGenerationRequest(h, true) }()
			featureAwait(t, entered)
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://origin.test/replacement", nil))
			close(release)
			var rec *httptest.ResponseRecorder
			select {
			case rec = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("conditional validation did not finish")
			}
			if rec.Body.String() != "body-v2" {
				t.Fatal(rec.Body.String())
			}
			rec = reviewGenerationRequest(h, false)
			if rec.Body.String() != "body-v3" || calls.Load() != 3 {
				t.Fatalf("response crossed mutation barrier: body=%q origins=%d headers=%v", rec.Body.String(), calls.Load(), rec.Header())
			}
			rec = reviewGenerationRequest(h, false)
			if rec.Body.String() != "body-v3" || rec.Header().Get(CacheHeader) != "HIT" || calls.Load() != 3 {
				t.Fatalf("post-mutation response did not remain fresh: body=%q origins=%d headers=%v", rec.Body.String(), calls.Load(), rec.Header())
			}
		})
	}
}

func TestValidationReplacementPreservesUpstreamDelayInAge(t *testing.T) {
	oldClock := Clock
	var elapsed atomic.Int64
	epoch := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	Clock = func() time.Time { return epoch.Add(time.Duration(elapsed.Add(1))) }
	defer func() { Clock = oldClock }()
	_, backend := replacementCache(t, true, true, nil)
	var calls atomic.Int64
	h := NewSharedHandler(backend, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call == 2 {
			// The response's Date is current, so only response delay plus Age
			// preserves these five seconds spent awaiting origin headers.
			elapsed.Add(int64(5 * time.Second))
			w.Header().Set("Age", "7")
		}
		w.Header().Set("Date", Clock().Format(http.TimeFormat))
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("ETag", `"v`+strconv.FormatInt(call, 10)+`"`)
		_, _ = io.WriteString(w, strings.Repeat("payload", int(call)))
	}))
	defer h.writes.Wait()
	_ = reviewGenerationRequest(h, false)
	_ = reviewGenerationRequest(h, true)
	rec := reviewGenerationRequest(h, false)
	age, err := strconv.Atoi(rec.Header().Get("Age"))
	if err != nil || age < 12 || age > 13 || calls.Load() != 2 || rec.Header().Get(CacheHeader) != "HIT" {
		t.Fatalf("validation age lost origin delay or repeats fetch: age=%d err=%v origins=%d headers=%v", age, err, calls.Load(), rec.Header())
	}
}

func TestValidationFenceAllows304AndOpaqueFullRefetch(t *testing.T) {
	for _, opaque := range []bool{false, true} {
		for _, outcome := range []string{"unchanged", "changed-vary", "withdrawn-policy", "validation-error"} {
			t.Run("opaque="+strconv.FormatBool(opaque)+"/"+outcome, func(t *testing.T) {
				_, backend := replacementCache(t, true, opaque, nil)
				var calls atomic.Int64
				h := NewSharedHandler(backend, replacementFenceOrigin(&calls, outcome))
				defer h.writes.Wait()
				_ = reviewGenerationRequest(h, false)
				rec := reviewGenerationRequest(h, true)
				if outcome == "validation-error" {
					if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "origin unavailable" {
						t.Fatalf("origin error replaced with predecessor: status=%d body=%q", rec.Code, rec.Body.String())
					}
				} else if rec.Body.String() != "same body" {
					t.Fatal(rec.Body.String())
				}
				wantCalls := int64(2)
				if outcome != "unchanged" {
					wantCalls = 3
				}
				rec = reviewGenerationRequest(h, false)
				if rec.Body.String() != "same body" || rec.Header().Get(CacheHeader) != "HIT" || calls.Load() != wantCalls {
					t.Fatalf("validation fence poisoned freshness: body=%q origins=%d want=%d headers=%v", rec.Body.String(), calls.Load(), wantCalls, rec.Header())
				}
				rec = reviewGenerationRequest(h, false)
				if rec.Body.String() != "same body" || rec.Header().Get(CacheHeader) != "HIT" || calls.Load() != wantCalls {
					t.Fatalf("304 did not clear conservative validation barrier: body=%q origins=%d headers=%v", rec.Body.String(), calls.Load(), rec.Header())
				}
			})
		}
	}
}

func replacementFenceOrigin(calls *atomic.Int64, outcome string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		call := calls.Add(1)
		w.Header().Set("Date", Clock().Format(http.TimeFormat))
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("ETag", `"same"`)
		if call == 2 {
			switch outcome {
			case "changed-vary":
				w.Header().Set("Vary", "Accept-Language")
			case "withdrawn-policy":
				w.Header().Set("Cache-Control", "no-store")
			case "validation-error":
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, "origin unavailable")
				return
			}
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if outcome == "validation-error" && call == 3 {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if outcome == "changed-vary" && call >= 3 {
			w.Header().Set("Vary", "Accept-Language")
		}
		_, _ = io.WriteString(w, "same body")
	}
}

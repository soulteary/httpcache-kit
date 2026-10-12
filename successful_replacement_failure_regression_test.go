package httpcache

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type replacementFailureCache struct {
	Cache
	reject atomic.Bool
}

func (c *replacementFailureCache) Store(res *Resource, keys ...string) error {
	if c.reject.Load() {
		return errors.New("replacement publication failed")
	}
	return c.Cache.Store(res, keys...)
}

type successfulReplacementOrigin struct {
	changed atomic.Bool
	calls   atomic.Int64
}

func (o *successfulReplacementOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.calls.Add(1)
	body, etag := "v1", `"v1"`
	if o.changed.Load() {
		body, etag = strings.Repeat("v2", 128), `"v2"`
	}
	w.Header().Set("Cache-Control", "max-age=3600")
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = io.WriteString(w, body)
}

func replacementRequest(h *Handler, validate bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://cache.example/replacement", nil)
	if validate {
		req.Header.Set("Cache-Control", "min-fresh=7200")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	h.writes.Wait()
	return rec
}

func assertReplacementResponse(t *testing.T, rec *httptest.ResponseRecorder, body string) {
	t.Helper()
	if rec.Code != http.StatusOK || rec.Body.String() != body {
		t.Fatalf("replacement response: status=%d body=%q, want 200 %q", rec.Code, rec.Body.String(), body)
	}
}

func TestSuccessfulReplacementFailureDoesNotReusePredecessor(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, failure := range []string{"handler stage", "cache store"} {
			t.Run(stringBool(disk)+"/"+failure, func(t *testing.T) {
				testSuccessfulReplacementFailure(t, disk, failure)
			})
		}
	}
}

func testSuccessfulReplacementFailure(t *testing.T, disk bool, failure string) {
	t.Helper()
	inner := generationBudgetCache(t, disk, 1<<20)
	failed := &replacementFailureCache{Cache: inner}
	var cache Cache = inner
	if failure == "cache store" {
		cache = failed // Also exercises the handler's opaque-cache generation record.
	}
	budget := &featureBudget{limit: 1 << 20}
	t.Cleanup(func() { budget.assertReleased(t) })
	origin := &successfulReplacementOrigin{}
	h := NewHandlerWithOptions(cache, origin, &HandlerOptions{StagingBudget: budget})
	h.Shared = true
	defer h.writes.Wait()
	assertReplacementResponse(t, replacementRequest(h, false), "v1")
	origin.changed.Store(true)
	failed.reject.Store(true)
	if failure == "handler stage" {
		budget.mu.Lock()
		budget.limit = 0
		budget.mu.Unlock()
	}
	current := strings.Repeat("v2", 128)
	assertReplacementResponse(t, replacementRequest(h, true), current)
	if origin.calls.Load() != 2 {
		t.Fatalf("full validation response repeated origin request: calls=%d, want 2", origin.calls.Load())
	}
	failed.reject.Store(false)
	budget.mu.Lock()
	budget.limit = 1 << 20
	budget.mu.Unlock()
	assertReplacementResponse(t, replacementRequest(h, false), current)
	assertReplacementResponse(t, replacementRequest(h, false), current)
	if origin.calls.Load() != 3 {
		t.Fatalf("replacement was not reusable after recovery: calls=%d, want 3", origin.calls.Load())
	}
}

type fullReplacementVaryOrigin struct {
	changed atomic.Bool
	calls   atomic.Int64
}

func (o *fullReplacementVaryOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.calls.Add(1)
	w.Header().Set("Cache-Control", "max-age=3600")
	w.Header().Set("ETag", `"old"`)
	w.Header().Set("Vary", "Accept-Language")
	body := "initial representation"
	if o.changed.Load() {
		w.Header().Set("ETag", `"current"`)
		w.Header().Set("Vary", "If-None-Match")
		body = "representation for " + r.Header.Get("If-None-Match")
	}
	_, _ = io.WriteString(w, body)
}

func fullReplacementVaryRequest(h *Handler, condition string, validate bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://cache.example/replacement-vary", nil)
	req.Header.Set("Accept-Language", "en")
	if condition != "" {
		req.Header.Set("If-None-Match", condition)
	}
	if validate {
		req.Header.Set("Cache-Control", "min-fresh=7200")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	h.writes.Wait()
	return rec
}

func TestSuccessfulReplacementVaryUsesActualValidationRequest(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(stringBool(disk), func(t *testing.T) {
			cache := generationBudgetCache(t, disk, 1<<20)
			origin := &fullReplacementVaryOrigin{}
			h := NewSharedHandler(cache, origin)
			defer h.writes.Wait()
			assertReplacementResponse(t, fullReplacementVaryRequest(h, "", false), "initial representation")
			origin.changed.Store(true)
			assertReplacementResponse(t, fullReplacementVaryRequest(h, `"client"`, true), `representation for "client"`)
			// The rewritten cache validator selected another representation.
			// Deliver and store only the response to the original request.
			assertReplacementResponse(t, fullReplacementVaryRequest(h, `"client"`, false), `representation for "client"`)
			assertReplacementResponse(t, fullReplacementVaryRequest(h, `"client"`, false), `representation for "client"`)
			if origin.calls.Load() != 3 {
				t.Fatalf("original-request variant was not reused: calls=%d, want 3", origin.calls.Load())
			}
		})
	}
}

func TestSuccessfulReplacementIsReusableAfterOneValidationFetch(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, opaque := range []bool{false, true} {
			t.Run(stringBool(disk)+"/opaque="+stringBool(opaque), func(t *testing.T) {
				inner := generationBudgetCache(t, disk, 1<<20)
				var cache Cache = inner
				if opaque {
					cache = impreciseCache{plainCache{inner: inner}}
				}
				origin := &successfulReplacementOrigin{}
				h := NewSharedHandler(cache, origin)
				defer h.writes.Wait()
				assertReplacementResponse(t, replacementRequest(h, false), "v1")
				origin.changed.Store(true)
				current := strings.Repeat("v2", 128)
				assertReplacementResponse(t, replacementRequest(h, true), current)
				assertReplacementResponse(t, replacementRequest(h, false), current)
				if origin.calls.Load() != 2 {
					t.Fatalf("full validation was not stored after its barrier: calls=%d, want 2", origin.calls.Load())
				}
			})
		}
	}
}

type concurrentReplacementOrigin struct {
	version atomic.Int64
	calls   atomic.Int64
	entered chan struct{}
	release chan struct{}
}

func (o *concurrentReplacementOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		o.version.Store(3)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	body, etag := "v1", `"v1"`
	switch o.version.Load() {
	case 2:
		body, etag = "v2", `"v2"`
	case 3:
		body, etag = "v3", `"v3"`
	}
	if o.calls.Add(1) == 2 {
		close(o.entered)
		<-o.release
	}
	w.Header().Set("Cache-Control", "max-age=3600")
	w.Header().Set("ETag", etag)
	_, _ = io.WriteString(w, body)
}

func TestSuccessfulReplacementDoesNotEraseConcurrentMutation(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, opaque := range []bool{false, true} {
			t.Run(stringBool(disk)+"/opaque="+stringBool(opaque), func(t *testing.T) {
				testConcurrentSuccessfulReplacement(t, disk, opaque)
			})
		}
	}
}

func testConcurrentSuccessfulReplacement(t *testing.T, disk, opaque bool) {
	t.Helper()
	inner := generationBudgetCache(t, disk, 1<<20)
	var cache Cache = inner
	if opaque {
		cache = impreciseCache{plainCache{inner: inner}}
	}
	origin := &concurrentReplacementOrigin{entered: make(chan struct{}), release: make(chan struct{})}
	h := NewSharedHandler(cache, origin)
	defer h.writes.Wait()
	assertReplacementResponse(t, replacementRequest(h, false), "v1")
	origin.version.Store(2)
	complete := make(chan *httptest.ResponseRecorder, 1)
	go func() { complete <- replacementRequest(h, true) }()
	featureAwait(t, origin.entered)
	mutation := httptest.NewRecorder()
	h.ServeHTTP(mutation, httptest.NewRequest(http.MethodPost, "http://cache.example/replacement", nil))
	close(origin.release)
	if mutation.Code != http.StatusNoContent {
		t.Fatalf("mutation status=%d, want 204", mutation.Code)
	}
	assertReplacementResponse(t, featureAwait(t, complete), "v2")
	assertReplacementResponse(t, replacementRequest(h, false), "v3")
	assertReplacementResponse(t, replacementRequest(h, false), "v3")
	if origin.calls.Load() != 3 {
		t.Fatalf("post-mutation generation did not replace the in-flight body: calls=%d, want 3", origin.calls.Load())
	}
}

type varyReplacementFailureOrigin struct{ successfulReplacementOrigin }

func (o *varyReplacementFailureOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.calls.Add(1)
	body := r.Header.Get("Accept-Language") + "/v1"
	if o.changed.Load() && r.Header.Get("Accept-Language") == "en" {
		body = "en/v2"
	}
	w.Header().Set("Cache-Control", "max-age=3600")
	w.Header().Set("Vary", "Accept-Language")
	w.Header().Set("ETag", `"`+body+`"`)
	if r.Header.Get("If-None-Match") == w.Header().Get("ETag") {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = io.WriteString(w, body)
}

func varyReplacementFailureRequest(h *Handler, language string, validate bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://cache.example/vary-replacement-failure", nil)
	req.Header.Set("Accept-Language", language)
	if validate {
		req.Header.Set("Cache-Control", "min-fresh=7200")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	h.writes.Wait()
	return rec
}

func TestSuccessfulReplacementFailureFencesOnlySelectedVaryResource(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, failure := range []string{"handler stage", "cache store", "opaque handler stage"} {
			t.Run(stringBool(disk)+"/"+failure, func(t *testing.T) {
				testVaryReplacementFailure(t, disk, failure)
			})
		}
	}
}

func testVaryReplacementFailure(t *testing.T, disk bool, failure string) {
	t.Helper()
	inner := generationBudgetCache(t, disk, 1<<20)
	failed := &replacementFailureCache{Cache: inner}
	var cache Cache = inner
	switch failure {
	case "cache store":
		cache = failed
	case "opaque handler stage":
		// This existing opaque-cache model cannot order or expose
		// invalidation. The handler must enforce its own variant marker.
		cache = &delayedStoreCache{inner: inner}
	}
	budget := &featureBudget{limit: 1 << 20}
	t.Cleanup(func() { budget.assertReleased(t) })
	origin := &varyReplacementFailureOrigin{}
	h := NewHandlerWithOptions(cache, origin, &HandlerOptions{StagingBudget: budget})
	h.Shared = true
	defer h.writes.Wait()
	assertReplacementResponse(t, varyReplacementFailureRequest(h, "en", false), "en/v1")
	assertReplacementResponse(t, varyReplacementFailureRequest(h, "fr", false), "fr/v1")
	origin.changed.Store(true)
	failed.reject.Store(true)
	if failure != "cache store" {
		budget.mu.Lock()
		budget.limit = 0
		budget.mu.Unlock()
	}
	assertReplacementResponse(t, varyReplacementFailureRequest(h, "en", true), "en/v2")
	assertReplacementResponse(t, varyReplacementFailureRequest(h, "fr", false), "fr/v1")
	if origin.calls.Load() != 3 {
		t.Fatalf("independent fresh variant was invalidated: calls=%d, want 3", origin.calls.Load())
	}
	failed.reject.Store(false)
	budget.mu.Lock()
	budget.limit = 1 << 20
	budget.mu.Unlock()
	assertReplacementResponse(t, varyReplacementFailureRequest(h, "en", false), "en/v2")
	assertReplacementResponse(t, varyReplacementFailureRequest(h, "en", false), "en/v2")
	if origin.calls.Load() != 4 {
		t.Fatalf("selected predecessor remained reusable or replacement stayed stale: calls=%d, want 4", origin.calls.Load())
	}
}

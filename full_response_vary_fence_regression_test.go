package httpcache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

type fullVaryPolicyOrigin struct {
	version atomic.Int64
	calls   atomic.Int64
	newVary string
	entered chan struct{}
	release chan struct{}
}

func (o *fullVaryPolicyOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		o.version.Store(3)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	version := o.version.Load()
	call := o.calls.Add(1)
	// Block the replacement refetch, after the Vary-policy barrier. A POST
	// during this fetch must still supersede its publication.
	if call == 4 && o.entered != nil {
		close(o.entered)
		<-o.release
	}
	vary, body := "Accept-Language", r.Header.Get("Accept-Language")+"/v1"
	if version > 1 {
		vary, body = o.newVary, "encoded/v"+strconv.FormatInt(version, 10)
	}
	w.Header().Set("Cache-Control", "max-age=3600")
	w.Header().Set("ETag", strconv.Quote(body))
	if vary != "" {
		w.Header().Set("Vary", vary)
	}
	if r.Header.Get("If-None-Match") == strconv.Quote(body) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = io.WriteString(w, body)
}

func fullVaryPolicyRequest(language string, force bool) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://cache.example/full-vary-policy", nil)
	req.Header.Set("Accept-Language", language)
	req.Header.Set("Accept-Encoding", "gzip")
	if force {
		req.Header.Set("Cache-Control", "min-fresh=7200")
	}
	return req
}

func serveFullVaryPolicy(h *Handler, language string, force bool) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, fullVaryPolicyRequest(language, force))
	h.writes.Wait()
	return rec
}

func seedFullVaryPolicy(t *testing.T, h *Handler) {
	t.Helper()
	assertReplacementResponse(t, serveFullVaryPolicy(h, "en", false), "en/v1")
	assertReplacementResponse(t, serveFullVaryPolicy(h, "fr", false), "fr/v1")
}

func TestFullValidationVaryPolicyFailureFencesDiscovery(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, failure := range []string{"handler stage", "cache store", "opaque handler stage"} {
			for _, vary := range []string{"Accept-Encoding", ""} {
				t.Run(stringBool(disk)+"/"+failure+"/Vary="+vary, func(t *testing.T) {
					testFullVaryPolicyFailure(t, disk, failure, vary)
				})
			}
		}
	}
}

func testFullVaryPolicyFailure(t *testing.T, disk bool, failure, vary string) {
	t.Helper()
	inner := generationBudgetCache(t, disk, 1<<20)
	failed := &replacementFailureCache{Cache: inner}
	var cache Cache = inner
	switch failure {
	case "cache store":
		cache = failed
	case "opaque handler stage":
		cache = &delayedStoreCache{inner: inner}
	}
	budget := &featureBudget{limit: 1 << 20}
	t.Cleanup(func() {
		setFullVaryPolicyFailure(budget, failed, failure, false)
		budget.assertReleased(t)
	})
	origin := &fullVaryPolicyOrigin{newVary: vary}
	origin.version.Store(1)
	h := NewHandlerWithOptions(cache, origin, &HandlerOptions{StagingBudget: budget})
	h.Shared = true
	defer h.writes.Wait()
	seedFullVaryPolicy(t, h)
	origin.version.Store(2)
	setFullVaryPolicyFailure(budget, failed, failure, true)
	assertReplacementResponse(t, serveFullVaryPolicy(h, "en", true), "encoded/v2")
	assertFullVaryDiscoveryStale(t, h)
	// The old French entry was never selected for validation. The obsolete
	// discovery policy must nevertheless prevent it from being a fresh HIT.
	assertReplacementResponse(t, serveFullVaryPolicy(h, "fr", false), "encoded/v2")
	setFullVaryPolicyFailure(budget, failed, failure, false)
	assertReplacementResponse(t, serveFullVaryPolicy(h, "en", false), "encoded/v2")
	assertFullVaryPolicyHit(t, h, origin, vary)
}

func setFullVaryPolicyFailure(budget *featureBudget, failed *replacementFailureCache, failure string, reject bool) {
	failed.reject.Store(reject)
	if failure != "cache store" {
		budget.mu.Lock()
		budget.limit = 1 << 20
		if reject {
			budget.limit = 0
		}
		budget.mu.Unlock()
	}
}

func assertFullVaryDiscoveryStale(t *testing.T, h *Handler) {
	t.Helper()
	req, err := newCacheRequest(fullVaryPolicyRequest("fr", false))
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.lookup(req)
	if err == ErrNotFoundInCache {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Close() }()
	if !res.IsStale() {
		t.Fatal("old unselected language remained fresh through the obsolete discovery policy")
	}
}

func assertFullVaryPolicyHit(t *testing.T, h *Handler, origin *fullVaryPolicyOrigin, vary string) {
	t.Helper()
	calls := origin.calls.Load()
	rec := serveFullVaryPolicy(h, "fr", false)
	assertReplacementResponse(t, rec, "encoded/v2")
	if rec.Header().Get(CacheHeader) != "HIT" || rec.Header().Get("Vary") != vary || origin.calls.Load() != calls {
		t.Fatalf("new discovery policy was not reusable: headers=%v calls=%d, want %d", rec.Header(), origin.calls.Load(), calls)
	}
}

func TestFullValidationVaryPolicyRebuildKeepsFetchOrdering(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, opaque := range []bool{false, true} {
			t.Run(stringBool(disk)+"/opaque="+stringBool(opaque), func(t *testing.T) {
				testFullVaryPolicyMutation(t, disk, opaque)
			})
		}
	}
}

func testFullVaryPolicyMutation(t *testing.T, disk, opaque bool) {
	t.Helper()
	inner := generationBudgetCache(t, disk, 1<<20)
	var cache Cache = inner
	if opaque {
		cache = &delayedStoreCache{inner: inner}
	}
	origin := &fullVaryPolicyOrigin{newVary: "Accept-Encoding", entered: make(chan struct{}), release: make(chan struct{})}
	origin.version.Store(1)
	h := NewSharedHandler(cache, origin)
	defer h.writes.Wait()
	seedFullVaryPolicy(t, h)
	origin.version.Store(2)
	complete := make(chan *httptest.ResponseRecorder, 1)
	go func() { complete <- serveFullVaryPolicy(h, "en", true) }()
	featureAwait(t, origin.entered)
	mutation := httptest.NewRecorder()
	h.ServeHTTP(mutation, httptest.NewRequest(http.MethodPost, "http://cache.example/full-vary-policy", nil))
	close(origin.release)
	if mutation.Code != http.StatusNoContent {
		t.Fatalf("mutation status=%d, want 204", mutation.Code)
	}
	assertReplacementResponse(t, featureAwait(t, complete), "encoded/v2")
	assertReplacementResponse(t, serveFullVaryPolicy(h, "fr", false), "encoded/v3")
	calls := origin.calls.Load()
	assertReplacementResponse(t, serveFullVaryPolicy(h, "en", false), "encoded/v3")
	if origin.calls.Load() != calls {
		t.Fatalf("post-mutation generation stayed stale: calls=%d, want %d", origin.calls.Load(), calls)
	}
}

func TestFullValidationNormalizedVaryPolicyKeepsSingleFetch(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(stringBool(disk), func(t *testing.T) {
			cache := generationBudgetCache(t, disk, 1<<20)
			origin := &fullVaryPolicyOrigin{newVary: "accept-language"}
			origin.version.Store(1)
			h := NewSharedHandler(cache, origin)
			defer h.writes.Wait()
			seedFullVaryPolicy(t, h)
			origin.version.Store(2)
			assertReplacementResponse(t, serveFullVaryPolicy(h, "en", true), "encoded/v2")
			assertReplacementResponse(t, serveFullVaryPolicy(h, "fr", false), "fr/v1")
			assertReplacementResponse(t, serveFullVaryPolicy(h, "en", false), "encoded/v2")
			if origin.calls.Load() != 3 {
				t.Fatalf("equivalent Vary policy caused a refetch or fenced another language: calls=%d, want 3", origin.calls.Load())
			}
		})
	}
}

func TestFullValidationChangedVaryPolicyRebuildsNewIndex(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(stringBool(disk), func(t *testing.T) {
			cache := generationBudgetCache(t, disk, 1<<20)
			origin := &fullVaryPolicyOrigin{newVary: "Accept-Encoding"}
			origin.version.Store(1)
			h := NewSharedHandler(cache, origin)
			defer h.writes.Wait()
			seedFullVaryPolicy(t, h)
			origin.version.Store(2)
			assertReplacementResponse(t, serveFullVaryPolicy(h, "en", true), "encoded/v2")
			if origin.calls.Load() != 4 {
				t.Fatalf("changed Vary did not refetch after its policy barrier: calls=%d, want 4", origin.calls.Load())
			}
			assertFullVaryPolicyHit(t, h, origin, "Accept-Encoding")
		})
	}
}

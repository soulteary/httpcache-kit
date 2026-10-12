package httpcache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

type fullStatusVaryOrigin struct {
	status  int
	newVary string
	changed atomic.Bool
	calls   atomic.Int64
}

func (o *fullStatusVaryOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.calls.Add(1)
	vary, body := "Accept-Language", r.Header.Get("Accept-Language")+"/v1"
	if o.changed.Load() {
		vary, body = o.newVary, "encoded/v2"
	}
	w.Header().Set("Cache-Control", "max-age=3600")
	w.Header().Set("Date", Clock().Format(http.TimeFormat))
	w.Header().Set("ETag", strconv.Quote(body))
	if vary != "" {
		w.Header().Set("Vary", vary)
	}
	if o.status >= 300 && o.status < 400 {
		w.Header().Set("Location", "http://cache.example/current")
	}
	// Redirects and errors retain precedence over matching client validators.
	w.WriteHeader(o.status)
	_, _ = io.WriteString(w, body)
}

func serveFullStatusVary(h *Handler, language string, force bool) *httptest.ResponseRecorder {
	req := fullVaryPolicyRequest(language, force)
	req.Header.Set("If-None-Match", `"encoded/v2"`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	h.writes.Wait()
	return rec
}

func assertFullStatusVary(t *testing.T, rec *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if rec.Code != status || rec.Body.String() != body {
		t.Fatalf("full replacement: status=%d body=%q, want %d %q", rec.Code, rec.Body.String(), status, body)
	}
	if status >= 300 && status < 400 && rec.Header().Get("Location") != "http://cache.example/current" {
		t.Fatalf("redirect replacement lost Location: %v", rec.Header())
	}
}

func TestCacheableNon2xxVaryReplacementFencesDiscovery(t *testing.T) {
	for _, status := range []int{http.StatusMultipleChoices, http.StatusMovedPermanently, http.StatusFound, http.StatusNotFound, http.StatusGone} {
		for _, disk := range []bool{false, true} {
			for _, failure := range []string{"handler stage", "cache store", "opaque handler stage"} {
				for _, vary := range []string{"Accept-Encoding", ""} {
					t.Run(strconv.Itoa(status)+"/"+stringBool(disk)+"/"+failure+"/Vary="+vary, func(t *testing.T) {
						testFullStatusVaryFailure(t, status, disk, failure, vary)
					})
				}
			}
		}
	}
}

func testFullStatusVaryFailure(t *testing.T, status int, disk bool, failure, vary string) {
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
	origin := &fullStatusVaryOrigin{status: status, newVary: vary}
	h := NewHandlerWithOptions(cache, origin, &HandlerOptions{StagingBudget: budget})
	h.Shared = true
	defer h.writes.Wait()
	assertFullStatusVary(t, serveFullStatusVary(h, "en", false), status, "en/v1")
	assertFullStatusVary(t, serveFullStatusVary(h, "fr", false), status, "fr/v1")
	origin.changed.Store(true)
	setFullVaryPolicyFailure(budget, failed, failure, true)
	assertFullStatusVary(t, serveFullStatusVary(h, "en", true), status, "encoded/v2")
	assertFullVaryDiscoveryStale(t, h)
	assertFullStatusVary(t, serveFullStatusVary(h, "fr", false), status, "encoded/v2")
	setFullVaryPolicyFailure(budget, failed, failure, false)
	assertFullStatusVary(t, serveFullStatusVary(h, "en", false), status, "encoded/v2")
	calls := origin.calls.Load()
	hit := serveFullStatusVary(h, "fr", false)
	assertFullStatusVary(t, hit, status, "encoded/v2")
	if hit.Header().Get(CacheHeader) != "HIT" || hit.Header().Get("Vary") != vary || origin.calls.Load() != calls {
		t.Fatalf("replacement policy was not reusable: headers=%v calls=%d, want %d", hit.Header(), origin.calls.Load(), calls)
	}
}

func TestUnstoreableErrorVaryDoesNotRepeatOrigin(t *testing.T) {
	cache := generationBudgetCache(t, false, 1<<20)
	var calls atomic.Int64
	origin := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("ETag", `"same"`)
		if calls.Add(1) > 1 {
			w.Header().Set("Vary", "Accept-Encoding")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "origin unavailable")
			return
		}
		_, _ = io.WriteString(w, "original")
	})
	h := NewSharedHandler(cache, origin)
	defer h.writes.Wait()
	assertReplacementResponse(t, serveFullVaryPolicy(h, "en", false), "original")
	assertFullStatusVary(t, serveFullVaryPolicy(h, "en", true), http.StatusServiceUnavailable, "origin unavailable")
	if calls.Load() != 2 {
		t.Fatalf("uncacheable error changed Vary and unnecessarily repeated origin: calls=%d", calls.Load())
	}
}

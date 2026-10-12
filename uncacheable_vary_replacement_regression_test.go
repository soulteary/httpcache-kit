package httpcache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

type uncacheableVaryReplacementOrigin struct {
	status int
	policy string
	calls  atomic.Int64
}

func (o *uncacheableVaryReplacementOrigin) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	switch o.calls.Add(1) {
	case 1:
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Vary", "Accept-Language")
		w.Header().Set("ETag", `"old"`)
		w.WriteHeader(o.status)
		_, _ = io.WriteString(w, "old representation")
	case 2:
		w.Header().Set("Cache-Control", o.policy)
		w.Header().Set("Vary", "Accept-Encoding")
		w.Header().Set("ETag", `"new"`)
		w.Header().Set("Location", "https://origin.test/received-replacement")
		w.WriteHeader(o.status)
		_, _ = io.WriteString(w, "received replacement")
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "unexpected replacement refetch")
	}
}

func TestUncacheableChangedVaryDeliversReceivedReplacement(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusMovedPermanently, http.StatusNotFound, http.StatusGone} {
		for _, policy := range []string{"no-store", "private, max-age=3600"} {
			for _, disk := range []bool{false, true} {
				for _, opaque := range []bool{false, true} {
					t.Run(strconv.Itoa(status)+"/"+policy+"/disk="+stringBool(disk)+"/opaque="+stringBool(opaque), func(t *testing.T) {
						testUncacheableVaryReplacement(t, status, policy, disk, opaque)
					})
				}
			}
		}
	}
}

func testUncacheableVaryReplacement(t *testing.T, status int, policy string, disk, opaque bool) {
	t.Helper()
	inner := generationBudgetCache(t, disk, 1<<20)
	var cache Cache = inner
	if opaque {
		cache = impreciseCache{plainCache{inner: inner}}
	}
	origin := &uncacheableVaryReplacementOrigin{status: status, policy: policy}
	h := NewSharedHandler(cache, origin)
	defer h.writes.Wait()
	first := serveFullVaryPolicy(h, "en", false)
	if first.Code != status || first.Body.String() != "old representation" {
		t.Fatalf("seed response: status=%d body=%q", first.Code, first.Body.String())
	}
	replacement := serveFullVaryPolicy(h, "en", true)
	assertUncacheableVaryReplacement(t, replacement, origin)
	assertSelectedVaryPredecessorStale(t, h)
	// A later request may try origin again, but it must not reuse the fenced
	// old representation or publish the uncacheable response as a fresh HIT.
	third := serveFullVaryPolicy(h, "en", false)
	if third.Code != http.StatusServiceUnavailable || third.Body.String() != "unexpected replacement refetch" || origin.calls.Load() != 3 {
		t.Fatalf("follow-up reused predecessor: status=%d body=%q calls=%d", third.Code, third.Body.String(), origin.calls.Load())
	}
}

func assertUncacheableVaryReplacement(t *testing.T, rec *httptest.ResponseRecorder, origin *uncacheableVaryReplacementOrigin) {
	t.Helper()
	if rec.Code != origin.status || rec.Body.String() != "received replacement" || origin.calls.Load() != 2 {
		t.Fatalf("uncacheable Vary replacement was discarded: status=%d body=%q calls=%d", rec.Code, rec.Body.String(), origin.calls.Load())
	}
	if rec.Header().Get("Cache-Control") != origin.policy || rec.Header().Get("Vary") != "Accept-Encoding" ||
		rec.Header().Get("Location") != "https://origin.test/received-replacement" {
		t.Fatalf("received replacement metadata changed: %v", rec.Header())
	}
}

func assertSelectedVaryPredecessorStale(t *testing.T, h *Handler) {
	t.Helper()
	req, err := newCacheRequest(fullVaryPolicyRequest("en", false))
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
		t.Fatal("uncacheable replacement left its selected predecessor fresh")
	}
}

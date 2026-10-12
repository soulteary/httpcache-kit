package httpcache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

const fullVaryFenceURL = "http://cache.example/full-vary-policy"

func fullVaryFenceRequest(h *Handler, language, encoding string, force bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, fullVaryFenceURL, nil)
	r.Header.Set("Accept-Language", language)
	r.Header.Set("Accept-Encoding", encoding)
	if force {
		r.Header.Set("Cache-Control", "min-fresh=7200")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	h.writes.Wait()
	return rec
}

func TestFullResponseVaryChangeFencesDiscoveryBeforeFailedPublication(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusMovedPermanently, http.StatusNotFound} {
		t.Run(strconv.Itoa(status), func(t *testing.T) { testFullVaryFenceStatus(t, status) })
	}
}

func testFullVaryFenceStatus(t *testing.T, status int) {
	t.Helper()
	for _, disk := range []bool{false, true} {
		for _, opaque := range []bool{false, true} {
			for _, selectedOnly := range []bool{false, true} {
				for _, failure := range []string{"handler stage", "backend store", "cache stage"} {
					if failure == "cache stage" && !disk {
						continue
					}
					t.Run(stringBool(disk)+"/opaque="+strconv.FormatBool(opaque)+"/selected-stale="+strconv.FormatBool(selectedOnly)+"/"+failure, func(t *testing.T) {
						testFullResponseVaryChangeFailure(t, status, disk, opaque, selectedOnly, failure)
					})
				}
			}
		}
	}
}

func fullVaryFenceOrigin(status int, changed *atomic.Bool, calls *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body := r.Header.Get("Accept-Language") + "/v1"
		w.Header().Set("Vary", "Accept-Language")
		if changed.Load() {
			w.Header().Set("Vary", "Accept-Encoding")
			body = r.Header.Get("Accept-Encoding") + "/v2"
		}
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("ETag", `"`+body+`"`)
		if r.Header.Get("If-None-Match") == w.Header().Get("ETag") {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
}

func testFullResponseVaryChangeFailure(t *testing.T, status int, disk, opaque, selectedOnly bool, failure string) {
	t.Helper()
	assertResponse := func(rec *httptest.ResponseRecorder, body string) {
		t.Helper()
		if rec.Code != status || rec.Body.String() != body {
			t.Fatalf("replacement response: status=%d body=%q, want %d %q", rec.Code, rec.Body.String(), status, body)
		}
	}
	handlerBudget, cacheBudget := &featureBudget{limit: 1 << 20}, &featureBudget{limit: 1 << 20}
	t.Cleanup(func() { handlerBudget.assertReleased(t); cacheBudget.assertReleased(t) })
	native, backend := replacementCache(t, disk, opaque, cacheBudget)
	var changed atomic.Bool
	var calls atomic.Int64
	origin := fullVaryFenceOrigin(status, &changed, &calls)
	h := NewHandlerWithOptions(backend, origin, &HandlerOptions{StagingBudget: handlerBudget})
	h.Shared = true
	defer h.writes.Wait()
	assertResponse(fullVaryFenceRequest(h, "en", "gzip", false), "en/v1")
	assertResponse(fullVaryFenceRequest(h, "fr", "gzip", false), "fr/v1")
	if selectedOnly {
		// Published db7/d486 versions fenced only the selected representation.
		// Such persistent state must not be mistaken for a governing base fence.
		r := httptest.NewRequest(http.MethodGet, fullVaryFenceURL, nil)
		r.Header.Set("Accept-Language", "en")
		key := NewRequestKey(r).Vary("Accept-Language", r).String()
		h.recordStale(key)
		backend.Invalidate(key)
	}
	changed.Store(true)
	switch failure {
	case "handler stage":
		handlerBudget.mu.Lock()
		handlerBudget.limit = 0
		handlerBudget.mu.Unlock()
	case "backend store":
		native.fail.Store(true)
	case "cache stage":
		cacheBudget.mu.Lock()
		cacheBudget.limit = 0
		cacheBudget.mu.Unlock()
	}
	assertResponse(fullVaryFenceRequest(h, "en", "gzip", true), "gzip/v2")
	if calls.Load() > 4 {
		t.Fatalf("changed policy required more than one full refetch: origins=%d", calls.Load())
	}
	handlerBudget.mu.Lock()
	handlerBudget.limit = 1 << 20
	handlerBudget.mu.Unlock()
	cacheBudget.mu.Lock()
	cacheBudget.limit = 1 << 20
	cacheBudget.mu.Unlock()
	native.fail.Store(false)
	rec := fullVaryFenceRequest(h, "fr", "gzip", false)
	assertResponse(rec, "gzip/v2")
	if calls.Load() != 6 || rec.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("old discovery rule survived failed publication: origins=%d headers=%v", calls.Load(), rec.Header())
	}
	assertResponse(fullVaryFenceRequest(h, "en", "gzip", false), "gzip/v2")
	assertResponse(fullVaryFenceRequest(h, "fr", "identity", false), "identity/v2")
	if calls.Load() != 7 {
		t.Fatalf("new Vary selection did not distinguish encodings: origins=%d", calls.Load())
	}
	for _, language := range []string{"en", "fr"} {
		for _, encoding := range []string{"gzip", "identity"} {
			rec = fullVaryFenceRequest(h, language, encoding, false)
			assertResponse(rec, encoding+"/v2")
			if rec.Header().Get(CacheHeader) != "HIT" || calls.Load() != 7 {
				t.Fatalf("rebuilt variants repeatedly validated: origins=%d headers=%v", calls.Load(), rec.Header())
			}
		}
	}
}

func TestUnchangedValidationVaryPolicyKeepsPeerHits(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, opaque := range []bool{false, true} {
			t.Run(stringBool(disk)+"/opaque="+strconv.FormatBool(opaque), func(t *testing.T) {
				_, backend := replacementCache(t, disk, opaque, nil)
				var full, notModified atomic.Int64
				h := NewSharedHandler(backend, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					language := r.Header.Get("Accept-Language")
					w.Header().Set("Vary", "Accept-Language")
					w.Header().Set("ETag", `"`+language+`"`)
					w.Header().Set("Cache-Control", "max-age=3600")
					if r.Header.Get("If-None-Match") == w.Header().Get("ETag") {
						notModified.Add(1)
						w.WriteHeader(http.StatusNotModified)
						return
					}
					full.Add(1)
					_, _ = io.WriteString(w, language)
				}))
				defer h.writes.Wait()
				assertReplacementResponse(t, fullVaryFenceRequest(h, "en", "identity", false), "en")
				assertReplacementResponse(t, fullVaryFenceRequest(h, "fr", "identity", false), "fr")
				assertReplacementResponse(t, fullVaryFenceRequest(h, "en", "identity", true), "en")
				assertReplacementResponse(t, fullVaryFenceRequest(h, "fr", "identity", false), "fr")
				for _, language := range []string{"en", "fr", "en", "fr"} {
					rec := fullVaryFenceRequest(h, language, "identity", false)
					assertReplacementResponse(t, rec, language)
					if rec.Header().Get(CacheHeader) != "HIT" || full.Load() != 2 || notModified.Load() != 1 {
						t.Fatalf("unmodified peers repeatedly validated: full=%d 304=%d headers=%v", full.Load(), notModified.Load(), rec.Header())
					}
				}
			})
		}
	}
}

func TestFullResponseVaryRebuildIncludesEveryStoreableStatus(t *testing.T) {
	h := NewSharedHandler(nil, nil)
	req, err := newCacheRequest(httptest.NewRequest(http.MethodGet, fullVaryFenceURL, nil))
	if err != nil {
		t.Fatal(err)
	}
	previous := http.Header{"Vary": {"Accept-Language"}}
	replacement := http.Header{"Vary": {"Accept-Encoding"}, "Cache-Control": {"max-age=60"}}
	for status := range storeable {
		if !h.fullResponseVaryChanged(req, status, previous, replacement) {
			t.Errorf("storeable status %d did not require a policy rebuild", status)
		}
	}
	if h.fullResponseVaryChanged(req, http.StatusNotModified, previous, replacement) {
		t.Error("304 must retain its separate representation validation path")
	}
}

func TestUncacheableValidationErrorsKeepOriginalResponseWithoutVaryReplay(t *testing.T) {
	for _, tc := range []struct {
		status int
		policy string
	}{
		{http.StatusNotFound, "no-store"},
		{http.StatusNotFound, "private, max-age=60"},
		{http.StatusServiceUnavailable, "public, max-age=60"},
		{http.StatusUnauthorized, "public, max-age=60"},
		{http.StatusBadRequest, "public, max-age=60"},
	} {
		t.Run(strconv.Itoa(tc.status)+"/"+tc.policy, func(t *testing.T) {
			_, backend := replacementCache(t, false, false, nil)
			var calls atomic.Int64
			h := NewSharedHandler(backend, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch calls.Add(1) {
				case 1:
					w.Header().Set("Cache-Control", "max-age=3600")
					w.Header().Set("Vary", "Accept-Language")
					w.Header().Set("ETag", `"warm"`)
					_, _ = io.WriteString(w, "warm")
				case 2:
					w.Header().Set("Cache-Control", tc.policy)
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, "original error response")
				default:
					_, _ = io.WriteString(w, "unexpected replay replaced the origin error")
				}
			}))
			defer h.writes.Wait()
			assertReplacementResponse(t, fullVaryFenceRequest(h, "en", "gzip", false), "warm")
			rec := fullVaryFenceRequest(h, "en", "gzip", true)
			if rec.Code != tc.status || rec.Body.String() != "original error response" || calls.Load() != 2 {
				t.Fatalf("uncacheable response was replayed: status=%d body=%q origins=%d", rec.Code, rec.Body.String(), calls.Load())
			}
		})
	}
}

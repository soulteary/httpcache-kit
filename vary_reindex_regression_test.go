package httpcache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

type reviewVaryUpstream struct {
	changed     atomic.Bool
	refetchFail atomic.Bool
	full        atomic.Int64
	validated   atomic.Int64
}

func (u *reviewVaryUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := "variant A"
	vary := "Accept-Language"
	if u.changed.Load() {
		vary = "Accept-Encoding"
		if r.Header.Get("Accept-Encoding") == "br" {
			body = "variant B"
		}
	} else if r.Header.Get("Accept-Language") == "fr" {
		body = "variant B"
	}
	w.Header().Set("Cache-Control", "max-age=3600")
	w.Header().Set("Vary", vary)
	w.Header().Set("ETag", `"`+body+`"`)
	if r.Header.Get("If-None-Match") == w.Header().Get("ETag") {
		u.validated.Add(1)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	u.full.Add(1)
	if u.refetchFail.Load() {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	_, _ = io.WriteString(w, body)
}

func reviewVaryRequest(h *Handler, language, encoding string, force bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://cache.example/vary-policy", nil)
	req.Header.Set("Accept-Language", language)
	req.Header.Set("Accept-Encoding", encoding)
	if force {
		req.Header.Set("Cache-Control", "min-fresh=7200")
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	h.writes.Wait()
	return recorder
}

func assertReviewVaryBody(t *testing.T, rec *httptest.ResponseRecorder, body string) {
	t.Helper()
	if rec.Code != http.StatusOK || rec.Body.String() != body {
		t.Fatalf("response %d %q; want 200 %q", rec.Code, rec.Body.String(), body)
	}
}

func TestReviewValidationReindexesChangedVaryPolicy(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(stringBool(disk), func(t *testing.T) {
			cache := generationBudgetCache(t, disk, 1<<20)
			upstream := &reviewVaryUpstream{}
			h := NewSharedHandler(cache, upstream)
			defer h.writes.Wait()
			// The base discovery entry belongs to French while the English
			// variant is validated. Neither old key can index the new policy.
			assertReviewVaryBody(t, reviewVaryRequest(h, "en", "gzip", false), "variant A")
			assertReviewVaryBody(t, reviewVaryRequest(h, "fr", "gzip", false), "variant B")
			upstream.changed.Store(true)
			assertReviewVaryBody(t, reviewVaryRequest(h, "en", "gzip", true), "variant A")
			assertReviewVaryBody(t, reviewVaryRequest(h, "en", "br", false), "variant B")
			// The new encoding variants are reused across languages, with no
			// further origin request or old Accept-Language selection.
			for _, tc := range []struct{ language, encoding, body string }{{"fr", "gzip", "variant A"}, {"fr", "br", "variant B"}} {
				rec := reviewVaryRequest(h, tc.language, tc.encoding, false)
				assertReviewVaryBody(t, rec, tc.body)
				if rec.Header().Get(CacheHeader) != "HIT" || rec.Header().Get("Vary") != "Accept-Encoding" {
					t.Fatalf("new Vary discovery was not reused: %v", rec.Header())
				}
			}
			if upstream.full.Load() != 4 || upstream.validated.Load() != 1 {
				t.Fatalf("full fetches=%d validations=%d; want 4 and 1", upstream.full.Load(), upstream.validated.Load())
			}
		})
	}
}

func TestReviewChangedVaryFailedRefetchLeavesOldKeysStale(t *testing.T) {
	cache := generationBudgetCache(t, false, 1<<20)
	upstream := &reviewVaryUpstream{}
	h := NewSharedHandler(cache, upstream)
	defer h.writes.Wait()
	assertReviewVaryBody(t, reviewVaryRequest(h, "en", "gzip", false), "variant A")
	upstream.changed.Store(true)
	upstream.refetchFail.Store(true)
	if rec := reviewVaryRequest(h, "en", "gzip", true); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed full refetch returned %d %q", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "http://cache.example/vary-policy", nil)
	req.Header.Set("Accept-Language", "en")
	key := NewRequestKey(req)
	for _, staleKey := range []string{key.String(), key.Vary("Accept-Language", req).String()} {
		if _, marked := cache.StaleAt(staleKey); !marked {
			t.Errorf("changed Vary left the old key fresh: %q", staleKey)
		}
	}
	upstream.refetchFail.Store(false)
	assertReviewVaryBody(t, reviewVaryRequest(h, "en", "br", false), "variant B")
}

func TestReviewValidationRetainsEquivalentVarySelection(t *testing.T) {
	cache := generationBudgetCache(t, false, 1<<20)
	var calls atomic.Int64
	h := NewSharedHandler(cache, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("ETag", `"same"`)
		if r.Header.Get("If-None-Match") != "" {
			// Field order, casing, duplicates and field-line grouping do not
			// change which request headers select a representation.
			w.Header().Set("Vary", "accept-encoding, ACCEPT-LANGUAGE, Accept-Encoding")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Add("Vary", "Accept-Language")
		w.Header().Add("Vary", "Accept-Encoding")
		_, _ = io.WriteString(w, "unchanged")
	}))
	defer h.writes.Wait()
	assertReviewVaryBody(t, reviewVaryRequest(h, "en", "gzip", false), "unchanged")
	assertReviewVaryBody(t, reviewVaryRequest(h, "en", "gzip", true), "unchanged")
	rec := reviewVaryRequest(h, "en", "gzip", false)
	assertReviewVaryBody(t, rec, "unchanged")
	if calls.Load() != 2 || rec.Header().Get(CacheHeader) != "HIT" {
		t.Fatalf("equivalent Vary policy was refetched: calls=%d headers=%v", calls.Load(), rec.Header())
	}
}

package httpcache

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type reviewConditionalVaryOrigin struct {
	changed  atomic.Bool
	calls    atomic.Int64
	last     atomic.Value
	modified time.Time
	trailers bool
}

func (u *reviewConditionalVaryOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.calls.Add(1)
	u.last.Store(r.Header.Clone())
	w.Header().Set("Cache-Control", "max-age=3600")
	w.Header().Set("ETag", `"same"`)
	vary := "Accept-Language"
	if u.changed.Load() {
		vary = "Accept-Encoding"
	}
	w.Header().Set("Vary", vary)
	if u.trailers {
		w.Header().Set("Trailer", "X-Origin-Checksum")
	}
	http.ServeContent(w, r, "", u.modified, strings.NewReader("body stored representation"))
	if u.trailers {
		w.Header().Set("X-Origin-Checksum", "body-checksum")
	}
}

func reviewConditionalVaryRequest(condition string, modified time.Time) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://cache.example/vary-conditional", nil)
	req.Header.Set("Accept-Language", "en")
	req.Header.Set("Accept-Encoding", "gzip")
	switch condition {
	case "etag":
		req.Header.Set("If-None-Match", `"same"`)
	case "date":
		req.Header.Set("If-Modified-Since", modified.Format(http.TimeFormat))
	case "unmatched":
		req.Header.Set("If-None-Match", `"other"`)
	}
	return req
}

func reviewServeConditionalVary(h *Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	h.writes.Wait()
	return rec
}

func TestReviewChangedVaryMatchingConditionsCacheFullResponse(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, condition := range []string{"etag", "date"} {
			t.Run(stringBool(disk)+"/"+condition, func(t *testing.T) {
				cache := generationBudgetCache(t, disk, 1<<20)
				upstream := &reviewConditionalVaryOrigin{modified: time.Now().Add(-time.Hour).UTC().Truncate(time.Second)}
				h := NewSharedHandler(cache, upstream)
				defer h.writes.Wait()
				assertReviewVaryBody(t, reviewServeConditionalVary(h, reviewConditionalVaryRequest("", upstream.modified)), "body stored representation")
				upstream.changed.Store(true)
				req := reviewConditionalVaryRequest(condition, upstream.modified)
				req.Header.Set("Cache-Control", "min-fresh=7200")
				original := req.Header.Clone()
				rec := reviewServeConditionalVary(h, req)
				if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
					t.Fatalf("original matching condition returned %d %q", rec.Code, rec.Body.String())
				}
				if !reflect.DeepEqual(req.Header, original) {
					t.Fatalf("refetch mutated the original client headers: %v", req.Header)
				}
				repeat := req.Clone(req.Context())
				repeat.Header.Del("Cache-Control")
				rec = reviewServeConditionalVary(h, repeat)
				if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 || upstream.calls.Load() != 3 {
					t.Fatalf("repeated matching request was not served from the rebuilt cache: %d %q calls=%d", rec.Code, rec.Body.String(), upstream.calls.Load())
				}
				assertReviewVaryRefetchHeaders(t, upstream, req)
				// The client receives 304, but the new base and encoding variant
				// must contain a complete body, usable by another language.
				hit := reviewConditionalVaryRequest("", upstream.modified)
				hit.Header.Set("Accept-Language", "fr")
				rec = reviewServeConditionalVary(h, hit)
				assertReviewVaryBody(t, rec, "body stored representation")
				if upstream.calls.Load() != 3 || rec.Header().Get(CacheHeader) != "HIT" || rec.Header().Get("Vary") != "Accept-Encoding" {
					t.Fatalf("changed Vary was not cached after conditional delivery: calls=%d headers=%v", upstream.calls.Load(), rec.Header())
				}
			})
		}
	}
}

func assertReviewVaryRefetchHeaders(t *testing.T, upstream *reviewConditionalVaryOrigin, client *http.Request) {
	t.Helper()
	header := upstream.last.Load().(http.Header)
	if header.Get("If-None-Match") != "" || header.Get("If-Modified-Since") != "" {
		t.Fatalf("Vary refetch remained conditional: %v", header)
	}
	for _, name := range []string{"If-Match", "If-Unmodified-Since", "Range", "If-Range"} {
		if header.Get(name) != client.Header.Get(name) {
			t.Errorf("Vary refetch changed %s: got %q want %q", name, header.Get(name), client.Header.Get(name))
		}
	}
}

func TestReviewChangedVaryRefetchPreservesRangeDelivery(t *testing.T) {
	for _, tc := range []struct {
		condition string
		status    int
		body      string
	}{{"etag", http.StatusNotModified, ""}, {"unmatched", http.StatusPartialContent, "body"}} {
		t.Run(tc.condition, func(t *testing.T) {
			cache := generationBudgetCache(t, false, 1<<20)
			upstream := &reviewConditionalVaryOrigin{modified: time.Now().Add(-time.Hour).UTC().Truncate(time.Second)}
			h := NewSharedHandler(cache, upstream)
			defer h.writes.Wait()
			reviewServeConditionalVary(h, reviewConditionalVaryRequest("", upstream.modified))
			upstream.changed.Store(true)
			req := reviewConditionalVaryRequest(tc.condition, upstream.modified)
			req.Header.Set("Cache-Control", "min-fresh=7200")
			req.Header.Set("Range", "bytes=0-3")
			rec := reviewServeConditionalVary(h, req)
			if rec.Code != tc.status || rec.Body.String() != tc.body {
				t.Fatalf("ranged conditional delivery returned %d %q; want %d %q", rec.Code, rec.Body.String(), tc.status, tc.body)
			}
			assertReviewVaryRefetchHeaders(t, upstream, req)
			if tc.status == http.StatusPartialContent && rec.Header().Get("Content-Range") != "bytes 0-3/26" {
				t.Fatalf("original range was not delivered: %v", rec.Header())
			}
		})
	}
}

func TestReviewVaryRefetchRetainsExclusivePreconditions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{{"If-Match", http.StatusPreconditionFailed}, {"If-Unmodified-Since", http.StatusPreconditionFailed}, {"If-Range", http.StatusPartialContent}} {
		t.Run(tc.name, func(t *testing.T) {
			cache := generationBudgetCache(t, false, 1<<20)
			upstream := &reviewConditionalVaryOrigin{modified: time.Now().Add(-time.Hour).UTC().Truncate(time.Second)}
			h := NewSharedHandler(cache, upstream)
			defer h.writes.Wait()
			req := reviewConditionalVaryRequest("unmatched", upstream.modified)
			switch tc.name {
			case "If-Match":
				req.Header.Set(tc.name, `"different"`)
			case "If-Unmodified-Since":
				req.Header.Set(tc.name, upstream.modified.Add(-time.Hour).Format(http.TimeFormat))
			case "If-Range":
				req.Header.Set(tc.name, `"same"`)
				req.Header.Set("Range", "bytes=0-3")
			}
			cacheReq, err := newCacheRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			// These exclusive conditions normally bypass cache lookup. Check
			// the narrow refetch contract directly so it cannot strip them if
			// an embedding application reaches this path with such a request.
			rec := httptest.NewRecorder()
			h.refetchChangedVaryResponse(rec, cacheReq, NewResourceBytes(http.StatusOK, nil, nil))
			h.writes.Wait()
			if rec.Code != tc.status {
				t.Fatalf("exclusive condition returned %d; want %d", rec.Code, tc.status)
			}
			assertReviewVaryRefetchHeaders(t, upstream, req)
		})
	}
}

func TestReviewChangedVaryUnmatchedConditionPublishesTrailers(t *testing.T) {
	cache := generationBudgetCache(t, false, 1<<20)
	upstream := &reviewConditionalVaryOrigin{modified: time.Now().Add(-time.Hour).UTC().Truncate(time.Second), trailers: true}
	h := NewSharedHandler(cache, upstream)
	defer h.writes.Wait()
	reviewServeConditionalVary(h, reviewConditionalVaryRequest("", upstream.modified))
	upstream.changed.Store(true)
	req := reviewConditionalVaryRequest("unmatched", upstream.modified)
	req.Header.Set("Cache-Control", "min-fresh=7200")
	rec := reviewServeConditionalVary(h, req)
	assertReviewVaryBody(t, rec, "body stored representation")
	response := rec.Result()
	defer func() { _ = response.Body.Close() }()
	if response.Trailer.Get("X-Origin-Checksum") != "body-checksum" {
		t.Fatalf("refetch lost the final origin trailer: %v", response.Trailer)
	}
}

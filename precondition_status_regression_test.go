package httpcache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// Redirects and failures take precedence over matching conditions, including
// during revalidation (RFC 9110 section 13.2.1). Cacheability does not change
// that ordering, and subsequent hits must retain the original status and body.
func TestValidationPreservesRedirectAndErrorStatusWithMatchingConditions(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusNotFound, http.StatusGone} {
		for _, condition := range []string{"etag", "date"} {
			t.Run(strconv.Itoa(status)+"/"+condition, func(t *testing.T) {
				testValidationPreservesNonSuccess(t, status, condition)
			})
		}
	}
}

func testValidationPreservesNonSuccess(t *testing.T, status int, condition string) {
	t.Helper()
	var calls atomic.Int64
	c := NewMemoryCache().(ExtendedCache)
	defer func() { _ = c.Close() }()
	modified := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	h := NewSharedHandler(c, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
		w.Header().Set("ETag", `"v1"`)
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, "old representation")
			return
		}
		w.Header().Set("ETag", `"v2"`)
		w.Header().Set("Location", "/new-location")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "redirect or error representation")
	}))
	defer h.writes.Wait()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://cache.example/object", nil))
	h.writes.Wait()
	for _, force := range []bool{true, false} {
		r := httptest.NewRequest("GET", "http://cache.example/object", nil)
		if condition == "etag" {
			r.Header.Set("If-None-Match", `"v2"`)
		} else {
			r.Header.Set("If-Modified-Since", modified.Format(http.TimeFormat))
		}
		if force {
			r.Header.Set("Cache-Control", "min-fresh=7200")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		h.writes.Wait()
		if w.Code != status || w.Body.String() != "redirect or error representation" {
			t.Fatalf("force=%v status=%d body=%q; want status=%d and the original body", force, w.Code, w.Body.String(), status)
		}
		if w.Header().Get("ETag") != `"v2"` || calls.Load() != 2 {
			t.Fatalf("force=%v etag=%q calls=%d; want the new stored response without a refetch", force, w.Header().Get("ETag"), calls.Load())
		}
	}
}

func TestValidationAppliesOriginalConditionsToNonAuthoritativeResponse(t *testing.T) {
	for _, condition := range []string{"etag", "date"} {
		t.Run(condition, func(t *testing.T) {
			var calls atomic.Int64
			c := NewMemoryCache().(ExtendedCache)
			defer func() { _ = c.Close() }()
			modified := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
			h := NewSharedHandler(c, nonAuthoritativeUpstream(&calls, modified))
			defer h.writes.Wait()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://cache.example/object", nil))
			h.writes.Wait()
			r := httptest.NewRequest("GET", "http://cache.example/object", nil)
			r.Header.Set("Cache-Control", "min-fresh=7200")
			if condition == "etag" {
				r.Header.Set("If-None-Match", `"v2"`)
			} else {
				r.Header.Set("If-Modified-Since", modified.Format(http.TimeFormat))
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			h.writes.Wait()
			if w.Code != http.StatusNotModified || w.Body.Len() != 0 || w.Header().Get("ETag") != `"v2"` {
				t.Fatalf("original condition was lost: status=%d body=%q etag=%q", w.Code, w.Body.String(), w.Header().Get("ETag"))
			}
			w = httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "http://cache.example/object", nil))
			if w.Code != http.StatusNonAuthoritativeInfo || w.Body.String() != "transformed representation" || calls.Load() != 2 {
				t.Fatalf("complete 203 was not stored: status=%d body=%q calls=%d", w.Code, w.Body.String(), calls.Load())
			}
			r.Header.Del("Cache-Control")
			w = httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusNotModified || w.Body.Len() != 0 || calls.Load() != 2 {
				t.Fatalf("cached 203 lost original condition: status=%d body=%q calls=%d", w.Code, w.Body.String(), calls.Load())
			}
		})
	}
}

func nonAuthoritativeUpstream(calls *atomic.Int64, modified time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", modified.Add(-time.Hour).Format(http.TimeFormat))
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, "old representation")
			return
		}
		w.Header().Set("ETag", `"v2"`)
		w.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
		etag := r.Header.Get("If-None-Match")
		if etag == `"v2"` || (etag == "" && r.Header.Get("If-Modified-Since") == modified.Format(http.TimeFormat)) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusNonAuthoritativeInfo)
		_, _ = io.WriteString(w, "transformed representation")
	}
}

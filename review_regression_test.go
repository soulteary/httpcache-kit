package httpcache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReviewUnsatisfiableRangeUsesOriginalClientConditions(t *testing.T) {
	for _, tc := range []struct {
		condition string
		status    int
		calls     int64
	}{
		{"etag", http.StatusNotModified, 3},
		{"date", http.StatusNotModified, 3},
		{"unmatched", http.StatusRequestedRangeNotSatisfiable, 3},
		{"unchanged", http.StatusRequestedRangeNotSatisfiable, 2},
		{"absent", http.StatusRequestedRangeNotSatisfiable, 2},
	} {
		t.Run(tc.condition, func(t *testing.T) {
			var calls atomic.Int64
			c := NewMemoryCache().(ExtendedCache)
			defer func() { _ = c.Close() }()
			modified := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
			h := NewSharedHandler(c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				etag := `"v2"`
				if calls.Add(1) == 1 {
					etag = `"v1"`
				}
				w.Header().Set("ETag", etag)
				w.Header().Set("Cache-Control", "max-age=3600")
				http.ServeContent(w, r, "", modified, strings.NewReader("abcdef"))
			}))
			defer h.writes.Wait()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://cache.example/object", nil))
			h.writes.Wait()
			r := httptest.NewRequest("GET", "http://cache.example/object", nil)
			r.Header.Set("Range", "bytes=100-200")
			r.Header.Set("Cache-Control", "min-fresh=7200")
			switch tc.condition {
			case "etag":
				r.Header.Set("If-None-Match", `"v2"`)
			case "date":
				r.Header.Set("If-Modified-Since", modified.Format(http.TimeFormat))
			case "unmatched":
				r.Header.Set("If-None-Match", `"v3"`)
			case "unchanged":
				r.Header.Set("If-None-Match", `"v1"`)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			h.writes.Wait()
			if w.Code != tc.status || calls.Load() != tc.calls {
				t.Fatalf("status=%d calls=%d; want status=%d calls=%d", w.Code, calls.Load(), tc.status, tc.calls)
			}
			if tc.status == http.StatusNotModified && w.Body.Len() != 0 {
				t.Fatalf("304 included a body: %q", w.Body.String())
			}
		})
	}
}

func TestReviewInvalidExpiresRemainsConditionallyCacheable(t *testing.T) {
	for _, expires := range []string{"0", "-1", "not-a-date"} {
		t.Run(expires, func(t *testing.T) {
			var calls, validations atomic.Int64
			c := NewMemoryCache().(ExtendedCache)
			defer func() { _ = c.Close() }()
			h := NewSharedHandler(c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Expires", expires)
				w.Header().Set("ETag", `"same"`)
				if r.Header.Get("If-None-Match") == `"same"` {
					validations.Add(1)
					w.WriteHeader(http.StatusNotModified)
					return
				}
				_, _ = io.WriteString(w, "unchanged")
			}))
			defer h.writes.Wait()
			for range 2 {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest("GET", "http://cache.example/object", nil))
				h.writes.Wait()
				if w.Code != http.StatusOK || w.Body.String() != "unchanged" {
					t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
				}
			}
			if calls.Load() != 2 || validations.Load() != 1 || c.Stats().ItemCount != 1 {
				t.Fatalf("calls=%d validations=%d cached=%d; want a stored stale representation and one conditional request", calls.Load(), validations.Load(), c.Stats().ItemCount)
			}
			request, err := newCacheRequest(httptest.NewRequest("GET", "http://cache.example/object", nil))
			if err != nil {
				t.Fatal(err)
			}
			res, err := c.Retrieve(request.Key.String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = res.Close() }()
			age, err := res.MaxAge(true)
			if err != nil || age != 0 || res.HeuristicFreshness() != 0 {
				t.Fatalf("invalid expiration became fresh: max-age=%s err=%v", age, err)
			}
		})
	}
}

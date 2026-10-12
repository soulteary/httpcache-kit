package httpcache

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestConditionalUnsatisfiableRangeRetriesOriginalClientRequest(t *testing.T) {
	for _, condition := range []string{"etag", "date", "etag-cache-date-client"} {
		t.Run(condition, func(t *testing.T) {
			var calls atomic.Int64
			cache := NewMemoryCache().(ExtendedCache)
			defer func() { _ = cache.Close() }()
			current := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
			h := NewSharedHandler(cache, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				w.Header().Set("Cache-Control", "max-age=3600")
				body, modified := "long old representation", current.Add(-time.Hour)
				if n == 1 {
					if condition != "date" {
						w.Header().Set("ETag", `"old"`)
					}
				} else {
					body, modified = "new", current
					if condition != "date" {
						w.Header().Set("ETag", `"current"`)
					}
					if (n == 2 || n == 3) && r.Header.Get("Range") != "bytes=10-12" {
						t.Error("retry lost original Range")
					}
					switch n {
					case 2:
						if condition != "date" && r.Header.Get("If-None-Match") != `"old"` {
							t.Error("validation did not use cached ETag")
						}
						if condition == "date" && r.Header.Get("If-Modified-Since") != current.Add(-time.Hour).Format(http.TimeFormat) {
							t.Error("validation did not use cached modification date")
						}
					case 3:
						if condition == "etag-cache-date-client" && r.Header.Get("If-None-Match") != "" {
							t.Error("retry retained the cache ETag ahead of the client's date")
						}
						if condition == "etag" && r.Header.Get("If-None-Match") != `"current"` {
							t.Error("retry did not restore original client ETag")
						}
						if condition != "etag" && r.Header.Get("If-Modified-Since") != current.Format(http.TimeFormat) {
							t.Error("retry did not restore original client modification date")
						}
					}
				}
				http.ServeContent(w, r, "", modified, strings.NewReader(body))
			}))
			defer func() { _ = h.Shutdown(context.Background()) }()
			server := httptest.NewServer(h)
			defer server.Close()
			warm, err := http.Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, warm.Body)
			_ = warm.Body.Close()
			h.writes.Wait()
			req, err := http.NewRequest("GET", server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Range", "bytes=10-12")
			req.Header.Set("Cache-Control", "min-fresh=7200")
			if condition == "etag" {
				req.Header.Set("If-None-Match", `"current"`)
			} else {
				req.Header.Set("If-Modified-Since", current.Format(http.TimeFormat))
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if err != nil || res.StatusCode != 304 || len(body) != 0 || calls.Load() != 3 {
				t.Fatalf("wrong range/precondition response: status=%d body=%q calls=%d err=%v", res.StatusCode, body, calls.Load(), err)
			}
			cached, err := cache.Retrieve("GET:/")
			if err != nil {
				t.Fatal("could not inspect original stored representation", err)
			}
			defer func() { _ = cached.Close() }()
			if !cached.IsStale() {
				t.Fatal("failed validation left the old representation fresh")
			}
			for range 2 {
				res, err = http.Get(server.URL)
				if err != nil {
					t.Fatal(err)
				}
				body, err = io.ReadAll(res.Body)
				_ = res.Body.Close()
				h.writes.Wait()
				if err != nil || res.StatusCode != 200 || string(body) != "new" || calls.Load() != 4 {
					t.Fatalf("old body reused after client 304: status=%d body=%q calls=%d err=%v", res.StatusCode, body, calls.Load(), err)
				}
			}
		})
	}
}

func TestConditionalFailureDoesNotRetryUnchangedClientCondition(t *testing.T) {
	var calls atomic.Int64
	cache := NewMemoryCache().(ExtendedCache)
	defer func() { _ = cache.Close() }()
	h := NewSharedHandler(cache, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) > 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "unavailable")
			return
		}
		w.Header().Set("ETag", `"same"`)
		w.Header().Set("Cache-Control", "max-age=3600")
		_, _ = io.WriteString(w, "original")
	}))
	defer h.writes.Wait()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://cache.example/index", nil))
	h.writes.Wait()
	req := httptest.NewRequest("GET", "http://cache.example/index", nil)
	req.Header.Set("Cache-Control", "min-fresh=7200")
	req.Header.Set("If-None-Match", `"same"`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 503 || w.Body.String() != "unavailable" || calls.Load() != 2 {
		t.Fatalf("unchanged client condition retried: status=%d calls=%d", w.Code, calls.Load())
	}
}

func TestMalformedExpiresKeepsStaleValidatorsForRevalidation(t *testing.T) {
	for _, expires := range []string{"0", "not-a-date", ""} {
		for _, validator := range []string{"etag", "last-modified", "none"} {
			t.Run(expires+"/"+validator, func(t *testing.T) {
				var full, validations atomic.Int64
				cache := NewMemoryCache().(ExtendedCache)
				defer func() { _ = cache.Close() }()
				modified := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second).Format(http.TimeFormat)
				h := NewSharedHandler(cache, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Expires", expires)
					switch validator {
					case "etag":
						w.Header().Set("ETag", `"same"`)
					case "last-modified":
						w.Header().Set("Last-Modified", modified)
					}
					if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
						validations.Add(1)
						w.WriteHeader(http.StatusNotModified)
						return
					}
					full.Add(1)
					_, _ = io.WriteString(w, "complete")
				}))
				defer h.writes.Wait()
				for range 3 {
					w := httptest.NewRecorder()
					h.ServeHTTP(w, httptest.NewRequest("GET", "http://cache.example/index", nil))
					h.writes.Wait()
					if w.Code != 200 || w.Body.String() != "complete" {
						t.Fatalf("invalid expiration lost representation: %d %q", w.Code, w.Body.String())
					}
				}
				if validator == "none" {
					if full.Load() != 3 || validations.Load() != 0 || cache.Stats().ItemCount != 0 {
						t.Fatal("invalid expiration without validators was cached")
					}
				} else if full.Load() != 1 || validations.Load() != 2 || cache.Stats().ItemCount == 0 {
					t.Fatalf("invalid expiration was dropped or served fresh: full=%d validations=%d stats=%+v", full.Load(), validations.Load(), cache.Stats())
				}
			})
		}
	}
}

package httpcache

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestChangedVaryRebuildStoresFullResponseForMatchingClientConditions(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, condition := range []string{"etag", "date"} {
			t.Run(stringBool(disk)+"/"+condition, func(t *testing.T) {
				cache := generationBudgetCache(t, disk, 1<<20)
				var changed atomic.Bool
				var full, validations atomic.Int64
				modified := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
				h := NewSharedHandler(cache, followupChangedVaryOrigin(&changed, &full, &validations, modified))
				defer h.writes.Wait()
				assertReviewVaryBody(t, reviewVaryRequest(h, "en", "gzip", false), "variant A")
				assertReviewVaryBody(t, reviewVaryRequest(h, "fr", "gzip", false), "variant B")
				changed.Store(true)
				r := httptest.NewRequest(http.MethodGet, "http://cache.example/vary-policy", nil)
				r.Header.Set("Accept-Language", "en")
				r.Header.Set("Accept-Encoding", "gzip")
				r.Header.Set("Cache-Control", "min-fresh=7200")
				if condition == "etag" {
					r.Header.Set("If-None-Match", `"variant-A"`)
				} else {
					r.Header.Set("If-Modified-Since", modified.Format(http.TimeFormat))
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, r)
				h.writes.Wait()
				if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 || rec.Header().Get("Vary") != "Accept-Encoding" {
					t.Fatalf("client condition was lost: status=%d body=%q headers=%v", rec.Code, rec.Body.String(), rec.Header())
				}
				if full.Load() != 3 || validations.Load() != 1 {
					t.Fatalf("rebuild did not fetch a complete representation: full=%d validations=%d; want 3 and 1", full.Load(), validations.Load())
				}
				// The client got no body, but storage must receive a full response
				// with the new discovery/variant keys, reusable across languages.
				for _, language := range []string{"en", "fr"} {
					rec = reviewVaryRequest(h, language, "gzip", false)
					assertReviewVaryBody(t, rec, "variant A")
					if rec.Header().Get(CacheHeader) != "HIT" || rec.Header().Get("Vary") != "Accept-Encoding" {
						t.Fatalf("reindexed gzip variant was not reused: %v", rec.Header())
					}
				}
				assertReviewVaryBody(t, reviewVaryRequest(h, "en", "br", false), "variant B")
				rec = reviewVaryRequest(h, "fr", "br", false)
				assertReviewVaryBody(t, rec, "variant B")
				if rec.Header().Get(CacheHeader) != "HIT" || full.Load() != 4 || validations.Load() != 1 {
					t.Fatalf("new variant repeats origin requests: full=%d validations=%d headers=%v", full.Load(), validations.Load(), rec.Header())
				}
			})
		}
	}
}

func followupChangedVaryOrigin(changed *atomic.Bool, full, validations *atomic.Int64, modified time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, vary := "variant A", "Accept-Language"
		if changed.Load() {
			vary = "Accept-Encoding"
			if r.Header.Get("Accept-Encoding") == "br" {
				body = "variant B"
			}
		} else if r.Header.Get("Accept-Language") == "fr" {
			body = "variant B"
		}
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Vary", vary)
		etagValue := `"variant-A"`
		if body == "variant B" {
			etagValue = `"variant-B"`
		}
		w.Header().Set("ETag", etagValue)
		w.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
		etag := r.Header.Get("If-None-Match")
		if etag == w.Header().Get("ETag") || (etag == "" && r.Header.Get("If-Modified-Since") == modified.Format(http.TimeFormat)) {
			validations.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		full.Add(1)
		_, _ = io.WriteString(w, body)
	})
}

func TestClientPreconditionsApplyToEverySuccessfulStatus(t *testing.T) {
	modified := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	for _, status := range []int{http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNonAuthoritativeInfo, http.StatusNoContent, http.StatusResetContent, http.StatusPartialContent, http.StatusMultiStatus, http.StatusAlreadyReported, http.StatusIMUsed} {
		for _, condition := range []string{"etag", "date", "if-match", "if-unmodified-since"} {
			t.Run(strconv.Itoa(status)+"/"+condition, func(t *testing.T) {
				r := httptest.NewRequest(http.MethodGet, "http://cache.example/object", nil)
				want := http.StatusNotModified
				switch condition {
				case "etag":
					r.Header.Set("If-None-Match", `"current"`)
				case "date":
					r.Header.Set("If-Modified-Since", modified.Format(http.TimeFormat))
				case "if-match":
					r.Header.Set("If-Match", `"different"`)
					want = http.StatusPreconditionFailed
				case "if-unmodified-since":
					r.Header.Set("If-Unmodified-Since", modified.Add(-time.Hour).Format(http.TimeFormat))
					want = http.StatusPreconditionFailed
				}
				rec := httptest.NewRecorder()
				w := newClientPreconditionWriter(rec, r)
				w.Header().Set("ETag", `"current"`)
				w.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
				w.WriteHeader(status)
				_, _ = w.Write([]byte("upstream representation"))
				if rec.Code != want || rec.Body.Len() != 0 || rec.Header().Get("ETag") != `"current"` {
					t.Fatalf("condition was lost: status=%d body=%q headers=%v; want %d without a body", rec.Code, rec.Body.String(), rec.Header(), want)
				}
			})
		}
	}
}

func TestValidationAppliesOriginalConditionsToNoContentResponse(t *testing.T) {
	for _, condition := range []string{"etag", "date", "unmatched"} {
		t.Run(condition, func(t *testing.T) {
			cache := generationBudgetCache(t, false, 1<<20)
			var calls atomic.Int64
			modified := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
			h := NewSharedHandler(cache, followupNoContentOrigin(&calls, modified))
			defer h.writes.Wait()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://cache.example/object", nil))
			h.writes.Wait()
			r := httptest.NewRequest(http.MethodGet, "http://cache.example/object", nil)
			r.Header.Set("Cache-Control", "min-fresh=7200")
			switch condition {
			case "etag":
				r.Header.Set("If-None-Match", `"current"`)
			case "date":
				r.Header.Set("If-Modified-Since", modified.Format(http.TimeFormat))
			default:
				r.Header.Set("If-None-Match", `"absent"`)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			h.writes.Wait()
			want := http.StatusNotModified
			if condition == "unmatched" {
				want = http.StatusNoContent
			}
			if rec.Code != want || rec.Body.Len() != 0 || rec.Header().Get("ETag") != `"current"` {
				t.Fatalf("validation lost original condition: status=%d body=%q headers=%v", rec.Code, rec.Body.String(), rec.Header())
			}
			key := NewRequestKey(r).String()
			if _, stale := cache.StaleAt(key); !stale {
				t.Fatal("unstoreable 204 left the old representation fresh")
			}
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://cache.example/object", nil))
			h.writes.Wait()
			if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 || calls.Load() != 3 {
				t.Fatalf("anonymous followup reused the old representation: status=%d body=%q calls=%d", rec.Code, rec.Body.String(), calls.Load())
			}
			r.Header.Del("Cache-Control")
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != want || rec.Body.Len() != 0 || calls.Load() != 4 {
				t.Fatalf("uncached 204 lost original condition: status=%d body=%q calls=%d", rec.Code, rec.Body.String(), calls.Load())
			}
		})
	}
}

func followupNoContentOrigin(calls *atomic.Int64, modified time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		if calls.Add(1) == 1 {
			w.Header().Set("ETag", `"old"`)
			w.Header().Set("Last-Modified", modified.Add(-time.Hour).Format(http.TimeFormat))
			_, _ = io.WriteString(w, "old representation")
			return
		}
		w.Header().Set("ETag", `"current"`)
		w.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
		if r.Header.Get("If-None-Match") == `"current"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// Range selection remains the upstream's responsibility during a Vary rebuild;
// the partial body must be delivered intact and never stored as a whole object.
func TestChangedVaryRebuildPreservesUpstreamRangeHandling(t *testing.T) {
	cache := generationBudgetCache(t, false, 1<<20)
	var changed atomic.Bool
	var full atomic.Int64
	h := NewSharedHandler(cache, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("ETag", `"same"`)
		vary := "Accept-Language"
		if changed.Load() {
			vary = "Accept-Encoding"
		}
		w.Header().Set("Vary", vary)
		if r.Header.Get("If-None-Match") == "" {
			full.Add(1)
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader([]byte("abcdefghij")))
	}))
	defer h.writes.Wait()
	assertReviewVaryBody(t, reviewVaryRequest(h, "en", "gzip", false), "abcdefghij")
	changed.Store(true)
	r := httptest.NewRequest(http.MethodGet, "http://cache.example/vary-policy", nil)
	r.Header.Set("Accept-Language", "en")
	r.Header.Set("Accept-Encoding", "gzip")
	r.Header.Set("Cache-Control", "min-fresh=7200")
	r.Header.Set("Range", "bytes=2-4")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	h.writes.Wait()
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "cde" || rec.Header().Get("Content-Range") != "bytes 2-4/10" {
		t.Fatalf("upstream range was altered: status=%d body=%q headers=%v", rec.Code, rec.Body.String(), rec.Header())
	}
	assertReviewVaryBody(t, reviewVaryRequest(h, "en", "gzip", false), "abcdefghij")
	if full.Load() != 3 {
		t.Fatalf("partial rebuild was stored as a complete object: full=%d; want warm, range, full refetch", full.Load())
	}
}

func TestClientPreconditionsKeepNonSuccessStatusPrecedence(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusNotFound, http.StatusServiceUnavailable} {
		for _, header := range []string{"If-None-Match", "If-Match"} {
			t.Run(strconv.Itoa(status)+"/"+header, func(t *testing.T) {
				r := httptest.NewRequest(http.MethodGet, "http://cache.example/object", nil)
				value := `"current"`
				if header == "If-Match" {
					value = `"different"`
				}
				r.Header.Set(header, value)
				rec := httptest.NewRecorder()
				w := newClientPreconditionWriter(rec, r)
				w.Header().Set("ETag", `"current"`)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "original error or redirect")
				if rec.Code != status || rec.Body.String() != "original error or redirect" {
					t.Fatalf("non-success response lost precedence: status=%d body=%q", rec.Code, rec.Body.String())
				}
			})
		}
	}
}

func TestUncacheableValidationResponseInvalidatesOldRepresentation(t *testing.T) {
	cache := generationBudgetCache(t, false, 1<<20)
	var calls atomic.Int64
	h := NewSharedHandler(cache, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Cache-Control", "max-age=3600")
			w.Header().Set("ETag", `"old"`)
			_, _ = io.WriteString(w, "old public representation")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("ETag", `"current"`)
		_, _ = io.WriteString(w, "current private representation")
	}))
	defer h.writes.Wait()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://cache.example/object", nil))
	h.writes.Wait()
	r := httptest.NewRequest(http.MethodGet, "http://cache.example/object", nil)
	r.Header.Set("Cache-Control", "min-fresh=7200")
	r.Header.Set("If-None-Match", `"current"`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	h.writes.Wait()
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Fatalf("original client condition was lost: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if _, stale := cache.StaleAt(NewRequestKey(r).String()); !stale {
		t.Fatal("no-store replacement left the old public representation fresh")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://cache.example/object", nil))
	h.writes.Wait()
	if rec.Code != http.StatusOK || rec.Body.String() != "current private representation" || calls.Load() != 3 {
		t.Fatalf("anonymous followup reused obsolete content: status=%d body=%q calls=%d", rec.Code, rec.Body.String(), calls.Load())
	}
}

func TestChangedVaryRebuildOrdersOpaqueCacheGenerationAfterInvalidation(t *testing.T) {
	inner := generationBudgetCache(t, false, 1<<20)
	cache := impreciseCache{plainCache{inner: inner}}
	origin := &reviewConditionalVaryOrigin{modified: time.Now().Add(-time.Hour).UTC().Truncate(time.Second)}
	h := NewSharedHandler(cache, origin)
	defer h.writes.Wait()
	reviewServeConditionalVary(h, reviewConditionalVaryRequest("", origin.modified))
	origin.changed.Store(true)
	req := reviewConditionalVaryRequest("etag", origin.modified)
	req.Header.Set("Cache-Control", "min-fresh=7200")
	rec := reviewServeConditionalVary(h, req)
	if rec.Code != http.StatusNotModified || origin.calls.Load() != 3 {
		t.Fatalf("conditional rebuild returned %d after %d calls", rec.Code, origin.calls.Load())
	}
	hit := reviewConditionalVaryRequest("", origin.modified)
	hit.Header.Set("Accept-Language", "fr")
	rec = reviewServeConditionalVary(h, hit)
	assertReviewVaryBody(t, rec, "body stored representation")
	if origin.calls.Load() != 3 || rec.Header().Get(CacheHeader) != "HIT" {
		t.Fatalf("opaque cache rebuild was recorded before invalidation: calls=%d headers=%v", origin.calls.Load(), rec.Header())
	}
}

func TestUncacheableBodylessResponseSerializesLiveTrailerUpdates(t *testing.T) {
	cache := generationBudgetCache(t, false, 1<<20)
	h := NewSharedHandler(cache, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Trailer", "X-Final-Checksum")
		w.WriteHeader(http.StatusOK)
		// A declared trailer can be updated after header publication, including
		// before any body write. Exercise live-map mutation while the main
		// goroutine makes its storage decision for this empty response.
		for n := range 128 {
			w.Header().Set("X-Final-Checksum", strconv.Itoa(n))
			runtime.Gosched()
		}
		w.Header().Set("X-Final-Checksum", "complete")
	}))
	defer h.writes.Wait()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://cache.example/empty", nil))
	h.writes.Wait()
	res := rec.Result()
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK || rec.Body.Len() != 0 || res.Trailer.Get("X-Final-Checksum") != "complete" {
		t.Fatalf("bodyless response or final trailer was lost: status=%d body=%q trailers=%v", res.StatusCode, rec.Body.String(), res.Trailer)
	}
	if cache.Stats().ItemCount != 0 {
		t.Fatal("no-store response was cached")
	}
}

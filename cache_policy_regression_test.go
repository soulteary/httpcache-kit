package httpcache

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheControlFieldLinesAndCase(t *testing.T) {
	h := http.Header{"Cache-Control": {"MAX-AGE=60"}, "cache-control": {"No-Store"}}
	cc, err := ParseCacheControlHeaders(h)
	if err != nil {
		t.Fatal(err)
	}
	if !cc.Has("no-store") || !cc.Has("MAX-AGE") {
		t.Fatalf("directives lost: %v", cc)
	}
	if age, err := cc.Duration("MAX-AGE"); err != nil || age != time.Minute {
		t.Fatalf("max-age=%v err=%v", age, err)
	}
	req := httptest.NewRequest("GET", "http://cache.example/package.deb", nil)
	req.Header = h
	cacheReq, err := newCacheRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if cacheReq.isCacheable() {
		t.Fatal("no-store in the second field line was ignored")
	}
}

func TestAuthenticatedNoStoreAfterExistingRequestDirective(t *testing.T) {
	var hits atomic.Int64
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		body := r.Header.Get("Authorization")
		if body == "" {
			body = "anonymous"
		} else {
			w.Header().Add("Cache-Control", "no-store")
		}
		_, _ = io.WriteString(w, body)
	})
	h := NewSharedHandler(NewMemoryCache(), upstream)
	t.Cleanup(func() { h.writes.Wait() })
	request := func(private bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "http://cache.example/package.deb", nil)
		if private {
			req.Header.Set("Authorization", "Bearer alice")
			req.Header.Set("Cache-Control", "max-age=3600")
			req.Header.Add("Cache-Control", "no-store")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		h.writes.Wait()
		return rec
	}
	request(false)
	rec := request(true)
	if rec.Body.String() != "Bearer alice" || hits.Load() != 2 {
		t.Fatalf("private request used anonymous cache: body=%q hits=%d", rec.Body.String(), hits.Load())
	}
	if rec := request(false); rec.Body.String() != "anonymous" || hits.Load() != 2 {
		t.Fatalf("private response replaced shared entry: %q hits=%d", rec.Body.String(), hits.Load())
	}
}

func TestExplicitZeroFreshness(t *testing.T) {
	for _, tc := range []struct {
		policy string
		shared bool
		want   time.Duration
	}{
		{"max-age=0", false, 0},
		{"max-age=0, s-maxage=120", true, 120 * time.Second},
		{"max-age=3600, s-maxage=0", true, 0},
		{"max-age=3600, s-maxage=0", false, time.Hour},
	} {
		t.Run(tc.policy+"/shared="+stringBool(tc.shared), func(t *testing.T) {
			res := NewResourceBytes(200, []byte("old"), http.Header{"Cache-Control": {tc.policy}, "Last-Modified": {"Wed, 01 Jan 2020 00:00:00 GMT"}, "Expires": {time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)}})
			age, err := res.MaxAge(tc.shared)
			if err != nil || age != tc.want {
				t.Fatalf("freshness=%v want=%v err=%v", age, tc.want, err)
			}
			if !res.HasExplicitExpiration() || res.HeuristicFreshness() != 0 {
				t.Fatal("explicit zero expiration fell back to Last-Modified heuristic")
			}
		})
	}
	for _, policy := range []string{"max-age=0", "public, max-age=3600, s-maxage=0"} {
		t.Run("handler/"+policy, func(t *testing.T) {
			var hits atomic.Int64
			h := NewSharedHandler(NewMemoryCache(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set("Cache-Control", policy)
				w.Header().Set("Last-Modified", "Wed, 01 Jan 2020 00:00:00 GMT")
				_, _ = io.WriteString(w, "metadata")
			}))
			t.Cleanup(func() { h.writes.Wait() })
			for i := 0; i < 2; i++ {
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://cache.example/InRelease", nil))
				h.writes.Wait()
			}
			if hits.Load() < 2 {
				t.Fatal("explicit zero freshness reused without contacting upstream")
			}
		})
	}
}

func stringBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestValidatorRejectsNon304AndCanceledResponses(t *testing.T) {
	for _, status := range []int{200, 401, 403, 404, 500, 502, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			original := http.Header{"Etag": {`"v1"`}, "Content-Length": {"9"}, "Cache-Control": {"public, max-age=1, must-revalidate"}}
			res := NewResourceBytes(200, []byte("old-index"), original.Clone())
			validator := &Validator{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "9")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "new-index")
			})}
			if validator.Validate(httptest.NewRequest("GET", "http://cache.example/InRelease", nil), res) {
				t.Fatalf("status %d accepted as validation", status)
			}
			if !reflect.DeepEqual(res.Header(), original) {
				t.Fatal("failed validation changed cached headers")
			}
			body, _ := io.ReadAll(res)
			if string(body) != "old-index" {
				t.Fatalf("cached body changed: %q", body)
			}
		})
	}
}

func TestValidator304MergesRepresentationMetadata(t *testing.T) {
	old := http.Header{"Etag": {`"v1"`}, "Content-Length": {"8"}, "Content-Type": {"application/octet-stream"}, "Content-Encoding": {"gzip"}, "Cache-Control": {"max-age=3600"}, "Age": {"60"}, "Last-Modified": {"Wed, 01 Jan 2020 00:00:00 GMT"}}
	res := NewResourceBytes(200, []byte("complete"), old.Clone())
	// Prime the parsed directive cache before the 304 changes freshness.
	if _, err := res.MaxAge(true); err != nil {
		t.Fatal(err)
	}
	validator := &Validator{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=1800")
		w.WriteHeader(304)
	})}
	if !validator.Validate(httptest.NewRequest("GET", "http://cache.example/package.deb", nil), res) {
		t.Fatal("valid 304 rejected")
	}
	for _, name := range []string{"Etag", "Content-Length", "Content-Type", "Content-Encoding", "Last-Modified"} {
		if res.Header().Get(name) != old.Get(name) {
			t.Fatalf("304 lost %s: %v", name, res.Header())
		}
	}
	if maxAge, err := res.MaxAge(true); err != nil || maxAge != 30*time.Minute {
		t.Fatalf("cached directives not refreshed: %v %v", maxAge, err)
	}
	if res.Header().Get("Age") == "60" {
		t.Fatal("previous response Age survived revalidation")
	}
}

func TestRepositoryValidationOverHTTP(t *testing.T) {
	for _, status := range []int{200, 304, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var hits atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := hits.Add(1)
				w.Header().Set("Cache-Control", "public, max-age=3600, must-revalidate")
				if n == 1 {
					w.Header().Set("Etag", `"v1"`)
					w.Header().Set("Content-Length", "9")
					_, _ = io.WriteString(w, "old-index")
					return
				}
				if status == 304 {
					w.WriteHeader(304)
					return
				}
				w.Header().Set("Content-Length", "9")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "new-index")
			}))
			defer upstream.Close()
			target, _ := url.Parse(upstream.URL)
			cache, err := NewDiskCacheWithConfig(t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := cache.Close(); err != nil {
					t.Error(err)
				}
			}()
			h := NewSharedHandler(cache, httputil.NewSingleHostReverseProxy(target))
			request := func(validate bool) *httptest.ResponseRecorder {
				req := httptest.NewRequest("GET", "http://cache.example/InRelease", nil)
				if validate {
					req.Header.Set("Cache-Control", "min-fresh=7200")
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				h.writes.Wait()
				return rec
			}
			request(false)
			rec := request(true)
			assertRepositoryValidationResponse(t, status, rec, hits.Load(), cache)
		})
	}
}

func assertRepositoryValidationResponse(t *testing.T, status int, rec *httptest.ResponseRecorder, hits int64, cache Cache) {
	t.Helper()
	if status != http.StatusNotModified {
		if rec.Code != status || rec.Body.String() != "new-index" {
			t.Fatalf("non-304 response: status=%d body=%q want status=%d/new-index", rec.Code, rec.Body.String(), status)
		}
		return
	}
	if rec.Code != http.StatusOK || rec.Body.String() != "old-index" || hits != 2 {
		t.Fatalf("304 response: status=%d body=%q hits=%d", rec.Code, rec.Body.String(), hits)
	}
	header, err := cache.Header("GET:http://cache.example/InRelease")
	if err != nil {
		t.Fatal(err)
	}
	if header.Get("Content-Length") != "9" || !strings.Contains(header.Get("Content-Type"), "text/plain") {
		t.Fatalf("persisted metadata lost: %v", header)
	}
}

func TestValidatorCanceled304LeavesResourceUntouched(t *testing.T) {
	original := http.Header{"Etag": {`"v1"`}, "Content-Length": {"8"}, "Cache-Control": {"max-age=3600"}}
	for _, cancelBefore := range []bool{false, true} {
		t.Run(stringBool(cancelBefore), func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://cache.example/package.deb", nil)
			ctx, cancel := context.WithCancel(req.Context())
			defer cancel()
			req = req.WithContext(ctx)
			if cancelBefore {
				cancel()
			}
			res := NewResourceBytes(200, []byte("complete"), original.Clone())
			var called bool
			validator := &Validator{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				cancel()
				w.Header().Set("Cache-Control", "max-age=7200")
				w.WriteHeader(304)
			})}
			if validator.Validate(req, res) {
				t.Fatal("canceled request was accepted as validation")
			}
			if cancelBefore && called {
				t.Fatal("already canceled request contacted upstream")
			}
			if !reflect.DeepEqual(res.Header(), original) {
				t.Fatal("canceled validation changed stored metadata")
			}
		})
	}
}

func TestValidationCannotFreshenWithdrawnSharedPolicy(t *testing.T) {
	for _, policy := range []string{"no-store", "no-cache", "private"} {
		t.Run(policy, func(t *testing.T) {
			var hits atomic.Int64
			cache := NewMemoryCache()
			h := NewSharedHandler(cache, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := hits.Add(1)
				w.Header().Set("Etag", `"v1"`)
				if n == 1 {
					w.Header().Set("Cache-Control", "public, max-age=3600")
					_, _ = io.WriteString(w, "complete")
					return
				}
				w.Header().Set("Cache-Control", policy)
				if r.Header.Get("If-None-Match") != "" {
					w.WriteHeader(304)
					return
				}
				_, _ = io.WriteString(w, "complete")
			}))
			t.Cleanup(func() { h.writes.Wait() })
			get := func(directive string) *httptest.ResponseRecorder {
				req := httptest.NewRequest("GET", "http://cache.example/package.deb", nil)
				if directive != "" {
					req.Header.Set("Cache-Control", directive)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				h.writes.Wait()
				return rec
			}
			get("")
			get("min-fresh=7200")
			before := hits.Load()
			if rec := get("only-if-cached"); rec.Code != 504 || hits.Load() != before {
				t.Fatalf("withdrawn cache policy remained reusable: status=%d hits=%d/%d", rec.Code, hits.Load(), before)
			}
		})
	}
}

func TestValidator304CanonicalizesMetadataUpdates(t *testing.T) {
	res := NewResourceBytes(200, []byte("body"), http.Header{"Etag": {`"v1"`}, "Cache-Control": {"max-age=3600"}, "Content-Length": {"4"}})
	validator := &Validator{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["cache-control"] = []string{"max-age=0"}
		w.Header()["etag"] = []string{`"v1"`}
		w.WriteHeader(304)
	})}
	if !validator.Validate(httptest.NewRequest("GET", "http://cache.example/object", nil), res) {
		t.Fatal("compatible lowercase metadata rejected")
	}
	if got := res.Header().Values("Cache-Control"); !reflect.DeepEqual(got, []string{"max-age=0"}) {
		t.Fatalf("old metadata survived case-insensitive replacement: %v", res.Header())
	}
	if maxAge, err := res.MaxAge(true); err != nil || maxAge != 0 {
		t.Fatalf("freshness=%v err=%v", maxAge, err)
	}
	// Validator comparison must normalize raw names too; a changed lowercase
	// ETag cannot be ignored while its value overwrites the stored validator.
	validator.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["etag"] = []string{`"v2"`}
		w.WriteHeader(304)
	})
	if validator.Validate(httptest.NewRequest("GET", "http://cache.example/object", nil), res) {
		t.Fatal("changed lowercase ETag accepted as validation")
	}
	if res.Header().Get("Etag") != `"v1"` {
		t.Fatal("rejected validation changed stored validator")
	}
}

func TestShared304MetadataPolicyAndPrivacy(t *testing.T) {
	for _, backend := range []string{"memory", "disk"} {
		t.Run(backend, func(t *testing.T) {
			cache := newRegressionTestCache(t, backend)
			for _, privateField := range []bool{false, true} {
				t.Run(stringBool(privateField), func(t *testing.T) {
					var hits atomic.Int64
					path := "http://cache.example/" + stringBool(privateField)
					h := NewSharedHandler(cache, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						hits.Add(1)
						w.Header().Set("ETag", `"v1"`)
						if r.Header.Get("If-None-Match") != "" {
							if privateField {
								w.Header()["cache-control"] = []string{`max-age=3600, private="X-Secret"`}
								w.Header()["x-secret"] = []string{"alice"}
							} else {
								w.Header()["cache-control"] = []string{"max-age=0"}
							}
							w.WriteHeader(304)
							return
						}
						w.Header().Set("Cache-Control", "max-age=3600")
						_, _ = io.WriteString(w, "body")
					}))
					t.Cleanup(func() { h.writes.Wait() })
					get := func(directive string) *httptest.ResponseRecorder {
						req := httptest.NewRequest("GET", path, nil)
						if directive != "" {
							req.Header.Set("Cache-Control", directive)
						}
						rec := httptest.NewRecorder()
						h.ServeHTTP(rec, req)
						h.writes.Wait()
						return rec
					}
					get("")
					validated := get("min-fresh=7200")
					third := get("")
					if privateField {
						if validated.Header().Get("X-Secret") != "" || third.Header().Get("X-Secret") != "" {
							t.Fatalf("private metadata served: validated=%v third=%v", validated.Header(), third.Header())
						}
						stored, err := cache.Header("GET:" + path)
						if err != nil {
							t.Fatal(err)
						}
						if stored.Get("X-Secret") != "" {
							t.Fatalf("private 304 metadata persisted: %v", stored)
						}
						if hits.Load() != 2 {
							t.Fatalf("unexpected private-field refreshes: %d", hits.Load())
						}
					} else if hits.Load() != 3 {
						t.Fatalf("lowercase zero lifetime ignored: hits=%d validated=%v third=%v", hits.Load(), validated.Header(), third.Header())
					}
				})
			}
		})
	}
}

func TestExternallyConstructedCacheControlKeepsMixedCaseEntries(t *testing.T) {
	for _, spelling := range []string{"MAX-AGE", "mAx-AgE"} {
		t.Run(spelling, func(t *testing.T) {
			cc := CacheControl{spelling: {"60"}, "No-Store": {}}
			before := CacheControl{spelling: {"60"}, "No-Store": {}}
			assertMixedCaseDirectiveLookups(t, cc)
			if value, exists := cc.Get("NO-STORE"); !exists || value != "" {
				t.Fatalf("mixed-case flag lookup: value=%q exists=%v", value, exists)
			}
			if !reflect.DeepEqual(cc, before) {
				t.Fatal("reading directives mutated caller's map")
			}
			cc.Add("max-age", "120")
			cc.Add("MAX-AGE", "180")
			cc.Add("NO-STORE", "")
			if len(cc) != 2 || !reflect.DeepEqual(cc[spelling], []string{"60", "120", "180"}) {
				t.Fatalf("Add split or renamed externally constructed directive: %v", cc)
			}
			cc.Add("PUBLIC", "")
			if !cc.Has("public") || len(cc) != 3 {
				t.Fatalf("new directive was not normalized: %v", cc)
			}
		})
	}
}

func assertMixedCaseDirectiveLookups(t *testing.T, cc CacheControl) {
	t.Helper()
	for _, query := range []string{"max-age", "MAX-AGE", "mAx-AgE"} {
		if value, exists := cc.Get(query); !exists || value != "60" {
			t.Fatalf("literal lookup %q: value=%q exists=%v", query, value, exists)
		}
		if !cc.Has(query) {
			t.Fatalf("literal Has(%q) false", query)
		}
		if duration, err := cc.Duration(query); err != nil || duration != time.Minute {
			t.Fatalf("literal Duration(%q)=%v err=%v", query, duration, err)
		}
	}
}

func newRegressionTestCache(t *testing.T, backend string) ExtendedCache {
	t.Helper()
	var cache ExtendedCache
	if backend == "memory" {
		cache = NewMemoryCache().(ExtendedCache)
	} else {
		var err error
		cache, err = NewDiskCacheWithConfig(t.TempDir(), nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if err := cache.Close(); err != nil {
			t.Error(err)
		}
	})
	return cache
}

func TestSMaxageOnlyUsesPrivateHeuristicFreshness(t *testing.T) {
	for _, policy := range []string{"s-maxage=0", "s-maxage=60"} {
		for _, shared := range []bool{false, true} {
			t.Run(policy+"/shared="+stringBool(shared), func(t *testing.T) {
				var hits atomic.Int64
				h := NewHandler(NewMemoryCache(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					w.Header().Set("Cache-Control", policy)
					w.Header().Set("Last-Modified", time.Now().UTC().Add(-24*time.Hour).Format(http.TimeFormat))
					if r.Header.Get("If-Modified-Since") != "" {
						w.WriteHeader(304)
						return
					}
					_, _ = io.WriteString(w, "metadata")
				}))
				h.Shared = shared
				t.Cleanup(func() { h.writes.Wait() })
				for i := 0; i < 2; i++ {
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, httptest.NewRequest("GET", "http://cache.example/InRelease", nil))
					h.writes.Wait()
					if rec.Code != 200 || rec.Body.String() != "metadata" {
						t.Fatalf("response=%d %q", rec.Code, rec.Body.String())
					}
				}
				wantHits := int64(1)
				if shared && policy == "s-maxage=0" {
					wantHits = 2
				}
				if hits.Load() != wantHits {
					t.Fatalf("profile ignored shared-only freshness: policy=%s shared=%v hits=%d want=%d", policy, shared, hits.Load(), wantHits)
				}
				res := NewResourceBytes(200, nil, http.Header{"Cache-Control": {policy}, "Last-Modified": {time.Now().UTC().Add(-24 * time.Hour).Format(http.TimeFormat)}})
				if !res.HasExplicitExpiration() {
					t.Fatal("public metadata predicate no longer recognizes s-maxage")
				}
				if res.heuristicFreshness(false) <= 0 || res.heuristicFreshness(true) != 0 {
					t.Fatal("heuristic expiration was not cache-profile aware")
				}
			})
		}
	}
}

package httpcache

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestValidator304RemovesTransientWarnings(t *testing.T) {
	transformation := `214 proxy "Transformed, with \"quoted, text\" and \\ escapes" "Wed, 21 Oct 2015 07:28:00 GMT"`
	miscellaneous := `299 proxy "Persistent, miscellaneous" "Wed, 21 Oct 2015 07:28:00 GMT"`
	for _, tc := range []struct {
		name     string
		stored   []string
		incoming []string
		want     []string
	}{
		{
			name:   "multiple stored field lines",
			stored: []string{`110 - "Response is Stale"`, transformation, `113 - "Heuristic Expiration"`, miscellaneous},
			want:   []string{transformation, miscellaneous},
		},
		{
			name:   "stored list with quoted commas and escapes",
			stored: []string{`110 - "Stale, possibly", ` + transformation + `, 113 - "Heuristic, old", ` + miscellaneous},
			want:   []string{transformation + ", " + miscellaneous},
		},
		{
			name:     "304 supplements retained persistent warnings",
			stored:   []string{transformation},
			incoming: []string{`110 - "Response is Stale"`, miscellaneous},
			want:     []string{transformation, miscellaneous},
		},
		{
			name:     "incoming list with quoted commas and escapes",
			incoming: []string{`110 - "Stale, possibly", ` + transformation + `, 113 - "Heuristic, old", ` + miscellaneous},
			want:     []string{transformation + ", " + miscellaneous},
		},
		{
			name:     "repeated persistent values stay bounded",
			stored:   []string{transformation},
			incoming: []string{transformation, miscellaneous},
			want:     []string{transformation, miscellaneous},
		},
		{
			name:     "all transient warnings disappear",
			stored:   []string{`110 - "Response is Stale", 113 - "Heuristic Expiration"`},
			incoming: []string{`199 proxy "Transient, miscellaneous"`},
		},
		{
			name:   "do not classify malformed code prefixes as 1xx",
			stored: []string{`1100 proxy "Unrecognized", unknown`},
			want:   []string{`1100 proxy "Unrecognized", unknown`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := NewResourceBytes(200, []byte("body"), http.Header{"Etag": {`"v1"`}, "Warning": tc.stored})
			validator := &Validator{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("If-None-Match") != `"v1"` {
					t.Errorf("conditional validator missing: %v", r.Header)
				}
				// Embedding handlers may use raw lower-case map keys.
				w.Header()["warning"] = tc.incoming
				w.WriteHeader(http.StatusNotModified)
			})}
			// Repeated validation must retain the same persistent warnings
			// without accumulating copies from identical 304 field values.
			for i := 0; i < 2; i++ {
				if !validator.Validate(httptest.NewRequest("GET", "http://cache.example/object", nil), res) {
					t.Fatal("compatible 304 rejected")
				}
				if got := res.Header().Values("Warning"); !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("validation %d Warning=%q, want=%q", i+1, got, tc.want)
				}
				if len(tc.want) == 0 {
					if _, exists := res.Header()["Warning"]; exists {
						t.Fatal("empty Warning field remains after transient values were removed")
					}
				}
			}
		})
	}
}

func TestRejectedValidationPreservesWarningMetadata(t *testing.T) {
	for _, mode := range []string{"upstream failure", "changed validator", "canceled validation"} {
		t.Run(mode, func(t *testing.T) {
			original := http.Header{
				"Etag":          {`"v1"`},
				"Cache-Control": {"max-age=3600"},
				"Warning":       {`110 - "Response is Stale", 214 proxy "Transformed, old"`, `113 - "Heuristic Expiration"`},
			}
			res := NewResourceBytes(200, []byte("body"), original.Clone())
			if _, err := res.cacheControl(); err != nil {
				t.Fatal(err)
			}
			originalDirectives := res.cc
			req := httptest.NewRequest("GET", "http://cache.example/object", nil)
			ctx, cancel := context.WithCancel(req.Context())
			defer cancel()
			req = req.WithContext(ctx)
			validator := &Validator{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Warning", `299 proxy "Updated, persistent"`)
				w.Header().Set("Cache-Control", "max-age=7200")
				switch mode {
				case "upstream failure":
					w.WriteHeader(http.StatusServiceUnavailable)
				case "changed validator":
					w.Header().Set("ETag", `"v2"`)
					w.WriteHeader(http.StatusNotModified)
				case "canceled validation":
					cancel()
					w.WriteHeader(http.StatusNotModified)
				}
			})}
			if validator.Validate(req, res) {
				t.Fatal("rejected response accepted as validation")
			}
			if !reflect.DeepEqual(res.Header(), original) || !reflect.DeepEqual(res.cc, originalDirectives) || !res.RequestTime.IsZero() {
				t.Fatalf("rejected validation mutated stored metadata: %v", res.Header())
			}
		})
	}
}

func TestShared304WarningCleanupPersists(t *testing.T) {
	for _, backend := range []string{"memory", "disk"} {
		t.Run(backend, func(t *testing.T) {
			cache := newRegressionTestCache(t, backend)
			var hits atomic.Int64
			retained := `214 proxy "Transformed, persistently"`
			added := `299 proxy "Persistent, new"`
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set("ETag", `"v1"`)
				w.Header().Set("Cache-Control", "max-age=3600")
				if r.Header.Get("If-None-Match") != "" {
					w.Header().Add("Warning", `110 - "Response is Stale", `+added)
					w.WriteHeader(http.StatusNotModified)
					return
				}
				w.Header().Add("Warning", `110 - "Response is Stale", `+retained)
				w.Header().Add("Warning", `113 - "Heuristic Expiration"`)
				_, _ = io.WriteString(w, "body")
			})
			h := NewSharedHandler(cache, upstream)
			t.Cleanup(func() { h.writes.Wait() })
			get := func(validate bool) *httptest.ResponseRecorder {
				req := httptest.NewRequest("GET", "http://cache.example/object", nil)
				if validate {
					req.Header.Set("Cache-Control", "min-fresh=7200")
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				h.writes.Wait()
				return rec
			}
			get(false)
			for _, rec := range []*httptest.ResponseRecorder{get(true), get(false)} {
				if rec.Code != 200 || rec.Body.String() != "body" || rec.Header().Get(CacheHeader) != "HIT" {
					t.Fatalf("validated representation not reused: status=%d body=%q headers=%v", rec.Code, rec.Body.String(), rec.Header())
				}
				if got := rec.Header().Values("Warning"); !reflect.DeepEqual(got, []string{retained, added}) {
					t.Fatalf("cached response retained transient warnings: %q", got)
				}
			}
			stored, err := cache.Header("GET:http://cache.example/object")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stored.Values("Warning"), []string{retained, added}) || hits.Load() != 2 {
				t.Fatalf("warning cleanup not persisted: warnings=%q upstream hits=%d", stored.Values("Warning"), hits.Load())
			}
		})
	}
}

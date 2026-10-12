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

func TestConditionalRangeUsesOriginalClientPrecondition(t *testing.T) {
	var calls atomic.Int64
	c := NewMemoryCache().(ExtendedCache)
	defer func() { _ = c.Close() }()
	h := NewSharedHandler(c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Cache-Control", "max-age=3600")
		if n == 1 {
			w.Header().Set("ETag", `"v1"`)
		} else {
			w.Header().Set("ETag", `"v2"`)
		}
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader("abcdef"))
	}))
	defer h.writes.Wait()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://cache.example/index", nil))
	h.writes.Wait()
	r := httptest.NewRequest("GET", "http://cache.example/index", nil)
	r.Header.Set("If-None-Match", `"v2"`)
	r.Header.Set("Range", "bytes=0-2")
	r.Header.Set("Cache-Control", "min-fresh=7200")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	h.writes.Wait()
	if w.Code != 304 || w.Body.Len() != 0 || calls.Load() != 2 {
		t.Fatalf("partial client precondition failed: %d %q calls=%d", w.Code, w.Body.String(), calls.Load())
	}
}

func TestVaryKeysPreserveDistinctFieldLineBoundaries(t *testing.T) {
	var calls atomic.Int64
	c := NewMemoryCache().(ExtendedCache)
	defer func() { _ = c.Close() }()
	h := NewSharedHandler(c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("Vary", "Cookie")
		_, _ = io.WriteString(w, strings.Join(r.Header.Values("Cookie"), "|"))
	}))
	defer h.writes.Wait()
	for _, values := range [][]string{{"session=a", "role=b"}, {"session=a, role=b"}, {"session=a, role=b"}} {
		r := httptest.NewRequest("GET", "http://cache.example/index", nil)
		r.Header["Cookie"] = values
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		h.writes.Wait()
		if w.Body.String() != strings.Join(values, "|") {
			t.Fatalf("field boundaries aliased: %q", w.Body.String())
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d want2", calls.Load())
	}
}

func TestConditionalValidatorPanicBecomesFailedResponse(t *testing.T) {
	var calls atomic.Int64
	c := NewMemoryCache().(ExtendedCache)
	defer func() { _ = c.Close() }()
	h := NewSharedHandler(c, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) > 1 {
			panic("upstream panic")
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Cache-Control", "max-age=3600")
		_, _ = io.WriteString(w, "body")
	}))
	defer h.writes.Wait()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://cache.example/index", nil))
	h.writes.Wait()
	r := httptest.NewRequest("GET", "http://cache.example/index", nil)
	r.Header.Set("Cache-Control", "min-fresh=7200")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 502 || calls.Load() != 2 {
		t.Fatalf("panic response=%d calls=%d", w.Code, calls.Load())
	}
}

func TestConditionalClientWrapperPreservesNetworkTrailers(t *testing.T) {
	var calls atomic.Int64
	c := NewMemoryCache().(ExtendedCache)
	defer func() { _ = c.Close() }()
	h := NewSharedHandler(c, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("ETag", `"v1"`)
		if n > 1 {
			w.Header().Set("ETag", `"v2"`)
			w.Header().Set("Trailer", "X-Declared")
		}
		_, _ = io.WriteString(w, "body")
		if n > 1 {
			w.Header().Set("X-Declared", "done")
			w.Header().Set(http.TrailerPrefix+"X-Late", "late")
		}
	}))
	defer func() { _ = h.Shutdown(context.Background()) }()
	server := httptest.NewServer(h)
	defer server.Close()
	first, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, first.Body)
	_ = first.Body.Close()
	h.writes.Wait()
	r, err := http.NewRequest("GET", server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Cache-Control", "min-fresh=7200")
	r.Header.Set("If-None-Match", `"unmatched"`)
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if err != nil || res.StatusCode != 200 || res.Trailer.Get("X-Declared") != "done" || res.Trailer.Get("X-Late") != "late" {
		t.Fatalf("lost trailers: status=%d trailer=%+v err=%v", res.StatusCode, res.Trailer, err)
	}
}

func TestConditionalFullResponseIsReusedAndStored(t *testing.T) {
	var calls atomic.Int64
	cache := NewMemoryCache().(ExtendedCache)
	defer func() { _ = cache.Close() }()
	h := NewSharedHandler(cache, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Cache-Control", "max-age=3600")
		switch n {
		case 1:
			w.Header().Set("ETag", `"v1"`)
			_, _ = io.WriteString(w, "old-index")
		case 2:
			if r.Header.Get("If-None-Match") != `"v1"` {
				t.Error("missing conditional validator")
			}
			w.Header().Set("ETag", `"v2"`)
			_, _ = io.WriteString(w, "new-index")
		default:
			w.WriteHeader(503)
		}
	}))
	defer h.writes.Wait()
	get := func(force bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://cache.example/InRelease", nil)
		if force {
			r.Header.Set("Cache-Control", "min-fresh=7200")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		h.writes.Wait()
		return w
	}
	get(false)
	for _, force := range []bool{true, false} {
		w := get(force)
		if w.Code != 200 || w.Body.String() != "new-index" || w.Header().Get("ETag") != `"v2"` || calls.Load() != 2 {
			t.Fatalf("status=%d body=%q etag=%q calls=%d", w.Code, w.Body.String(), w.Header().Get("ETag"), calls.Load())
		}
	}
}

func TestAllVaryFieldLinesAndRequestValues(t *testing.T) {
	var calls atomic.Int64
	cache := NewMemoryCache().(ExtendedCache)
	defer func() { _ = cache.Close() }()
	h := NewSharedHandler(cache, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Add("Vary", "Accept-Encoding")
		w.Header()["vary"] = []string{"Accept-Language"}
		_, _ = io.WriteString(w, strings.Join(r.Header.Values("Accept-Language"), "/"))
	}))
	defer h.writes.Wait()
	for _, language := range []string{"zh", "en", "en"} {
		r := httptest.NewRequest("GET", "http://cache.example/object", nil)
		r.Header["Accept-Language"] = []string{"shared", language}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		h.writes.Wait()
		if w.Body.String() != "shared/"+language {
			t.Fatalf("wrong variant: %q", w.Body.String())
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls=%d want2", calls.Load())
	}
}

func TestVaryWildcardInAdditionalFieldIsNotStored(t *testing.T) {
	var calls atomic.Int64
	cache := NewMemoryCache().(ExtendedCache)
	defer func() { _ = cache.Close() }()
	h := NewSharedHandler(cache, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Add("Vary", "Accept-Encoding")
		w.Header().Add("Vary", "*")
		_, _ = io.WriteString(w, "body")
	}))
	defer h.writes.Wait()
	for range 2 {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://cache.example/object", nil))
		h.writes.Wait()
	}
	if calls.Load() != 2 || cache.Stats().ItemCount != 0 {
		t.Fatalf("wildcard cached: calls=%d stats=%+v", calls.Load(), cache.Stats())
	}
}

func TestExpiresLifetimeUsesOriginDateOnce(t *testing.T) {
	old := Clock
	defer func() { Clock = old }()
	base := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	Clock = func() time.Time { return base.Add(6 * time.Second) }
	r := NewResourceBytes(200, nil, http.Header{"Date": {base.Format(http.TimeFormat)}, "Expires": {base.Add(10 * time.Second).Format(http.TimeFormat)}, ProxyDateHeader: {base.Format(http.TimeFormat)}})
	q, err := newCacheRequest(httptest.NewRequest("GET", "http://cache.example/object", nil))
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := (&Handler{Shared: true}).freshness(r, q)
	if err != nil || remaining != 4*time.Second {
		t.Fatalf("remaining=%s err=%v", remaining, err)
	}
	r.Header().Set("Expires", "0")
	r.Header().Set("Last-Modified", base.Add(-24*time.Hour).Format(http.TimeFormat))
	if !r.HasExplicitExpiration() || r.HeuristicFreshness() != 0 {
		t.Fatal("invalid explicit Expires used heuristic freshness")
	}
}

func TestConditionalFullResponseHonorsOriginalClientValidator(t *testing.T) {
	for _, condition := range []string{"etag", "date"} {
		t.Run(condition, func(t *testing.T) {
			var calls atomic.Int64
			cache := NewMemoryCache().(ExtendedCache)
			defer func() { _ = cache.Close() }()
			modified := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
			h := NewSharedHandler(cache, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				w.Header().Set("Cache-Control", "max-age=3600")
				if n == 1 {
					w.Header().Set("ETag", `"v1"`)
					_, _ = io.WriteString(w, "old")
					return
				}
				if n != 2 || r.Header.Get("If-None-Match") != `"v1"` {
					t.Error("unexpected validation request")
				}
				w.Header().Set("ETag", `"v2"`)
				w.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
				w.Header().Set("Content-Length", "3")
				_, _ = io.WriteString(w, "new")
			}))
			defer h.writes.Wait()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://cache.example/index", nil))
			h.writes.Wait()
			req := httptest.NewRequest("GET", "http://cache.example/index", nil)
			req.Header.Set("Cache-Control", "min-fresh=7200")
			if condition == "etag" {
				req.Header.Set("If-None-Match", `W/"other", W/"v2"`)
			} else {
				req.Header.Set("If-Modified-Since", modified.Format(http.TimeFormat))
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			h.writes.Wait()
			if w.Code != 304 || w.Body.Len() != 0 || calls.Load() != 2 || w.Header().Get("Content-Length") != "" {
				t.Fatalf("wrong client response: %d %q %+v calls=%d", w.Code, w.Body.String(), w.Header(), calls.Load())
			}
			w = httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "http://cache.example/index", nil))
			if w.Code != 200 || w.Body.String() != "new" || calls.Load() != 2 {
				t.Fatal("new full representation was not cached")
			}
		})
	}
}

func TestVaryValidationDoesNotFreshenAnotherBaseGeneration(t *testing.T) {
	var calls atomic.Int64
	cache := NewMemoryCache().(ExtendedCache)
	defer func() { _ = cache.Close() }()
	h := NewSharedHandler(cache, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		language := r.Header.Get("Accept-Language")
		w.Header().Set("Vary", "Accept-Language")
		w.Header().Set("ETag", `"`+language+`"`)
		w.Header().Set("Cache-Control", "max-age=3600")
		if r.Header.Get("If-None-Match") == `"`+language+`"` {
			w.WriteHeader(304)
			return
		}
		_, _ = io.WriteString(w, language)
	}))
	defer h.writes.Wait()
	get := func(language string, force bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "http://cache.example/index", nil)
		req.Header.Set("Accept-Language", language)
		if force {
			req.Header.Set("Cache-Control", "min-fresh=7200")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		h.writes.Wait()
		return w
	}
	get("en", false)
	get("fr", false)
	for _, force := range []bool{true, false} {
		w := get("en", force)
		if w.Code != 200 || w.Body.String() != "en" || calls.Load() != 3 {
			t.Fatalf("validated variant was refetched: %d %q calls=%d", w.Code, w.Body.String(), calls.Load())
		}
	}
	if w := get("fr", false); w.Code != 200 || w.Body.String() != "fr" || calls.Load() != 3 {
		t.Fatal("newer base variant was modified")
	}
}

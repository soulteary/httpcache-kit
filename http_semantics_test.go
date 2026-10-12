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

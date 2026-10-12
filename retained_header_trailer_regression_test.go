package httpcache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type retainedTrailerWireFixture struct {
	h         *Handler
	server    *httptest.Server
	client    *http.Client
	completed chan struct{}
}

func newRetainedTrailerWireFixture(t *testing.T, h *Handler, serve http.HandlerFunc) *retainedTrailerWireFixture {
	t.Helper()
	f := &retainedTrailerWireFixture{h: h, completed: make(chan struct{}, 1)}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { f.completed <- struct{}{} }()
		serve(w, r)
	}))
	f.client = f.server.Client()
	f.client.Timeout = 5 * time.Second
	t.Cleanup(func() {
		f.server.CloseClientConnections()
		f.server.Close()
		f.h.writes.Wait()
	})
	return f
}

func (f *retainedTrailerWireFixture) response(t *testing.T, req *http.Request) (*http.Response, string) {
	t.Helper()
	res, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading complete response: %v", err)
	}
	select {
	case <-f.completed:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not finish after the client consumed the body")
	}
	f.h.writes.Wait()
	return res, string(body)
}

func retainedTrailerWireRequest(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestRetainedOriginHeaderPublishesNetworkTrailer(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(stringBool(enabled), func(t *testing.T) {
			cache := NewMemoryCacheWithConfig(DefaultCacheConfig().WithCleanupInterval(0))
			t.Cleanup(func() { _ = cache.Close() })
			budget := &featureBudget{limit: 1 << 20}
			opts := &HandlerOptions{TempDir: t.TempDir()}
			if enabled {
				opts.StagingBudget = budget
			}
			const body = "origin body with a retained Header reference"
			h := NewHandlerWithOptions(cache, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				// Header is a live map. Holding its reference across WriteHeader
				// is a valid way for an origin to publish declared trailer values.
				header := w.Header()
				header.Set("Cache-Control", "max-age=3600")
				header.Set("Trailer", "X-Origin-Checksum, X-Origin-Live")
				_, _ = io.WriteString(w, body)
				header.Set("X-Origin-Checksum", "complete")
				w.Header().Set("X-Origin-Checksum", "complete-final")
				// A second trailer uses a freshly obtained Header map. Finishing
				// the retained map must preserve this independently updated value.
				w.Header().Set("X-Origin-Live", "live-complete")
			}), opts)
			f := newRetainedTrailerWireFixture(t, h, h.ServeHTTP)
			res, got := f.response(t, retainedTrailerWireRequest(t, f.server.URL+"/retained-header"))
			if res.StatusCode != http.StatusOK || got != body {
				t.Fatalf("origin response changed: status=%d body=%q", res.StatusCode, got)
			}
			if res.Trailer.Get("X-Origin-Checksum") != "complete-final" {
				t.Fatalf("retained Header lost the final trailer: headers=%v trailers=%v", res.Header, res.Trailer)
			}
			if res.Trailer.Get("X-Origin-Live") != "live-complete" {
				t.Fatalf("retained Header overwrote the live trailer: headers=%v trailers=%v", res.Header, res.Trailer)
			}
			budget.assertReleased(t)
		})
	}
}

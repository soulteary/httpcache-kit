package httpcache

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
)

const retainedValuesBody = "body with retained trailer values"

func retainedValuesOrigin(calls *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		version := `"old"`
		if calls.Add(1) > 1 {
			version = `"current"`
		}
		header := w.Header()
		header.Set("Cache-Control", "max-age=3600")
		header.Set("ETag", version)
		header.Set("Trailer", "X-Origin-Values")
		header["X-Origin-Values"] = []string{"pending", "second-pending"}
		// Values returns the live Header slice. A declared trailer can be
		// updated through this reference after the initial response commit.
		values := header.Values("X-Origin-Values")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, retainedValuesBody)
		values[0], values[1] = "done", "second-done"
	})
}

func TestRetainedTrailerValuesWorkWithNativeHTTPWriter(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(retainedValuesOrigin(&calls))
	defer server.Close()
	response := retainedValuesResponse(t, server, nil)
	assertRetainedValuesTrailer(t, response)
}

func TestRetainedTrailerValuesSurviveStagingCommit(t *testing.T) {
	for _, budgeted := range []bool{false, true} {
		t.Run(stringBool(budgeted), func(t *testing.T) {
			h, cache, _ := newRetainedValuesHandler(t, budgeted)
			server := httptest.NewServer(h)
			defer server.Close()
			response := retainedValuesResponse(t, server, nil)
			assertRetainedValuesTrailer(t, response)
			h.writes.Wait()
			assertRetainedValuesSnapshot(t, cache)
		})
	}
}

func TestRetainedTrailerValuesSurviveConditionalForwarding(t *testing.T) {
	for _, budgeted := range []bool{false, true} {
		t.Run(stringBool(budgeted), func(t *testing.T) {
			h, cache, calls := newRetainedValuesHandler(t, budgeted)
			server := httptest.NewServer(h)
			defer server.Close()
			_ = retainedValuesResponse(t, server, nil)
			h.writes.Wait()
			response := retainedValuesResponse(t, server, http.Header{
				"Cache-Control": {"min-fresh=7200"},
				"If-None-Match": {`"unmatched"`},
			})
			assertRetainedValuesTrailer(t, response)
			if calls.Load() != 2 {
				t.Fatalf("validation origin calls=%d, want 2", calls.Load())
			}
			if budgeted && response.Trailer.Get(CacheStagingHeader) != "ADMITTED" {
				t.Fatalf("conditional forwarding lost staging outcome: %v", response.Trailer)
			}
			h.writes.Wait()
			assertRetainedValuesSnapshot(t, cache)
		})
	}
}

func newRetainedValuesHandler(t *testing.T, budgeted bool) (*Handler, Cache, *atomic.Int64) {
	t.Helper()
	cache := NewMemoryCache().(ExtendedCache)
	opts := &HandlerOptions{TempDir: t.TempDir()}
	if budgeted {
		opts.StagingBudget = &featureBudget{limit: 1 << 20}
	}
	calls := &atomic.Int64{}
	h := NewHandlerWithOptions(cache, retainedValuesOrigin(calls), opts)
	t.Cleanup(func() {
		_ = h.Shutdown(context.Background())
		_ = cache.Close()
	})
	return h, cache, calls
}

func retainedValuesResponse(t *testing.T, server *httptest.Server, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, server.URL+"/trailer-values", nil)
	if err != nil {
		t.Fatal(err)
	}
	if header != nil {
		req.Header = header
	}
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(body) != retainedValuesBody {
		t.Fatalf("retained values changed response: status=%d body=%q err=%v", response.StatusCode, body, err)
	}
	return response
}

func assertRetainedValuesTrailer(t *testing.T, response *http.Response) {
	t.Helper()
	if !slices.Equal(response.Trailer.Values("X-Origin-Values"), []string{"done", "second-done"}) {
		t.Fatalf("retained Header.Values update was detached: headers=%v trailers=%v", response.Header, response.Trailer)
	}
}

func assertRetainedValuesSnapshot(t *testing.T, cache Cache) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/trailer-values", nil)
	resource, err := cache.Retrieve(NewRequestKey(req).String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resource.Close() }()
	if !slices.Equal(resource.Header().Values("X-Origin-Values"), []string{"pending", "second-pending"}) {
		t.Fatalf("live trailer update changed storage snapshot: %v", resource.Header())
	}
}

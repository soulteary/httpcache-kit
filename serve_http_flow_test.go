package httpcache

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newServeFlowCache(t *testing.T) ExtendedCache {
	t.Helper()
	c := NewMemoryCacheWithConfig(DefaultCacheConfig().WithCleanupInterval(0))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestServeHTTPMissFollowerCancellationPreservesLeader(t *testing.T) {
	var upstreamCalls atomic.Int32
	h := NewHandler(newServeFlowCache(t), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		upstreamCalls.Add(1)
	}))
	req := httptest.NewRequest(http.MethodGet, "http://example.org/canceled-follower", nil)
	key := NewRequestKey(req).String()
	flight, _ := h.claimMiss(key)
	t.Cleanup(func() { h.finishMiss(key, flight) })
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rec, req)
	}()
	waitForHandlerFlowEvent(t, done, "canceled miss follower did not return")

	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("canceled follower called upstream %d times", got)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("canceled follower wrote a response: %q", rec.Body.String())
	}
	if current, leader := h.claimMiss(key); leader || current != flight {
		t.Fatal("canceled follower removed the active leader flight")
	}
}

type serveFlowLookupFailureCache struct {
	Cache
	retrieves atomic.Int32
	failure   error
}

func (c *serveFlowLookupFailureCache) Retrieve(string) (*Resource, error) {
	if c.retrieves.Add(1) == 1 {
		return nil, ErrNotFoundInCache
	}
	return nil, c.failure
}

func TestServeHTTPMissFollowerRefetchesWhenLeaderLookupFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
	}{
		{"uncached response", ErrNotFoundInCache},
		{"cache read failure", errors.New("cache read failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &serveFlowLookupFailureCache{
				Cache:   newServeFlowCache(t),
				failure: tc.failure,
			}
			var upstreamCalls atomic.Int32
			h := NewHandler(c, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				w.Header().Set("Cache-Control", "max-age=60")
				_, _ = w.Write([]byte("refetched"))
			}))
			t.Cleanup(func() { h.writes.Wait() })
			req := httptest.NewRequest(http.MethodGet, "http://example.org/failed-leader", nil)
			key := NewRequestKey(req).String()
			// A completed flight fixture makes the follower's wakeup independent
			// of scheduling between its initial miss and claimMiss.
			completed := make(chan struct{})
			close(completed)
			h.flights[key] = &missFlight{done: completed}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			h.writes.Wait()

			if rec.Code != http.StatusOK || rec.Body.String() != "refetched" {
				t.Fatalf("follower response = %d %q, want 200 refetched", rec.Code, rec.Body.String())
			}
			if got := upstreamCalls.Load(); got != 1 {
				t.Fatalf("upstream calls = %d, want 1 independent refetch", got)
			}
			if got := c.retrieves.Load(); got != 2 {
				t.Fatalf("cache lookups = %d, want initial miss and completed leader lookup", got)
			}
		})
	}
}

type serveFlowFreshenFailureCache struct {
	Cache
	res      *Resource
	freshens atomic.Int32
}

func (c *serveFlowFreshenFailureCache) Retrieve(string) (*Resource, error) {
	return c.res, nil
}

func (c *serveFlowFreshenFailureCache) Freshen(*Resource, ...string) error {
	c.freshens.Add(1)
	return errors.New("cannot persist validation")
}

func TestServeHTTPFreshenFailureClosesAndRefetches(t *testing.T) {
	body := &closeCountingBody{Reader: bytes.NewReader([]byte("cached"))}
	res := NewResource(http.StatusOK, body, http.Header{
		"Cache-Control": {"max-age=3600"},
		"Date":          {time.Now().UTC().Format(http.TimeFormat)},
		"Etag":          {`"v1"`},
	})
	res.MarkStale()
	c := &serveFlowFreshenFailureCache{Cache: newServeFlowCache(t), res: res}
	var upstreamCalls atomic.Int32
	var closedBeforeRefetch atomic.Bool
	h := NewHandler(c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Etag", `"v1"`)
		if r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		closedBeforeRefetch.Store(atomic.LoadInt32(&body.closes) == 1)
		_, _ = w.Write([]byte("refetched"))
	}))
	t.Cleanup(func() { h.writes.Wait() })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://example.org/freshen-failure", nil))
	h.writes.Wait()

	if got := rec.Body.String(); got != "refetched" {
		t.Fatalf("body = %q, want a full refetch", got)
	}
	if got := rec.Header().Get(CacheHeader); got != "MISS" {
		t.Fatalf("cache status = %q, want MISS", got)
	}
	if got := upstreamCalls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want validation and full refetch", got)
	}
	if got := c.freshens.Load(); got != 1 {
		t.Fatalf("freshen attempts = %d, want 1", got)
	}
	if !closedBeforeRefetch.Load() {
		t.Fatal("retrieved resource was not closed exactly once before the full refetch")
	}
	if !res.IsStale() {
		t.Fatal("failed Freshen cleared the retrieved resource's stale flag")
	}
}

func TestServeHTTPOnlyIfCachedClosesResourceRequiringValidation(t *testing.T) {
	body := &closeCountingBody{Reader: bytes.NewReader([]byte("cached"))}
	res := NewResource(http.StatusOK, body, http.Header{
		"Cache-Control": {"max-age=3600"},
		"Date":          {time.Now().UTC().Format(http.TimeFormat)},
	})
	res.MarkStale()
	var upstreamCalls atomic.Int32
	h := NewHandler(&staleResourceCache{Cache: newServeFlowCache(t), res: res}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		upstreamCalls.Add(1)
	}))
	req := httptest.NewRequest(http.MethodGet, "http://example.org/only-cached-stale", nil)
	req.Header.Set("Cache-Control", "only-if-cached")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", rec.Code)
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("only-if-cached called upstream %d times", got)
	}
	if got := atomic.LoadInt32(&body.closes); got != 1 {
		t.Fatalf("retrieved resource was closed %d times, want 1", got)
	}
}

func TestServeHTTPFreshnessRequestDirectives(t *testing.T) {
	for _, tc := range []struct {
		name      string
		directive string
		policy    string
		age       time.Duration
		shared    bool
		wantCache string
	}{
		{"min-fresh satisfied", "min-fresh=10", "max-age=60", 30 * time.Second, false, "HIT"},
		{"min-fresh exceeds lifetime", "min-fresh=120", "max-age=60", 30 * time.Second, false, "MISS"},
		{"invalid min-fresh", "min-fresh=invalid", "max-age=60", 30 * time.Second, false, "MISS"},
		{"invalid request max-age", "max-age=invalid", "max-age=60", 30 * time.Second, false, "MISS"},
		{"unlimited max-stale", "max-stale", "max-age=60", 2 * time.Minute, false, "HIT"},
		{"sufficient max-stale", "max-stale=120", "max-age=60", 2 * time.Minute, false, "HIT"},
		{"insufficient max-stale", "max-stale=1", "max-age=60", 2 * time.Minute, false, "MISS"},
		{"invalid max-stale", "max-stale=invalid", "max-age=60", 2 * time.Minute, false, "MISS"},
		{"expired must-revalidate", "max-stale", "max-age=60, must-revalidate", 2 * time.Minute, false, "MISS"},
		{"fresh must-revalidate", "", "max-age=60, must-revalidate", 30 * time.Second, false, "HIT"},
		{"shared proxy-revalidate", "max-stale", "max-age=60, proxy-revalidate", 2 * time.Minute, true, "MISS"},
		{"private proxy-revalidate", "max-stale", "max-age=60, proxy-revalidate", 2 * time.Minute, false, "HIT"},
		{"shared s-maxage", "max-stale", "max-age=3600, s-maxage=60", 2 * time.Minute, true, "MISS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := NewResourceBytes(http.StatusOK, []byte("cached"), http.Header{
				"Cache-Control": {tc.policy},
				"Date":          {time.Now().UTC().Add(-tc.age).Format(http.TimeFormat)},
			})
			var upstreamCalls atomic.Int32
			h := NewHandler(&staleResourceCache{Cache: newServeFlowCache(t), res: res}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				w.Header().Set("Cache-Control", "max-age=60")
				_, _ = w.Write([]byte("origin"))
			}))
			h.Shared = tc.shared
			t.Cleanup(func() { h.writes.Wait() })
			req := httptest.NewRequest(http.MethodGet, "http://example.org/freshness-directive", nil)
			req.Header.Set("Cache-Control", tc.directive)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			h.writes.Wait()

			if got := rec.Header().Get(CacheHeader); got != tc.wantCache {
				t.Fatalf("cache status = %q, want %s", got, tc.wantCache)
			}
			wantBody, wantUpstream := "cached", int32(0)
			if tc.wantCache == "MISS" {
				wantBody, wantUpstream = "origin", 1
			}
			if got := rec.Body.String(); got != wantBody {
				t.Fatalf("body = %q, want %q", got, wantBody)
			}
			if got := upstreamCalls.Load(); got != wantUpstream {
				t.Fatalf("upstream calls = %d, want %d", got, wantUpstream)
			}
		})
	}
}

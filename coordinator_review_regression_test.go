package httpcache

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type reviewExpiredCoordinator struct {
	clientCancel context.CancelFunc
	lease        *reviewExpiredLease
}

type reviewExpiredLease struct {
	ctx    context.Context
	closed int
}

func (c *reviewExpiredCoordinator) Acquire(ctx context.Context, _ string) (MissLease, error) {
	leaseCtx, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	cancel()
	c.lease = &reviewExpiredLease{ctx: leaseCtx}
	if c.clientCancel != nil {
		c.clientCancel()
	}
	return c.lease, nil
}

func (l *reviewExpiredLease) Context() context.Context { return l.ctx }
func (l *reviewExpiredLease) Close() error {
	l.closed++
	return nil
}

func TestMissCoordinatorExpiredLeaseResponse(t *testing.T) {
	for _, cancelClient := range []bool{false, true} {
		name := "live client"
		if cancelClient {
			name = "canceled client"
		}
		t.Run(name, func(t *testing.T) {
			cache := NewMemoryCacheWithConfig(&CacheConfig{})
			defer func() { _ = cache.Close() }()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			coordinator := &reviewExpiredCoordinator{}
			if cancelClient {
				coordinator.clientCancel = cancel
			}
			var upstreamCalls atomic.Int64
			h := NewHandlerWithOptions(cache, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				upstreamCalls.Add(1)
			}), &HandlerOptions{MissCoordinator: coordinator})
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/object", nil).WithContext(ctx))
			assertReviewExpiredLease(t, h, coordinator, upstreamCalls.Load())
			if cancelClient {
				if recorder.Body.Len() != 0 || len(recorder.Header()) != 0 {
					t.Fatalf("canceled client received a response: %d %q %v", recorder.Code, recorder.Body.String(), recorder.Header())
				}
				return
			}
			if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "cache fill coordination unavailable") {
				t.Fatalf("expired ownership returned %d %q instead of 503", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func assertReviewExpiredLease(t *testing.T, h *Handler, coordinator *reviewExpiredCoordinator, upstreamCalls int64) {
	t.Helper()
	if coordinator.lease.ctx.Err() != context.DeadlineExceeded || coordinator.lease.closed != 1 {
		t.Fatalf("expired lease was not released once: err=%v closed=%d", coordinator.lease.ctx.Err(), coordinator.lease.closed)
	}
	if upstreamCalls != 0 {
		t.Fatalf("expired ownership reached upstream %d time(s)", upstreamCalls)
	}
	h.flightsMu.Lock()
	remaining := len(h.flights)
	h.flightsMu.Unlock()
	if remaining != 0 {
		t.Fatalf("expired ownership stranded %d miss flights", remaining)
	}
}

// The policy deliberately reads RequestURI, which is the server's original
// escaped request target. Replacing only URL on an invalidation request used
// to leave both response-named resources fresh under this valid policy.
func reviewRequestURIKey(r *http.Request) (Key, error) {
	u, err := url.ParseRequestURI(r.RequestURI)
	if err != nil {
		return Key{}, err
	}
	return NewKey(r.Method, u, r.Header), nil
}

func TestRequestKeyRequestURIInvalidatesResponseTargets(t *testing.T) {
	for _, form := range []struct{ name, prefix string }{{"origin form", ""}, {"absolute form", "http://example.org"}} {
		t.Run(form.name, func(t *testing.T) {
			prefix := form.prefix
			cache := NewMemoryCacheWithConfig(&CacheConfig{})
			defer func() { _ = cache.Close() }()
			var generation atomic.Int64
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					generation.Add(1)
					w.Header().Set("Location", "/objects/a%2Fb?version=A")
					w.Header().Set("Content-Location", "http://example.org/summary?")
					w.WriteHeader(http.StatusNoContent)
					return
				}
				w.Header().Set("Cache-Control", "max-age=3600")
				w.Header().Set("Date", Clock().Format(http.TimeFormat))
				body := "before mutation"
				if generation.Load() != 0 {
					body = "after mutation"
				}
				_, _ = io.WriteString(w, body)
			})
			h := NewHandlerWithOptions(cache, upstream, &HandlerOptions{RequestKey: reviewRequestURIKey})
			t.Cleanup(func() { h.writes.Wait() })
			targets := []string{prefix + "/objects/a%2Fb?version=A", prefix + "/summary?"}
			for _, target := range targets {
				assertReviewCachedBody(t, h, target, "before mutation", "MISS")
				assertReviewCachedBody(t, h, target, "before mutation", "HIT")
			}
			mutation := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, prefix+"/object", nil)
			req.Host = "example.org"
			h.ServeHTTP(mutation, req)
			if mutation.Code != http.StatusNoContent {
				t.Fatalf("mutation returned %d", mutation.Code)
			}
			for _, target := range targets {
				assertReviewInvalidatedMethods(t, cache, target)
				assertReviewCachedBody(t, h, target, "after mutation", "MISS")
				assertReviewCachedBody(t, h, target, "after mutation", "HIT")
			}
		})
	}
}

func TestRequestKeyInvalidationPreservesRequestTargetForm(t *testing.T) {
	for _, tc := range []struct {
		name, mutation, targetURL, targetURI, targetHost string
		clientRequest                                    bool
	}{
		{"origin form", "/object", "/objects/a%2Fb?", "/objects/a%2Fb?", "example.org", false},
		{"absolute form", "http://example.org/object", "http://EXAMPLE.org:80/objects/a%2Fb?", "http://EXAMPLE.org:80/objects/a%2Fb?", "EXAMPLE.org:80", false},
		{"client request", "http://example.org/object", "http://EXAMPLE.org:80/objects/a%2Fb?", "", "EXAMPLE.org:80", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.mutation, nil)
			if tc.clientRequest {
				var err error
				req, err = http.NewRequest(http.MethodPost, tc.mutation, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			req.Host = "example.org"
			originalURI := req.RequestURI
			cacheReq, err := newCacheRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			var calls int
			h := &Handler{requestKey: func(target *http.Request) (Key, error) {
				calls++
				if calls == 2 {
					assertReviewRequestTarget(t, target, tc.targetURL, tc.targetURI, tc.targetHost)
				}
				return NewRequestKey(target), nil
			}}
			res := NewResourceBytes(http.StatusOK, nil, http.Header{"Location": {"http://EXAMPLE.org:80/objects/a%2Fb?"}})
			h.invalidationKeys(res, cacheReq)
			if calls != 2 {
				t.Fatalf("policy received %d requests, want original and Location", calls)
			}
			if req.Method != http.MethodPost || req.RequestURI != originalURI || req.URL.String() != tc.mutation || req.Host != "example.org" {
				t.Fatalf("invalidation changed the original request: %+v", req)
			}
		})
	}
}

func assertReviewRequestTarget(t *testing.T, req *http.Request, wantURL, wantURI, wantHost string) {
	t.Helper()
	if req.Method != http.MethodGet || req.URL.String() != wantURL || req.RequestURI != wantURI || req.Host != wantHost {
		t.Errorf("invalidation request: method=%s URL=%q RequestURI=%q Host=%q; want GET %q %q %q", req.Method, req.URL, req.RequestURI, req.Host, wantURL, wantURI, wantHost)
	}
}

func assertReviewCachedBody(t *testing.T, h *Handler, target, body, status string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = "example.org"
	h.ServeHTTP(recorder, req)
	h.writes.Wait()
	if recorder.Code != http.StatusOK || recorder.Body.String() != body || recorder.Header().Get(CacheHeader) != status {
		t.Fatalf("GET %q returned %d %q (%s), want %q (%s)", target, recorder.Code, recorder.Body.String(), recorder.Header().Get(CacheHeader), body, status)
	}
}

func assertReviewInvalidatedMethods(t *testing.T, cache ExtendedCache, target string) {
	t.Helper()
	key, err := reviewRequestURIKey(httptest.NewRequest(http.MethodGet, target, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if _, stale := cache.(staleAtChecker).StaleAt(key.ForMethod(method).String()); !stale {
			t.Errorf("%s %q was not invalidated", method, target)
		}
	}
}

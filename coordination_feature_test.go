package httpcache

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type featureOwnershipKey struct{}
type featureCoordinator struct {
	token   chan struct{}
	entered chan string
	closed  atomic.Int64
}
type featureMissLease struct {
	parent *featureCoordinator
	ctx    context.Context
	once   sync.Once
}

func newFeatureCoordinator() *featureCoordinator {
	c := &featureCoordinator{token: make(chan struct{}, 1), entered: make(chan string, 8)}
	c.token <- struct{}{}
	return c
}
func (c *featureCoordinator) Acquire(ctx context.Context, key string) (MissLease, error) {
	c.entered <- key
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.token:
	}
	return &featureMissLease{parent: c, ctx: context.WithValue(ctx, featureOwnershipKey{}, key)}, nil
}
func (l *featureMissLease) Context() context.Context { return l.ctx }
func (l *featureMissLease) Close() error {
	l.once.Do(func() { l.parent.closed.Add(1); l.parent.token <- struct{}{} })
	return nil
}

type featureContextCache struct {
	Cache
	started chan context.Context
	allow   chan struct{}
}

func (c *featureContextCache) StoreContext(ctx context.Context, res *Resource, keys ...string) error {
	c.started <- ctx
	<-c.allow
	if ctx.Err() != nil || ctx.Value(featureOwnershipKey{}) != keys[0] {
		return errors.New("missing live ownership values")
	}
	return c.Cache.(ContextStore).StoreContext(ctx, res, keys...)
}
func featureAwait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("synchronization timed out")
		var zero T
		return zero
	}
}
func featureLogicalKey(r *http.Request) (Key, error) {
	logical := *r.URL
	logical.Scheme = "https"
	logical.Host = "verified-logical-repository.test"
	return NewKey(r.Method, &logical, r.Header), nil
}

func TestMissCoordinatorRetainsOwnershipThroughBackgroundStore(t *testing.T) {
	underlying := NewMemoryCacheWithConfig(&CacheConfig{Integrity: IntegritySHA256})
	defer func() { _ = underlying.Close() }()
	cache := &featureContextCache{Cache: underlying, started: make(chan context.Context, 1), allow: make(chan struct{})}
	coord := newFeatureCoordinator()
	var calls atomic.Int64
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Host != "mirror-one.test" {
			t.Errorf("logical identity changed routing URL: %s", r.URL)
		}
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Date", Clock().Format(http.TimeFormat))
		_, _ = io.WriteString(w, "shared-package")
	})
	opts := &HandlerOptions{RequestKey: featureLogicalKey, MissCoordinator: coord}
	first, second := NewHandlerWithOptions(cache, upstream, opts), NewHandlerWithOptions(cache, upstream, opts)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstReq := httptest.NewRequest("GET", "https://mirror-one.test/by-hash/SHA256/identity", nil).WithContext(ctx)
	firstRec := httptest.NewRecorder()
	first.ServeHTTP(firstRec, firstReq)
	key := featureAwait(t, coord.entered)
	storeCtx := featureAwait(t, cache.started)
	// The client has finished. Backend publication must still retain the lease
	// and preserve its ownership values without inheriting this cancellation.
	cancel()
	if storeCtx.Err() != nil || storeCtx.Value(featureOwnershipKey{}) != key || coord.closed.Load() != 0 {
		t.Fatalf("ownership lost before Store: err=%v closed=%d", storeCtx.Err(), coord.closed.Load())
	}
	secondReq := httptest.NewRequest("GET", "https://mirror-two.test/by-hash/SHA256/identity", nil)
	secondRec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { second.ServeHTTP(secondRec, secondReq); close(done) }()
	if secondKey := featureAwait(t, coord.entered); secondKey != key {
		t.Fatalf("logical keys differ: %q / %q", key, secondKey)
	}
	if calls.Load() != 1 || coord.closed.Load() != 0 {
		t.Fatalf("duplicate fill while publication pending: upstream=%d released=%d", calls.Load(), coord.closed.Load())
	}
	select {
	case <-done:
		t.Fatal("follower finished before leader publication")
	default:
	}
	close(cache.allow)
	featureAwait(t, done)
	first.writes.Wait()
	second.writes.Wait()
	if calls.Load() != 1 || coord.closed.Load() != 2 || secondRec.Body.String() != "shared-package" || secondRec.Header().Get(CacheHeader) != "HIT" {
		t.Fatalf("coordinated fill: calls=%d closed=%d status=%d headers=%v body=%q", calls.Load(), coord.closed.Load(), secondRec.Code, secondRec.Header(), secondRec.Body.String())
	}
}

type refusedFeatureCoordinator struct{}

func (refusedFeatureCoordinator) Acquire(context.Context, string) (MissLease, error) {
	return nil, errors.New("lock backend unavailable")
}
func TestMissCoordinatorFailsClosedAndReleasesUncacheableFill(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(map[bool]string{false: "uncacheable", true: "unavailable"}[refused], func(t *testing.T) {
			c := NewMemoryCacheWithConfig(&CacheConfig{})
			defer func() { _ = c.Close() }()
			coord := newFeatureCoordinator()
			var coordinator MissCoordinator = coord
			if refused {
				coordinator = refusedFeatureCoordinator{}
			}
			var calls atomic.Int64
			h := NewHandlerWithOptions(c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Cache-Control", "no-store")
				_, _ = io.WriteString(w, "private")
			}), &HandlerOptions{MissCoordinator: coordinator})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "https://origin.test/a", nil))
			h.writes.Wait()
			if refused {
				if rec.Code != 503 || calls.Load() != 0 {
					t.Fatalf("failed-open lock: %d calls=%d", rec.Code, calls.Load())
				}
			} else if rec.Code != 200 || rec.Body.String() != "private" || coord.closed.Load() != 1 {
				t.Fatalf("uncacheable lease retained: %d %q closed=%d", rec.Code, rec.Body.String(), coord.closed.Load())
			}
		})
	}
}

func TestMissCoordinatorWaitingRequestCancellation(t *testing.T) {
	c := NewMemoryCacheWithConfig(&CacheConfig{})
	defer func() { _ = c.Close() }()
	coord := newFeatureCoordinator()
	owner, err := coord.Acquire(context.Background(), "held")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	<-coord.entered
	var calls atomic.Int64
	h := NewHandlerWithOptions(c, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }), &HandlerOptions{MissCoordinator: coord})
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "https://origin.test/wait", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() { h.ServeHTTP(httptest.NewRecorder(), req); close(done) }()
	featureAwait(t, coord.entered)
	cancel()
	featureAwait(t, done)
	if calls.Load() != 0 {
		t.Fatal("canceled lock waiter reached upstream")
	}
	h.flightsMu.Lock()
	remaining := len(h.flights)
	h.flightsMu.Unlock()
	if remaining != 0 {
		t.Fatalf("canceled waiter stranded %d local flights", remaining)
	}
}

func TestRequestKeyPolicyPreservesVaryAndInvalidationOrigin(t *testing.T) {
	c := NewMemoryCacheWithConfig(&CacheConfig{})
	defer func() { _ = c.Close() }()
	var calls atomic.Int64
	h := NewHandlerWithOptions(c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Date", Clock().Format(http.TimeFormat))
		w.Header().Set("Vary", "Accept-Language")
		_, _ = io.WriteString(w, r.Header.Get("Accept-Language"))
	}), &HandlerOptions{RequestKey: featureLogicalKey})
	for _, tc := range []struct{ host, language string }{{"one.test", "en"}, {"two.test", "fr"}, {"two.test", "en"}, {"one.test", "fr"}} {
		req := httptest.NewRequest("GET", "https://"+tc.host+"/object", nil)
		req.Header.Set("Accept-Language", tc.language)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		h.writes.Wait()
		if rec.Body.String() != tc.language {
			t.Fatalf("logical key mixed Vary variants: %q", rec.Body.String())
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("logical Vary identity fetched %d times", calls.Load())
	}
	req, _ := newCacheRequest(httptest.NewRequest("POST", "https://one.test/object", nil))
	req.Key, _ = featureLogicalKey(req.Request)
	res := NewResourceBytes(200, nil, http.Header{"Location": {"/neighbor"}, "Content-Location": {"https://foreign.test/secret"}})
	keys := h.invalidationKeys(res, req)
	if len(keys) != 4 {
		t.Fatalf("same-origin logical invalidation keys: %v", keys)
	}
	for _, key := range keys {
		if !strings.Contains(key, "verified-logical-repository.test") || strings.Contains(key, "foreign.test") {
			t.Fatalf("invalid logical invalidation identity: %q", key)
		}
	}
}

func TestRequestKeyPolicyErrorRejectsBeforeUpstream(t *testing.T) {
	c := NewMemoryCache()
	var called bool
	h := NewHandlerWithOptions(c, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }), &HandlerOptions{RequestKey: func(*http.Request) (Key, error) { return Key{}, errors.New("secret identity error") }})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "https://origin.test/object", nil))
	if rec.Code != 400 || called || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("invalid identity response: %d %q called=%t", rec.Code, rec.Body.String(), called)
	}
	// Method separation remains the policy's responsibility and ordinary Key
	// retains it without requiring an application to invent string encodings.
	u, _ := url.Parse("https://logical.test/object")
	if NewKey("GET", u, nil).String() == NewKey("HEAD", u, nil).String() {
		t.Fatal("method keys collide")
	}
}

package httpcache

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHEADVaryValidationInvalidatesActualGETDiscovery(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, policy := range []string{"no-store", "changed-vary"} {
			t.Run(stringBool(disk)+"/"+policy, func(t *testing.T) {
				cache := generationBudgetCache(t, disk, 1<<20)
				var changed atomic.Bool
				var calls atomic.Int64
				h := NewSharedHandler(cache, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					language, vary := r.Header.Get("Accept-Language"), "Accept-Language"
					w.Header().Set("Cache-Control", "max-age=3600")
					if changed.Load() {
						if policy == "no-store" {
							w.Header().Set("Cache-Control", "no-store")
						} else {
							language, vary = "en", "Accept-Encoding"
						}
					}
					w.Header().Set("Vary", vary)
					etag := `"` + language + `-v1"`
					// A HEAD 304 changes policy while retaining the selected body.
					if changed.Load() && policy == "no-store" && r.Method == http.MethodGet {
						etag = `"` + language + `-v2"`
					}
					w.Header().Set("ETag", etag)
					if r.Header.Get("If-None-Match") == etag {
						w.WriteHeader(http.StatusNotModified)
						return
					}
					w.WriteHeader(http.StatusOK)
					if r.Method != http.MethodHead {
						_, _ = io.WriteString(w, etag)
					}
				}))
				defer h.writes.Wait()
				for _, language := range []string{"en", "fr"} {
					r := httptest.NewRequest(http.MethodGet, "http://cache.example/head-vary", nil)
					r.Header.Set("Accept-Language", language)
					r.Header.Set("Accept-Encoding", "gzip")
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, r)
					h.writes.Wait()
					assertReviewVaryBody(t, rec, `"`+language+`-v1"`)
				}
				changed.Store(true)
				head := httptest.NewRequest(http.MethodHead, "http://cache.example/head-vary", nil)
				head.Header.Set("Accept-Language", "en")
				head.Header.Set("Accept-Encoding", "gzip")
				head.Header.Set("Cache-Control", "min-fresh=7200")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, head)
				h.writes.Wait()
				if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
					t.Fatalf("HEAD response changed: status=%d body=%q", rec.Code, rec.Body.String())
				}
				getKey := NewRequestKey(head).ForMethod(http.MethodGet).String()
				if _, stale := cache.StaleAt(getKey); !stale {
					t.Error("HEAD validation left its GET discovery key fresh")
				}
				before := calls.Load()
				get := httptest.NewRequest(http.MethodGet, head.URL.String(), nil)
				get.Header.Set("Accept-Language", "fr")
				get.Header.Set("Accept-Encoding", "gzip")
				rec = httptest.NewRecorder()
				h.ServeHTTP(rec, get)
				h.writes.Wait()
				want := `"en-v1"`
				if policy == "no-store" {
					want = `"fr-v2"`
				}
				assertReviewVaryBody(t, rec, want)
				if calls.Load() == before || rec.Header().Get(CacheHeader) == "HIT" {
					t.Fatal("another old GET variant bypassed the HEAD policy update")
				}
			})
		}
	}
}

func TestVaryRebuildStorageKeysUseActualUnconditionalRequest(t *testing.T) {
	for _, field := range []string{"If-None-Match", "If-Modified-Since"} {
		t.Run(field, func(t *testing.T) {
			cache := generationBudgetCache(t, false, 1<<20)
			var changed atomic.Bool
			var calls atomic.Int64
			modified := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
			h := NewSharedHandler(cache, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				body, etag, date, vary := "conditional representation", `"conditional"`, modified, "Accept-Language"
				if changed.Load() {
					vary = field
					if r.Header.Get(field) == "" {
						body, etag, date = "unconditional representation", `"unconditional"`, modified.Add(time.Hour)
					}
				}
				w.Header().Set("Cache-Control", "max-age=3600")
				w.Header().Set("Vary", vary)
				w.Header().Set("ETag", etag)
				http.ServeContent(w, r, "", date, strings.NewReader(body))
			}))
			defer h.writes.Wait()
			warm := httptest.NewRequest(http.MethodGet, "http://cache.example/conditional-vary-key", nil)
			warm.Header.Set("Accept-Language", "en")
			h.ServeHTTP(httptest.NewRecorder(), warm)
			h.writes.Wait()
			changed.Store(true)
			client := warm.Clone(warm.Context())
			value := `"conditional"`
			if field == "If-Modified-Since" {
				value = modified.Format(http.TimeFormat)
			}
			client.Header.Set(field, value)
			client.Header.Set("Cache-Control", "min-fresh=7200")
			original := client.Header.Clone()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, client)
			h.writes.Wait()
			assertReviewVaryBody(t, rec, "unconditional representation")
			if !reflect.DeepEqual(original, client.Header) {
				t.Fatal("storage fetch mutated the original client conditions")
			}
			unconditional := client.Clone(client.Context())
			unconditional.Header.Del("If-None-Match")
			unconditional.Header.Del("If-Modified-Since")
			key := NewRequestKey(client)
			stored, err := cache.Retrieve(key.Vary(field, unconditional).String())
			if err != nil {
				t.Fatalf("rebuilt variant was indexed under the client's removed condition: %v", err)
			}
			_ = stored.Close()
			if wrong, err := cache.Retrieve(key.Vary(field, client).String()); err == nil {
				_ = wrong.Close()
				t.Fatal("unconditional body was indexed under a conditional request")
			}
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, warm)
			h.writes.Wait()
			assertReviewVaryBody(t, rec, "unconditional representation")
			if calls.Load() != 3 || rec.Header().Get(CacheHeader) != "HIT" {
				t.Fatalf("unconditional variant was not reusable: calls=%d headers=%v", calls.Load(), rec.Header())
			}
			client.Header.Del("Cache-Control")
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, client)
			h.writes.Wait()
			if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 || calls.Load() != 4 {
				t.Fatalf("conditional request reused the other representation: status=%d body=%q calls=%d", rec.Code, rec.Body.String(), calls.Load())
			}
		})
	}
}

type handlerHerdOwnership struct{}
type handlerHerdMetrics struct {
	NopMetrics
	misses atomic.Int64
	seen   chan struct{}
}

func (m *handlerHerdMetrics) RecordCacheMiss(string) {
	m.misses.Add(1)
	select {
	case m.seen <- struct{}{}:
	default:
	}
}

type handlerHerdCoordinator struct {
	token     chan struct{}
	acquired  atomic.Int64
	released  atomic.Int64
	maxFrames atomic.Int64
}

type handlerHerdLease struct {
	owner *handlerHerdCoordinator
	ctx   context.Context
	once  sync.Once
}

func (c *handlerHerdCoordinator) Acquire(ctx context.Context, key string) (MissLease, error) {
	pcs := make([]uintptr, 256)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	var count int64
	for {
		frame, more := frames.Next()
		if strings.HasSuffix(frame.Function, ".(*Handler).resolveMiss") {
			count++
		}
		if !more {
			break
		}
	}
	for previous := c.maxFrames.Load(); count > previous; previous = c.maxFrames.Load() {
		if c.maxFrames.CompareAndSwap(previous, count) {
			break
		}
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.token:
	}
	c.acquired.Add(1)
	return &handlerHerdLease{owner: c, ctx: context.WithValue(ctx, handlerHerdOwnership{}, key)}, nil
}
func (l *handlerHerdLease) Context() context.Context { return l.ctx }
func (l *handlerHerdLease) Close() error {
	l.once.Do(func() { l.owner.released.Add(1); l.owner.token <- struct{}{} })
	return nil
}

type handlerHerdFailedStore struct {
	Cache
	stores       atomic.Int64
	badOwnership atomic.Bool
}

func (c *handlerHerdFailedStore) StoreContext(ctx context.Context, _ *Resource, keys ...string) error {
	c.stores.Add(1)
	if ctx.Err() != nil || ctx.Value(handlerHerdOwnership{}) != keys[0] {
		c.badOwnership.Store(true)
	}
	return errors.New("intentional handler herd store failure")
}

func newHandlerHerdCoordinator() *handlerHerdCoordinator {
	c := &handlerHerdCoordinator{token: make(chan struct{}, 1)}
	c.token <- struct{}{}
	return c
}

func TestCoordinatedUncacheableHerdRecordsOneMissAndKeepsBoundedStack(t *testing.T) {
	for _, failedStore := range []bool{false, true} {
		t.Run(stringBool(failedStore), func(t *testing.T) {
			const clients = 24
			inner := generationBudgetCache(t, false, 1<<20)
			failing := &handlerHerdFailedStore{Cache: inner}
			var backend Cache = inner
			if failedStore {
				backend = failing
			}
			coord := newHandlerHerdCoordinator()
			metrics := &handlerHerdMetrics{seen: make(chan struct{}, clients)}
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int64
			h := NewHandlerWithOptions(backend, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					close(entered)
					<-release
				}
				control := "no-store"
				if failedStore {
					control = "max-age=3600"
				}
				w.Header().Set("Cache-Control", control)
				_, _ = io.WriteString(w, "uncached representation")
			}), &HandlerOptions{MissCoordinator: coord})
			h.SetMetrics(metrics)
			done := make(chan *httptest.ResponseRecorder, clients)
			for range clients {
				go func() {
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://cache.example/herd", nil))
					done <- rec
				}()
			}
			featureAwait(t, entered)
			for range clients {
				featureAwait(t, metrics.seen)
			}
			close(release)
			for range clients {
				rec := featureAwait(t, done)
				assertReviewVaryBody(t, rec, "uncached representation")
			}
			h.writes.Wait()
			if metrics.misses.Load() != clients || coord.maxFrames.Load() != 1 {
				t.Fatalf("retries multiplied misses or stack frames: misses=%d frames=%d; want %d and 1", metrics.misses.Load(), coord.maxFrames.Load(), clients)
			}
			if calls.Load() != clients || coord.acquired.Load() != clients || coord.released.Load() != clients {
				t.Fatalf("fill ownership was lost: calls=%d acquired=%d released=%d", calls.Load(), coord.acquired.Load(), coord.released.Load())
			}
			if failedStore && (failing.stores.Load() != clients || failing.badOwnership.Load()) {
				t.Fatalf("failed publication lost live lease context: stores=%d badContext=%v", failing.stores.Load(), failing.badOwnership.Load())
			}
			h.flightsMu.Lock()
			remaining := len(h.flights)
			h.flightsMu.Unlock()
			if remaining != 0 {
				t.Fatalf("herd stranded %d miss flights", remaining)
			}
		})
	}
}

func TestCoordinatedHerdCancellationDoesNotAcquireOrReleaseLeaderLease(t *testing.T) {
	cache := generationBudgetCache(t, false, 1<<20)
	coord := newHandlerHerdCoordinator()
	metrics := &handlerHerdMetrics{seen: make(chan struct{}, 2)}
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	h := NewHandlerWithOptions(cache, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		close(entered)
		<-release
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, "leader body")
	}), &HandlerOptions{MissCoordinator: coord})
	h.SetMetrics(metrics)
	leaderDone := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://cache.example/canceled-herd", nil))
		close(leaderDone)
	}()
	featureAwait(t, entered)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	follower := httptest.NewRequest(http.MethodGet, "http://cache.example/canceled-herd", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	followerDone := make(chan struct{})
	go func() { h.ServeHTTP(rec, follower); close(followerDone) }()
	for range 2 {
		featureAwait(t, metrics.seen)
	}
	cancel()
	featureAwait(t, followerDone)
	if rec.Body.Len() != 0 || calls.Load() != 1 || coord.acquired.Load() != 1 || coord.released.Load() != 0 {
		t.Fatalf("canceled follower disturbed leader: body=%q calls=%d acquired=%d released=%d", rec.Body.String(), calls.Load(), coord.acquired.Load(), coord.released.Load())
	}
	close(release)
	featureAwait(t, leaderDone)
	h.writes.Wait()
	if coord.released.Load() != 1 || metrics.misses.Load() != 2 {
		t.Fatalf("cancellation retained lease or repeated miss: released=%d misses=%d", coord.released.Load(), metrics.misses.Load())
	}
}

func TestColdConditional304NeverPublishesAnEmptyRepresentation(t *testing.T) {
	cache := generationBudgetCache(t, false, 1<<20)
	var calls atomic.Int64
	h := NewHandlerWithOptions(cache, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("ETag", `"current"`)
		if r.Header.Get("If-None-Match") == `"current"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = io.WriteString(w, "complete representation")
	}), &HandlerOptions{RequestKey: featureLogicalKey})
	h.Shared = true
	defer h.writes.Wait()
	cold := httptest.NewRequest(http.MethodGet, "https://one.test/by-hash/SHA256/logical", nil)
	cold.Header.Set("If-None-Match", `"current"`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, cold)
	h.writes.Wait()
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 || cache.Stats().ItemCount != 0 {
		t.Fatalf("cold304 became a cached representation: status=%d body=%q entries=%d", rec.Code, rec.Body.String(), cache.Stats().ItemCount)
	}
	for range 2 {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "https://two.test/by-hash/SHA256/logical", nil))
		h.writes.Wait()
		assertReviewVaryBody(t, rec, "complete representation")
		if calls.Load() != 2 {
			t.Fatalf("anonymous request did not fetch/store the complete representation: calls=%d", calls.Load())
		}
	}
}

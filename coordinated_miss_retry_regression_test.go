package httpcache

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

type retryMissMetrics struct {
	NopMetrics
	misses atomic.Int64
}

func (m *retryMissMetrics) RecordCacheMiss(string) { m.misses.Add(1) }

type retryFailedStoreCache struct {
	Cache
	stores atomic.Int64
}

func (c *retryFailedStoreCache) Store(*Resource, ...string) error {
	c.stores.Add(1)
	return errors.New("simulated publication failure")
}

// Observing the first use of Done lets the test release the leader only after
// all followers are waiting on cancellation-aware coordination. It preserves
// the real parent context and does not depend on scheduling delays.
type retryWaitingContext struct {
	context.Context
	waiting chan<- struct{}
	once    sync.Once
}

func (c *retryWaitingContext) Done() <-chan struct{} {
	c.once.Do(func() { c.waiting <- struct{}{} })
	return c.Context.Done()
}

type retryClientResponse struct {
	id       int
	recorder *httptest.ResponseRecorder
}

func retryStartClient(h *Handler, ctx context.Context, id int, responses chan<- retryClientResponse) {
	req := httptest.NewRequest(http.MethodGet, "http://cache.example/coordinated-retry", nil).WithContext(ctx)
	req.Header.Set("X-Client-ID", strconv.Itoa(id))
	rec := httptest.NewRecorder()
	go func() {
		h.ServeHTTP(rec, req)
		responses <- retryClientResponse{id: id, recorder: rec}
	}()
}

func retryBlockedOrigin(failedStore bool) (http.Handler, <-chan struct{}, func(), *atomic.Int64) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var calls atomic.Int64
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		status := http.StatusTooManyRequests
		w.Header().Set("Cache-Control", "no-store")
		if failedStore {
			status = http.StatusOK
			w.Header().Set("Cache-Control", "max-age=3600")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "response for client "+r.Header.Get("X-Client-ID"))
	})
	return upstream, entered, func() { once.Do(func() { close(release) }) }, &calls
}

func TestCoordinatedMissRetriesCountEachClientOnce(t *testing.T) {
	for _, failedStore := range []bool{false, true} {
		t.Run(map[bool]string{false: "uncacheable", true: "failed store"}[failedStore], func(t *testing.T) {
			const clients = 32
			underlying := NewMemoryCacheWithConfig(&CacheConfig{})
			defer func() { _ = underlying.Close() }()
			cache := &retryFailedStoreCache{Cache: underlying}
			coordinator := newFeatureCoordinator()
			coordinator.entered = make(chan string, clients)
			upstream, entered, release, calls := retryBlockedOrigin(failedStore)
			metrics := &retryMissMetrics{}
			h := NewHandlerWithOptions(cache, upstream, &HandlerOptions{MissCoordinator: coordinator})
			h.SetMetrics(metrics)
			defer h.writes.Wait()
			defer release()
			responses := make(chan retryClientResponse, clients)
			retryStartClient(h, context.Background(), 0, responses)
			featureAwait(t, entered)
			waiting := make(chan struct{}, clients-1)
			for id := 1; id < clients; id++ {
				ctx := &retryWaitingContext{Context: context.Background(), waiting: waiting}
				retryStartClient(h, ctx, id, responses)
			}
			for range clients - 1 {
				featureAwait(t, waiting)
			}
			release()
			assertRetryClientResponses(t, responses, clients, failedStore)
			h.writes.Wait()
			assertRetryCoordinationComplete(t, h, coordinator, metrics, calls.Load(), clients)
			if failedStore && cache.stores.Load() != clients {
				t.Fatalf("store attempts=%d; want %d", cache.stores.Load(), clients)
			}
		})
	}
}

func assertRetryClientResponses(t *testing.T, responses <-chan retryClientResponse, clients int, failedStore bool) {
	t.Helper()
	status := http.StatusTooManyRequests
	if failedStore {
		status = http.StatusOK
	}
	seen := make(map[int]bool)
	for range clients {
		response := featureAwait(t, responses)
		if seen[response.id] {
			t.Fatalf("duplicate response for client %d", response.id)
		}
		seen[response.id] = true
		wantBody := "response for client " + strconv.Itoa(response.id)
		if response.recorder.Code != status || response.recorder.Body.String() != wantBody {
			t.Fatalf("client %d received %d %q; want %d %q", response.id, response.recorder.Code, response.recorder.Body.String(), status, wantBody)
		}
	}
}

func assertRetryCoordinationComplete(t *testing.T, h *Handler, coordinator *featureCoordinator, metrics *retryMissMetrics, calls int64, clients int) {
	t.Helper()
	if calls != int64(clients) || coordinator.closed.Load() != int64(clients) {
		t.Fatalf("origin calls=%d closed leases=%d; want %d of each", calls, coordinator.closed.Load(), clients)
	}
	if metrics.misses.Load() != int64(clients) {
		t.Errorf("%d client requests recorded %d cache misses", clients, metrics.misses.Load())
	}
	assertNoRetryFlights(t, h)
}

func assertNoRetryFlights(t *testing.T, h *Handler) {
	t.Helper()
	h.flightsMu.Lock()
	remaining := len(h.flights)
	h.flightsMu.Unlock()
	if remaining != 0 {
		t.Errorf("coordination stranded %d local flights", remaining)
	}
}

func TestCoordinatedRetryCanceledFollowerDoesNotAcquire(t *testing.T) {
	underlying := NewMemoryCacheWithConfig(&CacheConfig{})
	defer func() { _ = underlying.Close() }()
	coordinator := newFeatureCoordinator()
	upstream, entered, release, calls := retryBlockedOrigin(false)
	metrics := &retryMissMetrics{}
	h := NewHandlerWithOptions(underlying, upstream, &HandlerOptions{MissCoordinator: coordinator})
	h.SetMetrics(metrics)
	defer h.writes.Wait()
	defer release()
	responses := make(chan retryClientResponse, 2)
	retryStartClient(h, context.Background(), 0, responses)
	featureAwait(t, entered)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := make(chan struct{}, 1)
	retryStartClient(h, &retryWaitingContext{Context: ctx, waiting: waiting}, 1, responses)
	featureAwait(t, waiting)
	cancel()
	canceled := featureAwait(t, responses)
	if canceled.id != 1 || canceled.recorder.Body.Len() != 0 || calls.Load() != 1 {
		t.Fatalf("canceled follower reached origin or received a body: id=%d body=%q calls=%d", canceled.id, canceled.recorder.Body.String(), calls.Load())
	}
	release()
	assertRetryClientResponses(t, responses, 1, false)
	h.writes.Wait()
	if coordinator.closed.Load() != 1 || metrics.misses.Load() != 2 || len(coordinator.entered) != 1 {
		t.Fatalf("canceled request accounting: leases=%d misses=%d acquisitions=%d", coordinator.closed.Load(), metrics.misses.Load(), len(coordinator.entered))
	}
	assertNoRetryFlights(t, h)
}

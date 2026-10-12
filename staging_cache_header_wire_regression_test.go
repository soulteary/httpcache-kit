package httpcache

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type wireStagingBudget struct {
	*featureBudget
	refuseLease bool
}

func (b *wireStagingBudget) NewLease(ctx context.Context, kind StageKind) (StagingLease, error) {
	if b.refuseLease {
		return nil, ErrStagingBudgetExceeded
	}
	return b.featureBudget.NewLease(ctx, kind)
}

type wireStagingFixture struct {
	h         *Handler
	server    *httptest.Server
	client    *http.Client
	budget    *wireStagingBudget
	calls     atomic.Int64
	prefix    string
	tail      string
	release   chan struct{}
	completed chan struct{}
	once      sync.Once
}

func newWireStagingFixture(t *testing.T, refusal string) *wireStagingFixture {
	t.Helper()
	f := &wireStagingFixture{
		budget:    &wireStagingBudget{featureBudget: &featureBudget{limit: 1 << 20}, refuseLease: refusal == "lease"},
		prefix:    strings.Repeat("first-byte-before-origin-finishes\n", 1024),
		tail:      strings.Repeat("later-origin-bytes\n", 2048),
		release:   make(chan struct{}),
		completed: make(chan struct{}, 2),
	}
	if refusal == "reserve" {
		// Admit the entire first write. Refuse only the subsequent tail, after
		// the client has already received response headers and its first byte.
		f.budget.limit = int64(len(f.prefix))
	}
	c := NewMemoryCacheWithConfig(DefaultCacheConfig().WithCleanupInterval(0))
	f.h = NewHandlerWithOptions(c, http.HandlerFunc(f.origin), &HandlerOptions{StagingBudget: f.budget, TempDir: t.TempDir()})
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.h.ServeHTTP(w, r)
		f.completed <- struct{}{}
	}))
	f.client = f.server.Client()
	f.client.Timeout = 5 * time.Second
	t.Cleanup(func() {
		f.unblock()
		f.server.CloseClientConnections()
		f.server.Close()
		f.h.writes.Wait()
		_ = c.Close()
	})
	return f
}

func (f *wireStagingFixture) origin(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	w.Header().Set("Cache-Control", "max-age=3600")
	w.Header().Set("Date", Clock().Format(http.TimeFormat))
	w.Header().Set("Content-Length", strconv.Itoa(len(f.prefix)+len(f.tail)))
	if _, err := io.WriteString(w, f.prefix); err != nil {
		return
	}
	select {
	case <-f.release:
	case <-r.Context().Done():
		return
	}
	_, _ = io.WriteString(w, f.tail)
}

func (f *wireStagingFixture) unblock() {
	f.once.Do(func() { close(f.release) })
}

// A real net/http client snapshots committed headers. ResponseRecorder.Header
// instead exposes the mutable server map and can hide late SKIP assignments.
func (f *wireStagingFixture) streamingResponse(t *testing.T) *http.Response {
	t.Helper()
	res, err := f.client.Get(f.server.URL + "/wire-staging")
	if err != nil {
		t.Fatalf("response did not begin while origin was still blocked: %v", err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	var first [1]byte
	if _, err := io.ReadFull(res.Body, first[:]); err != nil || first[0] != f.prefix[0] {
		t.Fatalf("first body byte unavailable before origin completion: byte=%q err=%v", first[0], err)
	}
	f.unblock()
	remaining, err := io.ReadAll(res.Body)
	if err != nil || string(first[:])+string(remaining) != f.prefix+f.tail {
		t.Fatalf("staging outcome truncated origin body: length=%d err=%v", 1+len(remaining), err)
	}
	f.awaitRequestCompletion(t)
	f.h.writes.Wait()
	return res
}

func (f *wireStagingFixture) assertNextRequest(t *testing.T, cached bool) {
	t.Helper()
	res, err := f.client.Get(f.server.URL + "/wire-staging")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil || string(body) != f.prefix+f.tail || res.StatusCode != http.StatusOK {
		t.Fatalf("next origin response: status=%d length=%d err=%v", res.StatusCode, len(body), err)
	}
	f.awaitRequestCompletion(t)
	f.h.writes.Wait()
	wantCalls := int64(2)
	if cached {
		wantCalls = 1
		if res.Header.Get(CacheHeader) != "HIT" {
			t.Fatalf("successful admission was not reused: headers=%v", res.Header)
		}
	}
	if f.calls.Load() != wantCalls {
		t.Fatalf("origin calls=%d, want %d", f.calls.Load(), wantCalls)
	}
	f.budget.assertReleased(t)
}

func (f *wireStagingFixture) awaitRequestCompletion(t *testing.T) {
	t.Helper()
	// Content-Length lets a client finish reading before ServeHTTP returns.
	// Wait until Store has been scheduled before joining background writes.
	select {
	case <-f.completed:
	case <-time.After(5 * time.Second):
		t.Fatal("cache handler did not finish after origin response completed")
	}
}

func TestStagingLeaseRefusalReportsSkipOnWire(t *testing.T) {
	f := newWireStagingFixture(t, "lease")
	res := f.streamingResponse(t)
	f.assertNextRequest(t, false)
	if res.Header.Get(CacheHeader) != "SKIP" {
		t.Fatalf("lease refusal committed X-Cache=%q; final trailer=%q, want initial SKIP", res.Header.Get(CacheHeader), res.Trailer.Get(CacheStagingHeader))
	}
}

func TestStagingReserveRefusalReportsFinalSkipOnWire(t *testing.T) {
	f := newWireStagingFixture(t, "reserve")
	res := f.streamingResponse(t)
	f.assertNextRequest(t, false)
	// Later Reserve failure cannot rewrite already transmitted headers. A
	// declared final trailer can report SKIP without buffering the response.
	if res.Header.Get(CacheHeader) != "MISS" || res.Trailer.Get(CacheStagingHeader) != "SKIP" {
		t.Fatalf("late refusal invisible on wire: initial X-Cache=%q final trailer=%q", res.Header.Get(CacheHeader), res.Trailer.Get(CacheStagingHeader))
	}
}

func TestStagingAdmissionPreservesStreamingFirstByteOnWire(t *testing.T) {
	f := newWireStagingFixture(t, "")
	res := f.streamingResponse(t)
	if res.StatusCode != http.StatusOK || res.Header.Get(CacheHeader) != "MISS" {
		t.Fatalf("initial fill: status=%d X-Cache=%q", res.StatusCode, res.Header.Get(CacheHeader))
	}
	if res.Trailer.Get(CacheStagingHeader) != "ADMITTED" {
		t.Fatalf("staging admission missing: trailers=%v", res.Trailer)
	}
	f.assertNextRequest(t, true)
}

package httpcache

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type featureBudget struct {
	mu                sync.Mutex
	limit, used, peak int64
	kinds             []StageKind
	active            int
}
type featureBudgetLease struct {
	budget *featureBudget
	held   int64
	closed bool
}

func (b *featureBudget) NewLease(ctx context.Context, kind StageKind) (StagingLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.kinds = append(b.kinds, kind)
	b.active++
	return &featureBudgetLease{budget: b}, nil
}
func (l *featureBudgetLease) Reserve(n int64) error {
	b := l.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if l.closed || n < 0 || n > b.limit-b.used {
		return ErrStagingBudgetExceeded
	}
	l.held += n
	b.used += n
	if b.used > b.peak {
		b.peak = b.used
	}
	return nil
}
func (l *featureBudgetLease) Release(n int64) {
	b := l.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if n < 0 || n > l.held {
		panic("invalid test budget refund")
	}
	l.held -= n
	b.used -= n
}
func (l *featureBudgetLease) Close() {
	b := l.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if l.closed {
		return
	}
	l.closed = true
	b.used -= l.held
	l.held = 0
	b.active--
}
func (b *featureBudget) assertReleased(t *testing.T) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used != 0 || b.active != 0 || b.peak > b.limit {
		t.Fatalf("budget lifecycle: used=%d active=%d peak=%d limit=%d", b.used, b.active, b.peak, b.limit)
	}
}

func TestStagingBudgetSharedOverlappingCopies(t *testing.T) {
	const body = "bounded-package-payload"
	for _, tc := range []struct {
		name   string
		limit  int64
		cached bool
	}{
		{"handler-refused", int64(len(body) - 1), false},
		{"overlap-refused", int64(len(body)), false},
		{"all-copies-admitted", 8192, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			budget := &featureBudget{limit: tc.limit}
			dir, tmp := t.TempDir(), t.TempDir()
			c, err := NewDiskCacheWithConfig(dir, &CacheConfig{Integrity: IntegritySHA256, StagingBudget: budget})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			h := NewHandlerWithOptions(c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", "max-age=3600")
				w.Header().Set("Date", Clock().Format(http.TimeFormat))
				_, _ = io.WriteString(w, body)
			}), &HandlerOptions{StagingBudget: budget, TempDir: tmp})
			req := httptest.NewRequest("GET", "http://origin.test/package.deb", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			h.writes.Wait()
			if rec.Code != 200 || rec.Body.String() != body {
				t.Fatalf("budget rejection truncated origin response: %d %q", rec.Code, rec.Body.String())
			}
			res, err := c.Retrieve(NewRequestKey(req).String())
			if tc.cached {
				if err != nil {
					t.Fatal(err)
				}
				_ = res.Close()
			} else if !errors.Is(err, ErrNotFoundInCache) {
				if res != nil {
					_ = res.Close()
				}
				t.Fatalf("failed admission published an entry: %v", err)
			}
			budget.assertReleased(t)
			entries, err := os.ReadDir(tmp)
			if err != nil || len(entries) != 0 {
				t.Fatalf("handler orphan files: %v %v", entries, err)
			}
			entries, err = os.ReadDir(filepath.Join(dir, stagingPrefix, formatPrefix))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("native orphan stages: %v", entries)
			}
			if tc.cached {
				budget.mu.Lock()
				got := append([]StageKind(nil), budget.kinds...)
				budget.mu.Unlock()
				want := []StageKind{StageHandlerBody, StageCacheBody, StageCacheHeader}
				if len(got) != len(want) {
					t.Fatalf("stage kinds %v", got)
				}
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("stage kinds %v", got)
					}
				}
			}
		})
	}
}

func TestStagingBudgetNativeFailurePreservesCommittedEntry(t *testing.T) {
	dir := t.TempDir()
	budget := &featureBudget{limit: 8192}
	c, err := NewDiskCacheWithConfig(dir, &CacheConfig{Integrity: IntegritySHA256, StagingBudget: budget})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	const key = "bounded-replacement"
	if err := c.Store(integrityFeatureResource("original"), key); err != nil {
		t.Fatal(err)
	}
	budget.assertReleased(t)
	// Admit the body, refuse the header while that first temporary copy is held.
	budget.mu.Lock()
	budget.limit = int64(len("replaced"))
	budget.peak = 0
	budget.mu.Unlock()
	err = c.Store(integrityFeatureResource("replaced"), key)
	if !errors.Is(err, ErrStagingBudgetExceeded) {
		t.Fatalf("Store error=%v", err)
	}
	integrityFeatureBody(t, c, key, "original")
	budget.assertReleased(t)
}

type cancelStageReader struct {
	data   *strings.Reader
	cancel context.CancelFunc
	read   bool
}

func (r *cancelStageReader) Read(p []byte) (int, error) {
	if r.read {
		r.cancel()
		return 0, context.Canceled
	}
	r.read = true
	return r.data.Read(p)
}
func (r *cancelStageReader) Seek(off int64, whence int) (int64, error) {
	r.read = false
	return r.data.Seek(off, whence)
}
func (r *cancelStageReader) Close() error { return nil }
func TestStoreContextCancellationReleasesStaging(t *testing.T) {
	budget := &featureBudget{limit: 8192}
	dir := t.TempDir()
	c, err := NewDiskCacheWithConfig(dir, &CacheConfig{StagingBudget: budget})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := NewResource(200, &cancelStageReader{data: strings.NewReader("partial-body"), cancel: cancel}, http.Header{})
	if err := c.(ContextStore).StoreContext(ctx, res, "canceled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("StoreContext error=%v", err)
	}
	budget.assertReleased(t)
	if got, err := c.Retrieve("canceled"); !errors.Is(err, ErrNotFoundInCache) {
		if got != nil {
			_ = got.Close()
		}
		t.Fatalf("canceled store exposed entry: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, stagingPrefix, formatPrefix))
	if err != nil || len(entries) != 0 {
		t.Fatalf("canceled stages: %v %v", entries, err)
	}
}

func TestDisableStagingSweepPreservesLiveInstance(t *testing.T) {
	dir := t.TempDir()
	c, err := NewDiskCacheWithConfig(dir, &CacheConfig{DisableStagingSweep: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	stage, _, err := c.(*cache).stageBody(strings.NewReader("live-not-yet-published"), "live")
	if err != nil {
		t.Fatal(err)
	}
	defer c.(*cache).abandonStaged(stage)
	second, err := NewDiskCacheWithConfig(dir, &CacheConfig{DisableStagingSweep: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if _, err := os.Stat(stage.tmpPath); err != nil {
		t.Fatalf("second instance removed live stage: %v", err)
	}
	if err := c.(*cache).commitStaged(stage); err != nil {
		t.Fatalf("first instance could not publish its body: %v", err)
	}
}

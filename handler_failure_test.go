package httpcache

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestPassUpstreamInterruptedBodyIsNotStored(t *testing.T) {
	for _, tempUnavailable := range []bool{false, true} {
		t.Run(stringBool(tempUnavailable), func(t *testing.T) {
			dir := t.TempDir()
			if tempUnavailable {
				t.Setenv("TMPDIR", filepath.Join(dir, "missing"))
			} else {
				t.Setenv("TMPDIR", dir)
			}
			upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Cache-Control", "max-age=3600")
				_, _ = io.WriteString(w, "partial")
				_ = w.(*responseStreamer).pipeWriter.CloseWithError(io.ErrUnexpectedEOF)
			})
			assertFailedPassUpstream(t, dir, upstream, "partial")
		})
	}
}

func TestPassUpstreamAnonymousStagingDoesNotRequireReopen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("open anonymous files require POSIX unlink")
	}
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		_, _ = io.WriteString(w, "complete")
		// The first pipe write has completed, so staging exists. Its name must
		// already have been unlinked while the live descriptor remains readable.
		files, err := filepath.Glob(filepath.Join(dir, "httpcache-*.tmp"))
		if err != nil || len(files) != 0 {
			t.Errorf("named handler staging remains: %v %v", files, err)
		}
	})
	c := NewMemoryCacheWithConfig(&CacheConfig{})
	t.Cleanup(func() { _ = c.Close() })
	h := NewHandler(c, upstream)
	request := httptest.NewRequest(http.MethodGet, "http://example.org/anonymous", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request)
	h.writes.Wait()
	if rec.Body.String() != "complete" || rec.Header().Get(CacheHeader) != "MISS" {
		t.Fatalf("body=%q headers=%v", rec.Body.String(), rec.Header())
	}
	res, err := c.Retrieve(NewRequestKey(request).String())
	if err != nil {
		t.Fatalf("anonymous descriptor could not be cached: %v", err)
	}
	defer func() { _ = res.Close() }()
	got, err := io.ReadAll(res)
	if err != nil || string(got) != "complete" {
		t.Fatalf("stored body=%q error=%v", got, err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("handler staging leftovers: %v %v", files, err)
	}
}

func assertFailedPassUpstream(t *testing.T, dir string, upstream http.Handler, wantBody string) {
	t.Helper()
	c := NewMemoryCacheWithConfig(DefaultCacheConfig().WithCleanupInterval(0))
	t.Cleanup(func() { _ = c.Close() })
	h := NewHandler(c, upstream)
	request, err := newCacheRequest(httptest.NewRequest(http.MethodGet, "http://example.org/failure", nil))
	if err != nil {
		t.Fatal(err)
	}
	completed := 0
	rec := httptest.NewRecorder()
	h.passUpstream(rec, request, func() { completed++ })
	h.writes.Wait()
	if completed != 1 {
		t.Fatalf("completion called %d times, want 1", completed)
	}
	if rec.Body.String() != wantBody || rec.Header().Get(CacheHeader) != "SKIP" {
		t.Fatalf("body=%q headers=%v", rec.Body.String(), rec.Header())
	}
	res, err := c.Retrieve(request.Key.String())
	if res != nil {
		_ = res.Close()
	}
	if !errors.Is(err, ErrNotFoundInCache) {
		t.Fatalf("failed response was cached: %v", err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary files remain: %v, error=%v", files, err)
	}
}

func TestStoreResourceAfterShutdownClosesBodyAndCompletes(t *testing.T) {
	dir := t.TempDir()
	file, err := os.CreateTemp(dir, "body-*")
	if err != nil {
		t.Fatal(err)
	}
	body := &tempFileReadSeekCloser{file: file, path: file.Name()}
	c := NewMemoryCacheWithConfig(DefaultCacheConfig().WithCleanupInterval(0))
	t.Cleanup(func() { _ = c.Close() })
	h := NewHandler(c, http.NotFoundHandler())
	if err := h.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	request, err := newCacheRequest(httptest.NewRequest(http.MethodGet, "http://example.org/closed", nil))
	if err != nil {
		t.Fatal(err)
	}
	completed := 0
	h.storeResource(NewResource(http.StatusOK, body, http.Header{}), request, func() { completed++ })
	if completed != 1 || c.Stats().ItemCount != 0 {
		t.Fatalf("completion=%d stats=%+v", completed, c.Stats())
	}
	if _, err := os.Stat(body.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging file remains after shutdown: %v", err)
	}
	if _, err := file.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("resource body was not closed: %v", err)
	}
}

type legacyStaleCache struct {
	Cache
	markers map[string]time.Time
	queried []string
}

func (c *legacyStaleCache) StaleAt(key string) (time.Time, bool) {
	c.queried = append(c.queried, key)
	at, marked := c.markers[key]
	return at, marked
}

func TestLegacyStaleSnapshotReadsBaseLast(t *testing.T) {
	at := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	c := &legacyStaleCache{markers: map[string]time.Time{"base": at, "variant": at.Add(time.Second)}}
	h := NewHandler(c, nil)
	got := h.staleSnapshot([]string{"base", "missing", "variant"})
	if !reflect.DeepEqual(got, c.markers) {
		t.Fatalf("snapshot=%v, want=%v", got, c.markers)
	}
	if !reflect.DeepEqual(c.queried, []string{"variant", "missing", "base"}) {
		t.Fatalf("marker read order=%v", c.queried)
	}
}

func TestHandlerFreshGenerationPreservesNewerRecordAndPrunesExpired(t *testing.T) {
	c := NewMemoryCacheWithConfig(DefaultCacheConfig().WithCleanupInterval(0))
	t.Cleanup(func() { _ = c.Close() })
	h := NewHandler(plainCache{inner: c}, nil)
	now := Clock()
	h.localFresh = map[string]time.Time{
		"expired": now.Add(-2 * defaultStaleMarkerTTL),
		"newer":   now.Add(time.Hour),
	}
	h.recordFresh(time.Time{}, "new", "newer")
	if _, present := h.localFresh["expired"]; present {
		t.Fatal("expired freshness record retained")
	}
	if !h.localFresh["newer"].Equal(now.Add(time.Hour)) || h.localFresh["new"].Before(now) {
		t.Fatalf("freshness records=%v", h.localFresh)
	}
	h.localFresh["expired"] = now.Add(-2 * defaultStaleMarkerTTL)
	h.recordStale("new")
	if _, present := h.localFresh["expired"]; present {
		t.Fatal("invalidation retained an expired freshness record")
	}
	if h.locallyFreshAfter("new", h.localStale["new"]) {
		t.Fatal("fetch preceding invalidation considered fresh")
	}
}

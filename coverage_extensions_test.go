package httpcache

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/soulteary/vfs-kit"
)

func TestValidationCancellationUnblocksBeforeAndAfterHeaders(t *testing.T) {
	for _, sendHeaders := range []bool{false, true} {
		t.Run(stringBool(sendHeaders), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, finished := make(chan struct{}), make(chan struct{})
			allowReturn := make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(allowReturn) }) }
			defer release()
			upstream := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				defer close(finished)
				if sendHeaders {
					w.WriteHeader(http.StatusNotModified)
				}
				close(entered)
				<-req.Context().Done()
				<-allowReturn
			})
			h := NewHandler(nil, upstream)
			request, err := newCacheRequest(httptest.NewRequest(http.MethodGet, "http://example.org/cancel", nil).WithContext(ctx))
			if err != nil {
				t.Fatal(err)
			}
			res := NewResourceBytes(http.StatusOK, []byte("old body"), http.Header{"Etag": {`"old"`}})
			result := make(chan [2]bool, 1)
			go func() {
				validated, served := h.validateUpstreamResponse(httptest.NewRecorder(), request, res)
				result <- [2]bool{validated, served}
			}()
			awaitCoverageSignal(t, entered)
			cancel()
			select {
			case got := <-result:
				if got != [2]bool{} {
					t.Fatalf("canceled validation result=%v, want neither validated nor served", got)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("validation did not stop after cancellation")
			}
			release()
			awaitCoverageSignal(t, finished)
			if !res.RequestTime.IsZero() || res.Header().Get(ProxyDateHeader) != "" {
				t.Fatal("canceled validation refreshed the cached generation")
			}
		})
	}
}

func awaitCoverageSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not complete the expected transition")
	}
}

func TestValidationInterruptedNotModifiedDoesNotRefresh(t *testing.T) {
	h := NewHandler(nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusEarlyHints)
		w.WriteHeader(http.StatusNotModified)
		panic("upstream interrupted after 304 headers")
	}))
	request, err := newCacheRequest(httptest.NewRequest(http.MethodGet, "http://example.org/interrupted", nil))
	if err != nil {
		t.Fatal(err)
	}
	res := NewResourceBytes(http.StatusOK, []byte("old body"), http.Header{"Etag": {`"old"`}})
	validated, served := h.validateUpstreamResponse(httptest.NewRecorder(), request, res)
	if validated || served || !res.RequestTime.IsZero() {
		t.Fatal("interrupted 304 refreshed the cached generation")
	}
}

func TestValidationFlushCommitsHeadersAndPreservesBody(t *testing.T) {
	exchange := newValidationExchange()
	defer func() { _ = exchange.reader.Close() }()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		exchange.serve(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Result", "first")
			w.(http.Flusher).Flush()
			w.Header().Set("X-Result", "late")
			_, _ = io.WriteString(w, "streamed body")
		}), httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	awaitCoverageSignal(t, exchange.ready)
	if exchange.status != http.StatusOK || exchange.snapshot.Get("X-Result") != "first" {
		t.Fatalf("flushed headers=%v, status=%d", exchange.snapshot, exchange.status)
	}
	body, err := io.ReadAll(exchange.reader)
	if err != nil || string(body) != "streamed body" {
		t.Fatalf("flushed body=%q, error=%v", body, err)
	}
	awaitCoverageSignal(t, finished)
}

func TestFreshenMissingBackingFilesReleasesReservations(t *testing.T) {
	for _, prefix := range []string{bodyPrefix, headerPrefix} {
		t.Run(prefix, func(t *testing.T) {
			c := generationBudgetCache(t, false, 1<<20)
			const key = "missing-freshening-target"
			if err := c.Store(generationBudgetResource("original", `"old"`), key); err != nil {
				t.Fatal(err)
			}
			res, err := c.Retrieve(key)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = res.Close() }()
			before := c.Stats()
			if err := c.fs.Remove(prefix + formatPrefix + hashKey(key)); err != nil {
				t.Fatal(err)
			}
			if err := c.Freshen(res, key); err == nil {
				t.Fatal("freshening succeeded after a backing file disappeared")
			}
			if stats := c.Stats(); stats.ItemCount != before.ItemCount || stats.TotalSize != before.TotalSize {
				t.Fatalf("failed freshening changed capacity: before=%+v after=%+v", before, stats)
			}
			if len(c.reservations) != 0 || c.reservedSize != 0 || len(c.writers) != 0 {
				t.Fatal("failed freshening retained a reservation or writer lock")
			}
			if err := c.Store(generationBudgetResource("replacement", `"new"`), key); err != nil {
				t.Fatalf("repair after missing file: %v", err)
			}
			assertStoreBody(t, c, key, "replacement")
		})
	}
}

func TestFreshenCannotClearNewerInvalidation(t *testing.T) {
	c := generationBudgetCache(t, false, 1<<20)
	const key = "invalidated-freshening-target"
	if err := c.Store(generationBudgetResource("original", `"old"`), key); err != nil {
		t.Fatal(err)
	}
	res, err := c.Retrieve(key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Close() }()
	res.RequestTime = time.Now().Add(-time.Hour)
	c.Invalidate(key)
	if err := c.Freshen(res, key); err == nil {
		t.Fatal("old validation cleared a newer invalidation")
	}
	current, err := c.Retrieve(key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = current.Close() }()
	if !current.IsStale() {
		t.Fatal("superseded validation made the cached body fresh")
	}
}

func TestFreshenLegacyHeadersPersistsGeneration(t *testing.T) {
	c := generationBudgetCache(t, false, 1<<20)
	const key = "legacy-freshening-target"
	if err := c.Store(generationBudgetResource("original", `"old"`), key); err != nil {
		t.Fatal(err)
	}
	legacyHeader := "HTTP/1.1 200 OK\r\nEtag: \"old\"\r\nContent-Length: 8\r\n\r\n"
	if _, _, err := c.vfsWrite(headerPrefix+formatPrefix+hashKey(key), strings.NewReader(legacyHeader)); err != nil {
		t.Fatal(err)
	}
	res, err := c.Retrieve(key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Close() }()
	previous := res.storedAt
	res.Header().Set("X-Validated", "yes")
	if err := c.Freshen(res, key); err != nil {
		t.Fatalf("freshen old header format: %v", err)
	}
	h, persisted, _, err := c.readHeaderRecord(headerPrefix+formatPrefix+hashKey(key), key)
	if err != nil || persisted.IsZero() || persisted.Before(previous) || h.Get("X-Validated") != "yes" {
		t.Fatalf("legacy generation was not upgraded: header=%v storedAt=%v error=%v", h, persisted, err)
	}
	assertStoreBody(t, c, key, "original")
}

func TestFailedLargerReplacementRemainsChargedUntilCleanup(t *testing.T) {
	fs := &selectiveRemoveFailVFS{VFS: vfs.Memory()}
	c := NewVFSCacheWithConfig(fs, DefaultCacheConfig().WithMaxSize(1024).WithCleanupInterval(0)).(*cache)
	t.Cleanup(func() { _ = c.Close() })
	const key = "stuck-replacement"
	if err := c.Store(storedResource("old"), key); err != nil {
		t.Fatal(err)
	}
	before := c.Stats().TotalSize
	fs.failHash = hashKey(key)
	err := c.Store(storedResource(strings.Repeat("x", 2048)), key)
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("failed replacement error=%v, want cleanup failure", err)
	}
	if stats := c.Stats(); stats.ItemCount != 1 || stats.TotalSize <= before || stats.TotalSize < 2048 {
		t.Fatalf("residual replacement bytes vanished from accounting: %+v", stats)
	}
	fs.failHash = ""
	result := c.Cleanup()
	if result.RemovedItems != 1 || result.RemovedBytes < 2048 || c.Stats().TotalSize != 0 {
		t.Fatalf("cleanup did not reclaim charged residual: result=%+v stats=%+v", result, c.Stats())
	}
}

func TestSizeCleanupSkipsReservedEntry(t *testing.T) {
	c := generationBudgetCache(t, false, 1<<20)
	if err := c.Store(storedResource("protected"), "active"); err != nil {
		t.Fatal(err)
	}
	if err := c.Store(storedResource("evictable"), "idle"); err != nil {
		t.Fatal(err)
	}
	c.beginReservation(hashKey("active"))
	defer c.releaseReservation(hashKey("active"))
	c.config.MaxSize = 1
	removed, _ := c.enforceMaxSize()
	if removed != 1 || c.Stats().ItemCount != 1 {
		t.Fatalf("cleanup did not preserve just the active entry: removed=%d stats=%+v", removed, c.Stats())
	}
	assertStoreBody(t, c, "active", "protected")
	if _, err := c.Retrieve("idle"); !errors.Is(err, ErrNotFoundInCache) {
		t.Fatalf("idle entry survived size cleanup: %v", err)
	}
}

func TestTemporaryCacheWritesRejectMissingParent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	if stage, err := newHandlerStage(context.Background(), dir, nil); stage != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing staging parent: stage=%v error=%v", stage, err)
	}
	if n, err := atomicWriteFile(filepath.Join(dir, "markers.json"), strings.NewReader("{}")); n != 0 || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing snapshot parent: bytes=%d error=%v", n, err)
	}
}

func TestLegacyBarrierRecoveryPersistsRestartTime(t *testing.T) {
	fs := vfs.Memory()
	config := DefaultCacheConfig().WithCleanupInterval(0)
	first := NewVFSCacheWithConfig(fs, config).(*cache)
	if err := first.Store(storedResource("before interruption"), "legacy-barrier"); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]time.Time{"legacy-barrier": invalidationBarrierTime})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.vfsWrite(staleMapPath, strings.NewReader(string(data))); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	restartedAt := Clock()
	reopened := NewVFSCacheWithConfig(fs, config).(*cache)
	t.Cleanup(func() { _ = reopened.Close() })
	marker, marked := reopened.StaleAt("legacy-barrier")
	if !marked || marker.Equal(invalidationBarrierTime) || marker.Before(restartedAt) {
		t.Fatalf("interrupted legacy marker was not recovered: %v, present=%t", marker, marked)
	}
	var persisted staleSnapshot
	if err := reopened.readStaleJSON(staleMapSlotPath(1), &persisted); err != nil {
		t.Fatal(err)
	}
	if !persisted.Markers["legacy-barrier"].Equal(marker) {
		t.Fatal("recovered legacy marker was not persisted for the next restart")
	}
	res, err := reopened.Retrieve("legacy-barrier")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Close() }()
	if !res.IsStale() {
		t.Fatal("legacy barrier recovery republished a pre-mutation body as fresh")
	}
}

func TestResourceExpiresParsesMissingValidAndMalformedDates(t *testing.T) {
	want := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, value string
		want        time.Time
		wantErr     bool
	}{
		{name: "missing"},
		{name: "valid", value: want.Format(http.TimeFormat), want: want},
		{name: "malformed", value: "not an HTTP date", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := NewResourceBytes(http.StatusOK, nil, http.Header{"Expires": {tc.value}})
			got, err := res.Expires()
			if !got.Equal(tc.want) || (err != nil) != tc.wantErr {
				t.Fatalf("Expires=%v, error=%v; want %v, error=%t", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

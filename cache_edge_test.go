package httpcache

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/soulteary/vfs-kit"
)

func TestCacheScanIgnoresSubdirectories(t *testing.T) {
	fs := vfs.Memory()
	config := DefaultCacheConfig().WithCleanupInterval(0)
	first := NewVFSCacheWithConfig(fs, config)
	if err := first.Store(storedResource("payload"), "stored-key"); err != nil {
		t.Fatalf("seed entry: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close original cache: %v", err)
	}
	if err := fs.Mkdir(bodyPrefix+formatPrefix+"subdirectory", 0o700); err != nil {
		t.Fatalf("create nested directory: %v", err)
	}
	reopened := NewVFSCacheWithConfig(fs, config)
	t.Cleanup(func() { _ = reopened.Close() })
	if stats := reopened.Stats(); stats.ItemCount != 1 {
		t.Fatalf("scan indexed a directory as an entry: %+v", stats)
	}
	assertStoreBody(t, reopened, "stored-key", "payload")
}

type cacheEdgeReadFaultFile struct {
	vfs.RFile
	err    error
	closes *int
}

func (f *cacheEdgeReadFaultFile) Read(p []byte) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.RFile.Read(p)
}

func (f *cacheEdgeReadFaultFile) Close() error {
	(*f.closes)++
	return f.RFile.Close()
}

type cacheEdgeReadFaultVFS struct {
	vfs.VFS
	err          error
	bodyCloses   int
	headerCloses int
}

func (fs *cacheEdgeReadFaultVFS) Open(path string) (vfs.RFile, error) {
	f, err := fs.VFS.Open(path)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(path, headerPrefix) {
		return &cacheEdgeReadFaultFile{RFile: f, err: fs.err, closes: &fs.headerCloses}, nil
	}
	return &cacheEdgeReadFaultFile{RFile: f, closes: &fs.bodyCloses}, nil
}

func TestCacheHeaderReadFailureClosesBothFiles(t *testing.T) {
	fs := &cacheEdgeReadFaultVFS{VFS: vfs.Memory()}
	c := NewVFSCacheWithConfig(fs, DefaultCacheConfig().WithCleanupInterval(0))
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Store(storedResource("payload"), "read-fault"); err != nil {
		t.Fatalf("seed entry: %v", err)
	}
	readErr := errors.New("injected header read error")
	fs.err = readErr
	if _, err := c.Retrieve("read-fault"); !errors.Is(err, readErr) {
		t.Fatalf("Retrieve error = %v, want header read failure", err)
	}
	if fs.bodyCloses != 1 || fs.headerCloses != 1 {
		t.Fatalf("open files were not closed after header read failure: body=%d header=%d", fs.bodyCloses, fs.headerCloses)
	}
	if stats := c.Stats(); stats.HitCount != 0 || stats.MissCount != 0 {
		t.Fatalf("operational read error counted as a lookup result: %+v", stats)
	}
	fs.err = nil
	assertStoreBody(t, c, "read-fault", "payload")
}

func TestCachePurgeFailureKeepsMarkersUntilRetry(t *testing.T) {
	fs := &selectiveRemoveFailVFS{VFS: vfs.Memory()}
	config := DefaultCacheConfig().WithCleanupInterval(0)
	c := NewVFSCacheWithConfig(fs, config)
	t.Cleanup(func() { _ = c.Close() })
	for _, key := range []string{"stuck", "removable"} {
		if err := c.Store(storedResource("old"), key); err != nil {
			t.Fatalf("seed %q: %v", key, err)
		}
	}
	c.Invalidate("stuck", "removable")
	fs.failHash = hashKey("stuck")
	if err := c.Purge(); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("Purge error = %v, want backing-file removal failure", err)
	}
	assertCacheCounts(t, c, 1, 2)
	assertCacheReopensStale(t, fs, config, "stuck")
	if _, err := c.Retrieve("removable"); err != ErrNotFoundInCache {
		t.Fatalf("removable entry survived purge: %v", err)
	}
	fs.failHash = ""
	if err := c.Purge(); err != nil {
		t.Fatalf("retry purge: %v", err)
	}
	assertCacheCounts(t, c, 0, 0)
	reopened := NewVFSCacheWithConfig(fs, config)
	t.Cleanup(func() { _ = reopened.Close() })
	assertCacheCounts(t, reopened, 0, 0)
}

func TestCacheCleanupWithTTLDisabledRetainsOldMarker(t *testing.T) {
	c := NewMemoryCacheWithConfig(DefaultCacheConfig().WithTTL(0).WithStaleMapTTL(time.Hour).WithCleanupInterval(0)).(*cache)
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Store(storedResource("old"), "retained"); err != nil {
		t.Fatalf("seed entry: %v", err)
	}
	// Without entry expiry, even a marker much older than StaleMapTTL must
	// continue governing the cached body.
	c.staleMutex.Lock()
	c.stale["retained"] = time.Now().Add(-48 * time.Hour)
	c.staleMutex.Unlock()
	result := c.Cleanup()
	if result.RemovedItems != 0 || result.RemovedStaleEntries != 0 {
		t.Fatalf("TTL-disabled cleanup removed a body or its marker: %+v", result)
	}
	assertCacheCounts(t, c, 1, 1)
}

func TestCacheCleanupSizeLimitSkipsFailedVictim(t *testing.T) {
	fs := &selectiveRemoveFailVFS{VFS: vfs.Memory()}
	c := NewVFSCacheWithConfig(fs, DefaultCacheConfig().WithTTL(0).WithMaxSize(1<<20).WithCleanupInterval(0)).(*cache)
	t.Cleanup(func() { _ = c.Close() })
	for _, key := range []string{"stuck", "removable"} {
		if err := c.Store(storedResource("payload"), key); err != nil {
			t.Fatalf("seed %q: %v", key, err)
		}
	}
	c.lruMutex.RLock()
	stuckSize := c.lruIndex[hashKey("stuck")].size
	removableSize := c.lruIndex[hashKey("removable")].size
	c.lruMutex.RUnlock()
	c.config.MaxSize = stuckSize
	fs.failHash = hashKey("stuck")
	result := c.Cleanup()
	if result.RemovedItems != 1 || result.RemovedBytes != removableSize {
		t.Fatalf("cleanup did not skip the stuck victim: %+v", result)
	}
	if stats := c.Stats(); stats.ItemCount != 1 || stats.TotalSize != stuckSize {
		t.Fatalf("cleanup accounting = %+v, want the retained victim only", stats)
	}
	assertStoreBody(t, c, "stuck", "payload")
	if _, err := c.Retrieve("removable"); err != ErrNotFoundInCache {
		t.Fatalf("removable victim survived cleanup: %v", err)
	}
}

func TestCacheInvalidJournalSlotFallsBackToCompleteSnapshot(t *testing.T) {
	for _, invalid := range []string{
		`{"generation":0,"markers":{}}`,
		`{"generation":100,"markers":null}`,
	} {
		t.Run(invalid, func(t *testing.T) {
			fs := vfs.Memory()
			config := DefaultCacheConfig().WithCleanupInterval(0)
			first := NewVFSCacheWithConfig(fs, config).(*cache)
			if err := first.Store(storedResource("old"), "marked"); err != nil {
				t.Fatalf("seed entry: %v", err)
			}
			first.Invalidate("marked")
			// Invalidate writes generation 1's barrier and generation 2's
			// final marker; keep the complete final snapshot in slot 0.
			if _, _, err := first.vfsWrite(staleMapSlotPath(1), strings.NewReader(invalid)); err != nil {
				t.Fatalf("replace invalid slot: %v", err)
			}
			_ = first.Close()
			assertCacheReopensStale(t, fs, config, "marked")
		})
	}
}

func TestReadStoredHeadersRejectsIncompleteMetadata(t *testing.T) {
	for _, record := range []string{
		"",
		storedAtPreamble + "invalid\r\nHTTP/1.1 200 OK\r\n\r\n",
		storedAtPreamble + "2026-10-12T00:00:00Z\r\n",
	} {
		t.Run(record, func(t *testing.T) {
			if _, _, err := readStoredHeaders(bufio.NewReader(strings.NewReader(record))); err == nil {
				t.Fatal("incomplete or malformed metadata was accepted")
			}
		})
	}
}

func TestDiskInvalidationPersistenceFailureRetainsMarker(t *testing.T) {
	c, dir := diskCache(t)
	const key = "marker-persistence-failure"
	if err := c.Store(storedResource("old"), key); err != nil {
		t.Fatalf("seed entry: %v", err)
	}
	// A directory at the snapshot target rejects atomic replacement. The
	// invalidation must still take effect for the running cache.
	if err := os.Mkdir(filepath.Join(dir, staleMapPath), 0o700); err != nil {
		t.Fatalf("obstruct snapshot target: %v", err)
	}
	c.Invalidate(key)
	res, err := c.Retrieve(key)
	if err != nil {
		t.Fatalf("retrieve invalidated entry: %v", err)
	}
	defer func() { _ = res.Close() }()
	if !res.IsStale() {
		t.Fatal("persistence failure erased the in-memory invalidation")
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read cache directory: %v", err)
	}
	for _, file := range files {
		if strings.HasPrefix(file.Name(), "."+staleMapPath+".tmp-") {
			t.Fatalf("failed snapshot left a temporary file: %s", file.Name())
		}
	}
}

func assertCacheCounts(t *testing.T, c ExtendedCache, items, stale int) {
	t.Helper()
	if stats := c.Stats(); stats.ItemCount != items || stats.StaleCount != stale {
		t.Fatalf("cache counts = %+v, want %d entries and %d markers", stats, items, stale)
	}
}

func assertCacheReopensStale(t *testing.T, fs vfs.VFS, config *CacheConfig, key string) {
	t.Helper()
	reopened := NewVFSCacheWithConfig(fs, config)
	defer func() { _ = reopened.Close() }()
	res, err := reopened.Retrieve(key)
	if err != nil {
		t.Fatalf("retrieve %q after restart: %v", key, err)
	}
	defer func() { _ = res.Close() }()
	if !res.IsStale() {
		t.Fatalf("%q became fresh after restart", key)
	}
}

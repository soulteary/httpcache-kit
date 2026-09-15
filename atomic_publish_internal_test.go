package httpcache

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func diskCache(t *testing.T) (*cache, string) {
	t.Helper()
	dir := t.TempDir()
	c, err := NewDiskCacheWithConfig(dir, DefaultCacheConfig())
	if err != nil {
		t.Fatalf("NewDiskCacheWithConfig: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*cache), dir
}

// Re-storing an entry must not take the old one away while the new one is
// being written. The in-place write opened the target with O_CREATE|O_TRUNC,
// which emptied a perfectly good entry for the duration of the copy, so every
// concurrent reader missed and went to the origin. With atomic publication a
// reader sees the old contents or the new ones, always complete.
func TestRestoreKeepsPreviousEntryReadable(t *testing.T) {
	c, _ := diskCache(t)

	const key = "GET:http://example.com/pool/main/pkg.deb"
	oldBody := strings.Repeat("v1", 64<<10)
	newBody := strings.Repeat("v2", 64<<10)

	if err := c.Store(storedResource(oldBody), key); err != nil {
		t.Fatalf("initial Store: %v", err)
	}

	var misses, partial atomic.Int32
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if err := c.Store(storedResource(newBody), key); err != nil {
				t.Errorf("re-Store: %v", err)
				return
			}
		}
	}()

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				res, err := c.Retrieve(key)
				if err == ErrNotFoundInCache {
					misses.Add(1)
					continue
				}
				if err != nil {
					t.Errorf("Retrieve: %v", err)
					return
				}
				got, readErr := readAllResource(res)
				_ = res.Close()
				if readErr != nil {
					t.Errorf("read body: %v", readErr)
					return
				}
				if got != oldBody && got != newBody {
					partial.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if n := misses.Load(); n > 0 {
		t.Errorf("%d retrievals missed while the entry was being replaced; the previous entry must stay readable", n)
	}
	if n := partial.Load(); n > 0 {
		t.Errorf("%d retrievals saw neither the old nor the new body in full", n)
	}
}

// The header file is never observable in the empty state the in-place write
// published before copying into it.
func TestHeaderFileIsNeverObservedEmpty(t *testing.T) {
	c, dir := diskCache(t)

	const key = "GET:http://example.com/dists/stable/InRelease"
	headerPath := filepath.Join(dir, filepath.FromSlash(headerPrefix+formatPrefix+hashKey(key)))

	if err := c.Store(storedResource("seed"), key); err != nil {
		t.Fatalf("initial Store: %v", err)
	}

	var empty atomic.Int32
	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if fi, err := os.Stat(headerPath); err == nil && fi.Size() == 0 {
				empty.Add(1)
			}
		}
	}()

	for i := 0; i < 200; i++ {
		if err := c.Store(storedResource(strings.Repeat("x", 4096)), key); err != nil {
			t.Fatalf("Store: %v", err)
		}
	}
	close(stop)
	wg.Wait()

	if n := empty.Load(); n > 0 {
		t.Errorf("observed the header file empty %d times; publication should be atomic", n)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, os.ErrInvalid }

// A failed write must leave the entry that is already there untouched, and
// must not report the target as modified -- callers discard on that.
func TestFailedAtomicWriteLeavesPreviousEntry(t *testing.T) {
	c, _ := diskCache(t)

	const key = "GET:http://example.com/x"
	if err := c.Store(storedResource("original"), key); err != nil {
		t.Fatalf("Store: %v", err)
	}

	path := headerPrefix + formatPrefix + hashKey(key)
	staged, n, err := c.stageWrite(path, failingReader{})
	if err == nil {
		t.Fatal("expected the write to fail")
	}
	if anyPublished(staged) {
		t.Error("a failed staged write must not report the target as published")
	}
	c.abandonStaged(staged)
	_ = n

	res, err := c.Retrieve(key)
	if err != nil {
		t.Fatalf("previous entry should still be readable, got %v", err)
	}
	got, err := readAllResource(res)
	_ = res.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got != "original" {
		t.Errorf("body = %q, want %q", got, "original")
	}
}

// No temporary files are left behind, successful or not.
func TestAtomicWriteLeavesNoTempFiles(t *testing.T) {
	c, dir := diskCache(t)

	const key = "GET:http://example.com/y"
	for i := 0; i < 10; i++ {
		if err := c.Store(storedResource("payload"), key); err != nil {
			t.Fatalf("Store: %v", err)
		}
	}
	staged, _, _ := c.stageWrite(headerPrefix+formatPrefix+hashKey(key), failingReader{})
	c.abandonStaged(staged)

	var leftovers []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.Contains(info.Name(), ".tmp-") {
			leftovers = append(leftovers, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(leftovers) > 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

func readAllResource(res *Resource) (string, error) {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(res); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func versionedResource(version, body string) *Resource {
	h := http.Header{}
	h.Set("Cache-Control", "max-age=3600")
	h.Set("X-Version", version)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	return NewResource(http.StatusOK, nopSeekCloser{strings.NewReader(body)}, h)
}

// Codex review on #10: Retrieve opens the body first and reads the header
// second, so a publish landing between those two steps handed back one
// version's payload with the other's status and headers. Rename made that
// worse than the in-place write it replaced: the open descriptor keeps the
// whole old inode alive, so the mismatch is a complete body of the wrong
// version rather than a short read.
//
// Retrieve now holds publishMu across both lookups and Store commits both
// files under its write side, so the commit cannot land in between.
func TestRetrievePairsHeaderWithItsOwnBody(t *testing.T) {
	c, _ := diskCache(t)

	const key = "GET:http://example.com/p"
	v1 := strings.Repeat("A", 4096)
	v2 := strings.Repeat("B", 2048)

	if err := c.Store(versionedResource("1", v1), key); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var mismatched, checked atomic.Int32
	stop := make(chan struct{})
	var writer, readers sync.WaitGroup

	writer.Add(1)
	go func() {
		defer writer.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			v, body := "1", v1
			if i%2 == 0 {
				v, body = "2", v2
			}
			if err := c.Store(versionedResource(v, body), key); err != nil {
				return
			}
		}
	}()

	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 150; j++ {
				res, err := c.Retrieve(key)
				if err != nil {
					continue
				}
				got, readErr := readAllResource(res)
				ver := res.Header().Get("X-Version")
				_ = res.Close()
				if readErr != nil {
					continue
				}
				checked.Add(1)
				want := v1
				if ver == "2" {
					want = v2
				}
				if got != want {
					mismatched.Add(1)
				}
			}
		}()
	}

	// Readers finish first; only then is the writer told to stop, so waiting
	// on it cannot deadlock.
	readers.Wait()
	close(stop)
	writer.Wait()

	if checked.Load() == 0 {
		t.Fatal("no retrievals completed")
	}
	if n := mismatched.Load(); n > 0 {
		t.Errorf("%d of %d retrievals paired a header with another version's body", n, checked.Load())
	}
}

// Codex review on #10: staging files must not land where scanExistingCache
// walks. A body temporary there would be indexed under its own filename as if
// it were a cache key, inflating the LRU; a header temporary would never be
// scanned and so never reclaimed.
func TestStagingFilesLiveOutsideScannedDirectories(t *testing.T) {
	c, dir := diskCache(t)

	for _, path := range []string{
		bodyPrefix + formatPrefix + hashKey("GET:http://example.com/a"),
		headerPrefix + formatPrefix + hashKey("GET:http://example.com/a"),
	} {
		staged, _, err := c.stageWrite(path, strings.NewReader("payload"))
		if err != nil {
			t.Fatalf("stageWrite %s: %v", path, err)
		}
		if staged.tmpPath == "" {
			t.Fatalf("%s was not staged on a disk-backed cache", path)
		}
		wantDir := filepath.Join(dir, filepath.FromSlash(stagingPrefix+formatPrefix))
		if got := filepath.Dir(staged.tmpPath); got != wantDir {
			t.Errorf("staged %s in %s, want %s", path, got, wantDir)
		}
		// Nothing is visible in the scanned directory until the commit.
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(path))); !os.IsNotExist(err) {
			t.Errorf("%s exists before the commit (stat err = %v)", path, err)
		}
		c.abandonStaged(staged)
	}
}

// Staging files an interrupted process left behind are reclaimed on startup,
// and never counted as cache entries.
func TestStartupSweepsAbandonedStagingFiles(t *testing.T) {
	dir := t.TempDir()

	c, err := NewDiskCacheWithConfig(dir, DefaultCacheConfig())
	if err != nil {
		t.Fatalf("NewDiskCacheWithConfig: %v", err)
	}
	if err := c.Store(storedResource("payload"), "GET:http://example.com/keep"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	_ = c.Close()

	// Simulate a crash between staging and commit.
	stagingDir := filepath.Join(dir, filepath.FromSlash(stagingPrefix+formatPrefix))
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	orphan := filepath.Join(stagingDir, "deadbeef.tmp-123456")
	if err := os.WriteFile(orphan, []byte(strings.Repeat("x", 8192)), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	reopened, err := NewDiskCacheWithConfig(dir, DefaultCacheConfig())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("abandoned staging file survived startup (stat err = %v)", err)
	}

	rc := reopened.(*cache)
	rc.lruMutex.RLock()
	indexed := len(rc.lruIndex)
	rc.lruMutex.RUnlock()
	if indexed != 1 {
		t.Errorf("lruIndex has %d entries, want 1 (the orphan must not be indexed)", indexed)
	}
}

package httpcache

import (
	"bytes"
	"os"
	"path/filepath"
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
	n, modified, err := c.publishWrite(path, failingReader{})
	if err == nil {
		t.Fatal("expected the write to fail")
	}
	if modified {
		t.Error("a failed atomic write must not report the target as modified")
	}
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
	_, _, _ = c.publishWrite(headerPrefix+formatPrefix+hashKey(key), failingReader{})

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

package httpcache

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/soulteary/vfs-kit"
)

func storedResource(body string) *Resource {
	h := http.Header{}
	h.Set("Cache-Control", "max-age=3600")
	return NewResource(http.StatusOK, nopSeekCloser{strings.NewReader(body)}, h)
}

// Store publishes the body first and the header last, so the header file is
// the entry's commit marker. vfsWrite opens with O_CREATE|O_TRUNC, which makes
// an empty header file visible before any bytes reach it. A Retrieve landing
// in that window must report a miss, not an error: the entry simply is not
// committed yet, which a reader cannot tell apart from one never stored.
func TestRetrieveTreatsUncommittedHeaderAsMiss(t *testing.T) {
	fs := vfs.Memory()
	c := NewVFSCacheWithConfig(fs, DefaultCacheConfig())

	const key = "GET:http://example.com/pool/main/pkg.deb"
	if err := c.Store(storedResource("package-bytes"), key); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Reproduce the window: body committed, header truncated to zero bytes.
	headerPath := headerPrefix + formatPrefix + hashKey(key)
	f, err := fs.OpenFile(headerPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatalf("truncate header: %v", err)
	}
	_ = f.Close()

	res, err := c.Retrieve(key)
	if res != nil {
		_ = res.Close()
	}
	if err != ErrNotFoundInCache {
		t.Fatalf("Retrieve returned %v, want ErrNotFoundInCache (an uncommitted entry is a miss)", err)
	}
}

// A header whose record never terminates is the same situation as an empty
// one: the copy stopped before the entry was committed.
func TestRetrieveTreatsUnterminatedHeaderAsMiss(t *testing.T) {
	fs := vfs.Memory()
	c := NewVFSCacheWithConfig(fs, DefaultCacheConfig())

	const key = "GET:http://example.com/dists/stable/InRelease"
	if err := c.Store(storedResource("index-bytes"), key); err != nil {
		t.Fatalf("Store: %v", err)
	}

	headerPath := headerPrefix + formatPrefix + hashKey(key)
	full, err := vfs.ReadFile(fs, headerPath)
	if err != nil {
		t.Fatalf("read header: %v", err)
	}
	if len(full) < 8 {
		t.Fatalf("header unexpectedly short: %d bytes", len(full))
	}
	f, err := fs.OpenFile(headerPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatalf("rewrite header: %v", err)
	}
	// Drop the terminating blank line, as an interrupted copy would leave:
	// timestamp and status line are readable, the header block never ends.
	if _, err := io.Copy(f, bytes.NewReader(full[:len(full)-2])); err != nil {
		t.Fatalf("write partial header: %v", err)
	}
	_ = f.Close()

	res, err := c.Retrieve(key)
	if res != nil {
		_ = res.Close()
	}
	if err != ErrNotFoundInCache {
		t.Fatalf("Retrieve returned %v, want ErrNotFoundInCache", err)
	}
}

// A genuinely malformed header (complete, but not parseable) is still an
// error: that one is corruption, not an in-flight write.
func TestRetrieveStillErrorsOnMalformedHeader(t *testing.T) {
	fs := vfs.Memory()
	c := NewVFSCacheWithConfig(fs, DefaultCacheConfig())

	const key = "GET:http://example.com/x"
	if err := c.Store(storedResource("body"), key); err != nil {
		t.Fatalf("Store: %v", err)
	}

	headerPath := headerPrefix + formatPrefix + hashKey(key)
	f, err := fs.OpenFile(headerPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatalf("rewrite header: %v", err)
	}
	// A complete line that is not a status line, followed by a header block.
	if _, err := io.WriteString(f, "NOT-A-STATUS-LINE\r\n\r\n"); err != nil {
		t.Fatalf("write malformed header: %v", err)
	}
	_ = f.Close()

	res, err := c.Retrieve(key)
	if res != nil {
		_ = res.Close()
	}
	if err == nil {
		t.Fatal("expected an error for a malformed header")
	}
	if err == ErrNotFoundInCache {
		t.Fatal("a malformed header is corruption, not a miss")
	}
}

// End to end through the Handler: concurrent requests for the same uncached
// URL must never surface the in-flight store as a failed response. Before the
// fix this produced a 502 carrying
// "failed to read headers from ... : EOF".
func TestConcurrentFirstFetchNeverFails(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Cache-Control", "max-age=3600")
		_, _ = io.WriteString(w, "package-bytes")
	})

	var bad atomic.Int32
	for round := 0; round < 30; round++ {
		// Disk-backed, like production: vfs.Memory() is not safe for the
		// concurrent Open/Close this test deliberately generates.
		c, err := NewDiskCacheWithConfig(t.TempDir(), DefaultCacheConfig())
		if err != nil {
			t.Fatalf("NewDiskCacheWithConfig: %v", err)
		}
		h := NewHandler(c, upstream)

		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rec := httptest.NewRecorder()
				req, _ := http.NewRequest(http.MethodGet, "http://example.com/pool/main/pkg.deb", nil)
				h.ServeHTTP(rec, req)
				if rec.Code >= 400 {
					bad.Add(1)
					t.Errorf("status %d: %s", rec.Code, rec.Body.String())
				}
			}()
		}
		wg.Wait()
		_ = c.Close()
	}
	if n := bad.Load(); n > 0 {
		t.Errorf("%d concurrent first-fetch responses failed", n)
	}
}

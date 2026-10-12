package httpcache

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreWithoutKeysDoesNotAccessResource(t *testing.T) {
	c := NewMemoryCacheWithConfig(DefaultCacheConfig().WithCleanupInterval(0))
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Store(nil); err != nil {
		t.Fatalf("Store without keys: %v", err)
	}
	if stats := c.Stats(); stats.ItemCount != 0 || stats.TotalSize != 0 {
		t.Fatalf("Store without keys changed stats: %+v", stats)
	}
}

type storeSeekFailureBody struct {
	ReadSeekCloser
	err error
}

func (b *storeSeekFailureBody) Seek(int64, int) (int64, error) { return 0, b.err }

func TestStoreRewindFailurePreservesPreviousEntry(t *testing.T) {
	c := NewMemoryCacheWithConfig(DefaultCacheConfig().WithCleanupInterval(0)).(*cache)
	t.Cleanup(func() { _ = c.Close() })
	const key = "rewind-failure"
	if err := c.Store(storedResource("original"), key); err != nil {
		t.Fatalf("seed entry: %v", err)
	}
	before := c.Stats()
	seekErr := errors.New("injected rewind failure")
	body := &storeSeekFailureBody{ReadSeekCloser: nopSeekCloser{strings.NewReader("replacement")}, err: seekErr}
	err := c.Store(NewResource(http.StatusOK, body, http.Header{}), key, "later-key")
	if !errors.Is(err, seekErr) || !strings.Contains(err.Error(), key) {
		t.Fatalf("Store error = %v, want rewind failure naming %q", err, key)
	}
	if after := c.Stats(); after.ItemCount != before.ItemCount || after.TotalSize != before.TotalSize {
		t.Fatalf("failed rewind changed entry accounting: before=%+v after=%+v", before, after)
	}
	assertStoreBody(t, c, key, "original")
	if _, err := c.Retrieve("later-key"); err != ErrNotFoundInCache {
		t.Fatalf("later key error = %v, want ErrNotFoundInCache", err)
	}
}

func TestStoreContentLengthBoundaries(t *testing.T) {
	tests := []struct {
		name   string
		length string
		want   string
	}{
		{name: "missing", want: "payload"},
		{name: "malformed", length: "invalid", want: "payload"},
		{name: "negative", length: "-1", want: "payload"},
		{name: "overflow", length: "9223372036854775808", want: "payload"},
		{name: "zero", length: "0", want: ""},
		{name: "bounded", length: "2", want: "pa"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewMemoryCacheWithConfig(DefaultCacheConfig().WithCleanupInterval(0)).(*cache)
			t.Cleanup(func() { _ = c.Close() })
			res := NewResourceBytes(http.StatusOK, []byte("payload"), http.Header{"Content-Length": {tt.length}})
			if err := c.Store(res, "base", "variant"); err != nil {
				t.Fatalf("Store: %v", err)
			}
			assertStoreBody(t, c, "base", tt.want)
			assertStoreBody(t, c, "variant", tt.want)
			if stats := c.Stats(); stats.ItemCount != 2 {
				t.Fatalf("stored item count = %d, want 2", stats.ItemCount)
			}
		})
	}
}

type storeReadFailureBody struct {
	ReadSeekCloser
	err error
}

func (b *storeReadFailureBody) Read(p []byte) (int, error) {
	n, _ := b.ReadSeekCloser.Read(p)
	return n, b.err
}

func TestStoreStagedFailurePreservesPreviousEntry(t *testing.T) {
	readErr := errors.New("injected staged body read failure")
	tests := []struct {
		name string
		body ReadSeekCloser
		hdr  http.Header
	}{
		{
			name: "read failure",
			body: &storeReadFailureBody{ReadSeekCloser: nopSeekCloser{strings.NewReader("partial")}, err: readErr},
			hdr:  http.Header{},
		},
		{
			name: "short declared body",
			body: nopSeekCloser{strings.NewReader("short")},
			hdr:  http.Header{"Content-Length": {"50"}},
		},
		{
			name: "unknown length admission failure",
			body: nopSeekCloser{strings.NewReader(strings.Repeat("x", 4096))},
			hdr:  http.Header{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, dir := diskCache(t)
			const key = "staged-store-failure"
			if err := c.Store(storedResource("original"), key); err != nil {
				t.Fatalf("seed entry: %v", err)
			}
			before := c.Stats()
			c.config.MaxSize = before.TotalSize + 256
			if err := c.Store(NewResource(http.StatusOK, tt.body, tt.hdr), key); err == nil {
				t.Fatal("Store succeeded despite the failed staged replacement")
			}
			if after := c.Stats(); after.ItemCount != before.ItemCount || after.TotalSize != before.TotalSize {
				t.Fatalf("staged failure changed accounting: before=%+v after=%+v", before, after)
			}
			assertStoreBody(t, c, key, "original")
			assertStoreStagingEmpty(t, dir)
		})
	}
}

func TestStoreStagingDirectoryFailurePreservesPreviousEntry(t *testing.T) {
	c, dir := diskCache(t)
	const key = "blocked-staging-directory"
	if err := c.Store(storedResource("original"), key); err != nil {
		t.Fatalf("seed entry: %v", err)
	}
	before := c.Stats()
	stagingDir := filepath.Join(dir, filepath.FromSlash(stagingPrefix+formatPrefix))
	if err := os.Remove(stagingDir); err != nil {
		t.Fatalf("remove empty staging directory: %v", err)
	}
	if err := os.WriteFile(stagingDir, []byte("directory obstruction"), 0o600); err != nil {
		t.Fatalf("obstruct staging directory: %v", err)
	}
	err := c.Store(storedResource("replacement"), key)
	if err == nil || !strings.Contains(err.Error(), "failed to create staging directory") {
		t.Fatalf("Store error = %v, want staging directory failure", err)
	}
	if after := c.Stats(); after.ItemCount != before.ItemCount || after.TotalSize != before.TotalSize {
		t.Fatalf("staging directory failure changed accounting: before=%+v after=%+v", before, after)
	}
	assertStoreBody(t, c, key, "original")
}

// Empty target directories make rename fail deterministically without relying
// on filesystem permissions, which privileged test runners can bypass.
func TestStoreCommitFailureRemovesPartialPublication(t *testing.T) {
	for _, prefix := range []string{bodyPrefix, headerPrefix} {
		t.Run(prefix, func(t *testing.T) {
			c, dir := diskCache(t)
			const key = "failed-commit"
			target := filepath.Join(dir, filepath.FromSlash(prefix+formatPrefix+hashKey(key)))
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Fatalf("create conflicting target: %v", err)
			}
			err := c.Store(storedResource("payload"), key)
			if err == nil || !strings.Contains(err.Error(), "failed to publish cache file") {
				t.Fatalf("Store error = %v, want publication failure", err)
			}
			if stats := c.Stats(); stats.ItemCount != 0 || stats.TotalSize != 0 {
				t.Fatalf("failed publication was admitted: %+v", stats)
			}
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				t.Fatalf("remove conflicting target: %v", err)
			}
			if _, err := c.Retrieve(key); err != ErrNotFoundInCache {
				t.Fatalf("failed publication lookup = %v, want ErrNotFoundInCache", err)
			}
			assertStoreStagingEmpty(t, dir)
		})
	}
}

func TestStorePartialCommitTracksUnremovableResidual(t *testing.T) {
	c, dir := diskCache(t)
	const key = "partial-commit-residual"
	target := filepath.Join(dir, filepath.FromSlash(headerPrefix+formatPrefix+hashKey(key)))
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("create conflicting header target: %v", err)
	}
	blocker := filepath.Join(target, "blocker")
	if err := os.WriteFile(blocker, []byte("cannot remove directory yet"), 0o600); err != nil {
		t.Fatalf("populate conflicting target: %v", err)
	}
	err := c.Store(storedResource("payload"), key)
	if err == nil || !strings.Contains(err.Error(), "failed to publish cache file") || !strings.Contains(err.Error(), "discard entry after failed store") {
		t.Fatalf("Store error = %v, want publication and cleanup errors", err)
	}
	if stats := c.Stats(); stats.ItemCount != 1 || stats.TotalSize <= 0 {
		t.Fatalf("unremovable residual was not tracked: %+v", stats)
	}
	body := filepath.Join(dir, filepath.FromSlash(bodyPrefix+formatPrefix+hashKey(key)))
	if _, err := os.Stat(body); !os.IsNotExist(err) {
		t.Fatalf("partially published body survived discard: %v", err)
	}
	assertStoreStagingEmpty(t, dir)
	if err := os.Remove(blocker); err != nil {
		t.Fatalf("unblock residual cleanup: %v", err)
	}
	if err := c.Purge(); err != nil {
		t.Fatalf("retry residual removal: %v", err)
	}
	if stats := c.Stats(); stats.ItemCount != 0 || stats.TotalSize != 0 {
		t.Fatalf("residual accounting after removal: %+v", stats)
	}
}

func assertStoreBody(t *testing.T, c Cache, key, want string) {
	t.Helper()
	res, err := c.Retrieve(key)
	if err != nil {
		t.Fatalf("Retrieve(%q): %v", key, err)
	}
	defer func() { _ = res.Close() }()
	body, err := io.ReadAll(res)
	if err != nil {
		t.Fatalf("read %q: %v", key, err)
	}
	if string(body) != want {
		t.Fatalf("body for %q = %q, want %q", key, body, want)
	}
}

func assertStoreStagingEmpty(t *testing.T, dir string) {
	t.Helper()
	files, err := os.ReadDir(filepath.Join(dir, filepath.FromSlash(stagingPrefix+formatPrefix)))
	if err != nil {
		t.Fatalf("read staging directory: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("failed Store left staging files: %v", files)
	}
}

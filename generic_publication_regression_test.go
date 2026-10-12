package httpcache

import (
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	vfs "github.com/soulteary/vfs-kit"
)

// Hold the next header replacement after the new body has been closed, but
// before the old metadata has been replaced. This is a valid generic VFS.
type genericPublicationVFS struct {
	vfs.VFS
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (v *genericPublicationVFS) OpenFile(path string, flag int, perm os.FileMode) (vfs.WFile, error) {
	if strings.HasPrefix(path, headerPrefix) && v.armed.CompareAndSwap(true, false) {
		close(v.entered)
		<-v.release
	}
	return v.VFS.OpenFile(path, flag, perm)
}

func TestCacheGenericSingleKeyStorePublishesOneGeneration(t *testing.T) {
	fs := &genericPublicationVFS{VFS: vfs.Memory(), entered: make(chan struct{}), release: make(chan struct{})}
	c := NewVFSCacheWithConfig(fs, DefaultCacheConfig().WithCleanupInterval(0)).(*cache)
	t.Cleanup(func() { _ = c.Close() })
	const key = "GET:http://origin.test/package"
	if err := c.Store(generationBudgetResource("old-body", `"old"`), key); err != nil {
		t.Fatal(err)
	}
	fs.armed.Store(true)
	stored := make(chan error, 1)
	go func() { stored <- c.Store(generationBudgetResource("new-body", `"new"`), key) }()
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(fs.release) }) })
	select {
	case <-fs.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("store did not reach header replacement")
	}
	type observed struct {
		body, etag string
		err        error
	}
	read := make(chan observed, 1)
	go func() {
		res, err := c.Retrieve(key)
		if err != nil {
			read <- observed{err: err}
			return
		}
		defer func() { _ = res.Close() }()
		body, err := readAllResource(res)
		read <- observed{body: body, etag: res.Header().Get("Etag"), err: err}
	}()
	var got observed
	var early bool
	select {
	case got = <-read:
		early = true
	case <-time.After(25 * time.Millisecond):
	}
	release.Do(func() { close(fs.release) })
	if err := featureAwait(t, stored); err != nil {
		t.Fatal(err)
	}
	if !early {
		got = featureAwait(t, read)
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.body == "old-body" && got.etag == `"old"` || got.body == "new-body" && got.etag == `"new"` {
		return
	}
	t.Fatalf("mixed generation returned: body=%q etag=%q", got.body, got.etag)
}

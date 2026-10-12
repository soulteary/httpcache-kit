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

// Pause a generic VFS immediately after an in-place header truncation.
type freshenPublicationVFS struct {
	vfs.VFS
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (v *freshenPublicationVFS) OpenFile(path string, flag int, perm os.FileMode) (vfs.WFile, error) {
	f, err := v.VFS.OpenFile(path, flag, perm)
	if err == nil && strings.HasPrefix(path, headerPrefix) && v.armed.CompareAndSwap(true, false) {
		close(v.entered)
		<-v.release
	}
	return f, err
}

func TestCacheGenericFreshenHidesHeaderReplacement(t *testing.T) {
	fs := &freshenPublicationVFS{VFS: vfs.Memory(), entered: make(chan struct{}), release: make(chan struct{})}
	c := NewVFSCacheWithConfig(fs, DefaultCacheConfig().WithCleanupInterval(0)).(*cache)
	t.Cleanup(func() { _ = c.Close() })
	const key = "GET:http://origin.test/package"
	if err := c.Store(generationBudgetResource("body", `"same"`), key); err != nil {
		t.Fatal(err)
	}
	old, err := c.Retrieve(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = old.Close() })
	old.Header().Set("X-Validated", "yes")
	fs.armed.Store(true)
	freshened := make(chan error, 1)
	go func() { freshened <- c.Freshen(old, key) }()
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(fs.release) }) })
	select {
	case <-fs.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("freshening did not reach header truncation")
	}
	read := make(chan error, 1)
	go func() {
		res, err := c.Retrieve(key)
		if err == nil {
			_ = res.Close()
		}
		read <- err
	}()
	var readErr error
	var completedEarly bool
	select {
	case readErr = <-read:
		completedEarly = true
	case <-time.After(25 * time.Millisecond):
	}
	release.Do(func() { close(fs.release) })
	select {
	case err := <-freshened:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("freshening did not complete")
	}
	if !completedEarly {
		select {
		case readErr = <-read:
		case <-time.After(3 * time.Second):
			t.Fatal("cache reader did not complete")
		}
	}
	if readErr != nil {
		t.Fatalf("reader observed transient incomplete freshening header: %v", readErr)
	}
}

func TestCacheFreshenRejectsReplacementWithEqualClockTimestamp(t *testing.T) {
	previousClock := Clock
	instant := previousClock()
	Clock = func() time.Time { return instant }
	t.Cleanup(func() { Clock = previousClock })
	c := generationBudgetCache(t, true, 1<<20)
	const key = "GET:http://origin.test/package"
	if err := c.Store(generationBudgetResource("oldA", `W/"same"`), key); err != nil {
		t.Fatal(err)
	}
	old, err := c.Retrieve(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = old.Close() })
	old.Header().Set("X-Generation", "old")
	newer := generationBudgetResource("newB", `W/"same"`)
	newer.Header().Set("X-Generation", "new")
	if err := c.Store(newer, key); err != nil {
		t.Fatal(err)
	}
	if err := c.Freshen(old, key); err == nil {
		current, retrieveErr := c.Retrieve(key)
		if retrieveErr != nil {
			t.Fatal(retrieveErr)
		}
		defer func() { _ = current.Close() }()
		t.Fatalf("superseded resource accepted with equal timestamps; replacement metadata X-Generation=%q", current.Header().Get("X-Generation"))
	}
}

func TestCacheMemoryRetrievedResourceClosePreservesReplacement(t *testing.T) {
	c := generationBudgetCache(t, false, 1<<20)
	const key = "GET:http://origin.test/package"
	if err := c.Store(generationBudgetResource("old-body", `"old"`), key); err != nil {
		t.Fatal(err)
	}
	old, err := c.Retrieve(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = old.Close() })
	if err := c.Store(generationBudgetResource("new-body", `"new"`), key); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	assertGenerationBudgetEntry(t, c, key, "new-body", `"new"`)
}

func TestCacheFreshenVariantIgnoresReplacedDiscoveryBase(t *testing.T) {
	previousClock := Clock
	instant := previousClock()
	Clock = func() time.Time { return instant }
	t.Cleanup(func() { Clock = previousClock })
	c := generationBudgetCache(t, true, 1<<20)
	const base = "GET:http://origin.test/package"
	const variant = base + ":Accept=variantA"
	if err := c.Store(generationBudgetResource("oldA", `W/"a"`), base, variant); err != nil {
		t.Fatal(err)
	}
	old, err := c.Retrieve(variant)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = old.Close() })
	if err := c.Store(generationBudgetResource("newB", `W/"b"`), base, base+":Accept=variantB"); err != nil {
		t.Fatal(err)
	}
	old.Header().Set("X-Validated", "yes")
	if err := c.Freshen(old, base, variant); err != nil {
		t.Fatalf("unchanged retrieved variant rejected after only discovery base changed: %v", err)
	}
	assertGenerationBudgetEntry(t, c, base, "newB", `W/"b"`)
	current, err := c.Retrieve(variant)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = current.Close() }()
	if current.Header().Get("X-Validated") != "yes" {
		t.Fatal("actual retrieved variant was not freshened")
	}
}

func TestCacheReopenedDiskFreshenRejectsReplacedGeneration(t *testing.T) {
	for _, clockChange := range []time.Duration{0, -time.Minute} {
		t.Run(clockChange.String(), func(t *testing.T) {
			previousClock := Clock
			now := time.Date(2026, 10, 13, 12, 0, 0, 123, time.UTC)
			Clock = func() time.Time { return now }
			t.Cleanup(func() { Clock = previousClock })
			dir := t.TempDir()
			config := DefaultCacheConfig().WithCleanupInterval(0)
			initial, err := NewDiskCacheWithConfig(dir, config)
			if err != nil {
				t.Fatal(err)
			}
			const key = "GET:http://origin.test/package"
			if err := initial.Store(generationBudgetResource("oldA", `W/"same"`), key); err != nil {
				t.Fatal(err)
			}
			if err := initial.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := NewDiskCacheWithConfig(dir, config)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			c := reopened.(*cache)
			old, err := c.Retrieve(key)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = old.Close() })
			old.Header().Set("X-Generation", "old")
			now = now.Add(clockChange)
			newer := generationBudgetResource("newB", `W/"same"`)
			newer.Header().Set("X-Generation", "new")
			if err := c.Store(newer, key); err != nil {
				t.Fatal(err)
			}
			if err := c.Freshen(old, key); err == nil {
				t.Fatal("resource retrieved after disk reopen freshened a replacement")
			}
			assertGenerationBudgetEntry(t, c, key, "newB", `W/"same"`)
			current, err := c.Retrieve(key)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = current.Close() }()
			if current.Header().Get("X-Generation") != "new" {
				t.Fatal("superseded freshening changed replacement metadata")
			}
		})
	}
}

func TestCacheRetrievedResourceCanFreshenRepeatedly(t *testing.T) {
	previousClock := Clock
	instant := previousClock()
	Clock = func() time.Time { return instant }
	t.Cleanup(func() { Clock = previousClock })
	c := generationBudgetCache(t, true, 1<<20)
	const key = "GET:http://origin.test/package"
	if err := c.Store(generationBudgetResource("body", `"same"`), key); err != nil {
		t.Fatal(err)
	}
	res, err := c.Retrieve(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Close() })
	for _, validation := range []string{"first", "second"} {
		res.Header().Set("X-Validated", validation)
		if err := c.Freshen(res, key); err != nil {
			t.Fatalf("%s freshening rejected its own updated generation: %v", validation, err)
		}
	}
	current, err := c.Retrieve(key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = current.Close() }()
	if current.Header().Get("X-Validated") != "second" {
		t.Fatal("second freshening was not persisted")
	}
}

type generationReadVFS struct {
	vfs.VFS
	armed      atomic.Bool
	headerPath string
	entered    chan struct{}
	release    chan struct{}
	removed    chan struct{}
	removeOnce sync.Once
}

func (v *generationReadVFS) Open(path string) (vfs.RFile, error) {
	f, err := v.VFS.Open(path)
	if err == nil && path == v.headerPath && v.armed.CompareAndSwap(true, false) {
		return &generationReadFile{RFile: f, owner: v}, nil
	}
	return f, err
}

func (v *generationReadVFS) Remove(path string) error {
	err := v.VFS.Remove(path)
	if err == nil && path == v.headerPath {
		v.removeOnce.Do(func() { close(v.removed) })
	}
	return err
}

type generationReadFile struct {
	vfs.RFile
	owner *generationReadVFS
}

func (f *generationReadFile) Stat() (os.FileInfo, error) {
	info, err := f.RFile.(interface{ Stat() (os.FileInfo, error) }).Stat()
	close(f.owner.entered)
	<-f.owner.release
	return info, err
}

func generationAwait(t *testing.T, event <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-event:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func TestCacheRetrieveRetainsGenerationAcrossCapacityEviction(t *testing.T) {
	previousClock := Clock
	instant := previousClock()
	Clock = func() time.Time { return instant }
	t.Cleanup(func() { Clock = previousClock })
	c := generationBudgetCache(t, true, 1<<20)
	const key = "GET:http://origin.test/package"
	resource := func(body, generation string) *Resource {
		res := generationBudgetResource(body, `W/"same"`)
		res.Header().Set("X-Generation", generation)
		return res
	}
	if err := c.Store(resource("oldA", "old"), key); err != nil {
		t.Fatal(err)
	}
	c.config.MaxSize = c.Stats().TotalSize
	fs := &generationReadVFS{
		VFS: c.fs, headerPath: headerPrefix + formatPrefix + hashKey(key),
		entered: make(chan struct{}), release: make(chan struct{}), removed: make(chan struct{}),
	}
	c.fs = fs
	fs.armed.Store(true)
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(fs.release) }) })
	type retrieval struct {
		res *Resource
		err error
	}
	retrieved := make(chan retrieval, 1)
	go func() {
		res, err := c.Retrieve(key)
		retrieved <- retrieval{res: res, err: err}
	}()
	generationAwait(t, fs.entered, "complete header read")
	stored := make(chan error, 1)
	go func() { stored <- c.Store(resource("othr", "mid"), "other-key") }()
	generationAwait(t, fs.removed, "capacity eviction of retrieved key")
	if c.entryGeneration(hashKey(key)) != nil {
		t.Fatal("capacity eviction did not remove the original ledger entry")
	}
	release.Do(func() { close(fs.release) })
	var old *Resource
	select {
	case result := <-retrieved:
		if result.err != nil {
			t.Fatal(result.err)
		}
		old = result.res
	case <-time.After(3 * time.Second):
		t.Fatal("retrieval did not complete")
	}
	t.Cleanup(func() { _ = old.Close() })
	select {
	case err := <-stored:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("capacity replacement did not complete")
	}
	if err := c.Store(resource("newB", "new"), key); err != nil {
		t.Fatal(err)
	}
	if err := c.Freshen(old, key); err == nil {
		t.Fatal("retrieved resource lost its generation identity during capacity eviction")
	}
	assertGenerationBudgetEntry(t, c, key, "newB", `W/"same"`)
}

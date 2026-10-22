package httpcache

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func generationBudgetCache(t *testing.T, disk bool, maxSize int64) *cache {
	t.Helper()
	config := DefaultCacheConfig().WithMaxSize(maxSize).WithCleanupInterval(0)
	var c ExtendedCache
	if disk {
		var err error
		c, err = NewDiskCacheWithConfig(t.TempDir(), config)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		c = NewMemoryCacheWithConfig(config)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*cache)
}

func generationBudgetResource(body, etag string) *Resource {
	return NewResourceBytes(200, []byte(body), http.Header{
		"Etag": {etag}, "Content-Length": {strconv.Itoa(len(body))}, "Cache-Control": {"max-age=3600"},
	})
}

func TestCacheFreshenRejectsSupersededGeneration(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(stringBool(disk), func(t *testing.T) {
			c := generationBudgetCache(t, disk, 1<<20)
			const key = "GET:http://cache.example/package.deb"
			if err := c.Store(generationBudgetResource("oldA", `W/"same"`), key); err != nil {
				t.Fatal(err)
			}
			old, err := c.Retrieve(key)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = old.Close() }()
			old.Header().Set("X-Generation", "old")
			// Matching weak validators do not allow a previously retrieved
			// object to overwrite metadata belonging to a replacement.
			newer := generationBudgetResource("newB", `W/"same"`)
			newer.Header().Set("X-Generation", "new")
			if err := c.Store(newer, key); err != nil {
				t.Fatal(err)
			}
			if err := c.Freshen(old, key); err == nil {
				t.Fatal("superseded resource freshened the replacement")
			}
			current, err := c.Retrieve(key)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = current.Close() }()
			body, err := io.ReadAll(current)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != "newB" || current.Header().Get("X-Generation") != "new" || current.IsStale() {
				t.Fatalf("new generation changed: body=%q headers=%v stale=%v", body, current.Header(), current.IsStale())
			}
		})
	}
}

func TestCacheFreshenAndStorePublishOneGeneration(t *testing.T) {
	oldClock := Clock
	var armed atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	Clock = func() time.Time {
		if armed.CompareAndSwap(true, false) {
			close(entered)
			<-release
		}
		return oldClock()
	}
	t.Cleanup(func() { Clock = oldClock })
	c := generationBudgetCache(t, true, 1<<20)
	const key = "GET:http://cache.example/package.deb"
	if err := c.Store(generationBudgetResource("oldA", `"v1"`), key); err != nil {
		t.Fatal(err)
	}
	old, err := c.Retrieve(key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()
	armed.Store(true)
	freshened := make(chan error, 1)
	go func() { freshened <- c.Freshen(old, key) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("freshening did not reach the check/publication window")
	}
	stored := make(chan error, 1)
	go func() { stored <- c.Store(generationBudgetResource("newB", `"v2"`), key) }()
	var earlyStore error
	var completedEarly bool
	select {
	case earlyStore = <-stored:
		completedEarly = true
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-freshened; err != nil {
		t.Fatal(err)
	}
	if !completedEarly {
		earlyStore = <-stored
	}
	if earlyStore != nil {
		t.Fatal(earlyStore)
	}
	if completedEarly {
		t.Fatal("Store published between Freshen's generation check and header commit")
	}
	assertGenerationBudgetEntry(t, c, key, "newB", `"v2"`)
}

func assertGenerationBudgetEntry(t *testing.T, c *cache, key, wantBody, wantETag string) {
	t.Helper()
	current, err := c.Retrieve(key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = current.Close() }()
	body, err := io.ReadAll(current)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != wantBody || current.Header().Get("Etag") != wantETag {
		t.Fatalf("mixed generation: body=%q Etag=%q", body, current.Header().Get("Etag"))
	}
}

type generationBudgetBody struct {
	reader  *bytes.Reader
	started chan<- struct{}
	release <-chan struct{}
	once    sync.Once
}

func (b *generationBudgetBody) Read(p []byte) (int, error) {
	b.once.Do(func() { b.started <- struct{}{}; <-b.release })
	return b.reader.Read(p)
}
func (b *generationBudgetBody) Seek(offset int64, whence int) (int64, error) {
	return b.reader.Seek(offset, whence)
}
func (b *generationBudgetBody) Close() error { return nil }

func TestCacheParallelStoresReserveCapacity(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(stringBool(disk), func(t *testing.T) {
			c := generationBudgetCache(t, disk, 2000)
			started, release := make(chan struct{}, 1), make(chan struct{})
			res := NewResource(200, &generationBudgetBody{reader: bytes.NewReader(bytes.Repeat([]byte("a"), 1200)), started: started, release: release}, http.Header{"Content-Length": {"1200"}})
			first := make(chan error, 1)
			go func() { first <- c.Store(res, "first") }()
			select {
			case <-started:
			case <-time.After(time.Second):
				close(release)
				select { case <-first: } // Drain the completed store after releasing the blocked stream.
				t.Fatal("first store did not reach streaming after admission")
			}
			secondErr := c.Store(generationBudgetResource(strings.Repeat("b", 1200), `"v2"`), "second")
			close(release)
			if err := <-first; err != nil {
				t.Fatal(err)
			}
			if secondErr == nil {
				t.Fatal("second admission ignored the first writer's reserved capacity")
			}
			// After publication, a later store can evict the first entry.
			if err := c.Store(generationBudgetResource(strings.Repeat("b", 1200), `"v2"`), "second"); err != nil {
				t.Fatal(err)
			}
			if stats := c.Stats(); stats.TotalSize > 2000 || stats.ItemCount != 1 {
				t.Fatalf("capacity was exceeded: %+v", stats)
			}
		})
	}
}

func TestCacheDifferentKeysStreamConcurrently(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(stringBool(disk), func(t *testing.T) {
			c := generationBudgetCache(t, disk, 10000)
			started, release := make(chan struct{}, 2), make(chan struct{})
			done := make(chan error, 2)
			for _, key := range []string{"first", "second"} {
				res := NewResource(200, &generationBudgetBody{reader: bytes.NewReader(bytes.Repeat([]byte("x"), 1200)), started: started, release: release}, http.Header{"Content-Length": {"1200"}})
				go func() { done <- c.Store(res, key) }()
			}
			concurrent := true
			for i := 0; i < 2; i++ {
				select {
				case <-started:
				case <-time.After(time.Second):
					concurrent = false
				}
			}
			close(release)
			for i := 0; i < 2; i++ {
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("released stream did not complete")
				}
			}
			if !concurrent {
				t.Fatal("different keys serialized their body streams")
			}
		})
	}
}

func TestCacheFailedStoreReleasesReservation(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(stringBool(disk), func(t *testing.T) {
			c := generationBudgetCache(t, disk, 2000)
			truncated := NewResourceBytes(200, []byte("short"), http.Header{"Content-Length": {"1200"}})
			if err := c.Store(truncated, "failed"); err == nil {
				t.Fatal("truncated store succeeded")
			}
			if err := c.Store(generationBudgetResource(strings.Repeat("x", 1200), `"full"`), "full"); err != nil {
				t.Fatalf("failed write retained its reservation: %v", err)
			}
			if c.reservedSize != 0 || len(c.reservations) != 0 || len(c.writers) != 0 {
				t.Fatalf("completed writers retained state: reserved=%d entries=%d locks=%d", c.reservedSize, len(c.reservations), len(c.writers))
			}
		})
	}
}

func TestCacheUnknownLengthCountsSerializedHeaders(t *testing.T) {
	c := generationBudgetCache(t, true, 1000)
	if err := c.Store(generationBudgetResource("old", `"old"`), "same"); err != nil {
		t.Fatal(err)
	}
	tooLarge := NewResourceBytes(200, []byte("new"), http.Header{"X-Large": {strings.Repeat("x", 1200)}})
	if err := c.Store(tooLarge, "same"); err == nil {
		t.Fatal("unknown-length entry ignored its serialized header size")
	}
	assertStoreBody(t, c, "same", "old")
	if err := c.Store(generationBudgetResource("new", `"new"`), "same"); err != nil {
		t.Fatalf("rejected replacement retained capacity: %v", err)
	}
}

func TestCacheFreshenAccountsHeaderGrowth(t *testing.T) {
	c := generationBudgetCache(t, true, 1000)
	if err := c.Store(generationBudgetResource("body", `"v1"`), "same"); err != nil {
		t.Fatal(err)
	}
	res, err := c.Retrieve("same")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Close() }()
	res.Header().Set("X-Growth", strings.Repeat("x", 300))
	if err := c.Freshen(res, "same"); err != nil {
		t.Fatal(err)
	}
	var actual int64
	for _, prefix := range []string{bodyPrefix, headerPrefix} {
		info, err := os.Stat(filepath.Join(c.diskRoot, prefix+formatPrefix+hashKey("same")))
		if err != nil {
			t.Fatal(err)
		}
		actual += info.Size()
	}
	if stats := c.Stats(); stats.TotalSize != actual {
		t.Fatalf("freshened header size missing from ledger: actual=%d stats=%+v", actual, stats)
	}
	updated, err := c.Retrieve("same")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = updated.Close() }()
	updated.Header().Set("X-Growth", strings.Repeat("x", 1200))
	if err := c.Freshen(updated, "same"); err == nil {
		t.Fatal("freshened headers bypassed MaxSize")
	}
	assertStoreBody(t, c, "same", "body")
	if c.Stats().TotalSize != actual {
		t.Fatal("rejected freshening changed tracked capacity")
	}
}

func TestCacheWaitingReplacementCannotDeadlockInvalidation(t *testing.T) {
	c := generationBudgetCache(t, true, 1<<20)
	started, release := make(chan struct{}, 1), make(chan struct{})
	res := NewResource(200, &generationBudgetBody{reader: bytes.NewReader([]byte("body")), started: started, release: release}, http.Header{"Content-Length": {"4"}})
	done := make(chan error, 2)
	go func() { done <- c.Store(res, "same") }()
	<-started
	go func() { done <- c.Store(generationBudgetResource("next", `"next"`), "same") }()
	invalidated := make(chan struct{})
	go func() { c.Invalidate("same"); close(invalidated) }()
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, ErrNotFoundInCache) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("writer deadlocked with invalidation")
		}
	}
	select {
	case <-invalidated:
	case <-time.After(time.Second):
		t.Fatal("invalidation deadlocked with a waiting replacement")
	}
}

func TestCacheReplacementReservesOnlyGrowth(t *testing.T) {
	c := generationBudgetCache(t, true, 2000)
	if err := c.Store(generationBudgetResource(strings.Repeat("a", 800), `"old"`), "same"); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}, 1), make(chan struct{})
	res := NewResource(200, &generationBudgetBody{reader: bytes.NewReader(bytes.Repeat([]byte("b"), 1200)), started: started, release: release}, http.Header{"Content-Length": {"1200"}})
	done := make(chan error, 1)
	go func() { done <- c.Store(res, "same") }()
	select {
	case <-started:
	case err := <-done:
		close(release)
		t.Fatalf("replacement incorrectly required its full size in addition to the old entry: %v", err)
	case <-time.After(time.Second):
		close(release)
		select { case <-done: } // Drain the completed store after releasing the blocked stream.
		t.Fatal("replacement did not reach its body stream")
	}
	// Its old entry remains charged until publication; the reserved growth
	// must also constrain a different key rather than make the old bytes free.
	competing := c.Store(generationBudgetResource(strings.Repeat("c", 700), `"other"`), "other")
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if competing == nil {
		t.Fatal("parallel admission consumed a replacement's reserved growth")
	}
	if stats := c.Stats(); stats.TotalSize > 2000 || stats.ItemCount != 1 {
		t.Fatalf("replacement capacity drifted: %+v", stats)
	}
}

func TestCacheCleanupPreservesActiveReplacement(t *testing.T) {
	c := generationBudgetCache(t, true, 1<<20)
	if err := c.Store(generationBudgetResource("old", `"old"`), "same"); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}, 1), make(chan struct{})
	res := NewResource(200, &generationBudgetBody{reader: bytes.NewReader([]byte("new")), started: started, release: release}, http.Header{"Content-Length": {"3"}})
	done := make(chan error, 1)
	go func() { done <- c.Store(res, "same") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		select { case <-done: } // Drain the completed store after releasing the blocked stream.
		t.Fatal("replacement did not reach its body stream")
	}
	c.lruMutex.Lock()
	c.lruIndex[hashKey("same")].storedAt = time.Now().Add(-2 * c.config.TTL)
	c.lruMutex.Unlock()
	result := c.Cleanup()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if result.RemovedItems != 0 {
		t.Fatal("cleanup removed an entry while its replacement was active")
	}
	assertStoreBody(t, c, "same", "new")
}

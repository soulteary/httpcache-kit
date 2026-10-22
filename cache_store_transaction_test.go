package httpcache

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Refuse actual byte admission on the second body, after the base key's body
// and metadata have been prepared. A separate active lease must survive.
type transactionBudget struct {
	*featureBudget
	bodyCalls int
	bodySize  int64
}

func (b *transactionBudget) NewLease(ctx context.Context, kind StageKind) (StagingLease, error) {
	b.mu.Lock()
	if kind == StageCacheBody {
		b.bodyCalls++
		if b.bodyCalls == 2 {
			b.limit = b.used + b.bodySize - 1
		}
	}
	b.mu.Unlock()
	return b.featureBudget.NewLease(ctx, kind)
}

func assertTransactionReleased(t *testing.T, c *cache, dir string, budget *featureBudget, held int64, active int) {
	t.Helper()
	c.lruMutex.Lock()
	reserved, count := c.reservedSize, len(c.reservations)
	c.lruMutex.Unlock()
	if reserved != 0 || count != 0 {
		t.Fatalf("transaction retained reservations: bytes=%d keys=%d", reserved, count)
	}
	if budget != nil {
		budget.mu.Lock()
		used, leases := budget.used, budget.active
		budget.mu.Unlock()
		if used != held || leases != active {
			t.Fatalf("transaction changed active leases: bytes=%d leases=%d, want %d/%d", used, leases, held, active)
		}
	}
	if dir != "" {
		entries, err := os.ReadDir(filepath.Join(dir, stagingPrefix, formatPrefix))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("transaction left staging files: %v", entries)
		}
	}
}

func TestMultiKeyStoreBudgetFailurePreservesPreviousVariant(t *testing.T) {
	const base, variant = "GET:http://origin.test/package", "GET:http://origin.test/package:language=en"
	const body = "replacement"
	budget := &transactionBudget{featureBudget: &featureBudget{limit: 1 << 20}, bodyCalls: -10, bodySize: int64(len(body))}
	dir := t.TempDir()
	cc, err := NewDiskCacheWithConfig(dir, &CacheConfig{Integrity: IntegritySHA256, StagingBudget: budget, CleanupInterval: 0})
	if err != nil {
		t.Fatal(err)
	}
	c := cc.(*cache)
	t.Cleanup(func() { _ = c.Close() })
	// Evict the older base through real admission, leaving its fresh variant.
	if err := c.Store(integrityFeatureResource("old-variant"), base, variant); err != nil {
		t.Fatal(err)
	}
	c.config.MaxSize = c.Stats().TotalSize + 16
	if err := c.Store(integrityFeatureResource("old-variant"), "unrelated-filler"); err != nil {
		t.Fatal(err)
	}
	integrityFeatureMiss(t, c, base)
	integrityFeatureBody(t, c, variant, "old-variant")
	c.config.MaxSize = 1 << 20
	outside, err := budget.NewLease(context.Background(), StageHandlerBody)
	if err != nil {
		t.Fatal(err)
	}
	defer outside.Close()
	if err := outside.Reserve(11); err != nil {
		t.Fatal(err)
	}
	budget.mu.Lock()
	budget.bodyCalls = 0
	budget.mu.Unlock()
	if err := c.Store(integrityFeatureResource(body), base, variant); !errors.Is(err, ErrStagingBudgetExceeded) {
		t.Fatalf("Store error=%v, want failed second-key admission", err)
	}
	integrityFeatureMiss(t, c, base)
	previous := integrityFeatureBody(t, c, variant, "old-variant")
	if previous.IsStale() {
		t.Fatal("unpublished replacement invalidated the intact old variant")
	}
	assertTransactionReleased(t, c, dir, budget.featureBudget, 11, 1)
	budget.mu.Lock()
	budget.limit = 1 << 20
	budget.bodyCalls = -10 // Subsequent successful stores have no injected refusal.
	budget.mu.Unlock()
	if err := c.Store(integrityFeatureResource(body), base, variant, base); err != nil {
		t.Fatal(err)
	}
	integrityFeatureBody(t, c, base, body)
	integrityFeatureBody(t, c, variant, body)
	assertTransactionReleased(t, c, dir, budget.featureBudget, 11, 1)
}

func TestMultiKeyStoreCapacityFailureDoesNotPublishBase(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(stringBool(disk), func(t *testing.T) {
			c := generationBudgetCache(t, disk, 1<<20)
			if err := c.Store(generationBudgetResource("old", `"old"`), "variant"); err != nil {
				t.Fatal(err)
			}
			body := strings.Repeat("n", 4096)
			res := generationBudgetResource(body, `"new"`)
			header, err := c.serializeCacheHeader(res.Status(), res.Header(), Clock(), "base", int64(len(body)), strings.Repeat("0", 64))
			if err != nil {
				t.Fatal(err)
			}
			// The first new key fits alongside the protected old variant. The
			// larger variant replacement cannot fit as well, so admission for
			// the second key must fail rather than evict the pending base.
			c.config.MaxSize = c.Stats().TotalSize + int64(len(body)+len(header)) + 16
			if err := c.Store(res, "base", "variant"); err == nil {
				t.Fatal("combined transaction exceeded persistent capacity")
			}
			integrityFeatureMiss(t, c, "base")
			if disk {
				assertGenerationBudgetEntry(t, c, "variant", "old", `"old"`)
			} else {
				integrityFeatureMiss(t, c, "variant")
			}
			assertTransactionReleased(t, c, c.diskRoot, nil, 0, 0)
			if got := c.Stats().TotalSize; got > c.config.MaxSize {
				t.Fatalf("failed transaction exceeded capacity: %d > %d", got, c.config.MaxSize)
			}
		})
	}
}

type transactionBody struct {
	*strings.Reader
	seeks   int
	entered chan struct{}
	release chan struct{}
	cancel  context.CancelFunc
}

func (b *transactionBody) Seek(offset int64, whence int) (int64, error) {
	b.seeks++
	if b.seeks == 2 {
		if b.entered != nil {
			close(b.entered)
			<-b.release
		}
		b.cancel()
	}
	return b.Reader.Seek(offset, whence)
}
func (b *transactionBody) Close() error { return nil }

func TestMultiKeyStoreCancellationDoesNotPublishBase(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(stringBool(disk), func(t *testing.T) {
			c := generationBudgetCache(t, disk, 1<<20)
			if err := c.Store(generationBudgetResource("old-variant", `"old"`), "variant"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := &transactionBody{Reader: strings.NewReader("new-body"), cancel: cancel}
			res := NewResource(http.StatusOK, body, http.Header{"Cache-Control": {"max-age=3600"}})
			if err := c.StoreContext(ctx, res, "base", "variant"); !errors.Is(err, context.Canceled) {
				t.Fatalf("StoreContext error=%v", err)
			}
			integrityFeatureMiss(t, c, "base")
			if disk {
				assertGenerationBudgetEntry(t, c, "variant", "old-variant", `"old"`)
			} else {
				// A generic VFS writes in place, so roll back every affected key.
				integrityFeatureMiss(t, c, "variant")
			}
			assertTransactionReleased(t, c, c.diskRoot, nil, 0, 0)
		})
	}
}

func TestFailedMultiKeyStoreCannotDeleteConcurrentNewerGeneration(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(stringBool(disk), func(t *testing.T) {
			c := generationBudgetCache(t, disk, 1<<20)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := &transactionBody{Reader: strings.NewReader("older"), cancel: cancel, entered: make(chan struct{}), release: make(chan struct{})}
			var release sync.Once
			defer release.Do(func() { close(body.release) })
			failed := make(chan error, 1)
			go func() { failed <- c.StoreContext(ctx, NewResource(200, body, http.Header{}), "base", "variant") }()
			select {
			case <-body.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("store did not reach second-key preparation")
			}
			newer := make(chan error, 1)
			go func() { newer <- c.Store(generationBudgetResource("newer", `"new"`), "base") }()
			deadline := time.After(3 * time.Second)
			for {
				c.writerMu.Lock()
				writer := c.writers[hashKey("base")]
				joined := writer != nil && writer.users == 2
				c.writerMu.Unlock()
				if joined {
					break
				}
				select {
				case err := <-newer:
					release.Do(func() { close(body.release) })
					if err := <-failed; err != nil {
						t.Logf("cleanup store failed: %v", err)
					}
					t.Fatalf("newer writer passed incomplete transaction: %v", err)
				case <-deadline:
					t.Fatal("newer writer did not join the protected base key")
				default:
					runtime.Gosched()
				}
			}
			release.Do(func() { close(body.release) })
			if err := <-failed; !errors.Is(err, context.Canceled) {
				t.Fatalf("older StoreContext error=%v", err)
			}
			if err := <-newer; err != nil {
				t.Fatal(err)
			}
			assertGenerationBudgetEntry(t, c, "base", "newer", `"new"`)
			integrityFeatureMiss(t, c, "variant")
			assertTransactionReleased(t, c, c.diskRoot, nil, 0, 0)
		})
	}
}

func TestMultiKeyStoresWithReversedKeysComplete(t *testing.T) {
	c := generationBudgetCache(t, true, 1<<20)
	start, done := make(chan struct{}), make(chan error, 2)
	for _, keys := range [][]string{{"base", "variant"}, {"variant", "base"}} {
		go func() {
			<-start
			done <- c.Store(generationBudgetResource("same-body", `"same"`), keys...)
		}()
	}
	close(start)
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("overlapping transactions deadlocked")
		}
	}
	for _, key := range []string{"base", "variant"} {
		integrityFeatureBody(t, c, key, "same-body")
	}
	assertTransactionReleased(t, c, c.diskRoot, nil, 0, 0)
}

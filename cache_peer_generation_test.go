package httpcache

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	vfs "github.com/soulteary/vfs-kit"
)

func peerGenerationCache(t *testing.T, dir string, integrity IntegrityMode) *cache {
	t.Helper()
	cfg := DefaultCacheConfig().WithCleanupInterval(0)
	cfg.DisableStagingSweep = true // The caller coordinates multiple instances.
	cfg.Integrity = integrity
	c, err := NewDiskCacheWithConfig(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*cache)
}

func peerGenerationClock(t *testing.T) *time.Time {
	t.Helper()
	previous := Clock
	now := time.Date(2026, 10, 13, 12, 0, 0, 123, time.UTC)
	Clock = func() time.Time { return now }
	t.Cleanup(func() { Clock = previous })
	return &now
}

func TestCacheFreshenRejectsCoordinatedPeerGeneration(t *testing.T) {
	for _, integrity := range []IntegrityMode{IntegrityDisabled, IntegritySHA256} {
		for _, elapsed := range []time.Duration{time.Second, 0, -time.Minute} {
			for _, reopen := range []bool{false, true} {
				for _, sameRecord := range []bool{false, true} {
					name := fmt.Sprintf("integrity=%d/time=%s/reopen=%t/identical-record=%t", integrity, elapsed, reopen, sameRecord)
					t.Run(name, func(t *testing.T) {
						peerGenerationScenario(t, integrity, elapsed, reopen, sameRecord)
					})
				}
			}
		}
	}
}

func peerGenerationScenario(t *testing.T, integrity IntegrityMode, elapsed time.Duration, reopen, sameRecord bool) {
	t.Helper()
	now := peerGenerationClock(t)
	dir := t.TempDir()
	first := peerGenerationCache(t, dir, integrity)
	const key = "GET:http://origin.test/package"
	older := generationBudgetResource("oldA", `W/"same"`)
	older.Header().Set("Cache-Control", "max-age=3600")
	if err := first.Store(older, key); err != nil {
		t.Fatal(err)
	}
	if reopen {
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		first = peerGenerationCache(t, dir, integrity)
	}
	old, err := first.Retrieve(key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()
	originalHeader := old.storageRecord.file
	peer := peerGenerationCache(t, dir, integrity)
	*now = now.Add(elapsed)
	newer := generationBudgetResource("newB", `W/"same"`)
	newer.Header().Set("Cache-Control", "max-age=3600")
	if !sameRecord {
		newer.Header().Set("Cache-Control", "max-age=0")
		newer.Header().Set("X-Generation", "new")
	}
	// Repeated peer renames cannot recycle the still-open original
	// header inode, even when the serialized records are identical.
	for range 20 {
		if err := peer.Store(newer, key); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := originalHeader.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("retrieval did not keep original header alive: %v", err)
	}
	old.Header().Set("X-Validated", "old")
	if err := first.Freshen(old, key); err == nil {
		t.Fatal("old retrieval freshened a peer replacement")
	}
	assertPeerGenerationPreserved(t, first, key, newer)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := originalHeader.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("old header retained after Resource.Close: %v", err)
	}
}

func assertPeerGenerationPreserved(t *testing.T, first *cache, key string, newer *Resource) {
	t.Helper()
	current, err := first.Retrieve(key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = current.Close() }()
	body, err := io.ReadAll(current)
	if err != nil || string(body) != "newB" || current.Header().Get("X-Validated") != "" || current.Header().Get("Cache-Control") != newer.Header().Get("Cache-Control") {
		t.Fatalf("rejected validation changed peer generation: body=%q headers=%v err=%v", body, current.Header(), err)
	}
}

func TestCachePeerDiscoveryReplacementDoesNotRejectSelectedVariant(t *testing.T) {
	now := peerGenerationClock(t)
	dir := t.TempDir()
	first := peerGenerationCache(t, dir, IntegritySHA256)
	const base, variant = "GET:http://origin.test/package", "GET:http://origin.test/package:language=en"
	if err := first.Store(generationBudgetResource("oldA", `W/"en"`), base, variant); err != nil {
		t.Fatal(err)
	}
	old, err := first.Retrieve(variant)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()
	peer := peerGenerationCache(t, dir, IntegritySHA256)
	*now = now.Add(time.Second)
	if err := peer.Store(generationBudgetResource("newB", `W/"fr"`), base, base+":language=fr"); err != nil {
		t.Fatal(err)
	}
	old.Header().Set("X-Validated", "yes")
	if err := first.Freshen(old, base, variant); err != nil {
		t.Fatalf("unchanged selected variant rejected after only peer discovery changed: %v", err)
	}
	assertGenerationBudgetEntry(t, first, base, "newB", `W/"fr"`)
	current, err := first.Retrieve(variant)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = current.Close() }()
	if current.Header().Get("X-Validated") != "yes" {
		t.Fatal("selected variant was not freshened")
	}
}

func TestCacheRepeatedFreshenAdvancesRetainedProvenance(t *testing.T) {
	for _, elapsed := range []time.Duration{time.Second, 0, -time.Minute} {
		t.Run(elapsed.String(), func(t *testing.T) {
			now := peerGenerationClock(t)
			c := peerGenerationCache(t, t.TempDir(), IntegritySHA256)
			const key = "GET:http://origin.test/package"
			if err := c.Store(generationBudgetResource("body", `"same"`), key); err != nil {
				t.Fatal(err)
			}
			res, err := c.Retrieve(key)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = res.Close() }()
			for round := range 3 {
				previous := res.storageRecord.file
				*now = now.Add(elapsed)
				res.Header().Set("X-Validated", fmt.Sprint(round))
				if err := c.Freshen(res, key); err != nil {
					t.Fatalf("freshening round %d: %v", round, err)
				}
				if !res.storedAt.Equal(*now) {
					t.Fatal("own persisted timestamp did not advance")
				}
				if _, err := previous.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("superseded own header descriptor was not closed: %v", err)
				}
			}
			lastHeader := res.storageRecord.file
			if err := res.Close(); err != nil {
				t.Fatal(err)
			}
			if err := res.Close(); err != nil {
				t.Fatalf("Resource.Close not idempotent: %v", err)
			}
			if _, err := lastHeader.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("final descriptor remained open: %v", err)
			}
		})
	}
}

type peerHeaderTrackingVFS struct {
	vfs.VFS
	opens, closes atomic.Int64
	failStatAt    atomic.Int64
}

func (v *peerHeaderTrackingVFS) Open(path string) (vfs.RFile, error) {
	f, err := v.VFS.Open(path)
	if err == nil && strings.HasPrefix(path, headerPrefix) {
		ordinal := v.opens.Add(1)
		return &peerHeaderTrackingFile{RFile: f, owner: v, ordinal: ordinal}, nil
	}
	return f, err
}

type peerHeaderTrackingFile struct {
	vfs.RFile
	owner   *peerHeaderTrackingVFS
	ordinal int64
	closed  atomic.Bool
}

func (f *peerHeaderTrackingFile) Stat() (os.FileInfo, error) {
	if f.owner.failStatAt.Load() == f.ordinal {
		return nil, errors.New("injected header identity failure")
	}
	return f.RFile.(interface{ Stat() (os.FileInfo, error) }).Stat()
}

func (f *peerHeaderTrackingFile) Close() error {
	if f.closed.CompareAndSwap(false, true) {
		f.owner.closes.Add(1)
	}
	return f.RFile.Close()
}

func TestCacheCorruptBodyClosesRetainedHeader(t *testing.T) {
	c := peerGenerationCache(t, t.TempDir(), IntegritySHA256)
	const key = "GET:http://origin.test/package"
	if err := c.Store(generationBudgetResource("body", `"same"`), key); err != nil {
		t.Fatal(err)
	}
	tracking := &peerHeaderTrackingVFS{VFS: c.fs}
	c.fs = tracking
	if err := os.WriteFile(filepath.Join(c.diskRoot, bodyPrefix+formatPrefix+hashKey(key)), []byte("oops"), 0600); err != nil {
		t.Fatal(err)
	}
	if res, err := c.Retrieve(key); !errors.Is(err, ErrNotFoundInCache) || res != nil {
		t.Fatalf("corruption was accepted: %v/%v", res, err)
	}
	if tracking.opens.Load() == 0 || tracking.opens.Load() != tracking.closes.Load() {
		t.Fatalf("corrupt retrieval leaked header: opens=%d closes=%d", tracking.opens.Load(), tracking.closes.Load())
	}
}

func TestCacheHeaderProvenanceFailureClosesDescriptor(t *testing.T) {
	for _, freshen := range []bool{false, true} {
		t.Run(fmt.Sprintf("freshen=%t", freshen), func(t *testing.T) {
			c := peerGenerationCache(t, t.TempDir(), IntegritySHA256)
			const key = "GET:http://origin.test/package"
			if err := c.Store(generationBudgetResource("body", `"same"`), key); err != nil {
				t.Fatal(err)
			}
			tracking := &peerHeaderTrackingVFS{VFS: c.fs}
			c.fs = tracking
			if !freshen {
				tracking.failStatAt.Store(1)
				if res, err := c.Retrieve(key); res != nil || err == nil {
					t.Fatalf("header identity failure accepted: %v/%v", res, err)
				}
			} else {
				res, err := c.Retrieve(key)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = res.Close() }()
				// The third opened header is the newly published record whose
				// descriptor would replace the still-retained original header.
				tracking.failStatAt.Store(3)
				res.Header().Set("X-Validated", "yes")
				if err := c.Freshen(res, key); err == nil {
					t.Fatal("failed new header identity was accepted")
				}
				if _, err := res.storageRecord.file.Seek(0, io.SeekStart); err != nil {
					t.Fatalf("failed update discarded original descriptor prematurely: %v", err)
				}
				if err := res.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if tracking.opens.Load() == 0 || tracking.opens.Load() != tracking.closes.Load() {
				t.Fatalf("failed provenance leaked a descriptor: opens=%d closes=%d", tracking.opens.Load(), tracking.closes.Load())
			}
		})
	}
}

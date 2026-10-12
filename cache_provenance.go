package httpcache

import (
	"errors"
	"fmt"
	"sync"
	"time"

	vfs "github.com/soulteary/vfs-kit"
)

// Holding the opened native header keeps its inode alive after a peer's
// atomic rename. Equal timestamps and identical serialized headers cannot
// then mistake a recycled inode for the originally retrieved generation.
type headerRetainingBody struct {
	ReadSeekCloser
	mu     sync.Mutex
	header vfs.RFile
	closed bool
	err    error
}

func (b *headerRetainingBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed {
		b.closed = true
		b.err = errors.Join(b.ReadSeekCloser.Close(), b.header.Close())
	}
	return b.err
}

func (b *headerRetainingBody) replaceHeader(f vfs.RFile) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		_ = f.Close()
		return errors.New("cached resource is already closed")
	}
	old := b.header
	b.header = f
	return old.Close()
}

func (c *cache) advanceResourceProvenance(res *Resource, key string, raw []byte, at time.Time) error {
	if res.storageRecord == nil {
		return nil
	}
	updated := &headerProvenance{fingerprint: sha256Text(raw)}
	if c.diskRoot != "" {
		_, _, _, proof, err := c.readHeaderProvenance(headerPrefix+formatPrefix+hashKey(key), key, true, true)
		if err != nil {
			return fmt.Errorf("failed to identify freshened header for key %q: %w", key, err)
		}
		body, ok := res.ReadSeekCloser.(*headerRetainingBody)
		if !ok {
			_ = proof.file.Close()
			return errors.New("cached resource lost its retained header")
		}
		if err := body.replaceHeader(proof.file); err != nil {
			return fmt.Errorf("failed to replace retained header for key %q: %w", key, err)
		}
		updated = proof
	}
	res.storageRecord = updated
	res.SetStoredAt(at)
	return nil
}

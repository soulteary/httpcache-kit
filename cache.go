package httpcache

import (
	"bufio"
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"hash/fnv"
	"io"
	"net/http"
	"net/textproto"
	"os"
	pathutil "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/soulteary/vfs-kit"
)

// hash64Pool reuses FNV-1a 64-bit hashers to reduce allocations in hashKey.
var hash64Pool = sync.Pool{
	New: func() interface{} { return fnv.New64a() },
}

const (
	headerPrefix = "header/"
	bodyPrefix   = "body/"
	formatPrefix = "v1/"
	// stagingPrefix holds entry files that are written but not yet published.
	// It sits outside body/ and header/ so scanExistingCache never mistakes a
	// staging file for an entry, and sweepStaging clears whatever an
	// interrupted process left behind.
	stagingPrefix = "staging/"

	// storedAtPreamble persists the cache's full-precision local write or
	// validation time before the serialized HTTP response. Keeping metadata
	// outside the response header namespace means no origin field can collide
	// with it.
	storedAtPreamble = "HTTPCACHE/1 "
)

// Returned when a resource doesn't exist
var ErrNotFoundInCache = errors.New("not found in cache")

type Cache interface {
	Header(key string) (Header, error)
	Store(res *Resource, keys ...string) error
	Retrieve(key string) (*Resource, error)
	Invalidate(keys ...string)
	Freshen(res *Resource, keys ...string) error
}

// ExtendedCache extends Cache with management capabilities
type ExtendedCache interface {
	Cache
	// Stats returns current cache statistics
	Stats() CacheStats
	// Cleanup runs a manual cleanup cycle
	Cleanup() CleanupResult
	// Purge removes all cached items
	Purge() error
	// Close stops the cache and cleanup goroutines
	Close() error
}

// CacheStats holds cache statistics
type CacheStats struct {
	// TotalSize is the total size of cached items in bytes
	TotalSize int64
	// ItemCount is the number of cached items
	ItemCount int
	// StaleCount is the number of stale map entries
	StaleCount int
	// HitCount is the number of cache hits
	HitCount int64
	// MissCount is the number of cache misses
	MissCount int64
}

// CleanupResult holds the result of a cleanup operation
type CleanupResult struct {
	// RemovedItems is the number of items removed
	RemovedItems int
	// RemovedBytes is the number of bytes freed
	RemovedBytes int64
	// RemovedStaleEntries is the number of stale map entries removed
	RemovedStaleEntries int
	// Duration is how long the cleanup took
	Duration time.Duration
}

// cacheEntry tracks metadata for a cached item
type cacheEntry struct {
	key        string
	hashedKey  string
	size       int64
	storedAt   time.Time
	accessedAt time.Time
	element    *list.Element // for LRU tracking
}

type cacheKeyWriter struct {
	mu    sync.Mutex
	users int
}

// cache provides a storage mechanism for cached Resources
type cache struct {
	fs     vfs.VFS
	config *CacheConfig
	// diskRoot is set only by NewDiskCacheWithConfig. It lets the marker
	// snapshot use os.Rename, which the deliberately small vfs.VFS interface
	// does not expose; custom VFS backends use the two-slot journal instead.
	diskRoot string

	// generationMu orders complete stores/freshens against the FINAL timestamp
	// of an invalidation. Invalidate first installs a visible barrier marker,
	// then waits here for older writers before replacing it with the timestamp
	// that permanently judges their stored generations.
	generationMu sync.RWMutex

	// cleanupMu makes removal of an expired file and its governing stale marker
	// one observable transition for Retrieve. It is intentionally independent
	// of generationMu: a pending invalidation writer must not block lookups that
	// can already observe its published barrier.
	cleanupMu sync.RWMutex

	// publishMu makes an entry's body and header change together. Store holds
	// the write side across both renames; Retrieve holds the read side across
	// opening the body and reading the header, so it cannot pair one version's
	// body with the other's header.
	publishMu sync.RWMutex

	// Only writers of the same key share a long-lived lock. Different keys
	// remain streamable in parallel; the reference count bounds the lock map
	// to keys with an active or waiting writer.
	writerMu    sync.Mutex
	writers     map[string]*cacheKeyWriter
	directoryMu sync.Mutex

	// stale map with mutex protection
	stale           map[string]time.Time
	staleGeneration uint64
	staleMutex      sync.RWMutex

	// LRU tracking
	lruList   *list.List             // front = most recently used
	lruIndex  map[string]*cacheEntry // hashedKey -> entry
	lruMutex  sync.RWMutex
	totalSize int64
	// Admission reserves growth until publication updates totalSize. Even a
	// zero-growth reservation protects an entry being replaced or freshened.
	reservations map[string]int64
	reservedSize int64

	// Statistics
	hitCount  int64
	missCount int64
	statMutex sync.RWMutex

	// Cleanup control
	stopChan    chan struct{}
	cleanupDone chan struct{}
	stopped     bool
	closeOnce   sync.Once
}

var _ Cache = (*cache)(nil)
var _ ExtendedCache = (*cache)(nil)
var _ ContextStore = (*cache)(nil)

type Header struct {
	http.Header
	StatusCode int
}

// NewVFSCache returns a cache backend off the provided VFS
func NewVFSCache(fs vfs.VFS) Cache {
	return NewVFSCacheWithConfig(fs, nil)
}

// NewVFSCacheWithConfig returns a cache backend with custom configuration
func NewVFSCacheWithConfig(fs vfs.VFS, config *CacheConfig) ExtendedCache {
	return newVFSCacheWithConfig(fs, config, "")
}

func newVFSCacheWithConfig(fs vfs.VFS, config *CacheConfig, diskRoot string) ExtendedCache {
	if config == nil {
		config = DefaultCacheConfig()
	}
	config.Validate()

	c := &cache{
		fs:          fs,
		config:      config,
		diskRoot:    diskRoot,
		stale:       make(map[string]time.Time),
		lruList:     list.New(),
		lruIndex:    make(map[string]*cacheEntry),
		stopChan:    make(chan struct{}),
		cleanupDone: make(chan struct{}),
	}

	// A caller may supply a persistent or deliberately reused VFS. Restore it
	// here rather than only in NewDiskCacheWithConfig; otherwise its body and
	// header files survive reconstruction while the invalidation markers do
	// not, republishing pre-mutation entries as fresh.
	c.sweepStaging()
	if err := c.scanExistingCache(); err != nil {
		debugf("warning: failed to scan existing cache: %v", err)
	}
	c.loadStale()

	// Start cleanup goroutine if interval is configured
	if config.CleanupInterval > 0 {
		go c.cleanupLoop()
	} else {
		close(c.cleanupDone)
	}

	return c
}

// NewMemoryCache returns an ephemeral cache in memory
func NewMemoryCache() Cache {
	return NewVFSCache(vfs.Memory())
}

// NewMemoryCacheWithConfig returns an ephemeral cache with custom configuration
func NewMemoryCacheWithConfig(config *CacheConfig) ExtendedCache {
	return NewVFSCacheWithConfig(vfs.Memory(), config)
}

// NewDiskCache returns a disk-backed cache
func NewDiskCache(dir string) (Cache, error) {
	return NewDiskCacheWithConfig(dir, nil)
}

// NewDiskCacheWithConfig returns a disk-backed cache with custom configuration
func NewDiskCacheWithConfig(dir string, config *CacheConfig) (ExtendedCache, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0750); err != nil {
		return nil, err
	}
	fs, err := vfs.FS(root)
	if err != nil {
		return nil, err
	}
	chfs, err := vfs.Chroot("/", fs)
	if err != nil {
		return nil, err
	}
	// Supply the OS root before restoration and the cleanup goroutine start,
	// so every disk marker write can use atomic replacement without a race.
	return newVFSCacheWithConfig(chfs, config, root), nil
}

// scanExistingCache scans the cache directory for existing cached files
// and rebuilds the LRU index. Constructors call it for any potentially
// persistent VFS.
func (c *cache) scanExistingCache() error {
	start := Clock()
	scannedFiles := 0
	var totalSize int64

	// Scan body files (they contain the actual cached data)
	bodyDir := bodyPrefix + formatPrefix
	if err := c.scanDirectory(bodyDir, func(hashedKey string, info os.FileInfo) {
		// Create entry for this cached item
		storedAt := info.ModTime()
		entry := &cacheEntry{
			key:        hashedKey, // We don't have the original key, use hashed key
			hashedKey:  hashedKey,
			size:       info.Size(),
			storedAt:   storedAt,
			accessedAt: info.ModTime(),
		}

		// Check if corresponding header file exists and add its size
		headerPath := headerPrefix + formatPrefix + hashedKey
		if headerInfo, err := c.fs.Stat(headerPath); err == nil {
			entry.size += headerInfo.Size()
			// Freshen does not rewrite the body, so its mtime cannot carry the
			// new validation generation across a restart. The header is written
			// on both Store and Freshen, making its mtime the safe old-format
			// fallback; a new-format preamble supplies the exact timestamp.
			entry.storedAt = headerInfo.ModTime()
			entry.accessedAt = headerInfo.ModTime()
			// A scan has only the filename hash, not the original request key.
			// Validate the private record while deferring its key binding to lookup.
			if _, persistedAt, _, readErr := c.readHeaderRecordWithKeyCheck(headerPath, hashedKey, false); readErr == nil && !persistedAt.IsZero() {
				entry.storedAt = persistedAt
				entry.accessedAt = persistedAt
			}
		}

		// Add to LRU tracking
		c.lruMutex.Lock()
		entry.element = c.lruList.PushBack(entry) // Push to back (oldest first for startup)
		c.lruIndex[hashedKey] = entry
		c.totalSize += entry.size
		c.lruMutex.Unlock()

		scannedFiles++
		totalSize += entry.size
	}); err != nil {
		return err
	}

	duration := Clock().Sub(start)
	if scannedFiles > 0 {
		debugf("scanned %d existing cache files (%d bytes) in %s",
			scannedFiles, totalSize, duration)
	}

	return nil
}

// scanDirectory scans a directory for cached files and calls the callback for each
func (c *cache) scanDirectory(dir string, callback func(hashedKey string, info os.FileInfo)) error {
	files, err := c.fs.ReadDir(dir)
	if err != nil {
		if vfs.IsNotExist(err) {
			return nil // Directory doesn't exist yet, that's fine
		}
		return err
	}

	for _, info := range files {
		if info.IsDir() {
			continue
		}
		// The filename is the hashed key
		hashedKey := info.Name()
		callback(hashedKey, info)
	}

	return nil
}

// stagedEntry is one entry file written and waiting to be published.
//
// On a disk-backed cache the bytes live in a staging file that commitStaged
// renames into place; published stays false until then, so a failure anywhere
// before the commit leaves the entry that is already there untouched. Other
// VFS backends have no rename in the interface and write in place, so their
// contents are published the moment they are written and published is true.
type stagedEntry struct {
	tmpPath   string
	target    string
	published bool
	lease     StagingLease
}

// stageWrite writes path's new contents, publishing them immediately only on
// a backend that cannot stage. See stagedEntry.
//
// The in-place write is what made publication non-atomic: O_CREATE|O_TRUNC
// makes an empty file visible before any bytes are copied into it, which on a
// first store exposes a window where the entry looks present but unreadable,
// and on a re-store destroys a perfectly good entry for the duration of the
// write. readHeaderFile's terminator check still turns that window into a
// miss on backends that keep it.
func (c *cache) stageWrite(path string, r io.Reader) (*stagedEntry, int64, error) {
	return c.stageWriteContext(context.Background(), path, r)
}

func (c *cache) stageWriteContext(ctx context.Context, path string, r io.Reader) (*stagedEntry, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if err := c.mkdirAll(pathutil.Dir(path)); err != nil {
		return nil, 0, fmt.Errorf("failed to create cache directory for %q: %w", path, err)
	}
	if c.diskRoot == "" {
		n, published, err := c.vfsWrite(path, &contextReader{ctx: ctx, r: r})
		return &stagedEntry{published: published}, n, err
	}
	if err := c.mkdirAll(stagingPrefix + formatPrefix); err != nil {
		return nil, 0, fmt.Errorf("failed to create staging directory: %w", err)
	}
	stagingDir := filepath.Join(c.diskRoot, filepath.FromSlash(stagingPrefix+formatPrefix))
	target := filepath.Join(c.diskRoot, filepath.FromSlash(path))
	kind := StageCacheBody
	if strings.HasPrefix(path, headerPrefix) {
		kind = StageCacheHeader
	}
	lease, err := newStageLease(ctx, c.config.StagingBudget, kind)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.CreateTemp(stagingDir, filepath.Base(target)+".tmp-*")
	if err != nil {
		closeStageLease(lease)
		return nil, 0, err
	}
	e := &stagedEntry{tmpPath: f.Name(), target: target, lease: lease}
	n, copyErr := io.Copy(&stagingWriter{ctx: ctx, w: f, lease: lease}, r)
	err = errors.Join(copyErr, f.Close())
	if err != nil {
		c.abandonStaged(e)
	}
	return e, n, err
}

func (c *cache) mkdirAll(path string) error {
	// Some VFS implementations do not accept a directory created between
	// their Stat and Mkdir calls. Serialize only this short shared operation.
	c.directoryMu.Lock()
	defer c.directoryMu.Unlock()
	return vfs.MkdirAll(c.fs, path, 0700)
}

func (c *cache) lockKeyWriter(hashedKey string) func() {
	c.writerMu.Lock()
	if c.writers == nil {
		c.writers = make(map[string]*cacheKeyWriter)
	}
	writer := c.writers[hashedKey]
	if writer == nil {
		writer = &cacheKeyWriter{}
		c.writers[hashedKey] = writer
	}
	writer.users++
	c.writerMu.Unlock()
	writer.mu.Lock()
	return func() {
		writer.mu.Unlock()
		c.writerMu.Lock()
		writer.users--
		if writer.users == 0 {
			delete(c.writers, hashedKey)
		}
		c.writerMu.Unlock()
	}
}

// commitStaged publishes every staged file as one step, so a reader holding
// publishMu's read side sees the whole previous entry or the whole new one.
func (c *cache) commitStaged(entries ...*stagedEntry) error {
	c.publishMu.Lock()
	defer c.publishMu.Unlock()
	return c.commitStagedLocked(entries...)
}

// commitStagedLocked requires publishMu, allowing a failed multi-key commit
// to discard its partial publication before any reader observes it.
func (c *cache) commitStagedLocked(entries ...*stagedEntry) error {
	for _, e := range entries {
		if e == nil || e.tmpPath == "" {
			continue
		}
		if err := os.Rename(e.tmpPath, e.target); err != nil {
			return fmt.Errorf("failed to publish cache file %q: %w", e.target, err)
		}
		e.tmpPath = ""
		e.published = true
		closeStageLease(e.lease)
		e.lease = nil
	}
	return nil
}

// abandonStaged drops files that never made it to a commit.
func (c *cache) abandonStaged(entries ...*stagedEntry) {
	for _, e := range entries {
		if e == nil || e.tmpPath == "" {
			continue
		}
		if err := os.Remove(e.tmpPath); err != nil && !os.IsNotExist(err) {
			debugf("failed to remove staging file %s: %v", e.tmpPath, err)
			continue
		}
		e.tmpPath = ""
		closeStageLease(e.lease)
		e.lease = nil
	}
}

// anyPublished reports whether a failed store already changed something on
// disk, either through an in-place write or a partially completed commit.
func anyPublished(entries ...*stagedEntry) bool {
	for _, e := range entries {
		if e != nil && e.published {
			return true
		}
	}
	return false
}

// sweepStaging removes staging files an interrupted process left behind. They
// sit outside the scanned directories, so they never reach the LRU index, but
// nothing else would reclaim their space either.
func (c *cache) sweepStaging() {
	if c.diskRoot == "" || c.config.DisableStagingSweep {
		return
	}
	dir := stagingPrefix + formatPrefix
	files, err := c.fs.ReadDir(dir)
	if err != nil {
		return
	}
	for _, info := range files {
		if info.IsDir() {
			continue
		}
		if err := c.fs.Remove(dir + info.Name()); err != nil {
			debugf("failed to remove stale staging file %s: %v", dir+info.Name(), err)
		}
	}
}

// vfsWrite reports whether OpenFile succeeded and may therefore have
// truncated or partially replaced the target. Callers updating an existing
// cache entry use that bit to discard stale metadata after delayed copy/close
// failures without deleting a still-intact entry after a pre-open failure.
func (c *cache) vfsWrite(path string, r io.Reader) (int64, bool, error) {
	if err := c.mkdirAll(pathutil.Dir(path)); err != nil {
		return 0, false, fmt.Errorf("failed to create cache directory for %q: %w", path, err)
	}
	f, err := c.fs.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return 0, false, fmt.Errorf("failed to open cache file %q: %w", path, err)
	}
	n, err := io.Copy(f, r)
	if err != nil {
		_ = f.Close()
		return n, true, fmt.Errorf("failed to write cache file %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return n, true, fmt.Errorf("failed to close cache file %q: %w", path, err)
	}
	return n, true, nil
}

// atomicWriteFile writes a complete sibling temporary file before replacing
// path, and fsyncs it first. A failed copy, sync, or close therefore leaves
// the last valid target untouched; os.Rename makes the final same-filesystem
// replacement atomic.
//
// The stale map takes this: losing invalidation state would republish
// superseded content, so it is worth the fsync. Cache entries go through
// stageWriteContext instead, which skips it.

func atomicWriteFile(path string, r io.Reader) (int64, error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return 0, fmt.Errorf("failed to create temporary cache file for %q: %w", path, err)
	}
	tmpPath := f.Name()
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
		_ = os.Remove(tmpPath)
	}()

	n, err := io.Copy(f, r)
	if err != nil {
		return 0, fmt.Errorf("failed to write temporary cache file for %q: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("failed to sync temporary cache file for %q: %w", path, err)
	}
	closeErr := f.Close()
	closed = true
	if closeErr != nil {
		return 0, fmt.Errorf("failed to close temporary cache file for %q: %w", path, closeErr)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return 0, fmt.Errorf("failed to replace cache file %q: %w", path, err)
	}
	return n, nil
}

// Retrieve the Status and Headers for a given key path
func (c *cache) Header(key string) (Header, error) {
	h, err := c.header(key)
	return h, c.integrityMiss(key, err)
}

func (c *cache) header(key string) (Header, error) {
	c.cleanupMu.RLock()
	defer c.cleanupMu.RUnlock()
	c.publishMu.RLock()
	defer c.publishMu.RUnlock()
	hashed := hashKey(key)
	h, _, meta, err := c.readHeaderRecord(headerPrefix+formatPrefix+hashed, key)
	if err != nil || meta == nil {
		return h, err
	}
	f, err := c.fs.Open(bodyPrefix + formatPrefix + hashed)
	if err != nil {
		if vfs.IsNotExist(err) {
			return Header{}, &corruptEntryError{fingerprint: meta.fingerprint, reason: "body missing"}
		}
		return Header{}, err
	}
	defer func() { _ = f.Close() }()
	if err := verifyStoredBody(f, meta, meta.fingerprint); err != nil {
		return Header{}, err
	}
	return h, nil
}

// readHeaderFile reads a stored response header and its separate cache
// metadata preamble.
// headerRecordTerminator ends a serialized header record. headersToWriter
// emits http.Header.Write's output followed by a bare CRLF, so a complete
// record always ends with a blank line. No truncation of that record can end
// with one: header lines are written back to back, so cutting after any of
// them leaves a single CRLF, not two.
const headerRecordTerminator = "\r\n\r\n"

func (c *cache) readHeaderRecord(path, key string) (Header, time.Time, *integrityRecord, error) {
	return c.readHeaderRecordWithKeyCheck(path, key, true)
}

func (c *cache) readHeaderRecordWithKeyCheck(path, key string, checkKey bool) (Header, time.Time, *integrityRecord, error) {
	f, err := c.fs.Open(path)
	if err != nil {
		if vfs.IsNotExist(err) {
			return Header{}, time.Time{}, nil, ErrNotFoundInCache
		}
		return Header{}, time.Time{}, nil, fmt.Errorf("failed to open header file %q for key %q: %w", path, key, err)
	}
	defer func() { _ = f.Close() }()

	// Read the record whole. Headers are small, and completeness has to be
	// judged before parsing: a partial record can parse into a plain error
	// (a cut timestamp reads as a malformed one) that is indistinguishable
	// from corruption once the bytes are gone.
	raw, err := io.ReadAll(f)
	if err != nil {
		return Header{}, time.Time{}, nil, fmt.Errorf("failed to read header file %q for key %q: %w", path, key, err)
	}

	// The header file is this entry's commit marker. Store writes the body in
	// full first and the header last, so a reader that finds a complete header
	// knows the body behind it is complete too.
	//
	// vfsWrite opens with O_CREATE|O_TRUNC, publishing the file before any
	// bytes are copied into it, and neither that copy nor an arbitrary VFS
	// backend's write is atomic. A concurrent Retrieve landing in that window
	// opens the body, which is already written, and then reads a header that
	// is empty or cut short anywhere.
	//
	// That is not corruption. The entry is not committed yet, and from a
	// reader's side it is indistinguishable from one that was never stored, so
	// report the miss that callers already handle. Reporting an error here
	// surfaced to the client as a failed response for a perfectly cacheable
	// resource whenever two requests for the same uncached URL overlapped,
	// which is the ordinary case for a shared cache.
	if !bytes.HasSuffix(raw, []byte(headerRecordTerminator)) {
		return Header{}, time.Time{}, nil, ErrNotFoundInCache
	}

	var meta *integrityRecord
	if checkKey {
		meta, err = decodeIntegrity(raw, key, c.config.Integrity != IntegrityDisabled)
	} else {
		meta, err = decodeIntegrityRecord(raw, key, c.config.Integrity != IntegrityDisabled, false)
	}
	if err != nil {
		return Header{}, time.Time{}, nil, err
	}
	h, storedAt, err := readStoredHeaders(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		// The record is terminated, so this is a complete header that does not
		// parse: corruption, not a write in flight.
		if meta != nil {
			return Header{}, time.Time{}, nil, &corruptEntryError{fingerprint: meta.fingerprint, reason: "invalid HTTP header record"}
		}
		return Header{}, time.Time{}, nil, fmt.Errorf("failed to read headers from %q for key %q: %w", path, key, err)
	}

	return h, storedAt, meta, nil
}

// Store a resource against a number of keys. Resource bodies are streamed
// directly into the backing VFS. For multiple keys (for example a primary and
// Vary key), the seekable resource is rewound instead of being copied into an
// unbounded in-memory buffer.
func (c *cache) Store(res *Resource, keys ...string) error {
	return c.StoreContext(context.Background(), res, keys...)
}

// StoreContext is Store with cooperative cancellation between streamed writes.
func (c *cache) StoreContext(ctx context.Context, res *Resource, keys ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}

	// Make the stale check and every write one ordered operation relative to
	// Invalidate. Checking a marker and then releasing the lock before body or
	// header I/O left a window in which a mutation could land, after which the
	// older response was still written and tracked as newer.
	c.generationMu.RLock()
	defer c.generationMu.RUnlock()

	// Lock every target in a stable order, including keys not yet staged. A
	// failed transaction cannot remove a newer Store/Freshen generation, and
	// overlapping transactions cannot deadlock with reversed key lists.
	keys, unlock := c.lockStoreKeys(keys)
	defer unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.responseSuperseded(res, keys) {
		return nil
	}

	prepared := make([]*preparedCacheEntry, len(keys))
	for i, key := range keys {
		prepared[i] = &preparedCacheEntry{key: key, hashedKey: hashKey(key), storedAt: Clock()}
		// Protect all old variants before admission can evict any of them.
		c.beginReservation(prepared[i].hashedKey)
		defer c.releaseReservation(prepared[i].hashedKey)
	}

	// Generic VFS backends cannot rename staged files. Hide their in-place
	// multi-key writes from readers until either every key succeeds or every
	// affected key is discarded. Native disk staging remains concurrent.
	locked := c.diskRoot == "" && len(keys) > 1
	if locked {
		c.publishMu.Lock()
	}
	defer func() {
		if locked {
			c.publishMu.Unlock()
		}
	}()
	fail := func(err error) error {
		if !locked {
			c.publishMu.Lock()
			locked = true
		}
		return c.discardPreparedStore(err, prepared)
	}

	files, err := c.prepareStoreEntries(ctx, res, prepared)
	if err != nil {
		return fail(err)
	}
	if !locked {
		c.publishMu.Lock()
		locked = true
	}
	// Cancellation may arrive while waiting for another publication.
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := c.commitStagedLocked(files...); err != nil {
		return fail(err)
	}
	for _, entry := range prepared {
		c.trackEntry(entry.key, entry.hashedKey, entry.size, entry.storedAt)
	}
	return nil
}

// prepareStoreEntries stages each target without exposing any disk generation.
// Publication and rollback remain owned by StoreContext.
func (c *cache) prepareStoreEntries(ctx context.Context, res *Resource, entries []*preparedCacheEntry) ([]*stagedEntry, error) {
	expectedSize, hasExpectedSize := expectedBodySize(res.Header())
	var files []*stagedEntry
	for _, entry := range entries {
		if err := c.prepareResourceForKey(ctx, res, entry, expectedSize, hasExpectedSize); err != nil {
			return nil, err
		}
		files = append(files, entry.body, entry.header)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return files, nil
}

// lockStoreKeys deduplicates target hashes while preserving the caller's key
// order for staging. Only lock acquisition is sorted.
func (c *cache) lockStoreKeys(keys []string) ([]string, func()) {
	unique := make([]string, 0, len(keys))
	hashes := make([]string, 0, len(keys))
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		hashed := hashKey(key)
		if !seen[hashed] {
			seen[hashed] = true
			unique = append(unique, key)
			hashes = append(hashes, hashed)
		}
	}
	sort.Strings(hashes)
	unlocks := make([]func(), 0, len(hashes))
	for _, hashed := range hashes {
		unlocks = append(unlocks, c.lockKeyWriter(hashed))
	}
	return unique, func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
}

// preparedCacheEntry retains admission and staging state until all keys can
// be published together. Its size also accounts for failed in-place writes.
type preparedCacheEntry struct {
	key, hashedKey string
	storedAt       time.Time
	body, header   *stagedEntry
	size           int64
}

// discardPreparedStore requires all target writer locks and publishMu. Disk
// staging failures leave the old generation intact. If a VFS write or rename
// has published anything, discard all transaction keys, including untouched
// old variants, before readers or newer writers can reach a mixed generation.
func (c *cache) discardPreparedStore(err error, entries []*preparedCacheEntry) error {
	published := false
	for _, entry := range entries {
		published = published || anyPublished(entry.body, entry.header)
		c.abandonStaged(entry.body, entry.header)
	}
	if !published {
		return err
	}
	for _, entry := range entries {
		if discardErr := c.discardUnadmitted(entry.key, entry.hashedKey, entry.size, entry.storedAt); discardErr != nil {
			// A backend refusing removal must not serve its residual generation
			// as fresh. Preserve a concurrent Invalidate barrier if present.
			c.staleMutex.Lock()
			if now := Clock(); c.stale[entry.key].Before(now) {
				c.stale[entry.key] = now
			}
			c.persistStale(c.snapshotStaleLocked())
			c.staleMutex.Unlock()
			err = errors.Join(err, fmt.Errorf("discard entry after failed store for key %q: %w", entry.key, discardErr))
		}
	}
	return err
}

// responseSuperseded checks every governing key before any entry is changed.
// A multi-key store is all-or-nothing with respect to invalidation. Vary
// responses contain the governing base key followed by a variant key. If
// the base marker supersedes the response, continuing to the unmarked
// variant publishes the same pre-mutation body under that second key.
// The caller must hold generationMu throughout this check and every write.
func (c *cache) responseSuperseded(res *Resource, keys []string) bool {
	for _, key := range keys {
		if staleTime, marked := c.StaleAt(key); marked && !res.ReceivedAfter(staleTime) {
			debugf("not storing %q: response superseded by the invalidation at %s", keys, staleTime)
			return true
		}
	}
	return false
}

func expectedBodySize(h http.Header) (int64, bool) {
	length, err := strconv.ParseInt(h.Get("Content-Length"), 10, 64)
	if err != nil || length < 0 {
		return 0, false
	}
	return length, true
}

// prepareResourceForKey stages and admits one entry without publishing it.
// The caller holds generationMu and every transaction target's writer lock.
func (c *cache) prepareResourceForKey(ctx context.Context, res *Resource, entry *preparedCacheEntry, expectedSize int64, hasExpectedSize bool) error {
	key, hashedKey := entry.key, entry.hashedKey
	if _, err := res.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("failed to rewind resource for key %q: %w", key, err)
	}

	// The stale marker is deliberately LEFT IN PLACE.
	//
	// Retrieve already compares a stored response's Date against the
	// marker, so a response written after the invalidation supersedes it
	// without the marker having to be deleted. Deleting it here was
	// actively wrong twice over: a store that was already in flight when a
	// mutation completed erased the newer invalidation and republished the
	// pre-mutation response as fresh, and refetching ONE Vary variant
	// cleared the base marker that every OTHER variant is still judged
	// against. The cleanup goroutine expires markers on its own.
	storedAt := entry.storedAt
	headerData, err := c.serializeCacheHeader(res.Status(), res.Header(), storedAt, key, expectedSize, strings.Repeat("0", 64))
	if err != nil {
		return fmt.Errorf("failed to serialize headers for key %q: %w", key, err)
	}

	// If Content-Length is known, make room before the write. Unknown-length
	// streams are accounted for and evicted immediately after the write. The
	// persisted header is part of the admission size too; excluding it lets
	// otherwise equal replacements drift beyond MaxSize.
	if hasExpectedSize {
		if err := c.reserveEntry(hashedKey, expectedSize+int64(len(headerData))); err != nil {
			return fmt.Errorf("failed to make room for key %q: %w", key, err)
		}
	}

	var body io.Reader = res
	if hasExpectedSize {
		body = io.LimitReader(res, expectedSize)
	}
	// Body and header are written first and published together further
	// down. Until that commit the entry already on disk is untouched, so
	// every failure here just drops the staged files. On a backend that
	// writes in place there is nothing to stage, and the existing discard
	// path still applies -- anyPublished says which case this is.
	digest := sha256.New()
	if c.config.Integrity != IntegrityDisabled {
		body = io.TeeReader(body, digest)
	}
	bodyStaged, written, err := c.stageBodyContext(ctx, body, key)
	entry.body = bodyStaged
	entry.size = written + int64(len(headerData))
	if err != nil {
		return err
	}
	if hasExpectedSize && written != expectedSize {
		return fmt.Errorf("resource body for key %q was %d bytes, expected %d", key, written, expectedSize)
	}
	headerData, err = c.serializeCacheHeader(res.Status(), res.Header(), storedAt, key, written, fmt.Sprintf("%x", digest.Sum(nil)))
	if err != nil {
		return err
	}
	if err := c.reserveEntry(hashedKey, written+int64(len(headerData))); err != nil {
		return fmt.Errorf("failed to make room for key %q: %w", key, err)
	}

	headerStaged, headerBytes, err := c.stageSerializedHeaderContext(ctx, headerData, key)
	entry.header = headerStaged
	entry.size = written + headerBytes
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return err
	}

	return nil
}

func (c *cache) stageBody(r io.Reader, key string) (*stagedEntry, int64, error) {
	return c.stageBodyContext(context.Background(), r, key)
}

func (c *cache) stageBodyContext(ctx context.Context, r io.Reader, key string) (*stagedEntry, int64, error) {
	e, n, err := c.stageWriteContext(ctx, bodyPrefix+formatPrefix+hashKey(key), r)
	if err != nil {
		return e, n, fmt.Errorf("failed to store body for key %q: %w", key, err)
	}
	return e, n, nil
}

func serializeStoredHeader(code int, h http.Header, storedAt time.Time) ([]byte, error) {
	hb := &bytes.Buffer{}
	fmt.Fprintf(hb, "%s%s\r\n", storedAtPreamble, storedAt.UTC().Format(time.RFC3339Nano))
	fmt.Fprintf(hb, "HTTP/1.1 %d %s\r\n", code, http.StatusText(code))
	if err := headersToWriter(h, hb); err != nil {
		return nil, err
	}
	return hb.Bytes(), nil
}

func (c *cache) stageSerializedHeader(headerData []byte, key string) (*stagedEntry, int64, error) {
	return c.stageSerializedHeaderContext(context.Background(), headerData, key)
}

func (c *cache) stageSerializedHeaderContext(ctx context.Context, headerData []byte, key string) (*stagedEntry, int64, error) {
	e, n, err := c.stageWriteContext(ctx, headerPrefix+formatPrefix+hashKey(key), bytes.NewReader(headerData))
	if err != nil {
		return e, n, fmt.Errorf("failed to store header for key %q: %w", key, err)
	}
	return e, n, nil
}

// Retrieve returns a cached Resource for the given key
func (c *cache) Retrieve(key string) (*Resource, error) {
	return c.retrieveRecorded(key, true)
}

// retrieveUncounted performs a coordinator's internal recheck without counting
// another public cache access. Verification, corruption cleanup and LRU updates
// are identical to Retrieve.
func (c *cache) retrieveUncounted(key string) (*Resource, error) {
	return c.retrieveRecorded(key, false)
}

func (c *cache) retrieveRecorded(key string, record bool) (*Resource, error) {
	res, err := c.retrieve(key, record)
	return res, c.integrityResult(key, err, record)
}

func (c *cache) retrieve(key string, record bool) (*Resource, error) {
	// Cleanup removes an expired entry and its invalidation marker as one
	// generation transition. Holding the read side from file open through the
	// marker check means a retrieval either observes the old file WITH its
	// marker, or starts after cleanup and cannot open the file at all.
	c.cleanupMu.RLock()
	defer c.cleanupMu.RUnlock()

	// Hold publication still across both lookups. Opening the body and then
	// reading the header are two steps, and a commit landing between them
	// would hand back one version's payload with the other's status and
	// headers -- a Content-Length or Content-Encoding from a body that is no
	// longer there.
	c.publishMu.RLock()
	defer c.publishMu.RUnlock()

	hashedKey := hashKey(key)
	bodyPath := bodyPrefix + formatPrefix + hashedKey
	f, err := c.fs.Open(bodyPath)
	if err != nil {
		if vfs.IsNotExist(err) {
			if record {
				c.recordMiss()
			}
			return nil, ErrNotFoundInCache
		}
		return nil, fmt.Errorf("failed to open body file %q for key %q: %w", bodyPath, key, err)
	}
	h, persistedAt, meta, err := c.readHeaderRecord(headerPrefix+formatPrefix+hashedKey, key)
	if err != nil {
		_ = f.Close()
		if err == ErrNotFoundInCache {
			if record {
				c.recordMiss()
			}
			return nil, ErrNotFoundInCache
		}
		return nil, fmt.Errorf("failed to retrieve header for key %q: %w", key, err)
	}
	if meta != nil {
		if err := verifyStoredBody(f, meta, meta.fingerprint); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	res := NewResource(h.StatusCode, f, h.Header)
	if persistedAt.IsZero() {
		persistedAt = c.entryStoredAt(hashedKey)
	}
	res.SetStoredAt(persistedAt)
	res.storedKey = key

	// Check stale map with proper locking
	c.staleMutex.RLock()
	staleTime, exists := c.stale[key]
	c.staleMutex.RUnlock()

	if exists {
		if !res.StoredAfter(staleTime) {
			debugf("stale marker of %s found", staleTime)
			res.MarkStale()
		}
	}

	// Update LRU access time
	c.touchEntry(hashedKey)
	if record {
		c.recordHit()
	}

	return res, nil
}

func (c *cache) Invalidate(keys ...string) {
	debugf("invalidating %q", keys)
	if len(keys) == 0 {
		return
	}

	// Publish and persist an immediately visible barrier BEFORE waiting for
	// active stores. Retrieve does not take generationMu, so acquiring its write
	// side first left a successful mutation invisible for the entire duration of
	// an unrelated slow cache write. The far-future value conservatively marks
	// every currently writable generation stale; if the process dies while
	// waiting, loadStale recovers it to the restart time, after all writes that
	// could have survived that process.
	c.staleMutex.Lock()
	for _, key := range keys {
		c.stale[key] = invalidationBarrierTime
	}
	c.persistStale(c.snapshotStaleLocked())
	c.staleMutex.Unlock()

	// Existing stores may already have passed their stale check. Wait until
	// their stored timestamps are fixed, while the barrier keeps lookups from
	// treating those later file writes as post-mutation responses.
	c.generationMu.Lock()
	defer c.generationMu.Unlock()

	c.staleMutex.Lock()
	defer c.staleMutex.Unlock()
	now := Clock()
	for _, key := range keys {
		c.stale[key] = now
	}
	snapshot := c.snapshotStaleLocked()

	// Written through to the backing store, so a restart does not lose it.
	//
	// The markers are the ONLY record that the entries still on disk predate
	// a mutation, and scanExistingCache restores those entries. Keeping the
	// map in memory alone meant every deploy or crash republished the
	// pre-mutation representation -- Vary variants included -- as a fresh HIT.
	// Keep staleMutex held through persistence. Otherwise two invalidations
	// can snapshot in the right order but finish their writes in the opposite
	// order, allowing the older snapshot to erase the newer marker (or two
	// O_TRUNC writes to corrupt the file).
	c.persistStale(snapshot)
}

// staleMapPath is the atomic OS-backed snapshot and the legacy generic-VFS
// snapshot path. The other entries live under hashed-key prefixes, so it
// cannot collide with one.
const staleMapPath = "stale-markers.json"

// invalidationBarrierTime is persisted while Invalidate waits for writers
// that were already active. It must be JSON/RFC3339 representable.
var invalidationBarrierTime = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)

// staleSnapshot is the bounded two-slot journal used when all that is known
// about a caller-provided VFS is the small vfs.VFS interface. That interface
// has no Rename or Sync operation, so rewriting one snapshot in place cannot
// be made failure-safe. Each generation overwrites the older slot while the
// other slot remains intact; startup selects the highest complete generation.
type staleSnapshot struct {
	Generation uint64               `json:"generation"`
	Markers    map[string]time.Time `json:"markers"`
}

func staleMapSlotPath(generation uint64) string {
	return "stale-markers." + strconv.FormatUint(generation%2, 10) + ".json"
}

// snapshotStaleLocked copies the marker map. Callers hold staleMutex.
func (c *cache) snapshotStaleLocked() map[string]time.Time {
	out := make(map[string]time.Time, len(c.stale))
	for k, v := range c.stale {
		out[k] = v
	}
	return out
}

// persistStale writes the markers to the backing store. Callers hold
// staleMutex across this call, which serialises complete snapshots through
// their file write instead of merely serialising map access.
//
// Failures are logged, not returned: the in-memory markers are already
// correct, so the mutation this accompanies has still been honoured for the
// life of the process. What is lost is only the restart guarantee.
func (c *cache) persistStale(snapshot map[string]time.Time) {
	persisted := interface{}(snapshot)
	nextGeneration := c.staleGeneration
	if c.diskRoot == "" {
		nextGeneration++
		persisted = staleSnapshot{Generation: nextGeneration, Markers: snapshot}
	}

	encoded, err := json.Marshal(persisted)
	if err != nil {
		debugf("failed to encode invalidation markers: %v", err)
		return
	}
	var writeErr error
	if c.diskRoot != "" {
		_, writeErr = atomicWriteFile(filepath.Join(c.diskRoot, filepath.FromSlash(staleMapPath)), bytes.NewReader(encoded))
	} else {
		_, _, writeErr = c.vfsWrite(staleMapSlotPath(nextGeneration), bytes.NewReader(encoded))
	}
	if writeErr != nil {
		debugf("failed to persist invalidation markers: %v", writeErr)
		return
	}
	if c.diskRoot == "" {
		c.staleGeneration = nextGeneration
	}
}

// loadStale restores markers written by a previous process.
func (c *cache) loadStale() {
	// Caller-provided persistent VFS backends use alternating snapshots. A
	// malformed newest slot (for example after a short write) is ignored and
	// the other complete generation remains authoritative.
	if c.diskRoot == "" {
		var best staleSnapshot
		found := false
		for slot := uint64(0); slot < 2; slot++ {
			var candidate staleSnapshot
			if err := c.readStaleJSON(staleMapSlotPath(slot), &candidate); err != nil {
				if !vfs.IsNotExist(err) {
					debugf("failed to read invalidation marker slot %d: %v", slot, err)
				}
				continue
			}
			if candidate.Generation == 0 || candidate.Markers == nil {
				debugf("ignoring invalid invalidation marker slot %d", slot)
				continue
			}
			if !found || candidate.Generation > best.Generation {
				best = candidate
				found = true
			}
		}
		if found {
			c.staleGeneration = best.Generation
			if c.restoreStale(best.Markers) {
				c.persistRestoredStale()
			}
			return
		}
	}

	// Legacy generic-VFS snapshots and current OS-backed snapshots use the raw
	// marker map. Once a generic VFS writes a journal slot, the legacy file is
	// deliberately ignored so an older snapshot cannot resurrect swept keys.
	var restored map[string]time.Time
	if err := c.readStaleJSON(staleMapPath, &restored); err != nil {
		if !vfs.IsNotExist(err) {
			debugf("failed to read invalidation markers: %v", err)
		}
		return
	}
	if c.restoreStale(restored) {
		c.persistRestoredStale()
	}
}

func (c *cache) readStaleJSON(path string, dst interface{}) error {
	f, err := c.fs.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return json.NewDecoder(f).Decode(dst)
}

// restoreStale merges a snapshot and returns whether it recovered an
// invalidation that was interrupted while waiting for active writers.
func (c *cache) restoreStale(restored map[string]time.Time) bool {
	recovered := false
	recoveredAt := Clock()
	c.staleMutex.Lock()
	defer c.staleMutex.Unlock()
	for key, at := range restored {
		if at.Equal(invalidationBarrierTime) {
			at = recoveredAt
			recovered = true
		}
		// Never overwrite a marker this process has already written: it is
		// newer than anything on disk.
		if _, ok := c.stale[key]; !ok {
			c.stale[key] = at
		}
	}
	debugf("restored %d invalidation marker(s)", len(restored))
	return recovered
}

// persistRestoredStale replaces a recovered barrier with the restart time.
// No writer from the previous process can still be active, so that time is a
// final upper bound for every cache file that survived it.
func (c *cache) persistRestoredStale() {
	c.staleMutex.Lock()
	defer c.staleMutex.Unlock()
	c.persistStale(c.snapshotStaleLocked())
}

// StaleAt returns when key was invalidated, if it was.
//
// It lets the handler judge a Vary VARIANT against the base key's marker:
// variants live under their own keys, which invalidation cannot enumerate, so
// without this each one would have to be reached through a base entry that a
// single refetch makes fresh again.
func (c *cache) StaleAt(key string) (time.Time, bool) {
	c.staleMutex.RLock()
	defer c.staleMutex.RUnlock()
	t, ok := c.stale[key]
	return t, ok
}

// StaleSnapshot returns one atomic view of the requested invalidation
// markers. Handler validation uses it for a base key and its Vary key so an
// unsafe invalidation cannot be published between two independent StaleAt
// calls and escape the post-Freshen supersession check.
func (c *cache) StaleSnapshot(keys ...string) map[string]time.Time {
	c.staleMutex.RLock()
	defer c.staleMutex.RUnlock()

	snapshot := make(map[string]time.Time, len(keys))
	for _, key := range keys {
		if at, ok := c.stale[key]; ok {
			snapshot[key] = at
		}
	}
	return snapshot
}

func (c *cache) Freshen(res *Resource, keys ...string) error {
	var invalidate []string
	err := func() error {
		c.generationMu.RLock()
		defer c.generationMu.RUnlock()

		// Validation that started before a mutation must not freshen the old
		// body after that mutation. Validator records the precise local request
		// time on the Resource; older resources fall back to HTTP timestamps.
		for _, key := range keys {
			if staleTime, marked := c.StaleAt(key); marked && !res.ReceivedAfter(staleTime) {
				return fmt.Errorf("validation for key %q was superseded by invalidation at %s", key, staleTime)
			}
		}

		for _, key := range keys {
			if res.storedKey != "" && key != res.storedKey {
				continue
			}
			mismatch, err := c.freshenResourceForKey(res, key)
			if err != nil {
				return err
			}
			if mismatch {
				invalidate = append(invalidate, key)
			}
		}
		return nil
	}()
	if err != nil {
		return err
	}

	// Do not try to upgrade generationMu from a read lock inside the loop.
	// Invalidate takes its write side, so mismatched entries are handled only
	// after the ordered freshen operation has completed.
	if len(invalidate) > 0 {
		c.Invalidate(invalidate...)
	}
	return nil
}

// The writer lock covers both the generation/validator check and publication.
// A Store cannot replace the body between checking its header and updating it.
func (c *cache) freshenResourceForKey(res *Resource, key string) (bool, error) {
	hashedKey := hashKey(key)
	unlock := c.lockKeyWriter(hashedKey)
	defer unlock()
	c.beginReservation(hashedKey)
	defer c.releaseReservation(hashedKey)
	h, storedAt, meta, err := c.readHeaderRecord(headerPrefix+formatPrefix+hashedKey, key)
	if err != nil {
		// A newly fetched HEAD has nothing to freshen if no GET is stored.
		if errors.Is(err, ErrNotFoundInCache) && res.storedAt.IsZero() {
			return false, nil
		}
		return false, fmt.Errorf("failed to read freshening target %q: %w", key, err)
	}
	if storedAt.IsZero() {
		storedAt = c.entryStoredAt(hashedKey)
	}
	// A Resource retrieved before a replacement cannot refresh that newer
	// generation even if its weak validators or content length still match.
	// Base Vary entries can precede the retrieved variant, so only a newer
	// target supersedes it. Freshly fetched HEAD resources have no storedAt.
	if !res.storedAt.IsZero() && storedAt.After(res.storedAt) {
		return false, fmt.Errorf("freshening target %q was replaced after retrieval", key)
	}
	if h.StatusCode != res.Status() || !headersEqual(h.Header, res.Header()) {
		return true, nil
	}
	bodyInfo, err := c.fs.Stat(bodyPrefix + formatPrefix + hashedKey)
	if err != nil {
		return false, fmt.Errorf("failed to size freshening target %q: %w", key, err)
	}
	freshenedAt := Clock()
	headerData, err := serializeStoredHeader(h.StatusCode, res.Header(), freshenedAt)
	if meta != nil {
		meta.StoredAt = freshenedAt
		headerData, err = serializeIntegrityHeader(h.StatusCode, res.Header(), *meta)
	}
	if err != nil {
		return false, fmt.Errorf("failed to serialize freshening target %q: %w", key, err)
	}
	newSize := bodyInfo.Size() + int64(len(headerData))
	if err := c.reserveEntry(hashedKey, newSize); err != nil {
		return false, fmt.Errorf("failed to admit freshening target %q: %w", key, err)
	}
	return false, c.commitFreshenedHeader(key, hashedKey, headerData, newSize, freshenedAt)
}

// Called while the key writer lock and capacity reservation remain held.
func (c *cache) commitFreshenedHeader(key, hashedKey string, headerData []byte, newSize int64, freshenedAt time.Time) error {
	header, _, err := c.stageSerializedHeader(headerData, key)
	if err == nil {
		err = c.commitStaged(header)
	}
	if err != nil {
		c.abandonStaged(header)
		if anyPublished(header) {
			if discardErr := c.discardUnadmitted(key, hashedKey, newSize, freshenedAt); discardErr != nil {
				err = errors.Join(err, discardErr)
			}
		}
		return fmt.Errorf("failed to freshen header for key %q: %w", key, err)
	}
	// Header growth consumes real capacity too. Convert this reservation and
	// advance its persisted generation together in the LRU ledger.
	c.trackEntry(key, hashedKey, newSize, freshenedAt)
	return nil
}

func hashKey(key string) string {
	h := hash64Pool.Get().(hash.Hash64)
	defer func() {
		h.Reset()
		hash64Pool.Put(h)
	}()
	if _, err := h.Write([]byte(key)); err != nil {
		return "unable-to-calculate"
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func readStoredHeaders(r *bufio.Reader) (Header, time.Time, error) {
	tp := textproto.NewReader(r)
	line, err := tp.ReadLine()
	if err != nil {
		return Header{}, time.Time{}, err
	}

	var storedAt time.Time
	if strings.HasPrefix(line, storedAtPreamble) {
		storedAt, err = time.Parse(time.RFC3339Nano, strings.TrimPrefix(line, storedAtPreamble))
		if err != nil {
			return Header{}, time.Time{}, fmt.Errorf("malformed cache timestamp: %w", err)
		}
		line, err = tp.ReadLine()
		if err != nil {
			return Header{}, time.Time{}, err
		}
	}

	if strings.HasPrefix(line, integrityPreamble) {
		meta, parseErr := parseIntegrityPreamble(line)
		if parseErr != nil {
			return Header{}, time.Time{}, parseErr
		}
		storedAt = meta.StoredAt
		line, err = tp.ReadLine()
		if err != nil {
			return Header{}, time.Time{}, err
		}
	}

	f := strings.SplitN(line, " ", 3)
	if len(f) < 2 {
		return Header{}, time.Time{}, fmt.Errorf("malformed HTTP response: %s", line)
	}
	statusCode, err := strconv.Atoi(f[1])
	if err != nil {
		return Header{}, time.Time{}, fmt.Errorf("malformed HTTP status code: %s", f[1])
	}

	mimeHeader, err := tp.ReadMIMEHeader()
	if err != nil {
		return Header{}, time.Time{}, err
	}
	return Header{StatusCode: statusCode, Header: http.Header(mimeHeader)}, storedAt, nil
}

func readHeaders(r *bufio.Reader) (Header, error) {
	h, _, err := readStoredHeaders(r)
	return h, err
}

func headersToWriter(h http.Header, w io.Writer) error {
	if err := h.Write(w); err != nil {
		return err
	}
	// ReadMIMEHeader expects a trailing newline
	_, err := w.Write([]byte("\r\n"))
	return err
}

// LRU tracking methods

// trackEntry adds or updates an entry in the LRU index
func (c *cache) trackEntry(key, hashedKey string, size int64, storedAt time.Time) {
	c.lruMutex.Lock()
	defer c.lruMutex.Unlock()
	c.releaseReservationLocked(hashedKey)

	if storedAt.IsZero() {
		storedAt = Clock()
	}

	// Check if entry already exists
	if entry, exists := c.lruIndex[hashedKey]; exists {
		// Update existing entry
		c.totalSize -= entry.size
		entry.size = size
		entry.storedAt = storedAt
		entry.accessedAt = storedAt
		c.totalSize += size
		// Move to front of LRU list
		c.lruList.MoveToFront(entry.element)
	} else {
		// Create new entry
		entry := &cacheEntry{
			key:        key,
			hashedKey:  hashedKey,
			size:       size,
			storedAt:   storedAt,
			accessedAt: storedAt,
		}
		entry.element = c.lruList.PushFront(entry)
		c.lruIndex[hashedKey] = entry
		c.totalSize += size
	}
}

// entryStoredAt reports when this cache last WROTE the entry, at full
// precision. Zero when the entry is not tracked.
func (c *cache) entryStoredAt(hashedKey string) time.Time {
	c.lruMutex.Lock()
	defer c.lruMutex.Unlock()

	if entry, exists := c.lruIndex[hashedKey]; exists {
		return entry.storedAt
	}
	return time.Time{}
}

// touchEntry updates the access time and moves entry to front of LRU list
func (c *cache) touchEntry(hashedKey string) {
	c.lruMutex.Lock()
	defer c.lruMutex.Unlock()

	if entry, exists := c.lruIndex[hashedKey]; exists {
		entry.accessedAt = Clock()
		c.lruList.MoveToFront(entry.element)
	}
}

// discardUnadmitted removes every on-disk part and any old LRU record for a
// key whose body was already overwritten but whose replacement could not be
// committed. If the backing store refuses removal, the residual is retained
// in the LRU/size ledger so cleanup retries it and repeated rejected writes
// cannot grow the backing store invisibly.
func (c *cache) discardUnadmitted(key, hashedKey string, residualSize int64, storedAt time.Time) error {
	c.lruMutex.Lock()
	defer c.lruMutex.Unlock()

	if entry, exists := c.lruIndex[hashedKey]; exists {
		removeErr := c.removeEntry(entry)
		if removeErr != nil && residualSize > entry.size {
			c.totalSize += residualSize - entry.size
			entry.size = residualSize
		}
		return removeErr
	}

	var removeErr error
	for _, path := range []string{
		bodyPrefix + formatPrefix + hashedKey,
		headerPrefix + formatPrefix + hashedKey,
	} {
		if err := c.fs.Remove(path); err != nil && !vfs.IsNotExist(err) {
			removeErr = errors.Join(removeErr, fmt.Errorf("remove cache file %s: %w", path, err))
		}
	}
	if removeErr != nil {
		if residualSize <= 0 {
			residualSize = 1
		}
		if storedAt.IsZero() {
			storedAt = Clock()
		}
		entry := &cacheEntry{
			key:        key,
			hashedKey:  hashedKey,
			size:       residualSize,
			storedAt:   storedAt,
			accessedAt: storedAt,
		}
		entry.element = c.lruList.PushFront(entry)
		c.lruIndex[hashedKey] = entry
		c.totalSize += residualSize
	}
	return removeErr
}

// removeEntry removes an entry from the cache and LRU tracking
func (c *cache) removeEntry(entry *cacheEntry) error {
	// Remove from filesystem
	bodyPath := bodyPrefix + formatPrefix + entry.hashedKey
	headerPath := headerPrefix + formatPrefix + entry.hashedKey

	var removeErr error
	if err := c.fs.Remove(bodyPath); err != nil && !vfs.IsNotExist(err) {
		debugf("failed to remove body file %s: %v", bodyPath, err)
		removeErr = errors.Join(removeErr, fmt.Errorf("remove body file %s: %w", bodyPath, err))
	}
	if err := c.fs.Remove(headerPath); err != nil && !vfs.IsNotExist(err) {
		debugf("failed to remove header file %s: %v", headerPath, err)
		removeErr = errors.Join(removeErr, fmt.Errorf("remove header file %s: %w", headerPath, err))
	}
	if removeErr != nil {
		// Keep the LRU record so a later cleanup retries the residual files.
		// Dropping the record now would let marker cleanup assume the backing
		// entry is gone and republish it as fresh after a restart.
		return removeErr
	}

	// Remove from LRU tracking (assumes lruMutex is already held)
	c.lruList.Remove(entry.element)
	delete(c.lruIndex, entry.hashedKey)
	c.totalSize -= entry.size

	// The invalidation marker is deliberately NOT removed with the entry.
	//
	// It governs more than this entry: a base key's marker is what every Vary
	// VARIANT of that resource is judged against, and variants are tracked and
	// evicted independently. Dropping it when LRU eviction happened to take
	// the base entry left the surviving pre-mutation variants with nothing
	// marking them stale, so a refetch could publish a new base and a
	// concurrent lookup reach an old variant through it as a fresh HIT.
	//
	// cleanupStaleMap frees markers by AGE instead -- see staleRetention --
	// which is the only bound that knows how long an entry it governs can
	// still be around.

	return nil
}

func (c *cache) beginReservation(hashedKey string) {
	c.lruMutex.Lock()
	defer c.lruMutex.Unlock()
	if c.reservations == nil {
		c.reservations = make(map[string]int64)
	}
	c.reservations[hashedKey] = 0
}

func (c *cache) reserveEntry(hashedKey string, itemSize int64) error {
	c.lruMutex.Lock()
	defer c.lruMutex.Unlock()
	if err := c.evictForAdmissionLocked(hashedKey, itemSize); err != nil {
		return err
	}
	existingSize := int64(0)
	if entry := c.lruIndex[hashedKey]; entry != nil {
		existingSize = entry.size
	}
	growth := max(itemSize-existingSize, 0)
	c.reservedSize += growth - c.reservations[hashedKey]
	c.reservations[hashedKey] = growth
	return nil
}

func (c *cache) releaseReservation(hashedKey string) {
	c.lruMutex.Lock()
	defer c.lruMutex.Unlock()
	c.releaseReservationLocked(hashedKey)
}

func (c *cache) releaseReservationLocked(hashedKey string) {
	c.reservedSize -= c.reservations[hashedKey]
	delete(c.reservations, hashedKey)
}

// The caller holds lruMutex while checking committed bytes and all other
// writers' reserved growth. Active keys cannot be evicted to fund admission.
func (c *cache) evictForAdmissionLocked(hashedKey string, itemSize int64) error {
	if itemSize < 0 {
		return fmt.Errorf("cache entry size overflow")
	}
	if c.config.MaxSize <= 0 {
		return nil
	}
	if itemSize > c.config.MaxSize {
		return fmt.Errorf("item size %d exceeds cache size limit %d", itemSize, c.config.MaxSize)
	}

	// Replacing a key reclaims its old tracked size when trackEntry commits the
	// new value. Protect that entry from eviction (important for an
	// unknown-length body that has already overwritten its backing file) and
	// reserve only the net space the replacement needs.
	existingSize := int64(0)
	if existing, ok := c.lruIndex[hashedKey]; ok {
		existingSize = existing.size
	}
	otherReserved := c.reservedSize - c.reservations[hashedKey]
	// This operation may reclaim its own replacement's old bytes. Other
	// admissions receive no credit for a pending shrink until it commits.
	targetSize := c.config.MaxSize - otherReserved - itemSize + existingSize

	var removeErr error
	// Visit each entry at most once, from least to most recently used. A
	// failed victim stays tracked for a later retry, but must not prevent us
	// from reclaiming other removable entries during this admission attempt.
	for elem := c.lruList.Back(); c.totalSize > targetSize && elem != nil; {
		prev := elem.Prev()
		entry := elem.Value.(*cacheEntry)
		_, active := c.reservations[entry.hashedKey]
		if entry.hashedKey == hashedKey || active {
			elem = prev
			continue
		}
		debugf("evicting LRU entry: %s (size: %d, accessed: %s)",
			entry.key, entry.size, entry.accessedAt.Format(time.RFC3339))
		if err := c.removeEntry(entry); err != nil {
			removeErr = errors.Join(removeErr, err)
			elem = prev
			continue
		}

		// Record eviction metric
		getDefaultMetrics().RecordCacheEviction("lru")
		elem = prev
	}

	if c.totalSize > targetSize {
		err := fmt.Errorf("cache has %d bytes but must shrink to %d bytes before admission", c.totalSize, targetSize)
		if removeErr != nil {
			return errors.Join(err, removeErr)
		}
		return err
	}
	return nil
}

// Cleanup methods

// cleanupLoop runs periodic cleanup
func (c *cache) cleanupLoop() {
	ticker := time.NewTicker(c.config.CleanupInterval)
	defer ticker.Stop()
	defer close(c.cleanupDone)

	for {
		select {
		case <-ticker.C:
			result := c.Cleanup()
			if result.RemovedItems > 0 || result.RemovedStaleEntries > 0 {
				debugf("cleanup completed: removed %d items (%d bytes), %d stale entries in %s",
					result.RemovedItems, result.RemovedBytes, result.RemovedStaleEntries, result.Duration)
			}
		case <-c.stopChan:
			return
		}
	}
}

// Cleanup runs a manual cleanup cycle
func (c *cache) Cleanup() CleanupResult {
	start := Clock()
	result := CleanupResult{}

	// Remove TTL-expired entries before their invalidation markers. The marker
	// is the only durable evidence that an older file is stale; publishing its
	// removal first creates a fresh-read window and lets a crash restore the
	// old file without its marker. Use one cutoff instant for both sweeps so a
	// boundary entry cannot fall between them as cleanup runs.
	c.cleanupMu.Lock()
	if c.config.TTL > 0 {
		removed, bytes, complete := c.cleanupTTLExpired(start)
		result.RemovedItems += removed
		result.RemovedBytes += bytes
		if complete {
			result.RemovedStaleEntries = c.cleanupStaleMap(start)
		}
	} else {
		result.RemovedStaleEntries = c.cleanupStaleMap(start)
	}
	c.cleanupMu.Unlock()

	// Enforce size limit
	if c.config.MaxSize > 0 {
		removed, bytes := c.enforceMaxSize()
		result.RemovedItems += removed
		result.RemovedBytes += bytes
	}

	result.Duration = Clock().Sub(start)

	// Record cleanup duration and refresh the gauges from a fresh snapshot.
	getDefaultMetrics().RecordCleanupDuration(result.Duration.Seconds())
	getDefaultMetrics().UpdateCacheStats(c.Stats())

	return result
}

// staleRetention is how long an invalidation marker has to be kept.
//
// The marker is the ONLY record that entries older than it are pre-mutation,
// so dropping it while such an entry survives republishes that entry as a HIT.
// This age-based sweep is deliberately ordered after TTL eviction, because
// removeEntry cannot discard a base marker while older Vary variants may
// still depend on it.
//
// StaleMapTTL alone did not: its 24 hour default against the 7 day cache TTL
// left six days in which an infrequently requested representation -- a Vary
// variant especially, since those are judged against the BASE key's marker and
// may never be fetched in the meantime -- came back as fresh pre-mutation
// content.
//
// With TTL disabled an entry can survive indefinitely, so markers are then
// only removed with their entry, never by age. That trades unbounded growth of
// one time.Time per invalidated key against serving content a mutation
// already replaced.
func (c *cache) staleRetention() time.Duration {
	if c.config.TTL <= 0 {
		return 0
	}
	return max(c.config.StaleMapTTL, c.config.TTL)
}

// cleanupStaleMap removes old stale map entries
func (c *cache) cleanupStaleMap(now time.Time) int {
	retention := c.staleRetention()
	if retention <= 0 {
		return 0
	}

	c.staleMutex.Lock()
	defer c.staleMutex.Unlock()

	cutoff := now.Add(-retention)
	removed := 0

	for key, staleTime := range c.stale {
		if staleTime.Before(cutoff) {
			delete(c.stale, key)
			removed++
		}
	}

	if removed > 0 {
		// Written through, like Invalidate: otherwise a restart would restore
		// markers this sweep has just decided are no longer needed. Persistence
		// happens while staleMutex is held, so a newer snapshot cannot be
		// overtaken by this older one.
		snapshot := c.snapshotStaleLocked()
		c.persistStale(snapshot)
	}

	return removed
}

// cleanupTTLExpired removes items that have exceeded their TTL
func (c *cache) cleanupTTLExpired(now time.Time) (int, int64, bool) {
	c.lruMutex.Lock()
	defer c.lruMutex.Unlock()

	cutoff := now.Add(-c.config.TTL)
	removed := 0
	var bytesRemoved int64
	complete := true

	// Iterate from back (oldest) to front
	for elem := c.lruList.Back(); elem != nil; {
		entry := elem.Value.(*cacheEntry)
		prev := elem.Prev()

		if entry.storedAt.Before(cutoff) {
			if _, active := c.reservations[entry.hashedKey]; active {
				complete = false
				elem = prev
				continue
			}
			debugf("removing TTL-expired entry: %s (stored: %s)",
				entry.key, entry.storedAt.Format(time.RFC3339))
			if err := c.removeEntry(entry); err != nil {
				complete = false
			} else {
				bytesRemoved += entry.size
				removed++

				// Record eviction metric
				getDefaultMetrics().RecordCacheEviction("ttl")
			}
		}

		elem = prev
	}

	return removed, bytesRemoved, complete
}

// enforceMaxSize ensures cache doesn't exceed max size
func (c *cache) enforceMaxSize() (int, int64) {
	c.lruMutex.Lock()
	defer c.lruMutex.Unlock()

	removed := 0
	var bytesRemoved int64

	// A single undeletable oldest entry must not disable cleanup of every
	// newer entry. Visit each candidate once and retain failed entries for a
	// future retry.
	for elem := c.lruList.Back(); c.totalSize > c.config.MaxSize && elem != nil; {
		prev := elem.Prev()
		entry := elem.Value.(*cacheEntry)
		if _, active := c.reservations[entry.hashedKey]; active {
			elem = prev
			continue
		}
		debugf("enforcing max size, removing: %s (size: %d)",
			entry.key, entry.size)
		if err := c.removeEntry(entry); err != nil {
			elem = prev
			continue
		}
		bytesRemoved += entry.size
		removed++

		// Record eviction metric
		getDefaultMetrics().RecordCacheEviction("size_limit")
		elem = prev
	}

	return removed, bytesRemoved
}

// Statistics methods

func (c *cache) recordHit() {
	c.statMutex.Lock()
	c.hitCount++
	c.statMutex.Unlock()
}

func (c *cache) recordMiss() {
	c.statMutex.Lock()
	c.missCount++
	c.statMutex.Unlock()
}

// Stats returns current cache statistics
func (c *cache) Stats() CacheStats {
	c.lruMutex.RLock()
	totalSize := c.totalSize
	itemCount := c.lruList.Len()
	c.lruMutex.RUnlock()

	c.staleMutex.RLock()
	staleCount := len(c.stale)
	c.staleMutex.RUnlock()

	c.statMutex.RLock()
	hitCount := c.hitCount
	missCount := c.missCount
	c.statMutex.RUnlock()

	return CacheStats{
		TotalSize:  totalSize,
		ItemCount:  itemCount,
		StaleCount: staleCount,
		HitCount:   hitCount,
		MissCount:  missCount,
	}
}

// Purge removes all cached items
func (c *cache) Purge() error {
	c.generationMu.Lock()
	defer c.generationMu.Unlock()
	c.cleanupMu.Lock()
	defer c.cleanupMu.Unlock()

	c.lruMutex.Lock()
	defer c.lruMutex.Unlock()

	// Remove all entries. If any backing file remains, retain every stale
	// marker: it is still needed to keep that residual entry stale on restart.
	var purgeErr error
	for elem := c.lruList.Front(); elem != nil; {
		entry := elem.Value.(*cacheEntry)
		next := elem.Next()
		if err := c.removeEntry(entry); err != nil {
			purgeErr = errors.Join(purgeErr, err)
		}
		elem = next
	}
	if purgeErr != nil {
		return purgeErr
	}

	// Clear the stale map in the backing store too. Persisting an empty newest
	// generation is safer than deleting journal slots one by one: a restart can
	// never select an older non-empty generation between those removals.
	c.staleMutex.Lock()
	defer c.staleMutex.Unlock()
	c.stale = make(map[string]time.Time)
	c.persistStale(c.snapshotStaleLocked())

	return nil
}

// Close stops the cache and cleanup goroutines.
// Safe to call multiple times; only the first call performs the shutdown.
func (c *cache) Close() error {
	c.closeOnce.Do(func() {
		c.stopped = true
		close(c.stopChan)
		// Closing stopChan only requests shutdown. Join the cleanup loop so a
		// caller may safely release shared dependencies (including Clock in
		// tests) when Close returns.
		<-c.cleanupDone
	})
	return nil
}

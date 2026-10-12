package httpcache

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These regression tests exercise a full 200 validation response,
// rather than the changed-Vary 304 rebuild already covered elsewhere.
type publicationRegressionStoreFailure struct {
	Cache
	reject atomic.Bool
}

func (c *publicationRegressionStoreFailure) Store(res *Resource, keys ...string) error {
	if c.reject.Load() {
		return errors.New("independent review: publication rejected")
	}
	return c.Cache.Store(res, keys...)
}

func publicationRegressionRequest(h *Handler, method, language string, force bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://cache.example/independent-policy-replacement", nil)
	req.Header.Set("Accept-Language", language)
	req.Header.Set("Accept-Encoding", "gzip")
	if force {
		req.Header.Set("Cache-Control", "min-fresh=7200")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	h.writes.Wait()
	return rec
}

func publicationRegressionAssertBody(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("response=%d %q, want 200 %q; headers=%v", rec.Code, rec.Body.String(), want, rec.Header())
	}
}

func TestValidationStoreFailureFencesPredecessorWithMaxStale(t *testing.T) {
	for _, directive := range []string{"", "max-stale", "max-stale=0"} {
		t.Run(directive, func(t *testing.T) { publicationRegressionPredecessor(t, directive) })
	}
}

func publicationRegressionPredecessor(t *testing.T, directive string) {
	t.Helper()
	inner := generationBudgetCache(t, false, 1<<20)
	cache := &publicationRegressionStoreFailure{Cache: inner}
	var changed atomic.Bool
	h := NewSharedHandler(cache, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := "v1"
		if changed.Load() {
			body = "v2"
		}
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("ETag", `"`+body+`"`)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(h.writes.Wait)
	publicationRegressionAssertBody(t, publicationRegressionRequest(h, http.MethodGet, "en", false), "v1")
	changed.Store(true)
	cache.reject.Store(true)
	publicationRegressionAssertBody(t, publicationRegressionRequest(h, http.MethodGet, "en", true), "v2")
	cache.reject.Store(false)
	req := httptest.NewRequest(http.MethodGet, "http://cache.example/independent-policy-replacement", nil)
	if directive != "" {
		req.Header.Set("Cache-Control", directive)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	h.writes.Wait()
	publicationRegressionAssertBody(t, rec, "v2")
}

func TestUnsafeInvalidationRequiresValidationDespiteMaxStale(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, directive := range []string{"max-stale", "max-stale=0"} {
			t.Run(stringBool(disk)+"/"+directive, func(t *testing.T) {
				publicationRegressionUnsafeInvalidation(t, disk, directive)
			})
		}
	}
}

func publicationRegressionUnsafeInvalidation(t *testing.T, disk bool, directive string) {
	t.Helper()
	cache := generationBudgetCache(t, disk, 1<<20)
	var changed atomic.Bool
	h := NewSharedHandler(cache, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			changed.Store(true)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		body := "v1"
		if changed.Load() {
			body = "v2"
		}
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("ETag", `"`+body+`"`)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(h.writes.Wait)
	publicationRegressionAssertBody(t, publicationRegressionRequest(h, http.MethodGet, "en", false), "v1")
	mutation := publicationRegressionRequest(h, http.MethodPost, "en", false)
	if mutation.Code != http.StatusNoContent {
		t.Fatalf("mutation response=%d, want 204", mutation.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "http://cache.example/independent-policy-replacement", nil)
	req.Header.Set("Cache-Control", directive)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	h.writes.Wait()
	publicationRegressionAssertBody(t, rec, "v2")
}

func TestOpaqueLateStoreDoesNotBorrowNewGeneration(t *testing.T) {
	inner := generationBudgetCache(t, false, 1<<20)
	published := make(chan struct{})
	signal := &publicationRegressionPublicationSignal{Cache: inner, published: published, etag: `"v3"`}
	cache := &delayedStoreCache{inner: signal, started: make(chan struct{}), release: make(chan struct{})}
	var version atomic.Int64
	version.Store(1)
	h := NewSharedHandler(cache, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			version.Store(3)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		body := "v1"
		if version.Load() == 2 {
			body = "v2"
		}
		if version.Load() == 3 {
			body = "v3"
		}
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("ETag", `"`+body+`"`)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(h.writes.Wait)
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(cache.release) }) }
	t.Cleanup(unblock)
	publicationRegressionAssertBody(t, publicationRegressionRequest(h, http.MethodGet, "en", false), "v1")
	version.Store(2)
	cache.delay.Store(true)
	req := httptest.NewRequest(http.MethodGet, "http://cache.example/independent-policy-replacement", nil)
	req.Header.Set("Cache-Control", "min-fresh=7200")
	first := httptest.NewRecorder()
	h.ServeHTTP(first, req)
	publicationRegressionAssertBody(t, first, "v2")
	select {
	case <-cache.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first replacement publication did not pause")
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, req.URL.String(), nil))
	current := httptest.NewRecorder()
	h.ServeHTTP(current, httptest.NewRequest(http.MethodGet, req.URL.String(), nil))
	publicationRegressionAssertBody(t, current, "v3")
	publicationRegressionAwaitSignal(t, published, "post-mutation body publication")
	// The old v2 Store now publishes after the newer v3 has been recorded.
	// The max timestamp for this key cannot prove the older body is current.
	unblock()
	h.writes.Wait()
	publicationRegressionAssertBody(t, publicationRegressionRequest(h, http.MethodGet, "en", false), "v3")
}

type publicationRegressionPublicationSignal struct {
	Cache
	published chan struct{}
	etag      string
	once      sync.Once
}

func (c *publicationRegressionPublicationSignal) Store(res *Resource, keys ...string) error {
	err := c.Cache.Store(res, keys...)
	if err == nil && res.Header().Get("ETag") == c.etag {
		c.once.Do(func() { close(c.published) })
	}
	return err
}

type publicationRegressionCloseSignal struct {
	ReadSeekCloser
	closed chan struct{}
	once   sync.Once
}

func (r *publicationRegressionCloseSignal) Close() error {
	err := r.ReadSeekCloser.Close()
	r.once.Do(func() { close(r.closed) })
	return err
}

type publicationRegressionReorderedStore struct {
	Cache
	oldPaused, oldRelease, oldRecorded chan struct{}
	newPublished, newRelease           chan struct{}
	pauseOld, pauseNew                 atomic.Bool
}

func (c *publicationRegressionReorderedStore) Store(res *Resource, keys ...string) error {
	if res.Header().Get("ETag") == `"v2"` && c.pauseOld.CompareAndSwap(true, false) {
		// Handler closes this resource only after its publication bookkeeping.
		res.ReadSeekCloser = &publicationRegressionCloseSignal{ReadSeekCloser: res.ReadSeekCloser, closed: c.oldRecorded}
		close(c.oldPaused)
		<-c.oldRelease
	}
	err := c.Cache.Store(res, keys...)
	if res.Header().Get("ETag") == `"v3"` && c.pauseNew.CompareAndSwap(true, false) {
		close(c.newPublished)
		<-c.newRelease
	}
	return err
}

func (c *publicationRegressionReorderedStore) Invalidate(...string) {}

type publicationRegressionVersionOrigin struct{ version atomic.Int64 }

func (o *publicationRegressionVersionOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		o.version.Store(3)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	body := "v1"
	switch o.version.Load() {
	case 2:
		body = "v2"
	case 3:
		body = "v3"
	}
	w.Header().Set("Cache-Control", "max-age=3600")
	w.Header().Set("ETag", `"`+body+`"`)
	_, _ = io.WriteString(w, body)
}

func publicationRegressionServeUnjoined(h *Handler, force bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://cache.example/independent-publication-order", nil)
	if force {
		req.Header.Set("Cache-Control", "min-fresh=7200")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func publicationRegressionAwaitSignal(t *testing.T, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("did not observe %s", label)
	}
}

func TestOpaqueStoreReturnOrderDoesNotChangePublishedGeneration(t *testing.T) {
	inner := generationBudgetCache(t, false, 1<<20)
	cache := &publicationRegressionReorderedStore{
		Cache: inner, oldPaused: make(chan struct{}), oldRelease: make(chan struct{}), oldRecorded: make(chan struct{}),
		newPublished: make(chan struct{}), newRelease: make(chan struct{}),
	}
	origin := &publicationRegressionVersionOrigin{}
	origin.version.Store(1)
	h := NewSharedHandler(cache, origin)
	t.Cleanup(h.writes.Wait)
	var releaseOld, releaseNew sync.Once
	unblockOld := func() { releaseOld.Do(func() { close(cache.oldRelease) }) }
	unblockNew := func() { releaseNew.Do(func() { close(cache.newRelease) }) }
	t.Cleanup(func() { unblockOld(); unblockNew() })
	publicationRegressionAssertBody(t, publicationRegressionServeUnjoined(h, false), "v1")
	h.writes.Wait()
	origin.version.Store(2)
	cache.pauseOld.Store(true)
	cache.pauseNew.Store(true)
	publicationRegressionAssertBody(t, publicationRegressionServeUnjoined(h, true), "v2")
	publicationRegressionAwaitSignal(t, cache.oldPaused, "old Store before publication")
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://cache.example/independent-publication-order", nil))
	publicationRegressionAssertBody(t, publicationRegressionServeUnjoined(h, false), "v3")
	publicationRegressionAwaitSignal(t, cache.newPublished, "new Store after publication and before return")
	unblockOld()
	publicationRegressionAwaitSignal(t, cache.oldRecorded, "old Store bookkeeping completion")
	// Actual publication order is v3 then v2, but Store-return/bookkeeping
	// order is v2 then v3. Returning nil cannot identify the current body.
	unblockNew()
	h.writes.Wait()
	publicationRegressionAssertBody(t, publicationRegressionServeUnjoined(h, false), "v3")
}

type publicationRegressionPausedRetrieve struct {
	Cache
	pause             atomic.Bool
	captured, release chan struct{}
}

func (c *publicationRegressionPausedRetrieve) Retrieve(key string) (*Resource, error) {
	res, err := c.Cache.Retrieve(key)
	if c.pause.CompareAndSwap(true, false) {
		close(c.captured)
		<-c.release
	}
	return res, err
}

func TestOpaqueRetrievedBodyDoesNotBorrowLaterPublicationTime(t *testing.T) {
	inner := generationBudgetCache(t, false, 1<<20)
	cache := &publicationRegressionPausedRetrieve{Cache: inner, captured: make(chan struct{}), release: make(chan struct{})}
	origin := &publicationRegressionVersionOrigin{}
	origin.version.Store(1)
	h := NewSharedHandler(cache, origin)
	publicationRegressionAssertBody(t, publicationRegressionServeUnjoined(h, false), "v1")
	h.writes.Wait()
	cache.pause.Store(true)
	result := make(chan *httptest.ResponseRecorder, 1)
	finished := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(cache.release) }) }
	go func() {
		defer close(finished)
		result <- publicationRegressionServeUnjoined(h, false)
	}()
	t.Cleanup(func() {
		unblock()
		publicationRegressionAwaitSignal(t, finished, "paused reader completion")
		h.writes.Wait()
	})
	publicationRegressionAwaitSignal(t, cache.captured, "old resource retrieval")
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://cache.example/independent-publication-order", nil))
	publicationRegressionAssertBody(t, publicationRegressionServeUnjoined(h, false), "v3")
	h.writes.Wait()
	// The backend here forwards Invalidate and fences stores correctly. The
	// resource returned to the first reader still owns the old v1 snapshot.
	unblock()
	publicationRegressionAwaitSignal(t, finished, "paused reader completion")
	publicationRegressionAssertBody(t, <-result, "v3")
}

func TestOpaqueUnrelatedPublicationPreservesFreshHit(t *testing.T) {
	const firstURL = "http://cache.example/publication/a"
	const secondURL = "http://cache.example/publication/b"
	inner := generationBudgetCache(t, false, 1<<20)
	cache := &publicationRegressionPausedRetrieve{Cache: inner, captured: make(chan struct{}), release: make(chan struct{})}
	origin := &publicationRegressionVersionOrigin{}
	origin.version.Store(1)
	var firstGETs atomic.Int64
	h := NewSharedHandler(cache, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.String() == firstURL && r.Method == http.MethodGet {
			firstGETs.Add(1)
		}
		origin.ServeHTTP(w, r)
	}))
	t.Cleanup(h.writes.Wait)
	serve := func(method, url string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, url, nil))
		return rec
	}
	publicationRegressionAssertBody(t, serve(http.MethodGet, firstURL), "v1")
	h.writes.Wait()
	if rec := serve(http.MethodPost, firstURL); rec.Code != http.StatusNoContent {
		t.Fatalf("mutation response=%d, want 204", rec.Code)
	}
	publicationRegressionAssertBody(t, serve(http.MethodGet, firstURL), "v3")
	h.writes.Wait()
	key := NewRequestKey(httptest.NewRequest(http.MethodGet, firstURL, nil)).String()
	if _, marked := h.staleAt(key); !marked {
		t.Fatal("first resource does not retain its mutation marker")
	}
	before := firstGETs.Load()
	cache.pause.Store(true)
	result := make(chan *httptest.ResponseRecorder, 1)
	finished := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(cache.release) }) }
	go func() {
		defer close(finished)
		result <- serve(http.MethodGet, firstURL)
	}()
	t.Cleanup(func() {
		unblock()
		publicationRegressionAwaitSignal(t, finished, "paused first-resource reader")
	})
	publicationRegressionAwaitSignal(t, cache.captured, "fresh first-resource retrieval")
	publicationRegressionAssertBody(t, serve(http.MethodGet, secondURL), "v3")
	h.writes.Wait()
	unblock()
	publicationRegressionAwaitSignal(t, finished, "paused first-resource reader")
	rec := <-result
	publicationRegressionAssertBody(t, rec, "v3")
	if rec.Header().Get(CacheHeader) != "HIT" || firstGETs.Load() != before {
		t.Fatalf("unrelated publication caused revalidation: X-Cache=%q first-resource GETs=%d, want %d", rec.Header().Get(CacheHeader), firstGETs.Load(), before)
	}
}

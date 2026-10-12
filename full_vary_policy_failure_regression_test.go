package httpcache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

type fullVaryNativeFailureOrigin struct {
	changed atomic.Bool
	calls   atomic.Int64
	payload string
}

func (o *fullVaryNativeFailureOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.calls.Add(1)
	language := r.Header.Get("Accept-Language")
	body, vary, etag := language+"/old", "Accept-Language", strconv.Quote("old-"+language)
	if o.changed.Load() {
		body, vary, etag = o.payload, "Accept-Encoding", `"encoded-new"`
	}
	w.Header().Set("Cache-Control", "max-age=3600")
	w.Header().Set("ETag", etag)
	w.Header().Set("Vary", vary)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = io.WriteString(w, body)
}

func serveFullVaryNativeFailure(h *Handler, language string, force bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://cache.example/native-full-vary-policy", nil)
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

func TestFullVaryNativeCapacityFailureFencesOldLanguagePolicy(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(stringBool(disk), func(t *testing.T) {
			runFullVaryNativePublicationFailure(t, disk, false)
		})
	}
}

func TestFullVaryNativeHeaderStagingFailureFencesOldLanguagePolicy(t *testing.T) {
	runFullVaryNativePublicationFailure(t, true, true)
}

func runFullVaryNativePublicationFailure(t *testing.T, disk, stageHeader bool) {
	t.Helper()
	c := generationBudgetCache(t, disk, 1<<20)
	origin := &fullVaryNativeFailureOrigin{payload: strings.Repeat("new-encoded-body;", 256)}
	h := NewSharedHandler(c, origin)
	t.Cleanup(h.writes.Wait)
	publicationRegressionAssertBody(t, serveFullVaryNativeFailure(h, "en", false), "en/old")
	publicationRegressionAssertBody(t, serveFullVaryNativeFailure(h, "fr", false), "fr/old")
	origin.changed.Store(true)
	restore := refuseFullVaryNativePublication(t, c, len(origin.payload), stageHeader)
	publicationRegressionAssertBody(t, serveFullVaryNativeFailure(h, "en", true), origin.payload)
	assertFullVaryNativeDiscoveryFenced(t, c)
	// Neither the changed full response nor its real refetch can be published.
	// The untouched French variant must still validate through the base fence.
	publicationRegressionAssertBody(t, serveFullVaryNativeFailure(h, "fr", false), origin.payload)
	restore()
	publicationRegressionAssertBody(t, serveFullVaryNativeFailure(h, "fr", false), origin.payload)
	calls := origin.calls.Load()
	hit := serveFullVaryNativeFailure(h, "de", false)
	publicationRegressionAssertBody(t, hit, origin.payload)
	if hit.Header().Get(CacheHeader) != "HIT" || hit.Header().Get("Vary") != "Accept-Encoding" || origin.calls.Load() != calls {
		t.Fatalf("recovered encoding policy was not reusable: headers=%v calls=%d, want %d", hit.Header(), origin.calls.Load(), calls)
	}
}

func refuseFullVaryNativePublication(t *testing.T, c *cache, bodySize int, stageHeader bool) func() {
	t.Helper()
	if stageHeader {
		// The body lease fits exactly; its retained bytes prevent admitting the
		// following header stage before either file has been published.
		budget := &featureBudget{limit: int64(bodySize)}
		c.config.StagingBudget = budget
		restore := func() { c.config.StagingBudget = nil }
		t.Cleanup(func() { budget.assertReleased(t) })
		t.Cleanup(restore)
		return restore
	}
	previousMax := c.config.MaxSize
	c.config.MaxSize = c.Stats().TotalSize
	restore := func() { c.config.MaxSize = previousMax }
	t.Cleanup(restore)
	return restore
}

func assertFullVaryNativeDiscoveryFenced(t *testing.T, c *cache) {
	t.Helper()
	req, err := newCacheRequest(httptest.NewRequest(http.MethodGet, "http://cache.example/native-full-vary-policy", nil))
	if err != nil {
		t.Fatal(err)
	}
	base, err := c.Retrieve(req.Key.String())
	if err != nil {
		t.Fatal("failed publication unexpectedly removed the original discovery entry", err)
	}
	defer func() { _ = base.Close() }()
	if !base.IsStale() || base.Header().Get("Vary") != "Accept-Language" {
		t.Fatalf("obsolete discovery policy remained usable: stale=%v headers=%v", base.IsStale(), base.Header())
	}
}

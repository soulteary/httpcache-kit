package httpcache

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fullValidationChangedVaryOpaqueCache struct{ Cache }

type fullValidationChangedVaryOrigin struct {
	changed   atomic.Bool
	calls     atomic.Int64
	condition string
	modified  time.Time
}

func (o *fullValidationChangedVaryOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.calls.Add(1)
	vary, body, etag := "Accept-Language", "old language body", `"old"`
	modified := o.modified.Add(-time.Hour)
	if o.changed.Load() {
		vary, body, etag, modified = "Accept-Encoding", "current encoding body", `"current"`, o.modified
	}
	w.Header().Set("Cache-Control", "max-age=3600")
	w.Header().Set("Vary", vary)
	if o.condition == "etag" {
		w.Header().Set("ETag", etag)
	}
	http.ServeContent(w, r, "object", modified, strings.NewReader(body))
}

func fullValidationChangedVaryRequest(language string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://cache.example/full-validation-changed-vary", nil)
	req.Header.Set("Accept-Language", language)
	req.Header.Set("Accept-Encoding", "gzip")
	return req
}

func serveFullValidationChangedVary(h *Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	h.writes.Wait()
	return rec
}

func fullValidationChangedVaryCache(t *testing.T, disk, opaque bool) Cache {
	t.Helper()
	inner := generationBudgetCache(t, disk, 1<<20)
	if opaque {
		return &fullValidationChangedVaryOpaqueCache{Cache: inner}
	}
	return inner
}

func TestFullValidationChangedVaryMatchingClientRebuildsBodyAndIndex(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, opaque := range []bool{false, true} {
			for _, condition := range []string{"etag", "date"} {
				t.Run("disk="+strconv.FormatBool(disk)+"/opaque="+strconv.FormatBool(opaque)+"/"+condition, func(t *testing.T) {
					runFullValidationChangedVaryRebuild(t, disk, opaque, condition)
				})
			}
		}
	}
}

func runFullValidationChangedVaryRebuild(t *testing.T, disk, opaque bool, condition string) {
	t.Helper()
	backend := fullValidationChangedVaryCache(t, disk, opaque)
	origin := &fullValidationChangedVaryOrigin{condition: condition, modified: time.Now().Add(-time.Minute).UTC().Truncate(time.Second)}
	h := NewSharedHandler(backend, origin)
	t.Cleanup(h.writes.Wait)
	warm := fullValidationChangedVaryRequest("en")
	assertFullValidationChangedVaryBody(t, serveFullValidationChangedVary(h, warm), "old language body")
	origin.changed.Store(true)
	client := fullValidationChangedVaryRequest("en")
	setFullValidationChangedVaryCondition(client, condition, origin.modified)
	client.Header.Set("Cache-Control", "min-fresh=7200")
	assertFullValidationChangedVaryNotModified(t, serveFullValidationChangedVary(h, client))
	if origin.calls.Load() != 3 {
		t.Fatalf("full validation and ordered rebuild used %d origin calls, want 3 including warmup", origin.calls.Load())
	}
	// The client received no body, but the full replacement and its new Vary
	// index must already be reusable by another language with the same encoding.
	hit := serveFullValidationChangedVary(h, fullValidationChangedVaryRequest("fr"))
	assertFullValidationChangedVaryBody(t, hit, "current encoding body")
	assertFullValidationChangedVaryHit(t, hit, origin.calls.Load())
	repeat := fullValidationChangedVaryRequest("de")
	setFullValidationChangedVaryCondition(repeat, condition, origin.modified)
	rec := serveFullValidationChangedVary(h, repeat)
	assertFullValidationChangedVaryNotModified(t, rec)
	assertFullValidationChangedVaryHit(t, rec, origin.calls.Load())
}

func setFullValidationChangedVaryCondition(req *http.Request, condition string, modified time.Time) {
	if condition == "etag" {
		req.Header.Set("If-None-Match", `"current"`)
		return
	}
	req.Header.Set("If-Modified-Since", modified.Format(http.TimeFormat))
}

func assertFullValidationChangedVaryBody(t *testing.T, rec *httptest.ResponseRecorder, body string) {
	t.Helper()
	if rec.Code != http.StatusOK || rec.Body.String() != body {
		t.Fatalf("wrong representation: status=%d body=%q, want %q", rec.Code, rec.Body.String(), body)
	}
}

func assertFullValidationChangedVaryNotModified(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Fatalf("matching client condition lost: status=%d body=%q, want 304 without a body", rec.Code, rec.Body.String())
	}
}

func assertFullValidationChangedVaryHit(t *testing.T, rec *httptest.ResponseRecorder, calls int64) {
	t.Helper()
	if rec.Header().Get(CacheHeader) != "HIT" || rec.Header().Get("Vary") != "Accept-Encoding" || calls != 3 {
		t.Fatalf("client 304 did not leave a reusable rebuilt index: headers=%v origin calls=%d, want HIT without more than 3 calls", rec.Header(), calls)
	}
}

type fullValidationSelectingVaryOrigin struct {
	changed atomic.Bool
	calls   atomic.Int64
}

func (o *fullValidationSelectingVaryOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.calls.Add(1)
	vary, body, etag := "Accept-Language", "old language body", `"old"`
	if o.changed.Load() {
		vary = "If-None-Match"
		body, etag = "representation selected without client condition", `"unconditional-selected"`
		if r.Header.Get("If-None-Match") == `"current"` {
			body, etag = "representation selected by client condition", `"client-selected"`
		}
	}
	w.Header().Set("Cache-Control", "max-age=3600")
	w.Header().Set("Vary", vary)
	w.Header().Set("ETag", etag)
	http.ServeContent(w, r, "object", time.Time{}, strings.NewReader(body))
}

func TestFullValidationChangedVaryPreservesSelectingClientValidator(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, opaque := range []bool{false, true} {
			t.Run("disk="+strconv.FormatBool(disk)+"/opaque="+strconv.FormatBool(opaque), func(t *testing.T) {
				runFullValidationSelectingVary(t, disk, opaque)
			})
		}
	}
}

func runFullValidationSelectingVary(t *testing.T, disk, opaque bool) {
	t.Helper()
	backend := fullValidationChangedVaryCache(t, disk, opaque)
	origin := &fullValidationSelectingVaryOrigin{}
	h := NewSharedHandler(backend, origin)
	t.Cleanup(h.writes.Wait)
	assertFullValidationChangedVaryBody(t, serveFullValidationChangedVary(h, fullValidationChangedVaryRequest("en")), "old language body")
	origin.changed.Store(true)
	client := fullValidationChangedVaryRequest("en")
	client.Header.Set("If-None-Match", `"current"`)
	client.Header.Set("Cache-Control", "min-fresh=7200")
	rec := serveFullValidationChangedVary(h, client)
	assertFullValidationChangedVaryBody(t, rec, "representation selected by client condition")
	if rec.Header().Get("Vary") != "If-None-Match" || origin.calls.Load() != 3 {
		t.Fatalf("new selector was not fetched with the original client condition: headers=%v origin calls=%d", rec.Header(), origin.calls.Load())
	}
	repeat := fullValidationChangedVaryRequest("fr")
	repeat.Header.Set("If-None-Match", `"current"`)
	rec = serveFullValidationChangedVary(h, repeat)
	assertFullValidationChangedVaryBody(t, rec, "representation selected by client condition")
	if rec.Header().Get(CacheHeader) != "HIT" || origin.calls.Load() != 3 {
		t.Fatalf("client-selected variant was not reused: headers=%v origin calls=%d", rec.Header(), origin.calls.Load())
	}
}

package httpcache

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fullVaryMatchingOrigin struct {
	condition string
	modified  time.Time
	changed   atomic.Bool
	calls     atomic.Int64
	last      atomic.Value
}

func (o *fullVaryMatchingOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.calls.Add(1)
	o.last.Store(r.Header.Clone())
	body, tag, vary, modified := r.Header.Get("Accept-Language")+"/old", `"old"`, "Accept-Language", o.modified
	if o.changed.Load() {
		body, tag, vary, modified = "complete replacement body", `"current"`, "Accept-Encoding", modified.Add(time.Hour)
	}
	w.Header().Set("Cache-Control", "max-age=3600")
	w.Header().Set("Vary", vary)
	if o.condition == "etag" {
		w.Header().Set("ETag", tag)
	}
	http.ServeContent(w, r, "", modified, strings.NewReader(body))
}

func fullVaryMatchingRequest(language, condition string, modified time.Time, force bool) *http.Request {
	req := fullVaryPolicyRequest(language, force)
	switch condition {
	case "etag":
		req.Header.Set("If-None-Match", `"current"`)
	case "date":
		req.Header.Set("If-Modified-Since", modified.Add(time.Hour).Format(http.TimeFormat))
	}
	return req
}

func TestFullChangedVaryMatchingClientRebuildsCompleteBody(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, opaque := range []bool{false, true} {
			for _, condition := range []string{"etag", "date"} {
				t.Run(stringBool(disk)+"/opaque="+stringBool(opaque)+"/"+condition, func(t *testing.T) {
					testFullVaryMatchingRebuild(t, disk, opaque, condition)
				})
			}
		}
	}
}

func testFullVaryMatchingRebuild(t *testing.T, disk, opaque bool, condition string) {
	t.Helper()
	inner := generationBudgetCache(t, disk, 1<<20)
	var cache Cache = inner
	if opaque {
		cache = impreciseCache{plainCache{inner: inner}}
	}
	budget := &featureBudget{limit: 1 << 20}
	t.Cleanup(func() { budget.assertReleased(t) })
	origin := &fullVaryMatchingOrigin{condition: condition, modified: time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)}
	h := NewHandlerWithOptions(cache, origin, &HandlerOptions{StagingBudget: budget})
	h.Shared = true
	defer h.writes.Wait()
	assertReviewVaryBody(t, reviewServeConditionalVary(h, fullVaryMatchingRequest("en", "", origin.modified, false)), "en/old")
	assertReviewVaryBody(t, reviewServeConditionalVary(h, fullVaryMatchingRequest("fr", "", origin.modified, false)), "fr/old")
	origin.changed.Store(true)
	req := fullVaryMatchingRequest("en", condition, origin.modified, true)
	original := req.Header.Clone()
	assertFullVaryMatchingDelivery(t, reviewServeConditionalVary(h, req))
	if !reflect.DeepEqual(original, req.Header) {
		t.Fatalf("rebuild mutated client validators: %v", req.Header)
	}
	assertFullVaryRebuildFetchUnconditional(t, origin)
	repeat := fullVaryMatchingRequest("en", condition, origin.modified, false)
	assertFullVaryMatchingDelivery(t, reviewServeConditionalVary(h, repeat))
	if origin.calls.Load() != 4 {
		t.Fatalf("matching client condition did not reuse the rebuilt cache: origins=%d, want 4", origin.calls.Load())
	}
	// The new encoding selection must reuse the complete body across both the
	// previous French variant and a language that was never stored before.
	for _, language := range []string{"fr", "es"} {
		hit := reviewServeConditionalVary(h, fullVaryMatchingRequest(language, "", origin.modified, false))
		assertReviewVaryBody(t, hit, "complete replacement body")
		if origin.calls.Load() != 4 || hit.Header().Get(CacheHeader) != "HIT" || hit.Header().Get("Vary") != "Accept-Encoding" {
			t.Fatalf("changed discovery index did not reuse full body: calls=%d headers=%v", origin.calls.Load(), hit.Header())
		}
	}
}

func assertFullVaryMatchingDelivery(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Fatalf("matching client condition changed: status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func assertFullVaryRebuildFetchUnconditional(t *testing.T, origin *fullVaryMatchingOrigin) {
	t.Helper()
	header := origin.last.Load().(http.Header)
	if header.Get("If-None-Match") != "" || header.Get("If-Modified-Since") != "" || origin.calls.Load() != 4 {
		t.Fatalf("complete-body rebuild still carried matching conditions: calls=%d request=%v", origin.calls.Load(), header)
	}
}

type switchingRebuildVaryOrigin struct {
	condition string
	modified  time.Time
	calls     atomic.Int64
	last      atomic.Value
}

func (o *switchingRebuildVaryOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	call := o.calls.Add(1)
	o.last.Store(r.Header.Clone())
	body, tag, vary, modified := "old representation", `"old"`, "Accept-Language", o.modified
	if call == 2 {
		body, tag, vary, modified = "transitional encoding response", `"transitional"`, "Accept-Encoding", modified.Add(time.Hour)
	} else if call > 2 {
		body, tag, vary, modified = "wrong missing-condition selection", `"missing"`, o.condition, modified.Add(2*time.Hour)
		if r.Header.Get(o.condition) != "" {
			body, tag = "selected client representation", `"selected"`
		}
	}
	w.Header().Set("Cache-Control", "max-age=3600")
	w.Header().Set("Vary", vary)
	w.Header().Set("Trailer", "X-Selection-Complete")
	if o.condition == "If-None-Match" {
		w.Header().Set("ETag", tag)
	}
	http.ServeContent(w, r, "", modified, strings.NewReader(body))
	w.Header().Set("X-Selection-Complete", "complete")
}

func TestChangedVaryRebuildRechecksActualFetchSelection(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, opaque := range []bool{false, true} {
			for _, condition := range []string{"If-None-Match", "If-Modified-Since"} {
				t.Run(stringBool(disk)+"/opaque="+stringBool(opaque)+"/"+condition, func(t *testing.T) {
					testSwitchingRebuildVary(t, disk, opaque, condition)
				})
			}
		}
	}
}

func testSwitchingRebuildVary(t *testing.T, disk, opaque bool, condition string) {
	t.Helper()
	inner := generationBudgetCache(t, disk, 1<<20)
	var cache Cache = inner
	if opaque {
		cache = impreciseCache{plainCache{inner: inner}}
	}
	origin := &switchingRebuildVaryOrigin{condition: condition, modified: time.Now().Add(-3 * time.Hour).UTC().Truncate(time.Second)}
	h := NewSharedHandler(cache, origin)
	defer h.writes.Wait()
	assertReviewVaryBody(t, reviewServeConditionalVary(h, fullVaryPolicyRequest("en", false)), "old representation")
	client := fullVaryPolicyRequest("en", true)
	value := `"client"`
	if condition == "If-Modified-Since" {
		value = origin.modified.Add(time.Hour).Format(http.TimeFormat)
	}
	client.Header.Set(condition, value)
	selected := reviewServeConditionalVary(h, client)
	assertReviewVaryBody(t, selected, "selected client representation")
	if origin.calls.Load() != 4 || origin.last.Load().(http.Header).Get(condition) != value {
		t.Fatalf("actual rebuild Vary lost original request identity: calls=%d headers=%v", origin.calls.Load(), origin.last.Load())
	}
	response := selected.Result()
	defer func() { _ = response.Body.Close() }()
	if response.Trailer.Get("X-Selection-Complete") != "complete" {
		t.Fatalf("selected response trailer lost: %v", response.Trailer)
	}
	repeat := client.Clone(client.Context())
	repeat.Header.Del("Cache-Control")
	hit := reviewServeConditionalVary(h, repeat)
	assertReviewVaryBody(t, hit, "selected client representation")
	if hit.Header().Get(CacheHeader) != "HIT" || origin.calls.Load() != 4 || hit.Header().Get("Vary") != condition {
		t.Fatalf("fallback did not store the original-request variant: calls=%d headers=%v", origin.calls.Load(), hit.Header())
	}
}

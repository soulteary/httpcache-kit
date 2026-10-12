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

type conditionalVaryIdentityOrigin struct {
	condition   string
	modified    time.Time
	withoutETag bool
	changed     atomic.Bool
	calls       atomic.Int64
	last        atomic.Value
}

func (o *conditionalVaryIdentityOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.calls.Add(1)
	o.last.Store(r.Header.Clone())
	body, tag, vary, modified := "old selection", `"old"`, "Accept-Language", o.modified
	if o.changed.Load() {
		vary = o.condition
		if r.Header.Get(o.condition) == "" {
			body, tag, modified = "unconditional selection", `"new"`, modified.Add(time.Hour)
		}
	}
	w.Header().Set("Cache-Control", "max-age=3600")
	if !o.withoutETag {
		w.Header().Set("ETag", tag)
	}
	w.Header().Set("Vary", vary)
	http.ServeContent(w, r, "", modified, strings.NewReader(body))
}

func conditionalVaryIdentityRequest(condition, value string, force bool) *http.Request {
	r := httptest.NewRequest("GET", "http://cache.example/conditional-vary-identity", nil)
	r.Header.Set("Accept-Language", "en")
	if condition != "" {
		r.Header.Set(condition, value)
	}
	if force {
		r.Header.Set("Cache-Control", "min-fresh=7200")
	}
	return r
}

func TestReviewVaryRebuildStoresFetchedConditionalIdentity(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, condition := range []string{"If-None-Match", "If-Modified-Since"} {
			t.Run(stringBool(disk)+"/"+condition, func(t *testing.T) {
				testVaryRebuildConditionalIdentity(t, disk, condition)
			})
		}
	}
}

func testVaryRebuildConditionalIdentity(t *testing.T, disk bool, condition string) {
	t.Helper()
	cache := generationBudgetCache(t, disk, 1<<20)
	origin := &conditionalVaryIdentityOrigin{condition: condition, modified: time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second), withoutETag: condition == "If-Modified-Since"}
	// Original-request refetches must keep the policy-selected base identity.
	h := newNetworkVaryReviewHandler(t, cache, origin, &HandlerOptions{RequestKey: featureLogicalKey})
	assertReviewVaryBody(t, reviewServeConditionalVary(h, conditionalVaryIdentityRequest("", "", false)), "old selection")
	origin.changed.Store(true)
	value := `"old"`
	if condition == "If-Modified-Since" {
		value = origin.modified.Format(http.TimeFormat)
	}
	force := conditionalVaryIdentityRequest(condition, value, true)
	original := force.Header.Clone()
	first := reviewServeConditionalVary(h, force)
	if first.Code != http.StatusNotModified || first.Body.Len() != 0 || origin.calls.Load() != 3 || !reflect.DeepEqual(force.Header, original) {
		t.Fatalf("Vary rebuild did not preserve the client request: calls=%d headers=%v", origin.calls.Load(), force.Header)
	}
	// A 304 cannot rebuild the body. The first unconditional request must
	// replace the fenced old index using its own selected representation.
	unconditional := conditionalVaryIdentityRequest("", "", false)
	unconditional.URL.Host, unconditional.Host = "other-cache.example", "other-cache.example"
	rebuilt := reviewServeConditionalVary(h, unconditional)
	assertReviewVaryBody(t, rebuilt, "unconditional selection")
	if rebuilt.Header().Get("Vary") != condition || origin.calls.Load() != 5 {
		t.Fatalf("unconditional request did not rebuild the old index: calls=%d headers=%v", origin.calls.Load(), rebuilt.Header())
	}
	conditional := reviewServeConditionalVary(h, conditionalVaryIdentityRequest(condition, value, false))
	if conditional.Code != http.StatusNotModified || conditional.Body.Len() != 0 || origin.calls.Load() != 6 {
		t.Fatalf("unconditional body poisoned the conditional variant: status=%d body=%q calls=%d", conditional.Code, conditional.Body.String(), origin.calls.Load())
	}
	hit := reviewServeConditionalVary(h, unconditional)
	assertReviewVaryBody(t, hit, "unconditional selection")
	if hit.Header().Get(CacheHeader) != "HIT" || hit.Header().Get("Vary") != condition || origin.calls.Load() != 6 {
		t.Fatalf("fetched variant lost its request or logical base identity: calls=%d headers=%v", origin.calls.Load(), hit.Header())
	}
}

func TestReviewConditionalVaryRebuildPreservesSelectedRepresentation(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, condition := range []string{"If-None-Match", "If-Modified-Since"} {
			for _, matching := range []bool{false, true} {
				t.Run(stringBool(disk)+"/"+condition+"/matching="+stringBool(matching), func(t *testing.T) {
					testConditionalVarySelectedRepresentation(t, disk, condition, matching)
				})
			}
		}
	}
}

func testConditionalVarySelectedRepresentation(t *testing.T, disk bool, condition string, matching bool) {
	t.Helper()
	cache := generationBudgetCache(t, disk, 1<<20)
	origin := &conditionalVaryIdentityOrigin{condition: condition, modified: time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)}
	h := newNetworkVaryReviewHandler(t, cache, origin, nil)
	assertReviewVaryBody(t, reviewServeConditionalVary(h, conditionalVaryIdentityRequest("", "", false)), "old selection")
	origin.changed.Store(true)
	value := conditionalVaryIdentityValue(condition, origin.modified, matching)
	r := conditionalVaryIdentityRequest(condition, value, true)
	rec := reviewServeConditionalVary(h, r)
	wantStatus, wantBody := http.StatusOK, "old selection"
	if matching {
		wantStatus, wantBody = http.StatusNotModified, ""
	}
	if rec.Code != wantStatus || rec.Body.String() != wantBody || rec.Header().Get("ETag") != `"old"` {
		t.Fatalf("rebuild changed the client's selected representation: status=%d body=%q headers=%v; want %d %q", rec.Code, rec.Body.String(), rec.Header(), wantStatus, wantBody)
	}
	if origin.calls.Load() != 3 || origin.last.Load().(http.Header).Get(condition) != value || r.Header.Get(condition) != value {
		t.Fatalf("rebuild removed or changed the varied client condition: calls=%d request=%v origin=%v", origin.calls.Load(), r.Header, origin.last.Load())
	}
}

func conditionalVaryIdentityValue(condition string, modified time.Time, matching bool) string {
	if condition == "If-Modified-Since" {
		if !matching {
			modified = modified.Add(-time.Hour)
		}
		return modified.Format(http.TimeFormat)
	}
	if matching {
		return `"old"`
	}
	return `"other"`
}

func TestReviewKnownConditionalVaryPreservesOriginalOriginRequest(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, condition := range []string{"If-None-Match", "If-Modified-Since"} {
			t.Run(stringBool(disk)+"/"+condition, func(t *testing.T) {
				cache := generationBudgetCache(t, disk, 1<<20)
				origin := &conditionalVaryIdentityOrigin{condition: condition, modified: time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second), withoutETag: condition == "If-Modified-Since"}
				origin.changed.Store(true)
				h := newNetworkVaryReviewHandler(t, cache, origin, nil)
				assertReviewVaryBody(t, reviewServeConditionalVary(h, conditionalVaryIdentityRequest("", "", false)), "unconditional selection")
				forced := reviewServeConditionalVary(h, conditionalVaryIdentityRequest("", "", true))
				assertReviewVaryBody(t, forced, "unconditional selection")
				header := origin.last.Load().(http.Header)
				if origin.calls.Load() != 2 || header.Get("If-None-Match") != "" || header.Get("If-Modified-Since") != "" {
					t.Fatalf("cache validator changed the known Vary selection: calls=%d origin=%v", origin.calls.Load(), header)
				}
				hit := reviewServeConditionalVary(h, conditionalVaryIdentityRequest("", "", false))
				assertReviewVaryBody(t, hit, "unconditional selection")
				if hit.Header().Get(CacheHeader) != "HIT" || origin.calls.Load() != 2 {
					t.Fatalf("original-request replacement was not reused: calls=%d headers=%v", origin.calls.Load(), hit.Header())
				}
			})
		}
	}
}

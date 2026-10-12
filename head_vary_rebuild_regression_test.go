package httpcache

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type headVaryRebuildOrigin struct {
	changed  atomic.Bool
	calls    atomic.Int64
	modified time.Time
}

func (o *headVaryRebuildOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.calls.Add(1)
	body, vary := "variant A", "Accept-Language"
	if o.changed.Load() {
		vary = "Accept-Encoding"
		if r.Header.Get("Accept-Encoding") == "br" {
			body = "variant B"
		}
	} else if r.Header.Get("Accept-Language") == "fr" {
		body = "variant B"
	}
	w.Header().Set("Cache-Control", "max-age=3600")
	w.Header().Set("Vary", vary)
	w.Header().Set("ETag", strconv.Quote(strings.ReplaceAll(body, " ", "-")))
	http.ServeContent(w, r, "", o.modified, strings.NewReader(body))
}

func newNetworkVaryReviewHandler(t *testing.T, cache Cache, origin http.Handler, opts *HandlerOptions) *Handler {
	t.Helper()
	server := httptest.NewServer(origin)
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport
	h := NewHandlerWithOptions(cache, proxy, opts)
	h.Shared = true
	t.Cleanup(h.writes.Wait)
	return h
}

func headVaryRebuildRequest(method, language string, force bool) *http.Request {
	r := httptest.NewRequest(method, "http://cache.example/head-vary-rebuild", nil)
	r.Header.Set("Accept-Language", language)
	r.Header.Set("Accept-Encoding", "gzip")
	if force {
		r.Header.Set("Cache-Control", "min-fresh=7200")
	}
	return r
}

func TestReviewHeadVaryRebuildInvalidatesGetDiscovery(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(stringBool(disk), func(t *testing.T) {
			cache := generationBudgetCache(t, disk, 1<<20)
			origin := &headVaryRebuildOrigin{modified: time.Now().Add(-time.Hour).UTC().Truncate(time.Second)}
			h := newNetworkVaryReviewHandler(t, cache, origin, nil)
			assertReviewVaryBody(t, reviewServeConditionalVary(h, headVaryRebuildRequest("GET", "en", false)), "variant A")
			assertReviewVaryBody(t, reviewServeConditionalVary(h, headVaryRebuildRequest("GET", "fr", false)), "variant B")
			origin.changed.Store(true)
			head := reviewServeConditionalVary(h, headVaryRebuildRequest("HEAD", "en", true))
			if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Vary") != "Accept-Encoding" {
				t.Fatalf("HEAD rebuild returned %d %q %v", head.Code, head.Body.String(), head.Header())
			}
			if origin.calls.Load() != 4 {
				t.Fatalf("HEAD fallback made %d origin requests; want two seeds, validation and refetch", origin.calls.Load())
			}
			// The other language's GET variant has never been selected by HEAD.
			// Its old discovery base must nevertheless fence that representation.
			get := reviewServeConditionalVary(h, headVaryRebuildRequest("GET", "fr", false))
			assertReviewVaryBody(t, get, "variant A")
			if get.Header().Get("Vary") != "Accept-Encoding" || origin.calls.Load() != 5 {
				t.Fatalf("GET reused the old HEAD-fallback discovery index: calls=%d headers=%v", origin.calls.Load(), get.Header())
			}
			hit := reviewServeConditionalVary(h, headVaryRebuildRequest("GET", "en", false))
			assertReviewVaryBody(t, hit, "variant A")
			if hit.Header().Get(CacheHeader) != "HIT" || origin.calls.Load() != 5 {
				t.Fatalf("rebuilt GET policy was not reused across languages: calls=%d headers=%v", origin.calls.Load(), hit.Header())
			}
		})
	}
}

func TestReviewValidationKeysTrackAndResetDiscoveryBase(t *testing.T) {
	cache := generationBudgetCache(t, false, 1<<20)
	origin := &headVaryRebuildOrigin{modified: time.Now().Add(-time.Hour).UTC().Truncate(time.Second)}
	h := newNetworkVaryReviewHandler(t, cache, origin, nil)
	reviewServeConditionalVary(h, headVaryRebuildRequest("GET", "en", false))
	for _, method := range []string{"GET", "HEAD"} {
		t.Run(method, func(t *testing.T) {
			req, err := newCacheRequest(headVaryRebuildRequest(method, "en", false))
			if err != nil {
				t.Fatal(err)
			}
			res, err := h.lookup(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Close()
			getKey := req.Key.ForMethod("GET")
			want := []string{req.Key.String()}
			if method == "HEAD" {
				want = append(want, getKey.String())
			}
			want = append(want, getKey.Vary("Accept-Language", req.Request).String())
			if req.discoveryKey != getKey.String() || !reflect.DeepEqual(req.validationKeys(), want) {
				t.Fatalf("lookup base=%q validation keys=%v; want base=%q keys=%v", req.discoveryKey, req.validationKeys(), getKey.String(), want)
			}
			// A second coordinated lookup can miss. It must not reuse keys
			// recorded by an earlier successful retrieval on the same request.
			req.URL.Path = "/missing-after-lookup"
			req.Key = NewRequestKey(req.Request)
			if _, _, err := h.lookupBaseResource(req); err != ErrNotFoundInCache || req.discoveryKey != "" || req.servedKey != "" {
				t.Fatalf("miss retained prior lookup identity: error=%v base=%q variant=%q", err, req.discoveryKey, req.servedKey)
			}
		})
	}
}

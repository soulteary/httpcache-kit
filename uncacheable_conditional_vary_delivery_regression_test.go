package httpcache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

// Expose only Cache so the handler also exercises its own invalidation and
// publication evidence for a backend without native generation extensions.
type uncacheableVaryDeliveryOpaqueCache struct{ Cache }

func uncacheableVaryDeliveryRequest(force bool) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://cache.example/uncacheable-vary-delivery", nil)
	req.Header.Set("Accept-Language", "en")
	req.Header.Set("Accept-Encoding", "gzip")
	if force {
		req.Header.Set("Cache-Control", "min-fresh=7200")
	}
	return req
}

func serveUncacheableVaryDelivery(h *Handler, force bool) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, uncacheableVaryDeliveryRequest(force))
	h.writes.Wait()
	return rec
}

func assertUncacheableVaryPredecessorStored(t *testing.T, cache Cache) {
	t.Helper()
	req := uncacheableVaryDeliveryRequest(false)
	key := NewRequestKey(req).Vary("Accept-Language", req).String()
	res, err := cache.Retrieve(key)
	if err != nil {
		t.Fatalf("predecessor must be cached before validation: %v", err)
	}
	defer func() { _ = res.Close() }()
	if res.Header().Get("ETag") != `"old"` {
		t.Fatalf("unexpected cached predecessor: headers=%v", res.Header())
	}
}

type uncacheableVarySelectorOrigin struct {
	calls           atomic.Int64
	validatedOld    atomic.Bool
	refetchedClient atomic.Bool
	status          int
}

func (o *uncacheableVarySelectorOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if o.calls.Add(1) == 1 {
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Vary", "Accept-Language")
		w.Header().Set("ETag", `"old"`)
		_, _ = io.WriteString(w, "cacheable predecessor")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "If-None-Match")
	w.Header().Set("ETag", `"current"`)
	if o.status == http.StatusServiceUnavailable {
		// An error's matching validator cannot turn its status/body into 304.
		w.Header().Set("ETag", `"client"`)
	}
	body := "unexpected selection condition"
	switch r.Header.Get("If-None-Match") {
	case `"old"`:
		o.validatedOld.Store(true)
		body = "selected-by-cache-validator"
	case `"client"`:
		o.refetchedClient.Store(true)
		body = "selected-by-client-validator"
	}
	w.WriteHeader(o.status)
	_, _ = io.WriteString(w, body)
}

func TestUncacheableVarySelectingValidatorRefetchesOriginalRequest(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		for _, opaque := range []bool{false, true} {
			t.Run(strconv.Itoa(status)+"/opaque="+strconv.FormatBool(opaque), func(t *testing.T) {
				runUncacheableVarySelector(t, status, opaque)
			})
		}
	}
}

func runUncacheableVarySelector(t *testing.T, status int, opaque bool) {
	t.Helper()
	inner := NewMemoryCacheWithConfig(DefaultCacheConfig().WithCleanupInterval(0))
	t.Cleanup(func() { _ = inner.Close() })
	var backend Cache = inner
	if opaque {
		backend = &uncacheableVaryDeliveryOpaqueCache{Cache: inner}
	}
	origin := &uncacheableVarySelectorOrigin{status: status}
	h := NewSharedHandler(backend, origin)
	t.Cleanup(h.writes.Wait)
	first := serveUncacheableVaryDelivery(h, false)
	if first.Code != http.StatusOK || first.Body.String() != "cacheable predecessor" {
		t.Fatalf("predecessor was not delivered: status=%d body=%q", first.Code, first.Body.String())
	}
	assertUncacheableVaryPredecessorStored(t, inner)
	req := uncacheableVaryDeliveryRequest(true)
	req.Header.Set("If-None-Match", `"client"`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	h.writes.Wait()
	if rec.Code != status || rec.Body.String() != "selected-by-client-validator" {
		t.Fatalf("rewritten selecting condition changed the response: status=%d body=%q, want status=%d client-selected body", rec.Code, rec.Body.String(), status)
	}
	if rec.Header().Get("Vary") != "If-None-Match" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("client-selected response headers changed: %v", rec.Header())
	}
	if origin.calls.Load() != 3 || !origin.validatedOld.Load() || !origin.refetchedClient.Load() {
		t.Fatalf("selection was not retried with the original condition: calls=%d validatedOld=%v refetchedClient=%v", origin.calls.Load(), origin.validatedOld.Load(), origin.refetchedClient.Load())
	}
}

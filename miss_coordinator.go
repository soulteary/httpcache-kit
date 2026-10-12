package httpcache

import (
	"context"
	"errors"
	"net/http"
	"sync"
)

// MissCoordinator is an optional application-provided exclusive fill lock.
// Acquire waits with ctx until it owns key; callers recheck the cache after
// acquisition. It must not return a lease until previous owners have finished
// publishing. The backend must provide crash-safe ownership and, if necessary,
// fencing for its own storage; this interface alone is not distributed locking.
type MissCoordinator interface {
	Acquire(context.Context, string) (MissLease, error)
}

// MissLease owns a fill through background publication. Context preserves
// backend ownership/fencing values and must derive from the Acquire context.
// Close releases ownership, must be bounded and idempotent. A ContextStore
// backend receives those values during asynchronous Store.
type MissLease interface {
	Context() context.Context
	Close() error
}

func (h *Handler) leadMiss(w http.ResponseWriter, r *cacheRequest, complete func()) (*Resource, bool) {
	if h.missCoordinator == nil {
		h.passUpstream(w, r, complete)
		return nil, false
	}
	lease, err := h.missCoordinator.Acquire(r.Context(), r.Key.String())
	if err == nil && (lease == nil || lease.Context() == nil) {
		err = errors.New("miss coordinator returned no lease context")
	}
	if err != nil {
		if lease != nil {
			_ = lease.Close()
		}
		callComplete(complete)
		if r.Context().Err() == nil {
			http.Error(w, "cache fill coordination unavailable", http.StatusServiceUnavailable)
		}
		return nil, false
	}
	var once sync.Once
	done := func() {
		once.Do(func() {
			if err := lease.Close(); err != nil {
				h.errorf("releasing cache fill ownership: %v", err)
			}
			callComplete(complete)
		})
	}
	res, err := h.lookup(r)
	if err == nil {
		done()
		return res, true
	}
	if !errors.Is(err, ErrNotFoundInCache) {
		done()
		http.Error(w, "cache lookup failed", http.StatusInternalServerError)
		return nil, false
	}
	if err := lease.Context().Err(); err != nil {
		done()
		return nil, false
	}
	copy := *r
	copy.Request = r.WithContext(lease.Context())
	h.passUpstream(w, &copy, done)
	return nil, false
}

func (h *Handler) invalidationKeys(res *Resource, r *cacheRequest) []string {
	if h.requestKey == nil {
		return invalidationKeys(res, r)
	}
	requests := []*http.Request{r.Request}
	for _, header := range []string{"Location", "Content-Location"} {
		if raw := res.Header().Get(header); raw != "" {
			if u := r.sameOriginURL(raw); u != nil {
				copy := r.Clone(r.Context())
				copy.URL = u
				requests = append(requests, copy)
			}
		}
	}
	var keys []string
	for _, request := range requests {
		copy := request.Clone(request.Context())
		copy.Method = http.MethodGet
		key, err := h.requestKey(copy)
		if err != nil {
			// A failing policy cannot silently invalidate an unrelated key.
			h.errorf("resolving invalidation cache identity: %v", err)
			continue
		}
		keys = append(keys, key.String(), key.ForMethod(http.MethodHead).String())
	}
	return keys
}

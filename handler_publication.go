package httpcache

import (
	"context"
	"slices"
	"time"
)

// Opaque caches cannot expose the generation that their Retrieve returned.
// Overlapping publications therefore remain untrusted until a later isolated
// Store or Freshen. This never holds a handler lock during a backend call.
type localPublication struct {
	active  int
	overlap bool
}

// Each successful isolated publication owns an immutable proof. A retrieval
// keeps it alive while comparing snapshots, so even equal clocks cannot make
// a later publication look like the generation it actually read.
type localFreshProof struct{ at time.Time }

func (h *Handler) beginLocalPublication(at time.Time, keys []string) func(bool) {
	if _, ok := h.cache.(staleAtChecker); ok {
		return func(bool) {}
	}
	if at.IsZero() {
		at = Clock()
	}
	keys = slices.Clone(keys)
	slices.Sort(keys)
	keys = slices.Compact(keys)
	h.localStaleMu.Lock()
	if h.localPublications == nil {
		h.localPublications = make(map[string]*localPublication)
	}
	for _, key := range keys {
		publication := h.localPublications[key]
		if publication == nil {
			publication = &localPublication{}
			h.localPublications[key] = publication
		}
		publication.active++
		publication.overlap = publication.overlap || publication.active > 1
		delete(h.localFresh, key)
	}
	h.localStaleMu.Unlock()
	return func(success bool) { h.finishLocalPublication(at, keys, success) }
}

func (h *Handler) finishLocalPublication(at time.Time, keys []string, success bool) {
	h.localStaleMu.Lock()
	defer h.localStaleMu.Unlock()
	if h.localFresh == nil {
		h.localFresh = make(map[string]*localFreshProof)
	}
	cutoff := Clock().Add(-h.staleMarkerTTL())
	for key, recordedAt := range h.localFresh {
		if recordedAt.at.Before(cutoff) {
			delete(h.localFresh, key)
		}
	}
	for _, key := range keys {
		publication := h.localPublications[key]
		publication.active--
		if publication.active == 0 {
			if success && !publication.overlap {
				h.localFresh[key] = &localFreshProof{at: at}
			}
			delete(h.localPublications, key)
		}
	}
}

func (h *Handler) publishResource(ctx context.Context, res *Resource, at time.Time, keys ...string) error {
	complete := h.beginLocalPublication(at, keys)
	var err error
	if contextual, ok := h.cache.(ContextStore); ok {
		err = contextual.StoreContext(ctx, res, keys...)
	} else {
		err = h.cache.Store(res, keys...)
	}
	complete(err == nil)
	return err
}

func (h *Handler) freshenResource(res *Resource, keys ...string) error {
	complete := h.beginLocalPublication(res.RequestTime, keys)
	err := h.cache.Freshen(res, keys...)
	complete(err == nil)
	return err
}

func (h *Handler) generationRetriever(retrieve func(string) (*Resource, error)) func(string) (*Resource, error) {
	if _, ok := h.cache.(staleAtChecker); ok {
		return retrieve
	}
	return func(key string) (*Resource, error) {
		before, idleBefore := h.localPublicationSnapshot(key)
		res, err := retrieve(key)
		if err != nil || res == nil {
			return res, err
		}
		after, idleAfter := h.localPublicationSnapshot(key)
		res.handlerStoredAt = time.Time{}
		if idleBefore && idleAfter && before != nil && before == after {
			res.handlerStoredAt = before.at
		}
		return res, nil
	}
}

func (h *Handler) localPublicationSnapshot(key string) (*localFreshProof, bool) {
	h.localStaleMu.Lock()
	defer h.localStaleMu.Unlock()
	return h.localFresh[key], h.localPublications[key] == nil
}

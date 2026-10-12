package httpcache

import (
	"bytes"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestResponseCacheabilityPolicy(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		policy       string
		shared       bool
		requestAuth  string
		responseAuth string
		vary         string
		etag         string
		want         bool
	}{
		{name: "fresh private response", status: 200, policy: "max-age=60", want: true},
		{name: "no cache forbids storage", status: 200, policy: "no-cache, max-age=60"},
		{name: "no store forbids storage", status: 200, policy: "no-store, max-age=60"},
		{name: "vary wildcard forbids storage", status: 200, policy: "max-age=60", vary: "Accept-Language, *"},
		{name: "shared private response", status: 200, policy: "private, max-age=60", shared: true},
		{name: "private cache accepts private response", status: 200, policy: "private, max-age=60", want: true},
		{name: "shared private field list", status: 200, policy: `private="Set-Cookie", max-age=60`, shared: true, want: true},
		{name: "unsupported status", status: 201, policy: "public, max-age=60"},
		{name: "shared request authorization", status: 200, policy: "max-age=60", shared: true, requestAuth: "Bearer private"},
		{name: "public request authorization", status: 200, policy: "public, max-age=60", shared: true, requestAuth: "Bearer private", want: true},
		{name: "shared lifetime request authorization", status: 200, policy: "s-maxage=60", shared: true, requestAuth: "Bearer private", want: true},
		{name: "private request authorization", status: 200, policy: "max-age=60", requestAuth: "Bearer private", want: true},
		{name: "shared response authorization", status: 200, policy: "max-age=60", shared: true, responseAuth: "Bearer private"},
		{name: "public alone does not permit response authorization", status: 200, policy: "public, max-age=60", shared: true, responseAuth: "Bearer private"},
		{name: "revalidated response authorization", status: 200, policy: "must-revalidate, max-age=60", shared: true, responseAuth: "Bearer private", want: true},
		{name: "shared lifetime response authorization", status: 200, policy: "s-maxage=60", shared: true, responseAuth: "Bearer private", want: true},
		{name: "private response authorization", status: 200, policy: "max-age=60", responseAuth: "Bearer private", want: true},
		{name: "zero freshness without validator", status: 200, policy: "max-age=0"},
		{name: "zero freshness with validator", status: 200, policy: "max-age=0", etag: `"v1"`, want: true},
		{name: "invalid lifetime with validator", status: 200, policy: "max-age=invalid", etag: `"v1"`},
		{name: "shared zero overrides private lifetime", status: 200, policy: "max-age=60, s-maxage=0", shared: true},
		{name: "private cache ignores shared zero", status: 200, policy: "max-age=60, s-maxage=0", want: true},
		{name: "default status validator", status: 200, etag: `"v1"`, want: true},
		{name: "no expiration or validator", status: 200},
		{name: "nondefault status needs public", status: 404, etag: `"v1"`},
		{name: "public nondefault status validator", status: 404, policy: "public", etag: `"v1"`, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://example.org/item", nil)
			req.Header.Set("Authorization", tc.requestAuth)
			res := NewResourceBytes(tc.status, nil, http.Header{
				"Cache-Control": {tc.policy},
				"Authorization": {tc.responseAuth},
				"Vary":          {tc.vary},
				"Etag":          {tc.etag},
			})
			h := &Handler{Shared: tc.shared}
			if got := h.isCacheable(res, &cacheRequest{Request: req}); got != tc.want {
				t.Errorf("isCacheable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSameOriginURLReferenceForms(t *testing.T) {
	for _, tc := range []struct {
		name       string
		requestURL string
		host       string
		tls        bool
		raw        string
		want       string
	}{
		{name: "malformed target", requestURL: "/orders/1", host: "example.org", raw: "%"},
		{name: "invalid target port", requestURL: "/orders/1", host: "example.org", raw: "http://example.org:bad/item"},
		{name: "overflow target port", requestURL: "/orders/1", host: "example.org", raw: "http://example.org:65536/item"},
		{name: "missing target hostname", requestURL: "/orders/1", host: "example.org", raw: "http://:80/item"},
		{name: "invalid request authority", requestURL: "/orders/1", host: "[", raw: "http://example.org/item"},
		{name: "missing request hostname", requestURL: "/orders/1", host: ":80", raw: "http://example.org/item"},
		{name: "overflow request port", requestURL: "/orders/1", host: "example.org:65536", raw: "http://example.org/item"},
		{name: "request URL authority fallback", requestURL: "http://example.org/orders/1", raw: "http://example.org/item", want: "http://example.org/item"},
		{name: "http default port", requestURL: "/orders/1", host: "example.org", raw: "http://EXAMPLE.org:00080/item", want: "/item"},
		{name: "tls origin scheme", requestURL: "/orders/1", host: "example.org", tls: true, raw: "https://example.org:443/item", want: "/item"},
		{name: "tls cross scheme", requestURL: "/orders/1", host: "example.org", tls: true, raw: "http://example.org/item"},
		{name: "network reference inherits tls", requestURL: "/orders/1", host: "example.org", tls: true, raw: "//example.org:443/item?", want: "/item?"},
		{name: "network reference empty path", requestURL: "/orders/1", host: "example.org", raw: "//example.org?q", want: "/?q"},
		{name: "relative query retains request path", requestURL: "/orders/1?old", host: "example.org", raw: "?q", want: "/orders/1?q"},
		{name: "relative fragment retains request query", requestURL: "/orders/1?old", host: "example.org", raw: "#fragment", want: "/orders/1?old"},
		{name: "escaped path and empty query", requestURL: "/orders/1?old", host: "example.org", raw: "/objects/a%2Fb?#fragment", want: "/objects/a%2Fb?"},
		{name: "absolute authority spelling", requestURL: "https://example.org/orders/1", host: "example.org", raw: "https://user@example.org:00443/item", want: "https://user@example.org:00443/item"},
		{name: "ipv6 default port", requestURL: "/orders/1", host: "[2001:db8::1]", tls: true, raw: "https://[2001:db8::1]:00443/item", want: "/item"},
		{name: "different hostname", requestURL: "/orders/1", host: "example.org", raw: "http://elsewhere.org/item"},
		{name: "different effective port", requestURL: "/orders/1", host: "example.org", raw: "http://example.org:8080/item"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.requestURL, nil)
			req.Host = tc.host
			if tc.tls {
				req.TLS = &tls.ConnectionState{}
			}
			target := (&cacheRequest{Request: req}).sameOriginURL(tc.raw)
			if tc.want == "" {
				if target != nil {
					t.Fatalf("sameOriginURL = %s, want nil", target)
				}
				return
			}
			if target == nil {
				t.Fatal("sameOriginURL returned nil for a same-origin target")
			}
			if got := target.String(); got != tc.want {
				t.Errorf("target = %q, want %q", got, tc.want)
			}
			direct := httptest.NewRequest(http.MethodGet, tc.want, nil)
			if got, want := NewKey(http.MethodGet, target, req.Header).String(), NewRequestKey(direct).String(); got != want {
				t.Errorf("invalidation key = %q, direct request key = %q", got, want)
			}
		})
	}
}

func TestNormalizedOriginAuthority(t *testing.T) {
	for _, tc := range []struct {
		name, scheme, authority, host, port string
		ok                                  bool
	}{
		{name: "http default", scheme: "HTTP", authority: "example.org", host: "example.org", port: "80", ok: true},
		{name: "https default", scheme: "HTTPS", authority: "example.org", host: "example.org", port: "443", ok: true},
		{name: "explicit normalized port", scheme: "https", authority: "example.org:00443", host: "example.org", port: "443", ok: true},
		{name: "unknown scheme no default", scheme: "custom", authority: "example.org", host: "example.org", ok: true},
		{name: "ipv6 authority", scheme: "https", authority: "[2001:db8::1]", host: "2001:db8::1", port: "443", ok: true},
		{name: "empty authority", scheme: "http"},
		{name: "malformed authority", scheme: "http", authority: "["},
		{name: "no hostname", scheme: "http", authority: ":80"},
		{name: "invalid port", scheme: "http", authority: "example.org:bad"},
		{name: "overflow port", scheme: "http", authority: "example.org:65536"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, port, ok := normalizedOriginAuthority(tc.scheme, tc.authority)
			if host != tc.host || port != tc.port || ok != tc.ok {
				t.Errorf("normalized authority = (%q, %q, %v), want (%q, %q, %v)", host, port, ok, tc.host, tc.port, tc.ok)
			}
		})
	}
}

type lookupReply struct {
	res *Resource
	err error
}

type lookupReplyCache struct {
	Cache
	replies map[string]lookupReply
	keys    []string
}

func (c *lookupReplyCache) Retrieve(key string) (*Resource, error) {
	c.keys = append(c.keys, key)
	reply, ok := c.replies[key]
	if !ok {
		return nil, ErrNotFoundInCache
	}
	return reply.res, reply.err
}

type lookupTrackedBody struct {
	*bytes.Reader
	closes int
}

func (b *lookupTrackedBody) Close() error {
	b.closes++
	return nil
}

func TestLookupHEADFallbackPolicy(t *testing.T) {
	retrieveFailure := errors.New("retrieve failed")
	for _, tc := range []struct {
		name, policy, requestPolicy string
		retrieveErr, wantErr        error
		wantResource                bool
		wantCloses                  int
	}{
		{name: "explicit expiration", policy: "max-age=60", wantResource: true},
		{name: "zero explicit expiration still eligible", policy: "max-age=0", wantResource: true},
		{name: "shared lifetime ignored by private cache", policy: "s-maxage=60", wantErr: ErrNotFoundInCache, wantCloses: 1},
		{name: "no explicit expiration", wantErr: ErrNotFoundInCache, wantCloses: 1},
		{name: "request forbids cache", policy: "max-age=60", requestPolicy: "no-cache", wantErr: ErrNotFoundInCache, wantCloses: 1},
		{name: "get missing", retrieveErr: ErrNotFoundInCache, wantErr: ErrNotFoundInCache},
		{name: "get retrieval failure", retrieveErr: retrieveFailure, wantErr: retrieveFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodHead, "http://example.org/item", nil)
			req.Header.Set("Cache-Control", tc.requestPolicy)
			cacheReq, err := newCacheRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			body := &lookupTrackedBody{Reader: bytes.NewReader([]byte("cached"))}
			res := NewResource(http.StatusOK, body, http.Header{"Cache-Control": {tc.policy}})
			getKey := cacheReq.Key.ForMethod(http.MethodGet).String()
			cache := &lookupReplyCache{replies: map[string]lookupReply{getKey: {res: res, err: tc.retrieveErr}}}
			h := &Handler{cache: cache}
			got, err := h.lookup(cacheReq)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("lookup error = %v, want %v", err, tc.wantErr)
			}
			if (got == res) != tc.wantResource {
				t.Errorf("returned fallback resource = %v, want %v", got == res, tc.wantResource)
			}
			if body.closes != tc.wantCloses {
				t.Errorf("fallback body closes = %d, want %d", body.closes, tc.wantCloses)
			}
			wantServedKey := ""
			if tc.wantResource {
				wantServedKey = getKey
			}
			if cacheReq.servedKey != wantServedKey {
				t.Errorf("servedKey = %q, want %q", cacheReq.servedKey, wantServedKey)
			}
			if want := []string{cacheReq.Key.String(), getKey}; !reflect.DeepEqual(cache.keys, want) {
				t.Errorf("retrieved keys = %q, want %q", cache.keys, want)
			}
		})
	}
}

func TestLookupPreservesInitialRetrieveError(t *testing.T) {
	retrieveFailure := errors.New("retrieve failed")
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			req, err := newCacheRequest(httptest.NewRequest(method, "http://example.org/item", nil))
			if err != nil {
				t.Fatal(err)
			}
			body := &lookupTrackedBody{Reader: bytes.NewReader(nil)}
			res := NewResource(http.StatusOK, body, http.Header{})
			cache := &lookupReplyCache{replies: map[string]lookupReply{req.Key.String(): {res: res, err: retrieveFailure}}}
			got, err := (&Handler{cache: cache}).lookup(req)
			if got != res || !errors.Is(err, retrieveFailure) {
				t.Errorf("lookup = (%v, %v), want the original resource and error", got, err)
			}
			if body.closes != 0 || len(cache.keys) != 1 {
				t.Errorf("error lookup performed extra work: closes=%d, retrieved keys=%q", body.closes, cache.keys)
			}
		})
	}
}

func TestLookupVaryOwnershipAndInvalidation(t *testing.T) {
	retrieveFailure := errors.New("variant retrieve failed")
	for _, tc := range []struct {
		name                     string
		vary                     string
		variantErr, wantErr      error
		noVariant                bool
		baseStale, marked, newer bool
		wantStale, wantServedKey bool
		wantLookups              int
	}{
		{name: "wildcard rejected", vary: "Accept-Language, *", wantErr: ErrNotFoundInCache, noVariant: true, wantLookups: 1},
		{name: "variant retrieve failure", vary: "Accept-Language", variantErr: retrieveFailure, wantErr: retrieveFailure, noVariant: true, wantLookups: 2},
		{name: "variant missing", vary: "Accept-Language", variantErr: ErrNotFoundInCache, wantErr: ErrNotFoundInCache, noVariant: true, wantLookups: 2},
		{name: "nil variant from external cache", vary: "Accept-Language", noVariant: true, wantServedKey: true, wantLookups: 2},
		{name: "unmarked fresh variant", vary: "Accept-Language", wantServedKey: true, wantLookups: 2},
		{name: "base stale fallback", vary: "Accept-Language", baseStale: true, wantStale: true, wantServedKey: true, wantLookups: 2},
		{name: "marker invalidates old variant", vary: "Accept-Language", marked: true, wantStale: true, wantServedKey: true, wantLookups: 2},
		{name: "new variant survives stale base", vary: "Accept-Language", baseStale: true, marked: true, newer: true, wantServedKey: true, wantLookups: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := newCacheRequest(httptest.NewRequest(http.MethodGet, "http://example.org/item", nil))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Accept-Language", "en")
			baseBody := &lookupTrackedBody{Reader: bytes.NewReader(nil)}
			base := NewResource(http.StatusOK, baseBody, http.Header{"Vary": {tc.vary}})
			base.stale = tc.baseStale
			variantBody := &lookupTrackedBody{Reader: bytes.NewReader(nil)}
			variant := NewResource(http.StatusOK, variantBody, http.Header{})
			if tc.noVariant {
				variant = nil
			}
			baseKey := req.Key.String()
			variantKey := req.Key.Vary(tc.vary, req.Request).String()
			cache := &lookupReplyCache{replies: map[string]lookupReply{
				baseKey:    {res: base},
				variantKey: {res: variant, err: tc.variantErr},
			}}
			h := &Handler{cache: cache}
			if tc.marked {
				h.localStale = map[string]time.Time{baseKey: req.Time}
			}
			if tc.newer {
				h.localFresh = map[string]*localFreshProof{variantKey: {at: req.Time.Add(time.Second)}}
			}
			got, err := h.lookup(req)
			if got != variant || !errors.Is(err, tc.wantErr) {
				t.Errorf("lookup = (%v, %v), want (%v, %v)", got, err, variant, tc.wantErr)
			}
			if baseBody.closes != 1 || variantBody.closes != 0 {
				t.Errorf("body closes: base=%d variant=%d, want base=1 variant=0", baseBody.closes, variantBody.closes)
			}
			if variant != nil && variant.IsStale() != tc.wantStale {
				t.Errorf("variant stale = %v, want %v", variant.IsStale(), tc.wantStale)
			}
			if (req.servedKey == variantKey) != tc.wantServedKey {
				t.Errorf("servedKey = %q, want variant key recorded = %v", req.servedKey, tc.wantServedKey)
			}
			if len(cache.keys) != tc.wantLookups {
				t.Errorf("retrieved keys = %q, want %d lookups", cache.keys, tc.wantLookups)
			}
		})
	}
}

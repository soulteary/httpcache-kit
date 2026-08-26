package httpcache

import (
	"net/http"
	"net/url"
	"strings"
)

// Key represents a unique identifier for a resource in the cache
type Key struct {
	method string
	header http.Header
	u      url.URL
	vary   []string
}

// NewKey returns a new Key instance
func NewKey(method string, u *url.URL, h http.Header) Key {
	return Key{method: method, header: h, u: *u, vary: []string{}}
}

// NewRequestKey generates a Key for a request
func NewRequestKey(r *http.Request) Key {
	URL := r.URL

	if location := r.Header.Get("Content-Location"); location != "" {
		u, err := url.Parse(location)
		if err == nil {
			if !u.IsAbs() {
				u = r.URL.ResolveReference(u)
			}
			if u.Host != r.Host {
				debugf("illegal host %q in Content-Location", u.Host)
			} else {
				debugf("using Content-Location: %q", u.String())
				URL = u
			}
		} else {
			debugf("failed to parse Content-Location %q", location)
		}
	}

	return NewKey(r.Method, URL, r.Header)
}

// ForMethod returns a new Key with a given method
func (k Key) ForMethod(method string) Key {
	k2 := k
	k2.method = method
	return k2
}

// Vary returns a Key that is varied on particular headers in a http.Request
func (k Key) Vary(varyHeader string, r *http.Request) Key {
	k2 := k

	for _, header := range parseVary(varyHeader) {
		k2.vary = append(k2.vary, header+"="+r.Header.Get(header))
	}

	return k2
}

func (k Key) String() string {
	URL := canonicalURL(&k.u).String()
	var b strings.Builder
	b.Grow(len(k.method) + 1 + len(URL) + 3 + 10*len(k.vary)) // heuristic to reduce allocs
	b.WriteString(k.method)
	b.WriteString(":")
	b.WriteString(URL)
	if len(k.vary) > 0 {
		b.WriteString("::")
		for _, v := range k.vary {
			b.WriteString(v)
			b.WriteString(":")
		}
	}
	return b.String()
}

func canonicalURL(u *url.URL) *url.URL {
	// URI schemes and host names are case-insensitive, but paths and query
	// strings are not. Lower-casing the entire URL aliases distinct repository
	// objects and can make one response overwrite another in a shared cache.
	canonical := *u
	canonical.Scheme = strings.ToLower(canonical.Scheme)
	canonical.Host = strings.ToLower(canonical.Host)
	canonical.Fragment = ""
	return &canonical
}

func parseVary(varyHeader string) []string {
	parts := strings.Split(varyHeader, ",")
	headers := make([]string, 0, len(parts))
	for _, part := range parts {
		header := http.CanonicalHeaderKey(strings.TrimSpace(part))
		if header != "" {
			headers = append(headers, header)
		}
	}
	return headers
}

func varyWildcard(varyHeader string) bool {
	for _, header := range parseVary(varyHeader) {
		if header == "*" {
			return true
		}
	}
	return false
}

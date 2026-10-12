package httpcache

import (
	"bytes"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	CacheControlHeader = "Cache-Control"
)

type CacheControl map[string][]string

func ParseCacheControlHeaders(h http.Header) (CacheControl, error) {
	// HTTP field names are case-insensitive, including headers constructed
	// directly by an embedding application rather than net/http.
	var names []string
	for name := range h {
		if strings.EqualFold(name, CacheControlHeader) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var values []string
	for _, name := range names {
		values = append(values, h[name]...)
	}
	return ParseCacheControl(strings.Join(values, ", "))
}

func ParseCacheControl(input string) (CacheControl, error) {
	cc := make(CacheControl)
	length := len(input)
	isValue := false
	lastKey := ""

	for pos := 0; pos < length; pos++ {
		var token string
		switch input[pos] {
		case '"':
			if offset := strings.IndexAny(input[pos+1:], `"`); offset != -1 {
				token = input[pos+1 : pos+1+offset]
			} else {
				token = input[pos+1:]
			}
			pos += len(token) + 1
		case ',', '\n', '\r', ' ', '\t':
			continue
		case '=':
			isValue = true
			continue
		default:
			if offset := strings.IndexAny(input[pos:], "\"\n\t\r ,="); offset != -1 {
				token = input[pos : pos+offset]
			} else {
				token = input[pos:]
			}
			pos += len(token) - 1
		}
		if isValue {
			cc.Add(lastKey, token)
			isValue = false
		} else {
			cc.Add(token, "")
			lastKey = token
		}
	}

	return cc, nil
}

func (cc CacheControl) Get(key string) (string, bool) {
	storedKey, exists := cc.lookupKey(key)
	v := cc[storedKey]
	if exists && len(v) > 0 {
		return v[0], true
	}
	return "", exists
}

func (cc CacheControl) Add(key, val string) {
	if storedKey, exists := cc.lookupKey(key); exists {
		// Preserve the spelling of entries constructed through the exported
		// map type, rather than creating a second directive under a new key.
		key = storedKey
	} else {
		key = strings.ToLower(key)
		cc[key] = []string{}
	}
	if val != "" {
		cc[key] = append(cc[key], val)
	}
}

func (cc CacheControl) Has(key string) bool {
	_, exists := cc.lookupKey(key)
	return exists
}

// Parsed directives use lowercase keys, but callers may also build this
// exported map directly. Keep the common lookup fast and recognize existing
// mixed-case entries without mutating the map during reads.
func (cc CacheControl) lookupKey(key string) (string, bool) {
	lower := strings.ToLower(key)
	if _, exists := cc[lower]; exists {
		return lower, true
	}
	if _, exists := cc[key]; exists {
		return key, true
	}
	var found string
	for candidate := range cc {
		if strings.EqualFold(candidate, key) && (found == "" || candidate < found) {
			found = candidate
		}
	}
	return found, found != ""
}

func (cc CacheControl) Duration(key string) (time.Duration, error) {
	d, _ := cc.Get(key)
	return time.ParseDuration(d + "s")
}

func (cc CacheControl) String() string {
	keys := make([]string, 0, len(cc))
	for k := range cc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	buf := bytes.Buffer{}

	for _, k := range keys {
		vals := cc[k]
		if len(vals) == 0 {
			buf.WriteString(k + ", ")
		}
		for _, val := range vals {
			fmt.Fprintf(&buf, "%s=%q, ", k, val)
		}
	}

	return strings.TrimSuffix(buf.String(), ", ")
}

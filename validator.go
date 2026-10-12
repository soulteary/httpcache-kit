package httpcache

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

type Validator struct {
	Handler http.Handler
}

func (v *Validator) Validate(req *http.Request, res *Resource) bool {
	if req.Context().Err() != nil {
		return false
	}
	outreq, ok := conditionalRequest(req, res)
	if !ok {
		return false
	}

	t := Clock()
	// A conditional GET can return a complete new representation. This
	// boolean API cannot reuse that body, so discard it instead of buffering
	// a potentially package-sized response in an httptest recorder.
	resp := &validationResponse{header: make(http.Header)}
	v.Handler.ServeHTTP(resp, outreq)
	return applyValidationResponse(req, res, resp.status, resp.Header(), t)
}

func conditionalRequest(req *http.Request, res *Resource) (*http.Request, bool) {
	// Replacing a field named by Vary selects another representation. Such
	// entries must be fetched with the client's original conditions instead.
	if varyUsesClientValidators(combinedHeaderValues(res.Header(), "Vary")) {
		return nil, false
	}
	outreq := cloneRequest(req)
	headers := canonicalValidationHeaders(res.Header())
	switch {
	case headers.Get("Etag") != "":
		outreq.Header.Set("If-None-Match", headers.Get("Etag"))
	case headers.Get("Last-Modified") != "":
		outreq.Header.Set("If-Modified-Since", headers.Get("Last-Modified"))
	default:
		return nil, false
	}
	return outreq, true
}

func varyUsesClientValidators(vary string) bool {
	for _, field := range parseVary(vary) {
		if field == "If-None-Match" || field == "If-Modified-Since" {
			return true
		}
	}
	return false
}

// A newly received Vary can reveal that the validation request selected a
// different representation. Compare the exact field values used by Vary keys,
// including absent fields, empty fields and multiple field lines.
func varyChangedClientValidators(vary string, original, validation *http.Request) bool {
	for _, field := range parseVary(vary) {
		if (field == "If-None-Match" || field == "If-Modified-Since") &&
			varyFieldKey(original.Header, field) != varyFieldKey(validation.Header, field) {
			return true
		}
	}
	return false
}

func applyValidationResponse(req *http.Request, res *Resource, status int, header http.Header, t time.Time) bool {
	resHeaders := canonicalValidationHeaders(res.Header())
	responseHeaders := canonicalValidationHeaders(header)
	if req.Context().Err() != nil || status != http.StatusNotModified || !headersEqual(resHeaders, responseHeaders) {
		return false
	}

	// A 304 carries metadata updates for the stored representation, not a
	// replacement header set. Preserve omitted fields such as Content-Length,
	// Content-Type and Content-Encoding so the old body remains interpretable
	// and verifiable. Age describes this validation, never the previous fetch.
	merged := resHeaders.Clone()
	merged.Del("Age")
	for name, values := range responseHeaders {
		if name == "Warning" {
			continue
		}
		merged[name] = append([]string(nil), values...)
	}
	// RFC 7234 section 4.3.4 requires successful validation to discard 1xx
	// warnings and retain stored 2xx warnings. Unlike other metadata, new
	// Warning values supplement the retained values rather than replace them.
	warnings := append([]string(nil), resHeaders.Values("Warning")...)
	warnings = append(warnings, responseHeaders.Values("Warning")...)
	if warnings = validatedWarningValues(warnings); len(warnings) > 0 {
		merged["Warning"] = warnings
	} else {
		merged.Del("Warning")
	}
	if responseHeaders.Get("Date") == "" {
		merged.Set("Date", Clock().Format(http.TimeFormat))
	}
	if age, err := correctedAge(merged, t, Clock()); err == nil {
		merged.Set("Age", fmt.Sprintf("%.f", age.Seconds()))
	}

	res.header = merged
	res.cc = nil // Cache-Control may have changed in the 304 response.
	res.header.Set(ProxyDateHeader, Clock().Format(http.TimeFormat))
	// Preserve a full-precision local ordering point. Freshen uses it to
	// reject a validation that began before a concurrent mutation, rather
	// than treating its later header write as a newer representation.
	res.RequestTime = t
	return true
}

// Warning is a comma-separated list, but both its quoted text and optional
// quoted HTTP-date can contain commas. Honor quoted-pair escapes while finding
// list boundaries so retaining a 2xx value never changes its quoted content.
func validatedWarningValues(fields []string) []string {
	var result []string
	seen := make(map[string]struct{})
	for _, field := range fields {
		var retained []string
		for _, value := range splitWarningValues(field) {
			value = strings.Trim(value, " \t")
			if value == "" || isTransientWarning(value) {
				continue
			}
			// A 304 may repeat the same persistent warning on every validation.
			// Keep its first occurrence without growing the stored header forever.
			if _, exists := seen[value]; exists {
				continue
			}
			seen[value] = struct{}{}
			retained = append(retained, value)
		}
		if len(retained) > 0 {
			result = append(result, strings.Join(retained, ", "))
		}
	}
	return result
}

func isTransientWarning(value string) bool {
	return len(value) >= 4 && value[0] == '1' && value[1] >= '0' && value[1] <= '9' &&
		value[2] >= '0' && value[2] <= '9' && (value[3] == ' ' || value[3] == '\t')
}

func splitWarningValues(field string) []string {
	var values []string
	start, quoted, escaped := 0, false, false
	for i := 0; i < len(field); i++ {
		switch {
		case escaped:
			escaped = false
		case quoted && field[i] == '\\':
			escaped = true
		case field[i] == '"':
			quoted = !quoted
		case field[i] == ',' && !quoted:
			values = append(values, field[start:i])
			start = i + 1
		}
	}
	return append(values, field[start:])
}

// Embedding handlers may assign raw map keys rather than using Header.Set.
// Canonicalize both generations before comparing or replacing fields so a
// lowercase update cannot coexist with the old canonical name. Multiple field
// lines within a single generation retain their values and deterministic order.
func canonicalValidationHeaders(header http.Header) http.Header {
	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, name)
	}
	sort.Strings(names)
	canonical := make(http.Header, len(header))
	for _, name := range names {
		key := http.CanonicalHeaderKey(name)
		canonical[key] = append(canonical[key], header[name]...)
	}
	return canonical
}

var validationHeaders = []string{"ETag", "Content-MD5", "Last-Modified", "Content-Length", "Content-Encoding"}

// validationResponse intentionally retains no body. Preserve the first final
// status while allowing informational responses before it, as net/http does.
type validationResponse struct {
	header http.Header
	status int
}

func (w *validationResponse) Header() http.Header { return w.header }
func (w *validationResponse) WriteHeader(status int) {
	if status < 200 || w.status != 0 {
		return
	}
	w.status = status
}
func (w *validationResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return len(p), nil
}
func (w *validationResponse) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
}

func headersEqual(h1, h2 http.Header) bool {
	for _, header := range validationHeaders {
		if value := h2.Get(header); value != "" {
			if h1.Get(header) != value && (header != "Content-Encoding" || !strings.EqualFold(h1.Get(header), value)) {
				debugf("%s changed, %q != %q", header, value, h1.Get(header))
				return false
			}
		}
	}

	return true
}

// cloneRequest returns a clone of the provided *http.Request.
// The clone is a shallow copy of the struct and its Header map.
func cloneRequest(r *http.Request) *http.Request {
	r2 := new(http.Request)
	*r2 = *r
	r2.Header = make(http.Header)
	for k, s := range r.Header {
		r2.Header[k] = s
	}
	return r2
}

package httpcache

import (
	"bytes"
	"net/http"
)

// Validation uses the cache's validator. Delivery must still apply the client's
// original preconditions to the new representation, while storage receives the
// complete representation. ServeContent supplies the same ETag/date semantics
// as hits, including for successful responses without content.
type clientPreconditionWriter struct {
	http.ResponseWriter
	req      *http.Request
	header   http.Header
	suppress bool
}

func newClientPreconditionWriter(w http.ResponseWriter, req *http.Request) http.ResponseWriter {
	if req.Header.Get("If-None-Match") == "" && req.Header.Get("If-Modified-Since") == "" {
		return w
	}
	return &clientPreconditionWriter{ResponseWriter: w, req: req, header: w.Header().Clone()}
}
func (w *clientPreconditionWriter) Header() http.Header { return w.header }
func (w *clientPreconditionWriter) WriteHeader(status int) {
	header := w.header
	// Evaluate the client's conditions for every successful response, including
	// bodyless 204/205 responses. Redirects and errors take precedence (RFC 9110
	// section 13.2.1), even when they carry matching validators.
	if status >= http.StatusOK && status < http.StatusMultipleChoices {
		probe := &preconditionProbe{header: header.Clone()}
		req := w.req.Clone(w.req.Context())
		req.Header.Del("Range") // The upstream has already applied the range.
		modified, _ := http.ParseTime(header.Get("Last-Modified"))
		http.ServeContent(probe, req, "", modified, bytes.NewReader(nil))
		if probe.status == http.StatusNotModified || probe.status == http.StatusPreconditionFailed {
			status, header, w.suppress = probe.status, probe.header, true
		}
	}
	dst := w.ResponseWriter.Header()
	for name := range dst {
		delete(dst, name)
	}
	for name, values := range header {
		dst[name] = append([]string(nil), values...)
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *clientPreconditionWriter) Write(body []byte) (int, error) {
	if w.suppress {
		return len(body), nil
	}
	return w.ResponseWriter.Write(body)
}

type preconditionProbe struct {
	header http.Header
	status int
}

func (p *preconditionProbe) Header() http.Header         { return p.header }
func (p *preconditionProbe) WriteHeader(status int)      { p.status = status }
func (p *preconditionProbe) Write(b []byte) (int, error) { return len(b), nil }

// Trailers arrive after the initial client header copy. Keep the storage header
// map private, then publish only declared/final trailer fields to the network.
func (w *clientPreconditionWriter) finishTrailers() {
	if !w.suppress {
		copyValidationTrailers(w.ResponseWriter.Header(), w.header, w.header)
	}
}

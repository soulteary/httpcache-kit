package httpcache

import (
	"bytes"
	"net/http"
	"strings"
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
	// A client condition can suppress delivery while the complete origin body
	// still needs bounded staging. Delay only that response's metadata so a
	// later refusal can be reported without relying on body trailers.
	deferStaging  bool
	pendingStatus int
	pendingHeader http.Header
}

func newClientPreconditionWriter(w http.ResponseWriter, req *http.Request) http.ResponseWriter {
	if req.Header.Get("If-None-Match") == "" && req.Header.Get("If-Modified-Since") == "" &&
		req.Header.Get("If-Match") == "" && req.Header.Get("If-Unmodified-Since") == "" {
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
	if w.deferStaging && w.suppress {
		w.pendingStatus, w.pendingHeader = status, header
		return
	}
	w.commitHeader(status, header)
}

func (w *clientPreconditionWriter) commitHeader(status int, header http.Header) {
	dst := w.ResponseWriter.Header()
	for name := range dst {
		delete(dst, name)
	}
	for name, values := range header {
		dst[name] = append([]string(nil), values...)
	}
	w.ResponseWriter.WriteHeader(status)
}

// DeferStagingHeaders lets the handler postpone a conditional response with
// no delivered body until its synchronous staging outcome is known.
func (w *clientPreconditionWriter) DeferStagingHeaders() { w.deferStaging = true }

// FinishStagingHeaders commits a suppressed response once. Preserve the
// precondition probe's bodyless header filtering and copy only the cache
// metadata that the main path is allowed to update after origin WriteHeader.
// Origin trailer updates remain private and never become initial headers.
func (w *clientPreconditionWriter) FinishStagingHeaders() {
	if w.pendingStatus == 0 {
		w.finishTrailers()
		return
	}
	header := w.pendingHeader
	for _, name := range []string{CacheHeader, "Age", ProxyDateHeader} {
		if values, ok := w.header[name]; ok {
			header[name] = append([]string(nil), values...)
		} else {
			delete(header, name)
		}
	}
	header.Del("Trailer")
	header.Del(CacheStagingHeader)
	// A 412 probe can retain the origin's full-body length. The delivered
	// response contains no body, so that framing must not reach the client.
	header.Del("Content-Length")
	header.Del("Transfer-Encoding")
	for name := range header {
		if strings.HasPrefix(name, http.TrailerPrefix) {
			delete(header, name)
		}
	}
	status := w.pendingStatus
	w.pendingStatus, w.pendingHeader = 0, nil
	w.commitHeader(status, header)
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

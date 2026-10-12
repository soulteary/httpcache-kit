package httpcache

import (
	"context"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Combine every field line, including raw non-canonical map keys supplied by
// embedding handlers. A missing field and an empty field remain conservative
// cache misses when their representation differs.
func combinedHeaderValues(header http.Header, name string) string {
	var keys []string
	for key := range header {
		if strings.EqualFold(key, name) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var values []string
	for _, key := range keys {
		values = append(values, header[key]...)
	}
	return strings.Join(values, ", ")
}

// validateUpstreamResponse keeps a full validation response in the ordinary
// bounded streaming/store path. The public boolean Validator remains compatible
// with callers interested only in whether their existing resource was validated.
func (h *Handler) validateUpstreamResponse(w http.ResponseWriter, req *cacheRequest, res *Resource) (bool, bool) {
	out, ok := conditionalRequest(req.Request, res)
	if !ok || req.Context().Err() != nil {
		return false, false
	}
	// The validation may replace this representation with a full response.
	// Fence its predecessor before fetching, so failed staging or publication
	// cannot leave the old body reusable. A Vary discovery base can contain a
	// different fresh variant, so fence only this lookup's selected resource.
	// Starting after this barrier preserves a real mutation's ordering.
	key := req.validationResourceKey()
	h.recordStale(key)
	h.cache.Invalidate(key)
	if req.Context().Err() != nil {
		return false, false
	}
	start := Clock()
	validation := *req
	validation.Request, validation.Time = out, start
	exchange := newValidationExchange()
	stop := context.AfterFunc(req.Context(), func() { _ = exchange.reader.CloseWithError(req.Context().Err()) })
	defer stop()
	defer func() { _ = exchange.reader.Close() }()
	go exchange.serve(h.validator.Handler, out)
	select {
	case <-exchange.ready:
	case <-req.Context().Done():
		return false, false
	}
	if exchange.status == http.StatusNotModified {
		if _, err := io.Copy(io.Discard, exchange.reader); err != nil {
			return false, false
		}
		return applyValidationResponse(req.Request, res, exchange.status, exchange.snapshot, start), false
	}
	if retryOriginalPreconditions(exchange.status, req.Request, out) {
		// The rewritten validator did not establish that the stored body is
		// current. Retrying the client's validator can return 304 for a newer
		// representation, which cannot refresh our older body. Fence the old
		// generation before that retry so later readers must validate it too.
		keys := req.validationKeys()
		h.recordStale(keys...)
		h.cache.Invalidate(keys...)
		// Error responses can omit representation validators, so the delivery
		// wrapper cannot reconstruct the result of the original conditions.
		// Closing the pipe also releases a producer blocked on the error body.
		_ = exchange.reader.Close()
		return false, false
	}
	if varyChangedClientValidators(combinedHeaderValues(exchange.snapshot, "Vary"), req.Request, out) {
		// A rewritten selecting validator can produce another representation.
		// Preserve the original request when retrying that selection.
		_ = exchange.reader.Close()
		return false, false
	}
	if h.fullResponseVaryChanged(&validation, exchange.status, res.Header(), exchange.snapshot) {
		// Rebuild with a complete body even if the client's condition matches
		// the replacement. Use its new Vary policy to decide which conditions
		// can be removed, and apply the original conditions only to delivery.
		// The rebuild fences all old keys before starting a genuine new fetch.
		_ = exchange.reader.Close()
		h.refetchVaryResponse(w, req, combinedHeaderValues(exchange.snapshot, "Vary"))
		return false, true
	}
	client := newClientPreconditionWriter(w, req.Request)
	copied := make(chan bool, 1)
	h.passUpstreamWithHandler(client, &validation, nil, http.HandlerFunc(func(dst http.ResponseWriter, _ *http.Request) {
		for name, values := range exchange.snapshot {
			dst.Header()[name] = append([]string(nil), values...)
		}
		dst.WriteHeader(exchange.status)
		if _, err := io.Copy(dst, exchange.reader); err != nil {
			if stream, ok := dst.(*responseStreamer); ok {
				_ = stream.pipeWriter.CloseWithError(err)
			}
			copied <- false
			return
		}
		copied <- true
	}), start)
	finishValidationTrailers(req.Context(), client, exchange, copied)
	return false, true
}

func (h *Handler) fullResponseVaryChanged(req *cacheRequest, status int, previous, replacement http.Header) bool {
	if sameVaryFields(combinedHeaderValues(previous, "Vary"), combinedHeaderValues(replacement, "Vary")) {
		return false
	}
	// Only a response eligible for storage needs a rebuilt discovery index.
	// Deliver an uncacheable replacement as received; the selected predecessor
	// was already fenced before this fetch, so no extra origin request is needed.
	return h.isCacheable(NewResource(status, nil, replacement), req)
}

func (req *cacheRequest) validationResourceKey() string {
	if req.servedKey != "" {
		return req.servedKey
	}
	if req.discoveryKey != "" {
		return req.discoveryKey
	}
	return req.Key.String()
}

func finishValidationTrailers(ctx context.Context, client http.ResponseWriter, exchange *validationExchange, copied <-chan bool) {
	// An uncacheable response can update X-Cache after publishing its initial
	// headers. Publish final trailers only after both the copy and that main
	// path have finished, so they never read or write the header map together.
	select {
	case complete := <-copied:
		if !complete {
			return
		}
		copyValidationTrailers(client.Header(), exchange.header, exchange.snapshot)
		if filtered, ok := client.(interface{ finishTrailers() }); ok {
			filtered.finishTrailers()
		}
	case <-ctx.Done():
	}
}

func retryOriginalPreconditions(status int, original, validation *http.Request) bool {
	if status != http.StatusRequestedRangeNotSatisfiable && status != http.StatusPreconditionFailed {
		return false
	}
	if original.Header.Get("If-None-Match") == "" && original.Header.Get("If-Modified-Since") == "" {
		return false
	}
	return combinedHeaderValues(original.Header, "If-None-Match") != combinedHeaderValues(validation.Header, "If-None-Match") ||
		combinedHeaderValues(original.Header, "If-Modified-Since") != combinedHeaderValues(validation.Header, "If-Modified-Since")
}

func copyValidationTrailers(dst, final, initial http.Header) {
	for _, name := range parseVary(combinedHeaderValues(initial, "Trailer")) {
		if values := final.Values(name); len(values) > 0 {
			dst[name] = append([]string(nil), values...)
		}
	}
	for name, values := range final {
		if strings.HasPrefix(name, http.TrailerPrefix) {
			dst[name] = append([]string(nil), values...)
		}
	}
}

type validationExchange struct {
	header, snapshot http.Header
	status           int
	ready            chan struct{}
	once             sync.Once
	reader           *io.PipeReader
	writer           *io.PipeWriter
}

func newValidationExchange() *validationExchange {
	reader, writer := io.Pipe()
	return &validationExchange{header: make(http.Header), ready: make(chan struct{}), reader: reader, writer: writer}
}

func (e *validationExchange) Header() http.Header { return e.header }
func (e *validationExchange) WriteHeader(status int) {
	if status < 200 {
		return
	}
	e.once.Do(func() { e.status = status; e.snapshot = e.header.Clone(); close(e.ready) })
}
func (e *validationExchange) Write(body []byte) (int, error) {
	e.WriteHeader(http.StatusOK)
	return e.writer.Write(body)
}
func (e *validationExchange) Flush() { e.WriteHeader(http.StatusOK) }
func (e *validationExchange) serve(handler http.Handler, req *http.Request) {
	defer func() {
		if recovered := recover(); recovered != nil {
			e.WriteHeader(http.StatusBadGateway)
			_ = e.writer.CloseWithError(io.ErrUnexpectedEOF)
			return
		}
		e.WriteHeader(http.StatusOK)
		_ = e.writer.Close()
	}()
	handler.ServeHTTP(e, req)
}

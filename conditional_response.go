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
	start := Clock()
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
	if exchange.status == http.StatusPartialContent {
		keys := req.validationKeys()
		h.recordStale(keys...)
		h.cache.Invalidate(keys...)
	}
	h.passUpstreamWithHandler(newClientPreconditionWriter(w, req.Request), req, nil, http.HandlerFunc(func(dst http.ResponseWriter, _ *http.Request) {
		for name, values := range exchange.snapshot {
			dst.Header()[name] = append([]string(nil), values...)
		}
		dst.WriteHeader(exchange.status)
		if _, err := io.Copy(dst, exchange.reader); err != nil {
			if stream, ok := dst.(*responseStreamer); ok {
				_ = stream.pipeWriter.CloseWithError(err)
			}
		} else {
			copyValidationTrailers(dst.Header(), exchange.header, exchange.snapshot)
			if stream, ok := dst.(*responseStreamer); ok {
				if filtered, ok := stream.ResponseWriter.(interface{ finishTrailers() }); ok {
					filtered.finishTrailers()
				}
			}
		}
	}), start)
	return false, true
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

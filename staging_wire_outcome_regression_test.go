package httpcache

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type wireRejectBudget struct{ leases atomic.Int64 }

func (b *wireRejectBudget) NewLease(context.Context, StageKind) (StagingLease, error) {
	b.leases.Add(1)
	return nil, ErrStagingBudgetExceeded
}

func TestBudgetStagingOutcomesReachRealClients(t *testing.T) {
	for _, protocol := range []string{"http1", "http2"} {
		for _, knownLength := range []bool{false, true} {
			for _, outcome := range []string{"new-lease-refused", "reserve-refused", "admitted"} {
				t.Run(fmt.Sprintf("%s/known-length-%t/%s", protocol, knownLength, outcome), func(t *testing.T) {
					testBudgetStagingWireOutcome(t, protocol, knownLength, outcome)
				})
			}
		}
	}
}

func testBudgetStagingWireOutcome(t *testing.T, protocol string, knownLength bool, outcome string) {
	t.Helper()
	const body = "tiny"
	budget := &featureBudget{limit: 8192}
	var admission StagingBudget = budget
	switch outcome {
	case "new-lease-refused":
		admission = &wireRejectBudget{}
	case "reserve-refused":
		budget.limit = 0
	}
	c := NewMemoryCacheWithConfig(DefaultCacheConfig().WithCleanupInterval(0))
	t.Cleanup(func() { _ = c.Close() })
	h := NewHandlerWithOptions(c, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
		if knownLength {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.Header().Set("Transfer-Encoding", "identity")
		}
		_, _ = io.WriteString(w, body)
	}), &HandlerOptions{StagingBudget: admission, TempDir: t.TempDir()})
	t.Cleanup(func() { _ = h.Shutdown(context.Background()) })
	srv := newStagingWireServer(t, h, protocol)
	res, err := srv.Client().Get(srv.URL + "/body")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil || string(got) != body || res.StatusCode != http.StatusOK {
		t.Fatalf("origin delivery: status=%d body=%q err=%v", res.StatusCode, got, err)
	}
	if (protocol == "http2") != (res.ProtoMajor == 2) {
		t.Fatalf("unexpected HTTP protocol: %s", res.Proto)
	}
	wantHeader, wantTrailer := "MISS", "ADMITTED"
	switch outcome {
	case "new-lease-refused":
		wantHeader, wantTrailer = "SKIP", ""
	case "reserve-refused":
		wantTrailer = "SKIP"
	}
	if res.Header.Get(CacheHeader) != wantHeader || res.Trailer.Get(CacheStagingHeader) != wantTrailer {
		t.Fatalf("wire outcome: headers=%v trailers=%v", res.Header, res.Trailer)
	}
	h.writes.Wait()
	assertStagingWireStorage(t, c, srv.URL+"/body", outcome == "admitted", knownLength)
	budget.assertReleased(t)
}

func newStagingWireServer(t *testing.T, h http.Handler, protocol string) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	if protocol == "http2" {
		srv.EnableHTTP2 = true
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return srv
}

func assertStagingWireStorage(t *testing.T, c ExtendedCache, target string, admitted, knownLength bool) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.URL.Scheme, request.URL.Host = "", "" // A real server receives origin-form.
	if request.URL.Path == "" {
		request.URL.Path = "/"
	}
	key := NewRequestKey(request).String()
	stored, err := c.Retrieve(key)
	if !admitted {
		if stored != nil {
			_ = stored.Close()
		}
		if !errors.Is(err, ErrNotFoundInCache) {
			t.Fatalf("refused body was cached: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stored.Close() }()
	if stored.Header().Get(CacheHeader) != "MISS" || stored.Header().Get(CacheStagingHeader) != "" ||
		strings.Contains(stored.Header().Get("Trailer"), CacheStagingHeader) {
		t.Fatalf("synthetic delivery metadata entered storage: %v", stored.Header())
	}
	if knownLength && stored.Header().Get("Content-Length") != "4" {
		t.Fatalf("origin content length lost from storage: %v", stored.Header())
	}
}

func TestBudgetEarlyBypassReportsSkipInInitialHeaders(t *testing.T) {
	for _, reason := range []string{"temporary-file", "uncacheable", "head", "no-content", "reset-content", "not-modified"} {
		t.Run(reason, func(t *testing.T) {
			testBudgetEarlyBypass(t, reason)
		})
	}
}

func testBudgetEarlyBypass(t *testing.T, reason string) {
	t.Helper()
	budget := &featureBudget{limit: 8192}
	dir, method, status := t.TempDir(), http.MethodGet, http.StatusOK
	switch reason {
	case "temporary-file":
		dir += "/missing"
	case "head":
		method = http.MethodHead
	case "no-content":
		status = http.StatusNoContent
	case "reset-content":
		status = http.StatusResetContent
	case "not-modified":
		status = http.StatusNotModified
	}
	c := NewMemoryCacheWithConfig(DefaultCacheConfig().WithCleanupInterval(0))
	t.Cleanup(func() { _ = c.Close() })
	h := NewHandlerWithOptions(c, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		if reason == "uncacheable" {
			w.Header().Set("Cache-Control", "no-store")
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = io.WriteString(w, "complete")
		}
	}), &HandlerOptions{StagingBudget: budget, TempDir: dir})
	t.Cleanup(func() { _ = h.Shutdown(context.Background()) })
	srv := newStagingWireServer(t, h, "http1")
	req, err := http.NewRequest(method, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assertBudgetEarlyBypassResponse(t, res, status, method)
	h.writes.Wait()
	if c.Stats().ItemCount != 0 {
		t.Fatalf("bypassed response was cached: %+v", c.Stats())
	}
	budget.assertReleased(t)
}

func assertBudgetEarlyBypassResponse(t *testing.T, res *http.Response, status int, method string) {
	t.Helper()
	body, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil || res.StatusCode != status || res.Header.Get(CacheHeader) != "SKIP" || res.Trailer.Get(CacheStagingHeader) != "" {
		t.Fatalf("early bypass response: status=%d headers=%v trailers=%v err=%v", res.StatusCode, res.Header, res.Trailer, err)
	}
	if status == http.StatusOK && method != http.MethodHead && string(body) != "complete" {
		t.Fatalf("early bypass truncated body: %q", body)
	}
}

func TestBudgetLaterRefusalPreservesStreamingAndOriginTrailers(t *testing.T) {
	const firstSize = 64 * 1024
	first, tail := strings.Repeat("a", firstSize), "final-origin-bytes"
	continueOrigin := make(chan struct{})
	budget := &featureBudget{limit: firstSize}
	c := NewMemoryCacheWithConfig(DefaultCacheConfig().WithCleanupInterval(0))
	t.Cleanup(func() { _ = c.Close() })
	h := NewHandlerWithOptions(c, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Retain the Header map across WriteHeader, as net/http permits.
		header := w.Header()
		header.Set("Cache-Control", "max-age=3600")
		header.Set("Content-Length", strconv.Itoa(len(first)+len(tail)))
		header.Set("Trailer", "X-Origin-Checksum")
		_, _ = io.WriteString(w, first)
		<-continueOrigin
		_, _ = io.WriteString(w, tail)
		header.Set("X-Origin-Checksum", "complete")
		header.Set(http.TrailerPrefix+"X-Origin-Late", "late")
	}), &HandlerOptions{StagingBudget: budget, TempDir: t.TempDir()})
	t.Cleanup(func() { _ = h.Shutdown(context.Background()) })
	srv := newStagingWireServer(t, h, "http1")
	res, err := srv.Client().Get(srv.URL)
	if err != nil {
		close(continueOrigin)
		t.Fatal(err)
	}
	beforeRefusal := make([]byte, 1024)
	_, earlyErr := io.ReadFull(res.Body, beforeRefusal)
	close(continueOrigin)
	rest, lateErr := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if earlyErr != nil || lateErr != nil || string(beforeRefusal)+string(rest) != first+tail {
		t.Fatalf("stream delivery interrupted: early=%v late=%v bytes=%d", earlyErr, lateErr, len(beforeRefusal)+len(rest))
	}
	if res.Header.Get(CacheHeader) != "MISS" || res.Trailer.Get(CacheStagingHeader) != "SKIP" ||
		res.Trailer.Get("X-Origin-Checksum") != "complete" || res.Trailer.Get("X-Origin-Late") != "late" {
		t.Fatalf("final staging/origin trailers lost: headers=%v trailers=%v", res.Header, res.Trailer)
	}
	h.writes.Wait()
	assertStagingWireStorage(t, c, srv.URL, false, false)
	budget.assertReleased(t)
}

func TestHTTP10BudgetedResponseSkipsBeforeStreaming(t *testing.T) {
	budget := &wireRejectBudget{}
	c := NewMemoryCacheWithConfig(DefaultCacheConfig().WithCleanupInterval(0))
	t.Cleanup(func() { _ = c.Close() })
	h := NewHandlerWithOptions(c, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		_, _ = io.WriteString(w, "complete-http10-body")
	}), &HandlerOptions{StagingBudget: budget, TempDir: t.TempDir()})
	t.Cleanup(func() { _ = h.Shutdown(context.Background()) })
	srv := newStagingWireServer(t, h, "http1")
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = fmt.Fprint(conn, "GET /legacy HTTP/1.0\r\nHost: legacy.test\r\n\r\n")
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil || string(body) != "complete-http10-body" || res.Header.Get(CacheHeader) != "SKIP" || budget.leases.Load() != 0 {
		t.Fatalf("HTTP/1.0 admission: body=%q headers=%v leases=%d err=%v", body, res.Header, budget.leases.Load(), err)
	}
	h.writes.Wait()
	assertStagingWireStorage(t, c, "http://legacy.test/legacy", false, false)
}

package httpcache

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type bodylessStagingBudget struct {
	mu          sync.Mutex
	rejectLease bool
	budget      *featureBudget
}

func (b *bodylessStagingBudget) NewLease(ctx context.Context, kind StageKind) (StagingLease, error) {
	b.mu.Lock()
	reject := b.rejectLease
	b.mu.Unlock()
	if reject {
		return nil, ErrStagingBudgetExceeded
	}
	return b.budget.NewLease(ctx, kind)
}

func (b *bodylessStagingBudget) setOutcome(outcome string) {
	b.mu.Lock()
	b.rejectLease = outcome == "lease-refused"
	b.mu.Unlock()
	b.budget.mu.Lock()
	b.budget.limit = 1 << 20
	if outcome == "reserve-refused" {
		b.budget.limit = 0
	}
	b.budget.mu.Unlock()
}

func TestStagingConditionalBodylessReportsRefusalOnWire(t *testing.T) {
	for _, condition := range []string{"etag", "date"} {
		for _, outcome := range []string{"lease-refused", "reserve-refused", "admitted"} {
			t.Run(condition+"/"+outcome, func(t *testing.T) {
				testStagingConditionalBodyless(t, condition, outcome)
			})
		}
	}
}

func testStagingConditionalBodyless(t *testing.T, condition, outcome string) {
	t.Helper()
	budget := &bodylessStagingBudget{budget: &featureBudget{limit: 1 << 20}}
	cache := NewMemoryCache().(ExtendedCache)
	defer func() { _ = cache.Close() }()
	var calls atomic.Int64
	modified := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	body := strings.Repeat("complete replacement ", 512)
	h := NewHandlerWithOptions(cache, bodylessValidationOrigin(&calls, condition, modified, body), &HandlerOptions{StagingBudget: budget})
	defer func() { _ = h.Shutdown(context.Background()) }()
	server := httptest.NewServer(h)
	defer server.Close()
	initial := bodylessStagingRequest(t, server, nil)
	if initial.StatusCode != http.StatusOK {
		t.Fatalf("warm response status=%d", initial.StatusCode)
	}
	h.writes.Wait()
	budget.setOutcome(outcome)
	header := http.Header{"Cache-Control": {"min-fresh=7200"}}
	setBodylessClientCondition(header, condition, modified)
	response := bodylessStagingRequest(t, server, header)
	want := "SKIP"
	if outcome == "admitted" {
		want = "MISS"
	}
	if response.StatusCode != http.StatusNotModified || response.Header.Get(CacheHeader) != want {
		t.Fatalf("conditional staging outcome: status=%d X-Cache=%q; want 304 %s", response.StatusCode, response.Header.Get(CacheHeader), want)
	}
	if response.Header.Get("Content-Length") != "" || len(response.Trailer) != 0 {
		t.Fatalf("304 carried body framing or staging trailer: header=%v trailers=%v", response.Header, response.Trailer)
	}
	if outcome == "admitted" && (response.Header.Get("Age") == "" || response.Header.Get(ProxyDateHeader) == "") {
		t.Fatalf("deferred response lost final cache metadata: %v", response.Header)
	}
	h.writes.Wait()
	if outcome == "admitted" {
		assertBodylessStagingFollowupHit(t, h, server, cache, &calls, body)
	}
	assertBodylessBudgetReleased(t, budget)
}

func bodylessValidationOrigin(calls *atomic.Int64, condition string, modified time.Time, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		version := "old"
		responseModified := modified
		if n > 1 {
			version, responseModified = "current", modified.Add(time.Minute)
		}
		if condition == "etag" {
			w.Header().Set("ETag", `"`+version+`"`)
		}
		w.Header().Set("Last-Modified", responseModified.Format(http.TimeFormat))
		w.Header().Set("Date", Clock().Format(http.TimeFormat))
		w.Header().Set("Age", "0")
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Trailer", "X-Origin-Finished")
		_, _ = io.WriteString(w, body)
		w.Header().Set("X-Origin-Finished", "done")
	})
}

func setBodylessClientCondition(header http.Header, condition string, modified time.Time) {
	if condition == "etag" {
		header.Set("If-None-Match", `"current"`)
		return
	}
	header.Set("If-Modified-Since", modified.Add(time.Minute).Format(http.TimeFormat))
}

func bodylessStagingRequest(t *testing.T, server *httptest.Server, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, server.URL+"/object", nil)
	if err != nil {
		t.Fatal(err)
	}
	if header != nil {
		req.Header = header
	}
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode == http.StatusNotModified || response.StatusCode == http.StatusPreconditionFailed {
		if len(body) != 0 {
			t.Fatalf("suppressed response delivered body: %q", body)
		}
	}
	response.Body = io.NopCloser(strings.NewReader(string(body)))
	return response
}

func assertBodylessStagingFollowupHit(t *testing.T, h *Handler, server *httptest.Server, cache Cache, calls *atomic.Int64, body string) {
	t.Helper()
	response := bodylessStagingRequest(t, server, nil)
	got, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || response.Header.Get(CacheHeader) != "HIT" || string(got) != body || calls.Load() != 2 {
		t.Fatalf("bodyless response lost stored full body: status=%d X-Cache=%q body=%d calls=%d err=%v", response.StatusCode, response.Header.Get(CacheHeader), len(got), calls.Load(), err)
	}
	h.writes.Wait()
	req, err := http.NewRequest(http.MethodGet, server.URL+"/object", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.URL.Scheme, req.URL.Host = "", "" // A real server receives origin-form.
	resource, err := cache.Retrieve(NewRequestKey(req).String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resource.Close() }()
	if resource.Header().Get("Content-Length") != strconv.Itoa(len(body)) || strings.Contains(combinedHeaderValues(resource.Header(), "Trailer"), CacheStagingHeader) {
		t.Fatalf("delivery framing polluted storage metadata: %v", resource.Header())
	}
}

func assertBodylessBudgetReleased(t *testing.T, budget *bodylessStagingBudget) {
	t.Helper()
	budget.budget.mu.Lock()
	used, active := budget.budget.used, budget.budget.active
	budget.budget.mu.Unlock()
	if used != 0 || active != 0 {
		t.Fatalf("bodyless staging retained budget: used=%d active=%d", used, active)
	}
}

func TestStagingSuppressedIfMatchReportsRefusalOnWire(t *testing.T) {
	for _, outcome := range []string{"lease-refused", "reserve-refused", "admitted"} {
		t.Run(outcome, func(t *testing.T) {
			budget := &bodylessStagingBudget{budget: &featureBudget{limit: 1 << 20}}
			budget.setOutcome(outcome)
			cache := NewMemoryCache().(ExtendedCache)
			defer func() { _ = cache.Close() }()
			h := NewHandlerWithOptions(cache, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("ETag", `"current"`)
				w.Header().Set("Cache-Control", "max-age=3600")
				w.Header().Set("Content-Length", strconv.Itoa(len("full stored representation")))
				_, _ = io.WriteString(w, "full stored representation")
			}), &HandlerOptions{StagingBudget: budget})
			defer func() { _ = h.Shutdown(context.Background()) }()
			// Requests carrying If-Match normally bypass Handler lookup. Exercise
			// the delivery wrapper through the real bounded pass-upstream path.
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				cReq, err := newCacheRequest(req)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				client := newClientPreconditionWriter(w, req)
				h.passUpstream(client, cReq, nil)
				client.(*clientPreconditionWriter).FinishStagingHeaders()
			}))
			defer server.Close()
			response := bodylessStagingRequest(t, server, http.Header{"If-Match": {`"different"`}})
			want := "SKIP"
			if outcome == "admitted" {
				want = "MISS"
			}
			if response.StatusCode != http.StatusPreconditionFailed || response.Header.Get(CacheHeader) != want || len(response.Trailer) != 0 {
				t.Fatalf("If-Match outcome lost: status=%d X-Cache=%q trailer=%v", response.StatusCode, response.Header.Get(CacheHeader), response.Trailer)
			}
			h.writes.Wait()
			assertBodylessBudgetReleased(t, budget)
		})
	}
}

func TestStagingConditionalBodyForwardsOutcomeTrailer(t *testing.T) {
	budget := &bodylessStagingBudget{budget: &featureBudget{limit: 1 << 20}}
	cache := NewMemoryCache().(ExtendedCache)
	defer func() { _ = cache.Close() }()
	var calls atomic.Int64
	h := NewHandlerWithOptions(cache, bodylessValidationOrigin(&calls, "etag", time.Now().Add(-time.Hour), "complete body"), &HandlerOptions{StagingBudget: budget})
	defer func() { _ = h.Shutdown(context.Background()) }()
	server := httptest.NewServer(h)
	defer server.Close()
	_ = bodylessStagingRequest(t, server, nil)
	h.writes.Wait()
	response := bodylessStagingRequest(t, server, http.Header{"Cache-Control": {"min-fresh=7200"}, "If-None-Match": {`"unmatched"`}})
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "complete body" || response.Trailer.Get(CacheStagingHeader) != "ADMITTED" || response.Trailer.Get("X-Origin-Finished") != "done" {
		t.Fatalf("conditional body lost trailers: status=%d body=%q trailer=%v err=%v", response.StatusCode, body, response.Trailer, err)
	}
	h.writes.Wait()
	assertBodylessBudgetReleased(t, budget)
}

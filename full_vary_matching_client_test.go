package httpcache

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type matchingVaryFixture struct {
	h         *Handler
	server    *httptest.Server
	finished  chan struct{}
	changed   atomic.Bool
	calls     atomic.Int64
	last      atomic.Value
	condition string
	before    string
	after     string
	modified  time.Time
}

func TestFullChangedVaryRebuildsBeforeMatchingClientDelivery(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, budgeted := range []bool{false, true} {
			for _, condition := range []string{"etag", "date"} {
				for _, policy := range []struct{ name, before, after string }{
					{"added", "", "Accept-Encoding"},
					{"changed", "Accept-Language", "Accept-Encoding"},
					{"removed", "Accept-Language", ""},
				} {
					t.Run("disk="+stringBool(disk)+"/budget="+stringBool(budgeted)+"/"+condition+"/"+policy.name, func(t *testing.T) {
						testFullChangedVaryMatchingClient(t, disk, budgeted, condition, policy.before, policy.after)
					})
				}
			}
		}
	}
}

func newMatchingVaryFixture(t *testing.T, disk, budgeted bool, condition, before, after string) *matchingVaryFixture {
	t.Helper()
	f := &matchingVaryFixture{condition: condition, before: before, after: after,
		modified: time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second), finished: make(chan struct{}, 8)}
	cache := generationBudgetCache(t, disk, 1<<20)
	opts := &HandlerOptions{TempDir: t.TempDir()}
	if budgeted {
		budget := &featureBudget{limit: 1 << 20}
		opts.StagingBudget = budget
		t.Cleanup(func() { budget.assertReleased(t) })
	}
	f.h = NewHandlerWithOptions(cache, http.HandlerFunc(f.origin), opts)
	f.h.Shared = true
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { f.finished <- struct{}{} }()
		f.h.ServeHTTP(w, r)
	}))
	f.server.Client().Timeout = 5 * time.Second
	t.Cleanup(func() {
		f.server.Close()
		_ = f.h.Shutdown(context.Background())
	})
	return f
}

func (f *matchingVaryFixture) origin(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	f.last.Store(r.Header.Clone())
	body, vary, modified, etag := "old body", f.before, f.modified, `"old"`
	if f.changed.Load() {
		body, vary, modified, etag = "complete replacement body", f.after, f.modified.Add(time.Hour), `"current"`
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Date", Clock().Format(http.TimeFormat))
	if vary != "" {
		w.Header().Set("Vary", vary)
	}
	if f.condition == "etag" {
		w.Header().Set("ETag", etag)
	}
	http.ServeContent(w, r, "", modified, strings.NewReader(body))
}

func (f *matchingVaryFixture) request(t *testing.T, matching, force bool, language string) (*http.Response, string) {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, f.server.URL+"/full-vary", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Accept-Language", language)
	r.Header.Set("Accept-Encoding", "gzip")
	if matching {
		if f.condition == "etag" {
			r.Header.Set("If-None-Match", `"current"`)
		} else {
			r.Header.Set("If-Modified-Since", f.modified.Add(time.Hour).Format(http.TimeFormat))
		}
	}
	if force {
		r.Header.Set("Cache-Control", "min-fresh=7200")
	}
	res, err := f.server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("cache handler did not finish")
	}
	f.h.writes.Wait()
	return res, string(body)
}

func testFullChangedVaryMatchingClient(t *testing.T, disk, budgeted bool, condition, before, after string) {
	t.Helper()
	f := newMatchingVaryFixture(t, disk, budgeted, condition, before, after)
	res, body := f.request(t, false, false, "en")
	if res.StatusCode != http.StatusOK || body != "old body" || f.calls.Load() != 1 {
		t.Fatalf("initial response: status=%d body=%q calls=%d", res.StatusCode, body, f.calls.Load())
	}
	f.changed.Store(true)
	res, body = f.request(t, true, true, "en")
	if res.StatusCode != http.StatusNotModified || body != "" || res.Header.Get("Vary") != after {
		t.Fatalf("matching client delivery changed: status=%d body=%q headers=%v", res.StatusCode, body, res.Header)
	}
	if f.calls.Load() != 3 {
		t.Fatalf("full-policy rebuild requests=%d, want seed + validation + full rebuild", f.calls.Load())
	}
	res, body = f.request(t, true, false, "fr")
	assertMatchingVaryHit(t, f, res, body, http.StatusNotModified, "")
	res, body = f.request(t, false, false, "fr")
	assertMatchingVaryHit(t, f, res, body, http.StatusOK, "complete replacement body")
	if res.Header.Get("Vary") != after {
		t.Fatalf("replacement discovery policy was not persisted: %v", res.Header)
	}
}

func assertMatchingVaryHit(t *testing.T, f *matchingVaryFixture, res *http.Response, body string, status int, wantBody string) {
	t.Helper()
	if res.StatusCode != status || body != wantBody || res.Header.Get(CacheHeader) != "HIT" || f.calls.Load() != 3 {
		t.Fatalf("rebuilt representation not reused: status=%d body=%q X-Cache=%q calls=%d", res.StatusCode, body, res.Header.Get(CacheHeader), f.calls.Load())
	}
}

func TestFullChangedVaryPreservesSelectingClientConditions(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, condition := range []string{"etag", "date"} {
			t.Run("disk="+stringBool(disk)+"/"+condition, func(t *testing.T) {
				name := "If-None-Match"
				if condition == "date" {
					name = "If-Modified-Since"
				}
				f := newMatchingVaryFixture(t, disk, false, condition, "Accept-Language", name)
				_, _ = f.request(t, false, false, "en")
				f.changed.Store(true)
				res, body := f.request(t, true, true, "en")
				want := `"current"`
				if condition == "date" {
					want = f.modified.Add(time.Hour).Format(http.TimeFormat)
				}
				last := f.last.Load().(http.Header)
				if res.StatusCode != http.StatusNotModified || body != "" || last.Get(name) != want || f.calls.Load() != 3 {
					t.Fatalf("selecting condition changed: status=%d body=%q origin=%v calls=%d", res.StatusCode, body, last, f.calls.Load())
				}
			})
		}
	}
}

package httpcache

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

type successfulStatusCondition struct {
	name, header, value string
	matching            bool
}

func TestValidationAppliesOriginalConditionsToSuccessfulStatuses(t *testing.T) {
	modified := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	conditions := []successfulStatusCondition{
		{"matching-etag", "If-None-Match", `"v2"`, true},
		{"matching-date", "If-Modified-Since", modified.Format(http.TimeFormat), true},
		{"unmatched-etag", "If-None-Match", `"v3"`, false},
		{"unmatched-date", "If-Modified-Since", modified.Add(-time.Hour).Format(http.TimeFormat), false},
	}
	for _, status := range []int{http.StatusCreated, http.StatusAccepted, http.StatusNoContent, http.StatusResetContent} {
		for _, condition := range conditions {
			t.Run(strconv.Itoa(status)+"/"+condition.name, func(t *testing.T) {
				testValidationSuccessfulStatus(t, status, modified, condition)
			})
		}
	}
}

func testValidationSuccessfulStatus(t *testing.T, status int, modified time.Time, condition successfulStatusCondition) {
	t.Helper()
	cache := NewMemoryCache().(ExtendedCache)
	defer func() { _ = cache.Close() }()
	origin := &successfulStatusOrigin{status: status, modified: modified}
	h := NewSharedHandler(cache, origin)
	defer h.writes.Wait()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://cache.example/object", nil))
	h.writes.Wait()
	if cache.Stats().ItemCount != 1 {
		t.Fatal("the initial 200 representation was not cached")
	}
	r := httptest.NewRequest("GET", "http://cache.example/object", nil)
	r.Header.Set("Cache-Control", "min-fresh=7200")
	r.Header.Set(condition.header, condition.value)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	h.writes.Wait()
	wantStatus, wantBody := status, successfulStatusBody(status)
	if condition.matching {
		wantStatus, wantBody = http.StatusNotModified, ""
	}
	if w.Code != wantStatus || w.Body.String() != wantBody {
		t.Fatalf("status=%d body=%q; want status=%d body=%q", w.Code, w.Body.String(), wantStatus, wantBody)
	}
	if w.Header().Get("ETag") != `"v2"` {
		t.Fatalf("new representation ETag was lost: %v", w.Header())
	}
	if !condition.matching && w.Header().Get("Last-Modified") != modified.Format(http.TimeFormat) {
		t.Fatalf("successful response lost Last-Modified: %v", w.Header())
	}
	if r.Header.Get(condition.header) != condition.value {
		t.Fatalf("the original client condition was mutated: %v", r.Header)
	}
	validation := origin.validation.Load().(http.Header)
	if origin.calls.Load() != 2 || validation.Get("If-None-Match") != `"v1"` || validation.Get("If-Modified-Since") != r.Header.Get("If-Modified-Since") {
		t.Fatalf("calls=%d validation=%v; want one validation using the old cached ETag", origin.calls.Load(), validation)
	}
}

type successfulStatusOrigin struct {
	status     int
	modified   time.Time
	calls      atomic.Int64
	validation atomic.Value
}

func (o *successfulStatusOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "max-age=3600")
	w.Header().Set("ETag", `"v1"`)
	w.Header().Set("Last-Modified", o.modified.Add(-2*time.Hour).Format(http.TimeFormat))
	if o.calls.Add(1) == 1 {
		_, _ = io.WriteString(w, "old representation")
		return
	}
	o.validation.Store(r.Header.Clone())
	w.Header().Set("ETag", `"v2"`)
	w.Header().Set("Last-Modified", o.modified.Format(http.TimeFormat))
	etag := r.Header.Get("If-None-Match")
	if etag == `"v2"` || (etag == "" && r.Header.Get("If-Modified-Since") == o.modified.Format(http.TimeFormat)) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(o.status)
	if body := successfulStatusBody(o.status); body != "" {
		_, _ = io.WriteString(w, body)
	}
}

func successfulStatusBody(status int) string {
	if status == http.StatusNoContent || status == http.StatusResetContent {
		return ""
	}
	return "new successful representation"
}

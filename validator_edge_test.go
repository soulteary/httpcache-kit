package httpcache

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestValidationResponseStatusTransitions(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*validationResponse)
		want int
	}{
		{"informational then final", func(w *validationResponse) {
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusNotModified)
			w.WriteHeader(http.StatusInternalServerError)
			w.Flush()
		}, http.StatusNotModified},
		{"flush commits OK", func(w *validationResponse) {
			w.Flush()
			w.WriteHeader(http.StatusNotModified)
		}, http.StatusOK},
		{"write commits OK", func(w *validationResponse) { _, _ = w.Write([]byte("discarded")) }, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &validationResponse{header: make(http.Header)}
			tc.run(response)
			if response.status != tc.want {
				t.Fatalf("status=%d, want=%d", response.status, tc.want)
			}
		})
	}
}

func TestValidatorInvalidAgeDoesNotInventCorrectedAge(t *testing.T) {
	res := NewResourceBytes(http.StatusOK, []byte("body"), http.Header{"Etag": {`"v1"`}, "Age": {"999"}})
	v := &Validator{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Age", "invalid")
		w.WriteHeader(http.StatusNotModified)
	})}
	if !v.Validate(httptest.NewRequest(http.MethodGet, "http://example.org/object", nil), res) {
		t.Fatal("compatible validation rejected")
	}
	if res.Header().Get("Age") != "invalid" || res.RequestTime.IsZero() || res.Header().Get(ProxyDateHeader) == "" {
		t.Fatalf("validation metadata=%v, request time=%v", res.Header(), res.RequestTime)
	}
}

func TestValidatedWarningsIgnoreEmptyValuesAndKeepMalformedCodes(t *testing.T) {
	fields := []string{
		", \t, 110\tproxy \"stale\",",
		`1a0 proxy "unknown", 10a proxy "unknown", 1100 proxy "unknown", 299 proxy "kept"`,
		`299 proxy "kept", ,`,
	}
	want := []string{`1a0 proxy "unknown", 10a proxy "unknown", 1100 proxy "unknown", 299 proxy "kept"`}
	if got := validatedWarningValues(fields); !reflect.DeepEqual(got, want) {
		t.Fatalf("warnings=%q, want=%q", got, want)
	}
}

func TestResourceStoredAfterFallsBackToResponseDates(t *testing.T) {
	at := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	for _, header := range []string{"Date", ProxyDateHeader} {
		t.Run(header, func(t *testing.T) {
			res := NewResourceBytes(http.StatusOK, nil, http.Header{header: {at.Add(time.Second).Format(http.TimeFormat)}})
			if !res.StoredAfter(at) || res.StoredAfter(at.Add(time.Second)) {
				t.Fatal("fallback must use strictly newer response dates")
			}
			res.SetStoredAt(at)
			if res.StoredAfter(at) {
				t.Fatal("explicit stored time must take precedence over newer response date")
			}
		})
	}
}

package httpcache

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestValidator_Validate_ETag_Unchanged(t *testing.T) {
	etag := `"abc123"`
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			w.Header().Set("Etag", etag)
			return
		}
		w.Header().Set("Etag", etag)
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	v := &Validator{Handler: upstream}
	res := NewResourceBytes(200, []byte("ok"), http.Header{
		"Etag": []string{etag},
		"Date": []string{time.Now().UTC().Format(http.TimeFormat)},
	})
	req := httptest.NewRequest("GET", "http://example.org/", nil)
	ok := v.Validate(req, res)
	if !ok {
		t.Error("Validate should return true when upstream returns 304 with same ETag")
	}
}

func TestValidator_Validate_LastModified(t *testing.T) {
	lastMod := time.Now().UTC().Add(-time.Hour).Format(http.TimeFormat)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Modified-Since") != "" {
			w.WriteHeader(http.StatusNotModified)
			w.Header().Set("Last-Modified", lastMod)
			w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
			return
		}
		w.Header().Set("Last-Modified", lastMod)
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	v := &Validator{Handler: upstream}
	res := NewResourceBytes(200, []byte("ok"), http.Header{
		"Last-Modified": []string{lastMod},
		"Date":          []string{time.Now().UTC().Format(http.TimeFormat)},
	})
	req := httptest.NewRequest("GET", "http://example.org/", nil)
	ok := v.Validate(req, res)
	if !ok {
		t.Error("Validate should return true when upstream returns 304 with same Last-Modified")
	}
}

func TestValidator_Validate_HeadersChanged(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Etag", `"new-etag"`)
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("new body"))
	})
	v := &Validator{Handler: upstream}
	res := NewResourceBytes(200, []byte("old"), http.Header{
		"Etag": []string{`"old-etag"`},
		"Date": []string{time.Now().UTC().Format(http.TimeFormat)},
	})
	req := httptest.NewRequest("GET", "http://example.org/", nil)
	ok := v.Validate(req, res)
	if ok {
		t.Error("Validate should return false when upstream returns different ETag")
	}
}

// TestValidator_Validate_ResponseNoDate covers correctedAge error branch (handler returns 304 without Date).
func TestValidator_Validate_ResponseNoDate(t *testing.T) {
	etag := `"abc"`
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			w.Header().Set("Etag", etag)
			// Intentionally no Date -> correctedAge will fail, branch err != nil
			return
		}
		w.Header().Set("Etag", etag)
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	v := &Validator{Handler: upstream}
	res := NewResourceBytes(200, []byte("ok"), http.Header{
		"Etag": []string{etag},
		"Date": []string{time.Now().UTC().Format(http.TimeFormat)},
	})
	req := httptest.NewRequest("GET", "http://example.org/", nil)
	ok := v.Validate(req, res)
	if !ok {
		t.Error("Validate should still return true when 304 has same ETag (correctedAge error is non-fatal)")
	}
}

package httpcache

import (
	"net/http"
	"testing"
	"time"
)

func TestResource_DateAfter_NoDate(t *testing.T) {
	r := NewResourceBytes(200, []byte("x"), http.Header{})
	if r.DateAfter(time.Now()) {
		t.Error("DateAfter with no Date header should be false")
	}
}

func TestResource_DateAfter_InvalidDate(t *testing.T) {
	r := NewResourceBytes(200, []byte("x"), http.Header{"Date": []string{"invalid"}})
	if r.DateAfter(time.Now()) {
		t.Error("DateAfter with invalid Date should be false")
	}
}

func TestResource_MaxAge_Expires(t *testing.T) {
	future := time.Now().UTC().Add(time.Hour)
	r := NewResourceBytes(200, []byte("x"), http.Header{
		"Expires": []string{future.Format(http.TimeFormat)},
	})
	d, err := r.MaxAge(false)
	if err != nil {
		t.Fatal(err)
	}
	if d <= 0 {
		t.Errorf("expected positive max-age from Expires, got %v", d)
	}
}

func TestResource_MaxAge_SMaxage_Shared(t *testing.T) {
	r := NewResourceBytes(200, []byte("x"), http.Header{
		"Cache-Control": []string{"s-maxage=120"},
	})
	d, err := r.MaxAge(true)
	if err != nil {
		t.Fatal(err)
	}
	if d != 120*time.Second {
		t.Errorf("expected 120s, got %v", d)
	}
}

func TestResource_HasExplicitExpiration_Expires(t *testing.T) {
	r := NewResourceBytes(200, []byte("x"), http.Header{
		"Expires": []string{time.Now().UTC().Add(time.Hour).Format(http.TimeFormat)},
	})
	if !r.HasExplicitExpiration() {
		t.Error("HasExplicitExpiration should be true with Expires")
	}
}

func TestResource_HasExplicitExpiration_NoHeaders(t *testing.T) {
	r := NewResourceBytes(200, []byte("x"), http.Header{})
	if r.HasExplicitExpiration() {
		t.Error("HasExplicitExpiration should be false with no expiration headers")
	}
}

func TestResource_Age_WithProxyDate(t *testing.T) {
	past := time.Now().UTC().Add(-time.Minute)
	r := NewResourceBytes(200, []byte("x"), http.Header{
		"Date":        []string{past.Format(http.TimeFormat)},
		"Proxy-Date": []string{past.Format(http.TimeFormat)},
		"Age":         []string{"30"},
	})
	Clock = func() time.Time { return past.Add(2 * time.Minute) }
	defer func() { Clock = func() time.Time { return time.Now().UTC() } }()
	d, err := r.Age()
	if err != nil {
		t.Fatal(err)
	}
	if d <= 0 {
		t.Errorf("expected positive age, got %v", d)
	}
}

func TestResource_Age_NoDate(t *testing.T) {
	r := NewResourceBytes(200, []byte("x"), http.Header{})
	_, err := r.Age()
	if err == nil {
		t.Error("Age with no Date/Proxy-Date should error")
	}
}

func TestResource_MaxAge_MaxAgeDirective(t *testing.T) {
	r := NewResourceBytes(200, []byte("x"), http.Header{
		"Cache-Control": []string{"max-age=300"},
	})
	d, err := r.MaxAge(false)
	if err != nil {
		t.Fatal(err)
	}
	if d != 300*time.Second {
		t.Errorf("expected 300s, got %v", d)
	}
}

func TestResource_MustValidate_ProxyRevalidate_Shared(t *testing.T) {
	r := NewResourceBytes(200, []byte("x"), http.Header{
		"Cache-Control": []string{"max-age=60, proxy-revalidate"},
	})
	if !r.MustValidate(true) {
		t.Error("MustValidate(shared) should be true with proxy-revalidate")
	}
}

func TestResource_HasExplicitExpiration_SMaxage(t *testing.T) {
	r := NewResourceBytes(200, []byte("x"), http.Header{
		"Cache-Control": []string{"s-maxage=60"},
	})
	if !r.HasExplicitExpiration() {
		t.Error("HasExplicitExpiration should be true with s-maxage")
	}
}

func TestResource_RemovePrivateHeaders(t *testing.T) {
	r := NewResourceBytes(200, []byte("x"), http.Header{
		"Cache-Control": []string{"private=X-Secret, private=Authorization"},
		"X-Secret":      []string{"hide"},
		"Authorization": []string{"Bearer token"},
	})
	r.RemovePrivateHeaders()
	if r.Header().Get("X-Secret") != "" || r.Header().Get("Authorization") != "" {
		t.Error("private headers should be removed")
	}
}

func TestResource_MaxAge_SMaxage_Shared_InvalidDuration(t *testing.T) {
	r := NewResourceBytes(200, []byte("x"), http.Header{
		"Cache-Control": []string{"s-maxage=invalid"},
	})
	_, err := r.MaxAge(true)
	if err == nil {
		t.Error("expected error for invalid s-maxage duration")
	}
}

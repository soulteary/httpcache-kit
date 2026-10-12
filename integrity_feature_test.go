package httpcache

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func integrityFeatureCache(t *testing.T, dir string, mode IntegrityMode) ExtendedCache {
	t.Helper()
	c, err := NewDiskCacheWithConfig(dir, &CacheConfig{Integrity: mode})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func integrityFeaturePath(dir, prefix, key string) string {
	return filepath.Join(dir, filepath.FromSlash(prefix+formatPrefix+hashKey(key)))
}

func integrityFeatureResource(body string) *Resource {
	return NewResourceBytes(http.StatusOK, []byte(body), http.Header{
		"Cache-Control":  {"max-age=3600"},
		"Content-Length": {strconv.Itoa(len(body))},
		"Date":           {Clock().Format(http.TimeFormat)},
		"Etag":           {`"integrity-v1"`},
		"X-Origin":       {"trusted-origin"},
	})
}

func integrityFeatureBody(t *testing.T, c Cache, key, want string) *Resource {
	t.Helper()
	res, err := c.Retrieve(key)
	if err != nil {
		t.Fatalf("Retrieve(%q): %v", key, err)
	}
	t.Cleanup(func() { _ = res.Close() })
	got, err := io.ReadAll(res)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("Retrieve(%q) body = %q, want %q", key, got, want)
	}
	return res
}

func integrityFeatureMiss(t *testing.T, c Cache, key string) {
	t.Helper()
	res, err := c.Retrieve(key)
	if res != nil {
		_ = res.Close()
		t.Errorf("Retrieve(%q) exposed a resource before checking integrity", key)
	}
	if !errors.Is(err, ErrNotFoundInCache) {
		t.Fatalf("Retrieve(%q) error = %v, want ErrNotFoundInCache", key, err)
	}
}

func integrityFeatureDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type integrityFeatureRecord struct {
	StoredAt     time.Time `json:"stored_at"`
	Size         int64     `json:"size"`
	BodySHA256   string    `json:"body_sha256"`
	HeaderSHA256 string    `json:"header_sha256"`
	KeySHA256    string    `json:"key_sha256"`
}

func integrityFeatureReadRecord(t *testing.T, dir, key string) integrityFeatureRecord {
	t.Helper()
	raw, err := os.ReadFile(integrityFeaturePath(dir, headerPrefix, key))
	if err != nil {
		t.Fatal(err)
	}
	preamble, httpRecord, ok := bytes.Cut(raw, []byte("\r\n"))
	if !ok || !bytes.HasPrefix(preamble, []byte("HTTPCACHE/2 ")) {
		t.Fatalf("missing private integrity preamble: %q", preamble)
	}
	metadata, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(string(preamble), "HTTPCACHE/2 "))
	if err != nil {
		t.Fatalf("private metadata encoding: %v", err)
	}
	var record integrityFeatureRecord
	if err := json.Unmarshal(metadata, &record); err != nil {
		t.Fatal(err)
	}
	unsigned := record
	unsigned.HeaderSHA256 = ""
	// The private metadata and HTTP record are one digest generation.
	canonical, err := json.Marshal(struct {
		StoredAt   time.Time `json:"stored_at"`
		Size       int64     `json:"size"`
		BodySHA256 string    `json:"body_sha256"`
		KeySHA256  string    `json:"key_sha256"`
	}{unsigned.StoredAt, unsigned.Size, unsigned.BodySHA256, unsigned.KeySHA256})
	if err != nil {
		t.Fatal(err)
	}
	digestInput := append(append(canonical, byte(10)), httpRecord...)
	if want := integrityFeatureDigest(digestInput); record.HeaderSHA256 != want {
		t.Fatalf("persisted header SHA256 = %q, want %q", record.HeaderSHA256, want)
	}
	if want := integrityFeatureDigest([]byte(key)); record.KeySHA256 != want {
		t.Fatalf("persisted key SHA256 = %q, want %q", record.KeySHA256, want)
	}
	return record
}

func TestIntegritySHA256PersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	const key, body = "GET:http://cache.example/persist.deb", "persisted-package-payload"
	c := integrityFeatureCache(t, dir, IntegritySHA256)
	origin := integrityFeatureResource(body)
	wantHeaders := origin.Header().Clone()
	if err := c.Store(origin, key); err != nil {
		t.Fatal(err)
	}
	record := integrityFeatureReadRecord(t, dir, key)
	if record.Size != int64(len(body)) || record.BodySHA256 != integrityFeatureDigest([]byte(body)) {
		t.Fatalf("persisted body integrity = %+v", record)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := integrityFeatureCache(t, dir, IntegritySHA256)
	res := integrityFeatureBody(t, restarted, key, body)
	if !reflect.DeepEqual(res.Header(), wantHeaders) {
		t.Fatalf("private metadata changed Resource headers: got %v, want %v", res.Header(), wantHeaders)
	}
	h, err := restarted.Header(key)
	if err != nil || !reflect.DeepEqual(h.Header, wantHeaders) {
		t.Fatalf("private metadata changed Header result: %v, %v", h, err)
	}
}

func TestIntegritySHA256RejectsTamperingAndPreservesNeighbor(t *testing.T) {
	for _, restart := range []bool{false, true} {
		for _, kind := range []string{"same-length-body", "truncated-body", "appended-body", "same-length-header", "same-length-status"} {
			t.Run(fmt.Sprintf("%s/restart=%t", kind, restart), func(t *testing.T) {
				dir := t.TempDir()
				const key = "GET:http://cache.example/package.deb Accept-Language:en"
				const neighbor = "GET:http://cache.example/package.deb Accept-Language:fr"
				const body = "original-package-payload"
				c := integrityFeatureCache(t, dir, IntegritySHA256)
				if err := c.Store(integrityFeatureResource(body), key, neighbor); err != nil {
					t.Fatal(err)
				}
				if restart {
					if err := c.Close(); err != nil {
						t.Fatal(err)
					}
				}
				prefix := bodyPrefix
				if strings.Contains(kind, "header") || strings.Contains(kind, "status") {
					prefix = headerPrefix
				}
				path := integrityFeaturePath(dir, prefix, key)
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				originalSize := len(raw)
				switch kind {
				case "same-length-body":
					raw[0] ^= 1
				case "truncated-body":
					raw = raw[:len(raw)-1]
				case "appended-body":
					raw = append(raw, '!')
				case "same-length-header":
					raw = bytes.Replace(raw, []byte("trusted-origin"), []byte("hostile-origin"), 1)
				case "same-length-status":
					raw = bytes.Replace(raw, []byte("HTTP/1.1 200 OK"), []byte("HTTP/1.1 404 OK"), 1)
				}
				if strings.HasPrefix(kind, "same-length") && len(raw) != originalSize {
					t.Fatal("tamper fixture changed length")
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if restart {
					c = integrityFeatureCache(t, dir, IntegritySHA256)
				}
				before := c.Stats()
				integrityFeatureMiss(t, c, key)
				if after := c.Stats(); after.HitCount != before.HitCount || after.MissCount != before.MissCount+1 {
					t.Fatalf("corruption was not counted as a MISS: before=%+v after=%+v", before, after)
				}
				for _, prefix := range []string{bodyPrefix, headerPrefix} {
					if _, err := os.Stat(integrityFeaturePath(dir, prefix, key)); !os.IsNotExist(err) {
						t.Fatalf("corrupt entry file %s retained: %v", prefix, err)
					}
				}
				integrityFeatureBody(t, c, neighbor, body)
				if stats := c.Stats(); stats.ItemCount != 1 {
					t.Fatalf("exact-key discard affected neighboring entry: %+v", stats)
				}
			})
		}
	}
}

func TestIntegritySHA256RequiresPrivateDigestForLegacyEntries(t *testing.T) {
	for _, timestamped := range []bool{false, true} {
		t.Run(fmt.Sprintf("timestamped=%t", timestamped), func(t *testing.T) {
			dir := t.TempDir()
			const key, neighbor, body = "legacy-entry", "verified-neighbor", "legacy-package"
			c := integrityFeatureCache(t, dir, IntegritySHA256)
			if err := c.Store(integrityFeatureResource(body), neighbor); err != nil {
				t.Fatal(err)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			header := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\nDigest: sha-256=%s\r\n\r\n", len(body), integrityFeatureDigest([]byte(body)))
			if timestamped {
				header = storedAtPreamble + time.Now().UTC().Format(time.RFC3339Nano) + "\r\n" + header
			}
			for prefix, data := range map[string]string{bodyPrefix: body, headerPrefix: header} {
				if err := os.WriteFile(integrityFeaturePath(dir, prefix, key), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// A valid old entry, even with an origin Digest field, has no private
			// record. Required mode must not silently bless it during startup.
			strict := integrityFeatureCache(t, dir, IntegritySHA256)
			integrityFeatureMiss(t, strict, key)
			integrityFeatureBody(t, strict, neighbor, body)
		})
	}
}

func TestIntegritySHA256FreshenRetainsBodyChecksum(t *testing.T) {
	dir := t.TempDir()
	const key, body = "freshened-entry", "body-must-stay-the-same"
	c := integrityFeatureCache(t, dir, IntegritySHA256)
	if err := c.Store(integrityFeatureResource(body), key); err != nil {
		t.Fatal(err)
	}
	before := integrityFeatureReadRecord(t, dir, key)
	res := integrityFeatureBody(t, c, key, body)
	res.Header().Set("Cache-Control", "max-age=7200")
	res.Header().Set("X-Validated", "fresh")
	if err := c.Freshen(res, key); err != nil {
		t.Fatal(err)
	}
	after := integrityFeatureReadRecord(t, dir, key)
	if before.BodySHA256 != after.BodySHA256 || before.Size != after.Size || before.HeaderSHA256 == after.HeaderSHA256 {
		t.Fatalf("Freshen changed body integrity or failed to update header integrity: before=%+v after=%+v", before, after)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := integrityFeatureCache(t, dir, IntegritySHA256)
	current := integrityFeatureBody(t, restarted, key, body)
	if current.Header().Get("X-Validated") != "fresh" {
		t.Fatalf("freshened header did not persist: %v", current.Header())
	}
	if err := os.WriteFile(integrityFeaturePath(dir, bodyPrefix, key), bytes.Repeat([]byte("x"), len(body)), 0600); err != nil {
		t.Fatal(err)
	}
	integrityFeatureMiss(t, restarted, key)
}

func TestIntegritySHA256PrivateMetadataNeverReachesHandlerResponse(t *testing.T) {
	dir := t.TempDir()
	c := integrityFeatureCache(t, dir, IntegritySHA256)
	const body = "cached-response-package"
	req := httptest.NewRequest(http.MethodGet, "http://cache.example/private-metadata.deb", nil)
	key := NewRequestKey(req).String()
	origin := integrityFeatureResource(body)
	wantHeaders := origin.Header().Clone()
	if err := c.Store(origin, key); err != nil {
		t.Fatal(err)
	}
	record := integrityFeatureReadRecord(t, dir, key)
	res := integrityFeatureBody(t, c, key, body)
	if !reflect.DeepEqual(res.Header(), wantHeaders) {
		t.Fatalf("private metadata appeared in Resource.Header: %v", res.Header())
	}
	h := NewHandler(c, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("verified cached response unexpectedly reached upstream")
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(func() { h.writes.Wait() })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get(CacheHeader) != "HIT" || rec.Body.String() != body {
		t.Fatalf("cached response = status %d, headers %v, body %q", rec.Code, rec.Header(), rec.Body.String())
	}
	allowed := wantHeaders.Clone()
	for _, name := range []string{CacheHeader, "Age", "Via", "Accept-Ranges", "Last-Modified", "Content-Type"} {
		allowed[name] = nil
	}
	for name, values := range rec.Header() {
		if _, ok := allowed[name]; !ok {
			t.Errorf("unexpected response header %q: %v", name, values)
		}
		for _, value := range values {
			if strings.Contains(value, "HTTPCACHE/") || strings.Contains(value, record.BodySHA256) || strings.Contains(value, record.HeaderSHA256) || strings.Contains(value, record.KeySHA256) {
				t.Errorf("private integrity metadata leaked through %q", name)
			}
		}
	}
}

func TestIntegritySHA256RejectsMixedGenerationsAndMetadataTampering(t *testing.T) {
	for _, kind := range []string{"mixed-generation", "timestamp", "size", "body-digest", "key-binding"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			c := integrityFeatureCache(t, dir, IntegritySHA256)
			const key = "verified-generation"
			if err := c.Store(integrityFeatureResource("AAAA"), key); err != nil {
				t.Fatal(err)
			}
			path := integrityFeaturePath(dir, headerPrefix, key)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "mixed-generation" {
				// Emulate interruption between the sequential body/header renames.
				if err := c.Store(integrityFeatureResource("BBBB"), key); err != nil {
					t.Fatal(err)
				}
			} else {
				preamble, httpRecord, ok := bytes.Cut(raw, []byte("\r\n"))
				if !ok {
					t.Fatal("missing metadata line")
				}
				encoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(string(preamble), integrityPreamble))
				if err != nil {
					t.Fatal(err)
				}
				var record map[string]any
				if err := json.Unmarshal(encoded, &record); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "timestamp":
					record["stored_at"] = Clock().Add(24 * time.Hour).Format(time.RFC3339Nano)
				case "size":
					record["size"] = float64(3)
				case "body-digest":
					record["body_sha256"] = integrityFeatureDigest([]byte("BBBB"))
				case "key-binding":
					record["key_sha256"] = integrityFeatureDigest([]byte("another-key"))
				}
				encoded, err = json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				raw = append([]byte(integrityPreamble+base64.RawURLEncoding.EncodeToString(encoded)+"\r\n"), httpRecord...)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			restarted := integrityFeatureCache(t, dir, IntegritySHA256)
			integrityFeatureMiss(t, restarted, key)
		})
	}
}

func TestIntegritySHA256CorruptEntryRefetchesThroughHandler(t *testing.T) {
	dir := t.TempDir()
	c := integrityFeatureCache(t, dir, IntegritySHA256)
	req := httptest.NewRequest("GET", "https://cache.example/refetch", nil)
	key := NewRequestKey(req).String()
	if err := c.Store(integrityFeatureResource("old-package"), key); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(integrityFeaturePath(dir, bodyPrefix, key), []byte("bad-package"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	h := NewHandler(c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Header().Set("Date", Clock().Format(http.TimeFormat))
		_, _ = io.WriteString(w, "new-package")
	}))
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req.Clone(req.Context()))
		h.writes.Wait()
		if rec.Code != 200 || rec.Body.String() != "new-package" {
			t.Fatalf("refetch response: %d %q", rec.Code, rec.Body.String())
		}
		if i == 1 && rec.Header().Get(CacheHeader) != "HIT" {
			t.Fatalf("refetched object failed verification: %v", rec.Header())
		}
	}
	if calls != 1 {
		t.Fatalf("corruption refetch reached upstream %d times", calls)
	}
	integrityFeatureBody(t, c, key, "new-package")
}

func TestIntegritySHA256DiscardDoesNotDeleteNewerReplacement(t *testing.T) {
	dir := t.TempDir()
	c := integrityFeatureCache(t, dir, IntegritySHA256)
	const key = "replace-after-observation"
	if err := c.Store(integrityFeatureResource("old-package"), key); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(integrityFeaturePath(dir, headerPrefix, key))
	if err != nil {
		t.Fatal(err)
	}
	fault := &corruptEntryError{fingerprint: sha256Text(old), reason: "observed older damage"}
	if err := c.Store(integrityFeatureResource("new-package"), key); err != nil {
		t.Fatal(err)
	}
	c.(*cache).discardCorrupt(key, fault)
	integrityFeatureBody(t, c, key, "new-package")
}

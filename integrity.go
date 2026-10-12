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
	"time"
)

// IntegrityMode controls persisted cache verification.
type IntegrityMode uint8

const (
	IntegrityDisabled IntegrityMode = iota
	// IntegritySHA256 verifies body and private record checksums before exposing
	// an entry. Missing digests are misses, including pre-upgrade entries.
	IntegritySHA256
)

const integrityPreamble = "HTTPCACHE/2 "

type integrityRecord struct {
	StoredAt     time.Time `json:"stored_at"`
	Size         int64     `json:"size"`
	BodySHA256   string    `json:"body_sha256"`
	HeaderSHA256 string    `json:"header_sha256,omitempty"`
	KeySHA256    string    `json:"key_sha256"`
	fingerprint  string
}

type corruptEntryError struct {
	fingerprint string
	reason      string
}

func (e *corruptEntryError) Error() string { return "cache integrity failure: " + e.reason }

func sha256Text(b []byte) string {
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:])
}

func recordDigest(meta integrityRecord, record []byte) string {
	meta.HeaderSHA256 = ""
	encoded, _ := json.Marshal(meta) // Only concrete strings, integers and a valid timestamp.
	h := sha256.New()
	_, _ = h.Write(encoded)
	_, _ = h.Write([]byte{'\n'})
	_, _ = h.Write(record)
	return hex.EncodeToString(h.Sum(nil))
}

func validDigest(s string) bool {
	digest, err := hex.DecodeString(s)
	return err == nil && len(digest) == sha256.Size
}

func serializeIntegrityHeader(code int, header http.Header, meta integrityRecord) ([]byte, error) {
	var record bytes.Buffer
	fmt.Fprintf(&record, "HTTP/1.1 %d %s\r\n", code, http.StatusText(code))
	if err := headersToWriter(header, &record); err != nil {
		return nil, err
	}
	meta.HeaderSHA256 = recordDigest(meta, record.Bytes())
	encoded, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	preamble := integrityPreamble + base64.RawURLEncoding.EncodeToString(encoded) + "\r\n"
	return append([]byte(preamble), record.Bytes()...), nil
}

// decodeIntegrity validates private metadata and the exact serialized HTTP
// record. The metadata is never inserted into the response's header namespace.
func decodeIntegrity(raw []byte, key string, required bool) (*integrityRecord, error) {
	return decodeIntegrityRecord(raw, key, required, true)
}

func decodeIntegrityRecord(raw []byte, key string, required, checkKey bool) (*integrityRecord, error) {
	fail := func(reason string) (*integrityRecord, error) {
		return nil, &corruptEntryError{fingerprint: sha256Text(raw), reason: reason}
	}
	if !bytes.HasPrefix(raw, []byte(integrityPreamble)) {
		if required {
			return fail("required digest missing")
		}
		return nil, nil
	}
	cut := bytes.Index(raw, []byte("\r\n"))
	if cut < 0 {
		return fail("incomplete integrity record")
	}
	encoded, err := base64.RawURLEncoding.DecodeString(string(raw[len(integrityPreamble):cut]))
	if err != nil {
		return fail("invalid integrity encoding")
	}
	var meta integrityRecord
	if err := json.Unmarshal(encoded, &meta); err != nil || meta.StoredAt.IsZero() || meta.Size < 0 || !validDigest(meta.BodySHA256) || !validDigest(meta.HeaderSHA256) || !validDigest(meta.KeySHA256) {
		return fail("invalid integrity metadata")
	}
	if (checkKey && meta.KeySHA256 != sha256Text([]byte(key))) || meta.HeaderSHA256 != recordDigest(meta, raw[cut+2:]) {
		return fail("header, key or metadata checksum mismatch")
	}
	meta.fingerprint = sha256Text(raw)
	return &meta, nil
}

func parseIntegrityPreamble(line string) (*integrityRecord, error) {
	encoded, err := base64.RawURLEncoding.DecodeString(line[len(integrityPreamble):])
	if err != nil {
		return nil, err
	}
	var meta integrityRecord
	if err := json.Unmarshal(encoded, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func (c *cache) serializeCacheHeader(code int, header http.Header, at time.Time, key string, size int64, digest string) ([]byte, error) {
	if c.config.Integrity == IntegrityDisabled {
		return serializeStoredHeader(code, header, at)
	}
	return serializeIntegrityHeader(code, header, integrityRecord{StoredAt: at, Size: size, BodySHA256: digest, KeySHA256: sha256Text([]byte(key))})
}

func verifyStoredBody(body io.ReadSeeker, meta *integrityRecord, fingerprint string) error {
	h := sha256.New()
	size, err := io.Copy(h, body)
	if err != nil {
		return fmt.Errorf("read cached body for integrity verification: %w", err)
	}
	if size != meta.Size || hex.EncodeToString(h.Sum(nil)) != meta.BodySHA256 {
		return &corruptEntryError{fingerprint: fingerprint, reason: "body checksum or size mismatch"}
	}
	_, err = body.Seek(0, io.SeekStart)
	return err
}

// discardCorrupt removes only the exact observed generation. A replacement
// landing between failed verification and cleanup must not be deleted.
func (c *cache) discardCorrupt(key string, fault *corruptEntryError) {
	hashed := hashKey(key)
	unlock := c.lockKeyWriter(hashed)
	defer unlock()
	c.publishMu.Lock()
	defer c.publishMu.Unlock()
	f, err := c.fs.Open(headerPrefix + formatPrefix + hashed)
	if err != nil {
		return
	}
	raw, readErr := io.ReadAll(f)
	_ = f.Close()
	if readErr != nil || sha256Text(raw) != fault.fingerprint {
		return
	}
	size := int64(len(raw))
	if info, err := c.fs.Stat(bodyPrefix + formatPrefix + hashed); err == nil {
		size += info.Size()
	}
	if err := c.discardUnadmitted(key, hashed, size, time.Time{}); err != nil {
		debugf("discard corrupted cache entry: %v", err)
	}
}

func (c *cache) integrityMiss(key string, err error) error {
	return c.integrityResult(key, err, true)
}

func (c *cache) integrityResult(key string, err error, record bool) error {
	var fault *corruptEntryError
	if errors.As(err, &fault) {
		c.discardCorrupt(key, fault)
		if record {
			c.recordMiss()
		}
		return ErrNotFoundInCache
	}
	return err
}

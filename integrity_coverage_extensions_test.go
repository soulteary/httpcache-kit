package httpcache

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/soulteary/vfs-kit"
)

func TestIntegrityRejectsMalformedMetadataBeforeExposingResource(t *testing.T) {
	for _, tc := range []struct {
		name, metadata string
		alreadyEncoded bool
	}{
		{name: "invalid-base64", metadata: "!", alreadyEncoded: true},
		{name: "invalid-json", metadata: "{"},
		{name: "missing-fields", metadata: "{}"},
		{name: "negative-size", metadata: `{"stored_at":"2026-01-01T00:00:00Z","size":-1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			c := integrityFeatureCache(t, dir, IntegritySHA256)
			const key, neighbor = "malformed-metadata", "intact-metadata"
			if err := c.Store(integrityFeatureResource("payload"), key, neighbor); err != nil {
				t.Fatal(err)
			}
			encoded := tc.metadata
			if !tc.alreadyEncoded {
				encoded = base64.RawURLEncoding.EncodeToString([]byte(encoded))
			}
			raw := integrityPreamble + encoded + "\r\nHTTP/1.1 200 OK\r\n\r\n"
			if err := os.WriteFile(integrityFeaturePath(dir, headerPrefix, key), []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			before := c.Stats()
			integrityFeatureMiss(t, c, key)
			if after := c.Stats(); after.MissCount != before.MissCount+1 || after.HitCount != before.HitCount {
				t.Fatalf("malformed entry lookup stats: before=%+v after=%+v", before, after)
			}
			if _, err := os.Stat(integrityFeaturePath(dir, headerPrefix, key)); !os.IsNotExist(err) {
				t.Fatalf("malformed entry was not discarded: %v", err)
			}
			integrityFeatureBody(t, c, neighbor, "payload")
		})
	}
}

func TestIntegrityRejectsChecksumValidMalformedHTTPRecord(t *testing.T) {
	dir := t.TempDir()
	c := integrityFeatureCache(t, dir, IntegritySHA256)
	const key, body = "malformed-http-record", "payload"
	if err := c.Store(integrityFeatureResource(body), key); err != nil {
		t.Fatal(err)
	}
	record := []byte("HTTP/1.1 invalid OK\r\n\r\n")
	meta := integrityRecord{
		StoredAt: time.Now().UTC(), Size: int64(len(body)),
		BodySHA256: sha256Text([]byte(body)), KeySHA256: sha256Text([]byte(key)),
	}
	meta.HeaderSHA256 = recordDigest(meta, record)
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	raw := append([]byte(integrityPreamble+base64.RawURLEncoding.EncodeToString(encoded)+"\r\n"), record...)
	if err := os.WriteFile(integrityFeaturePath(dir, headerPrefix, key), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if h, err := c.Header(key); !errors.Is(err, ErrNotFoundInCache) || h.StatusCode != 0 {
		t.Fatalf("Header exposed invalid HTTP record: %+v, %v", h, err)
	}
	if _, err := os.Stat(integrityFeaturePath(dir, bodyPrefix, key)); !os.IsNotExist(err) {
		t.Fatalf("malformed HTTP entry body was not discarded: %v", err)
	}
}

type integrityCoverageFaultVFS struct {
	vfs.VFS
	readErr, seekErr error
	bodyCloses       int
}

type integrityCoverageFaultFile struct {
	vfs.RFile
	fs *integrityCoverageFaultVFS
}

func (fs *integrityCoverageFaultVFS) Open(path string) (vfs.RFile, error) {
	f, err := fs.VFS.Open(path)
	if err != nil || !strings.HasPrefix(path, bodyPrefix) {
		return f, err
	}
	return &integrityCoverageFaultFile{RFile: f, fs: fs}, nil
}

func (f *integrityCoverageFaultFile) Read(p []byte) (int, error) {
	if f.fs.readErr != nil {
		return 0, f.fs.readErr
	}
	return f.RFile.Read(p)
}

func (f *integrityCoverageFaultFile) Seek(offset int64, whence int) (int64, error) {
	if f.fs.seekErr != nil {
		return 0, f.fs.seekErr
	}
	return f.RFile.Seek(offset, whence)
}

func (f *integrityCoverageFaultFile) Close() error {
	f.fs.bodyCloses++
	return f.RFile.Close()
}

func TestIntegrityOperationalBodyErrorsPreserveCommittedEntry(t *testing.T) {
	for _, operation := range []string{"Header", "Retrieve"} {
		for _, fault := range []string{"read", "rewind"} {
			t.Run(operation+"/"+fault, func(t *testing.T) {
				fs := &integrityCoverageFaultVFS{VFS: vfs.Memory()}
				c := NewVFSCacheWithConfig(fs, &CacheConfig{Integrity: IntegritySHA256})
				t.Cleanup(func() { _ = c.Close() })
				const key = "temporary-backend-fault"
				if err := c.Store(integrityFeatureResource("payload"), key); err != nil {
					t.Fatal(err)
				}
				failure := errors.New("temporary body I/O failure")
				if fault == "read" {
					fs.readErr = failure
				} else {
					fs.seekErr = failure
				}
				before := c.Stats()
				var err error
				if operation == "Header" {
					_, err = c.Header(key)
				} else {
					var res *Resource
					res, err = c.Retrieve(key)
					if res != nil {
						_ = res.Close()
						t.Fatal("body I/O failure exposed a resource")
					}
				}
				if !errors.Is(err, failure) || errors.Is(err, ErrNotFoundInCache) {
					t.Fatalf("operational failure became a miss: %v", err)
				}
				if after := c.Stats(); after.HitCount != before.HitCount || after.MissCount != before.MissCount || after.ItemCount != before.ItemCount {
					t.Fatalf("operational failure changed entry or lookup counts: before=%+v after=%+v", before, after)
				}
				if fs.bodyCloses != 1 {
					t.Fatalf("failed verification retained body descriptor: closes=%d", fs.bodyCloses)
				}
				fs.readErr, fs.seekErr = nil, nil
				integrityFeatureBody(t, c, key, "payload")
			})
		}
	}
}

type integrityCoverageLease struct {
	reserved, released int64
	closes             int
}

func (l *integrityCoverageLease) Reserve(n int64) error { l.reserved += n; return nil }
func (l *integrityCoverageLease) Release(n int64)       { l.released += n }
func (l *integrityCoverageLease) Close()                { l.closes++ }

type integrityCoverageBudget struct {
	lease StagingLease
	err   error
	calls int
}

func (b *integrityCoverageBudget) NewLease(context.Context, StageKind) (StagingLease, error) {
	b.calls++
	return b.lease, b.err
}

func TestStagingLeaseCreationFailurePreservesOriginResponse(t *testing.T) {
	for _, failure := range []string{"refused", "refused-with-lease", "nil-lease"} {
		t.Run(failure, func(t *testing.T) {
			lease := &integrityCoverageLease{}
			budget := &integrityCoverageBudget{}
			if failure != "nil-lease" {
				budget.err = errors.New("storage admission unavailable")
			}
			if failure == "refused-with-lease" {
				budget.lease = lease
			}
			c := NewMemoryCacheWithConfig(&CacheConfig{})
			t.Cleanup(func() { _ = c.Close() })
			h := NewHandlerWithOptions(c, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Cache-Control", "max-age=3600")
				_, _ = io.WriteString(w, "uninterrupted-origin-response")
			}), &HandlerOptions{StagingBudget: budget, TempDir: t.TempDir()})
			req := httptest.NewRequest(http.MethodGet, "http://origin.test/admission-refused", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			h.writes.Wait()
			if rec.Code != http.StatusOK || rec.Body.String() != "uninterrupted-origin-response" {
				t.Fatalf("lease failure affected origin response: %d %q", rec.Code, rec.Body.String())
			}
			integrityFeatureMiss(t, c, NewRequestKey(req).String())
			if budget.calls != 1 {
				t.Fatalf("lease acquisition attempts=%d", budget.calls)
			}
			if failure == "refused-with-lease" && lease.closes != 1 {
				t.Fatalf("refused acquisition retained returned lease: closes=%d", lease.closes)
			}
		})
	}
}

type integrityCoveragePartialWriter struct{ calls int }

func (w *integrityCoveragePartialWriter) Write([]byte) (int, error) {
	w.calls++
	return 2, io.ErrShortWrite
}

func TestStagingShortWriteRefundsOnlyUnwrittenBytes(t *testing.T) {
	lease := &integrityCoverageLease{}
	output := &integrityCoveragePartialWriter{}
	w := &stagingWriter{ctx: context.Background(), w: output, lease: lease}
	n, err := w.Write([]byte("12345678"))
	if n != 2 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write result: %d, %v", n, err)
	}
	if lease.reserved != 8 || lease.released != 6 {
		t.Fatalf("short write budget: reserved=%d released=%d", lease.reserved, lease.released)
	}
}

func TestCanceledStagingDoesNotAcquireOrWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	budget := &integrityCoverageBudget{lease: &integrityCoverageLease{}}
	if stage, err := newHandlerStage(ctx, t.TempDir(), budget); !errors.Is(err, context.Canceled) || stage != nil {
		t.Fatalf("canceled stage creation: %v, %v", stage, err)
	}
	if budget.calls != 0 {
		t.Fatalf("canceled operation acquired %d leases", budget.calls)
	}
	lease := &integrityCoverageLease{}
	output := &integrityCoveragePartialWriter{}
	w := &stagingWriter{ctx: ctx, w: output, lease: lease}
	if n, err := w.Write([]byte("payload")); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled staging write: %d, %v", n, err)
	}
	if output.calls != 0 || lease.reserved != 0 {
		t.Fatalf("canceled write retained bytes: writes=%d reserved=%d", output.calls, lease.reserved)
	}
	r := &contextReader{ctx: ctx, r: strings.NewReader("payload")}
	if n, err := r.Read(make([]byte, 8)); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled staging read: %d, %v", n, err)
	}
}

func TestHandlerStageCreationFailureReleasesAdmission(t *testing.T) {
	lease := &integrityCoverageLease{}
	budget := &integrityCoverageBudget{lease: lease}
	if stage, err := newHandlerStage(context.Background(), t.TempDir()+"/missing", budget); !os.IsNotExist(err) || stage != nil {
		t.Fatalf("missing staging directory: %v, %v", stage, err)
	}
	if budget.calls != 1 || lease.closes != 1 {
		t.Fatalf("stage creation failure retained admission: acquisitions=%d closes=%d", budget.calls, lease.closes)
	}
}

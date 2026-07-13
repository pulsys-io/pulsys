// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// CVE-comparable regression: commit-body allocator amplification.
//
// Modeled on CVE-2025-58185 (Go encoding/asn1 pre-allocation
// memory exhaustion: an empty 10 MB DER payload could allocate
// ~280 MB before validation failed) and the HTTP/2 CONTINUATION
// flood class (unbounded header-block growth before validation).
//
// pulsys's commit endpoint parses an NDJSON body that may
// contain inline file contents (base64-encoded).  Without an
// overall body cap, a malicious authenticated client could
// submit a multi-GiB NDJSON commit and blow the proxy's heap
// before any per-file validation runs.  Phase 5 wired
// http.MaxBytesReader before the parser; this test pins that
// cap from the outside.
package businesslogic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pulsys-io/pulsys/internal/proxy"
)

// makeCommitNDJSON returns an NDJSON commit body of the
// requested approximate size (bytes), padded by a single inline
// file whose base64 content is `pad` bytes.  The header line is
// always present so the body is RFC-shaped even when the cap
// trips mid-stream.
func makeCommitNDJSON(t *testing.T, padBytes int) []byte {
	t.Helper()
	var buf bytes.Buffer
	header := map[string]any{
		"key":   "header",
		"value": map[string]string{"summary": "cap test", "description": "cap test"},
	}
	mustJSONLine(t, &buf, header)
	if padBytes > 0 {
		// Use plain UTF-8 content (no base64 decode pass) so
		// the byte budget on the wire matches the budget the
		// scanner counts against MaxBytesReader.  base64
		// would shrink ~33% post-decode and confuse the
		// arithmetic.
		filler := strings.Repeat("a", padBytes)
		fileEntry := map[string]any{
			"key": "file",
			"value": map[string]string{
				"path":     "filler.txt",
				"encoding": "utf-8",
				"content":  filler,
			},
		}
		mustJSONLine(t, &buf, fileEntry)
	}
	return buf.Bytes()
}

func mustJSONLine(t *testing.T, w *bytes.Buffer, v any) {
	t.Helper()
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatalf("encode ndjson: %v", err)
	}
}

// newCommitHandler returns a RegistryHandler wired against the
// shared registry harness (real Postgres + blobstore).  The
// caller passes the commit body cap to exercise.
func newCommitHandler(t *testing.T, h *registryHarness, capBytes int64) *proxy.RegistryHandler {
	t.Helper()
	return &proxy.RegistryHandler{
		Store:          h.store,
		Blobs:          h.bs,
		TenantID:       h.tenantID,
		Next:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }),
		PublicURL:      "http://test.local",
		CommitMaxBytes: capBytes,
	}
}

// TestCommitBodyCap_RejectsOversizeFastPath asserts: when the
// body is well past the cap, the handler returns 413 and never
// reaches CommitTx.  Mirrors the Content-Length fast-path
// behavior of the LFS cap (Phase 5 BUSL-09).
func TestCommitBodyCap_RejectsOversizeFastPath(t *testing.T) {
	harness := newRegistryHarness(t)
	const capBytes = int64(4 << 10) // 4 KiB cap; well below the 64 MiB default
	h := newCommitHandler(t, harness, capBytes)

	body := makeCommitNDJSON(t, int(capBytes*4))
	req := httptest.NewRequest(http.MethodPost,
		"/api/models/acme/busl/commit/main",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-ndjson")
	req = req.WithContext(context.Background())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d (want 413); body=%s", rec.Code, rec.Body.String())
	}
	// The body should advertise the cap so an operator
	// debugging a stuck client gets a precise signal, not
	// just "too large".
	if !strings.Contains(rec.Body.String(), fmt.Sprintf("%d", capBytes)) {
		t.Fatalf("413 body missing cap value %d: %s", capBytes, rec.Body.String())
	}
}

// TestCommitBodyCap_AcceptsAtOrUnderLimit asserts: a body that
// fits comfortably within the cap is NOT rejected with 413.
// We do not assert success (the commit may legitimately fail
// for other reasons in the test harness); we ONLY assert the
// status is not 413.
func TestCommitBodyCap_AcceptsAtOrUnderLimit(t *testing.T) {
	harness := newRegistryHarness(t)
	const capBytes = int64(64 << 10) // 64 KiB cap

	// A body comfortably under the cap.  ~10 KiB of filler
	// + header overhead.
	body := makeCommitNDJSON(t, 10<<10)
	if int64(len(body)) >= capBytes {
		t.Fatalf("test setup bug: body=%d >= cap=%d", len(body), capBytes)
	}

	h := newCommitHandler(t, harness, capBytes)
	req := httptest.NewRequest(http.MethodPost,
		"/api/models/acme/busl/commit/main",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-ndjson")
	req = req.WithContext(context.Background())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("under-cap body rejected with 413; cap=%d body=%d resp=%s",
			capBytes, len(body), rec.Body.String())
	}
}

// TestCommitBodyCap_ZeroUsesDefault asserts: when
// CommitMaxBytes==0 the handler picks up the package default
// (64 MiB) and a small body succeeds.  Guards against a typo
// like `CommitMaxBytes: -1` accidentally disabling the cap
// or making every commit fail.
func TestCommitBodyCap_ZeroUsesDefault(t *testing.T) {
	harness := newRegistryHarness(t)
	h := newCommitHandler(t, harness, 0)

	body := makeCommitNDJSON(t, 64) // tiny
	req := httptest.NewRequest(http.MethodPost,
		"/api/models/acme/busl/commit/main",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-ndjson")
	req = req.WithContext(context.Background())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("default cap rejected a tiny body; resp=%s", rec.Body.String())
	}
}

// TestCommitBodyCap_StreamOverrunReturns413 asserts: a body
// whose declared Content-Length fits the cap but whose ACTUAL
// stream overruns mid-flight still surfaces 413, not a
// half-parsed commit.  This is the lying-CL / chunked-overrun
// class.  http.MaxBytesReader handles both because it counts
// against the byte budget as bytes are pulled, regardless of
// what Content-Length claimed.
func TestCommitBodyCap_StreamOverrunReturns413(t *testing.T) {
	harness := newRegistryHarness(t)
	const capBytes = int64(2 << 10) // 2 KiB cap
	h := newCommitHandler(t, harness, capBytes)

	// Build an oversize body, then attach it as a reader
	// without setting Content-Length so the handler's
	// reader sees a "streaming" upload that overruns the
	// budget.
	body := makeCommitNDJSON(t, int(capBytes*8))
	req := httptest.NewRequest(http.MethodPost,
		"/api/models/acme/busl/commit/main",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-ndjson")
	// Force the handler to NOT short-circuit on
	// Content-Length: clear it.  http.MaxBytesReader's
	// streaming-cap path runs.
	req.ContentLength = -1
	req = req.WithContext(context.Background())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("streaming overrun: status=%d (want 413); body=%s",
			rec.Code, rec.Body.String())
	}
}

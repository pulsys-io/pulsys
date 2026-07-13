// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package businesslogic

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pulsys-io/pulsys/internal/blobstore"
	"github.com/pulsys-io/pulsys/internal/proxy"
)

// alwaysErrStore is the blobstore stub used by the size-cap
// tests: it never accepts a body, so even if the cap fails
// open the test catches it via the 5xx the handler would
// return.  We assert 413 specifically so a misclassified 5xx
// would fail too.
type alwaysErrStore struct{}

func (alwaysErrStore) Put(ctx context.Context, body io.Reader, opts blobstore.PutOptions) (blobstore.Stat, error) {
	// Drain to mirror real behavior (the handler streams the
	// body in), then refuse.  This is the path a future bug
	// that disabled the cap would walk; we want it to fail
	// loudly, not silently 413 by accident.
	_, _ = io.Copy(io.Discard, body)
	return blobstore.Stat{}, blobstore.ErrOIDMismatch
}

func (alwaysErrStore) Open(context.Context, string) (blobstore.ReadSeekCloser, blobstore.Stat, error) {
	return nil, blobstore.Stat{}, blobstore.ErrNotFound
}
func (alwaysErrStore) Stat(context.Context, string) (blobstore.Stat, error) {
	return blobstore.Stat{}, blobstore.ErrNotFound
}
func (alwaysErrStore) Delete(context.Context, string) error { return nil }

// newLFSHandler returns a RegistryHandler with the given cap
// (in bytes) suitable for direct ServeHTTP calls.  The other
// dependencies (Store, Next, etc.) are nil because the
// /lfs-storage path doesn't need them.
func newLFSHandler(maxBytes int64) *proxy.RegistryHandler {
	return &proxy.RegistryHandler{
		Blobs:       alwaysErrStore{},
		LFSMaxBytes: maxBytes,
	}
}

// validOID is the sha256 of an arbitrary 32-byte payload --
// shape only, the request never reaches the OID check because
// our stub always errors.
func validOID() string {
	sum := sha256.Sum256([]byte("phase5-busl-lfs-cap-test"))
	return hex.EncodeToString(sum[:])
}

// TestLFSUpload_RejectsOversizeContentLength pins the fast-
// path 413: a Content-Length that exceeds the cap is rejected
// BEFORE we open the blobstore.  The body is irrelevant; we
// send a single byte and prove the handler hits the CL guard
// without consuming it.
func TestLFSUpload_RejectsOversizeContentLength(t *testing.T) {
	cap := int64(1024)
	h := newLFSHandler(cap)

	req := httptest.NewRequest(http.MethodPut,
		"/lfs-storage/"+validOID(), bytes.NewReader([]byte("a")))
	req.ContentLength = cap + 1
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s want 413", rec.Code, rec.Body.String())
	}
}

// TestLFSUpload_RejectsOversizeStream covers the chunked /
// lying-CL path: the request has no Content-Length (chunked-
// like) but streams cap+1 bytes.  lfsLimitReader trips Tripped
// and we should still see 413.  Without the trip flag the
// blobstore would see a truncated stream and fail with 5xx;
// the test asserts 413 specifically so that misclassification
// would surface.
func TestLFSUpload_RejectsOversizeStream(t *testing.T) {
	cap := int64(1024)
	h := newLFSHandler(cap)

	body := bytes.Repeat([]byte("x"), int(cap+1))
	req := httptest.NewRequest(http.MethodPut,
		"/lfs-storage/"+validOID(), bytes.NewReader(body))
	req.ContentLength = -1 // simulate chunked
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s want 413", rec.Code, rec.Body.String())
	}
}

// TestLFSUpload_AcceptsExactlyAtCap pins the boundary: a body
// of EXACTLY cap bytes must NOT trip the limit reader.  Off-
// by-one in lfsLimitReader.N would fail this.  The blobstore
// stub then rejects with OID mismatch (422); we assert that
// instead of 413 to prove the cap path didn't fire.
func TestLFSUpload_AcceptsExactlyAtCap(t *testing.T) {
	cap := int64(1024)
	h := newLFSHandler(cap)

	body := bytes.Repeat([]byte("y"), int(cap))
	req := httptest.NewRequest(http.MethodPut,
		"/lfs-storage/"+validOID(), bytes.NewReader(body))
	req.ContentLength = cap
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s want 422 (OID mismatch from stub)", rec.Code, rec.Body.String())
	}
}

// TestLFSUpload_DefaultCapApplied pins that omitting
// LFSMaxBytes uses the package default (200 GiB) rather than
// "no cap".  We can't realistically stream 200 GiB in a test;
// instead, we set a Content-Length larger than the default
// and assert 413 -- proving the default isn't infinite.
func TestLFSUpload_DefaultCapApplied(t *testing.T) {
	h := &proxy.RegistryHandler{
		Blobs:       alwaysErrStore{},
		LFSMaxBytes: 0, // use default
	}

	const tooBig = int64(300) << 30 // 300 GiB > 200 GiB default
	req := httptest.NewRequest(http.MethodPut,
		"/lfs-storage/"+validOID(), bytes.NewReader([]byte("a")))
	req.ContentLength = tooBig
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s want 413 (default cap should apply)",
			rec.Code, rec.Body.String())
	}
}

// TestLFSUpload_NegativeCapTreatedAsUnset pins the defensive
// branch: a misconfigured LFSMaxBytes=-1 (typo) falls back to
// the default rather than silently disabling the cap.  Without
// this guard a single character in the operator's config file
// would expose the disk-DoS surface.
func TestLFSUpload_NegativeCapTreatedAsUnset(t *testing.T) {
	h := &proxy.RegistryHandler{
		Blobs:       alwaysErrStore{},
		LFSMaxBytes: -1,
	}

	const tooBig = int64(300) << 30
	req := httptest.NewRequest(http.MethodPut,
		"/lfs-storage/"+validOID(), bytes.NewReader([]byte("a")))
	req.ContentLength = tooBig
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s want 413 (negative cap should fall back to default)",
			rec.Code, rec.Body.String())
	}
}

// TestLFSUpload_RejectsMissingOID is a contract-shape check:
// the handler validates the URL before reading the body, so
// an attacker can't probe the cap by sending oversize bodies
// against a bogus path.
func TestLFSUpload_RejectsMissingOID(t *testing.T) {
	h := newLFSHandler(1024)

	req := httptest.NewRequest(http.MethodPut,
		"/lfs-storage/", bytes.NewReader([]byte("x")))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s want 400", rec.Code, rec.Body.String())
	}
}

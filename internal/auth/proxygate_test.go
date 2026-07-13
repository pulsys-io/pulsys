// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package auth

import (
	"context"
	"testing"
	"time"
)

// TestPATGate_AllowlistPathsBypassStore verifies that /healthz,
// /readyz, and /metrics admit without ever consulting the store.
// Implementation detail: a Store-less PATGate (Store: nil) would panic
// on lookup, so a successful 0 return means the allowlist short-
// circuited correctly.
func TestPATGate_AllowlistPathsBypassStore(t *testing.T) {
	g := &PATGate{Store: nil}
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		status, _ := g.Check(context.Background(), nil, []byte(path))
		if status != 0 {
			t.Errorf("%s: expected 0 (admit), got %d", path, status)
		}
	}
}

func TestPATGate_RejectsMissingAuth(t *testing.T) {
	g := NewPATGate(&stubAuthStore{})
	status, reason := g.Check(context.Background(), nil, []byte("/api/models/x"))
	if status != 401 {
		t.Fatalf("status = %d, want 401", status)
	}
	if reason == "" {
		t.Errorf("expected non-empty reason")
	}
}

func TestPATGate_RejectsMalformedBearer(t *testing.T) {
	g := NewPATGate(&stubAuthStore{})
	for _, h := range []string{
		"",
		"NotBearer xyz",
		"Bearer ",
		"Bearer    ",
	} {
		status, _ := g.Check(context.Background(), []byte(h), []byte("/api/models/x"))
		if status != 401 {
			t.Errorf("auth=%q: status=%d, want 401", h, status)
		}
	}
}

func TestPATGate_RejectsNonPATBearer(t *testing.T) {
	g := NewPATGate(&stubAuthStore{})
	status, reason := g.Check(context.Background(), []byte("Bearer hf_xxxsomerealhftoken"), []byte("/api/models/x"))
	if status != 401 {
		t.Fatalf("status = %d, want 401", status)
	}
	if reason == "" {
		t.Errorf("expected non-empty reason")
	}
}

// TestPATGate_RevokedTokenIsRejected is the gate-level mirror of the
// user's report: when the underlying Store rejects the PAT with
// ErrInvalidSession, the gate must return 401 with a reason that
// names the failure mode.
func TestPATGate_RevokedTokenIsRejected(t *testing.T) {
	g := NewPATGate(&stubAuthStore{patErr: ErrInvalidSession})
	status, reason := g.Check(context.Background(),
		[]byte("Bearer pulsys_7be71e62_revokedtokenvalue"),
		[]byte("/api/models/acme/widget"),
	)
	if status != 401 {
		t.Fatalf("status = %d, want 401", status)
	}
	if reason == "" || (reason == "authentication failed") {
		t.Errorf("reason = %q, want something mentioning revoked / invalid", reason)
	}
}

// TestPATGate_ValidTokenIsAdmittedAndCached verifies the two-state
// happy path: the first call hits the store, the second is served
// from the in-memory cache.  We assert cache-hit by switching the
// stub to return an error after the first call -- the gate must
// still admit because the cached "valid" entry has not expired.
func TestPATGate_ValidTokenIsAdmittedAndCached(t *testing.T) {
	tok := &APIToken{ID: "tok-1", TenantID: "tnt-1", Scopes: []string{"models:read"}}
	stub := &stubAuthStore{patTok: tok}
	g := NewPATGate(stub)

	bearer := []byte("Bearer pulsys_deadbeef_validsecretvalue")
	path := []byte("/api/models/acme/widget")

	if status, _ := g.Check(context.Background(), bearer, path); status != 0 {
		t.Fatalf("first call: status = %d, want 0", status)
	}

	// Simulate the row being revoked between calls.  The cached entry
	// must still admit because PositiveTTL has not elapsed; this is
	// the documented revocation window.
	stub.patTok = nil
	stub.patErr = ErrInvalidSession
	if status, _ := g.Check(context.Background(), bearer, path); status != 0 {
		t.Fatalf("cache hit: status = %d, want 0 (cached as valid)", status)
	}
}

// TestPATGate_RevocationTakesEffectAfterTTL drives the gate's clock
// past PositiveTTL and verifies that the stale cached entry is no
// longer trusted: the next call must re-consult the store, see the
// rejection, and propagate 401.
func TestPATGate_RevocationTakesEffectAfterTTL(t *testing.T) {
	tok := &APIToken{ID: "tok-1", TenantID: "tnt-1"}
	stub := &stubAuthStore{patTok: tok}
	now := time.Unix(1_700_000_000, 0)
	g := &PATGate{
		Store:       stub,
		PositiveTTL: 5 * time.Second,
		Clock:       func() time.Time { return now },
	}

	bearer := []byte("Bearer pulsys_deadbeef_validsecretvalue")
	path := []byte("/api/models/acme/widget")

	if status, _ := g.Check(context.Background(), bearer, path); status != 0 {
		t.Fatalf("first call: want 0, got %d", status)
	}

	// Revoke in the store.  Within the TTL the cache still admits.
	stub.patTok = nil
	stub.patErr = ErrInvalidSession
	if status, _ := g.Check(context.Background(), bearer, path); status != 0 {
		t.Fatalf("within TTL: want 0 (cached), got %d", status)
	}

	// Advance past the TTL.  Next call must hit the store and reject.
	now = now.Add(6 * time.Second)
	if status, _ := g.Check(context.Background(), bearer, path); status != 401 {
		t.Fatalf("after TTL: want 401 (cache expired, store rejects), got %d", status)
	}
}

// TestPATGate_InvalidateForcesImmediateRecheck verifies the
// admin-side hook for "the operator just revoked this token, evict
// the cached approval on this node so the next request fails."
func TestPATGate_InvalidateForcesImmediateRecheck(t *testing.T) {
	tok := &APIToken{ID: "tok-1", TenantID: "tnt-1"}
	stub := &stubAuthStore{patTok: tok}
	g := NewPATGate(stub)

	bearer := "Bearer pulsys_deadbeef_validsecretvalue"
	plain := "pulsys_deadbeef_validsecretvalue"
	path := []byte("/api/models/acme/widget")

	if status, _ := g.Check(context.Background(), []byte(bearer), path); status != 0 {
		t.Fatalf("first call: want 0, got %d", status)
	}

	stub.patTok = nil
	stub.patErr = ErrInvalidSession
	g.Invalidate(plain)

	if status, _ := g.Check(context.Background(), []byte(bearer), path); status != 401 {
		t.Fatalf("after Invalidate: want 401, got %d", status)
	}
}

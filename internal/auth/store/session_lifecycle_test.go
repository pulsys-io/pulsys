// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Session lifecycle regression test (OWASP WSTG-SESS-03, SESS-07).
//
// PURPOSE
//   The session credential (pulsys_session) is server-side
//   randomly-minted on every CreateSession call.  This file pins
//   the three invariants the LookupSession path must enforce:
//
//     1. UNIQUENESS / fixation defense (SESS-03): every
//        CreateSession returns a token whose hash is distinct from
//        every previously issued token.  An attacker cannot pre-set
//        a victim's session ID because Pulsys generates the token
//        server-side at /auth/session time, and the next CreateSession
//        always yields fresh bytes (32 bytes from crypto/rand).
//
//     2. EXPIRY enforcement (SESS-07): a session past expires_at
//        MUST not be looked up successfully, even before any
//        background cleanup job has run.  LookupSession enforces
//        `expires_at > now()` in the WHERE clause.
//
//     3. REVOCATION enforcement: a session whose revoked_at is set
//        MUST not be looked up successfully on the next call -- no
//        race window where the revoked credential still passes.
//
//   These properties were already part of the schema design (see
//   internal/auth/store/pg.go LookupSession), but they have never
//   had explicit regression tests.  Adding them here makes a
//   future "let's relax this WHERE clause for caching" change
//   fail in CI.

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/auth"
	authstore "github.com/pulsys-io/pulsys/internal/auth/store"
	"github.com/pulsys-io/pulsys/internal/testpg"
)

// newSessionFixture acquires a Postgres pool and seeds one tenant +
// one user; returns the store, the user ID, and the tenant ID for
// CreateSession callers.
func newSessionFixture(t *testing.T) (st *authstore.PG, userID, tenantID string) {
	t.Helper()
	pool := testpg.Acquire(t)
	st = authstore.NewPG(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tid, err := st.EnsureTenant(ctx, "sess-lifecycle", "Session Lifecycle Test Tenant")
	if err != nil {
		t.Fatalf("ensure tenant: %v", err)
	}
	uid, err := st.CreateUserOIDC(ctx, auth.User{
		TenantID:    tid,
		Email:       "sessuser@local",
		DisplayName: "sessuser",
		Role:        auth.RoleMember,
		OIDCSub:     "sub-sess",
		IsActive:    true,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return st, uid, tid //nolint:nakedret // named returns above for godoc readability
}

// TestSession_TokensAreUnique runs CreateSession many times and
// asserts no plain token recurs.  This is the structural defense
// against session fixation: an attacker who pre-sets a session
// cookie can never have it match a token CreateSession will later
// issue, because token generation never observes the request.
func TestSession_TokensAreUnique(t *testing.T) {
	st, uid, tid := newSessionFixture(t)
	ctx := context.Background()

	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		s, err := st.CreateSession(ctx, uid, tid, 1*time.Hour)
		if err != nil {
			t.Fatalf("create session #%d: %v", i, err)
		}
		if len(s.PlainToken) < 16 {
			t.Fatalf("session token too short: %d bytes -- entropy budget is 32 bytes / 256 bits", len(s.PlainToken))
		}
		if _, dup := seen[s.PlainToken]; dup {
			t.Fatalf("SESSION FIXATION RISK: CreateSession returned a duplicate token at iter %d (%q)", i, s.PlainToken)
		}
		seen[s.PlainToken] = struct{}{}
	}
}

// TestSession_CSRFTokensAreUniqueAcrossSessions asserts that each
// session's CSRF token is independently generated -- a CSRF token
// shared across sessions would let an attacker who phished one
// CSRF value reuse it against a different session.
func TestSession_CSRFTokensAreUniqueAcrossSessions(t *testing.T) {
	st, uid, tid := newSessionFixture(t)
	ctx := context.Background()
	seen := make(map[string]struct{}, 50)
	for i := 0; i < 50; i++ {
		s, err := st.CreateSession(ctx, uid, tid, 1*time.Hour)
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		if len(s.CSRFToken) < 8 {
			t.Fatalf("CSRF token too short: %d bytes", len(s.CSRFToken))
		}
		if _, dup := seen[s.CSRFToken]; dup {
			t.Fatalf("CSRF token reused across sessions at iter %d", i)
		}
		seen[s.CSRFToken] = struct{}{}
	}
}

// TestSession_ExpiredSessionRejected creates a session with a TTL
// well in the past (negative duration) and asserts LookupSession
// returns ErrInvalidSession without ever returning the session
// row.  This is the SESS-07 invariant.
//
// A negative TTL is the cleanest way to simulate expiry without
// time.Sleep, which would slow CI by N milliseconds per iteration.
func TestSession_ExpiredSessionRejected(t *testing.T) {
	st, uid, tid := newSessionFixture(t)
	ctx := context.Background()

	s, err := st.CreateSession(ctx, uid, tid, -1*time.Minute)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if !s.ExpiresAt.Before(time.Now()) {
		t.Fatalf("test bug: ExpiresAt %v is not in the past", s.ExpiresAt)
	}
	got, _, err := st.LookupSession(ctx, s.PlainToken)
	if err == nil {
		t.Fatalf("SESS-07 VIOLATION: expired session was looked up successfully (id=%s expires=%v)",
			got.ID, got.ExpiresAt)
	}
	if err != auth.ErrInvalidSession {
		t.Errorf("expected auth.ErrInvalidSession, got %v", err)
	}
}

// TestSession_RevokedSessionRejected creates a session, revokes
// it, and asserts the next LookupSession refuses it.  This is the
// regression hedge for the 2026-05-21 incident: a revoked
// credential MUST be rejected on the very next call, with no
// grace period.
func TestSession_RevokedSessionRejected(t *testing.T) {
	st, uid, tid := newSessionFixture(t)
	ctx := context.Background()

	s, err := st.CreateSession(ctx, uid, tid, 1*time.Hour)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	// Sanity: pre-revoke lookup succeeds.
	if _, _, err := st.LookupSession(ctx, s.PlainToken); err != nil {
		t.Fatalf("pre-revoke lookup failed: %v", err)
	}
	if err := st.RevokeSession(ctx, s.PlainToken); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	got, _, err := st.LookupSession(ctx, s.PlainToken)
	if err == nil {
		t.Fatalf("WSTG-SESS-07 / 2026-05-21 REGRESSION: revoked session still resolves (id=%s)", got.ID)
	}
	if err != auth.ErrInvalidSession {
		t.Errorf("expected auth.ErrInvalidSession, got %v", err)
	}
}

// TestSession_LookupRequiresHashedToken is the negative test for
// the SHA-256 token-hash design.  The DB stores token_hash, not
// the plaintext token.  Looking up a session by the *hash* string
// (the value an attacker would see if they only obtained a DB
// dump) MUST NOT succeed.
func TestSession_LookupRequiresHashedToken(t *testing.T) {
	st, uid, tid := newSessionFixture(t)
	ctx := context.Background()

	s, err := st.CreateSession(ctx, uid, tid, 1*time.Hour)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	// Hash of the plaintext is what the DB stores; using it as
	// the LookupSession arg would only succeed if the lookup
	// erroneously skipped the TokenHash() call.
	hashedAsString := string(auth.TokenHash(s.PlainToken))
	_, _, err = st.LookupSession(ctx, hashedAsString)
	if err == nil {
		t.Fatal("LookupSession resolved a HASHED token; the lookup must hash its input first or the DB dump is a credential")
	}
}

// TestSession_ConcurrentCreationsUnique stresses CreateSession
// from 16 goroutines * 64 iterations each.  Catches any race in
// the random-bytes path that would let two concurrent callers
// receive the same token (would manifest as a UNIQUE constraint
// violation on insert OR -- worse -- duplicate insertions across
// different shards if a future migration partitioned the table).
func TestSession_ConcurrentCreationsUnique(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping under -short")
	}
	st, uid, tid := newSessionFixture(t)
	const goroutines = 16
	const iter = 64
	type result struct {
		tok string
		err error
	}
	ch := make(chan result, goroutines*iter)
	for g := 0; g < goroutines; g++ {
		go func() {
			ctx := context.Background()
			for i := 0; i < iter; i++ {
				s, err := st.CreateSession(ctx, uid, tid, 1*time.Hour)
				if err != nil {
					ch <- result{err: err}
					continue
				}
				ch <- result{tok: s.PlainToken}
			}
		}()
	}
	seen := make(map[string]struct{}, goroutines*iter)
	for i := 0; i < goroutines*iter; i++ {
		r := <-ch
		if r.err != nil {
			t.Fatalf("create session: %v", r.err)
		}
		if _, dup := seen[r.tok]; dup {
			t.Fatalf("duplicate token under concurrent load: %q", r.tok)
		}
		seen[r.tok] = struct{}{}
	}
}

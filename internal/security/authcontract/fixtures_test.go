// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package authcontract

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	adminstore "github.com/pulsys-io/pulsys/internal/admin/store"
	"github.com/pulsys-io/pulsys/internal/auth"
	authstore "github.com/pulsys-io/pulsys/internal/auth/store"
	"github.com/pulsys-io/pulsys/internal/testpg"
	"github.com/jackc/pgx/v5/pgxpool"
)

// userIDs holds the per-role user UUIDs so RefreshSessions can mint
// new session rows without re-creating users.
type userIDs struct {
	reader, member, admin, owner string
}

// fixtures owns the seeded tenant + users + PATs + sessions used by
// every matrix run.  All identifiers are stable for the lifetime of
// the test so failure messages reference real, locatable rows.
//
// The fixtures are deliberately "complete from one tenant's
// perspective": every role and every scope class is present.
// Cross-tenant isolation is exercised separately by the admin/store
// tests; this matrix focuses on the auth gate itself.
type fixtures struct {
	Pool       *pgxpool.Pool
	AuthStore  *authstore.PG
	AdminStore *adminstore.AdminStore
	TenantID   string
	TenantName string

	// Plaintext PAT secrets (returned by GeneratePAT once at
	// creation; hashed in the tokens table).  Each PAT carries
	// exactly one scope class to keep the matrix unambiguous.
	patRead     string
	patWrite    string
	patAdminAll string
	patRevoked  string
	patExpired  string

	// Sessions per role.  Each carries (PlainToken, CSRFToken) so
	// the request applier can attach the matching cookies + header.
	// These rows are mutable across the matrix run: the admin
	// /auth/logout endpoint revokes whatever session cookie it sees,
	// so RefreshSessions re-mints them between endpoint sub-tests.
	sessReader  *auth.Session
	sessMember  *auth.Session
	sessAdmin   *auth.Session
	sessOwner   *auth.Session
	sessRevoked *auth.Session

	// Per-role user IDs, needed by RefreshSessions to mint new
	// session rows without re-creating the user rows.
	users userIDs
}

// newFixtures stands up an isolated database and seeds every row the
// matrix needs.  Skips the test (via testpg.Acquire) when no Postgres
// is configured.
func newFixtures(t *testing.T) *fixtures {
	t.Helper()
	pool := testpg.Acquire(t)
	pgAuth := authstore.NewPG(pool)
	pgAdmin := adminstore.NewAdminStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantName := "authcontract"
	tenantID, err := pgAuth.EnsureTenant(ctx, tenantName, "Auth Contract Tenant")
	if err != nil {
		t.Fatalf("ensure tenant: %v", err)
	}

	f := &fixtures{
		Pool:       pool,
		AuthStore:  pgAuth,
		AdminStore: pgAdmin,
		TenantID:   tenantID,
		TenantName: tenantName,
	}

	// Users (one per role).  OIDC sub is synthetic; we don't go
	// through the real IdP for the matrix.
	makeUser := func(sub, email string, role auth.Role) string {
		id, err := pgAuth.CreateUserOIDC(ctx, auth.User{
			TenantID: tenantID, Email: email, DisplayName: email,
			Role: role, OIDCSub: sub, IsActive: true,
		})
		if err != nil {
			t.Fatalf("create user %s: %v", role, err)
		}
		return id
	}
	f.users = userIDs{
		reader: makeUser("sub-reader", "reader@authcontract.local", auth.RoleReader),
		member: makeUser("sub-member", "member@authcontract.local", auth.RoleMember),
		admin:  makeUser("sub-admin", "admin@authcontract.local", auth.RoleAdmin),
		owner:  makeUser("sub-owner", "owner@authcontract.local", auth.RoleOwner),
	}

	// Sessions (one valid per role plus one revoked).  Mint the
	// initial set via the same helper the per-endpoint refresh uses
	// so the two code paths stay in sync.
	f.RefreshSessions(t)
	ownerUID := f.users.owner

	// PATs.  CreateToken hashes the supplied secret; we call
	// GeneratePAT first to mirror the production handler.
	mkPAT := func(name string, scopes []string, expires *time.Time) string {
		display, prefix, hash, err := auth.GeneratePAT()
		if err != nil {
			t.Fatalf("generate pat: %v", err)
		}
		if _, err := pgAdmin.CreateToken(ctx, tenantID, ownerUID, name, prefix, hash, scopes, expires); err != nil {
			t.Fatalf("create pat %s: %v", name, err)
		}
		return display
	}
	f.patRead = mkPAT("read", []string{"admin:read"}, nil)
	f.patWrite = mkPAT("write", []string{"admin:write"}, nil)
	f.patAdminAll = mkPAT("adminall", []string{"admin:*"}, nil)

	// Revoked PAT: create, then UPDATE tokens SET revoked_at = now().
	// CreateToken returns res.ID which RevokeToken takes.  This is the
	// exact path the admin UI walks when an operator clicks Revoke.
	displayRev, prefixRev, hashRev, err := auth.GeneratePAT()
	if err != nil {
		t.Fatalf("gen revoked pat: %v", err)
	}
	revRes, err := pgAdmin.CreateToken(ctx, tenantID, ownerUID, "revoked", prefixRev, hashRev, []string{"admin:*"}, nil)
	if err != nil {
		t.Fatalf("create revoked pat: %v", err)
	}
	if _, _, err := pgAdmin.RevokeToken(ctx, tenantID, revRes.ID); err != nil {
		t.Fatalf("revoke pat: %v", err)
	}
	f.patRevoked = displayRev

	// Expired PAT: create with expires_at = past.
	past := time.Now().UTC().Add(-1 * time.Hour)
	f.patExpired = mkPAT("expired", []string{"admin:*"}, &past)

	return f
}

// Apply mutates req in place to carry the credential cred.  This is
// the single source of truth for "what does class X look like on the
// wire"; the matrix runner calls this and nothing else.
//
// For sessions on mutating methods, both the CSRF cookie and header
// are set so the request survives auth.CSRFProtect.  The matrix
// asserts auth contracts, not CSRF behavior -- a separate test
// (internal/auth/csrf_test.go) covers CSRF in isolation.
func (f *fixtures) Apply(req *http.Request, cred Credential) {
	switch cred {
	case CredAnonymous:
		// no-op
	case CredBogusPAT:
		req.Header.Set("Authorization", "Bearer pulsys_deadbeef_thistokenwasneverissued")
	case CredRevokedPAT:
		req.Header.Set("Authorization", "Bearer "+f.patRevoked)
	case CredExpiredPAT:
		req.Header.Set("Authorization", "Bearer "+f.patExpired)
	case CredPATScopeRead:
		req.Header.Set("Authorization", "Bearer "+f.patRead)
	case CredPATScopeWrite:
		req.Header.Set("Authorization", "Bearer "+f.patWrite)
	case CredPATScopeAdminAll:
		req.Header.Set("Authorization", "Bearer "+f.patAdminAll)
	case CredSessionReader:
		f.applySession(req, f.sessReader)
	case CredSessionMember:
		f.applySession(req, f.sessMember)
	case CredSessionAdmin:
		f.applySession(req, f.sessAdmin)
	case CredSessionOwner:
		f.applySession(req, f.sessOwner)
	case CredSessionRevoked:
		f.applySession(req, f.sessRevoked)
	default:
		panic(fmt.Sprintf("authcontract: unknown credential %d", int(cred)))
	}
}

// RefreshSessions re-mints every session row.  This is the antidote
// to /auth/logout's "revoke the session cookie I just saw" side
// effect: the matrix calls /auth/logout with every credential class,
// which revokes our seeded session cookies.  Calling RefreshSessions
// between endpoints guarantees each endpoint sees a fresh set of
// live sessions.  We also re-revoke the "revoked" session so the
// CredSessionRevoked class stays meaningfully invalid.
//
// Cost: 5 INSERTs + 1 UPDATE per call.  Run once per endpoint in the
// matrix; trivially cheap.
func (f *fixtures) RefreshSessions(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mk := func(uid string) *auth.Session {
		s, err := f.AuthStore.CreateSession(ctx, uid, f.TenantID, 24*time.Hour)
		if err != nil {
			t.Fatalf("refresh session: %v", err)
		}
		return s
	}
	f.sessReader = mk(f.users.reader)
	f.sessMember = mk(f.users.member)
	f.sessAdmin = mk(f.users.admin)
	f.sessOwner = mk(f.users.owner)
	f.sessRevoked = mk(f.users.member)
	if err := f.AuthStore.RevokeSession(ctx, f.sessRevoked.PlainToken); err != nil {
		t.Fatalf("re-revoke session: %v", err)
	}
}

func (f *fixtures) applySession(req *http.Request, sess *auth.Session) {
	req.AddCookie(&http.Cookie{
		Name:  auth.SessionCookieName,
		Value: sess.PlainToken,
	})
	if !auth.SafeMethod(req.Method) && !auth.CSRFExemptPath(req.URL.Path) {
		req.Header.Set(auth.CSRFHeaderName, sess.CSRFToken)
		req.AddCookie(&http.Cookie{
			Name:  auth.CSRFCookieName,
			Value: sess.CSRFToken,
		})
	}
}

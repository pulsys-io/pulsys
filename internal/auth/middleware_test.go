// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// stubAuthStore satisfies auth.Store for middleware unit tests.  Only
// LookupAPIToken / LookupSession / TouchSession are exercised by the
// Authenticator; every other method panics so an over-broad change to
// the middleware would surface as a loud test failure instead of a
// silent panic in production.
type stubAuthStore struct {
	patTok *APIToken
	patErr error

	sessSess *Session
	sessUser *User
	sessErr  error
}

func (s *stubAuthStore) LookupAPIToken(_ context.Context, _ string) (*APIToken, error) {
	return s.patTok, s.patErr
}

func (s *stubAuthStore) LookupSession(_ context.Context, _ string) (*Session, *User, error) {
	return s.sessSess, s.sessUser, s.sessErr
}

func (s *stubAuthStore) TouchSession(_ context.Context, _ string, _ time.Duration) error {
	return nil
}

func (s *stubAuthStore) EnsureTenant(context.Context, string, string) (string, error) {
	panic("EnsureTenant not used by middleware")
}
func (s *stubAuthStore) GetTenantIDByName(context.Context, string) (string, error) {
	panic("GetTenantIDByName not used by middleware")
}
func (s *stubAuthStore) GetOIDCProviderByTenant(context.Context, string) (*OIDCProvider, error) {
	panic("GetOIDCProviderByTenant not used by middleware")
}
func (s *stubAuthStore) UpsertOIDCProvider(context.Context, OIDCProvider) error {
	panic("UpsertOIDCProvider not used by middleware")
}
func (s *stubAuthStore) FindUserByOIDCSub(context.Context, string, string) (*User, error) {
	panic("FindUserByOIDCSub not used by middleware")
}
func (s *stubAuthStore) CreateUserOIDC(context.Context, User) (string, error) {
	panic("CreateUserOIDC not used by middleware")
}
func (s *stubAuthStore) UpdateUserProfile(context.Context, string, string, string, Role) error {
	panic("UpdateUserProfile not used by middleware")
}
func (s *stubAuthStore) CreateSession(context.Context, string, string, time.Duration) (*Session, error) {
	panic("CreateSession not used by middleware")
}
func (s *stubAuthStore) RevokeSession(context.Context, string) error {
	panic("RevokeSession not used by middleware")
}
func (s *stubAuthStore) WithTenant(context.Context, string, func(ctx context.Context) error) error {
	panic("WithTenant not used by middleware")
}

// TestPATMiddleware_RevokedTokenIsRejected documents a security bug
// in Authenticator.Middleware reproduced by the user on 2026-05-21:
//
//	"I revoked my token, but revoking it still allowed me to download
//	 via the terminal with hf."
//
// Root cause: when a PAT is revoked, store.LookupAPIToken returns
// auth.ErrInvalidSession ("auth: invalid or expired session").
// Authenticator.Middleware (internal/auth/middleware.go) decides
// whether to reject the request with:
//
//	if err != nil && !strings.Contains(err.Error(), "invalid") {
//	    http.Error(w, "authentication failed", http.StatusUnauthorized)
//	}
//
// Because ErrInvalidSession contains the substring "invalid", the
// error is SWALLOWED.  The middleware then attaches a zero-value
// Actor to the request context and calls next.ServeHTTP -- exactly
// as if the client had not sent any credential at all.
//
// On any handler that doesn't itself require an authenticated actor
// (e.g. the pulsys data plane; see
// TestProxyDataPlaneRequiresValidPAT in internal/proxy), the revoked
// PAT therefore continues to work.
//
// This test asserts the *correct* behavior (401 before next runs)
// and will fail until the middleware stops treating
// ErrInvalidSession as an anonymous request.
func TestPATMiddleware_RevokedTokenIsRejected(t *testing.T) {
	a := &Authenticator{Store: &stubAuthStore{patErr: ErrInvalidSession}}

	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		actor := ActorFromContext(r.Context())
		t.Logf("next called with actor=%+v", actor)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/models/acme/widget", nil)
	req.Header.Set("Authorization", "Bearer pulsys_deadbeef_revokedtokenvalue")
	rec := httptest.NewRecorder()

	a.Middleware(next).ServeHTTP(rec, req)

	if nextCalled || rec.Code != http.StatusUnauthorized {
		t.Fatalf(
			"BUG: revoked PAT must be rejected with 401 before the next handler runs.\n"+
				"  got: status=%d next_called=%v\n"+
				"  want: status=401 next_called=false\n"+
				"Root cause: internal/auth/middleware.go (Authenticator.Middleware) "+
				"checks `strings.Contains(err.Error(), \"invalid\")` and swallows the "+
				"error, because auth.ErrInvalidSession's message is "+
				"%q.  The handler is then invoked anonymously and downstream "+
				"code (e.g. the pulsys data plane) serves the request.",
			rec.Code, nextCalled, ErrInvalidSession.Error(),
		)
	}
}

// TestPATMiddleware_AnonymousIsRejectedWhenAuthRequired documents that
// the current middleware contract treats "no bearer, no cookie" as an
// allowed anonymous request (next is called with a zero Actor).  That
// is the correct contract for an auth middleware that is meant to be
// *composed* with route-level RequireRole guards.  This test exists
// to make the contract explicit so a later refactor that flips the
// default to "deny" doesn't silently break the admin OIDC login
// endpoints, which depend on anonymous access.
//
// (The proxy data plane's separate "no PAT enforcement at all" bug
// is covered in internal/proxy.)
func TestPATMiddleware_AnonymousIsAllowedAndYieldsZeroActor(t *testing.T) {
	a := &Authenticator{Store: &stubAuthStore{}}

	var gotActor Actor
	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		gotActor = ActorFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	a.Middleware(next).ServeHTTP(rec, req)

	if !nextCalled {
		t.Fatal("anonymous request should reach next handler")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous request got status=%d, want 200 (RequireRole is the deny gate, not Middleware)", rec.Code)
	}
	if gotActor.Type != "" {
		t.Fatalf("anonymous request should yield zero Actor, got %+v", gotActor)
	}
}

// TestPATMiddleware_ValidPATPopulatesTokenActor exercises the happy
// path so the regression tests above are anchored against a working
// reference: when LookupAPIToken returns a valid token, the
// middleware should attach a Token-typed Actor with tenant + scopes
// to the request context.
func TestPATMiddleware_ValidPATPopulatesTokenActor(t *testing.T) {
	want := &APIToken{
		ID:       "tok-1",
		TenantID: "tenant-1",
		UserID:   "user-1",
		Prefix:   "deadbeef",
		Scopes:   []string{"admin:read"},
	}
	a := &Authenticator{Store: &stubAuthStore{patTok: want}}

	var got Actor
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = ActorFromContext(r.Context())
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer pulsys_deadbeef_validsecretvalue")
	a.Middleware(next).ServeHTTP(httptest.NewRecorder(), req)

	if got.Type != ActorToken {
		t.Fatalf("actor type = %q, want %q", got.Type, ActorToken)
	}
	if got.TenantID != want.TenantID || got.TokenID != want.ID || got.UserID != want.UserID {
		t.Fatalf("actor identity mismatch: got %+v want tenant=%s token=%s user=%s",
			got, want.TenantID, want.ID, want.UserID)
	}
	if len(got.Scopes) != 1 || got.Scopes[0] != "admin:read" {
		t.Fatalf("scopes = %v", got.Scopes)
	}
}

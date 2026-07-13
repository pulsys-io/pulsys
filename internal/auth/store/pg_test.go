// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/auth"
	"github.com/pulsys-io/pulsys/internal/auth/store"
	"github.com/pulsys-io/pulsys/internal/testpg"
)

func testPool(t *testing.T) (*store.PG, func()) {
	t.Helper()
	// Per-test isolated DB via the migrated template clone.
	// No cross-package races on the shared admin DSN.
	pool := testpg.Acquire(t)
	return store.NewPG(pool), func() {}
}

func TestEnsureTenantAndOIDCProvider(t *testing.T) {
	s, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()

	tid, err := s.EnsureTenant(ctx, "test-acme", "Acme Test")
	if err != nil {
		t.Fatal(err)
	}
	if tid == "" {
		t.Fatal("empty tenant id")
	}
	err = s.UpsertOIDCProvider(ctx, auth.OIDCProvider{
		TenantID:       tid,
		Issuer:         "https://issuer.example.com",
		ClientID:       "client",
		ClientSecret:   "secret",
		RedirectURI:    "http://localhost/auth/oidc/callback",
		Scopes:         "openid profile email",
		Enabled:        true,
		GroupsClaim:    "groups",
		OwnerGroups:    []string{"pulsys:owner"},
		AdminGroups:    []string{"pulsys:admin"},
		JITDefaultRole: auth.RoleMember,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetOIDCProviderByTenant(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientID != "client" {
		t.Fatalf("client_id %q", got.ClientID)
	}
}

func TestSessionRoundTrip(t *testing.T) {
	s, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()

	tid, err := s.EnsureTenant(ctx, "sess-tenant", "Session Tenant")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := s.CreateUserOIDC(ctx, auth.User{
		TenantID:    tid,
		Email:       "u@test.local",
		DisplayName: "U",
		Role:        auth.RoleMember,
		OIDCSub:     "sub-123",
	})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := s.CreateSession(ctx, uid, tid, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if sess.PlainToken == "" {
		t.Fatal("empty session token")
	}
	gotSess, user, err := s.LookupSession(ctx, sess.PlainToken)
	if err != nil {
		t.Fatal(err)
	}
	if user.ID != uid || gotSess.UserID != uid {
		t.Fatal("user mismatch")
	}
	if err := s.RevokeSession(ctx, sess.PlainToken); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LookupSession(ctx, sess.PlainToken); err == nil {
		t.Fatal("expected revoked session error")
	}
}

func TestFindUserByOIDCSubMissing(t *testing.T) {
	s, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()

	tid, _ := s.EnsureTenant(ctx, "find-user", "Find User")
	u, err := s.FindUserByOIDCSub(ctx, tid, "missing")
	if err != nil {
		t.Fatal(err)
	}
	if u != nil {
		t.Fatal("expected nil user")
	}
}

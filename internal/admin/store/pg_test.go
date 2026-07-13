// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/admin/store"
	"github.com/pulsys-io/pulsys/internal/auth"
	authstore "github.com/pulsys-io/pulsys/internal/auth/store"
	"github.com/pulsys-io/pulsys/internal/testpg"
)

func testAdminStore(t *testing.T) (*store.AdminStore, *authstore.PG, func()) {
	t.Helper()
	// Per-test isolated DB via the migrated template clone.
	// No cross-package races on the shared admin DSN.
	pool := testpg.Acquire(t)
	return store.NewAdminStore(pool), authstore.NewPG(pool), func() {}
}

func TestAdminTokensSettingsAudit(t *testing.T) {
	adminSt, authSt, cleanup := testAdminStore(t)
	defer cleanup()
	ctx := context.Background()

	tid, err := authSt.EnsureTenant(ctx, "admin-api-test", "Admin API Test")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := authSt.CreateUserOIDC(ctx, auth.User{
		TenantID:    tid,
		Email:       "admin@test.local",
		DisplayName: "Admin",
		Role:        auth.RoleAdmin,
		OIDCSub:     "admin-sub",
	})
	if err != nil {
		t.Fatal(err)
	}

	display, prefix, hash, err := auth.GeneratePAT()
	if err != nil {
		t.Fatal(err)
	}
	created, err := adminSt.CreateToken(ctx, tid, uid, "ci-token", prefix, hash, []string{"admin:read"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" {
		t.Fatal("empty token id")
	}

	tokens, err := adminSt.ListTokens(ctx, tid, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 || tokens[0].Prefix != prefix {
		t.Fatalf("tokens %+v", tokens)
	}

	got, err := authSt.LookupAPIToken(ctx, display)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != created.ID {
		t.Fatalf("lookup id %q want %q", got.ID, created.ID)
	}

	val := json.RawMessage(`{"enabled":true}`)
	st, err := adminSt.UpsertSetting(ctx, tid, "auth", "session_ttl", val, 0, uid)
	if err != nil {
		t.Fatal(err)
	}
	if st.Version < 1 {
		t.Fatal("version")
	}
	settings, err := adminSt.ListSettings(ctx, tid, "auth")
	if err != nil {
		t.Fatal(err)
	}
	if len(settings) != 1 {
		t.Fatalf("settings len %d", len(settings))
	}

	actorID := uid
	if err := adminSt.InsertAudit(ctx, tid, "user", &actorID, "token.create", "tokens/"+created.ID, "success", nil, "127.0.0.1", "test"); err != nil {
		t.Fatal(err)
	}
	audit, err := adminSt.ListAudit(ctx, tid, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 1 || audit[0].Action != "token.create" {
		t.Fatalf("audit %+v", audit)
	}

	if _, _, err := adminSt.RevokeToken(ctx, tid, created.ID); err != nil {
		t.Fatal(err)
	}
	_, p2, h2, err := auth.GeneratePAT()
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour)
	_, err = adminSt.CreateToken(ctx, tid, uid, "expiring", p2, h2, []string{"admin:read"}, &exp)
	if err != nil {
		t.Fatal(err)
	}
}

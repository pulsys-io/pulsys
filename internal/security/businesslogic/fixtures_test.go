// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package businesslogic

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/admin/api"
	adminstore "github.com/pulsys-io/pulsys/internal/admin/store"
	"github.com/pulsys-io/pulsys/internal/auth"
	authstore "github.com/pulsys-io/pulsys/internal/auth/store"
	"github.com/pulsys-io/pulsys/internal/testpg"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fixtures stands up an isolated Postgres + a real admin API
// handler mounted on a serve mux.  The handler is identical to
// the production wiring except for two seams:
//
//   - PATCache is a fake CountingInvalidator the tests can
//     assert against without spinning up the data plane.
//   - AuditInsert (when non-nil) shunts audit rows to an
//     in-memory recorder; tests that want to assert audit
//     emission set it, tests that don't leave it on the real
//     store so the audit row hits Postgres.
//
// The fixtures live in their own _test.go file so the test
// helpers are not exported into the production package surface.
type fixtures struct {
	t           *testing.T
	Pool        *pgxpool.Pool
	AuthStore   *authstore.PG
	AdminStore  *adminstore.AdminStore
	TenantID    string
	AdminUserID string
	OwnerUserID string
	PATCache    *countingInvalidator
	Handler     *api.Handler
	Mux         *http.ServeMux
}

type auditRow struct {
	action, resource, outcome string
}

// countingInvalidator records every InvalidateByHash call.
// PATGate's contract is "called with the hash of the revoked
// token"; tests assert both the count and the specific hash to
// catch off-by-one and wrong-token bugs.
//
// Safe for concurrent calls because the production PATGate
// itself is called from many request goroutines; a test stub
// without a mutex would race on its own counters under the
// concurrent-retry test.
type countingInvalidator struct {
	mu     sync.Mutex
	Calls  int
	Hashes [][]byte
}

func (c *countingInvalidator) InvalidateByHash(hash []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Calls++
	cp := make([]byte, len(hash))
	copy(cp, hash)
	c.Hashes = append(c.Hashes, cp)
}

// newFixtures builds a ready-to-use admin handler against an
// isolated Postgres.  The handler is mounted via Handler.Mount
// at /admin/api/v1/* so URLs in tests match production exactly.
func newFixtures(t *testing.T) *fixtures {
	t.Helper()
	pool := testpg.Acquire(t)
	pgAuth := authstore.NewPG(pool)
	pgAdmin := adminstore.NewAdminStore(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantID, err := pgAuth.EnsureTenant(ctx, "businesslogic", "BUSL Test Tenant")
	if err != nil {
		t.Fatalf("ensure tenant: %v", err)
	}

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
	adminUID := makeUser("sub-busl-admin", "admin@busl.local", auth.RoleAdmin)
	ownerUID := makeUser("sub-busl-owner", "owner@busl.local", auth.RoleOwner)

	cache := &countingInvalidator{}
	h := &api.Handler{
		Store:    pgAdmin,
		PATCache: cache,
	}
	mux := http.NewServeMux()
	h.Mount(mux)

	return &fixtures{
		t:           t,
		Pool:        pool,
		AuthStore:   pgAuth,
		AdminStore:  pgAdmin,
		TenantID:    tenantID,
		AdminUserID: adminUID,
		OwnerUserID: ownerUID,
		PATCache:    cache,
		Handler:     h,
		Mux:         mux,
	}
}

// adminCtx returns a context bearing an admin-role Actor so
// requireAccess(RoleAdmin, ...) lets the request through.
// The actor's TenantID is the seeded tenant so every store
// query selects the right row set.
func (f *fixtures) adminCtx() context.Context {
	return auth.ContextWithActor(context.Background(), auth.Actor{
		Type:     auth.ActorUser,
		TenantID: f.TenantID,
		UserID:   f.AdminUserID,
		Role:     auth.RoleAdmin,
		Scopes:   []string{"admin:*"},
	})
}

// memberCtx returns a member-role actor for "not enough
// privilege" coverage in some sub-tests.
func (f *fixtures) memberCtx() context.Context {
	return auth.ContextWithActor(context.Background(), auth.Actor{
		Type:     auth.ActorUser,
		TenantID: f.TenantID,
		UserID:   f.AdminUserID,
		Role:     auth.RoleMember,
		Scopes:   []string{"admin:read"},
	})
}

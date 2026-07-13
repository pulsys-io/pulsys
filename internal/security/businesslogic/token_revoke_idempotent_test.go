// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package businesslogic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/admin/api"
	"github.com/pulsys-io/pulsys/internal/admin/audit"
	adminstore "github.com/pulsys-io/pulsys/internal/admin/store"
	"github.com/pulsys-io/pulsys/internal/auth"
	authstore "github.com/pulsys-io/pulsys/internal/auth/store"
	"github.com/pulsys-io/pulsys/internal/testpg"
)

// recordingAudit captures every InsertAudit call so the test
// can assert the revoke-retry path emits exactly one row.
type recordingAudit struct {
	mu   sync.Mutex
	rows []auditRow
}

func (r *recordingAudit) InsertAudit(_ context.Context, _, _ string, _ *string,
	action, resource, outcome string, _ json.RawMessage, _, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, auditRow{action: action, resource: resource, outcome: outcome})
	return nil
}

// fakeTenantResolver lets the audit middleware resolve a tenant
// id without hitting Postgres.  We pass the same id our actor
// already carries so the resolver path is exercised but the
// answer is deterministic.
type fakeTenantResolver struct{ id string }

func (f fakeTenantResolver) GetTenantIDByName(_ context.Context, _ string) (string, error) {
	return f.id, nil
}

// revokeIdempotentSetup builds the full handler chain used by
// the idempotent-revoke tests below.  Each test gets its own
// isolated Postgres + audit recorder + cache counter so they
// can run in parallel under -race.
type revokeIdempotentSetup struct {
	t        *testing.T
	tenantID string
	adminUID string
	ownerUID string
	cache    *countingInvalidator
	audit    *recordingAudit
	store    *adminstore.AdminStore
	handler  http.Handler
}

func newRevokeIdempotentSetup(t *testing.T, tenantName string) *revokeIdempotentSetup {
	t.Helper()
	pool := testpg.Acquire(t)
	pgAuth := authstore.NewPG(pool)
	pgAdmin := adminstore.NewAdminStore(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tenantID, err := pgAuth.EnsureTenant(ctx, tenantName, "BUSL idempotent")
	if err != nil {
		t.Fatalf("ensure tenant: %v", err)
	}
	adminUID, err := pgAuth.CreateUserOIDC(ctx, auth.User{
		TenantID: tenantID, Email: "a@" + tenantName + ".local", DisplayName: "A",
		Role: auth.RoleAdmin, OIDCSub: "sub-" + tenantName + "-admin", IsActive: true,
	})
	if err != nil {
		t.Fatalf("create admin user: %v", err)
	}
	ownerUID, err := pgAuth.CreateUserOIDC(ctx, auth.User{
		TenantID: tenantID, Email: "o@" + tenantName + ".local", DisplayName: "O",
		Role: auth.RoleOwner, OIDCSub: "sub-" + tenantName + "-owner", IsActive: true,
	})
	if err != nil {
		t.Fatalf("create owner user: %v", err)
	}

	recAudit := &recordingAudit{}
	cache := &countingInvalidator{}
	h := &api.Handler{Store: pgAdmin, PATCache: cache}
	adminMux := http.NewServeMux()
	h.Mount(adminMux)
	wrapped := (&audit.Middleware{
		Store:        recAudit,
		TenantName:   tenantName,
		TenantLookup: fakeTenantResolver{id: tenantID},
	}).Wrap(adminMux)

	return &revokeIdempotentSetup{
		t:        t,
		tenantID: tenantID,
		adminUID: adminUID,
		ownerUID: ownerUID,
		cache:    cache,
		audit:    recAudit,
		store:    pgAdmin,
		handler:  wrapped,
	}
}

func (s *revokeIdempotentSetup) actorCtx() context.Context {
	return auth.ContextWithActor(context.Background(), auth.Actor{
		Type:     auth.ActorUser,
		TenantID: s.tenantID,
		UserID:   s.adminUID,
		Role:     auth.RoleAdmin,
		Scopes:   []string{"admin:*"},
	})
}

func (s *revokeIdempotentSetup) mintToken(name string) *adminstore.TokenCreateResult {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, prefix, hash, err := auth.GeneratePAT()
	if err != nil {
		s.t.Fatalf("generate pat: %v", err)
	}
	tok, err := s.store.CreateToken(ctx, s.tenantID, s.ownerUID, name,
		prefix, hash, []string{"models:read"}, nil)
	if err != nil {
		s.t.Fatalf("create token: %v", err)
	}
	return tok
}

func (s *revokeIdempotentSetup) doRevoke(id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodDelete,
		"/admin/api/v1/tokens/"+id, nil).WithContext(s.actorCtx())
	w := httptest.NewRecorder()
	s.handler.ServeHTTP(w, req)
	return w
}

// TestPATRevoke_DoubleRevokeIdempotent pins the Phase 5
// idempotency contract: a CI pipeline that retries an
// interrupted DELETE must see 204 + zero additional audit
// rows on the replay.
//
//  1. First DELETE -> 204 + audit row (outcome=success).
//  2. Second DELETE (same id) -> 204, X-Pulsys-Idempotent-Replay
//     consumed by the middleware (never reaches client),
//     NO additional audit row.
//  3. PATCache.InvalidateByHash called on BOTH requests (the
//     replay still pokes the local cache in case it was
//     repopulated between the two requests).
func TestPATRevoke_DoubleRevokeIdempotent(t *testing.T) {
	s := newRevokeIdempotentSetup(t, "revoke-idempotent")
	tok := s.mintToken("to-revoke")

	first := s.doRevoke(tok.ID)
	if first.Code != http.StatusNoContent {
		t.Fatalf("first revoke: status=%d body=%s", first.Code, first.Body.String())
	}
	if hv := first.Header().Get("X-Pulsys-Idempotent-Replay"); hv != "" {
		t.Fatalf("first revoke leaked X-Pulsys-Idempotent-Replay=%q to client", hv)
	}

	second := s.doRevoke(tok.ID)
	if second.Code != http.StatusNoContent {
		t.Fatalf("second revoke: status=%d body=%s", second.Code, second.Body.String())
	}
	if hv := second.Header().Get("X-Pulsys-Idempotent-Replay"); hv != "" {
		t.Fatalf("second revoke leaked X-Pulsys-Idempotent-Replay=%q to client", hv)
	}

	s.audit.mu.Lock()
	defer s.audit.mu.Unlock()
	if len(s.audit.rows) != 1 {
		t.Fatalf("audit rows=%+v want exactly one token.revoke row", s.audit.rows)
	}
	if s.audit.rows[0].action != "token.revoke" || s.audit.rows[0].outcome != "success" {
		t.Fatalf("audit row %+v want action=token.revoke outcome=success", s.audit.rows[0])
	}
	if s.cache.Calls != 2 {
		t.Fatalf("PATCache.Calls=%d want 2 (one per request)", s.cache.Calls)
	}
}

// TestPATRevoke_ConcurrentRetriesAreSafe is the property-test
// shape of the previous test: N concurrent revoke retries
// against the same token id all return 204, no goroutine
// observes a 500, and exactly one audit row is appended.
//
// Under -race this would have caught a future regression where
// the idempotent branch raced with the audit middleware (e.g.
// dropping the replay header inconsistently under concurrent
// flushes).
func TestPATRevoke_ConcurrentRetriesAreSafe(t *testing.T) {
	s := newRevokeIdempotentSetup(t, "revoke-concurrent")
	tok := s.mintToken("ctok")

	const N = 8
	var ok, bad atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := s.doRevoke(tok.ID)
			if w.Code == http.StatusNoContent {
				ok.Add(1)
			} else {
				bad.Add(1)
				t.Errorf("concurrent revoke: status=%d body=%s", w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("non-204 responses=%d", bad.Load())
	}
	if ok.Load() != N {
		t.Fatalf("204 responses=%d want %d", ok.Load(), N)
	}
	s.audit.mu.Lock()
	defer s.audit.mu.Unlock()
	if len(s.audit.rows) != 1 {
		t.Fatalf("audit rows=%d want 1; rows=%+v", len(s.audit.rows), s.audit.rows)
	}
}

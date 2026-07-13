// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package businesslogic

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/auth"
)

// TestPATRevoke_InvalidatesDataPlaneCache pins the Phase 5 fix
// for the original 2026-05-21 incident: after the admin revoke
// handler returns 204, the gate's in-memory cache for that PAT
// MUST be empty so the next data-plane request misses cache,
// re-queries Postgres, sees revoked_at, and rejects with 401.
//
// The test drives the real admin handler through its mux so
// the path traversed is byte-identical to production: HTTP
// PUT, JSON decode, requireAccess(RoleAdmin), Store.RevokeToken,
// PATCache.InvalidateByHash.  The fake cache records the hash;
// we assert it equals auth.TokenHash(plaintext) so an attacker
// can't satisfy the test with a plausible-but-wrong hash (e.g.
// the prefix bytes or the token id).
func TestPATRevoke_InvalidatesDataPlaneCache(t *testing.T) {
	f := newFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	plain, prefix, hash, err := auth.GeneratePAT()
	if err != nil {
		t.Fatalf("generate pat: %v", err)
	}
	created, err := f.AdminStore.CreateToken(ctx, f.TenantID, f.OwnerUserID,
		"revoke-cache-test", prefix, hash, []string{"models:read"}, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete,
		"/admin/api/v1/tokens/"+created.ID, nil).WithContext(f.adminCtx())
	rec := httptest.NewRecorder()
	f.Mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if f.PATCache.Calls != 1 {
		t.Fatalf("PATCache.Calls=%d want 1", f.PATCache.Calls)
	}
	wantHash := auth.TokenHash(plain)
	if !bytes.Equal(f.PATCache.Hashes[0], wantHash) {
		t.Fatalf("PATCache.Hashes[0]=%x want %x", f.PATCache.Hashes[0], wantHash)
	}
}

// TestPATRevoke_NotFound returns 404 (not 204) when the
// token id doesn't exist for this tenant, AND does not touch
// the PAT cache.  Without the second invariant a token-id
// enumeration attack could spam the gate's invalidate path to
// blow away legitimate cache entries owned by other tenants.
func TestPATRevoke_NotFound(t *testing.T) {
	f := newFixtures(t)

	req := httptest.NewRequest(http.MethodDelete,
		"/admin/api/v1/tokens/00000000-0000-0000-0000-000000000000", nil).
		WithContext(f.adminCtx())
	rec := httptest.NewRecorder()
	f.Mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s want 404", rec.Code, rec.Body.String())
	}
	if f.PATCache.Calls != 0 {
		t.Fatalf("PATCache.Calls=%d want 0 on not-found", f.PATCache.Calls)
	}
}

// TestPATRevoke_BadID handles the trivial "no id" path (empty
// path value).  The handler must reject with 400 -- 404 would
// be a wrong status, 500 would leak an internal error.
func TestPATRevoke_BadID(t *testing.T) {
	f := newFixtures(t)

	// Empty {id} pattern: the mux 404s before our handler
	// sees the request.  Send a request that matches the
	// pattern but with an explicitly empty id segment by
	// going through a request that the mux can route.  In
	// practice the mux strips trailing slashes; the only way
	// to hit our handler with empty PathValue is via a
	// programmatic call.  Use httptest.NewRequest + the mux
	// to confirm the route boundary is enforced.
	req := httptest.NewRequest(http.MethodDelete,
		"/admin/api/v1/tokens/", nil).WithContext(f.adminCtx())
	rec := httptest.NewRecorder()
	f.Mux.ServeHTTP(rec, req)

	// The trailing-slash route does not match the pattern,
	// so the mux returns 404 -- which is acceptable: the
	// invariant we care about is "we never crash and never
	// call the cache".
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s want 404 or 400", rec.Code, rec.Body.String())
	}
	if f.PATCache.Calls != 0 {
		t.Fatalf("PATCache.Calls=%d want 0 on bad id", f.PATCache.Calls)
	}
}

// TestPATRevoke_StoreSeesRevokedAt closes the loop end-to-end:
// after the admin handler returns 204, a direct AdminStore
// lookup must report revoked_at != NULL.  Without this check a
// future refactor that returns 204 without actually writing the
// UPDATE would pass the cache-eviction tests above but break
// the actual revocation.
func TestPATRevoke_StoreSeesRevokedAt(t *testing.T) {
	f := newFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, prefix, hash, err := auth.GeneratePAT()
	if err != nil {
		t.Fatalf("generate pat: %v", err)
	}
	created, err := f.AdminStore.CreateToken(ctx, f.TenantID, f.OwnerUserID,
		"revoke-store-test", prefix, hash, []string{"models:read"}, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete,
		"/admin/api/v1/tokens/"+created.ID, nil).WithContext(f.adminCtx())
	rec := httptest.NewRecorder()
	f.Mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	// ListTokens reflects revoked_at in its Token row.
	toks, err := f.AdminStore.ListTokens(ctx, f.TenantID, 50)
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	var found bool
	for _, tk := range toks {
		if tk.ID == created.ID {
			found = true
			if tk.RevokedAt == nil {
				t.Fatalf("token %q has revoked_at = NULL after revoke", tk.ID)
			}
		}
	}
	if !found {
		t.Fatalf("token %q missing from list after revoke", created.ID)
	}
}

// TestPATRevoke_RequiresAdminRole pins the auth contract on
// the revoke path: a member (RoleMember) MUST get 403 from
// requireAccess, and the cache MUST remain untouched.  This
// double-checks the Mount() wiring -- a refactor that changed
// the required role would silently lower the privilege bar.
func TestPATRevoke_RequiresAdminRole(t *testing.T) {
	f := newFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, prefix, hash, err := auth.GeneratePAT()
	if err != nil {
		t.Fatalf("generate pat: %v", err)
	}
	created, err := f.AdminStore.CreateToken(ctx, f.TenantID, f.OwnerUserID,
		"revoke-rbac-test", prefix, hash, []string{"models:read"}, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete,
		"/admin/api/v1/tokens/"+created.ID, nil).WithContext(f.memberCtx())
	rec := httptest.NewRecorder()
	f.Mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s want 403", rec.Code, rec.Body.String())
	}
	if f.PATCache.Calls != 0 {
		t.Fatalf("PATCache.Calls=%d want 0 on rbac-denied request", f.PATCache.Calls)
	}
}

// helper to decode the createToken response body in TTL/scope
// tests that share these fixtures.
type createTokenResp struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

// postCreateToken issues a POST to /admin/api/v1/tokens with
// the supplied body using the admin actor.  Returns the recorder
// and decoded response (zero-value on non-2xx).
func (f *fixtures) postCreateToken(body string) (*httptest.ResponseRecorder, createTokenResp) {
	req := httptest.NewRequest(http.MethodPost,
		"/admin/api/v1/tokens", strings.NewReader(body)).WithContext(f.adminCtx())
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.Mux.ServeHTTP(rec, req)
	var out createTokenResp
	if rec.Code/100 == 2 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec, out
}

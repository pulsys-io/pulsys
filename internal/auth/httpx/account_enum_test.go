// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Account / tenant enumeration regression test (OWASP WSTG-IDNT-04).
//
// PURPOSE
//   /auth/session is the only public POST that touches the auth
//   store.  An unauthenticated probe MUST NOT be able to use it
//   as an oracle to:
//
//     (a) enumerate which tenant slugs exist on this deployment, OR
//     (b) enumerate which email/sub claims are pre-provisioned in a
//         tenant that has RequirePreprovisioned = true.
//
//   Previously the handler echoed `err.Error()` for tenant
//   resolution failures (giving `"store: tenant 'X' not found"`)
//   AND distinguished `auth.ErrLoginDenied` with a 403 + "login
//   denied" body (revealing whether the IdP-issued user existed
//   in this tenant's allow-list).  Both were enumeration oracles.
//
//   The fix collapses every credential-style failure into the
//   same response shape:
//
//       HTTP/1.1 401 Unauthorized
//       Content-Type: text/plain; charset=utf-8
//       <body>session establishment failed
//
//   /auth/oidc/config received the parallel fix: any tenant
//   resolution failure returns the same 503 body that an
//   unconfigured (but existing) tenant already returned.
//
// THIS TEST asserts the response body, status, AND Content-Type
// are byte-identical across the three failure shapes:
//
//   1. Tenant override pointing at a slug that DOES NOT EXIST
//   2. Tenant override pointing at a slug that EXISTS but has
//      NO OIDC provider configured
//   3. Tenant override pointing at a slug that EXISTS with a
//      provider, but the supplied id_token is gibberish so
//      verification will fail
//
//   Cases 1, 2, and 3 are the three branches an external prober
//   could distinguish.  After the fix all three MUST be
//   indistinguishable from the wire.

package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/auth"
	"github.com/pulsys-io/pulsys/internal/auth/oidc"
	authstore "github.com/pulsys-io/pulsys/internal/auth/store"
	"github.com/pulsys-io/pulsys/internal/testpg"
)

// newEnumFixture builds a Handler bound to a real Postgres pool
// with two seeded tenants: "configured" (has a sham OIDC provider
// row so resolveTenant succeeds) and "barren" (no OIDC provider).
// A third probe uses a tenant slug that simply does not exist.
type enumFixture struct {
	h               *Handler
	configuredSlug  string
	barrenSlug      string
	nonexistentSlug string
}

func newEnumFixture(t *testing.T) enumFixture {
	t.Helper()
	pool := testpg.Acquire(t)
	st := authstore.NewPG(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	confID, err := st.EnsureTenant(ctx, "enum-configured", "Enum Configured")
	if err != nil {
		t.Fatalf("ensure tenant configured: %v", err)
	}
	barID, err := st.EnsureTenant(ctx, "enum-barren", "Enum Barren")
	if err != nil {
		t.Fatalf("ensure tenant barren: %v", err)
	}
	// Insert a sham OIDC provider for "configured" so EstablishSession
	// reaches the verifier step (which will fail because the issuer
	// URL doesn't resolve in the test).  We never need the verifier
	// to succeed -- we only care that the FAILURE response shape is
	// identical to the other two cases.
	if err := st.UpsertOIDCProvider(ctx, auth.OIDCProvider{
		TenantID:       confID,
		Issuer:         "https://example.invalid",
		ClientID:       "cid",
		ClientSecret:   "secret",
		RedirectURI:    "https://app.example/callback",
		Scopes:         "openid email profile",
		Enabled:        true,
		GroupsClaim:    "groups",
		OwnerGroups:    []string{},
		AdminGroups:    []string{},
		JITDefaultRole: auth.RoleMember,
	}); err != nil {
		t.Fatalf("upsert oidc provider: %v", err)
	}
	_ = barID // barren intentionally has no provider row

	return enumFixture{
		h: &Handler{
			OIDC:       &oidc.Service{Store: st, SessionTTL: time.Hour, Now: time.Now},
			TenantName: "default",
		},
		configuredSlug:  "enum-configured",
		barrenSlug:      "enum-barren",
		nonexistentSlug: "does-not-exist-anywhere",
	}
}

// captureResponse runs a request through h.establishSession and
// returns the byte-for-byte response surface we want to compare
// across cases: status code, Content-Type header, and body bytes.
type capturedResponse struct {
	status      int
	contentType string
	body        []byte
}

func (c capturedResponse) equal(o capturedResponse) (bool, string) {
	if c.status != o.status {
		return false, "status differs"
	}
	if c.contentType != o.contentType {
		return false, "Content-Type differs"
	}
	if !bytes.Equal(c.body, o.body) {
		return false, "body differs"
	}
	return true, ""
}

func (c capturedResponse) String() string {
	return "status=" + http.StatusText(c.status) + " ct=" + c.contentType + " body=" + strings.TrimSpace(string(c.body))
}

func captureSession(t *testing.T, h *Handler, tenant string) capturedResponse {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{
		"id_token": "not.a.real.jwt.so.verification.fails",
		"tenant":   tenant,
	})
	req := httptest.NewRequest(http.MethodPost, "/auth/session", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.establishSession(rec, req)
	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	return capturedResponse{
		status:      res.StatusCode,
		contentType: res.Header.Get("Content-Type"),
		body:        body,
	}
}

func captureOIDCConfig(t *testing.T, h *Handler, tenant string) capturedResponse {
	t.Helper()
	url := "/auth/oidc/config?tenant_id=" + tenant
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	h.oidcConfig(rec, req)
	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	return capturedResponse{
		status:      res.StatusCode,
		contentType: res.Header.Get("Content-Type"),
		body:        body,
	}
}

// TestSession_EnumerationOracle_Indistinguishable asserts the
// three failure branches in /auth/session produce identical
// response surfaces.
func TestSession_EnumerationOracle_Indistinguishable(t *testing.T) {
	f := newEnumFixture(t)

	// Case 1: tenant slug does NOT exist at all.
	nonexist := captureSession(t, f.h, f.nonexistentSlug)
	// Case 2: tenant exists but has NO OIDC provider configured.
	//         resolveTenant succeeds, EstablishSession fails inside
	//         GetOIDCProviderByTenant -> ErrNoOIDCProvider.
	barren := captureSession(t, f.h, f.barrenSlug)
	// Case 3: tenant exists with an OIDC provider, but the
	//         id_token we sent is gibberish so verification fails.
	confBadToken := captureSession(t, f.h, f.configuredSlug)

	t.Logf("nonexistent  -> %s", nonexist)
	t.Logf("barren       -> %s", barren)
	t.Logf("conf+badtok  -> %s", confBadToken)

	// All three MUST collapse to 401 with the same body.
	if nonexist.status != http.StatusUnauthorized {
		t.Errorf("WSTG-IDNT-04: nonexistent tenant returned %d (expected 401); attacker can enumerate tenant slugs", nonexist.status)
	}
	if ok, why := nonexist.equal(barren); !ok {
		t.Errorf("WSTG-IDNT-04: nonexistent vs barren tenant responses %s\n  nonexistent: %s\n  barren:      %s",
			why, nonexist, barren)
	}
	if ok, why := barren.equal(confBadToken); !ok {
		t.Errorf("WSTG-IDNT-04: barren vs configured+badtoken responses %s\n  barren:       %s\n  conf+badtok: %s",
			why, barren, confBadToken)
	}
}

// TestSession_BodyMatchesIdTokenRequired_PathStaysDistinct
// asserts that the "missing id_token" 400 branch is INTENTIONALLY
// distinct from the 401 enumeration branch.  Validation feedback
// on the request shape itself is fine -- the SPA needs to know
// the body was malformed.  Only credential-style failures must be
// collapsed.
func TestSession_BodyMatchesIdTokenRequired_PathStaysDistinct(t *testing.T) {
	f := newEnumFixture(t)
	// Empty body, no id_token field at all.
	req := httptest.NewRequest(http.MethodPost, "/auth/session", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.h.establishSession(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing id_token should be 400 (validation), got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "id_token") {
		t.Errorf("expected id_token validation message, got %q", rec.Body.String())
	}
}

// TestOIDCConfig_EnumerationOracle_Indistinguishable asserts the
// parallel invariant for the discovery endpoint.
func TestOIDCConfig_EnumerationOracle_Indistinguishable(t *testing.T) {
	f := newEnumFixture(t)

	// /auth/oidc/config uses the tenant_id query param verbatim
	// when supplied (no DB lookup of the slug).  To exercise the
	// "tenant slug not found" path through GetTenantIDByName, we
	// must omit tenant_id and rely on the fallback that consults
	// TenantName.  Set TenantName to a nonexistent slug for one
	// probe and to the real slug for the other; the responses
	// MUST be identical.
	hNon := *f.h
	hNon.TenantName = f.nonexistentSlug
	hBar := *f.h
	hBar.TenantName = f.barrenSlug

	nonexist := captureOIDCConfig(t, &hNon, "")
	barren := captureOIDCConfig(t, &hBar, "")

	t.Logf("nonexistent default tenant -> %s", nonexist)
	t.Logf("barren default tenant       -> %s", barren)

	if nonexist.status != http.StatusServiceUnavailable {
		t.Errorf("WSTG-IDNT-04: nonexistent tenant returned %d (expected 503); reveals tenant existence", nonexist.status)
	}
	if ok, why := nonexist.equal(barren); !ok {
		t.Errorf("WSTG-IDNT-04: /auth/oidc/config nonexistent vs barren tenant responses %s\n  nonexistent: %s\n  barren:      %s",
			why, nonexist, barren)
	}
}

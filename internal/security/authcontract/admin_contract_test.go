// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package authcontract

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pulsys-io/pulsys/internal/admin"
	"github.com/pulsys-io/pulsys/internal/auth"
	"github.com/pulsys-io/pulsys/internal/observability"
)

// adminEndpoints returns the full auth contract for everything the
// admin HTTP surface exposes -- /auth/*, /admin/api/v1/*, /metrics,
// /healthz.  Adding an endpoint without extending this table trips
// TestAdminContract_Completeness (see completeness_test.go), so the
// table is the source of truth.
//
// Outcomes are derived from the production policy:
//
//   - /auth/oidc/config, /auth/session, /auth/logout, /healthz,
//     /metrics are public.  Any caller reaches the handler.  Where
//     the handler then needs valid input, the test sends a body that
//     deliberately fails validation -- we assert only that the
//     response is NOT 401 / 403, so body-validation errors are fine.
//
//   - /auth/csrf needs a live session cookie.  Anonymous and any
//     credential without a session cookie get 401.
//
//   - /admin/api/v1/* sits behind authn.Middleware +
//     requireAuthenticated + requireAccess(minRole, scope).  The
//     contract below mirrors handler.go's Mount table verbatim.
//
// The matrix runner asserts every (endpoint, credential) cell.
func adminEndpoints(f *fixtures) []Endpoint {
	tokenJSON := []byte(`{"name":"matrix-token","scopes":["models:read"]}`)
	importJSON := []byte(`{"repo_id":"Qwen/Qwen2.5-0.5B","revision":"main"}`)
	purgeJSON := []byte(`{"path_prefix":"/matrix/"}`)
	settingJSON := []byte(`{"value":{"v":1}}`)

	return []Endpoint{
		// ----- public surface (no auth) -----
		{
			Method:   "GET",
			Path:     "/healthz",
			Note:     "liveness probe; public by design",
			Outcomes: publicAlwaysAdmits(),
		},
		{
			Method:   "GET",
			Path:     "/metrics",
			Note:     "Prometheus scrape; public by design (scrape from inside the trust zone)",
			Outcomes: publicAlwaysAdmits(),
		},

		// ----- /auth/* (public + session-only) -----
		{
			Method: "GET",
			Path:   "/auth/oidc/config",
			Note:   "IdP metadata; public so the SPA can bootstrap the login flow",
			// No OIDC provider is configured in the matrix fixtures,
			// so the handler returns 503 for admitted callers.  503
			// is NOT 401/403, so Admitted still holds.
			Outcomes: publicReachableButMiddlewareGated(),
		},
		{
			Method:      "POST",
			Path:        "/auth/session",
			Body:        []byte(`{}`),
			ContentType: "application/json",
			Note:        "session establishment; CSRF-exempt; public",
			// Body has no id_token so the handler returns 400.
			// 400 is NOT 401/403 -> Admitted.
			Outcomes: publicReachableButMiddlewareGated(),
		},
		{
			Method: "POST",
			Path:   "/auth/logout",
			Note:   "logout; idempotent regardless of credentials",
			// Logout always returns 204 for admitted callers.
			Outcomes: publicReachableButMiddlewareGated(),
		},
		{
			Method:   "GET",
			Path:     "/auth/csrf",
			Note:     "double-submit CSRF token; needs a live session",
			Outcomes: csrfEndpoint(),
		},

		// ----- /admin/api/v1/* (requireAccess) -----
		{
			Method:   "GET",
			Path:     "/admin/api/v1/tenant",
			Note:     "reader+ or admin:read",
			Outcomes: protectedAccess(auth.RoleReader, "admin:read"),
		},
		{
			Method:   "GET",
			Path:     "/admin/api/v1/users",
			Note:     "admin+ or admin:write",
			Outcomes: protectedAccess(auth.RoleAdmin, "admin:write"),
		},
		{
			Method:   "GET",
			Path:     "/admin/api/v1/tokens",
			Note:     "member+ or admin:read",
			Outcomes: protectedAccess(auth.RoleMember, "admin:read"),
		},
		{
			Method:      "POST",
			Path:        "/admin/api/v1/tokens",
			Body:        tokenJSON,
			ContentType: "application/json",
			Note:        "admin+ or admin:write",
			Outcomes:    protectedAccess(auth.RoleAdmin, "admin:write"),
		},
		{
			Method:   "DELETE",
			Path:     "/admin/api/v1/tokens/00000000-0000-0000-0000-000000000000",
			Note:     "admin+ or admin:write",
			Outcomes: protectedAccess(auth.RoleAdmin, "admin:write"),
		},
		{
			Method:   "GET",
			Path:     "/admin/api/v1/settings",
			Note:     "reader+ or admin:read",
			Outcomes: protectedAccess(auth.RoleReader, "admin:read"),
		},
		{
			Method:      "PUT",
			Path:        "/admin/api/v1/settings/tenant/matrix",
			Body:        settingJSON,
			ContentType: "application/json",
			Note:        "admin+ or admin:write",
			Outcomes:    protectedAccess(auth.RoleAdmin, "admin:write"),
		},
		{
			Method:   "GET",
			Path:     "/admin/api/v1/audit",
			Note:     "reader+ or admin:read",
			Outcomes: protectedAccess(auth.RoleReader, "admin:read"),
		},
		{
			Method:   "GET",
			Path:     "/admin/api/v1/imports",
			Note:     "reader+ or admin:read",
			Outcomes: protectedAccess(auth.RoleReader, "admin:read"),
		},
		{
			Method:      "POST",
			Path:        "/admin/api/v1/imports",
			Body:        importJSON,
			ContentType: "application/json",
			Note:        "admin+ or admin:write",
			Outcomes:    protectedAccess(auth.RoleAdmin, "admin:write"),
		},
		{
			Method:   "GET",
			Path:     "/admin/api/v1/imports/00000000-0000-0000-0000-000000000000",
			Note:     "reader+ or admin:read",
			Outcomes: protectedAccess(auth.RoleReader, "admin:read"),
		},
		{
			Method:   "POST",
			Path:     "/admin/api/v1/imports/00000000-0000-0000-0000-000000000000/cancel",
			Note:     "admin+ or admin:write",
			Outcomes: protectedAccess(auth.RoleAdmin, "admin:write"),
		},
		{
			Method:   "POST",
			Path:     "/admin/api/v1/imports/00000000-0000-0000-0000-000000000000/force-cancel",
			Note:     "admin+ or admin:write",
			Outcomes: protectedAccess(auth.RoleAdmin, "admin:write"),
		},
		{
			Method:   "DELETE",
			Path:     "/admin/api/v1/imports/00000000-0000-0000-0000-000000000000",
			Note:     "admin+ or admin:write",
			Outcomes: protectedAccess(auth.RoleAdmin, "admin:write"),
		},
		{
			Method:   "GET",
			Path:     "/admin/api/v1/models",
			Note:     "reader+ or admin:read",
			Outcomes: protectedAccess(auth.RoleReader, "admin:read"),
		},
		{
			Method:   "GET",
			Path:     "/admin/api/v1/models/grouped",
			Note:     "reader+ or admin:read",
			Outcomes: protectedAccess(auth.RoleReader, "admin:read"),
		},
		{
			Method:      "DELETE",
			Path:        "/admin/api/v1/models/cache",
			Body:        purgeJSON,
			ContentType: "application/json",
			Note:        "admin+ or admin:write",
			Outcomes:    protectedAccess(auth.RoleAdmin, "admin:write"),
		},
		{
			Method:   "GET",
			Path:     "/admin/api/v1/cache/stats",
			Note:     "reader+ or admin:read",
			Outcomes: protectedAccess(auth.RoleReader, "admin:read"),
		},
	}
}

// publicAlwaysAdmits returns the outcomes map for routes that bypass
// every middleware (/healthz, /metrics).  Every credential is
// admitted because the route is registered directly on the root mux
// before any auth wrapper runs.
func publicAlwaysAdmits() map[Credential]Outcome {
	out := make(map[Credential]Outcome, 12)
	for _, c := range allCredentials() {
		out[c] = Admitted
	}
	return out
}

// publicReachableButMiddlewareGated returns the outcomes for routes
// under /auth/ whose handler does NOT enforce auth -- but the
// surrounding authn.Middleware does, so any *invalid* credential
// gets rejected upstream.  Anonymous and valid credentials reach the
// handler; bogus / revoked / expired creds are short-circuited.
func publicReachableButMiddlewareGated() map[Credential]Outcome {
	out := map[Credential]Outcome{
		CredAnonymous:        Admitted,
		CredBogusPAT:         Unauth401,
		CredRevokedPAT:       Unauth401,
		CredExpiredPAT:       Unauth401,
		CredPATScopeRead:     Admitted,
		CredPATScopeWrite:    Admitted,
		CredPATScopeAdminAll: Admitted,
		CredSessionReader:    Admitted,
		CredSessionMember:    Admitted,
		CredSessionAdmin:     Admitted,
		CredSessionOwner:     Admitted,
		CredSessionRevoked:   Unauth401,
	}
	return out
}

// csrfEndpoint returns the outcomes for GET /auth/csrf.  The handler
// reads the session cookie directly; everything except a valid
// session 401s (including valid PATs, which carry no cookie).
func csrfEndpoint() map[Credential]Outcome {
	return map[Credential]Outcome{
		CredAnonymous:        Unauth401,
		CredBogusPAT:         Unauth401, // middleware rejects first
		CredRevokedPAT:       Unauth401,
		CredExpiredPAT:       Unauth401,
		CredPATScopeRead:     Unauth401, // handler's own cookie check
		CredPATScopeWrite:    Unauth401,
		CredPATScopeAdminAll: Unauth401,
		CredSessionReader:    Admitted,
		CredSessionMember:    Admitted,
		CredSessionAdmin:     Admitted,
		CredSessionOwner:     Admitted,
		CredSessionRevoked:   Unauth401,
	}
}

// protectedAccess builds the outcomes map for an endpoint guarded by
// requireAccess(minRole, scope).  Mirrors authz.go's branches:
//
//   - ActorType == ""              -> 401 (requireAuthenticated)
//   - ActorUser and Role >= minRole -> Admitted, else 403
//   - ActorToken and (HasScope(scope) || HasScope("admin:*")) -> Admitted, else 403
//
// Bogus / revoked / expired PATs and the revoked session are rejected
// by the middleware (401), so they never reach requireAccess.
func protectedAccess(minRole auth.Role, scope string) map[Credential]Outcome {
	out := map[Credential]Outcome{
		CredAnonymous:      Unauth401,
		CredBogusPAT:       Unauth401,
		CredRevokedPAT:     Unauth401,
		CredExpiredPAT:     Unauth401,
		CredSessionRevoked: Unauth401,
	}

	// PAT branch.  Each PAT carries exactly one scope; admin:*
	// matches every endpoint, others match only by string equality.
	out[CredPATScopeRead] = patOutcomeFor("admin:read", scope)
	out[CredPATScopeWrite] = patOutcomeFor("admin:write", scope)
	out[CredPATScopeAdminAll] = Admitted // admin:* always passes

	// Session branch.  Role >= minRole admits.
	out[CredSessionReader] = sessionOutcomeFor(auth.RoleReader, minRole)
	out[CredSessionMember] = sessionOutcomeFor(auth.RoleMember, minRole)
	out[CredSessionAdmin] = sessionOutcomeFor(auth.RoleAdmin, minRole)
	out[CredSessionOwner] = sessionOutcomeFor(auth.RoleOwner, minRole)

	return out
}

func patOutcomeFor(have, required string) Outcome {
	if have == required {
		return Admitted
	}
	return Forbidden403
}

func sessionOutcomeFor(have, required auth.Role) Outcome {
	if have.AtLeast(required) {
		return Admitted
	}
	return Forbidden403
}

// TestAdminContract drives every (endpoint, credential) cell against
// a freshly bootstrapped admin handler.  Failures are reported as
// sub-tests so a CI run surfaces every violation in one pass.
func TestAdminContract(t *testing.T) {
	f := newFixtures(t)
	handler := admin.NewHandler(admin.Config{
		Pool:       f.Pool,
		CacheDir:   t.TempDir(),
		TenantName: f.TenantName,
		Metrics:    observability.NewRegistry(),
	})

	endpoints := adminEndpoints(f)

	for _, ep := range endpoints {
		ep := ep
		// Refresh sessions before every endpoint run so any side
		// effect from the previous endpoint (notably /auth/logout
		// revoking the session cookie it sees) can't poison the
		// next endpoint's session-class assertions.  Done outside
		// t.Run so it executes serially in the parent goroutine,
		// even when the sub-tests below call t.Parallel().
		f.RefreshSessions(t)
		t.Run(ep.String(), func(t *testing.T) {
			for _, cred := range allCredentials() {
				cred := cred
				want := ep.requireOutcome(cred)
				t.Run(cred.String(), func(t *testing.T) {
					// Credentials per endpoint run sequentially.
					// Parallelising them inside an endpoint would
					// race /auth/logout against the other session
					// classes for that same endpoint -- which is
					// exactly the kind of order-dependent flakiness
					// the matrix is supposed to surface, not hide.
					req := buildRequest(t, ep)
					f.Apply(req, cred)
					rec := httptest.NewRecorder()
					handler.ServeHTTP(rec, req)
					if msg := want.Check(rec.Code); msg != "" {
						t.Errorf("auth contract violated for %s with %s\n  want: %s\n  got:  %d %s\n  body: %s\n  note: %s",
							ep, cred, want, rec.Code, http.StatusText(rec.Code),
							strings.TrimSpace(rec.Body.String()), ep.Note)
					}
				})
			}
		})
	}
}

// buildRequest builds an *http.Request for the endpoint with no
// credentials attached.  Fixtures.Apply layers them in.
func buildRequest(t *testing.T, ep Endpoint) *http.Request {
	t.Helper()
	var body *bytes.Reader
	if ep.Body != nil {
		body = bytes.NewReader(ep.Body)
	}
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(ep.Method, ep.Path, body)
	} else {
		req = httptest.NewRequest(ep.Method, ep.Path, http.NoBody)
	}
	if ep.ContentType != "" {
		req.Header.Set("Content-Type", ep.ContentType)
	}
	return req
}

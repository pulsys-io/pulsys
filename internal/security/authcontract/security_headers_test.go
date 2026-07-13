// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Security-headers regression test (OWASP WSTG-CONF-06).
//
// PURPOSE
//   Every response from the admin surface MUST carry a fixed
//   set of defense-in-depth response headers regardless of:
//
//     - status code (200, 4xx, 5xx, 3xx) -- failures are the
//       MOST important branch to harden because that's what an
//       attacker probing the surface sees
//     - response content type (JSON, text/plain, Prometheus
//       text/plain;version=0.0.4, empty bodies)
//     - which handler in the chain wrote the response (custom
//       notFound, http.Error, writeJSON, observability healthz)
//
//   This test exercises every code path on the admin handler
//   and asserts the security-headers middleware (added in this
//   patch) decorates every response.
//
//   Required headers and rationale:
//
//     X-Content-Type-Options: nosniff
//       Browsers MUST NOT sniff response bodies as a different
//       MIME type than what we declared.  Prevents stored-XSS
//       via JSON that contains HTML-ish content.
//
//     X-Frame-Options: DENY
//       Click-jacking defense.  The admin SPA must never be
//       framed.  DENY is stricter than SAMEORIGIN.
//
//     Referrer-Policy: no-referrer
//       URLs may contain tenant slugs; outbound links must not
//       leak them via Referer header.
//
//     Cross-Origin-Opener-Policy: same-origin
//       Specter-class isolation for the admin browsing context.
//
//     Cross-Origin-Resource-Policy: same-origin
//       Stops other origins embedding our resources via
//       <script> / <img> / @font-face.
//
//     Permissions-Policy: restrictive
//       Denies access to powerful browser APIs the admin SPA
//       never needs.
//
//   We deliberately DO NOT assert Strict-Transport-Security
//   here -- that's the LB's responsibility (see
//   docs/security.md).

package authcontract

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/admin"
	adminstore "github.com/pulsys-io/pulsys/internal/admin/store"
	"github.com/pulsys-io/pulsys/internal/auth"
	authstore "github.com/pulsys-io/pulsys/internal/auth/store"
	"github.com/pulsys-io/pulsys/internal/observability"
	"github.com/pulsys-io/pulsys/internal/testpg"
)

// requiredSecurityHeaders is the contract every admin response
// MUST satisfy.  Empty value = "must be present and non-empty";
// non-empty value = "must be present and equal to the listed
// value exactly".
var requiredSecurityHeaders = map[string]string{
	"X-Content-Type-Options":       "nosniff",
	"X-Frame-Options":              "DENY",
	"Referrer-Policy":              "no-referrer",
	"Cross-Origin-Opener-Policy":   "same-origin",
	"Cross-Origin-Resource-Policy": "same-origin",
	"Permissions-Policy":           "", // presence-only
}

func newHeadersFixture(t *testing.T) (http.Handler, string) {
	t.Helper()
	pool := testpg.Acquire(t)
	pgAuth := authstore.NewPG(pool)
	pgAdmin := adminstore.NewAdminStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tid, err := pgAuth.EnsureTenant(ctx, "headers", "Headers")
	if err != nil {
		t.Fatalf("ensure tenant: %v", err)
	}
	uid, err := pgAuth.CreateUserOIDC(ctx, auth.User{
		TenantID: tid, Email: "h@local", DisplayName: "h",
		Role: auth.RoleOwner, OIDCSub: "sub-h", IsActive: true,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	display, prefix, hash, err := auth.GeneratePAT()
	if err != nil {
		t.Fatalf("gen pat: %v", err)
	}
	if _, err := pgAdmin.CreateToken(ctx, tid, uid, "h-admin", prefix, hash, []string{"admin:*"}, nil); err != nil {
		t.Fatalf("create token: %v", err)
	}
	h := admin.NewHandler(admin.Config{
		Pool:       pool,
		CacheDir:   t.TempDir(),
		TenantName: "headers",
		Metrics:    observability.NewRegistry(),
	})
	return h, display
}

// headerProbe is one (method, path, expectStatus) probe.
// expectStatus is a status-class filter rather than an exact
// match, because some paths may legitimately return 200 OR
// 404 depending on store state -- both are valid response
// surfaces that the middleware MUST decorate.
type headerProbe struct {
	name        string
	method      string
	path        string
	headers     map[string]string
	body        []byte
	expectClass string // "2xx", "3xx", "4xx", "5xx", "any"
}

var headerProbes = []headerProbe{
	{name: "metrics_public", method: "GET", path: "/metrics", expectClass: "2xx"},
	{name: "healthz_public", method: "GET", path: "/healthz", expectClass: "any"},
	{name: "unmounted_admin_404",
		method: "GET", path: "/admin/api/v1/does/not/exist",
		expectClass: "4xx"},
	{name: "admin_auth_required",
		method: "GET", path: "/admin/api/v1/tenant",
		expectClass: "4xx"}, // 401 without PAT
	{name: "admin_with_pat_200",
		method: "GET", path: "/admin/api/v1/tenant",
		headers:     map[string]string{"_USE_PAT": "yes"},
		expectClass: "2xx"},
	{name: "method_not_allowed",
		method:      "POST",
		path:        "/healthz",
		expectClass: "4xx",
	},
	{name: "auth_session_bad_json",
		method:      "POST",
		path:        "/auth/session",
		headers:     map[string]string{"Content-Type": "application/json"},
		body:        []byte(`{not-valid-json...`),
		expectClass: "4xx",
	},
	{name: "auth_oidc_config_unconfigured",
		method:      "GET",
		path:        "/auth/oidc/config",
		expectClass: "any"},
}

// TestSecurityHeaders_AdminSurface walks every probe in
// headerProbes, exercises the admin handler, and asserts every
// required header is present and (where applicable) carries the
// expected value.
func TestSecurityHeaders_AdminSurface(t *testing.T) {
	handler, pat := newHeadersFixture(t)

	for _, p := range headerProbes {
		p := p
		t.Run(p.name, func(t *testing.T) {
			req := httptest.NewRequest(p.method, p.path, bytes.NewReader(p.body))
			for k, v := range p.headers {
				if k == "_USE_PAT" {
					req.Header.Set("Authorization", "Bearer "+pat)
					continue
				}
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			t.Logf("%s -> %d", p.path, rec.Code)
			if !statusInClass(rec.Code, p.expectClass) {
				t.Logf("status %d not in %s but proceeding to header check", rec.Code, p.expectClass)
			}

			for name, want := range requiredSecurityHeaders {
				got := rec.Header().Get(name)
				if got == "" {
					t.Errorf("WSTG-CONF-06: %s missing %q header (status %d, body[:120]=%q)",
						p.path, name, rec.Code, truncate(rec.Body.String(), 120))
					continue
				}
				if want != "" && got != want {
					t.Errorf("WSTG-CONF-06: %s %q=%q, want %q",
						p.path, name, got, want)
				}
			}
		})
	}
}

// TestSecurityHeaders_NoServerHeaderLeakage asserts the admin
// handler does NOT emit a "Server: ..." or "X-Powered-By: ..."
// header that betrays implementation (Go version, framework
// version).  Both are explicit information-disclosure markers
// in scanner reports.
func TestSecurityHeaders_NoServerHeaderLeakage(t *testing.T) {
	handler, pat := newHeadersFixture(t)
	req := httptest.NewRequest("GET", "/admin/api/v1/tenant", nil)
	req.Header.Set("Authorization", "Bearer "+pat)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	for _, bad := range []string{"Server", "X-Powered-By", "X-AspNet-Version", "X-Runtime"} {
		if v := rec.Header().Get(bad); v != "" {
			t.Errorf("WSTG-INFO-02: response leaks %q=%q (technology disclosure)", bad, v)
		}
	}
}

// statusInClass reports whether code matches one of:
//
//	"any" - any non-zero status
//	"2xx", "3xx", "4xx", "5xx" - the conventional class
func statusInClass(code int, class string) bool {
	if class == "any" {
		return code > 0
	}
	if len(class) != 3 {
		return false
	}
	first := class[0]
	switch first {
	case '2':
		return code >= 200 && code < 300
	case '3':
		return code >= 300 && code < 400
	case '4':
		return code >= 400 && code < 500
	case '5':
		return code >= 500 && code < 600
	}
	return false
}

// truncate (already defined in xss_json_response_test.go in the
// same package) -- avoid redefinition.
var _ = strings.TrimSpace

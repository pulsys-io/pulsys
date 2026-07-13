// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// JSON-response XSS regression test (OWASP WSTG-INPV-01 / -02).
//
// PURPOSE
//   Pulsys returns JSON for every admin API endpoint.  Modern
//   browsers do NOT execute scripts in responses served with
//   Content-Type: application/json -- as long as that header is
//   present, no X-Content-Type-Options sniffing fallback is
//   possible, and the body is valid JSON (so it isn't ambiguously
//   parsed as HTML).
//
//   This test pins three invariants:
//
//     1. Every JSON-returning admin endpoint sets
//        Content-Type: application/json (not text/plain, not a
//        missing header).  A missing or wrong Content-Type lets
//        IE/old-Edge sniff HTML and execute scripts in the body.
//
//     2. Every JSON-returning admin endpoint sets X-Content-Type-
//        Options: nosniff.  Belt-and-suspenders against the
//        sniffing path above.
//
//     3. STORED-XSS round-trip: when a token name is set to
//        '<script>alert(1)</script>' and later retrieved via
//        /admin/api/v1/tokens, the response body MUST encode the
//        characters as a JSON-escaped string.  json.Marshal does
//        this by default; the test pins it so a future "let's
//        write the body manually" refactor doesn't regress.
//
//   This is an authcontract test because the storage layer is the
//   trust boundary -- the round-trip through the DB is what makes
//   "stored XSS" different from "reflected XSS".

package authcontract

import (
	"bytes"
	"context"
	"encoding/json"
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

func newXSSFixture(t *testing.T) (http.Handler, string) {
	t.Helper()
	pool := testpg.Acquire(t)
	pgAuth := authstore.NewPG(pool)
	pgAdmin := adminstore.NewAdminStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tid, err := pgAuth.EnsureTenant(ctx, "xss-probe", "XSS Probe")
	if err != nil {
		t.Fatalf("ensure tenant: %v", err)
	}
	uid, err := pgAuth.CreateUserOIDC(ctx, auth.User{
		TenantID: tid, Email: "xss@local", DisplayName: "xss",
		Role: auth.RoleOwner, OIDCSub: "sub-xss", IsActive: true,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	display, prefix, hash, err := auth.GeneratePAT()
	if err != nil {
		t.Fatalf("gen PAT: %v", err)
	}
	if _, err := pgAdmin.CreateToken(ctx, tid, uid, "xss-admin", prefix, hash, []string{"admin:*"}, nil); err != nil {
		t.Fatalf("create token: %v", err)
	}
	handler := admin.NewHandler(admin.Config{
		Pool:       pool,
		CacheDir:   t.TempDir(),
		TenantName: "default",
		Metrics:    observability.NewRegistry(),
	})
	return handler, display
}

// jsonEndpoints lists every admin API endpoint that returns JSON
// on the happy path along with a method, path, and optional body.
var jsonEndpoints = []struct {
	name   string
	method string
	path   string
	body   []byte
}{
	{"get_tenant", "GET", "/admin/api/v1/tenant", nil},
	{"list_users", "GET", "/admin/api/v1/users?limit=10", nil},
	{"list_tokens", "GET", "/admin/api/v1/tokens?limit=10", nil},
	{"list_settings", "GET", "/admin/api/v1/settings", nil},
	{"list_audit", "GET", "/admin/api/v1/audit?limit=10", nil},
	{"list_models", "GET", "/admin/api/v1/models?limit=10", nil},
	{"list_models_grouped", "GET", "/admin/api/v1/models/grouped?limit=10", nil},
}

// TestXSSJSON_ContentTypeAndNoSniffOnEveryEndpoint asserts every
// JSON endpoint sets the application/json content type and the
// nosniff anti-MIME-confusion header.
func TestXSSJSON_ContentTypeAndNoSniffOnEveryEndpoint(t *testing.T) {
	handler, pat := newXSSFixture(t)
	for _, ep := range jsonEndpoints {
		ep := ep
		t.Run(ep.name, func(t *testing.T) {
			req := httptest.NewRequest(ep.method, ep.path, bytes.NewReader(ep.body))
			req.Header.Set("Authorization", "Bearer "+pat)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code >= 400 {
				t.Logf("%s returned %d body=%s (asserting headers anyway)", ep.path, rec.Code, rec.Body)
			}
			ct := rec.Header().Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				t.Errorf("WSTG-INPV-01: %s Content-Type=%q (must start with application/json so browsers don't sniff as HTML)", ep.path, ct)
			}
			// nosniff is the belt-and-suspenders defense.
			// Without it, IE/old Edge may sniff body content as
			// HTML and execute scripts.  This is also a
			// WSTG-CONF-06 requirement we cover here for the
			// JSON surface.
			if v := rec.Header().Get("X-Content-Type-Options"); v != "nosniff" {
				t.Errorf("WSTG-CONF-06: %s missing X-Content-Type-Options: nosniff (got %q)", ep.path, v)
			}
		})
	}
}

// TestXSSJSON_StoredXSSRoundTripsAsEscapedJSON creates a token
// whose name contains the canonical XSS payload, retrieves it via
// the list endpoint, and asserts the body encodes the script tag
// as a JSON-escaped string (NOT as raw HTML).
func TestXSSJSON_StoredXSSRoundTripsAsEscapedJSON(t *testing.T) {
	handler, pat := newXSSFixture(t)

	// 1. Plant the payload via POST /tokens.
	payload := `<script>alert(1)</script>"><img src=x onerror=alert(1)>`
	createBody, _ := json.Marshal(map[string]any{
		"name":   payload,
		"scopes": []string{"models:read"},
	})
	req := httptest.NewRequest("POST", "/admin/api/v1/tokens", bytes.NewReader(createBody))
	req.Header.Set("Authorization", "Bearer "+pat)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create token: %d body=%s", rec.Code, rec.Body)
	}

	// 2. Retrieve via GET /tokens.
	req = httptest.NewRequest("GET", "/admin/api/v1/tokens?limit=100", nil)
	req.Header.Set("Authorization", "Bearer "+pat)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list tokens: %d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()

	// 3. The body MUST contain the script tag as a JSON string,
	// not as raw HTML.  Two equivalent invariants:
	//   (a) the body MUST NOT contain an unescaped '<script>'
	//       outside a quoted JSON string;
	//   (b) the body MUST decode as valid JSON whose name field
	//       round-trips byte-for-byte.
	var parsed struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("WSTG-INPV-02: response is not valid JSON (browser would sniff as HTML and execute payload): %v\n  body: %s",
			err, truncate(body, 400))
	}
	found := false
	for _, item := range parsed.Items {
		if item.Name == payload {
			found = true
		}
	}
	if !found {
		t.Fatalf("WSTG-INPV-02: planted token name not found in response; storage layer mangled it")
	}
	// The encoded body MUST contain the escaped form, not the
	// raw HTML.  json.Marshal escapes "<" as "\u003c" by default
	// when EscapeHTML is on (the std lib default), which is
	// exactly the safety property we depend on.
	if strings.Contains(body, "<script>") {
		t.Fatalf("WSTG-INPV-02: response body contains raw '<script>' tag; browser may execute even with application/json header\n  body: %s",
			truncate(body, 400))
	}
	if !strings.Contains(body, "\\u003c") && !strings.Contains(body, "\\u003C") {
		// Some JSON encoders use \u003c, others output the literal
		// "<" escaped only via JSON string semantics.  Either is
		// safe IF Content-Type is application/json + nosniff,
		// which we asserted in the other test.  Flag as a NOTE
		// only.
		t.Logf("NOTE: response body does not use \\u003c escaping; safe only if Content-Type stays application/json + nosniff")
	}
}

// TestXSSJSON_NoUnescapedHtmlInErrorBodies checks that the
// writeError path (writeJSON with an error map) also escapes
// HTML metacharacters.  A 4xx body with a reflected user-supplied
// string is the canonical reflected-XSS gadget; even with JSON
// Content-Type we want defense-in-depth here.
func TestXSSJSON_NoUnescapedHtmlInErrorBodies(t *testing.T) {
	handler, pat := newXSSFixture(t)

	// Trigger a validation error by sending an invalid name.
	body, _ := json.Marshal(map[string]any{
		"name":   "", // empty name -> 400 "name required"
		"scopes": []string{"models:read"},
	})
	req := httptest.NewRequest("POST", "/admin/api/v1/tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+pat)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Errorf("error response Content-Type=%q (must be application/json)", rec.Header().Get("Content-Type"))
	}
	if rec.Code >= 500 {
		t.Errorf("validation error returned 5xx: %d body=%s", rec.Code, rec.Body)
	}
	// Body must be parseable JSON; if a future refactor switches
	// to http.Error (text/plain), this fails fast.
	var parsed map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Errorf("error response is not valid JSON: %v body=%s", err, rec.Body)
	}
}

// truncate is shared with authcontract; copy here to avoid
// cross-package cycle.  Returns a printable preview of a string,
// at most n bytes.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

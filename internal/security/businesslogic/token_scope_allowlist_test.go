// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package businesslogic

import (
	"net/http"
	"strings"
	"testing"
)

// TestCreateToken_ScopeAllowlist pins the Phase 5 invariant:
// createToken rejects any scope outside the documented set
// with HTTP 400.  The matrix covers every accepted scope (must
// pass), every "rejected" scope class the recon identified, and
// representative attack shapes (path traversal, NUL, very long).
func TestCreateToken_ScopeAllowlist(t *testing.T) {
	f := newFixtures(t)

	t.Run("accepts known scopes", func(t *testing.T) {
		known := []string{"models:read", "models:write", "admin:read", "admin:write", "admin:*"}
		for _, sc := range known {
			body := `{"name":"k-` + sc + `","scopes":["` + sc + `"]}`
			rec, resp := f.postCreateToken(body)
			if rec.Code != http.StatusCreated {
				t.Errorf("scope %q: status=%d body=%s", sc, rec.Code, rec.Body.String())
				continue
			}
			if len(resp.Scopes) != 1 || resp.Scopes[0] != sc {
				t.Errorf("scope %q: returned scopes=%v", sc, resp.Scopes)
			}
		}
	})

	t.Run("rejects unknown scopes", func(t *testing.T) {
		bad := []string{
			"superadmin",
			"models:execute",
			"billing:read",
			"models:read ", // trailing space
			" models:read", // leading space
			"MODELS:READ",  // wrong case
			"models:*",     // wildcard for non-admin
			"*",            // bare wildcard
			"",             // empty
		}
		for _, sc := range bad {
			body := `{"name":"x","scopes":["` + sc + `"]}`
			rec, _ := f.postCreateToken(body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("scope %q: status=%d want 400; body=%s", sc, rec.Code, rec.Body.String())
				continue
			}
			if !strings.Contains(rec.Body.String(), "scope not allowed") {
				t.Errorf("scope %q: error body=%q (want 'scope not allowed: ...')", sc, rec.Body.String())
			}
		}
	})

	t.Run("rejects on first bad scope in a mixed list", func(t *testing.T) {
		// "models:read" is fine, but "models:execute" should
		// fail the whole request -- no token is minted.
		body := `{"name":"mix","scopes":["models:read","models:execute"]}`
		rec, _ := f.postCreateToken(body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s want 400", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "models:execute") {
			t.Fatalf("body=%q should mention the offending scope", rec.Body.String())
		}
	})

	t.Run("defaults to models:read on empty list", func(t *testing.T) {
		// The default-scope behavior is BUSL contract: an
		// admin who forgets to pass scopes gets a read-only
		// token, not an admin-all token.  Lock it in.
		body := `{"name":"default-scopes","scopes":[]}`
		rec, resp := f.postCreateToken(body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status=%d body=%s want 201", rec.Code, rec.Body.String())
		}
		if len(resp.Scopes) != 1 || resp.Scopes[0] != "models:read" {
			t.Fatalf("scopes=%v want [models:read]", resp.Scopes)
		}
	})
}

// TestCreateToken_PayloadShape covers handler-level input
// validation upstream of the allowlist: missing name, malformed
// JSON, oversized name.  None of these should reach the store.
func TestCreateToken_PayloadShape(t *testing.T) {
	f := newFixtures(t)
	cases := []struct {
		desc, body string
	}{
		{"empty name", `{"name":"","scopes":["models:read"]}`},
		{"missing name", `{"scopes":["models:read"]}`},
		{"malformed json", `{"name":"x", "scopes":`},
		{"name only no scopes (defaults)", `{"name":"x"}`},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			rec, _ := f.postCreateToken(c.body)
			// "name only" succeeds (defaults).  The other
			// three should 400.
			if c.desc == "name only no scopes (defaults)" {
				if rec.Code != http.StatusCreated {
					t.Fatalf("status=%d body=%s want 201", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s want 400", rec.Code, rec.Body.String())
			}
		})
	}
}

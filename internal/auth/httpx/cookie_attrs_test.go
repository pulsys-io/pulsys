// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Session + CSRF cookie attribute regression test (OWASP WSTG-SESS-02).
//
// PURPOSE
//   The session and CSRF cookies are the credential surface for the
//   admin SPA.  Every cookie attribute that affects their security
//   posture MUST be asserted explicitly -- a future refactor that
//   drops HttpOnly or Secure or SameSite would otherwise ship
//   silently and only be caught by a customer audit.
//
//   Mandatory invariants:
//
//     pulsys_session
//       - HttpOnly: true  (JS in the SPA must NEVER be able to read
//                          the session token; otherwise an XSS in
//                          admin-ui exfiltrates the credential)
//       - Secure:   true when SecureCookies is set (production)
//       - SameSite: Lax  (allow top-level navigation -- needed for
//                          OAuth callbacks; reject cross-site POSTs
//                          which is the CSRF vector)
//       - Path:     "/"  (cookie must be sent to every admin route)
//       - Domain:   ""   (host-only; no subdomain widening)
//       - Expires:  in the future, matches session TTL
//
//     pulsys_csrf
//       - HttpOnly: false (the SPA reads this and echoes it in a
//                          header -- the double-submit token must be
//                          script-readable, by design)
//       - Secure:   true when SecureCookies is set
//       - SameSite: Strict (CSRF token must NEVER ride along on a
//                            cross-site request, ever)
//       - Path:     "/"
//       - Domain:   "" (host-only)
//
//   Logout also sets a clearing cookie (empty value, MaxAge=-1); we
//   assert that the clearing cookie carries the SAME flags as the
//   live cookie so an intermediary doesn't reject it for flag drift.

package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/auth"
)

// TestSetSessionCookie_AttributesProduction asserts every required
// cookie attribute on the live session cookie in production
// (SecureCookies = true).
func TestSetSessionCookie_AttributesProduction(t *testing.T) {
	h := &Handler{SecureCookies: true}
	rec := httptest.NewRecorder()
	exp := time.Now().UTC().Add(8 * time.Hour)
	h.setSessionCookie(rec, "session-plain-token", exp)

	cookies := decodeCookies(t, rec.Result().Cookies(), auth.SessionCookieName)
	if len(cookies) != 1 {
		t.Fatalf("expected exactly one Set-Cookie for %q, got %d", auth.SessionCookieName, len(cookies))
	}
	c := cookies[0]

	if c.Value != "session-plain-token" {
		t.Errorf("cookie value mismatch: got %q", c.Value)
	}
	if !c.HttpOnly {
		t.Error("WSTG-SESS-02: session cookie MUST be HttpOnly (XSS would otherwise exfiltrate it)")
	}
	if !c.Secure {
		t.Error("WSTG-SESS-02: session cookie MUST be Secure in production (cleartext over HTTP leaks the credential)")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("WSTG-SESS-02: session cookie SameSite MUST be Lax (got %v); needed for OAuth callbacks while still blocking cross-site form POSTs", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("session cookie Path MUST be \"/\", got %q (a narrower path would prevent admin routes from receiving it)", c.Path)
	}
	if c.Domain != "" {
		t.Errorf("session cookie Domain MUST be empty (host-only); got %q -- a Domain= attribute widens scope to subdomains", c.Domain)
	}
	if c.Expires.IsZero() {
		t.Error("session cookie MUST set an Expires (or Max-Age); otherwise it becomes a session cookie that survives forever in some browsers")
	}
}

// TestSetSessionCookie_AttributesDev asserts that Secure is FALSE
// only when SecureCookies is false (dev mode over plain HTTP);
// all other attributes MUST be identical to production.
func TestSetSessionCookie_AttributesDev(t *testing.T) {
	h := &Handler{SecureCookies: false}
	rec := httptest.NewRecorder()
	h.setSessionCookie(rec, "tok", time.Now().Add(time.Hour))

	c := decodeCookies(t, rec.Result().Cookies(), auth.SessionCookieName)[0]
	if c.Secure {
		t.Error("dev mode: SecureCookies=false should produce Secure=false (else SPA over plain HTTP cannot read it)")
	}
	if !c.HttpOnly {
		t.Error("dev mode MUST still set HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("dev mode SameSite must still be Lax, got %v", c.SameSite)
	}
}

// TestSetCSRFCookie_Attributes asserts the double-submit CSRF cookie
// has the inverted HttpOnly contract (script-readable by design)
// plus Strict SameSite (a CSRF token MUST NEVER ride a cross-site
// request).
func TestSetCSRFCookie_Attributes(t *testing.T) {
	t.Run("production", func(t *testing.T) {
		rec := httptest.NewRecorder()
		auth.SetCSRFCookie(rec, "csrf-token-value", true)
		c := decodeCookies(t, rec.Result().Cookies(), auth.CSRFCookieName)[0]
		if c.HttpOnly {
			t.Error("WSTG-SESS-02: CSRF double-submit cookie MUST NOT be HttpOnly; the SPA reads it and echoes it in a header")
		}
		if !c.Secure {
			t.Error("CSRF cookie MUST be Secure in production")
		}
		if c.SameSite != http.SameSiteStrictMode {
			t.Errorf("WSTG-SESS-02: CSRF cookie SameSite MUST be Strict (got %v); Lax would let cross-site GETs send it", c.SameSite)
		}
		if c.Path != "/" {
			t.Errorf("CSRF cookie Path MUST be \"/\", got %q", c.Path)
		}
		if c.Domain != "" {
			t.Errorf("CSRF cookie Domain MUST be empty (host-only), got %q", c.Domain)
		}
	})
	t.Run("dev", func(t *testing.T) {
		rec := httptest.NewRecorder()
		auth.SetCSRFCookie(rec, "csrf", false)
		c := decodeCookies(t, rec.Result().Cookies(), auth.CSRFCookieName)[0]
		if c.Secure {
			t.Error("dev mode: CSRF cookie Secure should be false")
		}
	})
}

// TestClearCookies_AttributesMatchLive asserts that the clearing
// cookies (logout) carry the SAME flags as the live cookies they
// replace.  An intermediary that sees a cookie change Secure/
// HttpOnly/SameSite mid-session may refuse to overwrite, leaving
// the stale credential in place.
func TestClearCookies_AttributesMatchLive(t *testing.T) {
	t.Run("session_clear_matches_live", func(t *testing.T) {
		// Build a logout request that goes through Handler.logout.
		// We deliberately don't supply a session cookie so the
		// handler's revoke-path is skipped (it would dereference
		// the OIDC store which isn't wired in this unit test).
		// The clearing-cookie attributes are independent of the
		// revoke path -- they're set unconditionally.
		h := &Handler{SecureCookies: true}
		req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
		rec := httptest.NewRecorder()
		h.logout(rec, req)

		clears := decodeCookies(t, rec.Result().Cookies(), auth.SessionCookieName)
		if len(clears) != 1 {
			t.Fatalf("expected one clearing Set-Cookie for %q, got %d", auth.SessionCookieName, len(clears))
		}
		c := clears[0]
		if c.Value != "" {
			t.Errorf("clearing cookie must have empty value, got %q", c.Value)
		}
		if c.MaxAge >= 0 {
			t.Errorf("clearing cookie must have MaxAge < 0 (got %d) so the browser deletes it", c.MaxAge)
		}
		if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
			t.Errorf("clearing session cookie flags drift from live: HttpOnly=%v Secure=%v SameSite=%v Path=%q",
				c.HttpOnly, c.Secure, c.SameSite, c.Path)
		}
	})
	t.Run("csrf_clear_matches_live", func(t *testing.T) {
		rec := httptest.NewRecorder()
		auth.ClearCSRFCookie(rec, true)
		c := decodeCookies(t, rec.Result().Cookies(), auth.CSRFCookieName)[0]
		if c.Value != "" || c.MaxAge >= 0 {
			t.Error("CSRF clearing cookie must be empty + MaxAge<0")
		}
		if c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
			t.Errorf("CSRF clearing cookie flags drift from live: HttpOnly=%v Secure=%v SameSite=%v",
				c.HttpOnly, c.Secure, c.SameSite)
		}
	})
}

// TestCookies_NoLeakingSensitiveDataInHeaders sanity-checks that no
// other header in the response embeds the cookie value (a common
// regression pattern when devs add diagnostic headers like
// "X-Session-Token: <value>" for debugging and forget to remove
// them).
func TestCookies_NoLeakingSensitiveDataInHeaders(t *testing.T) {
	h := &Handler{SecureCookies: true}
	rec := httptest.NewRecorder()
	tok := "extremely-secret-session-token-value"
	h.setSessionCookie(rec, tok, time.Now().Add(time.Hour))

	for k, vs := range rec.Result().Header {
		// Set-Cookie naturally carries the value; that's fine.
		if strings.EqualFold(k, "Set-Cookie") {
			continue
		}
		for _, v := range vs {
			if strings.Contains(v, tok) {
				t.Fatalf("session token leaked outside Set-Cookie: header %q = %q", k, v)
			}
		}
	}
}

// decodeCookies filters a *http.Response.Cookies() result to a
// specific name.  Used to assert only on the cookie under test.
func decodeCookies(t *testing.T, all []*http.Cookie, name string) []*http.Cookie {
	t.Helper()
	out := make([]*http.Cookie, 0, 1)
	for _, c := range all {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

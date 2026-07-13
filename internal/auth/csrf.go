// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

const (
	// CSRFCookieName is the readable double-submit cookie (NOT HttpOnly).
	CSRFCookieName = "pulsys_csrf"

	// CSRFHeaderName is sent by the admin SPA on mutating requests.
	CSRFHeaderName = "X-Pulsys-CSRF"
)

// SafeMethod reports HTTP methods that do not require CSRF validation.
func SafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// CSRFExemptPath reports auth endpoints that establish sessions without
// an existing CSRF token.
func CSRFExemptPath(path string) bool {
	switch path {
	case "/auth/session", "/auth/oidc/config":
		return true
	default:
		return false
	}
}

// ValidateCSRF checks the double-submit header/cookie pair against the
// server-side session token for human actors.  PAT bearer auth skips CSRF.
func ValidateCSRF(r *http.Request, actor Actor, sessionCSRF string) bool {
	if actor.Type == ActorToken {
		return true
	}
	if actor.Type != ActorUser {
		return false
	}
	if sessionCSRF == "" {
		return false
	}
	header := strings.TrimSpace(r.Header.Get(CSRFHeaderName))
	if header == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(header), []byte(sessionCSRF)) != 1 {
		return false
	}
	cookie, err := r.Cookie(CSRFCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(header)) == 1
}

// CSRFProtect rejects mutating requests from human sessions when the
// double-submit token is missing or mismatched.
func CSRFProtect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if SafeMethod(r.Method) || CSRFExemptPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		actor := ActorFromContext(r.Context())
		if actor.Type == "" {
			next.ServeHTTP(w, r)
			return
		}
		if !ValidateCSRF(r, actor, CSRFTokenFromContext(r.Context())) {
			http.Error(w, "csrf token missing or invalid", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SetCSRFCookie writes the readable double-submit cookie.
func SetCSRFCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}

// ClearCSRFCookie removes the CSRF cookie on logout.
func ClearCSRFCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}

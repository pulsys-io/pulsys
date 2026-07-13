// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidateCSRFTokenBypassPAT(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/admin/api/v1/tokens", nil)
	actor := Actor{Type: ActorToken, TenantID: "t1"}
	if !ValidateCSRF(r, actor, "") {
		t.Fatal("PAT should bypass CSRF")
	}
}

func TestValidateCSRFDoubleSubmit(t *testing.T) {
	const tok = "abc123csrf"
	r := httptest.NewRequest(http.MethodPost, "/admin/api/v1/tokens", nil)
	r.Header.Set(CSRFHeaderName, tok)
	r.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tok})
	actor := Actor{Type: ActorUser, TenantID: "t1"}
	if !ValidateCSRF(r, actor, tok) {
		t.Fatal("expected valid CSRF")
	}
}

func TestValidateCSRFRejectsMismatch(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/admin/api/v1/tokens", nil)
	r.Header.Set(CSRFHeaderName, "header")
	r.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: "cookie"})
	actor := Actor{Type: ActorUser}
	if ValidateCSRF(r, actor, "server") {
		t.Fatal("expected rejection")
	}
}

func TestCSRFProtectExemptsSessionEstablish(t *testing.T) {
	called := false
	h := CSRFProtect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/auth/session", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !called || rec.Code != http.StatusOK {
		t.Fatalf("called=%v status=%d", called, rec.Code)
	}
}

func TestCSRFProtectBlocksHumanMutation(t *testing.T) {
	h := CSRFProtect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1/tokens", nil)
	req = req.WithContext(ContextWithActor(req.Context(), Actor{Type: ActorUser}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d", rec.Code)
	}
}

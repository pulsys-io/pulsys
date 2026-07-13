// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package httpx

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/pulsys-io/pulsys/internal/auth"
	"github.com/pulsys-io/pulsys/internal/auth/oidc"
)

// Handler exposes OIDC config + session establishment for the admin SPA.
//
// The SPA (P6) owns the full OIDC authorization-code + PKCE flow against
// the external IdP. After the SPA obtains an id_token, it POSTs it here;
// the backend verifies the token, JIT-provisions the user, and sets the
// HttpOnly pulsys_session cookie.
type Handler struct {
	OIDC          *oidc.Service
	TenantName    string // default tenant slug for v1 single-tenant AMI
	SecureCookies bool
	Logger        *slog.Logger // optional; nil falls back to slog.Default()
}

// log returns h.Logger if set, otherwise slog.Default().  Used by the
// account-enumeration defense so operators can still debug failed
// session establishments while clients get a uniform 401.
func (h *Handler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// Mount registers routes on mux:
//
//	GET  /auth/oidc/config   public IdP metadata for the SPA (no secret)
//	GET  /auth/csrf          sync CSRF cookie for existing session
//	POST /auth/session       verify id_token from SPA, issue session cookie
//	POST /auth/logout        revoke session
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/oidc/config", h.oidcConfig)
	mux.HandleFunc("GET /auth/csrf", h.csrfToken)
	mux.HandleFunc("POST /auth/session", h.establishSession)
	mux.HandleFunc("POST /auth/logout", h.logout)
}

func (h *Handler) oidcConfig(w http.ResponseWriter, r *http.Request) {
	tenantID, err := h.resolveTenant(r)
	if err != nil {
		// WSTG-IDNT-04: same enumeration-defense posture as
		// establishSession -- never echo the resolver error.  An
		// unknown tenant and an unconfigured tenant both return
		// 503 with a fixed body so an unauthenticated probe cannot
		// distinguish them.
		h.log().Warn("oidc_config_tenant_resolve_failed",
			slog.String("tenant_query", r.URL.Query().Get("tenant_id")),
			slog.String("err", err.Error()))
		http.Error(w, "OIDC not configured for this tenant", http.StatusServiceUnavailable)
		return
	}
	cfg, err := h.OIDC.PublicConfig(r.Context(), tenantID)
	if err != nil {
		if err == auth.ErrNoOIDCProvider {
			http.Error(w, "OIDC not configured for this tenant", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "failed to load OIDC config", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cfg)
}

type sessionRequest struct {
	IDToken string `json:"id_token"`
	Tenant  string `json:"tenant,omitempty"`
}

func (h *Handler) establishSession(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var req sessionRequest
	if err := json.Unmarshal(body, &req); err != nil || req.IDToken == "" {
		http.Error(w, "id_token required", http.StatusBadRequest)
		return
	}
	tenantID, err := h.resolveTenantWithOverride(r, req.Tenant)
	if err != nil {
		// WSTG-IDNT-04: do NOT echo the resolver error; doing so
		// turned this into a tenant-name enumeration oracle
		// ("store: tenant 'acme' not found" vs the generic 401 path
		// for known tenants).  Log the real reason internally and
		// return the same response shape as every other auth-side
		// failure below so an unauthenticated probe cannot
		// distinguish "tenant doesn't exist" from "id_token bad".
		h.log().Warn("session_establish_tenant_resolve_failed",
			slog.String("tenant_override", req.Tenant),
			slog.String("err", err.Error()))
		http.Error(w, "session establishment failed", http.StatusUnauthorized)
		return
	}
	sess, user, err := h.OIDC.EstablishSession(r.Context(), tenantID, req.IDToken)
	if err != nil {
		// WSTG-IDNT-04: collapse JIT-denial into the same generic
		// response as any other establishment failure so an
		// attacker with a valid IdP id_token cannot probe which
		// emails are pre-provisioned in this tenant (an
		// enumeration oracle that previously returned 403 "login
		// denied" specifically for the unknown-but-blocked-by-
		// RequirePreprovisioned case).  Operators retain the
		// distinction via the structured log below.
		h.log().Warn("session_establish_failed",
			slog.String("tenant_id", tenantID),
			slog.Bool("login_denied", err == auth.ErrLoginDenied),
			slog.String("err", err.Error()))
		http.Error(w, "session establishment failed", http.StatusUnauthorized)
		return
	}
	h.setSessionCookie(w, sess.PlainToken, sess.ExpiresAt)
	auth.SetCSRFCookie(w, sess.CSRFToken, h.SecureCookies)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"user_id":    user.ID,
		"email":      user.Email,
		"role":       user.Role,
		"csrf_token": sess.CSRFToken,
	})
}

func (h *Handler) csrfToken(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(auth.SessionCookieName)
	if err != nil || c.Value == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sess, _, err := h.OIDC.Store.LookupSession(r.Context(), c.Value)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	auth.SetCSRFCookie(w, sess.CSRFToken, h.SecureCookies)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"csrf_token": sess.CSRFToken})
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.SessionCookieName); err == nil && c.Value != "" {
		_ = h.OIDC.Store.RevokeSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	auth.ClearCSRFCookie(w, h.SecureCookies)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   h.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) resolveTenant(r *http.Request) (string, error) {
	return h.resolveTenantWithOverride(r, "")
}

func (h *Handler) resolveTenantWithOverride(r *http.Request, tenantOverride string) (string, error) {
	if tid := r.URL.Query().Get("tenant_id"); tid != "" {
		return tid, nil
	}
	name := tenantOverride
	if name == "" {
		name = h.TenantName
	}
	if name == "" {
		name = "default"
	}
	return h.OIDC.Store.GetTenantIDByName(r.Context(), name)
}

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package admin

import (
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pulsys-io/pulsys/internal/admin/api"
	"github.com/pulsys-io/pulsys/internal/admin/audit"
	adminstore "github.com/pulsys-io/pulsys/internal/admin/store"
	"github.com/pulsys-io/pulsys/internal/auth"
	authhttpx "github.com/pulsys-io/pulsys/internal/auth/httpx"
	"github.com/pulsys-io/pulsys/internal/auth/oidc"
	authstore "github.com/pulsys-io/pulsys/internal/auth/store"
	"github.com/pulsys-io/pulsys/internal/cache"
	"github.com/pulsys-io/pulsys/internal/db"
	"github.com/pulsys-io/pulsys/internal/observability"
	"github.com/riverqueue/river"
)

// Config wires the admin HTTP surface (auth + /admin/api/v1).
type Config struct {
	Pool          *pgxpool.Pool
	DBPool        *db.Pool // optional wrapper for /healthz
	CacheDir      string
	Cache         *cache.Store
	TenantName    string
	SecureCookies bool
	Metrics       *observability.Registry

	// PATCache, when set, is the data-plane PAT validation
	// cache (production: *auth.PATGate).  The admin token-
	// revoke handler invalidates the entry for the revoked
	// token's hash so revocation takes effect on the local
	// proxy in the same request -- without this, the gate
	// continues to admit the revoked token for up to its
	// PositiveTTL (60s default).  Nil-safe (omitted in dev
	// mode and in tests that don't exercise the data plane).
	PATCache api.PATCacheInvalidator

	// RiverClient enqueues and lists cache-import jobs. Nil disables /imports.
	RiverClient *river.Client[pgx.Tx]
}

// NewHandler returns an http.Handler serving /auth/* (public) and
// /admin/api/v1/* (authenticated).  Mount at / or compose via CombinedHandler.
func NewHandler(cfg Config) http.Handler {
	pgAuth := authstore.NewPG(cfg.Pool)
	adminSt := adminstore.NewAdminStore(cfg.Pool)
	if cfg.RiverClient != nil {
		adminSt.SetRiverClient(cfg.RiverClient)
	}
	oidcSvc := &oidc.Service{Store: pgAuth}
	authHTTP := &authhttpx.Handler{
		OIDC:          oidcSvc,
		TenantName:    cfg.TenantName,
		SecureCookies: cfg.SecureCookies,
	}
	apiH := &api.Handler{
		Store:    adminSt,
		CacheDir: cfg.CacheDir,
		Cache:    cfg.Cache,
		PATCache: cfg.PATCache,
	}
	authn := &auth.Authenticator{Store: pgAuth}
	auditMW := &audit.Middleware{
		Store:        adminSt,
		TenantName:   cfg.TenantName,
		TenantLookup: pgAuth,
	}

	root := http.NewServeMux()
	authMux := http.NewServeMux()
	authHTTP.Mount(authMux)
	root.Handle("/auth/", authn.Middleware(auth.CSRFProtect(auditMW.Wrap(authMux))))

	adminMux := http.NewServeMux()
	apiH.Mount(adminMux)
	root.Handle("/admin/", authn.Middleware(auth.CSRFProtect(requireAuthenticated(auditMW.Wrap(adminMux)))))

	if cfg.Metrics != nil {
		root.Handle("GET /metrics", cfg.Metrics.Handler())
	}
	root.HandleFunc("GET /healthz", observability.HealthHandler(cfg.DBPool))

	return securityHeaders(root)
}

// securityHeaders is the response-side defense-in-depth
// middleware applied to the entire admin surface (auth + admin
// API + metrics + healthz).  It sets the headers that all
// modern browsers and security scanners (Burp, OWASP ZAP) ask
// for, on every response, regardless of status code.
//
// Headers set:
//
//	X-Content-Type-Options: nosniff
//	  -- already set on JSON paths via writeJSON; this is the
//	     fallback for paths that bypass writeJSON (notFound,
//	     http.Error, metrics, healthz).
//
//	X-Frame-Options: DENY
//	  -- prevents the admin SPA from being framed by any other
//	     site (click-jacking defense).  DENY is stricter than
//	     SAMEORIGIN and is correct because we never legitimately
//	     embed admin pages inside other pages.
//
//	Referrer-Policy: no-referrer
//	  -- a Pulsys admin page that links out to an upstream Hub
//	     page must not leak the admin URL (which may contain
//	     tenant slugs) in the Referer header.
//
//	Cross-Origin-Opener-Policy: same-origin
//	  -- isolates the admin SPA browsing context from
//	     cross-origin popups (Specter-class defense).
//
//	Cross-Origin-Resource-Policy: same-origin
//	  -- prevents other origins from embedding our resources
//	     (font/script/img sniffing defense).
//
//	Permissions-Policy: <restrictive>
//	  -- denies the admin SPA access to camera, microphone,
//	     geolocation, payment, USB; none are needed.
//
// We deliberately DO NOT set:
//
//	Strict-Transport-Security
//	  -- HSTS is the LB's responsibility.  The data plane and
//	     admin port may run over plain HTTP behind the LB; us
//	     asserting HSTS would either be a lie (we can't see if
//	     the client used TLS) or duplicative.  See
//	     docs/security.md.
//
//	Content-Security-Policy
//	  -- CSP for the JSON admin API has no effect (browsers
//	     don't execute scripts in JSON responses with proper
//	     Content-Type).  The admin-ui SPA sets its own CSP via
//	     the Next.js framework.  Setting a server-side CSP here
//	     would risk breaking the SPA's own policy without
//	     adding defense.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		next.ServeHTTP(w, r)
	})
}

// CombinedHandler serves the Pulsys admin surface (/auth/*,
// /admin/*, /metrics, /healthz) and refuses every other path with a
// deterministic 404.  It is mounted on the admin listener
// (cfg.AdminListen, default 127.0.0.1:6060).
//
// SECURITY: this handler does NOT mount http.DefaultServeMux any
// more.  Previously a blank-import of "net/http/pprof" and "expvar"
// in cmd/pulsys/main.go silently registered /debug/pprof/* and
// /debug/vars on the default mux, which CombinedHandler then
// exposed unauthenticated.  pprof now ships behind an OPT-IN
// -pprof-listen flag on a separate listener (see
// docs/security.md).
func CombinedHandler(cfg Config) http.Handler {
	pulsys := NewHandler(cfg)
	mux := http.NewServeMux()
	mux.Handle("/auth/", pulsys)
	mux.Handle("/admin/", pulsys)
	mux.Handle("/metrics", pulsys)
	mux.Handle("/healthz", pulsys)
	// Default 404 for every other path -- closed-by-default
	// surface so forced-browsing probes don't get oracle signals
	// from inconsistent error shapes.
	mux.HandleFunc("/", notFound)
	return mux
}

// notFound is the deterministic 404 every unmounted path on the
// admin listener returns.  Body shape is intentionally identical
// across paths so a path-existence oracle cannot infer which
// routes are mounted vs which simply do not match.
func notFound(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":"not_found"}`))
}

func requireAuthenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor := auth.ActorFromContext(r.Context())
		if actor.Type == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

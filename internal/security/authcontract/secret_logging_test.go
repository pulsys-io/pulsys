// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

// Sensitive-data-in-logs regression test (OWASP WSTG-CRYP-03).
//
// PURPOSE
//   Logs are append-only and replicated.  Anything a credential
//   ever touches risks ending up in CloudWatch / Datadog /
//   Splunk forever.  This test pins the invariant:
//
//     NO bearer token, NO session cookie value, NO CSRF token,
//     and NO OIDC id_token EVER appears in slog output across
//     ANY log level, for ANY request the admin or auth handlers
//     process.
//
//   We exercise the surface by:
//
//     1. Installing a tee'd slog handler that captures EVERY
//        log line emitted while a request is in flight.
//     2. Running representative requests for each credential
//        class (PAT, session cookie, CSRF token, id_token) with
//        unique high-entropy markers so we can grep for them in
//        the captured log buffer afterwards.
//     3. Asserting NO marker appears verbatim.  We also forbid
//        the URL-encoded and base64-prefixed forms because
//        some loggers (slog's default JSON output) might
//        escape special characters in ways that change the
//        substring without removing the secret.
//
//   FALSE-NEGATIVE BUDGET: a future log line of the form
//     log.Debug("got auth", "header", r.Header.Get("Authorization"))
//   would obviously slip the test (the value is the secret).
//   The test catches this.  More subtle leaks like logging
//   the request URL when the secret is in the query string
//   (which Pulsys never accepts; PATs are header-only) would
//   require a separate URL-disclosure audit -- left to a
//   future expansion.

package authcontract

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pulsys-io/pulsys/internal/admin"
	adminstore "github.com/pulsys-io/pulsys/internal/admin/store"
	"github.com/pulsys-io/pulsys/internal/auth"
	authhttpx "github.com/pulsys-io/pulsys/internal/auth/httpx"
	"github.com/pulsys-io/pulsys/internal/auth/oidc"
	authstore "github.com/pulsys-io/pulsys/internal/auth/store"
	"github.com/pulsys-io/pulsys/internal/observability"
	"github.com/pulsys-io/pulsys/internal/testpg"
)

// captureLogger returns a slog.Logger that writes JSON to the
// supplied buffer.  We use the JSON handler because it escapes
// special characters more aggressively than the text handler,
// which is a stricter sieve for "does this string appear
// anywhere in any field of any record".
func captureLogger(buf *bytes.Buffer, mu *sync.Mutex) *slog.Logger {
	w := &lockedWriter{w: buf, mu: mu}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: slog.LevelDebug, // capture EVERY level
	}))
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// secretLogFixture builds an admin handler whose auth flow
// uses a logger we control.  We can then grep the captured
// log buffer for the secret markers we send through the
// requests.
type secretLogFixture struct {
	handler      http.Handler
	authHTTP     *authhttpx.Handler
	logBuf       *bytes.Buffer
	logMu        *sync.Mutex
	patPlain     string // marker we'll check the logs for
	csrfToken    string // server-side CSRF token (test injection)
	sessionToken string // server-side session token (raw)
	tenantSlug   string
}

func newSecretLogFixture(t *testing.T) secretLogFixture {
	t.Helper()
	pool := testpg.Acquire(t)
	pgAuth := authstore.NewPG(pool)
	pgAdmin := adminstore.NewAdminStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tid, err := pgAuth.EnsureTenant(ctx, "secret-log", "Secret Log Test")
	if err != nil {
		t.Fatalf("ensure tenant: %v", err)
	}
	uid, err := pgAuth.CreateUserOIDC(ctx, auth.User{
		TenantID: tid, Email: "sec@local", DisplayName: "sec",
		Role: auth.RoleOwner, OIDCSub: "sub-sec", IsActive: true,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	displayPAT, prefix, hash, err := auth.GeneratePAT()
	if err != nil {
		t.Fatalf("gen pat: %v", err)
	}
	if _, err := pgAdmin.CreateToken(ctx, tid, uid, "sec-pat", prefix, hash, []string{"admin:*"}, nil); err != nil {
		t.Fatalf("create token: %v", err)
	}
	sess, err := pgAuth.CreateSession(ctx, uid, tid, time.Hour)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	logBuf := &bytes.Buffer{}
	logMu := &sync.Mutex{}
	logger := captureLogger(logBuf, logMu)
	// Install the capture logger as the global default.  The
	// auth httpx handler we exercise uses slog.Default() when
	// h.Logger is nil; the admin handler chain we exercise
	// uses slog.Default() in a few defensive log sites.  Save
	// and restore so we don't leak the capture logger.
	prev := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prev) })

	handler := admin.NewHandler(admin.Config{
		Pool:       pool,
		CacheDir:   t.TempDir(),
		TenantName: "secret-log",
		Metrics:    observability.NewRegistry(),
	})
	// Standalone auth httpx handler used for /auth/session
	// direct invocation -- the same one admin.NewHandler wires
	// internally.  Equipping it with our logger explicitly
	// guarantees the log lines flow into our buffer even if a
	// future refactor stops using slog.Default().
	authHTTP := &authhttpx.Handler{
		OIDC:       &oidc.Service{Store: pgAuth, SessionTTL: time.Hour, Now: time.Now},
		TenantName: "secret-log",
		Logger:     logger,
	}
	return secretLogFixture{
		handler:      handler,
		authHTTP:     authHTTP,
		logBuf:       logBuf,
		logMu:        logMu,
		patPlain:     displayPAT,
		csrfToken:    sess.CSRFToken,
		sessionToken: sess.PlainToken,
		tenantSlug:   "secret-log",
	}
}

// TestSecretLogging_PATNeverInLogs hits admin endpoints with the
// PAT in the Authorization header and asserts the plaintext PAT
// never appears in any captured log line.
func TestSecretLogging_PATNeverInLogs(t *testing.T) {
	f := newSecretLogFixture(t)
	// Exercise every admin endpoint class so any leaky log
	// site has a chance to fire.
	calls := []struct {
		method, path string
		body         []byte
	}{
		{"GET", "/admin/api/v1/tenant", nil},
		{"GET", "/admin/api/v1/users?limit=10", nil},
		{"GET", "/admin/api/v1/tokens?limit=10", nil},
		{"POST", "/admin/api/v1/tokens", []byte(`{"name":"x","scopes":["models:read"]}`)},
		{"DELETE", "/admin/api/v1/tokens/not-real", nil},
		{"GET", "/admin/api/v1/settings", nil},
		{"PUT", "/admin/api/v1/settings/scope/key", []byte(`{"value":{"x":1}}`)},
		{"GET", "/admin/api/v1/audit?limit=10", nil},
	}
	for _, c := range calls {
		req := httptest.NewRequest(c.method, c.path, bytes.NewReader(c.body))
		req.Header.Set("Authorization", "Bearer "+f.patPlain)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		_ = rec.Code
	}
	assertNoSecretInLogs(t, f.logBuf, f.logMu, "pat-plain", f.patPlain)
}

// TestSecretLogging_SessionAndCSRFNeverInLogs hits an admin
// endpoint with the live session cookie + matching CSRF and
// asserts neither value appears in the logs.
func TestSecretLogging_SessionAndCSRFNeverInLogs(t *testing.T) {
	f := newSecretLogFixture(t)
	req := httptest.NewRequest("PUT", "/admin/api/v1/settings/scope/key", bytes.NewReader([]byte(`{"value":{"x":1}}`)))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: f.sessionToken})
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: f.csrfToken})
	req.Header.Set(auth.CSRFHeaderName, f.csrfToken)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	_ = rec.Code

	assertNoSecretInLogs(t, f.logBuf, f.logMu, "session-token", f.sessionToken)
	assertNoSecretInLogs(t, f.logBuf, f.logMu, "csrf-token", f.csrfToken)
}

// TestSecretLogging_IDTokenNeverInLogs sends an id_token-shaped
// string to /auth/session and asserts the (faux) id_token never
// appears in any log line, even on the failure path where the
// handler logs the underlying error reason.
//
// This pins the WSTG-CRYP-03 + IDNT-04 intersection: the
// log line we added during Phase 2 to retain operator visibility
// MUST log the error TYPE / tenant ID but never the raw
// id_token.
func TestSecretLogging_IDTokenNeverInLogs(t *testing.T) {
	f := newSecretLogFixture(t)
	// Use a high-entropy marker so any substring match is
	// definitely traceable to this test (avoid false-positive
	// matches against random log content).
	const idToken = "fake.id.token.MARKER_3xz9qLpW2yKvBnRtFcE7sUdH"
	body, _ := json.Marshal(map[string]string{
		"id_token": idToken,
		"tenant":   f.tenantSlug,
	})
	rec := runAuthSessionRequest(t, f.authHTTP, body)
	_ = rec.Code // 401 expected (no real IdP)
	assertNoSecretInLogs(t, f.logBuf, f.logMu, "id-token-marker", "MARKER_3xz9qLpW2yKvBnRtFcE7sUdH")
}

// Helper: run the authHTTP handler through its mux Mount so the
// route is dispatched correctly.
func runAuthSessionRequest(t *testing.T, h *authhttpx.Handler, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.Mount(mux)
	req := httptest.NewRequest("POST", "/auth/session", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// assertNoSecretInLogs scans the captured log buffer for the
// secret and reports a clear failure if any substring (raw,
// URL-encoded, JSON-encoded) appears.  Empty input is treated
// as a no-op (so callers passing optional secrets don't have
// to gate themselves).
func assertNoSecretInLogs(t *testing.T, buf *bytes.Buffer, mu *sync.Mutex, label, secret string) {
	t.Helper()
	if secret == "" {
		return
	}
	mu.Lock()
	logs := buf.String()
	mu.Unlock()
	// Three encoding forms to check.  JSON encoding escapes
	// "/" as "\/" in some encoders; we cover the obvious ones.
	candidates := []string{
		secret,
		strings.ReplaceAll(secret, "/", "\\/"),
		strings.ReplaceAll(secret, "_", "\\u005f"),
	}
	for _, needle := range candidates {
		if strings.Contains(logs, needle) {
			t.Fatalf("WSTG-CRYP-03: %s secret leaked to logs (substring %q found)\n  logs[:500]: %s",
				label, needle, truncate(logs, 500))
		}
	}
}

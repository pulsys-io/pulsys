// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package audit

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"

	"github.com/pulsys-io/pulsys/internal/auth"
)

type TenantResolver interface {
	GetTenantIDByName(ctx context.Context, name string) (string, error)
}

// AuditStore appends rows to audit_log.
type AuditStore interface {
	InsertAudit(ctx context.Context, tenantID, actorType string, actorID *string, action, resource, outcome string, metadata json.RawMessage, clientIP, userAgent string) error
}

// Middleware records audit rows for mutating admin and auth requests.
type Middleware struct {
	Store        AuditStore
	TenantName   string
	TenantLookup TenantResolver
}

// Wrap returns a handler that logs mutations after the response status is known.
//
// Idempotency contract: handlers can mark a response as an
// idempotent replay by setting the response header
// "X-Pulsys-Idempotent-Replay: true".  When set, this middleware
// SKIPS audit emission for that request.  Used today by the
// token revoke handler so a CI pipeline retrying an interrupted
// DELETE doesn't produce a duplicate "token.revoke" audit row.
// The replay header is consumed before the response is flushed
// to the wire so it never reaches the client.
const idempotentReplayHeader = "X-Pulsys-Idempotent-Replay"

func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !shouldAudit(r) {
			next.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if rec.idempotent {
			return
		}
		m.record(r, rec.status)
	})
}

func shouldAudit(r *http.Request) bool {
	if r.Method == http.MethodDelete && r.URL.Path == "/admin/api/v1/models/cache" {
		return false // handler writes cache.purge audit row with purge stats
	}
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
	default:
		return false
	}
	p := r.URL.Path
	if strings.HasPrefix(p, "/admin/api/v1/") {
		return true
	}
	return p == "/auth/session" || p == "/auth/logout"
}

func (m *Middleware) record(r *http.Request, status int) {
	action, resource := mapAction(r.Method, r.URL.Path)
	if action == "" {
		return
	}
	actor := auth.ActorFromContext(r.Context())
	tenantID := actor.TenantID
	if tenantID == "" {
		tenantID = m.resolveTenant(r)
	}
	if tenantID == "" {
		return
	}
	outcome := outcomeForStatus(status)
	meta, _ := json.Marshal(map[string]any{
		"method": r.Method,
		"path":   r.URL.Path,
		"status": status,
	})
	actorType := string(actor.Type)
	if actorType == "" {
		actorType = "user"
	}
	_ = m.Store.InsertAudit(r.Context(), tenantID, actorType, actorIDPtr(actor), action, resource, outcome, meta, clientIP(r), r.UserAgent())
}

func (m *Middleware) resolveTenant(r *http.Request) string {
	if tid := r.URL.Query().Get("tenant_id"); tid != "" {
		return tid
	}
	name := m.TenantName
	if name == "" {
		name = "default"
	}
	if m.TenantLookup == nil {
		return ""
	}
	id, err := m.TenantLookup.GetTenantIDByName(r.Context(), name)
	if err != nil {
		return ""
	}
	return id
}

func mapAction(method, path string) (action, resource string) {
	switch {
	case method == http.MethodPost && path == "/admin/api/v1/tokens":
		return "token.create", "tokens"
	case method == http.MethodDelete && strings.HasPrefix(path, "/admin/api/v1/tokens/"):
		return "token.revoke", strings.TrimPrefix(path, "/admin/")
	case method == http.MethodPut && strings.HasPrefix(path, "/admin/api/v1/settings/"):
		return "settings.update", strings.TrimPrefix(path, "/admin/api/v1/settings/")
	case method == http.MethodPost && path == "/admin/api/v1/imports":
		return "import.create", "imports"
	case method == http.MethodPost && strings.HasPrefix(path, "/admin/api/v1/imports/") && strings.HasSuffix(path, "/cancel"):
		return "import.cancel", strings.TrimPrefix(path, "/admin/")
	case method == http.MethodDelete && strings.HasPrefix(path, "/admin/api/v1/imports/"):
		return "import.delete", strings.TrimPrefix(path, "/admin/")
	case method == http.MethodPost && path == "/auth/session":
		return "auth.session.create", "sessions"
	case method == http.MethodPost && path == "/auth/logout":
		return "auth.session.revoke", "sessions"
	default:
		if strings.HasPrefix(path, "/admin/api/v1/") {
			return "admin.mutate", strings.TrimPrefix(path, "/admin/")
		}
		return "", ""
	}
}

func outcomeForStatus(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "success"
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return "denied"
	default:
		return "failure"
	}
}

func actorIDPtr(a auth.Actor) *string {
	switch a.Type {
	case auth.ActorUser:
		if a.UserID != "" {
			return &a.UserID
		}
	case auth.ActorToken:
		if a.TokenID != "" {
			return &a.TokenID
		}
	}
	return nil
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type statusRecorder struct {
	http.ResponseWriter
	status     int
	idempotent bool
}

// WriteHeader captures the status, snapshots the idempotency
// header BEFORE it is flushed to the wire, then strips it so
// the client never sees the X-Pulsys-* internal signaling.
// Necessary because http.ResponseWriter doesn't let us mutate
// headers after WriteHeader returns.
func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	if r.Header().Get(idempotentReplayHeader) == "true" {
		r.idempotent = true
		r.Header().Del(idempotentReplayHeader)
	}
	r.ResponseWriter.WriteHeader(code)
}

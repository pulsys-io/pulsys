// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package auth

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Authenticator resolves an Actor from session cookies or bearer PATs.
type Authenticator struct {
	Store      Store
	SessionTTL time.Duration
}

// ActorFromRequest returns the authenticated Actor, optional session CSRF
// token (human sessions only), or a zero Actor.
func (a *Authenticator) ActorFromRequest(ctx context.Context, r *http.Request) (Actor, string, error) {
	if tok, ok := ParseBearer(r.Header.Get("Authorization")); ok {
		if IsPAT(tok) {
			actor, err := a.actorFromPAT(ctx, tok)
			return actor, "", err
		}
	}
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		return a.actorFromSession(ctx, c.Value)
	}
	return Actor{}, "", nil
}

func (a *Authenticator) actorFromSession(ctx context.Context, plain string) (Actor, string, error) {
	sess, user, err := a.Store.LookupSession(ctx, plain)
	if err != nil {
		return Actor{}, "", err
	}
	ttl := a.SessionTTL
	if ttl <= 0 {
		ttl = 8 * time.Hour
	}
	_ = a.Store.TouchSession(ctx, sess.ID, ttl)
	return Actor{
		Type:     ActorUser,
		TenantID: user.TenantID,
		UserID:   user.ID,
		Role:     user.Role,
		Email:    user.Email,
	}, sess.CSRFToken, nil
}

func (a *Authenticator) actorFromPAT(ctx context.Context, plain string) (Actor, error) {
	tok, err := a.Store.LookupAPIToken(ctx, plain)
	if err != nil {
		return Actor{}, err
	}
	return Actor{
		Type:     ActorToken,
		TenantID: tok.TenantID,
		TokenID:  tok.ID,
		UserID:   tok.UserID,
		Scopes:   tok.Scopes,
	}, nil
}

// Middleware attaches the resolved Actor to request context under
// ContextKeyActor.
//
// Policy:
//
//   - No credential presented (no Authorization header, no session
//     cookie):  attach a zero Actor and call next.  Anonymous endpoints
//     (OIDC login, healthz) rely on this; protected endpoints close
//     the gate via RequireRole / RequireScope.
//
//   - Credential presented but rejected by the store
//     (errors.Is(err, ErrInvalidSession), the only error
//     ActorFromRequest can return today): respond 401 with
//     WWW-Authenticate: Bearer error="invalid_token" and DO NOT call
//     next.  An explicit bad credential must never silently fall
//     through to anonymous treatment -- that was the bug behind the
//     2026-05-21 report where revoked PATs continued to work.
//
//   - Any other error (DB outage, etc.): respond 401.  Failing closed
//     on store errors is safer than allowing anonymous fallthrough
//     when the auth backend is unreachable.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, csrf, err := a.ActorFromRequest(r.Context(), r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			if errors.Is(err, ErrInvalidSession) {
				http.Error(w, "invalid or revoked credential", http.StatusUnauthorized)
			} else {
				http.Error(w, "authentication failed", http.StatusUnauthorized)
			}
			return
		}
		ctx := ContextWithActor(r.Context(), actor)
		if csrf != "" {
			ctx = ContextWithCSRF(ctx, csrf)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireRole wraps a handler and returns 403 when the actor lacks min role.
func RequireRole(min Role, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor := ActorFromContext(r.Context())
		if actor.Type != ActorUser || !actor.Role.AtLeast(min) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

type ctxKey int

const contextKeyActor ctxKey = 1
const contextKeyCSRF ctxKey = 2

// ContextWithCSRF stores the server-side session CSRF token in ctx.
func ContextWithCSRF(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, contextKeyCSRF, token)
}

// CSRFTokenFromContext returns the session CSRF token when present.
func CSRFTokenFromContext(ctx context.Context) string {
	v := ctx.Value(contextKeyCSRF)
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// ContextWithActor stores actor in ctx.
func ContextWithActor(ctx context.Context, actor Actor) context.Context {
	return context.WithValue(ctx, contextKeyActor, actor)
}

// ActorFromContext returns the Actor stored by Middleware, or zero value.
func ActorFromContext(ctx context.Context) Actor {
	v := ctx.Value(contextKeyActor)
	if v == nil {
		return Actor{}
	}
	a, _ := v.(Actor)
	return a
}

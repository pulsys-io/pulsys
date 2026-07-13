// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package api

import (
	"net/http"

	"github.com/pulsys-io/pulsys/internal/auth"
)

// requireAccess allows human users with at least minRole, or PAT actors
// carrying tokenScope (or admin:* / *).
func requireAccess(minRole auth.Role, tokenScope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor := auth.ActorFromContext(r.Context())
		if actor.Type == "" {
			writeError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		switch actor.Type {
		case auth.ActorUser:
			if !actor.Role.AtLeast(minRole) {
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
		case auth.ActorToken:
			if !actor.HasScope(tokenScope) && !actor.HasScope("admin:*") {
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
		default:
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}
		next(w, r)
	}
}

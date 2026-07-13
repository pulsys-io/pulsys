// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package auth

import "fmt"

// ActorType identifies how the request was authenticated.
type ActorType string

const (
	ActorUser   ActorType = "user"
	ActorToken  ActorType = "token"
	ActorSystem ActorType = "system"
)

// Role is a tenant-scoped RBAC label stored on users.role.
type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
	RoleReader Role = "reader"
)

// ParseRole validates a role string.
func ParseRole(s string) (Role, error) {
	switch Role(s) {
	case RoleOwner, RoleAdmin, RoleMember, RoleReader:
		return Role(s), nil
	default:
		return "", fmt.Errorf("auth: invalid role %q", s)
	}
}

// Rank returns a numeric ordering for role comparison (higher = more privilege).
func (r Role) Rank() int {
	switch r {
	case RoleOwner:
		return 4
	case RoleAdmin:
		return 3
	case RoleMember:
		return 2
	case RoleReader:
		return 1
	default:
		return 0
	}
}

// AtLeast reports whether r has at least the privilege of min.
func (r Role) AtLeast(min Role) bool {
	return r.Rank() >= min.Rank()
}

// Actor is the authenticated principal attached to a request context.
type Actor struct {
	Type     ActorType
	TenantID string
	UserID   string // set when Type == ActorUser
	TokenID  string // set when Type == ActorToken
	Role     Role   // set for users; tokens inherit scopes instead
	Scopes   []string
	Email    string // display / audit; users only
}

// IsHuman reports whether the actor represents an OIDC-backed user session.
func (a Actor) IsHuman() bool { return a.Type == ActorUser }

// HasScope reports whether the actor carries scope (tokens) or role (users).
// Human actors with admin/owner roles implicitly pass admin-scoped checks.
func (a Actor) HasScope(scope string) bool {
	if a.Type == "" {
		return false
	}
	if a.Type == ActorUser && a.Role.AtLeast(RoleAdmin) {
		return true
	}
	for _, s := range a.Scopes {
		if s == scope || s == "*" {
			return true
		}
	}
	return false
}

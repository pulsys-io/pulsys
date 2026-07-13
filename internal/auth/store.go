// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package auth

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// OIDCProvider is the tenant's external IdP configuration.
type OIDCProvider struct {
	ID                    string
	TenantID              string
	Issuer                string
	ClientID              string
	ClientSecret          string
	RedirectURI           string
	Scopes                string
	Enabled               bool
	GroupsClaim           string
	OwnerGroups           []string
	AdminGroups           []string
	JITDefaultRole        Role
	RequirePreprovisioned bool
}

// User is a tenant-scoped human identity (OIDC-only; no password).
type User struct {
	ID          string
	TenantID    string
	Email       string
	DisplayName string
	Role        Role
	OIDCSub     string
	IsActive    bool
}

// Session is a server-side human session.
type Session struct {
	ID         string
	UserID     string
	TenantID   string
	CSRFToken  string
	ExpiresAt  time.Time
	PlainToken string // only populated on create; never stored
}

// APIToken is a hashed bearer credential.
type APIToken struct {
	ID       string
	TenantID string
	UserID   string
	Prefix   string
	Hash     []byte
	Scopes   []string
}

// ErrNoOIDCProvider is returned when a tenant has no enabled IdP.
var ErrNoOIDCProvider = errors.New("auth: no OIDC provider configured for tenant")

// ErrLoginDenied is returned when JIT provisioning is disabled for unknown users.
var ErrLoginDenied = errors.New("auth: login denied; user not pre-provisioned")

// ErrInvalidSession is returned when a session cookie or token is invalid/expired.
var ErrInvalidSession = errors.New("auth: invalid or expired session")

// Store is the persistence contract for auth (Postgres in P3).
type Store interface {
	// Tenants
	EnsureTenant(ctx context.Context, name, displayName string) (tenantID string, err error)
	GetTenantIDByName(ctx context.Context, name string) (tenantID string, err error)

	// OIDC
	GetOIDCProviderByTenant(ctx context.Context, tenantID string) (*OIDCProvider, error)
	UpsertOIDCProvider(ctx context.Context, p OIDCProvider) error

	// Users (OIDC JIT; never touches password_hash)
	FindUserByOIDCSub(ctx context.Context, tenantID, oidcSub string) (*User, error)
	CreateUserOIDC(ctx context.Context, u User) (userID string, err error)
	UpdateUserProfile(ctx context.Context, userID, email, displayName string, role Role) error

	// Sessions
	CreateSession(ctx context.Context, userID, tenantID string, ttl time.Duration) (*Session, error)
	LookupSession(ctx context.Context, plainToken string) (*Session, *User, error)
	TouchSession(ctx context.Context, sessionID string, ttl time.Duration) error
	RevokeSession(ctx context.Context, plainToken string) error

	// API tokens
	LookupAPIToken(ctx context.Context, plainToken string) (*APIToken, error)

	// RLS helper: run fn inside a transaction with pulsys.tenant_id set.
	WithTenant(ctx context.Context, tenantID string, fn func(ctx context.Context) error) error
}

// ValidateProvider checks required OIDC provider fields.
func ValidateProvider(p OIDCProvider) error {
	if p.TenantID == "" {
		return fmt.Errorf("auth: empty tenant_id")
	}
	if p.Issuer == "" || p.ClientID == "" || p.ClientSecret == "" || p.RedirectURI == "" {
		return fmt.Errorf("auth: incomplete OIDC provider configuration")
	}
	if _, err := ParseRole(string(p.JITDefaultRole)); err != nil {
		return err
	}
	return nil
}

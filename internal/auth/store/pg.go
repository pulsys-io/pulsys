// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pulsys-io/pulsys/internal/auth"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PG implements auth.Store against Postgres.
type PG struct {
	Pool *pgxpool.Pool
}

func NewPG(pool *pgxpool.Pool) *PG {
	return &PG{Pool: pool}
}

func (s *PG) EnsureTenant(ctx context.Context, name, displayName string) (string, error) {
	var id string
	err := s.Pool.QueryRow(ctx,
		`SELECT id FROM tenants WHERE name = $1 AND deleted_at IS NULL`, name,
	).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	err = s.Pool.QueryRow(ctx, `
		INSERT INTO tenants(name, display_name) VALUES ($1, $2) RETURNING id
	`, name, displayName).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: ensure tenant: %w", err)
	}
	return id, nil
}

func (s *PG) GetTenantIDByName(ctx context.Context, name string) (string, error) {
	var id string
	err := s.Pool.QueryRow(ctx,
		`SELECT id FROM tenants WHERE name = $1 AND deleted_at IS NULL`, name,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("store: tenant %q not found", name)
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *PG) GetOIDCProviderByTenant(ctx context.Context, tenantID string) (*auth.OIDCProvider, error) {
	var p auth.OIDCProvider
	err := s.Pool.QueryRow(ctx, `
		SELECT id, tenant_id, issuer, client_id, client_secret, redirect_uri,
		       scopes, enabled, groups_claim, owner_groups, admin_groups,
		       jit_default_role, require_preprovisioned
		FROM oidc_providers
		WHERE tenant_id = $1 AND enabled = true
	`, tenantID).Scan(
		&p.ID, &p.TenantID, &p.Issuer, &p.ClientID, &p.ClientSecret, &p.RedirectURI,
		&p.Scopes, &p.Enabled, &p.GroupsClaim, &p.OwnerGroups, &p.AdminGroups,
		&p.JITDefaultRole, &p.RequirePreprovisioned,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, auth.ErrNoOIDCProvider
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *PG) UpsertOIDCProvider(ctx context.Context, p auth.OIDCProvider) error {
	if err := auth.ValidateProvider(p); err != nil {
		return err
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO oidc_providers (
			tenant_id, issuer, client_id, client_secret, redirect_uri, scopes,
			enabled, groups_claim, owner_groups, admin_groups,
			jit_default_role, require_preprovisioned
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (tenant_id) DO UPDATE SET
			issuer = EXCLUDED.issuer,
			client_id = EXCLUDED.client_id,
			client_secret = EXCLUDED.client_secret,
			redirect_uri = EXCLUDED.redirect_uri,
			scopes = EXCLUDED.scopes,
			enabled = EXCLUDED.enabled,
			groups_claim = EXCLUDED.groups_claim,
			owner_groups = EXCLUDED.owner_groups,
			admin_groups = EXCLUDED.admin_groups,
			jit_default_role = EXCLUDED.jit_default_role,
			require_preprovisioned = EXCLUDED.require_preprovisioned,
			updated_at = now()
	`, p.TenantID, p.Issuer, p.ClientID, p.ClientSecret, p.RedirectURI, p.Scopes,
		p.Enabled, p.GroupsClaim, p.OwnerGroups, p.AdminGroups,
		string(p.JITDefaultRole), p.RequirePreprovisioned)
	return err
}

func (s *PG) FindUserByOIDCSub(ctx context.Context, tenantID, oidcSub string) (*auth.User, error) {
	var u auth.User
	err := s.Pool.QueryRow(ctx, `
		SELECT id, tenant_id, email, display_name, role, oidc_sub, is_active
		FROM users
		WHERE tenant_id = $1 AND oidc_sub = $2 AND deleted_at IS NULL
	`, tenantID, oidcSub).Scan(
		&u.ID, &u.TenantID, &u.Email, &u.DisplayName, &u.Role, &u.OIDCSub, &u.IsActive,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *PG) CreateUserOIDC(ctx context.Context, u auth.User) (string, error) {
	var id string
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO users(tenant_id, email, display_name, role, oidc_sub, password_hash)
		VALUES ($1, $2, $3, $4, $5, NULL)
		RETURNING id
	`, u.TenantID, u.Email, u.DisplayName, string(u.Role), u.OIDCSub).Scan(&id)
	return id, err
}

func (s *PG) UpdateUserProfile(ctx context.Context, userID, email, displayName string, role auth.Role) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE users
		SET email = $2, display_name = $3, role = $4, updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL
	`, userID, email, displayName, string(role))
	return err
}

// GrantOwner is the audited break-glass recovery operation: it promotes an
// existing tenant user to the owner role (matched by email OR oidc_sub) and
// reactivates them, then records a system audit_log row in the same
// transaction so the change is never silent.  Exactly one of email/oidcSub
// must be non-empty.  It exists so an operator with database access can
// recover owner access after an OIDC misconfiguration locks everyone out,
// without bypassing the audit trail.  It returns the affected user id and the
// role the user held before promotion.
func (s *PG) GrantOwner(ctx context.Context, tenantID, email, oidcSub string) (userID string, prevRole auth.Role, err error) {
	email = strings.TrimSpace(email)
	oidcSub = strings.TrimSpace(oidcSub)
	if (email == "") == (oidcSub == "") {
		return "", "", fmt.Errorf("store: grant owner: exactly one of email or oidc_sub must be set")
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Set the RLS tenant scope so the audit_log insert policy is satisfied.
	if _, err := tx.Exec(ctx, `SET LOCAL pulsys.tenant_id = $1`, tenantID); err != nil {
		return "", "", err
	}

	var (
		selector string
		arg      string
	)
	if email != "" {
		selector = "email = $2"
		arg = email
	} else {
		selector = "oidc_sub = $2"
		arg = oidcSub
	}
	err = tx.QueryRow(ctx, `
		SELECT id, role FROM users
		WHERE tenant_id = $1 AND `+selector+` AND deleted_at IS NULL
	`, tenantID, arg).Scan(&userID, &prevRole)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("store: grant owner: no matching user in tenant")
	}
	if err != nil {
		return "", "", err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE users
		SET role = $2, is_active = true, updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL
	`, userID, string(auth.RoleOwner)); err != nil {
		return "", "", err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_log (tenant_id, actor_type, action, resource, outcome, metadata)
		VALUES ($1, 'system', 'user.grant_owner', $2, 'success', $3::jsonb)
	`, tenantID, userID, fmt.Sprintf(
		`{"previous_role":%q,"selector":%q,"via":"pulsys-db break-glass"}`,
		string(prevRole), selectorLabel(email, oidcSub),
	)); err != nil {
		return "", "", err
	}

	if err := tx.Commit(ctx); err != nil {
		return "", "", err
	}
	return userID, prevRole, nil
}

func selectorLabel(email, oidcSub string) string {
	if email != "" {
		return "email:" + email
	}
	return "oidc_sub:" + oidcSub
}

func (s *PG) CreateSession(ctx context.Context, userID, tenantID string, ttl time.Duration) (*auth.Session, error) {
	plain, err := auth.RandomToken(32)
	if err != nil {
		return nil, err
	}
	csrf, err := auth.RandomToken(16)
	if err != nil {
		return nil, err
	}
	expires := time.Now().UTC().Add(ttl)
	var id string
	err = s.Pool.QueryRow(ctx, `
		INSERT INTO sessions(token_hash, user_id, tenant_id, csrf_token, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id
	`, auth.TokenHash(plain), userID, tenantID, csrf, expires).Scan(&id)
	if err != nil {
		return nil, err
	}
	return &auth.Session{
		ID:         id,
		UserID:     userID,
		TenantID:   tenantID,
		CSRFToken:  csrf,
		ExpiresAt:  expires,
		PlainToken: plain,
	}, nil
}

func (s *PG) LookupSession(ctx context.Context, plainToken string) (*auth.Session, *auth.User, error) {
	hash := auth.TokenHash(plainToken)
	var sess auth.Session
	err := s.Pool.QueryRow(ctx, `
		SELECT s.id, s.user_id, s.tenant_id, s.csrf_token, s.expires_at
		FROM sessions s
		WHERE s.token_hash = $1 AND s.revoked_at IS NULL AND s.expires_at > now()
	`, hash).Scan(&sess.ID, &sess.UserID, &sess.TenantID, &sess.CSRFToken, &sess.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, auth.ErrInvalidSession
	}
	if err != nil {
		return nil, nil, err
	}
	var u auth.User
	err = s.Pool.QueryRow(ctx, `
		SELECT id, tenant_id, email, display_name, role, oidc_sub, is_active
		FROM users WHERE id = $1 AND deleted_at IS NULL
	`, sess.UserID).Scan(&u.ID, &u.TenantID, &u.Email, &u.DisplayName, &u.Role, &u.OIDCSub, &u.IsActive)
	if err != nil {
		return nil, nil, err
	}
	if !u.IsActive {
		return nil, nil, auth.ErrInvalidSession
	}
	return &sess, &u, nil
}

func (s *PG) TouchSession(ctx context.Context, sessionID string, ttl time.Duration) error {
	expires := time.Now().UTC().Add(ttl)
	_, err := s.Pool.Exec(ctx, `
		UPDATE sessions SET last_seen_at = now(), expires_at = $2
		WHERE id = $1 AND revoked_at IS NULL
	`, sessionID, expires)
	return err
}

func (s *PG) RevokeSession(ctx context.Context, plainToken string) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE sessions SET revoked_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL
	`, auth.TokenHash(plainToken))
	return err
}

func (s *PG) LookupAPIToken(ctx context.Context, plainToken string) (*auth.APIToken, error) {
	if !auth.IsPAT(plainToken) {
		return nil, auth.ErrInvalidSession
	}
	hash := auth.TokenHash(plainToken)
	var t auth.APIToken
	var owner *string
	err := s.Pool.QueryRow(ctx, `
		SELECT id, tenant_id, owner_user_id, prefix, hash, scopes
		FROM tokens
		WHERE hash = $1 AND deleted_at IS NULL AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > now())
	`, hash).Scan(&t.ID, &t.TenantID, &owner, &t.Prefix, &t.Hash, &t.Scopes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, auth.ErrInvalidSession
	}
	if err != nil {
		return nil, err
	}
	if owner != nil {
		t.UserID = *owner
	}
	return &t, nil
}

func (s *PG) WithTenant(ctx context.Context, tenantID string, fn func(ctx context.Context) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL pulsys.tenant_id = $1`, tenantID); err != nil {
		return err
	}
	if err := fn(ctx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

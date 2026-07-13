// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// Tenant row for admin API.
type Tenant struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
}

// User row for admin API (no password fields).
type User struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role"`
	IsActive    bool      `json:"is_active"`
	CreatedAt   time.Time `json:"created_at"`
}

// Token row for admin API (never exposes hash).
type Token struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scopes     []string   `json:"scopes"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// TokenCreateResult includes the one-time plaintext secret.
type TokenCreateResult struct {
	Token
	Secret string `json:"secret"`
}

// Setting row for admin API.
type Setting struct {
	Scope     string          `json:"scope"`
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	Version   int64           `json:"version"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// AuditEntry for admin API.
type AuditEntry struct {
	ID         string          `json:"id"`
	ActorType  string          `json:"actor_type"`
	ActorID    *string         `json:"actor_id,omitempty"`
	Action     string          `json:"action"`
	Resource   *string         `json:"resource,omitempty"`
	Outcome    string          `json:"outcome"`
	Metadata   json.RawMessage `json:"metadata"`
	OccurredAt time.Time       `json:"occurred_at"`
}

// AdminStore is the Postgres-backed admin query surface.
type AdminStore struct {
	Pool  *pgxpool.Pool
	river *river.Client[pgx.Tx]
}

func NewAdminStore(pool *pgxpool.Pool) *AdminStore {
	return &AdminStore{Pool: pool}
}

// GetTenant returns a tenant by id.
func (s *AdminStore) GetTenant(ctx context.Context, tenantID string) (*Tenant, error) {
	var t Tenant
	err := s.Pool.QueryRow(ctx, `
		SELECT id, name, display_name, created_at
		FROM tenants WHERE id = $1 AND deleted_at IS NULL
	`, tenantID).Scan(&t.ID, &t.Name, &t.DisplayName, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("tenant not found")
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ListUsers lists active users for a tenant.
func (s *AdminStore) ListUsers(ctx context.Context, tenantID string, limit int) ([]User, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, email, display_name, role, is_active, created_at
		FROM users
		WHERE tenant_id = $1 AND deleted_at IS NULL
		ORDER BY created_at DESC
		LIMIT $2
	`, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &u.IsActive, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListTokens lists API tokens for a tenant.
func (s *AdminStore) ListTokens(ctx context.Context, tenantID string, limit int) ([]Token, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, name, prefix, scopes, last_used_at, expires_at, created_at, revoked_at
		FROM tokens
		WHERE tenant_id = $1 AND deleted_at IS NULL
		ORDER BY created_at DESC
		LIMIT $2
	`, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.Name, &t.Prefix, &t.Scopes, &t.LastUsedAt, &t.ExpiresAt, &t.CreatedAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateToken inserts a new API token and returns the one-time secret.
func (s *AdminStore) CreateToken(ctx context.Context, tenantID, ownerUserID, name, prefix string, hash []byte, scopes []string, expiresAt *time.Time) (*TokenCreateResult, error) {
	var res TokenCreateResult
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO tokens(tenant_id, owner_user_id, name, prefix, hash, scopes, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, name, prefix, scopes, created_at
	`, tenantID, nullUUID(ownerUserID), name, prefix, hash, scopes, expiresAt).Scan(
		&res.ID, &res.Name, &res.Prefix, &res.Scopes, &res.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

func nullUUID(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// RevokeToken sets revoked_at on a token owned by the tenant.
//
// Returns:
//
//   - hash:        the stored sha256 hash of the revoked PAT, so
//     the caller can evict it from any in-process
//     validation cache (see PATGate.InvalidateByHash).
//     Always populated on err == nil, even on the
//     already-revoked branch where alreadyRevoked
//     is true (idempotent path).
//   - alreadyRevoked: true if the token was already in the
//     revoked state.  Lets the HTTP handler return
//     a 204 / 200 No-Op response (enterprise retry
//     safety) instead of a 404 that would emit a
//     spurious "outcome=failure" audit row on every
//     legitimate retry of an interrupted DELETE.
//   - err == ErrNotFound when the token id/tenant pair has no
//     matching row at all.
//
// The hash is read under the same statement that performs the
// UPDATE (or the SELECT on the already-revoked branch) so there
// is no TOCTOU window where the row could be deleted between
// our lookup and the revoke.
func (s *AdminStore) RevokeToken(ctx context.Context, tenantID, tokenID string) (hash []byte, alreadyRevoked bool, err error) {
	// First try the UPDATE -- only matches if the token exists
	// and is currently active.  The RETURNING clause hands us
	// back the hash without a second roundtrip.
	err = s.Pool.QueryRow(ctx, `
		UPDATE tokens
		   SET revoked_at = now()
		 WHERE id = $1
		   AND tenant_id = $2
		   AND deleted_at IS NULL
		   AND revoked_at IS NULL
		RETURNING hash
	`, tokenID, tenantID).Scan(&hash)
	if err == nil {
		return hash, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	// No active row matched.  Either the token is already
	// revoked (idempotent retry) or the id doesn't exist for
	// this tenant.  Disambiguate via SELECT so the caller can
	// return the right HTTP code.
	err = s.Pool.QueryRow(ctx, `
		SELECT hash
		  FROM tokens
		 WHERE id = $1
		   AND tenant_id = $2
		   AND deleted_at IS NULL
	`, tokenID, tenantID).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, ErrTokenNotFound
	}
	if err != nil {
		return nil, false, err
	}
	return hash, true, nil
}

// ErrTokenNotFound disambiguates "no such token id/tenant pair"
// from the idempotent "already revoked" branch in RevokeToken.
// Callers can errors.Is against this to map to a 404 response
// without leaking the underlying pgx error.
var ErrTokenNotFound = errors.New("token not found")

// ListSettings returns settings for a tenant, optionally filtered by scope.
func (s *AdminStore) ListSettings(ctx context.Context, tenantID, scope string) ([]Setting, error) {
	var rows pgx.Rows
	var err error
	if scope != "" {
		rows, err = s.Pool.Query(ctx, `
			SELECT scope, key, value, version, updated_at
			FROM settings WHERE tenant_id = $1 AND scope = $2
			ORDER BY scope, key
		`, tenantID, scope)
	} else {
		rows, err = s.Pool.Query(ctx, `
			SELECT scope, key, value, version, updated_at
			FROM settings WHERE tenant_id = $1
			ORDER BY scope, key
		`, tenantID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Setting
	for rows.Next() {
		var st Setting
		if err := rows.Scan(&st.Scope, &st.Key, &st.Value, &st.Version, &st.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// UpsertSetting writes a setting with strict optimistic
// concurrency.
//
// Contract:
//
//   - expectVersion == 0  =>  caller is asserting "create this
//     key for the first time".  We
//     INSERT, returning ErrSettingConflict
//     if the key already exists (so the
//     caller is forced to re-read and
//     decide whether to retry as an
//     update).
//   - expectVersion >= 1  =>  caller is asserting "I read version
//     N, replace it".  We UPDATE WHERE
//     version = N, returning
//     ErrSettingConflict on a mismatch.
//     A mismatch means either the row
//     doesn't exist OR another writer
//     already bumped the version.
//
// The pre-Phase-5 implementation treated expectVersion == 0 as
// "upsert without version check" (INSERT ... ON CONFLICT DO
// UPDATE), which silently overwrote any existing row.  Two
// concurrent admins both PUTing without version => last writer
// wins, no warning, no audit signal of the conflict.  WSTG
// BUSL-03 fails: enterprise expectation of "I read X then wrote
// X+1" is silently violated.  Phase 5 closes that by making
// every UPDATE explicit.
func (s *AdminStore) UpsertSetting(ctx context.Context, tenantID, scope, key string, value json.RawMessage, expectVersion int64, updatedBy string) (*Setting, error) {
	if expectVersion < 0 {
		return nil, fmt.Errorf("expectVersion must be >= 0")
	}
	if expectVersion >= 1 {
		var st Setting
		err := s.Pool.QueryRow(ctx, `
			UPDATE settings
			SET value = $5, version = version + 1, updated_at = now(), updated_by = $6
			WHERE tenant_id = $1 AND scope = $2 AND key = $3 AND version = $4
			RETURNING scope, key, value, version, updated_at
		`, tenantID, scope, key, expectVersion, value, nullUUID(updatedBy)).Scan(
			&st.Scope, &st.Key, &st.Value, &st.Version, &st.UpdatedAt,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSettingConflict
		}
		if err != nil {
			return nil, err
		}
		return &st, nil
	}
	// expectVersion == 0: pure INSERT.  Refuse on conflict so
	// the caller has to read-then-update with a real version.
	var st Setting
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO settings(tenant_id, scope, key, value, updated_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING scope, key, value, version, updated_at
	`, tenantID, scope, key, value, nullUUID(updatedBy)).Scan(
		&st.Scope, &st.Key, &st.Value, &st.Version, &st.UpdatedAt,
	)
	if err != nil {
		// 23505 = unique_violation -- key already exists.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrSettingConflict
		}
		return nil, err
	}
	return &st, nil
}

// ErrSettingConflict is returned when an UpsertSetting call
// loses the optimistic concurrency race (either the row already
// exists on a version=0 create, or the version field doesn't
// match on a version>=1 update).  Maps to HTTP 409 Conflict at
// the handler.
var ErrSettingConflict = errors.New("setting version conflict")

// ListAudit returns recent audit entries for a tenant.
func (s *AdminStore) ListAudit(ctx context.Context, tenantID string, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, actor_type, actor_id, action, resource, outcome, metadata, occurred_at
		FROM audit_log
		WHERE tenant_id = $1
		ORDER BY occurred_at DESC
		LIMIT $2
	`, tenantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.ActorType, &e.ActorID, &e.Action, &e.Resource, &e.Outcome, &e.Metadata, &e.OccurredAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// InsertAudit appends an audit log entry.
func (s *AdminStore) InsertAudit(ctx context.Context, tenantID, actorType string, actorID *string, action, resource, outcome string, metadata json.RawMessage, clientIP, userAgent string) error {
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	var ip any
	if clientIP != "" {
		ip = clientIP
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO audit_log(tenant_id, actor_type, actor_id, action, resource, outcome, metadata, ip, user_agent)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, tenantID, actorType, actorID, action, nullString(resource), outcome, metadata, ip, nullString(userAgent))
	return err
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

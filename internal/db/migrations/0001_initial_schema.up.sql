-- 0001_initial_schema.up.sql
--
-- Pulsys initial Postgres schema.
--
-- Conventions encoded in this migration:
--   * Every row uses a uuid PK from gen_random_uuid().
--   * All timestamps are TIMESTAMPTZ in UTC.
--   * Soft delete via deleted_at on tenants, users, tokens.
--   * Hard insert-only for audit_log (RLS lands in P3).
--   * Every user-facing row carries tenant_id; tenancy
--     enforcement code lands in P3 along with RLS policies.

BEGIN;

-- citext gives us case-insensitive comparison for email columns
-- without forcing application-layer lower() everywhere.  Bundled
-- with Postgres since 9.1; available in RDS + Aurora.
CREATE EXTENSION IF NOT EXISTS "citext";

-- ---------------------------------------------------------------------------
-- tenants
-- ---------------------------------------------------------------------------
-- Top-level isolation unit.  The AMI bootstrap inserts a "default"
-- tenant so the first admin sign-in has a tenant to bind to.

CREATE TABLE tenants (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name         text NOT NULL,
    display_name text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    deleted_at   timestamptz NULL
);

-- Tenant name is a DNS-style slug; uniqueness is global.
CREATE UNIQUE INDEX tenants_name_uq
    ON tenants (name)
    WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- users
-- ---------------------------------------------------------------------------
-- Human accounts.  Pulsys is OIDC-only (external IdP); password_hash
-- is retained for migration stability but is NEVER populated.
-- oidc_sub is the primary identity link.
-- role is a tenant-scoped RBAC label enforced by the admin plane.

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    email         citext NOT NULL,
    display_name  text NOT NULL,
    password_hash text NULL,
    is_active     boolean NOT NULL DEFAULT true,
    role          text NOT NULL DEFAULT 'member',
    oidc_sub      text NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    deleted_at    timestamptz NULL,
    CONSTRAINT users_role_chk CHECK (role IN ('owner','admin','member','reader'))
);

-- Per-tenant email uniqueness.  citext already gives us
-- case-insensitive equality; the lower() expression index makes
-- the same constraint reachable without citext if a future
-- migration drops the extension.
CREATE UNIQUE INDEX users_tenant_email_uq
    ON users (tenant_id, lower(email::text))
    WHERE deleted_at IS NULL;

-- OIDC sub is globally unique within a tenant once set.
CREATE UNIQUE INDEX users_tenant_oidc_sub_uq
    ON users (tenant_id, oidc_sub)
    WHERE oidc_sub IS NOT NULL AND deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- tokens
-- ---------------------------------------------------------------------------
-- Personal access tokens + robot tokens.  The plaintext is never
-- stored; only the sha256 of the token (in `hash`) and the first
-- 8 characters of the plaintext (in `prefix`) are persisted.
-- The auth layer hashes the bearer, finds candidates by prefix,
-- and exact-matches by hash.

CREATE TABLE tokens (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    owner_user_id uuid NULL REFERENCES users(id) ON DELETE RESTRICT,
    name          text NOT NULL,
    prefix        text NOT NULL,
    hash          bytea NOT NULL,
    scopes        text[] NOT NULL DEFAULT '{}',
    last_used_at  timestamptz NULL,
    expires_at    timestamptz NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    revoked_at    timestamptz NULL,
    deleted_at    timestamptz NULL,
    CONSTRAINT tokens_prefix_len_chk CHECK (length(prefix) BETWEEN 4 AND 16),
    CONSTRAINT tokens_hash_len_chk   CHECK (octet_length(hash) = 32)
);

-- sha256 collisions are unreachable; the uniqueness constraint
-- exists so a bug in the auth layer that double-hashes a token
-- cannot produce two rows that look identical.
CREATE UNIQUE INDEX tokens_hash_uq
    ON tokens (hash)
    WHERE deleted_at IS NULL;

-- Hot lookup: auth layer hashes the bearer, finds candidates by
-- (tenant_id, prefix), then exact-matches the hash.
CREATE INDEX tokens_tenant_prefix_idx
    ON tokens (tenant_id, prefix)
    WHERE deleted_at IS NULL AND revoked_at IS NULL;

-- ---------------------------------------------------------------------------
-- audit_log
-- ---------------------------------------------------------------------------
-- Append-only audit trail.  P2 enforces immutability in the
-- application layer; P3 layers a row-level INSERT-only RLS
-- policy and revokes UPDATE / DELETE from the application role.

CREATE TABLE audit_log (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    actor_type   text NOT NULL,
    actor_id     uuid NULL,
    action       text NOT NULL,
    resource     text NULL,
    outcome      text NOT NULL,
    metadata     jsonb NOT NULL DEFAULT '{}'::jsonb,
    ip           inet NULL,
    user_agent   text NULL,
    occurred_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT audit_log_actor_type_chk CHECK (actor_type IN ('user','token','system')),
    CONSTRAINT audit_log_outcome_chk    CHECK (outcome    IN ('success','failure','denied'))
);

-- Primary admin-UI query: recent events for a tenant.
CREATE INDEX audit_log_tenant_occurred_at_idx
    ON audit_log (tenant_id, occurred_at DESC);

-- Secondary: per-actor history.
CREATE INDEX audit_log_tenant_actor_occurred_at_idx
    ON audit_log (tenant_id, actor_id, occurred_at DESC)
    WHERE actor_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- settings
-- ---------------------------------------------------------------------------
-- Tenant-scoped key/value config with an optimistic-lock version
-- counter.  The application increments + checks `version` when
-- writing to avoid lost updates from concurrent admin actions.

CREATE TABLE settings (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    scope       text NOT NULL,
    key         text NOT NULL,
    value       jsonb NOT NULL,
    version     bigint NOT NULL DEFAULT 1,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    updated_by  uuid NULL REFERENCES users(id) ON DELETE SET NULL
);

-- One row per (tenant, scope, key).
CREATE UNIQUE INDEX settings_tenant_scope_key_uq
    ON settings (tenant_id, scope, key);

COMMIT;

-- 0002_auth_oidc.up.sql
--
-- Pulsys OIDC-only auth layer.
--
-- Adds:
--   * oidc_providers   one external IdP config per tenant (v1)
--   * oidc_auth_states short-lived PKCE/state storage
--   * sessions         server-side human sessions after OIDC callback
--   * RLS policies     tenant isolation on P2 tables (requires the app
--                      to SET pulsys.tenant_id per transaction)

BEGIN;

-- ---------------------------------------------------------------------------
-- oidc_providers
-- ---------------------------------------------------------------------------
-- One OIDC issuer per tenant in v1.  client_secret is stored as-is;
-- protect it with database-level encryption-at-rest and restrict table
-- access rather than relying on application-layer encryption.

CREATE TABLE oidc_providers (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               uuid NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    issuer                  text NOT NULL,
    client_id               text NOT NULL,
    client_secret           text NOT NULL,
    redirect_uri            text NOT NULL,
    scopes                  text NOT NULL DEFAULT 'openid profile email',
    enabled                 boolean NOT NULL DEFAULT true,
    groups_claim            text NOT NULL DEFAULT 'groups',
    owner_groups            text[] NOT NULL DEFAULT '{pulsys:owner}',
    admin_groups            text[] NOT NULL DEFAULT '{pulsys:admin}',
    jit_default_role        text NOT NULL DEFAULT 'member',
    require_preprovisioned  boolean NOT NULL DEFAULT false,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT oidc_providers_jit_role_chk CHECK (
        jit_default_role IN ('owner','admin','member','reader')
    )
);

CREATE UNIQUE INDEX oidc_providers_tenant_uq
    ON oidc_providers (tenant_id);

CREATE INDEX oidc_providers_enabled_idx
    ON oidc_providers (tenant_id)
    WHERE enabled = true;

-- ---------------------------------------------------------------------------
-- oidc_auth_states
-- ---------------------------------------------------------------------------
-- Holds PKCE verifiers + nonces between /auth/oidc/login and callback.
-- Rows expire within minutes and are deleted after successful exchange.

CREATE TABLE oidc_auth_states (
    state          text PRIMARY KEY,
    tenant_id      uuid NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    code_verifier  text NOT NULL,
    nonce          text NOT NULL,
    expires_at     timestamptz NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX oidc_auth_states_expires_idx
    ON oidc_auth_states (expires_at);

-- ---------------------------------------------------------------------------
-- sessions
-- ---------------------------------------------------------------------------
-- Opaque server-side sessions issued after a successful OIDC callback.
-- The cookie carries the plaintext token once; only sha256(token) is
-- stored.  password_hash on users is never touched.

CREATE TABLE sessions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash   bytea NOT NULL,
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    tenant_id    uuid NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    csrf_token   text NOT NULL,
    expires_at   timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    created_at   timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz NULL,
    CONSTRAINT sessions_token_hash_len_chk CHECK (octet_length(token_hash) = 32)
);

CREATE UNIQUE INDEX sessions_token_hash_uq
    ON sessions (token_hash)
    WHERE revoked_at IS NULL;

CREATE INDEX sessions_user_active_idx
    ON sessions (user_id, expires_at DESC)
    WHERE revoked_at IS NULL;

CREATE INDEX sessions_expires_idx
    ON sessions (expires_at)
    WHERE revoked_at IS NULL;

-- ---------------------------------------------------------------------------
-- Row-level security (defense in depth for RDS / multi-tenant SaaS)
-- ---------------------------------------------------------------------------
-- Effective only when the application role is NOT a superuser and the
-- connection sets:  SET LOCAL pulsys.tenant_id = '<uuid>';
-- The bundled AMI connects as postgres (superuser) which bypasses RLS;
-- policies are still created so RDS deployments can use a dedicated role.

ALTER TABLE users ENABLE ROW LEVEL SECURITY;
ALTER TABLE tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log ENABLE ROW LEVEL SECURITY;

CREATE POLICY users_tenant_isolation ON users
    USING (
        tenant_id = NULLIF(current_setting('pulsys.tenant_id', true), '')::uuid
    );

CREATE POLICY tokens_tenant_isolation ON tokens
    USING (
        tenant_id = NULLIF(current_setting('pulsys.tenant_id', true), '')::uuid
    );

CREATE POLICY settings_tenant_isolation ON settings
    USING (
        tenant_id = NULLIF(current_setting('pulsys.tenant_id', true), '')::uuid
    );

CREATE POLICY audit_log_tenant_isolation ON audit_log
    USING (
        tenant_id = NULLIF(current_setting('pulsys.tenant_id', true), '')::uuid
    );

-- audit_log is append-only for the application role when not superuser.
CREATE POLICY audit_log_insert_only ON audit_log
    FOR INSERT
    WITH CHECK (
        tenant_id = NULLIF(current_setting('pulsys.tenant_id', true), '')::uuid
    );

COMMIT;

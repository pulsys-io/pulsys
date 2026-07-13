-- 0003_drop_oidc_auth_states.down.sql
--
-- Restores oidc_auth_states as it existed in 0002 (unused by current
-- backend code; kept for migration reversibility only).

BEGIN;

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

COMMIT;

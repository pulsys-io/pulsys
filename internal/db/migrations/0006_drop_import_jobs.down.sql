-- 0006_drop_import_jobs.down.sql
--
-- Inverse of 0006_drop_import_jobs.up.sql — restores the bespoke queue table.

BEGIN;

CREATE TABLE import_jobs (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    created_by    uuid NULL REFERENCES users(id) ON DELETE SET NULL,
    type          text NOT NULL,
    status        text NOT NULL DEFAULT 'queued',
    payload       jsonb NOT NULL DEFAULT '{}'::jsonb,
    progress      jsonb NOT NULL DEFAULT '{}'::jsonb,
    error         text NULL,
    attempt       integer NOT NULL DEFAULT 0,
    lease_owner   text NULL,
    lease_until   timestamptz NULL,
    started_at    timestamptz NULL,
    completed_at  timestamptz NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT import_jobs_type_chk CHECK (type IN ('hf_cache_import')),
    CONSTRAINT import_jobs_status_chk CHECK (status IN ('queued','running','succeeded','failed','canceled')),
    CONSTRAINT import_jobs_attempt_nonneg_chk CHECK (attempt >= 0),
    CONSTRAINT import_jobs_payload_object_chk CHECK (jsonb_typeof(payload) = 'object'),
    CONSTRAINT import_jobs_progress_object_chk CHECK (jsonb_typeof(progress) = 'object'),
    CONSTRAINT import_jobs_lease_owner_chk CHECK (lease_owner IS NULL OR length(lease_owner) BETWEEN 1 AND 128)
);

CREATE INDEX import_jobs_tenant_created_at_idx
    ON import_jobs (tenant_id, created_at DESC);

CREATE INDEX import_jobs_tenant_status_created_at_idx
    ON import_jobs (tenant_id, status, created_at DESC);

CREATE INDEX import_jobs_claim_idx
    ON import_jobs (status, lease_until, created_at)
    WHERE status IN ('queued','running');

ALTER TABLE import_jobs ENABLE ROW LEVEL SECURITY;

CREATE POLICY import_jobs_tenant_isolation ON import_jobs
    USING (tenant_id = NULLIF(current_setting('pulsys.tenant_id', true), '')::uuid);

COMMIT;

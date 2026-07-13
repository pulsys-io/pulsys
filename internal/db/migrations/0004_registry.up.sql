-- 0004_registry.up.sql
--
-- Adds the self-hosted model registry: tenant-scoped repositories,
-- commits, branches, and mirror declarations, plus a globally
-- content-addressed blob table that backs both inline file content
-- and LFS objects.
--
-- Why a single `blobs` table for inline + LFS:
--
--   Both are sha256-addressed. Inline content from a commit payload
--   (small README, config.json) and LFS objects from a `PUT /lfs-
--   storage/{oid}` end up indistinguishable on disk - same hash, same
--   storage_url, same refcount accounting. Keeping them in one table
--   means commit and LFS verify share one INSERT ... ON CONFLICT path
--   and dedup is automatic.
--
-- Why global dedup (not tenant-scoped):
--
--   The blob table is keyed by sha256 only, so two tenants uploading
--   byte-identical bytes share storage. Access control is enforced at
--   the `repos`/`file_revisions` layer - a tenant can never reach a
--   blob's bytes without owning a file_revisions row that points to
--   it. The blob row itself carries no tenant_id and never returns
--   directly from a request.
--
-- Why a mirrors table:
--
--   The default policy is strict-private: a client request for
--   `acme/widget` 404s unless either (a) a row exists in `repos`
--   (registry hit) or (b) a row exists in `mirrors` declaring an
--   upstream to fetch from. There is no implicit passthrough.

BEGIN;

-- ---------------------------------------------------------------------------
-- repos
-- ---------------------------------------------------------------------------
CREATE TABLE repos (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    repo_type     text NOT NULL,
    namespace     text NOT NULL,
    name          text NOT NULL,
    private       boolean NOT NULL DEFAULT false,
    created_by    uuid NULL REFERENCES users(id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    deleted_at    timestamptz NULL,
    CONSTRAINT repos_repo_type_chk CHECK (repo_type IN ('models','datasets','spaces')),
    CONSTRAINT repos_namespace_chk CHECK (namespace ~ '^[a-zA-Z0-9][a-zA-Z0-9._-]{0,95}$'),
    CONSTRAINT repos_name_chk      CHECK (name      ~ '^[a-zA-Z0-9][a-zA-Z0-9._-]{0,95}$')
);

CREATE UNIQUE INDEX repos_tenant_path_uq
    ON repos (tenant_id, repo_type, namespace, name)
    WHERE deleted_at IS NULL;

CREATE INDEX repos_tenant_updated_at_idx
    ON repos (tenant_id, updated_at DESC)
    WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- blobs
-- ---------------------------------------------------------------------------
-- Globally content-addressed. `storage_url` is the abstract URL the
-- blobstore returns (e.g. file:///var/lib/pulsys/blobs/ab/abc...);
-- callers MUST NEVER parse it - go through internal/blobstore.
CREATE TABLE blobs (
    oid          char(64) PRIMARY KEY,
    size_bytes   bigint   NOT NULL,
    storage_url  text     NOT NULL,
    refcount     bigint   NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT blobs_size_nonneg_chk     CHECK (size_bytes >= 0),
    CONSTRAINT blobs_refcount_nonneg_chk CHECK (refcount   >= 0)
);

-- ---------------------------------------------------------------------------
-- commits
-- ---------------------------------------------------------------------------
CREATE TABLE commits (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_id     uuid NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    sha         char(40) NOT NULL,
    parent_sha  char(40) NULL,
    author_id   uuid NULL REFERENCES users(id) ON DELETE SET NULL,
    summary     text NOT NULL,
    description text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (repo_id, sha)
);

CREATE INDEX commits_repo_created_at_idx
    ON commits (repo_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- branches
-- ---------------------------------------------------------------------------
CREATE TABLE branches (
    repo_id    uuid NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    name       text NOT NULL,
    commit_sha char(40) NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repo_id, name),
    CONSTRAINT branches_name_chk CHECK (name ~ '^[a-zA-Z0-9][a-zA-Z0-9._/-]{0,95}$')
);

-- ---------------------------------------------------------------------------
-- file_revisions
-- ---------------------------------------------------------------------------
-- One row per (commit, path). The blob_oid join carries the bytes.
-- is_lfs distinguishes pointer files from inline content for the HF
-- wire response shape; it does NOT change how we store the bytes.
CREATE TABLE file_revisions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    commit_id    uuid NOT NULL REFERENCES commits(id) ON DELETE CASCADE,
    path         text NOT NULL,
    blob_oid     char(64) NOT NULL REFERENCES blobs(oid) ON DELETE RESTRICT,
    is_lfs       boolean NOT NULL DEFAULT false,
    UNIQUE (commit_id, path),
    CONSTRAINT file_revisions_path_chk CHECK (length(path) > 0 AND length(path) <= 4096)
);

CREATE INDEX file_revisions_blob_idx
    ON file_revisions (blob_oid);

CREATE INDEX file_revisions_commit_path_idx
    ON file_revisions (commit_id, path);

-- ---------------------------------------------------------------------------
-- mirrors
-- ---------------------------------------------------------------------------
-- Admin-managed: explicit declaration that `acme/widget` should be
-- backed by an upstream when the local registry has nothing. A
-- mirror is consulted ONLY when no row exists in `repos` for the
-- same (tenant, type, ns, name).
CREATE TABLE mirrors (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      uuid NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    repo_type      text NOT NULL,
    namespace      text NOT NULL,
    name           text NOT NULL,
    upstream_host  text NOT NULL DEFAULT 'huggingface.co',
    pinned_sha     char(40) NULL,
    created_by     uuid NULL REFERENCES users(id) ON DELETE SET NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    deleted_at     timestamptz NULL,
    CONSTRAINT mirrors_repo_type_chk CHECK (repo_type IN ('models','datasets','spaces'))
);

CREATE UNIQUE INDEX mirrors_tenant_path_uq
    ON mirrors (tenant_id, repo_type, namespace, name)
    WHERE deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- RLS
-- ---------------------------------------------------------------------------
ALTER TABLE repos          ENABLE ROW LEVEL SECURITY;
ALTER TABLE commits        ENABLE ROW LEVEL SECURITY;
ALTER TABLE branches       ENABLE ROW LEVEL SECURITY;
ALTER TABLE file_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE mirrors        ENABLE ROW LEVEL SECURITY;

CREATE POLICY repos_tenant_isolation ON repos
    USING (tenant_id = NULLIF(current_setting('pulsys.tenant_id', true), '')::uuid);

-- commits/branches/file_revisions inherit tenancy via repo_id; the
-- policy joins back to repos to enforce isolation without forcing
-- callers to filter manually.
CREATE POLICY commits_tenant_isolation ON commits
    USING (
        repo_id IN (
            SELECT id FROM repos
            WHERE tenant_id = NULLIF(current_setting('pulsys.tenant_id', true), '')::uuid
        )
    );

CREATE POLICY branches_tenant_isolation ON branches
    USING (
        repo_id IN (
            SELECT id FROM repos
            WHERE tenant_id = NULLIF(current_setting('pulsys.tenant_id', true), '')::uuid
        )
    );

CREATE POLICY file_revisions_tenant_isolation ON file_revisions
    USING (
        commit_id IN (
            SELECT c.id FROM commits c
            JOIN repos r ON r.id = c.repo_id
            WHERE r.tenant_id = NULLIF(current_setting('pulsys.tenant_id', true), '')::uuid
        )
    );

CREATE POLICY mirrors_tenant_isolation ON mirrors
    USING (tenant_id = NULLIF(current_setting('pulsys.tenant_id', true), '')::uuid);

-- blobs deliberately has NO RLS: it is content-addressed and shared
-- globally. Access is gated by file_revisions ownership, not by row
-- visibility in this table.

COMMIT;

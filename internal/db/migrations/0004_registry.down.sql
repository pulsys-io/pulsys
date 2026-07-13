-- 0004_registry.down.sql
--
-- Inverse of 0004_registry.up.sql.

BEGIN;

DROP POLICY IF EXISTS mirrors_tenant_isolation       ON mirrors;
DROP POLICY IF EXISTS file_revisions_tenant_isolation ON file_revisions;
DROP POLICY IF EXISTS branches_tenant_isolation      ON branches;
DROP POLICY IF EXISTS commits_tenant_isolation       ON commits;
DROP POLICY IF EXISTS repos_tenant_isolation         ON repos;

ALTER TABLE IF EXISTS mirrors        DISABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS file_revisions DISABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS branches       DISABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS commits        DISABLE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS repos          DISABLE ROW LEVEL SECURITY;

DROP TABLE IF EXISTS mirrors;
DROP TABLE IF EXISTS file_revisions;
DROP TABLE IF EXISTS branches;
DROP TABLE IF EXISTS commits;
DROP TABLE IF EXISTS blobs;
DROP TABLE IF EXISTS repos;

COMMIT;

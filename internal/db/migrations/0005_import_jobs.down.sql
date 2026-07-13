-- 0005_import_jobs.down.sql
--
-- Inverse of 0005_import_jobs.up.sql.

BEGIN;

DROP POLICY IF EXISTS import_jobs_tenant_isolation ON import_jobs;
ALTER TABLE IF EXISTS import_jobs DISABLE ROW LEVEL SECURITY;
DROP TABLE IF EXISTS import_jobs;

COMMIT;

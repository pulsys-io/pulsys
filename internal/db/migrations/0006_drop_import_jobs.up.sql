-- 0006_drop_import_jobs.up.sql
--
-- Import jobs move to River (river_job). The bespoke import_jobs queue is
-- removed now that the admin API and worker use github.com/riverqueue/river.
--
-- Plan: import-worker-p2p/river-migrate

BEGIN;

DROP POLICY IF EXISTS import_jobs_tenant_isolation ON import_jobs;
ALTER TABLE IF EXISTS import_jobs DISABLE ROW LEVEL SECURITY;
DROP TABLE IF EXISTS import_jobs;

COMMIT;

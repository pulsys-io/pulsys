-- 0002_auth_oidc.down.sql
--
-- Reverses 0002_auth_oidc.up.sql.

BEGIN;

DROP POLICY IF EXISTS audit_log_insert_only ON audit_log;
DROP POLICY IF EXISTS audit_log_tenant_isolation ON audit_log;
DROP POLICY IF EXISTS settings_tenant_isolation ON settings;
DROP POLICY IF EXISTS tokens_tenant_isolation ON tokens;
DROP POLICY IF EXISTS users_tenant_isolation ON users;

ALTER TABLE audit_log DISABLE ROW LEVEL SECURITY;
ALTER TABLE settings DISABLE ROW LEVEL SECURITY;
ALTER TABLE tokens DISABLE ROW LEVEL SECURITY;
ALTER TABLE users DISABLE ROW LEVEL SECURITY;

DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS oidc_auth_states;
DROP TABLE IF EXISTS oidc_providers;

COMMIT;

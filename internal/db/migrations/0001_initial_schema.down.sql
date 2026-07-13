-- 0001_initial_schema.down.sql
--
-- Reverses 0001_initial_schema.up.sql.  Tables are dropped in
-- reverse dependency order so foreign keys never block the drop.
--
-- citext is left installed because other databases on the same
-- cluster may use it; dropping an extension as part of a single
-- application's migration is intrusive.

BEGIN;

DROP TABLE IF EXISTS settings;
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS tokens;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS tenants;

COMMIT;

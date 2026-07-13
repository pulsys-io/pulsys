-- 0003_drop_oidc_auth_states.up.sql
--
-- PKCE and the authorization redirect live in the admin SPA (P6).
-- The backend no longer stores in-flight OAuth state; it only
-- verifies id_tokens POSTed by the frontend after PKCE completes.

BEGIN;

DROP TABLE IF EXISTS oidc_auth_states;

COMMIT;

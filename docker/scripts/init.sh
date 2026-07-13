#!/bin/sh
# pulsys-init — docker compose one-shot bootstrap (migrate, tenant, OIDC).
set -eu

: "${PULSYS_DB_DSN:?PULSYS_DB_DSN required}"
: "${PULSYS_OIDC_ISSUER:?PULSYS_OIDC_ISSUER required}"

KC_INTERNAL="${PULSYS_KEYCLOAK_INTERNAL_URL:-http://keycloak:8080}"
OIDC_CLIENT_ID="${PULSYS_OIDC_CLIENT_ID:-pulsys-admin}"
OIDC_CLIENT_SECRET="${PULSYS_OIDC_CLIENT_SECRET:-public-dev-only}"
OIDC_REDIRECT_URI="${PULSYS_OIDC_REDIRECT_URI:-http://localhost:3000/auth/oidc/callback}"

echo "pulsys-init: waiting for postgres..."
until pulsys-db health >/dev/null 2>&1; do
  sleep 1
done
echo "pulsys-init: postgres ready"

echo "pulsys-init: waiting for keycloak (${KC_INTERNAL})..."
until curl -sf "${KC_INTERNAL}/realms/pulsys/.well-known/openid-configuration" >/dev/null; do
  sleep 2
done
echo "pulsys-init: keycloak ready"

echo "pulsys-init: migrate up"
pulsys-db migrate up

echo "pulsys-init: ensure default tenant"
pulsys-db tenant ensure --name default --display-name "Default Tenant"

echo "pulsys-init: configure OIDC"
pulsys-db oidc configure \
  --tenant default \
  --issuer "${PULSYS_OIDC_ISSUER}" \
  --client-id "${OIDC_CLIENT_ID}" \
  --client-secret "${OIDC_CLIENT_SECRET}" \
  --redirect-uri "${OIDC_REDIRECT_URI}" \
  --owner-groups pulsys:owner

echo "pulsys-init: complete"

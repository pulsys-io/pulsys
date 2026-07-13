#!/bin/sh
# pulsys-pulsys — launch pulsys inside docker compose.
set -eu

exec pulsys \
  -listen "${PULSYS_LISTEN:-:8080}" \
  -admin-listen "${PULSYS_ADMIN_LISTEN:-0.0.0.0:6060}" \
  -public-base-url "${PULSYS_PUBLIC_BASE_URL:-http://localhost:8080}" \
  -cache-dir "${PULSYS_CACHE_DIR:-/var/lib/pulsys/cache}" \
  -log-level "${PULSYS_LOG_LEVEL:-info}" \
  "$@"

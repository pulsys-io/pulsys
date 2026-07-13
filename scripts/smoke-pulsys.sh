#!/usr/bin/env bash
# smoke-pulsys.sh -- quick liveness check for the local `docker compose` stack.
#
# Usage:
#   PULSYS_SMOKE_BASE=http://localhost:3000 bash scripts/smoke-pulsys.sh
#
# Env:
#   PULSYS_SMOKE_BASE   console origin (admin SPA + /auth + /admin)  [http://localhost:3000]
#   PULSYS_PROXY_BASE   HF cache proxy ingress                       [http://localhost:8082]
set -euo pipefail

CONSOLE="${PULSYS_SMOKE_BASE:-http://localhost:3000}"
PROXY="${PULSYS_PROXY_BASE:-http://localhost:8082}"

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

check() {
	local name="$1" url="$2"
	printf '  %-22s %-32s ... ' "$name" "$url"
	if curl -fsS -o /dev/null --max-time 10 "$url"; then
		echo "ok"
	else
		echo "FAILED"
		fail "$name not reachable at $url (is 'docker compose up' running?)"
	fi
}

echo "==> Pulsys smoke check"
check "console"      "$CONSOLE/"
check "proxy readyz" "$PROXY/readyz"
echo "==> All checks passed"

#!/usr/bin/env bash
# security-tests-linux.sh -- run the OWASP security test
# matrix on Linux from a macOS dev box via docker compose.
#
# Phase 4.5 of the OWASP WSTG hardening pass adds this
# runner so the Linux-only iouring code paths in
# internal/coreserver/iouring_*_linux.go are exercised by
# the same security tests that pass on Darwin.  Without
# this, the Linux build is only exercised by the
# .github/workflows/linux.yml CI step which runs the entire
# test suite but does NOT force iouring on (because most
# tests don't care about it).
#
# This script also brings up an isolated Postgres sidecar
# so the authcontract tests, which need a real RLS-bound
# Postgres for cross-tenant IDOR + session-lifecycle, run
# at full fidelity instead of skipping.
#
# Usage:
#   scripts/security-tests-linux.sh                # full run
#   PULSYS_KEEP_VOLUMES=1 scripts/security-tests-linux.sh
#       # leave the pg tmpfs volume after exit (faster rerun
#       # but volumes will accrete state across runs)
#
# Requirements:
#   - docker + docker compose v2 (or OrbStack / colima)
#   - Linux kernel 6.1+ on the host VM for io_uring success;
#     older kernels will fail because
#     PULSYS_TEST_IOURING_REQUIRE is on by default.  Override
#     by exporting PULSYS_TEST_IOURING_REQUIRE=0 (CI will
#     emit a warning but still pass).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"

COMPOSE_FILE="docker-compose.security-tests.yml"

echo "==> security-tests-linux"
echo "    repo:         ${ROOT}"
echo "    compose file: ${COMPOSE_FILE}"
echo "    docker host:"
docker info --format '      kernel: {{.KernelVersion}}  os: {{.OperatingSystem}}  cpus: {{.NCPU}}  mem: {{.MemTotal}}' \
    2>/dev/null || true

# Honor an override so a dev on an older Docker Desktop
# kernel can still run the suite at lower fidelity.
PULSYS_TEST_IOURING_REQUIRE="${PULSYS_TEST_IOURING_REQUIRE:-1}"
export PULSYS_TEST_IOURING_REQUIRE

echo "==> bringing up compose stack"
# --remove-orphans cleans up any leftover container from a
# previous interrupted run.  --build is forced because we
# almost always want the latest source.
docker compose -f "${COMPOSE_FILE}" up \
    --build \
    --abort-on-container-exit \
    --exit-code-from security-tests \
    --remove-orphans
RC=$?

echo "==> security-tests exit code: ${RC}"

if [ -z "${PULSYS_KEEP_VOLUMES:-}" ]; then
    echo "==> tearing down"
    docker compose -f "${COMPOSE_FILE}" down --volumes --remove-orphans
else
    echo "==> leaving volumes up (PULSYS_KEEP_VOLUMES=${PULSYS_KEEP_VOLUMES})"
fi

exit "${RC}"

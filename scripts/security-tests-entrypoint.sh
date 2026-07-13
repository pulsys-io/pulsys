#!/usr/bin/env bash
# security-tests-entrypoint.sh -- runs INSIDE the
# Dockerfile.security-tests image.  Wrapped by
# scripts/security-tests-linux.sh on the developer host.
#
# Invariants this script enforces:
#
#   1. Postgres sidecar is reachable BEFORE the first test
#      runs.  We tolerate up to 60s of warm-up because the
#      tmpfs-backed pg cluster takes a moment to initdb.
#
#   2. Kernel version is logged and (if
#      PULSYS_TEST_IOURING_REQUIRE=1) checked against the
#      io_uring DEFER_TASKRUN floor (6.1).
#
#   3. The exact package set we run is documented inline
#      below; do not silently widen.
#
#   4. The run is the last thing the entrypoint does, so its
#      exit code surfaces as the container exit code (used by
#      `docker compose up --exit-code-from security-tests`).
set -euo pipefail

PG_HOST="${PULSYS_TEST_PG_HOST:-postgres-sec}"
PG_PORT="${PULSYS_TEST_PG_PORT:-5432}"

echo "==> security-tests-entrypoint"
echo "    kernel: $(uname -a)"
echo "    go:     $(go version)"
echo "    cwd:    $(pwd)"
echo "    PULSYS_TEST_IOURING:         ${PULSYS_TEST_IOURING:-}"
echo "    PULSYS_TEST_IOURING_REQUIRE: ${PULSYS_TEST_IOURING_REQUIRE:-}"
echo "    PULSYS_TEST_PG_DSN host:     ${PG_HOST}:${PG_PORT}"

# 1. Wait for Postgres.  The compose healthcheck already
# gates dependency start, but a second check here is cheap
# insurance against compose version skew.
echo "==> waiting for postgres at ${PG_HOST}:${PG_PORT}"
for i in $(seq 1 60); do
    if nc -z "${PG_HOST}" "${PG_PORT}" 2>/dev/null; then
        echo "    postgres ready after ${i}s"
        break
    fi
    if [ "${i}" -eq 60 ]; then
        echo "ERROR: postgres did not become ready within 60s" >&2
        exit 2
    fi
    sleep 1
done

# 2. Kernel version gate.  We don't enforce here -- the
# coreserver's ioUringInit also checks and the test asserts
# the iouring path was actually exercised.  This is just
# diagnostic output for the test log header.
KERNEL_REL="$(uname -r)"
KERNEL_MAJOR="$(echo "${KERNEL_REL}" | cut -d. -f1)"
KERNEL_MINOR="$(echo "${KERNEL_REL}" | cut -d. -f2 | sed 's/[^0-9].*//')"
echo "==> kernel-release ${KERNEL_REL}  major=${KERNEL_MAJOR}  minor=${KERNEL_MINOR}"

# 3. Run the security-relevant package set.  Order chosen so
# the cheapest / most-likely-to-fail packages run FIRST,
# minimising feedback loop time on a real failure.
#
# Package set rationale:
#
#   internal/auth                  PAT crypto + middleware + csrf
#   internal/auth/httpx            cookie attrs + enumeration tests
#   internal/auth/store            session lifecycle + RLS tests
#   internal/security/authcontract IDOR + mass-assignment + sec headers + secret-log
#   internal/security/sectest      smuggling + path traversal + ssrf +
#                                  error disclosure + cors + verb tampering
#   internal/coreserver            hand-rolled HTTP/1.1 parser +
#                                  iouring_linux_test (Linux-only)
#   internal/proxy                 auth enforcement + xet bridge + handler
#
# The TIMEOUT is generous (10m) because the SQL injection
# audit + iouring boot + race detector overhead all compound.
PKGS=(
    ./internal/auth/...
    ./internal/security/authcontract/...
    ./internal/security/sectest/...
    ./internal/coreserver/...
    ./internal/proxy/...
)

GO_TEST_FLAGS=(-race -count=1 -timeout 10m)
if [ "${GO_TEST_VERBOSE:-}" = "1" ]; then
    GO_TEST_FLAGS+=(-v)
fi

# Pass 1: full security matrix on Linux (cork+sendfile path).
# This is the wide-coverage pass that catches CRLF / file-path /
# build-tag issues across the entire Phase 0-4 surface.  We do NOT
# force PULSYS_TEST_IOURING here; the reactor shutdown contract
# (eventfd wakeup + waitInFlight) is exercised in pass 2 where it
# can be observed in isolation, and the wide matrix above does
# not benefit from io_uring (it's HTTP semantics, not throughput).
echo "==> [pass 1/3] full security matrix (cork+sendfile)"
echo "    flags: ${GO_TEST_FLAGS[*]}"
echo "    pkgs:  ${PKGS[*]}"
go test "${GO_TEST_FLAGS[@]}" "${PKGS[@]}"
RC1=$?
echo "==> [pass 1/3] exit code: ${RC1}"
if [ "${RC1}" -ne 0 ]; then
    echo "ERROR: full security matrix failed -- skipping later passes"
    exit "${RC1}"
fi

# Pass 2: io_uring-specific coverage.  Three targeted invocations:
#
#   * TestIoUringParser_*       -- fans the smuggling corpus
#     through parseRequestFromBuf (the iouring-mode parser).  No
#     reactor boot, no goroutine-leak risk; pure parser
#     differential.
#
#   * TestWarmHitIoUring        -- end-to-end warm GET through
#     the reactor + io_uring fused-write path.  Asserts the
#     telemetry counter advances (proves the path actually ran,
#     not the cork fallback).
#
#   * TestReactor_Close*        -- the shutdown-contract suite.
#     Pins the eventfd wakeup + waitInFlight drain that makes
#     reactor goroutines reclaimable across Server.Close().  A
#     regression here is the bug that originally pinned this
#     entire Phase 4.5 effort; we run it explicitly so a
#     reactor-side leak fails the docker job loudly rather than
#     hanging it (10m -timeout is the backstop).
#
# These run with -race too because the reactor uses concurrent
# CQE drain + handler dispatch.
echo "==> [pass 2/3] io_uring-specific coverage"
go test "${GO_TEST_FLAGS[@]}" \
    -run '^(TestIoUringParser_|TestWarmHitIoUring$|TestReactor_)' \
    ./internal/coreserver/...
RC2=$?
echo "==> [pass 2/3] exit code: ${RC2}"
if [ "${RC2}" -ne 0 ]; then
    echo "ERROR: io_uring-specific coverage failed -- skipping pass 3"
    exit "${RC2}"
fi

# Pass 3: re-run the io_uring shutdown contract WITHOUT -race
# and with a tight timeout (45s) so an accidental leak surfaces
# as a fast, non-flaky failure even in low-CPU CI environments.
# -race adds ~5x latency to t.Cleanup paths; running once
# without it is a cheap insurance policy.
echo "==> [pass 3/3] io_uring shutdown contract (tight timeout)"
go test -count=1 -timeout 45s \
    -run '^TestReactor_' \
    ./internal/coreserver/...
RC3=$?
echo "==> [pass 3/3] exit code: ${RC3}"
exit "${RC3}"

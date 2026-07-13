#!/usr/bin/env bash
# ssm-sweep.sh -- run the full tunings sweep on the bench instance,
# download the tarball + sweep-report.md, and write the run's report to
# tmp/bench/tunings-report.md (a generated artifact; the tuning rationale
# itself lives in docs/internals.md).
#
# Usage:
#   scripts/ssm-sweep.sh
#   scripts/ssm-sweep.sh duration=10 rounds=1            # quick smoke
#   scripts/ssm-sweep.sh only=bbr,tfo                     # subset
#   scripts/ssm-sweep.sh conns="128 512" payloads="4k 16m"
#
# Args are key=value pairs forwarded to the SSM document; see
# infra/cdk/lib/bench-docs.ts for the parameter list.  Spaces in values
# must be quoted to survive shell splitting (e.g. payloads="4k 16m").
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
source "$ROOT/scripts/lib/ssm-common.sh"

DEST="$ROOT/tmp/bench/sweep"
mkdir -p "$DEST"

DOC_NAME="$(stack_output RunSweepCmdOut | sed -n 's/.*--document-name \([^ ]*\) .*/\1/p')"
if [ -z "$DOC_NAME" ]; then
    echo "FATAL: could not parse sweep document name from RunSweepCmdOut" >&2
    exit 1
fi

echo "==> dispatching $DOC_NAME with args: $*" >&2
CMD_ID="$(ssm_send "$DOC_NAME" "$@")"
echo "==> command id $CMD_ID" >&2

LOG="$DEST/last-run.log"
ssm_wait "$CMD_ID" | tee "$LOG"

echo "==> downloading sweep artifacts from s3://*/sweep/ into $DEST" >&2
pull_s3 sweep "$DEST"

# The newest sweep-report.md becomes the generated tmp/bench/tunings-report.md.
# We pick the latest by mtime since each sweep tarball expands to a
# distinct timestamped directory.
LATEST_REPORT="$(ls -t "$DEST"/*-sweep-report.md "$DEST"/sweep-report.md 2>/dev/null | head -1 || true)"

# Some artifacts come in as untarred files; if so, we untar the latest
# tarball into DEST so the embedded sweep-report.md is reachable.
if [ -z "$LATEST_REPORT" ]; then
    LATEST_TAR="$(ls -t "$DEST"/*.tar.gz 2>/dev/null | head -1 || true)"
    if [ -n "$LATEST_TAR" ]; then
        echo "==> extracting $LATEST_TAR" >&2
        tar -xzf "$LATEST_TAR" -C "$DEST"
        LATEST_REPORT="$(find "$DEST" -name 'sweep-report.md' -print0 | xargs -0 ls -t | head -1 || true)"
    fi
fi

REPORT_OUT="$ROOT/tmp/bench/tunings-report.md"
if [ -n "$LATEST_REPORT" ] && [ -f "$LATEST_REPORT" ]; then
    cp "$LATEST_REPORT" "$REPORT_OUT"
    echo "==> updated $REPORT_OUT from $LATEST_REPORT"
else
    echo "WARNING: no sweep-report.md found in $DEST -- $REPORT_OUT NOT updated" >&2
fi

ls -lh "$DEST"/*.tar.gz 2>/dev/null | tail -5 || true

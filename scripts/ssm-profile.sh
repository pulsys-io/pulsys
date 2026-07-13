#!/usr/bin/env bash
# ssm-profile.sh -- run the profile harness on the bench instance and
# download the artifact tarball to tmp/bench/profile/.
#
# Usage:
#   scripts/ssm-profile.sh                  # defaults: 60s, c=256, 4k
#   scripts/ssm-profile.sh duration=30 conns=64 payload=256k roundTag=cork
#
# Args are key=value pairs forwarded to the SSM document's parameters.
# Defaults are defined on the document itself (see
# infra/cdk/lib/bench-docs.ts).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
source "$ROOT/scripts/lib/ssm-common.sh"

DEST="$ROOT/tmp/bench/profile"
mkdir -p "$DEST"

DOC_NAME="$(stack_output RunProfileCmdOut | sed -n 's/.*--document-name \([^ ]*\) .*/\1/p')"
if [ -z "$DOC_NAME" ]; then
    echo "FATAL: could not parse profile document name from RunProfileCmdOut" >&2
    exit 1
fi

echo "==> dispatching $DOC_NAME with args: $*" >&2
CMD_ID="$(ssm_send "$DOC_NAME" "$@")"
echo "==> command id $CMD_ID" >&2

# Tee the script's stdout (the artifact upload log) to the local terminal
# while also keeping it for parsing.
LOG="$DEST/last-run.log"
ssm_wait "$CMD_ID" | tee "$LOG"

echo "==> downloading new artifacts from s3://*/profile/ into $DEST" >&2
pull_s3 profile "$DEST"
echo "==> profile artifacts in $DEST"
ls -lh "$DEST"/*.tar.gz | tail -5

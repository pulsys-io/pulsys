#!/usr/bin/env bash
# ssm-sync-scripts.sh -- push the local working tree to the bench instance.
#
# Avoids an AMI rebuild for iterative shell/Go changes during profiling.
# Tars scripts/, internal/, cmd/, go.mod, go.sum from this checkout,
# uploads to s3://$bucket/sync/scripts-$ts.tar.gz, then runs an inline
# SSM shell command that downloads + extracts over /opt/pulsys-src/.
#
# Usage:
#   scripts/ssm-sync-scripts.sh                 # scripts only (fast, ~1s)
#   scripts/ssm-sync-scripts.sh full            # also Go source (~30s)
#
# After this, scripts/ssm-profile.sh / scripts/ssm-bench.sh see the new
# files.  Go-source changes additionally rebuild pulsys on the instance.
set -euo pipefail
export PATH="/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
source "$ROOT/scripts/lib/ssm-common.sh"

MODE="${1:-scripts}"
case "$MODE" in
scripts|full) ;;
*) echo "FATAL: mode must be 'scripts' or 'full'" >&2; exit 2 ;;
esac

BUCKET="$(stack_output ResultsBucketOut)"
INSTANCE="$(stack_output InstanceIdOut)"
[ -n "$BUCKET" ] && [ -n "$INSTANCE" ] || {
	echo "FATAL: cannot resolve ResultsBucketOut + InstanceIdOut (deploy infra/cdk?)" >&2
	exit 1
}

TS="$(date -u +%Y%m%dT%H%M%SZ)"
KEY="sync/scripts-${TS}.tar.gz"
TARBALL="$(mktemp -t pulsys-sync.XXXXXX.tar.gz)"
trap 'rm -f "$TARBALL"' EXIT

case "$MODE" in
scripts)
	PATHS=(scripts)
	;;
full)
	PATHS=(scripts internal cmd go.mod go.sum)
	;;
esac
tar --exclude='*.log' --exclude='node_modules' --exclude='tmp' \
    -czf "$TARBALL" "${PATHS[@]}"

echo "==> uploading $(du -sh "$TARBALL" | cut -f1) to s3://$BUCKET/$KEY" >&2
aws s3 cp --region "${AWS_REGION:-us-east-1}" "$TARBALL" "s3://$BUCKET/$KEY" --no-progress

SCRIPT_LINES=(
	"set -euo pipefail"
	"source /etc/profile.d/pulsys-go.sh 2>/dev/null || true"
	"aws s3 cp s3://${BUCKET}/${KEY} /tmp/sync.tar.gz --no-progress"
	"tar -xzf /tmp/sync.tar.gz -C /opt/pulsys-src"
	"chmod +x /opt/pulsys-src/scripts/*.sh /opt/pulsys-src/scripts/lib/*.sh 2>/dev/null || true"
)
if [ "$MODE" = "full" ]; then
	SCRIPT_LINES+=(
		"cd /opt/pulsys-src"
		"export GOCACHE=\${GOCACHE:-/opt/go/cache}"
		"export GOPATH=\${GOPATH:-/opt/go}"
		"export PATH=/usr/local/go/bin:/opt/go/bin:\$PATH"
		"go build -trimpath -ldflags '-s -w' -o /usr/local/bin/pulsys ./cmd/pulsys"
		"go build -trimpath -ldflags '-s -w' -o /usr/local/bin/fake-hf  ./cmd/fake-hf"
		"echo 'rebuilt pulsys + fake-hf (download client is the Python hf CLI + hf_transfer)'"
	)
fi
SCRIPT_LINES+=("echo 'sync complete (mode=${MODE})'")

PARAMS_JSON="$(jq -nc --argjson lines "$(printf '%s\n' "${SCRIPT_LINES[@]}" | jq -R . | jq -s .)" \
	'{commands: $lines, executionTimeout: ["300"]}')"

echo "==> running sync on $INSTANCE (mode=$MODE)" >&2
CMD_ID="$(aws ssm send-command \
	--region "${AWS_REGION:-us-east-1}" \
	--document-name AWS-RunShellScript \
	--instance-ids "$INSTANCE" \
	--parameters "$PARAMS_JSON" \
	--query 'Command.CommandId' --output text)"

ssm_wait "$CMD_ID"
echo "==> sync complete"

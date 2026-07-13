#!/usr/bin/env bash
# ssm-hf-download.sh -- real-world `hf download` test through pulsys on EC2.
#
# Drives scripts/hf_download_bench.sh on the bench instance via SSM.  Pulls
# the resulting CSV back to tmp/bench/ec2/hf-download/.
#
# HF token comes from the stack-owned Secrets Manager secret (output
# HfTokenSecretOut); set its value once via scripts/run-aws-benchmarks.sh or
# `aws secretsmanager put-secret-value`.
#
# Usage:
#   scripts/ssm-hf-download.sh                                   # default model
#   scripts/ssm-hf-download.sh model=Qwen/Qwen2.5-32B-Instruct
#   scripts/ssm-hf-download.sh model=microsoft/phi-4 skip_direct=1
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
source "$ROOT/scripts/lib/ssm-common.sh"

MODEL="Qwen/Qwen2.5-32B-Instruct"
SKIP_DIRECT=0
WORKERS="${WORKERS:-16}"
for arg in "$@"; do
	case "$arg" in
		model=*)        MODEL="${arg#model=}" ;;
		skip_direct=*)  SKIP_DIRECT="${arg#skip_direct=}" ;;
		workers=*)      WORKERS="${arg#workers=}" ;;
		*)
			echo "FATAL: unknown arg '$arg' (expected model=..., skip_direct=0|1, workers=N)" >&2
			exit 2
			;;
	esac
done

INSTANCE="$(stack_output InstanceIdOut)"
[ -n "$INSTANCE" ] || { echo "FATAL: no InstanceIdOut" >&2; exit 1; }

echo "==> syncing scripts + building Go binaries on $INSTANCE"
bash "$ROOT/scripts/ssm-sync-scripts.sh" full >/dev/null

echo "==> mounting hf-data volume (no-op if already mounted or absent)"
MOUNT_CMD_ID="$(aws ssm send-command \
	--region "$AWS_REGION" \
	--document-name AWS-RunShellScript \
	--instance-ids "$INSTANCE" \
	--parameters 'commands=["sudo bash /opt/pulsys-src/scripts/mount-hf-data-volume.sh 2>&1 || true"]' \
	--query 'Command.CommandId' --output text)"
sleep 5
aws ssm get-command-invocation --region "$AWS_REGION" \
	--command-id "$MOUNT_CMD_ID" --instance-id "$INSTANCE" \
	--query 'StandardOutputContent' --output text 2>/dev/null | tail -5 >&2 || true

echo "==> running hf_download_bench (model=$MODEL workers=$WORKERS skip_direct=$SKIP_DIRECT)"

# HF token: the instance fetches it from the CDK-owned Secrets Manager secret
# (its IAM role has GetSecretValue), so no token rides through SSM command
# history.  We only pass the secret ARN, which is not sensitive.
SECRET_ARN="$(stack_output HfTokenSecretOut)"
[ -n "$SECRET_ARN" ] || { echo "FATAL: no HfTokenSecretOut output; redeploy the stack" >&2; exit 1; }

CMD_LINES=(
	"set -euo pipefail"
	"export MODEL='$MODEL'"
	"export WORKERS='$WORKERS'"
	"export SKIP_DIRECT='$SKIP_DIRECT'"
	"export HF_TOKEN_SECRET_ARN='$SECRET_ARN'"
	"export AWS_REGION='$AWS_REGION' AWS_DEFAULT_REGION='$AWS_REGION'"
	"sudo -E bash /opt/pulsys-src/scripts/hf_download_bench.sh"
)
PARAMS_JSON="$(jq -nc \
	--argjson lines "$(printf '%s\n' "${CMD_LINES[@]}" | jq -R . | jq -s .)" \
	'{commands: $lines, executionTimeout: ["3600"]}')"

CMD_ID="$(aws ssm send-command \
	--region "$AWS_REGION" \
	--document-name AWS-RunShellScript \
	--instance-ids "$INSTANCE" \
	--parameters "$PARAMS_JSON" \
	--query 'Command.CommandId' --output text)"

echo "==> command id $CMD_ID"
DEST="$ROOT/tmp/bench/ec2/hf-download"
mkdir -p "$DEST"
LOG="$DEST/last-run.log"
ssm_wait "$CMD_ID" | tee "$LOG"

# Pull the CSV out of the host log.
aws ssm send-command \
	--region "$AWS_REGION" \
	--document-name AWS-RunShellScript \
	--instance-ids "$INSTANCE" \
	--parameters 'commands=["cat /var/log/pulsys-real/results.csv"]' \
	--query 'Command.CommandId' --output text \
	| { read CMDID; sleep 3
		aws ssm get-command-invocation \
			--region "$AWS_REGION" \
			--command-id "$CMDID" \
			--instance-id "$INSTANCE" \
			--query 'StandardOutputContent' \
			--output text >"$DEST/results.csv"; }

# Tag the CSV with the model for archival.
TAG="$(echo "$MODEL" | tr '/' '_')"
cp "$DEST/results.csv" "$DEST/results-$TAG.csv"

echo
echo "==> results saved to $DEST/results.csv (also $DEST/results-$TAG.csv)"
echo
column -t -s, "$DEST/results.csv"

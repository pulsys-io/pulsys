#!/usr/bin/env bash
# ssm-set-hf-token.sh -- store HF_TOKEN on the bench instance, once.
#
# Writes /etc/pulsys/hf-token (mode 600, root) so that hf_download_bench.sh
# and any other on-instance tooling can pick up the token without it being
# re-passed (and logged) on every SSM command.
#
# Usage:
#   HF_TOKEN=hf_xxx scripts/ssm-set-hf-token.sh
#   scripts/ssm-set-hf-token.sh hf_xxx          # token as arg (less ideal)
#
# Verify after:
#   scripts/ssm-shell.sh  # then on the instance: sudo cat /etc/pulsys/hf-token
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
source "$ROOT/scripts/lib/ssm-common.sh"

TOKEN="${1:-${HF_TOKEN:-}}"
if [ -z "$TOKEN" ]; then
	echo "FATAL: HF_TOKEN env var not set and no arg given" >&2
	echo "  usage: HF_TOKEN=hf_xxx scripts/ssm-set-hf-token.sh" >&2
	exit 2
fi
case "$TOKEN" in
	hf_*) ;;
	*) echo "WARN: token does not start with 'hf_' (typo?)" >&2 ;;
esac

INSTANCE="$(stack_output InstanceIdOut)"
[ -n "$INSTANCE" ] || { echo "FATAL: no InstanceIdOut" >&2; exit 1; }

# The token still appears in this one SSM command's parameter history; the
# tradeoff is one logged write vs. one-per-bench logged use.  For tighter
# secrecy, store in SSM Parameter Store as SecureString and fetch on-host.
echo "==> writing /etc/pulsys/hf-token on $INSTANCE"
PARAMS_JSON="$(jq -nc --arg token "$TOKEN" '{
	commands: [
		"set -euo pipefail",
		"sudo mkdir -p /etc/pulsys",
		"umask 077",
		"echo -n \"\($token)\" | sudo tee /etc/pulsys/hf-token > /dev/null",
		"sudo chmod 600 /etc/pulsys/hf-token",
		"sudo chown root:root /etc/pulsys/hf-token",
		"echo \"sha256=$(sudo cat /etc/pulsys/hf-token | sha256sum | cut -c1-8)\""
	],
	executionTimeout: ["60"]
}')"

CMD_ID="$(aws ssm send-command \
	--region "$AWS_REGION" \
	--document-name AWS-RunShellScript \
	--instance-ids "$INSTANCE" \
	--parameters "$PARAMS_JSON" \
	--query 'Command.CommandId' --output text)"

ssm_wait "$CMD_ID"
echo "==> HF_TOKEN persisted at /etc/pulsys/hf-token (mode 600, root)"
echo "    subsequent scripts/ssm-hf-download.sh runs need no local HF_TOKEN env"

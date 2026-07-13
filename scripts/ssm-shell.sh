#!/usr/bin/env bash
# ssm-shell.sh -- open an SSM Session Manager shell to the bench instance.
#
# Reads InstanceIdOut from the CDK stack outputs and runs `aws ssm
# start-session`.  Requires the Session Manager plugin to be installed
# (it's not bundled with aws-cli v2):
#   brew install --cask session-manager-plugin
#
# Env:
#   AWS_REGION       (default us-east-1)
#   HF_STACK_NAME    (default PulsysBench)
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
source "$ROOT/scripts/lib/ssm-common.sh"

INSTANCE="$(stack_output InstanceIdOut)"
if [ -z "$INSTANCE" ]; then
    echo "FATAL: could not resolve InstanceIdOut from CFN stack $HF_STACK_NAME in $AWS_REGION" >&2
    exit 1
fi
echo "==> opening SSM session to $INSTANCE (region $AWS_REGION)" >&2
exec aws ssm start-session --region "$AWS_REGION" --target "$INSTANCE"

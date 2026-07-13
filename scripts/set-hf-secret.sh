#!/usr/bin/env bash
# set-hf-secret.sh -- store the read-only Hugging Face token in Secrets Manager.
#
# One-time per account/region. The bench instance's IAM role is granted
# GetSecretValue on this secret by the CDK stack, and the on-instance bench
# scripts fetch it at run time -- so the token is never baked into an AMI,
# committed to the repo, or echoed through SSM command history.
#
# Usage:
#   HF_TOKEN=hf_xxx scripts/set-hf-secret.sh
#   scripts/set-hf-secret.sh hf_xxx
#
# Verify:
#   aws secretsmanager get-secret-value --secret-id pulsys/hf-token \
#     --query SecretString --output text | sha256sum
set -euo pipefail
: "${AWS_REGION:=us-east-1}"
SECRET_NAME="pulsys/hf-token"

TOKEN="${1:-${HF_TOKEN:-}}"
if [ -z "$TOKEN" ]; then
	echo "FATAL: pass the token as arg or HF_TOKEN env" >&2
	exit 2
fi
case "$TOKEN" in
	hf_*) ;;
	*) echo "WARN: token does not start with 'hf_' (typo?)" >&2 ;;
esac

# Validate before storing: a dead token here poisons every cold fill.
CODE="$(curl -s -o /dev/null -w '%{http_code}' \
	-H "Authorization: Bearer $TOKEN" https://huggingface.co/api/whoami-v2)"
if [ "$CODE" != "200" ]; then
	echo "FATAL: token rejected by huggingface.co (whoami-v2 -> HTTP $CODE); not storing" >&2
	exit 1
fi
echo "==> token valid (whoami-v2 200)"

if aws secretsmanager describe-secret --region "$AWS_REGION" --secret-id "$SECRET_NAME" >/dev/null 2>&1; then
	aws secretsmanager put-secret-value --region "$AWS_REGION" \
		--secret-id "$SECRET_NAME" --secret-string "$TOKEN" >/dev/null
	echo "==> updated secret $SECRET_NAME in $AWS_REGION"
else
	aws secretsmanager create-secret --region "$AWS_REGION" \
		--name "$SECRET_NAME" \
		--description "Pulsys bench: read-only Hugging Face token for cold fills" \
		--secret-string "$TOKEN" >/dev/null
	echo "==> created secret $SECRET_NAME in $AWS_REGION"
fi

#!/usr/bin/env bash
# ssm-teardown.sh -- tear down ALL pulsys benchmark infra in EC2.
#
# `cdk destroy` (or a CloudFormation delete) removes the bench instance, its
# security group, IAM role, launch template, and SSM documents.  But two things
# survive a stack delete on purpose and would otherwise cost money forever:
#
#   * the S3 results bucket (RemovalPolicy.RETAIN), and
#   * the Packer-baked AMI + its EBS snapshot + the /pulsys/*-ami/* SSM pointers.
#
# This script removes everything so a benchmark run leaves no lingering spend.
#
# Usage:
#   scripts/ssm-teardown.sh             # dry run: print exactly what would be deleted
#   scripts/ssm-teardown.sh --yes       # actually delete
#   KEEP_RESULTS=1 scripts/ssm-teardown.sh --yes   # keep the S3 results bucket
#
# Honors AWS_REGION (default us-east-1) and HF_STACK_NAME (default PulsysBench).
set -euo pipefail
: "${AWS_REGION:=us-east-1}"
: "${HF_STACK_NAME:=PulsysBench}"
: "${KEEP_RESULTS:=}"

YES=""
[ "${1:-}" = "--yes" ] && YES=1

run() {
	if [ -n "$YES" ]; then
		echo "+ $*" >&2
		"$@"
	else
		echo "[dry-run] $*" >&2
	fi
}

echo "==> region=$AWS_REGION stack=$HF_STACK_NAME keep_results=${KEEP_RESULTS:-no} mode=$([ -n "$YES" ] && echo DELETE || echo dry-run)" >&2

# ----- 1. resolve what we'll delete BEFORE we start deleting ----------------
BUCKET="$(aws cloudformation describe-stacks --region "$AWS_REGION" --stack-name "$HF_STACK_NAME" \
	--query "Stacks[0].Outputs[?OutputKey=='ResultsBucketOut'].OutputValue | [0]" --output text 2>/dev/null || true)"
[ "$BUCKET" = "None" ] && BUCKET=""

# AMI ids from the SSM pointers (latest + any per-commit copies).
AMI_IDS="$(aws ssm get-parameters-by-path --region "$AWS_REGION" --path /pulsys --recursive \
	--query "Parameters[?contains(Name, '-ami/')].Value" --output text 2>/dev/null | tr '\t' '\n' | sort -u || true)"
PARAM_NAMES="$(aws ssm get-parameters-by-path --region "$AWS_REGION" --path /pulsys --recursive \
	--query "Parameters[?contains(Name, '-ami/')].Name" --output text 2>/dev/null | tr '\t' '\n' | sort -u || true)"

echo "    results bucket: ${BUCKET:-<none>}" >&2
echo "    AMI ids:        $(echo "$AMI_IDS" | tr '\n' ' ')" >&2
echo "    SSM params:     $(echo "$PARAM_NAMES" | tr '\n' ' ')" >&2

# ----- 2. delete the CloudFormation stack -----------------------------------
STACK_STATUS="$(aws cloudformation describe-stacks --region "$AWS_REGION" --stack-name "$HF_STACK_NAME" \
	--query 'Stacks[0].StackStatus' --output text 2>/dev/null || echo MISSING)"
if [ "$STACK_STATUS" != "MISSING" ]; then
	run aws cloudformation delete-stack --region "$AWS_REGION" --stack-name "$HF_STACK_NAME"
	if [ -n "$YES" ]; then
		echo "==> waiting for stack delete to complete..." >&2
		aws cloudformation wait stack-delete-complete --region "$AWS_REGION" --stack-name "$HF_STACK_NAME" || true
	fi
else
	echo "    stack $HF_STACK_NAME not present; skipping stack delete" >&2
fi

# ----- 3. deregister AMIs + delete their snapshots --------------------------
for ami in $AMI_IDS; do
	[ -n "$ami" ] || continue
	[ "$ami" = "None" ] && continue
	SNAPS="$(aws ec2 describe-images --region "$AWS_REGION" --image-ids "$ami" \
		--query 'Images[0].BlockDeviceMappings[].Ebs.SnapshotId' --output text 2>/dev/null | tr '\t' '\n' || true)"
	run aws ec2 deregister-image --region "$AWS_REGION" --image-id "$ami"
	for snap in $SNAPS; do
		[ -n "$snap" ] && [ "$snap" != "None" ] || continue
		run aws ec2 delete-snapshot --region "$AWS_REGION" --snapshot-id "$snap"
	done
done

# ----- 4. delete the SSM AMI pointers ---------------------------------------
for p in $PARAM_NAMES; do
	[ -n "$p" ] || continue
	run aws ssm delete-parameter --region "$AWS_REGION" --name "$p"
done

# ----- 5. empty + delete the retained results bucket ------------------------
if [ -n "$BUCKET" ] && [ -z "$KEEP_RESULTS" ]; then
	run aws s3 rm "s3://$BUCKET" --recursive
	run aws s3api delete-bucket --region "$AWS_REGION" --bucket "$BUCKET"
elif [ -n "$BUCKET" ]; then
	echo "    KEEP_RESULTS set; leaving s3://$BUCKET in place" >&2
fi

if [ -z "$YES" ]; then
	echo >&2
	echo "==> dry run only. Re-run with --yes to actually delete." >&2
else
	echo "==> teardown complete." >&2
fi

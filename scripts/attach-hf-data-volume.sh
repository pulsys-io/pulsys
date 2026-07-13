#!/usr/bin/env bash
# attach-hf-data-volume.sh -- create + attach a provisioned gp3 data volume to the
# bench instance without replacing the EC2 host (avoids vCPU quota during CFN swap).
#
# Defaults: 500 GiB gp3, 16000 IOPS, 1000 MiB/s throughput (gp3 maximums).
#
# Usage:
#   scripts/attach-hf-data-volume.sh
#   SIZE_GIB=1000 IOPS=16000 THROUGHPUT=1000 scripts/attach-hf-data-volume.sh
set -euo pipefail
export PATH="/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
source "$ROOT/scripts/lib/ssm-common.sh"

SIZE_GIB="${SIZE_GIB:-500}"
IOPS="${IOPS:-16000}"
THROUGHPUT="${THROUGHPUT:-1000}"
DEVICE="${DEVICE:-/dev/sdf}"

INSTANCE="$(stack_output InstanceIdOut)"
[ -n "$INSTANCE" ] || { echo "FATAL: no InstanceIdOut" >&2; exit 1; }

AZ="$(aws ec2 describe-instances --region "$AWS_REGION" --instance-ids "$INSTANCE" \
	--query 'Reservations[0].Instances[0].Placement.AvailabilityZone' --output text)"

echo "==> creating gp3 volume ${SIZE_GIB}GiB IOPS=$IOPS throughput=${THROUGHPUT}MiB/s in $AZ"
VOL_ID="$(aws ec2 create-volume --region "$AWS_REGION" \
	--availability-zone "$AZ" \
	--volume-type gp3 \
	--size "$SIZE_GIB" \
	--iops "$IOPS" \
	--throughput "$THROUGHPUT" \
	--tag-specifications "ResourceType=volume,Tags=[{Key=Name,Value=pulsys-hf-data}]" \
	--query 'VolumeId' --output text)"

echo "==> waiting for volume $VOL_ID"
aws ec2 wait volume-available --region "$AWS_REGION" --volume-ids "$VOL_ID"

echo "==> attaching $VOL_ID to $INSTANCE as $DEVICE"
aws ec2 attach-volume --region "$AWS_REGION" \
	--volume-id "$VOL_ID" \
	--instance-id "$INSTANCE" \
	--device "$DEVICE"

echo "==> formatting + mounting via SSM"
bash "$ROOT/scripts/ssm-sync-scripts.sh" scripts >/dev/null
PARAMS_JSON="$(jq -nc --arg cmd 'sudo bash /opt/pulsys-src/scripts/mount-hf-data-volume.sh' \
	'{commands: [$cmd], executionTimeout: ["300"]}')"
CMD_ID="$(aws ssm send-command --region "$AWS_REGION" \
	--document-name AWS-RunShellScript \
	--instance-ids "$INSTANCE" \
	--parameters "$PARAMS_JSON" \
	--query 'Command.CommandId' --output text)"
ssm_wait "$CMD_ID"

echo "==> done. volume=$VOL_ID instance=$INSTANCE mount=/mnt/hf-data"
echo "    Verify: aws ec2 describe-volumes --volume-ids $VOL_ID --query 'Volumes[0].[Iops,Throughput,Size]'"

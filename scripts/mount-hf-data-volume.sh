#!/usr/bin/env bash
# mount-hf-data-volume.sh -- format and mount the dedicated hf-data EBS volume.
#
# The bench CDK stack attaches a second gp3 volume at /dev/sdf (Nitro: /dev/nvme1n1).
# Run once on a fresh instance (or after attach):
#
#   sudo bash /opt/pulsys-src/scripts/mount-hf-data-volume.sh
#
# Mount point: /mnt/hf-data  (used by hf_download_bench.sh for cache + client dirs)
set -euo pipefail

MOUNT=/mnt/hf-data

find_dev() {
	for d in /dev/nvme1n1 /dev/xvdf /dev/sdf; do
		if [ -b "$d" ]; then
			echo "$d"
			return
		fi
	done
	return 1
}

DEV="$(find_dev)" || {
	echo "FATAL: no secondary block device found (expected nvme1n1 or xvdf)" >&2
	lsblk >&2
	exit 1
}

if mountpoint -q "$MOUNT"; then
	echo "already mounted: $MOUNT"
	df -h "$MOUNT"
	exit 0
fi

if ! blkid "$DEV" | grep -q xfs; then
	echo "==> formatting $DEV as xfs"
	mkfs.xfs -f "$DEV"
fi

mkdir -p "$MOUNT"
mount "$DEV" "$MOUNT"
chmod 1777 "$MOUNT" || true

if ! grep -q "$MOUNT" /etc/fstab 2>/dev/null; then
	# Use actual device path from discovery.
	echo "$DEV $MOUNT xfs defaults,nofail 0 2" >>/etc/fstab
fi

echo "==> mounted $DEV at $MOUNT"
df -hT "$MOUNT"

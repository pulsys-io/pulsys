#!/usr/bin/env bash
# category2-tunings.sh -- install BEHAVIOURAL kernel tunings.
#
# Companion to sysctl-pulsys-category2.conf.  Handles knobs that are not
# expressible as sysctl entries:
#
#   * CPU frequency governor       (per-CPU sysfs)
#   * Transparent Huge Pages mode  (sysfs)
#   * RPS / RFS                    (per-NIC-queue sysfs + sysctl)
#   * irqbalance enable/disable    (systemd unit)
#
# Default state: every section is COMMENTED OUT.  Uncomment a section only
# after the corresponding survivor row appears in tmp/bench/tunings-report.md.
#
# This script is invoked by bench-ami.pkr.hcl as the LAST provisioner
# step.  It must be idempotent and must not fail if a sysfs path is
# missing (Graviton instances have no cpufreq, for example).
set -euxo pipefail

# ----- 1. install the sysctl file -----------------------------------------
install -m 0644 /tmp/sysctl-pulsys-category2.conf \
    /etc/sysctl.d/99-pulsys-category2.conf
sysctl --system

# ----- 2. CPU governor = performance (uncomment when survivor) ------------
# for f in /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor; do
#     echo performance > "$f" 2>/dev/null || true
# done

# ----- 3. THP = madvise (uncomment when survivor) ------------------------
# if [ -w /sys/kernel/mm/transparent_hugepage/enabled ]; then
#     echo madvise > /sys/kernel/mm/transparent_hugepage/enabled
# fi

# ----- 4. RPS / RFS across all cores (uncomment when survivor) ------------
# NCPU="$(nproc)"
# MASK="$(printf '%x' $(( (1 << NCPU) - 1 )))"
# for q in /sys/class/net/*/queues/rx-*/rps_cpus; do
#     echo "$MASK" > "$q" 2>/dev/null || true
# done
# sysctl -w net.core.rps_sock_flow_entries=32768

# ----- 5. disable irqbalance (uncomment when survivor) --------------------
# systemctl disable --now irqbalance || true

# ----- 6. provenance manifest --------------------------------------------
# Replace the placeholder written by category1-tunings.sh with the tuned
# variant.  Each uncommented section above should also append a line here
# documenting WHEN the survivor was identified and with WHAT delta.
cat >/etc/pulsys/tuning.md <<'EOF'
# pulsys AMI tuning manifest

This AMI applies **Category 1 (ceiling-lifting)** sysctls AND the
**Category 2 (behavioural)** survivors from `tmp/bench/tunings-report.md`.

See `infra/packer/files/sysctl-pulsys-category2.conf` and
`infra/packer/scripts/category2-tunings.sh` in the repo for which
survivors are active; each entry has a "sweep <date> commit <sha>
delta +X.Y%" provenance comment.

The original sweep report shipped with this AMI lives at
`/etc/pulsys/tunings-report.md` -- read it to see the full A/B table
that justified the active survivors.
EOF
chmod 0644 /etc/pulsys/tuning.md

# Clean up uploaded files.
rm -f /tmp/sysctl-pulsys-category2.conf

echo "==> category2-tunings complete"

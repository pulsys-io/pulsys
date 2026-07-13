#!/usr/bin/env bash
# category1-tunings.sh -- install only the ceiling-lifting sysctls.
#
# These are uncontroversial settings that remove kernel-imposed caps
# (rmem_max, somaxconn, file-max, etc.) without changing any defaults
# the kernel chooses inside those caps.  Safe on stock AMIs.
#
# Category 2 (behavioural) tunings -- BBR, RPS/RFS, transparent hugepages,
# CPU governor, etc. -- are deliberately NOT installed here.  Those are
# baked into the tuned AMI only after scripts/sweep_tunings.sh proves
# they win against the stock AMI on this exact workload.
set -euxo pipefail

install -m 0644 /tmp/sysctl-pulsys-category1.conf \
    /etc/sysctl.d/99-pulsys-category1.conf

install -m 0644 /tmp/limits-pulsys.conf \
    /etc/security/limits.d/99-pulsys.conf

# Apply right now so the rest of the build benefits (e.g. wrk smoke test
# during provisioning would otherwise hit the old somaxconn).
sysctl --system

# Record provenance: which AMI category this is.
cat >/etc/pulsys/tuning.md <<'EOF'
# pulsys AMI tuning manifest

This AMI applies **Category 1 (ceiling-lifting)** sysctls only.

See `infra/packer/files/sysctl-pulsys-category1.conf` in the repo for the
exact knobs and rationale.

**No Category 2 (behavioural) tunings are applied.**  This AMI is used
as the baseline for `scripts/sweep_tunings.sh`, which A/B-tests each
behavioural knob and produces `tmp/bench/tunings-report.md`.  Only knobs that
win in the sweep are baked into the tuned AMI (`bench-ami.pkr.hcl`).
EOF
chmod 0644 /etc/pulsys/tuning.md

# Clean up uploaded files.
rm -f /tmp/sysctl-pulsys-category1.conf /tmp/limits-pulsys.conf /tmp/pulsys.service

echo "==> category1-tunings complete"

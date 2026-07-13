#!/usr/bin/env bash
# build-tuned-ami.sh -- end-to-end wrapper for the Track B2 Packer build.
#
# Identical to build-stock-ami.sh but uses bench-ami.pkr.hcl and publishes
# to SSM /pulsys/bench-ami/latest instead of /pulsys/stock-ami/latest.
#
# The tuned AMI bakes:
#   * everything the stock AMI bakes (deps, binaries, systemd unit)
#   * Category 1 sysctls (same as stock)
#   * Category 2 survivors from the sweep report (tmp/bench/tunings-report.md)
#   * /etc/pulsys/tunings-report.md  (the sweep report itself, for
#                                       provenance on a running host)
#
# Run AFTER `scripts/ssm-sweep.sh` has written tmp/bench/tunings-report.md and
# you have hand-edited:
#   infra/packer/files/sysctl-pulsys-category2.conf
#   infra/packer/scripts/category2-tunings.sh
# to include only the sweep survivors.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

REGION="${AWS_REGION:-us-east-1}"
INSTANCE_TYPE="${PACKER_INSTANCE_TYPE:-c7i.large}"
AMI_VERSION="${PULSYS_AMI_VERSION:-v0.1.0}"
TUNINGS_REPORT="${PULSYS_TUNINGS_REPORT:-$ROOT/tmp/bench/tunings-report.md}"

for tool in packer aws jq git; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "FATAL: $tool not found in PATH" >&2
        exit 1
    fi
done

if [ ! -f "$TUNINGS_REPORT" ]; then
    echo "FATAL: tunings report not found at $TUNINGS_REPORT" >&2
    exit 1
fi

GIT_COMMIT="$(git rev-parse --short HEAD)"
GIT_STATUS="$(git status --porcelain | wc -l | tr -d '[:space:]')"
if [ "$GIT_STATUS" != "0" ]; then
    echo "WARNING: working tree has uncommitted changes; AMI will reflect HEAD ($GIT_COMMIT)" >&2
fi

# Quick sanity check: warn if category2 sysctl file is still a stub.  Not
# fatal -- you might intentionally be re-baking the AMI without survivors
# to confirm the build pipeline works.
if ! grep -qE '^[^#[:space:]]' "$ROOT/infra/packer/files/sysctl-pulsys-category2.conf"; then
    echo "WARNING: sysctl-pulsys-category2.conf has no active entries; tuned AMI will be functionally identical to stock." >&2
fi

TARBALL_DIR="$ROOT/infra/packer/.tarball"
mkdir -p "$TARBALL_DIR"
TARBALL="$TARBALL_DIR/pulsys-${GIT_COMMIT}.tar.gz"
git archive --format=tar.gz --output="$TARBALL" HEAD
echo "==> built source tarball: $TARBALL ($(du -h "$TARBALL" | cut -f1))"

cd infra/packer
packer init bench-ami.pkr.hcl
packer validate \
    -var "region=$REGION" \
    -var "instance_type=$INSTANCE_TYPE" \
    -var "ami_version=$AMI_VERSION" \
    -var "git_commit=$GIT_COMMIT" \
    -var "source_tarball=$TARBALL" \
    -var "tunings_report=$TUNINGS_REPORT" \
    bench-ami.pkr.hcl

PACKER_LOG="${PACKER_LOG:-0}" \
packer build \
    -var "region=$REGION" \
    -var "instance_type=$INSTANCE_TYPE" \
    -var "ami_version=$AMI_VERSION" \
    -var "git_commit=$GIT_COMMIT" \
    -var "source_tarball=$TARBALL" \
    -var "tunings_report=$TUNINGS_REPORT" \
    bench-ami.pkr.hcl

AMI_ID="$(jq -r '.builds[-1].artifact_id' manifest-bench.json | cut -d: -f2)"
if [ -z "$AMI_ID" ] || [ "$AMI_ID" = "null" ]; then
    echo "FATAL: could not parse AMI ID from manifest-bench.json" >&2
    exit 1
fi
echo "==> built tuned AMI: $AMI_ID"

aws ssm put-parameter \
    --region "$REGION" \
    --name /pulsys/bench-ami/latest \
    --type String \
    --overwrite \
    --value "$AMI_ID" >/dev/null
echo "==> published to SSM /pulsys/bench-ami/latest in $REGION"

aws ssm put-parameter \
    --region "$REGION" \
    --name "/pulsys/bench-ami/${GIT_COMMIT}" \
    --type String \
    --overwrite \
    --value "$AMI_ID" >/dev/null
echo "==> published to SSM /pulsys/bench-ami/${GIT_COMMIT} in $REGION"

echo
echo "Tuned AMI ready.  Launch via Track C CDK stack:"
echo "  cd infra/cdk && cdk deploy -c amiKind=tuned"

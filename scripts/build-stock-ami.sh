#!/usr/bin/env bash
# build-stock-ami.sh -- end-to-end wrapper for the Track B0 Packer build.
#
# Produces:
#   1. infra/packer/.tarball/pulsys-<sha>.tar.gz  (gitignored)
#      via `git archive`, so only tracked files end up on the AMI.
#   2. infra/packer/manifest.json  (Packer's build artifact manifest).
#   3. AMI ID published to SSM parameter /pulsys/stock-ami/latest in the
#      target region (consumed by Track C's CDK stack).
#
# Usage:
#   scripts/build-stock-ami.sh                     # us-east-1, c7i.large
#   AWS_REGION=us-west-2 scripts/build-stock-ami.sh
#   PACKER_LOG=1 scripts/build-stock-ami.sh        # verbose
#
# Requirements (on the BUILD host, i.e. your laptop):
#   - packer >= 1.10
#   - aws CLI v2 with credentials for the target account/region
#   - jq
#   - git (repo must be a clean checkout; uncommitted edits will NOT make it
#     into the AMI because we use `git archive` rather than tar of the
#     working tree -- this is intentional to keep AMIs reproducible).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

REGION="${AWS_REGION:-us-east-1}"
INSTANCE_TYPE="${PACKER_INSTANCE_TYPE:-c7i.large}"
AMI_VERSION="${PULSYS_AMI_VERSION:-v0.1.0}"

# ----- preflight ----------------------------------------------------------
for tool in packer aws jq git; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "FATAL: $tool not found in PATH" >&2
        exit 1
    fi
done

GIT_COMMIT="$(git rev-parse --short HEAD)"
GIT_STATUS="$(git status --porcelain | wc -l | tr -d '[:space:]')"
if [ "$GIT_STATUS" != "0" ]; then
    echo "WARNING: working tree has uncommitted changes; AMI will reflect HEAD ($GIT_COMMIT), not your edits" >&2
fi

# ----- 1. build source tarball via git archive ----------------------------
TARBALL_DIR="$ROOT/infra/packer/.tarball"
mkdir -p "$TARBALL_DIR"
TARBALL="$TARBALL_DIR/pulsys-${GIT_COMMIT}.tar.gz"
git archive --format=tar.gz --output="$TARBALL" HEAD
echo "==> built source tarball: $TARBALL ($(du -h "$TARBALL" | cut -f1))"

# ----- 2. packer init + validate ------------------------------------------
cd infra/packer
packer init stock-ami.pkr.hcl
packer validate \
    -var "region=$REGION" \
    -var "instance_type=$INSTANCE_TYPE" \
    -var "ami_version=$AMI_VERSION" \
    -var "git_commit=$GIT_COMMIT" \
    -var "source_tarball=$TARBALL" \
    stock-ami.pkr.hcl

# ----- 3. packer build ----------------------------------------------------
PACKER_LOG="${PACKER_LOG:-0}" \
packer build \
    -var "region=$REGION" \
    -var "instance_type=$INSTANCE_TYPE" \
    -var "ami_version=$AMI_VERSION" \
    -var "git_commit=$GIT_COMMIT" \
    -var "source_tarball=$TARBALL" \
    stock-ami.pkr.hcl

# ----- 4. publish AMI ID to SSM -------------------------------------------
AMI_ID="$(jq -r '.builds[-1].artifact_id' manifest.json | cut -d: -f2)"
if [ -z "$AMI_ID" ] || [ "$AMI_ID" = "null" ]; then
    echo "FATAL: could not parse AMI ID from manifest.json" >&2
    exit 1
fi
echo "==> built AMI: $AMI_ID"

aws ssm put-parameter \
    --region "$REGION" \
    --name /pulsys/stock-ami/latest \
    --type String \
    --overwrite \
    --value "$AMI_ID" >/dev/null
echo "==> published to SSM /pulsys/stock-ami/latest in $REGION"

aws ssm put-parameter \
    --region "$REGION" \
    --name "/pulsys/stock-ami/${GIT_COMMIT}" \
    --type String \
    --overwrite \
    --value "$AMI_ID" >/dev/null
echo "==> published to SSM /pulsys/stock-ami/${GIT_COMMIT} in $REGION"

echo
echo "Stock AMI ready.  Launch via Track C CDK stack:"
echo "  cd infra/cdk && cdk deploy -c amiKind=stock"

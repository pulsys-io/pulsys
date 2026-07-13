#!/usr/bin/env bash
# run-aws-benchmarks.sh -- one entry point for the HN-reproducible AWS suite.
#
# Steps (account/region come from your AWS CLI profile; nothing account-specific
# is committed to the repo):
#
#   1. Ensure stock AMI exists (build if missing)
#   2. Deploy PulsysBench (default c7i.12xlarge)
#   3. Persist HF token on the instance (for cold fills)
#   4. Sync scripts + rebuild binary from this checkout
#   5. Saturation bench (io_uring) -> docs/results/ec2/
#   6. Real hf + hf_transfer download (cold then warm loopback) -> cast rate
#   7. Render website/public/demos/hf-warm-demo.cast from the warm row
#
# Usage:
#   HF_TOKEN=hf_xxx scripts/run-aws-benchmarks.sh
#   HF_TOKEN=hf_xxx scripts/run-aws-benchmarks.sh --skip-ami   # AMI already published
#   HF_TOKEN=hf_xxx scripts/run-aws-benchmarks.sh --teardown   # destroy stack at end
#
# Env:
#   AWS_REGION          default us-east-1
#   HF_STACK_NAME       default PulsysBench
#   INSTANCE_TYPE       default c7i.12xlarge (override: c7i.4xlarge for cheaper loops)
#   HF_BENCH_MODEL      default Qwen/Qwen2.5-7B-Instruct (landing-page demo model)
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

: "${AWS_REGION:=us-east-1}"
: "${HF_STACK_NAME:=PulsysBench}"
: "${INSTANCE_TYPE:=c7i.12xlarge}"
: "${HF_BENCH_MODEL:=Qwen/Qwen2.5-7B-Instruct}"
export AWS_REGION HF_STACK_NAME

SKIP_AMI=0
TEARDOWN=0
for arg in "$@"; do
	case "$arg" in
		--skip-ami) SKIP_AMI=1 ;;
		--teardown) TEARDOWN=1 ;;
		-h|--help)
			sed -n '2,30p' "$0"
			exit 0
			;;
		*)
			echo "FATAL: unknown arg '$arg'" >&2
			exit 2
			;;
	esac
done

if [ -z "${HF_TOKEN:-}" ] && [ -z "${PULSYS_HF_TOKEN:-}" ]; then
	echo "FATAL: set HF_TOKEN (or PULSYS_HF_TOKEN) for cold fills against huggingface.co" >&2
	exit 2
fi
export HF_TOKEN="${HF_TOKEN:-$PULSYS_HF_TOKEN}"
export PULSYS_HF_TOKEN="${PULSYS_HF_TOKEN:-$HF_TOKEN}"

need() { command -v "$1" >/dev/null 2>&1 || { echo "FATAL: $1 required" >&2; exit 1; }; }
need aws
need jq
need npm

echo "==> identity"
aws sts get-caller-identity --output table

if [ "$SKIP_AMI" = 0 ]; then
	if ! aws ssm get-parameter --name /pulsys/stock-ami/latest --query Parameter.Value --output text >/dev/null 2>&1; then
		echo "==> no /pulsys/stock-ami/latest; building stock AMI (SKIP_DINGOSPEED_BUILD=1)"
		need packer
		SKIP_DINGOSPEED_BUILD=1 bash "$ROOT/scripts/build-stock-ami.sh"
	else
		AMI="$(aws ssm get-parameter --name /pulsys/stock-ami/latest --query Parameter.Value --output text)"
		echo "==> using existing stock AMI $AMI"
	fi
else
	aws ssm get-parameter --name /pulsys/stock-ami/latest --query Parameter.Value --output text >/dev/null
	echo "==> --skip-ami: using existing /pulsys/stock-ami/latest"
fi

ACCOUNT="$(aws sts get-caller-identity --query Account --output text)"
export CDK_DEFAULT_ACCOUNT="$ACCOUNT"
export CDK_DEFAULT_REGION="$AWS_REGION"

echo "==> deploy $HF_STACK_NAME ($INSTANCE_TYPE) in $AWS_REGION"
(
	cd "$ROOT/infra/cdk"
	npm install --silent
	npx cdk deploy \
		-c amiKind=stock \
		-c "instanceType=$INSTANCE_TYPE" \
		-c "stackName=$HF_STACK_NAME" \
		--require-approval never
)

echo "==> wait for SSM Online"
INSTANCE="$(aws cloudformation describe-stacks --stack-name "$HF_STACK_NAME" \
	--query "Stacks[0].Outputs[?OutputKey=='InstanceIdOut'].OutputValue" --output text)"
for i in $(seq 1 60); do
	STATUS="$(aws ssm describe-instance-information \
		--filters "Key=InstanceIds,Values=$INSTANCE" \
		--query 'InstanceInformationList[0].PingStatus' --output text 2>/dev/null || true)"
	if [ "$STATUS" = "Online" ]; then
		echo "    SSM Online ($INSTANCE)"
		break
	fi
	if [ "$i" = 60 ]; then
		echo "FATAL: instance $INSTANCE never reached SSM Online" >&2
		exit 1
	fi
	sleep 10
done

echo "==> persist HF token on instance (once)"
bash "$ROOT/scripts/ssm-set-hf-token.sh"

echo "==> sync working tree + rebuild pulsys on instance"
bash "$ROOT/scripts/ssm-sync-scripts.sh" full

echo "==> saturation bench (io_uring)"
bash "$ROOT/scripts/ssm-bench.sh" variant=saturate-iouring duration=30s

echo "==> hf + hf_transfer download (cold then warm over loopback)"
bash "$ROOT/scripts/ssm-hf-download.sh" "model=$HF_BENCH_MODEL" skip_direct=1

echo "==> render hero cast from warm row"
bash "$ROOT/scripts/render_hero_cast.sh" \
	"$ROOT/tmp/bench/ec2/hf-download/results.csv" \
	"$ROOT/website/public/demos/hf-warm-demo.cast" \
	"$HF_BENCH_MODEL"

if [ "$TEARDOWN" = 1 ]; then
	echo "==> teardown"
	bash "$ROOT/scripts/ssm-teardown.sh"
else
	echo
	echo "==> leave stack up. Tear down when done:"
	echo "    scripts/ssm-teardown.sh"
fi

echo
echo "==> done"
echo "    saturation artifacts: docs/results/ec2/"
echo "    hf download CSV:      tmp/bench/ec2/hf-download/results.csv"
echo "    hero cast:              website/public/demos/hf-warm-demo.cast"

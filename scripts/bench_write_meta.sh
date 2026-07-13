#!/usr/bin/env bash
# bench_write_meta.sh -- sidecar JSON for chart renderers (platform, host, vCPU).
#
#   bench_write_meta.sh matrix tmp/bench
#   BENCH_PLATFORM=ec2 bench_write_meta.sh saturate tmp/bench
set -euo pipefail

KIND="${1:-matrix}"
DIR="${2:-tmp/bench}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

platform="${BENCH_PLATFORM:-}"
if [ -z "$platform" ]; then
	case "$(uname -s)" in
		Darwin) platform=darwin ;;
		Linux) platform=ec2 ;;
		*) platform="$(uname -s | tr '[:upper:]' '[:lower:]')" ;;
	esac
fi

ncpu="${BENCH_META_VCPU:-$(getconf _NPROCESSORS_ONLN 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 0)}"
host="$(hostname -s 2>/dev/null || hostname)"
goos="$(go env GOOS 2>/dev/null || echo unknown)"
goarch="$(go env GOARCH 2>/dev/null || echo unknown)"
# Local chart regen for EC2/saturate CSVs: meta is written on the laptop.
if [ "$platform" = ec2 ] || [ "$platform" = saturate ]; then
	if [ "$(uname -s)" = Darwin ]; then
		goos="${BENCH_META_GOOS:-linux}"
		goarch="${BENCH_META_GOARCH:-amd64}"
	fi
fi
variant="${PULSYS_VARIANT:-default}"
instance="${EC2_INSTANCE_TYPE:-}"

mkdir -p "$DIR"
META="$DIR/matrix.meta.json"
cat >"$META" <<EOF
{
  "bench": "$KIND",
  "platform": "$platform",
  "hostname": "$host",
  "vcpu": $ncpu,
  "goos": "$goos",
  "goarch": "$goarch",
  "variant": "$variant",
  "instance_type": "$instance",
  "recorded_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
EOF
echo "wrote $META (platform=$platform bench=$KIND)"

#!/usr/bin/env bash
# render_charts.sh -- render benchmark charts.
#
#   scripts/render_charts.sh darwin   # vs DingoSpeed on Mac → docs/results/darwin/
#   scripts/render_charts.sh ec2      # pulsys on metal     → docs/results/ec2/
set -euo pipefail
export PATH="/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

SUITE="${1:?usage: render_charts.sh darwin|ec2}"

case "$SUITE" in
darwin)
	export BENCH_PLATFORM=darwin
	export BENCH_MATRIX="${BENCH_MATRIX:-tmp/bench/darwin/matrix.csv}"
	export BENCH_CHARTS_DIR=docs/results/darwin
	go run scripts/render_bench_matrix.go scripts/render_bench_matrix_extras.go
	OUT=docs/results/darwin
	;;
ec2)
	export SATURATE_CHARTS_DIR=docs/results/ec2
	if [ -z "${SATURATE_MATRIX:-}" ]; then
		for cand in tmp/bench/ec2/matrix.csv tmp/bench/matrix-saturate.csv; do
			[ -f "$cand" ] && export SATURATE_MATRIX="$cand" && break
		done
	fi
	go run scripts/render_saturate_charts.go
	OUT=docs/results/ec2
	;;
*)
	echo "FATAL: unknown suite $SUITE (darwin|ec2)" >&2
	exit 1
	;;
esac

if command -v rsvg-convert >/dev/null 2>&1; then
	for svg in "$OUT"/*.svg; do
		[ -f "$svg" ] || continue
		rsvg-convert -w 2200 "$svg" -o "${svg%.svg}.png"
	done
fi

echo "==> charts under $OUT/"

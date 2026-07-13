#!/usr/bin/env bash
# ssm-bench.sh -- run the full-machine pulsys benchmark on the bench
# instance and download results to tmp/bench/ec2/ + render docs/results/ec2/.
#
# Runs scripts/bench_saturate.sh on EC2 (pulsys alone, wrk at high
# concurrency, no DingoSpeed).  Each run is tagged by PULSYS variant
# (saturate, saturate-no-cork, saturate-iouring) so back-to-back runs
# accumulate into one comparison chart.
#
# Usage:
#   scripts/ssm-bench.sh                                   # default = saturate
#   scripts/ssm-bench.sh variant=saturate-no-cork          # cork-off A/B
#   scripts/ssm-bench.sh variant=saturate-iouring          # io_uring A/B
#   scripts/ssm-bench.sh duration=60s
#   scripts/ssm-bench.sh conns=512 payloads="4k 256k 4m 16m"
#
# After a run the merged matrix lives at:
#   tmp/bench/ec2/matrix.csv          (all variants concatenated)
#   tmp/bench/ec2/matrix-<variant>.csv (one file per variant; kept across runs)
#
# Reset accumulated history:
#   rm tmp/bench/ec2/matrix-*.csv
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
source "$ROOT/scripts/lib/ssm-common.sh"

DEST="$ROOT/tmp/bench/ec2"
mkdir -p "$DEST"

# Parse bench SSM parameters (only key=value pairs the document accepts).
VARIANT="saturate"
SSM_ARGS=()
for arg in "$@"; do
	case "$arg" in
		variant=*) VARIANT="${arg#variant=}"; SSM_ARGS+=("$arg") ;;
		duration=*|conns=*|payloads=*|rounds=*)
			SSM_ARGS+=("$arg")
			;;
		*)
			echo "FATAL: unknown argument '$arg'" >&2
			echo "  Use only: variant=... duration=... conns=... payloads=... rounds=..." >&2
			echo "  Example:  scripts/ssm-bench.sh variant=saturate-no-cork" >&2
			exit 2
			;;
	esac
done
case "$VARIANT" in
	saturate|saturate-no-cork|saturate-iouring) ;;
	*)
		echo "FATAL: invalid variant '$VARIANT' (want saturate | saturate-no-cork | saturate-iouring)" >&2
		exit 2
		;;
esac

DOC_NAME="$(stack_output RunBenchCmdOut 2>/dev/null | sed -n 's/.*--document-name \([^ ]*\) .*/\1/p' || true)"
if [ -z "$DOC_NAME" ]; then
	echo "FATAL: RunBenchCmdOut missing.  Redeploy infra/cdk." >&2
	exit 1
fi

echo "==> dispatching $DOC_NAME (variant=$VARIANT) with args: ${SSM_ARGS[*]:-<defaults>}" >&2
CMD_ID="$(ssm_send "$DOC_NAME" "${SSM_ARGS[@]}")"
echo "==> command id $CMD_ID" >&2

LOG="$DEST/last-run.log"
ssm_wait "$CMD_ID" | tee "$LOG"

echo "==> downloading bench artifacts from s3://*/bench/ into $DEST" >&2
pull_s3 bench "$DEST"

# pull_s3 mirrors the S3 layout: $DEST/<TS>-<variant>/matrix.csv.
# Pick the newest dir by the UTC timestamp in the name (20260516T210423Z-…),
# NOT filesystem mtime — re-downloading old prefixes touches their mtimes and
# would otherwise beat the run we just finished.
pick_latest_run_dir() {
	local variant="$1" dir best="" ts best_ts=""
	for dir in "$DEST"/*-"${variant}"/; do
		[ -d "$dir" ] || continue
		[ -f "${dir}matrix.csv" ] || continue
		ts="$(basename "$dir")"
		ts="${ts%-${variant}}"
		if [ -z "$best_ts" ] || [[ "$ts" > "$best_ts" ]]; then
			best="$dir"
			best_ts="$ts"
		fi
	done
	printf '%s' "$best"
}

LATEST_RUN="$(pick_latest_run_dir "$VARIANT")"
if [ -z "$LATEST_RUN" ]; then
	echo "FATAL: no matrix.csv found under $DEST/*-${VARIANT}/" >&2
	exit 1
fi
echo "==> using run dir $(basename "$LATEST_RUN") (newest by S3 key timestamp)"

# Sanity check: did the EC2 actually launch with PULSYS_VARIANT we asked
# for?  If not, the SSM doc on AWS is stale (CDK redeploy required) and the
# matrix has nothing useful for this variant.
ROW_COUNT="$(grep -c "^pulsys-${VARIANT}," "$LATEST_RUN/matrix.csv" 2>/dev/null || true)"
[ -z "$ROW_COUNT" ] && ROW_COUNT=0
if [ "$ROW_COUNT" -eq 0 ]; then
	# Maybe an even-newer dir exists but was mis-tagged; scan all variant dirs.
	for dir in "$DEST"/*-"${VARIANT}"/; do
		[ -d "$dir" ] || continue
		[ -f "${dir}matrix.csv" ] || continue
		c="$(grep -c "^pulsys-${VARIANT}," "${dir}matrix.csv" 2>/dev/null || true)"
		if [ "${c:-0}" -gt 0 ]; then
			LATEST_RUN="$dir"
			ROW_COUNT="$c"
			echo "==> recovered from $(basename "$LATEST_RUN") ($ROW_COUNT rows)" >&2
			break
		fi
	done
fi
if [ "$ROW_COUNT" -eq 0 ]; then
	echo >&2
	echo "================  VARIANT MISMATCH  ================" >&2
	echo "Asked for variant=${VARIANT} but the matrix.csv from this run" >&2
	echo "has zero pulsys-${VARIANT} rows.  The SSM document on AWS" >&2
	echo "is older than bench-docs.ts and is not forwarding SATURATE_VARIANT." >&2
	echo >&2
	echo "Fix: redeploy CDK so the SSM doc picks up SATURATE_VARIANT={{variant}}:" >&2
	echo "    (cd infra/cdk && npx cdk deploy --require-approval never)" >&2
	echo "Then re-run:" >&2
	echo "    scripts/ssm-bench.sh variant=${VARIANT}" >&2
	echo "====================================================" >&2
	echo >&2
	echo "Server actually ran:" >&2
	awk -F, 'NR>1 {print $1}' "$LATEST_RUN/matrix.csv" | sort -u | sed 's/^/  /' >&2
	exit 2
fi

# Stash this variant's CSV separately so back-to-back runs of different
# variants accumulate into the chart.  Each per-variant file is the
# canonical record for that variant; matrix.csv is the merged view.
HEAD="$(head -1 "$LATEST_RUN/matrix.csv")"
{
	echo "$HEAD"
	grep "^pulsys-${VARIANT}," "$LATEST_RUN/matrix.csv" || true
} >"$DEST/matrix-${VARIANT}.csv"
echo "==> stored matrix-${VARIANT}.csv ($ROW_COUNT rows)"

# Also pull the saturate report + tarball into convenient flat locations.
[ -f "$LATEST_RUN/report.md" ] && cp "$LATEST_RUN/report.md" "$DEST/saturate-report.md"
for tarball in "$LATEST_RUN"/saturate-*.tar.gz; do
	[ -f "$tarball" ] && cp "$tarball" "$DEST/"
done

# Rebuild the cumulative matrix.csv from all per-variant files.
# render_saturate_charts.go pins "saturate" first in the legend so the
# order on disk doesn't matter.
if ls "$DEST"/matrix-*.csv >/dev/null 2>&1; then
	# All per-variant files share the same CSV header; pick the first.
	HEADER="$(head -1 "$(ls "$DEST"/matrix-*.csv | head -1)")"
	{
		echo "$HEADER"
		for f in "$DEST"/matrix-*.csv; do
			tail -n +2 "$f"
		done
	} >"$DEST/matrix.csv"
	N="$(ls "$DEST"/matrix-*.csv | wc -l | tr -d ' ')"
	echo "==> merged $N variant file(s) into matrix.csv"
fi

# docs/results/ec2/ is intentionally empty in source control (only generated
# output lives here), so recreate it before copying the regenerated report.
mkdir -p "$ROOT/docs/results/ec2"
[ -f "$DEST/saturate-report.md" ] && cp "$DEST/saturate-report.md" "$ROOT/docs/results/ec2/report.md"

# Headline numbers (single source of truth for templatized doc claims).
if [ -f "$LATEST_RUN/headline.json" ]; then
	cp "$LATEST_RUN/headline.json" "$ROOT/docs/results/ec2/headline.json"
	echo "==> copied headline.json from $(basename "$LATEST_RUN")"
fi

bash "$ROOT/scripts/render_charts.sh" ec2

# Refresh the templatized claims in README/docs from the measured headline,
# so the prose always describes the instance this run actually measured.
if [ -f "$ROOT/docs/results/ec2/headline.json" ]; then
	bash "$ROOT/scripts/update_claims.sh" "$ROOT/docs/results/ec2/headline.json"
fi

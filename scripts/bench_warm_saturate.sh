#!/usr/bin/env bash
# bench_warm_saturate.sh -- measure warm-proxy throughput with parallel
# range GETs to /dev/null (client discard).  Separates proxy ceiling from
# Python hf download wallclock.
#
# Run on EC2 after the proxy cache is warm (e.g. after hf_download_bench cold).
#
# Usage:
#   HF_ENDPOINT=http://127.0.0.1:18080 \
#     scripts/bench_warm_saturate.sh Qwen/Qwen2.5-7B-Instruct
#   scripts/bench_warm_saturate.sh Qwen/Qwen2.5-7B-Instruct 384 16
#     # model, concurrency, chunk_mib
set -euo pipefail
export PATH="/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

MODEL="${1:?model id required (e.g. Qwen/Qwen2.5-7B-Instruct)}"
CONCURRENCY="${2:-${CONCURRENCY:-128}}"
CHUNK_MIB="${3:-${CHUNK_MIB:-16}}"
MIN_MIB="${MIN_MIB:-4}"
PORT="${PORT:-18080}"
REVISION="${REVISION:-main}"
RESULTS="${RESULTS:-/var/log/pulsys-real/saturate.csv}"

ENDPOINT="${HF_ENDPOINT:-http://127.0.0.1:$PORT}"
ENDPOINT="${ENDPOINT%/}"
CHUNK=$((CHUNK_MIB * 1024 * 1024))
MIN_BYTES=$((MIN_MIB * 1024 * 1024))

auth_hdr=()
if [ -n "${HF_TOKEN:-}" ]; then
	auth_hdr=(-H "Authorization: Bearer $HF_TOKEN")
fi

echo "================================================================"
echo "  bench_warm_saturate"
echo "  model=$MODEL  endpoint=$ENDPOINT  concurrency=$CONCURRENCY"
echo "  chunk=${CHUNK_MIB}MiB  min_file=${MIN_MIB}MiB"
echo "================================================================"

# List large files via Hub tree API (recursive).
tree_json="$(curl -sf "${auth_hdr[@]}" \
	"$ENDPOINT/api/models/$MODEL/tree/$REVISION?recursive=1")" || {
	echo "FATAL: could not fetch file tree from $ENDPOINT"
	exit 1
}

export MIN_BYTES
mapfile -t ARTIFACTS < <(python3 -c '
import json, os, sys
min_bytes = int(os.environ["MIN_BYTES"])
data = json.loads(sys.stdin.read())
out = []
for e in data:
    if e.get("type") != "file":
        continue
    path = e.get("path") or ""
    size = int(e.get("size") or 0)
    if size >= min_bytes:
        out.append((path, size))
for path, size in sorted(out, key=lambda x: -x[1]):
    print(f"{path}\t{size}")
' <<<"$tree_json")

if [ "${#ARTIFACTS[@]}" -eq 0 ]; then
	echo "FATAL: no files >= ${MIN_MIB} MiB in tree"
	exit 1
fi

total_bytes=0
declare -a URLS=()
for line in "${ARTIFACTS[@]}"; do
	path="${line%%	*}"
	size="${line##*	}"
	total_bytes=$((total_bytes + size))
	# Encode path segments for URL (spaces rare in HF paths).
	enc_path="$(
		python3 -c 'import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1], safe="/"))' "$path"
	)"
	URLS+=("$ENDPOINT/$MODEL/resolve/$REVISION/$enc_path	$size")
done

echo "  artifacts: ${#ARTIFACTS[@]}  total_bytes: $total_bytes"

# Build range tasks: each (url, range_header).
TASK_FILE="$(mktemp)"
trap 'rm -f "$TASK_FILE"' EXIT
for entry in "${URLS[@]}"; do
	url="${entry%%	*}"
	size="${entry##*	}"
	if [ "$size" -le "$CHUNK" ]; then
		printf '%s\t\n' "$url" >>"$TASK_FILE"
		continue
	fi
	off=0
	while [ "$off" -lt "$size" ]; do
		end=$((off + CHUNK - 1))
		[ "$end" -ge "$size" ] && end=$((size - 1))
		printf '%s\tbytes=%d-%d\n' "$url" "$off" "$end" >>"$TASK_FILE"
		off=$((end + 1))
	done
done
task_count=$(wc -l <"$TASK_FILE" | tr -d ' ')
echo "  range_tasks: $task_count"

run_wave() {
	local label="$1"
	local t0 t1 wall gbps mibps
	local active=0
	t0=$(python3 -c 'import time; print(f"{time.time():.6f}")')
	while IFS=$'\t' read -r url range; do
		(
			if [ -n "$range" ]; then
				curl -sf "${auth_hdr[@]}" -H "Range: $range" "$url" -o /dev/null
			else
				curl -sf "${auth_hdr[@]}" "$url" -o /dev/null
			fi
		) &
		active=$((active + 1))
		if [ "$active" -ge "$CONCURRENCY" ]; then
			wait -n 2>/dev/null || wait
			active=$((active - 1))
		fi
	done <"$TASK_FILE"
	wait || return 1
	t1=$(python3 -c 'import time; print(f"{time.time():.6f}")')
	wall=$(awk -v a="$t0" -v b="$t1" 'BEGIN { printf("%.3f\n", b - a) }')
	mibps=$(awk -v b="$total_bytes" -v w="$wall" 'BEGIN { if (w<=0) print 0; else printf("%.0f\n", b/w/1024/1024) }')
	gbps=$(awk -v b="$total_bytes" -v w="$wall" 'BEGIN { if (w<=0) print 0; else printf("%.2f\n", b*8/w/1e9) }')
	gbs=$(awk -v b="$total_bytes" -v w="$wall" 'BEGIN { if (w<=0) print 0; else printf("%.2f\n", b/w/1e9) }')
	printf "  %-12s wall=%-8ss  %6s MiB/s  %5s Gb/s  %5s GB/s\n" \
		"$label" "$wall" "$mibps" "$gbps" "$gbs"
	mkdir -p "$(dirname "$RESULTS")"
	echo "saturate,$label,$wall,$total_bytes,$mibps,$gbps,$gbs,$CONCURRENCY,$CHUNK_MIB" >>"$RESULTS"
}

echo
echo "--- warm saturate (parallel curl -> /dev/null) ---"
for round in 1 2 3; do
	run_wave "round$round" || exit 1
done

echo
echo "results: $RESULTS"

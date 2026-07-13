#!/usr/bin/env bash
# Quick diagnostic: simulate hf download's 8x parallel range pattern on a
# warm cache hit and report wallclock for pulsys vs DingoSpeed.
set -euo pipefail
export PATH="/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

SHARD_SIZE=$((256 * 1024 * 1024))
ranges=()
chunk=$((SHARD_SIZE / 8))
for i in 0 1 2 3 4 5 6 7; do
	start=$((i * chunk))
	end=$(( (i+1) * chunk - 1 ))
	ranges+=("bytes=${start}-${end}")
done

bench_proxy() {
	local name=$1 base=$2 path=$3
	for round in 1 2 3; do
		t0=$(perl -MTime::HiRes=time -e 'printf("%.6f", time)')
		for r in "${ranges[@]}"; do
			curl -s -H "Range: $r" "${base}${path}" -o /dev/null &
		done
		wait
		t1=$(perl -MTime::HiRes=time -e 'printf("%.6f", time)')
		awk -v a="$t0" -v b="$t1" -v n="$name" -v r="$round" \
			'BEGIN { printf("%-12s round %d: %.3fs\n", n, r, b - a) }'
	done
}

path="/bench/multi/resolve/main/model-00001-of-00003.safetensors"
echo "=== pulsys 8x parallel range x3 ==="
bench_proxy pulsys "http://127.0.0.1:18080" "$path"
echo "=== dingospeed 8x parallel range x3 ==="
bench_proxy dingospeed "http://127.0.0.1:18686" "$path"

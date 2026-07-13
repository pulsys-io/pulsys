#!/usr/bin/env bash
# bench_e2e.sh -- end-to-end "hf download" wallclock benchmark.
#
# Measures total wall time for `hf download bench/multi` (a synthetic
# 768 MiB model with 8 files: config + tokenizer + 3 x 256 MiB shards)
# through each HF-aware proxy, under TWO upstream conditions:
#
#   loopback   fake-hf served at full loopback speed.  Measures the
#              PROXY OVERHEAD delta vs. talking to the upstream
#              directly -- "how much does the proxy add when the
#              upstream is already fast?"
#
#   remote     fake-hf shaped to 100 Mbit / 20 ms RTT.  Simulates a
#              decent home/office connection to hf.co.  Measures the
#              PROXY VALUE: the first download crawls at upstream
#              speed, every subsequent download lands at loopback
#              speed -- the entire reason this thing exists.
#
# Comparison set (only Go-native HF-aware proxies; Python-based Olah
# was dropped because its throughput is so far below the Go floor that
# co-plotting it crushes the y-axis without revealing useful contrast):
#
#   direct        baseline: hf CLI -> fake-hf with no proxy in between
#   pulsys      this project
#   dingospeed    https://github.com/dingodb/dingospeed (Go)
#
# For each (upstream-condition, proxy) pair, two timings are reported:
#
#   cold   proxy cache empty.  Proxy must fetch from fake-hf, stream
#          to client, and write to its on-disk cache.
#   warm   proxy cache populated.  Client cache cleared between runs
#          (--force-download).  Proxy serves entirely from local disk.
#
# Output:
#   tmp/bench/e2e_results.csv
#       upstream,proxy,scenario,wallclock_s,bytes_per_s,model_size_bytes
#
# Usage:
#   scripts/bench_e2e.sh                 # both conditions, 3 rounds each
#   scripts/bench_e2e.sh loopback 1      # one condition, 1 round
set -euo pipefail

CONDS="${1:-loopback remote}"
ROUNDS="${2:-3}"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BENCH="$ROOT/tmp/bench"
LOGS="$BENCH/logs"
RESULTS="$BENCH/e2e_results.csv"

PORT_FAKEHF=18484
PORT_HFPROXY=18080
PORT_DINGOSPEED=18686

REPO="bench/multi"
WORKERS=8

# Total bytes a `hf download bench/multi` retrieves -- must match the
# multiModelFiles list in cmd/fake-hf/main.go.
MODEL_SIZE=$(( (1 << 10) + (256 << 10) + (4 << 10) + (1 << 10) + (8 << 10) + 3 * (256 << 20) ))

mkdir -p "$LOGS"
echo "upstream,proxy,scenario,wallclock_s,bytes_per_s,model_size_bytes" > "$RESULTS"

# Build / sanity-check binaries.
(cd "$ROOT" &&
	go build -o "$BENCH/bin/pulsys" ./cmd/pulsys &&
	go build -o "$BENCH/bin/fake-hf"  ./cmd/fake-hf)

if [ ! -x "$BENCH/dingospeed/bin/dingospeed" ]; then
	echo "DingoSpeed not built.  Run scripts/bench_compare.sh once first."
	exit 1
fi

cleanup() {
	pkill -f "tmp/bench/bin/fake-hf"   2>/dev/null || true
	pkill -f "tmp/bench/bin/pulsys"  2>/dev/null || true
	pkill -f "tmp/bench/dingospeed/bin/dingospeed" 2>/dev/null || true
	sleep 0.3
}
trap cleanup EXIT
cleanup

start_fakehf() {
	# $1 = upstream condition: "loopback" or "remote"
	# (set -u + empty arrays don't compose well in bash 3.2 on macOS,
	# so we expand to plain args rather than an array.)
	case "$1" in
		loopback)
			"$BENCH/bin/fake-hf" -listen "127.0.0.1:$PORT_FAKEHF" \
				>"$LOGS/e2e-fakehf.out" 2>&1 &
			;;
		remote)
			"$BENCH/bin/fake-hf" -listen "127.0.0.1:$PORT_FAKEHF" \
				-latency 20ms -bandwidth 100mbit \
				>"$LOGS/e2e-fakehf.out" 2>&1 &
			;;
		*) echo "bad upstream cond: $1"; exit 2 ;;
	esac
	for _ in $(seq 1 40); do
		if curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT_FAKEHF/api/models/bench/multi" | grep -q 200; then
			return
		fi
		sleep 0.1
	done
	echo "fake-hf failed to come up"
	exit 1
}

start_proxy() {
	# $1 = proxy label; spawns the proxy pointing at fake-hf.  Caller
	# is responsible for cleanup() before calling start_proxy again.
	case "$1" in
		pulsys)
			rm -rf "$BENCH/pulsys-cache-e2e"
			"$BENCH/bin/pulsys" \
				-listen "127.0.0.1:$PORT_HFPROXY" \
				-admin-listen "127.0.0.1:18099" \
				-cache-dir "$BENCH/pulsys-cache-e2e" \
				-default-upstream-host "127.0.0.1:$PORT_FAKEHF" \
				-upstream-scheme http \
				-public-base-url "http://127.0.0.1:$PORT_HFPROXY" \
				-allow-host 127.0.0.1 \
				-log-level warn \
				>"$LOGS/e2e-pulsys.out" 2>&1 &
			wait_for "$PORT_HFPROXY"
			;;
		dingospeed)
			rm -rf "$BENCH/dingospeed/repos"
			mkdir -p "$BENCH/dingospeed/repos"
			( cd "$BENCH/dingospeed" && ./bin/dingospeed ) \
				>"$LOGS/e2e-dingospeed.out" 2>&1 &
			wait_for "$PORT_DINGOSPEED"
			;;
		*) echo "unknown proxy: $1"; exit 2 ;;
	esac
}

stop_proxies() {
	pkill -f "tmp/bench/bin/pulsys"               2>/dev/null || true
	pkill -f "tmp/bench/dingospeed/bin/dingospeed"  2>/dev/null || true
	sleep 0.3
}

wait_for() {
	local port="$1"
	for _ in $(seq 1 80); do
		if curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/api/models/bench/multi" | grep -qE '^(200|204)$'; then
			return
		fi
		sleep 0.1
	done
	echo "service on port $port failed to come up"
	exit 1
}

# Times a single `hf download` invocation, reporting wall seconds with
# millisecond precision.  Suppresses progress bars so the wallclock is
# accurate (--quiet is huggingface_hub's switch).
time_one_dl() {
	local target="$1" endpoint="$2"
	rm -rf "$target"
	local t0 t1
	t0=$(perl -MTime::HiRes=time -e 'printf("%.6f\n", time)')
	HF_ENDPOINT="$endpoint" \
	HF_HUB_ENABLE_HF_TRANSFER=1 \
	HF_HUB_DISABLE_TELEMETRY=1 \
	HF_HUB_DISABLE_IMPLICIT_TOKEN=1 \
	HF_HUB_DOWNLOAD_TIMEOUT=120 \
	hf download "$REPO" --local-dir "$target" --max-workers "$WORKERS" --quiet \
		>/dev/null 2>&1 || true
	t1=$(perl -MTime::HiRes=time -e 'printf("%.6f\n", time)')
	awk -v a="$t0" -v b="$t1" 'BEGIN { printf("%.3f\n", b - a) }'
}

emit_csv() {
	local upstream="$1" proxy="$2" scenario="$3" wall="$4"
	local bps
	bps=$(awk -v w="$wall" -v s="$MODEL_SIZE" 'BEGIN { if (w <= 0) print 0; else printf("%.0f\n", s / w) }')
	printf "%-9s %-12s %-5s wall=%-9ss bps=%-10s\n" \
		"$upstream" "$proxy" "$scenario" "$wall" "$bps"
	echo "$upstream,$proxy,$scenario,$wall,$bps,$MODEL_SIZE" >> "$RESULTS"
}

run_proxy_scenarios() {
	local upstream="$1" proxy="$2"
	echo "    ---- $proxy ----"
	stop_proxies
	start_proxy "$proxy"
	local port
	case "$proxy" in
		pulsys)   port=$PORT_HFPROXY ;;
		dingospeed) port=$PORT_DINGOSPEED ;;
	esac

	for round in $(seq 1 "$ROUNDS"); do
		# Cold: proxy cache empty.  We restart the proxy here so each
		# round is a true cold round.
		stop_proxies
		start_proxy "$proxy"
		w=$(time_one_dl "/tmp/hfdl-cold-$proxy-r$round" "http://127.0.0.1:$port")
		emit_csv "$upstream" "$proxy" "cold" "$w"

		# Warm: proxy cache already populated by the cold run above,
		# so we skip the restart and just clear the client side.
		w=$(time_one_dl "/tmp/hfdl-warm-$proxy-r$round" "http://127.0.0.1:$port")
		emit_csv "$upstream" "$proxy" "warm" "$w"
	done
	stop_proxies
}

run_direct_scenarios() {
	local upstream="$1"
	echo "    ---- direct (no proxy) ----"
	for round in $(seq 1 "$ROUNDS"); do
		w=$(time_one_dl "/tmp/hfdl-direct-r$round" "http://127.0.0.1:$PORT_FAKEHF")
		emit_csv "$upstream" "direct" "cold" "$w"
		# A "warm" scenario for direct is meaningless (no proxy cache)
		# but we record it as identical to cold so the CSV shape is
		# uniform for the renderer.
		w=$(time_one_dl "/tmp/hfdl-direct-w-r$round" "http://127.0.0.1:$PORT_FAKEHF")
		emit_csv "$upstream" "direct" "warm" "$w"
	done
}

for upstream in $CONDS; do
	echo
	echo "==================  upstream condition: $upstream  =================="
	cleanup
	start_fakehf "$upstream"

	run_direct_scenarios "$upstream"
	run_proxy_scenarios  "$upstream" "pulsys"
	run_proxy_scenarios  "$upstream" "dingospeed"
done

echo
echo "results: $RESULTS"

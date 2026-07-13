#!/usr/bin/env bash
# bench_startup.sh -- time from binary exec to first successful 200.
#
# Why this matters: pulsys ships as a sidecar.  Pod-cold start time
# adds directly to user-facing latency on the first request after a
# rolling deploy or autoscaler spin-up.  DingoSpeed is built on top of
# Echo + Wire + a YAML loader; its startup cost is non-trivial.  This
# script reports both numbers.
#
# Output:
#   tmp/bench/startup.csv  -- server,round,wall_ms
set -euo pipefail
export PATH="/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

ROUNDS="${1:-5}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BENCH="$ROOT/tmp/bench"
LOGS="$BENCH/logs"
RESULTS="$BENCH/startup.csv"

PORT_FAKEHF=18484
PORT_HFPROXY=18080
PORT_DINGOSPEED=18686
URL_PREFIX="/models/bench/bench/resolve/main"

mkdir -p "$LOGS" "$BENCH/bin"

(cd "$ROOT" &&
	go build -o "$BENCH/bin/pulsys" ./cmd/pulsys &&
	go build -o "$BENCH/bin/fake-hf"  ./cmd/fake-hf)

if [ ! -x "$BENCH/dingospeed/bin/dingospeed" ]; then
	echo "DingoSpeed not built.  See scripts/bench_compare.sh."
	exit 1
fi

cleanup() {
	pkill -f "tmp/bench/bin/pulsys"              2>/dev/null || true
	pkill -f "tmp/bench/bin/fake-hf"               2>/dev/null || true
	pkill -f "tmp/bench/dingospeed/bin/dingospeed" 2>/dev/null || true
	sleep 0.3
}
trap cleanup EXIT
cleanup

echo "server,round,wall_ms" > "$RESULTS"

# Common helper: poll a URL until it returns 200/206, printing elapsed ms.
# Returns 0 on success, prints "TIMEOUT" if it takes >10s.
wait_for_ms() {
	local port="$1" path="$2" t0="$3"
	local code now ms
	for _ in $(seq 1 1000); do
		code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 0.5 \
			"http://127.0.0.1:$port$path" 2>/dev/null || true)
		case "$code" in
			200|206|302|303|307)
				now=$(/usr/bin/perl -MTime::HiRes=time -e 'printf("%.6f", time)')
				ms=$(awk -v a="$t0" -v b="$now" 'BEGIN { printf("%.1f", (b - a) * 1000) }')
				echo "$ms"
				return 0
				;;
		esac
		sleep 0.01
	done
	echo "TIMEOUT"
	return 1
}

# Pre-warm fake-hf once; restart cost would dominate otherwise.
"$BENCH/bin/fake-hf" -listen "127.0.0.1:$PORT_FAKEHF" >"$LOGS/startup-fakehf.out" 2>&1 &
sleep 0.3
curl -s -o /dev/null "http://127.0.0.1:$PORT_FAKEHF/api/models/bench/multi"

for round in $(seq 1 "$ROUNDS"); do
	echo "--- round $round / $ROUNDS ---"

	# pulsys
	pkill -f "tmp/bench/bin/pulsys" 2>/dev/null || true
	sleep 0.4
	rm -rf "$BENCH/pulsys-cache-startup"
	t0=$(/usr/bin/perl -MTime::HiRes=time -e 'printf("%.6f", time)')
	"$BENCH/bin/pulsys" \
		-listen "127.0.0.1:$PORT_HFPROXY" \
		-admin-listen "127.0.0.1:18099" \
		-cache-dir "$BENCH/pulsys-cache-startup" \
		-default-upstream-host "127.0.0.1:$PORT_FAKEHF" \
		-upstream-scheme http \
		-public-base-url "http://127.0.0.1:$PORT_HFPROXY" \
		-allow-host 127.0.0.1 \
		-log-level error \
		>"$LOGS/startup-pulsys-r$round.out" 2>&1 &
	ms=$(wait_for_ms "$PORT_HFPROXY" "$URL_PREFIX/4k.bin" "$t0")
	echo "pulsys   r$round: ${ms} ms"
	echo "pulsys,$round,$ms" >>"$RESULTS"

	# dingospeed
	pkill -f "tmp/bench/dingospeed/bin/dingospeed" 2>/dev/null || true
	sleep 0.4
	rm -rf "$BENCH/dingospeed/repos"
	mkdir -p "$BENCH/dingospeed/repos"
	t0=$(/usr/bin/perl -MTime::HiRes=time -e 'printf("%.6f", time)')
	( cd "$BENCH/dingospeed" && ./bin/dingospeed ) \
		>"$LOGS/startup-dingospeed-r$round.out" 2>&1 &
	ms=$(wait_for_ms "$PORT_DINGOSPEED" "$URL_PREFIX/4k.bin" "$t0")
	echo "dingospeed r$round: ${ms} ms"
	echo "dingospeed,$round,$ms" >>"$RESULTS"
done

echo
echo "results: $RESULTS"

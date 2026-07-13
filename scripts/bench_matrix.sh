#!/usr/bin/env bash
# bench_matrix.sh -- the comprehensive head-to-head benchmark vs DingoSpeed.
#
# Runs a full (payload-size, concurrency, scenario) matrix and emits one
# wide CSV per cell.  Designed to leave no room for argument: every
# realistic Hugging Face workload shape is measured, and every metric
# DingoSpeed could plausibly tie or win is reported.
#
# Matrix dimensions:
#
#   PAYLOADS:   4k 64k 256k 4m 10m 16m 64m 256m
#               (tiny metadata -> medium config -> full safetensors shard;
#                10m and 16m are the two hf-cli chunk defaults, so those
#                two MUST be the cleanest wins.)
#
#   CONCURRENCY: 1 4 8 16 32 64
#               (1 = sidecar single-client baseline.
#                8 = hf download --max-workers default.
#                32, 64 = stress + GC pressure.)
#
#   SCENARIOS:  warm cold
#               (warm = cache populated; cold = first request after
#                proxy start with empty cache.)
#
#   ROUNDS:     repeated ROUNDS times so the renderer can plot
#               median + min/max whiskers and we never cherry-pick
#               a lucky run.
#
# Output:
#
#   tmp/bench/matrix.csv
#       server,payload,scenario,concurrency,round,reqs_per_s,bytes_per_s,
#       p50_us,p90_us,p99_us,p99_9_us,total_reqs,total_bytes,
#       timeouts,duration_s
#
# Usage:
#   scripts/bench_matrix.sh                                # full matrix
#   scripts/bench_matrix.sh "256k 4m" "1 8" warm 2          # smoke
#   scripts/bench_matrix.sh "4k 256k 10m 64m" "1 8 32" both 3
set -euo pipefail
export PATH="/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

# Pulsys requires a server-side HF token (there is no open mode); it would
# refuse to start otherwise.  The loopback bench points pulsys at the local
# fake-hf upstream, so a placeholder satisfies the startup check without ever
# contacting Hugging Face.  A real PULSYS_HF_TOKEN in the environment (fetched
# from the stack's Secrets Manager secret for real-download runs) wins if present.
: "${PULSYS_HF_TOKEN:=bench-loopback-placeholder-token}"
export PULSYS_HF_TOKEN

PAYLOADS="${1:-4k 64k 256k 4m 10m 16m 64m 256m}"
CONNS="${2:-1 4 8 16 32 64}"
SCENARIOS_INPUT="${3:-both}"
ROUNDS="${4:-3}"
DURATION="${5:-5s}"

case "$SCENARIOS_INPUT" in
	both) SCENARIOS="warm cold" ;;
	warm) SCENARIOS="warm" ;;
	cold) SCENARIOS="cold" ;;
	*) echo "scenarios must be: warm|cold|both"; exit 2 ;;
esac

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BENCH="$ROOT/tmp/bench"
LOGS="$BENCH/logs"
RESULTS="$BENCH/matrix.csv"

PORT_FAKEHF=18484
PORT_HFPROXY=18080
PORT_DINGOSPEED=18686

URL_PREFIX="/models/bench/bench/resolve/main"

mkdir -p "$LOGS" "$BENCH/bin"

# Build everything we own unless the AMI already installed release bins
# (symlinks under tmp/bench/bin).  DingoSpeed must exist separately.
if [ ! -x "$BENCH/bin/pulsys" ] || [ ! -x "$BENCH/bin/fake-hf" ]; then
	(cd "$ROOT" &&
		go build -o "$BENCH/bin/pulsys" ./cmd/pulsys &&
		go build -o "$BENCH/bin/fake-hf"  ./cmd/fake-hf)
fi

if [ ! -x "$BENCH/dingospeed/bin/dingospeed" ]; then
	echo "DingoSpeed not built.  See scripts/bench_compare.sh for setup."
	exit 1
fi

cleanup() {
	pkill -f "tmp/bench/bin/pulsys"              2>/dev/null || true
	pkill -f "tmp/bench/bin/fake-hf"               2>/dev/null || true
	pkill -f "tmp/bench/dingospeed/bin/dingospeed" 2>/dev/null || true
	sleep 0.4
}
trap cleanup EXIT
cleanup

start_fakehf() {
	"$BENCH/bin/fake-hf" -listen "127.0.0.1:$PORT_FAKEHF" \
		>"$LOGS/matrix-fakehf.out" 2>&1 &
	wait_for "$PORT_FAKEHF" "/api/models/bench/multi"
}

start_pulsys() {
	rm -rf "$BENCH/pulsys-cache-matrix"
	# Variant gate: PULSYS_VARIANT
	#   default|no-cork|cork|iouring|reuseport|numa
	# selects the optimization being measured.  Defaults are TCPCork=on,
	# IoUring=off, Listeners=1, single process.  Each variant flips ONE
	# knob from the default so the A/B is unambiguous.  The variant
	# string is recorded in the CSV via tag_server() and in the run log
	# header.
	local variant="${PULSYS_VARIANT:-default}"
	local variant_flags=""
	case "$variant" in
		default)
			# Cork on (Linux default), one listener, no io_uring.
			:
			;;
		no-cork)
			variant_flags="-tcp-cork=false"
			;;
		cork)
			# Explicit cork=on; identical to "default" on Linux but
			# makes the variant tag distinct in the CSV when running
			# back-to-back against no-cork.
			variant_flags="-tcp-cork=true"
			;;
		iouring)
			variant_flags="-iouring=true"
			;;
		reuseport)
			variant_flags="-listeners=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)"
			;;
		numa)
			start_pulsys_numa
			return
			;;
		saturate)
			# NUMA shards (when available) + SO_REUSEPORT listeners on
			# every core in each shard.  The publishable "96-core demo".
			start_pulsys_saturate
			return
			;;
		saturate-no-cork)
			# Same shape as saturate but with TCP_CORK off.  On loopback
			# cork buys nothing and costs 2 setsockopt syscalls per
			# response (~25% of the response-path syscall budget at 4k).
			EXTRA_HFPROXY_FLAGS="-tcp-cork=false" \
				start_pulsys_saturate
			return
			;;
		saturate-iouring)
			EXTRA_HFPROXY_FLAGS="-tcp-cork=false -iouring=true" \
				start_pulsys_saturate
			return
			;;
		*)
			echo "unknown PULSYS_VARIANT=$variant (expected: default|no-cork|cork|iouring|reuseport|numa|saturate|saturate-no-cork|saturate-iouring)" >&2
			exit 2
			;;
	esac
	# shellcheck disable=SC2086  # we WANT word-splitting of variant_flags
	"$BENCH/bin/pulsys" \
		-listen "127.0.0.1:$PORT_HFPROXY" \
		-admin-listen "127.0.0.1:18099" \
		-cache-dir "$BENCH/pulsys-cache-matrix" \
		-default-upstream-host "127.0.0.1:$PORT_FAKEHF" \
		-upstream-scheme http \
		-public-base-url "http://127.0.0.1:$PORT_HFPROXY" \
		-allow-host 127.0.0.1 \
		-log-level warn \
		$variant_flags \
		>"$LOGS/matrix-pulsys.out" 2>&1 &
	wait_for "$PORT_HFPROXY" "/healthz"
}

# start_pulsys_numa launches ONE pulsys process per NUMA node, each
# pinned via `numactl --cpunodebind --membind` so its goroutines run
# on local cores and its heap+stack pages allocate from local memory.
# All processes share the listen port via SO_REUSEPORT (each process
# uses -listeners=1, which goes through coreserver.NewReuseportListeners
# and sets SO_REUSEPORT on the socket); the kernel routes incoming
# connections to whichever process's accept queue is closest.
#
# Why this matters on a 96-core box:  c7i.metal-24xl is dual-socket
# Sapphire Rapids.  Cross-socket UPI bandwidth is ~6x lower than local
# DRAM bandwidth.  A sendfile(socket@node1, file@node0) drags every
# response byte across the inter-socket link.  Sharding the proxy per
# node keeps each connection's socket buffers + the page-cache pages
# it pulls from on the same node.
#
# Falls back to a single un-pinned process when numactl is missing or
# reports only one node (e.g. running on a virt instance or laptop).
start_pulsys_numa() {
	if ! command -v numactl >/dev/null 2>&1; then
		echo "PULSYS_VARIANT=numa: numactl not installed; falling back to single un-pinned process" >&2
		"$BENCH/bin/pulsys" \
			-listen "127.0.0.1:$PORT_HFPROXY" \
			-admin-listen "127.0.0.1:18099" \
			-cache-dir "$BENCH/pulsys-cache-matrix" \
			-default-upstream-host "127.0.0.1:$PORT_FAKEHF" \
			-upstream-scheme http \
			-public-base-url "http://127.0.0.1:$PORT_HFPROXY" \
			-allow-host 127.0.0.1 \
			-log-level warn \
			-listeners=1 \
			>"$LOGS/matrix-pulsys.out" 2>&1 &
		wait_for "$PORT_HFPROXY" "/healthz"
		return
	fi

	# Parse NUMA node count from `numactl --hardware`:
	#   available: 2 nodes (0-1)
	local nodes
	nodes="$(numactl --hardware 2>/dev/null \
		| awk '/^available:/ {print $2; exit}')"
	if [ -z "$nodes" ] || [ "$nodes" -lt 1 ] 2>/dev/null; then
		nodes=1
	fi
	echo "PULSYS_VARIANT=numa: launching $nodes pinned process(es)" >&2

	local n=0
	while [ "$n" -lt "$nodes" ]; do
		# Admin port per process so they do not collide; the bench
		# never hits admin so the exact ports do not matter, only
		# that they differ across shards.
		local admin_port=$((18099 + n))
		numactl --cpunodebind="$n" --membind="$n" -- \
			"$BENCH/bin/pulsys" \
				-listen "0.0.0.0:$PORT_HFPROXY" \
				-admin-listen "127.0.0.1:$admin_port" \
				-cache-dir "$BENCH/pulsys-cache-matrix" \
				-default-upstream-host "127.0.0.1:$PORT_FAKEHF" \
				-upstream-scheme http \
				-public-base-url "http://127.0.0.1:$PORT_HFPROXY" \
				-allow-host 127.0.0.1 \
				-log-level warn \
				-listeners=1 \
				>"$LOGS/matrix-pulsys-node${n}.out" 2>&1 &
		n=$((n + 1))
	done
	wait_for "$PORT_HFPROXY" "/healthz"
}

# start_pulsys_saturate is the configuration for demonstrating full
# utilization of a large bare-metal host (e.g. c7i.metal-24xl, 96 vCPU).
#
#   * 2+ NUMA nodes: one pulsys process per node, pinned with numactl,
#     each with listeners = vCPU / nodes (48+48 accept queues on 96c).
#   * 1 NUMA node:  one process with listeners = vCPU (96 accept queues).
#
# Pair with BENCH_WRK_THREADS=max and conns >= 4*vCPU so wrk can feed
# every core.  See scripts/bench_saturate.sh for the full demo flow.
start_pulsys_saturate() {
	local ncpu nodes cores_per_node n
	ncpu=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)
	nodes=1
	if command -v numactl >/dev/null 2>&1; then
		nodes="$(numactl --hardware 2>/dev/null \
			| awk '/^available:/ {print $2; exit}')"
		[ -z "$nodes" ] || [ "$nodes" -lt 1 ] 2>/dev/null && nodes=1
	fi

	# Composable knobs: EXTRA_HFPROXY_FLAGS is appended to every process.
	# Used by saturate-no-cork / saturate-iouring sub-variants so we don't
	# need a copy of this launcher per knob.
	# shellcheck disable=SC2206  # we WANT word-splitting
	local extra=(${EXTRA_HFPROXY_FLAGS:-})

	if [ "$nodes" -gt 1 ]; then
		cores_per_node=$(( (ncpu + nodes - 1) / nodes ))
		echo "PULSYS_VARIANT=${PULSYS_VARIANT:-saturate}: ${nodes} NUMA shard(s), ${cores_per_node} listeners each ($(( nodes * cores_per_node )) accept queues on ${ncpu} vCPU)  extra=[${extra[*]}]" >&2
		n=0
		while [ "$n" -lt "$nodes" ]; do
			local admin_port=$((18099 + n))
			numactl --cpunodebind="$n" --membind="$n" -- \
				"$BENCH/bin/pulsys" \
					-listen "0.0.0.0:$PORT_HFPROXY" \
					-admin-listen "127.0.0.1:$admin_port" \
					-cache-dir "$BENCH/pulsys-cache-matrix" \
					-default-upstream-host "127.0.0.1:$PORT_FAKEHF" \
					-upstream-scheme http \
					-public-base-url "http://127.0.0.1:$PORT_HFPROXY" \
					-allow-host 127.0.0.1 \
					-log-level warn \
					-listeners="$cores_per_node" \
					"${extra[@]}" \
					>"$LOGS/matrix-pulsys-node${n}.out" 2>&1 &
			n=$((n + 1))
		done
	else
		echo "PULSYS_VARIANT=${PULSYS_VARIANT:-saturate}: single process, listeners=${ncpu}  extra=[${extra[*]}]" >&2
		"$BENCH/bin/pulsys" \
			-listen "0.0.0.0:$PORT_HFPROXY" \
			-admin-listen "127.0.0.1:18099" \
			-cache-dir "$BENCH/pulsys-cache-matrix" \
			-default-upstream-host "127.0.0.1:$PORT_FAKEHF" \
			-upstream-scheme http \
			-public-base-url "http://127.0.0.1:$PORT_HFPROXY" \
			-allow-host 127.0.0.1 \
			-log-level warn \
			-listeners="$ncpu" \
			"${extra[@]}" \
			>"$LOGS/matrix-pulsys.out" 2>&1 &
	fi
	wait_for "$PORT_HFPROXY" "/healthz"
}

# tag_server stamps the server name in the CSV with the variant suffix.
# When PULSYS_VARIANT is unset or "default", the row is tagged plain
# "pulsys" so existing renderer code (which keys on "pulsys") keeps
# working.  Non-default variants are tagged "pulsys-<variant>" so the
# rows can be filtered/aggregated by variant later.
tag_server() {
	local raw="$1"
	if [ "$raw" = "pulsys" ] && [ -n "${PULSYS_VARIANT:-}" ] && [ "${PULSYS_VARIANT}" != "default" ]; then
		printf 'pulsys-%s' "${PULSYS_VARIANT}"
		return
	fi
	printf '%s' "$raw"
}

start_dingospeed() {
	rm -rf "$BENCH/dingospeed/repos"
	mkdir -p "$BENCH/dingospeed/repos"
	( cd "$BENCH/dingospeed" && ./bin/dingospeed ) \
		>"$LOGS/matrix-dingospeed.out" 2>&1 &
	wait_for "$PORT_DINGOSPEED" "$URL_PREFIX/4k.bin"
}

stop_proxy() {
	case "$1" in
		pulsys)   pkill -f "tmp/bench/bin/pulsys" 2>/dev/null || true ;;
		dingospeed) pkill -f "tmp/bench/dingospeed/bin/dingospeed" 2>/dev/null || true ;;
	esac
	sleep 0.3
}

wait_for() {
	local port="$1" path="$2"
	for _ in $(seq 1 80); do
		code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 \
			"http://127.0.0.1:$port$path" || true)
		case "$code" in
			200|206|302|303|307) return ;;
		esac
		sleep 0.1
	done
	echo "service on port $port failed to come up (path=$path, last code=$code)"
	exit 1
}

# warm_cache_for: ensure server has the given payload sizes cached.
warm_cache_for() {
	local port="$1"; shift
	local -a auth=()
	[ -n "${AUTH_HEADER:-}" ] && auth=(-H "$AUTH_HEADER")
	for p in "$@"; do
		curl -s -L -o /dev/null --max-time 60 "${auth[@]}" \
			"http://127.0.0.1:$port$URL_PREFIX/${p}.bin" || true
	done
}

# wrk_one prints a CSV row for one (server, payload, scenario, concurrency, round).
# We use --latency so p50/p99/p99.9 are available.  --timeout 60s prevents
# wrk from counting partial transfers as completed.
wrk_one() {
	local server="$1" port="$2" payload="$3" scenario="$4" conns="$5" round="$6"
	local out="$LOGS/wrk_${server}_${payload}_${scenario}_c${conns}_r${round}.txt"

	# Default: min(conns, ncpu/2) wrk threads (fine for laptop matrix).
	# BENCH_WRK_THREADS=max: one wrk thread per vCPU (saturation runs on
	# bare metal).  Still capped at conns so we do not spawn idle threads.
	local ncpu threads dur_sec mpstat_pid=""
	ncpu=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)
	if [ "${BENCH_WRK_THREADS:-}" = "max" ]; then
		threads="$ncpu"
	else
		threads="$conns"
		if [ "$threads" -gt "$((ncpu / 2))" ]; then threads=$((ncpu / 2)); fi
	fi
	if [ "$threads" -gt "$conns" ]; then threads="$conns"; fi
	if [ "$threads" -lt 1 ]; then threads=1; fi

	dur_sec="${DURATION%s}"
	[ -z "$dur_sec" ] && dur_sec=5
	if [ "${BENCH_CAPTURE_MPSTAT:-}" = "1" ]; then
		mpstat -P ALL 1 "$dur_sec" \
			>"$LOGS/mpstat_${server}_${payload}_${scenario}_c${conns}_r${round}.txt" 2>&1 &
		mpstat_pid=$!
	fi

	local -a auth=()
	[ -n "${AUTH_HEADER:-}" ] && auth=(-H "$AUTH_HEADER")
	wrk -t "$threads" -c "$conns" -d "$DURATION" --latency --timeout 60s \
		"${auth[@]}" \
		"http://127.0.0.1:$port$URL_PREFIX/${payload}.bin" \
		>"$out" 2>&1 || true
	if [ -n "$mpstat_pid" ]; then
		wait "$mpstat_pid" 2>/dev/null || true
	fi

	# Parse wrk output.  All these awks are tolerant of missing fields.
	local rps bps p50 p90 p99 p99_9 total xfer timeouts dur
	rps=$(awk '/Requests\/sec:/{print $2}'     "$out")
	bps=$(awk '/Transfer\/sec:/{print $2}'     "$out")
	p50=$(awk '/^[[:space:]]*50%/{print $2}'   "$out")
	p90=$(awk '/^[[:space:]]*90%/{print $2}'   "$out")
	p99=$(awk '/^[[:space:]]*99%/{print $2}'   "$out")
	p99_9=$(awk '/^[[:space:]]*99\.999%/{print $2}' "$out")
	if [ -z "$p99_9" ]; then
		p99_9=$(awk '/^[[:space:]]*99\.99%/{print $2}' "$out")
	fi
	total=$(awk '/[0-9]+ requests in/{print $1}' "$out")
	xfer=$(awk  '/Transfer:/ && !/sec/{print $2}' "$out")
	timeouts=$(awk '/Socket errors:/{for(i=1;i<=NF;i++) if($i ~ /^timeout$/) print $(i+1)}' "$out" | tr -d ',')
	dur=$(awk    '/[0-9]+ requests in/{print $4}' "$out" | tr -d ',s')
	[ -z "$rps" ] && rps=0
	[ -z "$bps" ] && bps=0B
	[ -z "$p50" ] && p50=0us
	[ -z "$p90" ] && p90=0us
	[ -z "$p99" ] && p99=0us
	[ -z "$p99_9" ] && p99_9=0us
	[ -z "$total" ] && total=0
	[ -z "$xfer" ] && xfer=0B
	[ -z "$timeouts" ] && timeouts=0
	[ -z "$dur" ] && dur=0

	local tagged
	tagged="$(tag_server "$server")"
	printf "%-16s %-5s %-5s c=%-3s r=%-2s  rps=%-10s xfer/s=%-10s p50=%-8s p99=%-8s ok=%-7s to=%s\n" \
		"$tagged" "$payload" "$scenario" "$conns" "$round" \
		"$rps" "$bps" "$p50" "$p99" "$total" "$timeouts"
	echo "$tagged,$payload,$scenario,$conns,$round,$rps,$bps,$p50,$p90,$p99,$p99_9,$total,$xfer,$timeouts,$dur" >>"$RESULTS"
}

# Header row.  Schema MUST stay in sync with render_bench_matrix.go.
echo "server,payload,scenario,concurrency,round,reqs_per_s,bytes_per_s,p50,p90,p99,p99_9,total_reqs,total_bytes,timeouts,duration_s" > "$RESULTS"

echo
echo "==================  bench_matrix: pulsys vs DingoSpeed  =================="
echo "variant=${PULSYS_VARIANT:-default}  payloads=$PAYLOADS  conns=$CONNS  scenarios=$SCENARIOS  rounds=$ROUNDS  duration=$DURATION"
echo

# Pulsys has no open mode: stand up a local admin plane (Postgres) and seed one
# API token so the data-plane auth gate admits the benchmark traffic.  Skipped
# when PULSYS_DB_DSN + BENCH_PAT are already exported (e.g. an external DB).
if [ -z "${PULSYS_DB_DSN:-}" ] || [ -z "${BENCH_PAT:-}" ]; then
	eval "$(bash "$ROOT/scripts/bench_db_up.sh")"
fi
export PULSYS_IMPORT_WORKER=0   # keep background import polling off the hot path
if [ -z "${BENCH_PAT:-}" ]; then
	echo "FATAL: bench_db_up.sh did not produce a BENCH_PAT (data plane requires auth)" >&2
	exit 1
fi
# Every data-plane request (warm fill + wrk load) must carry this bearer token.
AUTH_HEADER="Authorization: Bearer ${BENCH_PAT}"

start_fakehf

# run_for_server: start the proxy fresh for cold rounds, reuse it warm.
run_for_server() {
	local server="$1" port="$2"
	echo "---- $server ----"

	for scenario in $SCENARIOS; do
		for payload in $PAYLOADS; do
			for conns in $CONNS; do
				for round in $(seq 1 "$ROUNDS"); do
					if [ "$scenario" = "cold" ]; then
						# Cold: restart proxy each round so cache is empty.
						stop_proxy "$server"
						case "$server" in
							pulsys)   start_pulsys ;;
							dingospeed) start_dingospeed ;;
						esac
						# A single GET to mark the time-to-first-byte
						# from a real client.  This populates cache and
						# acts as the "cold round" measurement.
						# wrk with -c1 -d1s also approximates cold-path
						# fairness; running wrk now measures the
						# blended path (mostly warm).
						# We do BOTH: a curl for true TTFB-cold, then
						# wrk for steady cold-path.  The wrk row is
						# the one the matrix renders.
						curl -s -o /dev/null \
							"http://127.0.0.1:$port$URL_PREFIX/${payload}.bin" || true
					else
						# Warm: start proxy once per (server, scenario)
						# cycle and warm every payload before the wrk
						# loop hits it.  Rely on the proxy still being
						# up from the previous cold-block (if any) or
						# from a previous warm-block iteration.
						if ! curl -s -o /dev/null -w '%{http_code}' --max-time 2 \
								"http://127.0.0.1:$port$URL_PREFIX/${payload}.bin" \
								| grep -qE '^(200|206)$'; then
							stop_proxy "$server"
							case "$server" in
								pulsys)   start_pulsys ;;
								dingospeed) start_dingospeed ;;
							esac
							warm_cache_for "$port" "$payload"
						fi
					fi
					wrk_one "$server" "$port" "$payload" "$scenario" "$conns" "$round"
				done
			done
		done
	done
	stop_proxy "$server"
}

run_for_server "pulsys" "$PORT_HFPROXY"
if [ "${BENCH_SKIP_DINGOSPEED:-}" != "1" ]; then
	run_for_server "dingospeed" "$PORT_DINGOSPEED"
fi

if [ "$(uname -s)" = "Darwin" ]; then
	mkdir -p "$ROOT/tmp/bench/darwin"
	cp "$RESULTS" "$ROOT/tmp/bench/darwin/matrix.csv"
	bash "$ROOT/scripts/bench_write_meta.sh" matrix "$ROOT/tmp/bench/darwin"
	for aux in footprint.csv e2e_results.csv; do
		[ -f "$BENCH/$aux" ] && cp "$BENCH/$aux" "$ROOT/tmp/bench/darwin/"
	done
	echo "darwin snapshot: tmp/bench/darwin/matrix.csv"
fi

echo
echo "results: $RESULTS"

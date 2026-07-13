#!/usr/bin/env bash
# bench_footprint.sh -- RSS + CPU footprint during a 60s sustained
# warm-hit hammering.  Sidecar deployments care about this number
# almost as much as throughput: every MB of RSS the proxy holds is one
# MB the application process can't use.
#
# Output:
#   tmp/bench/footprint.csv  -- server,payload,sample_s,rss_mb,cpu_pct,total_bytes
#
# How it works:
#   Start one proxy.  Warm one payload.  Run wrk -t4 -c8 -d60s.  Every
#   second, sample ps -o pid,rss,etime,time and the proxy's own
#   /debug/vars (pulsys) or HTTP body length (dingospeed).  Compute:
#     rss_mb     resident set in MB
#     cpu_pct    delta-cpu / delta-wall * 100 (multi-core, >100 OK)
#     total_bytes  cumulative bytes served (so we can derive CPU/GB)
set -uo pipefail
export PATH="/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

PAYLOADS="${1:-256k 16m 64m}"
DURATION_S="${2:-30}"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BENCH="$ROOT/tmp/bench"
LOGS="$BENCH/logs"
RESULTS="$BENCH/footprint.csv"

PORT_FAKEHF=18484
PORT_HFPROXY=18080
PORT_DINGOSPEED=18686
PORT_ADMIN=18099
URL_PREFIX="/models/bench/bench/resolve/main"

mkdir -p "$LOGS" "$BENCH/bin"

(cd "$ROOT" &&
	go build -o "$BENCH/bin/pulsys" ./cmd/pulsys &&
	go build -o "$BENCH/bin/fake-hf"  ./cmd/fake-hf)

if [ ! -x "$BENCH/dingospeed/bin/dingospeed" ]; then
	echo "DingoSpeed not built."
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

echo "server,payload,sample_s,rss_mb,cpu_s,total_bytes" > "$RESULTS"

# Pre-warm fake-hf.
"$BENCH/bin/fake-hf" -listen "127.0.0.1:$PORT_FAKEHF" >"$LOGS/footprint-fakehf.out" 2>&1 &
sleep 0.3

# rss_kb_for_pid: prints RSS in KB for given PID (BSD ps; macOS-friendly).
rss_kb_for_pid() {
	ps -o rss= -p "$1" 2>/dev/null | awk '{ print $1 }' | head -1
}

# cpu_seconds_for_pid: prints cumulative CPU time in seconds (utime+stime)
# using ps -o time (BSD format like 0:00.43 or 1:23.45 or 1-12:34:56).
cpu_seconds_for_pid() {
	local t
	t=$(ps -o time= -p "$1" 2>/dev/null | awk '{ print $1 }' | head -1)
	[ -z "$t" ] && { echo "0"; return; }
	# Normalise to seconds.  Forms seen:
	#   0:00.43         m:ss.frac
	#   1:23:45         h:mm:ss
	#   1-12:34:56      d-h:mm:ss
	awk -v t="$t" '
	BEGIN {
		days = 0; rest = t
		i = index(rest, "-")
		if (i > 0) { days = substr(rest, 1, i-1) + 0; rest = substr(rest, i+1) }
		n = split(rest, parts, ":")
		secs = 0
		if (n == 3)      secs = parts[1]*3600 + parts[2]*60 + parts[3]
		else if (n == 2) secs = parts[1]*60 + parts[2]
		else             secs = parts[1]
		printf("%.3f\n", days*86400 + secs)
	}'
}

run_for_server() {
	local server="$1" port="$2" pidpat="$3" payload="$4"
	echo "---- $server payload=$payload ----"
	pkill -f "$pidpat" >/dev/null 2>&1 || true
	sleep 0.4

	case "$server" in
		pulsys)
			rm -rf "$BENCH/pulsys-cache-footprint"
			"$BENCH/bin/pulsys" \
				-listen "127.0.0.1:$port" \
				-admin-listen "127.0.0.1:$PORT_ADMIN" \
				-cache-dir "$BENCH/pulsys-cache-footprint" \
				-default-upstream-host "127.0.0.1:$PORT_FAKEHF" \
				-upstream-scheme http \
				-public-base-url "http://127.0.0.1:$port" \
				-allow-host 127.0.0.1 \
				-log-level error \
				>"$LOGS/footprint-pulsys.out" 2>&1 &
			;;
		dingospeed)
			rm -rf "$BENCH/dingospeed/repos"
			mkdir -p "$BENCH/dingospeed/repos"
			( cd "$BENCH/dingospeed" && ./bin/dingospeed ) \
				>"$LOGS/footprint-dingospeed.out" 2>&1 &
			;;
	esac
	sleep 0.5
	local pid
	pid=$(pgrep -f "$pidpat" | head -1)
	[ -z "$pid" ] && { echo "$server failed to start"; return 1; }

	# Warm cache by curl'ing the payload.
	curl -s -o /dev/null --max-time 60 "http://127.0.0.1:$port$URL_PREFIX/${payload}.bin" || true
	curl -s -o /dev/null --max-time 60 "http://127.0.0.1:$port$URL_PREFIX/${payload}.bin" || true

	# Start wrk in background; sample once per second; stop wrk.
	wrk -t4 -c8 -d "${DURATION_S}s" --latency --timeout 60s \
		"http://127.0.0.1:$port$URL_PREFIX/${payload}.bin" \
		>"$LOGS/footprint-wrk-$server-$payload.out" 2>&1 &
	local wrk_pid=$!

	local t0 elapsed rss cpu
	t0=$(/usr/bin/perl -MTime::HiRes=time -e 'printf("%.6f", time)')
	while kill -0 "$wrk_pid" 2>/dev/null; do
		sleep 1
		now=$(/usr/bin/perl -MTime::HiRes=time -e 'printf("%.6f", time)')
		elapsed=$(awk -v a="$t0" -v b="$now" 'BEGIN { printf("%.0f", b - a) }')
		rss=$(rss_kb_for_pid "$pid")
		cpu=$(cpu_seconds_for_pid "$pid")
		[ -z "$rss" ] && rss=0
		[ -z "$cpu" ] && cpu=0
		rss_mb=$(awk -v r="$rss" 'BEGIN { printf("%.2f", r / 1024) }')
		printf "%-12s %-5s t=%3ss  RSS=%6sMB  CPU=%6ss\n" \
			"$server" "$payload" "$elapsed" "$rss_mb" "$cpu"
		# total_bytes from wrk we'll fill in 0 here -- the wrk log
		# below has the final number.
		echo "$server,$payload,$elapsed,$rss_mb,$cpu,0" >>"$RESULTS"
	done
	wait "$wrk_pid" 2>/dev/null || true

	# Extract final total bytes served from wrk log; replace the last
	# row's total_bytes with this number.
	local xfer total
	xfer=$(awk '/Transfer:/ && !/sec/{print $2}' "$LOGS/footprint-wrk-$server-$payload.out")
	total=$(echo "$xfer" | awk '
		/[GMK]B$/ { n=substr($0,1,length($0)-2)+0; u=substr($0,length($0)-1);
			if (u=="GB") print n*1e9; else if (u=="MB") print n*1e6; else print n*1e3; exit }
		{ print 0; exit }')
	# Update last row with the totals.
	awk -v s="$server" -v p="$payload" -v t="$total" '
		BEGIN { FS=OFS="," }
		{
			if ($1 == s && $2 == p) { last = NR; rows[NR] = $0; row6[NR] = $6 }
			rows[NR] = $0
		}
		END {
			n = NR
			for (i=1; i<=n; i++) {
				if (i == last) {
					split(rows[i], f, ",")
					f[6] = t
					out = f[1]
					for (j=2; j<=length(f); j++) out = out OFS f[j]
					print out
				} else {
					print rows[i]
				}
			}
		}' "$RESULTS" >"$RESULTS.tmp" && mv "$RESULTS.tmp" "$RESULTS"

	pkill -f "$pidpat" >/dev/null 2>&1 || true
	sleep 0.4
}

for payload in $PAYLOADS; do
	run_for_server "pulsys"   "$PORT_HFPROXY"   "tmp/bench/bin/pulsys"            "$payload"
	run_for_server "dingospeed" "$PORT_DINGOSPEED" "tmp/bench/dingospeed/bin/dingospeed" "$payload"
done

echo
echo "results: $RESULTS"

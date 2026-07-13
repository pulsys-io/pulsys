#!/usr/bin/env bash
# bench_compare.sh -- HTTP/1.1 warm-hit throughput benchmark.
#
# Comparison set (Go HF-aware caching proxies + Go kernel-floor reference):
#
#   DIRECT COMPETITOR (other Go HF mirror):
#     pulsys        this project (warm-hit fast path: coreserver + sf_hdtr)
#     dingospeed      https://github.com/dingodb/dingospeed
#
#   KERNEL-FLOOR REFERENCE (generic static file servers, not HF-aware):
#     caddy           reference for "what does a competent static-file server land at"
#     nginx           the canonical C static-file server (skipped if not installed)
#     go-net-http     stdlib http.FileServer reference
#
#   Caddy, nginx, and net/http are not HF proxies -- they cannot route
#   /api/models/<repo>/revision/... etc.  They are kept here only as
#   reference points so the chart can show whether pulsys is close
#   to the kernel sendfile(2) ceiling.  At >= 10 MiB payloads on macOS
#   loopback all three Go servers converge to ~5 GB/s (single-stream)
#   and tie within ~5% noise; that ceiling is the property of the
#   kernel, not the user-space implementation.
#
#   Python-based Olah is NOT included -- it is 8-20x slower than every
#   Go implementation here.  Plotting it crushes the y-axis without
#   revealing meaningful contrast between the Go contenders.  Use
#   scripts/bench_matrix.sh + the Olah column in any standalone report
#   if you specifically want to highlight the Python-vs-Go gap.
#
# All servers serve the SAME bytes under the SAME URL shape
# (/models/bench/bench/resolve/main/<size>.bin) on the same machine,
# so wrk numbers are directly comparable.
#
# Multi-round: each (server, payload) cell is repeated ROUNDS times
# and scripts/render_bench_svg.go takes the median + min/max whiskers
# from the resulting CSV.  This avoids cherry-picking a lucky run and
# visually communicates the noise floor on saturated payload sizes.
#
# Usage:
#   scripts/bench_compare.sh [DURATION] [CONNS] [THREADS] [PAYLOADS] [ROUNDS]
# Defaults: 8s, 8 conns, 4 threads, all payloads, 5 rounds.
#
# Quick smoke-test (~30s total):
#   scripts/bench_compare.sh 5s 8 2 "16k 256k" 1
#
# CONNS=8 by default to match `hf download --max-workers 8`, the real
# sidecar workload shape.  CONNS=64 (the previous default) measured
# wrk's own recv-loop scheduling rather than the server.
set -euo pipefail

DURATION="${1:-8s}"
CONNS="${2:-8}"
THREADS="${3:-4}"
PAYLOADS="${4:-16k 256k 4m 10m 16m 64m}"
ROUNDS="${5:-5}"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BENCH="$ROOT/tmp/bench"
LOGS="$BENCH/logs"
RESULTS="$BENCH/results.csv"

# Ports
PORT_CORESERVER=18080
PORT_CADDY=18282
PORT_NETHTTP=18383
PORT_FAKEHF=18484
PORT_NGINX=18585
PORT_DINGOSPEED=18686

# Paths
URL_PREFIX="/models/bench/bench/resolve/main"

mkdir -p "$LOGS" "$BENCH/bin"
echo "server,payload,reqs_per_s,bytes_per_s,p50_ms,p99_ms,transfer_total,completed,timeouts" > "$RESULTS"

ensure_bench_payloads() {
	mkdir -p "$BENCH/www"
	export HFPROXY_ROOT="$ROOT"
	python3 - <<'PY'
import os, pathlib
root = pathlib.Path(os.environ["HFPROXY_ROOT"]) / "tmp" / "bench" / "www"
root.mkdir(parents=True, exist_ok=True)
sizes = {
	"16k.bin":    16 * 1024,
	"256k.bin":   256 * 1024,
	"4m.bin":      4 << 20,
	"10m.bin":    10 << 20,
	"16m.bin":    16 << 20,
	"64m.bin":    64 << 20,
}
for name, n in sizes.items():
	path = root / name
	with open(path, "wb") as f:
		f.truncate(n)
PY
}

ensure_dingospeed() {
	if [ ! -x "$BENCH/dingospeed/bin/dingospeed" ]; then
		echo "==> DingoSpeed not found in $BENCH/dingospeed; please run:"
		echo "    git clone --depth 1 https://github.com/dingodb/dingospeed $BENCH/dingospeed"
		echo "    (cd $BENCH/dingospeed && make init && make wire && make build)"
		echo "    (and copy scripts/dingospeed-config.yaml -> $BENCH/dingospeed/config/config.yaml)"
		exit 1
	fi
}

# nginx is an optional kernel-floor reference. If it is not installed we
# skip it (no row in the CSV) rather than failing the whole comparison.
# When present we generate a self-contained config that serves the same
# tmp/bench/www payloads under the same URL prefix as every other server,
# writing pid/logs/temp dirs under tmp/bench so no root or /var access is
# needed.
HAVE_NGINX=0
ensure_nginx() {
	if ! command -v nginx >/dev/null 2>&1; then
		echo "==> nginx not found on PATH; skipping the nginx reference column."
		echo "    (install with: brew install nginx  /  apt-get install nginx)"
		return
	fi
	HAVE_NGINX=1
	local conf_dir="$BENCH/conf" prefix="$BENCH/nginx"
	mkdir -p "$conf_dir" "$prefix/temp/client_body" "$prefix/temp/proxy" \
		"$prefix/temp/fastcgi" "$prefix/temp/uwsgi" "$prefix/temp/scgi" "$prefix/logs"
	cat >"$conf_dir/nginx.conf" <<EOF
worker_processes 2;
daemon off;
pid $prefix/nginx.pid;
error_log $prefix/logs/error.log warn;
events { worker_connections 2048; }
http {
    access_log off;
    sendfile on;
    tcp_nopush on;
    keepalive_timeout 65;
    default_type application/octet-stream;
    client_body_temp_path $prefix/temp/client_body;
    proxy_temp_path $prefix/temp/proxy;
    fastcgi_temp_path $prefix/temp/fastcgi;
    uwsgi_temp_path $prefix/temp/uwsgi;
    scgi_temp_path $prefix/temp/scgi;
    server {
        listen 127.0.0.1:$PORT_NGINX;
        location $URL_PREFIX/ {
            alias $BENCH/www/;
        }
    }
}
EOF
}

ensure_bench_payloads
ensure_dingospeed
ensure_nginx

(cd "$ROOT" && go build -o "$BENCH/bin/bench-coreserver" ./cmd/bench-coreserver &&
	go build -o "$BENCH/bin/bench-nethttp"   ./cmd/bench-nethttp &&
	go build -o "$BENCH/bin/fake-hf"         ./cmd/fake-hf)

cleanup() {
    pkill -f "tmp/bench/bin/bench-coreserver" 2>/dev/null || true
    pkill -f "tmp/bench/bin/bench-nethttp"   2>/dev/null || true
    pkill -f "tmp/bench/bin/fake-hf"          2>/dev/null || true
    pkill -f "caddy.*tmp/bench"               2>/dev/null || true
    pkill -f "tmp/bench/conf/nginx.conf"      2>/dev/null || true
    pkill -f "tmp/bench/dingospeed/bin/dingospeed" 2>/dev/null || true
    sleep 0.3
}
trap cleanup EXIT
cleanup

# Reset DingoSpeed cache so warm hits go through its full mirror path.
rm -rf "$BENCH/dingospeed/repos"
mkdir -p "$BENCH/dingospeed/repos"

# Start fake-hf upstream first (DingoSpeed depends on it).
"$BENCH/bin/fake-hf" -listen "127.0.0.1:$PORT_FAKEHF" >"$LOGS/fake-hf.out" 2>&1 &

# Same-shape URLs across the board so wrk numbers are directly comparable.
"$BENCH/bin/bench-coreserver" -listen "127.0.0.1:$PORT_CORESERVER" \
    -www "$BENCH/www" -url-prefix "$URL_PREFIX" >"$LOGS/coreserver.out" 2>&1 &
"$BENCH/bin/bench-nethttp"   -listen "127.0.0.1:$PORT_NETHTTP" \
    -www "$BENCH/www" -url-prefix "$URL_PREFIX" >"$LOGS/nethttp.out"   2>&1 &
caddy run --config "$BENCH/conf/Caddyfile" --adapter caddyfile \
    >"$LOGS/caddy.out" 2>&1 &

# nginx (optional): foreground (daemon off in the generated conf), prefix +
# config both under tmp/bench so it needs no root or /var access.
if [ "$HAVE_NGINX" = 1 ]; then
    nginx -p "$BENCH/nginx" -c "$BENCH/conf/nginx.conf" >"$LOGS/nginx.out" 2>&1 &
fi

# DingoSpeed reads its config from ./config/config.yaml relative to its
# binary, so we cd into its tree before launching.
( cd "$BENCH/dingospeed" && ./bin/dingospeed ) >"$LOGS/dingospeed.out" 2>&1 &

# Wait for every started server to come up against a sample URL.
WAIT_PORTS="$PORT_CORESERVER $PORT_NETHTTP $PORT_CADDY $PORT_DINGOSPEED"
[ "$HAVE_NGINX" = 1 ] && WAIT_PORTS="$WAIT_PORTS $PORT_NGINX"
for p in $WAIT_PORTS; do
    for i in $(seq 1 80); do
        if curl -s -o /dev/null -w '%{http_code}' \
                "http://127.0.0.1:$p$URL_PREFIX/16k.bin" | grep -qE '^(200|206)$'; then
            break
        fi
        sleep 0.1
    done
done
sleep 0.3
echo "==> all servers up"

# DingoSpeed needs each payload curled once to populate cache.
echo "==> warming DingoSpeed cache"
for payload in $PAYLOADS; do
    curl -s -L -o /dev/null -w "    dingospeed  warm $payload: %{http_code} size=%{size_download} time=%{time_total}s\n" \
        "http://127.0.0.1:$PORT_DINGOSPEED$URL_PREFIX/${payload}.bin"
done

# run_one runs wrk against a (label, port, payload) and writes one CSV row.
#
# We pass --timeout 60s explicitly so wrk does NOT count "request didn't
# finish in 2s" as a completed transfer.  Without this, a slow server
# (e.g. Olah on 64 MiB) reports plausible-looking req/s and xfer/s that
# are actually computed from PARTIALLY-read bytes after wrk gave up.
run_one() {
    local label="$1" port="$2" payload="$3"
    local out="$LOGS/wrk_${label}_${payload}.txt"
    wrk -t "$THREADS" -c "$CONNS" -d "$DURATION" --latency --timeout 60s \
        "http://127.0.0.1:$port$URL_PREFIX/${payload}.bin" >"$out" 2>&1 || true

    local rps bps p50 p99 xfer completions timeouts
    rps=$(awk '/Requests\/sec:/{print $2}' "$out")
    bps=$(awk '/Transfer\/sec:/{print $2}' "$out")
    xfer=$(awk '/Transfer:/ && !/sec/{print $2}' "$out")
    p50=$(awk '/^[[:space:]]*50%/{print $2}' "$out")
    p99=$(awk '/^[[:space:]]*99%/{print $2}' "$out")
    completions=$(awk '/[0-9]+ requests in/{print $1}' "$out")
    timeouts=$(awk '/Socket errors:/{for(i=1;i<=NF;i++) if($i ~ /^timeout$/) print $(i+1)}' "$out" | tr -d ',')
    [ -z "$timeouts" ] && timeouts=0
    [ -z "$completions" ] && completions=0
    printf "%-12s %-6s rps=%-10s xfer/s=%-10s p50=%-9s p99=%-9s ok=%-7s timeouts=%s\n" \
        "$label" "$payload" "$rps" "$bps" "$p50" "$p99" "$completions" "$timeouts"
    echo "$label,$payload,$rps,$bps,$p50,$p99,$xfer,$completions,$timeouts" >> "$RESULTS"
}

echo
echo "==> warmup (1s)"
for p in $WAIT_PORTS; do
    wrk -t 2 -c 8 -d 1s "http://127.0.0.1:$p$URL_PREFIX/16k.bin" >/dev/null 2>&1 || true
done

echo
echo "==> run: duration=$DURATION conns=$CONNS threads=$THREADS rounds=$ROUNDS"
echo

for round in $(seq 1 "$ROUNDS"); do
    echo "--- round $round / $ROUNDS ---"
    for payload in $PAYLOADS; do
        # Direct competitor first, reference servers second -- so a
        # quick "tail -f" of the log is dominated by the comparison
        # that matters.
        run_one "pulsys"     $PORT_CORESERVER  $payload
        run_one "dingospeed"   $PORT_DINGOSPEED  $payload
        run_one "caddy"        $PORT_CADDY       $payload
        if [ "$HAVE_NGINX" = 1 ]; then
            run_one "nginx"    $PORT_NGINX       $payload
        fi
        run_one "go-net-http"  $PORT_NETHTTP     $payload
        echo
    done
    # Brief breather between rounds so the chip doesn't keep climbing
    # the thermal curve and skew later rounds downward.
    sleep 1
done

echo "results: $RESULTS"

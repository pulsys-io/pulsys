#!/usr/bin/env bash
# hf_download_bench.sh -- runs ON the EC2 bench instance.
#
# Times a real `hf download <model>` against pulsys in three modes:
#
#   direct  HF_ENDPOINT=https://huggingface.co            -- baseline (no proxy)
#   cold    HF_ENDPOINT=http://127.0.0.1:<port>           -- proxy cache empty,
#                                                            populates from
#                                                            huggingface.co
#   warm    HF_ENDPOINT=http://127.0.0.1:<port>           -- proxy cache hot,
#                                                            served by io_uring
#                                                            reactor over
#                                                            loopback
#
# Each phase wipes the *client* local-dir before downloading; the proxy
# cache survives between cold and warm so the second run hits sendfile.
#
# Usage (on the instance):
#   sudo /opt/pulsys-src/scripts/hf_download_bench.sh \
#       [model=Qwen/Qwen2.5-32B-Instruct] [skip_direct=0|1]
set -euo pipefail

MODEL="${1:-${MODEL:-Qwen/Qwen2.5-32B-Instruct}}"
SKIP_DIRECT="${2:-${SKIP_DIRECT:-0}}"
# 0 = leave page cache hot between cold and warm (matches the synthetic
#     io_uring bench: warm reads come from RAM via sendfile).
# 1 = drop_caches between cold and warm.  Forces warm to read from disk,
#     which on this gp3 EBS volume is throttled to 125 MiB/s baseline --
#     useful only for measuring the cold-page-cache floor.
DROP_PAGE_CACHE="${DROP_PAGE_CACHE:-0}"

PORT=18080
ADMIN_PORT=18099
LOG_DIR=/var/log/pulsys-real
PROXY_BIN=/usr/local/bin/pulsys
WORKERS="${WORKERS:-16}"
SATURATE_CONCURRENCY="${SATURATE_CONCURRENCY:-128}"
RESULTS_CSV="${RESULTS_CSV:-$LOG_DIR/results.csv}"

mkdir -p "$LOG_DIR"
# Prefer dedicated hf-data EBS volume when mounted (see scripts/mount-hf-data-volume.sh).
DATA_ROOT=/mnt/hf-data
if ! mountpoint -q "$DATA_ROOT" 2>/dev/null; then
	DATA_ROOT=/mnt
fi
CACHE_DIR="$DATA_ROOT/pulsys-cache-real"
CLIENT_DIR_DIRECT="$DATA_ROOT/hf-client-direct"
CLIENT_DIR_COLD="$DATA_ROOT/hf-client-cold"
CLIENT_DIR_WARM="$DATA_ROOT/hf-client-warm"
if [ ! -d "$DATA_ROOT" ]; then mkdir -p "$DATA_ROOT"; fi

# Official Python hf CLI accelerated by the Rust hf_transfer engine -- the only
# download client Pulsys benchmarks against.
# https://huggingface.co/docs/huggingface_hub/en/guides/cli
export HF_HUB_ENABLE_HF_TRANSFER=1
export HF_HUB_DISABLE_TELEMETRY=1
# Classic LFS resolve path (not Xet CDN). Keeps Authorization on every hop
# through Pulsys and matches a cleaner warm-loopback measurement.
export HF_HUB_DISABLE_XET=1
unset HF_HUB_DISABLE_IMPLICIT_TOKEN || true
export HF_HUB_DOWNLOAD_TIMEOUT=300

# Install huggingface_hub on fresh AMIs (new EC2 hosts do not have it by default).
# [cli] extra was removed in huggingface_hub >=1.0 (CLI is built-in now).
ensure_python_hf_cli() {
	if ! python3 -c 'import huggingface_hub' 2>/dev/null; then
		echo "==> installing official Python huggingface_hub (hf CLI)..."
		if ! command -v pip3 >/dev/null 2>&1; then
			sudo dnf install -y python3-pip
		fi
		sudo pip3 install -q huggingface_hub hf_transfer
		python3 -c 'import huggingface_hub' 2>/dev/null || {
			echo "FATAL: could not install huggingface_hub" >&2
			exit 1
		}
	fi
	# Xet CDN follow-ups rewrite to /_p/... and currently arrive without a
	# Pulsys Bearer on some client versions; for reproducible warm-loopback
	# numbers use classic LFS + hf_transfer instead.
	if python3 -c 'from huggingface_hub.utils._runtime import is_xet_available; raise SystemExit(0 if is_xet_available() else 1)' 2>/dev/null; then
		echo "==> uninstalling hf-xet so downloads use LFS + hf_transfer (not Xet CDN)"
		sudo pip3 uninstall -y hf-xet hf_xet 2>/dev/null || true
	fi
}

# Resolve HF_TOKEN for cold fills.  Source of truth is the Secrets Manager
# secret owned by the CDK stack (HfTokenSecretOut); the instance role has
# GetSecretValue on it, so the token never rides through SSM command history
# or gets baked into an AMI.  HF_TOKEN in env still wins for local runs.
load_hf_token() {
	if [ -z "${HF_TOKEN:-}" ] && [ -n "${HF_TOKEN_SECRET_ARN:-}" ]; then
		HF_TOKEN="$(aws secretsmanager get-secret-value \
			--secret-id "$HF_TOKEN_SECRET_ARN" \
			--query SecretString --output text 2>/dev/null || true)"
		export HF_TOKEN
	fi
	if [ -n "${HF_TOKEN:-}" ]; then
		# The python `hf` CLI reads HF_TOKEN; surface the (masked) presence
		# so log readers know auth is on.
		echo "  HF_TOKEN: set (sha256=$(printf '%s' "$HF_TOKEN" | sha256sum | cut -c1-8))"
		# Fail fast on a dead token instead of a confusing 401 mid-download.
		local code
		code="$(curl -s -o /dev/null -w '%{http_code}' \
			-H "Authorization: Bearer $HF_TOKEN" https://huggingface.co/api/whoami-v2 || true)"
		if [ "$code" != "200" ]; then
			echo "FATAL: HF token rejected by huggingface.co (whoami-v2 -> HTTP $code)" >&2
			echo "  refresh it: HF_TOKEN=hf_xxx scripts/run-aws-benchmarks.sh (puts value into the stack secret)" >&2
			exit 1
		fi
		echo "  HF_TOKEN: valid (whoami-v2 200)"
	else
		echo "FATAL: no HF token (env HF_TOKEN empty, secret lookup failed)" >&2
		echo "  expected HF_TOKEN_SECRET_ARN from the stack output HfTokenSecretOut" >&2
		exit 1
	fi
}

# Always invoke via python3 -m so we never pick up a Go binary named hf on PATH.
python_hf_download() {
	ensure_python_hf_cli
	python3 -m huggingface_hub.cli.hf download "$@"
}

stop_proxy() {
	pkill -f "$PROXY_BIN " 2>/dev/null || true
	# Wait up to 5s for the port to drain.
	for _ in $(seq 1 50); do
		ss -ltn "( sport = :$PORT )" 2>/dev/null | grep -q ":$PORT " || return 0
		sleep 0.1
	done
}

start_proxy() {
	local cache="$1"
	local mode="${2:-online}"   # online | offline | debug
	mkdir -p "$cache"
	local offline_flag=""
	local log_level="info"
	case "$mode" in
		offline)       offline_flag="-strict-offline" ;;
		debug)         log_level="debug" ;;
		offline-debug) offline_flag="-strict-offline"; log_level="debug" ;;
		online)        ;;
		*) echo "FATAL: unknown start_proxy mode: $mode" >&2; exit 2 ;;
	esac
	local out_log="$LOG_DIR/pulsys-$mode.out"
	"$PROXY_BIN" \
		-listen "127.0.0.1:$PORT" \
		-admin-listen "127.0.0.1:$ADMIN_PORT" \
		-cache-dir "$cache" \
		-listeners "$(nproc)" \
		-iouring \
		$offline_flag \
		-public-base-url "http://127.0.0.1:$PORT" \
		-read-timeout 30m -write-timeout 30m \
		-log-level "$log_level" \
		>"$out_log" 2>&1 &
	local pid=$!
	# Health probe: /healthz is local and works in both online and
	# offline mode (no upstream involved).
	for _ in $(seq 1 80); do
		if curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/healthz" | grep -q '^200$'; then
			echo "proxy up (pid $pid, port $PORT, mode=$mode, log=$out_log)"
			return
		fi
		sleep 0.1
	done
	echo "FATAL: pulsys ($mode) failed to come up; tail of log:"
	tail -50 "$out_log" || true
	exit 1
}

bytes_of_dir() {
	# Sum apparent size (LFS shards are dense blobs, so this is exact).
	du -sb "$1" 2>/dev/null | awk '{print $1}'
}

now() { python3 -c 'import time; print(f"{time.time():.6f}")'; }

# Resolve repo-relative scripts when running from /opt/pulsys-src or a checkout.
SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

ensure_python_hf_cli
load_hf_token

# Server uses PULSYS_HF_TOKEN for upstream; clients talking to Pulsys use BENCH_PAT.
if [ -z "${PULSYS_HF_TOKEN:-}" ] && [ -n "${HF_TOKEN:-}" ]; then
	export PULSYS_HF_TOKEN="$HF_TOKEN"
fi
if [ -z "${PULSYS_HF_TOKEN:-}" ]; then
	echo "FATAL: PULSYS_HF_TOKEN (or HF_TOKEN) required for cold fills" >&2
	exit 1
fi
UPSTREAM_HF_TOKEN="$PULSYS_HF_TOKEN"

# Pulsys has no open mode: local Postgres + one seeded API key for the data plane.
if [ -z "${PULSYS_DB_DSN:-}" ] || [ -z "${BENCH_PAT:-}" ]; then
	eval "$(bash "$SCRIPT_ROOT/scripts/bench_db_up.sh")"
fi
if [ -z "${BENCH_PAT:-}" ]; then
	echo "FATAL: bench_db_up.sh did not produce BENCH_PAT" >&2
	exit 1
fi
export PULSYS_DB_DSN PULSYS_HF_TOKEN PULSYS_IMPORT_WORKER=0

# Times a single download.  Args: label, endpoint, target_dir, client_token
time_one() {
	local label="$1" endpoint="$2" target="$3" client_token="${4:-}"
	rm -rf "$target"
	mkdir -p "$target"
	local t0 t1 bytes wall mbps gbps
	t0=$(now)
	HF_ENDPOINT="$endpoint" HF_TOKEN="$client_token" \
		python_hf_download "$MODEL" \
			--token "$client_token" \
			--local-dir "$target" \
			--max-workers "$WORKERS" \
			>"$LOG_DIR/$label.out" 2>"$LOG_DIR/$label.err" || {
				echo "  $label: python hf download FAILED; tail of stderr:"
				tail -20 "$LOG_DIR/$label.err"
				return 1
			}
	t1=$(now)
	bytes=$(bytes_of_dir "$target")
	wall=$(awk -v a="$t0" -v b="$t1" 'BEGIN { printf("%.3f\n", b - a) }')
	mbps=$(awk -v b="$bytes" -v w="$wall" 'BEGIN { if (w<=0) print 0; else printf("%.0f\n", b/w/1024/1024) }')
	gbps=$(awk -v b="$bytes" -v w="$wall" 'BEGIN { if (w<=0) print 0; else printf("%.2f\n", b*8/w/1e9) }')
	printf "  %-13s wall=%-8ss  bytes=%-14d  %6s MiB/s  %5s Gb/s\n" \
		"$label" "$wall" "$bytes" "$mbps" "$gbps"
	echo "$label,$wall,$bytes,$mbps,$gbps" >>"$RESULTS_CSV"
}

# Pull a handful of metrics from the proxy admin endpoint and print them
# so we can correlate cache misses / hits with the wallclock numbers.
proxy_stats() {
	local label="$1"
	echo "  -- proxy stats ($label):"
	curl -s "http://127.0.0.1:$ADMIN_PORT/debug/vars" 2>/dev/null \
		| python3 -c '
import json, sys
d = json.load(sys.stdin)
keys = [
	"pulsys_cache_hits", "pulsys_cache_misses",
	"pulsys_client_bytes_served", "pulsys_disk_bytes_written",
	"pulsys_artifact_upstream_bytes", "pulsys_artifact_upstream_fetches",
	"pulsys_metadata_upstream_bytes", "pulsys_metadata_upstream_fetches",
	"pulsys_sendfile_fused_calls", "pulsys_sendfile_body_only_calls",
	"pulsys_sendfile_eagains", "pulsys_io_uring_fused_calls",
]
for k in keys:
	if k in d:
		print(f"     {k:42s} {d[k]}")
' 2>/dev/null || true
}

echo "================================================================"
echo "  hf_download_bench:  $MODEL"
echo "  workers=$WORKERS  cache=$CACHE_DIR  proxy=$PROXY_BIN -iouring"
echo "================================================================"

mkdir -p "$LOG_DIR"
echo "phase,wall_s,bytes,MiB_s,Gb_s" >"$RESULTS_CSV"

if [ "$SKIP_DIRECT" != "1" ]; then
	echo
	echo "[1/5] direct -- HF_ENDPOINT=https://huggingface.co  (no proxy)"
	stop_proxy
	time_one direct "https://huggingface.co" "$CLIENT_DIR_DIRECT" "$UPSTREAM_HF_TOKEN"
	rm -rf "$CLIENT_DIR_DIRECT"
fi

echo
echo "[2/5] cold -- proxy cache empty, populating from huggingface.co"
stop_proxy
rm -rf "$CACHE_DIR"
start_proxy "$CACHE_DIR"
time_one cold "http://127.0.0.1:$PORT" "$CLIENT_DIR_COLD" "$BENCH_PAT"
rm -rf "$CLIENT_DIR_COLD"
proxy_stats "after cold"

echo
echo "[3/5] warm -- proxy cache populated, restart proxy (page cache kept hot)"
stop_proxy
if [ "$DROP_PAGE_CACHE" = "1" ]; then
	echo "  (DROP_PAGE_CACHE=1: dropping OS page cache -- this caps warm at the"
	echo "   EBS gp3 baseline throughput of 125 MiB/s and is not representative"
	echo "   of real warm-cache behaviour.  Use only to measure the cold-disk floor.)"
	sync
	echo 3 >/proc/sys/vm/drop_caches 2>/dev/null || true
fi
start_proxy "$CACHE_DIR"
time_one warm "http://127.0.0.1:$PORT" "$CLIENT_DIR_WARM" "$BENCH_PAT"
rm -rf "$CLIENT_DIR_WARM"
proxy_stats "after warm"

echo
echo "[4/5] warm saturate -- parallel curl ranges -> /dev/null (proxy ceiling)"
export HF_ENDPOINT="http://127.0.0.1:$PORT"
export MIN_MIB=4
if [ -x /opt/pulsys-src/scripts/bench_warm_saturate.sh ]; then
	bash /opt/pulsys-src/scripts/bench_warm_saturate.sh "$MODEL" "$SATURATE_CONCURRENCY" 16 \
		|| echo "  (saturate bench failed; continuing)"
else
	echo "  skip: bench_warm_saturate.sh not found"
fi

# ---------------------------------------------------------------------------
# [5/5] offline contract -- this is the production-critical assertion.
#
# After the cache is warmed, restart the proxy with -strict-offline.  Any request
# (artifact OR metadata) that misses the cache returns 504 with the exact
# (host, path, keyHex, range) in the body.  This catches both:
#
#   - artifact-body cache-key mismatches between clients
#   - missing metadata cache entries (tree, revision, etc.) -- without
#     these, the very first request from any download client 504s
#
# Expected behavior, byte-for-byte:
#   - Python `hf download` (hf_transfer) against offline proxy: SUCCESS, 0 upstream fetches.
#   - pulsys_offline_refusals MUST stay at 0.
#
# If anything 504s, the body of the 504 names the missing slot exactly.
# Together with the cache-key debug log it is enough to point at the bug.
# ---------------------------------------------------------------------------
echo
echo "[5/5] strict-offline -- restart proxy with -strict-offline (cache hit only, no egress)"
stop_proxy
start_proxy "$CACHE_DIR" offline-debug

OFFLINE_LOG="$LOG_DIR/offline-asserts.log"
: >"$OFFLINE_LOG"
OFFLINE_BASELINE_UPSTREAM=$(curl -s "http://127.0.0.1:$ADMIN_PORT/debug/vars" 2>/dev/null \
	| python3 -c '
import json, sys
try:
	d = json.load(sys.stdin)
except Exception:
	print(0); raise SystemExit
print(d.get("pulsys_artifact_upstream_bytes", 0))' 2>/dev/null || echo 0)
OFFLINE_BASELINE_REFUSALS=$(curl -s "http://127.0.0.1:$ADMIN_PORT/debug/vars" 2>/dev/null \
	| python3 -c '
import json, sys
try:
	d = json.load(sys.stdin)
except Exception:
	print(0); raise SystemExit
print(d.get("pulsys_offline_refusals", 0))' 2>/dev/null || echo 0)
echo "  baseline upstream_bytes=$OFFLINE_BASELINE_UPSTREAM refusals=$OFFLINE_BASELINE_REFUSALS" \
	| tee -a "$OFFLINE_LOG"

# python offline
time_one offline_python "http://127.0.0.1:$PORT" "$CLIENT_DIR_WARM" "$BENCH_PAT" || true
rm -rf "$CLIENT_DIR_WARM"
proxy_stats "after offline python"

# Assert no upstream egress occurred during offline phase.
OFFLINE_FINAL_UPSTREAM=$(curl -s "http://127.0.0.1:$ADMIN_PORT/debug/vars" 2>/dev/null \
	| python3 -c '
import json, sys
try:
	d = json.load(sys.stdin)
except Exception:
	print(0); raise SystemExit
print(d.get("pulsys_artifact_upstream_bytes", 0))' 2>/dev/null || echo 0)
OFFLINE_FINAL_REFUSALS=$(curl -s "http://127.0.0.1:$ADMIN_PORT/debug/vars" 2>/dev/null \
	| python3 -c '
import json, sys
try:
	d = json.load(sys.stdin)
except Exception:
	print(0); raise SystemExit
print(d.get("pulsys_offline_refusals", 0))' 2>/dev/null || echo 0)
OFFLINE_DELTA=$(( OFFLINE_FINAL_UPSTREAM - OFFLINE_BASELINE_UPSTREAM ))
REFUSAL_DELTA=$(( OFFLINE_FINAL_REFUSALS - OFFLINE_BASELINE_REFUSALS ))
{
	echo "  upstream_bytes_during_offline_phase = $OFFLINE_DELTA"
	echo "  offline_504_refusals_during_phase   = $REFUSAL_DELTA"
} | tee -a "$OFFLINE_LOG"

if [ "$OFFLINE_DELTA" != "0" ]; then
	echo "  HARD-FAIL: proxy egressed $OFFLINE_DELTA bytes during -offline phase" | tee -a "$OFFLINE_LOG"
fi
if [ "$REFUSAL_DELTA" != "0" ]; then
	echo "  HARD-FAIL: $REFUSAL_DELTA cache-miss-in-offline 504s; check $LOG_DIR/pulsys-offline-debug.out for key-hex of misses" | tee -a "$OFFLINE_LOG"
	echo "  -- key-hex log lines from offline proxy (first 40) --"
	grep -E 'cache-key' "$LOG_DIR/pulsys-offline-debug.out" 2>/dev/null | head -40 | tee -a "$OFFLINE_LOG"
fi

stop_proxy
echo
echo "================================================================"
echo "  results CSV: $RESULTS_CSV"
column -t -s, "$RESULTS_CSV"
echo "================================================================"

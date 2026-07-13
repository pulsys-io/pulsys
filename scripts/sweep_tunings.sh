#!/usr/bin/env bash
# sweep_tunings.sh -- A/B every Category 2 (behavioural) kernel tuning we
# are considering for the production AMI.  One variable at a time, with
# the stock baseline as the control, repeated $ROUNDS times per knob.
#
# Output:
#   /var/lib/pulsys/sweep/<timestamp>/sweep.csv
#   /var/lib/pulsys/sweep/<timestamp>/sweep-report.md
#   /var/lib/pulsys/sweep/<timestamp>.tar.gz
#
# CSV schema:
#   tuning,value,payload,conns,round,rps,bytes_per_s,p50_us,p99_us,p99_9_us,errors
#
# The report is what gets written to tmp/bench/tunings-report.md (Track B2):
# any tuning whose median RPS delta vs baseline is positive and whose 95%
# confidence interval does not cross zero is marked as a "survivor" and
# baked into bench-ami.pkr.hcl.  Everything else is left out.
#
# Design notes:
#   - Each knob is applied and then reverted.  We re-snapshot the relevant
#     sysctl value before applying, so subsequent rebuilds of the AMI
#     remain idempotent.
#   - We deliberately do NOT run the knobs cumulatively.  Compound effects
#     are measured in a second pass after the single-variable winners are
#     picked.
#   - Per-tuning runs use the same warmed-cache pulsys + fake-hf as
#     profile_baseline.sh, so flamegraphs from one are directly comparable
#     to the wrk numbers from the other.
#
# Environment knobs:
#   DURATION=30      seconds per wrk run
#   ROUNDS=3         repetitions per (tuning, payload, conns) cell
#   CONNS="64 256"   concurrency levels to sweep
#   PAYLOADS="4k 256k 16m"
#                    payload mix; small=cache-hot, mid=metadata, large=
#                    safetensors shard.  Keep narrow; the sweep already
#                    produces N tunings * N payloads * N conns * N rounds
#                    runs.
#   ONLY=""          if set, comma-separated list of tuning names to run
#                    (everything else skipped); useful for retesting one.
set -euo pipefail
export PATH=/usr/local/go/bin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

DURATION="${DURATION:-30}"
ROUNDS="${ROUNDS:-3}"
CONNS="${CONNS:-64 256}"
PAYLOADS="${PAYLOADS:-4k 256k 16m}"
ONLY="${ONLY:-}"

PORT_FAKEHF=18484
PORT_HFPROXY=18687
URL_PREFIX="/models/bench/bench/resolve/main"
CACHE_DIR=/var/lib/pulsys/cache
SWEEP_ROOT=/var/lib/pulsys/sweep
TS="$(date -u +%Y%m%dT%H%M%SZ)"
SWEEP_DIR="$SWEEP_ROOT/$TS"
CSV="$SWEEP_DIR/sweep.csv"

mkdir -p "$SWEEP_DIR" "$CACHE_DIR"

log() { echo "[$(date -u +%H:%M:%S)] $*"; }

# ----- helpers ------------------------------------------------------------
cleanup_procs() {
    pkill -f /usr/local/bin/pulsys 2>/dev/null || true
    pkill -f /usr/local/bin/fake-hf  2>/dev/null || true
    pkill -f /usr/local/bin/wrk      2>/dev/null || true
    sleep 0.3
}
trap cleanup_procs EXIT
cleanup_procs

wait_for() {
    local port="$1" path="$2"
    for _ in $(seq 1 100); do
        code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 \
            "http://127.0.0.1:$port$path" || true)"
        case "$code" in 200|206|302|303|307) return 0 ;; esac
        sleep 0.1
    done
    echo "FATAL: port $port unhealthy" >&2; exit 1
}

start_stack() {
    cleanup_procs
    rm -rf "$CACHE_DIR"/*
    /usr/local/bin/fake-hf -listen "127.0.0.1:$PORT_FAKEHF" \
        >>"$SWEEP_DIR/fakehf.log" 2>&1 &
    wait_for "$PORT_FAKEHF" "/api/models/bench/bench"
    /usr/local/bin/pulsys \
        -listen "0.0.0.0:$PORT_HFPROXY" \
        -admin-listen "127.0.0.1:18099" \
        -cache-dir "$CACHE_DIR" \
        -default-upstream-host "127.0.0.1:$PORT_FAKEHF" \
        -upstream-scheme http \
        -public-base-url "http://127.0.0.1:$PORT_HFPROXY" \
        -allow-host 127.0.0.1 \
        -log-level warn \
        >>"$SWEEP_DIR/pulsys.log" 2>&1 &
    wait_for "$PORT_HFPROXY" "$URL_PREFIX/4k.bin"
}

warm() {
    local payload="$1"
    curl -s -o /dev/null --max-time 60 \
        "http://127.0.0.1:$PORT_HFPROXY$URL_PREFIX/${payload}.bin"
}

# ----- one wrk measurement -> one CSV row ---------------------------------
# Args: tuning_name tuning_value payload conns round
measure() {
    local tuning="$1" tval="$2" payload="$3" conns="$4" round="$5"
    local ncpu threads
    ncpu="$(nproc)"
    threads=$(( conns < ncpu/2 ? conns : ncpu/2 ))
    [ "$threads" -lt 1 ] && threads=1

    local out="$SWEEP_DIR/wrk_${tuning}_${payload}_c${conns}_r${round}.txt"
    wrk -t "$threads" -c "$conns" -d "${DURATION}s" --latency --timeout 30s \
        "http://127.0.0.1:$PORT_HFPROXY$URL_PREFIX/${payload}.bin" \
        > "$out" 2>&1 || true

    local rps bps p50 p99 p99_9 errs
    rps=$(awk '/Requests\/sec:/{print $2}' "$out")
    bps=$(awk '/Transfer\/sec:/{print $2}' "$out")
    p50=$(awk '/^[[:space:]]*50%/{print $2}'   "$out")
    p99=$(awk '/^[[:space:]]*99%/{print $2}'   "$out")
    p99_9=$(awk '/^[[:space:]]*99\.999%/{print $2}' "$out")
    [ -z "$p99_9" ] && p99_9=$(awk '/^[[:space:]]*99\.99%/{print $2}' "$out")
    errs=$(awk '/Socket errors:/{for(i=1;i<=NF;i++) if($i ~ /^connect/) print $(i+1)}' "$out" | tr -d ',')
    : "${rps:=0}"; : "${bps:=0B}"; : "${p50:=0us}"; : "${p99:=0us}"; : "${p99_9:=0us}"; : "${errs:=0}"

    printf "%-26s %-10s %-5s c=%-3s r=%-2s  rps=%-10s p50=%-8s p99=%-8s\n" \
        "$tuning" "$tval" "$payload" "$conns" "$round" "$rps" "$p50" "$p99"
    echo "$tuning,$tval,$payload,$conns,$round,$rps,$bps,$p50,$p99,$p99_9,$errs" \
        >> "$CSV"
}

# ----- tuning definitions -------------------------------------------------
#
# Each tuning is a triple of functions:
#   <name>_apply   -- saves current state, applies the change
#   <name>_revert  -- restores the prior state
#   <name>_value   -- the value string recorded in CSV's `value` column
#
# To add a new tuning: define those three functions, then append the name
# to the TUNINGS array below.
#
# Restoration must be COMPLETE: re-running the sweep multiple times in a
# row must not drift the kernel state.  We snapshot to /tmp/sweep-state-*
# files which the revert function reads.

STATE_DIR=/tmp/sweep-state
mkdir -p "$STATE_DIR"

# --- baseline: no-op ------------------------------------------------------
baseline_value()  { echo "stock"; }
baseline_apply()  { :; }
baseline_revert() { :; }

# --- BBR congestion control ----------------------------------------------
bbr_value()  { echo "bbr"; }
bbr_apply() {
    sysctl -n net.ipv4.tcp_congestion_control > "$STATE_DIR/cc"
    modprobe tcp_bbr 2>/dev/null || true
    sysctl -w net.ipv4.tcp_congestion_control=bbr >/dev/null
}
bbr_revert() {
    [ -f "$STATE_DIR/cc" ] && sysctl -w "net.ipv4.tcp_congestion_control=$(cat "$STATE_DIR/cc")" >/dev/null
}

# --- TCP Fast Open (server + client) -------------------------------------
tfo_value()  { echo "3"; }
tfo_apply() {
    sysctl -n net.ipv4.tcp_fastopen > "$STATE_DIR/tfo"
    sysctl -w net.ipv4.tcp_fastopen=3 >/dev/null
}
tfo_revert() {
    [ -f "$STATE_DIR/tfo" ] && sysctl -w "net.ipv4.tcp_fastopen=$(cat "$STATE_DIR/tfo")" >/dev/null
}

# --- disable slow-start-after-idle ---------------------------------------
no_ssi_value()  { echo "0"; }
no_ssi_apply() {
    sysctl -n net.ipv4.tcp_slow_start_after_idle > "$STATE_DIR/ssi"
    sysctl -w net.ipv4.tcp_slow_start_after_idle=0 >/dev/null
}
no_ssi_revert() {
    [ -f "$STATE_DIR/ssi" ] && sysctl -w "net.ipv4.tcp_slow_start_after_idle=$(cat "$STATE_DIR/ssi")" >/dev/null
}

# --- skip TCP metrics save -----------------------------------------------
no_metrics_value()  { echo "1"; }
no_metrics_apply() {
    sysctl -n net.ipv4.tcp_no_metrics_save > "$STATE_DIR/nm"
    sysctl -w net.ipv4.tcp_no_metrics_save=1 >/dev/null
}
no_metrics_revert() {
    [ -f "$STATE_DIR/nm" ] && sysctl -w "net.ipv4.tcp_no_metrics_save=$(cat "$STATE_DIR/nm")" >/dev/null
}

# --- TCP notsent_lowat ----------------------------------------------------
# Reduce kernel write queue depth; HOL-blocks less under burst.  16 KiB is
# the Linux mailing-list "this is small enough to matter" recommendation.
notsent_lowat_value()  { echo "16384"; }
notsent_lowat_apply() {
    sysctl -n net.ipv4.tcp_notsent_lowat > "$STATE_DIR/nsl"
    sysctl -w net.ipv4.tcp_notsent_lowat=16384 >/dev/null
}
notsent_lowat_revert() {
    [ -f "$STATE_DIR/nsl" ] && sysctl -w "net.ipv4.tcp_notsent_lowat=$(cat "$STATE_DIR/nsl")" >/dev/null
}

# --- CPU governor = performance -------------------------------------------
governor_value()  { echo "performance"; }
governor_apply() {
    if [ -d /sys/devices/system/cpu/cpu0/cpufreq ]; then
        # Save current; assume uniform across CPUs.
        cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor > "$STATE_DIR/gov"
        for f in /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor; do
            echo performance > "$f" 2>/dev/null || true
        done
    else
        # AWS Graviton / Nitro: no cpufreq sysfs; nothing to do.
        echo "(no cpufreq sysfs)" > "$STATE_DIR/gov"
    fi
}
governor_revert() {
    if [ -f "$STATE_DIR/gov" ] && [ "$(cat "$STATE_DIR/gov")" != "(no cpufreq sysfs)" ]; then
        for f in /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor; do
            cat "$STATE_DIR/gov" > "$f" 2>/dev/null || true
        done
    fi
}

# --- Transparent Huge Pages = madvise ------------------------------------
thp_value()  { echo "madvise"; }
thp_apply() {
    cat /sys/kernel/mm/transparent_hugepage/enabled > "$STATE_DIR/thp"
    echo madvise > /sys/kernel/mm/transparent_hugepage/enabled 2>/dev/null || true
}
thp_revert() {
    if [ -f "$STATE_DIR/thp" ]; then
        local prev
        prev=$(sed -n 's/.*\[\([^]]*\)\].*/\1/p' "$STATE_DIR/thp")
        [ -n "$prev" ] && echo "$prev" > /sys/kernel/mm/transparent_hugepage/enabled 2>/dev/null || true
    fi
}

# --- RPS (Receive Packet Steering) across all cores ----------------------
rps_value()  { echo "all-cores"; }
rps_apply() {
    NCPU="$(nproc)"
    MASK=$(printf '%x' $(( (1 << NCPU) - 1 )))
    # Save current rps_cpus on the first NIC.
    for q in /sys/class/net/*/queues/rx-*/rps_cpus; do
        cp "$q" "$STATE_DIR/$(echo "$q" | tr '/' '_')" 2>/dev/null || true
        echo "$MASK" > "$q" 2>/dev/null || true
    done
    sysctl -w net.core.rps_sock_flow_entries=32768 >/dev/null 2>&1 || true
}
rps_revert() {
    for q in /sys/class/net/*/queues/rx-*/rps_cpus; do
        local saved="$STATE_DIR/$(echo "$q" | tr '/' '_')"
        [ -f "$saved" ] && cp "$saved" "$q" 2>/dev/null || true
    done
}

# --- irqbalance disabled (we rely on RPS/RFS instead) --------------------
no_irqbalance_value()  { echo "stopped"; }
no_irqbalance_apply() {
    systemctl is-active irqbalance > "$STATE_DIR/irq" 2>/dev/null || echo inactive > "$STATE_DIR/irq"
    systemctl stop irqbalance 2>/dev/null || true
}
no_irqbalance_revert() {
    if [ -f "$STATE_DIR/irq" ] && grep -q '^active' "$STATE_DIR/irq"; then
        systemctl start irqbalance 2>/dev/null || true
    fi
}

# --- busy_poll (NAPI busy-poll for waking responsiveness) ----------------
busy_poll_value()  { echo "50"; }
busy_poll_apply() {
    sysctl -n net.core.busy_poll > "$STATE_DIR/bp"
    sysctl -n net.core.busy_read > "$STATE_DIR/br"
    sysctl -w net.core.busy_poll=50 >/dev/null
    sysctl -w net.core.busy_read=50 >/dev/null
}
busy_poll_revert() {
    [ -f "$STATE_DIR/bp" ] && sysctl -w "net.core.busy_poll=$(cat "$STATE_DIR/bp")" >/dev/null
    [ -f "$STATE_DIR/br" ] && sysctl -w "net.core.busy_read=$(cat "$STATE_DIR/br")" >/dev/null
}

# Ordered list of (name, apply, revert).  baseline MUST come first; we use
# its rows as the reference for all delta% computations downstream.
TUNINGS=(
    baseline
    bbr
    tfo
    no_ssi
    no_metrics
    notsent_lowat
    governor
    thp
    rps
    no_irqbalance
    busy_poll
)

# ----- header -------------------------------------------------------------
echo "tuning,value,payload,conns,round,rps,bytes_per_s,p50,p99,p99_9,errors" > "$CSV"

# ----- main loop ----------------------------------------------------------
log "sweep starting: ${#TUNINGS[@]} tunings x ${ROUNDS} rounds, payloads=$PAYLOADS, conns=$CONNS"
log "output: $CSV"

for tuning in "${TUNINGS[@]}"; do
    if [ -n "$ONLY" ] && ! echo ",$ONLY," | grep -q ",$tuning,"; then
        continue
    fi

    log "===== $tuning ====="
    "${tuning}_apply"
    tval="$(${tuning}_value)"

    # Fresh stack for each tuning so prior cache state does not leak.
    start_stack
    for p in $PAYLOADS; do
        warm "$p"
    done
    # Settle.
    sleep 1

    for p in $PAYLOADS; do
        for c in $CONNS; do
            for r in $(seq 1 "$ROUNDS"); do
                measure "$tuning" "$tval" "$p" "$c" "$r"
            done
        done
    done

    cleanup_procs
    "${tuning}_revert"
done

# ----- post-process: render summary ---------------------------------------
log "rendering summary"

awk -F, 'NR>1 {
    key = $1 "," $3 "," $4
    rps[key] += $6; n[key]++
    if ($1=="baseline") base[$3 "," $4] = rps[key] / n[key]
}
END {
    print "tuning,payload,conns,rps_mean,delta_pct_vs_baseline" \
        > "/dev/stderr"
    for (k in rps) {
        split(k, kk, ",")
        tuning = kk[1]; payload = kk[2]; conns = kk[3]
        mean = rps[k] / n[k]
        bkey = payload "," conns
        if (bkey in base && base[bkey] > 0) {
            delta = (mean - base[bkey]) / base[bkey] * 100
        } else {
            delta = 0
        }
        printf "%s,%s,%s,%.0f,%+.1f\n", tuning, payload, conns, mean, delta
    }
}' "$CSV" 2> "$SWEEP_DIR/sweep-summary.csv"

sort -t, -k1,1 -k2,2 -k3,3n "$SWEEP_DIR/sweep-summary.csv" \
    -o "$SWEEP_DIR/sweep-summary.csv"

{
    echo "# Tunings sweep report -- $TS"
    echo
    echo "Generated by \`scripts/sweep_tunings.sh\` on a stock AMI."
    echo
    echo "Kernel: $(uname -r)"
    echo "vCPUs:  $(nproc)"
    echo
    echo "## Per-tuning delta vs baseline (mean RPS)"
    echo
    echo "| tuning | payload | conns | mean rps | delta % |"
    echo "|---|---|---|---|---|"
    awk -F, '{printf "| %s | %s | %s | %s | %s |\n", $1, $2, $3, $4, $5}' \
        "$SWEEP_DIR/sweep-summary.csv"
    echo
    echo "Survivor rule: a tuning is baked into the production AMI only if"
    echo "its mean RPS delta is positive across **all** (payload, conns)"
    echo "cells and no cell shows a regression worse than -1%."
} > "$SWEEP_DIR/sweep-report.md"

# ----- tarball ------------------------------------------------------------
cd "$SWEEP_ROOT"
TARBALL="${TS}.tar.gz"
tar -czf "$TARBALL" "$TS"
chmod 0644 "$TARBALL"

echo
echo "SWEEP_ARTIFACT=$SWEEP_ROOT/$TARBALL"
echo "SWEEP_CSV=$CSV"
echo "SWEEP_REPORT=$SWEEP_DIR/sweep-report.md"

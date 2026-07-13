#!/usr/bin/env bash
# profile_baseline.sh -- single-run profiling harness for the stock AMI.
#
# Drives wrk against pulsys for $DURATION seconds at $CONNS connections
# and concurrently captures:
#
#   * perf record -F 99 -g          (sampling profile for flamegraph)
#   * bpftrace syscount             (syscall name -> count)
#   * mpstat -P ALL 1 N             (per-core %usr/%sys/%irq/%soft)
#   * iostat -xz 1 N                (disk wait + utilization)
#   * ss -tin (before/after)        (per-socket retransmits, cwnd, rwnd)
#   * /proc/net/snmp (before/after) (kernel TCP counters)
#
# Output: /var/lib/pulsys/profile-runs/<timestamp>/ containing all raw
# artifacts plus flame.svg.  Tarball is written to .../<timestamp>.tar.gz
# and printed to stdout for the SSM document to upload to S3.
#
# Run via SSM with the PulsysProfile document (Track C); env vars come
# from SSM document parameters.
#
# Environment knobs (with defaults):
#   DURATION=60           seconds of wrk
#   CONNS=384             keep-alive conns (matches bench_saturate default)
#   THREADS=max           wrk -t value; "max"=vCPU, "auto"=min(CONNS, vCPU/2)
#   PAYLOAD=4k            warm-hit payload size; one of 4k|64k|256k|4m|16m
#   ROUND_TAG=baseline    tag used in artifact directory name
#   PULSYS_VARIANT=     server shape (must match bench_saturate defaults):
#                         saturate-no-cork (default on EC2: 96 listeners, cork off)
#                         saturate | saturate-iouring | no-cork | iouring | …
set -euo pipefail
export PATH=/usr/local/go/bin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

DURATION="${DURATION:-60}"
CONNS="${CONNS:-384}"
THREADS="${THREADS:-max}"
PAYLOAD="${PAYLOAD:-4k}"
ROUND_TAG="${ROUND_TAG:-baseline}"
VARIANT="${PULSYS_VARIANT:-saturate-no-cork}"

PORT_FAKEHF=18484
PORT_HFPROXY=18687
URL_PREFIX="/models/bench/bench/resolve/main"
CACHE_DIR=/var/lib/pulsys/cache
RUN_ROOT=/var/lib/pulsys/profile-runs
TS="$(date -u +%Y%m%dT%H%M%SZ)"
RUN_DIR="$RUN_ROOT/${TS}-${ROUND_TAG}"

mkdir -p "$RUN_DIR" "$CACHE_DIR"
chown -R pulsys:pulsys "$CACHE_DIR" 2>/dev/null || true

# ----- thread count -------------------------------------------------------
NCPU="$(nproc)"
if [ "$THREADS" = "max" ]; then
    THREADS="$NCPU"
elif [ "$THREADS" = "auto" ]; then
    THREADS=$(( CONNS < NCPU/2 ? CONNS : NCPU/2 ))
    [ "$THREADS" -lt 1 ] && THREADS=1
fi

# ----- helpers ------------------------------------------------------------
log() { echo "[$(date -u +%H:%M:%S)] $*"; }

cleanup() {
    pkill -f /usr/local/bin/pulsys 2>/dev/null || true
    pkill -f /usr/local/bin/fake-hf  2>/dev/null || true
    pkill -f /usr/local/bin/wrk      2>/dev/null || true
    pkill -f "perf record"           2>/dev/null || true
    pkill -f bpftrace                2>/dev/null || true
    pkill -f mpstat                  2>/dev/null || true
    pkill -f iostat                  2>/dev/null || true
    sleep 0.3
}
trap cleanup EXIT
cleanup

wait_for() {
    local port="$1" path="$2"
    for _ in $(seq 1 100); do
        code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 \
            "http://127.0.0.1:$port$path" || true)"
        case "$code" in
            200|206|302|303|307) return 0 ;;
        esac
        sleep 0.1
    done
    echo "FATAL: port $port path $path never returned a healthy status" >&2
    exit 1
}

# ----- start fake-hf + pulsys -------------------------------------------
log "starting fake-hf on :$PORT_FAKEHF"
/usr/local/bin/fake-hf -listen "127.0.0.1:$PORT_FAKEHF" \
    >"$RUN_DIR/fakehf.log" 2>&1 &
wait_for "$PORT_FAKEHF" "/api/models/bench/bench"

log "starting pulsys on :$PORT_HFPROXY (variant=$VARIANT)"
rm -rf "$CACHE_DIR"/*

variant_flags=""
case "$VARIANT" in
    default) ;;
    no-cork)   variant_flags="-tcp-cork=false" ;;
    cork)      variant_flags="-tcp-cork=true" ;;
    iouring)   variant_flags="-iouring=true" ;;
    reuseport) variant_flags="-listeners=$NCPU" ;;
    saturate|saturate-no-cork|saturate-iouring)
        # Match bench_saturate.sh on this host: one process, listeners=vCPU.
        # NUMA sharding is skipped so perf stays on one PID (use numa-shards
        # for multi-PID locality).  saturate-no-cork is the EC2 default.
        variant_flags="-listeners=$NCPU"
        case "$VARIANT" in
            saturate-no-cork) variant_flags="$variant_flags -tcp-cork=false" ;;
            saturate-iouring) variant_flags="$variant_flags -iouring=true" ;;
            saturate)         ;; # cork on (A/B only)
        esac
        ;;
    numa-shards)
        # Optional multi-PID profile.  Requires numactl; falls back to
        # single process if unavailable.  perf attaches to the highest
        # PID for the flamegraph; numastat-pid-after.txt covers the
        # rest.
        :
        ;;
    *)
        echo "FATAL: unknown PULSYS_VARIANT=$VARIANT" >&2
        exit 2
        ;;
esac

if [ "$VARIANT" = "numa-shards" ] && command -v numactl >/dev/null 2>&1; then
    NODES="$(numactl --hardware 2>/dev/null | awk '/^available:/ {print $2; exit}')"
    [ -z "$NODES" ] || [ "$NODES" -lt 1 ] 2>/dev/null && NODES=1
    log "numa-shards: launching $NODES pinned pulsys process(es)"
    n=0
    while [ "$n" -lt "$NODES" ]; do
        adminPort=$((18099 + n))
        numactl --cpunodebind="$n" --membind="$n" -- \
            /usr/local/bin/pulsys \
                -listen "0.0.0.0:$PORT_HFPROXY" \
                -admin-listen "127.0.0.1:$adminPort" \
                -cache-dir "$CACHE_DIR" \
                -default-upstream-host "127.0.0.1:$PORT_FAKEHF" \
                -upstream-scheme http \
                -public-base-url "http://127.0.0.1:$PORT_HFPROXY" \
                -allow-host 127.0.0.1 \
                -log-level warn \
                -listeners=1 \
                >"$RUN_DIR/pulsys-node${n}.log" 2>&1 &
        n=$((n + 1))
    done
else
    # shellcheck disable=SC2086
    /usr/local/bin/pulsys \
        -listen "0.0.0.0:$PORT_HFPROXY" \
        -admin-listen "127.0.0.1:18099" \
        -cache-dir "$CACHE_DIR" \
        -default-upstream-host "127.0.0.1:$PORT_FAKEHF" \
        -upstream-scheme http \
        -public-base-url "http://127.0.0.1:$PORT_HFPROXY" \
        -allow-host 127.0.0.1 \
        -log-level warn \
        $variant_flags \
        >"$RUN_DIR/pulsys.log" 2>&1 &
fi
wait_for "$PORT_HFPROXY" "$URL_PREFIX/4k.bin"

# Warm the requested payload into the cache.
log "warming $PAYLOAD"
curl -s -o /dev/null --max-time 60 \
    "http://127.0.0.1:$PORT_HFPROXY$URL_PREFIX/${PAYLOAD}.bin"

HFPID="$(pgrep -f /usr/local/bin/pulsys | head -1)"
if [ -z "$HFPID" ]; then
    echo "FATAL: pulsys did not start" >&2
    exit 1
fi
log "pulsys pid=$HFPID (variant=$VARIANT)"
echo "$VARIANT" > "$RUN_DIR/variant.txt"

# ----- preflight snapshots ------------------------------------------------
ss -tin > "$RUN_DIR/ss-before.txt"
cp /proc/net/snmp "$RUN_DIR/snmp-before.txt"
cp /proc/net/netstat "$RUN_DIR/netstat-before.txt"
uname -a               >"$RUN_DIR/uname.txt"
cat /proc/cmdline       >"$RUN_DIR/cmdline.txt"
sysctl -a 2>/dev/null   >"$RUN_DIR/sysctl-all.txt"
cat /proc/cpuinfo       >"$RUN_DIR/cpuinfo.txt"
free -h                 >"$RUN_DIR/free.txt"

# ----- NUMA topology + locality snapshots --------------------------------
# On a 96-vCPU bare metal host we are almost certainly running across 2+
# NUMA nodes (c7i.metal-24xl: dual-socket Sapphire Rapids; SNC may add
# more).  Capture topology up-front so the next-step decision ("do we
# need to shard pulsys per node?") is grounded in evidence, not guess.
#
# numastat -p captures the per-PID page distribution: numa_hit /
# numa_miss / other_node tell us whether sendfile is hauling page-cache
# bytes across the inter-socket link.
numactl --hardware           > "$RUN_DIR/numa-hardware.txt" 2>&1 || true
numactl --show               > "$RUN_DIR/numa-show.txt"     2>&1 || true
lscpu                        > "$RUN_DIR/lscpu.txt"         2>&1 || true
lscpu --extended             > "$RUN_DIR/lscpu-extended.txt" 2>&1 || true
hwloc-ls --no-io             > "$RUN_DIR/hwloc-ls.txt"      2>&1 || true
numastat                     > "$RUN_DIR/numastat-system-before.txt" 2>&1 || true
numastat -p "$HFPID"         > "$RUN_DIR/numastat-pid-before.txt"    2>&1 || true

# ----- start observers ----------------------------------------------------
log "starting perf record (F=99, -g, pid=$HFPID, ${DURATION}s)"
perf record -F 99 -g -p "$HFPID" \
    -o "$RUN_DIR/perf.data" -- sleep "$DURATION" >"$RUN_DIR/perf.log" 2>&1 &
PERF_PID=$!

log "starting bpftrace syscount"
bpftrace -e '
tracepoint:syscalls:sys_enter_* { @syscalls[probe] = count(); }
interval:s:'"$DURATION"' { exit(); }
' >"$RUN_DIR/syscount.txt" 2>&1 &
BPF_SYSCOUNT_PID=$!

log "starting bpftrace cpudist for pulsys on-CPU time"
bpftrace -e '
tracepoint:sched:sched_switch /args->prev_pid == '"$HFPID"' / {
    @oncpu_us = hist((nsecs - @last) / 1000);
}
tracepoint:sched:sched_switch /args->next_pid == '"$HFPID"' / {
    @last = nsecs;
}
interval:s:'"$DURATION"' { exit(); }
' >"$RUN_DIR/cpudist.txt" 2>&1 &
BPF_CPUDIST_PID=$!

mpstat -P ALL 1 "$DURATION" > "$RUN_DIR/mpstat.txt" 2>&1 &
MPSTAT_PID=$!

iostat -xz 1 "$DURATION" > "$RUN_DIR/iostat.txt" 2>&1 &
IOSTAT_PID=$!

# ----- run wrk ------------------------------------------------------------
log "wrk -t$THREADS -c$CONNS -d${DURATION}s payload=$PAYLOAD"
wrk -t "$THREADS" -c "$CONNS" -d "${DURATION}s" --latency --timeout 30s \
    "http://127.0.0.1:$PORT_HFPROXY$URL_PREFIX/${PAYLOAD}.bin" \
    >"$RUN_DIR/wrk.txt" 2>&1 || true

# Wait for observers to finish their fixed duration.
wait "$PERF_PID"          || true
wait "$BPF_SYSCOUNT_PID"  || true
wait "$BPF_CPUDIST_PID"   || true
wait "$MPSTAT_PID"        || true
wait "$IOSTAT_PID"        || true

# ----- post snapshots -----------------------------------------------------
ss -tin > "$RUN_DIR/ss-after.txt"
cp /proc/net/snmp     "$RUN_DIR/snmp-after.txt"
cp /proc/net/netstat  "$RUN_DIR/netstat-after.txt"
numastat              > "$RUN_DIR/numastat-system-after.txt" 2>&1 || true
numastat -p "$HFPID"  > "$RUN_DIR/numastat-pid-after.txt"    2>&1 || true

# Snapshot pulsys's own expvar counters for steady-state sanity.
curl -s --max-time 2 "http://127.0.0.1:18099/debug/vars" \
    > "$RUN_DIR/expvars.json" 2>/dev/null || true

# ----- flamegraph (non-fatal: tarball still ships if perf failed) ---------
log "generating flamegraph"
if [ -s "$RUN_DIR/perf.data" ]; then
    set +e
    set +o pipefail
    perf script -i "$RUN_DIR/perf.data" 2>"$RUN_DIR/perf-script.err" \
        | stackcollapse-perf.pl > "$RUN_DIR/folded.txt" 2>>"$RUN_DIR/perf-script.err"
    if [ -s "$RUN_DIR/folded.txt" ]; then
        flamegraph.pl --title="pulsys ${ROUND_TAG} ${PAYLOAD}" \
            < "$RUN_DIR/folded.txt" \
            > "$RUN_DIR/flame.svg" 2>>"$RUN_DIR/perf-script.err"
    else
        log "perf.data produced no folded stacks (see perf-script.err); skipping flamegraph"
    fi
    set -eo pipefail
else
    log "no perf.data (perf may have needed CAP_PERFMON); skipping flamegraph"
fi

# ----- summarize ----------------------------------------------------------
NUMA_NODE_COUNT="$(awk '/^available:/ {print $2; exit}' "$RUN_DIR/numa-hardware.txt" 2>/dev/null || echo "?")"

# mpstat 'all' aggregate over the run window (the line right before the
# Average block).  Pull %usr, %sys, %soft, %idle so the io_uring decision
# rule below has numbers to gate on.
mpstat_avg="$(awk '/^Average:/ && $2=="all" {print; exit}' "$RUN_DIR/mpstat.txt" 2>/dev/null || true)"
pct_sys="$(echo "$mpstat_avg" | awk '{for(i=1;i<=NF;i++) if($i~/^[0-9.]+$/) a[++n]=$i} END{print a[3]+0}')"
pct_usr="$(echo "$mpstat_avg" | awk '{for(i=1;i<=NF;i++) if($i~/^[0-9.]+$/) a[++n]=$i} END{print a[1]+0}')"
pct_soft="$(echo "$mpstat_avg" | awk '{for(i=1;i<=NF;i++) if($i~/^[0-9.]+$/) a[++n]=$i} END{print a[6]+0}')"

# Total syscalls + write+sendfile share.
sys_total="$(awk '/^@syscalls/ {gsub(/[\[\],:]/,"",$0); for(i=1;i<=NF;i++) if($i~/^[0-9]+$/) t+=$i} END{print t+0}' "$RUN_DIR/syscount.txt" 2>/dev/null || echo 0)"
getsyscall() {
    local n
    n=$(awk -v want="sys_enter_$1]" '$0 ~ want {gsub(/[\[\],:]/,""); for(i=1;i<=NF;i++) if($i~/^[0-9]+$/){print $i; exit}}' "$RUN_DIR/syscount.txt" 2>/dev/null)
    printf '%s' "${n:-0}"
}
sys_write="$(getsyscall write)"
sys_sendfile64="$(getsyscall sendfile64)"
sys_sendfile="$(getsyscall sendfile)"
sys_setsockopt="$(getsyscall setsockopt)"
sys_read="$(getsyscall read)"
sys_epoll_ctl="$(getsyscall epoll_ctl)"
sys_epoll_wait="$(getsyscall epoll_wait)"
sys_io_uring_enter="$(getsyscall io_uring_enter)"
# sendfile and sendfile64 both report; sum them.
sys_sf_total=$(( sys_sendfile + sys_sendfile64 ))
# Response-path = write + sendfile + setsockopt(cork) + epoll_ctl(register).
response_total=$(( sys_write + sys_sf_total + sys_setsockopt + sys_epoll_ctl ))
response_share=0
hot_share=0
if [ "${sys_total:-0}" -gt 0 ]; then
    response_share="$(awk -v r="$response_total" -v t="$sys_total" 'BEGIN{printf "%.1f", r*100/t}')"
    hot_share="$(awk -v w="${sys_write:-0}" -v s="$sys_sf_total" -v t="$sys_total" 'BEGIN{printf "%.1f", (w+s)*100/t}')"
fi

{
    echo "# pulsys profile run -- ${TS} ${ROUND_TAG} (variant=${VARIANT})"
    echo
    echo "Kernel:     $(uname -r)"
    echo "vCPUs:      $NCPU"
    echo "Memory:     $(awk '/MemTotal/{print $2/1024/1024 " GiB"}' /proc/meminfo)"
    echo "NUMA nodes: ${NUMA_NODE_COUNT}"
    echo "Variant:    ${VARIANT}"
    echo "wrk:        -t ${THREADS} -c ${CONNS} -d ${DURATION}s payload=${PAYLOAD}"
    echo
    echo "## wrk"
    grep -E '(Requests/sec|Transfer/sec|Latency Distribution|50%|90%|99%|99\.999%|Socket errors)' "$RUN_DIR/wrk.txt" || true
    echo
    echo "## CPU breakdown (avg across run)"
    echo "  %usr=${pct_usr}  %sys=${pct_sys}  %soft=${pct_soft}"
    echo
    echo "## syscalls (top of the response path)"
    printf "  total=%-12s  read=%s  write=%s  sendfile=%s  setsockopt=%s  epoll_ctl=%s  io_uring_enter=%s\n" \
        "$sys_total" "$(getsyscall read)" "$sys_write" "$sys_sf_total" \
        "$sys_setsockopt" "$sys_epoll_ctl" "$sys_io_uring_enter"
    printf "  write+sendfile share = %s%%   response-path (incl. cork + epoll_ctl) = %s%%\n" \
        "$hot_share" "$response_share"
    echo
    echo "## top syscalls"
    grep -E '^@syscalls' "$RUN_DIR/syscount.txt" \
        | sort -t']' -k2 -rn | head -15 || true
    echo
    echo "## Option B decision (post cork-off)"
    awk -v sys="$pct_sys" -v soft="$pct_soft" -v share="$hot_share" \
        -v resp="$response_share" -v cork="$sys_setsockopt" -v sf="$sys_sf_total" \
        -v epctl="$sys_epoll_ctl" -v epwait="$sys_epoll_wait" -v rd="$sys_read" \
        -v t="$sys_total" 'BEGIN{
        k = sys + soft
        printf "  kernel CPU = %.1f%% (sys+soft)\n", k
        printf "  write+sendfile share        = %.1f%%\n", share
        printf "  response-path share         = %.1f%%   (write+sendfile+cork+epoll_ctl)\n", resp
        if (sf > 0) printf "  setsockopt:sendfile ratio   = %.2f  (>=1.5 = cork still on)\n", cork/sf
        if (t > 0 && sf > 0) {
            printf "  epoll_ctl:sendfile          = %.2f\n", epctl/sf
            printf "  read:sendfile               = %.2f   (HTTP parse on bufio)\n", rd/sf
        }
        print ""
        if (cork > sf * 1.5) {
            print "  -> STOP: cork is still on.  Re-profile with PULSYS_VARIANT=saturate-no-cork."
            print "     Bench showed only ~1%% RPS gain from cork-off at 1.1M req/s; profile"
            print "     must use no-cork before judging Option B."
        } else if (k < 15) {
            print "  -> SKIP Option B: kernel CPU < 15%%.  Optimize user-space hot path."
        } else if (k >= 25 && epctl >= sf * 1.2 && resp >= 30) {
            print "  -> GO Option B (raw io_uring + SQPOLL, bypass Go netpoller)."
            print "     Evidence: cork off but epoll_ctl ~= 2x sendfile + high kernel CPU."
            print "     Flame should show runtime.lock2 / procyield / stealWork."
            print "     Library io_uring (Option A) only trims write+sendfile; netpoller"
            print "     is the ceiling — expect low single-digit %% from Option A alone."
        } else if (k >= 20 && share >= 20) {
            print "  -> MAYBE Option A first (library linked write+splice), then re-profile."
            print "     If RPS flat, escalate to Option B."
        } else {
            print "  -> Bottleneck is outside write/sendfile/epoll response path."
            print "     Read flame.svg + syscall histogram before committing to Option B."
        }
    }'
    echo
    echo "## NUMA topology"
    head -n 20 "$RUN_DIR/numa-hardware.txt" || true
    echo
    echo "## NUMA page locality (pulsys pid=$HFPID, after bench)"
    # numastat -p prints something like:
    #   Per-node process memory usage (in MBs) for PID 1234 (pulsys)
    #                            Node 0          Node 1           Total
    # Pull the meaningful rows so we can see if pulsys's RSS lives on
    # one node or got smeared across both.  A clean shard shows ~100%
    # on the local node; a smear is the "NUMA trap" symptom.
    grep -E '^(Heap|Stack|Private|Total|Huge)' "$RUN_DIR/numastat-pid-after.txt" 2>/dev/null || \
        cat "$RUN_DIR/numastat-pid-after.txt" 2>/dev/null || \
        echo "(numastat unavailable)"
} > "$RUN_DIR/SUMMARY.md"

# ----- tarball ------------------------------------------------------------
cd "$RUN_ROOT"
TARBALL="$(basename "$RUN_DIR").tar.gz"
tar -czf "$TARBALL" "$(basename "$RUN_DIR")"
chmod 0644 "$TARBALL"

# Emit the absolute path on its own line so the SSM document can grep it
# and feed it to `aws s3 cp`.
echo
echo "PROFILE_ARTIFACT=$RUN_ROOT/$TARBALL"

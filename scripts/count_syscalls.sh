#!/usr/bin/env bash
# count_syscalls.sh -- syscalls per HTTP response, by syscall, for any server.
#
# Counts every syscall entry made by the server process(es) while wrk drives
# load, then divides by the number of requests wrk completed.  The result is
# "syscalls per response" broken down by syscall name, e.g.
#
#   io_uring_enter   0.21 /req
#   sendfile64       1.00 /req
#
# How it counts (most accurate first):
#
#   linux  bpftrace on the raw_syscalls/syscalls tracepoints (default).
#          Exact counts, in-kernel aggregation, negligible overhead, so the
#          server runs at full speed while it is measured.  Needs root.
#   linux  strace -c -f (MODE=strace).  Also exact counts, but ptrace stops
#          the process on every syscall: throughput drops by an order of
#          magnitude and the Go scheduler behaves differently (more futex
#          and epoll traffic) while it is attached.  Fine for a quick look,
#          not for numbers you publish.
#   darwin dtrace syscall provider (MODE=dtrace, macOS).  Exact counts, low
#          overhead, but requires SIP to allow dtrace (csrutil enable
#          --without dtrace) and root.  dtruss is a wrapper around the same
#          provider with per-call printing, so it is slower.
#
# Usage:
#   sudo scripts/count_syscalls.sh <process-name> <url> [duration] [conns] [threads]
#
# Examples:
#   sudo scripts/count_syscalls.sh pulsys http://127.0.0.1:8080/<repo>/resolve/main/<file> 10s
#   sudo scripts/count_syscalls.sh nginx  http://127.0.0.1:18585/models/bench/bench/resolve/main/4k.bin
#   MODE=strace sudo -E scripts/count_syscalls.sh pulsys http://...
#
# <process-name> is matched exactly against the kernel comm (first 15 chars of
# the binary name), so every nginx worker or every Go thread is included.
set -euo pipefail

COMM="${1:?usage: $0 <process-name> <url> [duration] [conns] [threads]}"
URL="${2:?usage: $0 <process-name> <url> [duration] [conns] [threads]}"
DURATION="${3:-10s}"
CONNS="${4:-64}"
THREADS="${5:-2}"
MODE="${MODE:-}"
if [ -z "$MODE" ]; then
	case "$(uname -s)" in
	Darwin) MODE=dtrace ;;
	*) MODE=bpftrace ;;
	esac
fi

OUT="$(mktemp -d)"
trap 'rm -rf "$OUT"' EXIT
secs="${DURATION%s}"

pids="$(pgrep -x "$COMM" || true)"
if [ -z "$pids" ]; then
	echo "no process named '$COMM' is running" >&2
	exit 1
fi

case "$MODE" in
bpftrace)
	# Count by syscall name for every thread whose comm matches.  The probe
	# list is the per-syscall enter tracepoints, so names come out readable.
	bpftrace -q -e "
tracepoint:syscalls:sys_enter_* /comm == \"$COMM\"/ { @[probe] = count(); }
END { print(@); clear(@); }" >"$OUT/counts.raw" 2>"$OUT/bpftrace.err" &
	TRACER=$!
	sleep 2 # let bpftrace attach before load starts
	;;
strace)
	args=()
	for p in $pids; do args+=(-p "$p"); done
	strace -c -f -o "$OUT/counts.raw" "${args[@]}" &
	TRACER=$!
	sleep 1
	;;
dtrace)
	dtrace -q -n "
syscall:::entry /execname == \"$COMM\"/ { @[probefunc] = count(); }" -o "$OUT/counts.raw" &
	TRACER=$!
	sleep 2
	;;
*)
	echo "unknown MODE=$MODE (bpftrace|strace|dtrace)" >&2
	exit 1
	;;
esac

wrk -t "$THREADS" -c "$CONNS" -d "$DURATION" --latency "$URL" >"$OUT/wrk.txt"
kill -INT "$TRACER" 2>/dev/null || true
wait "$TRACER" 2>/dev/null || true

reqs="$(awk '/requests in/ {print $1}' "$OUT/wrk.txt")"
rps="$(awk '/Requests\/sec:/ {print $2}' "$OUT/wrk.txt")"

echo "process=$COMM mode=$MODE duration=$DURATION conns=$CONNS"
echo "requests=$reqs req/s=$rps"
echo

# Normalise the three tools' outputs to "name count" lines.
case "$MODE" in
bpftrace)
	sed -n 's/^@\[tracepoint:syscalls:sys_enter_\(.*\)\]: \([0-9]*\)$/\1 \2/p' "$OUT/counts.raw"
	;;
strace)
	# strace -c table: % time, seconds, usecs/call, calls, [errors], syscall
	awk '$1 ~ /^[0-9.]+$/ && NF >= 5 {print $NF, $4}' "$OUT/counts.raw"
	;;
dtrace)
	awk 'NF == 2 && $2 ~ /^[0-9]+$/ {print $1, $2}' "$OUT/counts.raw"
	;;
esac | sort -k2 -nr | awk -v reqs="$reqs" '
	{ total += $2; printf "  %-20s %12d  %8.3f /req\n", $1, $2, $2 / reqs }
	END { printf "  %-20s %12d  %8.3f /req\n", "TOTAL", total, total / reqs }'

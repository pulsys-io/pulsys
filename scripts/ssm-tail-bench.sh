#!/usr/bin/env bash
# ssm-tail-bench.sh -- watch a running hf_download_bench.sh on EC2.
#
# Polls /var/log/pulsys-real/ over SSM every few seconds and prints the
# tail of every *.out / *.err file plus current proxy stats.  Run this in
# a *second* terminal while scripts/ssm-hf-download.sh is blocked on
# ssm_wait in the first.
#
# Usage:
#   scripts/ssm-tail-bench.sh                # poll every 5s, default 200 lines
#   scripts/ssm-tail-bench.sh interval=10 lines=400
#   scripts/ssm-tail-bench.sh proc           # also show top + pidstat snapshot
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
source "$ROOT/scripts/lib/ssm-common.sh"

INTERVAL=5
LINES=200
SHOW_PROC=0
for arg in "$@"; do
	case "$arg" in
		interval=*) INTERVAL="${arg#interval=}" ;;
		lines=*)    LINES="${arg#lines=}" ;;
		proc)       SHOW_PROC=1 ;;
		*) echo "FATAL: unknown arg '$arg'" >&2; exit 2 ;;
	esac
done

INSTANCE="$(stack_output InstanceIdOut)"
[ -n "$INSTANCE" ] || { echo "FATAL: no InstanceIdOut" >&2; exit 1; }

# Heredoc with the snapshot command we run on every tick.  Kept small so the
# SSM command finishes within a few seconds.
build_cmd() {
	cat <<SH
set -uo pipefail
echo "===== \$(date -u +%FT%TZ) ====="
echo "-- df /mnt /mnt/hf-data --"
df -h /mnt /mnt/hf-data 2>/dev/null | head -5
if mountpoint -q /mnt/hf-data 2>/dev/null; then
	echo "-- du /mnt/hf-data --"
	sudo du -sh /mnt/hf-data/* 2>/dev/null | head -20
else
	echo "-- du /mnt --"
	sudo du -sh /mnt/hf-client-* /mnt/pulsys-cache-real 2>/dev/null | head -20
fi
echo "-- running clients --"
pgrep -af 'huggingface_hub.cli|hf_transfer|aria2|curl.*resolve/' | head -20 || true
echo "-- proxy expvar --"
curl -s --max-time 2 http://127.0.0.1:18099/debug/vars 2>/dev/null \
	| python3 -c 'import json,sys
try:
	d = json.load(sys.stdin)
except Exception as e:
	print(f"  (no metrics: {e})"); sys.exit(0)
keys = [k for k in d if k.startswith("pulsys_")]
for k in sorted(keys):
	print(f"  {k} = {d[k]}")' 2>/dev/null || echo "  (proxy not up yet)"
if [ "${SHOW_PROC}" = "1" ]; then
	echo "-- top (1s) --"
	top -b -n 1 -d 1 -o %CPU | head -25
fi
echo "-- log files in /var/log/pulsys-real --"
sudo ls -la /var/log/pulsys-real 2>/dev/null | head -30
for f in /var/log/pulsys-real/*.out /var/log/pulsys-real/*.err; do
	[ -f "\$f" ] || continue
	sz=\$(stat -c %s "\$f" 2>/dev/null || echo 0)
	echo
	echo "-- tail -n $LINES \$f (size=\${sz}B) --"
	sudo tail -n $LINES "\$f" 2>/dev/null || true
done
SH
}

echo "==> tailing $INSTANCE every ${INTERVAL}s (ctrl-c to stop)" >&2
while true; do
	clear || true
	PARAMS="$(jq -nc --arg c "$(build_cmd)" '{commands: [$c], executionTimeout: ["60"]}')"
	CMD_ID="$(aws ssm send-command \
		--region "$AWS_REGION" \
		--document-name AWS-RunShellScript \
		--instance-ids "$INSTANCE" \
		--parameters "$PARAMS" \
		--query 'Command.CommandId' --output text)"
	# Wait briefly for the snapshot to finish.
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		STATUS="$(aws ssm get-command-invocation \
			--region "$AWS_REGION" \
			--command-id "$CMD_ID" \
			--instance-id "$INSTANCE" \
			--query 'Status' --output text 2>/dev/null || echo InProgress)"
		case "$STATUS" in
			Success|Failed|Cancelled|TimedOut) break ;;
		esac
		sleep 1
	done
	aws ssm get-command-invocation \
		--region "$AWS_REGION" \
		--command-id "$CMD_ID" \
		--instance-id "$INSTANCE" \
		--query 'StandardOutputContent' --output text 2>/dev/null \
		| sed 's/	/\t/g'
	sleep "$INTERVAL"
done

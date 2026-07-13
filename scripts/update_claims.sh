#!/usr/bin/env bash
# update_claims.sh -- inject measured benchmark numbers into the docs.
#
# Single source of truth: docs/results/ec2/headline.json, written by
# scripts/bench_saturate.sh on every EC2 run.  Each target doc carries a marked
# region:
#
#     <!-- bench:headline:start -->
#     ...generated text...
#     <!-- bench:headline:end -->
#
# This script regenerates that region from headline.json, so re-running the
# benchmark on a different instance size refreshes the prose automatically
# instead of needing a hand-edit.  It is idempotent and safe to run repeatedly.
#
# Usage:
#   scripts/update_claims.sh [path/to/headline.json]   # default: docs/results/ec2/headline.json
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
HJSON="${1:-$ROOT/docs/results/ec2/headline.json}"

command -v jq >/dev/null 2>&1 || { echo "FATAL: jq is required" >&2; exit 1; }
if [ ! -f "$HJSON" ]; then
	echo "FATAL: headline json not found: $HJSON" >&2
	echo "  Run scripts/ssm-bench.sh (EC2) or bench_saturate.sh (local Linux) first." >&2
	exit 1
fi

instance="$(jq -r '.instance_type // "unknown"' "$HJSON")"
vcpu="$(jq -r '.vcpu // 0' "$HJSON")"
kernel="$(jq -r '.kernel // "unknown"' "$HJSON")"
rps="$(jq -r '.peak_rps // 0' "$HJSON")"
rps_pay="$(jq -r '.peak_rps_payload // ""' "$HJSON")"
gbs="$(jq -r '.peak_gb_per_s // 0' "$HJSON")"
gbps_pay="$(jq -r '.peak_gbps_payload // ""' "$HJSON")"

# ---- formatting helpers ---------------------------------------------------
# Payload label (4k|256k|4m|16m) -> human (4 KiB|256 KiB|4 MiB|16 MiB).
humanize_payload() {
	local p="$1" n unit
	case "$p" in
		*[kK]) n="${p%[kK]}"; unit="KiB" ;;
		*[mM]) n="${p%[mM]}"; unit="MiB" ;;
		*[gG]) n="${p%[gG]}"; unit="GiB" ;;
		*) printf '%s' "$p"; return ;;
	esac
	printf '%s %s' "$n" "$unit"
}

# req/s -> "1.25M req/s" when >= 1e6, else "123,456 req/s".
humanize_rps() {
	awk -v n="$1" 'BEGIN {
		n += 0
		if (n >= 1000000) { printf "%.2fM req/s", n / 1000000; exit }
		s = sprintf("%d", n); out = ""
		while (length(s) > 3) { out = "," substr(s, length(s)-2) out; s = substr(s, 1, length(s)-3) }
		printf "%s%s req/s", s, out
	}'
}

# GB/s -> whole number when >= 10, else one decimal.
humanize_gbs() {
	awk -v g="$1" 'BEGIN { g += 0; if (g >= 10) printf "%.0f", g; else printf "%.1f", g }'
}

rps_h="$(humanize_rps "$rps")"
rps_pay_h="$(humanize_payload "$rps_pay")"
gbs_h="$(humanize_gbs "$gbs")"
gbps_pay_h="$(humanize_payload "$gbps_pay")"

# ---- per-file rendered blocks ---------------------------------------------
readme_block() {
	cat <<EOF
On a ${vcpu}-vCPU \`${instance}\` (io_uring) it sustains **${rps_h}** at ${rps_pay_h} and
**${gbs_h} GB/s** loopback at ${gbps_pay_h}.
EOF
}

benchmarks_block() {
	cat <<EOF
Full-machine loopback (io_uring) on the committed reference run
(\`${instance}\`, ${vcpu} vCPU, Linux ${kernel}):
**${rps_h}** at ${rps_pay_h}, **${gbs_h} GB/s** at ${gbps_pay_h}.
EOF
}

internals_block() {
	cat <<EOF
On a ${vcpu}-vCPU \`${instance}\` it hits **${rps_h} @ ${rps_pay_h}**; larger
payloads are memory-bandwidth bound (~${gbs_h} GB/s loopback at ${gbps_pay_h}) and
tie the cork/sendfile path.
EOF
}

# ---- marker injection ------------------------------------------------------
inject() {
	local file="$1" repltext="$2"
	if [ ! -f "$file" ]; then
		echo "WARNING: $file not found; skipping" >&2
		return 0
	fi
	if ! grep -q '<!-- bench:headline:start -->' "$file"; then
		echo "WARNING: $file has no bench:headline markers; skipping" >&2
		return 0
	fi
	local repl tmp
	repl="$(mktemp)"
	tmp="$(mktemp)"
	printf '%s' "$repltext" >"$repl"
	awk -v rf="$repl" '
		BEGIN { while ((getline line < rf) > 0) blk = blk line "\n" }
		/<!-- bench:headline:start -->/ { print; printf "%s", blk; skip = 1; next }
		/<!-- bench:headline:end -->/   { skip = 0; print; next }
		skip { next }
		{ print }
	' "$file" >"$tmp"
	mv "$tmp" "$file"
	rm -f "$repl"
	echo "==> updated $(basename "$file")"
}

inject "$ROOT/README.md"            "$(readme_block)"
inject "$ROOT/docs/benchmarks.md"   "$(benchmarks_block)"
inject "$ROOT/docs/internals.md"    "$(internals_block)"

echo "==> claims refreshed from $HJSON (${instance}, ${vcpu} vCPU): ${rps_h}, ${gbs_h} GB/s"

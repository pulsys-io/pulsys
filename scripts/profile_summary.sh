#!/usr/bin/env bash
# profile_summary.sh -- extract the io_uring decision from a profile artifact.
#
# Inputs (auto-detects the latest tarball if none given):
#   scripts/profile_summary.sh                              # latest in tmp/bench/profile/
#   scripts/profile_summary.sh tmp/bench/profile/<tarball>  # specific run
#
# Prints (and writes to docs/results/ec2/profile-<tag>.md):
#   - variant / kernel / NUMA
#   - wrk Requests/sec, Transfer/sec, p50/p99
#   - mpstat %usr / %sys / %soft averaged across the run
#   - top 10 syscalls by count
#   - decision rule: skip io_uring, library route, or raw SQPOLL
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

TARBALL="${1:-}"
if [ -z "$TARBALL" ]; then
	# Newest by UTC timestamp embedded in the filename (20260516T210423Z-….tar.gz),
	# not filesystem mtime (S3 re-download touches old files).
	best=""
	best_ts=""
	for f in tmp/bench/profile/*.tar.gz; do
		[ -f "$f" ] || continue
		base="$(basename "$f" .tar.gz)"
		ts="${base%%-*}"
		if [ -z "$best_ts" ] || [[ "$ts" > "$best_ts" ]]; then
			best="$f"
			best_ts="$ts"
		fi
	done
	TARBALL="$best"
	[ -n "$TARBALL" ] || { echo "FATAL: no profile tarball — run scripts/ssm-profile.sh" >&2; exit 1; }
fi
echo "==> $TARBALL"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
tar -xzf "$TARBALL" -C "$WORK"
RUN_DIR="$(find "$WORK" -mindepth 1 -maxdepth 1 -type d | head -1)"
[ -n "$RUN_DIR" ] || { echo "FATAL: tarball is empty" >&2; exit 1; }

if [ -f "$RUN_DIR/SUMMARY.md" ]; then
	cat "$RUN_DIR/SUMMARY.md"
	mkdir -p docs/results/ec2
	TAG="$(basename "$TARBALL" .tar.gz)"
	cp "$RUN_DIR/SUMMARY.md" "docs/results/ec2/profile-${TAG}.md"
	echo
	echo "==> wrote docs/results/ec2/profile-${TAG}.md"
	[ -f "$RUN_DIR/flame.svg" ] && cp "$RUN_DIR/flame.svg" "docs/results/ec2/flame-${TAG}.svg" && \
		echo "==> wrote docs/results/ec2/flame-${TAG}.svg"
else
	echo "FATAL: tarball has no SUMMARY.md (was this built before profile_baseline.sh update?)" >&2
	exit 1
fi

#!/usr/bin/env bash
# render_hero_cast.sh -- write the landing-page asciinema cast from a measured
# warm hf + hf_transfer download row (loopback, after cache warm).
#
# Usage:
#   scripts/render_hero_cast.sh tmp/bench/ec2/hf-download/results.csv \
#       website/public/demos/hf-warm-demo.cast \
#       Qwen/Qwen2.5-7B-Instruct
#
# The cast timeline is a short terminal demo. The printed MiB/s / GB/s are the
# warm-row numbers from the CSV (not invented). Provenance sidecar (gitignored
# under tmp/): tmp/bench/ec2/hf-download/cast.meta.json
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

CSV="${1:?csv path}"
OUT="${2:?output .cast path}"
MODEL="${3:-Qwen/Qwen2.5-7B-Instruct}"

if [ ! -f "$CSV" ]; then
	echo "FATAL: missing $CSV" >&2
	exit 1
fi

# phase,wall_s,bytes,MiB_s,Gb_s
WARM_LINE="$(awk -F, '$1=="warm" {print; exit}' "$CSV")"
if [ -z "$WARM_LINE" ]; then
	echo "FATAL: no warm row in $CSV" >&2
	cat "$CSV" >&2 || true
	exit 1
fi

WALL="$(printf '%s' "$WARM_LINE" | cut -d, -f2)"
BYTES="$(printf '%s' "$WARM_LINE" | cut -d, -f3)"
MIB="$(printf '%s' "$WARM_LINE" | cut -d, -f4)"
GB="$(printf '%s' "$WARM_LINE" | cut -d, -f5)"

mkdir -p "$(dirname "$OUT")"
python3 - "$OUT" "$MODEL" "$WALL" "$BYTES" "$MIB" "$GB" <<'PY'
import json, sys, time
out, model, wall, bypasses, mib, gb = sys.argv[1:7]
width, height = 52, 8
header = {
    "version": 2,
    "width": width,
    "height": height,
    "timestamp": int(time.time()),
    "idle_time_limit": 1.5,
    "env": {"SHELL": "/bin/zsh", "TERM": "xterm-256color"},
}
lines = [
    (0.5, f"$ export HF_ENDPOINT=http://127.0.0.1:18080\r\n"),
    (1.0, f"$ export HF_TOKEN=pulsys_...\r\n"),
    (1.4, f"$ export HF_HUB_ENABLE_HF_TRANSFER=1\r\n"),
    (1.9, f"$ hf download {model}\r\n"),
    (2.4, "Resolving data files...\r\n"),
]
# Short progress strip ending on the measured warm rate.
pcts = [0, 12, 28, 45, 63, 81, 100]
t = 2.8
for p in pcts:
    filled = int(24 * p / 100)
    bar = "█" * filled + "░" * (24 - filled)
    lines.append((t, f"\r{bar}  {p:3d}%\x1b[K"))
    t += 0.22
lines.append((t, f"\r\nDownload complete (warm)  {mib} MiB/s  ({gb} Gb/s)\r\n"))
lines.append((t + 8.0, ""))

with open(out, "w", encoding="utf-8") as f:
    f.write(json.dumps(header, separators=(",", ":")) + "\n")
    for ts, text in lines:
        f.write(json.dumps([ts, "o", text], ensure_ascii=False) + "\n")
print(f"wrote {out}  warm={mib} MiB/s  {gb} Gb/s  wall={wall}s  bytes={bypasses}")
PY

META_DIR="$(dirname "$CSV")"
mkdir -p "$META_DIR"
INSTANCE_TYPE="$(curl -fsS -m 1 http://169.254.169.254/latest/meta-data/instance-type 2>/dev/null || echo unknown-local)"
# Prefer headline.json instance type when rendering on a laptop after SSM pull.
if [ -f "$ROOT/docs/results/ec2/headline.json" ]; then
	INSTANCE_TYPE="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["instance_type"])' "$ROOT/docs/results/ec2/headline.json" 2>/dev/null || echo "$INSTANCE_TYPE")"
fi
python3 - "$META_DIR/cast.meta.json" "$MODEL" "$WARM_LINE" "$INSTANCE_TYPE" <<'PY'
import json, sys, time
path, model, warm, itype = sys.argv[1:5]
phase, wall, bypasses, mib, gb = warm.split(",", 4)
json.dump({
    "generated_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    "model": model,
    "phase": phase,
    "wall_s": wall,
    "bytes": bypasses,
    "MiB_s": mib,
    "Gb_s": gb,
    "instance_type": itype,
    "client": "python3 -m huggingface_hub.cli.hf download + HF_HUB_ENABLE_HF_TRANSFER=1",
    "path": "HF_ENDPOINT=http://127.0.0.1:18080 after cold fill (warm loopback)",
}, open(path, "w"), indent=2)
print(f"wrote {path}")
PY

# Keep static fallback in index.astro honest: rate text is in the cast body.
echo "==> cast ready: $OUT"

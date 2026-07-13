#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TIER="smoke"
PROXY="${PROXY:-http://127.0.0.1:8080}"
ADMIN="${ADMIN:-http://127.0.0.1:6060}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tier) TIER="$2"; shift 2 ;;
    --proxy) PROXY="$2"; shift 2 ;;
    --admin) ADMIN="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

export HF_ENDPOINT="$PROXY"
export HF_HUB_DISABLE_TELEMETRY="${HF_HUB_DISABLE_TELEMETRY:-1}"

command -v hf >/dev/null 2>&1 || { echo "hf not on PATH" >&2; exit 1; }

artifact_line() {
  curl -fsS "${ADMIN}/debug/vars" 2>/dev/null | grep 'pulsys_artifact_upstream_bytes' | head -1 || true
}

echo "=== expvar (artifact upstream bytes) ==="
artifact_line || true

ROOT="$ROOT" TIER="$TIER" python3 <<'PY'
import json, os, subprocess, sys
from pathlib import Path
root = Path(os.environ["ROOT"])
tier = os.environ["TIER"]
data = json.loads((root / "scripts" / "e2e_models.json").read_text())
for job in data.get("jobs", []):
    jt = job.get("tier", "smoke")
    if tier != "all" and jt != tier:
        continue
    rid = job["id"]
    repo = (job.get("repo") or "").strip()
    if not repo:
        print(f"skip {rid}: empty repo")
        continue
    inc = job.get("include")
    out = root / ".e2e_out" / rid
    out.mkdir(parents=True, exist_ok=True)
    cmd = ["hf", "download", repo, "--local-dir", str(out)]
    if inc:
        cmd.extend(["--include", str(inc)])
    print("RUN", " ".join(cmd))
    subprocess.check_call(cmd)
print("done tier", tier)
PY

echo "=== expvar after run ==="
artifact_line || true

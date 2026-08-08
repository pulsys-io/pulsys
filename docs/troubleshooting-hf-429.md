# Fix Hugging Face "429 Client Error: Too Many Requests"

A `429` from the Hugging Face Hub means the Hub is rate-limiting the requests
coming from your IP address or token. It is not a permission error and it does
not mean you must buy Pro. This page shows the exact errors, explains what
triggers them, and lists the fixes in the order most likely to help.

## The error

Depending on the client, the same rate limit surfaces as one of these:

```text
huggingface_hub.errors.HfHubHTTPError: 429 Client Error: Too Many Requests
for url: https://huggingface.co/Qwen/Qwen3-32B/resolve/main/config.json
(Request ID: Root=1-...)
```

```text
requests.exceptions.HTTPError: 429 Client Error: Too Many Requests
for url: https://huggingface.co/api/whoami-v2
```

```text
WARNING:huggingface_hub.utils._http: HTTP Error 429 thrown while requesting
HEAD https://huggingface.co/<repo>/resolve/main/config.json
```

```text
HfHubHTTPError: 429 Too Many Requests for url: https://huggingface.co/api/models/<repo>.
Retry after 55 seconds (0/2500 requests remaining in current 300s window).
```

```text
Error: pull model manifest: 429: {"error":"We had to rate limit your IP.
To continue using our service, create a HF account or login to your existing
account, and make sure you pass a HF_TOKEN if you're using the API."}
```

The last one is `ollama pull hf.co/...`; the rest are `huggingface_hub`,
`datasets`, or `transformers`.

## Why it happens

The Hub applies rate limits per IP address and per token, in short rolling
windows (a few minutes), separately for each action type. File downloads and
their metadata checks fall in the "resolver" bucket: every `/resolve/...` GET
and every `HEAD` for a file counts against it.

You hit the limit when request volume from one source spikes. The common
causes are:

- **Many parallel workers.** A high `max_workers` on `snapshot_download`, or a
  training job with many data-loader processes, fans out hundreds of
  `/resolve/...` requests at once.
- **Many machines behind one egress IP.** A CI matrix, a Ray or Slurm cluster,
  or a Kubernetes node pool that all pull the same repo share one NAT IP, so
  the Hub sees a single high-volume client.
- **Uncached, repeated pulls.** Re-downloading the same model on every job or
  every container start multiplies the request count with no benefit.

Upgrading to Pro raises the ceiling but does not remove it, so an aggressive
client can still exceed a Pro account's resolver limit.

## Fixes

Work down this list; the first two resolve most cases.

### 1. Send a token

Authenticated requests get a higher quota than anonymous ones. Set `HF_TOKEN`
so downloads count against your account instead of a shared anonymous pool.

```bash
export HF_TOKEN=hf_...
```

For `ollama`, log in or pass a token as the linked error instructs; anonymous
pulls from a shared IP are throttled first.

### 2. Upgrade `huggingface_hub`

Version 1.2 and later read the `RateLimit` response header, wait exactly as
long as the header says, and retry automatically instead of failing. Older
`datasets` versions also sent an `expand=True` tree query that is heavily rate
limited; upgrading drops it.

```bash
pip install -U huggingface_hub datasets
```

### 3. Reduce parallelism

Lower the worker count so you stay under the resolver window.

```python
from huggingface_hub import snapshot_download

snapshot_download("Qwen/Qwen3-32B", max_workers=4)
```

### 4. Download once, then load offline

Fetch the snapshot a single time into a shared cache, then point every later
job at that cache so it makes no Hub requests at all.

```python
from huggingface_hub import snapshot_download

snapshot_download("Qwen/Qwen3-32B")            # once, fills HF_HOME
# later jobs:
from transformers import AutoModel
AutoModel.from_pretrained("Qwen/Qwen3-32B", local_files_only=True)
```

Set `HF_HUB_OFFLINE=1` to force every client to use the local cache and never
contact the Hub, which is useful in benchmarks and repeated CI runs where the
files are already present.

## When the real cause is a fleet re-downloading the same models

The fixes above help a single machine. They do not help when the request
volume comes from **many clients pulling the same models through one IP** —
a CI fleet, a training cluster, or an autoscaling node pool. Each machine has
its own empty cache, so `HF_HUB_OFFLINE` and `local_files_only=True` do not
apply, and lowering per-job workers only trades throughput for a slower version
of the same problem.

[Pulsys](https://pulsys.io) is a pull-through cache for the Hub that removes the
repeated traffic. Point clients at it with `HF_ENDPOINT`: the first pull of a
model fetches from Hugging Face and fills a local disk cache, and every pull
after that is served from disk with no upstream request. One shared cache turns
N machines re-downloading a model into a single upstream download.

```bash
export HF_ENDPOINT=https://pulsys.your-network.internal
export HF_TOKEN=pulsys_...   # a Pulsys API key; Pulsys holds the upstream HF token
python -c "from huggingface_hub import snapshot_download; snapshot_download('Qwen/Qwen3-32B')"
```

This addresses only the repeat-download cause. A cold cache still fetches from
Hugging Face on the first pull, so the very first request for a model can still
be rate limited if it is issued with high concurrency; Pulsys retries `429` and
`5xx` responses with bounded exponential backoff. See the
[architecture](architecture.md) for the request flow and the
[benchmarks](benchmarks.md) for warm-hit throughput.

## Quick checklist

- Set `HF_TOKEN` so requests use your account quota.
- Upgrade `huggingface_hub` to 1.2 or later so `429`s wait and retry instead of
  failing.
- Cap `max_workers` and reuse one long-lived cache across jobs.
- Prefetch once, then load with `local_files_only=True` or `HF_HUB_OFFLINE=1`.
- If many machines pull the same models through one IP, put a shared
  pull-through cache in front of the Hub so repeats never leave your network.

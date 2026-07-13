# Benchmarks

Measured numbers, the artifacts behind them, and how to reproduce everything.

## Headline

<!-- bench:headline:start -->
Full-machine loopback (io_uring) on the committed reference run
(`c7i.12xlarge`, 48 vCPU, Linux 6.1.176-221.360.amzn2023.x86_64):
**1.36M req/s** at 4 KiB, **90 GB/s** at 16 MiB.
<!-- bench:headline:end -->

These numbers are not hand-authored: they are emitted by the CDK + SSM harness
below into [`results/ec2/`](results/ec2/) (`report.md`, `headline.json`, and the
rps / throughput / latency charts) and the sentence above is regenerated from
`headline.json` by [`scripts/update_claims.sh`](../scripts/update_claims.sh).
Re-running the harness on a different instance size refreshes both the committed
artifacts and the headline. The committed run used the default `c7i.12xlarge`
(48 vCPU); different instance types will produce different numbers.

Warm-hit vs [DingoSpeed](https://github.com/dingodb/dingospeed) (Darwin laptop;
per-cell tables under [`results/darwin/summary.md`](results/darwin/summary.md)):

| Metric | pulsys | DingoSpeed |
|---|---|---|
| Warm 64 KiB @ c=8 | **73,871 req/s** | 4,050 req/s |
| Warm 256 KiB @ c=1 | **13,128 req/s** | 737 req/s |
| Warm 16 MiB @ c=1 | **358 req/s** | 198 req/s |
| Peak RSS, 64 MiB sustained load | **12.1 MB** | 853.4 MB |
| Cumulative CPU, identical 20 s load | **20 s** | 161 s |
| Warm-throughput cells won (of 35) | **32, 3 tied** | 0 |
| Warm-p99 cells won (of 35) | **35** | 0 |

## Receipts

Warm-hit microbenchmark (`go test -bench -benchmem ./internal/coreserver`, Apple M2 Pro):

```text
goos: darwin
goarch: arm64
pkg: github.com/pulsys-io/pulsys/internal/coreserver
cpu: Apple M2 Pro
BenchmarkCoreServerWarm_256KiB-12   17472    69094 ns/op   3794.01 MB/s   0 upstream_bytes/op   0 upstream_fetches/op   256 B/op   1 allocs/op
BenchmarkCoreServerWarm_4MiB-12      1257   827881 ns/op   5066.31 MB/s   0 upstream_bytes/op   0 upstream_fetches/op   257 B/op   1 allocs/op
```

Server-side allocations are **0**. A `pprof -alloc_objects` capture over
146,738 served warm requests attributes every sampled allocation to the profiler
itself or background GC, and **none** to the warm hot path (the 1 alloc/op above
is the in-process test client):

```text
$ go tool pprof -alloc_objects -top -cum allocs.pb.gz   # 4s warm hammering, 146,738 reqs
Showing nodes accounting for 678, 100.00% of 678 total
      flat  flat%   sum%        cum   cum%
       396 55.93% 60.45%        396 55.93%  runtime/pprof.allFrames                       <- the profiler itself
       177 25.00% 85.45%        225 31.78%  runtime/pprof.(*profileBuilder).emitLocation
        13     -      -          13  1.84%   runtime.gcBgMarkWorker                        <- background GC
# 0 objects attributed to coreserver.serveConn / tryServeWarm / sendFileWithHeaderViaRaw
```

Exactly **one `sendfile(2)` per response**, from expvar counters (`/debug/vars`)
bracketing a 3 s warm run:

```text
# before load
pulsys_cache_hits           = 2
pulsys_sendfile_fused_calls = 254
# after wrk -t4 -c8 -d3s of 256 KiB warm hits
pulsys_cache_hits           = 93469      (+93,467 hits)
pulsys_sendfile_fused_calls = 93721      (+93,467 calls)   # 1.000000 sendfile/hit
```

## How the warm path is measured

The implementation notes for the measured 0-allocation warm path, Linux
io_uring/sendfile path, and macOS `sendfile` + `sf_hdtr` fusion are in
[`internals.md`](internals.md).

## Datasets

Exactly two result sets live under [`results/`](results/):

- **`results/darwin/`** — head-to-head vs DingoSpeed on a laptop:
  `rps.svg`/`.png` (req/s by payload, c=1/4/8), `throughput.svg` (GB/s),
  `latency.svg` (p99), `footprint.svg` (RSS + cumulative CPU over a 20 s warm
  load), `e2e.svg` (`hf download` wallclock), and `summary.md` (per-cell table
  with `x faster` columns). Committed.
- **`results/ec2/`** — full-machine Pulsys (single server, wrk at c = 4 × vCPU)
  on the committed reference instance (default `c7i.12xlarge`, 48 vCPU). Every
  file here is the direct output of the CDK + SSM harness, so anyone reading them
  knows they were generated, not hand-authored: `report.md` (utilization + peak
  Gbps), `headline.json` (the machine-readable headline), `rps.svg`/`throughput.svg`/
  `latency.svg`, and `matrix-saturate-iouring.csv`. Re-running the harness
  overwrites them in place.

## Reproduce

Pulsys's headline numbers depend on a **Linux io_uring** fast path (`-iouring`,
kernel ≥ 6.1), which falls back silently if it can't engage. So "I ran it" is not
the same as "I measured it" — always verify engagement (below).

> **macOS note.** io_uring is Linux-only. On macOS the warm path uses
> `sendfile(2)` + `sf_hdtr` (still fast, still benchmarked — `results/darwin/`),
> but to reproduce the io_uring numbers you need a Linux host or the EC2 harness.

### Local comparison (Darwin or Linux)

Warm-hit throughput vs DingoSpeed, Caddy, and nginx, same bytes/URLs:

```bash
scripts/bench_compare.sh            # writes tmp/bench/results.csv
go run scripts/render_bench_svg.go  # renders the comparison chart

# Regenerate the committed darwin/ charts:
scripts/bench_matrix.sh             # tmp/bench/darwin/matrix.csv
scripts/bench_footprint.sh          # tmp/bench/darwin/footprint.csv
scripts/bench_e2e.sh                # real `hf download` (hf + hf_transfer) wallclock
scripts/render_charts.sh darwin     # -> docs/results/darwin/
```

### Any Linux host (no AWS, no AMI)

Requires kernel ≥ 6.1, Go, and for the matrix `wrk` + `sysstat` (`mpstat`). Bare
metal often produces higher and less noisy numbers because there is no hypervisor
between the process and the hardware.

```bash
go build -o /tmp/pulsys ./cmd/pulsys
/tmp/pulsys -listen 127.0.0.1:8080 -public-base-url http://127.0.0.1:8080 \
  -cache-dir /tmp/pulsys-cache -listeners "$(nproc)" \
  -iouring -tcp-cork=false -admin-listen 127.0.0.1:18099 &

export HF_ENDPOINT=http://127.0.0.1:8080
hf download Qwen/Qwen2.5-0.5B >/dev/null   # first run fills the cache
hf download Qwen/Qwen2.5-0.5B >/dev/null   # second run is a warm hit

# VERIFY io_uring served the warm hits (must be > 0):
curl -s http://127.0.0.1:18099/debug/vars | grep io_uring_fused
#   "pulsys_io_uring_fused_calls": 42        <- engaged

# Full-machine saturation matrix (the EC2-style report):
SATURATE_VARIANT=saturate-iouring scripts/bench_saturate.sh 30s
#   tmp/bench/saturate-report.md  (records instance type, kernel, io_uring status)
#   tmp/bench/matrix.csv
```

If the counter stays `0`, io_uring did not engage (old kernel `uname -r` < 6.1,
or a blocked `io_uring_setup` syscall) and you measured the cork/sendfile
fallback. Other variants for A/B: `saturate-no-cork`, `saturate` (cork on).

### Docker (functional check, not for headline numbers)

io_uring is a host-kernel feature; a container uses the host/VM kernel and the
default seccomp profile often blocks the io_uring syscalls (425-427), which have
tightened over time (gVisor/`runsc` does not support io_uring at all). For a local
functional test on a 6.1+ host:

```bash
docker build -t pulsys -f docker/Dockerfile .
docker run --rm --name pulsys --security-opt seccomp=unconfined \
  -p 8080:8080 -v pulsys-cache:/var/cache/pulsys \
  pulsys -listen :8080 -public-base-url http://localhost:8080 \
  -cache-dir /var/cache/pulsys -listeners 4 -iouring -admin-listen 127.0.0.1:18099
```

`seccomp=unconfined` is a local-test convenience; in production allowlist only
425-427. Docker Desktop on macOS/Windows can engage io_uring functionally but its
throughput numbers are virtualized and not representative.

### EC2 (one command)

Produces `docs/results/ec2/` and regenerates the landing-page cast from a
**warm** `hf download` + `hf_transfer` over loopback after cache warm.

```bash
HF_TOKEN=hf_xxx scripts/run-aws-benchmarks.sh
# optional: --skip-ami  --teardown
# optional: INSTANCE_TYPE=c7i.4xlarge
```

That wraps: stock AMI (if missing) → CDK `PulsysBench` → set token → sync/rebuild
→ saturate-iouring → cold+warm HF download → `render_hero_cast.sh`. Details:
[`infra/cdk/README.md`](../infra/cdk/README.md). Tear down with
`scripts/ssm-teardown.sh` when finished.

Manual equivalent (same order):

```bash
scripts/build-stock-ami.sh
cd infra/cdk && npm install && \
  CDK_DEFAULT_ACCOUNT=$(aws sts get-caller-identity --query Account --output text) \
  CDK_DEFAULT_REGION=${AWS_REGION:-us-east-1} \
  npx cdk deploy -c amiKind=stock --require-approval never && cd -
HF_TOKEN=hf_xxx scripts/ssm-set-hf-token.sh
scripts/ssm-sync-scripts.sh full
scripts/ssm-bench.sh variant=saturate-iouring duration=30s
scripts/ssm-hf-download.sh model=Qwen/Qwen2.5-7B-Instruct skip_direct=1
scripts/render_hero_cast.sh \
  tmp/bench/ec2/hf-download/results.csv \
  website/public/demos/hf-warm-demo.cast \
  Qwen/Qwen2.5-7B-Instruct
scripts/ssm-teardown.sh
```

Defaults assume region `us-east-1` and stack `PulsysBench` (override with
`AWS_REGION` / `HF_STACK_NAME`). Do not commit account IDs, instance IDs, or
`infra/cdk/cdk.context.json`.

### Verifying io_uring engagement

| Surface | What to look for |
|---|---|
| `pulsys_io_uring_fused_calls` on `/debug/vars` (expvar) | warm responses sent via io_uring linked `WRITE`+`SPLICE`; **> 0 means engaged** |
| same key on `/metrics` (Prometheus) | same counter, for scraping |
| `-iouring` flag | requests io_uring; requires kernel ≥ 6.1, else transparent fallback to cork/sendfile |
| `bench_saturate.sh` report header | records instance type + kernel in the artifact |

Keep the admin/observability surface on loopback (the examples use
`127.0.0.1:18099`; the binary default is `127.0.0.1:6060`). See
[`security.md`](security.md#admin-and-pprof-listeners).

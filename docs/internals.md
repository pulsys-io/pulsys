# Internals

The software and syscall layer: how the warm-cache hot path is implemented, the
per-platform sendfile/io_uring mechanisms, the Xet protocol handling, and the OS
tuning behind the published numbers. This is the engineer's view. For the
system/operator view see [`architecture.md`](architecture.md); for the numbers
and how to reproduce them see [`benchmarks.md`](benchmarks.md).

## The goal

A warm cache hit should move a cached file from the OS page cache to the client
socket with minimal kernel transitions, no user-space copy of the body, and no
per-request heap allocation in the server hot path. Pulsys implements this on
Linux (io_uring, kernel 6.1+) and macOS (`sendfile` + `sf_hdtr`), with a portable
`TCP_CORK` fallback on older Linux.

## Warm-path measurements

The table below describes what the current implementation measures on the warm
path. It is not a universal ranking of every HTTP server or every possible
kernel/runtime implementation.

| Axis | Pulsys measured | Design target |
|------------|-----------------|-----------|
| Allocations per warm hit (server side) | **0** | 0 |
| User-space bytes copied per warm hit | header length only (~110 B) | header length only |
| Syscalls per warm hit (Darwin, body ≤ `SO_SNDBUF`) | **1** (`sendfile` + `sf_hdtr` fused) | 1 |
| Syscalls per warm hit (Linux io_uring) | **1** submission | 1 |
| Syscalls per warm hit (Linux cork fallback) | **2** (`write` + `sendfile`, one TCP segment via `TCP_CORK`) | 1 (only via io_uring) |
| Cache-key compute per warm hit | 0 (memoised in `KeyHexCache`) | 0 |
| Body fd lookup per warm hit | 0 (LRU-cached `*os.File`, ref-counted close) | 0 |

### Axis 1: allocations

A `pprof -alloc_objects` capture over 4 s of warm hammering (36,684 req/s,
146,738 requests, `PULSYS_MEMPROFILE_RATE=1`) attributes every sampled allocation
to the profiler itself or background GC, and **none** to `serveConn`,
`tryServeWarm`, or `sendFileWithHeaderViaRaw`. Per-request server-side allocation
rate is `0 / 146,738 ≈ 0`.

Every per-connection and per-request object is pooled: `bufio.Reader`/`Writer`,
header scratch buffer, the sendfile state struct (`*sfHdtrState`), the `*Request`
struct, the body `*os.File` handle (LRU), and the cache key (`KeyHexCache`). The
one non-obvious win: the netpoller's `(*RawConn).Write(func(uintptr) bool)`
callback used to heap-allocate a fresh `funcval` per request. Pre-binding the
method value once at pool-init removes it:

```go
var sfHdtrStatePool = sync.Pool{New: func() any {
    st := &sfHdtrState{}
    st.cb = st.write // bound once, reused forever
    return st
}}
```

This keeps the measured server-side warm path at zero sampled per-request
allocations. The benchmark and allocation profile are the artifact to trust here,
not a broad claim about what other runtimes or servers could implement.

### Axis 2: syscalls

`expvar` counters (`pulsys_cache_hits`, `pulsys_sendfile_fused_calls`,
`pulsys_sendfile_eagains`) bracketing a 256 KiB warm `wrk` run show
`fused / hits = 1.000000`: exactly one `sendfile(2)` per response on Darwin, with
`sf_hdtr` carrying the entire HTTP head plus body in one kernel transition.

Above `SO_SNDBUF`, the kernel drains what it can and returns `EAGAIN`; the
netpoller parks the goroutine and re-enters on drain. This is a kernel constraint,
not a Go one. With autotuned `SO_SNDBUF` on Linux/AWS, real HF payloads (10-16 MiB
chunks) take 1-2 syscalls per response. `sendfile(2)`/`splice(2)` is the only
primitive that copies an fd to a socket without a user-space round trip, and
HTTP/1.1 requires at least one transition for the head plus body.

### Axis 3: user-space byte copies

The only user-space bytes touched per warm hit are the HTTP response head,
rendered once into a pooled buffer and handed to the kernel via the `sf_hdtr`
iovec. Body bytes pass directly from the file's page cache to the socket buffer
inside the kernel. For a 256 MiB shard with a 110-byte head, user-space touches
110 bytes against 268,435,456 kernel-copied: in the noise.

### Reproducing the measurement

```bash
PULSYS_MEMPROFILE_RATE=1 ./pulsys ...
wrk -t4 -c8 -d5s http://localhost:8080/<repo>/resolve/main/<file> &
curl -s http://localhost:6060/debug/pprof/allocs?seconds=4 -o allocs.pb.gz
go tool pprof -alloc_objects -top -cum allocs.pb.gz
curl -s http://localhost:6060/debug/vars | jq '{hits:.pulsys_cache_hits, fused:.pulsys_sendfile_fused_calls, eagains:.pulsys_sendfile_eagains}'
go test -run='TestWarmHitAllocFloor' -v ./internal/coreserver   # CI keeps this honest
```

## Platform fast paths

| Platform | Warm-path mechanism | Syscalls/hit |
|----------|---------------------|--------------|
| macOS | `sendfile(2)` + `sf_hdtr` (header + body fused) | 1 |
| Linux 6.1+, `-iouring` | io_uring linked `WRITE` (header) + `SPLICE`/sendfile (body) | 1 (submission) |
| Linux fallback | `write(headers)` + `sendfile(body)`, coalesced into one TCP segment via `TCP_CORK` | 2 |

### macOS: `sendfile` + `sf_hdtr`

A naive macOS file response is two syscalls (`write(headers)` then
`sendfile(body)`). Darwin's `sendfile(2)` takes an `sf_hdtr` argument (header and
trailer iovecs), so Pulsys passes the response head as the header iovec and the
cached file as the body; the kernel ships both in one call. Implementation:
[`internal/coreserver/sendfile_darwin.go`](../internal/coreserver/sendfile_darwin.go).

Measured at 16 KiB (`wrk -t4 -c64`, loopback):

| Server | Warm 16 KiB |
|--------|-------------|
| pulsys (fused `sf_hdtr`) | 1.64 GB/s |
| pulsys (unfused, 2 syscalls) | 1.05 GB/s |
| Go `net/http` file server | 1.13 GB/s |
| Caddy 2.x | 0.90 GB/s |

In these measurements the fused path is faster from 16 KiB to ~10 MiB. Above
~16 MiB the body transfer dominates and the sendfile-based paths converge.

Correctness detail: when a header is supplied, Darwin's `len` is in/out and counts
header plus file bytes together. Pulsys includes the remaining-header length in
each call's byte budget; on `EAGAIN` it records header and body bytes queued and
resumes (draining the header fully before counting file bytes, per the contract).
`EINTR` retries; other errnos surface to the caller.

### Linux: io_uring reactor (and the cork fallback)

The Linux fast path (`-iouring`, kernel 6.1+ for `SINGLE_ISSUER` +
`DEFER_TASKRUN`) runs one io_uring ring per `SO_REUSEPORT` listener with a full
netpoller bypass: accept, recv, and send all on the ring, raw sendfile for the
body. Implementation:
[`internal/coreserver/iouring_reactor_linux.go`](../internal/coreserver/iouring_reactor_linux.go)
and the sibling `iouring_*_linux.go` files.
<!-- bench:headline:start -->
On a 48-vCPU `c7i.12xlarge` it hits **1.36M req/s @ 4 KiB**; larger
payloads are memory-bandwidth bound (~90 GB/s loopback at 16 MiB) and
tie the cork/sendfile path.
<!-- bench:headline:end -->
The committed reference run uses a stock `c7i.12xlarge`; see
[`benchmarks.md`](benchmarks.md) and `results/ec2/headline.json` for the exact
measured figures and the per-payload A/B deltas.

The portable fallback (no io_uring) brackets `write(headers)` + `sendfile(body)`
in `TCP_CORK` so the pair ships as a single TCP segment. The cork bracket adds two
`setsockopt` calls but uses package-level callbacks, so the per-request allocation
count stays 0. Implementation:
[`internal/coreserver/sendfile_with_header_other.go`](../internal/coreserver/sendfile_with_header_other.go);
toggled by `-tcp-cork`.

Tuning lessons from the io_uring work worth keeping: `SINGLE_ISSUER` requires all
submissions from one thread, the SQ-array indirection must be initialized
correctly, and `TCP_NODELAY` is required or small responses stall in Nagle. A
still-open optimization is `IORING_REGISTER_FILES` + `REGISTER_BUFFERS` to skip
per-SQE fd validation and per-send page pinning (expected +1-3% at 4 KiB).

### Per-core listeners and NUMA sharding

`SO_REUSEPORT` per-core listeners (`-listeners`) let the kernel balance accepted
connections across cores. On dual-socket bare metal (e.g. `c7i.metal-24xl`,
Sapphire Rapids), cross-socket DRAM crosses the UPI link (~50 GB/s) versus
same-socket (~300 GB/s); a sendfile that fans page-cache bytes to a socket buffer
on another node pays UPI tax. The `numa`/`saturate` bench variants run one
`pulsys` per NUMA node pinned with `numactl --cpunodebind=N --membind=N`, all
sharing the listen port via `SO_REUSEPORT`, so each connection's socket buffers
and page-cache reads stay node-local. `scripts/profile_baseline.sh` captures
`numactl --hardware`, `numastat -p`, and `hwloc-ls` so the UPI cost is measured,
not assumed.

## Hugging Face Xet (CAS) handling

Hugging Face can serve large weights via **Xet**, an HTTP-visible layer over
chunked content-addressed storage. `huggingface_hub` may advertise Xet wiring
through `Link` headers (`rel="xet-auth"`, `rel="xet-reconstruction-info"`); with
those present, a client can bypass a simple reverse proxy and fetch straight from
CAS hosts. Pulsys keeps everything on the cache path transparently:

1. **Strip only the Xet `Link` relations** from forwarded responses (other `rel`
   values are untouched), so the client falls back to the normal `Location`
   redirect.
2. **Rewrite `Location`** to the `/_p/<upstream-host>/<path>` routing form
   (`internal/rewrite.LocationToProxy`) so redirects stay on Pulsys.
3. **Stable cache keys** for content-addressed hosts (e.g.
   `cas-bridge.xethub.hf.co`): presigned URLs change their signed query string on
   every redirect, but the path encodes object identity, so keys ignore the query
   noise and cold/warm runs hit the same on-disk slot.

Every byte fetched this way (including the consolidated cas-bridge body) still
streams through the disk cache like any other artifact.

## OS / kernel tuning

Pulsys ships an evidence-driven tuning split rather than baking in every plausible
sysctl:

- **Category 1** (ceiling-lifting: `rmem_max`, `somaxconn`, `fs.file-max`) is
  uncontroversial and baked into the stock AMI
  ([`infra/packer/files/sysctl-pulsys-category1.conf`](../infra/packer/files/sysctl-pulsys-category1.conf)).
- **Category 2** (behavioural: BBR, TFO, no-slow-start-after-idle, THP=madvise,
  RPS/RFS, CPU governor, irqbalance, busy-poll) is baked **only** if it wins an
  A/B sweep. `scripts/sweep_tunings.sh` snapshots, applies, re-warms, runs `wrk`,
  and reverts each candidate. A tuning survives only if mean RPS delta is positive
  across every `(payload, concurrency)` cell and no cell regresses by more than
  -1%. Survivors are hand-promoted into
  [`infra/packer/files/sysctl-pulsys-category2.conf`](../infra/packer/files/sysctl-pulsys-category2.conf)
  with a provenance comment (`# sweep <date> commit <sha> delta +X% across …`).

Run the sweep via `scripts/ssm-sweep.sh` (see [`benchmarks.md`](benchmarks.md) for
the full EC2 harness).

## Comparison to other Go servers

This table summarizes the implementations and benchmark results tested in this
repository. Treat it as a snapshot of this harness, not a permanent claim about
upstream projects.

| Mechanism | `net/http` FileServer | fasthttp | DingoSpeed | **pulsys** |
|-----------|----------------------|----------|------------|------------|
| Header fused into sendfile (Darwin `sf_hdtr`) | no | no | no | **yes** |
| Header via `TCP_CORK` + sendfile (Linux) | no | no | no | **yes** |
| io_uring reactor (Linux 6.1+) | no | no | no | **yes** |
| `SO_REUSEPORT` per-core listeners | no | no | no | **yes** |
| Pre-bound `sync.Pool` callback (no per-req funcval) | no | partial | no | **yes** |
| Pooled `*os.File`, ref-counted lazy close | no | no | re-opens per req | **yes** |
| Memoised cache key | n/a | n/a | per request | **yes** |
| Allocations per warm hit | ~5 | 6 | 12+ | **0** |
| Syscalls per warm hit (256 KiB, Darwin) | 2 | 2 | 2-3 | **1** |

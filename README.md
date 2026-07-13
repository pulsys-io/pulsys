<div align="center">

<img src="website/public/logo.svg" alt="Pulsys logo" width="72" height="72" />

# Pulsys

An authenticated pull-through cache for Hugging Face with a sendfile/io_uring
warm path.

[![CI](https://github.com/pulsys-io/pulsys/actions/workflows/linux.yml/badge.svg)](https://github.com/pulsys-io/pulsys/actions/workflows/linux.yml)
[![Security](https://github.com/pulsys-io/pulsys/actions/workflows/security.yml/badge.svg)](https://github.com/pulsys-io/pulsys/actions/workflows/security.yml)
[![Helm](https://github.com/pulsys-io/pulsys/actions/workflows/helm.yml/badge.svg)](https://github.com/pulsys-io/pulsys/actions/workflows/helm.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/pulsys-io/pulsys)](https://goreportcard.com/report/github.com/pulsys-io/pulsys)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/pulsys-io/pulsys/badge)](https://securityscorecards.dev/viewer/?uri=github.com/pulsys-io/pulsys)
[![SLSA 3](https://slsa.dev/images/gh-badge-level3.svg)](https://slsa.dev)
[![Release](https://img.shields.io/github/v/release/pulsys-io/pulsys?sort=semver)](https://github.com/pulsys-io/pulsys/releases)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

</div>

---

Pull the same model twice (CI fleet, training cluster, air-gapped network) and
you re-download the same bytes. Pulsys caches the Hugging Face wire protocol to
local disk: the first request streams from `huggingface.co`; every request after
is served from disk with no upstream egress. Warm hits use io_uring/sendfile on
Linux 6.1+ and `sendfile` + `sf_hdtr` on macOS.

<!-- bench:headline:start -->
On a 48-vCPU `c7i.12xlarge` (io_uring) it sustains **1.42M req/s** at 4 KiB and
**99 GB/s** loopback at 16 MiB.
<!-- bench:headline:end -->
Numbers, receipts, and reproduction (on a stock `c7i.12xlarge` by default):
[`docs/benchmarks.md`](docs/benchmarks.md).

Pulsys is authenticated by default (no open mode): it requires a Postgres admin
plane that issues per-request API keys, and its own read-only Hugging Face token
for upstream reads. It never forwards client credentials to Hugging Face.
Details: [`docs/security.md`](docs/security.md#credential-model).

## Clone

```bash
git clone --recurse-submodules https://github.com/pulsys-io/pulsys.git
cd pulsys
```

## Run locally

```bash
export PULSYS_HF_TOKEN=hf_your_readonly_token   # required: Pulsys reads HF with this
docker compose up --build
```

Brings up the proxy, admin console + SSO, a Keycloak dev IdP, and Postgres.

- Admin console: http://localhost:3000 (sign in `admin@pulsys.local` / `admin`)
- Proxy: http://localhost:8082 — walkthrough: [`DEVELOPMENT.md`](DEVELOPMENT.md#local-development)

Create a Pulsys API key in the admin UI, then point any HF client at the proxy:

```bash
export HF_ENDPOINT=http://localhost:8082
export HF_TOKEN=pulsys_...           # your Pulsys API key (proxy returns 401 without one)
hf download Qwen/Qwen2.5-0.5B        # first run fills the cache; next run is served from disk
```

`huggingface_hub`, `transformers`, `datasets`, the `hf` CLI, and `hf_transfer`
all work unchanged via `HF_ENDPOINT`. Config reference:
[`internal/config/config.go`](internal/config/config.go) (every flag has a
`PULSYS_`-prefixed env var).

## Deploy (Kubernetes)

```bash
helm install pulsys oci://ghcr.io/pulsys-io/charts/pulsys \
  --set proxy.publicBaseURL=https://hf.example.com \
  --set admin.enabled=true \
  --set postgres.host=postgres.db.svc \
  --set admin.imports.hfTokenSecret=pulsys-hf \
  --set persistence.size=200Gi
```

Or install from this repo:

```bash
helm install pulsys deploy/charts/pulsys \
  -f deploy/charts/pulsys/ci/admin-values.yaml
```

- Chart (values, JSON-schema-validated, kind-tested in CI): [`deploy/charts/pulsys/`](deploy/charts/pulsys/)
- Production SSO (Keycloak / Cognito / IAM Identity Center): [`docs/oidc.md`](docs/oidc.md)
- Production topology + hardening: [`docs/security.md`](docs/security.md#deployment-security-model)

## Project status & scope

Pulsys is production-targeted as a **single-node** pull-through cache: one proxy
instance on persistent disk, backed by a (separately HA-able) Postgres admin
plane. A node restart is a brief blip rather than data loss — the cache is
rebuildable from Hugging Face and admin/job state lives in Postgres.

| Capability | Status |
|---|---|
| Single-node proxy + warm cache | Supported |
| Authenticated admin plane (Postgres) | Supported |
| Kubernetes deploy (single replica) | Supported |
| Multi-node / high-availability **clustering** | **Not supported — [roadmap (v2)](ROADMAP.md)** |

> [!IMPORTANT]
> Run a **single** proxy replica (with health checks, auto-restart, and a
> persistent volume); make the Postgres admin plane HA separately. Running more
> than one replica is **not currently supported** — in-flight de-duplication and
> other coordination are per-process and not shared across nodes. Horizontal
> scale-out / HA clustering is a v2 item: [`ROADMAP.md`](ROADMAP.md).

## Test & benchmark

```bash
go test -race ./...                  # full suite
scripts/bench_compare.sh             # local warm-hit comparison vs DingoSpeed / Caddy / nginx
```

EC2 io_uring saturation (CDK + SSM), methodology, and io_uring verification:
[`docs/benchmarks.md`](docs/benchmarks.md). The default reference run uses
`c7i.12xlarge`; bare metal is an optional override.

## Security

Pulsys ships a custom HTTP/1.1 server with differential tests against Go's
standard library behavior, fuzzing, and public request-smuggling corpora on
every commit. Rationale, CVE remediation, and test provenance:
[`docs/security.md`](docs/security.md). To report a
vulnerability, see [SECURITY.md](SECURITY.md).

## Documentation

Full index: [`docs/`](docs/README.md). Common entry points:

| Topic | Doc |
|---|---|
| Build, test, code map | [`DEVELOPMENT.md`](DEVELOPMENT.md) |
| Benchmarks | [`docs/benchmarks.md`](docs/benchmarks.md) |
| Architecture | [`docs/architecture.md`](docs/architecture.md) |
| Security & threat model | [`docs/security.md`](docs/security.md) |
| Helm chart | [`deploy/charts/pulsys/`](deploy/charts/pulsys/) |
| Roadmap | [`ROADMAP.md`](ROADMAP.md) |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and the [Code of Conduct](CODE_OF_CONDUCT.md).
Commits are signed off under the [DCO](https://developercertificate.org/), PR
titles follow [Conventional Commits](https://www.conventionalcommits.org/), and
`go test -race`, `gofmt -s`, and `go vet` must pass.

## License

Licensed under the [Apache License 2.0](LICENSE). See [NOTICE](NOTICE) and
[THIRD-PARTY-LICENSES.md](THIRD-PARTY-LICENSES.md) for attribution and
dependency licenses.

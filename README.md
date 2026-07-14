<div align="center">

<img src="website/public/logo.svg" alt="Pulsys logo" width="72" height="72" />

# Pulsys

An authenticated pull-through cache for Hugging Face with a sendfile/io_uring
warm path.

[pulsys.io](https://pulsys.io) · [Docs](https://pulsys.io/docs) · [Blog](https://pulsys.io/blog)

[![CI](https://github.com/pulsys-io/pulsys/actions/workflows/linux.yml/badge.svg)](https://github.com/pulsys-io/pulsys/actions/workflows/linux.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/pulsys-io/pulsys.svg)](https://pkg.go.dev/github.com/pulsys-io/pulsys)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/pulsys-io/pulsys/badge)](https://securityscorecards.dev/viewer/?uri=github.com/pulsys-io/pulsys)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13603/badge)](https://www.bestpractices.dev/projects/13603)
[![SLSA 3](https://slsa.dev/images/gh-badge-level3.svg)](https://slsa.dev/spec/v1.0/levels)
[![Release](https://img.shields.io/github/v/release/pulsys-io/pulsys?sort=semver)](https://github.com/pulsys-io/pulsys/releases)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

</div>

---

Pulsys is an authenticated pull-through cache for the Hugging Face Hub. Point
Hugging Face clients at it with `HF_ENDPOINT`: the first pull of a model fills a
local disk cache, and every pull after that is served from disk with no upstream
egress.

Warm hits use io_uring on Linux 6.1+ and `sendfile` on macOS.
<!-- bench:headline:start -->
On a 48-vCPU `c7i.12xlarge` it sustains **1.36M req/s** at 4 KiB and **90 GB/s**
at 16 MiB.
<!-- bench:headline:end -->
See [`docs/benchmarks.md`](docs/benchmarks.md).

## Quick start

```bash
git clone --recurse-submodules https://github.com/pulsys-io/pulsys.git
cd pulsys
export PULSYS_HF_TOKEN=hf_your_readonly_token
docker compose up --build
```

Open the admin console at http://localhost:3000 (`admin@pulsys.local` / `admin`)
and create an API key at [http://localhost:3000/tokens](http://localhost:3000/tokens).
Then point any Hugging Face client at the proxy:

```bash
export HF_ENDPOINT=http://localhost:8082
export HF_TOKEN=pulsys_...           # the API key you just created
hf download Qwen/Qwen2.5-0.5B        # first run fills the cache; next run is served from disk
```

`huggingface_hub`, `transformers`, `datasets`, the `hf` CLI, and `hf_transfer`
work unchanged.

## Deploy

```bash
helm install pulsys oci://ghcr.io/pulsys-io/charts/pulsys \
  --set proxy.publicBaseURL=https://hf.example.com \
  --set postgres.host=postgres.db.svc \
  --set admin.imports.hfTokenSecret=pulsys-hf \
  --set persistence.size=200Gi
```

Pulsys runs as a single proxy instance backed by an external PostgreSQL.
Multi-node clustering is on the [roadmap](ROADMAP.md). Chart values, SSO setup,
and hardening: [`deploy/charts/pulsys/`](deploy/charts/pulsys/),
[`docs/oidc.md`](docs/oidc.md), [`docs/security.md`](docs/security.md).

## Documentation

Rendered docs: [pulsys.io/docs](https://pulsys.io/docs). Full index:
[`docs/`](docs/README.md). Common entry points:

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

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

Local full stack (builds from `docker/Dockerfile`: proxy, Postgres, Keycloak,
admin console):

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

Pulls `ghcr.io/pulsys-io/pulsys:latest` and
`ghcr.io/pulsys-io/pulsys-console:latest`. Needs
[Kind](https://kind.sigs.k8s.io/), Helm, and a Hugging Face read token.

```bash
git clone --recurse-submodules https://github.com/pulsys-io/pulsys.git
cd pulsys
export PULSYS_HF_TOKEN=hf_your_readonly_token

kind create cluster --name pulsys

kubectl apply --server-side -f \
  https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-1.24/releases/cnpg-1.24.1.yaml
kubectl -n cnpg-system rollout status deploy/cnpg-controller-manager --timeout=5m

kubectl apply -f deploy/charts/pulsys/examples/cnpg-cluster-kind.yaml
kubectl wait --for=condition=Ready cluster/pulsys-pg --timeout=5m

kubectl create secret generic pulsys-hf --from-literal=token="$PULSYS_HF_TOKEN"

helm upgrade --install pulsys deploy/charts/pulsys \
  -f deploy/charts/pulsys/examples/values-kind.yaml

kubectl port-forward svc/pulsys-console 3000:80 &
kubectl port-forward svc/pulsys-keycloak 8081:8080 &
kubectl port-forward svc/pulsys 8082:8080 &
```

Open http://localhost:3000 — `admin@pulsys.local` / `admin`.
Proxy: http://localhost:8082.

More chart options: [`deploy/charts/pulsys/`](deploy/charts/pulsys/).

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

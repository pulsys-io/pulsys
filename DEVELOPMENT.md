# Development

Everything you need to build, test, benchmark, and find your way around the
codebase. New to the project? Start with the [README](README.md), then come
back here.

## Table of contents

1. [Prerequisites](#prerequisites)
2. [Repository layout](#repository-layout)
3. [Local development](#local-development)
4. [Building](#building)
5. [Testing](#testing)
6. [Benchmarks](#benchmarks)
7. [Go documentation (pkgsite)](#go-documentation-pkgsite)
8. [Code style and gates](#code-style-and-gates)

---

## Prerequisites

Clone with submodules (required for the full HTTP parser corpus):

```bash
git clone --recurse-submodules https://github.com/pulsys-io/pulsys.git
# or, after a plain clone:
git submodule update --init --recursive
```

| Tool | Version | Why |
|------|---------|-----|
| Go | 1.25.x (pinned by the `go.mod` toolchain directive) | Build everything. |
| Docker / Docker Compose v2 | recent | Full-stack local dev, Linux-only test paths. |
| Node | 20.x | Build the admin UI and the marketing site. |
| Postgres | 16 (optional; compose provides one) | Native dev for the admin plane. |
| `benchstat` | optional | Comparing benchmark runs (`go install golang.org/x/perf/cmd/benchstat@latest`). |

---

## Repository layout

```
cmd/                        # Main binaries (each main package is small + obvious)
  pulsys/                 # The product binary. Wires config → coreserver → auth → admin.
  pulsys-db/                # Operational CLI: migrate, tenant, oidc configure, health.
  fake-hf/                  # Fake HF origin used by benchmarks + integration tests.
  bench-*/                  # Standalone bench harnesses (warm path / TTFB / stdlib compare).

internal/
  config/                   # Single source of truth for CLI flags + Config struct.
  coreserver/               # Custom HTTP/1.1 server, hot path, parser, io_uring reactor.
    iouring_*_linux.go      # Linux-only io_uring path (kernel >= 6.1, opt-in via -iouring).
    server.go               # Std-lib cork+sendfile path (Darwin + Linux default).
    parser_*.go             # Hardened HTTP/1.1 parser (smuggling-resistant).
  cache/                    # Disk-backed object store; meta.json + object dirs.
  upstream/                 # Origin fetcher: streams HF → disk on the first miss.
  proxy/                    # Routing, range handling, registry handlers (upload/commit/LFS).
  registry/                 # CommitTx, file_revisions, blob upserts, RLS-aware queries.
  blobstore/                # Local filesystem object store (LFS + inline blobs).
  hfhub/                    # HF API model/types + mockhub used by tests.
  classify/ rewrite/        # URL → cache-key classification; upstream URL rewriting.

  auth/                     # PATs, sessions, PKCE, CSRF, OIDC, RBAC, AuthGate impl.
    proxygate.go            # Data-plane PAT enforcement (every request, incl. warm hits).
    middleware.go           # Admin-plane session/PAT middleware.
    rbac.go                 # Role → permission matrix.

  admin/                    # Admin HTTP plane (mounted only when PULSYS_DB_DSN is set).
    api/                    # /admin/api/v1/* JSON handlers.
    audit/                  # Audit-log middleware on mutating routes.

  db/                       # pgx pool, migrations runner.
    migrations/             # 0001_initial → 0004_registry (numbered, idempotent).

  telemetry/ observability/ logx/   # expvar counters, /metrics + /healthz, slog wrapper.

  security/                 # Test-only packages owned by the security matrix.
    sectest/                # Black-box raw-TCP regression: smuggling, slowloris, CVE pins.
    authcontract/           # Per-route auth deniability matrix (Postgres-backed).
    businesslogic/          # OWASP WSTG-BUSL invariants (CAS races, size lies, etc.).

  testpg/ testserver/ integration/  # Test fixtures + end-to-end harnesses.

deploy/
  charts/pulsys/          # Helm chart (values.schema.json, helm-unittest, ci/ scenarios).
  docker/Dockerfile         # Combined OSS image (pulsys + pulsys-db).
  keycloak/                 # Hardened production Keycloak realm.

infra/
  cdk/                      # EC2 benchmark harness (CDK + SSM).
  packer/                   # Stock + tuned benchmark AMIs.

docker/                     # Local compose stack: Dockerfile + nginx + dev Keycloak realm.
docs/                       # Decision records, deployment, security, perf reports.
website/                    # Marketing / docs site.
admin-ui/                   # Admin SPA (Next.js, static export).
```

### Where to look for…

| Concern | Start here |
|---------|-----------|
| The HTTP/1.1 hot path (parser, warm hit, fallback) | [`internal/coreserver/server.go`](internal/coreserver/server.go) + `parser_*.go` |
| Linux io_uring reactor (kernel ≥ 6.1, opt-in) | [`internal/coreserver/iouring_reactor_linux.go`](internal/coreserver/iouring_reactor_linux.go) |
| Adding a CLI flag | [`internal/config/config.go`](internal/config/config.go) + wire in `cmd/pulsys/main.go` |
| Data-plane auth (PATs at warm-hit time) | [`internal/auth/proxygate.go`](internal/auth/proxygate.go) |
| Admin-plane auth (sessions, CSRF) | [`internal/auth/middleware.go`](internal/auth/middleware.go), [`docs/security.md`](docs/security.md#csrf-protection) |
| OIDC / identity providers | [`internal/auth/`](internal/auth/) + [`docs/oidc.md`](docs/oidc.md) |
| Admin API JSON contract | [`internal/admin/api/handler.go`](internal/admin/api/handler.go) |
| Registry / commit / LFS | [`internal/proxy/upload_handler.go`](internal/proxy/upload_handler.go) + [`internal/registry/`](internal/registry/) |
| Adding a security regression test | [`internal/security/`](internal/security/) |
| Disk cache layout | [`internal/cache/`](internal/cache/), [`docs/internals.md`](docs/internals.md) |
| Postgres schema | [`internal/db/migrations/`](internal/db/migrations/) — numbered, idempotent up/down |

---

## Local development

### One-command stack

Brings up Postgres, Keycloak (dev IdP), an init container that runs migrations +
OIDC config, pulsys, and the admin console.

```bash
docker compose up --build
```

| Service  | URL                   | Notes |
|----------|-----------------------|-------|
| Console  | http://localhost:3000 | Admin SPA + same-origin `/auth` and `/admin/api/v1`. |
| HF proxy | http://localhost:8082 | `HF_ENDPOINT=http://localhost:8082 hf download …` |
| Keycloak | http://localhost:8081 | Dev IdP UI. Admin login `admin` / `admin`. |

Sign in to the console with the dev realm user `admin@pulsys.local` / `admin`
(member of `pulsys:owner`, JIT-provisioned as `owner` on first login). If you
already run `pulsys` on `:8080`, compose maps the container proxy to host `:8082`
to avoid a clash (change the `ports` mapping in `compose.yaml` to prefer `:8080`).

> The bundled Keycloak realm and the `admin`/`admin` logins are **development
> conveniences only**. For production OIDC, see [`docs/oidc.md`](docs/oidc.md) and
> the hardened realm in [`deploy/keycloak/`](deploy/keycloak/).

What compose starts:

```
postgres ──┐
           ├── init (migrate + tenant + OIDC configure, one-shot)
keycloak ──┘
                │
                ▼
            pulsys (:8080 cache, :6060 admin API)
                │
                ▼
            console (nginx :80 → host :3000, static admin SPA)
```

- **Postgres 16** — schema via `pulsys-db migrate up`.
- **Keycloak 26** — dev OIDC issuer with the pre-imported `pulsys` realm.
- **init** — ensures the default tenant and OIDC provider config (one-shot).
- **pulsys** — cache proxy + admin API (mounted when `PULSYS_DB_DSN` is set).
- **console** — nginx edge matching the AMI layout (static SPA + API proxy).

Smoke-test the running stack, then reset volumes when done:

```bash
PULSYS_SMOKE_BASE=http://localhost:3000 bash scripts/smoke-pulsys.sh
docker compose down -v        # drops the pulsys_pg and pulsys_cache volumes
```

Optional compose overrides via shell env or a `.env` file in the repo root:
`PULSYS_OIDC_ISSUER` (default `http://localhost:8081/realms/pulsys`) and
`PULSYS_OIDC_REDIRECT_URI` (default `http://localhost:3000/auth/oidc/callback`).

### Native dev (no Docker)

```bash
go build -o bin/pulsys ./cmd/pulsys
go build -o bin/pulsys-db ./cmd/pulsys-db

# Pulsys has no open mode: the admin plane (Postgres) is required.
docker run --rm -d --name pulsys-pg \
  -e POSTGRES_USER=pulsys -e POSTGRES_PASSWORD=pulsys -e POSTGRES_DB=pulsys \
  -p 5432:5432 postgres:16-alpine
export PULSYS_DB_DSN='postgres://pulsys:pulsys@localhost:5432/pulsys?sslmode=disable'
./bin/pulsys-db migrate up

# PULSYS_DB_DSN and PULSYS_HF_TOKEN are both required; the proxy refuses to
# start without them.
export PULSYS_HF_TOKEN=hf_your_readonly_token
./bin/pulsys -listen :8080 -public-base-url http://127.0.0.1:8080 -cache-dir ./hf-cache
```

The admin UI is run separately: `cd admin-ui && npm install && npm run dev`.

---

## Building

```bash
go build -o bin/pulsys     ./cmd/pulsys
go build -o bin/pulsys-db    ./cmd/pulsys-db
```

Reproducible flags (used by the Docker / release builds):

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/pulsys ./cmd/pulsys
```

Combined OSS Docker image (both binaries):

```bash
docker build -f deploy/docker/Dockerfile -t pulsys:dev .
```

---

## Testing

### Day-to-day (Darwin or Linux)

```bash
go test ./...                 # fast smoke
go test -race -count=1 ./...  # canonical pre-commit
go vet ./...                  # always
```

`go test -race` is the source of truth and is what CI runs
([`.github/workflows/linux.yml`](.github/workflows/linux.yml)).

### Linux-only paths from macOS

`io_uring` and Linux-specific syscalls only compile on Linux. CI runs on
`ubuntu-latest` ([`.github/workflows/linux.yml`](.github/workflows/linux.yml));
locally you can use the security-test compose harness:

```bash
docker compose -f docker-compose.security-tests.yml run --rm security-tests
```

### Postgres-backed integration tests

Tests under `internal/security/businesslogic/`, `internal/security/authcontract/`,
and `internal/admin/store/` need a real Postgres and skip cleanly when
`PULSYS_TEST_PG_DSN` is unset:

```bash
docker run --rm -d --name pulsys-testpg \
  -e POSTGRES_USER=pulsys -e POSTGRES_PASSWORD=pulsys -e POSTGRES_DB=pulsys \
  -p 5433:5432 postgres:16-alpine
export PULSYS_TEST_PG_DSN='postgres://pulsys:pulsys@localhost:5433/pulsys?sslmode=disable'
go test -race ./internal/security/businesslogic/... ./internal/security/authcontract/...
```

### OWASP security matrix (Linux + io_uring + Postgres)

```bash
scripts/security-tests-linux.sh
```

Runs the full security matrix against a docker-compose stack with an isolated
Postgres and forces the Linux-only io_uring reactor path.

| Package | What it pins |
|---------|--------------|
| [`internal/security/sectest/`](internal/security/sectest/) | Black-box raw-TCP: smuggling, slowloris, CVE pins, parser-error observability. |
| [`internal/security/authcontract/`](internal/security/authcontract/) | Per-route auth deniability: revoked/wrong-scope PAT, expired session, cross-tenant. |
| [`internal/security/businesslogic/`](internal/security/businesslogic/) | OWASP WSTG-BUSL invariants: settings CAS races, LFS size lies, commit-path traversal. |

---

## Benchmarks

### Microbenchmarks

```bash
go test -run=__none -bench=BenchmarkCoreServerWarm  -benchmem ./internal/coreserver
go test -run=__none -bench=BenchmarkArtifactGetWarm -benchmem ./internal/proxy
```

The hot path holds **1 alloc/op, 256 B/op** on the 256 KiB / 4 MiB warm benches.
Any change that perturbs these numbers should include a `benchstat` table:

```bash
go install golang.org/x/perf/cmd/benchstat@latest
go test -run=NONE -bench=BenchmarkCoreServerWarm -benchmem -count=8 ./internal/coreserver > /tmp/before.txt
# ... make change ...
go test -run=NONE -bench=BenchmarkCoreServerWarm -benchmem -count=8 ./internal/coreserver > /tmp/after.txt
benchstat /tmp/before.txt /tmp/after.txt
```

### Full warm matrix (vs. DingoSpeed)

Reproduces the charts under `docs/results/darwin/` (~10 min):

```bash
scripts/bench_matrix.sh        # warm throughput across concurrency × size
scripts/bench_footprint.sh     # RSS + cumulative CPU during sustained load
scripts/bench_e2e.sh           # real `hf download` wallclock
scripts/render_charts.sh darwin
```

### io_uring saturation (Linux) and the EC2 harness

The full-machine io_uring numbers (`-iouring`, kernel ≥ 6.1) and how to reproduce
them on a plain Linux box, in Docker, or via the AMI + CDK + SSM harness — plus how
to **verify io_uring engaged** — are in
[`docs/benchmarks.md`](docs/benchmarks.md#reproduce).

Deep dives: [`docs/internals.md`](docs/internals.md) (the warm path, the
macOS sendfile/sf_hdtr fusion, the Linux io_uring reactor, and the Xet/CAS
chunked-blob format).

---

## Go documentation (pkgsite)

Browse the package docs locally exactly as they will render on pkg.go.dev:

```bash
go install golang.org/x/pkgsite/cmd/pkgsite@latest
pkgsite -open .
```

This serves a local pkg.go.dev UI for the module. Package docs follow the
[Go Doc Comments](https://go.dev/doc/comment) format; each `internal/security/*`
package has a `doc.go` describing its taxonomy. When adding a new package, add a
package comment and, where it helps, testable `Example` functions (they render in
the docs and run under `go test`).

---

## Code style and gates

- `gofmt -s` formatting and `go vet ./...` are mandatory.
- `go test -race ./...` must pass.
- `govulncheck ./...` for dependency vulnerabilities.
- `golangci-lint run` and `gosec` run in CI.

Style references we follow: [Effective Go](https://go.dev/doc/effective_go),
[Go Code Review Comments](https://go.dev/wiki/CodeReviewComments), and the
[Google Go Style Guide](https://google.github.io/styleguide/go/). See
[CONTRIBUTING.md](CONTRIBUTING.md) for the contribution workflow.

An optional local pre-commit hook (secret scan + `gofmt` + `go vet`) is available:

```bash
ln -s ../../scripts/git-hooks/pre-commit .git/hooks/pre-commit
```

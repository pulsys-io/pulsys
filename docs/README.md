# Pulsys documentation

Start at the [project README](../README.md) for the 60-second quick starts. This
index is the map for everything else, grouped by what you are trying to do.

## Operate it (run, deploy, trust, measure)

| Doc | Purpose |
|-----|---------|
| [`../DEVELOPMENT.md`](../DEVELOPMENT.md) | Build, run the local stack, test, and find your way around the code. |
| [`architecture.md`](architecture.md) | System and infrastructure view: components, request flow, deployment topology, cache warming, and quota. |
| [`oidc.md`](oidc.md) | SSO setup for Keycloak, AWS Cognito, and IAM Identity Center, plus break-glass owner recovery. |
| [`security.md`](security.md) | Credential model, parser hardening, threat model, deployment hardening, supply-chain posture. |
| [`benchmarks.md`](benchmarks.md) | Headline numbers, the receipts, and how to reproduce them (local, Linux, EC2). |

## Understand it (how it is built, and why)

| Doc | Purpose |
|-----|---------|
| [`internals.md`](internals.md) | Software and syscall layer: warm-path implementation (io_uring, sendfile/sf_hdtr), Xet/CAS protocol, and OS tuning. |

## Deploy references (live with the code)

| Path | Purpose |
|------|---------|
| [`../deploy/charts/pulsys/README.md`](../deploy/charts/pulsys/README.md) | Helm chart: values reference and examples. |
| [`../infra/cdk/README.md`](../infra/cdk/README.md) | Benchmark AMI build + CDK + SSM harness. |
| [`../admin-ui/README.md`](../admin-ui/README.md) | Admin console (Next.js SPA). |
| [`results/`](results/) | Generated benchmark datasets and charts (Darwin head-to-head; EC2 ships empty). |

## Project files (repo root)

| File | Purpose |
|------|---------|
| [`../CONTRIBUTING.md`](../CONTRIBUTING.md) | How to contribute (DCO, Conventional Commits, review). |
| [`../SECURITY.md`](../SECURITY.md) | How to report a vulnerability (policy). For the engineering detail, see [`security.md`](security.md). |
| [`../CODE_OF_CONDUCT.md`](../CODE_OF_CONDUCT.md) | Community standards. |

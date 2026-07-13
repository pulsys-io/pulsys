# Roadmap

This roadmap is **directional, not a commitment.** Items describe intended
direction and rough priority; they have **no committed timeline**, and
priorities may change. Nothing here is guaranteed to ship in any particular
release.

For what is supported **today**, see the
[Project status & scope](README.md#project-status--scope) section of the README.

## v2 — High-availability clustering

**Status: planned — not yet supported.**

Pulsys today runs as a single proxy node. The admin/job state (Postgres) can
already run HA, but the proxy data plane is one process:

- In-flight fetch de-duplication (`AcquireRange`) and other request
  coordination are **in-process**, so they do not span nodes.
- The disk cache is **local** to the node.

Consequently, running more than one proxy replica is **not currently
supported**:

- **Shared (RWX) volume, multiple replicas** — replicas can race cache writes
  with no cross-node write lock, risking corrupted cached objects.
- **Separate volumes, multiple replicas** — each node warms independently:
  duplicated storage and duplicated upstream fetches, with no shared de-dup.

### Intended design

Keep each node's **local disk + zero-copy warm path** (`sendfile` / `io_uring`)
— a shared network filesystem would defeat the performance story — and add a
**consistent-hash routing tier** that pins each cache key to a node. Same-key
requests land on the same node, which preserves local-disk serving and lets the
per-node `AcquireRange` de-duplication keep working across the fleet. This
generalizes the existing single-node contention handling (observable via the
`pulsys_inflight_contended_passthrough` metric).

## Managed cloud support

Pulsys deploys today via Docker Compose and the
[Helm chart](deploy/charts/pulsys/) (single replica, external Postgres).
Deeper cloud integration is planned:

- **EKS deployment guide.** An end-to-end walkthrough: EBS-backed cache
  volume, RDS/Aurora for the admin plane, IAM Identity Center SSO
  (building on [`docs/oidc.md`](docs/oidc.md)), and ALB ingress.
- **AWS Marketplace AMI.** A production AMI derived from the existing
  [Packer benchmark AMIs](infra/packer/) (systemd unit, baked sysctls),
  hardened for standalone EC2 deployments.
- **Terraform module.** Opinionated single-node EC2 + RDS + EBS deployment
  for teams not running Kubernetes.

## Storage backends

The cache and registry blob store are **local disk only** today — deliberate,
because the zero-copy warm path (`sendfile` / `io_uring`) serves straight from
the local filesystem.

- **S3-compatible cold tier.** An `S3Store` backend behind the existing
  `blobstore` interface (already sketched in
  [`internal/blobstore/doc.go`](internal/blobstore/doc.go)): S3/MinIO as the
  durable tier, staged onto local disk for warm serving so the performance
  story is preserved.
- **NFS-backed cold tier.** Same staging model for shared-filesystem
  environments.

## Registry & supply-chain candidates

- **Artifact malware scanning.** Scan imported/uploaded artifacts (e.g.
  pickle/safetensors inspection) before they become servable, for air-gapped
  delivery pipelines.
- **Tag locking.** Pin a repo tag/revision so it cannot be silently
  re-pointed — consistency guarantees from development to production. The
  registry `mirrors` table already supports a `pinned_sha`; tag locking
  generalizes this to first-class policy.
- **Multi-region replication.** Asynchronous, resumable cache replication
  between regions for low-latency local access. Depends on the clustering
  and storage-backend work above.

## Other candidates

- **Fan-out streaming for in-flight contention.** Today, a request that
  contends with a long in-flight whole-file fetch falls through to an
  independent, non-caching pass-through — correct for the client, but it costs
  a duplicate upstream fetch for that file. A fan-out reader that serves the
  waiter from the partially-written cache file would remove the duplication.
  Worth doing only if `pulsys_inflight_contended_passthrough` shows meaningful
  cold-overlap traffic in practice.

_This list is non-exhaustive; additional items are tracked in the issue
tracker._

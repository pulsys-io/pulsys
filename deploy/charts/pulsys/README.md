# pulsys

![Version: 0.1.0](https://img.shields.io/badge/Version-0.1.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.1.0](https://img.shields.io/badge/AppVersion-0.1.0-informational?style=flat-square)
[![helm CI](https://github.com/pulsys-io/pulsys/actions/workflows/helm.yml/badge.svg)](https://github.com/pulsys-io/pulsys/actions/workflows/helm.yml)

A high-performance Hugging Face disk-caching reverse proxy with a required Pulsys admin plane (OIDC, RBAC, audit, imports).

`pulsys` is a high-performance, disk-caching reverse proxy for the Hugging
Face Hub. Point any HF client at it (`HF_ENDPOINT=...`) and downloads are served
from a local cache at line rate. Pulsys is authenticated by default: the
**Pulsys admin plane** (OIDC login, RBAC, audit log, model imports) is required,
so every data-plane request carries a per-request Pulsys API key.

> This chart is also published to an OCI registry:
> `oci://ghcr.io/pulsys-io/charts/pulsys`.

## TL;DR

```bash
# Pulsys is authenticated by default. Provide an external Postgres and a Secret
# holding a read-only Hugging Face token, then install:
kubectl create secret generic pulsys-hf --from-literal=token=hf_your_readonly_token

helm install pulsys oci://ghcr.io/pulsys-io/charts/pulsys \
  --set proxy.publicBaseURL=https://hf.example.com \
  --set admin.enabled=true \
  --set postgres.host=postgres.db.svc \
  --set admin.imports.hfTokenSecret=pulsys-hf

# Issue a Pulsys API key in the admin UI, then:
HF_ENDPOINT=http://<proxy-host>:8080 HF_TOKEN=pulsys_... hf download gpt2
```

## Deployment requirements

Pulsys has no open mode. The chart requires:

- `admin.enabled=true`: the admin plane that issues and enforces API keys (off by default so a bare install fails fast).
- An EXTERNAL PostgreSQL database (this chart never bundles one): set `postgres.host`, `postgres.existingSecret`, or `postgres.dsn`.
- `admin.imports.hfTokenSecret`: a Secret (key `token`) with Pulsys's read-only Hugging Face token.

A bare `helm install` fails fast with actionable messages until these are set.
For the admin plane database we recommend the [CloudNativePG](https://cloudnative-pg.io)
operator. See [`examples/cnpg-cluster.yaml`](examples/cnpg-cluster.yaml).

## Admin plane with CloudNativePG

```bash
# 1. Install the CNPG operator (once per cluster), then apply a Cluster:
kubectl apply -f examples/cnpg-cluster.yaml

# 2. Install the chart pointing at the CNPG-generated app Secret:
helm install pulsys oci://ghcr.io/pulsys-io/charts/pulsys \
  --set proxy.publicBaseURL=https://hf.example.com \
  --set admin.enabled=true \
  --set postgres.existingSecret=pulsys-pg-app \
  --set postgres.existingSecretKey=uri \
  --set oidc.enabled=true \
  --set oidc.issuer=https://idp.example.com/realms/pulsys \
  --set oidc.redirectURI=https://hf.example.com/auth/oidc/callback \
  --set oidc.existingSecret=pulsys-oidc
```

OIDC values map 1:1 to the IdP setup guides in
[`docs/oidc.md`](https://github.com/pulsys-io/pulsys/blob/main/docs/oidc.md) (Keycloak,
AWS Cognito, AWS IAM Identity Center).

## Ports

| Port | Name | Purpose |
| ---- | ---- | ------- |
| `8080` | `http` | Data-plane ingress (HF cache). Serves `/healthz`. |
| `6060` | `admin` | Admin API (`/admin/api/v1`), `/metrics`, `/healthz`. Keep internal. |

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| ajbeach2 |  | <https://github.com/ajbeach2> |

## Source Code

* <https://github.com/pulsys-io/pulsys>

## Requirements

Kubernetes: `>=1.27.0-0`

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| admin.enabled | bool | `false` | Enable the Pulsys admin plane (OIDC, RBAC, audit, imports). Required: Pulsys has no open mode, so the chart refuses to render with this false. Off by default so a bare `helm install` fails fast with that message. |
| admin.imports.hfTokenSecret | string | `""` | REQUIRED. Name of an existing Secret holding Pulsys's read-only Hugging Face token under key `token`. Sets PULSYS_HF_TOKEN, used for all upstream auth (cold-miss reads + imports). Pulsys refuses to start without it. |
| admin.imports.jobTimeout | string | `""` | Per-job import timeout (Go duration); empty uses 24h. |
| admin.imports.maxWorkers | string | `""` | Max concurrent import fetch workers; empty uses 2*GOMAXPROCS. |
| admin.imports.worker | bool | `true` | Run the background model-import worker in the proxy process. |
| admin.init.enabled | bool | `true` | Run initContainers on the proxy Deployment that migrate the database, ensure the tenant, and (when oidc.enabled) configure the OIDC provider before the proxy starts. Migrations are lock-guarded and safe when pod starts overlap. |
| admin.tenant | string | `"default"` | Tenant name used by the admin plane. |
| affinity | object | `{}` | Affinity rules. |
| fullnameOverride | string | `""` | Override the fully qualified app name. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.repository | string | `"ghcr.io/pulsys-io/pulsys"` | Container image repository for the pulsys binary. |
| image.tag | string | `""` | Image tag. Defaults to the chart's appVersion when empty. |
| imagePullSecrets | list | `[]` | Image pull secrets for private registries. |
| ingress.annotations | object | `{}` | Ingress annotations. |
| ingress.className | string | `""` | IngressClass name. |
| ingress.enabled | bool | `false` | Create an Ingress for the data plane. |
| ingress.hosts | list | `[{"host":"chart-example.local","paths":[{"path":"/","pathType":"Prefix"}]}]` | Ingress host rules. |
| ingress.tls | list | `[]` | TLS configuration. |
| metrics.serviceMonitor.enabled | bool | `false` | Create a Prometheus Operator ServiceMonitor scraping /metrics on the admin port. |
| metrics.serviceMonitor.interval | string | `"30s"` | Scrape interval. |
| metrics.serviceMonitor.labels | object | `{}` | Extra labels for the ServiceMonitor (e.g. to match a Prometheus selector). |
| nameOverride | string | `""` | Override the chart name. |
| nodeSelector | object | `{}` | Node selector. |
| oidc.clientID | string | `"pulsys-admin"` | OIDC client ID. |
| oidc.clientSecret | string | `""` | OIDC client secret. Prefer existingSecret in production. |
| oidc.discoveryBase | string | `""` | Backchannel discovery base used by the proxy to reach the IdP in-cluster (PULSYS_OIDC_DISCOVERY_BASE). Empty falls back to the issuer. |
| oidc.enabled | bool | `false` | Enable OIDC login for the admin console. |
| oidc.existingSecret | string | `""` | Existing Secret holding the OIDC client secret under `client-secret`. |
| oidc.issuer | string | `""` | OIDC issuer URL (browser-facing; validated against the id_token `iss`). |
| oidc.ownerGroups | string | `"pulsys:owner"` | Group(s) mapped to the tenant owner role, comma-joined. |
| oidc.redirectURI | string | `""` | OAuth redirect URI registered with the IdP. |
| persistence.accessModes | list | `["ReadWriteOnce"]` | Access modes for the cache PVC. |
| persistence.annotations | object | `{}` | Extra annotations on the PVC. |
| persistence.enabled | bool | `true` | Provision a PersistentVolumeClaim for the cache. |
| persistence.existingClaim | string | `""` | Use an existing PVC instead of creating one. |
| persistence.size | string | `"100Gi"` | Requested cache volume size. |
| persistence.storageClass | string | `""` | StorageClass; empty uses the cluster default. |
| podAnnotations | object | `{}` | Annotations added to the Pod. |
| podLabels | object | `{}` | Labels added to the Pod. |
| podSecurityContext | object | `{"fsGroup":65532,"runAsGroup":65532,"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod-level security context. Defaults run the process as an unprivileged user with a writable cache volume. |
| postgres.database | string | `"pulsys"` |  |
| postgres.dsn | string | `""` | Full Postgres DSN. Highest precedence. Use existingSecret in production. |
| postgres.existingSecret | string | `""` | Existing Secret containing the DSN; preferred for production / CNPG. |
| postgres.existingSecretKey | string | `"dsn"` | Key within existingSecret holding the DSN. |
| postgres.host | string | `""` | Individual connection fields (used to assemble a DSN when dsn and existingSecret are both empty). `password` is rendered into a Secret. |
| postgres.password | string | `""` |  |
| postgres.port | int | `5432` |  |
| postgres.sslmode | string | `"require"` |  |
| postgres.user | string | `"pulsys"` |  |
| probes | object | `{"liveness":{"enabled":true,"failureThreshold":3,"initialDelaySeconds":5,"periodSeconds":10,"timeoutSeconds":3},"readiness":{"enabled":true,"failureThreshold":3,"initialDelaySeconds":3,"periodSeconds":10,"timeoutSeconds":3}}` | Liveness/readiness probe configuration (HTTP GET /healthz on the proxy port). |
| proxy.allowHosts | list | `[]` | Override the upstream host allowlist (comma-joined). Empty uses the built-in Hugging Face allowlist. |
| proxy.cacheDir | string | `"/var/lib/pulsys/cache"` | On-disk cache directory (mounted from persistence). |
| proxy.cacheMaxBytes | int | `0` | Maximum total on-disk cache bytes; 0 leaves the cache unbounded. |
| proxy.defaultUpstreamHost | string | `"huggingface.co"` | Default upstream host. |
| proxy.extraArgs | list | `[]` | Extra raw CLI flags appended to the pulsys command (e.g. tuning flags like "-listeners=8" or "-iouring"). |
| proxy.extraEnv | list | `[]` | Extra environment variables (list of name/value or name/valueFrom). |
| proxy.logLevel | string | `"info"` | Log level: debug, info, warn, error. |
| proxy.publicBaseURL | string | `""` | Public base URL used to rewrite upstream Location headers. REQUIRED. Set to the externally reachable URL of the proxy (scheme + host[:port]). |
| proxy.upstreamScheme | string | `"https"` | Upstream scheme: https (production) or http (test against a local fake). |
| replicaCount | int | `1` | Number of proxy replicas. Must be 1: in-flight de-duplication and cache coordination are per-process, so multi-replica deployments are not supported (see ROADMAP.md for horizontal scale-out). |
| resources | object | `{"limits":{"memory":"1Gi"},"requests":{"cpu":"250m","memory":"128Mi"}}` | Compute resource requests/limits. |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true}` | Container-level security context. |
| service.adminPort | int | `6060` | Admin/metrics service port (auth, /admin/api/v1, /metrics). Kept on a separate port so it can be scoped away from the public data plane. |
| service.annotations | object | `{}` | Extra annotations for the Service. |
| service.nodePort | string | `nil` | NodePort for the data-plane port when type=NodePort. |
| service.port | int | `8080` | Data-plane (proxy) service port. |
| service.type | string | `"ClusterIP"` | Service type for the data-plane ingress. |
| serviceAccount.annotations | object | `{}` | Annotations for the ServiceAccount (e.g. an IRSA / Workload Identity role ARN). |
| serviceAccount.automountServiceAccountToken | bool | `false` | Automount the ServiceAccount API token. |
| serviceAccount.create | bool | `true` | Create a ServiceAccount. |
| serviceAccount.name | string | `""` | Name of the ServiceAccount to use. Generated when empty and create=true. |
| tolerations | list | `[]` | Tolerations. |
| topologySpreadConstraints | list | `[]` | Topology spread constraints. |

## Verifying an install

```bash
helm test <release> --namespace <namespace>
```

The bundled test hits `/healthz` on both the data-plane and admin ports.

## How this chart is tested

Every change to the chart runs a layered pipeline in
[`helm.yml`](https://github.com/pulsys-io/pulsys/blob/main/.github/workflows/helm.yml):

1. `helm lint` + `chart-testing` (version-bump and maintainer checks)
2. `helm-docs` drift check (this README is generated; stale docs fail CI)
3. `values.schema.json` validation — accepts known-good values, rejects bad ones
4. `helm-unittest` snapshot/unit tests (no cluster)
5. `kubeconform` manifest validation against k8s 1.27 / 1.29 / 1.31 schemas
6. Static security analysis: polaris, kube-score, and a Trivy config scan
7. Full install on [kind](https://kind.sigs.k8s.io) across oldest + newest
   supported Kubernetes, against a real CloudNativePG cluster, followed by
   `helm test` and an in-place `helm upgrade` + re-test

----------------------------------------------
Autogenerated from chart metadata using [helm-docs v1.14.2](https://github.com/norwoodj/helm-docs/releases/v1.14.2)

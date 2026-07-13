# infra/cdk -- pulsys bench stack

CDK app that provisions the EC2 instance and SSM tooling used to run the
pulsys benchmark harness on Linux. Reproduction guide: `docs/benchmarks.md`.

## Shape

```
Dedicated VPC (pulsys-bench, public subnets only, no NAT)
  └── EC2 c7i.12xlarge (48 vCPU, 96 GiB — default, Nitro virt)
        m5zn.metal / z1d.metal (48 vCPU bare metal) or c7i.metal-24xl (96) — overrides
        AMI:   /pulsys/stock-ami/latest  (or /pulsys/bench-ami/latest)
        EBS:   200 GiB gp3 root volume (perf.data + flamegraph headroom)
        IAM:   AmazonSSMManagedInstanceCore + S3 RW to ResultsBucket
        SG:    NO inbound rules; outbound all
        Key:   NOT associated -- access is SSM-only

S3 ResultsBucket   (retained on stack delete; lifecycle -> Glacier)

SSM Documents:
  PulsysProfile<id>  -- runs scripts/profile_baseline.sh, uploads to S3
  PulsysSweep<id>    -- runs scripts/sweep_tunings.sh,    uploads to S3
  PulsysBench<id>    -- runs scripts/bench_matrix.sh,     uploads to S3
```

### Why bare metal?

The default is `c7i.12xlarge` (48 vCPU), which is enough to reproduce the
committed reference run without requiring bare-metal quota. Bare metal is still
useful when you want less virtualization noise or full PMU access. Concretely on
metal:

| What you get on metal that you don't on virt |
|--|
| No hypervisor layer between the process and hardware. This can reduce variance and change the io_uring/sendfile cost profile. |
| Full perf/PMU access.  `perf stat` reports cycle counters, branch miss rates, cache misses, frontend stalls.  On virt these events are partially filtered. |
| Zero noisy-neighbour jitter.  The Category 2 tuning sweep produces statistically clean A/B deltas because the host is exclusively ours. |
| `bpftrace` + `eBPF` work with the full kernel event set (some tracepoints are gated on virt). |

### Trade-offs you should know about before deploying

| Cost | ~$1.3/hr `c6i.8xlarge`; ~$2.5/hr `m5zn.metal`; ~$5/hr `c7i.metal-24xl` (us-east-1).  Tear down between runs. |
|--|--|
| Boot time | ~10-15 min vs ~1 min for virt.  Bare metal initialises the full hardware platform on cold start. |
| vCPU quota | Default **32** (`c6i.8xlarge`).  Bare metal: **48** (`m5zn.metal`) or **96** (`c7i.metal-24xl`). |
| Spot | Bare metal Spot availability is patchy.  Treat this as on-demand. |
| Capacity | Bare metal capacity is regionally lumpy.  If deploy fails with `InsufficientInstanceCapacity`, destroy and redeploy (the stack's VPC spans 2 AZs; CDK may place the instance in either public subnet). |

### Iterating faster

For dev loops where the reference numbers do not matter (writing the harness,
verifying a tuning idea, smoke testing a new variant), use a smaller virtualized
size:

```sh
npx cdk deploy -c instanceType=c7i.4xlarge       # 16 vCPU, $0.70/hr
npx cdk deploy -c instanceType=c7i.metal-48xl    # 192 vCPU, 100 Gbps net
npx cdk deploy -c instanceType=c7id.metal-24xl   # +1.9 TiB local NVMe
```

### NUMA

`c7i.metal-24xl` is dual-socket Sapphire Rapids → at least 2 NUMA
nodes (4 if AWS leaves Sub-NUMA Clustering on in firmware).  Cross-
socket DRAM bandwidth is ~6× lower than local DRAM.  The
benchmark harness handles this explicitly:

- `scripts/profile_baseline.sh` captures `numactl --hardware`,
  `hwloc-ls`, and `numastat -p $HFPID` so the first profile run
  documents the actual topology.
- `scripts/bench_matrix.sh` accepts `PULSYS_VARIANT=numa`, which
  launches one `pulsys` per NUMA node, each pinned via
  `numactl --cpunodebind --membind`, all sharing the listen port
  through `SO_REUSEPORT`.  See `docs/internals.md` § "Per-core listeners
  and NUMA sharding" for the rationale.

Do the `default` vs `numa` A/B once on a fresh AMI to decide whether
the NUMA-sharded layout is worth the operational complexity for the
production sidecar AMI.

## Prerequisites

```sh
# AWS credentials in the target account/region.
aws sts get-caller-identity

# CDK toolkit installed globally (one-time).
npm install -g aws-cdk

# Bootstrap the target account/region (one-time).
cdk bootstrap aws://<account>/<region>
```

The AMI lookup is via SSM parameter, set by the Packer wrappers:

```sh
# Build + publish the stock AMI (one-time per region, or when the source changes):
scripts/build-stock-ami.sh

# After running the sweep + updating sysctl-pulsys-category2.conf,
# build + publish the tuned AMI:
scripts/build-tuned-ami.sh
```

## Deploy

```sh
cd infra/cdk
npm install                                      # one-time
npx cdk synth                                    # dry-run synthesis
npx cdk deploy -c amiKind=stock                  # default: c7i.12xlarge (48 vCPU)
npx cdk deploy -c amiKind=stock -c instanceType=m5zn.metal        # 48 vCPU bare metal
npx cdk deploy -c amiKind=stock -c instanceType=c7i.metal-24xl  # 96 vCPU bare metal
npx cdk deploy -c amiKind=tuned                  # tuned AMI variant
npx cdk deploy -c amiKind=stock -c instanceType=c7i.4xlarge  # virt, for dev loops
```

The deploy prints the following outputs you will copy from CloudFormation:

| Output             | Use                                                |
|--------------------|----------------------------------------------------|
| `VpcIdOut`         | default VPC id (looked up at deploy)                   |
| `InstanceIdOut`    | target for `aws ssm send-command` / `start-session` |
| `ResultsBucketOut` | `aws s3 cp s3://<bucket>/...`                       |
| `StartShellCmdOut` | one-shot copy/paste to open an SSM shell             |
| `RunProfileCmdOut` | one-shot copy/paste to run the profile harness       |
| `RunSweepCmdOut`   | one-shot copy/paste to run the sweep harness         |
| `RunBenchCmdOut`   | one-shot copy/paste to run the bench matrix          |

The helper scripts in `scripts/ssm-*.sh` wrap these for the common cases.

## Tear down

```sh
npx cdk destroy
```

The `ResultsBucket` has `RemovalPolicy.RETAIN`; results survive the stack
delete.  To clean them up, empty + delete the bucket via the console or
`aws s3 rb s3://<bucket> --force` after running destroy.

## Notes

- The stack uses the account **default VPC** in the deploy region (`Vpc.fromLookup`
  with `isDefault: true`).  Ensure a default VPC exists there (or recreate one
  in the EC2 console).  `cdk destroy` removes the instance and security group
  only; it does **not** delete the VPC.  Set `CDK_DEFAULT_ACCOUNT` and
  `CDK_DEFAULT_REGION` (or rely on the AWS CLI profile) so the lookup hits
  the VPC you expect.
- SSM document names are auto-generated by CloudFormation (no name
  collision when redeploying the stack); the generated names are surfaced
  via the `Run*Cmd` outputs.
- The `BenchInstance` has no associated keypair.  This is intentional --
  Packer's ephemeral ed25519 key is wiped by cloud-init on first boot,
  and CDK never re-adds one.  Access is exclusively via SSM Session
  Manager (`aws ssm start-session ...`).

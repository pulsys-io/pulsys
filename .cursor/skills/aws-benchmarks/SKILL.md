---
name: aws-benchmarks
description: >-
  Run Pulsys AWS EC2 benchmarks (stock AMI, CDK PulsysBench, saturation +
  warm hf/hf_transfer download) and regenerate the landing-page cast from the
  measured warm rate. Use when reproducing docs/results/ec2, updating hero
  cast numbers, preparing HN-facing receipts, or the user mentions AWS bench,
  SSM bench, or cast download rate.
---

# AWS benchmarks (simple path)

Goal: produce **published EC2 saturation artifacts** and a **landing-page cast** whose MiB/s comes from a real warm `hf download` + `hf_transfer` over **loopback after cache warm**.

Do **not** commit account IDs, instance IDs, VPC IDs, or `cdk.context.json`. Those stay local.

## One command

```bash
HF_TOKEN=hf_xxx scripts/run-aws-benchmarks.sh
```

Optional:

```bash
HF_TOKEN=hf_xxx scripts/run-aws-benchmarks.sh --skip-ami    # AMI already in SSM
HF_TOKEN=hf_xxx scripts/run-aws-benchmarks.sh --teardown    # destroy stack at end
INSTANCE_TYPE=c7i.4xlarge HF_TOKEN=hf_xxx scripts/run-aws-benchmarks.sh  # cheaper loop
```

What it does:

1. Builds/publishes stock AMI if `/pulsys/stock-ami/latest` is missing  
2. `cdk deploy` stack `PulsysBench` (default `c7i.12xlarge`)  
3. Writes HF token on the instance  
4. Syncs this checkout + rebuilds `pulsys` on the host  
5. `ssm-bench.sh variant=saturate-iouring` → `docs/results/ec2/`  
6. `ssm-hf-download.sh` cold then **warm** over `127.0.0.1`  
7. `render_hero_cast.sh` → `website/public/demos/hf-warm-demo.cast`

## Manual equivalent (same order)

```bash
scripts/build-stock-ami.sh          # once per region (skip if AMI param exists)
cd infra/cdk && npm i && \
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
scripts/ssm-teardown.sh             # when finished (stops burn)
```

## Cast rule

The hero cast **must** print the **warm** row from `tmp/bench/ec2/hf-download/results.csv` (`phase=warm`). That row is measured with:

- `HF_HUB_ENABLE_HF_TRANSFER=1`
- `HF_ENDPOINT=http://127.0.0.1:18080` (loopback to Pulsys)
- cache already filled by the cold phase
- client auth = bench `pulsys_*` PAT; upstream token = `PULSYS_HF_TOKEN` on the server

Do not invent rates. Do not use Darwin laptop numbers for the EC2 cast claim.

## Public-repo hygiene

| Commit | Never commit |
|--------|----------------|
| `docs/results/ec2/*` headline + charts | `infra/cdk/cdk.context.json` |
| `website/public/demos/hf-warm-demo.cast` | account / instance / AMI IDs |
| scripts + `infra/cdk/bin/bench.ts` | raw HF tokens |

## Prerequisites

- AWS CLI credentials that can run EC2 + SSM + CloudFormation + S3  
- `packer` (only when AMI is missing)  
- `HF_TOKEN` with read access to the model used for cold fill  
- Default region `us-east-1` (override with `AWS_REGION`)

## Cost

Default host `c7i.12xlarge` is on the order of a few dollars per hour. Tear down with `scripts/ssm-teardown.sh` when done.

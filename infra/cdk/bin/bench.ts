#!/usr/bin/env node
// bin/bench.ts -- CDK entrypoint for the pulsys bench stack.
//
// Context:
//   amiKind        "stock" | "tuned"          (default: stock)
//   instanceType   EC2 instance type string    (default: c7i.12xlarge, 48 vCPU)
//   stackName      CloudFormation stack name   (default: PulsysBench)
//
// Why a non-metal instance by default:
//
//   * Reproducible without bare-metal quota -- c7i.12xlarge is a standard
//     On-Demand Nitro instance anyone can launch.  AL2023 ships kernel
//     6.1, so the io_uring warm path engages just like on metal.
//   * Cheaper + faster to boot (~$2/hr, ~2 min) than a metal SKU.
//   * Loopback saturation is CPU/memory-bandwidth bound, so a 48 vCPU
//     virt instance still drives a meaningful full-machine demo.
//
// Bare metal still wins on absolute ceiling (no hypervisor) and full PMU
// access for the profile harness.  Override when quota allows:
//
//   npx cdk deploy -c instanceType=c7i.24xlarge    # 96 vCPU virt
//   npx cdk deploy -c instanceType=m5zn.metal      # 48 vCPU bare metal
//   npx cdk deploy -c instanceType=c7i.metal-24xl  # 96 vCPU bare metal
//
// Usage:
//   cd infra/cdk && npm install
//   npx cdk synth                                       # synth -> cdk.out/
//   npx cdk deploy -c amiKind=stock                     # deploy bench host
//   npx cdk deploy -c amiKind=tuned                     # swap to tuned AMI
//   npx cdk deploy -c instanceType=c7i.metal-24xl       # 96 vCPU bare metal
//   npx cdk destroy                                     # tear down (S3 retained)
import * as cdk from 'aws-cdk-lib';
import { BenchStack } from '../lib/bench-stack';

const app = new cdk.App();

const region =
  process.env.CDK_DEFAULT_REGION ||
  process.env.AWS_REGION ||
  'us-east-1';
// Account must come from the caller's environment / AWS profile.
// Do not hardcode account IDs, instance IDs, or AMI IDs here.
const account =
  process.env.CDK_DEFAULT_ACCOUNT ||
  process.env.AWS_ACCOUNT_ID;

const amiKindRaw = (app.node.tryGetContext('amiKind') as string) ?? 'stock';
if (amiKindRaw !== 'stock' && amiKindRaw !== 'tuned') {
  throw new Error(`amiKind must be "stock" or "tuned"; got ${amiKindRaw}`);
}
const amiKind = amiKindRaw as 'stock' | 'tuned';

const instanceType = (app.node.tryGetContext('instanceType') as string) ?? 'c7i.12xlarge';
const stackName = (app.node.tryGetContext('stackName') as string) ?? 'PulsysBench';

new BenchStack(app, stackName, {
  env: { account, region },
  amiKind,
  instanceType,
  description:
    'pulsys benchmark harness: EC2 bench host (default c7i.12xlarge, 48 vCPU) with SSM-only access, S3 results bucket, and SSM documents driving the profile/sweep/bench harness.',
});

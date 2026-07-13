// lib/bench-stack.ts -- the pulsys benchmark CDK stack.
//
// Shape:
//
//   Account default VPC (lookup at deploy time)
//     └── EC2 bench host (c7i.12xlarge by default: 48 vCPU virt, Intel SPR;
//         c7i.24xlarge / c7i.metal-24xl when more cores or bare metal wanted)
//          │
//           AMI: SSM parameter /pulsys/{stock,bench}-ami/latest
//           IAM: AmazonSSMManagedInstanceCore + S3 RW to ResultsBucket
//           SG : NO inbound rules; outbound all (SSM-only access)
//           Key: NOT associated -- ed25519 key from Packer is wiped on
//                first boot by cloud-init (see common-provision.sh)
//
//   The non-metal default is reproducible without bare-metal quota and
//   still runs the io_uring warm path (AL2023 kernel 6.1).  Bare metal
//   additionally gives us:
//     - Zero hypervisor overhead in the absolute ceiling numbers.
//     - Full perf/PMU access -- the profile harness collects branch
//       miss + cache miss + cycle counters that Nitro filters on
//       virt instances.
//     - Zero noisy-neighbour jitter -- the A/B sweep against
//       Category 2 kernel tunings runs against an exclusively-ours
//       host, which means smaller error bars and faster convergence.
//
//   S3 ResultsBucket  (RETAIN on stack delete; lifecycle -> Glacier @ 30d)
//
//   SSM Documents:
//     PulsysProfile  -- profile_baseline.sh + upload artifact
//     PulsysSweep    -- sweep_tunings.sh   + upload artifact
//     PulsysBench    -- bench_saturate.sh  + upload matrix.csv (no DingoSpeed on EC2)
//
// Outputs (consumed by scripts/ssm-*.sh):
//
//   InstanceId          -- target for ssm send-command / start-session
//   ResultsBucket       -- target for `aws s3 cp s3://...`
//   AmiKind, AmiId      -- visible provenance
//   StartShellCmd       -- copy/paste to open an SSM shell
//   RunProfileCmd       -- copy/paste to run the profile document
//   RunSweepCmd         -- copy/paste to run the sweep document
//   RunBenchCmd         -- copy/paste to run the bench document
import * as cdk from 'aws-cdk-lib';
import { Construct } from 'constructs';
import * as ec2 from 'aws-cdk-lib/aws-ec2';
import * as iam from 'aws-cdk-lib/aws-iam';
import * as s3 from 'aws-cdk-lib/aws-s3';
import * as ssm from 'aws-cdk-lib/aws-ssm';
import { BenchDocuments } from './bench-docs';

export interface BenchStackProps extends cdk.StackProps {
  /** "stock" picks /pulsys/stock-ami/latest, "tuned" picks /pulsys/bench-ami/latest */
  readonly amiKind: 'stock' | 'tuned';
  /**
   * EC2 instance type.  Default is c7i.12xlarge (48 vCPU), a standard
   * On-Demand Nitro instance that runs the io_uring warm path on AL2023.
   * Use c7i.24xlarge (96 vCPU) for more cores, or m5zn.metal (48) /
   * c7i.metal-24xl (96) for bare metal when quota allows and you want the
   * absolute ceiling with no hypervisor between the ring and the silicon.
   */
  readonly instanceType: string;
}

export class BenchStack extends cdk.Stack {
  constructor(scope: Construct, id: string, props: BenchStackProps) {
    super(scope, id, props);

    // ---------------------------------------------------------------
    // 1. VPC + security group
    // ---------------------------------------------------------------
    // Use the account's default VPC (must exist in the deploy region).
    // Instance is placed in a public subnet so it can reach SSM and S3
    // without a NAT.  No inbound rules on the SG; access is SSM only.
    const vpc = ec2.Vpc.fromLookup(this, 'DefaultVpc', {
      isDefault: true,
    });

    const sg = new ec2.SecurityGroup(this, 'BenchSg', {
      vpc,
      description: 'pulsys bench: SSM-only access (NO inbound), outbound all',
      allowAllOutbound: true,
    });
    // Deliberately NO addIngressRule calls.  Access is exclusively via
    // SSM Session Manager + ssm send-command.

    // ---------------------------------------------------------------
    // 2. IAM role for the instance
    // ---------------------------------------------------------------
    const role = new iam.Role(this, 'BenchRole', {
      assumedBy: new iam.ServicePrincipal('ec2.amazonaws.com'),
      managedPolicies: [
        iam.ManagedPolicy.fromAwsManagedPolicyName('AmazonSSMManagedInstanceCore'),
        // CloudWatch logs for SSM session transcripts is nice for audit.
        iam.ManagedPolicy.fromAwsManagedPolicyName('CloudWatchAgentServerPolicy'),
      ],
    });

    // ---------------------------------------------------------------
    // 3. Results bucket
    // ---------------------------------------------------------------
    const results = new s3.Bucket(this, 'ResultsBucket', {
      blockPublicAccess: s3.BlockPublicAccess.BLOCK_ALL,
      encryption: s3.BucketEncryption.S3_MANAGED,
      versioned: false,
      // RETAIN keeps history of every run; cdk destroy preserves results.
      removalPolicy: cdk.RemovalPolicy.RETAIN,
      enforceSSL: true,
      lifecycleRules: [
        {
          id: 'tier-to-glacier-30d',
          enabled: true,
          transitions: [
            {
              storageClass: s3.StorageClass.GLACIER_INSTANT_RETRIEVAL,
              transitionAfter: cdk.Duration.days(30),
            },
            {
              storageClass: s3.StorageClass.DEEP_ARCHIVE,
              transitionAfter: cdk.Duration.days(180),
            },
          ],
          // Multipart aborts >7d should never happen here but be cheap.
          abortIncompleteMultipartUploadAfter: cdk.Duration.days(7),
        },
      ],
    });
    results.grantReadWrite(role);

    // ---------------------------------------------------------------
    // 4. AMI lookup (via SSM parameter, set by Packer wrappers)
    // ---------------------------------------------------------------
    const paramName =
      props.amiKind === 'tuned'
        ? '/pulsys/bench-ami/latest'
        : '/pulsys/stock-ami/latest';
    const amiId = ssm.StringParameter.valueForStringParameter(this, paramName);
    const machineImage = ec2.MachineImage.genericLinux({
      [this.region]: amiId,
    });

    // ---------------------------------------------------------------
    // 5. Launch template + EC2 instance
    // ---------------------------------------------------------------
    // gp3 iops/throughput only apply on a Launch Template (not ec2.Instance
    // blockDevices directly — see aws-cdk issue #34033).
    const benchUserData = ec2.UserData.custom(`#!/bin/bash
set -euo pipefail
DEV=""
for d in /dev/nvme1n1 /dev/xvdf /dev/sdf; do
  [ -b "$d" ] && DEV="$d" && break
done
if [ -z "$DEV" ]; then exit 0; fi
MOUNT=/mnt/hf-data
mkdir -p "$MOUNT"
if ! mountpoint -q "$MOUNT"; then
  if ! blkid "$DEV" | grep -q xfs; then mkfs.xfs -f "$DEV"; fi
  mount "$DEV" "$MOUNT"
  grep -q hf-data /etc/fstab || echo "$DEV $MOUNT xfs defaults,nofail 0 2" >> /etc/fstab
fi
`);

    const launchTemplate = new ec2.LaunchTemplate(this, 'BenchLaunchTemplate', {
      instanceType: new ec2.InstanceType(props.instanceType),
      machineImage,
      role,
      securityGroup: sg,
      userData: benchUserData,
      blockDevices: [
        {
          deviceName: '/dev/xvda',
          volume: ec2.BlockDeviceVolume.ebs(200, {
            volumeType: ec2.EbsDeviceVolumeType.GP3,
            deleteOnTermination: true,
          }),
        },
        {
          deviceName: '/dev/sdf',
          // 500 GiB gp3 data volume @ max provisioned throughput (1000 MiB/s)
          // and high IOPS for parallel warm range reads.  Sequential 63 GiB
          // cold-fill is throughput-bound (~1 GB/s cap), not IOPS-bound.
          volume: ec2.BlockDeviceVolume.ebs(500, {
            volumeType: ec2.EbsDeviceVolumeType.GP3,
            deleteOnTermination: true,
            iops: 16000,
            throughput: 1000,
          }),
        },
      ],
    });

    const subnetId = vpc.selectSubnets({
      subnetType: ec2.SubnetType.PUBLIC,
    }).subnetIds[0];

    const instance = new ec2.CfnInstance(this, 'BenchInstance', {
      launchTemplate: {
        launchTemplateId: launchTemplate.launchTemplateId,
        version: launchTemplate.latestVersionNumber,
      },
      subnetId,
    });
    const instanceId = instance.ref;

    // SSM Session Manager preferences for richer shells.  Optional;
    // helpful when you eventually want logs of every interactive
    // session sent to S3 or CloudWatch.

    // ---------------------------------------------------------------
    // 6. SSM documents
    // ---------------------------------------------------------------
    const docs = new BenchDocuments(this, 'BenchDocs', {
      resultsBucketName: results.bucketName,
      region: this.region,
    });

    // ---------------------------------------------------------------
    // 7. Outputs
    // ---------------------------------------------------------------
    // Construct IDs for outputs are suffixed with "Out" to avoid colliding
    // with the construct IDs of the resources they describe (e.g. the
    // ResultsBucket s3.Bucket construct uses ID "ResultsBucket").
    new cdk.CfnOutput(this, 'VpcIdOut', {
      value: vpc.vpcId,
      description: 'Default VPC used for the bench instance',
    });
    new cdk.CfnOutput(this, 'InstanceIdOut', {
      value: instanceId,
      description: 'Target EC2 instance for SSM send-command / start-session',
      exportName: `${this.stackName}-InstanceId`,
    });
    new cdk.CfnOutput(this, 'ResultsBucketOut', {
      value: results.bucketName,
      description: 'S3 bucket where profile/sweep/bench artifacts land',
      exportName: `${this.stackName}-ResultsBucket`,
    });
    new cdk.CfnOutput(this, 'AmiKindOut', { value: props.amiKind });
    new cdk.CfnOutput(this, 'AmiIdOut', { value: amiId });
    new cdk.CfnOutput(this, 'StartShellCmdOut', {
      value: `aws ssm start-session --region ${this.region} --target ${instanceId}`,
    });
    new cdk.CfnOutput(this, 'RunProfileCmdOut', {
      value: `aws ssm send-command --region ${this.region} --document-name ${docs.profileDocName} --instance-ids ${instanceId}`,
    });
    new cdk.CfnOutput(this, 'RunSweepCmdOut', {
      value: `aws ssm send-command --region ${this.region} --document-name ${docs.sweepDocName} --instance-ids ${instanceId}`,
    });
    new cdk.CfnOutput(this, 'RunBenchCmdOut', {
      value: `aws ssm send-command --region ${this.region} --document-name ${docs.benchDocName} --instance-ids ${instanceId}`,
    });
  }
}

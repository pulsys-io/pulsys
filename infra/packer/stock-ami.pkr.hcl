# stock-ami.pkr.hcl -- pulsys benchmark BASELINE AMI (Track B0).
#
# Bakes Amazon Linux 2023 + Category 1 (ceiling-lifting) sysctls + all build
# and benchmarking dependencies.  Used for two purposes:
#
#   1. As the host on which scripts/sweep_tunings.sh runs to A/B every
#      Category 2 (behavioral) tuning we are considering for the production
#      AMI.  Output: tmp/bench/tunings-report.md.
#
#   2. As the honest baseline column in the published EC2 benchmark.  This
#      is what "stock AL2023 with reasonable ceiling settings" looks like
#      so the gains attributable to Category 2 tunings and to io_uring are
#      not muddled with one another.
#
# Build:
#   scripts/build-stock-ami.sh           # uses defaults
#   AWS_REGION=us-west-2 scripts/build-stock-ami.sh
#
# The wrapper produces infra/packer/.tarball/pulsys.tar.gz, runs `packer
# build`, then publishes the resulting AMI ID to SSM parameter
# /pulsys/stock-ami/latest in the chosen region (consumed by CDK in
# Track C).  No SSH key remains on the resulting image; ingress to running
# instances is SSM-only.

packer {
  required_plugins {
    amazon = {
      source  = "github.com/hashicorp/amazon"
      version = ">= 1.3.0"
    }
  }
}

variable "region" {
  type        = string
  default     = "us-east-1"
  description = "AWS region to build the AMI in (must match where you intend to launch via CDK)."
}

variable "instance_type" {
  type        = string
  default     = "c7i.large"
  description = "Build-time instance.  Smaller is fine; the AMI itself is launched on a larger instance for benchmarks."
}

variable "ami_version" {
  type        = string
  default     = "v0.1.0"
  description = "Version tag baked into the AMI name and tags.  Bump on intentional re-builds."
}

variable "git_commit" {
  type        = string
  default     = "unknown"
  description = "Commit SHA of the source tarball.  Recorded in AMI tags for provenance."
}

variable "source_tarball" {
  type        = string
  description = "Local path to a gzipped tarball of the repo (produced by scripts/build-stock-ami.sh)."
}

variable "subnet_id" {
  type        = string
  default     = ""
  description = "Optional subnet ID for the build instance.  Leave empty to use the default VPC's default subnet."
}

variable "vpc_id" {
  type        = string
  default     = ""
  description = "Optional VPC ID.  Leave empty to use the account's default VPC."
}

# Latest Amazon Linux 2023 x86_64 published by Amazon.  We pin nothing -- we
# always build on top of the most recent AL2023 so security patches are
# inherited.
data "amazon-ami" "al2023" {
  filters = {
    name                = "al2023-ami-2023*-x86_64"
    root-device-type    = "ebs"
    virtualization-type = "hvm"
    state               = "available"
  }
  most_recent = true
  owners      = ["amazon"]
  region      = var.region
}

source "amazon-ebs" "stock" {
  region                      = var.region
  ami_name                    = "pulsys-stock-${var.ami_version}-{{timestamp}}"
  ami_description             = "pulsys benchmark baseline AMI (AL2023 + Category 1 sysctls only).  No behavioural tunings.  Used as sweep host and baseline column."
  instance_type               = var.instance_type
  source_ami                  = data.amazon-ami.al2023.id
  ssh_username                = "ec2-user"
  associate_public_ip_address = true

  # Use an ephemeral ed25519 keypair generated per-build; Packer discards
  # the key after the build.  This means SSH cannot be used to access the
  # resulting AMI -- access is SSM-only, by design.
  temporary_key_pair_type = "ed25519"

  # Restrict the temporary build-time security group to this host's public
  # IP only (Packer auto-detects).  No 0.0.0.0/0 ingress.
  temporary_security_group_source_public_ip = true

  subnet_id = var.subnet_id
  vpc_id    = var.vpc_id

  # Larger root volume for build artifacts; the running instance will mount
  # an instance-store NVMe at /var/lib/pulsys/cache via CDK user-data, so
  # this root volume only needs space for binaries + tools + logs.
  launch_block_device_mappings {
    device_name           = "/dev/xvda"
    volume_size           = 30
    volume_type           = "gp3"
    delete_on_termination = true
  }

  tags = {
    Name        = "pulsys-stock"
    Version     = var.ami_version
    Commit      = var.git_commit
    Stock       = "true"
    Tuned       = "false"
    PackerBuild = "{{timestamp}}"
  }
}

build {
  name    = "pulsys-stock"
  sources = ["source.amazon-ebs.stock"]

  # Upload the repo source tarball.  Produced by the wrapper script via
  # `git archive` so only tracked files end up on the AMI.
  provisioner "file" {
    source      = "${var.source_tarball}"
    destination = "/tmp/pulsys.tar.gz"
  }

  # Tuning + systemd file artifacts.  Path is relative to this .pkr.hcl
  # file's directory via Packer's path.root.
  provisioner "file" {
    source      = "${path.root}/files/sysctl-pulsys-category1.conf"
    destination = "/tmp/sysctl-pulsys-category1.conf"
  }
  provisioner "file" {
    source      = "${path.root}/files/limits-pulsys.conf"
    destination = "/tmp/limits-pulsys.conf"
  }
  provisioner "file" {
    source      = "${path.root}/files/pulsys.service"
    destination = "/tmp/pulsys.service"
  }

  # Provisioning scripts.
  provisioner "file" {
    source      = "${path.root}/scripts/common-provision.sh"
    destination = "/tmp/common-provision.sh"
  }
  provisioner "file" {
    source      = "${path.root}/scripts/category1-tunings.sh"
    destination = "/tmp/category1-tunings.sh"
  }

  # Run provisioning under sudo.
  provisioner "shell" {
    execute_command = "sudo -E bash '{{ .Path }}'"
    inline = [
      "set -euxo pipefail",
      "bash /tmp/common-provision.sh",
      "bash /tmp/category1-tunings.sh",
      "bash -c 'echo \"=== final kernel check ===\"; uname -r; major=$(uname -r | cut -d. -f1); minor=$(uname -r | cut -d. -f2); if [ \"$major\" -lt 6 ] || { [ \"$major\" -eq 6 ] && [ \"$minor\" -lt 1 ]; }; then echo \"FATAL: kernel < 6.1, io_uring DEFER_TASKRUN unavailable\" >&2; exit 1; fi'",
    ]
  }

  post-processor "manifest" {
    output = "${path.root}/manifest.json"
  }
}

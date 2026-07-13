# bench-ami.pkr.hcl -- pulsys benchmark TUNED AMI (Track B2).
#
# Identical to stock-ami.pkr.hcl except it ALSO applies a curated set of
# Category 2 (behavioural) kernel tunings from
# `files/sysctl-pulsys-category2.conf` and `scripts/category2-tunings.sh`.
#
# Source of truth: those two files are populated BY HAND from the survivors
# in the generated sweep report (tmp/bench/tunings-report.md, produced by
# `scripts/sweep_tunings.sh`).  Do not invent tunings here; if a knob is
# not in the report's survivor table, it does not ship.
#
# Build:
#   scripts/build-tuned-ami.sh
#
# Output: AMI ID written to SSM /pulsys/bench-ami/latest and to a
# commit-tagged parameter /pulsys/bench-ami/<commit-sha>.

packer {
  required_plugins {
    amazon = {
      source  = "github.com/hashicorp/amazon"
      version = ">= 1.3.0"
    }
  }
}

variable "region"          { type = string  default = "us-east-1" }
variable "instance_type"   { type = string  default = "c7i.large" }
variable "ami_version"     { type = string  default = "v0.1.0" }
variable "git_commit"      { type = string  default = "unknown" }
variable "source_tarball"  { type = string }
variable "subnet_id"       { type = string  default = "" }
variable "vpc_id"          { type = string  default = "" }
# Path to the sweep report that justifies the tunings in
# files/sysctl-pulsys-category2.conf.  Baked onto the AMI for provenance.
# This is a generated artifact (scripts/ssm-sweep.sh writes it to
# tmp/bench/tunings-report.md); build-tuned-ami.sh requires it to exist
# and passes it via -var. The tuning rationale lives in docs/internals.md.
variable "tunings_report" {
  type    = string
  default = "../../tmp/bench/tunings-report.md"
}

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

source "amazon-ebs" "bench" {
  region                                    = var.region
  ami_name                                  = "pulsys-bench-${var.ami_version}-{{timestamp}}"
  ami_description                           = "pulsys benchmark tuned AMI: AL2023 + Category 1 + Category 2 sweep survivors.  See /etc/pulsys/tuning.md on the running instance for provenance."
  instance_type                             = var.instance_type
  source_ami                                = data.amazon-ami.al2023.id
  ssh_username                              = "ec2-user"
  associate_public_ip_address               = true
  temporary_key_pair_type                   = "ed25519"
  temporary_security_group_source_public_ip = true
  subnet_id                                 = var.subnet_id
  vpc_id                                    = var.vpc_id

  launch_block_device_mappings {
    device_name           = "/dev/xvda"
    volume_size           = 30
    volume_type           = "gp3"
    delete_on_termination = true
  }

  tags = {
    Name        = "pulsys-bench"
    Version     = var.ami_version
    Commit      = var.git_commit
    Stock       = "false"
    Tuned       = "true"
    PackerBuild = "{{timestamp}}"
  }
}

build {
  name    = "pulsys-bench"
  sources = ["source.amazon-ebs.bench"]

  # Source tarball + shared artifacts (same as stock).
  provisioner "file" {
    source      = "${var.source_tarball}"
    destination = "/tmp/pulsys.tar.gz"
  }
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
  provisioner "file" {
    source      = "${path.root}/scripts/common-provision.sh"
    destination = "/tmp/common-provision.sh"
  }
  provisioner "file" {
    source      = "${path.root}/scripts/category1-tunings.sh"
    destination = "/tmp/category1-tunings.sh"
  }

  # Category 2 additions (only on this template).
  provisioner "file" {
    source      = "${path.root}/files/sysctl-pulsys-category2.conf"
    destination = "/tmp/sysctl-pulsys-category2.conf"
  }
  provisioner "file" {
    source      = "${path.root}/scripts/category2-tunings.sh"
    destination = "/tmp/category2-tunings.sh"
  }

  # Bake the sweep report onto the AMI for provenance.  Path is resolved
  # by the build wrapper (`scripts/build-tuned-ami.sh`); the default
  # points at the generated tmp/bench/tunings-report.md.
  provisioner "file" {
    source      = "${var.tunings_report}"
    destination = "/tmp/tunings-report.md"
  }

  provisioner "shell" {
    execute_command = "sudo -E bash '{{ .Path }}'"
    inline = [
      "set -euxo pipefail",
      "bash /tmp/common-provision.sh",
      "bash /tmp/category1-tunings.sh",
      "bash /tmp/category2-tunings.sh",
      "install -m 0644 /tmp/tunings-report.md /etc/pulsys/tunings-report.md",
      "bash -c 'echo \"=== final kernel check ===\"; uname -r; major=$(uname -r | cut -d. -f1); minor=$(uname -r | cut -d. -f2); if [ \"$major\" -lt 6 ] || { [ \"$major\" -eq 6 ] && [ \"$minor\" -lt 1 ]; }; then echo \"FATAL: kernel < 6.1\" >&2; exit 1; fi'",
    ]
  }

  post-processor "manifest" {
    output = "${path.root}/manifest-bench.json"
  }
}

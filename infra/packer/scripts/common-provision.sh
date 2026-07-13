#!/usr/bin/env bash
# common-provision.sh -- shared provisioning for stock and tuned AMIs.
#
# Runs as root via Packer's shell provisioner.  Installs build deps, fetches
# Go from upstream, builds pulsys + bench binaries, clones + builds
# DingoSpeed for head-to-head comparisons, drops a systemd unit, and
# disables interactive SSH so the resulting AMI is SSM-only.
#
# Idempotent enough for re-runs during development; in CI this runs exactly
# once per AMI build.
set -euxo pipefail

# ----- pinned versions ------------------------------------------------------
# Bump intentionally; keep in lockstep with go.mod's go directive.
GO_VERSION="${GO_VERSION:-1.25.0}"
# Caddy GA, used in the bench matrix as a "kernel-floor" reference.
CADDY_VERSION="${CADDY_VERSION:-2.10.0}"
# DingoSpeed ref to compare against.  Pin to a SHA for reproducibility once
# we have benchmark numbers we want to lock in.
DINGOSPEED_REF="${DINGOSPEED_REF:-main}"
# DingoSpeed pulls github.com/bytedance/sonic, which lags new Go releases.
# pulsys builds with GO_VERSION (1.25); compile DingoSpeed with 1.24.x.
DINGO_GO_TOOLCHAIN="${DINGO_GO_TOOLCHAIN:-go1.24.5}"
# Set SKIP_DINGOSPEED_BUILD=1 to finish the AMI without head-to-head binary
# ssm-bench (the only EC2 bench) doesn't need DingoSpeed.
SKIP_DINGOSPEED_BUILD="${SKIP_DINGOSPEED_BUILD:-0}"

REPO_TARBALL=/tmp/pulsys.tar.gz
REPO_DIR=/opt/pulsys-src
BUILD_OUT=/usr/local/bin

# ----- 1. base packages -----------------------------------------------------
# `dnf update` first so we layer on top of fully-patched AL2023.
dnf -y update
# AL2023 base image ships curl-minimal; do not install the full "curl"
# package — it conflicts with curl-minimal during dnf update/install.
dnf -y install \
    gcc make git jq tar gzip \
    sysstat procps-ng ca-certificates \
    nginx \
    openssl-devel zlib-devel \
    perf bpftrace \
    numactl numactl-libs hwloc hwloc-libs \
    awscli-2

# AL2023 ships amazon-ssm-agent enabled by default; the explicit enable is
# belt-and-suspenders against a future image flipping the default.
systemctl enable --now amazon-ssm-agent || true

# ----- 2. install Go from upstream -----------------------------------------
# AL2023's golang package lags behind go.mod; install upstream pinned
# version into /usr/local/go.  PATH update lives in /etc/profile.d so SSM
# shell sessions also see it.
curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o /tmp/go.tar.gz
rm -rf /usr/local/go
tar -C /usr/local -xzf /tmp/go.tar.gz
rm -f /tmp/go.tar.gz
ln -sfn /usr/local/go/bin/go    /usr/local/bin/go
ln -sfn /usr/local/go/bin/gofmt /usr/local/bin/gofmt

export PATH=/usr/local/go/bin:$PATH
export GOPATH=/opt/go
mkdir -p "$GOPATH"
cat >/etc/profile.d/pulsys-go.sh <<'EOF'
export PATH=/usr/local/go/bin:/opt/go/bin:$PATH
export GOPATH=/opt/go
EOF
chmod 0644 /etc/profile.d/pulsys-go.sh

# ----- 3. install wrk (HTTP bench client) ----------------------------------
# wrk has no AL2023 RPM; we build the small C source.  About 3 seconds on
# a c7i.large.  Static enough that the resulting binary is portable across
# AL2023 patch levels.
git clone --depth 1 https://github.com/wg/wrk /tmp/wrk
make -C /tmp/wrk -j"$(nproc)"
install -m 0755 /tmp/wrk/wrk /usr/local/bin/wrk
rm -rf /tmp/wrk

# ----- 4. install caddy (reference HTTP server) ----------------------------
# Used as a kernel-floor data point in the bench matrix.
curl -fsSL \
    "https://github.com/caddyserver/caddy/releases/download/v${CADDY_VERSION}/caddy_${CADDY_VERSION}_linux_amd64.tar.gz" \
    -o /tmp/caddy.tar.gz
tar -C /tmp -xzf /tmp/caddy.tar.gz caddy
install -m 0755 /tmp/caddy /usr/local/bin/caddy
rm -f /tmp/caddy.tar.gz /tmp/caddy

# ----- 5. official Python hf CLI (huggingface_hub) + Rust hf_transfer -------
# The download client for e2e / hf_download_bench benchmarks.
dnf install -y python3-pip
pip3 install -q 'huggingface_hub[cli]' hf_transfer
python3 -m huggingface_hub.cli.hf version

# ----- 6. install FlameGraph + perf helpers --------------------------------
git clone --depth 1 https://github.com/brendangregg/FlameGraph /opt/FlameGraph
ln -sfn /opt/FlameGraph/flamegraph.pl         /usr/local/bin/flamegraph.pl
ln -sfn /opt/FlameGraph/stackcollapse-perf.pl /usr/local/bin/stackcollapse-perf.pl

# ----- 7. extract source + build pulsys + bench helpers ------------------
rm -rf "$REPO_DIR"
mkdir -p "$REPO_DIR"
tar -C "$REPO_DIR" -xzf "$REPO_TARBALL"
chown -R root:root "$REPO_DIR"

cd "$REPO_DIR"
go mod download

# -trimpath strips $GOPATH from stack traces (cleaner pprof output).
# -ldflags '-s -w' drops the symbol table; we keep separate dbg builds on
# the laptop, the AMI binary is the release-shape one.
GOFLAGS=(-trimpath -ldflags='-s -w')
go build "${GOFLAGS[@]}" -o "$BUILD_OUT/pulsys"         ./cmd/pulsys
go build "${GOFLAGS[@]}" -o "$BUILD_OUT/fake-hf"          ./cmd/fake-hf
go build "${GOFLAGS[@]}" -o "$BUILD_OUT/bench-coreserver" ./cmd/bench-coreserver
go build "${GOFLAGS[@]}" -o "$BUILD_OUT/bench-nethttp"    ./cmd/bench-nethttp
go build "${GOFLAGS[@]}" -o "$BUILD_OUT/bench-ttfb"       ./cmd/bench-ttfb

# Also keep a debug build (with symbols) for pprof / `go tool pprof http://...`.
# Drop it next to the release binary so the bench harness can swap them.
mkdir -p /usr/local/lib/pulsys
go build -trimpath -o /usr/local/lib/pulsys/pulsys-dbg ./cmd/pulsys

# ----- 8. clone + build DingoSpeed -----------------------------------------
DINGO_DIR=/opt/dingospeed
if [ "$SKIP_DINGOSPEED_BUILD" = "1" ]; then
	echo "==> SKIP_DINGOSPEED_BUILD=1: omitting DingoSpeed (saturate-only AMI)" >&2
else
	git clone --depth 1 -b "$DINGOSPEED_REF" https://github.com/dingodb/dingospeed "$DINGO_DIR"
	export GOTOOLCHAIN="$DINGO_GO_TOOLCHAIN"
	GOBIN="$GOPATH/bin" go install github.com/google/wire/cmd/wire@latest
	export PATH="$GOPATH/bin:$PATH"
	cd "$DINGO_DIR"
	make init || true
	make wire
	make build
	unset GOTOOLCHAIN
	mkdir -p "$DINGO_DIR/config"
	cp "$REPO_DIR/scripts/dingospeed-config.yaml" "$DINGO_DIR/config/config.yaml"
fi

# Wire the AMI layout into the paths scripts/bench_matrix.sh + the existing
# bench harness expect.  The scripts assume a /opt/pulsys-src/tmp/bench/
# tree containing both binaries and a DingoSpeed checkout.  We point those
# at the canonical install locations so re-runs do not rebuild things and
# so `bash scripts/bench_matrix.sh` works as-is.
mkdir -p "$REPO_DIR/tmp/bench/bin"
ln -sfn /usr/local/bin/pulsys         "$REPO_DIR/tmp/bench/bin/pulsys"
ln -sfn /usr/local/bin/fake-hf          "$REPO_DIR/tmp/bench/bin/fake-hf"
ln -sfn /usr/local/bin/bench-coreserver "$REPO_DIR/tmp/bench/bin/bench-coreserver"
ln -sfn /usr/local/bin/bench-nethttp    "$REPO_DIR/tmp/bench/bin/bench-nethttp"
ln -sfn /usr/local/bin/bench-ttfb       "$REPO_DIR/tmp/bench/bin/bench-ttfb"
if [ -d "$DINGO_DIR" ] && [ -x "$DINGO_DIR/bin/dingospeed" ]; then
	ln -sfn /opt/dingospeed "$REPO_DIR/tmp/bench/dingospeed"
fi

# ----- 8. service user + state dirs ----------------------------------------
useradd --system --home /var/lib/pulsys --shell /sbin/nologin pulsys || true
mkdir -p /var/lib/pulsys /var/log/pulsys /etc/pulsys
chown -R pulsys:pulsys /var/lib/pulsys /var/log/pulsys

# Default env file.  Overridden at bench-time by the SSM document, which
# writes a fresh env file before starting the proxy.
cat >/etc/pulsys/pulsys.env <<'EOF'
PULSYS_LISTEN=0.0.0.0:18687
PULSYS_CACHE_DIR=/var/lib/pulsys/cache
PULSYS_UPSTREAM=https://huggingface.co
EOF
chmod 0644 /etc/pulsys/pulsys.env

# ----- 9. systemd unit -----------------------------------------------------
# Disabled by default.  Enable from CDK user-data or SSM document when
# running as a sidecar instead of a bench target.
install -m 0644 /tmp/pulsys.service /etc/systemd/system/pulsys.service
systemctl daemon-reload

# ----- 10. disable interactive SSH -----------------------------------------
# Packer still needs sshd for the duration of the build; the cloud-init
# config below ensures the ephemeral keypair Packer used is wiped on
# first boot.  Combined with CDK launching the AMI WITHOUT a keypair,
# this gives effective SSM-only access on production launches.
mkdir -p /etc/cloud/cloud.cfg.d
cat >/etc/cloud/cloud.cfg.d/99-pulsys-no-ssh-keys.cfg <<'EOF'
# Wipe any baked-in authorized_keys at first boot.  CDK does not associate
# a keypair, so this leaves the host SSH-keyless and reachable only via SSM.
runcmd:
  - [ rm, -f, /root/.ssh/authorized_keys ]
  - [ rm, -f, /home/ec2-user/.ssh/authorized_keys ]
EOF

# ----- 11. clean ----------------------------------------------------------
dnf clean all
rm -rf /var/cache/dnf "$REPO_TARBALL"
# Note: /tmp/sysctl-* and /tmp/limits-* are left for category1-tunings.sh
# (the next provisioner step) to install.

echo "==> common-provision complete"

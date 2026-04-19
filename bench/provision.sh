#!/usr/bin/env bash
# bench/provision.sh — provisions a fresh Ubuntu 24.04 GCE instance with
# everything tachyon needs to build + bench. Idempotent: safe to re-run.
#
# This script runs ON THE REMOTE VM (not on your laptop). Invoke it via:
#   bench/gcloud-up.sh        # wrapper that scp's + runs this
set -euxo pipefail

export DEBIAN_FRONTEND=noninteractive

# --- system packages ---------------------------------------------------------
sudo apt-get update
sudo apt-get install -y --no-install-recommends \
  build-essential git curl ca-certificates pkg-config \
  autoconf automake libtool \
  linux-tools-common "linux-tools-$(uname -r)" \
  bpftrace strace ltrace htop \
  libssl-dev zlib1g-dev \
  bombardier nghttp2-client jq

# --- Go (official tarball, matches host dev version) -------------------------
GO_VER=1.25.5
if ! go version 2>/dev/null | grep -q "go${GO_VER}"; then
  cd /tmp
  curl -fsSL "https://go.dev/dl/go${GO_VER}.linux-amd64.tar.gz" -o go.tgz
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf go.tgz
  sudo ln -sf /usr/local/go/bin/go /usr/local/bin/go
  sudo ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
  cd -
fi

# --- wrk2 from source (coordinated-omission-aware load gen) ------------------
if ! command -v wrk2 >/dev/null; then
  tmp=$(mktemp -d)
  git clone --depth=1 https://github.com/giltene/wrk2.git "$tmp/wrk2"
  make -C "$tmp/wrk2" -j"$(nproc)"
  sudo install -m755 "$tmp/wrk2/wrk" /usr/local/bin/wrk2
  rm -rf "$tmp"
fi

# --- sanity checks -----------------------------------------------------------
go version
wrk2 --version 2>&1 | head -1 || true   # wrk2 --version exits nonzero, that's fine
bombardier --version 2>&1 | head -1
command -v h2load
command -v perf
ls /usr/include/linux/io_uring.h

echo "provision OK"

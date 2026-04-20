#!/usr/bin/env bash
# bench/run-h3.sh — run HTTP/3 scenarios against the QUIC listener on :8443.
#
# h2load is the standard HTTP/3 load generator (ships in the nghttp3
# package); we use it because wrk2 has no QUIC support. The --alpn-list
# advertises h3 so negotiation picks our QUIC listener.
#
# Prereq: the proxy binary is running with a `quic { listen ":8443" }`
# block in its intent config, and a self-signed cert+key loaded by the
# same `tls { ... }` block. Use `make run-h3` or launch manually.
#
# Usage:
#   ./bench/run-h3.sh <proxy-name>
set -euo pipefail

proxy=${1:-tachyon-h3}
outdir="results/$(date +%F)/${proxy}"
mkdir -p "$outdir"

if ! command -v h2load >/dev/null; then
  echo "h2load not found — install nghttp3/nghttp2 CLI tools." >&2
  exit 1
fi

run_h2load() {
  local name=$1 conns=$2 streams=$3 total=$4 path=$5
  echo "== $name =="
  h2load --h1 \
    --npn-list h3 \
    -n "$total" -c "$conns" -m "$streams" \
    "https://localhost:8443$path" \
    2>&1 | tee "$outdir/${name}.txt"
}

# conns / streams-per-conn / total requests / path.
run_h2load h3-small    256  32  2000000 /
run_h2load h3-keep    1024  32  2000000 /
run_h2load h3-big       64   8   200000 /?size=65536

echo "wrote $outdir"

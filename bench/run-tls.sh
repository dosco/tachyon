#!/usr/bin/env bash
# bench/run-tls.sh — run scenarios against the TLS listener on :8443.
#
# Uses the same scenario set as run.sh so results are directly
# comparable. wrk2 and bombardier both support HTTPS; we pass -k /
# --insecure because the cert is self-signed. Skipping cert validation
# does not affect the crypto path (TLS 1.3 handshake + AES-GCM record
# processing are identical regardless of whether the cert is trusted).
#
# Usage:
#   ./bench/run-tls.sh <proxy-name>
set -euo pipefail

proxy=${1:-tachyon-tls}
outdir="results/$(date +%F)/${proxy}"
mkdir -p "$outdir"

run_wrk2() {
  local name=$1 conns=$2 dur=$3 rate=$4 path=$5
  echo "== $name =="
  wrk2 -t4 -c"$conns" -d"$dur" -R"$rate" --latency "https://localhost:8443$path" \
    | tee "$outdir/${name}.txt"
}

run_wrk2 tls-small   256  60s 100000 /
run_wrk2 tls-keep   1024  60s 100000 /
run_wrk2 tls-big      64  60s  20000 /?size=65536

echo "wrote $outdir"

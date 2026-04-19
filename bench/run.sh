#!/usr/bin/env bash
# bench/run.sh - run the defined scenarios against a running proxy.
#
# Usage:
#   ./bench/run.sh <proxy-name>      # e.g. tachyon, nginx, pingora
#
# Assumes:
#   - The proxy under test is already listening on :8080.
#   - ./origin is listening on :9000.
#   - wrk2 and bombardier are installed.
#
# Writes results to results/<date>/<proxy>/<scenario>.txt.
set -euo pipefail

proxy=${1:-tachyon}
outdir="results/$(date +%F)/${proxy}"
mkdir -p "$outdir"

run_wrk2() {
  local name=$1 conns=$2 dur=$3 rate=$4 path=$5
  echo "== $name =="
  wrk2 -t4 -c"$conns" -d"$dur" -R"$rate" --latency "http://localhost:8080$path" \
    | tee "$outdir/${name}.txt"
}

# Scenarios.
run_wrk2 plain-small 256  60s 100000 /
run_wrk2 plain-keep  1024 60s 100000 /
run_wrk2 plain-big    64  60s  20000 /?size=65536
run_wrk2 slow-client 2048 120s 10000 /

echo "wrote $outdir"

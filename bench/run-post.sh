#!/usr/bin/env bash
# bench/run-post.sh — POST-body scenarios against :8080.
#
# Uses wrk2 Lua scripts to inject a request body. Tests the proxy's
# request-body forwarding path (chunked or content-length pass-through)
# which is exercised by POST but not by GET.
#
# Usage:
#   ./bench/run-post.sh <proxy-name>
set -euo pipefail

proxy=${1:-tachyon}
outdir="results/$(date +%F)/${proxy}"
mkdir -p "$outdir"

run_wrk2_post() {
  local name=$1 conns=$2 dur=$3 rate=$4 script=$5
  echo "== $name =="
  wrk2 -t4 -c"$conns" -d"$dur" -R"$rate" --latency \
    -s "$script" \
    "http://localhost:8080/" \
    | tee "$outdir/${name}.txt"
}

# Small body: API-style POST, high concurrency, capped rate.
run_wrk2_post post-small-256  256 60s 50000 bench/post-small.lua
# Large body: streaming POST, low concurrency, lower rate.
run_wrk2_post post-large-64    64 60s  5000 bench/post-large.lua

echo "wrote $outdir"

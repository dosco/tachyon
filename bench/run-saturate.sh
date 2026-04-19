#!/usr/bin/env bash
# bench/run-saturate.sh - find the real ceiling.
#
# wrk2 at 1M target (unreachable) + bombardier unthrottled. Both are
# 30s to keep the total matrix under 5 min.
set -euo pipefail

proxy=${1:-tachyon}
outdir="results/$(date +%F)/${proxy}-sat"
mkdir -p "$outdir"

echo "== wrk2 saturate (256c, 30s, rate=1M) =="
wrk2 -t8 -c256 -d30s -R1000000 --latency http://localhost:8080/ \
  | tee "$outdir/sat-wrk2-256.txt"

echo "== wrk2 saturate (1024c, 30s, rate=1M) =="
wrk2 -t8 -c1024 -d30s -R1000000 --latency http://localhost:8080/ \
  | tee "$outdir/sat-wrk2-1024.txt"

echo "== bombardier (256c, 30s, unthrottled) =="
bombardier -c 256 -d 30s -l http://localhost:8080/ \
  | tee "$outdir/sat-bomb-256.txt"

echo "wrote $outdir"

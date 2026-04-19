#!/usr/bin/env bash
# bench/compare-post.sh — run all three proxies through POST scenarios.
#
# Usage:
#   bench/compare-post.sh                # tachyon nginx pingora
#   bench/compare-post.sh tachyon nginx  # specific proxies
set -euo pipefail

cd ~/tachyon

if [ $# -gt 0 ]; then
  proxies=("$@")
else
  proxies=(tachyon nginx pingora)
fi

pkill -f '^./origin' 2>/dev/null || true
sleep 0.3
./origin -addr :9000 -size 1024 > /tmp/origin-bench.log 2>&1 &
origin_pid=$!
trap "kill $origin_pid 2>/dev/null || true" EXIT

for _ in $(seq 1 30); do
  ss -tlnp 2>/dev/null | grep -q ':9000' && break
  sleep 0.1
done
echo "origin ready (pid $origin_pid)"
echo ""

for p in "${proxies[@]}"; do
  echo "════════════════════════════════════════"
  echo "  proxy: $p (POST)"
  echo "════════════════════════════════════════"
  bash bench/proxies/"$p".start
  bash bench/run-post.sh "$p"
  bash bench/proxies/"$p".stop
  sleep 1
  echo ""
done

echo "all done — results in results/$(date +%F)/"

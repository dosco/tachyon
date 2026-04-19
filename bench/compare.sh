#!/usr/bin/env bash
# bench/compare.sh — run every proxy through all bench scenarios in turn.
#
# Each proxy is managed by a pair of shim scripts:
#   bench/proxies/<name>.start  — starts the proxy on :8080, exits when ready
#   bench/proxies/<name>.stop   — kills it
#
# The origin server is started once and shared across all proxy runs.
#
# Usage:
#   bench/compare.sh                  # runs tachyon nginx pingora
#   bench/compare.sh tachyon nginx    # run specific proxies only
set -euo pipefail

cd ~/tachyon

if [ $# -gt 0 ]; then
  proxies=("$@")
else
  proxies=(tachyon nginx pingora)
fi

# Start the shared origin.
pkill -f '^./origin' 2>/dev/null || true
sleep 0.3
./origin -addr :9000 -size 1024 > /tmp/origin-bench.log 2>&1 &
origin_pid=$!
trap "kill $origin_pid 2>/dev/null || true" EXIT

# Wait for origin to be ready.
for _ in $(seq 1 30); do
  ss -tlnp 2>/dev/null | grep -q ':9000' && break
  sleep 0.1
done

echo "origin ready (pid $origin_pid)"
echo ""

for p in "${proxies[@]}"; do
  echo "════════════════════════════════════════"
  echo "  proxy: $p"
  echo "════════════════════════════════════════"
  bash bench/proxies/"$p".start
  bash bench/run.sh "$p"
  bash bench/proxies/"$p".stop
  sleep 1
  echo ""
done

echo "all done — results in results/$(date +%F)/"

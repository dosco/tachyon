#!/usr/bin/env bash
# bench/compare-tls.sh — run tachyon and nginx through TLS scenarios.
#
# Usage:
#   bench/compare-tls.sh                 # tachyon-tls nginx-tls
#   bench/compare-tls.sh tachyon-tls     # single proxy
set -euo pipefail

cd ~/tachyon

if [ $# -gt 0 ]; then
  proxies=("$@")
else
  proxies=(tachyon-tls nginx-tls)
fi

# Generate self-signed cert once for both proxies.
if [[ ! -f /tmp/bench-tls.crt ]]; then
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
    -keyout /tmp/bench-tls.key -out /tmp/bench-tls.crt \
    -days 365 -nodes -subj '/CN=bench' 2>/dev/null
  echo "generated P-256 self-signed cert at /tmp/bench-tls.{crt,key}"
fi

# Shared origin.
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
  echo "  proxy: $p (TLS)"
  echo "════════════════════════════════════════"
  bash bench/proxies/"$p".start
  bash bench/run-tls.sh "$p"
  bash bench/proxies/"$p".stop
  sleep 1
  echo ""
done

echo "all done — results in results/$(date +%F)/"

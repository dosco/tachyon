#!/usr/bin/env bash
# bench/matrix.sh — tachyon vs nginx vs Pingora, h2load headline matrix.
#
# Runs three proxy scenarios (small, keep, burst) for each proxy and prints
# RPS + latency stats. Writes full output to /tmp/bench.out.
#
# Usage:
#   bash bench/matrix.sh
set -uo pipefail
exec > >(tee /tmp/bench.out) 2>&1
cd ~/tachyon

pgrep -f '\./origin' >/dev/null \
  || (setsid nohup ./origin -addr :9000 -size 1024 </dev/null >/tmp/origin.log 2>&1 &)
sleep 0.5

cleanup() {
  pkill -9 -f h2load           2>/dev/null || true
  pkill -f pingora-bench-proxy 2>/dev/null || true
  pkill -f '^\./tachyon'       2>/dev/null || true
  sudo nginx -s quit           2>/dev/null || true
  sleep 1.2
}
wait_port() {
  for _ in $(seq 1 80); do ss -lnt 2>/dev/null | grep -q ":$1 " && return 0; sleep 0.1; done
  return 1
}

start_nginx()   { bash bench/proxies/nginx.start   >/tmp/p.log 2>&1 && wait_port 8080; }
start_pingora() { bash bench/proxies/pingora.start >/tmp/p.log 2>&1 & sleep 2; wait_port 8080; }
start_tach()    { setsid nohup ./tachyon -config config.yaml -workers "$(nproc)" </dev/null >/tmp/p.log 2>&1 & wait_port 8080; }

run_h2() {
  local label="$1"; shift
  printf '  %-28s ' "$label"
  out=$(timeout 60 h2load "$@" 2>&1)
  rps=$(echo  "$out" | awk '/finished in/ {for(i=1;i<=NF;i++)if($i=="req/s,")print $(i-1)}')
  min=$(echo  "$out" | awk '/time for request:/ {print $4}')
  max=$(echo  "$out" | awk '/time for request:/ {print $5}')
  mean=$(echo "$out" | awk '/time for request:/ {print $6}')
  sd=$(echo   "$out" | awk '/time for request:/ {print $7}')
  codes=$(echo "$out" | awk '/status codes:/ {print $3" 2xx "$5" 4xx "$7" 5xx"}')
  printf 'rps=%-11s min=%-7s mean=%-8s max=%-8s sd=%-8s %s\n' \
    "$rps" "$min" "$mean" "$max" "$sd" "$codes"
}

run_matrix() {
  local name="$1" fn="$2"
  echo; echo "[$name]"; cleanup
  $fn || { echo "  FAILED"; return; }
  run_h2 "small c=64  n=500k" -n 500000  -c 64  --h1 http://127.0.0.1:8080/
  run_h2 "keep  c=256 n=1M"   -n 1000000 -c 256 --h1 http://127.0.0.1:8080/
  run_h2 "burst c=512 n=1M"   -n 1000000 -c 512 --h1 http://127.0.0.1:8080/
}

echo "=== $(nproc) cpus, $(uname -r) ==="
run_matrix "nginx"   start_nginx
run_matrix "pingora" start_pingora
run_matrix "tachyon" start_tach
cleanup
echo ""
echo "full output saved to /tmp/bench.out"

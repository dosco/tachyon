#!/usr/bin/env bash
# bench/io-variants.sh — compare tachyon -io std vs -io uring variants.
#
# Starts two origin servers (1 KB and 64 KB bodies) and runs each io mode
# through the same four scenarios, reporting the median of three trials.
#
# Usage:
#   bash bench/io-variants.sh
set -uo pipefail
cd ~/tachyon
BIN=./tachyon

pkill -9 -f "tachyon" 2>/dev/null || true
pkill -9 -f 'origin'  2>/dev/null || true
sleep 1

setsid nohup ./origin -addr 127.0.0.1:9000 -size 1024  </dev/null >/tmp/origin_s.log 2>&1 &
setsid nohup ./origin -addr 127.0.0.1:9002 -size 65536 </dev/null >/tmp/origin_b.log 2>&1 &
for p in 9000 9002; do
  for _ in $(seq 1 30); do
    (exec 3<>/dev/tcp/127.0.0.1/$p) 2>/dev/null && { exec 3<&- 3>&-; break; }
    sleep 0.1
  done
done

cat > /tmp/io-variants.yaml <<EOF
listen: ":8080"
upstreams:
  small: { addrs: ["127.0.0.1:9000"], idle_per_host: 512 }
  big:   { addrs: ["127.0.0.1:9002"], idle_per_host: 512 }
routes:
  - { host: "*", path: "/big", upstream: "big" }
  - { host: "*", path: "/",    upstream: "small" }
EOF

stop_tach() {
  pkill -9 -f "tachyon" 2>/dev/null || true; sleep 1
  for _ in $(seq 1 30); do
    ss -lnt 2>/dev/null | grep -q ':8080 ' || return 0
    sleep 0.2
  done
}

start_tach() {
  # shellcheck disable=SC2086
  setsid nohup $BIN -config /tmp/io-variants.yaml $1 -workers "$(nproc)" </dev/null >/tmp/tach.log 2>&1 &
  for _ in $(seq 1 60); do ss -lnt | grep -q ':8080 ' && return 0; sleep 0.1; done
  return 1
}

median3() {
  local path=$1 n=$2 c=$3 r1 r2 r3
  r1=$(timeout 60 h2load -n "$n" -c "$c" -m 1 --h1 "http://127.0.0.1:8080$path" 2>&1 \
    | awk '/finished in/ {for(i=1;i<=NF;i++)if($i=="req/s,")print $(i-1)}')
  r2=$(timeout 60 h2load -n "$n" -c "$c" -m 1 --h1 "http://127.0.0.1:8080$path" 2>&1 \
    | awk '/finished in/ {for(i=1;i<=NF;i++)if($i=="req/s,")print $(i-1)}')
  r3=$(timeout 60 h2load -n "$n" -c "$c" -m 1 --h1 "http://127.0.0.1:8080$path" 2>&1 \
    | awk '/finished in/ {for(i=1;i<=NF;i++)if($i=="req/s,")print $(i-1)}')
  echo "$r1 $r2 $r3" | tr ' ' '\n' | sort -n | awk 'NR==2'
}

for cfg in \
  "stdlib                     :-io std" \
  "uring splice=default (16K) :-io uring" \
  "uring splice=1 (always)    :-io uring -splice-min=1" \
  "uring splice=off           :-io uring -splice-min=0"
do
  label="${cfg%%:*}"; flags="${cfg##*:}"
  echo "========= $label ========="
  stop_tach
  start_tach "$flags" || { echo "  START FAILED"; continue; }
  echo "  small c=64  n=500k  : $(median3 /    500000  64) req/s"
  echo "  keep  c=256 n=1M    : $(median3 /   1000000 256) req/s"
  echo "  burst c=512 n=1M    : $(median3 /   1000000 512) req/s"
  echo "  big   c=64  n=50k   : $(median3 /big  50000  64) req/s"
  stop_tach
done

pkill -9 -f 'origin' 2>/dev/null || true

#!/usr/bin/env bash
# bench/gc-cost.sh — measure GC overhead by comparing GOGC=100 vs GOGC=off.
#
# Runs each combination of io mode × GC setting and reports throughput
# and GC pause stats. gctrace output saved to /tmp/gctrace-<io>-<gogc>.log.
#
# Usage:
#   bash bench/gc-cost.sh
set -uo pipefail
cd ~/tachyon

./origin -addr 127.0.0.1:9000 -size 1024  >/tmp/origin-s.log 2>&1 &
./origin -addr 127.0.0.1:9002 -size 65536 >/tmp/origin-b.log 2>&1 &
trap 'pkill -9 -f origin 2>/dev/null || true' EXIT

for p in 9000 9002; do
  for _ in $(seq 1 30); do
    (exec 3<>/dev/tcp/127.0.0.1/$p) 2>/dev/null && { exec 3<&- 3>&-; break; }
    sleep 0.1
  done
done

for gogc in 100 off; do
  for io in std uring; do
    echo "=== GOGC=$gogc -io=$io ==="
    pkill -9 -f tachyon 2>/dev/null || true; sleep 1
    GODEBUG=gctrace=1 GOGC=$gogc ./tachyon -config config.yaml -io "$io" -workers 1 \
      >/tmp/gctrace-"$io"-"$gogc".log 2>&1 &
    sleep 1
    h2load -n 500000 -c 64 -m 1 --h1 http://127.0.0.1:8080/     | grep 'finished in'
    h2load -n  20000 -c 64 -m 1 --h1 http://127.0.0.1:8080/big  | grep 'finished in'
    pkill -9 -f tachyon
    echo "  GC count : $(grep -c '^gc ' /tmp/gctrace-"$io"-"$gogc".log)"
    echo "  last GC  : $(grep '^gc ' /tmp/gctrace-"$io"-"$gogc".log | tail -1)"
    echo ""
  done
done

# GC pause math: each gc line shows "A+B+C ms clock".
# A (sweep termination) and C (mark termination) are stop-the-world; B is not.

# Benchmark

The only meaningful statement of the form "tachyon beats Pingora" is one
that cites a specific harness. This file is that harness.

## Machine

- Linux x86_64, kernel 6.1+ (`IORING_REGISTER_PBUF_RING` needs 5.19,
  `IORING_OP_SEND_ZC` needs 6.0).
- 8c/16t bare-metal. Hetzner AX41 or equivalent. Hyper-threading on.
- Clock governor fixed at `performance`.
- tachyon worker processes pinned to physical cores 0-7; origin pinned to
  SMT siblings 8-15.

macOS or cloud VMs with noisy-neighbor CPU are not part of the test - numbers
vary wildly and the io_uring path doesn't exist on Darwin.

## Origin

`bench/origin` on port 9000 serving a fixed-size body (default 1 KiB) via
net/http keep-alive. It is deliberately faster than any proxy under test so
it never bottlenecks the measurement.

## Proxies

All pointed at `localhost:9000`, listening on `localhost:8080`:

- nginx 1.27, `worker_processes auto`, `access_log off`
- Pingora 0.4 (release + LTO), default config
- Envoy 1.30 (sanity floor)
- Traefik 3.1 (sanity floor)
- tachyon (PGO build: `go build -pgo=auto ./cmd/tachyon`; kTLS is on by default on Linux)

## Scenarios

Run with `bench/run.sh`:

| name        | conc | dur  | tls | h2  | body  |
|-------------|------|------|-----|-----|-------|
| plain-small | 256  | 60s  | off | off | 1 KiB |
| plain-keep  | 1024 | 60s  | off | off | 1 KiB |
| tls-small   | 256  | 60s  | on  | off | 1 KiB |
| h2-small    | 256  | 60s  | on  | on  | 1 KiB |
| plain-big   | 64   | 60s  | off | off | 64 KiB|
| slow-client | 2048 | 120s | off | off | rate-limited |

Use **wrk2** for anything where p99 matters - plain `wrk` is subject to
coordinated omission and will give overly optimistic tail numbers. Use
bombardier for throughput sanity, and h2load for H2 specifically.

## Metrics

Each run collects:

- RPS (tool-reported)
- p50/p90/p99/p99.9 from wrk2
- CPU% per worker via `pidstat -p <pid> 1`
- Syscall count via `perf stat -e syscalls:sys_enter_*`
- Context switches via `perf stat -e context-switches`
- Per-request mallocs via tachyon's own `runtime.ReadMemStats().Mallocs`
  delta; the steady-state target is **0 per request**.

## Passing the bar

"tachyon beats Pingora" is defined as all of:

- `plain-keep`:  RPS >= 110% Pingora, p99 <= 90% Pingora
- `h2-small`:    RPS >= 105% Pingora, p99 <= 100% Pingora
- `tls-small`:   RPS >= 100% Pingora (kTLS is on by default on Linux)
- all scenarios: p99.9 <= 3x p99 (no GC spike signature)

## Reproducing

```
# on the test box
cd tachyon
go build -o origin ./bench/origin
go build -o tachyon ./cmd/tachyon
./bench/run.sh              # writes results/<date>/matrix.csv
./bench/parse.py results/<date>/matrix.csv > results/<date>/summary.md
```

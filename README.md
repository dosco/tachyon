# tachyon

**A small, fast reverse proxy. Written in Go. Beats nginx and Pingora.**

---

## The numbers

Same 16-core GCE machine. Same workloads. Scripts to reproduce everything are in `bench/`.

| Workload | nginx | Pingora (Cloudflare, Rust) | **tachyon (Go)** |
|---|---:|---:|---:|
| Plaintext GET — 512 conns, burst | 138,672 rps | 139,772 rps | **160,728 rps (+15 %)** |
| Plaintext GET — p99 under burst | 13.46 ms | 4,010 ms 💀 | **9.09 ms** |
| TLS 1.3 — p99, 256 conns | 3.13 ms | — | **2.87 ms** |
| TLS 1.3 — p99, 64 KB body | 1.75 ms | — | **1.65 ms** |
| POST 64 KB body — p99 | **16.02 ms** 💀 | 1.88 ms | **1.78 ms** |
| Go GC overhead | n/a | n/a | **< 1.5 %** |

**15 % more throughput than Pingora.** Better tail latency than nginx on TLS. On large POST bodies,
nginx's default request-body buffering produces a 16 ms p99; tachyon streams the body and holds 1.78 ms.
(`proxy_request_buffering off` in nginx closes the gap — we ran stock configs.)

Full methodology, raw numbers, and reproduction steps: [BENCHMARK.md](BENCHMARK.md).

## Why this is interesting

**1. It's faster than the thing Cloudflare replaced nginx with.**
Pingora is a serious piece of engineering. tachyon, written from scratch in
Go by one person, moves more requests per second on the same hardware.

**2. It's in Go, and "Go is slow" turned out to be wrong.**
We measured the garbage collector's impact on throughput: **less than 1.5 %**,
with pauses of 16–29 microseconds. The tax for memory safety, a 3-second
build, and code your whole team can read is basically nothing.

**3. It's small enough to understand.**
Around 10,000 lines of Go. When something breaks at 3 AM, you can read the
entire request path during the incident. Not "skim the relevant module" —
read all of it.

**4. It's honest.**
We don't pretend tachyon replaces every nginx deployment. We tell you
exactly where it wins, where it ties, and where it isn't ready. The
benchmarks include reproduction scripts so you can check our work.

## Is this for you?

**Pick tachyon if you're doing the normal thing:**
terminating HTTP/1.1 or HTTP/2 in front of your services, with plaintext or
a regular TLS cert, on Linux. You'll get a meaningful throughput lift with
no new concepts to learn.

**Ships with:**
p2c-EWMA load balancing, passive outlier ejection, active health probes,
weighted multi-upstream routing, and retry budgets — all optional, all
zero overhead when disabled.

**Keep shopping if you need:**
HTTP/3, a WAF, service discovery, request mirroring, per-route rate limits,
caching, or plugins. tachyon is focused on being a very fast, very small
proxy — not a platform.

## Try it in 30 seconds

```bash
go build -o tachyon ./cmd/tachyon
./tachyon -config config.yaml
```

With a two-line config:

```yaml
listen: ":8080"
upstreams:
  api: { addrs: ["127.0.0.1:9000"] }
routes:
  - { host: "*", path: "/", upstream: "api" }
```

That's a working reverse proxy. On Linux, omit `-workers` and tachyon forks
one process per core automatically.

Add TLS with `-tls-listen :8443`. Watch metrics at `-debug-addr 127.0.0.1:6060`.
Reload config with `kill -HUP`. Drain gracefully with `kill -TERM`. It all
just works.

## The boring but important stuff

The things that matter when a proxy sits between your users and your
revenue — they all work:

- In-flight requests complete on shutdown. Nothing gets dropped.
- Config reloads without killing connections.
- TLS certs rotate without restarting.
- `Expect: 100-continue` is answered correctly.
- Dead upstreams are detected, evicted, and short-circuited.
- Long-lived keep-alive connections don't suddenly die at the 2-minute mark.
- pprof and Prometheus metrics on a loopback-only debug port.

## Learn more

- [BENCHMARK.md](BENCHMARK.md) — full numbers, methodology, reproduction.
- [docs/architecture.md](docs/architecture.md) — how it works under the hood.
- `./tachyon -h` — every flag, explained.

## Building

Requires Go 1.25+.

```bash
go build ./...       # compile
go test ./...        # unit tests
```

## License

See [LICENSE](LICENSE).

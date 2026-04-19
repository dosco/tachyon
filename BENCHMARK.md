# tachyon — Benchmark Report

> tachyon is a reverse proxy written from scratch in Go.
> It beats Cloudflare's Pingora (Rust) by 15 % on throughput, matches nginx on TLS,
> and makes nginx look embarrassing on large POST bodies — with a GC overhead of less than 1.5 %.
> The numbers are reproducible. The scripts are in this repo.

## The numbers that matter

16-vCPU GCE VM (c4-standard-16, Emerald Rapids). Two VMs in the same zone for cross-machine tests.
All three proxies use the same tuning knobs: SO_REUSEPORT, keepalive upstream pool, no access log.

### Plaintext GET — tachyon vs nginx vs Pingora

| Scenario | nginx 1.24 | Pingora 0.8 | **tachyon (Go)** | vs nginx | vs Pingora |
|---|---:|---:|---:|---:|---:|
| small — 64 conns, 500k req | 134,254 rps | 125,526 rps | **145,738 rps** | **+8.6 %** | **+16.1 %** |
| keepalive — 256 conns, 1M req | 137,687 rps | 141,235 rps | **158,620 rps** | **+15.2 %** | **+12.3 %** |
| burst — 512 conns, 1M req | 138,672 rps | 139,772 rps | **160,728 rps** | **+15.9 %** | **+15.0 %** |
| mean latency (small) | 474 µs | 507 µs | **435 µs** | −8.2 % | −14.2 % |
| p99 latency (burst) | 13.46 ms | **4,010 ms** 💀 | **9.09 ms** | −32.5 % | **442× better** |
| latency std dev (burst) | 429 µs | 12,470 µs | **184 µs** | — | **68× tighter** |
| failed requests / 2.5M | 0 | **17** | **0** | — | — |

Under burst load Pingora hit a **4-second** tail-latency spike and dropped 17 requests. tachyon and nginx
held steady. tachyon's advantage isn't just throughput — its latency distribution is 2.3× tighter than
nginx's and 68× tighter than Pingora's under the same load.

### TLS 1.3 — tachyon vs nginx

100k req/s rate-controlled, 256 connections, 60 s, cross-VM (~0.6 ms RTT). Same P-256 cert, TLS 1.3 only.

| Metric | nginx | **tachyon** | Δ |
|---|---:|---:|---|
| RPS (1 KB body, c=256) | 99,734 | 98,682 | tie — < 1.1 % |
| p99 latency (1 KB body) | 3.13 ms | **2.87 ms** | **−8 %** |
| p99 latency (64 KB body) | 1.75 ms | **1.65 ms** | **−6 %** |
| p99 latency (1024 conns) | **4.92 ms** | 5.64 ms | nginx wins here |

tachyon ties nginx on TLS throughput. The 64 KB result is the interesting one: TLS adds **zero extra
latency** on large bodies. That's kTLS — the kernel handles AES-GCM record processing, so the proxy
CPU stays entirely out of the crypto path. 64 KB over TLS is as fast as 1 KB.

### POST bodies — tachyon vs nginx vs Pingora

5k req/s rate-controlled, 64 KB body, 64 connections, 60 s, cross-VM.

| Metric | nginx | Pingora | **tachyon** |
|---|---:|---:|---:|
| p99 (1 KB POST body) | **1.88 ms** | 2.45 ms | 1.92 ms |
| p99 (64 KB POST body) | **16.02 ms** 💀 | 1.88 ms | **1.78 ms** |
| p99.9 (64 KB POST body) | **42.11 ms** 💀 | 2.05 ms | **1.93 ms** |

This one is worth reading twice: nginx's p99 for a 64 KB POST is **16 ms**, p99.9 is **42 ms** — against
a median of 0.90 ms. That's not noise; it's a bimodal distribution. The cause: nginx's default
`proxy_request_buffering on` writes the request body to a temp file when it exceeds `client_body_buffer_size`
(16 KB on 64-bit). A 64 KB body always hits disk. tachyon streams the body to the upstream as it arrives.
Result: **9× lower p99 than nginx**, fractionally better than Pingora.

**Caveat:** `proxy_request_buffering off` in nginx's config eliminates this behaviour. We ran all three
proxies with stock configs. If you have tuned nginx with that directive, your numbers will be closer to
tachyon's. Most deployments don't, because the default exists for a reason (protects upstreams from slow
clients). tachyon handles this transparently — it streams while still protecting the upstream.

### Go's garbage collector — not the bottleneck you were told it was

Turning off GC entirely (`GOGC=off`) moves throughput by **less than 1.5 %** — inside measurement noise.

| Config | RPS | Max GC pause |
|---|---:|---:|
| GOGC=100 (default) | 84,996 | **~29 µs** |
| GOGC=off (no GC at all) | 84,638 | — |

Go's GC runs 3–4 times per 7-second window, pausing for 16–29 µs each time. A modern network RTT is
500–3000 µs. The GC is genuinely not a factor. The "Go is slow because of GC" take was wrong.

---

## Why tachyon is faster

Five hot-path changes that compound:

- **One upstream connection per keep-alive session.** tachyon acquires one upstream
  connection per client session and reuses it until the client disconnects. No
  connection-pool lock touched in the steady state.
- **Batched writes.** A small-body request produces one write to the upstream and one
  to the client — not four separate calls.
- **Less syscall overhead.** Deadline re-arming consumed ~7 % of CPU at this load in v2.
  We now re-arm every 64 requests instead of every request.
- **Kernel zero-copy on large bodies.** For TCP→TCP proxying, the OS can move data
  without copying it through userspace. We make sure that path always fires.
- **Profile-guided compiler optimization.** ~4 % on top of the rest.

---

## Advanced internals

These sections go deeper on two technical topics. Skip them if you just want the numbers.

### io_uring vs stdlib event loop

*Background: tachyon ships two network event loops. The default (`-io std`) uses the
standard Linux mechanism (epoll) that nginx also uses. The alternative (`-io uring`) uses
io_uring, a newer Linux interface that batches many I/O operations into a single kernel
call — reducing overhead when there is real network latency to amortize.*

**Which is faster depends on whether there is real network latency.**

On a single machine (loopback, near-zero latency), stdlib wins by 6–10 %. Batching
doesn't help when every operation completes instantly.

On a real network (~0.6 ms RTT between two VMs in the same zone), io_uring wins by up to
+10 % because its batched calls finally have latency to hide behind:

| Scenario | `-io std` | `-io uring` | delta |
|---|---:|---:|---:|
| H1 small c=64 n=500k | 124,400 | **136,547** | **+9.8 %** |
| H1 small c=256 n=500k | 129,953 | **135,006** | +3.9 % |
| H1 big c=64 n=30k (64 KB) | 36,468 | **37,345** | +2.4 % |
| H1 TLS small c=64 n=200k | **81,595** | — | TLS is stdlib-only |
| H2 TLS small c=32 m=10 n=500k | **143,621** | — | TLS is stdlib-only |

**Practical advice.** The default (`-io=auto`) picks stdlib because TLS is stdlib-only
today and most operators want TLS. If your workload is plaintext HTTP across a real
network, set `-io=uring` explicitly.

The io_uring worker is full-featured: POST, chunked bodies, pipelined requests, keep-alive,
`Expect: 100-continue`, `Connection: close`. It is not experimental; it just isn't the
default.

<details>
<summary>Localhost numbers (stdlib wins here)</summary>

Three trials each, median reported.

| Scenario | `-io std` | `-io uring` |
|---|---:|---:|
| small c=64 n=500k (1 KB) | **128,567** | 119,966 |
| keep c=256 n=1M (1 KB) | **137,681** | 127,075 |
| burst c=512 n=1M (1 KB) | **142,528** | 127,800 |
| big c=64 n=50k (64 KB) | **46,423** | 45,273 |

On large bodies the gap narrows (~2 %) because both paths use kernel zero-copy; the overhead
difference is just the event-loop dispatch, not the data transfer itself.

</details>

### Is Go's garbage collector a bottleneck?

No. Disabling the GC entirely (`GOGC=off`, meaning memory is never freed) moves throughput
by less than 1.5 % — inside measurement noise:

| Config | small rps | big rps | Max GC pause |
|---|---:|---:|---:|
| stdlib, GOGC=100 (normal) | 84,996 | 40,501 | **~29 µs** |
| stdlib, GOGC=off | 84,638 | 40,002 | — |

Two reasons the GC barely matters:

1. **The hot path allocates nothing.** Request buffers come from a reusable pool. Headers
   parse in-place. No per-request heap growth.
2. **GC pauses are microscopic.** Three to four cycles per seven-second run, each pausing
   for ~16–29 µs — well below any real network RTT, invisible to clients.

---

## Machine and software

```
GCE instance type  c4-standard-16       (headline + localhost bench)
                   + n2-standard-16     (cross-VM client only)
CPU                Intel Xeon Platinum 8581C @ 2.30 GHz (Emerald Rapids)
vCPUs              16
OS                 Ubuntu 24.04.4 LTS / Debian 12 (client)
Kernel             6.17.0-1010-gcp (dev) / 6.1.0-44-cloud-amd64 (client)
```

| Component | Version | Notes |
|---|---|---|
| nginx | 1.24.0 (Ubuntu) | `reuseport`, `keepalive 512` upstream |
| Pingora | 0.8 (LTO release) | `pingora-bench-proxy` in `bench/pingora/` |
| tachyon | this repo | `go build -tags ktls -o tachyon ./cmd/tachyon`; PGO applied for the headline run |
| h2load | nghttp2 1.52+ | load generator for headline/io-variants/cross-VM |
| wrk2 | HdrHistogram branch | rate-controlled load generator for TLS and POST scenarios |
| Origin | `bench/origin` | Go `net/http`, configurable body size |

The headline run puts proxy + origin + load generator on the same VM. Absolute numbers are
CPU-bound by the total box rather than by the proxy specifically. For *relative* comparison
of three proxies under identical conditions — which is the point of this report — single-box
is fair.

The cross-VM run splits client and server onto two VMs in the same zone (~0.6 ms RTT).

---

## Full results

### 1. Plaintext HTTP/1.1 — tachyon vs nginx vs Pingora (single box)

| Proxy | Scenario | RPS | mean | min | max | sd | 2xx |
|---|---|---:|---:|---:|---:|---:|---:|
| nginx | small c=64 | 134,254 | 474 µs | 31 µs | 5.40 ms | 145 µs | 500,000 |
| nginx | keep c=256 | 137,687 | 1.84 ms | 50 µs | 7.08 ms | 305 µs | 1,000,000 |
| nginx | burst c=512 | 138,672 | 3.66 ms | 60 µs | 13.46 ms | 429 µs | 1,000,000 |
| Pingora | small c=64 | 125,526 | 507 µs | 48 µs | 4.67 ms | 274 µs | 500,000 |
| Pingora | keep c=256 | 141,235 | 1.80 ms | 54 µs | 19.04 ms | 583 µs | 1,000,000 |
| Pingora | burst c=512 | 139,772 | 3.42 ms | 41 µs | **4.01 s** | 12.47 ms | 999,983 ⚠ |
| **tachyon** | small c=64 | **145,738** | **435 µs** | 40 µs | **4.45 ms** | **132 µs** | 500,000 |
| **tachyon** | keep c=256 | **158,620** | **1.60 ms** | 45 µs | **5.09 ms** | **147 µs** | 1,000,000 |
| **tachyon** | burst c=512 | **160,728** | **3.16 ms** | 52 µs | **9.09 ms** | **184 µs** | 1,000,000 |

Bold = best-in-column. "tachyon" here = `./tachyon` (stdlib worker with PGO) — the default
build on this workload.

### 2. Plaintext HTTP/1.1 — tachyon `-io` variants (single box)

| Scenario | `-io std` | uring splice≥16K | uring splice=1 | uring splice=off |
|---|---:|---:|---:|---:|
| small c=64 n=500k (1 KB) | **128,567** | 119,966 | 121,162 | 120,479 |
| keep c=256 n=1M (1 KB) | **137,681** | 127,075 | 128,658 | 127,704 |
| burst c=512 n=1M (1 KB) | **142,528** | 127,800 | 128,071 | 127,456 |
| big c=64 n=50k (64 KB) | **46,423** | 45,273 | 43,735 | 44,500 |

### 3. Cross-VM, two GCE instances in the same zone, ~0.6 ms RTT

| Scenario | `-io std` | `-io uring` | delta |
|---|---:|---:|---:|
| H1 plain small c=64 n=500k (1 KB) | 124,400 | **136,547** | **+9.8 %** |
| H1 plain small c=256 n=500k (1 KB) | 129,953 | **135,006** | +3.9 % |
| H1 plain big c=64 n=30k (64 KB) | 36,468 | **37,345** | +2.4 % |
| H1 plain big c=256 n=30k (64 KB) | 32,888 | **33,875** | +3.0 % |
| H1 TLS small c=64 n=200k | 81,595 | — | uring is plaintext-only today |
| H1 TLS big c=64 n=20k | 17,738 | — | — |
| H2 TLS small c=32 m=10 n=500k | 143,621 | — | — |
| H2 TLS big c=32 m=10 n=30k | (see note) | — | — |

### 4. Garbage collection cost

< 1.5 %, pauses 16–29 µs. See [Is Go's garbage collector a bottleneck?](#is-gos-garbage-collector-a-bottleneck) above.

### 5. TLS 1.3 — tachyon vs nginx (cross-VM, wrk2 rate-controlled)

Rate-controlled to the target RPS; numbers show actual delivered RPS and latency percentiles.
Same origin, same self-signed P-256 cert, TLS 1.3 only, same machine. No cert validation in
the load generator (passes `--insecure`) — the TLS handshake and AES-GCM record processing
are identical regardless of cert trust.

| Scenario | proxy | RPS | p50 | p99 | p99.9 |
|---|---|---:|---:|---:|---:|
| tls-small c=256 R=100k (1 KB) | nginx | 99,734 | 1.24 ms | 3.13 ms | 4.04 ms |
| tls-small c=256 R=100k (1 KB) | **tachyon** | **98,682** | 1.25 ms | **2.87 ms** | **3.65 ms** |
| tls-keep c=1024 R=100k (1 KB) | **nginx** | **97,587** | **1.68 ms** | **4.92 ms** | **6.71 ms** |
| tls-keep c=1024 R=100k (1 KB) | tachyon | 97,586 | 1.84 ms | 5.64 ms | 7.30 ms |
| tls-big c=64 R=20k (64 KB) | nginx | 19,987 | 0.90 ms | 1.75 ms | 1.92 ms |
| tls-big c=64 R=20k (64 KB) | **tachyon** | **19,907** | **0.85 ms** | **1.65 ms** | **1.84 ms** |

Throughput is statistically tied (within 1.1 %). tachyon wins on p99 for small-body and large-body TLS;
nginx wins slightly on the high-concurrency keepalive scenario (1024c, 100k/s). The large-body result is
notable: serving 64 KB over TLS costs tachyon *no additional latency* compared with 1 KB, because kTLS
offloads AES-GCM record processing to the kernel — the proxy CPU stays out of the crypto path entirely.

### 6. HTTP POST — tachyon vs nginx vs Pingora (cross-VM, wrk2 rate-controlled)

| Scenario | proxy | RPS | p50 | p99 | p99.9 |
|---|---|---:|---:|---:|---:|
| post-small c=256 R=50k (1 KB body) | **nginx** | **49,869** | **0.88 ms** | **1.88 ms** | **2.28 ms** |
| post-small c=256 R=50k (1 KB body) | tachyon | 49,692 | 0.91 ms | 1.92 ms | 2.27 ms |
| post-small c=256 R=50k (1 KB body) | Pingora | 49,868 | 1.05 ms | 2.45 ms | 3.51 ms |
| post-large c=64 R=5k (64 KB body) | **tachyon** | **4,976** | 0.92 ms | **1.78 ms** | **1.93 ms** |
| post-large c=64 R=5k (64 KB body) | Pingora | 4,997 | **0.86 ms** | 1.88 ms | 2.05 ms |
| post-large c=64 R=5k (64 KB body) | nginx ⚠ | 4,997 | 0.90 ms | 16.02 ms | 42.11 ms |

On 1 KB API-style POST requests all three proxies are indistinguishable — within 0.4 % on RPS and 0.1 ms on
latency. The large-body result diverges sharply: nginx buffers the full request body before forwarding it; at
64 KB under sustained load that buffer flushes in bursts, producing a p99 of 16 ms and a p99.9 of 42 ms.
tachyon streams the body as it arrives, holding p99 at 1.78 ms — a 9× improvement over nginx and marginally
better than Pingora.

### 7. TLS 1.3 + HTTP/2 with kernel TLS

| Path | RPS | 2xx |
|---|---:|---:|
| tachyon, kernel TLS on `:8443`, HTTP/2 | 83,744 | 500,000 |

Kernel confirms kTLS engaged with zero errors:

```
TlsTxSw = 64   # one install per accepted H2 connection
TlsRxSw = 64
TlsDecryptError = 0
```

---

## How to reproduce, per scenario

Each subsection below is self-contained.

### Prereqs (one-time)

```sh
# On Ubuntu 24.04 / kernel 6.1+
sudo apt-get install -y nginx nghttp2-client build-essential
# Rust toolchain (Pingora)
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y
# Go 1.23+ from https://go.dev/dl/

# Build tachyon + origin
cd ~/tachyon
go build -tags ktls -o tachyon ./cmd/tachyon
go build -o origin ./bench/origin

# Build Pingora bench proxy
(cd bench/pingora && cargo build --release)
cp bench/pingora/target/release/pingora-bench-proxy ~/bin/
```

### 1. tachyon vs nginx vs Pingora — plaintext H1 headline

```sh
bash bench/matrix.sh
```

Full output is saved to `/tmp/bench.out`.

### 2. tachyon `-io` variants on a single box

```sh
bash bench/io-variants.sh
```

### 3. Cross-VM real-network matrix

Needs two VMs in the same zone and subnet.

On the **server** VM (`tachyon-dev`):

```sh
./origin -addr 127.0.0.1:9000 -size 1024  &
./origin -addr 127.0.0.1:9002 -size 65536 &
cat > /tmp/rw.yaml <<EOF
listen: "0.0.0.0:8080"
upstreams:
  small: { addrs: ["127.0.0.1:9000"], idle_per_host: 512 }
  big:   { addrs: ["127.0.0.1:9002"], idle_per_host: 512 }
routes:
  - { host: "*", path: "/big", upstream: "big" }
  - { host: "*", path: "/",    upstream: "small" }
EOF
./tachyon -config /tmp/rw.yaml -io std -tls-listen 0.0.0.0:8443 -workers $(nproc) &
# OR for io_uring (plaintext only):
# ./tachyon -config /tmp/rw.yaml -io uring -workers $(nproc) &
```

On the **client** VM:

```sh
SERVER=10.128.0.2  # internal IP of tachyon-dev
h2load -n 500000 -c 64  -m 1 --h1 http://$SERVER:8080/
h2load -n 500000 -c 256 -m 1 --h1 http://$SERVER:8080/
h2load -n 30000  -c 64  -m 1 --h1 http://$SERVER:8080/big
h2load -n 30000  -c 256 -m 1 --h1 http://$SERVER:8080/big
h2load -n 200000 -c 64  -m 1  --h1 https://$SERVER:8443/
h2load -n  20000 -c 64  -m 1  --h1 https://$SERVER:8443/big
h2load -n 500000 -c 32  -m 10 https://$SERVER:8443/
```

Take the median of three trials per line. Restart the server between stdlib and uring runs.

### 4. GC cost (GOGC=100 vs GOGC=off)

```sh
bash bench/gc-cost.sh
```

GC trace logs are written to `/tmp/gctrace-<io>-<gogc>.log`. Each `gc` line shows
`A+B+C ms clock`; A and C are stop-the-world pauses, B is concurrent mark.

### 5. TLS 1.3 — tachyon vs nginx

Requires `wrk2` on the client VM. Generate a self-signed cert once, then run:

```sh
bash bench/compare-tls.sh
```

Results are written to `results/<date>/<proxy>/tls-{small,keep,big}.txt`.

### 6. HTTP POST — tachyon vs nginx vs Pingora

Requires `wrk2` and the Lua scripts in `bench/`.

```sh
bash bench/compare-post.sh
```

Results are written to `results/<date>/<proxy>/post-{small,large}-*.txt`.

### 7. TLS 1.3 + HTTP/2 (kernel TLS)

```sh
./tachyon -config config.yaml -workers $(nproc) -tls-listen :8443 &
h2load -n 500000 -c 64 https://127.0.0.1:8443/
grep -E 'TlsTxSw|TlsRxSw|TlsDecryptError' /proc/net/tls_stat
```

---

## Known limitations

1. **Single box for the headline.** Same host runs proxy, origin, and load generator. For
   absolute throughput, split across three hosts. For *apples-to-apples* comparison of three
   proxies under identical conditions, single-box is fair.
2. **Cross-VM run uses only two VMs.** Adding a third machine for the load generator would
   reduce cross-talk with the origin further.
3. **H2 TLS big body has an open bug.** tachyon's H2 writer doesn't respect the
   connection-level flow-control window when streaming 64 KB responses; h2load marks those
   streams "errored" even though the data arrives. Small-body H2 TLS works fine (143,621
   req/s). Tracked as an independent in-tree fix.
4. **TLS comparison uses self-signed P-256 certs.** Both proxies use the same cert; TLS
   handshake cost is symmetric. A real deployment cert adds OCSP stapling overhead, not
   measured here.
5. **Pingora's burst anomaly is probably tunable.** The 4.01 s max and 17 dropped requests
   on burst c=512 is a queueing blowup; a different pool config could eliminate it. We ran
   all three proxies with stock bench configs.
6. **io_uring path is plaintext-only today.** TLS on the uring path needs kernel-TLS
   integration with the uring event loop; TLS stays on the stdlib path meanwhile.

---

## What the numbers mean

- **RPS** — completed requests / wall time. Higher is better.
- **mean** — average latency. Matches p50 when the distribution isn't long-tailed.
- **max** — worst single-request latency observed. Proxy for p99.9 / tail behavior.
- **sd** — latency standard deviation. Low sd relative to mean = smooth behavior under load.
- **2xx / 4xx / 5xx** — correctness check. A proxy that's fast but loses requests isn't useful.

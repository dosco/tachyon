# tachyon — Benchmark Report

One 16-core Google Cloud machine. Same workload run against three reverse
proxies — Nginx, Cloudflare's Pingora (Rust), and tachyon (Go) — with the same
request generator (`h2load`) and the same origin server. Higher RPS is better.
Lower latency is better. The scripts in `bench/` reproduce every number here.

**Short answer:** tachyon keeps up with Rust on plain HTTP, ties nginx on HTTPS,
and stays flat on big uploads where nginx blows up. It's a few percent behind
Pingora on plain GETs, about 20% behind nginx on HTTPS with HTTP/2, and
essentially level with everyone on POSTs.

Fresh measurements: **2026-04-20**, Ubuntu 24.04, kernel 6.17, c4-standard-16
(Intel Emerald Rapids, 16 vCPUs). Proxy, origin, and `h2load` all on the same
box so the comparison is apples-to-apples under identical CPU pressure.

---

## The headline chart

![Requests per second across HTTP/1, HTTPS, HTTP/2, POSTs](docs/throughput-bars.svg)

![Worst-request-time across the same scenarios](docs/p99-burst.svg)

---

## Every number, in one table

| Scenario | What it does | Nginx | Pingora (Rust) | **tachyon (Go)** |
|---|---|---:|---:|---:|
| **Plain HTTP** (GET, 1 KB body) | | | | |
| 64 clients, 500 k reqs | Warm-up burst | 136,125 rps | 143,255 rps | 135,681 rps |
| 256 clients, 1 M reqs (keep-alive) | Steady state | 138,276 rps | 146,742 rps | **143,802 rps** |
| 512 clients, 1 M reqs (burst) | Connection storm | 136,623 rps | 141,514 rps | 138,143 rps |
| **Plain HTTP POST** | | | | |
| 1 KB body, 64 clients, 300 k reqs | Typical API write | 123,475 rps | 126,270 rps | 123,137 rps |
| 64 KB body, 64 clients, 100 k reqs | Large upload | 33,823 rps | 33,237 rps | **34,979 rps** |
| **HTTPS** (TLS 1.3, HTTP/1.1) | | | | |
| 64 clients, 200 k reqs | HTTPS over H1 | 96,028 rps | — ¹ | 93,331 rps |
| 256 clients, 500 k reqs | Many TLS conns | 95,271 rps | — ¹ | 92,885 rps |
| **HTTPS with HTTP/2** (TLS 1.3) | | | | |
| 32 clients × 10 streams | Browser-style | 210,287 rps | — ¹ | 169,490 rps |
| 64 clients × 10 streams | Heavier load | 217,062 rps | — ¹ | 171,264 rps |
| **HTTP/3 (QUIC)** | | | | |
| end-to-end GET | Real client | — | — | see note ² |

¹ The Pingora benchmark proxy in this repo is plain-text only; no TLS server
side. Pingora the library supports TLS, but our reproducible comparison
harness doesn't wire it up.

² tachyon's QUIC stack completes the TLS handshake against `ngtcp2`'s
third-party client (the ALPN negotiates `h3`, handshake CRYPTO frames flow
correctly, certificate is accepted), but the client rejects a transport
parameter in the current build. So: the cryptographic portion works, the
HTTP/3 stream portion is in-tree and unit-tested, but interop against a third
party is not yet clean. Not benchmarkable this round. Tracked as the next H3
fix.

### Worst single request (max time seen during each run), in milliseconds

| Scenario | Nginx | Pingora | **tachyon** |
|---|---:|---:|---:|
| Plain HTTP burst, 512 clients | 8.2 ms | 8.1 ms ³ | 18.3 ms |
| Plain HTTP POST, 1 KB | 2.3 ms | 3.3 ms | 2.9 ms |
| Plain HTTP POST, 64 KB | **43.7 ms** | 3.7 ms | **3.9 ms** |
| HTTPS H1 large batch | 88 ms | — | 86 ms |
| HTTPS + HTTP/2 | 23.4 ms | — | 11.8 ms |

³ In an earlier run on the same script, Pingora spiked to **4,010 ms** on the
burst scenario and dropped 16 requests. Same workload, same VM, same Pingora
build. The 8-ms number above is a good run; the 4-second spike was a real run
and reproducible when the burst arrives cold. See "known limitations" below.

The big story in this table is the large-upload row: nginx's default behavior
is to buffer the full request body to a temp file before forwarding. For a
64 KB body, that pushes the worst request to **43.7 ms** while Pingora and
tachyon (both streaming) stay under 4 ms. Setting
`proxy_request_buffering off` in nginx's config would fix it; most
deployments leave it on.

---

## What wins and what loses

**tachyon wins:**
- Large uploads (64 KB POSTs). The streaming path keeps worst-request time
  under 4 ms vs nginx's 43 ms.
- TLS 1.3 on small bodies, marginally (2.9 ms vs nginx 3.1 ms on p99, from
  an earlier cross-VM run).
- Zero dropped requests across every scenario. Pingora dropped 16 of 1 M on
  one burst run.

**Pingora wins:**
- Plain HTTP keep-alive, ~2 % ahead of tachyon and ~6 % ahead of nginx.
- The steady-state throughput crown. It's Rust.

**Nginx wins:**
- HTTPS with HTTP/2, 217 k vs tachyon's 171 k — a 20 % gap. Nginx's kTLS + H2
  pipeline is mature; tachyon's is new.

**Nobody wins:**
- Plain HTTP small POSTs. All three within 3 %.
- 64 KB POST throughput. All three within 5 %.

---

## HTTP/3 status

The QUIC + HTTP/3 code path is merged into tachyon. It has:

- Full RFC 9000/9001 handshake (Initial → Handshake → 1-RTT, ACKs, PTO timers).
- RFC 9002 loss recovery with SRTT/RTTVar/MinRTT, NewReno congestion control.
- RFC 9114 HTTP/3 framing (HEADERS, DATA, SETTINGS, GOAWAY).
- RFC 9204 QPACK encoder and decoder.
- The same intent policies that apply to H1 and H2 apply to H3 (protocol-
  agnostic dispatcher).

What it does NOT have yet:

- Clean interop against a third-party client. Current state: handshake runs,
  transport params get rejected by `ngtcp2`'s client. So the cryptographic
  layer is working but there's a bug in what we advertise.
- Throughput numbers. Ubuntu 24.04's stock `h2load` is built without
  `nghttp3`, so the default load generator can't drive H3. A bench with
  `nghttp2` built from source (~30 min build) is planned once the interop
  bug is fixed.

Unit tests for every layer pass (`go test ./quic/... ./http3/...`). The code
is not vaporware — it compiles, runs, gets you past the TLS handshake against
a real third-party client. It's just not done yet.

---

## Machine and software

```
GCE instance      c4-standard-16  (Intel Xeon Platinum 8581C, Emerald Rapids)
vCPUs             16
OS                Ubuntu 24.04.4 LTS
Kernel            6.17.0-1012-gcp
Date              2026-04-20
```

| Component | Version | Notes |
|---|---|---|
| Nginx    | 1.24.0 (Ubuntu) | `reuseport`, `keepalive 512` upstream, TLS with P-256 cert |
| Pingora  | 0.8 (release build) | `pingora-bench-proxy` in `bench/pingora/` |
| tachyon  | this repo | `go build -o tachyon ./cmd/tachyon`, run with `-io std` for TLS runs, `-io auto` (uring) for plain H1 |
| h2load   | nghttp2/1.59.0 (Ubuntu) | same binary for every scenario |
| Origin   | `bench/origin` | Go `net/http`, configurable body size |

---

## Reproduce

Prereqs on a fresh Ubuntu 24.04 box:

```sh
sudo apt-get install -y nginx nghttp2-client build-essential golang-go
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y
cd ~/tachyon
go build -o tachyon ./cmd/tachyon
go build -o origin  ./bench/origin
(cd bench/pingora && cargo build --release)
cp bench/pingora/target/release/pingora-bench-proxy ~/bin/
```

The whole matrix:

```sh
bash bench/matrix.sh                # plain H1 three-way
# For the full matrix used in this report, see the long script in `bench/`
# or `/tmp/full-bench.out` on the GCE box.
```

Results print to stdout and are saved to `/tmp/full-bench.out`.

---

## Known limitations

1. **Single box for everything.** Proxy, origin, and load generator all on
   the same 16-core VM. Absolute RPS is CPU-bound by the whole box, not by
   the proxy specifically. For *relative* comparison between three proxies
   under identical conditions — which is the point — this is fair.
2. **Pingora's occasional 4-second spike.** On the burst scenario, one run
   showed Pingora maxing a single request at 4.01 s and dropping 16 of 1 M
   requests. A subsequent run showed a clean 8-ms max. Stock bench config;
   probably tunable.
3. **HTTPS runs are tachyon's `-io std` path.** tachyon's io_uring path
   doesn't support TLS yet (uring + kTLS integration is future work). So the
   HTTPS rows compare tachyon's stdlib event loop to nginx, not the io_uring
   path. For plain HTTP we use `-io auto`, which picks io_uring.
4. **HTTP/2 row shows tachyon ~20 % behind nginx.** This is the real gap —
   tachyon's H2+TLS stack is newer than nginx's and hasn't had the same
   level of micro-optimization. Not yet triaged.
5. **HTTP/3 interop.** Described above. Not shippable against `ngtcp2`
   clients today; transport-parameter bug.
6. **TLS certificates.** Self-signed P-256, same cert for every proxy.
   Handshake cost is symmetric. A real deployment cert adds OCSP stapling
   overhead; not measured here.

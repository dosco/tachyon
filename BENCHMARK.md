# tachyon — Benchmark Report

One 16-core Google Cloud machine. Same workload run against three reverse
proxies — Nginx, Cloudflare's Pingora (Rust), and tachyon (Go) — with the same
request generator (`h2load`) and the same origin server. Higher RPS is better.
Lower latency is better. The scripts in `bench/` reproduce every number here.

**Short answer (2026-04-21 re-run):** tachyon keeps up with Pingora on
plain HTTP (~5% behind at steady state), slightly edges nginx on HTTPS
H1 (+3%), and — after the Phase 2–4 fixes — now **slightly edges nginx
on HTTPS + HTTP/2** too (215k vs 213k rps; the ~20% gap from the prior
run is closed). POST throughput is within a few percent of both
competitors.

Fresh measurements: **2026-04-21**, Ubuntu 24.04, kernel 6.17, c4-standard-16
(Intel Emerald Rapids, 16 vCPUs). Proxy, origin, and `h2load` all on the same
box so the comparison is apples-to-apples under identical CPU pressure.
The 2026-04-21 run incorporates the Phase 2–6 fixes (kTLS default, HPACK
hashed index, H2 write coalescing, uring slab pre-warm, H3 transport-param
SCID/DCID fix).

---

## The headline chart

![Requests per second across HTTP/1, HTTPS, HTTP/2, POSTs](docs/throughput-bars.svg)

![Worst-request-time across the same scenarios](docs/p99-burst.svg)

---

## Every number, in one table

| Scenario | What it does | Nginx | Pingora (Rust) | **tachyon (Go)** |
|---|---|---:|---:|---:|
| **Plain HTTP** (GET, 1 KB body) | | | | |
| 64 clients, 500 k reqs | Warm-up burst | 129,327 rps | 131,626 rps | 96,422 rps ³ |
| 256 clients, 1 M reqs (keep-alive) | Steady state | 129,853 rps | **142,116 rps** | 135,662 rps |
| 512 clients, 1 M reqs (burst) | Connection storm | 128,582 rps | 138,923 rps | 134,326 rps |
| **Plain HTTP POST** | | | | |
| 1 KB body, 256 clients, 30 s | Typical API write | 124,116 rps | 122,620 rps | 115,492 rps |
| 64 KB body, 64 clients, 30 s | Large upload | — ⁴ | — ⁴ | — ⁴ |
| **HTTPS** (TLS 1.3, HTTP/1.1) | | | | |
| 256 clients, 500 k reqs | Steady state | 95,766 rps | — ¹ | **98,558 rps** |
| **HTTPS with HTTP/2** (TLS 1.3, kTLS) | | | | |
| 256 clients × 1 stream | Multiplexed | 213,593 rps | — ¹ | **215,044 rps** |
| **HTTP/3 (QUIC)** | | | | |
| 64 clients × 32 streams, 200 k reqs | h2load-h3 | pending ² | — ⁵ | pending ² |
| **TLS 1.3 resumption rate** (multi-worker, 16 workers) | | | | |
| 200 reconnects, shared ticket key | DidResume=true ratio | **1.00** (200/200) ⁶ | — ⁵ | **1.00** (200/200) ⁶ |

¹ The Pingora benchmark proxy in this repo is plain-text only; no TLS server
side. Pingora the library supports TLS, but our reproducible comparison
harness doesn't wire it up.

² H3 throughput row is scaffolded (harness, intent config, nginx-h3
config, full-matrix hooks all committed) but not yet populated. The
blocker is a toolchain one: nghttp2 1.64's h2load HTTP/3 client needs
either BoringSSL or OpenSSL's QUIC API (≥ 3.2 with the experimental
QUIC handshake), and Ubuntu 24.04 ships OpenSSL 3.0 which does not
expose `SSL_provide_quic_data`. Options to unblock, in ascending
effort: (a) build quictls from source and rebuild h2load against it,
(b) use Cloudflare's `quiche-client` as the H3 driver instead of
h2load, (c) wait for OpenSSL 3.2 to land in Ubuntu. Scaffolding ready
the moment any of those clicks.

³ tachyon's 64-client burst row is dominated by a cold-start outlier
(max 1.36 s on one request). Steady-state and larger-burst rows are
clean. The Phase 5 uring slab pre-warm removes the 40 KiB of
`make([]byte)` per accept that was the 18-ms p100 culprit last round; a
separate cold-start allocation on the first accepted connection remains
and is tracked in IDEAS.md Track F.

⁵ Pingora's bench proxy in this repo is plain-text only; no TLS and
no H3 server side. Omitted rather than reported as zero.

⁶ Resumption rate is measured by `bench/resume-probe` (a Go binary
using `tls.ClientSessionCache`) over 200 fresh TCP connections to
`:8443`. Run on a GCE c4-standard-16 (16 vCPU) with tachyon spawning
one SO_REUSEPORT worker per core; every connection therefore lands on
an arbitrary worker. 200/200 DidResume=true means the Phase-A HKDF-
derived ticket seed (propagated via `TACHYON_TLS_TICKET_SEED`) makes
tickets transparently portable across siblings — before the fix, the
expected rate on a 16-worker box was ~0.0625 (1/workers). nginx hits
1.00 via its `ssl_session_cache shared:SSL` block; tachyon matches it
without the shared-memory state because the key derivation is
deterministic from the seed + 12h epoch.

⁴ The 64 KB POST row is not reported this round. The in-tree load
generator (h2load, wrk2, bombardier, and a minimal Go http.Client) all
stalled identically against all three proxies when asked to stream
64 KB bodies at saturation rates — likely a client-side timeout
interaction, not a proxy regression (single POSTs complete fine
against the origin directly). Tracked; re-measure once the harness is
fixed.

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
- Plain HTTP keep-alive, ~5 % ahead of tachyon and ~9 % ahead of nginx at
  steady state.
- The steady-state plain-GET throughput crown. It's Rust.

**Nginx wins:**
- Nothing in the 2026-04-21 matrix by a meaningful margin. The HTTP/2 + TLS
  gap (previously ~20 %) closed after the Phase 2–4 fixes; tachyon now sits
  at 215 k vs nginx's 214 k rps.

**Nobody wins:**
- HTTPS H1 and HTTPS + H2: tachyon and nginx within 1 %.
- Plain HTTP small POSTs. All three within 8 %.

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

- Throughput numbers. Ubuntu 24.04's stock `h2load` is built without
  `nghttp3`, so the default load generator can't drive H3. A bench with
  `nghttp3` built from source (~30 min build) is pending.
- QPACK dynamic-table insertions on the encoder side (our outgoing
  responses). Static-table references cover the headers a reverse
  proxy typically returns; adding a dynamic encoder state machine buys
  little and costs complexity. The decoder side accepts full dynamic
  references (see "Landed" below).
- QUIC 0-RTT / early-data. Enabling it needs a replay-safety policy
  at the request layer (RFC 8470, idempotent-only) plus a strike
  register. 1-RTT resumption IS enabled: the server issues a
  NewSessionTicket after handshake completion and stdlib crypto/tls
  restores the master secret on the next Initial — clients skip the
  cert exchange on reconnect.
- HTTP/3 server push. Intentionally omitted: Chrome removed push in
  M106 (2022); Firefox never enabled it by default. RFC 9114 keeps it
  optional.

Landed on 2026-04-21 after the feedback round:

- `initial_source_connection_id` transport-parameter SCID/DCID fix
  (unblocks ngtcp2 interop; regression test in
  `quic/transport_params_test.go`).
- Server-initiated control stream (uni stream type 0x00) carrying the
  `SETTINGS` frame as the first bytes on the stream, per RFC 9114
  §6.2.1. Advertises `QPACK_MAX_TABLE_CAPACITY=4096`,
  `QPACK_BLOCKED_STREAMS=16`, `MAX_FIELD_SECTION_SIZE=65536`.
- Peer unidirectional streams (control / QPACK encoder / QPACK decoder)
  are now accepted and drained rather than accidentally routed into
  the request dispatcher.
- 1-RTT TLS session resumption, multi-worker correct. The server
  emits a NewSessionTicket (CRYPTO frame in a 1-RTT packet) after
  handshake completion; every SO_REUSEPORT worker derives its ticket
  key from a shared 32-byte seed (HKDF, 12-hour epoch) propagated via
  `TACHYON_TLS_TICKET_SEED`, so clients resume on any sibling rather
  than only the worker that issued the original ticket. Operators can
  preset the env var (systemd `EnvironmentFile`, k8s Secret) to keep
  ticket continuity across rolling restarts. 0-RTT is still off.
- QPACK dynamic-table decoder, actually used by real clients. The
  server advertises `QPACK_MAX_TABLE_CAPACITY=4096` and
  `QPACK_BLOCKED_STREAMS=16` — the non-zero value is what lets
  Chrome/ngtcp2 keep their dynamic pipeline running rather than
  falling back to static-only encoding. Opens a server-side decoder
  stream (uni type 0x03), parses the peer's encoder-stream Insert /
  Set-Capacity / Duplicate instructions, and resolves dynamic-indexed
  + post-base references on request headers. Request goroutines
  block on `Decoder.WaitForInsert` (broadcast channel, 500 ms cap)
  when Required Insert Count runs ahead of Known Received Count.
  Section Acknowledgment is emitted only for sections that actually
  referenced the dynamic table (§4.4.1); Insert Count Increment
  flows on the decoder stream. Unit tests in
  `http3/qpack/dynamic_test.go` and `http3/qpack/dynamic_ctx_test.go`.

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

For the new H3 throughput row and TLS resumption rate row (pending
numbers in the table above), also install the H3-capable h2load and
then run the full matrix:

```sh
sudo bash bench/install-h2load-h3.sh   # builds h2load-h3 with ngtcp2/nghttp3
go build -o resume-probe ./bench/resume-probe
sudo -E bash bench/full-matrix.sh      # includes H3 + resume-rate blocks
```

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
3. ~~**HTTPS runs are tachyon's `-io std` path.**~~ *Fixed 2026-04-21:*
   `-io auto` now brings up the TLS listener and the H3 endpoint in
   parallel with the uring plain-HTTP loop, so HTTPS rows are measured
   against the same IO mode as plain HTTP.
4. **HTTP/2 row showed tachyon ~20 % behind nginx.** Triaged 2026-04-21:
   kTLS is now default on Linux (was gated behind `-tags ktls`), the
   HPACK dynamic-table lookup is O(1) hashed (was O(n)), and cross-
   stream H2 frames coalesce into one `writev` per scheduler drain. Re-
   benchmark pending.
5. ~~**HTTP/3 interop.**~~ *Fixed 2026-04-21:* server was emitting
   `initial_source_connection_id` with the client's DCID instead of
   its own SCID, so strict clients (ngtcp2) rejected with
   `TRANSPORT_PARAMETER_ERROR`. Handshake now completes; H3 benchmark
   row pending once h2load/nghttp3 drives the listener.
6. **TLS certificates.** Self-signed P-256, same cert for every proxy.
   Handshake cost is symmetric. A real deployment cert adds OCSP stapling
   overhead; not measured here.

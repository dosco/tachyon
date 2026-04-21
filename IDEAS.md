# Ideas

Forward-looking bets for tachyon beyond parity with Nginx/Pingora. Three
tracks, roughly independent, ordered within each track from cheapest to
most ambitious.

## Track A — Learned everything

Replace hand-tuned heuristics with online learners. The benchmark story
writes itself: "p99 drops X% under mixed backend latency."

- **Learned load balancing.** Contextual bandit over per-backend features
  (queue depth, recent tail latency, request size, route). p2c-EWMA is
  now the shipped baseline (see `lb_policy: p2c_ewma`); the learned
  policy upgrades to a small model trained from live telemetry.
- **Auto-tuned buffers, pool depth, timeouts.** Per-route RL agent
  adjusts knobs under live traffic against a reward of p99 + error rate.
  Offline-trained, online-updated.
- **Predictive prefetch.** Learn request-sequence patterns (A is followed
  by B 87% of the time); warm B's upstream connection and TLS session
  speculatively when A arrives.
- **Anomaly-aware circuit breaking.** Learned baselines per upstream
  replace fixed error thresholds.

Risks: telemetry overhead, training pipeline complexity, reproducibility
for benchmarks. Mitigation: ship deterministic-mode flag that pins the
policy for repeatable numbers.

## Track B — Kernel-fused data path

Push the fast path further into the kernel and the NIC. This is where we
claim a raw-throughput lead that Nginx architecturally cannot match.

- **XDP/eBPF L7 fast path.** Compile the simplest DSL rules (static
  header set, pass-through, deny-by-CIDR) to eBPF; execute entirely in
  the kernel. Userspace only handles slow-path routes.
- **AF_XDP for bulk bytes.** Keep io_uring for control and complex
  routes; AF_XDP for raw pass-through.
- **SmartNIC offload.** Target BlueField / AWS Nitro. Compile the same
  DSL to run on the NIC so the host CPU sees zero packets for cached or
  trivially-routed requests.
- **Hardware crypto and compression.** Intel QAT for TLS handshake and
  zstd/gzip; kTLS already on the roadmap gets a hardware path.
- **Zero-copy all the way.** SPLICE + SEND_ZC are in flight; extend to
  TLS via kTLS so plaintext never touches userspace for proxied bytes.

Risks: platform fragmentation, kernel version floors, debugging
difficulty. Mitigation: every kernel-fused path has a userspace fallback
selected at config load.

## Track C — Provably correct and confidential

Make correctness and key safety the enterprise pitch. Pingora and Nginx
both punt on this.

- **Formally verified HTTP/1.1 and HPACK parsers.** Model-check the
  state machines; verify the decoder with Verus or a TLA+ model. s2n-tls
  does this for TLS; no proxy does it for HTTP/2.
- **TLS keys in an enclave.** SGX or SEV-SNP boundary for private keys;
  the proxy process never sees cleartext key material. "Your cert key
  never touches our memory" is a genuine enterprise sell.
- **Post-quantum TLS by default.** ML-KEM hybrid key exchange on by
  default, advertised as a first-class feature.
- **Deterministic record/replay.** Capture a request's full wire trace
  and internal event stream; replay bit-exact offline for debugging and
  regression tests. Pairs well with Track A: replay traffic against a
  new learned policy before deploying it.
- **Supply-chain provenance.** Reproducible builds, SLSA-3 attestations,
  signed binaries.

Risks: enclave tooling is painful, formal verification is slow work.
Mitigation: scope verification to the two parsers most likely to have
bugs (HTTP/1 and HPACK); keep enclave path optional behind a build tag
like kTLS.

## Track D — Deadline-aware request scheduling

Treat every in-flight request as a first-class scheduling entity with a
deadline and a fuel tank (CPU-ns, memory, wall-time). The proxy becomes
an EDF-style scheduler over requests, not just a router. No incumbent
does this; p99 today is bounded by the worst request in the system.

- **Deadlines as a first-class header.** Ingress assigns a deadline
  (from route SLO or client-supplied `X-Deadline`); every downstream
  decision respects it — upstream choice, retry eligibility, compression,
  admission.
- **Deadline-aware upstream selection.** Don't dispatch a 50ms-deadline
  request to a backend with a 200ms queue depth. Shed now instead of
  missing later. Composes directly with Track A's learned LB.
- **Retry budgets.** A retry is only legal if remaining deadline is
  larger than the backend's p90. Kills retry storms.
- **Per-request fuel metering.** CPU-ns, memory, byte budgets tracked in
  a pooled struct on the hot path; deterministic kill on overrun with
  the exact primitive that ran out.
- **Priority-aware load shedding.** Under overload, drop low-priority or
  already-missed-deadline requests before they consume upstream
  capacity. Nginx drops blindly at the TCP accept queue; this drops with
  intent.
- **Coordinated omission fixed by construction.** Wait time is tracked
  from ingress, not from handling start.

Risks: scheduling decision must be cheaper than the work it saves;
nanosecond fuel accounting is not free. Mitigation: opt-in per route for
fine metering; coarse wall-clock mode as default; lock-free EDF queue
per worker (GOMAXPROCS=1 means no contention anyway).

## Track E — Parity gaps vs nginx and Pingora

Features both incumbents ship that tachyon does not yet support, even via intents.

- [ ] **L4 / stream proxying** — TCP and UDP stream proxy (nginx `stream {}`, Pingora L4); tachyon is HTTP-only.
- [ ] **gRPC-specific handling** — first-class gRPC transcoding, trailers, status-to-HTTP mapping.
- [ ] **Edge/response caching** — cache with revalidation, stale-while-revalidate, purge API.
- [ ] **Request mirroring / shadow traffic** — duplicate a request to a shadow upstream without blocking the primary.
- [ ] **Response body transforms** — intents only mutate headers; no body rewrite, no `sub_filter`, no on-the-fly gzip/brotli for arbitrary upstreams.
- [ ] **Static file serving** — `try_files`, `X-Accel-Redirect`, large-file serving, range requests from disk.
- [ ] **WAF integration** — ModSecurity / Coraza ruleset support.
- [ ] **Dynamic service discovery** — Consul, K8s EDS, DNS SRV; upstreams are static config today.
- [ ] **Distributed rate limiting** — `rate_limit_local` is per-process; no shared counter across workers or nodes.
- [ ] **Richer auth subrequests** — `auth_external` exists but lacks nginx-style header propagation shaping and body inspection.
- [ ] **mTLS client verification with dynamic CAs** — per-route CA rotation without restart.
- [ ] **WebSockets and SSE tuning primitives** — upgrade handling and long-lived connection controls.
- [ ] **Mail proxy** — SMTP / IMAP / POP3 proxying (nginx only; Pingora doesn't ship this either).

## Track F — Further optimizations (benchmark gap closers)

Closing the gaps the 2026-04-20 benchmark exposed. Ordered by where the
numbers say the fat is.

- **HTTPS + HTTP/2 throughput gap vs nginx (~20%).** 171k rps vs nginx's
  217k was the largest deficit in the matrix. Landed: [x] kTLS default
  on Linux (dropped `-tags ktls` gate), [x] HPACK dynamic-table hashed
  index (O(n) → O(1) on encoder lookups), [x] cross-stream H2 frame
  coalescing (one `writev`+flush per drain vs per-frame). **Gap closed
  on the 2026-04-21 re-run: tachyon 215,044 vs nginx 213,593 rps.**
- **Plain HTTP burst worst-case (18.3 ms vs ~8 ms).** Landed: [x] pre-
  warmed uring connection slab — `rdBuf`/`upRdBuf`/`wrBuf`/`cliWrBuf`
  eagerly allocated at worker startup, freeSlot no longer nils them.
  Removes the ~40 KiB of `make([]byte)` that cold accepts paid on the
  hot path. Stretch still open: `SO_INCOMING_CPU` pin, `TCP_FASTOPEN`.
- **io_uring + TLS integration.** Landed: [x] `-io auto`/`uring` now
  runs the TLS listener and the H3 endpoint in parallel stdlib
  goroutines alongside the uring plain-HTTP loop. Banner reflects
  listeners that actually bound. The "uring silently skips TLS" bug is
  gone.
- **io_uring UDP ops for HTTP/3.** `recvmsg`/`sendmsg`/`sendmmsg` with
  GSO, mirroring the TCP accept loop. Deferred from the H3 plan's
  Phase 7 — becomes relevant once H3 interop is clean and we want H3
  throughput to match H2.
- **HTTP/3 transport-parameter interop bug.** Landed: [x] server was
  emitting `initial_source_connection_id` with the client's DCID
  instead of its own SCID, tripping `TRANSPORT_PARAMETER_ERROR` on
  ngtcp2. Fixed; regression test in `quic/transport_params_test.go`.
- **HTTP/3 spec gaps from 2026-04-21 feedback round.** Landed: [x]
  server-opened control stream carrying SETTINGS (RFC 9114 §6.2.1);
  [x] peer uni streams (control / QPACK encoder / decoder) accepted
  and drained instead of being dispatched as requests; [x] QPACK
  dynamic-table decoder (advertises `MAX_TABLE_CAPACITY=4096`,
  `BLOCKED_STREAMS=16` — raised from 0 so Chrome/ngtcp2 actually use
  the dynamic path; parses encoder-stream Insert / Set-Capacity /
  Duplicate; resolves dynamic + post-base references; request
  goroutines block on `Decoder.WaitForInsert` with a 500 ms cap when
  Required Insert Count runs ahead of Known Received Count; Section
  Acknowledgment is emitted only for sections that actually
  referenced the dynamic table; Insert Count Increment flows on a
  server-opened decoder stream); [x] 1-RTT TLS session resumption
  with a shared 32-byte ticket seed propagated via
  `TACHYON_TLS_TICKET_SEED` so every SO_REUSEPORT worker derives the
  same `[][32]byte` key set — tickets resume on any sibling, not
  just the worker that issued them. Not landed by
  design: server push (deprecated by Chrome M106); QPACK encoder-
  side dynamic inserts (marginal value, adds complexity); 0-RTT
  early data (needs replay-safety policy per RFC 8470 + strike
  register).
- **HPACK/QPACK zero-alloc verification under load.** Existing benches
  assert 0 allocs on the hot path; extend to the H2 encoder write side
  and the QPACK encoder once H3 ships. Gap-check against nginx's
  per-request byte count on the wire.
- **Scheduler + GC interaction.** GC pauses are 16–29 µs today. Under
  the burst scenario that's a plausible contributor to the 18 ms worst
  case. Investigate GOGC tuning, per-worker heap sizing (SO_REUSEPORT
  fork already isolates heaps), and arenas for request-scoped
  allocations that currently survive a tick.
- **Large-upload streaming headroom.** tachyon already beats nginx by
  10× on 64 KB upload worst-case, but raw rps is still bounded by the
  copy loop. SPLICE for request bodies upstream, SEND_ZC for responses
  downstream, and a shared uring SQ for both directions should lift the
  POST-heavy rps number.

Risks: most items touch the hot path — one wrong move regresses the
143k plain-GET number that's already competitive with Pingora.
Mitigation: every change ships behind the existing allocation-budget
assertions in generated benchmarks; PRs that blow a budget fail CI.

## Sequencing

A and C are mostly software. B is hardware-dependent and slower. D
composes with all three. A reasonable order:

1. Track A first milestone: learned load balancer shipped behind a
   feature flag, with benchmark numbers vs p2c-EWMA.
2. Track D first milestone: coarse-mode deadline propagation and
   deadline-aware retry budgets (basic retry budget shipped; deadline
   awareness is the next step).
3. Track C first milestone: verified HTTP/1 parser and PQ-TLS default.
4. Track B first milestone: XDP fast path for the simplest DSL rules.

Each track has independent value; none blocks the others.

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

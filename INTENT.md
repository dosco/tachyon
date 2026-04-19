# Plan: INTENT.md — deep design for tachyon's intent-compiled filter system

## Context

Tachyon is a Go L7 reverse proxy (`/Users/vr/src/tachyon`) competing with
Nginx, Pingora, and Traefik. It has no extensibility system today —
`internal/proxy/h1_handler.go` is monolithic, `internal/router/match.go`
is the only decision point, nothing accepts user-written filters or
middleware. Users can only extend tachyon by editing Go source.

In prior discussion we landed on "intent-compiled filters" as tachyon's
extensibility story: users write typed declarative *intent*, an
authoring-time toolchain emits deterministic native Go, the Go gets
linked into a specialized tachyon binary. No runtime interpreter, no
WASM VM, no ABI boundary. The fast path is always native code.

The user now wants INTENT.md that goes deeper on this design,
specifically by grounding it in what people actually use Nginx config,
OpenResty Lua, Pingora filters, and Traefik middleware for. The doc
must cover the *use cases* first, then show that the intent language
actually covers them, then walk through the compile pipeline and
authoring workflow.

This plan describes the structure, depth, and worked examples of the
INTENT.md file to be created at the repo root
(`/Users/vr/src/tachyon/INTENT.md`).

## Style constraints (from existing docs)

- ~70 char line wrap
- H2 / H3 headings, single-dash bullets, no emoji
- Prose-heavy with short code blocks; backticks for inline identifiers
- Terse, direct, addresses reader as peer ("We bet that...")
- Reference existing files with repo-relative paths

## Target document outline

### 1. Thesis (tight opening, ~15 lines)

Every incumbent in this space assumes a human reads a docs website and
writes config. Nginx has a reference manual. Envoy has schemas on
envoyproxy.io. Traefik has a middleware catalog on traefik.io. Pingora
has rustdoc. Lua extensions live wherever OpenResty publishes them.
The config is written by a human with a browser tab open.

Tachyon is **LLM-first**: the agent is the author, the binary is the
reference. Every grammar rule, every primitive, every predicate, every
error message is emitted by the `tachyon` CLI in a form designed for
a coding agent (Claude Code, Codex, Cursor) to introspect, draft,
lint, compile, and replay-test without ever opening a website.

That reframes what the extensibility layer is for. Nginx's config
exists because a human writes it; ours exists because an agent writes
it, and the agent's source of truth is the binary in front of them,
version-locked. Three consequences fall out:

1. **Self-documenting CLI** (§12), with an optional MCP server mode
   for pre-configured agents. No stale docs, no drift between "what
   tachyon supports" and "what the website says."
2. **Typed, total authoring surface** small enough for a small model
   to get right on the first pass with high probability.
3. **Native runtime**: intent compiles to Go and links into a
   specialized tachyon binary, so extensibility costs nothing at
   runtime. (This pattern is known — Caddy modules do it — but pairs
   uniquely well with an LLM-first authoring flow because the agent
   can rebuild the binary, not just the config.)

Details: §§11–13.

### 2. What people actually use their proxy config for

A concrete survey grouped by capability, so we can show intent covers
each. This section is the "think deeper" the user asked for.

Groups to enumerate (with 2–5 examples each):

- **Routing.** Host, path, header, cookie, query, method; longest-prefix
  and regex; priority; host wildcards.
- **Header manipulation.** Add/remove/rewrite req+resp; X-Forwarded-*
  handling; trace-context propagation; strip cookies crossing service
  boundaries; CORS and security headers.
- **URL rewriting.** Path rewrites, strip-prefix, add-prefix; HTTP→HTTPS;
  www/apex canonicalization; trailing slash.
- **Auth.** Basic, API key (header/query), JWT verify (sig, exp, aud,
  iss), mTLS, HMAC, IP allow/deny, `auth_request` subrequests, OAuth
  introspection.
- **Rate limiting.** Per IP, per user (from JWT claim / cookie / header),
  per API key, per route; burst + sustained; distributed counters; 429
  + Retry-After; different limits per tier.
- **Load balancing.** Round-robin, least-conn, p2c-EWMA, consistent
  hash, weighted, session affinity; active + passive health checks;
  retry-to-next-upstream.
- **Caching.** Response cache with TTL; cache-key customization (vary by
  header, ignore query params); stale-while-revalidate; purge;
  bypass-on-auth.
- **TLS.** SNI routing; per-tenant certs; client cert auth; OCSP
  stapling; HTTP→HTTPS redirect; HSTS; version/cipher restriction.
- **WAF / security.** Block by UA / country / IP lists; body size
  limits; path traversal; SQLi/XSS heuristics; bot detection.
- **Observability.** Access logs, structured; slow-request logs;
  metrics; trace context propagation; log sampling.
- **Traffic management.** A/B splits; canary %; blue/green; shadow
  mirror; feature flags via header; maintenance mode.
- **Content transformation.** gzip/brotli/zstd; response body rewrite;
  template injection.
- **Protocol.** WebSocket, gRPC, HTTP/3; PROXY protocol; keepalive
  tuning.

What OpenResty Lua gets used for that plain config can't do:
- Custom auth flows with external calls (JWT + DB lookup)
- Rate limiting against Redis
- Dynamic upstream lookup (K8s/Consul)
- Request body JSON rewriting
- Correlation ID injection with business logic
- Call multiple upstreams and merge
- Custom WAF rules

What Pingora phase callbacks get customized:
- `early_request_filter`, `request_filter`, `upstream_peer`,
  `upstream_request_filter`, `response_filter`, `logging`.

What Traefik middlewares cover (dozens):
- `forwardAuth`, `ipAllowList`, `rateLimit`, `circuitBreaker`, `retry`,
  `compress`, `headers`, `redirectScheme`, `stripPrefix`, `chain`.

### 3. The intent language

Keep the language small and typed. Show grammar at a glance, then
primitives, then worked examples.

#### 3.1 Grammar sketch

```
policy NAME {
  scope    <predicate>           # restricts where this applies
  when     <predicate>           # conditional
  apply    <action>              # primary action
  else     <action>?             # alternative branch
  on_error <action>?             # failure path
  priority INT                   # composition order
}

filter NAME {                    # multi-step pipeline form
  on request  { <step>; ... }
  on response { <step>; ... }
  on error    { <step>; ... }
}
```

Predicates are total Boolean expressions over typed request/response
attributes. Actions are chosen from a closed palette. No loops, no
unbounded recursion, no arbitrary I/O.

#### 3.2 Predicate vocabulary

Request: `req.host`, `req.path`, `req.method`, `req.header("X")`,
`req.cookie("X")`, `req.query("X")`, `req.ip`, `req.tls.sni`,
`req.tls.client_cert.cn`, `req.jwt.claim("X")`, `req.body.json("$.field")`.

Response: `resp.status`, `resp.header("X")`, `resp.size`.

Env: `now()`, `geoip(req.ip).country`, `deadline_remaining()`.

Operators: `==`, `!=`, `<`, `<=`, `>`, `>=`, `&&`, `||`, `!`,
`in_cidr(...)`, `matches(regex)` (anchored, bounded), `starts_with`,
`ends_with`, `in [...]`.

#### 3.3 Action palette

Grouped into the use-case categories from §2 so the mapping is obvious.

- **Routing**: `route_to(pool)`, `mirror_to(pool, sample: 0.01)`,
  `split { weight: {pool-a: 90, pool-b: 10} }`.
- **Headers**: `set_header("X", expr)`, `remove_header("X")`,
  `append_header("X", expr)`, `copy_header(from, to)`.
- **Rewriting**: `rewrite_path(template)`, `rewrite_host(expr)`,
  `redirect(code, url)`, `strip_prefix("/api")`, `add_prefix("/v2")`.
- **Auth**: `auth_basic(realm, users: secret_ref)`,
  `auth_api_key(source: header("X-Api-Key"), store: kv_ref)`,
  `auth_jwt(iss, aud, jwks_ref)`, `auth_mtls(ca_ref)`,
  `auth_hmac(secret_ref, header: "X-Sig")`,
  `auth_subrequest(url, forward_headers: [...])`,
  `allow_cidrs([...])`, `deny_cidrs([...])`.
- **Rate limit**: `rate_limit(rps, burst, key: expr, scope: local|shared)`,
  `concurrent_limit(max, key: expr)`.
- **Cache**: `cache_get(key: expr, ttl)`, `cache_put(ttl,
  vary: [...])`, `cache_bypass_if(predicate)`.
- **TLS**: handled at listener config, but intent can
  `require_mtls()`, `require_min_tls(1.3)`.
- **WAF**: `block_if(predicate)`, `limit_body(bytes)`,
  `limit_header_size(bytes)`.
- **Observability**: `log(fields: {...}, level)`,
  `sample(rate)`, `trace_tag("k", expr)`, `emit_metric(name, value, labels)`.
- **Traffic**: `canary(to: pool, percent: 5)`,
  `shadow(to: pool, sample: 0.1)`, `maintenance(respond: 503)`.
- **Transform**: `compress(algo: "zstd", min_size)`,
  `response_rewrite(regex, replacement)`.
- **Control flow**: `respond(status, body?, headers?)`, `drop`,
  `retry(max, backoff)`, `timeout(duration)`.

Every action is fuel-accounted (Track D) and deadline-aware.

#### 3.4 State

Bounded, statically sized state primitives:

- `counter NAME { window: 1m, buckets: 60 }` — for rate limiting.
- `map NAME { key: type, value: type, max_entries: 10000, ttl: 5m }` —
  bounded, LRU-evicted.
- `shared_counter NAME { backend: redis_ref }` — for distributed cases,
  still declared not ad-hoc.

Every state object is declared at the top level so the compiler knows
the full memory footprint before generating code.

#### 3.5 External calls

`http_lookup` is the only primitive that leaves the proxy:

```
external auth_service {
  url         "https://authz.internal/check"
  timeout     20ms
  max_retries 1
  fallback    allow | deny | predicate
}
```

Required `fallback` means the generated code always has a terminating
behavior even when the call fails — no cascading timeouts.

### 4. Worked examples covering the use-case survey

Each example is ~10–20 lines of intent showing it reads naturally,
followed by one-line notes on what compiles out.

- **Example 1 — JWT auth + per-user rate limit + trace propagation.**
  Covers auth, rate limit, headers, observability.
- **Example 2 — Canary with header override + shadow.** Covers traffic
  management.
- **Example 3 — Cache with vary + stale-while-revalidate bypass on
  auth.** Covers caching.
- **Example 4 — mTLS + per-tenant route to backend + strip sensitive
  headers.** Covers TLS, routing, header manipulation.
- **Example 5 — External auth subrequest with fallback-deny + log
  sampling.** Covers auth + observability.
- **Example 6 — Body size limit + UA block + CIDR deny + 429 with
  Retry-After.** Covers WAF.

These six cover >80% of what real Nginx + OpenResty configs do.

### 5. The compile pipeline

```
intent/*.intent
      │   parser, types, totality check, fuel bounds
      ▼
typed AST
      │   peephole opt, match-tree construction, DCE
      ▼
optimized IR
      │   go/ast + dave/jennifer codegen
      ▼
generated/ (Go package: filters.go, matchers.go, state.go)
      │   go build -pgo=prod.profile
      ▼
specialized tachyon binary
```

Integration points in the existing codebase:
- Generated code registers with a new `Filter` hook slot added to
  `internal/proxy/h1_handler.go` (`ServeConn` at line 48).
- Router (`internal/router/match.go`) already picks upstream; filters
  run around routing via pre/post slots.
- Config (`internal/router/config.go`) gains a `filters:` key listing
  which compiled filter-set to activate per route.

Build time: target <10s for a reasonable intent set. Matches Go's
compile speed; would be painful in Rust.

#### Performance contract for generated code

This is non-negotiable. Tachyon's entire thesis is beating Nginx and
Pingora on throughput and p99. If generated filters slow the hot path,
intent is a gimmick. The codegen must emit Go that looks and performs
like the hand-written hot path in `http1/` and `internal/proxy/`.

Rules the codegen *must* obey:

- **Zero per-request heap allocations** on the common path. State
  objects (counters, bounded maps, match contexts) live in pooled
  structs obtained via `sync.Pool` or per-worker arenas reusing the
  existing `buf/` allocator. Bench-gated.
- **No reflection.** Ever. All generated code uses concrete types.
- **No interface dispatch on the hot path.** Filters are concrete
  structs with generated methods. The handler calls them directly, not
  through an interface slice, so the Go compiler can inline.
- **Static match trees.** Route and predicate matching is a flat
  switch/compiled trie generated from the intent, not a map lookup at
  request time. Constant strings are interned at build time. Header
  name comparison uses `bytes.Equal` on pre-lowercased keys, never
  `strings.EqualFold`.
- **No per-request map allocations.** Where a map is logically needed
  (e.g. declared `map NAME {...}` state), the compiler emits a
  statically sized open-addressing hash with pre-allocated backing
  arrays. `max_entries` from §3.4 determines capacity at codegen time.
- **Compiled regex only when unavoidable.** Grammar predicates like
  `starts_with`, `ends_with`, `in [...]` compile to direct byte checks.
  `matches(regex)` exists but flags the primitive as "slow path" in
  the codegen report so users see the cost.
- **Escape-analysis friendly.** Generated functions accept pointer
  receivers to pooled structs, return primitive values or write into
  caller-provided buffers. No closures capturing per-request state.
  No variadics.
- **Small functions.** Each primitive call site is a short inline-able
  function; the Go compiler's inliner is the target. The codegen
  checker rejects emitted functions whose budget exceeds the inliner's
  threshold on the hot path.
- **PGO is automatic, with three profile sources.** See the PGO
  subsection below. The build always takes a `-pgo=...` input; the
  question is only *which* profile.

Compiler-enforced checks (codegen gate, runs on every `tachyon intent
compile`):

- `go test -run=NONE -bench=BenchmarkGenerated -benchmem` must report
  **0 allocs/op** for the generated hot-path benchmarks.
- A synthetic microbench compares the generated filter chain against a
  hand-written equivalent for a canonical workload; the regression
  threshold is **≤10ns p50 delta, ≤0 allocs delta**. Over budget
  fails the build.
- Emit a codegen report: per-primitive ns estimate, inline-or-not,
  allocs, escape-analysis summary. Agents can read the report via
  `tachyon intent compile --report=json` (§12) and iterate.

Why this is plausibly achievable in Go: the rest of tachyon already
proves it. `http1/` has a zero-alloc parser, `buf/` has a pooled
allocator, HPACK is zero-alloc. Generated code imports those same
packages and follows the same patterns — because the codegen is
written by us, we can guarantee it does.

Comparison discipline: every benchmark in `bench/` must be reproducible
against Nginx (fasthttp and stdlib `net/http` reverse-proxy baselines
already exist in tree per README.md) with and without intent filters
active. An intent-enabled tachyon that loses to Nginx on the same
hardware is a bug, not a tradeoff.

#### PGO: how the profile is generated

The codegen does not just emit filter code — it emits a matching
benchmark package that exercises every policy, and uses that
benchmark to produce a PGO profile before the final `go build`. This
answers the "where does `prod.profile` come from on day one?"
question without requiring users to do anything manual.

Three profile sources, in preference order:

1. **Prod-captured** (best). A real pprof captured from a running
   tachyon under representative load. `tachyon intent compile
   --pgo=/path/to/prod.pprof`. Standard Go PGO workflow.
2. **Replay-captured** (second best). Tachyon already records and
   replays traffic (Track C and §12's `tachyon traffic record` /
   `traffic replay`). Replay the last N hours against the candidate
   binary, capture pprof, feed to the build:
   `tachyon intent compile --pgo=@replay:last-24h`. No risk to prod,
   no wait for live data.
3. **Synthetic** (day-one fallback, always available). The codegen
   emits `generated/filters_bench_test.go` alongside the filter code.
   Every primitive ships with a bench template; the whole-filter
   bench stitches the per-primitive benches together weighted by the
   optional `traffic_hint` block in the intent, or uniformly if no
   hint is given. `tachyon intent compile --pgo=synthetic` runs the
   bench under pprof, feeds the resulting profile to `go build`.

`--pgo=auto` (the default) picks the best available: prod profile if
one is registered, else latest replay-derived profile, else
synthetic. The chosen source is printed in the compile report so the
agent can see what it got.

Minimum viable `traffic_hint`:

```
traffic_hint {
  policy /api/v1/*       share: 0.75
  policy /api/v1/admin/* share: 0.05  # high security, low traffic
  policy /static/*       share: 0.20
}
```

Entirely optional — without it the synthetic bench weights all
policies equally and we get a generic-but-valid profile. With it, the
synthesized bench reflects real-world distribution and PGO does its
job more effectively.

The generated bench file also doubles as the zero-alloc gate from the
performance contract: it runs under `go test -bench=...
-benchmem` and any regression trips the build. So one artifact
(generated bench) serves two purposes — PGO input and regression
guard — which keeps the pipeline small.

New CLI commands this implies (added to §12):

- `tachyon intent profile [--synthetic|--from-replay=<artifact>|
  --from-pprof=<file>]` — materialize a profile on demand, emit it
  to `.tachyon/pgo/<hash>.pprof`, register it as the default for
  subsequent compiles.
- `tachyon intent compile --pgo=auto|synthetic|@replay:...|path`
  — explicit control when the agent wants something other than the
  default.
- `tachyon intent compile --report=json` includes the chosen profile
  source, the bench numbers it produced, and the PGO-applied
  optimizations delta (inlines taken, branches specialized) so the
  agent can see what PGO actually did.

### 6. What intent intentionally cannot express

Explicitly listed non-goals so users know when to reach for the escape
hatch:

- Arbitrary computation over request bodies beyond declared JSON-path
  reads.
- Multi-request coordination (distributed transactions).
- New protocol implementations (WebSocket sub-protocols etc).

### 7. The escape hatch — Go filter plugin

For the 1%: a compile-time Go plugin slot. User writes a Go package
implementing a small interface, drops it into `plugins/`, rebuilds. Not
a runtime plugin (don't use Go's `plugin`). Same pipeline — the user's
Go is just linked in alongside the generated Go. Interface is minimal:

```go
type Filter interface {
    OnRequest(ctx *FilterCtx) error
    OnResponse(ctx *FilterCtx) error
}
```

`FilterCtx` exposes fuel-accounted primitives. Plugins that blow fuel
budgets are killed deterministically like any other filter.

### 8. Authoring workflow — conversational by design

The sci-fi part. CLI flow:

```
$ tachyon intent propose "rate-limit /api/* per user;
                           enterprise tenants get 5x headroom"
→ proposed intent/ratelimit.intent    (28 lines)
→ proposed generated/ratelimit.go     (187 lines)
→ perf delta on replayed prod traffic (from Track C):
    p50: +40ns   p99: unchanged   allocs/req: +0
→ apply / refine / cancel?
```

The model drafts the intent; the human reviews the intent *and* the
generated Go *and* the perf delta on real replayed traffic. Runtime is
still pure compiled native code — no LLM, no WASM, no interpreter.

The intent language's small typed surface is precisely what makes it
LLM-authorable reliably. Free-form English is too ambiguous;
general-purpose code is too unbounded. Intent is the narrow middle.

#### What "really good at being an LLM target" actually requires

Models fail at DSLs in predictable ways: they invent primitives that
sound plausible, they guess at argument order, they silently coerce
types, they miss required fields, they recreate the same rule two
different ways. Every design decision below is aimed at one of those
failure modes.

**Closed vocabulary, one-obvious-way.** Every primitive, predicate,
and operator is named and enumerated in `tachyon intent primitives`.
No plugin escape hatch from within the DSL — if it's not listed, it
doesn't exist. For each use case there is exactly one canonical
primitive; the linter rejects equivalent rewrites and suggests the
canonical form. Two ways to express the same rule is how models
produce inconsistent code across a repo.

**Keyword-oriented syntax over punctuation-dense.** `when ... apply
... else ... on_error ...` reads the way the agent is already
thinking. No nested-brace object literals, no sigils, no
whitespace-sensitive layout. Keywords are more resilient to
tokenization than punctuation-heavy grammars.

**Block ordering mirrors intent flow.** `scope → when → apply → else
→ on_error → priority`. The same order every time. The linter
reorders fields into canonical form automatically so diffs stay
clean.

**No implicit conversions, no default coercions.** `rate_limit(rps:
"1000")` is a type error, not a silent coercion. Models coerce by
default; the DSL doesn't.

**Strongly typed attributes with dotted accessors.** Predicates are
always of the form `req.header("X")`, `req.jwt.claim("sub")`,
`resp.status`. Actions are always verb snake_case. The accessor
vs. verb split means the agent can tell predicate-context from
action-context at a glance.

**Naming discipline.** snake_case everywhere, no camelCase, no
abbreviations that aren't already industry-standard (`rps`, `ttl`,
`cidr` ok; `rl`, `rhl` not). Direction is in the name:
`cache_get` / `cache_put`, `set_header` / `remove_header`. No
overloaded `cache` that dispatches on arguments.

**Exhaustive, stable error catalog.** Every lint and compile error has
a stable code (`E042`, `E103`) and a structured fix hint. The agent
looks up the code programmatically via `tachyon intent errors --json`
and applies the fix without re-asking the model. Free-form error
prose is banned; every message has a code and a fix template.

**Totality + type check catches it all at lint time.** No runtime
config errors. If `tachyon intent lint` passes, the filter will load.
This lets the agent trust the feedback loop: lint → fix → lint → done.
Without totality the agent learns to be paranoid and hedges
everything, which produces bloated intents.

**Canonical examples per primitive.** `tachyon intent primitive
rate_limit --examples` returns 3–5 working snippets covering common
patterns (per-IP, per-JWT-claim, burst-shaped, shared across workers).
The agent reads the example that matches its task and adapts, rather
than synthesizing from grammar alone.

**Scaffolds for the top 20 patterns.** `tachyon intent scaffold
rate-limit-per-user` emits a working starter intent. The agent calls
scaffolds before generating from scratch; scaffold output is
guaranteed to lint and compile. Covers the Pareto set of use cases
from §2.

**Agent-repairable errors.** `tachyon intent repair <file>` reads the
lint output and proposes a patch. For deterministic cases (missing
fallback, wrong arity, wrong type) the repair is exact and doesn't
need the model. For ambiguous cases the repair returns a structured
"needs decision" with the options enumerated, so the model chooses
between N typed alternatives rather than generating free-form.

**Comments as first-class signal.** `# ...` comments survive codegen
and appear in the generated Go. The agent can annotate its reasoning
in the intent; reviewers see the rationale in both the source of
truth and the compiled artifact. Also useful for the `tachyon explain`
trace (§12) which can cite comments.

**Grammar is small and diffable.** Target: the full grammar fits in
~200 lines of EBNF. `tachyon intent grammar --ebnf` is under a
couple of KB. The agent can hold the whole grammar in its working
memory; no paging through reference pages.

**Versioned grammar with typed migrations.** Every intent file
declares `intent_version: "0.1"`. Grammar changes come with an
automated migration that `tachyon intent migrate` applies. The agent
never has to guess which version it's writing for.

**No semantic redundancy.** The linter enforces a canonical form and
rejects equivalent rewrites. Two agents working the same repo produce
the same file.

**Closed set of external effect kinds.** `external` blocks have a
finite `kind` enum (`openai_compatible`, `http_json`, `redis`, ...).
No free-form pluggable adapters. Fewer knobs means fewer model
hallucinations about what's possible.

These rules together make the intent language something a small model
can get right on the first pass with high probability — which is the
actual bar, not "a large model can probably do it with enough
retries." Everything about the authoring loop (grammar introspection,
scaffolds, repair, typed errors) is designed around that bar.

### 9. Comparison table

| System          | Runtime cost    | Safety    | Self-documenting CLI | Agent-first authoring | Retargetable |
|-----------------|-----------------|-----------|----------------------|-----------------------|--------------|
| Nginx + Lua     | interpreter+GC  | sandbox   | no                   | no                    | no           |
| Envoy + WASM    | VM + ABI        | sandbox   | partial (schema file)| no                    | no           |
| Pingora (Rust)  | native          | unsafe ok | no                   | no                    | no           |
| Traefik         | native          | fixed     | partial              | no                    | no           |
| Caddy modules   | native          | total     | partial              | no                    | no           |
| **Tachyon**     | **native**      | **total** | **yes (CLI, MCP opt)** | **yes**             | **yes**      |

The column that actually isn't crowded is "self-documenting CLI."
No existing proxy makes its binary the authoritative reference a
coding agent can introspect. That is the real wedge.

### 10. Retargeting

The same intent file can recompile to different targets as tachyon
evolves:

- Today: Go userspace filter.
- Track B lands: simple intents (CIDR deny, header tags) compile to
  eBPF, executed in the kernel.
- SmartNIC eventually: the same intents compile to NIC bytecode.

Users don't rewrite. The compiler picks the target (or user overrides)
based on which primitives the intent uses.

### 11. Intent lifecycle via external coding agents

We do not build an authoring copilot into tachyon. Every user already
has a capable coding agent — Claude Code, Codex, Cursor, whatever comes
next — and those agents are better at reading, writing, and reviewing
intent than anything we'd ship ourselves. Tachyon's job is to be the
*best target* for those agents via the self-documenting CLI in §12
(and the optional MCP server mode for pre-configured agents).

Because the CLI emits its own grammar, primitive catalog, error codes,
and replay tooling in machine-readable form, the agent does not need
our website, our docs, or any out-of-band reference material to do
good work. That unlocks the lifecycle below. Tachyon provides the
interface and primitives; the agent provides the intelligence.

- **Intent synthesis from traffic.** Point tachyon at recorded traffic
  and an objective ("reduce 429s from tenant X", "cut inference spend
  30%", "block the attack pattern from last Tuesday"). The model
  proposes intent changes, each one scored against the recorded traffic
  for expected effect. Humans approve; compiler emits code.
- **Intent-from-incident.** Given a post-mortem timestamp, regenerate a
  guard intent that would have caught or contained the incident, and
  verify by replaying the incident window.
- **Intent repair on upgrade.** When a tachyon release deprecates a
  primitive or changes a default, a model rewrites affected intents and
  produces a PR; CI runs the replay test.
- **Intent critique.** On every change, a reviewer model reads the
  intent and flags missing guards: no timeout, no rate limit on new
  route, no fallback on external call, no PII redaction on a
  user-body-bearing path.
- **Drift detection.** Continuous model watches intent + live traffic
  and flags stale policies: "this rate limit hasn't triggered in 6
  months", "this route's traffic shape changed and the cache keys are
  missing `x-tenant`", "this JWT claim check is effectively
  unconditional — every request has it".
- **Natural-language runtime debug.** "Why did this request get a 429?"
  walks the recorded trace + intent source and answers in prose, with
  clickable references to the specific policy lines.
- **Red-team simulation.** Before deploy, an adversarial model
  generates traffic aimed at bypassing the new intents; the simulator
  reports misses. This is fuzzing with taste.
- **Cost envelope preview.** "This policy adds $X/day in inference,
  uses Y MB of state memory, increases p99 by Zms on replayed
  traffic." Shown at propose-time, not discovered in prod.

These lifecycle pieces share one design rule: **the external agent
never writes to prod directly.** It writes to the intent source; the
intent compiler writes the Go; the deploy pipeline rolls it out. Every
change is reviewable text and checked-in native code.

### 12. The self-documenting CLI (the actual differentiator)

This is the section to emphasize. No proxy in this space was designed
with the agent as the primary author. `kubectl explain` and
`terraform providers schema -json` partially do it for adjacent tools;
nothing in the proxy world does. That's the real wedge.

Design rule: **the binary is the reference.** A coding agent with only
the `tachyon` binary on PATH should be able to author, lint, compile,
and replay-test an intent without ever reading a website. All discovery
is CLI-emitted, version-locked, and machine-readable.

CLI is the primary surface because it requires zero configuration —
drop the binary on PATH and Claude Code / Codex / Cursor can shell out
immediately. MCP is an optional second surface for users who have
already wired up MCP in their agent; it exposes the same commands as
typed tools. Neither supersedes the other.

#### Introspection commands

- `tachyon intent grammar [--json|--ebnf]` — the full grammar. Version
  stamped. Agents fetch this at the start of a session.
- `tachyon intent primitives [--json]` — list every action primitive
  with its type signature, required fields, fuel estimate, and notes.
- `tachyon intent primitive <name> [--json]` — deep details for one:
  examples, fallback requirements, compatible predicates, error codes.
- `tachyon intent predicates [--json]` — request/response/env attribute
  catalog with types.
- `tachyon intent schema` — JSON Schema for intent files (editor and
  agent validation).
- `tachyon intent examples [--primitive=X]` — curated, compilable
  examples; filterable by primitive so an agent can see working code
  for exactly what it is about to write.
- `tachyon intent errors [--json]` — every error code the lint/compile
  step can emit, with structured fix hints (e.g. "E042: missing
  fallback. Add `fallback: allow|deny|pass_through` to the external
  block").

#### Authoring commands

- `tachyon intent lint <file> [--json]` — structured diagnostics with
  precise spans and machine-consumable fix hints. No free-form prose.
- `tachyon intent compile <files...>` — codegen + `go build`. Reports
  codegen size, fuel estimate per policy, build time, binary size
  delta.
- `tachyon intent diff <a> <b>` — semantic diff: "policy X added branch
  for header Y", "rate limit raised 100→1000". Not textual.
- `tachyon intent simulate <files> --traffic=<artifact>` — dry run
  against recorded traffic, no upstream calls.

#### Operating commands

- `tachyon traffic record --route=... --duration=...` — capture live
  traffic to a replay artifact.
- `tachyon traffic replay <artifact> --intent=<files>` — replay with
  candidate intent; report status histograms, 429 rates, cache hit
  rates, per-policy fire counts, latency deltas.
- `tachyon traffic query <artifact> <expr>` — slice replay data for
  agent questions ("which requests would trip policy X?").
- `tachyon explain <request_id>` — full trace for a captured request:
  which policies matched, which fired, what each primitive returned,
  fuel consumed per step.

#### Optional MCP server mode

`tachyon mcp` runs tachyon as a Model Context Protocol server over
stdio (the one transport Claude Code, Codex, and Cursor all support
without extra configuration). We deliberately do not ship SSE or HTTP
transports — stdio is enough, and extra surfaces just give users more
to misconfigure. Every command above is available as a typed MCP tool:
`intent.grammar`, `intent.lint`, `intent.compile`, `traffic.replay`,
`explain.request`. This is a convenience for users whose agents are
already MCP-configured; users whose agents are not simply shell out to
the CLI. Both surfaces expose identical functionality and the same
JSON schemas. CLI is primary; MCP is optional.

#### Structural rules that make this work

- Every command supports `--json` and emits stable, documented schemas.
- Standard `--help` on every command returns structured usage an agent
  can parse as a fallback before using `--json`.
- Every error has a stable error code, a span, and a machine-readable
  fix hint. No human-only prose-only errors.
- Schemas and grammar are versioned; `tachyon intent grammar` stamps
  the tachyon version so agents can detect drift.
- The CLI never writes to prod. It writes intent files, generated Go,
  and replay reports. Humans or CI do deploys.

Everything the lifecycle in §11 needs is some composition of these
commands. We are the interface; someone else's agent is the
intelligence.

### 13. Hyperparameter tuning via the same infrastructure

The replay + benchmark + CLI stack we're building for intent is also a
general evaluation harness. Once we have it, tuning tachyon's own
operational knobs — `idle_per_host`, connect/read/write/idle timeouts,
io_uring buffer sizes, HTTP/2 max-concurrent-streams, HPACK table size,
TLS session cache, keepalive, worker count, rate-limit defaults — is
the same problem as tuning filter code, just with a different search
space.

Every incumbent leaves this to humans with a docs page and a stopwatch
(and the user's own note is right: there is no single scalar objective
for a proxy; it's always multi-objective, trading throughput, p99,
memory, error rate, and stability). The agent-plus-replay loop is a
natural fit for that trade-off because it lets the user declare the
objective once and drive an automated search against recorded traffic.

#### What the user declares

An `objective.tachyon` file (or CLI flags) specifying the goal:

```
objective {
  minimize  metric.latency.p99
  subject_to {
    metric.memory.rss      <= 4GB
    metric.error_rate       <  0.001
    metric.goodput          >= baseline * 0.98
    metric.latency.p50      <= baseline + 5ms
  }
  workload    replay:@replay:last-24h     # or synthetic:mix-A
  budget      trials: 200, wall: 30min
}
```

The constraint set is the honest way to handle multi-objective. Users
pick what to minimize and what must stay within bounds. No single
magic score; the CLI surfaces a Pareto frontier if one exists.

#### What tachyon tunes

A declared knob catalog emitted by `tachyon knobs [--json]` — the same
self-documentation pattern as intent primitives. Each knob has a type,
a range, a default, a description, and a link to the code that uses
it. Example entries:

- `upstream.idle_per_host` int [1, 1024] default 16
- `upstream.connect_timeout` duration [10ms, 30s] default 1s
- `http2.max_concurrent_streams` int [16, 4096] default 250
- `http2.hpack_table_size` int [1024, 65536] default 4096
- `iouring.provided_buffer_size` int [4KB, 256KB] default 16KB
- `workers.count` int [1, 256] default NumCPU
- `scheduler.edf_tie_breaker` enum default "arrival"

The catalog is derived from the Go source — every tunable is annotated
(`// tachyon:knob name=... range=... default=...`) and
`tachyon knobs` emits the live catalog. No separate docs to drift.

#### The tuning loop (no external optimizer required)

The search algorithm is not a library dependency. It's either the
coding agent itself — proposing candidates based on knob catalog,
objective, and prior-trial metrics — or a small built-in heuristic
that ships with tachyon in ~150 lines of Go. No optuna, no Ax, no
GP libraries.

The harness exposes trial evaluation as CLI commands so anything can
drive it:

- `tachyon tune trial --config=candidate.yaml --workload=<replay>` —
  build tachyon with the candidate knobs, run replay, emit metrics as
  JSON. Idempotent, no state.
- `tachyon tune history [--json]` — return all trials so far for this
  tuning session with their configs and metrics.
- `tachyon tune knobs` — same self-documented knob catalog described
  above.
- `tachyon tune recommend [--pareto]` — among the trials run so far,
  return the best feasible config per the objective, or the Pareto
  frontier.

**Default driver: the agent.** Claude Code (or whatever is on hand)
reads knobs + objective + history, proposes the next trial config,
calls `tachyon tune trial`, reads metrics, reasons about what to try
next, proposes again. The LLM is the surrogate model; its prior over
"bigger buffer → more memory, shorter timeout → more false errors,
more workers → more CPU contention past NumCPU" is exactly the domain
knowledge a Gaussian process would have to relearn from scratch. The
agent can also read traces (`tachyon explain`) to form hypotheses and
targeted interventions, which BO cannot.

This is the same agent-as-intelligence + tachyon-as-harness pattern
we use for intent authoring. It requires zero net-new tachyon code
beyond the trial harness.

**Fallback driver: built-in, dependency-free.** For CI pipelines or
cases where no agent is around, `tachyon tune auto
--objective=objective.tachyon --budget=N` runs a simple built-in
search with no external libraries:

1. **Latin hypercube sampling** for the first ~sqrt(budget) trials —
   deterministic coverage of the search space. ~30 lines.
2. **Coordinate descent** over the remaining budget — pick the knob
   with the strongest observed effect on the objective, sweep a few
   values holding others fixed, move to the next knob. ~80 lines.
3. **Best-feasible selection** at the end — cheapest correct behavior,
   ~10 lines.

Total: ~150 lines of Go, no dependencies, understandable at a glance,
deterministic for reproducibility. Not as sample-efficient as a real
BO, but for ≤15 knobs and ≤200 trials the gap is small and it never
depends on anything we'd have to ship or license.

Users can pick their driver:

- `tachyon tune auto` — built-in, for CI.
- Agent-driven — any coding agent calling the `tachyon tune trial/
  history/recommend` commands or the matching MCP tools.
- Both are compatible: an agent can seed the search with the built-in
  Latin hypercube output, then take over for the fine-tuning phase.

Output:

```
tune complete: 187 trials in 24m.
recommended config → tuned.yaml
pareto table: 8 non-dominated configs (see tuned-pareto.json)

delta vs current prod config:
  p99 latency:     -22% (-34ms)
  goodput:         +4%
  memory.rss:      +8% (within 4GB bound)
  error_rate:      unchanged
```

The CLI never writes to prod. It writes `tuned.yaml`. Humans or CI
promote.

#### Why this is almost free to build

- **Evaluation harness:** replay + bench from §5/§12 — already present.
- **Fitness function:** metrics already emitted by tachyon
  (`metrics/` package per README.md).
- **Search:** external agent via CLI/MCP, or ~150 lines of built-in
  Latin hypercube + coordinate descent. No library dependency.
- **Knob catalog:** `// tachyon:knob` comments + a generator — same
  pattern as the intent grammar.

The only net-new work is the trial harness, the knob annotations, and
the tiny built-in fallback. Everything else is reuse.

#### Composition with Track A

Track A (learned everything) tunes *online* against live traffic;
`tachyon tune` tunes *offline* against recorded traffic. Both inform
the same knob catalog. Offline tuning sets the priors; online learning
refines around them. The two are complementary, not redundant.

#### Honest limits

- Replay fidelity is the ceiling. If replay doesn't capture important
  failure modes (backend jitter, packet loss, cold-cache behavior)
  then offline tuning won't find them. Mitigation: declare worst-case
  replay slices (the slowest hour, the outage day) and require that
  recommended configs survive them too.
- Some knobs interact with the environment (NIC queue depth, TCP
  tuning, kernel version). Tuning surfaces the best config *for this
  replay*; prod-specific re-tuning is still valuable.
- Multi-objective search is only as good as the declared constraints.
  The honest failure mode is a user under-specifying and getting a
  config that wins the stated objective at the cost of something they
  forgot to constrain. Mitigation: the `recommended config` diff is
  always shown against the current prod config across *all* metrics,
  not just the objective, so regressions in unconstrained metrics are
  visible.

### 14. Open questions (honest caveats)

- How do we version intent across tachyon releases? Proposal:
  `intent_version: "0.1"` header on every file; compiler rejects
  unknown; migration tool handles upgrades.
- How do we test filters before prod? Proposal: `tachyon intent test`
  replays captured traffic against a candidate binary and asserts on
  generated logs/metrics.
- Do we need a "dry-run" mode where generated filters run but log
  actions instead of applying? Probably yes, default on for first
  deploy of any new intent.

## Files to create / reference

- **Create**: `/Users/vr/src/tachyon/INTENT.md` — the final deliverable.
- **Reference in the doc**: `internal/proxy/h1_handler.go` (line 48 —
  `ServeConn` is the integration point for pre/post-filter slots);
  `internal/router/config.go` (Config struct for the new `filters:`
  key); `internal/router/match.go` (routing decision); `ROADME.md`
  (Tracks A/B/C/D — intent is the natural Track E).

## Verification

INTENT.md is a design doc, not code. Verification:

1. Read back to check the ~70 char wrap and heading hierarchy match
   other docs in `docs/`.
2. Cross-reference every use-case category in §2 against the action
   palette in §3.3 — there must be at least one primitive per category
   or an explicit note in §6 that it's out of scope.
3. Each of the six worked examples in §4 compiles (mentally) against
   the grammar in §3.1 and the vocabulary in §3.2/§3.3.
4. Comparison table (§9) is factually defensible for each competitor.
5. The integration points referenced in §5 match the actual files
   surveyed by the Explore agent (h1_handler.go:48, router/config.go,
   router/match.go — all confirmed to exist).
6. The lifecycle pieces in §11 all flow agent → intent source →
   compiled Go → deploy. No path where an agent writes to prod directly.
7. Every CLI command in §12 is demonstrably drivable without external
   docs: grammar, primitives, predicates, schema, examples, errors,
   lint, compile, diff, simulate, record, replay, query, explain. All
   support both `--help` and `--json`. Optional MCP exposes the same
   surface.
8. The comparison table (§9) leads with "self-documenting CLI" as the
   column where tachyon is genuinely alone.
9. The performance contract in §5 is stated as non-negotiable and has
   executable gates: zero allocs/op in generated hot-path benches, and
   ≤10ns p50 delta vs hand-written equivalents, enforced on every
   `tachyon intent compile`. The doc explicitly claims intent-enabled
   tachyon must not lose to Nginx on the same hardware.
10. The §8 "really good at being an LLM target" rules are concrete and
    testable: closed vocabulary, keyword-oriented syntax, canonical
    form enforced by the linter, stable error codes, scaffolds per
    top-20 use case, `tachyon intent repair`, comments preserved
    through codegen, grammar under ~200 lines EBNF, versioned with
    automated migrations.
11. MCP server is stdio-only. No SSE or HTTP transports are described.
12. PGO is explained concretely: generated benchmark file per intent
    set, three profile sources (synthetic / replay / prod), `--pgo=auto`
    default, `tachyon intent profile` for explicit control, and the
    generated bench doubles as the zero-alloc regression gate.
13. Hyperparameter tuning in §13 is framed as reuse of the intent
    infra (replay + bench + CLI introspection), takes a user-declared
    multi-objective with constraints (acknowledging no single scalar
    exists), derives its knob catalog from `// tachyon:knob`
    annotations on the Go source, and writes a recommended config
    plus Pareto table — never directly to prod.
14. The search driver is either the external coding agent (primary,
    via `tachyon tune trial/history/recommend` CLI + MCP tools) or a
    built-in dependency-free Latin hypercube + coordinate descent
    (~150 lines, for CI). No optuna/Ax/GP library dependency.

No code changes, no tests to run. Review is the verification.
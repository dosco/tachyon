# Intent-Compiled Filters

## Wager

Proxy extensibility usually takes one of three forms:

- human-written config
- an embedded runtime like Lua or WASM
- native code written against an internal API

Tachyon should take a fourth path:

- users write small, typed `intent` files
- tachyon compiles them into native Go
- the compiler also emits tests and benchmarks
- the standard Go toolchain proves both semantics and cost
- the final artifact is a specialized tachyon binary

That last point is the actual wedge. The novel part is not "a DSL for
proxy rules." Plenty of systems have one. The novel part is that a
policy change comes with executable evidence:

- generated filter code
- generated unit and integration tests
- generated microbenchmarks
- a profile ready for `go build -pgo`

We are not trying to bolt scripting onto a fast proxy. We are trying to
make tachyon the first reverse proxy whose extension story is native,
self-testing, and self-benchmarking from day one.

## Why This Is Buildable Here

This is not a moonshot detached from the current repo. The pieces we
need already exist:

- `internal/proxy/h1_handler.go` has one clear request path to wrap
- `internal/router/match.go` already gives us deterministic route
  selection
- `internal/router/match_bench_test.go` shows we already care about
  hot-path benchmark discipline
- `e2e/harness_test.go` already spins real proxy + origin tests using
  Go's own testing tools
- `README.md` and `BENCHMARK.md` already treat pprof and PGO as part of
  the engineering story

So v1 should lean hard on the Go toolchain we already use:

- `testing.T` for semantic checks
- `testing.AllocsPerRun` for allocation gates
- `testing.B` for steady-state hot-path benchmarks
- `go test -fuzz` for parser and compiler hardening
- `go test -cpuprofile` plus `go build -pgo` for profile-guided builds

The point is not to invent a giant new control plane. The point is to
turn intent into normal Go engineering artifacts.

## Product

An intent file is the source of truth for route-local behavior.

Operators attach named policies to routes in config:

```yaml
routes:
  - host: "api.example.com"
    path: "/v1/"
    upstream: "api"
    intents: ["api_guard", "tenant_headers"]
```

At build time, `tachyon intent build`:

1. parses and type-checks `intent/*.intent`
2. lowers them to a small IR
3. emits native Go into `internal/intent/generated/current/`
4. emits tests and benchmarks beside that Go
5. runs `go test`
6. runs `go test -bench ... -cpuprofile ...`
7. builds tachyon with `go build -pgo=...`

The runtime path stays native Go. There is no interpreter, no VM, no
runtime plugin ABI, and no config parser in the hot path.

## V1 Scope

V1 should be deliberately small and complete.

In:

- route-local request matching on host, path, method, headers, query,
  cookies, client IP, and TLS SNI
- header mutation on request and response
- redirects and fixed responses
- upstream override and simple canary routing
- API key auth
- JWT verification against configured keys
- bounded local rate limiting
- structured log fields and metrics tags
- external auth subrequests with explicit timeout and fallback
- generated tests, benchmarks, and PGO profile

Out:

- arbitrary request or response body rewriting
- regex-heavy matching in the hot path
- distributed rate limiting in v1
- full response caching
- protocol-specific hooks for WebSocket, gRPC, or HTTP/3
- runtime plugins via Go's `plugin` package

This is the right cut because it covers the majority of what people
actually use proxy middleware for, while staying small enough to reason
about mechanically.

## The Real Use Cases

The intent system only matters if it covers the jobs operators actually
have today.

The important categories are:

- auth and admission
  API key, JWT, client IP allow/deny, external auth service
- rate limiting
  per-IP, per-header, per-tenant, burst + steady-state
- traffic shaping
  route override, canary percentage, maintenance mode
- header policy
  add, drop, normalize, propagate trace and tenant identifiers
- edge hygiene
  redirects, body-size limits, path-prefix cleanup
- observability
  structured logs, sampled logs, request tags, metric labels

That is enough to replace a large amount of real Nginx config, a large
amount of Traefik middleware, and a meaningful fraction of what people
reach for OpenResty or custom filters to do.

## Design Rules

The system should follow six hard rules.

### 1. Native Or Nothing

Generated code must look like code we would hand-write in tachyon:

- concrete structs
- direct calls
- no reflection
- no interface dispatch in the hot path
- no per-request maps
- no per-request heap allocation on common pure-policy paths

### 2. One Obvious Authoring Form

V1 gets one policy form, not three.

Each policy has:

- one `match`
- one `request` block
- one `response` block
- one `error` block
- zero or more `case` blocks
- zero or one `budget` block

That is enough. Multiple surface syntaxes are how agents drift.

### 3. Closed Vocabulary

If a primitive does not appear in `tachyon intent primitives`, it does
not exist. There is no "maybe this works" space.

### 4. Every External Effect Is Declared

Anything that crosses process boundaries must say:

- where it goes
- how long it may take
- what happens on failure

No hidden network I/O. No silent fallback.

### 5. Intent Must Compile Into Proof

Every intent file should produce:

- native Go
- semantic tests
- allocation checks
- steady-state benchmarks

The source is not just a policy description. It is a policy plus proof
that the policy works and a measurement of what it costs.

### 6. The Binary Is The Reference

An agent with the `tachyon` binary on `PATH` should be able to discover
the grammar, examples, primitives, errors, and verification commands
without opening a website.

## Programming Model

V1 should keep one simple execution model.

1. Tachyon parses the request as it does today.
2. The router selects the route as it does today.
3. Tachyon loads the compiled policy chain attached to that route.
4. Request-phase actions run in priority order.
5. If no action returns a terminal response, tachyon forwards upstream.
6. Response-phase actions run on the upstream response.
7. Error-phase actions run only on local or upstream failure paths.

Important semantics:

- higher `priority` runs first
- ties break by source order after canonical formatting
- `respond`, `deny`, and `redirect` are terminal
- `route_to` overrides the route's chosen upstream and continues
- `set_header`, `remove_header`, `emit_metric`, and `log` are
  non-terminal
- `auth_external` is only valid in `request`
- `error` never re-enters `request` or `response`

This is intentionally boring. That is a feature.

## Language Sketch

V1 should be small enough that the full grammar fits in a couple of KB
and can be held in working memory by a small model.

```text
intent_version "0.1"

policy NAME {
  priority INT
  match <predicate>

  request  { <request_step>; ... }
  response { <response_step>; ... }
  error    { <error_step>; ... }

  case NAME   { ... }
  budget      { ... }
}
```

Predicates in v1:

- `req.host`
- `req.path`
- `req.method`
- `req.header("x-name")`
- `req.query("name")`
- `req.cookie("name")`
- `req.ip`
- `req.tls.sni`

Operators in v1:

- `==`, `!=`
- `&&`, `||`, `!`
- `has_prefix`
- `has_suffix`
- `one_of`
- `in_cidr`

No general regex in v1. Regex can come later if we can prove bounded
cost and a compelling need. Prefix, suffix, equality, and set-membership
cover the important cases cheaply.

Request actions in v1:

- `respond(status, body?)`
- `deny(status)`
- `redirect(code, url)`
- `route_to(pool)`
- `canary(percent, to)`
- `set_header(name, value)`
- `remove_header(name)`
- `strip_prefix(prefix)`
- `add_prefix(prefix)`
- `auth_api_key(header, secret_ref)`
- `auth_jwt(issuer, audience, jwks_ref)`
- `auth_external(service_ref)`
- `rate_limit_local(key, rps, burst)`
- `limit_body(bytes)`
- `timeout(duration)`
- `log(fields)`
- `emit_metric(name, labels)`

Response actions in v1:

- `set_header(name, value)`
- `remove_header(name)`
- `log(fields)`
- `emit_metric(name, labels)`

Error actions in v1:

- `respond(status, body?)`
- `log(fields)`
- `emit_metric(name, labels)`

## Example

This is roughly the shape we should optimize around:

```text
intent_version "0.1"

policy api_guard {
  priority 100
  match req.host == "api.example.com" &&
        req.path.has_prefix("/v1/")

  request {
    auth_api_key(header: "x-api-key", secret_ref: "secrets/api_keys")
    rate_limit_local(key: req.header("x-api-key"), rps: 100, burst: 200)
    set_header("x-proxy", "tachyon")
  }

  response {
    set_header("x-served-by", "tachyon")
  }

  case authorized {
    req {
      method "GET"
      host   "api.example.com"
      path   "/v1/orders"
      header "x-api-key" "gold-user"
    }
    expect {
      upstream "api"
      status   200
      header   "x-served-by" "tachyon"
    }
  }

  budget {
    request.max_allocs_op 0
    request.max_bytes_op  0
  }
}
```

The key idea is that `case` and `budget` live with the policy. Intent is
not just config. Intent is config plus executable proof.

## The Revolutionary Part: Executable Policy Proof

This is the part worth building the whole system around.

A compiled intent set should emit four kinds of artifacts:

### 1. Generated Runtime Code

`filters_gen.go` contains concrete request, response, and error methods
that the proxy handler calls directly.

### 2. Generated Semantic Tests

`filters_gen_test.go` turns every `case` block into table-driven Go
tests.

Those tests should use normal Go tools:

- pure action tests run directly against the generated runtime package
- route-level tests use a tiny in-process harness
- end-to-end integration tests can reuse the existing `e2e` machinery
  built on `httptest`

This gives us standard `go test` ergonomics, normal CI output, and no
custom test runner to maintain.

### 3. Generated Allocation Gates

The compiler should emit tests that call `testing.AllocsPerRun` on the
common request path for pure policies and fail if allocs drift above the
declared budget.

That matters because allocation checks are deterministic enough to be
real gates in ordinary CI. This is where Go's standard testing package
is better than a bespoke proxy-specific harness.

### 4. Generated Benchmarks

`filters_gen_bench_test.go` should emit a small benchmark suite for each
compiled policy set:

- `BenchmarkNoMatch`
- `BenchmarkRequestHotPath`
- `BenchmarkRequestRateLimitHotKey`
- `BenchmarkResponseHeaders`
- `BenchmarkExternalAuthFastFail`

Every benchmark must call `b.ReportAllocs()`.

These benchmarks serve three jobs:

- they tell us what the policy costs
- they generate a profile for `go build -pgo`
- they create a stable perf surface the agent can reason about

The result is a very unusual development loop:

1. write or synthesize intent
2. compile it
3. run `go test`
4. run `go test -bench`
5. build the specialized binary with the resulting profile

That is concrete, local, fast, and grounded in the same toolchain used
for the rest of tachyon.

## Compiler Architecture

The compiler should be a normal internal Go subsystem:

```text
internal/intent/
  parse/       - lexer + parser
  sema/        - type checking, validation, canonicalization
  ir/          - tiny typed lowering target
  codegen/     - Go emitter + generated tests + generated benchmarks
  runtime/     - small helper types used by generated code

internal/intent/generated/current/
  filters_gen.go
  filters_gen_test.go
  filters_gen_bench_test.go
```

This is enough for v1. There is no reason to start with a multi-target
compiler or a giant runtime.

The pipeline is:

```text
intent/*.intent
    -> parse
    -> type check
    -> canonicalize
    -> lower to IR
    -> emit Go
    -> emit tests
    -> emit benchmarks
    -> go test
    -> go test -bench -cpuprofile
    -> go build -pgo
```

Code generation should be simple. `go/format` plus templates is enough
for v1. We do not need a fancy codegen framework to emit straight-line
Go.

## Integration Points In The Existing Tree

Three concrete edits make the system real.

### 1. Route Config

`internal/router/config.go` gains an `Intents []string` field on each
route rule.

### 2. Handler Hooks

`internal/proxy/h1_handler.go` gains direct calls into generated
request, response, and error hooks around the upstream forwarding path.

The important constraint is that the call site stays concrete enough for
the compiler to inline through the generated code where possible.

### 3. Generated Package Registration

At startup, tachyon loads the compiled intent registry from
`internal/intent/generated/current/` and attaches named policy chains to
configured routes.

There is no dynamic load step in production. The binary already contains
the policy code.

## Performance Contract

The performance story has to be explicit or the whole idea collapses.

We should divide policies into three classes.

### Class A: Pure Local Policies

Examples:

- path match + redirect
- add or remove headers
- fixed deny on IP or header match

Contract:

- `0 allocs/op`
- `0 B/op`
- no reflection
- no interface dispatch
- no heap-backed maps in the request path

### Class B: Local Stateful Policies

Examples:

- local rate limit
- canary selection
- local counters

Contract:

- `0 allocs/op` in steady state
- bounded memory declared at compile time
- hot-key benchmark emitted automatically

### Class C: External Policies

Examples:

- external auth

Contract:

- explicit timeout
- explicit fallback
- zero-allocation path until the outbound call is made
- benchmark coverage for the local wrapper path

This split matters because it keeps the performance story honest.
"Intent compiles to native code" is true, but not every primitive has
the same cost class. The system should say that out loud.

## Go-Native Verification Loop

The verification loop should be as close as possible to normal Go
development.

### Semantic Verification

`tachyon intent verify` should:

1. generate code, tests, and benchmarks
2. run `go test ./internal/intent/generated/current`
3. fail on semantic mismatch or allocation-budget violation

### Performance Verification

`tachyon intent bench` should run:

```bash
go test ./internal/intent/generated/current \
  -run '^$' \
  -bench . \
  -benchmem \
  -cpuprofile .tachyon/pgo/current.pprof
```

That gives us:

- ns/op
- allocs/op
- bytes/op
- a CPU profile for PGO

We do not need a bespoke profiler. Go already ships one.

### Build

`tachyon intent build` should end with:

```bash
go build -pgo .tachyon/pgo/current.pprof ./cmd/tachyon
```

### Fuzzing

The compiler packages should ship normal Go fuzz targets:

- `FuzzParseIntent`
- `FuzzTypeCheckIntent`
- `FuzzLowerIntent`

As the feature matures, `case` blocks can also seed fuzz corpora for
generated matcher and rewrite logic. Again, the right move is to use
Go's standard fuzzing support instead of inventing our own engine.

## Agent Workflow

The author should usually be a coding agent, not a human hand-typing a
new grammar from memory.

That means the binary must expose a small, stable CLI surface:

- `tachyon intent grammar`
- `tachyon intent primitives`
- `tachyon intent examples`
- `tachyon intent scaffold <pattern>`
- `tachyon intent lint <files...>`
- `tachyon intent build <files...>`
- `tachyon intent verify <files...>`
- `tachyon intent bench <files...>`
- `tachyon intent explain <case>`

The rule is simple:

- the CLI is the reference
- the website is secondary
- an agent can discover everything it needs from the installed binary

That is how this becomes agent-native instead of agent-marketed.

## Implementation Plan

The right implementation strategy is staged and ruthless about scope.

### Phase 1: Pure Policies

Build:

- parser
- type checker
- canonical formatter
- request and response codegen
- route attachment
- primitives for headers, redirects, fixed responses, path rewriting,
  and route override

Ship with:

- generated semantic tests
- generated alloc gates
- generated no-match and hot-path benchmarks

### Phase 2: Stateful Local Policies

Add:

- local rate limiting
- canary routing
- bounded counters

Ship with:

- hot-key benchmarks
- explicit memory declarations in generated reports

### Phase 3: External Policies

Add:

- external auth blocks
- timeout and fallback enforcement
- explain output for failure paths

Ship with:

- fast-fail benchmarks
- integration cases against `httptest` origins

### Phase 4: Replay And Diff

Add:

- traffic capture and replay
- semantic diff between intent versions
- replay-driven perf reports

Replay is powerful, but it does not belong in the critical path for v1.
The system is already valuable before it exists.

## What Makes This Revolutionary

A lot of systems can claim one of these:

- native runtime
- typed config
- decent benchmarks
- decent agent ergonomics

Very few can claim all of them together:

- typed declarative policy source
- native compiled runtime
- generated tests beside the generated code
- generated benchmarks beside the generated code
- built-in path to PGO
- self-documenting CLI for coding agents

That combination is the actual product.

The ambition should be:

> A tachyon policy change is not accepted because it "looks right."
> It is accepted because the compiler emitted native code, `go test`
> proved the semantics, `go test -bench` measured the hot path, and the
> resulting profile was fed back into the build.

That is a real system we can build. It is narrow enough to ship, strong
enough to matter, and different enough to be worth doing.

# Intent Guide

Intents are declarative policy files that compile into native Go and attach to routes in `config.yaml`. They let you express rate limits, header mutations, access control, and routing decisions without touching proxy code.

## Quick Start

**1. Create a policy file:**

```
intent_version "0.1"

policy add_tracing_header {
  priority 100
  match req.host == "api.example.com"

  request {
    set_header("x-trace-origin", "tachyon")
  }

  response {
    set_header("x-served-by", "tachyon")
  }

  case happy_path {
    request.host "api.example.com"
    request.path "/"
    expect.request_header "x-trace-origin", "tachyon"
    expect.response_header "x-served-by", "tachyon"
  }

  budget {
    request.allocs <= 1
    response.allocs <= 0
  }
}
```

**2. Compile and test:**

```bash
tachyon intent build intent/*.intent
```

This runs the sema pass, generates `internal/intent/generated/current/`, runs all generated tests and allocation budgets, and rebuilds the binary with a fresh PGO profile.

**3. Attach to a route in `config.yaml`:**

```yaml
routes:
  - host: "api.example.com"
    path: "/"
    upstream: pool-a
    intents:
      - add_tracing_header
```

## Policy Syntax

```
intent_version "0.1"

policy NAME {
  priority INT         # higher fires first; default 100
  match EXPR           # optional; omit to match all requests

  request  { ACTION... }   # fires before upstream
  response { ACTION... }   # fires after upstream responds
  error    { ACTION... }   # fires if upstream errors

  case CASE_NAME {         # named test case (optional, multiple allowed)
    request.method  "GET"
    request.host    "example.com"
    request.path    "/"
    request.header  "x-api-key", "k1"
    request.query   "debug", "1"
    request.cookie  "session", "abc"
    expect.status   200
    expect.body     "ok"
    expect.request_header  "x-forwarded-for", "1.2.3.4"
    expect.response_header "x-served-by", "tachyon"
    expect.upstream "pool-b"
    expect.path     "/v2/endpoint"
  }

  budget {
    request.allocs  <= 4   # allocation limit for request phase
    response.allocs <= 2   # allocation limit for response phase
  }
}
```

## Match Expressions

Combine with `&&`. All conditions must be true for a policy to fire.

| Expression | Matches when... |
|---|---|
| `req.host == "example.com"` | Host header equals value |
| `req.method == "GET"` | HTTP method equals value |
| `req.path == "/healthz"` | Path is exactly this string |
| `req.path.has_prefix("/api/")` | Path starts with prefix |
| `req.path.has_suffix(".json")` | Path ends with suffix |
| `req.header("x-api-key") == "secret"` | Named header equals value |
| `req.query("debug") == "1"` | Query parameter equals value |
| `req.cookie("role") == "admin"` | Cookie value equals string |
| `req.ip == "10.0.0.1"` | Client IP equals value |

### Examples

```
# Any request to /api/*
match req.path.has_prefix("/api/")

# POST to a specific host
match req.host == "api.example.com" && req.method == "POST"

# Only JSON endpoints
match req.path.has_suffix(".json")

# Exact healthcheck path
match req.path == "/healthz"

# Requests with a specific cookie and path prefix
match req.path.has_prefix("/admin") && req.cookie("role") == "admin"
```

## Actions

### Request phase

| Action | Description |
|---|---|
| `set_header("name", "value")` | Add or overwrite a request header sent to upstream |
| `remove_header("name")` | Remove a request header |
| `strip_prefix("/v1")` | Remove path prefix before forwarding |
| `add_prefix("/v2")` | Prepend a prefix to the path |
| `route_to("pool-name")` | Override the upstream pool for this request |
| `canary(10, "pool-canary")` | Send 10% of traffic to `pool-canary` |
| `rate_limit_local("ip", rps, burst)` | Local token-bucket rate limit; returns 429 on exceed |
| `auth_external("https://authz/check", 401)` | Forward to auth service; deny with 401 on failure |
| `deny(403)` | Return 403 immediately (default status if omitted) |
| `respond(200, "body")` | Return a fixed response without forwarding |
| `redirect(301, "https://new.host/")` | Return a redirect |
| `log("message")` | Emit a structured log entry |
| `emit_metric("metric.name")` | Emit a counter metric |

### Response phase

| Action | Description |
|---|---|
| `set_header("name", "value")` | Add or overwrite a response header |
| `remove_header("name")` | Remove a response header |
| `log("message")` | Emit a structured log entry |
| `emit_metric("metric.name")` | Emit a counter metric |

> **Note:** Terminal actions (`deny`, `respond`, `redirect`) and routing actions (`route_to`, `canary`, `strip_prefix`, `add_prefix`, `rate_limit_local`, `auth_external`) are only valid in `request` blocks.

## Runtime Classes

Policies are automatically classified during `tachyon intent build`:

| Class | Actions used | Uring path | Stdlib path |
|---|---|---|---|
| **A** | Stateless: set_header, remove_header, deny, respond, redirect, route_to, strip/add_prefix, log, emit_metric | ✓ | ✓ |
| **B** | Local stateful: rate_limit_local, canary | ✓ | ✓ |
| **C** | External calls: auth_external | ✗ | ✓ |

Routes with Class C policies are automatically served by the stdlib worker even on Linux. Tachyon logs a warning at startup.

## Rate Limiting

`rate_limit_local` uses a per-process token bucket. The key argument determines what is rate-limited:

| Key | Limits by... |
|---|---|
| `"ip"` | Client IP address |
| `"header:x-api-key"` | Value of `X-Api-Key` header |
| any literal string | Global single bucket |

```
policy api_rate_limit {
  priority 150
  match req.path.has_prefix("/api/")

  request {
    rate_limit_local("header:x-api-key", 100, 200)
  }
}
```

Returns `429 Too Many Requests` with `Retry-After: 1` when the bucket is exhausted.

## Canary Deployments

```
policy canary_v2 {
  priority 90
  match req.path.has_prefix("/api/")

  request {
    canary(5, "pool-v2")
  }
}
```

Routes 5% of matching traffic to `pool-v2`. The remaining 95% continues to the route's default upstream.

## External Auth

```
policy require_auth {
  priority 300
  match req.path.has_prefix("/secure/")

  request {
    auth_external("https://authz.internal/check", 401)
  }
}
```

Tachyon makes a `GET` to the URL with these headers:

- `X-Tachyon-Method` — original HTTP method
- `X-Tachyon-Path` — original request path
- `X-Tachyon-Host` — original `Host` header
- `X-Tachyon-Client-IP` — client IP (if available)

A 2xx response allows the request to proceed. Anything else returns the configured deny status (default 403).

**Timeout:** 200ms. A timeout or network error denies the request.

## Allocation Budgets

Budgets assert that hot-path execution stays allocation-free. They're enforced by generated Go tests and checked in `tachyon intent build`:

```
budget {
  request.allocs  <= 0   # exact zero — enforce 0 allocs
  response.allocs <= 0
}
```

Use `<= N` for policies that genuinely need N allocations (e.g. rate_limit_local allocates on first key insert). Common numbers:

- `0` — pure header mutations, deny, respond
- `1` — first rate-limit bucket creation or canary routing
- `4` — auth_external (HTTP round trip allocation cost)

## Test Cases

Each `case` block generates a test in `registry_gen_test.go`. Add cases for every meaningful input:

```
case blocked_path {
  request.path "/admin/panel"
  expect.status 403
}

case allowed_path {
  request.path "/public/page"
  # no expect.status → asserts no terminal response
}
```

Run just the generated tests:

```bash
go test ./internal/intent/generated/current/ -v
```

## CLI Reference

| Command | Description |
|---|---|
| `tachyon intent grammar` | Print DSL reference |
| `tachyon intent primitives` | List all available actions |
| `tachyon intent errors` | Print the stable compiler error catalog |
| `tachyon intent agent` | Print the agent-oriented CLI workflow contract |
| `tachyon intent scaffold NAME` | Print a starter policy skeleton |
| `tachyon intent lint FILE...` | Parse and validate, no code generation |
| `tachyon intent build FILE...` | Validate, generate, test, PGO-build |
| `tachyon intent verify FILE...` | Validate and generate; run tests but skip PGO build |
| `tachyon intent bench FILE...` | Generate and run benchmarks |
| `tachyon intent diff OLD NEW` | Show semantic diff between two policy sets |
| `tachyon intent explain --case POLICY/CASE` | Trace a specific case through the live registry |

## Semantic Error Codes

| Code | Meaning |
|---|---|
| E001 | Unexpected top-level line in policy file |
| E002 | Unterminated policy block |
| E011 | Invalid priority value |
| E012 | Invalid match expression |
| E013 | Unexpected line inside policy block |
| E020 | Invalid action syntax |
| E021 | Unexpected line inside block |
| E022 | Invalid budget line |
| E023 | Invalid case line |
| E102 | Duplicate policy name across files |
| **E200** | Action used in wrong phase (e.g. `deny` in response block) |
| **E201** | Multiple terminal actions in request block |
| **E202** | Contradictory match conditions (same field, different values) |

The CLI surfaces compiler failures in a stable envelope:

```text
intent_error code=E200 message="..."
```

## Debugging

**Explain a case interactively:**

```bash
tachyon intent explain --case sample_headers/adds_headers
```

Outputs the request inputs, expected results, and a full policy execution trace as JSON.

**Replay traffic against a new policy set:**

```bash
# 1. Record live traffic (runs proxy with a recording sidecar)
tachyon traffic record --out traffic.ndjson.gz

# 2. Update your policies and rebuild
tachyon intent build intent/*.intent

# 3. Replay captured traffic and inspect changes
tachyon traffic replay traffic.ndjson.gz
```

**Trace a single captured request:**

```bash
tachyon traffic explain --artifact traffic.ndjson.gz --id 12345
```

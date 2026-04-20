# End-to-End Example

This is a full, reproducible Tachyon intent workflow using checked-in
files in this repo.

The example uses:

- [intent/example_workflow.intent](/Users/vr/src/tachyon/intent/example_workflow.intent)
- [intent/config.intent](/Users/vr/src/tachyon/intent/config.intent)
- [origin.go](/Users/vr/src/tachyon/examples/end-to-end/origin.go)

What it demonstrates:

- request header mutation before proxying upstream
- response header mutation on the way back to the client
- a terminal deny rule that short-circuits before the origin
- the full `tachyon intent ...` build / explain workflow

## 1. Build The Binary

```bash
go build -o tachyon ./cmd/tachyon
```

## 2. Inspect The DSL From The Binary

The binary is the contract for both humans and coding agents:

```bash
./tachyon intent agent
./tachyon intent grammar
./tachyon intent primitives
./tachyon intent errors
```

## 3. Inspect The Example Policy

The example intent source is [example_workflow.intent](/Users/vr/src/tachyon/intent/example_workflow.intent).

You can validate the file and inspect one of its generated traces before
you rebuild anything:

```bash
./tachyon intent lint intent/example_workflow.intent
./tachyon intent explain --case example_proxy_headers/forwards_with_headers
```

## 4. Compile, Test, Benchmark, And Rebuild

This is the main intent workflow:

```bash
./tachyon intent build intent/*.intent
```

That one command:

- parses and type-checks the intent files
- regenerates Go into `internal/intent/generated/current/`
- runs generated tests and allocation checks
- runs generated benchmarks
- writes the current benchmark profile into `.tachyon/pgo/current.pprof`
- rebuilds `./tachyon` with `go build -pgo`

Useful outputs after the build:

- generated code: `internal/intent/generated/current/`
- benchmark JSON: `.tachyon/bench/current.json`
- CPU profile for PGO: `.tachyon/pgo/current.pprof`

## 5. Run The Example Live

Start the example origin in one terminal:

```bash
go run ./examples/end-to-end/origin.go
```

Start Tachyon in a second terminal:

```bash
./tachyon serve -config intent/
```

The topology is defined in [intent/config.intent](/Users/vr/src/tachyon/intent/config.intent), which is compiled into the binary alongside the policies. It attaches these two policies to the `example_workflow` route:

- `example_proxy_headers`
- `example_block_admin_debug`

## 6. Exercise The Happy Path

In a third terminal:

```bash
curl -i -H 'Host: example.local' http://127.0.0.1:18080/hello
```

What to look for:

- `X-Origin-Seen-Proxy: tachyon`
  This proves the request header mutation reached the origin.
- `X-Example-Served-By: tachyon`
  This proves the response header mutation ran on the proxy response path.
- response body `origin path=/hello`
  This proves the request forwarded upstream normally.

## 7. Exercise The Terminal Rule

```bash
curl -i -H 'Host: example.local' 'http://127.0.0.1:18080/admin?debug=1'
```

Expected result:

- HTTP `403`
- the request is denied by `example_block_admin_debug`
- the origin does not receive the request

## 8. Optional: Record And Replay Traffic

If you want to see the traffic tooling end to end, run record mode
instead of `serve`:

```bash
./tachyon traffic record --config intent/ --out .tachyon/replays/example.ndjson.gz
```

Generate a little traffic with the same `curl` commands above, then stop
the process and replay the captured envelopes:

```bash
./tachyon traffic replay --config intent/ .tachyon/replays/example.ndjson.gz
./tachyon traffic explain --config intent/ --artifact .tachyon/replays/example.ndjson.gz --id 1
```

That lets you inspect:

- matched routes
- policy fire counts
- terminal decision counts
- the explain trace for one captured request

## 9. Repo Tests That Cover This Example

The checked-in example is also exercised by tests:

```bash
go test ./cmd/tachyon ./internal/intent
go test ./e2e -tags=integration -run TestExampleWorkflowPolicies
```

Those tests keep the documented workflow aligned with the actual
generated registry and runtime behavior.

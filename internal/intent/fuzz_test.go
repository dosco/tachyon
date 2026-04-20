package intent

import (
	"testing"
)

// FuzzParseIntent feeds arbitrary text to the intent parser.
// The parser must never panic and must return a clean error for invalid input.
func FuzzParseIntent(f *testing.F) {
	f.Add(`intent_version "0.1"`)
	f.Add(`intent_version "0.1"
policy p { priority 100 match req.host == "example.com" request { set_header("x", "y") } }`)
	f.Add(`intent_version "0.1"
policy p {
  priority 100
  match req.path.has_prefix("/api/") && req.method == "GET"
  request { deny(403) }
  case c { request.method "GET" request.path "/api/" expect.status 403 }
  budget { request.allocs <= 4 }
}`)
	f.Add(`intent_version "0.1"
policy q { match req.query("k") == "v" request { respond(200, "ok") } }`)
	f.Add(`intent_version "0.1"
policy q { match req.cookie("s") == "v" request { route_to("pool") } }`)

	f.Fuzz(func(t *testing.T, src string) {
		_, _ = ParseFiles(nil) // ensure Discover path is reachable
		file, err := parseSource("<fuzz>", src)
		if err != nil || file == nil {
			return
		}
		// If parsing succeeds, basic invariants must hold.
		for _, p := range file.Policies {
			if p.Name == "" {
				t.Error("parsed policy with empty name")
			}
		}
	})
}

// FuzzBuildBundle feeds arbitrary policy source through the full bundle
// pipeline including code generation.  Generation must never panic.
func FuzzBuildBundle(f *testing.F) {
	f.Add(`intent_version "0.1"
policy p { priority 1 match req.host == "a" request { set_header("k","v") } }`)
	f.Add(`intent_version "0.1"
policy p { match req.query("q") == "1" request { deny() } }`)
	f.Add(`intent_version "0.1"
policy p { match req.cookie("c") == "x" request { respond(200, "hi") } }`)
	f.Add(`intent_version "0.1"
policy p {
  match req.method == "POST"
  request { rate_limit_local("ip", 5, 10) }
  case c { request.method "POST" request.path "/" }
  budget { request.allocs <= 8 }
}`)

	f.Fuzz(func(t *testing.T, src string) {
		file, err := parseSource("<fuzz>", src)
		if err != nil || file == nil {
			return
		}
		b := Bundle{Version: file.Version, Policies: file.Policies}
		if b.Version == "" {
			b.Version = "0.1"
		}
		// Generate must not panic even on unusual policy shapes.
		_ = Generate(b, "")
	})
}

// FuzzMatchExpr feeds arbitrary match expressions to the parser.
func FuzzMatchExpr(f *testing.F) {
	f.Add(`req.host == "example.com"`)
	f.Add(`req.method == "GET"`)
	f.Add(`req.path.has_prefix("/api/")`)
	f.Add(`req.header("x-key") == "val"`)
	f.Add(`req.query("q") == "1"`)
	f.Add(`req.cookie("session") == "abc"`)
	f.Add(`req.ip == "127.0.0.1"`)
	f.Add(`req.host == "a" && req.method == "GET"`)
	f.Add(``)
	f.Add(`req.unknown("x") == "y"`)

	f.Fuzz(func(t *testing.T, expr string) {
		// Cap input length so fuzz workers don't stall on pathological
		// multi-megabyte strings — parseMatch does multiple linear scans
		// (strings.Split, HasPrefix, trimQuoted) and a giant input can
		// starve the worker past Go's fuzz shutdown deadline, which the
		// test framework reports as "context deadline exceeded" at the
		// end of the run. 4 KiB is well above any realistic match expr.
		if len(expr) > 4096 {
			t.Skip()
		}
		_, _ = parseMatch(expr)
	})
}

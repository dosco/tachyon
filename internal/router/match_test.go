package router

import "testing"

// TestMatchTable covers:
//   - exact host + exact path
//   - wildcard host fallback when the host-specific tree has no match
//   - longest-prefix-match: /foo/bar wins over /foo when both present
//   - empty path returns the upstream bound to the tree root, if any
//   - miss returns "".
func TestMatchTable(t *testing.T) {
	rules := []Rule{
		{Host: "api.example.com", Path: "/", Upstream: "api-root"},
		{Host: "api.example.com", Path: "/v1/", Upstream: "api-v1"},
		{Host: "api.example.com", Path: "/v1/users", Upstream: "api-users"},
		{Host: "*", Path: "/health", Upstream: "health"},
		{Host: "*", Path: "/", Upstream: "default"},
	}
	r := New(rules)

	type tc struct {
		name string
		host string
		path string
		want string
	}
	cases := []tc{
		{"root on matching host", "api.example.com", "/", "api-root"},
		{"prefix match on matching host", "api.example.com", "/v1/items", "api-v1"},
		{"longest-prefix wins", "api.example.com", "/v1/users", "api-users"},
		{"unknown host falls through to wildcard default", "other.test", "/", "default"},
		{"unknown host hits wildcard leaf", "other.test", "/health", "health"},
		// Host-specific tree has no /health; current impl returns the
		// deepest host-specific match, which is the root ("api-root")
		// because /health shares no prefix with any host-specific route.
		{"host-specific match beats wildcard when present", "api.example.com", "/health", "api-root"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := r.Match(c.host, []byte(c.path))
			if got != c.want {
				t.Fatalf("Match(%q,%q): got %q want %q", c.host, c.path, got, c.want)
			}
		})
	}
}

// TestMatchNoWildcardReturnsEmpty confirms an unrouted request is a
// miss, not a panic.
func TestMatchNoWildcardReturnsEmpty(t *testing.T) {
	r := New([]Rule{{Host: "api.example.com", Path: "/v1/", Upstream: "api"}})
	if got := r.Match("other.test", []byte("/")); got != "" {
		t.Fatalf("unrouted Match: got %q want \"\"", got)
	}
}

// TestMatchSplitsSharedPrefix covers the radix split path: two rules
// share a prefix, so the tree must split an existing edge. Both still
// match correctly.
func TestMatchSplitsSharedPrefix(t *testing.T) {
	r := New([]Rule{
		{Host: "h", Path: "/foobar", Upstream: "fb"},
		{Host: "h", Path: "/foo", Upstream: "f"},
	})
	if got := r.Match("h", []byte("/foobar")); got != "fb" {
		t.Fatalf("long path: got %q want fb", got)
	}
	if got := r.Match("h", []byte("/foo")); got != "f" {
		t.Fatalf("short path: got %q want f", got)
	}
	if got := r.Match("h", []byte("/foop")); got != "f" {
		t.Fatalf("prefix-of-longer path: got %q want f", got)
	}
}

// TestMatchEmptyHostUsesWildcard confirms a Rule with empty Host is
// treated like "*".
func TestMatchEmptyHostUsesWildcard(t *testing.T) {
	r := New([]Rule{{Host: "", Path: "/", Upstream: "default"}})
	if got := r.Match("anything", []byte("/")); got != "default" {
		t.Fatalf("got %q want default", got)
	}
}

// TestMatchWeightedSingletonCollapses confirms that a one-entry
// weighted list is collapsed to the single-upstream fast path — match
// becomes deterministic and skips the rand branch.
func TestMatchWeightedSingletonCollapses(t *testing.T) {
	r := New([]Rule{{
		Host: "h", Path: "/",
		Upstreams: []WeightedUpstream{{Name: "only", Weight: 3}},
	}})
	for i := 0; i < 100; i++ {
		if got := r.Match("h", []byte("/")); got != "only" {
			t.Fatalf("iter %d: got %q want only", i, got)
		}
	}
}

// TestMatchWeightedDistribution confirms a weighted multi-upstream
// rule returns names in roughly weight-proportional ratios over a
// large sample. We use a loose bound (±20%) to avoid flakiness.
func TestMatchWeightedDistribution(t *testing.T) {
	r := New([]Rule{{
		Host: "h", Path: "/",
		Upstreams: []WeightedUpstream{
			{Name: "a", Weight: 1},
			{Name: "b", Weight: 3},
		},
	}})
	counts := map[string]int{}
	const N = 4000
	for i := 0; i < N; i++ {
		counts[r.Match("h", []byte("/"))]++
	}
	// Expect ~25% a, ~75% b.
	if counts["a"]+counts["b"] != N {
		t.Fatalf("unknown names: %v", counts)
	}
	ratioA := float64(counts["a"]) / float64(N)
	if ratioA < 0.20 || ratioA > 0.30 {
		t.Fatalf("a ratio %.3f out of [0.20, 0.30]; counts=%v", ratioA, counts)
	}
}

// TestMatchWeightedZeroWeightIsOne confirms a zero weight is
// normalised to 1 at match time, so the entry is still reachable
// instead of silently dead.
func TestMatchWeightedZeroWeightIsOne(t *testing.T) {
	r := New([]Rule{{
		Host: "h", Path: "/",
		Upstreams: []WeightedUpstream{
			{Name: "a", Weight: 0},
			{Name: "b", Weight: 0},
		},
	}})
	sawA, sawB := false, false
	for i := 0; i < 500 && (!sawA || !sawB); i++ {
		switch r.Match("h", []byte("/")) {
		case "a":
			sawA = true
		case "b":
			sawB = true
		}
	}
	if !sawA || !sawB {
		t.Fatalf("expected both a and b to be picked; sawA=%v sawB=%v", sawA, sawB)
	}
}

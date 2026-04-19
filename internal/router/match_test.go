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
			if !got.Found || got.Upstream != c.want {
				t.Fatalf("Match(%q,%q): got %+v want upstream %q", c.host, c.path, got, c.want)
			}
			if got.RouteID < 0 {
				t.Fatalf("route id: got %+v", got)
			}
		})
	}
}

func TestMatchCarriesStableRouteID(t *testing.T) {
	r := New([]Rule{
		{RouteID: 11, Host: "example.com", Path: "/api/", Upstream: "api"},
		{RouteID: 99, Host: "*", Path: "/", Upstream: "default"},
	})
	got := r.Match("example.com", []byte("/api/x"))
	if !got.Found {
		t.Fatal("expected route match")
	}
	if got.RouteID != 11 {
		t.Fatalf("route id: got %d want 11", got.RouteID)
	}
}

// TestMatchNoWildcardReturnsEmpty confirms an unrouted request is a
// miss, not a panic.
func TestMatchNoWildcardReturnsEmpty(t *testing.T) {
	r := New([]Rule{{Host: "api.example.com", Path: "/v1/", Upstream: "api"}})
	if got := r.Match("other.test", []byte("/")); got.Found {
		t.Fatalf("unrouted Match: got %+v want miss", got)
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
	if got := r.Match("h", []byte("/foobar")); !got.Found || got.Upstream != "fb" {
		t.Fatalf("long path: got %+v want fb", got)
	}
	if got := r.Match("h", []byte("/foo")); !got.Found || got.Upstream != "f" {
		t.Fatalf("short path: got %+v want f", got)
	}
	if got := r.Match("h", []byte("/foop")); !got.Found || got.Upstream != "f" {
		t.Fatalf("prefix-of-longer path: got %+v want f", got)
	}
}

// TestMatchEmptyHostUsesWildcard confirms a Rule with empty Host is
// treated like "*".
func TestMatchEmptyHostUsesWildcard(t *testing.T) {
	r := New([]Rule{{Host: "", Path: "/", Upstream: "default"}})
	if got := r.Match("anything", []byte("/")); !got.Found || got.Upstream != "default" {
		t.Fatalf("got %+v want default", got)
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
		if got := r.Match("h", []byte("/")); !got.Found || got.Upstream != "only" {
			t.Fatalf("iter %d: got %+v want only", i, got)
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
		counts[r.Match("h", []byte("/")).Upstream]++
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
		switch r.Match("h", []byte("/")).Upstream {
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

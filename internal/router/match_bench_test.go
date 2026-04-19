package router

import "testing"

// BenchmarkMatchSimple exercises the single-upstream fast path — the
// shape every bench config has. Regressions here are bench regressions
// and must be caught pre-merge.
func BenchmarkMatchSimple(b *testing.B) {
	r := New([]Rule{
		{Host: "*", Path: "/", Upstream: "pool-a"},
	})
	p := []byte("/api/users/123")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.Match("example.com", p)
	}
}

// BenchmarkMatchHostSpecific covers the realistic multi-rule case
// where one of several host-specific rules matches and a wildcard
// fallback exists. Still the fast path (single-upstream terminal).
func BenchmarkMatchHostSpecific(b *testing.B) {
	r := New([]Rule{
		{Host: "a.example", Path: "/v1/", Upstream: "a1"},
		{Host: "a.example", Path: "/", Upstream: "aroot"},
		{Host: "*", Path: "/", Upstream: "default"},
	})
	p := []byte("/v1/users/123")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.Match("a.example", p)
	}
}

// BenchmarkMatchWeightedTwo covers the smallest non-fast-path case —
// a two-entry weighted terminal. The delta vs BenchmarkMatchSimple is
// the per-request cost of enabling canaries for a route.
func BenchmarkMatchWeightedTwo(b *testing.B) {
	r := New([]Rule{{
		Host: "*", Path: "/",
		Upstreams: []WeightedUpstream{
			{Name: "pool-a", Weight: 95},
			{Name: "pool-b", Weight: 5},
		},
	}})
	p := []byte("/api/users/123")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.Match("example.com", p)
	}
}

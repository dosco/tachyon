package current

import (
	"net/http"
	"testing"

	"tachyon/internal/intent/runtime"
	"tachyon/internal/router"
)

type benchRoundTripper func(*http.Request) (*http.Response, error)

func (fn benchRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func benchmarkRouteSet(names ...string) runtime.RoutePolicySet {
	rp, err := BuildRoutePrograms([]router.Rule{{RouteID: 1, Upstream: "origin", Intents: names}})
	if err != nil {
		panic(err)
	}
	return rp.ByRouteID[1]
}

func BenchmarkNoMatch(b *testing.B) {
	b.ReportAllocs()
	set := benchmarkRouteSet("bad")
	state := runtime.NewState()
	req := runtime.StaticRequest{MethodValue: http.MethodGet, HostValue: "bench.invalid", PathValue: "/miss"}
	for i := 0; i < b.N; i++ {
		_ = runtime.ExecuteRequest(set, state, req, "origin")
	}
}

func BenchmarkRequestHotPath(b *testing.B) {
	b.ReportAllocs()
	set := benchmarkRouteSet("bad")
	state := runtime.NewState()
	req := runtime.StaticRequest{MethodValue: http.MethodGet, HostValue: "example.com", PathValue: "/hot"}
	for i := 0; i < b.N; i++ {
		_ = runtime.ExecuteRequest(set, state, req, "origin")
	}
}

func BenchmarkRequestRateLimitHotKey(b *testing.B) {
	b.ReportAllocs()
	b.Skip("no local rate limit policy in generated bundle")
}

func BenchmarkResponseHeaders(b *testing.B) {
	b.ReportAllocs()
	b.Skip("no response-phase policy in generated bundle")
}

func BenchmarkExternalAuthFastFail(b *testing.B) {
	b.ReportAllocs()
	b.Skip("no external auth policy in generated bundle")
}

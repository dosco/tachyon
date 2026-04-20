package current

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

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
	set := benchmarkRouteSet("example_block_admin_debug", "example_proxy_headers", "sample_auth_external", "sample_cookie_auth", "sample_exact_path", "sample_headers", "sample_query_filter", "sample_rate_limit", "sample_suffix_match", "sample_terminal")
	state := runtime.NewState()
	req := runtime.StaticRequest{MethodValue: http.MethodGet, HostValue: "bench.invalid", PathValue: "/miss"}
	for i := 0; i < b.N; i++ {
		_ = runtime.ExecuteRequest(set, state, req, "origin")
	}
}

func BenchmarkRequestHotPath(b *testing.B) {
	b.ReportAllocs()
	set := benchmarkRouteSet("example_block_admin_debug")
	state := runtime.NewState()
	req := runtime.StaticRequest{MethodValue: http.MethodGet, HostValue: "example.com", PathValue: "/hot"}
	for i := 0; i < b.N; i++ {
		_ = runtime.ExecuteRequest(set, state, req, "origin")
	}
}

func BenchmarkRequestRateLimitHotKey(b *testing.B) {
	b.ReportAllocs()
	set := benchmarkRouteSet("sample_rate_limit")
	state := runtime.NewState()
	req := runtime.StaticRequest{MethodValue: http.MethodGet, HostValue: "example.com", PathValue: "/limited", HeadersValue: map[string]string{"x-api-key": "bench-hot-key"}, ClientIPValue: "127.0.0.1"}
	for i := 0; i < b.N; i++ {
		state.SetNowTime(time.Unix(int64(i), 0).UTC())
		_ = runtime.ExecuteRequest(set, state, req, "origin")
	}
}

func BenchmarkResponseHeaders(b *testing.B) {
	b.ReportAllocs()
	set := benchmarkRouteSet("example_proxy_headers")
	for i := 0; i < b.N; i++ {
		_ = runtime.ExecuteResponse(set, func(string) string { return "" })
	}
}

func BenchmarkExternalAuthFastFail(b *testing.B) {
	b.ReportAllocs()
	set := benchmarkRouteSet("sample_auth_external")
	state := runtime.NewState()
	state.SetHTTPClient(&http.Client{Transport: benchRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader("deny")), Header: make(http.Header)}, nil
	})})
	req := runtime.StaticRequest{MethodValue: http.MethodGet, HostValue: "example.com", PathValue: "/auth"}
	for i := 0; i < b.N; i++ {
		_ = runtime.ExecuteRequest(set, state, req, "origin")
	}
}

package current

import (
	"testing"

	"tachyon/internal/intent/runtime"
	"tachyon/internal/router"
)

func generatedRouteSet(t *testing.T, name string) runtime.RoutePolicySet {
	t.Helper()
	rp, err := BuildRoutePrograms([]router.Rule{{RouteID: 1, Upstream: "origin", Intents: []string{name}}})
	if err != nil {
		t.Fatalf("build route programs: %v", err)
	}
	return rp.ByRouteID[1]
}

func generatedHeaderValue(muts runtime.HeaderMutations, name string) (string, bool) {
	return muts.Find(name)
}

func generatedStatefulPolicy(name string) bool {
	switch name {
	case "sample_auth_external", "sample_rate_limit":
		return true
	default:
		return false
	}
}

func TestRegistryVersion(t *testing.T) {
	if Registry.Version == "" {
		t.Fatal("registry version must be set")
	}
}

func TestRegistryPolicyNames(t *testing.T) {
	if got := len(Registry.Policies); got != 10 {
		t.Fatalf("registry policy count: got %d want 10", got)
	}
	if _, ok := Registry.Policies["example_block_admin_debug"]; !ok {
		t.Fatalf("missing policy example_block_admin_debug")
	}
	if _, ok := Registry.Policies["example_proxy_headers"]; !ok {
		t.Fatalf("missing policy example_proxy_headers")
	}
	if _, ok := Registry.Policies["sample_auth_external"]; !ok {
		t.Fatalf("missing policy sample_auth_external")
	}
	if _, ok := Registry.Policies["sample_cookie_auth"]; !ok {
		t.Fatalf("missing policy sample_cookie_auth")
	}
	if _, ok := Registry.Policies["sample_exact_path"]; !ok {
		t.Fatalf("missing policy sample_exact_path")
	}
	if _, ok := Registry.Policies["sample_headers"]; !ok {
		t.Fatalf("missing policy sample_headers")
	}
	if _, ok := Registry.Policies["sample_query_filter"]; !ok {
		t.Fatalf("missing policy sample_query_filter")
	}
	if _, ok := Registry.Policies["sample_rate_limit"]; !ok {
		t.Fatalf("missing policy sample_rate_limit")
	}
	if _, ok := Registry.Policies["sample_suffix_match"]; !ok {
		t.Fatalf("missing policy sample_suffix_match")
	}
	if _, ok := Registry.Policies["sample_terminal"]; !ok {
		t.Fatalf("missing policy sample_terminal")
	}
}

func TestGeneratedCases(t *testing.T) {
	t.Run("example_block_admin_debug/blocks_debug_admin", func(t *testing.T) {
		set := generatedRouteSet(t, "example_block_admin_debug")
		req := runtime.StaticRequest{MethodValue: "GET", HostValue: "example.local", PathValue: "/admin", QueryValue: map[string]string{"debug": "1"}}
		result := runtime.ExecuteRequest(set, runtime.NewState(), req, "origin")
		if !result.HasTerminal {
			t.Fatal("expected terminal response")
		}
		if got := result.Terminal.Status; got != 403 {
			t.Fatalf("terminal status: got %d want 403", got)
		}
	})
	t.Run("example_proxy_headers/forwards_with_headers", func(t *testing.T) {
		set := generatedRouteSet(t, "example_proxy_headers")
		req := runtime.StaticRequest{MethodValue: "GET", HostValue: "example.local", PathValue: "/hello"}
		result := runtime.ExecuteRequest(set, runtime.NewState(), req, "origin")
		if result.HasTerminal {
			t.Fatalf("unexpected terminal response: %#v", result.Terminal)
		}
		if got, ok := generatedHeaderValue(result.HeaderMutations, "x-example-proxy"); !ok || got != "tachyon" {
			t.Fatalf("request header mutation x-example-proxy: got %q present=%v want %q", got, ok, "tachyon")
		}
		response := runtime.ExecuteResponse(set, func(string) string { return "" })
		if got, ok := generatedHeaderValue(response.HeaderMutations, "x-example-served-by"); !ok || got != "tachyon" {
			t.Fatalf("response header mutation x-example-served-by: got %q present=%v want %q", got, ok, "tachyon")
		}
	})
	t.Run("sample_cookie_auth/admin_cookie_set", func(t *testing.T) {
		set := generatedRouteSet(t, "sample_cookie_auth")
		req := runtime.StaticRequest{MethodValue: "GET", HostValue: "example.com", PathValue: "/admin/dashboard", CookieValue: map[string]string{"role": "admin"}}
		result := runtime.ExecuteRequest(set, runtime.NewState(), req, "origin")
		if result.HasTerminal {
			t.Fatalf("unexpected terminal response: %#v", result.Terminal)
		}
		if got, ok := generatedHeaderValue(result.HeaderMutations, "x-role"); !ok || got != "admin" {
			t.Fatalf("request header mutation x-role: got %q present=%v want %q", got, ok, "admin")
		}
	})
	t.Run("sample_exact_path/healthz_exact", func(t *testing.T) {
		set := generatedRouteSet(t, "sample_exact_path")
		req := runtime.StaticRequest{MethodValue: "GET", HostValue: "example.com", PathValue: "/healthz"}
		result := runtime.ExecuteRequest(set, runtime.NewState(), req, "origin")
		if !result.HasTerminal {
			t.Fatal("expected terminal response")
		}
		if got := result.Terminal.Status; got != 200 {
			t.Fatalf("terminal status: got %d want 200", got)
		}
		if got := result.Terminal.Body; got != "ok" {
			t.Fatalf("terminal body: got %q want %q", got, "ok")
		}
	})
	t.Run("sample_headers/adds_headers", func(t *testing.T) {
		set := generatedRouteSet(t, "sample_headers")
		req := runtime.StaticRequest{MethodValue: "GET", HostValue: "example.com", PathValue: "/"}
		result := runtime.ExecuteRequest(set, runtime.NewState(), req, "origin")
		if result.HasTerminal {
			t.Fatalf("unexpected terminal response: %#v", result.Terminal)
		}
		if got, ok := generatedHeaderValue(result.HeaderMutations, "x-proxy"); !ok || got != "tachyon" {
			t.Fatalf("request header mutation x-proxy: got %q present=%v want %q", got, ok, "tachyon")
		}
		response := runtime.ExecuteResponse(set, func(string) string { return "" })
		if got, ok := generatedHeaderValue(response.HeaderMutations, "x-served-by"); !ok || got != "tachyon" {
			t.Fatalf("response header mutation x-served-by: got %q present=%v want %q", got, ok, "tachyon")
		}
	})
	t.Run("sample_query_filter/debug_denied", func(t *testing.T) {
		set := generatedRouteSet(t, "sample_query_filter")
		req := runtime.StaticRequest{MethodValue: "GET", HostValue: "example.com", PathValue: "/search", QueryValue: map[string]string{"debug": "1"}}
		result := runtime.ExecuteRequest(set, runtime.NewState(), req, "origin")
		if !result.HasTerminal {
			t.Fatal("expected terminal response")
		}
		if got := result.Terminal.Status; got != 403 {
			t.Fatalf("terminal status: got %d want 403", got)
		}
	})
	t.Run("sample_query_filter/non_debug_allowed", func(t *testing.T) {
		set := generatedRouteSet(t, "sample_query_filter")
		req := runtime.StaticRequest{MethodValue: "GET", HostValue: "example.com", PathValue: "/search", QueryValue: map[string]string{"debug": "0"}}
		result := runtime.ExecuteRequest(set, runtime.NewState(), req, "origin")
		if result.HasTerminal {
			t.Fatalf("unexpected terminal response: %#v", result.Terminal)
		}
	})
	t.Run("sample_rate_limit/first_hit_allows", func(t *testing.T) {
		set := generatedRouteSet(t, "sample_rate_limit")
		req := runtime.StaticRequest{MethodValue: "GET", HostValue: "example.com", PathValue: "/limited", HeadersValue: map[string]string{"x-api-key": "demo-key"}}
		result := runtime.ExecuteRequest(set, runtime.NewState(), req, "origin")
		if result.HasTerminal {
			t.Fatalf("unexpected terminal response: %#v", result.Terminal)
		}
	})
	t.Run("sample_suffix_match/json_suffix", func(t *testing.T) {
		set := generatedRouteSet(t, "sample_suffix_match")
		req := runtime.StaticRequest{MethodValue: "GET", HostValue: "example.com", PathValue: "/data/export.json"}
		result := runtime.ExecuteRequest(set, runtime.NewState(), req, "origin")
		if result.HasTerminal {
			t.Fatalf("unexpected terminal response: %#v", result.Terminal)
		}
		if got, ok := generatedHeaderValue(result.HeaderMutations, "accept"); !ok || got != "application/json" {
			t.Fatalf("request header mutation accept: got %q present=%v want %q", got, ok, "application/json")
		}
	})
	t.Run("sample_terminal/blocks_prefix", func(t *testing.T) {
		set := generatedRouteSet(t, "sample_terminal")
		req := runtime.StaticRequest{MethodValue: "GET", HostValue: "example.com", PathValue: "/blocked/demo"}
		result := runtime.ExecuteRequest(set, runtime.NewState(), req, "origin")
		if !result.HasTerminal {
			t.Fatal("expected terminal response")
		}
		if got := result.Terminal.Status; got != 451 {
			t.Fatalf("terminal status: got %d want 451", got)
		}
		if got := result.Terminal.Body; got != "blocked by intent" {
			t.Fatalf("terminal body: got %q want %q", got, "blocked by intent")
		}
	})
}

func TestGeneratedBudgets(t *testing.T) {
	t.Run("example_block_admin_debug/request.allocs", func(t *testing.T) {
		set := generatedRouteSet(t, "example_block_admin_debug")
		var state *runtime.State
		if generatedStatefulPolicy("example_block_admin_debug") {
			state = runtime.NewState()
		}
		req := runtime.StaticRequest{MethodValue: "GET", HostValue: "example.local", PathValue: "/admin", QueryValue: map[string]string{"debug": "1"}}
		allocs := testing.AllocsPerRun(100, func() {
			_ = runtime.ExecuteRequest(set, state, req, "origin")
		})
		if allocs > 3.000 {
			t.Fatalf("request alloc budget exceeded: got %0.3f want <= 3.000", allocs)
		}
	})
	t.Run("example_proxy_headers/request.allocs", func(t *testing.T) {
		set := generatedRouteSet(t, "example_proxy_headers")
		var state *runtime.State
		if generatedStatefulPolicy("example_proxy_headers") {
			state = runtime.NewState()
		}
		req := runtime.StaticRequest{MethodValue: "GET", HostValue: "example.local", PathValue: "/hello"}
		allocs := testing.AllocsPerRun(100, func() {
			_ = runtime.ExecuteRequest(set, state, req, "origin")
		})
		if allocs > 1.000 {
			t.Fatalf("request alloc budget exceeded: got %0.3f want <= 1.000", allocs)
		}
	})
	t.Run("example_proxy_headers/response.allocs", func(t *testing.T) {
		set := generatedRouteSet(t, "example_proxy_headers")
		allocs := testing.AllocsPerRun(100, func() {
			_ = runtime.ExecuteResponse(set, func(string) string { return "" })
		})
		if allocs > 0.000 {
			t.Fatalf("response alloc budget exceeded: got %0.3f want <= 0.000", allocs)
		}
	})
	t.Run("sample_cookie_auth/request.allocs", func(t *testing.T) {
		set := generatedRouteSet(t, "sample_cookie_auth")
		var state *runtime.State
		if generatedStatefulPolicy("sample_cookie_auth") {
			state = runtime.NewState()
		}
		req := runtime.StaticRequest{MethodValue: "GET", HostValue: "example.com", PathValue: "/admin/dashboard", CookieValue: map[string]string{"role": "admin"}}
		allocs := testing.AllocsPerRun(100, func() {
			_ = runtime.ExecuteRequest(set, state, req, "origin")
		})
		if allocs > 1.000 {
			t.Fatalf("request alloc budget exceeded: got %0.3f want <= 1.000", allocs)
		}
	})
	t.Run("sample_exact_path/request.allocs", func(t *testing.T) {
		set := generatedRouteSet(t, "sample_exact_path")
		var state *runtime.State
		if generatedStatefulPolicy("sample_exact_path") {
			state = runtime.NewState()
		}
		req := runtime.StaticRequest{MethodValue: "GET", HostValue: "example.com", PathValue: "/healthz"}
		allocs := testing.AllocsPerRun(100, func() {
			_ = runtime.ExecuteRequest(set, state, req, "origin")
		})
		if allocs > 3.000 {
			t.Fatalf("request alloc budget exceeded: got %0.3f want <= 3.000", allocs)
		}
	})
	t.Run("sample_headers/request.allocs", func(t *testing.T) {
		set := generatedRouteSet(t, "sample_headers")
		var state *runtime.State
		if generatedStatefulPolicy("sample_headers") {
			state = runtime.NewState()
		}
		req := runtime.StaticRequest{MethodValue: "GET", HostValue: "example.com", PathValue: "/"}
		allocs := testing.AllocsPerRun(100, func() {
			_ = runtime.ExecuteRequest(set, state, req, "origin")
		})
		if allocs > 1.000 {
			t.Fatalf("request alloc budget exceeded: got %0.3f want <= 1.000", allocs)
		}
	})
	t.Run("sample_headers/response.allocs", func(t *testing.T) {
		set := generatedRouteSet(t, "sample_headers")
		allocs := testing.AllocsPerRun(100, func() {
			_ = runtime.ExecuteResponse(set, func(string) string { return "" })
		})
		if allocs > 0.000 {
			t.Fatalf("response alloc budget exceeded: got %0.3f want <= 0.000", allocs)
		}
	})
	t.Run("sample_suffix_match/request.allocs", func(t *testing.T) {
		set := generatedRouteSet(t, "sample_suffix_match")
		var state *runtime.State
		if generatedStatefulPolicy("sample_suffix_match") {
			state = runtime.NewState()
		}
		req := runtime.StaticRequest{MethodValue: "GET", HostValue: "example.com", PathValue: "/data/export.json"}
		allocs := testing.AllocsPerRun(100, func() {
			_ = runtime.ExecuteRequest(set, state, req, "origin")
		})
		if allocs > 1.000 {
			t.Fatalf("request alloc budget exceeded: got %0.3f want <= 1.000", allocs)
		}
	})
	t.Run("sample_terminal/request.allocs", func(t *testing.T) {
		set := generatedRouteSet(t, "sample_terminal")
		var state *runtime.State
		if generatedStatefulPolicy("sample_terminal") {
			state = runtime.NewState()
		}
		req := runtime.StaticRequest{MethodValue: "GET", HostValue: "example.com", PathValue: "/blocked/demo"}
		allocs := testing.AllocsPerRun(100, func() {
			_ = runtime.ExecuteRequest(set, state, req, "origin")
		})
		if allocs > 3.000 {
			t.Fatalf("request alloc budget exceeded: got %0.3f want <= 3.000", allocs)
		}
	})
}

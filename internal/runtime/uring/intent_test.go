//go:build linux

package uring

import (
	"net/http"
	"strings"
	"testing"

	cur "tachyon/internal/intent/generated/current"
	irt "tachyon/internal/intent/runtime"
	"tachyon/internal/router"
)

// We test the lookup helpers directly and exercise the intent runtime
// via a staticView that mirrors what intentRequestView does.

func TestRawQueryLookup(t *testing.T) {
	cases := []struct {
		query string
		name  string
		want  string
	}{
		{"", "k", ""},
		{"k=v", "k", "v"},
		{"k=v&other=x", "k", "v"},
		{"other=x&k=v", "k", "v"},
		{"K=V", "k", "V"}, // case-insensitive key
		{"k=", "k", ""},
		{"k=v1&k=v2", "k", "v1"}, // first wins
		{"novalue", "novalue", ""},
		{"novalue&k=v", "k", "v"},
	}
	for _, tc := range cases {
		got := rawQueryLookup(tc.query, tc.name)
		if got != tc.want {
			t.Errorf("rawQueryLookup(%q, %q) = %q; want %q", tc.query, tc.name, got, tc.want)
		}
	}
}

func TestRawCookieLookup(t *testing.T) {
	cases := []struct {
		header string
		name   string
		want   string
	}{
		{"", "s", ""},
		{"session=abc", "session", "abc"},
		{"session=abc; role=admin", "role", "admin"},
		{"a=1;b=2;c=3", "b", "2"},
		{"Session=ABC", "session", "ABC"}, // case-insensitive
		{"novalue", "novalue", ""},
		{"k=v1; k=v2", "k", "v1"}, // first wins
	}
	for _, tc := range cases {
		got := rawCookieLookup(tc.header, tc.name)
		if got != tc.want {
			t.Errorf("rawCookieLookup(%q, %q) = %q; want %q", tc.header, tc.name, got, tc.want)
		}
	}
}

// routeSet builds a RoutePolicySet from the generated registry for one policy.
func routeSet(t *testing.T, policyName string) irt.RoutePolicySet {
	t.Helper()
	rp, err := cur.BuildRoutePrograms([]router.Rule{
		{RouteID: 1, Upstream: "origin", Intents: []string{policyName}},
	})
	if err != nil {
		t.Fatalf("BuildRoutePrograms: %v", err)
	}
	return rp.ByRouteID[1]
}

// staticView is a lightweight RequestView used in uring unit tests.
type staticView struct {
	method   string
	host     string
	path     string
	clientIP string
	headers  map[string]string // lower-cased
}

func (v staticView) Method() string { return v.method }
func (v staticView) Path() string   { return v.path }
func (v staticView) Host() string   { return v.host }
func (v staticView) ClientIP() string { return v.clientIP }
func (v staticView) Header(name string) string {
	return v.headers[strings.ToLower(name)]
}
func (v staticView) Query(name string) string {
	q := strings.IndexByte(v.path, '?')
	if q < 0 {
		return ""
	}
	return rawQueryLookup(v.path[q+1:], name)
}
func (v staticView) Cookie(name string) string {
	return rawCookieLookup(v.headers["cookie"], name)
}

func TestUringIntentHeaderMutation(t *testing.T) {
	set := routeSet(t, "sample_headers")
	state := irt.NewState()

	view := staticView{
		method: http.MethodGet,
		host:   "example.com",
		path:   "/hot",
	}
	result := irt.ExecuteRequest(set, state, view, "origin")
	if result.HasTerminal {
		t.Fatalf("unexpected terminal: %+v", result.Terminal)
	}
	val, ok := result.HeaderMutations.Find("x-proxy")
	if !ok || val != "tachyon" {
		t.Fatalf("x-proxy mutation: got %q present=%v; want tachyon", val, ok)
	}

	resp := irt.ExecuteResponse(set, func(string) string { return "" })
	val, ok = resp.HeaderMutations.Find("x-served-by")
	if !ok || val != "tachyon" {
		t.Fatalf("x-served-by response mutation: got %q present=%v", val, ok)
	}
}

func TestUringIntentTerminalRespond(t *testing.T) {
	set := routeSet(t, "sample_terminal")
	state := irt.NewState()

	view := staticView{
		method: http.MethodGet,
		host:   "example.com",
		path:   "/blocked/path",
	}
	result := irt.ExecuteRequest(set, state, view, "origin")
	if !result.HasTerminal {
		t.Fatal("expected terminal for /blocked path")
	}
	if result.Terminal.Status != 451 {
		t.Fatalf("terminal status: got %d want 451", result.Terminal.Status)
	}
	if result.Terminal.Body != "blocked by intent" {
		t.Fatalf("terminal body: got %q", result.Terminal.Body)
	}
}

func TestUringIntentQueryMatch(t *testing.T) {
	set := routeSet(t, "sample_query_filter")
	state := irt.NewState()

	// debug=1 should be denied
	view := staticView{
		method: http.MethodGet,
		host:   "example.com",
		path:   "/search?debug=1",
	}
	result := irt.ExecuteRequest(set, state, view, "origin")
	if !result.HasTerminal {
		t.Fatal("expected 403 deny for debug=1")
	}
	if result.Terminal.Status != 403 {
		t.Fatalf("status: got %d want 403", result.Terminal.Status)
	}

	// debug=0 should pass through
	view2 := staticView{
		method: http.MethodGet,
		host:   "example.com",
		path:   "/search?debug=0",
	}
	result2 := irt.ExecuteRequest(set, state, view2, "origin")
	if result2.HasTerminal {
		t.Fatalf("unexpected terminal for debug=0: %+v", result2.Terminal)
	}

	// no query at all should also pass through
	view3 := staticView{
		method: http.MethodGet,
		host:   "example.com",
		path:   "/search",
	}
	result3 := irt.ExecuteRequest(set, state, view3, "origin")
	if result3.HasTerminal {
		t.Fatalf("unexpected terminal for no query: %+v", result3.Terminal)
	}
}

func TestUringIntentCookieMatch(t *testing.T) {
	set := routeSet(t, "sample_cookie_auth")
	state := irt.NewState()

	// role=admin cookie → header mutation applied
	view := staticView{
		method:  http.MethodGet,
		host:    "example.com",
		path:    "/admin/dashboard",
		headers: map[string]string{"cookie": "role=admin"},
	}
	result := irt.ExecuteRequest(set, state, view, "origin")
	if result.HasTerminal {
		t.Fatalf("unexpected terminal: %+v", result.Terminal)
	}
	val, ok := result.HeaderMutations.Find("x-role")
	if !ok || val != "admin" {
		t.Fatalf("x-role mutation: got %q present=%v; want admin", val, ok)
	}

	// wrong cookie value → policy doesn't match, no mutation
	view2 := staticView{
		method:  http.MethodGet,
		host:    "example.com",
		path:    "/admin/dashboard",
		headers: map[string]string{"cookie": "role=viewer"},
	}
	result2 := irt.ExecuteRequest(set, state, view2, "origin")
	if result2.HasTerminal {
		t.Fatalf("unexpected terminal for viewer cookie: %+v", result2.Terminal)
	}
	if _, ok := result2.HeaderMutations.Find("x-role"); ok {
		t.Fatal("x-role should not be set for viewer cookie")
	}
}

func TestUringIntentNoMatch(t *testing.T) {
	set := routeSet(t, "sample_headers")
	state := irt.NewState()

	// Different host — policy should not fire
	view := staticView{
		method: http.MethodGet,
		host:   "other.example.com",
		path:   "/",
	}
	result := irt.ExecuteRequest(set, state, view, "origin")
	if result.HasTerminal {
		t.Fatalf("unexpected terminal for non-matching host: %+v", result.Terminal)
	}
	if result.HeaderMutations.Len() > 0 {
		t.Fatal("unexpected header mutations for non-matching host")
	}
}

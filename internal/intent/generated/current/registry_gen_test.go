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
	return false
}

func TestRegistryVersion(t *testing.T) {
	if Registry.Version == "" {
		t.Fatal("registry version must be set")
	}
}

func TestRegistryPolicyNames(t *testing.T) {
	if got := len(Registry.Policies); got != 1 {
		t.Fatalf("registry policy count: got %d want 1", got)
	}
	if _, ok := Registry.Policies["bad"]; !ok {
		t.Fatalf("missing policy bad")
	}
}

func TestGeneratedCases(t *testing.T) {
	t.Skip("no generated cases in bundle")
}

func TestGeneratedBudgets(t *testing.T) {
	t.Skip("no generated budgets in bundle")
}

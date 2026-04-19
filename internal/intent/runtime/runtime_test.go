package runtime

import (
	"testing"

	"tachyon/internal/router"
)

func TestBindRoutesRejectsUnknownPolicy(t *testing.T) {
	routes := []router.Rule{{RouteID: 1, Intents: []string{"missing"}}}
	_, err := BindRoutes(routes, EmptyRegistry())
	if err == nil {
		t.Fatal("expected unknown policy error")
	}
}

func TestBindRoutesMarksStdlibRequirement(t *testing.T) {
	reg := Registry{
		Version: "0.1",
		Policies: map[string]PolicyMeta{
			"authz": {Name: "authz", RequiresClassC: true},
		},
	}
	routes := []router.Rule{{RouteID: 7, Intents: []string{"authz"}}}
	got, err := BindRoutes(routes, reg)
	if err != nil {
		t.Fatalf("bind routes: %v", err)
	}
	if !got.RequiresStdlib {
		t.Fatal("expected stdlib requirement")
	}
	if !got.ByRouteID[7].RequiresStdlib {
		t.Fatal("expected route-local stdlib requirement")
	}
}

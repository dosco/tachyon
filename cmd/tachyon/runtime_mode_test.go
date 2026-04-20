package main

import (
	"errors"
	"testing"

	irt "tachyon/internal/intent/runtime"
)

func TestValidateRuntimeSelectionRejectsUringForStdlibIntent(t *testing.T) {
	err := validateRuntimeSelection(true, irt.RoutePrograms{RequiresStdlib: true})
	if !errors.Is(err, errStdlibOnlyIntentRuntime) {
		t.Fatalf("error = %v, want %v", err, errStdlibOnlyIntentRuntime)
	}
}

func TestValidateRuntimeSelectionAllowsStdlibIntentOnStdRuntime(t *testing.T) {
	if err := validateRuntimeSelection(false, irt.RoutePrograms{RequiresStdlib: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRuntimeSelectionAllowsUringWhenPoliciesAreCompatible(t *testing.T) {
	if err := validateRuntimeSelection(true, irt.RoutePrograms{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

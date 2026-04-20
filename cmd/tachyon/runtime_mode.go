package main

import (
	"errors"

	irt "tachyon/internal/intent/runtime"
)

var errStdlibOnlyIntentRuntime = errors.New("attached intents require stdlib runtime; run with -io=std")

func validateRuntimeSelection(useUring bool, programs irt.RoutePrograms) error {
	if useUring && programs.RequiresStdlib {
		return errStdlibOnlyIntentRuntime
	}
	return nil
}

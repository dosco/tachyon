//go:build !linux

package runtime

// PinToCPU is a no-op on non-Linux platforms. We keep the symbol so the rest
// of the codebase can call it unconditionally.
func PinToCPU(cpu int) error { return nil }

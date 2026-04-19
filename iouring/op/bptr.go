// Shared helper: take a buffer's base pointer as unsafe.Pointer without
// allocating. Kept in its own file because every op file wants it.

//go:build linux

package op

import "unsafe"

// bptr returns &b[0] as unsafe.Pointer, or nil for an empty slice. The
// kernel tolerates a nil data pointer when len=0.
func bptr(b []byte) unsafe.Pointer {
	if len(b) == 0 {
		return nil
	}
	return unsafe.Pointer(&b[0])
}

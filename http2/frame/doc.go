// Package frame is the HTTP/2 frame layer.
//
// One file per frame type. Each file defines:
//
//   - the frame's wire layout as a typed struct
//   - a zero-alloc Read function that fills the struct from a []byte
//   - an Append function that writes the frame into a caller buffer
//
// Common parts (9-byte frame header, length/type/flags/stream-id codec) live
// in header.go so every frame file can stay tiny.
package frame

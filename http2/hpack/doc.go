// Package hpack is a zero-allocation HPACK codec.
//
// # Design summary
//
//   - Static table: a generated `switch idx` returning .rodata slices. No
//     map, no allocation; the compiler turns the switch into a jump table.
//   - Dynamic table: a fixed ring of (nameOff, nameLen, valOff, valLen)
//     tuples indexing into a [4096]byte arena owned by each H2 conn. We
//     insert, we drop, we never allocate.
//   - Huffman: 256-entry table-driven decoder; bit-writer encoder.
//   - Insertion heuristic: only insert :status, content-type, server into
//     the dynamic table on the encoder side. Never insert date,
//     content-length, etag - they're per-response and just push useful
//     entries out.
//
// This gets roughly 70% of Pingora's compression ratio at a small fraction
// of the implementation complexity. If post-bench measurement shows we're
// leaving p99 on the table, we widen the heuristic - empirically.
//
// # Layout (planned)
//
//   - static_table.go    - generated switch; don't hand-edit
//   - dynamic_table.go   - ring in arena
//   - huffman_decode.go  - table-driven decoder
//   - huffman_encode.go  - bit writer
//   - decoder.go         - HPACK field decoder
//   - encoder.go         - HPACK field encoder with insertion heuristic
package hpack

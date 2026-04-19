// Package buf provides the allocation primitives used everywhere in tachyon's
// hot path: power-of-two byte slabs backed by sync.Pool, and an append-only
// arena that stores variable-length header keys and values packed into one
// contiguous buffer so the parser can hand out (offset, length) pairs instead
// of allocating per-header Go strings.
//
// # Why not just use make([]byte, n)?
//
// The fast path of a reverse proxy receives, parses, rewrites, and forwards
// many millions of bytes per second. Every allocation visits the Go heap,
// which means GC work proportional to live-object count. A proxy has very
// little truly long-lived state: once a request is done, everything about it
// can go back into a bounded pool. sync.Pool is the right shape for that, but
// using it well requires choosing a small number of size classes so pools
// actually get reused - hence Slab.
//
// # Layout
//
//   - slab.go  - size classes and Slab type.
//   - pool.go  - one sync.Pool per class, Get/Put API.
//   - arena.go - append-only byte storage + (off,len) index for header KV.
//
// # Typical usage
//
//	s := buf.Get(buf.ClassRead)       // 16 KiB []byte
//	defer buf.Put(s)
//
//	var a buf.Arena
//	a.Reset(s.Bytes())                // arena writes into s
//	k := a.Put([]byte("host"))        // (off, len) into s
//	v := a.Put([]byte("example.com"))
package buf

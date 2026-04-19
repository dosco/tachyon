// Package http2 is a zero-allocation HTTP/2 server implementation.
//
// # Status
//
// Phase 5 scaffolding. The production plan is:
//
//   - Server-side only. Upstream connections stay HTTP/1.1 (matches
//     Pingora's common deployment; halves the code we'd otherwise need).
//   - Frame set: DATA, HEADERS+CONTINUATION, SETTINGS, WINDOW_UPDATE,
//     RST_STREAM, GOAWAY, PING, PRIORITY (parse and discard). No
//     PUSH_PROMISE (deprecated).
//   - Flow control: int32 windows per conn/stream, adaptive receive-window
//     growth capped at 16 MiB to match Pingora's BDP tuning.
//   - Streams: fixed [256]slot per conn with short collision chain; GOAWAY
//     if exceeded.
//
// # Why not x/net/http2?
//
// x/net/http2 allocates roughly 8 objects per request and uses bufio.Reader
// for framing - both hurt p99. For a proxy where the H2 server face is the
// work, this matters.
//
// # Sub-packages
//
//   - http2/frame - one small file per frame family
//   - http2/hpack - static+dynamic tables, Huffman codec
//
// See docs/hpack.md for the HPACK design rationale.
package http2

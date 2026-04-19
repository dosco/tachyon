// Package op contains one file per io_uring operation we use.
//
// Each file is intentionally small (rarely over 60 lines) because each op is
// just "fill in these fields of an SQE, note the kernel version that added
// it, describe what the completion means."
//
// # Planned ops
//
//   - accept.go   - IORING_OP_ACCEPT, multishot variant
//   - recv.go     - IORING_OP_RECV + RECV_MULTISHOT + provided buffer id
//   - send.go     - IORING_OP_SEND + IORING_OP_SEND_ZC
//   - connect.go  - IORING_OP_CONNECT (+ linked timeout)
//   - close.go    - IORING_OP_CLOSE
//   - timeout.go  - IORING_OP_TIMEOUT, used for idle-conn reaping
//   - splice.go   - IORING_OP_SPLICE, for zero-user-space body forwarding
package op

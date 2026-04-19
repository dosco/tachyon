// user_data packing/unpacking.
//
// The event loop routes CQEs back to their owning connection via the
// 64-bit user_data word we set on submission:
//
//	 63            56 55           32 31            0
//	+------+--------+----+---+-------+---------------+
//	| op   |  slot  |  seq         |
//	+------+--------+----+---+-------+---------------+
//	 8 bits  24 bits           32 bits
//
// op: which operation the CQE belongs to. Ties into the dispatch switch
//     in server.go.
// slot: index into the worker's conn slab.
// seq: per-slot generation counter; bumped on close(). Lets late CQEs
//     for a recycled slot be dropped safely.

//go:build linux

package uring

type opTag uint8

const (
	opAcceptMulti opTag = iota + 1
	opRecvClient
	opSendClient
	opConnectUp
	opSendUp
	opRecvUp
	opCloseClient
	opCloseUp
	// opSendUpBody forwards request body bytes to the upstream during
	// body-forwarding state.
	//
	// P3a unified client-fd reads under a single multishot recv (opRecvClient),
	// so opRecvClientBody no longer exists — body bytes flow through
	// onRecvClient's state-dispatch into feedBodyBytes.
	opSendUpBody
	// opTick is the in-ring idle-reaper tick (P3h). One TIMEOUT SQE per
	// server, re-armed every 5 seconds. On fire, we walk the conn slab
	// and close any client conn stuck in stReadingRequest for > 30 s.
	// slot and seq are unused.
	opTick
	// opSpliceIn / opSpliceOut form the zero-copy body-forward chain
	// (P3f, plaintext only). SpliceIn moves bytes src_fd → pipe_write;
	// SpliceOut moves them pipe_read → dst_fd. Both encode the slot so
	// the completion handler can keep the pipeline going until
	// respBodyRemaining hits 0.
	opSpliceIn
	opSpliceOut
)

func packUD(op opTag, slot uint32, seq uint32) uint64 {
	return uint64(op)<<56 | uint64(slot&0x00FFFFFF)<<32 | uint64(seq)
}

func unpackUD(ud uint64) (opTag, uint32, uint32) {
	return opTag(ud >> 56), uint32((ud >> 32) & 0x00FFFFFF), uint32(ud)
}

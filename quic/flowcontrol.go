package quic

// Flow-control budgets (Phase 6). The numbers here are the initial
// windows we advertise to peers and the thresholds at which we emit
// MAX_DATA / MAX_STREAM_DATA updates. They mirror what
// encodeServerTransportParams ships: keep the two in sync.
//
// The conservative default of 1 MiB conn / 64 KiB stream keeps
// server memory predictable under many concurrent idle streams while
// still letting a single active stream burst without stalling.
const (
	localConnFlowWindow   uint64 = 1 << 20 // initial_max_data
	localStreamFlowWindow uint64 = 1 << 16 // initial_max_stream_data_*
)

// flowUpdateThreshold decides when to bump an advertised limit. When
// the peer has consumed more than (max - consumed) < threshold*max
// of the window, we issue a new MAX_*. 50% is the classic choice:
// large enough to amortize the per-update cost, small enough that a
// full BDP remains available during the round-trip.
const flowUpdateThreshold float64 = 0.5

// Package recovery implements RFC 9002 loss detection and RTT
// estimation. The shape mirrors the pseudo-code in the RFC:
//
//   - per packet-number-space tracking of sent ack-eliciting packets
//   - SRTT/RTTvar/min_rtt updated on newly-acked largest
//   - loss detection on packet-number threshold (default 3) or time
//     threshold (9/8 * max(latest_rtt, SRTT)) past send time
//   - PTO backoff computed as (SRTT + max(4*RTTvar, kGranularity) +
//     max_ack_delay) * 2^ptoCount
//
// Congestion control is handled by a separate CongestionController
// passed in; this package only tracks what was sent, what was lost,
// and how long the round-trip took.
package recovery

import (
	"time"
)

// Space is a packet-number space: Initial, Handshake, Application.
// Names match RFC 9001/9002.
type Space uint8

const (
	SpaceInitial Space = iota
	SpaceHandshake
	SpaceApplication
	numSpaces
)

// Packet is one tracked in-flight packet. The caller is responsible
// for retaining whatever it needs to retransmit the frames carried
// inside — this package only tracks metadata for RTT/loss decisions.
type Packet struct {
	Number       uint64
	Space        Space
	SentTime     time.Time
	Size         int  // bytes on the wire (for congestion accounting)
	AckEliciting bool // carried a frame that elicits an ACK
	InFlight     bool // counted against the congestion window
}

// Constants from RFC 9002 §6.1.2 / §A.2.
const (
	PacketThreshold   = 3
	TimeThresholdNum  = 9
	TimeThresholdDen  = 8
	Granularity       = 1 * time.Millisecond
	InitialRTT        = 333 * time.Millisecond
	MaxPTOBackoff     = 10
	PersistentCongDur = 3 // PTO periods before persistent congestion
)

// Recovery is the per-connection recovery state.
type Recovery struct {
	sent      [numSpaces]map[uint64]*Packet
	largestIn [numSpaces]uint64
	haveLarge [numSpaces]bool

	SRTT       time.Duration
	RTTVar     time.Duration
	MinRTT     time.Duration
	LatestRTT  time.Duration
	sampled    bool
	MaxAckDelay time.Duration

	PTOCount int
	// LossTime per-space is the time at which loss detection
	// should next fire for that space (zero = no pending loss).
	LossTime [numSpaces]time.Time
	// LastAckElicitingSent per-space tracks the most recent send
	// time used to arm the PTO timer.
	LastAckElicitingSent [numSpaces]time.Time
}

// New constructs a Recovery ready for use with the caller-specified
// max_ack_delay (from the peer's transport parameters).
func New(maxAckDelay time.Duration) *Recovery {
	r := &Recovery{MaxAckDelay: maxAckDelay}
	for i := range r.sent {
		r.sent[i] = make(map[uint64]*Packet)
	}
	return r
}

// OnSent records a newly-sent packet. Only ack-eliciting packets are
// useful for RTT/loss/PTO, but the map stores every packet so the
// caller can correlate ACK ranges exactly.
func (r *Recovery) OnSent(p Packet) {
	r.sent[p.Space][p.Number] = &p
	if p.AckEliciting {
		r.LastAckElicitingSent[p.Space] = p.SentTime
	}
}

// OnAck processes an ACK frame's decoded contents: the largest-
// acknowledged number, an inclusive list of acknowledged ranges
// (each [lo, hi]), the peer-reported ack_delay, and the arrival
// time. Returns the packets newly removed from the in-flight map.
func (r *Recovery) OnAck(space Space, largest uint64, ranges [][2]uint64, ackDelay time.Duration, now time.Time) []*Packet {
	sent := r.sent[space]
	var newly []*Packet
	// Walk each range and lift every acked packet out.
	for _, rg := range ranges {
		for n := rg[0]; n <= rg[1]; n++ {
			if pk, ok := sent[n]; ok {
				newly = append(newly, pk)
				delete(sent, n)
			}
		}
	}
	// Update RTT only when the largest-acknowledged is newly acked
	// AND is ack-eliciting (RFC 9002 §5.1).
	for _, pk := range newly {
		if pk.Number == largest && pk.AckEliciting {
			r.updateRTT(now.Sub(pk.SentTime), ackDelay)
			break
		}
	}
	if largest > r.largestIn[space] || !r.haveLarge[space] {
		r.largestIn[space] = largest
		r.haveLarge[space] = true
	}
	// A successful RTT sample resets the PTO backoff (§6.2.1).
	if len(newly) > 0 {
		r.PTOCount = 0
	}
	return newly
}

func (r *Recovery) updateRTT(sample, ackDelay time.Duration) {
	r.LatestRTT = sample
	if !r.sampled {
		r.MinRTT = sample
		r.SRTT = sample
		r.RTTVar = sample / 2
		r.sampled = true
		return
	}
	if sample < r.MinRTT {
		r.MinRTT = sample
	}
	// Adjusted sample: clip ack_delay to max_ack_delay, then subtract
	// if the remaining sample is still >= MinRTT (§5.3).
	adj := sample
	if ackDelay > r.MaxAckDelay {
		ackDelay = r.MaxAckDelay
	}
	if adj-ackDelay >= r.MinRTT {
		adj -= ackDelay
	}
	// EWMA updates: α=1/8 for SRTT, β=1/4 for RTTVar.
	var rttvar_sample time.Duration
	if r.SRTT > adj {
		rttvar_sample = r.SRTT - adj
	} else {
		rttvar_sample = adj - r.SRTT
	}
	r.RTTVar = (3*r.RTTVar + rttvar_sample) / 4
	r.SRTT = (7*r.SRTT + adj) / 8
}

// DetectLoss walks the in-flight map for space and returns packets
// considered lost under the threshold rules. The caller should
// retransmit their frames and free the Packet entries.
func (r *Recovery) DetectLoss(space Space, now time.Time) []*Packet {
	if !r.haveLarge[space] {
		return nil
	}
	lossDelay := r.SRTT
	if r.LatestRTT > lossDelay {
		lossDelay = r.LatestRTT
	}
	lossDelay = lossDelay * TimeThresholdNum / TimeThresholdDen
	if lossDelay < Granularity {
		lossDelay = Granularity
	}
	lostSendTime := now.Add(-lossDelay)

	largest := r.largestIn[space]
	var lost []*Packet
	r.LossTime[space] = time.Time{}
	for n, pk := range r.sent[space] {
		if n > largest {
			continue
		}
		// Packet threshold: more than PacketThreshold newer packets acked.
		if largest-n >= PacketThreshold || !pk.SentTime.After(lostSendTime) {
			lost = append(lost, pk)
			delete(r.sent[space], n)
			continue
		}
		// Otherwise compute when this packet will be declared lost.
		willBeLostAt := pk.SentTime.Add(lossDelay)
		if r.LossTime[space].IsZero() || willBeLostAt.Before(r.LossTime[space]) {
			r.LossTime[space] = willBeLostAt
		}
	}
	return lost
}

// PTOPeriod returns the current probe timeout period, RFC 9002 §6.2.1:
//   PTO = smoothed_rtt + max(4*rttvar, kGranularity) + max_ack_delay
// backed off by 2^PTOCount. Callers arm their timer at
// LastAckElicitingSent[space] + PTOPeriod().
func (r *Recovery) PTOPeriod() time.Duration {
	srtt := r.SRTT
	if !r.sampled {
		srtt = InitialRTT
	}
	v := 4 * r.RTTVar
	if v < Granularity {
		v = Granularity
	}
	period := srtt + v + r.MaxAckDelay
	for i := 0; i < r.PTOCount && i < MaxPTOBackoff; i++ {
		period *= 2
	}
	return period
}

// OnPTO is called when a PTO timer fires. It bumps the backoff; the
// caller should send ack-eliciting probes in the relevant space.
func (r *Recovery) OnPTO() {
	r.PTOCount++
}

// InFlight returns the number of packets currently tracked in space.
// Useful for tests and for congestion-control byte accounting.
func (r *Recovery) InFlight(space Space) int { return len(r.sent[space]) }

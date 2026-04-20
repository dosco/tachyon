// Package congestion implements QUIC congestion controllers. The only
// one here today is NewReno as described in RFC 9002 §7.3/§B: a
// straightforward slow-start + congestion-avoidance loop with one
// recovery period per loss event. The Controller interface is small
// enough that swapping in CUBIC or BBR later means a new file, not a
// refactor of the recovery loop that calls it.
package congestion

import "time"

// MaxDatagramSize is the conservative QUIC datagram floor (RFC 9000
// §14). We treat it as the MSS for congestion math.
const MaxDatagramSize = 1200

// Defaults from RFC 9002 §B.2.
const (
	InitialWindowPackets = 10
	MinWindowPackets     = 2
	LossReductionFactor  = 2 // cwnd halves on loss
	PersistentCongWindow = MinWindowPackets * MaxDatagramSize
)

// Controller is the slim interface the recovery loop talks to. The
// sender asks CanSend before putting a packet on the wire; reports
// OnSent so bytes_in_flight stays honest; reports OnAck/OnLost as the
// peer's ACK frames are processed; and falls back to OnPersistentCong
// when the PTO backoff indicates the path has gone dark.
type Controller interface {
	CanSend(bytesInFlight int) bool
	OnSent(bytes int)
	OnAck(acked int, sentTime time.Time, now time.Time)
	OnLost(lost int, largestLostSent time.Time)
	OnPersistentCong()
	Window() int
}

// NewReno is a reference NewReno. Fields are exported so tests and
// metrics can peek without a wrapper.
type NewReno struct {
	Cwnd            int
	Ssthresh        int
	RecoveryStart   time.Time
	inRecovery      bool
}

// New returns a NewReno at initial window and infinite ssthresh.
func New() *NewReno {
	return &NewReno{
		Cwnd:     InitialWindowPackets * MaxDatagramSize,
		Ssthresh: 1<<63 - 1,
	}
}

func (n *NewReno) Window() int { return n.Cwnd }

func (n *NewReno) CanSend(bytesInFlight int) bool {
	return bytesInFlight < n.Cwnd
}

func (n *NewReno) OnSent(int) {} // accounting lives in the recovery caller

// OnAck grows the window. Slow start doubles on each ack up to
// ssthresh; congestion avoidance adds MSS * acked / cwnd per RFC 9002.
// Acks for packets sent before the current recovery period entered
// are ignored for window growth.
func (n *NewReno) OnAck(acked int, sentTime, now time.Time) {
	if n.inRecovery && !sentTime.After(n.RecoveryStart) {
		return
	}
	n.inRecovery = false
	if n.Cwnd < n.Ssthresh {
		n.Cwnd += acked
		return
	}
	// Congestion avoidance: one MSS per RTT, approximated.
	n.Cwnd += MaxDatagramSize * acked / n.Cwnd
}

// OnLost halves cwnd unless we're still inside the recovery period
// started by the previous loss (RFC 9002 §7.3.2).
func (n *NewReno) OnLost(_ int, largestLostSent time.Time) {
	if n.inRecovery && !largestLostSent.After(n.RecoveryStart) {
		return
	}
	n.inRecovery = true
	n.RecoveryStart = largestLostSent
	n.Cwnd /= LossReductionFactor
	if n.Cwnd < MinWindowPackets*MaxDatagramSize {
		n.Cwnd = MinWindowPackets * MaxDatagramSize
	}
	n.Ssthresh = n.Cwnd
}

// OnPersistentCong collapses the window to the minimum and clears the
// recovery marker; the next ack will re-enter slow start.
func (n *NewReno) OnPersistentCong() {
	n.Cwnd = PersistentCongWindow
	n.inRecovery = false
}

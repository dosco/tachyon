// io_uring struct layouts and constants.
//
// x/sys/unix exposes only the syscall numbers for io_uring (SYS_IO_URING_*),
// not the ABI structs or flag constants. We mirror <linux/io_uring.h>
// here, keeping the layout binary-compatible with the kernel header.
//
// Kernel reference: include/uapi/linux/io_uring.h (Linux 6.x).

//go:build linux

package iouring

import "unsafe"

// ---------------------------------------------------------------------
// io_uring_params + sub-structs.
// ---------------------------------------------------------------------

// SQRingOffsets mirrors struct io_sqring_offsets.
type SQRingOffsets struct {
	Head        uint32
	Tail        uint32
	Ring_mask   uint32
	Ring_entries uint32
	Flags       uint32
	Dropped     uint32
	Array       uint32
	Resv1       uint32
	UserAddr    uint64
}

// CQRingOffsets mirrors struct io_cqring_offsets.
type CQRingOffsets struct {
	Head         uint32
	Tail         uint32
	Ring_mask    uint32
	Ring_entries uint32
	Overflow     uint32
	Cqes         uint32
	Flags        uint32
	Resv1        uint32
	UserAddr     uint64
}

// Params mirrors struct io_uring_params.
type Params struct {
	Sq_entries     uint32
	Cq_entries     uint32
	Flags          uint32
	Sq_thread_cpu  uint32
	Sq_thread_idle uint32
	Features       uint32
	Wq_fd          uint32
	Resv           [3]uint32
	Sq_off         SQRingOffsets
	Cq_off         CQRingOffsets
}

// ---------------------------------------------------------------------
// SQE + CQE.
// ---------------------------------------------------------------------

// SQE mirrors struct io_uring_sqe. 64 bytes on all supported arches.
// Fields named to match the kernel struct where possible; go-legal
// names differ where C uses unions.
type SQE struct {
	Opcode      uint8
	Flags       uint8
	IOPrio      uint16
	Fd          int32
	Off         uint64 // union: off / addr2 / cmd_op
	Addr        uint64 // union: addr / splice_off_in
	Len         uint32 // union: len / poll_flags / etc
	OpFlags     uint32 // union of every per-op flag word
	UserData    uint64
	BufIndex    uint16 // union: buf_index / buf_group
	Personality uint16
	SpliceFdIn  int32  // union: splice_fd_in / file_index / addr_len
	Addr3       uint64
	Pad2        [1]uint64
}

// Assert SQE is 64 bytes, matching the kernel ABI.
var _ = [1]struct{}{}[64-unsafe.Sizeof(SQE{})]

// CQE mirrors struct io_uring_cqe. 16 bytes.
type CQE struct {
	UserData uint64
	Res      int32
	Flags    uint32
}

var _ = [1]struct{}{}[16-unsafe.Sizeof(CQE{})]

// ---------------------------------------------------------------------
// Setup flags.
// ---------------------------------------------------------------------

const (
	SetupIOPoll       uint32 = 1 << 0
	SetupSQPoll       uint32 = 1 << 1
	SetupSQAff        uint32 = 1 << 2
	SetupCQSize       uint32 = 1 << 3
	SetupClamp        uint32 = 1 << 4
	SetupAttachWQ     uint32 = 1 << 5
	SetupRDisabled    uint32 = 1 << 6
	SetupSubmitAll    uint32 = 1 << 7
	SetupCoopTaskrun  uint32 = 1 << 8
	SetupTaskrunFlag  uint32 = 1 << 9
	SetupSQE128       uint32 = 1 << 10
	SetupCQE32        uint32 = 1 << 11
	SetupSingleIssuer uint32 = 1 << 12
	SetupDeferTaskrun uint32 = 1 << 13
)

// ---------------------------------------------------------------------
// Enter flags.
// ---------------------------------------------------------------------

const (
	EnterGetEvents uint32 = 1 << 0
	EnterSQWakeup  uint32 = 1 << 1
	EnterSQWait    uint32 = 1 << 2
	EnterExtArg    uint32 = 1 << 3
	EnterRegRing   uint32 = 1 << 4
)

// ---------------------------------------------------------------------
// SQ ring flag bits (the mmap'd sqFlags word).
// ---------------------------------------------------------------------

const (
	SQNeedWakeup uint32 = 1 << 0
	SQCQOverflow uint32 = 1 << 1
	SQTaskrun    uint32 = 1 << 2
)

// ---------------------------------------------------------------------
// CQE flags.
// ---------------------------------------------------------------------

const (
	CQEFBuffer       uint32 = 1 << 0
	CQEFMore         uint32 = 1 << 1
	CQEFSockNonempty uint32 = 1 << 2
	CQEFNotif        uint32 = 1 << 3
	CQEFBufMore      uint32 = 1 << 4
)

// ---------------------------------------------------------------------
// Opcodes. Only the ones tachyon uses — add on demand.
// ---------------------------------------------------------------------

const (
	OpNop         = 0
	OpReadv       = 1
	OpWritev      = 2
	OpFsync       = 3
	OpReadFixed   = 4
	OpWriteFixed  = 5
	OpPollAdd     = 6
	OpPollRemove  = 7
	OpSendmsg     = 9
	OpRecvmsg     = 10
	OpTimeout     = 11
	OpAccept      = 13
	OpAsyncCancel = 14
	OpConnect     = 16
	OpClose       = 19
	OpRead        = 22
	OpWrite       = 23
	OpSend        = 26
	OpRecv        = 27
	OpSplice      = 30
	OpProvideBuffers = 31
	OpRemoveBuffers  = 32
	OpSendZC      = 44
	OpSendMsgZC   = 45
)

// ---------------------------------------------------------------------
// Register opcodes.
// ---------------------------------------------------------------------

const (
	RegisterBuffers   = 0
	RegisterFiles     = 2
	RegisterPbufRing  = 22
	UnregisterPbufRing = 23
)

// ---------------------------------------------------------------------
// Per-op flag bits the SQE's OpFlags union uses.
// ---------------------------------------------------------------------

const (
	// Recv/Send msg_flags live in OpFlags. We only need non-blocking
	// and "select from provided buffer group".
	RecvSendDontwait = 0x40 // MSG_DONTWAIT
	SQESelectBuffer  = 1 << 5 // IOSQE_BUFFER_SELECT

	// IOSQE_* bits live in SQE.Flags (not OpFlags).
	SQEFixedFile    = 1 << 0
	SQEIoDrain      = 1 << 1
	SQEIoLink       = 1 << 2
	SQEIoHardlink   = 1 << 3
	SQEAsync        = 1 << 4
	SQEBufferSelect = 1 << 5

	// Multishot accept/recv.
	AcceptMultishot = 1 << 0
	RecvMultishot   = 1 << 1 // IORING_RECV_MULTISHOT (via OpFlags for recv)
)

// Feature bits reported in Params.Features after setup.
const (
	FeatSingleMmap    = 1 << 0
	FeatNodrop        = 1 << 1
	FeatSubmitStable  = 1 << 2
	FeatRWCurPos      = 1 << 3
	FeatCurPersonality = 1 << 4
	FeatFastPoll      = 1 << 5
	FeatPoll32Bits    = 1 << 6
	FeatSQPollNonfixed = 1 << 7
	FeatExtArg        = 1 << 8
	FeatNativeWorkers = 1 << 9
	FeatRsrcTags      = 1 << 10
)

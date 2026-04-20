package main

import (
	"flag"
	"os"
	"time"
)

// flags holds all command-line knobs. Kept in its own type so main.go reads
// as a sequence of steps rather than a mix of parsing and logic.
type flags struct {
	config   string
	workers  int
	cpuBase  int
	// ioMode selects the event loop. "auto" (the default) uses io_uring on
	// Linux ≥5.7 and stdlib elsewhere. "std" forces the stdlib/epoll path
	// (faster on loopback-only deployments). "uring" forces io_uring.
	// End users should not have to set this; auto picks correctly.
	ioMode   string
	// cpuProfile, if set, writes a CPU pprof profile to this path for the
	// lifetime of the process. Used by Phase 7 (PGO) to capture a profile
	// from a real bench run that `go build -pgo=<file>` can consume.
	cpuProfile string
	// drain is the graceful-shutdown timeout: on SIGTERM/SIGINT we stop
	// accepting new conns and wait up to this long for in-flight
	// dispatches to complete before exiting. 0 = exit immediately.
	drain time.Duration
	// deadlineMode controls how often read/write deadlines are re-armed
	// on the client and upstream conns.
	//
	//   - "amortized" (default): bump every DeadlineMaxUses requests or
	//     every DeadlineRefresh wall-clock seconds. ~1000x fewer
	//     SetDeadline syscalls on the bench hot path; keep-alive conns
	//     still survive past the 2-minute window.
	//   - "perreq": bump on every request. Slower in benches but useful
	//     when bisecting a stall suspected to be deadline-related.
	deadlineMode string
	// bufZero controls how much of a pooled slab is zeroed before return.
	//
	//   - "bounded" (default): clear only s.b[:written], relying on the
	//     Slab's high-water mark tracking. Cheap — ~200–400 ns per request
	//     on the bench hot path, O(headers-bytes) not O(slab-cap).
	//   - "full": clear the full slab capacity on every Reset. Paranoid
	//     policy for deployments that don't trust callers to MarkWritten
	//     correctly, or want the absolute strongest cross-request
	//     isolation on TLS.
	bufZero string
	// debugAddr, when non-empty, enables a loopback-only HTTP endpoint
	// exposing net/http/pprof and Prometheus /metrics. Must bind to
	// 127.0.0.1, ::1, or localhost — startup refuses anything else.
	debugAddr string
	// accessLog toggles per-request debug logging via slog. Off by
	// default; when on, log.Enabled guards the cost so disabled
	// access-log is free on the hot path.
	accessLog bool
	// uringSQPoll enables IORING_SETUP_SQPOLL on each worker's ring
	// (P3g). The kernel spawns a dedicated poller thread per ring
	// that pulls SQEs directly, so the hot path no longer needs
	// io_uring_enter to submit. Off by default — costs one kernel
	// thread per worker, wasted on idle proxies; turn on for
	// sustained high-rate deployments. Only applies when -io=uring.
	uringSQPoll bool
	// spliceMin is the minimum body length (bytes) above which the uring
	// worker switches from recv+send to SPLICE zero-copy on plaintext
	// Content-Length response bodies (P3f). Below this threshold the
	// extra pipe + two-SQE chain overhead outweighs the bytes saved.
	//
	//   - 0           : SPLICE always disabled (recv+copy+send every body)
	//   - 16384 (def) : SPLICE on ≥16 KiB bodies
	//   - very large  : effectively disables SPLICE (for A/B benching)
	//
	// Only applies to -io=uring. On -io=std the stdlib net.TCPConn's
	// WriteTo already uses splice automatically for TCP→TCP io.Copy.
	spliceMin int64
	// workload is an operator hint for the `-io auto` resolver (P3j).
	//
	//   - "small"  : small bodies dominate (1-4 KiB GETs, JSON APIs).
	//                uring has no known win here; auto picks stdlib.
	//   - "mixed"  : unknown / default. Same as "small" until P3f's
	//                SPLICE gains are measured to hold on mixed traffic.
	//   - "big"    : ≥16 KiB bodies dominate (file downloads, video,
	//                large API responses). SPLICE zero-copies those;
	//                auto picks uring on kernels ≥5.7.
	//
	// With `-io std` / `-io uring`, this flag is ignored.
	workload string
}

func parseFlags() flags {
	return parseFlagsArgs(flag.CommandLine, nil)
}

func parseFlagsForServe(args []string) flags {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	return parseFlagsArgs(fs, args)
}

func parseFlagsArgs(fs *flag.FlagSet, args []string) flags {
	var f flags
	fs.StringVar(&f.config, "config", "intent/", "path to .intent source directory (compiled topology lives in the binary; this flag is retained for traffic subcommands that need the source tree)")
	fs.IntVar(&f.workers, "workers", 0,
		"number of worker processes; 0 = GOMAXPROCS, 1 = run in-process")
	fs.IntVar(&f.cpuBase, "cpu-base", 0,
		"first CPU to pin worker 0 to (linux only)")
	fs.StringVar(&f.ioMode, "io", "auto",
		"event loop: auto (default; io_uring on Linux ≥5.7, stdlib elsewhere), std (epoll; faster on loopback-only), or uring (force)")
	// Back-compat: -uring still works as "force uring".
	var legacyUring bool
	fs.BoolVar(&legacyUring, "uring", false,
		"deprecated alias for -io=uring")
	fs.StringVar(&f.cpuProfile, "cpuprofile", "",
		"write a CPU pprof profile here; file is suitable for `go build -pgo=<file>`")
	fs.DurationVar(&f.drain, "drain", 30*time.Second,
		"graceful shutdown timeout; on SIGTERM/SIGINT wait this long for in-flight requests to complete")
	fs.StringVar(&f.deadlineMode, "deadline-mode", "amortized",
		"when to re-arm read/write deadlines: amortized (default) or perreq (strict)")
	fs.StringVar(&f.bufZero, "buf-zero", "bounded",
		"pool-slab zeroing policy: bounded (default; clear written region) or full (clear entire slab)")
	fs.StringVar(&f.debugAddr, "debug-addr", "",
		"if set, serve pprof + /metrics on this loopback addr (e.g. 127.0.0.1:6060). Refuses non-loopback binds.")
	fs.BoolVar(&f.accessLog, "access-log", false,
		"emit one slog Debug line per request (off by default; off-path is zero cost)")
	fs.BoolVar(&f.uringSQPoll, "uring-sqpoll", false,
		"enable IORING_SETUP_SQPOLL on each uring worker ring (kernel poller thread per worker; off by default — only useful under sustained load). Requires -io=uring.")
	fs.StringVar(&f.workload, "workload", "mixed",
		"deprecated: no longer affects -io=auto selection; kept for backwards compatibility")
	fs.Int64Var(&f.spliceMin, "splice-min", 16384,
		"uring: min Content-Length (bytes) to trigger SPLICE body forward. 0 disables SPLICE; very large values also disable it (A/B benching). Requires -io=uring.")
	if args == nil {
		fs.Parse(os.Args[1:])
	} else {
		fs.Parse(args)
	}
	if legacyUring {
		f.ioMode = "uring"
	}
	return f
}

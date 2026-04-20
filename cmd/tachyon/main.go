// Command tachyon runs the reverse proxy. By default it forks one worker
// process per CPU (Linux only); -workers=1 runs a single process for local
// development.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"

	"tachyon/buf"
	cur "tachyon/internal/intent/generated/current"
	"tachyon/internal/proxy"
	"tachyon/internal/router"
	trt "tachyon/internal/runtime"
	"tachyon/internal/traffic"
	"tachyon/internal/upstream"
	"tachyon/quic"
)

func main() {
	handled, err := runCLI(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if handled {
		return
	}

	var f flags
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		f = parseFlagsForServe(os.Args[2:])
	} else {
		f = parseFlags()
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Apply -buf-zero.
	buf.SetFullZero(f.bufZero == "full")

	// --- Parent: print banner, fork or run inline ------------------------

	workerIdx, isChild := os.LookupEnv(trt.EnvWorkerID)
	n := f.workers
	if n == 0 {
		n = runtime.NumCPU()
	}
	if !isChild {
		cfg := cur.LoadConfig()
		if _, err := cur.BuildRoutePrograms(cfg.Routes); err != nil {
			log.Error("bind intents", "err", err)
			os.Exit(1)
		}
		tlsCfg := cur.TLSConfig()
		tlsAddr := ""
		if tlsCfg != nil {
			tlsAddr = tlsCfg.Addr
		}
		quicCfgBanner := cur.QUICConfig()
		quicAddr := ""
		if quicCfgBanner != nil {
			quicAddr = quicCfgBanner.Addr
		}
		printBanner(cfg.Listen, tlsAddr, quicAddr, n)
		if n > 1 && trt.CanFork() {
			if err := trt.ForkWorkers(n); err != nil {
				log.Error("fork failed", "err", err)
			}
			return
		}
	}

	// --- Child or single-process mode ------------------------------------

	idx := 0
	if isChild {
		if v, err := strconv.Atoi(workerIdx); err == nil {
			idx = v
		}
	}
	runWorker(f, idx, log)
}

func runWorker(f flags, idx int, log *slog.Logger) {
	runtime.GOMAXPROCS(1)
	_ = trt.PinToCPU(f.cpuBase + idx)

	if f.cpuProfile != "" && idx == 0 {
		pf, err := os.Create(f.cpuProfile)
		if err != nil {
			log.Error("cpuprofile create", "err", err, "path", f.cpuProfile)
			os.Exit(1)
		}
		if err := pprof.StartCPUProfile(pf); err != nil {
			log.Error("cpuprofile start", "err", err)
			os.Exit(1)
		}
		defer func() {
			pprof.StopCPUProfile()
			_ = pf.Close()
			log.Info("cpuprofile written", "path", f.cpuProfile)
		}()
	}

	cfg := cur.LoadConfig()
	tlsCfg := cur.TLSConfig()
	quicCfg := cur.QUICConfig()
	routePrograms, err := cur.BuildRoutePrograms(cfg.Routes)
	if err != nil {
		log.Error("bind intents", "err", err)
		os.Exit(1)
	}

	ln, err := trt.Listen(cfg.Listen)
	if err != nil {
		log.Error("listen", "err", err, "addr", cfg.Listen)
		os.Exit(1)
	}

	ctx, stop := installShutdownSignals(context.Background())
	defer stop()

	// QUIC endpoint lifted out to startQUIC once we've built the proxy
	// handler below. For the uring path we currently skip H3 (Phase 7).
	var quicEP *quic.Endpoint
	if quicCfg != nil && quicCfg.Addr != "" {
		pc, err := trt.ListenPacket(quicCfg.Addr)
		if err != nil {
			log.Error("quic listen", "err", err, "addr", quicCfg.Addr)
			os.Exit(1)
		}
		quicEP = quic.NewEndpoint(pc, log)
		if qtls, err := buildQUICTLSConfig(quicCfg); err != nil {
			log.Error("quic tls config", "err", err)
			os.Exit(1)
		} else {
			quicEP.SetTLSConfig(qtls)
		}
	}

	if idx == 0 {
		if out := os.Getenv(traffic.EnvRecordOut); out != "" {
			if err := traffic.Enable(out); err != nil {
				log.Error("record enable", "err", err, "path", out)
				os.Exit(1)
			}
			defer traffic.Close()
		}
	}

	if idx == 0 {
		if err := startDebugServer(ctx, f.debugAddr, log); err != nil {
			log.Error("debug addr", "err", err)
			os.Exit(1)
		}
	}

	caps := probeUringCaps()
	useUring := resolveIOMode(f.ioMode, caps)
	if err := validateRuntimeSelection(useUring, routePrograms); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}

	if useUring {
		_ = ln.Close() // we build our own raw listen fd for uring
		if err := runUring(cfg, routePrograms, log, f.uringSQPoll, f.spliceMin); err != nil {
			log.Error("serve", "err", err)
			os.Exit(1)
		}
		return
	}

	h := proxy.NewHandler(router.New(cfg.Routes), upstream.NewPools(cfg.Upstreams), routePrograms)
	if quicEP != nil {
		startH3(ctx, quicEP, h, log)
	}
	if f.deadlineMode == "perreq" {
		h.SetStrictDeadlines(true)
	}
	if f.accessLog {
		alog := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
		h.SetAccessLog(alog)
	}
	w := &trt.Worker{Listener: ln, Handler: h, Log: log}

	// Optional TLS listener, driven entirely from the compiled TLSConfig.
	var tw *trt.Worker
	if tlsCfg != nil && tlsCfg.Addr != "" {
		var err error
		tw, _, err = startTLSWorker(tlsCfg, h, log, idx)
		if err != nil {
			log.Error("tls listen", "err", err, "addr", tlsCfg.Addr)
			os.Exit(1)
		}
		go func() {
			if err := tw.Serve(ctx); err != nil {
				log.Error("tls serve", "err", err)
			}
		}()
	}

	if err := w.Serve(ctx); err != nil {
		log.Error("serve", "err", err)
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), f.drain)
	defer cancel()
	ok := w.Drain(drainCtx)
	if tw != nil {
		if !tw.Drain(drainCtx) {
			ok = false
		}
	}
	if !ok {
		log.Warn("drain timeout; some requests still in flight", "drain", f.drain)
	}
	h.Pools().CloseAll()
}

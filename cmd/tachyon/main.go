// Command tachyon runs the reverse proxy. By default it forks one worker
// process per CPU (Linux only); -workers=1 runs a single process for local
// development.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"

	"tachyon/buf"
	"tachyon/internal/intent"
	cur "tachyon/internal/intent/generated/current"
	irt "tachyon/internal/intent/runtime"
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
		// Pick (or pick up) the shared TLS session-ticket seed before
		// forking so every worker inherits the same value via env and
		// therefore derives identical ticket keys — a ticket issued by
		// one worker resumes on any sibling.
		if ensureTicketSeed() == nil {
			log.Error("ticket seed: rand read failed")
			os.Exit(1)
		}
		// The banner prints the addresses the worker is *configured* to
		// bind. The worker prints a second, authoritative line after
		// the listeners have actually bound (see runWorker), so a
		// misconfiguration or kernel refusal surfaces in the log rather
		// than being masked by this advisory banner.
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

	cfg, tlsCfg, quicCfg, routePrograms := loadRuntimeConfig(log, f.config)

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

	// Build the shared proxy handler BEFORE branching on IO mode. TLS
	// and QUIC listeners share it with (or in uring mode, serve
	// alongside) the plaintext path, so it must exist regardless of
	// which plaintext runtime we pick.
	h := proxy.NewHandler(router.New(cfg.Routes), upstream.NewPools(cfg.Upstreams), routePrograms)
	if f.deadlineMode == "perreq" {
		h.SetStrictDeadlines(true)
	}
	if f.accessLog {
		alog := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
		h.SetAccessLog(alog)
	}

	// H3 endpoint runs in its own goroutines independently of the
	// TCP runtime — there's no reason to skip it on the uring path.
	if quicEP != nil {
		startH3(ctx, quicEP, h, log)
	}

	// TLS listener likewise runs in its own goroutine on the stdlib
	// event loop. Running it in parallel with uring's plaintext loop
	// is the whole point of this restructuring: pre-fix the uring
	// branch would return early (below) and skip this setup entirely,
	// so :8443 never bound and the banner silently lied.
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

	// Authoritative post-bind banner. Tells the operator exactly what
	// bound, as opposed to the parent-process banner which reflects
	// configured intent.
	logBoundListeners(log, cfg.Listen, useUring, tlsCfg, tw, quicEP)

	if useUring {
		_ = ln.Close() // we build our own raw listen fd for uring
		if err := runUring(cfg, routePrograms, log, f.uringSQPoll, f.spliceMin); err != nil {
			log.Error("serve", "err", err)
			os.Exit(1)
		}
		// uring.Serve currently returns on fatal error only; graceful
		// shutdown of the uring loop is a separate item. Drain the TLS
		// worker before exit if it's running.
		drainCtx, cancel := context.WithTimeout(context.Background(), f.drain)
		defer cancel()
		if tw != nil && !tw.Drain(drainCtx) {
			log.Warn("drain timeout; some TLS requests still in flight", "drain", f.drain)
		}
		h.Pools().CloseAll()
		return
	}

	w := &trt.Worker{Listener: ln, Handler: h, Log: log}

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

// loadRuntimeConfig parses .intent files at startup, falling back to
// compiled defaults if none are found.
func loadRuntimeConfig(log *slog.Logger, configDir string) (*router.Config, *router.TLSConfig, *router.QUICConfig, irt.RoutePrograms) {
	paths, err := filepath.Glob(filepath.Join(configDir, "*.intent"))
	if err != nil || len(paths) == 0 {
		log.Warn("no .intent files, using compiled defaults", "dir", configDir)
		cfg := cur.LoadConfig()
		progs, _ := cur.BuildRoutePrograms(cfg.Routes)
		return cfg, cur.TLSConfig(), cur.QUICConfig(), progs
	}

	bundle, err := intent.ParseFiles(paths)
	if err != nil {
		log.Error("parse intents", "err", err)
		os.Exit(1)
	}

	cfg := bundleToConfig(bundle)
	tlsCfg := bundleTLSConfig(bundle)
	quicCfg := bundleQUICConfig(bundle)
	programs := bundleToPrograms(bundle)

	log.Info("config loaded from intents", "routes", len(cfg.Routes), "pools", len(cfg.Upstreams))
	return cfg, tlsCfg, quicCfg, programs
}

func bundleTLSConfig(b intent.Bundle) *router.TLSConfig {
	if b.TLS == nil {
		return nil
	}
	return &router.TLSConfig{Addr: b.TLS.Addr, Cert: b.TLS.Cert, Key: b.TLS.Key}
}

func bundleQUICConfig(b intent.Bundle) *router.QUICConfig {
	if b.QUIC == nil {
		return nil
	}
	return &router.QUICConfig{Addr: b.QUIC.Addr, Cert: b.QUIC.Cert, Key: b.QUIC.Key, ALPN: b.QUIC.ALPN}
}

// bundleToConfig converts a parsed intent bundle into a router.Config.
func bundleToConfig(b intent.Bundle) *router.Config {
	cfg := &router.Config{
		Listen:    b.Listener.Addr,
		Routes:    make([]router.Rule, len(b.Routes)),
		Upstreams: make(map[string]router.Upstream, len(b.Pools)),
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	for i, r := range b.Routes {
		cfg.Routes[i] = router.Rule{
			Name:     r.Name,
			Host:     r.Host,
			Path:     r.Path,
			Upstream: r.Upstream,
			Intents:  r.Apply,
			RouteID:  i,
		}
	}
	for _, p := range b.Pools {
		cfg.Upstreams[p.Name] = router.Upstream{
			Addrs:          p.Addrs,
			IdlePerHost:    p.IdlePerHost,
			ConnectTimeout: p.ConnectTimeout,
			LBPolicy:       p.LBPolicy,
			OutlierDetection: p.OutlierDetection,
			HealthCheck:    p.HealthCheck,
			RetryBudget:    p.RetryBudget,
		}
	}
	return cfg
}

// bundleToPrograms converts a parsed intent bundle to route programs.
func bundleToPrograms(b intent.Bundle) irt.RoutePrograms {
	policies := make(map[string]irt.PolicyMeta, len(b.Policies))
	for _, p := range b.Policies {
		reqClassC := false
		for _, a := range p.Request {
			if a.Kind == irt.ActionAuthExternal {
				reqClassC = true
				break
			}
		}
		policies[p.Name] = irt.PolicyMeta{
			Name:           p.Name,
			Priority:       p.Priority,
			Match:          p.Match,
			Request:        p.Request,
			Response:       p.Response,
			Error:          p.Error,
			RequiresClassC: reqClassC,
		}
	}
	reg := irt.Registry{Version: b.Version, Policies: policies}
	rules := make([]router.Rule, len(b.Routes))
	for i, r := range b.Routes {
		rules[i] = router.Rule{
			Name:     r.Name,
			Host:     r.Host,
			Path:     r.Path,
			Upstream: r.Upstream,
			Intents:  r.Apply,
			RouteID:  i,
		}
	}
	programs, err := irt.BindRoutes(rules, reg)
	if err != nil {
		return irt.EmptyRoutePrograms()
	}
	return programs
}

// logBoundListeners emits a single structured log line with the actual
// addresses each listener succeeded at binding. This is the source of
// truth for "did TLS and QUIC actually come up" — the parent banner is
// only a configured-intent summary.
func logBoundListeners(log *slog.Logger, plain string, useUring bool, tlsCfg *router.TLSConfig, tw *trt.Worker, quicEP *quic.Endpoint) {
	mode := "stdlib"
	if useUring {
		mode = "uring"
	}
	attrs := []any{"plain", plain, "plain_io", mode}
	if tw != nil && tlsCfg != nil {
		attrs = append(attrs, "tls", tlsCfg.Addr)
	} else {
		attrs = append(attrs, "tls", "-")
	}
	if quicEP != nil {
		if p := quicEP.Port(); p != "" {
			attrs = append(attrs, "h3", ":"+p)
		} else {
			attrs = append(attrs, "h3", "bound")
		}
	} else {
		attrs = append(attrs, "h3", "-")
	}
	log.Info("listeners bound", attrs...)
}

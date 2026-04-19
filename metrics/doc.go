// Package metrics is tachyon's HDR-histogram based latency recorder.
//
// Every worker owns a histogram; on SIGUSR1 the workers atomically merge
// into a process-wide rollup and dump percentiles. No Prometheus scrape
// endpoint by default - a proxy under benchmark doesn't need one, and we
// don't want an HTTP server stealing cycles from the event loop.
//
// # Layout (planned)
//
//   - hdr.go   - HDR histogram (int64, 3 significant figures, configurable max)
//   - merge.go - per-worker -> process rollup, SIGUSR1 handler
package metrics

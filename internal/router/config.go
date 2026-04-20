// Package router defines the typed topology shape consumed by the proxy.
//
// The source of truth for these values is the .intent DSL; the compiler
// (`tachyon intent build`) generates Go that populates these structs and
// writes them to `internal/intent/generated/current/config_gen.go`. There is
// no YAML loader and no runtime config parsing.
package router

import "time"

// Rule is one routing rule.
//
// Exactly one of Upstream (single-upstream shorthand) or Upstreams
// (explicit weighted list) must be set. The compiler rejects rules with
// both set, or with neither.
type Rule struct {
	// Name is the stable identifier from the `.intent` source. Used by
	// topology case tests and debug tooling.
	Name      string
	Host      string
	Path      string
	Upstream  string
	Upstreams []WeightedUpstream
	Intents   []string

	// RouteID is assigned from source order at compile time. It is used by
	// the generated intent registry to bind route-local programs to the
	// router's match result.
	RouteID int
}

// WeightedUpstream is one entry in a Rule's weighted multi-upstream list.
// Weight is relative; 0 is normalised to 1 by the compiler.
type WeightedUpstream struct {
	Name   string
	Weight int
}

// Upstream is a named pool definition.
type Upstream struct {
	Addrs            []string
	IdlePerHost      int
	ConnectTimeout   time.Duration
	OutlierDetection *OutlierDetection
	// LBPolicy selects the upstream-address policy. Empty / "rr" =
	// round-robin (default); "p2c_ewma" = power-of-two-choices with
	// latency EWMA.
	LBPolicy    string
	HealthCheck *HealthCheck
	RetryBudget *RetryBudget
}

// HealthCheck configures the per-pool active health probe.
type HealthCheck struct {
	Interval time.Duration
	Path     string
	Timeout  time.Duration
}

// RetryBudget bounds retry traffic to a configurable fraction of
// successful requests. Nil → retries disabled.
type RetryBudget struct {
	RetryPercent int
	MinTokens    int
}

// OutlierDetection enables passive ejection of upstream addresses that
// return streaks of 5xx or gateway errors. Nil → ejection disabled.
type OutlierDetection struct {
	Consecutive5xx        int
	ConsecutiveGatewayErr int
	EjectionDuration      time.Duration
	MaxEjectedPercent     int
}

// Config is the full topology shape.
type Config struct {
	Listen    string
	Routes    []Rule
	Upstreams map[string]Upstream
}

// TLSConfig is the optional TLS listener definition; nil means no TLS listener.
type TLSConfig struct {
	Addr string
	Cert string
	Key  string
}

// QUICConfig is the optional HTTP/3 / QUIC listener definition; nil means
// no QUIC listener is bound. Cert/Key fall back to the sibling TLSConfig
// when left empty — both listeners typically share one cert.
type QUICConfig struct {
	Addr string
	Cert string
	Key  string
	// ALPN advertised in the TLS handshake. Defaults to ["h3"] when empty.
	ALPN []string
}

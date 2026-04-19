package router

import (
	"errors"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Rule is one entry from the config file.
//
// Exactly one of Upstream (single-upstream shorthand) or Upstreams
// (explicit weighted list) must be set. A Rule with both set, or with
// neither, is rejected at Load.
type Rule struct {
	Host      string             `yaml:"host"`
	Path      string             `yaml:"path"`
	Upstream  string             `yaml:"upstream"`
	Upstreams []WeightedUpstream `yaml:"upstreams"`
}

// WeightedUpstream is one entry in a Rule's weighted multi-upstream
// list. Weight is relative; 0 is normalised to 1 at Load. The router
// pre-computes a cumulative-weight table so pick cost is one rand +
// one linear scan (fine for 2–10 upstreams; nothing bigger is
// realistic).
type WeightedUpstream struct {
	Name   string `yaml:"name"`
	Weight int    `yaml:"weight"`
}

// Upstream is a named pool definition.
type Upstream struct {
	Addrs            []string          `yaml:"addrs"`
	IdlePerHost      int               `yaml:"idle_per_host"`
	ConnectTimeout   time.Duration     `yaml:"connect_timeout"`
	OutlierDetection *OutlierDetection `yaml:"outlier_detection"`
	// LBPolicy selects the upstream-address policy. Empty / "rr" =
	// round-robin (default); "p2c_ewma" = power-of-two-choices with
	// latency EWMA. Unknown values are rejected at Load.
	LBPolicy     string        `yaml:"lb_policy"`
	HealthCheck  *HealthCheck  `yaml:"health_check"`
	RetryBudget  *RetryBudget  `yaml:"retry_budget"`
}

// HealthCheck configures the per-pool active health probe. When present,
// tachyon starts one background goroutine per pool that issues an HTTP
// HEAD to every address at the given interval; addresses that fail are
// removed from rotation until they start responding again.
//
// Absent block → feature disabled (no goroutine, no overhead).
type HealthCheck struct {
	// Interval between probe rounds. Default "10s".
	Interval time.Duration `yaml:"interval"`
	// Path for the HTTP HEAD probe. Default "/health".
	Path string `yaml:"path"`
	// Timeout for the full dial + HTTP round-trip. Default "1s".
	Timeout time.Duration `yaml:"timeout"`
}

// RetryBudget bounds retry traffic to a configurable fraction of
// successful requests. Absent block → retries are never attempted
// (the handler sends 502 immediately on gateway error, as before).
type RetryBudget struct {
	// RetryPercent controls how quickly the budget replenishes.
	// One retry token is added per (100/RetryPercent) successes.
	// E.g. 20 means one token per 5 successes. Default 20.
	RetryPercent int `yaml:"retry_percent"`
	// MinTokens is always available regardless of recent success count.
	// Prevents starvation at startup or after idle periods. Default 3.
	MinTokens int `yaml:"min_tokens"`
}

// OutlierDetection enables passive ejection of upstream addresses that
// return streaks of 5xx or gateway errors. Leaving the whole section
// absent from the YAML disables ejection entirely (no runtime cost on
// the hot path). Zero fields within the struct fall back to defaults.
type OutlierDetection struct {
	Consecutive5xx        int           `yaml:"consecutive_5xx"`
	ConsecutiveGatewayErr int           `yaml:"consecutive_gateway_errors"`
	EjectionDuration      time.Duration `yaml:"ejection_duration"`
	MaxEjectedPercent     int           `yaml:"max_ejected_percent"`
}

// Config is the full YAML shape.
type Config struct {
	Listen    string              `yaml:"listen"`
	Routes    []Rule              `yaml:"routes"`
	Upstreams map[string]Upstream `yaml:"upstreams"`
}

// Load parses a YAML config from path and applies defaults where fields are
// zero. The returned Config is ready to pass to the proxy glue.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	for name, u := range c.Upstreams {
		if u.IdlePerHost == 0 {
			u.IdlePerHost = 256
		}
		if u.ConnectTimeout == 0 {
			u.ConnectTimeout = time.Second
		}
		switch u.LBPolicy {
		case "", "rr", "p2c_ewma":
		default:
			return nil, errors.New("router: unknown lb_policy " + u.LBPolicy + " for upstream " + name)
		}
		c.Upstreams[name] = u
	}
	if len(c.Routes) == 0 {
		return nil, errors.New("router: config has no routes")
	}
	for i, rule := range c.Routes {
		hasSingle := rule.Upstream != ""
		hasMulti := len(rule.Upstreams) > 0
		if hasSingle && hasMulti {
			return nil, errors.New("router: route may set either upstream or upstreams, not both")
		}
		if !hasSingle && !hasMulti {
			return nil, errors.New("router: route has no upstream")
		}
		if hasMulti {
			for j, wu := range rule.Upstreams {
				if wu.Name == "" {
					return nil, errors.New("router: route upstream entry has empty name")
				}
				if wu.Weight < 0 {
					return nil, errors.New("router: route upstream weight must be >= 0")
				}
				if wu.Weight == 0 {
					rule.Upstreams[j].Weight = 1
				}
			}
			c.Routes[i] = rule
		}
	}
	return &c, nil
}

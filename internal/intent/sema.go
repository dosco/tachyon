package intent

import (
	"path/filepath"
	"strings"

	rt "tachyon/internal/intent/runtime"
	"tachyon/internal/router"
)

// RuntimeClass describes the capability tier a policy requires.
type RuntimeClass int

const (
	ClassA RuntimeClass = iota
	ClassB
	ClassC
)

func (c RuntimeClass) String() string {
	switch c {
	case ClassA:
		return "A"
	case ClassB:
		return "B"
	case ClassC:
		return "C"
	default:
		return "unknown"
	}
}

// IRPolicy is a validated, canonicalized policy ready for code generation.
type IRPolicy struct {
	Name           string
	Priority       int
	Match          []rt.MatchCondition
	Request        []rt.Action
	Response       []rt.Action
	Error          []rt.Action
	Cases          []PolicyCase
	Budget         PolicyBudget
	Primitives     []string
	Class          RuntimeClass
	RequiresClassC bool
	SourceFile     string
}

// IRBundle is a validated bundle ready for code generation.
type IRBundle struct {
	Version   string
	Policies  []IRPolicy
	Pools     []Pool
	Routes    []Route // with RouteIDs assigned and upstreams normalised
	Listener  Listener
	TLS       *TLSConfig
	QUIC      *QUICConfig
	TopoCases []TopoCase
}

// requestOnlyActions cannot appear in response or error blocks.
var requestOnlyActions = map[rt.ActionKind]bool{
	rt.ActionRespond:        true,
	rt.ActionDeny:           true,
	rt.ActionRedirect:       true,
	rt.ActionRouteTo:        true,
	rt.ActionCanary:         true,
	rt.ActionStripPrefix:    true,
	rt.ActionAddPrefix:      true,
	rt.ActionRateLimitLocal: true,
	rt.ActionAuthExternal:   true,
}

// Check validates a Bundle and returns an IRBundle ready for code generation.
func Check(bundle Bundle) (IRBundle, error) {
	out := IRBundle{Version: bundle.Version, Policies: make([]IRPolicy, 0, len(bundle.Policies))}
	policyNames := map[string]bool{}
	for _, p := range bundle.Policies {
		ir, err := checkPolicy(p)
		if err != nil {
			return IRBundle{}, err
		}
		out.Policies = append(out.Policies, ir)
		policyNames[p.Name] = true
	}

	// Pools.
	poolNames := map[string]bool{}
	for _, pool := range bundle.Pools {
		if pool.Name == "" || pool.Name == "*" {
			return IRBundle{}, errf("E315", "pool %q: reserved pool name", pool.Name)
		}
		if len(pool.Addrs) == 0 {
			return IRBundle{}, errf("E310", "pool %q: addrs must not be empty", pool.Name)
		}
		switch pool.LBPolicy {
		case "", "rr", "p2c_ewma":
		default:
			return IRBundle{}, errf("E308", "pool %q: unknown lb_policy %q", pool.Name, pool.LBPolicy)
		}
		if pool.OutlierDetection != nil {
			if pool.OutlierDetection.MaxEjectedPercent < 0 || pool.OutlierDetection.MaxEjectedPercent > 100 {
				return IRBundle{}, errf("E311", "pool %q: max_ejected_percent must be in 0..100", pool.Name)
			}
		}
		if pool.HealthCheck != nil && pool.HealthCheck.Path != "" && !strings.HasPrefix(pool.HealthCheck.Path, "/") {
			return IRBundle{}, errf("E320", "pool %q: health_check path must start with /", pool.Name)
		}
		if pool.RetryBudget != nil {
			if pool.RetryBudget.RetryPercent < 0 || pool.RetryBudget.RetryPercent > 100 {
				return IRBundle{}, errf("E319", "pool %q: retry_percent must be in 0..100", pool.Name)
			}
		}
		poolNames[pool.Name] = true
		out.Pools = append(out.Pools, pool)
	}

	// Routes. Preserve bundle.Routes ordering; the order reflects source
	// order across all files after bundle assembly.
	routeNameSeen := map[string]bool{}
	type hostPathKey struct{ host, path string }
	hostPathSeen := map[hostPathKey]string{}
	for i, rr := range bundle.Routes {
		if rr.Name == "" {
			return IRBundle{}, errf("E305", "route at %s:%d: route block requires a name", rr.SourceFile, rr.Line)
		}
		if routeNameSeen[rr.Name] {
			return IRBundle{}, errf("E304", "duplicate route name %q", rr.Name)
		}
		routeNameSeen[rr.Name] = true

		hasSingle := rr.Upstream != ""
		hasMulti := len(rr.Upstreams) > 0
		if hasSingle && hasMulti {
			return IRBundle{}, errf("E306", "route %q: set either upstream or upstreams, not both", rr.Name)
		}
		if !hasSingle && !hasMulti {
			return IRBundle{}, errf("E306", "route %q: no upstream specified", rr.Name)
		}
		if hasSingle {
			if !poolNames[rr.Upstream] {
				return IRBundle{}, errf("E300", "route %q: references unknown pool %q", rr.Name, rr.Upstream)
			}
		}
		if hasMulti {
			for j, wu := range rr.Upstreams {
				if wu.Name == "" {
					return IRBundle{}, errf("E301", "route %q: weighted upstream entry %d has empty name", rr.Name, j)
				}
				if !poolNames[wu.Name] {
					return IRBundle{}, errf("E301", "route %q: weighted upstream %q references unknown pool", rr.Name, wu.Name)
				}
				if wu.Weight < 0 {
					return IRBundle{}, errf("E309", "route %q: weighted upstream %q has negative weight", rr.Name, wu.Name)
				}
				if wu.Weight == 0 {
					rr.Upstreams[j].Weight = 1
				}
			}
		}
		for _, applyName := range rr.Apply {
			if !policyNames[applyName] {
				return IRBundle{}, errf("E302", "route %q: apply references undefined policy %q", rr.Name, applyName)
			}
		}
		key := hostPathKey{host: rr.Host, path: rr.Path}
		if other, ok := hostPathSeen[key]; ok {
			return IRBundle{}, errf("E306", "route %q: conflicts with route %q (same host+path)", rr.Name, other)
		}
		hostPathSeen[key] = rr.Name
		// Carry route ID by slice index.
		_ = i
		out.Routes = append(out.Routes, rr)
	}

	if len(out.Routes) == 0 && len(bundle.Pools) > 0 {
		return IRBundle{}, errf("E316", "no routes declared; at least one route is required")
	}

	// Listener.
	listener := bundle.Listener
	if listener.Addr == "" {
		listener.Addr = ":8080"
	}
	if !strings.Contains(listener.Addr, ":") {
		return IRBundle{}, errf("E314", "listener addr %q: missing port", listener.Addr)
	}
	out.Listener = listener

	// TLS.
	if bundle.TLS != nil {
		tls := *bundle.TLS
		// Both-or-neither for cert/key.
		hasCert := tls.Cert != ""
		hasKey := tls.Key != ""
		if hasCert != hasKey {
			return IRBundle{}, errf("E313", "tls block must set both cert and key, or neither")
		}
		// Resolve cert/key relative to the source file's directory. Find a
		// representative source file to anchor the path. Prefer any .intent
		// file in the bundle.
		anchor := anchorDir(bundle)
		if hasCert {
			tls.Cert = resolveRelative(anchor, tls.Cert)
			tls.Key = resolveRelative(anchor, tls.Key)
		}
		if tls.Addr != "" && !strings.Contains(tls.Addr, ":") {
			return IRBundle{}, errf("E314", "tls listen %q: missing port", tls.Addr)
		}
		if tls.Addr != "" && tls.Addr == listener.Addr {
			return IRBundle{}, errf("E317", "tls listen %q collides with plaintext listener", tls.Addr)
		}
		out.TLS = &tls
	}

	// QUIC (HTTP/3). Follows the same cert/key resolution rules as tls;
	// falls back to the sibling tls { ... } block when cert/key omitted.
	if bundle.QUIC != nil {
		q := *bundle.QUIC
		if q.Addr == "" {
			return IRBundle{}, errf("E325", "quic block must set listen")
		}
		if !strings.Contains(q.Addr, ":") {
			return IRBundle{}, errf("E314", "quic listen %q: missing port", q.Addr)
		}
		if q.Addr == listener.Addr {
			return IRBundle{}, errf("E317", "quic listen %q collides with plaintext listener", q.Addr)
		}
		hasCert := q.Cert != ""
		hasKey := q.Key != ""
		if hasCert != hasKey {
			return IRBundle{}, errf("E313", "quic block must set both cert and key, or neither")
		}
		anchor := anchorDir(bundle)
		if hasCert {
			q.Cert = resolveRelative(anchor, q.Cert)
			q.Key = resolveRelative(anchor, q.Key)
		} else if out.TLS != nil {
			q.Cert = out.TLS.Cert
			q.Key = out.TLS.Key
		} else {
			return IRBundle{}, errf("E326", "quic block requires cert/key, or a sibling tls { cert, key } block")
		}
		if len(q.ALPN) == 0 {
			q.ALPN = []string{"h3"}
		}
		out.QUIC = &q
	}

	// Topology cases: validate route references.
	for _, tc := range bundle.TopoCases {
		if tc.Expect.Route != "" && !routeNameSeen[tc.Expect.Route] {
			return IRBundle{}, errf("E318", "case %q: expect.route references undefined route %q", tc.Name, tc.Expect.Route)
		}
	}
	out.TopoCases = bundle.TopoCases

	return out, nil
}

// anchorDir picks a directory to resolve TLS-relative paths against. We use
// the directory of the first source file seen in the bundle (policies,
// pools, routes, or topology cases — whichever appears first).
func anchorDir(b Bundle) string {
	if len(b.Policies) > 0 && b.Policies[0].SourceFile != "" {
		return filepath.Dir(b.Policies[0].SourceFile)
	}
	if len(b.Pools) > 0 && b.Pools[0].SourceFile != "" {
		return filepath.Dir(b.Pools[0].SourceFile)
	}
	if len(b.Routes) > 0 && b.Routes[0].SourceFile != "" {
		return filepath.Dir(b.Routes[0].SourceFile)
	}
	if len(b.TopoCases) > 0 && b.TopoCases[0].SourceFile != "" {
		return filepath.Dir(b.TopoCases[0].SourceFile)
	}
	return "."
}

func resolveRelative(anchor, p string) string {
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return p
	}
	abs, err := filepath.Abs(filepath.Join(anchor, p))
	if err != nil {
		return filepath.Join(anchor, p)
	}
	return abs
}

func checkPolicy(p Policy) (IRPolicy, error) {
	for _, act := range p.Response {
		if requestOnlyActions[act.Kind] {
			return IRPolicy{}, errf("E200", "policy %q: %q is not valid in a response block", p.Name, act.Kind)
		}
	}
	for _, act := range p.Error {
		if requestOnlyActions[act.Kind] {
			return IRPolicy{}, errf("E200", "policy %q: %q is not valid in an error block", p.Name, act.Kind)
		}
	}
	terminals := 0
	for _, act := range p.Request {
		switch act.Kind {
		case rt.ActionRespond, rt.ActionDeny, rt.ActionRedirect:
			terminals++
		}
	}
	if terminals > 1 {
		return IRPolicy{}, errf("E201", "policy %q: multiple terminal actions in request block; only the first will execute", p.Name)
	}
	match := make([]rt.MatchCondition, len(p.Match))
	copy(match, p.Match)
	for i, cond := range match {
		if cond.Field == rt.MatchHeader {
			match[i].Name = strings.ToLower(cond.Name)
		}
	}
	seen := map[string]string{}
	for _, cond := range match {
		key := string(cond.Field) + "|" + cond.Name
		if prev, ok := seen[key]; ok && prev != cond.Value {
			return IRPolicy{}, errf("E202", "policy %q: contradictory conditions for %s(%q): %q vs %q",
				p.Name, cond.Field, cond.Name, prev, cond.Value)
		}
		seen[key] = cond.Value
	}
	class := ClassA
	for _, prim := range p.Primitives {
		switch rt.ActionKind(prim) {
		case rt.ActionAuthExternal:
			class = ClassC
		case rt.ActionRateLimitLocal, rt.ActionCanary:
			if class < ClassB {
				class = ClassB
			}
		}
	}
	return IRPolicy{
		Name:           p.Name,
		Priority:       p.Priority,
		Match:          match,
		Request:        p.Request,
		Response:       p.Response,
		Error:          p.Error,
		Cases:          p.Cases,
		Budget:         p.Budget,
		Primitives:     p.Primitives,
		Class:          class,
		RequiresClassC: p.RequiresClassC,
		SourceFile:     p.SourceFile,
	}, nil
}

// Unused import guard (router) — used in type references below.
var _ = router.Rule{}

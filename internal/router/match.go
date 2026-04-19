package router

import "math/rand/v2"

// Router is the externally visible matcher. It holds a per-host radix
// tree plus a wildcard tree used when no host-specific rule matches.
//
// Host matching is exact (case-insensitive); we do not implement
// suffix matching because Pingora doesn't either, and it complicates
// the hot path. Wildcard host "*" is the fallback.
type Router struct {
	hosts    map[string]*node
	wildcard *node
}

// MatchResult is the router's hot-path output. It carries the selected
// route's stable source-order ID plus the concrete upstream chosen for
// this request.
type MatchResult struct {
	Found    bool
	RouteID  int
	Upstream string
}

// New returns a Router populated from the given rules. Insertion order
// is irrelevant; longest-prefix-match resolves ambiguity.
//
// A rule with Upstream set becomes a single-upstream terminal (the
// fast path; no rand, no weighted pick at match time). A rule with
// Upstreams set becomes a weighted terminal with a pre-computed
// cumulative-weight table; this keeps Match's per-request cost to one
// rand + one linear scan over the list.
func New(rules []Rule) *Router {
	r := &Router{hosts: map[string]*node{}}
	for i, rule := range rules {
		if rule.RouteID == 0 && i != 0 {
			rule.RouteID = i
		}
		upstream, weighted := ruleTarget(rule)
		if rule.Host == "*" || rule.Host == "" {
			if r.wildcard == nil {
				r.wildcard = &node{}
			}
			r.wildcard.insert([]byte(rule.Path), upstream, weighted, rule.RouteID)
			continue
		}
		n, ok := r.hosts[rule.Host]
		if !ok {
			n = &node{}
			r.hosts[rule.Host] = n
		}
		n.insert([]byte(rule.Path), upstream, weighted, rule.RouteID)
	}
	return r
}

// ruleTarget normalises a Rule into the pair the radix node stores.
// A shorthand Upstream stays as a string (single-upstream fast path).
// A multi-upstream list becomes a cumulative-weight table; a list of
// length 1 is collapsed back to shorthand so a degenerate weighted
// entry still hits the fast path.
func ruleTarget(rule Rule) (string, []weightedEntry) {
	if rule.Upstream != "" {
		return rule.Upstream, nil
	}
	if len(rule.Upstreams) == 1 {
		return rule.Upstreams[0].Name, nil
	}
	entries := make([]weightedEntry, len(rule.Upstreams))
	cum := 0
	for i, wu := range rule.Upstreams {
		w := wu.Weight
		if w <= 0 {
			w = 1
		}
		cum += w
		entries[i] = weightedEntry{Name: wu.Name, CumWeight: cum}
	}
	return "", entries
}

// Match returns the selected route and upstream for (host, path), or a
// zero-value MatchResult with Found=false for no route.
//
// host and path are case-sensitive here; the proxy caller normalises
// host to lowercase before calling.
func (r *Router) Match(host string, path []byte) MatchResult {
	if n, ok := r.hosts[host]; ok {
		if term := n.match(path); term != nil {
			return pick(term)
		}
	}
	if r.wildcard != nil {
		if term := r.wildcard.match(path); term != nil {
			return pick(term)
		}
	}
	return MatchResult{}
}

// pick returns the terminal node's concrete match result. The
// single-upstream case is an untaken branch — no rand, no allocation.
// Weighted picks use math/rand/v2's goroutine-safe top-level generator;
// GOMAXPROCS=1 per worker means lock contention is nil.
func pick(n *node) MatchResult {
	if len(n.weighted) == 0 {
		return MatchResult{Found: true, RouteID: n.routeID, Upstream: n.upstream}
	}
	total := n.weighted[len(n.weighted)-1].CumWeight
	r := rand.IntN(total)
	for _, e := range n.weighted {
		if r < e.CumWeight {
			return MatchResult{Found: true, RouteID: n.routeID, Upstream: e.Name}
		}
	}
	// Unreachable; the last entry's CumWeight == total > r.
	return MatchResult{
		Found:    true,
		RouteID:  n.routeID,
		Upstream: n.weighted[len(n.weighted)-1].Name,
	}
}

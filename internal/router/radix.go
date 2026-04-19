package router

import "bytes"

// node is one node of a path-prefix trie. Routes always end at a node
// whose target is set. Lookup walks as far as it can and returns the
// deepest target it saw — classic longest-prefix-match.
//
// A terminal node carries exactly one of:
//
//   - upstream: a single-upstream shorthand (fast path; no rand, no
//     weighted pick). This is what every existing config produces and
//     what the bench exercises.
//   - weighted: a cumulative-weight table for a multi-upstream rule.
//     Picked per-request by Router.Match.
type node struct {
	// edge is the byte-string labelling this node's incoming edge.
	// Root's edge is empty.
	edge []byte

	// children is a small sorted slice keyed by children[i].edge[0].
	children []*node

	// Terminal target. Empty string and nil slice means structural-only.
	upstream string
	weighted []weightedEntry
}

// weightedEntry is one row of a terminal node's cumulative-weight
// table. CumWeight is the running sum; the last entry's CumWeight is
// the total. Picking is: r = rand.IntN(total); first entry with
// r < CumWeight wins.
type weightedEntry struct {
	Name      string
	CumWeight int
}

// isTerminal reports whether this node is a route terminus.
func (n *node) isTerminal() bool {
	return n.upstream != "" || len(n.weighted) > 0
}

// insert adds (prefix, target) to the tree rooted at n. Exactly one of
// upstream or weighted must be non-zero; the caller is responsible.
func (n *node) insert(prefix []byte, upstream string, weighted []weightedEntry) {
	if len(prefix) == 0 {
		n.upstream = upstream
		n.weighted = weighted
		return
	}
	for _, ch := range n.children {
		i := commonPrefix(prefix, ch.edge)
		if i == 0 {
			continue
		}
		if i == len(ch.edge) {
			ch.insert(prefix[i:], upstream, weighted)
			return
		}
		// Split ch: create an intermediate node with edge = ch.edge[:i],
		// push the old ch down under it.
		mid := &node{edge: ch.edge[:i]}
		ch.edge = ch.edge[i:]
		mid.children = append(mid.children, ch)
		for j := range n.children {
			if n.children[j] == ch {
				n.children[j] = mid
				break
			}
		}
		if i == len(prefix) {
			mid.upstream = upstream
			mid.weighted = weighted
		} else {
			mid.children = append(mid.children, &node{
				edge:     prefix[i:],
				upstream: upstream,
				weighted: weighted,
			})
		}
		return
	}
	n.children = append(n.children, &node{
		edge:     prefix,
		upstream: upstream,
		weighted: weighted,
	})
}

// match walks as far into the tree as path allows, returning the
// deepest terminal node seen. Returns nil for no match.
func (n *node) match(path []byte) *node {
	var best *node
	if n.isTerminal() {
		best = n
	}
	for _, ch := range n.children {
		if len(path) < len(ch.edge) {
			continue
		}
		if !bytes.Equal(path[:len(ch.edge)], ch.edge) {
			continue
		}
		if got := ch.match(path[len(ch.edge):]); got != nil {
			best = got
		} else if ch.isTerminal() {
			best = ch
		}
		break // at most one child can share a first byte
	}
	return best
}

func commonPrefix(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

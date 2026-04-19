package intent

import (
	"strings"

	rt "tachyon/internal/intent/runtime"
)

// RuntimeClass describes the capability tier a policy requires.
type RuntimeClass int

const (
	// ClassA policies use only local, stateless operations.
	ClassA RuntimeClass = iota
	// ClassB policies use local stateful operations (rate limiting, canary).
	ClassB
	// ClassC policies call external services and are not supported on uring paths.
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
	Version  string
	Policies []IRPolicy
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

// Check validates a Bundle, classifies each policy by RuntimeClass, and returns
// a validated IRBundle ready for code generation.
// Returns an *Error with a stable E2xx code on semantic failures.
func Check(bundle Bundle) (IRBundle, error) {
	out := IRBundle{Version: bundle.Version, Policies: make([]IRPolicy, 0, len(bundle.Policies))}
	for _, p := range bundle.Policies {
		ir, err := checkPolicy(p)
		if err != nil {
			return IRBundle{}, err
		}
		out.Policies = append(out.Policies, ir)
	}
	return out, nil
}

func checkPolicy(p Policy) (IRPolicy, error) {
	// E200: action used in wrong phase.
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

	// E201: multiple terminal actions in request block produce unreachable code.
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

	// Canonicalize: lowercase header names in match conditions.
	match := make([]rt.MatchCondition, len(p.Match))
	copy(match, p.Match)
	for i, cond := range match {
		if cond.Field == rt.MatchHeader {
			match[i].Name = strings.ToLower(cond.Name)
		}
	}

	// E202: contradictory match conditions (same field+name, different values).
	seen := map[string]string{}
	for _, cond := range match {
		key := string(cond.Field) + "|" + cond.Name
		if prev, ok := seen[key]; ok && prev != cond.Value {
			return IRPolicy{}, errf("E202", "policy %q: contradictory conditions for %s(%q): %q vs %q",
				p.Name, cond.Field, cond.Name, prev, cond.Value)
		}
		seen[key] = cond.Value
	}

	// Classify: highest class wins.
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

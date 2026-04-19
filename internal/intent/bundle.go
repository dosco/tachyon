package intent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	rt "tachyon/internal/intent/runtime"
)

type Policy struct {
	Name           string
	Priority       int
	Match          []rt.MatchCondition
	Request        []rt.Action
	Response       []rt.Action
	Error          []rt.Action
	Cases          []PolicyCase
	Budget         PolicyBudget
	Primitives     []string
	RequiresClassC bool
	SourceFile     string
}

type PolicyCase struct {
	Name    string
	Request CaseRequest
	Expect  CaseExpect
}

type CaseRequest struct {
	Method   string
	Host     string
	Path     string
	ClientIP string
	Headers  map[string]string
	Query    map[string]string
	Cookies  map[string]string
}

type CaseExpect struct {
	Status          *int
	Body            *string
	Upstream        string
	Path            string
	RequestHeaders  map[string]string
	ResponseHeaders map[string]string
}

type PolicyBudget struct {
	RequestAllocs  *float64
	ResponseAllocs *float64
}

type Bundle struct {
	Version  string
	Policies []Policy
}

func Discover(paths []string) ([]string, error) {
	if len(paths) > 0 {
		cp := append([]string(nil), paths...)
		sort.Strings(cp)
		return cp, nil
	}
	matches, err := filepath.Glob("intent/*.intent")
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}

func ParseFiles(paths []string) (Bundle, error) {
	files, err := Discover(paths)
	if err != nil {
		return Bundle{}, err
	}
	b := Bundle{Version: "0.1"}
	seen := map[string]string{}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Bundle{}, err
		}
		policies, version, err := parseSource(path, string(raw))
		if err != nil {
			return Bundle{}, err
		}
		if version != "" {
			b.Version = version
		}
		for _, p := range policies {
			if prev, ok := seen[p.Name]; ok {
				return Bundle{}, errf("E102", "duplicate policy %q in %s and %s", p.Name, prev, p.SourceFile)
			}
			seen[p.Name] = p.SourceFile
			b.Policies = append(b.Policies, p)
		}
	}
	sort.Slice(b.Policies, func(i, j int) bool {
		return strings.Compare(b.Policies[i].Name, b.Policies[j].Name) < 0
	})
	return b, nil
}

func (b Bundle) FindCase(ref string) (Policy, PolicyCase, error) {
	if strings.Contains(ref, "/") {
		policyName, caseName, _ := strings.Cut(ref, "/")
		for _, p := range b.Policies {
			if p.Name != policyName {
				continue
			}
			for _, c := range p.Cases {
				if c.Name == caseName {
					return p, c, nil
				}
			}
			return Policy{}, PolicyCase{}, fmt.Errorf("case %q not found on policy %q", caseName, policyName)
		}
		return Policy{}, PolicyCase{}, fmt.Errorf("policy %q not found", policyName)
	}

	var (
		foundPolicy *Policy
		foundCase   *PolicyCase
	)
	for i := range b.Policies {
		for j := range b.Policies[i].Cases {
			if b.Policies[i].Cases[j].Name != ref {
				continue
			}
			if foundPolicy != nil {
				return Policy{}, PolicyCase{}, fmt.Errorf("case name %q is ambiguous; use policy/case", ref)
			}
			foundPolicy = &b.Policies[i]
			foundCase = &b.Policies[i].Cases[j]
		}
	}
	if foundPolicy == nil || foundCase == nil {
		return Policy{}, PolicyCase{}, fmt.Errorf("case %q not found", ref)
	}
	return *foundPolicy, *foundCase, nil
}

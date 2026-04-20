package intent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	rt "tachyon/internal/intent/runtime"
	"tachyon/internal/router"
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

// Pool is an upstream pool declared via `pool NAME { ... }`.
type Pool struct {
	Name             string
	Addrs            []string
	IdlePerHost      int
	ConnectTimeout   time.Duration
	LBPolicy         string
	OutlierDetection *router.OutlierDetection
	HealthCheck      *router.HealthCheck
	RetryBudget      *router.RetryBudget
	SourceFile       string
	Line             int
}

// Route is a routing rule declared via `route NAME { ... }`.
type Route struct {
	Name       string
	Host       string
	Path       string
	Upstream   string
	Upstreams  []router.WeightedUpstream
	Apply      []string
	SourceFile string
	Line       int
}

// Listener is the plaintext listener block.
type Listener struct {
	Addr string
}

// TLSConfig is the optional `tls { ... }` block. Cert/Key are absolute after sema.
type TLSConfig struct {
	Addr string
	Cert string
	Key  string
}

// QUICConfig is the optional `quic { ... }` block. Cert/Key are absolute
// after sema; when Cert/Key are empty, the runtime falls back to the
// sibling TLSConfig. ALPN defaults to []string{"h3"} when empty.
type QUICConfig struct {
	Addr string
	Cert string
	Key  string
	ALPN []string
}

// TopoCase is a top-level routing assertion, declared via `case NAME { ... }`
// at file scope (not nested inside a policy).
type TopoCase struct {
	Name       string
	Request    CaseRequest
	Expect     TopoExpect
	SourceFile string
	Line       int
}

type TopoExpect struct {
	Route    string
	Upstream string
	Applied  []string
	Status   *int
}

type Bundle struct {
	Version   string
	Policies  []Policy
	Pools     []Pool
	Routes    []Route
	Listener  Listener
	TLS       *TLSConfig
	QUIC      *QUICConfig
	TopoCases []TopoCase
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
	seenPolicy := map[string]string{}
	seenPool := map[string]string{}
	seenRoute := map[string]string{}
	seenTopoCase := map[string]string{}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Bundle{}, err
		}
		file, err := parseSource(path, string(raw))
		if err != nil {
			return Bundle{}, err
		}
		if file.Version != "" {
			b.Version = file.Version
		}
		for _, p := range file.Policies {
			if prev, ok := seenPolicy[p.Name]; ok {
				return Bundle{}, errf("E102", "duplicate policy %q in %s and %s", p.Name, prev, p.SourceFile)
			}
			seenPolicy[p.Name] = p.SourceFile
			b.Policies = append(b.Policies, p)
		}
		for _, pool := range file.Pools {
			if prev, ok := seenPool[pool.Name]; ok {
				return Bundle{}, errf("E303", "duplicate pool %q in %s and %s", pool.Name, prev, pool.SourceFile)
			}
			seenPool[pool.Name] = pool.SourceFile
			b.Pools = append(b.Pools, pool)
		}
		for _, rr := range file.Routes {
			if prev, ok := seenRoute[rr.Name]; ok {
				return Bundle{}, errf("E304", "duplicate route %q in %s and %s", rr.Name, prev, rr.SourceFile)
			}
			seenRoute[rr.Name] = rr.SourceFile
			b.Routes = append(b.Routes, rr)
		}
		for _, tc := range file.TopoCases {
			if prev, ok := seenTopoCase[tc.Name]; ok {
				return Bundle{}, errf("E321", "duplicate topology case %q in %s and %s", tc.Name, prev, tc.SourceFile)
			}
			seenTopoCase[tc.Name] = tc.SourceFile
			b.TopoCases = append(b.TopoCases, tc)
		}
		if file.Listener != nil {
			if b.Listener.Addr != "" {
				return Bundle{}, errf("E322", "duplicate listener block in %s", path)
			}
			b.Listener = *file.Listener
		}
		if file.TLS != nil {
			if b.TLS != nil {
				return Bundle{}, errf("E323", "duplicate tls block in %s", path)
			}
			tls := *file.TLS
			b.TLS = &tls
		}
		if file.QUIC != nil {
			if b.QUIC != nil {
				return Bundle{}, errf("E324", "duplicate quic block in %s", path)
			}
			q := *file.QUIC
			b.QUIC = &q
		}
	}
	sort.Slice(b.Policies, func(i, j int) bool {
		return strings.Compare(b.Policies[i].Name, b.Policies[j].Name) < 0
	})
	sort.Slice(b.Pools, func(i, j int) bool {
		return strings.Compare(b.Pools[i].Name, b.Pools[j].Name) < 0
	})
	// Routes preserve source order; do not sort — route IDs depend on order.
	sort.Slice(b.TopoCases, func(i, j int) bool {
		return strings.Compare(b.TopoCases[i].Name, b.TopoCases[j].Name) < 0
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

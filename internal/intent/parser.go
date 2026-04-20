package intent

import (
	"bufio"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	rt "tachyon/internal/intent/runtime"
	"tachyon/internal/router"
)

var (
	reVersion  = regexp.MustCompile(`^intent_version\s+"([^"]+)"$`)
	rePolicy   = regexp.MustCompile(`^policy\s+([A-Za-z_][A-Za-z0-9_]*)\s*\{$`)
	reCase     = regexp.MustCompile(`^case\s+([A-Za-z_][A-Za-z0-9_]*)\s*\{$`)
	rePool     = regexp.MustCompile(`^pool\s+([A-Za-z_][A-Za-z0-9_]*)\s*\{$`)
	reRoute    = regexp.MustCompile(`^route\s+([A-Za-z_][A-Za-z0-9_]*)\s*\{$`)
	reListener = regexp.MustCompile(`^listener\s*\{$`)
	reTLS      = regexp.MustCompile(`^tls\s*\{$`)
	reQUIC     = regexp.MustCompile(`^quic\s*\{$`)
	reRouteAnon = regexp.MustCompile(`^route\s*\{$`)
)

// parsedFile is the raw output of a single .intent file parse.
type parsedFile struct {
	Version   string
	Policies  []Policy
	Pools     []Pool
	Routes    []Route
	Listener  *Listener
	TLS       *TLSConfig
	QUIC      *QUICConfig
	TopoCases []TopoCase
}

// parseSource parses a single .intent file. The parser is line-oriented.
// Top-level blocks supported: policy, pool, route, listener, tls, case.
func parseSource(path, src string) (*parsedFile, error) {
	out := &parsedFile{}
	lines := splitLines(src)
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(stripComment(lines[i]))
		if line == "" {
			continue
		}
		lineNo := i + 1

		if m := reVersion.FindStringSubmatch(line); m != nil {
			out.Version = m[1]
			continue
		}
		if m := rePolicy.FindStringSubmatch(line); m != nil {
			body, end, err := collectBlock(lines, i)
			if err != nil {
				return nil, errf("E002", "%s:%d: %v", path, lineNo, err)
			}
			p, err := parsePolicy(path, lineNo, m[1], body)
			if err != nil {
				return nil, err
			}
			out.Policies = append(out.Policies, p)
			i = end
			continue
		}
		if m := rePool.FindStringSubmatch(line); m != nil {
			body, end, err := collectBlock(lines, i)
			if err != nil {
				return nil, errf("E002", "%s:%d: %v", path, lineNo, err)
			}
			pool, err := parsePool(path, lineNo, m[1], body)
			if err != nil {
				return nil, err
			}
			out.Pools = append(out.Pools, pool)
			i = end
			continue
		}
		if m := reRoute.FindStringSubmatch(line); m != nil {
			body, end, err := collectBlock(lines, i)
			if err != nil {
				return nil, errf("E002", "%s:%d: %v", path, lineNo, err)
			}
			rr, err := parseRoute(path, lineNo, m[1], body)
			if err != nil {
				return nil, err
			}
			out.Routes = append(out.Routes, rr)
			i = end
			continue
		}
		if reRouteAnon.MatchString(line) {
			return nil, errf("E305", "%s:%d: route block requires a name", path, lineNo)
		}
		if reListener.MatchString(line) {
			body, end, err := collectBlock(lines, i)
			if err != nil {
				return nil, errf("E002", "%s:%d: %v", path, lineNo, err)
			}
			if out.Listener != nil {
				return nil, errf("E322", "%s:%d: duplicate listener block", path, lineNo)
			}
			l, err := parseListener(path, lineNo, body)
			if err != nil {
				return nil, err
			}
			out.Listener = &l
			i = end
			continue
		}
		if reTLS.MatchString(line) {
			body, end, err := collectBlock(lines, i)
			if err != nil {
				return nil, errf("E002", "%s:%d: %v", path, lineNo, err)
			}
			if out.TLS != nil {
				return nil, errf("E323", "%s:%d: duplicate tls block", path, lineNo)
			}
			tc, err := parseTLS(path, lineNo, body)
			if err != nil {
				return nil, err
			}
			out.TLS = &tc
			i = end
			continue
		}
		if reQUIC.MatchString(line) {
			body, end, err := collectBlock(lines, i)
			if err != nil {
				return nil, errf("E002", "%s:%d: %v", path, lineNo, err)
			}
			if out.QUIC != nil {
				return nil, errf("E324", "%s:%d: duplicate quic block", path, lineNo)
			}
			qc, err := parseQUIC(path, lineNo, body)
			if err != nil {
				return nil, err
			}
			out.QUIC = &qc
			i = end
			continue
		}
		if m := reCase.FindStringSubmatch(line); m != nil {
			body, end, err := collectBlock(lines, i)
			if err != nil {
				return nil, errf("E002", "%s:%d: %v", path, lineNo, err)
			}
			tc, err := parseTopoCase(path, lineNo, m[1], body)
			if err != nil {
				return nil, err
			}
			out.TopoCases = append(out.TopoCases, tc)
			i = end
			continue
		}
		return nil, errf("E001", "%s:%d: unexpected top-level line %q", path, lineNo, line)
	}
	return out, nil
}

// splitLines breaks source into lines preserving order, using a bufio scanner
// so CRLF is handled consistently.
func splitLines(src string) []string {
	sc := bufio.NewScanner(strings.NewReader(src))
	sc.Buffer(make([]byte, 0, 1024), 1024*1024)
	var out []string
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

// collectBlock, given the index of a line ending in `{`, scans forward and
// returns the inner body lines (between the opening brace and the matching
// close) along with the index of the closing `}`. Supports nested blocks.
func collectBlock(lines []string, start int) ([]string, int, error) {
	depth := 1
	body := []string{}
	for i := start + 1; i < len(lines); i++ {
		trim := strings.TrimSpace(stripComment(lines[i]))
		if trim == "}" {
			depth--
			if depth == 0 {
				return body, i, nil
			}
		} else if strings.HasSuffix(trim, "{") {
			depth++
		}
		body = append(body, lines[i])
	}
	return nil, 0, fmt.Errorf("unterminated block")
}

// parsePolicy parses the body of a policy block.
func parsePolicy(path string, startLine int, name string, body []string) (Policy, error) {
	p := Policy{Name: name, Priority: 100, SourceFile: path}
	var curCase *PolicyCase
	var curBudget *PolicyBudget
	section := ""
	sectionDepth := 0
	for i := 0; i < len(body); i++ {
		raw := body[i]
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		lineNo := startLine + i + 1
		if sectionDepth > 0 {
			if line == "}" {
				sectionDepth--
				if sectionDepth == 0 {
					if section == "case" && curCase != nil {
						p.Cases = append(p.Cases, *curCase)
						curCase = nil
					}
					if section == "budget" && curBudget != nil {
						p.Budget = *curBudget
						curBudget = nil
					}
					section = ""
				}
				continue
			}
			if strings.HasSuffix(line, "{") {
				sectionDepth++
				continue
			}
			switch section {
			case "request", "response", "error":
				action, primitive, err := parseAction(line)
				if err != nil {
					return Policy{}, errf("E020", "%s:%d: %v", path, lineNo, err)
				}
				p.Primitives = appendUnique(p.Primitives, primitive)
				if primitive == string(rt.ActionAuthExternal) {
					p.RequiresClassC = true
				}
				switch section {
				case "request":
					p.Request = append(p.Request, action)
				case "response":
					p.Response = append(p.Response, action)
				case "error":
					p.Error = append(p.Error, action)
				}
			case "budget":
				if err := parseBudgetLine(curBudget, line); err != nil {
					return Policy{}, errf("E022", "%s:%d: %v", path, lineNo, err)
				}
			case "case":
				if err := parseCaseLine(curCase, line); err != nil {
					return Policy{}, errf("E023", "%s:%d: %v", path, lineNo, err)
				}
			default:
				return Policy{}, errf("E021", "%s:%d: unexpected nested line %q", path, lineNo, line)
			}
			continue
		}

		// Top-level (policy body) directives.
		if strings.HasPrefix(line, "priority ") {
			v, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "priority ")))
			if err != nil {
				return Policy{}, errf("E011", "%s:%d: invalid priority", path, lineNo)
			}
			p.Priority = v
			continue
		}
		if strings.HasPrefix(line, "match ") {
			conds, err := parseMatch(strings.TrimSpace(strings.TrimPrefix(line, "match ")))
			if err != nil {
				return Policy{}, errf("E012", "%s:%d: %v", path, lineNo, err)
			}
			p.Match = conds
			continue
		}
		if strings.HasSuffix(line, "{") {
			head := strings.TrimSpace(strings.TrimSuffix(line, "{"))
			switch head {
			case "request", "response", "error":
				section = head
				sectionDepth = 1
				continue
			case "budget":
				section = "budget"
				curBudget = &PolicyBudget{}
				sectionDepth = 1
				continue
			}
			if m := reCase.FindStringSubmatch(line); m != nil {
				section = "case"
				curCase = &PolicyCase{
					Name: m[1],
					Request: CaseRequest{
						Method: "GET",
						Host:   "example.com",
						Path:   "/",
					},
				}
				sectionDepth = 1
				continue
			}
		}
		return Policy{}, errf("E013", "%s:%d: unexpected policy line %q", path, lineNo, line)
	}
	return p, nil
}

// parsePool parses the body of a pool block.
func parsePool(path string, startLine int, name string, body []string) (Pool, error) {
	pool := Pool{Name: name, SourceFile: path, Line: startLine}
	for i := 0; i < len(body); i++ {
		raw := body[i]
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		lineNo := startLine + i + 1
		if strings.HasSuffix(line, "{") {
			head := strings.TrimSpace(strings.TrimSuffix(line, "{"))
			inner, end, err := collectBlock(body, i)
			if err != nil {
				return Pool{}, errf("E002", "%s:%d: %v", path, lineNo, err)
			}
			switch head {
			case "outlier_detection":
				od, err := parseOutlierDetection(path, startLine+i+1, inner)
				if err != nil {
					return Pool{}, err
				}
				pool.OutlierDetection = od
			case "health_check":
				hc, err := parseHealthCheck(path, startLine+i+1, inner)
				if err != nil {
					return Pool{}, err
				}
				pool.HealthCheck = hc
			case "retry_budget":
				rb, err := parseRetryBudget(path, startLine+i+1, inner)
				if err != nil {
					return Pool{}, err
				}
				pool.RetryBudget = rb
			default:
				return Pool{}, errf("E330", "%s:%d: unknown pool sub-block %q", path, lineNo, head)
			}
			i = end
			continue
		}
		key, rest, ok := cutDirective(line)
		if !ok {
			return Pool{}, errf("E330", "%s:%d: unexpected pool line %q", path, lineNo, line)
		}
		args := splitArgs(rest)
		switch key {
		case "addrs":
			for _, a := range args {
				pool.Addrs = append(pool.Addrs, trimQuoted(a))
			}
		case "idle_per_host":
			v, err := strconv.Atoi(strings.TrimSpace(rest))
			if err != nil {
				return Pool{}, errf("E330", "%s:%d: invalid idle_per_host", path, lineNo)
			}
			pool.IdlePerHost = v
		case "connect_timeout":
			d, err := time.ParseDuration(trimQuoted(rest))
			if err != nil {
				return Pool{}, errf("E307", "%s:%d: invalid duration %q", path, lineNo, rest)
			}
			pool.ConnectTimeout = d
		case "lb_policy":
			pool.LBPolicy = trimQuoted(rest)
		default:
			return Pool{}, errf("E330", "%s:%d: unknown pool field %q", path, lineNo, key)
		}
	}
	return pool, nil
}

func parseOutlierDetection(path string, startLine int, body []string) (*router.OutlierDetection, error) {
	od := &router.OutlierDetection{}
	for i, raw := range body {
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		lineNo := startLine + i
		key, rest, ok := cutDirective(line)
		if !ok {
			return nil, errf("E330", "%s:%d: unexpected outlier_detection line %q", path, lineNo, line)
		}
		switch key {
		case "consecutive_5xx":
			v, err := strconv.Atoi(strings.TrimSpace(rest))
			if err != nil {
				return nil, errf("E330", "%s:%d: invalid consecutive_5xx", path, lineNo)
			}
			od.Consecutive5xx = v
		case "consecutive_gateway_errors":
			v, err := strconv.Atoi(strings.TrimSpace(rest))
			if err != nil {
				return nil, errf("E330", "%s:%d: invalid consecutive_gateway_errors", path, lineNo)
			}
			od.ConsecutiveGatewayErr = v
		case "ejection_duration":
			d, err := time.ParseDuration(trimQuoted(rest))
			if err != nil {
				return nil, errf("E307", "%s:%d: invalid duration %q", path, lineNo, rest)
			}
			od.EjectionDuration = d
		case "max_ejected_percent":
			v, err := strconv.Atoi(strings.TrimSpace(rest))
			if err != nil {
				return nil, errf("E330", "%s:%d: invalid max_ejected_percent", path, lineNo)
			}
			od.MaxEjectedPercent = v
		default:
			return nil, errf("E330", "%s:%d: unknown outlier_detection field %q", path, lineNo, key)
		}
	}
	return od, nil
}

func parseHealthCheck(path string, startLine int, body []string) (*router.HealthCheck, error) {
	hc := &router.HealthCheck{}
	for i, raw := range body {
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		lineNo := startLine + i
		key, rest, ok := cutDirective(line)
		if !ok {
			return nil, errf("E330", "%s:%d: unexpected health_check line %q", path, lineNo, line)
		}
		switch key {
		case "interval":
			d, err := time.ParseDuration(trimQuoted(rest))
			if err != nil {
				return nil, errf("E307", "%s:%d: invalid duration %q", path, lineNo, rest)
			}
			hc.Interval = d
		case "path":
			hc.Path = trimQuoted(rest)
		case "timeout":
			d, err := time.ParseDuration(trimQuoted(rest))
			if err != nil {
				return nil, errf("E307", "%s:%d: invalid duration %q", path, lineNo, rest)
			}
			hc.Timeout = d
		default:
			return nil, errf("E330", "%s:%d: unknown health_check field %q", path, lineNo, key)
		}
	}
	return hc, nil
}

func parseRetryBudget(path string, startLine int, body []string) (*router.RetryBudget, error) {
	rb := &router.RetryBudget{}
	for i, raw := range body {
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		lineNo := startLine + i
		key, rest, ok := cutDirective(line)
		if !ok {
			return nil, errf("E330", "%s:%d: unexpected retry_budget line %q", path, lineNo, line)
		}
		switch key {
		case "retry_percent":
			v, err := strconv.Atoi(strings.TrimSpace(rest))
			if err != nil {
				return nil, errf("E330", "%s:%d: invalid retry_percent", path, lineNo)
			}
			rb.RetryPercent = v
		case "min_tokens":
			v, err := strconv.Atoi(strings.TrimSpace(rest))
			if err != nil {
				return nil, errf("E330", "%s:%d: invalid min_tokens", path, lineNo)
			}
			rb.MinTokens = v
		default:
			return nil, errf("E330", "%s:%d: unknown retry_budget field %q", path, lineNo, key)
		}
	}
	return rb, nil
}

// parseRoute parses the body of a route block.
func parseRoute(path string, startLine int, name string, body []string) (Route, error) {
	rr := Route{Name: name, SourceFile: path, Line: startLine}
	for i, raw := range body {
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		lineNo := startLine + i + 1
		key, rest, ok := cutDirective(line)
		if !ok {
			return Route{}, errf("E330", "%s:%d: unexpected route line %q", path, lineNo, line)
		}
		switch key {
		case "host":
			rr.Host = trimQuoted(rest)
		case "path":
			rr.Path = trimQuoted(rest)
		case "upstream":
			rr.Upstream = trimQuoted(rest)
		case "upstreams":
			// "pool_a" weight 95, "pool_b" weight 5
			parts := splitArgs(rest)
			for _, part := range parts {
				wu, err := parseWeightedUpstream(strings.TrimSpace(part))
				if err != nil {
					return Route{}, errf("E330", "%s:%d: %v", path, lineNo, err)
				}
				rr.Upstreams = append(rr.Upstreams, wu)
			}
		case "apply":
			for _, a := range splitArgs(rest) {
				rr.Apply = append(rr.Apply, trimQuoted(a))
			}
		default:
			return Route{}, errf("E330", "%s:%d: unknown route field %q", path, lineNo, key)
		}
	}
	return rr, nil
}

func parseWeightedUpstream(s string) (router.WeightedUpstream, error) {
	// Expect: "name" weight N  (weight keyword optional; if missing, weight=1).
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return router.WeightedUpstream{}, fmt.Errorf("empty weighted upstream")
	}
	name := trimQuoted(fields[0])
	weight := 1
	if len(fields) >= 3 && fields[1] == "weight" {
		v, err := strconv.Atoi(fields[2])
		if err != nil {
			return router.WeightedUpstream{}, fmt.Errorf("invalid weight %q", fields[2])
		}
		weight = v
	}
	return router.WeightedUpstream{Name: name, Weight: weight}, nil
}

// parseListener parses the body of a listener block.
func parseListener(path string, startLine int, body []string) (Listener, error) {
	l := Listener{}
	for i, raw := range body {
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		lineNo := startLine + i + 1
		key, rest, ok := cutDirective(line)
		if !ok {
			return Listener{}, errf("E330", "%s:%d: unexpected listener line %q", path, lineNo, line)
		}
		switch key {
		case "addr":
			l.Addr = trimQuoted(rest)
		default:
			return Listener{}, errf("E330", "%s:%d: unknown listener field %q", path, lineNo, key)
		}
	}
	return l, nil
}

// parseTLS parses the body of a tls block.
func parseTLS(path string, startLine int, body []string) (TLSConfig, error) {
	tc := TLSConfig{}
	for i, raw := range body {
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		lineNo := startLine + i + 1
		key, rest, ok := cutDirective(line)
		if !ok {
			return TLSConfig{}, errf("E330", "%s:%d: unexpected tls line %q", path, lineNo, line)
		}
		switch key {
		case "listen":
			tc.Addr = trimQuoted(rest)
		case "cert":
			tc.Cert = trimQuoted(rest)
		case "key":
			tc.Key = trimQuoted(rest)
		default:
			return TLSConfig{}, errf("E330", "%s:%d: unknown tls field %q", path, lineNo, key)
		}
	}
	return tc, nil
}

// parseQUIC parses the body of a quic block.
func parseQUIC(path string, startLine int, body []string) (QUICConfig, error) {
	qc := QUICConfig{}
	for i, raw := range body {
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		lineNo := startLine + i + 1
		key, rest, ok := cutDirective(line)
		if !ok {
			return QUICConfig{}, errf("E330", "%s:%d: unexpected quic line %q", path, lineNo, line)
		}
		switch key {
		case "listen":
			qc.Addr = trimQuoted(rest)
		case "cert":
			qc.Cert = trimQuoted(rest)
		case "key":
			qc.Key = trimQuoted(rest)
		case "alpn":
			for _, a := range splitArgs(rest) {
				v := trimQuoted(a)
				if v != "" {
					qc.ALPN = append(qc.ALPN, v)
				}
			}
		default:
			return QUICConfig{}, errf("E330", "%s:%d: unknown quic field %q", path, lineNo, key)
		}
	}
	return qc, nil
}

// parseTopoCase parses the body of a top-level `case NAME { ... }` block.
func parseTopoCase(path string, startLine int, name string, body []string) (TopoCase, error) {
	tc := TopoCase{
		Name:       name,
		SourceFile: path,
		Line:       startLine,
		Request: CaseRequest{
			Method: "GET",
			Host:   "example.com",
			Path:   "/",
		},
	}
	for i, raw := range body {
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		lineNo := startLine + i + 1
		key, rest, ok := cutDirective(line)
		if !ok {
			return TopoCase{}, errf("E023", "%s:%d: unexpected case line %q", path, lineNo, line)
		}
		args := splitArgs(rest)
		// Allow existing request.* style first, then topology expect.*
		switch key {
		case "request.method":
			tc.Request.Method = trimQuoted(rest)
		case "request.host":
			tc.Request.Host = trimQuoted(rest)
		case "request.path":
			tc.Request.Path = trimQuoted(rest)
		case "request.client_ip":
			tc.Request.ClientIP = trimQuoted(rest)
		case "request.header":
			if len(args) != 2 {
				return TopoCase{}, errf("E023", "%s:%d: request.header requires name and value", path, lineNo)
			}
			if tc.Request.Headers == nil {
				tc.Request.Headers = map[string]string{}
			}
			tc.Request.Headers[strings.ToLower(trimQuoted(args[0]))] = trimQuoted(args[1])
		case "request.query":
			if len(args) != 2 {
				return TopoCase{}, errf("E023", "%s:%d: request.query requires name and value", path, lineNo)
			}
			if tc.Request.Query == nil {
				tc.Request.Query = map[string]string{}
			}
			tc.Request.Query[trimQuoted(args[0])] = trimQuoted(args[1])
		case "request.cookie":
			if len(args) != 2 {
				return TopoCase{}, errf("E023", "%s:%d: request.cookie requires name and value", path, lineNo)
			}
			if tc.Request.Cookies == nil {
				tc.Request.Cookies = map[string]string{}
			}
			tc.Request.Cookies[trimQuoted(args[0])] = trimQuoted(args[1])
		case "expect.route":
			tc.Expect.Route = trimQuoted(rest)
		case "expect.upstream":
			tc.Expect.Upstream = trimQuoted(rest)
		case "expect.applied":
			for _, a := range args {
				tc.Expect.Applied = append(tc.Expect.Applied, trimQuoted(a))
			}
		case "expect.status":
			v, err := strconv.Atoi(strings.TrimSpace(rest))
			if err != nil {
				return TopoCase{}, errf("E023", "%s:%d: invalid expect.status", path, lineNo)
			}
			tc.Expect.Status = &v
		default:
			return TopoCase{}, errf("E023", "%s:%d: unsupported case directive %q", path, lineNo, key)
		}
	}
	return tc, nil
}

// cutDirective splits a line like `key rest...` returning key and the rest.
// Returns ok=false on lines that don't look like directives (no space and
// no value after the key).
func cutDirective(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", "", false
	}
	idx := strings.IndexAny(line, " \t")
	if idx < 0 {
		// Bare key. Treat as valid for directives like `deny` with no args:
		// not applicable to topology lines — return ok=false.
		return line, "", true
	}
	return line[:idx], strings.TrimSpace(line[idx:]), true
}

func parseCaseLine(pc *PolicyCase, line string) error {
	key, raw, ok := strings.Cut(line, " ")
	if !ok {
		return fmt.Errorf("invalid case directive %q", line)
	}
	args := splitArgs(strings.TrimSpace(raw))
	switch key {
	case "request.method":
		if len(args) != 1 {
			return fmt.Errorf("request.method requires one value")
		}
		pc.Request.Method = trimQuoted(args[0])
	case "request.host":
		if len(args) != 1 {
			return fmt.Errorf("request.host requires one value")
		}
		pc.Request.Host = trimQuoted(args[0])
	case "request.path":
		if len(args) != 1 {
			return fmt.Errorf("request.path requires one value")
		}
		pc.Request.Path = trimQuoted(args[0])
	case "request.client_ip":
		if len(args) != 1 {
			return fmt.Errorf("request.client_ip requires one value")
		}
		pc.Request.ClientIP = trimQuoted(args[0])
	case "request.header":
		if len(args) != 2 {
			return fmt.Errorf("request.header requires name and value")
		}
		if pc.Request.Headers == nil {
			pc.Request.Headers = map[string]string{}
		}
		pc.Request.Headers[strings.ToLower(trimQuoted(args[0]))] = trimQuoted(args[1])
	case "request.query":
		if len(args) != 2 {
			return fmt.Errorf("request.query requires name and value")
		}
		if pc.Request.Query == nil {
			pc.Request.Query = map[string]string{}
		}
		pc.Request.Query[trimQuoted(args[0])] = trimQuoted(args[1])
	case "request.cookie":
		if len(args) != 2 {
			return fmt.Errorf("request.cookie requires name and value")
		}
		if pc.Request.Cookies == nil {
			pc.Request.Cookies = map[string]string{}
		}
		pc.Request.Cookies[trimQuoted(args[0])] = trimQuoted(args[1])
	case "expect.status":
		if len(args) != 1 {
			return fmt.Errorf("expect.status requires one value")
		}
		status, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("expect.status: %w", err)
		}
		pc.Expect.Status = &status
	case "expect.body":
		if len(args) != 1 {
			return fmt.Errorf("expect.body requires one value")
		}
		body := trimQuoted(args[0])
		pc.Expect.Body = &body
	case "expect.upstream":
		if len(args) != 1 {
			return fmt.Errorf("expect.upstream requires one value")
		}
		pc.Expect.Upstream = trimQuoted(args[0])
	case "expect.path":
		if len(args) != 1 {
			return fmt.Errorf("expect.path requires one value")
		}
		pc.Expect.Path = trimQuoted(args[0])
	case "expect.request_header":
		if len(args) != 2 {
			return fmt.Errorf("expect.request_header requires name and value")
		}
		if pc.Expect.RequestHeaders == nil {
			pc.Expect.RequestHeaders = map[string]string{}
		}
		pc.Expect.RequestHeaders[strings.ToLower(trimQuoted(args[0]))] = trimQuoted(args[1])
	case "expect.response_header":
		if len(args) != 2 {
			return fmt.Errorf("expect.response_header requires name and value")
		}
		if pc.Expect.ResponseHeaders == nil {
			pc.Expect.ResponseHeaders = map[string]string{}
		}
		pc.Expect.ResponseHeaders[strings.ToLower(trimQuoted(args[0]))] = trimQuoted(args[1])
	default:
		return fmt.Errorf("unsupported case directive %q", key)
	}
	return nil
}

func parseBudgetLine(pb *PolicyBudget, line string) error {
	parts := strings.Fields(line)
	if len(parts) != 3 || parts[1] != "<=" {
		return fmt.Errorf("invalid budget line %q", line)
	}
	value, err := strconv.ParseFloat(parts[2], 64)
	if err != nil {
		return fmt.Errorf("invalid budget value %q: %w", parts[2], err)
	}
	switch parts[0] {
	case "request.allocs":
		pb.RequestAllocs = &value
	case "response.allocs":
		pb.ResponseAllocs = &value
	default:
		return fmt.Errorf("unsupported budget metric %q", parts[0])
	}
	return nil
}

func parseMatch(expr string) ([]rt.MatchCondition, error) {
	parts := strings.Split(expr, "&&")
	var conds []rt.MatchCondition
	for _, raw := range parts {
		part := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(part, "req.host == "):
			conds = append(conds, rt.MatchCondition{Field: rt.MatchHost, Value: trimQuoted(strings.TrimSpace(strings.TrimPrefix(part, "req.host == ")))})
		case strings.HasPrefix(part, "req.method == "):
			conds = append(conds, rt.MatchCondition{Field: rt.MatchMethod, Value: trimQuoted(strings.TrimSpace(strings.TrimPrefix(part, "req.method == ")))})
		case strings.HasPrefix(part, "req.ip == "):
			conds = append(conds, rt.MatchCondition{Field: rt.MatchIP, Value: trimQuoted(strings.TrimSpace(strings.TrimPrefix(part, "req.ip == ")))})
		case strings.HasPrefix(part, "req.path.has_prefix(") && strings.HasSuffix(part, ")"):
			conds = append(conds, rt.MatchCondition{
				Field: rt.MatchPathPrefix,
				Value: trimQuoted(strings.TrimSuffix(strings.TrimPrefix(part, "req.path.has_prefix("), ")")),
			})
		case strings.HasPrefix(part, "req.path.has_suffix(") && strings.HasSuffix(part, ")"):
			conds = append(conds, rt.MatchCondition{
				Field: rt.MatchPathSuffix,
				Value: trimQuoted(strings.TrimSuffix(strings.TrimPrefix(part, "req.path.has_suffix("), ")")),
			})
		case strings.HasPrefix(part, "req.path == "):
			conds = append(conds, rt.MatchCondition{
				Field: rt.MatchPath,
				Value: trimQuoted(strings.TrimSpace(strings.TrimPrefix(part, "req.path == "))),
			})
		case strings.HasPrefix(part, "req.header("):
			end := strings.Index(part, ")")
			if end < 0 {
				return nil, fmt.Errorf("invalid header predicate %q", part)
			}
			name := trimQuoted(part[len("req.header("):end])
			rest := strings.TrimSpace(part[end+1:])
			if !strings.HasPrefix(rest, "==") {
				return nil, fmt.Errorf("invalid header predicate %q", part)
			}
			value := trimQuoted(strings.TrimSpace(strings.TrimPrefix(rest, "==")))
			conds = append(conds, rt.MatchCondition{Field: rt.MatchHeader, Name: strings.ToLower(name), Value: value})
		case strings.HasPrefix(part, "req.query("):
			end := strings.Index(part, ")")
			if end < 0 {
				return nil, fmt.Errorf("invalid query predicate %q", part)
			}
			name := trimQuoted(part[len("req.query("):end])
			rest := strings.TrimSpace(part[end+1:])
			if !strings.HasPrefix(rest, "==") {
				return nil, fmt.Errorf("invalid query predicate %q", part)
			}
			value := trimQuoted(strings.TrimSpace(strings.TrimPrefix(rest, "==")))
			conds = append(conds, rt.MatchCondition{Field: rt.MatchQuery, Name: name, Value: value})
		case strings.HasPrefix(part, "req.cookie("):
			end := strings.Index(part, ")")
			if end < 0 {
				return nil, fmt.Errorf("invalid cookie predicate %q", part)
			}
			name := trimQuoted(part[len("req.cookie("):end])
			rest := strings.TrimSpace(part[end+1:])
			if !strings.HasPrefix(rest, "==") {
				return nil, fmt.Errorf("invalid cookie predicate %q", part)
			}
			value := trimQuoted(strings.TrimSpace(strings.TrimPrefix(rest, "==")))
			conds = append(conds, rt.MatchCondition{Field: rt.MatchCookie, Name: name, Value: value})
		default:
			return nil, fmt.Errorf("unsupported match expression %q", part)
		}
	}
	return conds, nil
}

func parseAction(line string) (rt.Action, string, error) {
	open := strings.IndexByte(line, '(')
	close := strings.LastIndexByte(line, ')')
	if open <= 0 || close < open {
		return rt.Action{}, "", fmt.Errorf("invalid action %q", line)
	}
	name := strings.TrimSpace(line[:open])
	args := splitArgs(line[open+1 : close])
	switch rt.ActionKind(name) {
	case rt.ActionRespond:
		if len(args) < 1 {
			return rt.Action{}, "", fmt.Errorf("respond requires status")
		}
		status, err := strconv.Atoi(args[0])
		if err != nil {
			return rt.Action{}, "", fmt.Errorf("respond status: %w", err)
		}
		body := ""
		if len(args) > 1 {
			body = trimQuoted(args[1])
		}
		return rt.Action{Kind: rt.ActionRespond, Int1: status, Str1: body}, name, nil
	case rt.ActionDeny:
		status := 403
		if len(args) > 0 && args[0] != "" {
			v, err := strconv.Atoi(args[0])
			if err != nil {
				return rt.Action{}, "", fmt.Errorf("deny status: %w", err)
			}
			status = v
		}
		return rt.Action{Kind: rt.ActionDeny, Int1: status}, name, nil
	case rt.ActionRedirect:
		if len(args) != 2 {
			return rt.Action{}, "", fmt.Errorf("redirect requires code and url")
		}
		code, err := strconv.Atoi(args[0])
		if err != nil {
			return rt.Action{}, "", fmt.Errorf("redirect code: %w", err)
		}
		return rt.Action{Kind: rt.ActionRedirect, Int1: code, Str1: trimQuoted(args[1])}, name, nil
	case rt.ActionRouteTo:
		return rt.Action{Kind: rt.ActionRouteTo, Str1: trimQuoted(args[0])}, name, nil
	case rt.ActionCanary:
		if len(args) != 2 {
			return rt.Action{}, "", fmt.Errorf("canary requires percent and pool")
		}
		pct, err := strconv.Atoi(args[0])
		if err != nil {
			return rt.Action{}, "", fmt.Errorf("canary percent: %w", err)
		}
		return rt.Action{Kind: rt.ActionCanary, Int1: pct, Str1: trimQuoted(args[1])}, name, nil
	case rt.ActionSetHeader:
		if len(args) != 2 {
			return rt.Action{}, "", fmt.Errorf("set_header requires name and value")
		}
		return rt.Action{Kind: rt.ActionSetHeader, Str1: trimQuoted(args[0]), Str2: trimQuoted(args[1])}, name, nil
	case rt.ActionRemoveHeader:
		if len(args) != 1 {
			return rt.Action{}, "", fmt.Errorf("remove_header requires name")
		}
		return rt.Action{Kind: rt.ActionRemoveHeader, Str1: trimQuoted(args[0])}, name, nil
	case rt.ActionStripPrefix:
		return rt.Action{Kind: rt.ActionStripPrefix, Str1: trimQuoted(args[0])}, name, nil
	case rt.ActionAddPrefix:
		return rt.Action{Kind: rt.ActionAddPrefix, Str1: trimQuoted(args[0])}, name, nil
	case rt.ActionRateLimitLocal:
		if len(args) != 3 {
			return rt.Action{}, "", fmt.Errorf("rate_limit_local requires key, rps, burst")
		}
		rps, err := strconv.Atoi(args[1])
		if err != nil {
			return rt.Action{}, "", fmt.Errorf("rate_limit_local rps: %w", err)
		}
		burst, err := strconv.Atoi(args[2])
		if err != nil {
			return rt.Action{}, "", fmt.Errorf("rate_limit_local burst: %w", err)
		}
		return rt.Action{Kind: rt.ActionRateLimitLocal, Str1: trimQuoted(args[0]), Int1: rps, Int2: burst}, name, nil
	case rt.ActionAuthExternal:
		if len(args) < 1 {
			return rt.Action{}, "", fmt.Errorf("auth_external requires url")
		}
		status := 403
		if len(args) > 1 {
			v, err := strconv.Atoi(args[1])
			if err != nil {
				return rt.Action{}, "", fmt.Errorf("auth_external deny status: %w", err)
			}
			status = v
		}
		return rt.Action{Kind: rt.ActionAuthExternal, Str1: trimQuoted(args[0]), Int1: status}, name, nil
	case rt.ActionLog:
		value := ""
		if len(args) > 0 {
			value = trimQuoted(args[0])
		}
		return rt.Action{Kind: rt.ActionLog, Str1: value}, name, nil
	case rt.ActionEmitMetric:
		value := ""
		if len(args) > 0 {
			value = trimQuoted(args[0])
		}
		return rt.Action{Kind: rt.ActionEmitMetric, Str1: value}, name, nil
	default:
		return rt.Action{}, "", fmt.Errorf("unsupported action %q", name)
	}
}

func splitArgs(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var (
		args    []string
		cur     strings.Builder
		inQuote bool
	)
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch ch {
		case '"':
			inQuote = !inQuote
			cur.WriteByte(ch)
		case ',':
			if inQuote {
				cur.WriteByte(ch)
				continue
			}
			args = append(args, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	args = append(args, strings.TrimSpace(cur.String()))
	return args
}

func trimQuoted(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func stripComment(s string) string {
	if idx := strings.IndexByte(s, '#'); idx >= 0 {
		return s[:idx]
	}
	return s
}

func appendUnique(dst []string, v string) []string {
	for _, cur := range dst {
		if cur == v {
			return dst
		}
	}
	return append(dst, v)
}

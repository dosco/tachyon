package intent

import (
	"bufio"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	rt "tachyon/internal/intent/runtime"
)

var (
	reVersion = regexp.MustCompile(`^intent_version\s+"([^"]+)"$`)
	rePolicy  = regexp.MustCompile(`^policy\s+([A-Za-z_][A-Za-z0-9_]*)\s*\{$`)
	reCase    = regexp.MustCompile(`^case\s+([A-Za-z_][A-Za-z0-9_]*)\s*\{$`)
)

func parseSource(path, src string) ([]Policy, string, error) {
	var (
		policies  []Policy
		version   string
		cur       *Policy
		curCase   *PolicyCase
		curBudget *PolicyBudget
		section   string
		depth     int
	)
	sc := bufio.NewScanner(strings.NewReader(src))
	for lineNo := 1; sc.Scan(); lineNo++ {
		line := strings.TrimSpace(stripComment(sc.Text()))
		if line == "" {
			continue
		}
		if cur == nil {
			if m := reVersion.FindStringSubmatch(line); m != nil {
				version = m[1]
				continue
			}
			if m := rePolicy.FindStringSubmatch(line); m != nil {
				p := Policy{Name: m[1], Priority: 100, SourceFile: path}
				cur = &p
				depth = 1
				continue
			}
			return nil, "", errf("E001", "%s:%d: unexpected top-level line %q", path, lineNo, line)
		}

		switch {
		case line == "}":
			depth--
			if depth == 1 {
				if section == "case" && curCase != nil {
					cur.Cases = append(cur.Cases, *curCase)
					curCase = nil
				}
				if section == "budget" && curBudget != nil {
					cur.Budget = *curBudget
					curBudget = nil
				}
				section = ""
			}
			if depth == 0 {
				policies = append(policies, *cur)
				cur = nil
			}
			continue
		case strings.HasSuffix(line, "{"):
			switch strings.TrimSpace(strings.TrimSuffix(line, "{")) {
			case "request", "response", "error":
				section = strings.TrimSpace(strings.TrimSuffix(line, "{"))
			case "budget":
				section = "budget"
				curBudget = &PolicyBudget{}
			default:
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
				}
			}
			depth++
			continue
		}

		if depth == 1 {
			if strings.HasPrefix(line, "priority ") {
				v, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "priority ")))
				if err != nil {
					return nil, "", errf("E011", "%s:%d: invalid priority", path, lineNo)
				}
				cur.Priority = v
				continue
			}
			if strings.HasPrefix(line, "match ") {
				conds, err := parseMatch(strings.TrimSpace(strings.TrimPrefix(line, "match ")))
				if err != nil {
					return nil, "", errf("E012", "%s:%d: %v", path, lineNo, err)
				}
				cur.Match = conds
				continue
			}
			return nil, "", errf("E013", "%s:%d: unexpected policy line %q", path, lineNo, line)
		}

		switch section {
		case "request", "response", "error":
			action, primitive, err := parseAction(line)
			if err != nil {
				return nil, "", errf("E020", "%s:%d: %v", path, lineNo, err)
			}
			cur.Primitives = appendUnique(cur.Primitives, primitive)
			if primitive == string(rt.ActionAuthExternal) {
				cur.RequiresClassC = true
			}
			switch section {
			case "request":
				cur.Request = append(cur.Request, action)
			case "response":
				cur.Response = append(cur.Response, action)
			case "error":
				cur.Error = append(cur.Error, action)
			}
		case "budget":
			if curBudget == nil {
				return nil, "", errf("E021", "%s:%d: unexpected budget line %q", path, lineNo, line)
			}
			if err := parseBudgetLine(curBudget, line); err != nil {
				return nil, "", errf("E022", "%s:%d: %v", path, lineNo, err)
			}
		case "case":
			if curCase == nil {
				return nil, "", errf("E021", "%s:%d: unexpected case line %q", path, lineNo, line)
			}
			if err := parseCaseLine(curCase, line); err != nil {
				return nil, "", errf("E023", "%s:%d: %v", path, lineNo, err)
			}
		default:
			return nil, "", errf("E021", "%s:%d: unexpected nested line %q", path, lineNo, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, "", err
	}
	if cur != nil {
		return nil, "", errf("E002", "%s: unterminated policy block", path)
	}
	return policies, version, nil
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

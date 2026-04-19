package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"tachyon/internal/router"
)

type ActionKind string

const (
	ActionRespond        ActionKind = "respond"
	ActionDeny           ActionKind = "deny"
	ActionRedirect       ActionKind = "redirect"
	ActionRouteTo        ActionKind = "route_to"
	ActionCanary         ActionKind = "canary"
	ActionSetHeader      ActionKind = "set_header"
	ActionRemoveHeader   ActionKind = "remove_header"
	ActionStripPrefix    ActionKind = "strip_prefix"
	ActionAddPrefix      ActionKind = "add_prefix"
	ActionRateLimitLocal ActionKind = "rate_limit_local"
	ActionAuthExternal   ActionKind = "auth_external"
	ActionLog            ActionKind = "log"
	ActionEmitMetric     ActionKind = "emit_metric"
)

type MatchField string

const (
	MatchHost       MatchField = "host"
	MatchPathPrefix MatchField = "path_prefix"
	MatchPath       MatchField = "path"
	MatchPathSuffix MatchField = "path_suffix"
	MatchMethod     MatchField = "method"
	MatchHeader     MatchField = "header"
	MatchIP         MatchField = "ip"
	MatchQuery      MatchField = "query"
	MatchCookie     MatchField = "cookie"
)

type MatchCondition struct {
	Field MatchField
	Name  string
	Value string
}

type Action struct {
	Kind ActionKind
	Str1 string
	Str2 string
	Int1 int
	Int2 int
}

// PolicyMeta is the generated registry's runtime-facing compiled policy
// description. The current implementation executes these generated
// structs through a shared runtime engine; future phases can swap in a
// more specialized code path without changing the registry boundary.
type PolicyMeta struct {
	Name           string
	Priority       int
	Match          []MatchCondition
	Request        []Action
	Response       []Action
	Error          []Action
	Primitives     []string
	RequiresClassC bool
}

type Registry struct {
	Version  string
	Policies map[string]PolicyMeta
}

type RoutePolicySet struct {
	RouteID        int
	PolicyNames    []string
	Policies       []PolicyMeta
	RequiresStdlib bool
}

type RoutePrograms struct {
	ByRouteID      map[int]RoutePolicySet
	RequiresStdlib bool
}

type HeaderMutation struct {
	Name   string
	Value  string
	Remove bool
}

type HeaderMutations struct {
	inline [2]HeaderMutation
	n      int
	extra  []HeaderMutation
}

func (h *HeaderMutations) Add(m HeaderMutation) {
	if h.n < len(h.inline) {
		h.inline[h.n] = m
		h.n++
		return
	}
	h.extra = append(h.extra, m)
}

func (h HeaderMutations) Len() int {
	return h.n + len(h.extra)
}

func (h HeaderMutations) At(i int) HeaderMutation {
	if i < h.n {
		return h.inline[i]
	}
	return h.extra[i-h.n]
}

func (h HeaderMutations) Each(fn func(HeaderMutation) bool) {
	for i := 0; i < h.n; i++ {
		if !fn(h.inline[i]) {
			return
		}
	}
	for _, m := range h.extra {
		if !fn(m) {
			return
		}
	}
}

func (h HeaderMutations) Find(name string) (string, bool) {
	for i := 0; i < h.n; i++ {
		if h.inline[i].Remove {
			continue
		}
		if strings.EqualFold(h.inline[i].Name, name) {
			return h.inline[i].Value, true
		}
	}
	for _, m := range h.extra {
		if m.Remove {
			continue
		}
		if strings.EqualFold(m.Name, name) {
			return m.Value, true
		}
	}
	return "", false
}

func (h HeaderMutations) Removed(name string) bool {
	for i := 0; i < h.n; i++ {
		if h.inline[i].Remove && strings.EqualFold(h.inline[i].Name, name) {
			return true
		}
	}
	for _, m := range h.extra {
		if m.Remove && strings.EqualFold(m.Name, name) {
			return true
		}
	}
	return false
}

func (h HeaderMutations) Overridden(name string) bool {
	for i := 0; i < h.n; i++ {
		if !h.inline[i].Remove && strings.EqualFold(h.inline[i].Name, name) {
			return true
		}
	}
	for _, m := range h.extra {
		if !m.Remove && strings.EqualFold(m.Name, name) {
			return true
		}
	}
	return false
}

func (h HeaderMutations) MarshalJSON() ([]byte, error) {
	if h.Len() == 0 {
		return []byte("null"), nil
	}
	out := make([]HeaderMutation, 0, h.Len())
	h.Each(func(m HeaderMutation) bool {
		out = append(out, m)
		return true
	})
	return json.Marshal(out)
}

type TerminalResponse struct {
	Status  int
	Body    string
	Headers HeaderMutations
}

type ActionTrace struct {
	Kind    ActionKind `json:"kind"`
	Details string     `json:"details,omitempty"`
}

type PolicyTrace struct {
	Name    string        `json:"name"`
	Matched bool          `json:"matched"`
	Actions []ActionTrace `json:"actions,omitempty"`
}

type Trace struct {
	RouteID        int           `json:"route_id"`
	Policies       []PolicyTrace `json:"policies"`
	TerminalStatus int           `json:"terminal_status,omitempty"`
}

type RequestResult struct {
	Terminal         TerminalResponse
	HasTerminal      bool
	UpstreamOverride string
	PathOverride     string
	HeaderMutations  HeaderMutations
	Trace            Trace
}

type ResponseResult struct {
	HeaderMutations HeaderMutations
	Trace           Trace
}

func (r RequestResult) MarshalJSON() ([]byte, error) {
	type requestResultJSON struct {
		Terminal         any             `json:"Terminal"`
		UpstreamOverride string          `json:"UpstreamOverride"`
		PathOverride     string          `json:"PathOverride"`
		HeaderMutations  HeaderMutations `json:"HeaderMutations"`
		Trace            Trace           `json:"Trace"`
	}
	payload := requestResultJSON{
		Terminal:         nil,
		UpstreamOverride: r.UpstreamOverride,
		PathOverride:     r.PathOverride,
		HeaderMutations:  r.HeaderMutations,
		Trace:            r.Trace,
	}
	if r.HasTerminal {
		payload.Terminal = r.Terminal
	}
	return json.Marshal(payload)
}

type RequestView interface {
	Method() string
	Path() string
	Host() string
	Header(name string) string
	ClientIP() string
	Query(name string) string
	Cookie(name string) string
}

type StaticRequest struct {
	MethodValue   string            `json:"method"`
	PathValue     string            `json:"path"`
	HostValue     string            `json:"host"`
	HeadersValue  map[string]string `json:"headers,omitempty"`
	ClientIPValue string            `json:"client_ip,omitempty"`
	QueryValue    map[string]string `json:"query,omitempty"`
	CookieValue   map[string]string `json:"cookies,omitempty"`
}

func (s StaticRequest) Method() string   { return s.MethodValue }
func (s StaticRequest) Path() string     { return s.PathValue }
func (s StaticRequest) Host() string     { return s.HostValue }
func (s StaticRequest) ClientIP() string { return s.ClientIPValue }
func (s StaticRequest) Header(name string) string {
	if s.HeadersValue == nil {
		return ""
	}
	if v, ok := s.HeadersValue[name]; ok {
		return v
	}
	for k, v := range s.HeadersValue {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}
func (s StaticRequest) Query(name string) string {
	if s.QueryValue == nil {
		return ""
	}
	if v, ok := s.QueryValue[name]; ok {
		return v
	}
	for k, v := range s.QueryValue {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}
func (s StaticRequest) Cookie(name string) string {
	if s.CookieValue == nil {
		return ""
	}
	if v, ok := s.CookieValue[name]; ok {
		return v
	}
	for k, v := range s.CookieValue {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

type State struct {
	mu      sync.Mutex
	buckets map[string]map[string]*bucket
	client  *http.Client
	now     func() time.Time
	nowTime time.Time
}

type bucket struct {
	windowStart time.Time
	count       int
}

var (
	ErrUnknownPolicy = errors.New("intent: unknown policy")
)

func EmptyRegistry() Registry {
	return Registry{
		Version:  "0.1",
		Policies: map[string]PolicyMeta{},
	}
}

func NewState() *State {
	return &State{
		buckets: map[string]map[string]*bucket{},
		client: &http.Client{
			Timeout: 200 * time.Millisecond,
		},
		now: func() time.Time {
			return time.Now().UTC()
		},
	}
}

func (s *State) SetNowFunc(fn func() time.Time) {
	if fn == nil {
		s.now = func() time.Time {
			return time.Now().UTC()
		}
		s.nowTime = time.Time{}
		return
	}
	s.nowTime = time.Time{}
	s.now = fn
}

func (s *State) SetHTTPClient(client *http.Client) {
	if client == nil {
		s.client = &http.Client{Timeout: 200 * time.Millisecond}
		return
	}
	s.client = client
}

func (s *State) SetNowTime(t time.Time) {
	s.nowTime = t
}

func EmptyRoutePrograms() RoutePrograms {
	return RoutePrograms{ByRouteID: map[int]RoutePolicySet{}}
}

func BindRoutes(routes []router.Rule, reg Registry) (RoutePrograms, error) {
	out := RoutePrograms{ByRouteID: make(map[int]RoutePolicySet, len(routes))}
	for _, route := range routes {
		set := RoutePolicySet{
			RouteID:     route.RouteID,
			PolicyNames: slices.Clone(route.Intents),
		}
		for _, name := range route.Intents {
			meta, ok := reg.Policies[name]
			if !ok {
				return RoutePrograms{}, fmt.Errorf("%w %q on route %d", ErrUnknownPolicy, name, route.RouteID)
			}
			set.Policies = append(set.Policies, meta)
			if meta.RequiresClassC {
				set.RequiresStdlib = true
				out.RequiresStdlib = true
			}
		}
		slices.SortFunc(set.Policies, func(a, b PolicyMeta) int {
			switch {
			case a.Priority > b.Priority:
				return -1
			case a.Priority < b.Priority:
				return 1
			default:
				return strings.Compare(a.Name, b.Name)
			}
		})
		out.ByRouteID[route.RouteID] = set
	}
	return out, nil
}

func ExecuteRequest(set RoutePolicySet, state *State, req RequestView, upstream string) RequestResult {
	return executeRequest(set, state, req, upstream, false)
}

func ExecuteRequestTraced(set RoutePolicySet, state *State, req RequestView, upstream string) RequestResult {
	return executeRequest(set, state, req, upstream, true)
}

func executeRequest(set RoutePolicySet, state *State, req RequestView, upstream string, traced bool) RequestResult {
	var res RequestResult
	if traced {
		res.Trace.RouteID = set.RouteID
	}
	for _, policy := range set.Policies {
		var pt *PolicyTrace
		if traced {
			pt = &PolicyTrace{Name: policy.Name}
		}
		if !matchesAll(policy.Match, req) {
			appendPolicyTrace(&res.Trace, pt)
			continue
		}
		if pt != nil {
			pt.Matched = true
		}
		for _, action := range policy.Request {
			if stop := applyRequestAction(&res, &pt, action, state, req, upstream, policy.Name); stop {
				if traced && res.HasTerminal {
					res.Trace.TerminalStatus = res.Terminal.Status
				}
				break
			}
		}
		appendPolicyTrace(&res.Trace, pt)
		if res.HasTerminal {
			break
		}
	}
	return res
}

func ExecuteResponse(set RoutePolicySet, respHeaders func(string) string) ResponseResult {
	return executeResponse(set, respHeaders, false)
}

func ExecuteResponseTraced(set RoutePolicySet, respHeaders func(string) string) ResponseResult {
	return executeResponse(set, respHeaders, true)
}

func executeResponse(set RoutePolicySet, respHeaders func(string) string, traced bool) ResponseResult {
	_ = respHeaders
	var res ResponseResult
	if traced {
		res.Trace.RouteID = set.RouteID
	}
	for _, policy := range set.Policies {
		var pt *PolicyTrace
		if traced {
			pt = &PolicyTrace{Name: policy.Name, Matched: true}
		}
		for _, action := range policy.Response {
			switch action.Kind {
			case ActionSetHeader:
				res.HeaderMutations.Add(HeaderMutation{Name: action.Str1, Value: action.Str2})
				appendActionTrace(pt, action.Kind, action.Str1)
			case ActionRemoveHeader:
				res.HeaderMutations.Add(HeaderMutation{Name: action.Str1, Remove: true})
				appendActionTrace(pt, action.Kind, action.Str1)
			case ActionLog, ActionEmitMetric:
				appendActionTrace(pt, action.Kind, action.Str1)
			}
		}
		appendPolicyTrace(&res.Trace, pt)
	}
	return res
}

func appendPolicyTrace(trace *Trace, pt *PolicyTrace) {
	if pt == nil {
		return
	}
	trace.Policies = append(trace.Policies, *pt)
}

func matchesAll(conds []MatchCondition, req RequestView) bool {
	for _, cond := range conds {
		switch cond.Field {
		case MatchHost:
			if req.Host() != cond.Value {
				return false
			}
		case MatchPathPrefix:
			if !strings.HasPrefix(req.Path(), cond.Value) {
				return false
			}
		case MatchPath:
			if req.Path() != cond.Value {
				return false
			}
		case MatchPathSuffix:
			if !strings.HasSuffix(req.Path(), cond.Value) {
				return false
			}
		case MatchMethod:
			if req.Method() != cond.Value {
				return false
			}
		case MatchHeader:
			if req.Header(cond.Name) != cond.Value {
				return false
			}
		case MatchIP:
			if req.ClientIP() != cond.Value {
				return false
			}
		case MatchQuery:
			if req.Query(cond.Name) != cond.Value {
				return false
			}
		case MatchCookie:
			if req.Cookie(cond.Name) != cond.Value {
				return false
			}
		}
	}
	return true
}

func applyRequestAction(res *RequestResult, pt **PolicyTrace, action Action, state *State, req RequestView, upstream, policyName string) bool {
	switch action.Kind {
	case ActionRespond:
		res.HasTerminal = true
		res.Terminal = TerminalResponse{Status: action.Int1, Body: action.Str1}
		appendActionTrace(*pt, action.Kind, strconv.Itoa(action.Int1))
		return true
	case ActionDeny:
		res.HasTerminal = true
		res.Terminal = TerminalResponse{Status: action.Int1}
		appendActionTrace(*pt, action.Kind, strconv.Itoa(action.Int1))
		return true
	case ActionRedirect:
		res.HasTerminal = true
		res.Terminal = TerminalResponse{
			Status: action.Int1,
		}
		res.Terminal.Headers.Add(HeaderMutation{Name: "Location", Value: action.Str1})
		appendActionTrace(*pt, action.Kind, action.Str1)
		return true
	case ActionRouteTo:
		res.UpstreamOverride = action.Str1
		appendActionTrace(*pt, action.Kind, action.Str1)
	case ActionCanary:
		if rand.IntN(100) < action.Int1 {
			res.UpstreamOverride = action.Str1
			appendActionTrace(*pt, action.Kind, action.Str1)
		}
	case ActionSetHeader:
		res.HeaderMutations.Add(HeaderMutation{Name: action.Str1, Value: action.Str2})
		appendActionTrace(*pt, action.Kind, action.Str1)
	case ActionRemoveHeader:
		res.HeaderMutations.Add(HeaderMutation{Name: action.Str1, Remove: true})
		appendActionTrace(*pt, action.Kind, action.Str1)
	case ActionStripPrefix:
		path := req.Path()
		if strings.HasPrefix(path, action.Str1) {
			res.PathOverride = strings.TrimPrefix(path, action.Str1)
			if res.PathOverride == "" {
				res.PathOverride = "/"
			}
			appendActionTrace(*pt, action.Kind, action.Str1)
		}
	case ActionAddPrefix:
		res.PathOverride = action.Str1 + req.Path()
		appendActionTrace(*pt, action.Kind, action.Str1)
	case ActionRateLimitLocal:
		if state != nil && !state.allow(policyName, action.Str1, req, action.Int1) {
			res.HasTerminal = true
			res.Terminal = TerminalResponse{
				Status: 429,
			}
			res.Terminal.Headers.Add(HeaderMutation{Name: "Retry-After", Value: "1"})
			appendActionTrace(*pt, action.Kind, action.Str1)
			return true
		}
		appendActionTrace(*pt, action.Kind, action.Str1)
	case ActionAuthExternal:
		if state != nil && !state.authExternal(action.Str1, req) {
			status := action.Int1
			if status == 0 {
				status = 403
			}
			res.HasTerminal = true
			res.Terminal = TerminalResponse{Status: status}
			appendActionTrace(*pt, action.Kind, action.Str1)
			return true
		}
		appendActionTrace(*pt, action.Kind, action.Str1)
	case ActionLog, ActionEmitMetric:
		appendActionTrace(*pt, action.Kind, action.Str1)
	}
	_ = upstream
	return false
}

func appendActionTrace(pt *PolicyTrace, kind ActionKind, details string) {
	if pt == nil {
		return
	}
	pt.Actions = append(pt.Actions, ActionTrace{Kind: kind, Details: details})
}

func (s *State) allow(policyName, keySpec string, req RequestView, limit int) bool {
	if limit <= 0 {
		return true
	}
	now := time.Now().UTC()
	if !s.nowTime.IsZero() {
		now = s.nowTime
	} else if s.now != nil {
		now = s.now()
	}
	key := keyFor(keySpec, req)
	s.mu.Lock()
	defer s.mu.Unlock()
	policyBuckets := s.buckets[policyName]
	if policyBuckets == nil {
		policyBuckets = map[string]*bucket{}
		s.buckets[policyName] = policyBuckets
	}
	b := policyBuckets[key]
	if b == nil {
		policyBuckets[key] = &bucket{windowStart: now, count: 1}
		return true
	}
	if now.Sub(b.windowStart) >= time.Second {
		b.windowStart = now
		b.count = 1
		return true
	}
	if b.count >= limit {
		return false
	}
	b.count++
	return true
}

func keyFor(keySpec string, req RequestView) string {
	switch {
	case keySpec == "" || keySpec == "ip":
		return req.ClientIP()
	case strings.HasPrefix(keySpec, "header:"):
		return req.Header(strings.TrimPrefix(keySpec, "header:"))
	default:
		return keySpec
	}
}

func (s *State) authExternal(url string, req RequestView) bool {
	httpReq, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	httpReq.Header.Set("X-Tachyon-Method", req.Method())
	httpReq.Header.Set("X-Tachyon-Path", req.Path())
	httpReq.Header.Set("X-Tachyon-Host", req.Host())
	if req.ClientIP() != "" {
		httpReq.Header.Set("X-Tachyon-Client-IP", req.ClientIP())
	}
	resp, err := s.client.Do(httpReq)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

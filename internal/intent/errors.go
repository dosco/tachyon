package intent

import (
	"fmt"
	"strings"
)

type ErrorCode struct {
	Code    string
	Stage   string
	Summary string
}

var ErrorCatalog = []ErrorCode{
	{Code: "E001", Stage: "parse", Summary: "unexpected top-level line in policy file"},
	{Code: "E002", Stage: "parse", Summary: "unterminated policy block"},
	{Code: "E011", Stage: "parse", Summary: "invalid priority value"},
	{Code: "E012", Stage: "parse", Summary: "invalid match expression"},
	{Code: "E013", Stage: "parse", Summary: "unexpected line inside policy block"},
	{Code: "E020", Stage: "parse", Summary: "invalid action syntax"},
	{Code: "E021", Stage: "parse", Summary: "unexpected line inside nested block"},
	{Code: "E022", Stage: "parse", Summary: "invalid budget line"},
	{Code: "E023", Stage: "parse", Summary: "invalid case line"},
	{Code: "E102", Stage: "bundle", Summary: "duplicate policy name across files"},
	{Code: "E200", Stage: "sema", Summary: "action used in wrong phase"},
	{Code: "E201", Stage: "sema", Summary: "multiple terminal actions in request block"},
	{Code: "E202", Stage: "sema", Summary: "contradictory match conditions"},
	{Code: "E300", Stage: "sema", Summary: "route references unknown pool"},
	{Code: "E301", Stage: "sema", Summary: "weighted-route entry references unknown pool"},
	{Code: "E302", Stage: "sema", Summary: "route apply references undefined policy"},
	{Code: "E303", Stage: "bundle", Summary: "duplicate pool name"},
	{Code: "E304", Stage: "bundle", Summary: "duplicate route name"},
	{Code: "E305", Stage: "parse", Summary: "route block missing required name"},
	{Code: "E306", Stage: "sema", Summary: "conflicting routes (same host and path)"},
	{Code: "E307", Stage: "parse", Summary: "invalid duration string"},
	{Code: "E308", Stage: "sema", Summary: "invalid lb_policy value"},
	{Code: "E309", Stage: "sema", Summary: "weighted-route weight negative"},
	{Code: "E310", Stage: "sema", Summary: "pool addrs must not be empty"},
	{Code: "E311", Stage: "sema", Summary: "max_ejected_percent out of range (0..100)"},
	{Code: "E312", Stage: "sema", Summary: "TLS cert/key path cannot be resolved"},
	{Code: "E313", Stage: "sema", Summary: "TLS block has one of cert/key without the other"},
	{Code: "E314", Stage: "sema", Summary: "listener addr missing port"},
	{Code: "E315", Stage: "sema", Summary: "reserved pool name"},
	{Code: "E316", Stage: "sema", Summary: "no routes declared"},
	{Code: "E317", Stage: "sema", Summary: "duplicate listener addr (plain vs TLS collision)"},
	{Code: "E318", Stage: "sema", Summary: "topology case references undefined route"},
	{Code: "E319", Stage: "sema", Summary: "retry_percent out of range (0..100)"},
	{Code: "E320", Stage: "sema", Summary: "health_check path must start with /"},
	{Code: "E321", Stage: "bundle", Summary: "duplicate topology case name"},
	{Code: "E322", Stage: "parse", Summary: "duplicate listener block"},
	{Code: "E323", Stage: "parse", Summary: "duplicate tls block"},
	{Code: "E324", Stage: "parse", Summary: "duplicate quic block"},
	{Code: "E325", Stage: "sema", Summary: "quic block missing listen"},
	{Code: "E326", Stage: "sema", Summary: "quic block requires cert/key or a sibling tls block"},
	{Code: "E330", Stage: "parse", Summary: "unexpected field in topology block"},
}

type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Msg)
}

func errf(code, format string, args ...any) error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

func LookupErrorCode(code string) (ErrorCode, bool) {
	for _, entry := range ErrorCatalog {
		if entry.Code == code {
			return entry, true
		}
	}
	return ErrorCode{}, false
}

func ErrorCatalogText() string {
	var b strings.Builder
	b.WriteString("intent error codes:\n")
	for _, entry := range ErrorCatalog {
		fmt.Fprintf(&b, "  %s  stage=%s  %s\n", entry.Code, entry.Stage, entry.Summary)
	}
	return b.String()
}

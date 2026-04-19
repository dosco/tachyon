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

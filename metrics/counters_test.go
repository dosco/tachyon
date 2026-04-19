package metrics

import (
	"bytes"
	"strings"
	"testing"
)

// resetGlobal zeroes every counter so tests don't leak state into
// each other. Tests are not run in parallel, so direct Store is fine.
func resetGlobal() {
	Global.Requests.Store(0)
	Global.OK2xx.Store(0)
	Global.Err4xx.Store(0)
	Global.Err5xx.Store(0)
	Global.UpDialErr.Store(0)
	Global.UpWriteErr.Store(0)
	Global.UpReadErr.Store(0)
}

// TestRecordStatus asserts that each call bumps Requests once and the
// correct class counter once. Codes outside the 2xx/4xx/5xx space
// still bump Requests but no class.
func TestRecordStatus(t *testing.T) {
	resetGlobal()
	RecordStatus(200)
	RecordStatus(201)
	RecordStatus(404)
	RecordStatus(502)
	RecordStatus(301) // 3xx: counted as a request, no class bump

	s := Read()
	if s.Requests != 5 {
		t.Fatalf("Requests: got %d want 5", s.Requests)
	}
	if s.OK2xx != 2 {
		t.Fatalf("OK2xx: got %d want 2", s.OK2xx)
	}
	if s.Err4xx != 1 {
		t.Fatalf("Err4xx: got %d want 1", s.Err4xx)
	}
	if s.Err5xx != 1 {
		t.Fatalf("Err5xx: got %d want 1", s.Err5xx)
	}
}

// TestWritePrometheus checks the output is well-formed Prometheus
// text-exposition format with every expected metric present.
func TestWritePrometheus(t *testing.T) {
	resetGlobal()
	RecordStatus(200)
	RecordStatus(404)
	Global.UpDialErr.Add(3)

	var out bytes.Buffer
	if err := WritePrometheus(&out); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	got := out.String()

	mustContain := []string{
		"# HELP tachyon_requests_total",
		"# TYPE tachyon_requests_total counter",
		"tachyon_requests_total 2",
		"# HELP tachyon_responses_total",
		`tachyon_responses_total{code="2xx"} 1`,
		`tachyon_responses_total{code="4xx"} 1`,
		`tachyon_responses_total{code="5xx"} 0`,
		`tachyon_upstream_errors_total{code="dial"} 3`,
		`tachyon_upstream_errors_total{code="write"} 0`,
		`tachyon_upstream_errors_total{code="read"} 0`,
	}
	for _, want := range mustContain {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\nfull output:\n%s", want, got)
		}
	}
}

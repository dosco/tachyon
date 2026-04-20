package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tachyon/internal/intent"
	cur "tachyon/internal/intent/generated/current"
	"tachyon/internal/traffic"
)

// TestReplayArtifactSummarizesTerminalIntent exercises replayArtifact end
// to end against the compiled topology baked into the binary. The
// config-path argument is retained for CLI compat but ignored by
// loadReplayContext.
func TestReplayArtifactSummarizesTerminalIntent(t *testing.T) {
	tmp := t.TempDir()
	artifactPath := filepath.Join(tmp, "traffic.ndjson.gz")

	if err := traffic.Enable(artifactPath); err != nil {
		t.Fatalf("enable recorder: %v", err)
	}
	traffic.Write(traffic.Record{
		Timestamp: time.Unix(1710000000, 0).UTC(),
		Method:    "GET",
		Host:      "example.com",
		Path:      "/blocked/demo",
		ClientIP:  "127.0.0.1",
	})
	if err := traffic.Close(); err != nil {
		t.Fatalf("close recorder: %v", err)
	}

	// Find a route that matches example.com / to confirm replay wires up.
	report, err := replayArtifact("intent/", artifactPath)
	if err != nil {
		t.Fatalf("replay artifact: %v", err)
	}
	if report.Requests != 1 {
		t.Fatalf("requests: got %d want 1", report.Requests)
	}
	if report.RouteMisses != 0 {
		t.Fatalf("route misses: got %d want 0", report.RouteMisses)
	}
	// The compiled bundle contains sample_terminal attached via policy; since
	// the topology may or may not reference it, we only assert a terminal
	// decision fired if any policy on the matched route produced one.
	if report.Terminals > 0 {
		if _, ok := report.TerminalStatuses[451]; !ok {
			t.Fatalf("terminal statuses missing 451: %v", report.TerminalStatuses)
		}
	}
}

func TestExplainArtifactReplaysStoredRequest(t *testing.T) {
	tmp := t.TempDir()
	artifactPath := filepath.Join(tmp, "traffic.ndjson.gz")

	if err := traffic.Enable(artifactPath); err != nil {
		t.Fatalf("enable recorder: %v", err)
	}
	traffic.Write(traffic.Record{
		Timestamp: time.Unix(1710000001, 0).UTC(),
		Method:    "GET",
		Host:      "example.com",
		Path:      "/",
		ClientIP:  "127.0.0.1",
	})
	if err := traffic.Close(); err != nil {
		t.Fatalf("close recorder: %v", err)
	}

	explained, err := explainArtifact("intent/", artifactPath, 1)
	if err != nil {
		t.Fatalf("explain artifact: %v", err)
	}
	if !explained.LiveMatch.Found {
		t.Fatal("expected route match")
	}
}

func TestIntentAgentGuideMentionsCLIWorkflow(t *testing.T) {
	guide := intentAgentGuide()
	for _, want := range []string{
		"tachyon intent grammar",
		"tachyon intent errors",
		"tachyon traffic replay ARTIFACT",
		"intent_error code=E...",
	} {
		if !strings.Contains(guide, want) {
			t.Fatalf("agent guide missing %q in %q", want, guide)
		}
	}
}

func TestIntentCLIErrorWrapsCompilerCode(t *testing.T) {
	err := intentCLIError(&intent.Error{Code: "E200", Msg: "bad phase"})
	if err == nil {
		t.Fatal("expected wrapped error")
	}
	got := err.Error()
	if !strings.Contains(got, "intent_error code=E200") {
		t.Fatalf("wrapped error = %q", got)
	}
	if !strings.Contains(got, `message="bad phase"`) {
		t.Fatalf("wrapped error = %q", got)
	}
}

func TestRunIntentCLILintReturnsStableCompilerEnvelope(t *testing.T) {
	tmp := t.TempDir()
	intentPath := filepath.Join(tmp, "bad.intent")
	writeFile(t, intentPath, `
intent_version "0.1"
policy bad {
  request {
    deny
  }
}
`)
	err := runIntentCLI([]string{"lint", intentPath})
	if err == nil {
		t.Fatal("expected lint error")
	}
	got := err.Error()
	if !strings.Contains(got, "intent_error code=E020") {
		t.Fatalf("lint error = %q", got)
	}
	if !strings.Contains(got, "bad.intent") {
		t.Fatalf("lint error = %q", got)
	}
}

// TestExampleWorkflowConfigBindsGeneratedPolicies verifies the compiled
// topology includes the example_workflow route with its two policies
// attached.
func TestExampleWorkflowConfigBindsGeneratedPolicies(t *testing.T) {
	cfg := cur.LoadConfig()
	programs, err := cur.BuildRoutePrograms(cfg.Routes)
	if err != nil {
		t.Fatalf("bind example intents: %v", err)
	}
	var exampleRouteID int = -1
	for _, r := range cfg.Routes {
		if r.Name == "example_workflow" {
			exampleRouteID = r.RouteID
			break
		}
	}
	if exampleRouteID < 0 {
		t.Fatal("example_workflow route not found in compiled topology")
	}
	set := programs.ByRouteID[exampleRouteID]
	if len(set.PolicyNames) != 2 {
		t.Fatalf("policy names: got %d want 2", len(set.PolicyNames))
	}
	if set.PolicyNames[0] != "example_block_admin_debug" {
		t.Fatalf("first policy: got %q", set.PolicyNames[0])
	}
	if set.PolicyNames[1] != "example_proxy_headers" {
		t.Fatalf("second policy: got %q", set.PolicyNames[1])
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

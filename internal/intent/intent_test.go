package intent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseFilesCollectsPolicies(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.intent")
	src := `intent_version "0.1"

policy sample {
  match req.host == "example.com"
  request {
    set_header("x", "y")
    auth_external("authz")
  }
  case allows_request {
    request.method "GET"
    request.host "example.com"
    request.path "/"
    expect.request_header "x", "y"
  }
  budget {
    request.allocs <= 4
    response.allocs <= 3
  }
}
`
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := ParseFiles([]string{p})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(b.Policies) != 1 {
		t.Fatalf("policies: got %d want 1", len(b.Policies))
	}
	if !b.Policies[0].RequiresClassC {
		t.Fatal("expected auth_external to mark class C")
	}
	if len(b.Policies[0].Cases) != 1 {
		t.Fatalf("cases: got %d want 1", len(b.Policies[0].Cases))
	}
	if got := b.Policies[0].Cases[0].Expect.RequestHeaders["x"]; got != "y" {
		t.Fatalf("expected request header case assertion, got %q", got)
	}
	if b.Policies[0].Budget.RequestAllocs == nil || *b.Policies[0].Budget.RequestAllocs != 4 {
		t.Fatalf("request budget: got %#v", b.Policies[0].Budget.RequestAllocs)
	}
	if b.Policies[0].Budget.ResponseAllocs == nil || *b.Policies[0].Budget.ResponseAllocs != 3 {
		t.Fatalf("response budget: got %#v", b.Policies[0].Budget.ResponseAllocs)
	}
}

func TestParseFilesRejectsDuplicatePolicies(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "a.intent")
	p2 := filepath.Join(dir, "b.intent")
	src := `intent_version "0.1"
policy dup {
  request {
    set_header("x", "y")
  }
}
`
	for _, p := range []string{p1, p2} {
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ParseFiles([]string{p1, p2}); err == nil {
		t.Fatal("expected duplicate policy error")
	}
}

func TestCheckRejectsRequestOnlyActionInResponseBlock(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.intent")
	src := `intent_version "0.1"
policy bad {
  response {
    deny(403)
  }
}
`
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := ParseFiles([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Check(b); err == nil {
		t.Fatal("expected E200 error for deny in response block")
	} else if e, ok := err.(*Error); !ok || e.Code != "E200" {
		t.Fatalf("expected E200, got %v", err)
	}
}

func TestCheckRejectsMultipleTerminals(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.intent")
	src := `intent_version "0.1"
policy bad {
  request {
    deny(403)
    respond(200, "ok")
  }
}
`
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := ParseFiles([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Check(b); err == nil {
		t.Fatal("expected E201 error for multiple terminals")
	} else if e, ok := err.(*Error); !ok || e.Code != "E201" {
		t.Fatalf("expected E201, got %v", err)
	}
}

func TestCheckRejectsContradictoryMatchConditions(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.intent")
	src := `intent_version "0.1"
policy bad {
  match req.host == "a.com" && req.host == "b.com"
  request {
    set_header("x", "y")
  }
}
`
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := ParseFiles([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Check(b); err == nil {
		t.Fatal("expected E202 error for contradictory match conditions")
	} else if e, ok := err.(*Error); !ok || e.Code != "E202" {
		t.Fatalf("expected E202, got %v", err)
	}
}

func TestCheckClassifiesPolicies(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.intent")
	src := `intent_version "0.1"
policy class_a {
  request {
    set_header("x", "y")
  }
}
policy class_b {
  request {
    rate_limit_local("ip", 10, 10)
  }
}
policy class_c {
  request {
    auth_external("http://authz/check")
  }
}
`
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := ParseFiles([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	ir, err := Check(b)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	byName := map[string]IRPolicy{}
	for _, p := range ir.Policies {
		byName[p.Name] = p
	}
	if got := byName["class_a"].Class; got != ClassA {
		t.Errorf("class_a: got class %v want A", got)
	}
	if got := byName["class_b"].Class; got != ClassB {
		t.Errorf("class_b: got class %v want B", got)
	}
	if got := byName["class_c"].Class; got != ClassC {
		t.Errorf("class_c: got class %v want C", got)
	}
}

func TestCheckCanonicalizesHeaderNames(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.intent")
	src := `intent_version "0.1"
policy hdr {
  match req.header("X-Api-Key") == "secret"
  request {
    set_header("x", "y")
  }
}
`
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := ParseFiles([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	ir, err := Check(b)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	for _, cond := range ir.Policies[0].Match {
		if cond.Name != "x-api-key" {
			t.Errorf("expected lowercase header name, got %q", cond.Name)
		}
	}
}

func TestParseNewPathPredicates(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.intent")
	src := `intent_version "0.1"
policy exact {
  match req.path == "/healthz"
  request {
    respond(200, "ok")
  }
}
policy suffix {
  match req.path.has_suffix(".json")
  request {
    set_header("content-type", "application/json")
  }
}
`
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := ParseFiles([]string{p})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(b.Policies) != 2 {
		t.Fatalf("expected 2 policies, got %d", len(b.Policies))
	}
}

func TestParseFilesRejectsUnknownCaseDirective(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.intent")
	src := `intent_version "0.1"

policy sample {
  case bad {
    expect.nope "x"
  }
}
`
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFiles([]string{p}); err == nil {
		t.Fatal("expected invalid case directive error")
	}
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"tachyon/internal/intent"
	cur "tachyon/internal/intent/generated/current"
	irt "tachyon/internal/intent/runtime"
	"tachyon/internal/router"
	"tachyon/internal/traffic"
)

func runCLI(args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "serve":
		return false, nil
	case "intent":
		return true, runIntentCLI(args[1:])
	case "traffic":
		return true, runTrafficCLI(args[1:])
	default:
		if strings.HasPrefix(args[0], "-") {
			return false, nil
		}
		return false, nil
	}
}

func runIntentCLI(args []string) error {
	if len(args) == 0 {
		fmt.Println("usage: tachyon intent <grammar|primitives|examples|scaffold|agent|errors|lint|build|verify|bench|diff|explain>")
		return nil
	}
	switch args[0] {
	case "grammar":
		fmt.Println(intent.Grammar)
		return nil
	case "primitives":
		fmt.Println(intent.Primitives)
		return nil
	case "examples":
		fmt.Println(intent.Example)
		return nil
	case "agent":
		fmt.Println(intentAgentGuide())
		return nil
	case "errors":
		fmt.Print(intent.ErrorCatalogText())
		return nil
	case "scaffold":
		name := "sample"
		if len(args) > 1 {
			name = args[1]
		}
		fmt.Println(intent.Scaffold(name))
		return nil
	case "lint":
		_, err := intent.ParseFiles(args[1:])
		return intentCLIError(err)
	case "build":
		b, err := intent.ParseFiles(args[1:])
		if err != nil {
			return intentCLIError(err)
		}
		if err := intent.Generate(b, ""); err != nil {
			return intentCLIError(err)
		}
		if err := runGo("test", "./internal/intent/..."); err != nil {
			return err
		}
		if err := runIntentBench(); err != nil {
			return err
		}
		return runGo("build", "-pgo=.tachyon/pgo/current.pprof", "./cmd/tachyon")
	case "verify":
		b, err := intent.ParseFiles(args[1:])
		if err != nil {
			return intentCLIError(err)
		}
		if err := intent.Generate(b, ""); err != nil {
			return intentCLIError(err)
		}
		return runGo("test", "./internal/intent/...")
	case "bench":
		b, err := intent.ParseFiles(args[1:])
		if err != nil {
			return intentCLIError(err)
		}
		if err := intent.Generate(b, ""); err != nil {
			return intentCLIError(err)
		}
		return runIntentBench()
	case "diff":
		if len(args) != 3 {
			return fmt.Errorf("usage: tachyon intent diff <old-dir-or-file> <new-dir-or-file>")
		}
		oldBundle, err := parseBundleArg(args[1])
		if err != nil {
			return intentCLIError(err)
		}
		newBundle, err := parseBundleArg(args[2])
		if err != nil {
			return intentCLIError(err)
		}
		fmt.Println(intent.Diff(oldBundle, newBundle))
		return nil
	case "explain":
		fs := flag.NewFlagSet("intent explain", flag.ContinueOnError)
		caseName := fs.String("case", "", "policy/case name")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *caseName == "" {
			fmt.Printf("generated registry version: %s\n", cur.Registry.Version)
			fmt.Printf("known policies: %d\n", len(cur.Registry.Policies))
			return nil
		}
		payload, err := explainIntentCase(*caseName)
		if err != nil {
			return intentCLIError(err)
		}
		fmt.Println(payload)
		return nil
	default:
		return fmt.Errorf("unknown intent subcommand %q", args[0])
	}
}

func runTrafficCLI(args []string) error {
	if len(args) == 0 {
		fmt.Println("usage: tachyon traffic <record|replay|explain>")
		return nil
	}
	switch args[0] {
	case "record":
		fs := flag.NewFlagSet("traffic record", flag.ContinueOnError)
		out := fs.String("out", "", "gzip NDJSON artifact path")
		config := fs.String("config", "intent/", "path to config file")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *out == "" {
			return fmt.Errorf("traffic record requires --out")
		}
		return runTrafficRecord(*config, *out, fs.Args())
	case "replay":
		fs := flag.NewFlagSet("traffic replay", flag.ContinueOnError)
		config := fs.String("config", "intent/", "path to config file")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: tachyon traffic replay [--config path] <artifact>")
		}
		report, err := replayArtifact(*config, fs.Arg(0))
		if err != nil {
			return err
		}
		printReplayReport(report)
		return nil
	case "explain":
		fs := flag.NewFlagSet("traffic explain", flag.ContinueOnError)
		artifact := fs.String("artifact", "", "artifact path")
		id := fs.String("id", "", "request id")
		config := fs.String("config", "intent/", "path to config file")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *artifact == "" || *id == "" {
			return fmt.Errorf("traffic explain requires --artifact and --id")
		}
		reqID, err := strconv.ParseUint(*id, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid request id %q: %w", *id, err)
		}
		explained, err := explainArtifact(*config, *artifact, reqID)
		if err != nil {
			return err
		}
		return printTrafficExplain(explained)
	default:
		return fmt.Errorf("unknown traffic subcommand %q", args[0])
	}
}

func parseBundleArg(arg string) (intent.Bundle, error) {
	info, err := os.Stat(arg)
	if err == nil && info.IsDir() {
		matches, globErr := filepath.Glob(filepath.Join(arg, "*.intent"))
		if globErr != nil {
			return intent.Bundle{}, globErr
		}
		return intent.ParseFiles(matches)
	}
	return intent.ParseFiles([]string{arg})
}

func explainIntentCase(ref string) (string, error) {
	bundle, err := intent.ParseFiles(nil)
	if err != nil {
		return "", intentCLIError(err)
	}
	policy, c, err := bundle.FindCase(ref)
	if err != nil {
		return "", err
	}
	set, err := cur.BuildRoutePrograms([]router.Rule{{RouteID: 1, Upstream: "origin", Intents: []string{policy.Name}}})
	if err != nil {
		return "", err
	}
	req := irt.StaticRequest{
		MethodValue:   defaultValue(c.Request.Method, "GET"),
		HostValue:     defaultValue(c.Request.Host, "example.com"),
		PathValue:     defaultValue(c.Request.Path, "/"),
		ClientIPValue: c.Request.ClientIP,
		HeadersValue:  c.Request.Headers,
	}
	reqResult := irt.ExecuteRequestTraced(set.ByRouteID[1], irt.NewState(), req, "origin")
	respResult := irt.ExecuteResponseTraced(set.ByRouteID[1], func(string) string { return "" })

	report := map[string]any{
		"policy":         policy.Name,
		"case":           c.Name,
		"request":        req,
		"expect":         c.Expect,
		"request_result": reqResult,
		"response_trace": respResult.Trace,
	}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func intentCLIError(err error) error {
	if err == nil {
		return nil
	}
	var ierr *intent.Error
	if errors.As(err, &ierr) {
		return fmt.Errorf("intent_error code=%s message=%q", ierr.Code, ierr.Msg)
	}
	return err
}

func intentAgentGuide() string {
	return `tachyon agent workflow:
  topology + behavior source: intent/*.intent
  generated artifacts: internal/intent/generated/current/
  ephemeral artifacts: .tachyon/

discover:
  tachyon intent grammar
  tachyon intent primitives
  tachyon intent examples
  tachyon intent errors

author:
  tachyon intent scaffold NAME
  edit intent/*.intent

verify:
  tachyon intent lint intent/*.intent
  tachyon intent build intent/*.intent
  tachyon intent verify intent/*.intent
  tachyon intent bench intent/*.intent

review behavior:
  tachyon intent diff OLD NEW
  tachyon intent explain --case POLICY/CASE
  tachyon traffic replay ARTIFACT
  tachyon traffic explain --artifact ARTIFACT --id REQUEST_ID

compiler failures:
  intent_error code=E... message="..."
  use "tachyon intent errors" for the stable code catalog`
}

func runGo(args ...string) error {
	cmd := exec.Command("go", args...)
	cmd.Env = goEnv()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runIntentBench() error {
	if err := os.MkdirAll(".tachyon/pgo", 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(".tachyon/bench", 0o755); err != nil {
		return err
	}
	out, err := os.Create(".tachyon/bench/current.json")
	if err != nil {
		return err
	}
	defer out.Close()

	cmd := exec.Command("go", "test", "./internal/intent/generated/current", "-run", "^$", "-bench", ".", "-benchmem", "-json", "-cpuprofile", ".tachyon/pgo/current.pprof")
	cmd.Env = goEnv()
	cmd.Stdout = io.MultiWriter(os.Stdout, out)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runTrafficRecord(configPath, out string, serveArgs []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"serve", "-config", configPath}
	args = append(args, serveArgs...)
	if !hasFlag(args, "-workers") {
		args = append(args, "-workers=1")
	}
	cmd := exec.CommandContext(context.Background(), exe, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(), traffic.EnvRecordOut+"="+out)
	return cmd.Run()
}

type replayReport struct {
	Artifact             string
	Config               string
	Requests             int
	RouteMisses          int
	Terminals            int
	PolicyMatches        map[string]int
	ActionFires          map[string]int
	TerminalStatuses     map[int]int
	StdlibRequiredRoutes map[int]int
}

type explainedRecord struct {
	Record      traffic.Record
	LiveMatch   router.MatchResult
	ReplayTrace irt.Trace
	HasTerminal bool
	Terminal    irt.TerminalResponse
}

func replayArtifact(configPath, artifactPath string) (replayReport, error) {
	cfg, r, programs, err := loadReplayContext(configPath)
	if err != nil {
		return replayReport{}, err
	}
	_ = cfg
	records, err := traffic.ReadAll(artifactPath)
	if err != nil {
		return replayReport{}, err
	}
	state := irt.NewState()
	report := replayReport{
		Artifact:             artifactPath,
		Config:               configPath,
		Requests:             len(records),
		PolicyMatches:        map[string]int{},
		ActionFires:          map[string]int{},
		TerminalStatuses:     map[int]int{},
		StdlibRequiredRoutes: map[int]int{},
	}
	for _, rec := range records {
		state.SetNowFunc(func() time.Time { return rec.Timestamp })
		match, result := replayRecord(r, programs, state, rec)
		if !match.Found {
			report.RouteMisses++
			continue
		}
		if set := programs.ByRouteID[match.RouteID]; set.RequiresStdlib {
			report.StdlibRequiredRoutes[match.RouteID]++
		}
		for _, policy := range result.Trace.Policies {
			if policy.Matched {
				report.PolicyMatches[policy.Name]++
			}
			for _, action := range policy.Actions {
				report.ActionFires[string(action.Kind)]++
			}
		}
		if result.HasTerminal {
			report.Terminals++
			report.TerminalStatuses[result.Terminal.Status]++
		}
	}
	return report, nil
}

func explainArtifact(configPath, artifactPath string, requestID uint64) (explainedRecord, error) {
	records, err := traffic.ReadAll(artifactPath)
	if err != nil {
		return explainedRecord{}, err
	}
	var selected *traffic.Record
	for i := range records {
		if records[i].ID == requestID {
			selected = &records[i]
			break
		}
	}
	if selected == nil {
		return explainedRecord{}, fmt.Errorf("request id %d not found in %s", requestID, artifactPath)
	}
	_, r, programs, err := loadReplayContext(configPath)
	if err != nil {
		return explainedRecord{}, err
	}
	state := irt.NewState()
	state.SetNowFunc(func() time.Time { return selected.Timestamp })
	match, result := replayRecord(r, programs, state, *selected)
	return explainedRecord{
		Record:      *selected,
		LiveMatch:   match,
		ReplayTrace: result.Trace,
		HasTerminal: result.HasTerminal,
		Terminal:    result.Terminal,
	}, nil
}

// loadReplayContext returns the compiled topology baked into the binary. The
// configPath argument is accepted for backwards compat with CLI plumbing but
// is not read at runtime — the topology lives in `current.LoadConfig()`.
func loadReplayContext(configPath string) (*router.Config, *router.Router, irt.RoutePrograms, error) {
	_ = configPath
	cfg := cur.LoadConfig()
	programs, err := cur.BuildRoutePrograms(cfg.Routes)
	if err != nil {
		return nil, nil, irt.RoutePrograms{}, err
	}
	return cfg, router.New(cfg.Routes), programs, nil
}

func replayRecord(r *router.Router, programs irt.RoutePrograms, state *irt.State, rec traffic.Record) (router.MatchResult, irt.RequestResult) {
	match := r.Match(strings.ToLower(rec.Host), []byte(rec.Path))
	if !match.Found {
		return match, irt.RequestResult{Trace: irt.Trace{RouteID: -1}}
	}
	result := irt.ExecuteRequestTraced(programs.ByRouteID[match.RouteID], state, irt.StaticRequest{
		MethodValue:   rec.Method,
		PathValue:     rec.Path,
		HostValue:     rec.Host,
		HeadersValue:  rec.Headers,
		ClientIPValue: rec.ClientIP,
	}, match.Upstream)
	return match, result
}

func printReplayReport(report replayReport) {
	fmt.Printf("artifact: %s\n", report.Artifact)
	fmt.Printf("config: %s\n", report.Config)
	fmt.Printf("requests: %d\n", report.Requests)
	fmt.Printf("route misses: %d\n", report.RouteMisses)
	fmt.Printf("terminal decisions: %d\n", report.Terminals)
	if len(report.TerminalStatuses) > 0 {
		fmt.Println("terminal status counts:")
		for _, line := range sortedIntCountLines(report.TerminalStatuses) {
			fmt.Println(line)
		}
	}
	if len(report.PolicyMatches) > 0 {
		fmt.Println("policy match counts:")
		for _, line := range sortedStringCountLines(report.PolicyMatches) {
			fmt.Println(line)
		}
	}
	if len(report.ActionFires) > 0 {
		fmt.Println("action fire counts:")
		for _, line := range sortedStringCountLines(report.ActionFires) {
			fmt.Println(line)
		}
	}
	if len(report.StdlibRequiredRoutes) > 0 {
		fmt.Println("compatibility warnings:")
		for _, line := range sortedIntCountLines(report.StdlibRequiredRoutes) {
			fmt.Printf("  route %s replayed through a stdlib-only intent set\n", line)
		}
	}
}

func printTrafficExplain(explained explainedRecord) error {
	fmt.Printf("request id: %d\n", explained.Record.ID)
	fmt.Printf("timestamp: %s\n", explained.Record.Timestamp.Format(time.RFC3339Nano))
	fmt.Printf("request: %s %s host=%s client_ip=%s\n", explained.Record.Method, explained.Record.Path, explained.Record.Host, explained.Record.ClientIP)
	if explained.LiveMatch.Found {
		fmt.Printf("matched route: %d upstream=%s\n", explained.LiveMatch.RouteID, explained.LiveMatch.Upstream)
	} else {
		fmt.Println("matched route: none")
	}
	if explained.HasTerminal {
		fmt.Printf("replayed terminal: status=%d body=%q\n", explained.Terminal.Status, explained.Terminal.Body)
	} else {
		fmt.Println("replayed terminal: none")
	}
	fmt.Println("captured trace:")
	captured, err := json.MarshalIndent(explained.Record.Trace, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(captured))
	fmt.Println("replayed trace:")
	replayed, err := json.MarshalIndent(explained.ReplayTrace, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(replayed))
	return nil
}

func hasFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == name || strings.HasPrefix(arg, name+"=") {
			return true
		}
	}
	return false
}

func goEnv() []string {
	env := os.Environ()
	for _, entry := range env {
		if strings.HasPrefix(entry, "GOCACHE=") {
			return env
		}
	}
	return append(env, "GOCACHE=/tmp/go-build")
}

func defaultValue(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func sortedStringCountLines(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, fmt.Sprintf("  %s: %d", k, counts[k]))
	}
	return lines
}

func sortedIntCountLines(counts map[int]int) []string {
	keys := make([]int, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, fmt.Sprintf("%d: %d", k, counts[k]))
	}
	return lines
}

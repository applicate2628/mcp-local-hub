package drmemory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newRequest wraps raw JSON args as a CallToolRequest so tests can invoke
// runTool without constructing the full MCP request plumbing. Mirrors the
// perftools direct-construction style.
func newRequest(t *testing.T, args map[string]any) *mcp.CallToolRequest {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: raw}}
}

func contentText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("empty Content")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("Content[0] is not TextContent: %T", res.Content[0])
	}
	return tc.Text
}

// TestRunTool_ParsesInjectedResults drives the handler end-to-end with
// BOTH seams injected: a fake findExe returning a fixed path and a fake
// runner that writes a canned results.txt into a temp logdir and returns
// its path + a non-zero target exit code. The handler must parse the
// canned blob into the structured runResult — never touching the real
// (slow) drmemory.exe.
func TestRunTool_ParsesInjectedResults(t *testing.T) {
	logdir := t.TempDir()
	subdir := filepath.Join(logdir, "DrMemory-target.exe.1234.000")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	resultsPath := filepath.Join(subdir, "results.txt")
	if err := os.WriteFile(resultsPath, []byte(cannedResults), 0o644); err != nil {
		t.Fatal(err)
	}

	var gotExe, gotTarget, gotCwd string
	var gotArgs []string
	var gotLight, gotCheckUninit bool

	ds := &DrMemoryServer{
		findExe: func() (string, error) { return `C:\fake\drmemory.exe`, nil },
		run: func(_ context.Context, exePath, target string, args []string, cwd string, light, checkUninit bool) (*runOutput, error) {
			gotExe, gotTarget, gotCwd = exePath, target, cwd
			gotArgs, gotLight, gotCheckUninit = args, light, checkUninit
			body, _ := os.ReadFile(resultsPath)
			return &runOutput{
				ExitCode:    7,
				ResultsText: string(body),
				Stderr:      "drmemory: nudge complete",
				ResultsPath: resultsPath,
			}, nil
		},
	}

	req := newRequest(t, map[string]any{
		"exe":                 `C:\proj\target.exe`,
		"args":                []string{"--input", "data.bin"},
		"cwd":                 `C:\proj`,
		"light":               true,
		"check_uninitialized": false,
	})

	res, err := ds.runTool(t.Context(), req)
	if err != nil {
		t.Fatalf("runTool returned error: %v", err)
	}
	if res.IsError {
		t.Fatalf("runTool IsError=true: %s", contentText(t, res))
	}

	// Seam wiring assertions.
	if gotExe != `C:\fake\drmemory.exe` {
		t.Errorf("exePath = %q, want injected drmemory path", gotExe)
	}
	if gotTarget != `C:\proj\target.exe` {
		t.Errorf("target = %q, want C:\\proj\\target.exe", gotTarget)
	}
	if gotCwd != `C:\proj` {
		t.Errorf("cwd = %q, want C:\\proj", gotCwd)
	}
	if len(gotArgs) != 2 || gotArgs[0] != "--input" || gotArgs[1] != "data.bin" {
		t.Errorf("args = %v, want [--input data.bin]", gotArgs)
	}
	if !gotLight {
		t.Error("light not forwarded as true")
	}
	if gotCheckUninit {
		t.Error("check_uninitialized=false not forwarded (got true)")
	}

	// Structured-result assertions.
	var parsed runResult
	body := contentText(t, res)
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, body)
	}
	if parsed.ExitCode != 7 {
		t.Errorf("exit_code = %d, want 7", parsed.ExitCode)
	}
	if parsed.ErrorCount != 2 {
		t.Errorf("error_count = %d, want 2", parsed.ErrorCount)
	}
	if parsed.LeakCount != 1 {
		t.Errorf("leak_count = %d, want 1", parsed.LeakCount)
	}
	if len(parsed.Errors) != 3 {
		t.Fatalf("errors len = %d, want 3", len(parsed.Errors))
	}
	if parsed.Errors[0].Type != "UNADDRESSABLE ACCESS" {
		t.Errorf("errors[0].type = %q, want UNADDRESSABLE ACCESS", parsed.Errors[0].Type)
	}
	if parsed.ResultsPath != resultsPath {
		t.Errorf("results_path = %q, want %q", parsed.ResultsPath, resultsPath)
	}
	if parsed.DurationMS < 0 {
		t.Errorf("duration_ms = %d, want >= 0", parsed.DurationMS)
	}
	if !strings.Contains(parsed.Stderr, "nudge complete") {
		t.Errorf("stderr not surfaced: %q", parsed.Stderr)
	}
	if !strings.Contains(parsed.Summary, "ERRORS FOUND:") {
		t.Errorf("summary missing ERRORS FOUND: %q", parsed.Summary)
	}
}

// TestRunTool_DrMemoryNotFound verifies the not-found path surfaces a
// STRUCTURED, non-empty result (never an opaque IsError text, never empty)
// carrying the install-guidance error in the `error` field — WITHOUT ever
// invoking the runner. The "never empty / always structured" contract is the
// fix for the live-agent "drmemory_run returns EMPTY" symptom.
func TestRunTool_DrMemoryNotFound(t *testing.T) {
	var runnerCalled bool
	ds := &DrMemoryServer{
		findExe: func() (string, error) { return "", ErrDrMemoryNotFound },
		run: func(context.Context, string, string, []string, string, bool, bool) (*runOutput, error) {
			runnerCalled = true
			return &runOutput{}, nil
		},
	}

	res, err := ds.runTool(t.Context(), newRequest(t, map[string]any{"exe": `C:\proj\target.exe`}))
	if err != nil {
		t.Fatalf("runTool returned error: %v", err)
	}
	if res.IsError {
		t.Fatal("expected a STRUCTURED result (IsError=false) when drmemory.exe not found, not a bare tool error")
	}
	if runnerCalled {
		t.Error("runner invoked despite drmemory.exe not found — must short-circuit")
	}
	// The body must be parseable JSON whose `error` names the not-found cause.
	var parsed runResult
	body := contentText(t, res)
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("not-found result is not valid JSON: %v\n%s", err, body)
	}
	if !strings.Contains(parsed.Error, "drmemory.exe not found") {
		t.Errorf("error field = %q, want to mention drmemory.exe not found", parsed.Error)
	}
	if parsed.ExitCode != -1 {
		t.Errorf("exit_code = %d, want -1 on not-found", parsed.ExitCode)
	}
}

// TestRunTool_MissingExe rejects a call with no target exe before touching
// any seam.
func TestRunTool_MissingExe(t *testing.T) {
	ds := &DrMemoryServer{
		findExe: func() (string, error) { return `C:\fake\drmemory.exe`, nil },
		run: func(context.Context, string, string, []string, string, bool, bool) (*runOutput, error) {
			t.Fatal("runner must not be called when exe is missing")
			return nil, nil
		},
	}
	res, err := ds.runTool(t.Context(), newRequest(t, map[string]any{}))
	if err != nil {
		t.Fatalf("runTool returned error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError=true for missing exe")
	}
	if !strings.Contains(contentText(t, res), "missing required parameter: exe") {
		t.Errorf("unexpected error text: %s", contentText(t, res))
	}
}

// TestRunTool_DefaultsCheckUninitializedTrue verifies the documented
// default: when check_uninitialized is omitted the handler forwards true.
func TestRunTool_DefaultsCheckUninitializedTrue(t *testing.T) {
	var gotCheckUninit bool
	var gotLight bool
	ds := &DrMemoryServer{
		findExe: func() (string, error) { return `C:\fake\drmemory.exe`, nil },
		run: func(_ context.Context, _, _ string, _ []string, _ string, light, checkUninit bool) (*runOutput, error) {
			gotLight, gotCheckUninit = light, checkUninit
			return &runOutput{ExitCode: 0, ResultsText: "NO ERRORS FOUND:\n"}, nil
		},
	}
	res, err := ds.runTool(t.Context(), newRequest(t, map[string]any{"exe": `C:\proj\t.exe`}))
	if err != nil {
		t.Fatalf("runTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", contentText(t, res))
	}
	if !gotCheckUninit {
		t.Error("check_uninitialized default = false, want true")
	}
	if gotLight {
		t.Error("light default = true, want false")
	}
}

// TestRunTool_RunnerFailureSurfaces verifies a genuine runner failure
// (not an *exec.ExitError) is encoded into a STRUCTURED result (never empty)
// carrying the cause + the resolved drmemory path + any captured stderr /
// command line the runner attached, so a swallowed launch failure is visible.
func TestRunTool_RunnerFailureSurfaces(t *testing.T) {
	ds := &DrMemoryServer{
		findExe: func() (string, error) { return `C:\fake\drmemory.exe`, nil },
		run: func(context.Context, string, string, []string, string, bool, bool) (*runOutput, error) {
			// Mimic defaultRun's launch-failure path: a populated runOutput
			// (with stderr + command line) returned ALONGSIDE the error.
			return &runOutput{
				ExitCode:    -1,
				Stderr:      "drmemory: failed to load client library",
				CommandLine: `C:\fake\drmemory.exe -batch -logdir TMP -- C:\proj\t.exe`,
			}, errFake
		},
	}
	res, err := ds.runTool(t.Context(), newRequest(t, map[string]any{"exe": `C:\proj\t.exe`}))
	if err != nil {
		t.Fatalf("runTool: %v", err)
	}
	if res.IsError {
		t.Fatal("expected a STRUCTURED result (IsError=false) on runner failure, not a bare tool error")
	}
	var parsed runResult
	body := contentText(t, res)
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("runner-failure result is not valid JSON: %v\n%s", err, body)
	}
	if !strings.Contains(parsed.Error, "drmemory run failed") {
		t.Errorf("error field = %q, want to mention 'drmemory run failed'", parsed.Error)
	}
	if parsed.DrMemoryPath != `C:\fake\drmemory.exe` {
		t.Errorf("drmemory_path = %q, want the resolved exe path", parsed.DrMemoryPath)
	}
	if !strings.Contains(parsed.Stderr, "failed to load client library") {
		t.Errorf("stderr not surfaced on failure: %q", parsed.Stderr)
	}
	if !strings.Contains(parsed.CommandLine, "drmemory.exe -batch") {
		t.Errorf("command_line not surfaced on failure: %q", parsed.CommandLine)
	}
}

// TestRunTool_PanicRecoveredToStructuredError verifies a panic in the runner
// seam is recovered into a STRUCTURED error result (never an empty result,
// never a crashed daemon — the "port went down" symptom).
func TestRunTool_PanicRecoveredToStructuredError(t *testing.T) {
	ds := &DrMemoryServer{
		findExe: func() (string, error) { return `C:\fake\drmemory.exe`, nil },
		run: func(context.Context, string, string, []string, string, bool, bool) (*runOutput, error) {
			panic("simulated DynamoRIO first-run fault")
		},
	}
	res, err := ds.runTool(t.Context(), newRequest(t, map[string]any{"exe": `C:\proj\t.exe`}))
	if err != nil {
		t.Fatalf("runTool returned a Go error instead of recovering: %v", err)
	}
	if res == nil {
		t.Fatal("runTool returned nil result on panic (must be structured, non-empty)")
	}
	if res.IsError {
		t.Fatal("expected a STRUCTURED result on panic, not a bare tool error")
	}
	var parsed runResult
	body := contentText(t, res)
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("panic result is not valid JSON: %v\n%s", err, body)
	}
	if !strings.Contains(parsed.Error, "internal panic") {
		t.Errorf("error field = %q, want to mention an internal panic", parsed.Error)
	}
	if parsed.ExitCode != -1 {
		t.Errorf("exit_code = %d, want -1 on panic", parsed.ExitCode)
	}
}

// TestRunTool_NoResultsTxtSurfacesError verifies the swallowed-failure mode:
// a run that "succeeds" (no Go error) but produced NO results.txt and NO
// summary must NOT look like a clean empty success — the handler attaches an
// explicit `error` plus the exit code / stderr / command line so a failed
// first-run (missing DynamoRIO bits) is diagnosable.
func TestRunTool_NoResultsTxtSurfacesError(t *testing.T) {
	ds := &DrMemoryServer{
		findExe: func() (string, error) { return `C:\fake\drmemory.exe`, nil },
		run: func(context.Context, string, string, []string, string, bool, bool) (*runOutput, error) {
			return &runOutput{
				ExitCode:    1,
				ResultsText: "", // no results.txt written
				ResultsPath: "",
				Stderr:      "drmemory: error initializing DynamoRIO",
				CommandLine: `C:\fake\drmemory.exe -batch -logdir TMP -- C:\proj\t.exe`,
			}, nil
		},
	}
	res, err := ds.runTool(t.Context(), newRequest(t, map[string]any{"exe": `C:\proj\t.exe`}))
	if err != nil {
		t.Fatalf("runTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected structured result, got IsError: %s", contentText(t, res))
	}
	var parsed runResult
	if err := json.Unmarshal([]byte(contentText(t, res)), &parsed); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if parsed.Error == "" || !strings.Contains(parsed.Error, "no results.txt") {
		t.Errorf("error field = %q, want to mention the missing results.txt", parsed.Error)
	}
	if !strings.Contains(parsed.Stderr, "initializing DynamoRIO") {
		t.Errorf("stderr not surfaced: %q", parsed.Stderr)
	}
	if !strings.Contains(parsed.CommandLine, "drmemory.exe -batch") {
		t.Errorf("command_line not surfaced: %q", parsed.CommandLine)
	}
	if parsed.DrMemoryPath != `C:\fake\drmemory.exe` {
		t.Errorf("drmemory_path = %q, want resolved exe path", parsed.DrMemoryPath)
	}
}

// errFake is a sentinel runner failure used by TestRunTool_RunnerFailureSurfaces.
var errFake = &fakeErr{}

type fakeErr struct{}

func (*fakeErr) Error() string { return "spawn failed: access denied" }

// TestStatusTool_Available reports availability + version from the injected
// probe seam — never spawning the real (slow) drmemory.exe.
func TestStatusTool_Available(t *testing.T) {
	ds := &DrMemoryServer{
		probeVersion: func() (string, string, bool) {
			return `C:\fake\drmemory.exe`, "Dr. Memory version 2.6.0 build 0", true
		},
	}
	res, err := ds.statusTool(t.Context(), newRequest(t, map[string]any{}))
	if err != nil {
		t.Fatalf("statusTool error: %v", err)
	}
	if res.IsError {
		t.Fatalf("statusTool IsError=true: %s", contentText(t, res))
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(contentText(t, res)), &got); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if got["available"] != true {
		t.Errorf("available = %v, want true", got["available"])
	}
	if got["drmemory_path"] != `C:\fake\drmemory.exe` {
		t.Errorf("drmemory_path = %v, want C:\\fake\\drmemory.exe", got["drmemory_path"])
	}
	if got["version"] != "Dr. Memory version 2.6.0 build 0" {
		t.Errorf("version = %v, want the injected banner", got["version"])
	}
}

// TestStatusTool_Unavailable reports the not-available shape when the probe
// cannot resolve drmemory.exe (no install).
func TestStatusTool_Unavailable(t *testing.T) {
	ds := &DrMemoryServer{
		probeVersion: func() (string, string, bool) {
			return "", "", false
		},
	}
	res, err := ds.statusTool(t.Context(), newRequest(t, map[string]any{}))
	if err != nil {
		t.Fatalf("statusTool error: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(contentText(t, res)), &got); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if got["available"] != false {
		t.Errorf("available = %v, want false", got["available"])
	}
	if got["drmemory_path"] != "" {
		t.Errorf("drmemory_path = %v, want empty on unavailable", got["drmemory_path"])
	}
	if got["version"] != "" {
		t.Errorf("version = %v, want empty on unavailable", got["version"])
	}
}

// TestStatusTool_AvailableVersionUnknown covers the documented fallback: the
// binary resolves but `-version` is unsupported, so available stays true while
// version is empty (a present install must never report available:false).
func TestStatusTool_AvailableVersionUnknown(t *testing.T) {
	ds := &DrMemoryServer{
		probeVersion: func() (string, string, bool) {
			return `C:\fake\drmemory.exe`, "", true
		},
	}
	res, err := ds.statusTool(t.Context(), newRequest(t, map[string]any{}))
	if err != nil {
		t.Fatalf("statusTool error: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(contentText(t, res)), &got); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if got["available"] != true {
		t.Errorf("available = %v, want true (binary resolved even without -version)", got["available"])
	}
	if got["drmemory_path"] != `C:\fake\drmemory.exe` {
		t.Errorf("drmemory_path = %v, want the resolved path", got["drmemory_path"])
	}
	if got["version"] != "" {
		t.Errorf("version = %v, want empty when -version unsupported", got["version"])
	}
}

func TestDrmemoryEnabledRequiresExplicitOptIn(t *testing.T) {
	t.Setenv(enableUnsafeDrmemoryEnv, "")
	if drmemoryEnabled() {
		t.Fatal("drmemory should be disabled without explicit opt-in")
	}
	t.Setenv(enableUnsafeDrmemoryEnv, "1")
	if !drmemoryEnabled() {
		t.Fatal("drmemory should be enabled with opt-in =1")
	}
}

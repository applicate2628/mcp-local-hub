package vtune

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newRequest wraps raw JSON args as a CallToolRequest so tests can invoke
// profileTool without constructing the full MCP request plumbing.
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

// TestProfileTool_ParsesInjectedReport drives the handler end-to-end with BOTH
// seams injected: a fake findExe returning a fixed path and a fake runner that
// returns a canned report CSV + summary and a non-zero target exit code. The
// handler must parse the canned CSV into the structured profileResult — never
// touching the real (slow) vtune.exe.
func TestProfileTool_ParsesInjectedReport(t *testing.T) {
	var gotExe, gotTarget, gotCwd, gotAnalysis string
	var gotArgs []string

	vs := &VTuneServer{
		findExe: func() (string, error) { return `C:\fake\vtune.exe`, nil },
		run: func(_ context.Context, exePath, target string, args []string, cwd, analysis string) (*runOutput, error) {
			gotExe, gotTarget, gotCwd, gotAnalysis = exePath, target, cwd, analysis
			gotArgs = args
			return &runOutput{
				ExitCode:    7,
				ReportCSV:   cannedHotspotsCSV,
				Summary:     "Elapsed Time: 0.048s\nTop Hotspots\n...",
				Stderr:      "vtune: Executing actions 100 % done",
				ReportPath:  `C:\tmp\run-123\report.csv`,
				CommandLine: `"C:\fake\vtune.exe" -collect hotspots -r RES -- C:\proj\target.exe`,
			}, nil
		},
	}

	req := newRequest(t, map[string]any{
		"exe":           `C:\proj\target.exe`,
		"args":          []string{"--input", "data.bin"},
		"cwd":           `C:\proj`,
		"analysis_type": "hotspots",
	})

	res, err := vs.profileTool(t.Context(), req)
	if err != nil {
		t.Fatalf("profileTool returned error: %v", err)
	}
	if res.IsError {
		t.Fatalf("profileTool IsError=true: %s", contentText(t, res))
	}

	// Seam wiring assertions.
	if gotExe != `C:\fake\vtune.exe` {
		t.Errorf("exePath = %q, want injected vtune path", gotExe)
	}
	if gotTarget != `C:\proj\target.exe` {
		t.Errorf("target = %q, want C:\\proj\\target.exe", gotTarget)
	}
	if gotCwd != `C:\proj` {
		t.Errorf("cwd = %q, want C:\\proj", gotCwd)
	}
	if gotAnalysis != "hotspots" {
		t.Errorf("analysis = %q, want hotspots", gotAnalysis)
	}
	if len(gotArgs) != 2 || gotArgs[0] != "--input" || gotArgs[1] != "data.bin" {
		t.Errorf("args = %v, want [--input data.bin]", gotArgs)
	}

	// Structured-result assertions.
	var parsed profileResult
	body := contentText(t, res)
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, body)
	}
	if parsed.ExitCode != 7 {
		t.Errorf("exit_code = %d, want 7", parsed.ExitCode)
	}
	if parsed.AnalysisType != "hotspots" {
		t.Errorf("analysis_type = %q, want hotspots", parsed.AnalysisType)
	}
	if len(parsed.TopHotspots) != 3 {
		t.Fatalf("top_hotspots len = %d, want 3", len(parsed.TopHotspots))
	}
	if parsed.TopHotspots[0].Function != "NtDeviceIoControlFile" {
		t.Errorf("top_hotspots[0].function = %q, want NtDeviceIoControlFile", parsed.TopHotspots[0].Function)
	}
	if parsed.TopHotspots[0].CPUTimeSeconds != 0.107988 {
		t.Errorf("top_hotspots[0].cpu_time_seconds = %v, want 0.107988", parsed.TopHotspots[0].CPUTimeSeconds)
	}
	if parsed.ReportPath != `C:\tmp\run-123\report.csv` {
		t.Errorf("report_path = %q, want the injected path", parsed.ReportPath)
	}
	if !strings.Contains(parsed.Summary, "Elapsed Time") {
		t.Errorf("summary not surfaced: %q", parsed.Summary)
	}
	if !strings.Contains(parsed.Stderr, "done") {
		t.Errorf("stderr not surfaced: %q", parsed.Stderr)
	}
	if parsed.DurationMS < 0 {
		t.Errorf("duration_ms = %d, want >= 0", parsed.DurationMS)
	}
}

// TestProfileTool_DefaultsAnalysisHotspots verifies the documented default:
// when analysis_type is omitted the handler forwards "hotspots".
func TestProfileTool_DefaultsAnalysisHotspots(t *testing.T) {
	var gotAnalysis string
	vs := &VTuneServer{
		findExe: func() (string, error) { return `C:\fake\vtune.exe`, nil },
		run: func(_ context.Context, _, _ string, _ []string, _, analysis string) (*runOutput, error) {
			gotAnalysis = analysis
			return &runOutput{ExitCode: 0, ReportCSV: "Function\tCPU Time\n", Summary: "ok", ReportPath: "p"}, nil
		},
	}
	res, err := vs.profileTool(t.Context(), newRequest(t, map[string]any{"exe": `C:\proj\t.exe`}))
	if err != nil {
		t.Fatalf("profileTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", contentText(t, res))
	}
	if gotAnalysis != "hotspots" {
		t.Errorf("analysis default = %q, want hotspots", gotAnalysis)
	}
}

// TestProfileTool_RejectsUnknownAnalysisType verifies the allowlist guard: an
// unknown analysis_type is rejected with a STRUCTURED error naming the
// accepted set, WITHOUT ever resolving the exe or invoking the runner.
func TestProfileTool_RejectsUnknownAnalysisType(t *testing.T) {
	var findCalled, runCalled bool
	vs := &VTuneServer{
		findExe: func() (string, error) { findCalled = true; return `C:\fake\vtune.exe`, nil },
		run: func(context.Context, string, string, []string, string, string) (*runOutput, error) {
			runCalled = true
			return &runOutput{}, nil
		},
	}
	res, err := vs.profileTool(t.Context(), newRequest(t, map[string]any{
		"exe":           `C:\proj\t.exe`,
		"analysis_type": "bogus-type",
	}))
	if err != nil {
		t.Fatalf("profileTool: %v", err)
	}
	if res.IsError {
		t.Fatal("expected a STRUCTURED result (IsError=false) for an unknown analysis type")
	}
	if findCalled {
		t.Error("findExe invoked despite an invalid analysis type — must short-circuit first")
	}
	if runCalled {
		t.Error("runner invoked despite an invalid analysis type — must short-circuit first")
	}
	var parsed profileResult
	if err := json.Unmarshal([]byte(contentText(t, res)), &parsed); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if !strings.Contains(parsed.Error, "unknown analysis_type") {
		t.Errorf("error field = %q, want to mention the unknown analysis type", parsed.Error)
	}
	// The error must enumerate at least one accepted type so the operator can fix.
	if !strings.Contains(parsed.Error, "hotspots") {
		t.Errorf("error field = %q, want to enumerate accepted types", parsed.Error)
	}
}

// TestProfileTool_VTuneNotFound verifies the not-found path surfaces a
// STRUCTURED, non-empty result carrying the install-guidance error — WITHOUT
// invoking the runner.
func TestProfileTool_VTuneNotFound(t *testing.T) {
	var runnerCalled bool
	vs := &VTuneServer{
		findExe: func() (string, error) { return "", ErrVTuneNotFound },
		run: func(context.Context, string, string, []string, string, string) (*runOutput, error) {
			runnerCalled = true
			return &runOutput{}, nil
		},
	}
	res, err := vs.profileTool(t.Context(), newRequest(t, map[string]any{"exe": `C:\proj\target.exe`}))
	if err != nil {
		t.Fatalf("profileTool returned error: %v", err)
	}
	if res.IsError {
		t.Fatal("expected a STRUCTURED result (IsError=false) when vtune.exe not found")
	}
	if runnerCalled {
		t.Error("runner invoked despite vtune.exe not found — must short-circuit")
	}
	var parsed profileResult
	if err := json.Unmarshal([]byte(contentText(t, res)), &parsed); err != nil {
		t.Fatalf("not-found result is not valid JSON: %v", err)
	}
	if !strings.Contains(parsed.Error, "vtune not found") {
		t.Errorf("error field = %q, want to mention vtune not found", parsed.Error)
	}
	if parsed.ExitCode != -1 {
		t.Errorf("exit_code = %d, want -1 on not-found", parsed.ExitCode)
	}
}

// TestProfileTool_MissingExe rejects a call with no target exe before touching
// any seam.
func TestProfileTool_MissingExe(t *testing.T) {
	vs := &VTuneServer{
		findExe: func() (string, error) { return `C:\fake\vtune.exe`, nil },
		run: func(context.Context, string, string, []string, string, string) (*runOutput, error) {
			t.Fatal("runner must not be called when exe is missing")
			return nil, nil
		},
	}
	res, err := vs.profileTool(t.Context(), newRequest(t, map[string]any{}))
	if err != nil {
		t.Fatalf("profileTool returned error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError=true for missing exe")
	}
	if !strings.Contains(contentText(t, res), "missing required parameter: exe") {
		t.Errorf("unexpected error text: %s", contentText(t, res))
	}
}

// TestProfileTool_RunnerFailureSurfaces verifies a genuine runner failure is
// encoded into a STRUCTURED result (never empty) carrying the cause + the
// resolved vtune path + any captured stderr / command line the runner
// attached, so a swallowed launch failure is visible.
func TestProfileTool_RunnerFailureSurfaces(t *testing.T) {
	vs := &VTuneServer{
		findExe: func() (string, error) { return `C:\fake\vtune.exe`, nil },
		run: func(context.Context, string, string, []string, string, string) (*runOutput, error) {
			return &runOutput{
				ExitCode:    -1,
				Stderr:      "vtune: error: failed to load the collector",
				CommandLine: `"C:\fake\vtune.exe" -collect hotspots -r RES -- C:\proj\t.exe`,
			}, errFake
		},
	}
	res, err := vs.profileTool(t.Context(), newRequest(t, map[string]any{"exe": `C:\proj\t.exe`}))
	if err != nil {
		t.Fatalf("profileTool: %v", err)
	}
	if res.IsError {
		t.Fatal("expected a STRUCTURED result (IsError=false) on runner failure")
	}
	var parsed profileResult
	if err := json.Unmarshal([]byte(contentText(t, res)), &parsed); err != nil {
		t.Fatalf("runner-failure result is not valid JSON: %v", err)
	}
	if !strings.Contains(parsed.Error, "vtune run failed") {
		t.Errorf("error field = %q, want to mention 'vtune run failed'", parsed.Error)
	}
	if parsed.VTunePath != `C:\fake\vtune.exe` {
		t.Errorf("vtune_path = %q, want the resolved exe path", parsed.VTunePath)
	}
	if !strings.Contains(parsed.Stderr, "failed to load the collector") {
		t.Errorf("stderr not surfaced on failure: %q", parsed.Stderr)
	}
	if !strings.Contains(parsed.CommandLine, "-collect hotspots") {
		t.Errorf("command_line not surfaced on failure: %q", parsed.CommandLine)
	}
}

// TestProfileTool_TimeoutSurfaces verifies a timed-out run is surfaced as a
// STRUCTURED result with timed_out=true and a raise-timeout hint.
func TestProfileTool_TimeoutSurfaces(t *testing.T) {
	vs := &VTuneServer{
		findExe: func() (string, error) { return `C:\fake\vtune.exe`, nil },
		run: func(ctx context.Context, _, _ string, _ []string, _, _ string) (*runOutput, error) {
			return &runOutput{
				ExitCode:    -1,
				Stderr:      "partial",
				CommandLine: "cmd",
				TimedOut:    true,
			}, context.DeadlineExceeded
		},
	}
	res, err := vs.profileTool(t.Context(), newRequest(t, map[string]any{"exe": `C:\proj\t.exe`, "timeout_sec": 1}))
	if err != nil {
		t.Fatalf("profileTool: %v", err)
	}
	if res.IsError {
		t.Fatal("expected a STRUCTURED result on timeout")
	}
	var parsed profileResult
	if err := json.Unmarshal([]byte(contentText(t, res)), &parsed); err != nil {
		t.Fatalf("timeout result is not valid JSON: %v", err)
	}
	if !parsed.TimedOut {
		t.Error("timed_out = false, want true")
	}
	if !strings.Contains(parsed.Error, "timed out") {
		t.Errorf("error field = %q, want to mention the timeout", parsed.Error)
	}
}

// TestProfileTool_PanicRecoveredToStructuredError verifies a panic in the
// runner seam is recovered into a STRUCTURED error result (never an empty
// result, never a crashed daemon).
func TestProfileTool_PanicRecoveredToStructuredError(t *testing.T) {
	vs := &VTuneServer{
		findExe: func() (string, error) { return `C:\fake\vtune.exe`, nil },
		run: func(context.Context, string, string, []string, string, string) (*runOutput, error) {
			panic("simulated collector fault")
		},
	}
	res, err := vs.profileTool(t.Context(), newRequest(t, map[string]any{"exe": `C:\proj\t.exe`}))
	if err != nil {
		t.Fatalf("profileTool returned a Go error instead of recovering: %v", err)
	}
	if res == nil {
		t.Fatal("profileTool returned nil result on panic (must be structured, non-empty)")
	}
	if res.IsError {
		t.Fatal("expected a STRUCTURED result on panic, not a bare tool error")
	}
	var parsed profileResult
	if err := json.Unmarshal([]byte(contentText(t, res)), &parsed); err != nil {
		t.Fatalf("panic result is not valid JSON: %v", err)
	}
	if !strings.Contains(parsed.Error, "internal panic") {
		t.Errorf("error field = %q, want to mention an internal panic", parsed.Error)
	}
	if parsed.ExitCode != -1 {
		t.Errorf("exit_code = %d, want -1 on panic", parsed.ExitCode)
	}
}

// TestProfileTool_NoReportSurfacesError verifies the swallowed-failure mode: a
// run that "succeeds" (no Go error) but produced NO report and NO summary must
// NOT look like a clean empty success — the handler attaches an explicit
// `error` plus the exit code / stderr / command line.
func TestProfileTool_NoReportSurfacesError(t *testing.T) {
	vs := &VTuneServer{
		findExe: func() (string, error) { return `C:\fake\vtune.exe`, nil },
		run: func(context.Context, string, string, []string, string, string) (*runOutput, error) {
			return &runOutput{
				ExitCode:    1,
				ReportCSV:   "",
				Summary:     "",
				ReportPath:  "",
				Stderr:      "vtune: error: finalization failed",
				CommandLine: `"C:\fake\vtune.exe" -collect hotspots -r RES -- C:\proj\t.exe`,
			}, nil
		},
	}
	res, err := vs.profileTool(t.Context(), newRequest(t, map[string]any{"exe": `C:\proj\t.exe`}))
	if err != nil {
		t.Fatalf("profileTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected structured result, got IsError: %s", contentText(t, res))
	}
	var parsed profileResult
	if err := json.Unmarshal([]byte(contentText(t, res)), &parsed); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if parsed.Error == "" || !strings.Contains(parsed.Error, "no report") {
		t.Errorf("error field = %q, want to mention the missing report", parsed.Error)
	}
	if !strings.Contains(parsed.Stderr, "finalization failed") {
		t.Errorf("stderr not surfaced: %q", parsed.Stderr)
	}
	// top_hotspots must be a non-nil empty slice (stable JSON shape).
	if parsed.TopHotspots == nil {
		t.Error("top_hotspots is nil; want [] for a stable JSON shape")
	}
}

// errFake is a sentinel runner failure used by TestProfileTool_RunnerFailureSurfaces.
var errFake = &fakeErr{}

type fakeErr struct{}

func (*fakeErr) Error() string { return "spawn failed: access denied" }

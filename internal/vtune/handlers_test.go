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
	var gotKeepResult bool

	vs := &VTuneServer{
		findExe: func() (string, error) { return `C:\fake\vtune.exe`, nil },
		run: func(_ context.Context, exePath, target string, args []string, cwd, analysis string, keepResult bool) (*runOutput, error) {
			gotExe, gotTarget, gotCwd, gotAnalysis = exePath, target, cwd, analysis
			gotArgs = args
			gotKeepResult = keepResult
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
	// keep_result was omitted, so the handler must forward the default false.
	if gotKeepResult {
		t.Error("keep_result forwarded true despite being omitted; want default false")
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
		run: func(_ context.Context, _, _ string, _ []string, _, analysis string, _ bool) (*runOutput, error) {
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

func TestProfileTool_StartReturnsDurableRunAndStatusDoesNotNeedCallerContext(t *testing.T) {
	d := &scriptedVTuneDriver{collectStarted: make(chan struct{}), releaseCollect: make(chan struct{})}
	o := newScriptedOwner(t, d)
	vs := &VTuneServer{
		findExe: func() (string, error) { return `C:\fake\vtune.exe`, nil },
		owner:   o,
	}
	res, err := vs.profileTool(t.Context(), newRequest(t, map[string]any{
		"action": "start", "exe": `C:\proj\t.exe`, "analysis_type": "hotspots", "timeout_sec": 60, "idempotency_key": "caller-free",
	}))
	if err != nil || res.IsError {
		t.Fatalf("start err=%v body=%s", err, contentText(t, res))
	}
	var started profileResult
	if err := json.Unmarshal([]byte(contentText(t, res)), &started); err != nil {
		t.Fatal(err)
	}
	if started.RunID == "" || (started.State != vtuneRunPrepared && started.State != vtuneRunCollecting) {
		t.Fatalf("start=%+v", started)
	}
	<-d.collectStarted
	status, err := vs.profileTool(context.Background(), newRequest(t, map[string]any{"action": "status", "run_id": started.RunID}))
	if err != nil || status.IsError {
		t.Fatalf("status err=%v", err)
	}
	var snapshot profileResult
	if err := json.Unmarshal([]byte(contentText(t, status)), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.RunID != started.RunID || snapshot.State == "" {
		t.Fatalf("status=%+v", snapshot)
	}
	close(d.releaseCollect)
}

// TestProfileTool_RejectsUnknownAnalysisType verifies the allowlist guard: an
// unknown analysis_type is rejected with a STRUCTURED error naming the
// accepted set, WITHOUT ever resolving the exe or invoking the runner.
func TestProfileTool_RejectsUnknownAnalysisType(t *testing.T) {
	var findCalled, runCalled bool
	vs := &VTuneServer{
		findExe: func() (string, error) { findCalled = true; return `C:\fake\vtune.exe`, nil },
		run: func(context.Context, string, string, []string, string, string, bool) (*runOutput, error) {
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
		run: func(context.Context, string, string, []string, string, string, bool) (*runOutput, error) {
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
		run: func(context.Context, string, string, []string, string, string, bool) (*runOutput, error) {
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
		run: func(context.Context, string, string, []string, string, string, bool) (*runOutput, error) {
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
		run: func(ctx context.Context, _, _ string, _ []string, _, _ string, _ bool) (*runOutput, error) {
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
		run: func(context.Context, string, string, []string, string, string, bool) (*runOutput, error) {
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
		run: func(context.Context, string, string, []string, string, string, bool) (*runOutput, error) {
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

// TestProfileTool_KeepResultForwardedAndSurfaced verifies the keep_result flow:
// the handler forwards keep_result=true to the run seam, and the result_dir the
// runner returns is surfaced in the structured JSON for vtune_report to reuse.
func TestProfileTool_KeepResultForwardedAndSurfaced(t *testing.T) {
	var gotKeep bool
	vs := &VTuneServer{
		findExe: func() (string, error) { return `C:\fake\vtune.exe`, nil },
		run: func(_ context.Context, _, _ string, _ []string, _, _ string, keepResult bool) (*runOutput, error) {
			gotKeep = keepResult
			return &runOutput{
				ExitCode:   0,
				ReportCSV:  "Function\tCPU Time\nfoo\t1.0\n",
				Summary:    "ok",
				ReportPath: `C:\tmp\run-9\report.csv`,
				ResultDir:  `C:\tmp\run-9\result`,
			}, nil
		},
	}
	res, err := vs.profileTool(t.Context(), newRequest(t, map[string]any{
		"exe":         `C:\proj\t.exe`,
		"keep_result": true,
	}))
	if err != nil {
		t.Fatalf("profileTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", contentText(t, res))
	}
	if !gotKeep {
		t.Error("keep_result=true was not forwarded to the run seam")
	}
	var parsed profileResult
	if err := json.Unmarshal([]byte(contentText(t, res)), &parsed); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if parsed.ResultDir != `C:\tmp\run-9\result` {
		t.Errorf("result_dir = %q, want the kept result dir surfaced", parsed.ResultDir)
	}
}

// TestStatusTool_Available drives vtune_status with an injected probe reporting
// an available vtune, asserting the structured {available, vtune_path, version,
// sep_driver_note} shape.
func TestStatusTool_Available(t *testing.T) {
	vs := &VTuneServer{
		probeVersion: func() (string, string, string, bool) {
			return `C:\fake\vtune.exe`, "Intel(R) VTune(TM) Profiler 2026.2.0", "hotspots need no SEP driver", true
		},
	}
	res, err := vs.statusTool(t.Context(), newRequest(t, map[string]any{}))
	if err != nil {
		t.Fatalf("statusTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", contentText(t, res))
	}
	var parsed struct {
		Available     bool   `json:"available"`
		VTunePath     string `json:"vtune_path"`
		Version       string `json:"version"`
		SepDriverNote string `json:"sep_driver_note"`
	}
	if err := json.Unmarshal([]byte(contentText(t, res)), &parsed); err != nil {
		t.Fatalf("status result is not valid JSON: %v", err)
	}
	if !parsed.Available {
		t.Error("available = false, want true")
	}
	if parsed.VTunePath != `C:\fake\vtune.exe` {
		t.Errorf("vtune_path = %q, want the probed path", parsed.VTunePath)
	}
	if !strings.Contains(parsed.Version, "VTune") {
		t.Errorf("version = %q, want the probed version banner", parsed.Version)
	}
	if !strings.Contains(parsed.SepDriverNote, "SEP") {
		t.Errorf("sep_driver_note = %q, want the probed SEP note", parsed.SepDriverNote)
	}
}

// TestStatusTool_Unavailable verifies the not-available path surfaces
// available=false (so a caller can tell vtune is missing) without crashing.
func TestStatusTool_Unavailable(t *testing.T) {
	vs := &VTuneServer{
		probeVersion: func() (string, string, string, bool) {
			return "", "vtune not found: ...", "n/a", false
		},
	}
	res, err := vs.statusTool(t.Context(), newRequest(t, map[string]any{}))
	if err != nil {
		t.Fatalf("statusTool: %v", err)
	}
	var parsed struct {
		Available bool `json:"available"`
	}
	if err := json.Unmarshal([]byte(contentText(t, res)), &parsed); err != nil {
		t.Fatalf("status result is not valid JSON: %v", err)
	}
	if parsed.Available {
		t.Error("available = true, want false when vtune not found")
	}
}

// TestStatusTool_PanicRecovered verifies a panic in the probe seam is recovered
// into a structured, non-empty result rather than crashing the daemon.
func TestStatusTool_PanicRecovered(t *testing.T) {
	vs := &VTuneServer{
		probeVersion: func() (string, string, string, bool) { panic("boom") },
	}
	res, err := vs.statusTool(t.Context(), newRequest(t, map[string]any{}))
	if err != nil {
		t.Fatalf("statusTool returned a Go error instead of recovering: %v", err)
	}
	if res == nil || len(res.Content) == 0 {
		t.Fatal("statusTool returned empty result on panic; want structured non-empty")
	}
	if !strings.Contains(contentText(t, res), "internal panic") {
		t.Errorf("panic result missing the panic marker: %s", contentText(t, res))
	}
}

// TestListAnalysesTool surfaces the host's advertised analyses AND the server's
// validation allowlist, so a caller sees both the real capability set and what
// vtune_profile will accept.
func TestListAnalysesTool(t *testing.T) {
	vs := &VTuneServer{
		listAnalyses: func() (string, []string, string, bool) {
			return `C:\fake\vtune.exe`,
				[]string{"hotspots", "memory-access", "gpu-offload"},
				"raw -collect-list body",
				true
		},
	}
	res, err := vs.listAnalysesTool(t.Context(), newRequest(t, map[string]any{}))
	if err != nil {
		t.Fatalf("listAnalysesTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", contentText(t, res))
	}
	var parsed struct {
		Available       bool     `json:"available"`
		VTunePath       string   `json:"vtune_path"`
		HostAnalyses    []string `json:"host_analyses"`
		AllowedAnalyses []string `json:"allowed_analyses"`
		Raw             string   `json:"raw"`
	}
	if err := json.Unmarshal([]byte(contentText(t, res)), &parsed); err != nil {
		t.Fatalf("list result is not valid JSON: %v", err)
	}
	if !parsed.Available {
		t.Error("available = false, want true")
	}
	// host_analyses comes straight from the injected probe — including the
	// gpu-offload type that is NOT in the validation allowlist (the point of the
	// tool: surface what the host has, not just what we accept).
	if strings.Join(parsed.HostAnalyses, ",") != "hotspots,memory-access,gpu-offload" {
		t.Errorf("host_analyses = %v, want the probed host list verbatim", parsed.HostAnalyses)
	}
	// allowed_analyses is the server's allowlist (sorted), independent of the host.
	if len(parsed.AllowedAnalyses) != len(knownAnalysisTypes) {
		t.Errorf("allowed_analyses len = %d, want %d (the validation allowlist)", len(parsed.AllowedAnalyses), len(knownAnalysisTypes))
	}
	foundHotspots := false
	for _, a := range parsed.AllowedAnalyses {
		if a == "hotspots" {
			foundHotspots = true
		}
	}
	if !foundHotspots {
		t.Errorf("allowed_analyses = %v, want to include hotspots", parsed.AllowedAnalyses)
	}
}

// TestListAnalysesTool_UnavailableNormalizesNilList verifies the not-available
// path: available=false AND host_analyses is a non-nil empty slice (stable JSON
// shape), not null.
func TestListAnalysesTool_UnavailableNormalizesNilList(t *testing.T) {
	vs := &VTuneServer{
		listAnalyses: func() (string, []string, string, bool) {
			return "", nil, "vtune not found: ...", false
		},
	}
	res, err := vs.listAnalysesTool(t.Context(), newRequest(t, map[string]any{}))
	if err != nil {
		t.Fatalf("listAnalysesTool: %v", err)
	}
	body := contentText(t, res)
	if !strings.Contains(body, `"host_analyses":[]`) {
		t.Errorf("host_analyses not normalized to [] on the unavailable path: %s", body)
	}
	if !strings.Contains(body, `"available":false`) {
		t.Errorf("available not false on the unavailable path: %s", body)
	}
}

// TestReportTool_ReReportsExistingDir drives vtune_report end-to-end with an
// injected report seam: it forwards the result_dir + analysis, parses the
// canned CSV, and returns the same structured shape as vtune_profile.
func TestReportTool_ReReportsExistingDir(t *testing.T) {
	var gotExe, gotResultDir, gotAnalysis string
	vs := &VTuneServer{
		findExe: func() (string, error) { return `C:\fake\vtune.exe`, nil },
		report: func(_ context.Context, exePath, resultDir, analysis string) (*runOutput, error) {
			gotExe, gotResultDir, gotAnalysis = exePath, resultDir, analysis
			return &runOutput{
				ExitCode:    0,
				ReportCSV:   cannedHotspotsCSV,
				Summary:     "re-report summary",
				ReportPath:  `C:\tmp\report-1\report.csv`,
				CommandLine: `"C:\fake\vtune.exe" -report hotspots -r RES ...`,
			}, nil
		},
	}
	res, err := vs.reportTool(t.Context(), newRequest(t, map[string]any{
		"result_dir":    `C:\tmp\run-9\result`,
		"analysis_type": "threading",
	}))
	if err != nil {
		t.Fatalf("reportTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", contentText(t, res))
	}
	if gotExe != `C:\fake\vtune.exe` {
		t.Errorf("exePath = %q, want the resolved vtune path", gotExe)
	}
	if gotResultDir != `C:\tmp\run-9\result` {
		t.Errorf("resultDir = %q, want the forwarded result_dir", gotResultDir)
	}
	if gotAnalysis != "threading" {
		t.Errorf("analysis = %q, want the forwarded analysis_type", gotAnalysis)
	}
	var parsed profileResult
	if err := json.Unmarshal([]byte(contentText(t, res)), &parsed); err != nil {
		t.Fatalf("report result is not valid JSON: %v", err)
	}
	if len(parsed.TopHotspots) != 3 {
		t.Fatalf("top_hotspots len = %d, want 3 (parsed from the re-report CSV)", len(parsed.TopHotspots))
	}
	if !strings.Contains(parsed.Summary, "re-report summary") {
		t.Errorf("summary not surfaced: %q", parsed.Summary)
	}
	if parsed.AnalysisType != "threading" {
		t.Errorf("analysis_type = %q, want threading", parsed.AnalysisType)
	}
}

// TestReportTool_DefaultsAnalysisHotspots verifies the default: an omitted
// analysis_type forwards "hotspots" to the report seam.
func TestReportTool_DefaultsAnalysisHotspots(t *testing.T) {
	var gotAnalysis string
	vs := &VTuneServer{
		findExe: func() (string, error) { return `C:\fake\vtune.exe`, nil },
		report: func(_ context.Context, _, _, analysis string) (*runOutput, error) {
			gotAnalysis = analysis
			return &runOutput{ReportCSV: "Function\tCPU Time\n", Summary: "ok", ReportPath: "p"}, nil
		},
	}
	res, err := vs.reportTool(t.Context(), newRequest(t, map[string]any{"result_dir": `C:\tmp\r`}))
	if err != nil {
		t.Fatalf("reportTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", contentText(t, res))
	}
	if gotAnalysis != "hotspots" {
		t.Errorf("analysis default = %q, want hotspots", gotAnalysis)
	}
}

// TestReportTool_MissingResultDir rejects a call with no result_dir before
// touching any seam.
func TestReportTool_MissingResultDir(t *testing.T) {
	vs := &VTuneServer{
		findExe: func() (string, error) { return `C:\fake\vtune.exe`, nil },
		report: func(context.Context, string, string, string) (*runOutput, error) {
			t.Fatal("report seam must not be called when result_dir is missing")
			return nil, nil
		},
	}
	res, err := vs.reportTool(t.Context(), newRequest(t, map[string]any{}))
	if err != nil {
		t.Fatalf("reportTool: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError=true for missing result_dir")
	}
	if !strings.Contains(contentText(t, res), "missing required parameter: result_dir") {
		t.Errorf("unexpected error text: %s", contentText(t, res))
	}
}

// TestReportTool_RejectsUnknownAnalysisType verifies the allowlist guard short-
// circuits before resolving the exe or invoking the report seam.
func TestReportTool_RejectsUnknownAnalysisType(t *testing.T) {
	var findCalled, reportCalled bool
	vs := &VTuneServer{
		findExe: func() (string, error) { findCalled = true; return `C:\fake\vtune.exe`, nil },
		report: func(context.Context, string, string, string) (*runOutput, error) {
			reportCalled = true
			return &runOutput{}, nil
		},
	}
	res, err := vs.reportTool(t.Context(), newRequest(t, map[string]any{
		"result_dir":    `C:\tmp\r`,
		"analysis_type": "bogus-type",
	}))
	if err != nil {
		t.Fatalf("reportTool: %v", err)
	}
	if res.IsError {
		t.Fatal("expected a STRUCTURED result for an unknown analysis type")
	}
	if findCalled {
		t.Error("findExe invoked despite invalid analysis type — must short-circuit")
	}
	if reportCalled {
		t.Error("report seam invoked despite invalid analysis type — must short-circuit")
	}
	var parsed profileResult
	if err := json.Unmarshal([]byte(contentText(t, res)), &parsed); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if !strings.Contains(parsed.Error, "unknown analysis_type") {
		t.Errorf("error field = %q, want to mention the unknown analysis type", parsed.Error)
	}
}

// TestReportTool_FailureSurfaces verifies a report-seam failure is encoded into
// a STRUCTURED result carrying the cause + the resolved vtune path + captured
// stderr, never empty.
func TestReportTool_FailureSurfaces(t *testing.T) {
	vs := &VTuneServer{
		findExe: func() (string, error) { return `C:\fake\vtune.exe`, nil },
		report: func(context.Context, string, string, string) (*runOutput, error) {
			return &runOutput{
				Stderr:      "vtune: error: cannot open result dir",
				CommandLine: `"C:\fake\vtune.exe" -report hotspots -r RES ...`,
			}, errFake
		},
	}
	res, err := vs.reportTool(t.Context(), newRequest(t, map[string]any{"result_dir": `C:\tmp\bad`}))
	if err != nil {
		t.Fatalf("reportTool: %v", err)
	}
	if res.IsError {
		t.Fatal("expected a STRUCTURED result on report failure")
	}
	var parsed profileResult
	if err := json.Unmarshal([]byte(contentText(t, res)), &parsed); err != nil {
		t.Fatalf("failure result is not valid JSON: %v", err)
	}
	if !strings.Contains(parsed.Error, "vtune report failed") {
		t.Errorf("error field = %q, want to mention 'vtune report failed'", parsed.Error)
	}
	if parsed.VTunePath != `C:\fake\vtune.exe` {
		t.Errorf("vtune_path = %q, want the resolved exe path", parsed.VTunePath)
	}
	if !strings.Contains(parsed.Stderr, "cannot open result dir") {
		t.Errorf("stderr not surfaced on failure: %q", parsed.Stderr)
	}
}

// TestReportTool_PanicRecovered verifies a panic in the report seam is
// recovered into a structured error result (never empty, never a crash).
func TestReportTool_PanicRecovered(t *testing.T) {
	vs := &VTuneServer{
		findExe: func() (string, error) { return `C:\fake\vtune.exe`, nil },
		report: func(context.Context, string, string, string) (*runOutput, error) {
			panic("simulated reporter fault")
		},
	}
	res, err := vs.reportTool(t.Context(), newRequest(t, map[string]any{"result_dir": `C:\tmp\r`}))
	if err != nil {
		t.Fatalf("reportTool returned a Go error instead of recovering: %v", err)
	}
	if res == nil {
		t.Fatal("reportTool returned nil result on panic")
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
}

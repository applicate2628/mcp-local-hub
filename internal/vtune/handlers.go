package vtune

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// defaultTimeoutSec is the wall-clock cap applied when the caller omits
// timeout_sec. It covers the full collect+report cycle. 10 minutes is a
// conservative default for a non-trivial target.
const defaultTimeoutSec = 600

// maxHotspots caps the number of top_hotspots rows surfaced in the structured
// result, so a report with thousands of functions does not bloat the JSON
// body. VTune sorts hotspots heaviest-first, so the cap keeps the most
// actionable rows.
const maxHotspots = 50

// profileResult is the structured payload returned to the MCP client. It is
// marshalled to JSON and placed in a single TextContent so clients that only
// render text still see the full report.
//
// The diagnostic fields (Error, VTunePath, CommandLine) are populated on the
// FAILURE paths (vtune-not-found, launch failure, report missing, panic) so a
// swallowed failure is always visible. They are omitempty so a clean run's
// JSON is unchanged.
type profileResult struct {
	ExitCode     int       `json:"exit_code"`
	AnalysisType string    `json:"analysis_type"`
	Summary      string    `json:"summary"`
	TopHotspots  []Hotspot `json:"top_hotspots"`
	ReportPath   string    `json:"report_path"`
	// ResultDir is the retained per-run VTune result dir, populated ONLY when
	// the caller passed keep_result=true; feed it back to vtune_report to
	// re-report without re-profiling. Empty (and omitted) on a default run.
	ResultDir   string `json:"result_dir,omitempty"`
	CommandLine string `json:"command_line,omitempty"`
	Stderr      string `json:"stderr,omitempty"`
	DurationMS  int64  `json:"duration_ms"`
	TimedOut    bool   `json:"timed_out"`
	Truncated   bool   `json:"truncated"`
	// Error carries the cause when the run could not produce findings (exe not
	// found, invalid analysis type, launch failure, panic, missing report).
	// Empty on a successful parse.
	Error string `json:"error,omitempty"`
	// VTunePath is the resolved vtune.exe path used for the run, so a failed
	// launch is traceable.
	VTunePath          string         `json:"vtune_path,omitempty"`
	SchemaVersion      string         `json:"schema_version,omitempty"`
	RunID              string         `json:"run_id,omitempty"`
	State              string         `json:"state,omitempty"`
	Phase              string         `json:"phase,omitempty"`
	RequestDisposition string         `json:"request_disposition,omitempty"`
	FailureID          string         `json:"failure_id,omitempty"`
	Reportable         bool           `json:"reportable,omitempty"`
	StopReason         string         `json:"stop_reason,omitempty"`
	PhaseExitCodes     map[string]int `json:"phase_exit_codes,omitempty"`
	ReceiptSHA256      string         `json:"receipt_sha256,omitempty"`
	Quarantined        bool           `json:"quarantined,omitempty"`
}

// profileTool is the vtune_profile handler. It resolves vtune.exe via the
// findExe seam, validates analysis_type against the allowlist, profiles the
// target via the run seam (with a timeout context), parses the resulting CSV
// report, and returns the structured findings. Both seams are injected so
// tests never run the real profiler.
func (vs *VTuneServer) profileTool(ctx context.Context, req *mcp.CallToolRequest) (result *mcp.CallToolResult, retErr error) {
	// NEVER return empty and NEVER crash the daemon. A panic anywhere in the
	// handler is recovered here and turned into a structured error result, so
	// vtune_profile always returns a parseable answer the caller can diagnose.
	defer func() {
		if r := recover(); r != nil {
			result = structuredErrResult(profileResult{
				ExitCode: -1,
				Error:    fmt.Sprintf("vtune: internal panic: %v", r),
			})
			retErr = nil
		}
	}()

	var args struct {
		Exe            string   `json:"exe"`
		Args           []string `json:"args"`
		Cwd            string   `json:"cwd"`
		AnalysisType   string   `json:"analysis_type"`
		KeepResult     bool     `json:"keep_result"`
		TimeoutSec     int      `json:"timeout_sec"`
		Action         string   `json:"action"`
		RunID          string   `json:"run_id"`
		IdempotencyKey string   `json:"idempotency_key"`
		OperationID    string   `json:"operation_id"`
		Stop           bool     `json:"stop"`
		WaitSec        int      `json:"wait_sec"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	action := strings.TrimSpace(args.Action)
	if action == "" {
		action = "run"
	}
	if (action == "status" || action == "stop") && vs.owner != nil {
		if action == "status" {
			run, ok := vs.owner.Status(args.RunID)
			if !ok {
				return structuredErrResult(profileResult{ExitCode: -1, FailureID: failureRunNotFound, Error: failureRunNotFound}), nil
			}
			return structuredErrResult(profileResultFromRun(run)), nil
		}
		run, disposition, stopErr := vs.owner.Stop(args.RunID, args.OperationID, args.Stop)
		if stopErr != nil {
			return structuredErrResult(profileResult{ExitCode: -1, FailureID: failureIDFor(stopErr), Error: stopErr.Error()}), nil
		}
		res := profileResultFromRun(run)
		res.RequestDisposition = disposition
		return structuredErrResult(res), nil
	}
	if strings.TrimSpace(args.Exe) == "" {
		return errResult("missing required parameter: exe (path to the target .exe)"), nil
	}

	// Validate / default the analysis type against the allowlist BEFORE any
	// expensive work, so an unknown type is rejected with a clear, structured
	// error naming the accepted set instead of reaching vtune.exe.
	analysis := strings.TrimSpace(args.AnalysisType)
	if analysis == "" {
		analysis = defaultAnalysisType
	}
	if !knownAnalysisTypes[analysis] {
		return structuredErrResult(profileResult{
			ExitCode:     -1,
			AnalysisType: analysis,
			Error: fmt.Sprintf("unknown analysis_type %q: must be one of %s",
				analysis, strings.Join(sortedAnalysisTypes(), ", ")),
		}), nil
	}

	// Resolve vtune.exe via the injectable seam. On a not-found / probe failure
	// we surface a STRUCTURED result (never empty, never a bare IsError text)
	// carrying the install-guidance error so the caller can see exactly what
	// went wrong.
	exePath, err := vs.findExe()
	if err != nil {
		return structuredErrResult(profileResult{
			ExitCode:     -1,
			AnalysisType: analysis,
			Error:        err.Error(),
		}), nil
	}
	// Durable actions intentionally detach from the MCP caller. The existing
	// no-action path below remains the short synchronous compatibility surface.
	timeoutSec := args.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = defaultTimeoutSec
	}
	if action != "run" {
		if vs.owner == nil {
			return structuredErrResult(profileResult{ExitCode: -1, AnalysisType: analysis, FailureID: failureContainmentUnavailable, Error: "vtune durable run owner unavailable"}), nil
		}
		switch action {
		case "start":
			run, disposition, startErr := vs.owner.Start(vtuneRunRequest{Target: args.Exe, Args: args.Args, Cwd: args.Cwd, AnalysisType: analysis, TimeoutSec: timeoutSec, KeepResult: args.KeepResult, IdempotencyKey: args.IdempotencyKey, VTunePath: exePath})
			if startErr != nil {
				return structuredErrResult(profileResult{ExitCode: -1, AnalysisType: analysis, FailureID: failureIDFor(startErr), Error: startErr.Error()}), nil
			}
			res := profileResultFromRun(run)
			res.RequestDisposition = disposition
			return structuredErrResult(res), nil
		default:
			return structuredErrResult(profileResult{ExitCode: -1, AnalysisType: analysis, Error: "unknown action: must be one of run, start, status, stop"}), nil
		}
	}
	if deadline, hasDeadline := ctx.Deadline(); hasDeadline && time.Until(deadline) < time.Duration(timeoutSec+30)*time.Second {
		return structuredErrResult(profileResult{ExitCode: -1, AnalysisType: analysis, FailureID: failureCallerDeadlineTooShort, Error: "caller deadline is too short for synchronous VTune run; use action=start"}), nil
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	start := time.Now()
	out, err := vs.run(runCtx, exePath, args.Exe, args.Args, args.Cwd, analysis, args.KeepResult)
	durationMS := time.Since(start).Milliseconds()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			res := profileResult{
				ExitCode:     -1,
				AnalysisType: analysis,
				Error:        fmt.Sprintf("vtune run timed out after %d s (raise timeout_sec for a long-running target)", timeoutSec),
				VTunePath:    exePath,
				DurationMS:   durationMS,
				TimedOut:     true,
			}
			if out != nil {
				res.Stderr = out.Stderr
				res.CommandLine = out.CommandLine
				res.ResultDir = out.ResultDir
			}
			return structuredErrResult(res), nil
		}
		// Genuine launch failure (vtune.exe non-executable, missing oneAPI
		// runtime, etc.). Encode the error into a STRUCTURED result and surface
		// every diagnostic the runner captured (stderr + the command line) so a
		// swallowed launch failure is visible instead of returning empty.
		res := profileResult{
			ExitCode:     -1,
			AnalysisType: analysis,
			Error:        fmt.Sprintf("vtune run failed: %v", err),
			VTunePath:    exePath,
			DurationMS:   durationMS,
		}
		if out != nil {
			res.Stderr = out.Stderr
			res.CommandLine = out.CommandLine
			res.TimedOut = out.TimedOut
			res.ResultDir = out.ResultDir
			if out.ExitCode != 0 {
				res.ExitCode = out.ExitCode
			}
		}
		return structuredErrResult(res), nil
	}

	parsed := parseReport(out.ReportCSV)
	top := parsed.Hotspots
	if len(top) > maxHotspots {
		top = top[:maxHotspots]
	}

	res := profileResult{
		ExitCode:     out.ExitCode,
		AnalysisType: analysis,
		Summary:      out.Summary,
		TopHotspots:  top,
		ReportPath:   out.ReportPath,
		ResultDir:    out.ResultDir,
		CommandLine:  out.CommandLine,
		Stderr:       out.Stderr,
		DurationMS:   durationMS,
		TimedOut:     out.TimedOut,
		Truncated: strings.Contains(out.ReportCSV, "…[truncated]") ||
			strings.Contains(out.Summary, "…[truncated]"),
		VTunePath: exePath,
	}
	if res.TopHotspots == nil {
		res.TopHotspots = []Hotspot{}
	}

	// A run that produced NO report CSV (out.ReportPath == "") and NO summary
	// is a swallowed failure mode: VTune launched but never wrote a report
	// (finalization failure, missing result DB). Surface it explicitly rather
	// than returning an empty-looking success — the exit code, stderr, and
	// command line are already attached.
	if out.ReportPath == "" && strings.TrimSpace(out.Summary) == "" {
		res.Error = "vtune produced no report (the collect/report run may have failed before writing one — check stderr / exit_code / command_line)"
	}

	body, err := json.Marshal(res)
	if err != nil {
		return errResult(fmt.Sprintf("failed to marshal result: %v", err)), nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}, nil
}

// statusTool handles vtune_status: it probes vtune availability via the
// injectable seam and returns {available, vtune_path, version, sep_driver_note}.
// It runs NO profiling and executes no target, so it is ungated. Mirrors gdb's
// debugger_status.
func (vs *VTuneServer) statusTool(ctx context.Context, req *mcp.CallToolRequest) (result *mcp.CallToolResult, retErr error) {
	// Same never-crash discipline as profileTool: a panic in the probe seam is
	// recovered into a structured (non-empty) result.
	defer func() {
		if r := recover(); r != nil {
			body, _ := json.Marshal(map[string]any{
				"available": false,
				"error":     fmt.Sprintf("vtune: internal panic: %v", r),
			})
			result = textResult(string(body))
			retErr = nil
		}
	}()

	path, version, sepNote, available := vs.probeVersion()
	body, err := json.Marshal(map[string]any{
		"available":       available,
		"vtune_path":      path,
		"version":         version,
		"sep_driver_note": sepNote,
	})
	if err != nil {
		return errResult(fmt.Sprintf("failed to marshal status: %v", err)), nil
	}
	return textResult(string(body)), nil
}

// listAnalysesTool handles vtune_list_analyses: it asks the host's vtune for
// its actual supported collect types via the injectable seam and returns them
// alongside this server's validation allowlist, so a caller sees both the
// host's real capability set and the set vtune_profile will accept. Ungated
// (runs `vtune -collect-list`, never a caller-supplied target).
func (vs *VTuneServer) listAnalysesTool(ctx context.Context, req *mcp.CallToolRequest) (result *mcp.CallToolResult, retErr error) {
	defer func() {
		if r := recover(); r != nil {
			body, _ := json.Marshal(map[string]any{
				"available": false,
				"error":     fmt.Sprintf("vtune: internal panic: %v", r),
			})
			result = textResult(string(body))
			retErr = nil
		}
	}()

	path, hostAnalyses, raw, available := vs.listAnalyses()
	if hostAnalyses == nil {
		hostAnalyses = []string{}
	}
	body, err := json.Marshal(map[string]any{
		"available":        available,
		"vtune_path":       path,
		"host_analyses":    hostAnalyses,
		"allowed_analyses": sortedAnalysisTypes(),
		"raw":              truncate(raw, vtuneOutputCap),
	})
	if err != nil {
		return errResult(fmt.Sprintf("failed to marshal analyses: %v", err)), nil
	}
	return textResult(string(body)), nil
}

// reportTool handles vtune_report: it re-runs ONLY VTune's report phase against
// an existing result dir (left behind by a prior keep_result profile) via the
// report seam, parses the resulting CSV, and returns the SAME structured shape
// as vtune_profile. No collect, no target execution. Gated like vtune_profile.
func (vs *VTuneServer) reportTool(ctx context.Context, req *mcp.CallToolRequest) (result *mcp.CallToolResult, retErr error) {
	defer func() {
		if r := recover(); r != nil {
			result = structuredErrResult(profileResult{
				ExitCode: -1,
				Error:    fmt.Sprintf("vtune: internal panic: %v", r),
			})
			retErr = nil
		}
	}()

	var args struct {
		ResultDir    string `json:"result_dir"`
		AnalysisType string `json:"analysis_type"`
		TimeoutSec   int    `json:"timeout_sec"`
		RunID        string `json:"run_id"`
		WaitSec      int    `json:"wait_sec"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if strings.TrimSpace(args.RunID) != "" {
		if vs.owner == nil {
			return structuredErrResult(profileResult{ExitCode: -1, FailureID: failureContainmentUnavailable, Error: "vtune durable run owner unavailable"}), nil
		}
		wait := args.WaitSec
		if wait < 0 {
			wait = 0
		}
		if wait > 30 {
			wait = 30
		}
		until := time.Now().Add(time.Duration(wait) * time.Second)
		for {
			run, ok := vs.owner.Status(args.RunID)
			if !ok {
				return structuredErrResult(profileResult{ExitCode: -1, FailureID: failureRunNotFound, Error: failureRunNotFound}), nil
			}
			if run.Terminal() || wait == 0 || time.Now().After(until) {
				if run.Terminal() && !run.Reportable && run.FailureID == "" {
					run.FailureID = failureResultNonReportable
					if run.ExitCode == 0 {
						run.ExitCode = -1
					}
				}
				return structuredErrResult(profileResultFromRun(run)), nil
			}
			select {
			case <-ctx.Done():
				return structuredErrResult(profileResultFromRun(run)), nil
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	if strings.TrimSpace(args.ResultDir) == "" {
		return errResult("missing required parameter: result_dir (path to an existing VTune result dir)"), nil
	}

	// analysis_type only selects the report name (always "hotspots" today); it
	// is still validated against the allowlist so a typo is rejected with the
	// same clear error as vtune_profile rather than reaching vtune.exe.
	analysis := strings.TrimSpace(args.AnalysisType)
	if analysis == "" {
		analysis = defaultAnalysisType
	}
	if !knownAnalysisTypes[analysis] {
		return structuredErrResult(profileResult{
			ExitCode:     -1,
			AnalysisType: analysis,
			Error: fmt.Sprintf("unknown analysis_type %q: must be one of %s",
				analysis, strings.Join(sortedAnalysisTypes(), ", ")),
		}), nil
	}

	exePath, err := vs.findExe()
	if err != nil {
		return structuredErrResult(profileResult{
			ExitCode:     -1,
			AnalysisType: analysis,
			Error:        err.Error(),
		}), nil
	}

	timeoutSec := args.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = defaultTimeoutSec
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	start := time.Now()
	out, err := vs.report(runCtx, exePath, args.ResultDir, analysis)
	durationMS := time.Since(start).Milliseconds()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			res := profileResult{
				ExitCode:     -1,
				AnalysisType: analysis,
				Error:        fmt.Sprintf("vtune report timed out after %d s (raise timeout_sec)", timeoutSec),
				VTunePath:    exePath,
				DurationMS:   durationMS,
				TimedOut:     true,
			}
			if out != nil {
				res.Stderr = out.Stderr
				res.CommandLine = out.CommandLine
			}
			return structuredErrResult(res), nil
		}
		res := profileResult{
			ExitCode:     -1,
			AnalysisType: analysis,
			Error:        fmt.Sprintf("vtune report failed: %v", err),
			VTunePath:    exePath,
			DurationMS:   durationMS,
		}
		if out != nil {
			res.Stderr = out.Stderr
			res.CommandLine = out.CommandLine
		}
		return structuredErrResult(res), nil
	}

	parsed := parseReport(out.ReportCSV)
	top := parsed.Hotspots
	if len(top) > maxHotspots {
		top = top[:maxHotspots]
	}

	res := profileResult{
		ExitCode:     out.ExitCode,
		AnalysisType: analysis,
		Summary:      out.Summary,
		TopHotspots:  top,
		ReportPath:   out.ReportPath,
		CommandLine:  out.CommandLine,
		Stderr:       out.Stderr,
		DurationMS:   durationMS,
		TimedOut:     out.TimedOut,
		Truncated: strings.Contains(out.ReportCSV, "…[truncated]") ||
			strings.Contains(out.Summary, "…[truncated]"),
		VTunePath: exePath,
	}
	if res.TopHotspots == nil {
		res.TopHotspots = []Hotspot{}
	}

	// Same swallowed-failure guard as profileTool: a re-report that produced no
	// CSV and no summary is surfaced as an explicit error rather than an
	// empty-looking success.
	if out.ReportPath == "" && strings.TrimSpace(out.Summary) == "" {
		res.Error = "vtune produced no report from result_dir (the result dir may be incomplete or from a different vtune version — check stderr / command_line)"
		if res.ExitCode == 0 {
			res.ExitCode = -1
		}
	}

	body, err := json.Marshal(res)
	if err != nil {
		return errResult(fmt.Sprintf("failed to marshal result: %v", err)), nil
	}
	return textResult(string(body)), nil
}

// textResult wraps text in a non-error CallToolResult. Mirrors the gdb helper.
func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}

// sortedAnalysisTypes returns the allowlisted analysis types in a stable
// sorted order, for the "must be one of …" error message.
func sortedAnalysisTypes() []string {
	out := make([]string, 0, len(knownAnalysisTypes))
	for t := range knownAnalysisTypes {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// structuredErrResult marshals a profileResult into a NON-IsError
// CallToolResult (a structured, parseable JSON body) so failure paths surface
// the cause in the same shape as a successful run instead of an opaque
// tool-level error or an empty result. The TopHotspots slice is normalized to
// [] so the JSON shape is stable. On the (near-impossible) marshal failure it
// falls back to a hand-built JSON string — still structured, still non-empty.
func structuredErrResult(res profileResult) *mcp.CallToolResult {
	if res.TopHotspots == nil {
		res.TopHotspots = []Hotspot{}
	}
	body, err := json.Marshal(res)
	if err != nil {
		body = []byte(`{"exit_code":-1,"top_hotspots":[],"error":"vtune: result marshal failed"}`)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}
}

func profileResultFromRun(run vtuneRunRecord) profileResult {
	res := profileResult{
		ExitCode: run.ExitCode, AnalysisType: run.Request.AnalysisType,
		Error: run.Error, VTunePath: run.Request.VTunePath, SchemaVersion: run.SchemaVersion,
		RunID: run.RunID, State: run.State, Phase: run.Phase, RequestDisposition: run.RequestDisposition,
		FailureID: run.FailureID, Reportable: run.Reportable, StopReason: run.StopReason,
		PhaseExitCodes: run.PhaseExitCodes, ReceiptSHA256: run.ReceiptSHA256, Quarantined: run.Quarantined,
	}
	if run.Reportable && run.ReceiptSHA256 != "" && run.Request.KeepResult {
		res.ResultDir = run.Request.ResultDir
	}
	if run.Output != nil {
		res.Summary, res.ReportPath, res.CommandLine, res.Stderr = run.Output.Summary, run.Output.ReportPath, run.Output.CommandLine, run.Output.Stderr
		res.TopHotspots = parseReport(run.Output.ReportCSV).Hotspots
		if len(res.TopHotspots) > maxHotspots {
			res.TopHotspots = res.TopHotspots[:maxHotspots]
		}
	}
	if res.TopHotspots == nil {
		res.TopHotspots = []Hotspot{}
	}
	return res
}

func failureIDFor(err error) string {
	switch {
	case errors.Is(err, errVTuneRunNotFound):
		return failureRunNotFound
	case errors.Is(err, errVTuneIdempotencyConflict):
		return failureIdempotencyConflict
	default:
		return failureResultNonReportable
	}
}

// errResult builds a tool-level error CallToolResult (IsError=true) with a
// single text message. Mirrors the drmemory/godbolt error-result helper.
func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

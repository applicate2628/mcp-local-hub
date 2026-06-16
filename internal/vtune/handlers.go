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
	CommandLine  string    `json:"command_line,omitempty"`
	Stderr       string    `json:"stderr,omitempty"`
	DurationMS   int64     `json:"duration_ms"`
	TimedOut     bool      `json:"timed_out"`
	Truncated    bool      `json:"truncated"`
	// Error carries the cause when the run could not produce findings (exe not
	// found, invalid analysis type, launch failure, panic, missing report).
	// Empty on a successful parse.
	Error string `json:"error,omitempty"`
	// VTunePath is the resolved vtune.exe path used for the run, so a failed
	// launch is traceable.
	VTunePath string `json:"vtune_path,omitempty"`
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
		Exe          string   `json:"exe"`
		Args         []string `json:"args"`
		Cwd          string   `json:"cwd"`
		AnalysisType string   `json:"analysis_type"`
		TimeoutSec   int      `json:"timeout_sec"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
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

	timeoutSec := args.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = defaultTimeoutSec
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	start := time.Now()
	out, err := vs.run(runCtx, exePath, args.Exe, args.Args, args.Cwd, analysis)
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

// errResult builds a tool-level error CallToolResult (IsError=true) with a
// single text message. Mirrors the drmemory/godbolt error-result helper.
func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

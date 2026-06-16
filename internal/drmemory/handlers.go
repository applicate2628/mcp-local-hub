package drmemory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// defaultTimeoutSec is the wall-clock cap applied when the caller omits
// timeout_sec. Dr. Memory instrumentation runs 10-50x slower than native,
// so 20 minutes is a conservative default for a non-trivial target.
const defaultTimeoutSec = 1200

// runResult is the structured payload returned to the MCP client. It is
// marshalled to JSON and placed in a single TextContent so clients that
// only render text still see the full report.
//
// The diagnostic fields (Error, DrMemoryPath, CommandLine) are populated on
// the FAILURE paths (drmemory.exe-not-found, launch failure, results.txt
// missing/unparseable, panic) so a swallowed first-run setup failure — the
// "drmemory_run returns EMPTY" symptom the live agent hit — is always
// visible. They are omitempty so a clean run's JSON is unchanged.
type runResult struct {
	ExitCode    int        `json:"exit_code"`
	ErrorCount  int        `json:"error_count"`
	LeakCount   int        `json:"leak_count"`
	Errors      []MemError `json:"errors"`
	Summary     string     `json:"summary"`
	ResultsPath string     `json:"results_path"`
	Stderr      string     `json:"stderr,omitempty"`
	DurationMS  int64      `json:"duration_ms"`
	Truncated   bool       `json:"truncated"`
	// Error carries the cause when the run could not produce findings (exe
	// not found, launch failure, panic, missing/unparseable results.txt).
	// Empty on a successful parse.
	Error string `json:"error,omitempty"`
	// DrMemoryPath is the resolved drmemory.exe path used for the run, so a
	// failed first-run (symbol setup, missing DynamoRIO bits) is traceable.
	DrMemoryPath string `json:"drmemory_path,omitempty"`
	// CommandLine is the exact drmemory.exe argv (joined) that was executed,
	// surfaced so an operator can reproduce a failing launch by hand.
	CommandLine string `json:"command_line,omitempty"`
}

// runTool is the drmemory_run handler. It resolves drmemory.exe via the
// findExe seam, runs the target under Dr. Memory via the run seam (with a
// timeout context), parses the resulting results.txt, and returns the
// structured findings. Both seams are injected so tests never run the
// real instrumented process.
func (ds *DrMemoryServer) runTool(ctx context.Context, req *mcp.CallToolRequest) (result *mcp.CallToolResult, retErr error) {
	// NEVER return empty and NEVER crash the daemon. A panic anywhere in the
	// handler (the "port went down" symptom the live agent saw) is recovered
	// here and turned into a structured error result, so drmemory_run always
	// returns a parseable answer the caller can diagnose.
	defer func() {
		if r := recover(); r != nil {
			result = structuredErrResult(runResult{
				ExitCode: -1,
				Error:    fmt.Sprintf("drmemory: internal panic: %v", r),
			})
			retErr = nil
		}
	}()

	var args struct {
		Exe                string   `json:"exe"`
		Args               []string `json:"args"`
		Cwd                string   `json:"cwd"`
		TimeoutSec         int      `json:"timeout_sec"`
		CheckUninitialized *bool    `json:"check_uninitialized"`
		Light              bool     `json:"light"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if strings.TrimSpace(args.Exe) == "" {
		return errResult("missing required parameter: exe (path to the target .exe)"), nil
	}

	// Resolve drmemory.exe via the injectable seam. On a not-found / probe
	// failure we surface a STRUCTURED result (never empty, never a bare
	// IsError text) carrying the install-guidance error so the caller can
	// see exactly what went wrong.
	exePath, err := ds.findExe()
	if err != nil {
		return structuredErrResult(runResult{
			ExitCode: -1,
			Error:    err.Error(),
		}), nil
	}

	// check_uninitialized defaults to true when the caller omits it.
	checkUninit := true
	if args.CheckUninitialized != nil {
		checkUninit = *args.CheckUninitialized
	}

	timeoutSec := args.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = defaultTimeoutSec
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	start := time.Now()
	out, err := ds.run(runCtx, exePath, args.Exe, args.Args, args.Cwd, args.Light, checkUninit)
	durationMS := time.Since(start).Milliseconds()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return structuredErrResult(runResult{
				ExitCode:     -1,
				Error:        fmt.Sprintf("drmemory run timed out after %d s (instrumentation is 10-50x slow — raise timeout_sec)", timeoutSec),
				DrMemoryPath: exePath,
				DurationMS:   durationMS,
			}), nil
		}
		// Genuine launch failure (drmemory.exe non-executable, DynamoRIO
		// first-run setup failing, etc.). Encode the error into a STRUCTURED
		// result and surface every diagnostic the runner captured (stderr +
		// the exact command line) so a swallowed first-run setup failure is
		// visible instead of returning empty.
		res := runResult{
			ExitCode:     -1,
			Error:        fmt.Sprintf("drmemory run failed: %v", err),
			DrMemoryPath: exePath,
			DurationMS:   durationMS,
		}
		if out != nil {
			res.Stderr = out.Stderr
			res.CommandLine = out.CommandLine
			if out.ExitCode != 0 {
				res.ExitCode = out.ExitCode
			}
		}
		return structuredErrResult(res), nil
	}

	parsed := parseResults(out.ResultsText)

	result = &mcp.CallToolResult{}
	res := runResult{
		ExitCode:     out.ExitCode,
		ErrorCount:   parsed.ErrorCount,
		LeakCount:    parsed.LeakCount,
		Errors:       parsed.Errors,
		Summary:      parsed.Summary,
		ResultsPath:  out.ResultsPath,
		Stderr:       out.Stderr,
		DurationMS:   durationMS,
		Truncated:    strings.Contains(out.ResultsText, "…[truncated]"),
		DrMemoryPath: exePath,
		CommandLine:  out.CommandLine,
	}
	if res.Errors == nil {
		res.Errors = []MemError{}
	}

	// A run that produced NO results.txt (out.ResultsPath == "") and NO
	// parsed summary is a swallowed failure mode: drmemory launched but never
	// wrote a report (missing DynamoRIO bits, symbol-setup failure on first
	// run). Surface it explicitly rather than returning an empty-looking
	// success — the exit code, stderr, and command line are already attached.
	if out.ResultsPath == "" && strings.TrimSpace(parsed.Summary) == "" {
		res.Error = "drmemory produced no results.txt (the instrumented run may have failed before writing a report — check stderr / exit_code / command_line; a first run may need DynamoRIO symbol setup)"
	}

	body, err := json.Marshal(res)
	if err != nil {
		return errResult(fmt.Sprintf("failed to marshal result: %v", err)), nil
	}

	result.Content = []mcp.Content{&mcp.TextContent{Text: string(body)}}
	return result, nil
}

// statusTool handles drmemory_status: it probes Dr. Memory availability via the
// injectable seam and returns {available, drmemory_path, version}. It runs only
// `<drmemory> -version` (no instrumented target), so it is safe to expose
// unconditionally. The Go-exec probe works in the console-less daemon.
func (ds *DrMemoryServer) statusTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, version, available := ds.probeVersion()
	body, _ := json.Marshal(map[string]any{
		"available":     available,
		"drmemory_path": path,
		"version":       version,
	})
	return textResult(string(body)), nil
}

// textResult wraps text in a non-error CallToolResult.
func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}

// structuredErrResult marshals a runResult into a NON-IsError CallToolResult
// (a structured, parseable JSON body) so failure paths surface the cause in
// the same shape as a successful run instead of an opaque tool-level error
// or an empty result. The Errors slice is normalized to [] so the JSON shape
// is stable. On the (near-impossible) marshal failure it falls back to a
// hand-built JSON string — still structured, still non-empty.
func structuredErrResult(res runResult) *mcp.CallToolResult {
	if res.Errors == nil {
		res.Errors = []MemError{}
	}
	body, err := json.Marshal(res)
	if err != nil {
		body = []byte(`{"exit_code":-1,"errors":[],"error":"drmemory: result marshal failed"}`)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}
}

// errResult builds a tool-level error CallToolResult (IsError=true) with a
// single text message. Mirrors the godbolt/perftools error-result helper.
func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

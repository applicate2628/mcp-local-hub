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
}

// runTool is the drmemory_run handler. It resolves drmemory.exe via the
// findExe seam, runs the target under Dr. Memory via the run seam (with a
// timeout context), parses the resulting results.txt, and returns the
// structured findings. Both seams are injected so tests never run the
// real instrumented process.
func (ds *DrMemoryServer) runTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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

	// Resolve drmemory.exe via the injectable seam. A clear,
	// install-guidance error surfaces when it can't be found.
	exePath, err := ds.findExe()
	if err != nil {
		return errResult(err.Error()), nil
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
			return errResult(fmt.Sprintf("drmemory run timed out after %d s (instrumentation is 10-50x slow — raise timeout_sec)", timeoutSec)), nil
		}
		return errResult(fmt.Sprintf("drmemory run failed: %v", err)), nil
	}

	parsed := parseResults(out.ResultsText)

	result := runResult{
		ExitCode:    out.ExitCode,
		ErrorCount:  parsed.ErrorCount,
		LeakCount:   parsed.LeakCount,
		Errors:      parsed.Errors,
		Summary:     parsed.Summary,
		ResultsPath: out.ResultsPath,
		Stderr:      out.Stderr,
		DurationMS:  durationMS,
		Truncated:   strings.Contains(out.ResultsText, "…[truncated]"),
	}
	if result.Errors == nil {
		result.Errors = []MemError{}
	}

	body, err := json.Marshal(result)
	if err != nil {
		return errResult(fmt.Sprintf("failed to marshal result: %v", err)), nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}, nil
}

// errResult builds a tool-level error CallToolResult (IsError=true) with a
// single text message. Mirrors the godbolt/perftools error-result helper.
func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

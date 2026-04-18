package perftools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// diagLineRE matches the canonical clang-tidy stderr line format:
//
//	path/to/file.cpp:LINE:COL: severity: message [check-name]
//
// Severity is one of warning / error / note. The check-name tail in
// square brackets is optional on note lines.
var diagLineRE = regexp.MustCompile(
	`^(?P<file>.+?):(?P<line>\d+):(?P<col>\d+):\s+(?P<sev>warning|error|note):\s+(?P<msg>.+?)(?:\s+\[(?P<check>[^\]]+)\])?$`)

// Diagnostic is the per-issue record returned in the tool's JSON body.
// Fields match clang-tidy's output; the shape is stable across clang-tidy
// versions so consumers can rely on it.
type Diagnostic struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	Severity string `json:"severity"`
	Check    string `json:"check,omitempty"`
	Message  string `json:"message"`
}

// clangTidyResult is the top-level JSON shape returned to the client.
type clangTidyResult struct {
	Diagnostics []Diagnostic `json:"diagnostics"`
	RawStderr   string       `json:"raw_stderr,omitempty"`
	ExitCode    int          `json:"exit_code"`
}

// clangTidyTool runs clang-tidy against a list of files, using the
// project's compile_commands.json for flag resolution. Returns a
// structured JSON diagnostics list so callers can filter by check,
// severity, or file without re-parsing text.
func (tb *PerfToolbox) clangTidyTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if !tb.tools.ClangTidy.Installed {
		return errResult("clang-tidy not installed: " + tb.tools.ClangTidy.Error), nil
	}

	var args struct {
		Files       []string `json:"files"`
		ProjectRoot string   `json:"project_root"`
		Checks      string   `json:"checks"`
		ExtraArgs   []string `json:"extra_args"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if len(args.Files) == 0 {
		return errResult("missing required parameter: files (non-empty list of source file paths)"), nil
	}
	if args.ProjectRoot == "" {
		return errResult("missing required parameter: project_root (directory containing compile_commands.json)"), nil
	}

	cmdArgs := []string{"-p", args.ProjectRoot}
	if args.Checks != "" {
		cmdArgs = append(cmdArgs, "--checks="+args.Checks)
	}
	cmdArgs = append(cmdArgs, args.ExtraArgs...)
	cmdArgs = append(cmdArgs, args.Files...)

	cap, err := runCapture(ctx, tb.tools.ClangTidy.Path, "", cmdArgs)
	if err != nil {
		return errResult(fmt.Sprintf("failed to run clang-tidy: %v", err)), nil
	}

	// clang-tidy writes diagnostics to stdout AND stderr depending on the
	// version; parse both so we catch everything.
	combined := string(cap.Stdout) + "\n" + string(cap.Stderr)
	diags := parseClangTidyOutput(combined)

	result := clangTidyResult{
		Diagnostics: diags,
		RawStderr:   string(cap.Stderr),
		ExitCode:    cap.ExitCode,
	}
	body, _ := json.MarshalIndent(result, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}, nil
}

// parseClangTidyOutput scans clang-tidy's output for the file:line:col
// diagnostic format and returns a typed list. Unmatched lines — banners,
// summaries, source-code snippets, carets — are ignored silently.
func parseClangTidyOutput(s string) []Diagnostic {
	var out []Diagnostic
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		m := diagLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		lineNum, _ := strconv.Atoi(m[diagLineRE.SubexpIndex("line")])
		colNum, _ := strconv.Atoi(m[diagLineRE.SubexpIndex("col")])
		out = append(out, Diagnostic{
			File:     m[diagLineRE.SubexpIndex("file")],
			Line:     lineNum,
			Column:   colNum,
			Severity: m[diagLineRE.SubexpIndex("sev")],
			Check:    m[diagLineRE.SubexpIndex("check")],
			Message:  m[diagLineRE.SubexpIndex("msg")],
		})
	}
	return out
}

// errResult is the shared error-return helper used by every tool in
// this package — keeps the error surface consistent so MCP clients see
// the same shape regardless of which tool failed.
func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

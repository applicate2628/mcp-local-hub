package perftools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// IWYUReport is the per-file parsed output of include-what-you-use.
// Each block the tool emits (delimited by "---") produces one report.
type IWYUReport struct {
	File     string   `json:"file"`
	Add      []string `json:"add"`
	Remove   []string `json:"remove"`
	FullList []string `json:"full_list"`
}

// iwyuStatus encodes the three-way outcome distinction that IWYU's
// raw output makes implicit. Callers can branch on this instead of
// inspecting raw_output to tell "IWYU had nothing to suggest" apart
// from "IWYU couldn't parse the file" — the earlier API conflated
// both into an empty reports[].
//
//	"ok"            IWYU ran, produced at least one parseable block
//	"no-suggestions" IWYU ran, exited 0, produced no blocks (rare but
//	                 happens when the file is already include-clean)
//	"env-failure"   IWYU couldn't find stdlib headers or a compile
//	                 target — its output starts with "fatal error"
//	                 and no blocks were produced. Usually means the
//	                 caller's extra_args don't match their toolchain.
type iwyuStatus string

const (
	iwyuStatusOK            iwyuStatus = "ok"
	iwyuStatusNoSuggestions iwyuStatus = "no-suggestions"
	iwyuStatusEnvFailure    iwyuStatus = "env-failure"
)

// iwyuResult is the top-level JSON shape returned to the client.
type iwyuResult struct {
	Status    iwyuStatus   `json:"status"`
	Reports   []IWYUReport `json:"reports"`
	RawOutput string       `json:"raw_output,omitempty"`
	ExitCode  int          `json:"exit_code"`
}

// iwyuTool runs include-what-you-use against one source file. IWYU is
// intentionally one-file-per-invocation — batching is the iwyu_tool.py
// wrapper's job, which we don't use here because parsing its output
// format is more brittle than running iwyu directly per file.
func (tb *PerfToolbox) iwyuTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if !tb.tools.IWYU.Installed {
		return errResult("include-what-you-use not installed: " + tb.tools.IWYU.Error), nil
	}

	var args struct {
		File        string   `json:"file"`
		ProjectRoot string   `json:"project_root"`
		ExtraArgs   []string `json:"extra_args"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.File == "" {
		return errResult("missing required parameter: file"), nil
	}

	cmdArgs := append([]string{}, args.ExtraArgs...)
	cmdArgs = append(cmdArgs, args.File)

	cap, err := runCapture(ctx, tb.tools.IWYU.Path, args.ProjectRoot, cmdArgs)
	if err != nil {
		return errResult(fmt.Sprintf("include-what-you-use failed: %v", err)), nil
	}

	// IWYU writes its suggestions to stderr in practice (historical quirk);
	// parse both streams to be robust across versions.
	combined := string(cap.Stderr) + "\n" + string(cap.Stdout)
	reports := parseIWYUOutput(combined)
	// Always emit reports[] as an array (never null) so callers can
	// rely on the JSON shape and len() unconditionally.
	if reports == nil {
		reports = []IWYUReport{}
	}

	result := iwyuResult{
		Status:    classifyIWYUStatus(reports, combined, cap.ExitCode),
		Reports:   reports,
		RawOutput: combined,
		ExitCode:  cap.ExitCode,
	}
	body, _ := json.MarshalIndent(result, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}, nil
}

// classifyIWYUStatus decides the three-way status based on the parsed
// reports and raw output. Precedence: env-failure (raw output has the
// signature fatal-error marker) > ok (at least one report) >
// no-suggestions (IWYU ran fine, had nothing to say).
func classifyIWYUStatus(reports []IWYUReport, rawOutput string, exitCode int) iwyuStatus {
	if len(reports) == 0 {
		// Heuristic: IWYU's env-failure paths always include a "fatal
		// error:" marker in the output before it bails. The typical
		// cases are "fatal error: 'mm_malloc.h' file not found" when
		// the bundled clang headers don't match the host stdlib, and
		// "fatal error: 'X' file not found" for user-code includes.
		if strings.Contains(rawOutput, "fatal error:") {
			return iwyuStatusEnvFailure
		}
		return iwyuStatusNoSuggestions
	}
	_ = exitCode // reserved for future signal if IWYU starts using it
	return iwyuStatusOK
}

// parseIWYUOutput walks IWYU's text output and extracts per-file
// suggestion blocks. The format is:
//
//	path/to/file.cpp should add these lines:
//	#include <x>
//
//	path/to/file.cpp should remove these lines:
//	- #include <y>  // lines 3-3
//
//	The full include-list for path/to/file.cpp:
//	#include <x>
//	---
//
// Section order is stable; `---` delimits a file block.
func parseIWYUOutput(s string) []IWYUReport {
	var reports []IWYUReport
	for block := range strings.SplitSeq(s, "---") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		r := parseIWYUBlock(block)
		if r.File != "" {
			reports = append(reports, r)
		}
	}
	return reports
}

// parseIWYUBlock walks one file's worth of IWYU output.
func parseIWYUBlock(block string) IWYUReport {
	var r IWYUReport
	section := "" // "add" | "remove" | "full"

	for line := range strings.SplitSeq(block, "\n") {
		line = strings.TrimRight(line, "\r")
		trim := strings.TrimSpace(line)

		switch {
		case strings.HasSuffix(trim, "should add these lines:"):
			r.File = strings.TrimSpace(strings.TrimSuffix(trim, "should add these lines:"))
			section = "add"
		case strings.HasSuffix(trim, "should remove these lines:"):
			section = "remove"
		case strings.HasPrefix(trim, "The full include-list for "):
			section = "full"
		case trim == "":
			// Blank lines end a section's content stream but not the block.
			section = ""
		default:
			switch section {
			case "add":
				r.Add = append(r.Add, trim)
			case "remove":
				// Remove lines are prefixed with "- " in IWYU output.
				r.Remove = append(r.Remove, strings.TrimPrefix(trim, "- "))
			case "full":
				r.FullList = append(r.FullList, trim)
			}
		}
	}
	return r
}

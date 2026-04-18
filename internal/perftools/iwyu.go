package perftools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
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

// iwyuResult is the top-level JSON shape returned to the client.
type iwyuResult struct {
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

	cmd := exec.CommandContext(ctx, tb.tools.IWYU.Path, cmdArgs...)
	if args.ProjectRoot != "" {
		cmd.Dir = args.ProjectRoot
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	exitCode := 0
	if ee, ok := runErr.(*exec.ExitError); ok {
		// IWYU's exit code convention: non-zero means "suggestions made"
		// — it is NOT an error condition. Keep the code but don't fail.
		exitCode = ee.ExitCode()
	} else if runErr != nil {
		return errResult(fmt.Sprintf("include-what-you-use failed: %v\nstderr:\n%s", runErr, stderr.String())), nil
	}

	// IWYU writes its suggestions to stderr in practice (historical quirk);
	// parse both streams to be robust across versions.
	combined := stderr.String() + "\n" + stdout.String()
	reports := parseIWYUOutput(combined)
	// Always emit reports[] as an array (never null) so callers can
	// rely on the JSON shape and len() unconditionally.
	if reports == nil {
		reports = []IWYUReport{}
	}

	result := iwyuResult{
		Reports:   reports,
		RawOutput: combined,
		ExitCode:  exitCode,
	}
	body, _ := json.MarshalIndent(result, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}, nil
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
	for _, block := range strings.Split(s, "---") {
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

	for _, line := range strings.Split(block, "\n") {
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

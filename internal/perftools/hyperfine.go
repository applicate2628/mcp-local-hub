package perftools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// hyperfineTool runs hyperfine against one or more commands and returns
// its --export-json output verbatim, wrapped in an MCP TextContent.
// Hyperfine's own JSON schema is stable and well-documented, so there's
// no benefit to re-marshalling through Go structs — we just pass it
// through and let the MCP client consume it.
func (tb *PerfToolbox) hyperfineTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if !tb.tools.Hyperfine.Installed {
		return errResult("hyperfine not installed: " + tb.tools.Hyperfine.Error), nil
	}

	var args struct {
		Commands  []string `json:"commands"`
		Warmup    int      `json:"warmup"`
		MinRuns   int      `json:"min_runs"`
		MaxRuns   int      `json:"max_runs"`
		ExtraArgs []string `json:"extra_args"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if len(args.Commands) == 0 {
		return errResult("missing required parameter: commands (non-empty list)"), nil
	}

	// hyperfine writes its JSON to a file, not stdout. Use a tempfile.
	tmp, err := os.CreateTemp("", "hyperfine-*.json")
	if err != nil {
		return errResult(fmt.Sprintf("create tempfile: %v", err)), nil
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)

	cmdArgs := []string{"--export-json", tmpPath}
	if args.Warmup > 0 {
		cmdArgs = append(cmdArgs, "--warmup", strconv.Itoa(args.Warmup))
	}
	if args.MinRuns > 0 {
		cmdArgs = append(cmdArgs, "--min-runs", strconv.Itoa(args.MinRuns))
	}
	if args.MaxRuns > 0 {
		cmdArgs = append(cmdArgs, "--max-runs", strconv.Itoa(args.MaxRuns))
	}
	cmdArgs = append(cmdArgs, args.ExtraArgs...)
	cmdArgs = append(cmdArgs, args.Commands...)

	cmd := exec.CommandContext(ctx, tb.tools.Hyperfine.Path, cmdArgs...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr // ignore human-readable output; we only need the JSON export
	if err := cmd.Run(); err != nil {
		return errResult(fmt.Sprintf("hyperfine failed: %v\nstderr:\n%s", err, stderr.String())), nil
	}

	body, err := os.ReadFile(tmpPath)
	if err != nil {
		return errResult(fmt.Sprintf("read hyperfine export-json: %v", err)), nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}, nil
}

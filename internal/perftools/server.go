package perftools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// PerfToolbox is the MCP server instance. tools is populated once at
// startup by DetectTools() so request dispatch is a cheap pointer read.
type PerfToolbox struct {
	server *mcp.Server
	tools  *ToolCatalog
}

// Run spins up the MCP server on stdio. Called from both the embedded
// mcphub subcommand and the standalone cmd/perftools binary.
func Run(ctx context.Context) error {
	tb := &PerfToolbox{tools: DetectTools()}

	tb.server = mcp.NewServer(&mcp.Implementation{
		Name:    "mcp-local-hub-perftools",
		Version: "1.0.0",
	}, nil)

	registerResources(tb)
	registerTools(tb)

	if err := tb.server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("perftools server run: %w", err)
	}
	return nil
}

// registerResources mounts the discovery resource.
func registerResources(tb *PerfToolbox) {
	tb.server.AddResource(&mcp.Resource{
		URI:         "resource://tools",
		Name:        "tools",
		Description: "Catalog of detected perf-analysis tools with versions (clang-tidy, hyperfine, llvm-objdump, include-what-you-use).",
		MIMEType:    "application/json",
	}, tb.getToolsResource)
}

// registerTools mounts tool handlers. Handlers themselves are defined
// in their respective files; this function is the single registration
// point so Tasks 2-5 each add one AddTool call here.
func registerTools(tb *PerfToolbox) {
	tb.server.AddTool(&mcp.Tool{
		Name: "clang_tidy",
		Description: "Run clang-tidy against source files using the project's compile_commands.json. " +
			"Returns structured JSON with diagnostics[] (file, line, column, severity, check, message), " +
			"plus raw_stderr and exit_code. Requires compile_commands.json in project_root — generate via CMake " +
			"(-DCMAKE_EXPORT_COMPILE_COMMANDS=ON) or bear/compiledb for Make-based builds. " +
			"Common checks preset: performance-*,bugprone-*,readability-*.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"files": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "List of source file paths to analyze (absolute or relative to project_root).",
				},
				"project_root": map[string]interface{}{
					"type":        "string",
					"description": "Directory containing compile_commands.json. clang-tidy resolves flags per-file from this DB.",
				},
				"checks": map[string]interface{}{
					"type":        "string",
					"description": "Optional. Comma-separated check pattern (e.g. 'performance-*,bugprone-*'). Omit to use .clang-tidy config or default checks.",
				},
				"extra_args": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "Optional. Additional raw arguments passed verbatim before file list (e.g. ['--header-filter=.*', '--quiet']).",
				},
			},
			"required": []string{"files", "project_root"},
		},
	}, tb.clangTidyTool)

	tb.server.AddTool(&mcp.Tool{
		Name: "hyperfine",
		Description: "Run a statistical benchmark against one or more shell commands via hyperfine. " +
			"Returns JSON with results[] (one per command) containing mean, stddev, median, min, max, user, " +
			"system, and raw times[] in seconds. For N>=2 commands hyperfine also computes pairwise ratios " +
			"in its own output. Use warmup to stabilize caches (recommended 1-3) and min_runs/max_runs to " +
			"control the statistical budget.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"commands": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "Shell commands to benchmark. Pass 2+ to enable comparative ratios.",
				},
				"warmup": map[string]interface{}{
					"type":        "integer",
					"description": "Number of warmup runs per command before measurement. Optional (default 0).",
				},
				"min_runs": map[string]interface{}{
					"type":        "integer",
					"description": "Minimum runs per command. Optional (default hyperfine default = 10).",
				},
				"max_runs": map[string]interface{}{
					"type":        "integer",
					"description": "Maximum runs per command. Optional.",
				},
				"extra_args": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "Optional. Additional raw hyperfine arguments (e.g. ['--prepare', 'sync']).",
				},
			},
			"required": []string{"commands"},
		},
	}, tb.hyperfineTool)

	tb.server.AddTool(&mcp.Tool{
		Name: "llvm_objdump",
		Description: "Disassemble the user's REAL built binary (post-LTO, post-PGO, post-linker-inlining) via " +
			"llvm-objdump. Unlike godbolt's sandbox compile, this is the authoritative answer to 'what " +
			"instructions are actually in my .exe?'. Returns raw disassembly text. Use the function " +
			"parameter to limit output to a specific symbol (heavily recommended — full .text disassembly " +
			"of a real binary is typically megabytes).",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"binary": map[string]interface{}{
					"type":        "string",
					"description": "Path to the binary file (executable, object, shared lib, or archive).",
				},
				"function": map[string]interface{}{
					"type":        "string",
					"description": "Optional. Limit disassembly to this symbol name (demangled). Highly recommended for large binaries.",
				},
				"section": map[string]interface{}{
					"type":        "string",
					"description": "Optional. Limit disassembly to a named section (e.g. '.text', '.text.startup').",
				},
				"with_source": map[string]interface{}{
					"type":        "boolean",
					"description": "Optional. Interleave source lines with asm (requires binary built with -g). Default false.",
				},
				"intel": map[string]interface{}{
					"type":        "boolean",
					"description": "Optional. Use Intel asm syntax instead of AT&T. Default false (AT&T).",
				},
				"extra_args": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "Optional. Additional raw llvm-objdump args (e.g. ['--no-show-raw-insn']).",
				},
			},
			"required": []string{"binary"},
		},
	}, tb.llvmObjdumpTool)

	tb.server.AddTool(&mcp.Tool{
		Name: "iwyu",
		Description: "Run include-what-you-use on a source file. Returns structured JSON with reports[] — one entry " +
			"per file in the output — each carrying add[], remove[], and full_list[] include suggestions. Plus " +
			"raw_output for unparsed inspection. Unlike clang-tidy's include cleaner, IWYU does whole-transitive " +
			"analysis: 'header X forward-declares Y; you use Y; so you should include Y's definition directly'.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"file": map[string]interface{}{
					"type":        "string",
					"description": "Source file path to analyze.",
				},
				"project_root": map[string]interface{}{
					"type":        "string",
					"description": "Optional. Working directory for iwyu; relative #include paths resolve from here.",
				},
				"extra_args": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "Optional. Additional raw iwyu args (e.g. ['-std=c++17', '-Iinclude']).",
				},
			},
			"required": []string{"file"},
		},
	}, tb.iwyuTool)
}

// getToolsResource serves resource://tools — marshals the catalog to
// JSON and returns it as a single TextResourceContents.
func (tb *PerfToolbox) getToolsResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	body, err := json.MarshalIndent(tb.tools.AsMap(), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal tool catalog: %w", err)
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{
			{URI: req.Params.URI, MIMEType: "application/json", Text: string(body)},
		},
	}, nil
}

package perftools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// hyperfineOptInEnv gates tool registration for the `hyperfine` benchmark.
// It executes client-supplied shell commands — functional exactly as intended,
// but that same surface is a remote-code-execution vector for any MCP client
// that can reach the perftools server. Admin opts in with "1" to enable it;
// any other value (including absent) leaves the tool unregistered so the
// server refuses the tools/call with method-not-found.
const hyperfineOptInEnv = "MCP_LOCAL_HUB_ENABLE_UNSAFE_HYPERFINE"

// hyperfineEnabled reports whether the admin opted into exposing the
// hyperfine tool. Extracted from the registerTools branch so it is
// easy to unit-test without wiring a real MCP server.
func hyperfineEnabled() bool {
	return os.Getenv(hyperfineOptInEnv) == "1"
}

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

// registerResources mounts the discovery resource and the workflow guide.
func registerResources(tb *PerfToolbox) {
	tb.server.AddResource(&mcp.Resource{
		URI:         "resource://tools",
		Name:        "tools",
		Description: "Catalog of detected perf-analysis tools with versions (clang-tidy, hyperfine, llvm-objdump, include-what-you-use). Check this first before calling any tool — not all four may be installed on every host.",
		MIMEType:    "application/json",
	}, tb.getToolsResource)

	tb.server.AddResource(&mcp.Resource{
		URI:         "resource://workflow",
		Name:        "workflow",
		Description: "Workflow guide: when to use which tool, canonical perf-review patterns (clang_tidy → godbolt verify → hyperfine measure → llvm_objdump confirm), anti-patterns, and cross-server handoffs to godbolt. Read this first when orienting to the perftools MCP surface.",
		MIMEType:    "text/markdown",
	}, tb.getWorkflow)
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
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"files": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "List of source file paths to analyze (absolute or relative to project_root).",
				},
				"project_root": map[string]any{
					"type":        "string",
					"description": "Directory containing compile_commands.json. clang-tidy resolves flags per-file from this DB.",
				},
				"checks": map[string]any{
					"type":        "string",
					"description": "Optional. Comma-separated check pattern (e.g. 'performance-*,bugprone-*'). Omit to use .clang-tidy config or default checks.",
				},
				"extra_args": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional. Additional raw arguments passed verbatim before file list (e.g. ['--header-filter=.*', '--quiet']).",
				},
			},
			"required": []string{"files", "project_root"},
		},
	}, tb.clangTidyTool)

	// hyperfine registers client-supplied shell commands for execution,
	// so it is gated behind an explicit opt-in env var. Missing or not
	// "1" → tool unregistered, and tools/call returns method-not-found
	// from the MCP SDK layer as if the tool never existed.
	if hyperfineEnabled() {
		tb.server.AddTool(&mcp.Tool{
			Name: "hyperfine",
			Description: "Run a statistical benchmark against one or more shell commands via hyperfine. " +
				"Returns JSON with results[] (one per command) containing mean, stddev, median, min, max, user, " +
				"system, and raw times[] in seconds. For N>=2 commands hyperfine also computes pairwise ratios " +
				"in its own output. Use warmup to stabilize caches (recommended 1-3) and min_runs/max_runs to " +
				"control the statistical budget.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"commands": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Shell commands to benchmark. Pass 2+ to enable comparative ratios.",
					},
					"warmup": map[string]any{
						"type":        "integer",
						"description": "Number of warmup runs per command before measurement. Optional (default 0).",
					},
					"min_runs": map[string]any{
						"type":        "integer",
						"description": "Minimum runs per command. Optional (default hyperfine default = 10).",
					},
					"max_runs": map[string]any{
						"type":        "integer",
						"description": "Maximum runs per command. Optional.",
					},
					"extra_args": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Optional. Additional raw hyperfine arguments (e.g. ['--prepare', 'sync']).",
					},
				},
				"required": []string{"commands"},
			},
		}, tb.hyperfineTool)
	}

	tb.server.AddTool(&mcp.Tool{
		Name: "llvm_objdump",
		Description: "Disassemble the user's REAL built binary (post-LTO, post-PGO, post-linker-inlining) via " +
			"llvm-objdump. Unlike godbolt's sandbox compile, this is the authoritative answer to 'what " +
			"instructions are actually in my .exe?'. Returns raw disassembly text. Use the function " +
			"parameter to limit output to a specific symbol (heavily recommended — full .text disassembly " +
			"of a real binary is typically megabytes).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"binary": map[string]any{
					"type":        "string",
					"description": "Path to the binary file (executable, object, shared lib, or archive).",
				},
				"function": map[string]any{
					"type":        "string",
					"description": "Optional. Limit disassembly to this symbol name (demangled). Highly recommended for large binaries.",
				},
				"section": map[string]any{
					"type":        "string",
					"description": "Optional. Limit disassembly to a named section (e.g. '.text', '.text.startup').",
				},
				"with_source": map[string]any{
					"type":        "boolean",
					"description": "Optional. Interleave source lines with asm (requires binary built with -g). Default false.",
				},
				"intel": map[string]any{
					"type":        "boolean",
					"description": "Optional. Use Intel asm syntax instead of AT&T. Default false (AT&T).",
				},
				"extra_args": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
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
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file": map[string]any{
					"type":        "string",
					"description": "Source file path to analyze.",
				},
				"project_root": map[string]any{
					"type":        "string",
					"description": "Optional. Working directory for iwyu; relative #include paths resolve from here.",
				},
				"extra_args": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional. Additional raw iwyu args (e.g. ['-std=c++17', '-Iinclude']).",
				},
			},
			"required": []string{"file"},
		},
	}, tb.iwyuTool)

	// Workflow tool mirrors resource://workflow — exposed as a tool so
	// MCP clients that don't surface resources (e.g. Claude Code CLI's
	// agent view) can still fetch the ecosystem-context document.
	tb.server.AddTool(&mcp.Tool{
		Name:        "workflow",
		Description: "Returns the perftools MCP ecosystem-context document in markdown: when to use which tool (clang_tidy vs hyperfine vs llvm_objdump vs iwyu), canonical perf-review chain, tool-parameter patterns, anti-patterns, and cross-server handoffs to godbolt. Call this FIRST when orienting to the perftools MCP — tool descriptions alone cover 'what', this covers 'when and how to combine'.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, tb.workflowTool)

	// list_tools mirrors resource://tools — the availability catalog
	// (clang-tidy, hyperfine, llvm-objdump, iwyu with installed/version/
	// path). Call this FIRST before any perf-analysis tool to see which
	// binaries this host actually has.
	tb.server.AddTool(&mcp.Tool{
		Name:        "list_tools",
		Description: "Return the catalog of detected perf-analysis binaries (clang-tidy, hyperfine, llvm-objdump, include-what-you-use). Each entry carries installed (bool), version (string when installed), and path (or error). Same content as resource://tools. Call this FIRST — not every host has all four installed.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, tb.listToolsTool)
}

// listToolsTool is the tool-transport twin of getToolsResource. Same
// JSON body, returned as a TextContent CallToolResult instead of a
// ResourceContents ReadResourceResult.
func (tb *PerfToolbox) listToolsTool(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resReq := &mcp.ReadResourceRequest{Params: &mcp.ReadResourceParams{URI: "resource://tools"}}
	result, err := tb.getToolsResource(ctx, resReq)
	if err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
		}, nil
	}
	content := make([]mcp.Content, 0, len(result.Contents))
	for _, c := range result.Contents {
		if c == nil {
			continue
		}
		content = append(content, &mcp.TextContent{Text: c.Text})
	}
	return &mcp.CallToolResult{Content: content}, nil
}

// getToolsResource serves resource://tools — marshals the catalog to
// JSON and returns it as a single TextResourceContents. Honors the
// hyperfine opt-in gate: when unset, hyperfine is stripped from the
// advertised catalog so the "check tools first, then call" contract
// stays "callable if advertised" — not "maybe callable".
func (tb *PerfToolbox) getToolsResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	body, err := json.MarshalIndent(tb.availableToolsMap(), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal tool catalog: %w", err)
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{
			{URI: req.Params.URI, MIMEType: "application/json", Text: string(body)},
		},
	}, nil
}

// availableToolsMap returns tb.tools.AsMap() with tools filtered out when
// the corresponding registration gate is closed. Today that only affects
// hyperfine, but centralizing the filter keeps discovery consistent with
// the actual AddTool branches in registerTools.
func (tb *PerfToolbox) availableToolsMap() map[string]*ToolInfo {
	m := tb.tools.AsMap()
	if !hyperfineEnabled() {
		delete(m, "hyperfine")
	}
	return m
}

package godbolt

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Claude Code CLI surfaces MCP resources to the human user through a
// picker UI but does not inject them into the agent's tool context. The
// list-style resources in this package carry discovery data the agent
// needs (available compilers, libraries, formatters, popular flags,
// instruction docs) — without a tool wrapper, the agent cannot reach
// them. These wrappers are thin delegators to the existing resource
// handlers so we stay single-sourced on the HTTP plumbing.

// resourceResultToToolResult converts a ReadResourceResult into a
// CallToolResult by flattening each text content into a TextContent.
// Non-text content kinds from the resource layer are skipped — the
// godbolt resources currently only return text.
func resourceResultToToolResult(result *mcp.ReadResourceResult) *mcp.CallToolResult {
	content := make([]mcp.Content, 0, len(result.Contents))
	for _, c := range result.Contents {
		if c == nil {
			continue
		}
		content = append(content, &mcp.TextContent{Text: c.Text})
	}
	return &mcp.CallToolResult{Content: content}
}

func toolErrorResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}
}

// registerResourceTools adds tool-transport twins for the list-style
// resources. Called from registerTools after the write-capable tools
// are registered, so the tool-list ordering stays: primary actions
// first (compile/format), then the workflow orienter, then discovery
// helpers.
func registerResourceTools(gs *GodboltServer) {
	gs.server.AddTool(&mcp.Tool{
		Name:        "list_languages",
		Description: "Return the list of languages supported by godbolt.org. Same content as resource://languages. Use to discover valid language ids before calling list_compilers(language_id) or list_libraries(language_id). Response is JSON.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, gs.listLanguagesTool)

	gs.server.AddTool(&mcp.Tool{
		Name:        "list_compilers",
		Description: "Return the compilers godbolt.org offers for a language. Same content as resource://compilers/{language_id}. Each entry carries id, name, lang, compilerType, semver, and — critically — a tools[] field listing which godbolt-hosted analyzers (llvm-mca, pahole, readelf, etc.) can run against this compiler's output. Response is JSON.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"language_id": map[string]any{
					"type":        "string",
					"description": "Language id (e.g. 'c++', 'rust', 'go'). Discover via list_languages.",
				},
			},
			"required": []string{"language_id"},
		},
	}, gs.listCompilersTool)

	gs.server.AddTool(&mcp.Tool{
		Name:        "list_libraries",
		Description: "Return godbolt-hosted libraries available for a language. Same content as resource://libraries/{language_id}. Each entry carries id, name, url, and versions[] with version ids usable in compile_code's libraries[] parameter. Response is JSON.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"language_id": map[string]any{
					"type":        "string",
					"description": "Language id (e.g. 'c++', 'rust', 'go'). Discover via list_languages.",
				},
			},
			"required": []string{"language_id"},
		},
	}, gs.listLibrariesTool)

	gs.server.AddTool(&mcp.Tool{
		Name:        "list_formats",
		Description: "Return godbolt-hosted code formatters. Same content as resource://formats. Each entry carries id (usable as format_code's formatter parameter), name, exe, and list of supported styles. Response is JSON.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, gs.listFormatsTool)

	gs.server.AddTool(&mcp.Tool{
		Name:        "list_popular_arguments",
		Description: "Return godbolt's curated list of popular flag combinations for a specific compiler. Same content as resource://popularArguments/{compiler_id}. Each entry is a {description, timesused} pair keyed by the flag string. Most useful when user_arguments is unfamiliar territory (nvcc, icpx, embedded cross-compilers). Response is JSON.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"compiler_id": map[string]any{
					"type":        "string",
					"description": "Compiler id (e.g. 'gcc132', 'clang1701'). Discover via list_compilers(language_id).",
				},
			},
			"required": []string{"compiler_id"},
		},
	}, gs.listPopularArgumentsTool)

	gs.server.AddTool(&mcp.Tool{
		Name:        "lookup_instruction",
		Description: "Return godbolt's documentation for a specific assembly opcode. Same content as resource://asm/{instruction_set}/{opcode}. Useful when reading compile_code asm output and needing to understand an unfamiliar mnemonic (vpaddd, bzhi, pext, etc.). Response is plain text.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"instruction_set": map[string]any{
					"type":        "string",
					"description": "Instruction-set family (e.g. 'amd64', 'arm', 'riscv', 'avr').",
				},
				"opcode": map[string]any{
					"type":        "string",
					"description": "Instruction mnemonic (e.g. 'vpaddd', 'mov', 'fma').",
				},
			},
			"required": []string{"instruction_set", "opcode"},
		},
	}, gs.lookupInstructionTool)

	gs.server.AddTool(&mcp.Tool{
		Name:        "get_version",
		Description: "Return version information for the Compiler Explorer instance this server proxies. Same content as resource://version. Plain text.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, gs.getVersionTool)
}

func (gs *GodboltServer) listLanguagesTool(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resReq := &mcp.ReadResourceRequest{Params: &mcp.ReadResourceParams{URI: "resource://languages"}}
	result, err := gs.getLanguages(ctx, resReq)
	if err != nil {
		return toolErrorResult(err), nil
	}
	return resourceResultToToolResult(result), nil
}

func (gs *GodboltServer) listCompilersTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		LanguageID string `json:"language_id"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolErrorResult(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	if args.LanguageID == "" {
		return toolErrorResult(fmt.Errorf("missing required parameter: language_id")), nil
	}
	resReq := &mcp.ReadResourceRequest{
		Params: &mcp.ReadResourceParams{URI: "resource://compilers/" + args.LanguageID},
	}
	result, err := gs.getCompilers(ctx, resReq)
	if err != nil {
		return toolErrorResult(err), nil
	}
	return resourceResultToToolResult(result), nil
}

func (gs *GodboltServer) listLibrariesTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		LanguageID string `json:"language_id"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolErrorResult(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	if args.LanguageID == "" {
		return toolErrorResult(fmt.Errorf("missing required parameter: language_id")), nil
	}
	resReq := &mcp.ReadResourceRequest{
		Params: &mcp.ReadResourceParams{URI: "resource://libraries/" + args.LanguageID},
	}
	result, err := gs.getLibraries(ctx, resReq)
	if err != nil {
		return toolErrorResult(err), nil
	}
	return resourceResultToToolResult(result), nil
}

func (gs *GodboltServer) listFormatsTool(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resReq := &mcp.ReadResourceRequest{Params: &mcp.ReadResourceParams{URI: "resource://formats"}}
	result, err := gs.getFormatters(ctx, resReq)
	if err != nil {
		return toolErrorResult(err), nil
	}
	return resourceResultToToolResult(result), nil
}

func (gs *GodboltServer) listPopularArgumentsTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		CompilerID string `json:"compiler_id"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolErrorResult(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	if args.CompilerID == "" {
		return toolErrorResult(fmt.Errorf("missing required parameter: compiler_id")), nil
	}
	resReq := &mcp.ReadResourceRequest{
		Params: &mcp.ReadResourceParams{URI: "resource://popularArguments/" + args.CompilerID},
	}
	result, err := gs.getPopularArguments(ctx, resReq)
	if err != nil {
		return toolErrorResult(err), nil
	}
	return resourceResultToToolResult(result), nil
}

func (gs *GodboltServer) lookupInstructionTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		InstructionSet string `json:"instruction_set"`
		Opcode         string `json:"opcode"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolErrorResult(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	if args.InstructionSet == "" || args.Opcode == "" {
		return toolErrorResult(fmt.Errorf("missing required parameters: instruction_set and opcode")), nil
	}
	resReq := &mcp.ReadResourceRequest{
		Params: &mcp.ReadResourceParams{URI: "resource://asm/" + args.InstructionSet + "/" + args.Opcode},
	}
	result, err := gs.getInstructionInfo(ctx, resReq)
	if err != nil {
		return toolErrorResult(err), nil
	}
	return resourceResultToToolResult(result), nil
}

func (gs *GodboltServer) getVersionTool(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resReq := &mcp.ReadResourceRequest{Params: &mcp.ReadResourceParams{URI: "resource://version"}}
	result, err := gs.getVersion(ctx, resReq)
	if err != nil {
		return toolErrorResult(err), nil
	}
	return resourceResultToToolResult(result), nil
}

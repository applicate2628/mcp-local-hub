package godbolt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerResources attaches the six read-only resources (five HTTP GET
// endpoints plus one static version resource) to the MCP server. Called
// once from Run during startup.
func registerResources(gs *GodboltServer) {
	gs.server.AddResource(&mcp.Resource{
		URI:         "resource://languages",
		Name:        "languages",
		Description: "Get a list of currently supported languages from Godbolt Compiler Explorer.",
	}, gs.getLanguages)

	gs.server.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "resource://compilers/{language_id}",
		Name:        "compilers",
		Description: "Get a list of compilers available for a specific language.",
	}, gs.getCompilers)

	gs.server.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "resource://libraries/{language_id}",
		Name:        "libraries",
		Description: "Get available libraries and versions for a specific language.",
	}, gs.getLibraries)

	gs.server.AddResource(&mcp.Resource{
		URI:         "resource://formats",
		Name:        "formats",
		Description: "Get a list of available code formatters.",
	}, gs.getFormatters)

	gs.server.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "resource://asm/{instruction_set}/{opcode}",
		Name:        "instruction_info",
		Description: "Get documentation for a specific assembly instruction.",
	}, gs.getInstructionInfo)

	gs.server.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "resource://popularArguments/{compiler_id}",
		Name:        "popularArguments",
		Description: "Popular flag combinations for a specific compiler (discoverability for unfamiliar toolchains).",
		MIMEType:    "application/json",
	}, gs.getPopularArguments)

	gs.server.AddResource(&mcp.Resource{
		URI:         "resource://version",
		Name:        "version",
		Description: "Get the version information of the Compiler Explorer instance.",
	}, gs.getVersion)
}

// registerTools attaches the three write-capable tools (compile_code,
// compile_cmake, format_code) to the MCP server. Called once from Run
// during startup.
func registerTools(gs *GodboltServer) {
	// Tool 1: compile_code
	gs.server.AddTool(&mcp.Tool{
		Name:        "compile_code",
		Description: "Compile source code via godbolt.org. Returns structured JSON with asm[], stdout[], stderr[], optional execResult (if filters.execute=true), optional optOutput[] (if filters.optOutput=true), and optional tools[] output (if tools[] is non-empty — e.g. llvm-mca throughput tables, pahole struct layout). Use filters to control asm syntax and enable execute/optOutput. Use execute_parameters for stdin/args of the executed binary. Use tools for llvm-mca / pahole / other godbolt-hosted analyzers that operate on the compile result.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"compiler_id": map[string]any{
					"type":        "string",
					"description": "The compiler ID (e.g., 'gcc11')",
				},
				"source": map[string]any{
					"type":        "string",
					"description": "The source code to compile",
				},
				"user_arguments": map[string]any{
					"type":        "string",
					"description": "Optional compiler flags and options",
				},
				"files": map[string]any{
					"type":        "array",
					"description": "Optional additional source files",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"filename": map[string]any{"type": "string"},
							"contents": map[string]any{"type": "string"},
						},
					},
				},
				"libraries": map[string]any{
					"type":        "array",
					"description": "Optional libraries to link",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":      map[string]any{"type": "string"},
							"version": map[string]any{"type": "string"},
						},
					},
				},
				"filters": map[string]any{
					"type":                 "object",
					"description":          "godbolt.org filters object (optional). Supported keys: execute (run binary and return stdout/stderr/exit), optOutput (include LLVM optimization remarks in response), intel (Intel asm syntax vs AT&T), labels, directives, commentOnly, demangle, libraryCode, trim, binary, binaryObject. Values are booleans.",
					"additionalProperties": true,
				},
				"execute_parameters": map[string]any{
					"type":        "object",
					"description": "Parameters passed to the binary when filters.execute=true. Optional keys: stdin (string piped to the process), args (array of argv strings).",
					"properties": map[string]any{
						"stdin": map[string]any{"type": "string"},
						"args":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
				},
				"tools": map[string]any{
					"type":        "array",
					"description": "Run godbolt-hosted tools against the compile result. Most useful for perf work: [{\"id\":\"llvm-mcatrunk\",\"args\":\"-mcpu=skylake -timeline\"}] for cycle-accurate throughput/port-pressure analysis of the generated asm, [{\"id\":\"pahole\",\"args\":\"\"}] for struct layout / padding / cacheline audit. Not the place for clang-tidy / cppcheck / iwyu — those should wrap local binaries against your real compile_commands.json in a separate MCP. List available tools for a compiler via godbolt's /api/compilers/{language} entries (each has a tools[] field).",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":   map[string]any{"type": "string"},
							"args": map[string]any{"type": "string"},
						},
					},
				},
			},
			"required": []string{"compiler_id", "source"},
		},
	}, gs.compileTool)

	// Tool 2: compile_cmake
	gs.server.AddTool(&mcp.Tool{
		Name:        "compile_cmake",
		Description: "Compile a CMake project via godbolt.org. Returns structured JSON with asm[], stdout[], stderr[], optional execResult (if filters.execute=true), optional optOutput[] (if filters.optOutput=true), and optional tools[] output (if tools[] is non-empty — e.g. llvm-mca throughput tables, pahole struct layout). Use filters to control asm syntax and enable execute/optOutput. Use execute_parameters for stdin/args of the executed binary. Use tools for llvm-mca / pahole / other godbolt-hosted analyzers that operate on the compile result.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"compiler_id": map[string]any{
					"type":        "string",
					"description": "The compiler ID (e.g., 'gcc11')",
				},
				"source": map[string]any{
					"type":        "string",
					"description": "The CMakeLists.txt content or main source file",
				},
				"user_arguments": map[string]any{
					"type":        "string",
					"description": "Optional CMake or compiler flags",
				},
				"files": map[string]any{
					"type":        "array",
					"description": "Optional additional source files",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"filename": map[string]any{"type": "string"},
							"contents": map[string]any{"type": "string"},
						},
					},
				},
				"libraries": map[string]any{
					"type":        "array",
					"description": "Optional libraries to link",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":      map[string]any{"type": "string"},
							"version": map[string]any{"type": "string"},
						},
					},
				},
				"filters": map[string]any{
					"type":                 "object",
					"description":          "godbolt.org filters object (optional). Supported keys: execute (run binary and return stdout/stderr/exit), optOutput (include LLVM optimization remarks in response), intel (Intel asm syntax vs AT&T), labels, directives, commentOnly, demangle, libraryCode, trim, binary, binaryObject. Values are booleans.",
					"additionalProperties": true,
				},
				"execute_parameters": map[string]any{
					"type":        "object",
					"description": "Parameters passed to the binary when filters.execute=true. Optional keys: stdin (string piped to the process), args (array of argv strings).",
					"properties": map[string]any{
						"stdin": map[string]any{"type": "string"},
						"args":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
				},
				"tools": map[string]any{
					"type":        "array",
					"description": "Run godbolt-hosted tools against the compile result. Most useful for perf work: [{\"id\":\"llvm-mcatrunk\",\"args\":\"-mcpu=skylake -timeline\"}] for cycle-accurate throughput/port-pressure analysis of the generated asm, [{\"id\":\"pahole\",\"args\":\"\"}] for struct layout / padding / cacheline audit. Not the place for clang-tidy / cppcheck / iwyu — those should wrap local binaries against your real compile_commands.json in a separate MCP. List available tools for a compiler via godbolt's /api/compilers/{language} entries (each has a tools[] field).",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":   map[string]any{"type": "string"},
							"args": map[string]any{"type": "string"},
						},
					},
				},
			},
			"required": []string{"compiler_id", "source"},
		},
	}, gs.compileCMakeTool)

	// Tool 3: format_code
	gs.server.AddTool(&mcp.Tool{
		Name:        "format_code",
		Description: "Format source code using a specified code formatter.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"formatter": map[string]any{
					"type":        "string",
					"description": "The formatter ID (e.g., 'clangformat', 'rustfmt')",
				},
				"source": map[string]any{
					"type":        "string",
					"description": "The source code to format",
				},
			},
			"required": []string{"formatter", "source"},
		},
	}, gs.formatTool)
}

// getLanguages handles GET /api/languages
func (gs *GodboltServer) getLanguages(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	body, err := gs.fetchResource(gs.baseURL + "/languages")
	if err != nil {
		return nil, fmt.Errorf("failed to get languages: %w", err)
	}

	var languages any
	if err := json.Unmarshal(body, &languages); err != nil {
		return nil, fmt.Errorf("failed to unmarshal languages: %w", err)
	}

	result, _ := json.Marshal(languages)
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{
			{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     string(result),
			},
		},
	}, nil
}

// getCompilers handles GET /api/compilers/{language_id}
func (gs *GodboltServer) getCompilers(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	languageID := extractPathParam(req.Params.URI, "language_id")
	if languageID == "" {
		return nil, fmt.Errorf("missing language_id parameter")
	}

	body, err := gs.fetchResource(fmt.Sprintf("%s/compilers/%s", gs.baseURL, languageID))
	if err != nil {
		return nil, fmt.Errorf("failed to get compilers: %w", err)
	}

	var compilers any
	if err := json.Unmarshal(body, &compilers); err != nil {
		return nil, fmt.Errorf("failed to unmarshal compilers: %w", err)
	}

	result, _ := json.Marshal(compilers)
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{
			{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     string(result),
			},
		},
	}, nil
}

// getLibraries handles GET /api/libraries/{language_id}
func (gs *GodboltServer) getLibraries(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	languageID := extractPathParam(req.Params.URI, "language_id")
	if languageID == "" {
		return nil, fmt.Errorf("missing language_id parameter")
	}

	body, err := gs.fetchResource(fmt.Sprintf("%s/libraries/%s", gs.baseURL, languageID))
	if err != nil {
		return nil, fmt.Errorf("failed to get libraries: %w", err)
	}

	var libraries any
	if err := json.Unmarshal(body, &libraries); err != nil {
		return nil, fmt.Errorf("failed to unmarshal libraries: %w", err)
	}

	result, _ := json.Marshal(libraries)
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{
			{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     string(result),
			},
		},
	}, nil
}

// getFormatters handles GET /api/formats
func (gs *GodboltServer) getFormatters(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	body, err := gs.fetchResource(gs.baseURL + "/formats")
	if err != nil {
		return nil, fmt.Errorf("failed to get formatters: %w", err)
	}

	var formatters any
	if err := json.Unmarshal(body, &formatters); err != nil {
		return nil, fmt.Errorf("failed to unmarshal formatters: %w", err)
	}

	result, _ := json.Marshal(formatters)
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{
			{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     string(result),
			},
		},
	}, nil
}

// getInstructionInfo handles GET /api/asm/{instruction_set}/{opcode}
func (gs *GodboltServer) getInstructionInfo(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	instructionSet := extractPathParam(req.Params.URI, "instruction_set")
	opcode := extractPathParam(req.Params.URI, "opcode")
	if instructionSet == "" || opcode == "" {
		return nil, fmt.Errorf("missing instruction_set or opcode parameter")
	}

	body, err := gs.fetchResource(fmt.Sprintf("%s/asm/%s/%s", gs.baseURL, instructionSet, opcode))
	if err != nil {
		return nil, fmt.Errorf("failed to get instruction info: %w", err)
	}

	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{
			{
				URI:      req.Params.URI,
				MIMEType: "text/plain",
				Text:     string(body),
			},
		},
	}, nil
}

// getVersion handles GET /api/version
func (gs *GodboltServer) getVersion(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	body, err := gs.fetchResource(gs.baseURL + "/version")
	if err != nil {
		return nil, fmt.Errorf("failed to get version: %w", err)
	}

	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{
			{
				URI:      req.Params.URI,
				MIMEType: "text/plain",
				Text:     string(body),
			},
		},
	}, nil
}

// getPopularArguments handles resource://popularArguments/{compiler_id}
// — godbolt's curated list of popular flag combinations per compiler.
// Useful for discoverability on unfamiliar toolchains (nvcc, icpx,
// embedded cross-compilers) where the common -O3/-march=native defaults
// don't apply.
func (gs *GodboltServer) getPopularArguments(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	compilerID := extractPathParam(req.Params.URI, "compiler_id")
	if compilerID == "" {
		return nil, fmt.Errorf("missing compiler_id parameter")
	}

	body, err := gs.fetchResource(fmt.Sprintf("%s/popularArguments/%s", gs.baseURL, compilerID))
	if err != nil {
		return nil, fmt.Errorf("failed to get popular arguments: %w", err)
	}

	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{
			{
				URI:      req.Params.URI,
				MIMEType: "application/json",
				Text:     string(body),
			},
		},
	}, nil
}

// extractPathParam extracts a parameter value from a resource URI template
// e.g., from "resource://compilers/cpp" with template "resource://compilers/{language_id}"
// returns "cpp"
func extractPathParam(uri, paramName string) string {
	// Parse resource://path/to/param1/param2
	// and extract by position based on paramName
	parts := strings.Split(uri, "/")

	// Map parameter name to position for known templates.
	// strings.Split("resource://compilers/cpp", "/") yields
	// ["resource:", "", "compilers", "cpp"] — the empty string after
	// "resource:" comes from the "//" separator. All positions below
	// account for that empty parts[1].
	var paramPosition int
	switch {
	case paramName == "language_id":
		paramPosition = 3 // resource://compilers/cpp → parts=["resource:","","compilers","cpp"]
	case paramName == "instruction_set":
		paramPosition = 3 // resource://asm/x86/mov → parts=["resource:","","asm","x86","mov"]
	case paramName == "opcode":
		paramPosition = 4 // resource://asm/x86/mov → "mov" at index 4
	case paramName == "compiler_id":
		paramPosition = 3 // resource://popularArguments/gcc-13.2 → parts=["resource:","","popularArguments","gcc-13.2"]
	default:
		return ""
	}

	if paramPosition < len(parts) {
		return parts[paramPosition]
	}
	return ""
}

// compileTool handles the compile_code tool
// POST /api/compiler/{compiler_id}/compile
func (gs *GodboltServer) compileTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		CompilerID        string           `json:"compiler_id"`
		Source            string           `json:"source"`
		UserArguments     string           `json:"user_arguments"`
		Files             []any            `json:"files"`
		Libraries         []any            `json:"libraries"`
		Filters           map[string]any   `json:"filters"`
		ExecuteParameters map[string]any   `json:"execute_parameters"`
		Tools             []map[string]any `json:"tools"`
	}

	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("invalid arguments: %v", err),
				},
			},
		}, nil
	}

	if args.CompilerID == "" || args.Source == "" {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: "missing required parameters: compiler_id and source",
				},
			},
		}, nil
	}

	// Build request payload
	options := map[string]any{
		"userArguments": args.UserArguments,
		"libraries":     args.Libraries,
	}
	if len(args.Filters) > 0 {
		options["filters"] = args.Filters
	}
	if len(args.ExecuteParameters) > 0 {
		options["executeParameters"] = args.ExecuteParameters
	}
	payload := map[string]any{
		"source":  args.Source,
		"options": options,
	}
	if len(args.Files) > 0 {
		payload["files"] = args.Files
	}
	if len(args.Tools) > 0 {
		// tools is a top-level payload field in godbolt's API, NOT nested
		// inside options. Cross-checked against godbolt.org's
		// /api/compiler/{id}/compile examples.
		payload["tools"] = args.Tools
	}

	// Marshal payload to JSON
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("failed to marshal request: %v", err),
				},
			},
		}, nil
	}

	url := fmt.Sprintf("%s/compiler/%s/compile", gs.baseURL, args.CompilerID)
	body, err := gs.invokeCompile(ctx, url, payloadJSON)
	if err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("failed to call compiler: %v", err),
				},
			},
		}, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: string(body),
			},
		},
	}, nil
}

// compileCMakeTool handles the compile_cmake tool
// POST /api/compiler/{compiler_id}/cmake
func (gs *GodboltServer) compileCMakeTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		CompilerID        string           `json:"compiler_id"`
		Source            string           `json:"source"`
		UserArguments     string           `json:"user_arguments"`
		Files             []any            `json:"files"`
		Libraries         []any            `json:"libraries"`
		Filters           map[string]any   `json:"filters"`
		ExecuteParameters map[string]any   `json:"execute_parameters"`
		Tools             []map[string]any `json:"tools"`
	}

	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("invalid arguments: %v", err),
				},
			},
		}, nil
	}

	if args.CompilerID == "" || args.Source == "" {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: "missing required parameters: compiler_id and source",
				},
			},
		}, nil
	}

	// Build request payload
	options := map[string]any{
		"userArguments": args.UserArguments,
		"libraries":     args.Libraries,
	}
	if len(args.Filters) > 0 {
		options["filters"] = args.Filters
	}
	if len(args.ExecuteParameters) > 0 {
		options["executeParameters"] = args.ExecuteParameters
	}
	payload := map[string]any{
		"source":  args.Source,
		"options": options,
	}
	if len(args.Files) > 0 {
		payload["files"] = args.Files
	}
	if len(args.Tools) > 0 {
		payload["tools"] = args.Tools
	}

	// Marshal payload to JSON
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("failed to marshal request: %v", err),
				},
			},
		}, nil
	}

	url := fmt.Sprintf("%s/compiler/%s/cmake", gs.baseURL, args.CompilerID)
	body, err := gs.invokeCompile(ctx, url, payloadJSON)
	if err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("failed to call cmake compiler: %v", err),
				},
			},
		}, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: string(body),
			},
		},
	}, nil
}

// formatTool handles the format_code tool
// POST /api/format/{formatter}
// Extracts the "answer" field from the JSON response
func (gs *GodboltServer) formatTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		Formatter string `json:"formatter"`
		Source    string `json:"source"`
	}

	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("invalid arguments: %v", err),
				},
			},
		}, nil
	}

	if args.Formatter == "" || args.Source == "" {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: "missing required parameters: formatter and source",
				},
			},
		}, nil
	}

	// Build request payload
	payload := map[string]any{
		"source": args.Source,
	}

	// Marshal payload to JSON
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("failed to marshal request: %v", err),
				},
			},
		}, nil
	}

	// Make POST request to Godbolt
	url := fmt.Sprintf("%s/format/%s", gs.baseURL, args.Formatter)
	resp, err := gs.httpClient.Post(url, "application/json", bytes.NewReader(payloadJSON))
	if err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("failed to call formatter: %v", err),
				},
			},
		}, nil
	}
	defer resp.Body.Close()

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("failed to read formatter response: %v", err),
				},
			},
		}, nil
	}

	// Parse JSON response and extract "answer" field
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("failed to parse formatter response: %v", err),
				},
			},
		}, nil
	}

	// Extract the "answer" field
	answer, ok := response["answer"]
	if !ok {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: "formatter response missing 'answer' field",
				},
			},
		}, nil
	}

	// Convert answer to string
	answerStr, ok := answer.(string)
	if !ok {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: "formatter 'answer' field is not a string",
				},
			},
		}, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: answerStr,
			},
		},
	}, nil
}

// invokeCompile is the shared HTTP dispatch used by compileTool (and,
// after Task 3, compileCMakeTool). It sends the POST with Accept:
// application/json so godbolt returns the full structured response
// (code / asm / stdout / stderr / execResult / optOutput) instead of
// a text-only asm dump.
func (gs *GodboltServer) invokeCompile(ctx context.Context, url string, payload []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := gs.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call godbolt: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return body, nil
}

// fetchResource is the shared HTTP-GET path used by every resource
// handler (getLanguages / getCompilers / getLibraries / getFormatters /
// getInstructionInfo / getVersion / getPopularArguments). It wraps the
// Get+ReadAll boilerplate AND adds a status-code check so non-2xx
// responses — the typical "unknown compiler id", "language not found",
// stale opcode — surface as an error instead of being silently returned
// to the MCP client as a stray HTML 404 page or JSON error body that
// downstream code would treat as the real resource.
func (gs *GodboltServer) fetchResource(url string) ([]byte, error) {
	resp, err := gs.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("call godbolt: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		// Preserve the first 200 bytes of the body so callers can see
		// what godbolt actually said (often useful for debugging a bad
		// compiler_id or instruction_set).
		snippet := string(body)
		if len(snippet) > 200 {
			snippet = snippet[:200] + "…"
		}
		return nil, fmt.Errorf("godbolt %s returned HTTP %d: %s", url, resp.StatusCode, snippet)
	}
	return body, nil
}

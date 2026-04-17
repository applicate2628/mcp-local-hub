package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const godboltBaseURL = "https://godbolt.org/api"

type GodboltServer struct {
	httpClient *http.Client
	server     *mcp.Server
}

func main() {
	gs := &GodboltServer{
		httpClient: &http.Client{},
	}

	gs.server = mcp.NewServer(&mcp.Implementation{
		Name:    "godbolt-compiler-explorer",
		Version: "1.0.0",
	}, nil)

	// Register resources
	registerResources(gs)

	// Register tools
	registerTools(gs)

	// Run server over stdio
	if err := gs.server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("failed to run server: %v", err)
	}
}

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

	gs.server.AddResource(&mcp.Resource{
		URI:         "resource://version",
		Name:        "version",
		Description: "Get the version information of the Compiler Explorer instance.",
	}, gs.getVersion)
}

func registerTools(gs *GodboltServer) {
	// Tool 1: compile_code
	gs.server.AddTool(&mcp.Tool{
		Name:        "compile_code",
		Description: "Compile source code with a specified compiler. Returns assembly output and compilation messages.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"compiler_id": map[string]interface{}{
					"type":        "string",
					"description": "The compiler ID (e.g., 'gcc11')",
				},
				"source": map[string]interface{}{
					"type":        "string",
					"description": "The source code to compile",
				},
				"user_arguments": map[string]interface{}{
					"type":        "string",
					"description": "Optional compiler flags and options",
				},
				"files": map[string]interface{}{
					"type":        "array",
					"description": "Optional additional source files",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"filename": map[string]interface{}{"type": "string"},
							"contents": map[string]interface{}{"type": "string"},
						},
					},
				},
				"libraries": map[string]interface{}{
					"type":        "array",
					"description": "Optional libraries to link",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"id":      map[string]interface{}{"type": "string"},
							"version": map[string]interface{}{"type": "string"},
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
		Description: "Compile a CMake project with a specified compiler. Returns build output and compilation messages.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"compiler_id": map[string]interface{}{
					"type":        "string",
					"description": "The compiler ID (e.g., 'gcc11')",
				},
				"source": map[string]interface{}{
					"type":        "string",
					"description": "The CMakeLists.txt content or main source file",
				},
				"user_arguments": map[string]interface{}{
					"type":        "string",
					"description": "Optional CMake or compiler flags",
				},
				"files": map[string]interface{}{
					"type":        "array",
					"description": "Optional additional source files",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"filename": map[string]interface{}{"type": "string"},
							"contents": map[string]interface{}{"type": "string"},
						},
					},
				},
				"libraries": map[string]interface{}{
					"type":        "array",
					"description": "Optional libraries to link",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"id":      map[string]interface{}{"type": "string"},
							"version": map[string]interface{}{"type": "string"},
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
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"formatter": map[string]interface{}{
					"type":        "string",
					"description": "The formatter ID (e.g., 'clangformat', 'rustfmt')",
				},
				"source": map[string]interface{}{
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
	resp, err := gs.httpClient.Get(godboltBaseURL + "/languages")
	if err != nil {
		return nil, fmt.Errorf("failed to get languages: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read languages response: %w", err)
	}

	var languages interface{}
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

	resp, err := gs.httpClient.Get(fmt.Sprintf("%s/compilers/%s", godboltBaseURL, languageID))
	if err != nil {
		return nil, fmt.Errorf("failed to get compilers: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read compilers response: %w", err)
	}

	var compilers interface{}
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

	resp, err := gs.httpClient.Get(fmt.Sprintf("%s/libraries/%s", godboltBaseURL, languageID))
	if err != nil {
		return nil, fmt.Errorf("failed to get libraries: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read libraries response: %w", err)
	}

	var libraries interface{}
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
	resp, err := gs.httpClient.Get(godboltBaseURL + "/formats")
	if err != nil {
		return nil, fmt.Errorf("failed to get formatters: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read formatters response: %w", err)
	}

	var formatters interface{}
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

	resp, err := gs.httpClient.Get(fmt.Sprintf("%s/asm/%s/%s", godboltBaseURL, instructionSet, opcode))
	if err != nil {
		return nil, fmt.Errorf("failed to get instruction info: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read instruction info response: %w", err)
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
	resp, err := gs.httpClient.Get(godboltBaseURL + "/version")
	if err != nil {
		return nil, fmt.Errorf("failed to get version: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read version response: %w", err)
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

// extractPathParam extracts a parameter value from a resource URI template
// e.g., from "resource://compilers/cpp" with template "resource://compilers/{language_id}"
// returns "cpp"
func extractPathParam(uri, paramName string) string {
	// Parse resource://path/to/param1/param2
	// and extract by position based on paramName
	parts := strings.Split(uri, "/")

	// Map parameter name to position for known templates
	var paramPosition int
	switch {
	case paramName == "language_id":
		paramPosition = 2 // resource://compilers/cpp → parts[2] = "cpp"
	case paramName == "instruction_set":
		paramPosition = 2 // resource://asm/x86/mov → parts[2] = "x86"
	case paramName == "opcode":
		paramPosition = 3 // resource://asm/x86/mov → parts[3] = "mov"
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
		CompilerID    string        `json:"compiler_id"`
		Source        string        `json:"source"`
		UserArguments string        `json:"user_arguments"`
		Files         []interface{} `json:"files"`
		Libraries     []interface{} `json:"libraries"`
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
	payload := map[string]interface{}{
		"source": args.Source,
		"options": map[string]interface{}{
			"userArguments": args.UserArguments,
			"libraries":     args.Libraries,
		},
	}

	if len(args.Files) > 0 {
		payload["files"] = args.Files
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
	url := fmt.Sprintf("%s/compiler/%s/compile", godboltBaseURL, args.CompilerID)
	resp, err := gs.httpClient.Post(url, "application/json", bytes.NewReader(payloadJSON))
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
	defer resp.Body.Close()

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("failed to read compiler response: %v", err),
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
		CompilerID    string        `json:"compiler_id"`
		Source        string        `json:"source"`
		UserArguments string        `json:"user_arguments"`
		Files         []interface{} `json:"files"`
		Libraries     []interface{} `json:"libraries"`
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
	payload := map[string]interface{}{
		"source": args.Source,
		"options": map[string]interface{}{
			"userArguments": args.UserArguments,
			"libraries":     args.Libraries,
		},
	}

	if len(args.Files) > 0 {
		payload["files"] = args.Files
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
	url := fmt.Sprintf("%s/compiler/%s/cmake", godboltBaseURL, args.CompilerID)
	resp, err := gs.httpClient.Post(url, "application/json", bytes.NewReader(payloadJSON))
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
	defer resp.Body.Close()

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("failed to read cmake compiler response: %v", err),
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
	payload := map[string]interface{}{
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
	url := fmt.Sprintf("%s/format/%s", godboltBaseURL, args.Formatter)
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
	var response map[string]interface{}
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

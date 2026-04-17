package main

import (
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
	// Placeholder for tool registration
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

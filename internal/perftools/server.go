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
	// Tasks 2-5 will AddTool here.
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

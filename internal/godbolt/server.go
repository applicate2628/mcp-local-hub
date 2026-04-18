// Package godbolt implements the Godbolt Compiler Explorer MCP server
// over stdio. It is consumed as a library from two entry points:
//   - cmd/godbolt, a standalone binary for users who don't want the hub
//   - internal/godbolt.NewCommand, embedded as an mcphub subcommand
//
// Both entry points share the same handlers, SDK setup, and stdio
// transport via Run(ctx), so there is no behavior drift between shapes.
package godbolt

import (
	"context"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// godboltBaseURL is the Compiler Explorer REST API root. Kept unexported
// because it is an internal transport detail, not part of the package
// contract.
const godboltBaseURL = "https://godbolt.org/api"

// GodboltServer holds the HTTP client used to call the Compiler Explorer
// REST API and the MCP server instance that dispatches resource reads
// and tool calls to the methods on this struct.
type GodboltServer struct {
	httpClient *http.Client
	server     *mcp.Server
	baseURL    string // defaults to godboltBaseURL; overridable for tests
}

// Run wires up a fresh GodboltServer, registers all resources and tools,
// and serves the MCP protocol over stdio until ctx is cancelled or the
// transport closes. It is the single source of truth for both entry
// points; keep runtime behavior here, not in cmd/godbolt or NewCommand.
func Run(ctx context.Context) error {
	gs := &GodboltServer{
		httpClient: &http.Client{},
		baseURL:    godboltBaseURL,
	}

	gs.server = mcp.NewServer(&mcp.Implementation{
		Name:    "godbolt-compiler-explorer",
		Version: "1.0.0",
	}, nil)

	registerResources(gs)
	registerTools(gs)

	if err := gs.server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("godbolt server: %w", err)
	}
	return nil
}

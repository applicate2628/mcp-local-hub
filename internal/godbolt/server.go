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
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrGodboltDisabled is returned from every HTTP-touching handler when
// MCPHUB_GODBOLT_DISABLE is set. The MCP tool descriptions advertise
// the env var so agent clients know how to hard-block the server from
// sending source code off-host — useful when the current workspace
// holds confidential or export-controlled material.
var ErrGodboltDisabled = errors.New("godbolt server disabled by MCPHUB_GODBOLT_DISABLE env var (unset it to re-enable outbound requests to godbolt.org)")

// godboltDisabled reports whether outbound requests should be refused.
// Re-evaluated each call rather than cached so toggling the var for
// a single test / session doesn't require restart.
func godboltDisabled() bool {
	return os.Getenv("MCPHUB_GODBOLT_DISABLE") != ""
}

// godboltBaseURL is the Compiler Explorer REST API root. Kept unexported
// because it is an internal transport detail, not part of the package
// contract.
const godboltBaseURL = "https://godbolt.org/api"

const godboltHTTPTimeout = 30 * time.Second

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
		httpClient: &http.Client{Timeout: godboltHTTPTimeout},
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

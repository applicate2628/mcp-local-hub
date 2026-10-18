// Package kimi exposes the installed Orchestrarium Kimi wrapper as one thin,
// synchronous MCP tool. Wrapper policy and execution remain external owners.
package kimi

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Server struct {
	server *mcp.Server
	runner runExecutor
}

func newServer(runner runExecutor) *Server {
	s := &Server{runner: runner}
	s.server = mcp.NewServer(&mcp.Implementation{Name: "kimi", Version: "1.0.0"}, nil)
	s.server.AddTool(&mcp.Tool{
		Name:        "run_kimi",
		Description: "Run one bounded file-prompt task through the installed fixed kimi-code/k3 wrapper. Effort is high (default) or max; wrapper policy remains authoritative.",
		InputSchema: runKimiInputSchema(),
	}, s.runKimiTool)
	return s
}

func Run(ctx context.Context) error {
	s := newServer(NewRunner())
	if err := s.server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("kimi server: %w", err)
	}
	return nil
}

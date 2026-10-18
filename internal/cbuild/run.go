package cbuild

import (
	"context"
	"io"
	"path/filepath"

	"mcp-local-hub/internal/cbuild/mcp"
)

const version = "0.1.0"

// Run serves the existing CBuild MCP wire on caller-owned streams. It leaves
// signal and logging ownership with the mcphub composition root.
func Run(ctx context.Context, defaultDir string, stdin io.Reader, stdout io.Writer) error {
	if defaultDir != "" {
		if abs, err := filepath.Abs(defaultDir); err == nil {
			defaultDir = abs
		}
	}
	return mcp.NewServer("mcp-cbuild", version, Tools(defaultDir)).Serve(ctx, stdin, stdout)
}

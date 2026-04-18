// cmd/perftools is the standalone perf-toolbox MCP server binary —
// the same server that ships as `mcphub perftools`, packaged on its
// own for users who want the perf tools without the full mcphub stack.
package main

import (
	"os"

	"mcp-local-hub/internal/perftools"
)

func main() {
	if err := perftools.NewCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

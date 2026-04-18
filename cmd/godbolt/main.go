// cmd/godbolt is the standalone godbolt MCP server binary. For users who
// want to run godbolt without the mcphub hub. Identical runtime behavior
// to `mcphub godbolt`, just packaged as its own exe.
package main

import (
	"os"

	"mcp-local-hub/internal/godbolt"
)

func main() {
	if err := godbolt.NewCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

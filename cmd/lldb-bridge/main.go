// cmd/lldb-bridge is the standalone LLDB bridge binary. For users who want
// the bridge without the full mcphub stack. Identical runtime behavior to
// `mcphub lldb-bridge`, just packaged as its own exe.
package main

import (
	"os"

	"mcp-local-hub/internal/lldb"
)

func main() {
	if err := lldb.NewCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

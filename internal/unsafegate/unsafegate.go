// Package unsafegate is the single owner of the "arbitrary-local-execution MCP
// tool is opt-in, secure by default" gate shared by the oneapi-run, drmemory,
// and vtune servers. Each of those tools runs a caller-supplied executable
// (an arbitrary-local-code-execution surface for a broadly-configured MCP
// client), so its tool is registered ONLY after the operator explicitly opts
// in via a per-tool env var. When the opt-in is absent the daemon still serves
// the MCP protocol — it just registers no tool — so a misconfigured client
// cannot reach the surface.
package unsafegate

import (
	"fmt"
	"io"
	"os"
)

// Enabled reports whether the operator opted into the unsafe tool by setting
// envVar to exactly "1". Any other value (unset, "0", "true", " 1", …) keeps it
// disabled — secure by default. Pure (no side effects) so tests can assert it.
func Enabled(envVar string) bool {
	return os.Getenv(envVar) == "1"
}

// RegisterAllowed is Enabled plus a one-line diagnostic to stderr when the tool
// is DISABLED, so the secure-default is OBSERVABLE rather than a silently empty
// daemon (a healthy MCP daemon whose tools/list is empty, with no clue why).
// Call it ONCE at tool-registration time. It must write to stderr / a log sink,
// never stdout — stdout is the JSON-RPC channel for stdio MCP servers.
func RegisterAllowed(envVar, toolName string) bool {
	return registerAllowed(os.Stderr, envVar, toolName)
}

// registerAllowed is the io.Writer-seam form so tests can capture the
// diagnostic without touching the real stderr.
func registerAllowed(w io.Writer, envVar, toolName string) bool {
	if Enabled(envVar) {
		return true
	}
	fmt.Fprintf(w, "%s: tool NOT registered — set %s=1 to enable (this tool runs a caller-supplied executable, i.e. arbitrary local code execution; secure-default off)\n", toolName, envVar)
	return false
}

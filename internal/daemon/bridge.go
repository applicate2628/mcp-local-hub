// Package daemon — bridge.go is DEPRECATED as of Phase 2.
//
// Original role: wrapped a stdio MCP server in `npx supergateway` to expose it
// over HTTP. Replaced by internal/daemon/host.go, a native Go stdio-host that
// handles the HTTP→stdio proxy without requiring node/npm. bridge.go is kept
// for reference only; no production code path invokes it.
package daemon

import (
	"fmt"
	"strings"
)

// BuildBridgeSpec wraps a stdio MCP server invocation into a supergateway call
// that exposes it as HTTP on `port`. Returns a LaunchSpec ready for Launch().
//
// supergateway is a well-maintained community MCP stdio↔HTTP bridge.
// We spawn it via `npx -y supergateway --stdio "<inner cmd>" --port <N>`.
//
// innerCmd + innerArgs form the stdio server's own command line. supergateway
// expects this as a single shell-quoted string in the --stdio argument.
// Env is applied to the inner command's process (supergateway forwards it).
func BuildBridgeSpec(innerCmd string, innerArgs []string, port int, env map[string]string, logPath string) LaunchSpec {
	// Shell-quote the inner command line for supergateway.
	// Simple strategy: wrap each token in double quotes if it contains whitespace.
	quoted := make([]string, 0, len(innerArgs)+1)
	quoted = append(quoted, shellQuote(innerCmd))
	for _, a := range innerArgs {
		quoted = append(quoted, shellQuote(a))
	}
	stdioArg := strings.Join(quoted, " ")
	return LaunchSpec{
		Command:    "npx",
		Args:       []string{"-y", "supergateway", "--stdio", stdioArg, "--port", fmt.Sprintf("%d", port)},
		Env:        env,
		LogPath:    logPath,
		MaxLogSize: 10 * 1024 * 1024,
		LogKeep:    5,
	}
}

// shellQuote conservatively wraps a token in double quotes when it contains
// whitespace or shell metacharacters. Backslashes on Windows paths are preserved.
func shellQuote(s string) string {
	if s == "" {
		return `""`
	}
	needs := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '"' || r == '\'' || r == '&' || r == '|' {
			needs = true
			break
		}
	}
	if !needs {
		return s
	}
	// Escape internal double quotes by doubling them (cmd.exe-compatible).
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

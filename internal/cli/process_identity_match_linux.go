//go:build linux

package cli

import "mcp-local-hub/internal/process"

func pidMatchesMcphub(pid int) bool {
	return process.PIDExecutableMatches(pid, canonicalMcphubPath())
}

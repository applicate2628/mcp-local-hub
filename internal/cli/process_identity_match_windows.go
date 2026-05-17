//go:build windows

package cli

import (
	"strings"

	"mcp-local-hub/internal/process"
)

func pidMatchesMcphub(pid int) bool {
	ident, err := process.LookupProcessIdentity(pid)
	if err != nil {
		return false
	}
	return strings.EqualFold(ident.Basename, "mcphub.exe")
}

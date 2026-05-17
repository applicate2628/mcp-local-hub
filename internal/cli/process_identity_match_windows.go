//go:build windows

package cli

import (
	"path/filepath"

	"mcp-local-hub/internal/process"
)

func pidMatchesMcphub(pid int) bool {
	ident, err := process.LookupProcessIdentity(pid)
	if err != nil {
		return false
	}
	if ident.ExecutablePath == "" {
		return false
	}
	target := ident.ExecutablePath
	if abs, err := filepath.Abs(target); err == nil {
		target = abs
	}
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}
	return pathsEqual(target, canonicalMcphubPath())
}

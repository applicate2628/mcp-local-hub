//go:build windows

package api

import "mcp-local-hub/internal/process"

// defaultProcessOwnerSIDMatchesCurrent (Windows) delegates to the single shared
// owner-SID helper in internal/process. See process_owner_sid.go for the
// fail-closed contract.
func defaultProcessOwnerSIDMatchesCurrent(pid int) (bool, error) {
	return process.ProcessOwnerSIDMatchesCurrent(pid)
}

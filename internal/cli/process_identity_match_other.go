//go:build !windows && !linux

package cli

// pidMatchesMcphub is intentionally weaker on macOS/BSD for v0.5.0:
// Windows is GA and verifies image identity through CIM; Linux beta
// uses /proc/<pid>/exe in process_identity_match_linux.go. macOS/BSD
// do not expose Linux-style /proc, so sysctl-based identity is a
// separate macOS-preview hardening task. Until then these platforms
// keep the r2 IsPidAlive-only contract.
func pidMatchesMcphub(_ int) bool {
	return true
}

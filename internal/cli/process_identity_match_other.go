//go:build !windows && !linux

package cli

// pidMatchesMcphub returns false on macOS/BSD: v0.5.0 has no native
// identity probe for these platforms (no /proc), so fail-closed is
// the only safe behavior. A stale PID from supervisor-state.json
// is treated as not-running on startup, and the terminate path
// refuses to signal it. Follow-up task should add sysctl-based
// identity for darwin; tracked under v0.6 macOS GA.
func pidMatchesMcphub(_ int) bool {
	return false
}

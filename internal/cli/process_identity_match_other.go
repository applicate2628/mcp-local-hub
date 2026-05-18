//go:build !windows && !linux

package cli

// pidMatchesMcphub returns false on macOS/BSD: v0.5.0 has no native
// identity probe for these platforms (no /proc), so fail-closed is
// the only safe behavior for identity-sensitive call sites. Startup
// current-running reconciliation handles the unsupported-proof case
// separately with a liveness-only fallback to avoid duplicate daemons;
// the terminate path still refuses to signal without identity proof.
// Follow-up task should add sysctl-based identity for darwin; tracked
// under v0.6 macOS GA.
func pidMatchesMcphub(_ int) bool {
	return false
}

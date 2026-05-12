//go:build !windows

// hub_mcp_state_posix.go — Phase 2 Task 2.2 POSIX leg.
//
// POSIX missing-file detection. errors.Is(err, os.ErrNotExist) is
// usually enough on POSIX because syscall errors flow through to the
// fs sentinel chain. This helper exists for symmetry with the Windows
// leg, where the NT relative-open surfaces NTStatus values that don't
// match os.ErrNotExist directly.

package api

// isHubMcpStateMissingErrPlatform is the POSIX no-op extension to the
// portable isHubMcpStateMissingErr helper. The portable layer already
// checked errors.Is(err, os.ErrNotExist); on POSIX there's no further
// kernel-specific sentinel to consult.
func isHubMcpStateMissingErrPlatform(_ error) bool {
	return false
}

//go:build !windows

package cli

// pidMatchesMcphub is intentionally weaker on POSIX for v0.5.0:
// Windows is GA and can verify image identity through CIM, while
// Linux is beta and macOS is preview. Follow-up should add a Linux
// /proc/<pid>/comm identity check; until then POSIX keeps the r2
// IsPidAlive-only contract.
func pidMatchesMcphub(_ int) bool {
	return true
}

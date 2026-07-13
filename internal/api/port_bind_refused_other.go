//go:build !windows

package api

// IsPortBindRefusedErr — POSIX stub. The ephemeral-collision self-heal targets
// the Windows-specific WSAEADDRINUSE/WSAEACCES bind-refusal class (the OS
// ephemeral range overlapping the daemon pools is a Windows/WSL2 phenomenon), so
// the predicate is a compile-time no-op off Windows. Mirrors the
// hub_listener_rebind_posix.go stub the GUI hub-listener uses. A future POSIX
// analogue (EADDRINUSE) can populate this without touching the callers.
func IsPortBindRefusedErr(error) bool {
	return false
}

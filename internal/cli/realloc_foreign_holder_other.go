//go:build !windows

package cli

// reallocForeignHolder — POSIX stub. The ephemeral-collision self-heal targets
// the Windows WSAEADDRINUSE/WSAEACCES bind-refusal class, and the process
// identity probe used to resolve a holder's basename is Windows-only, so off
// Windows the L3 event simply omits foreign_holder (returning pid<=0).
func reallocForeignHolder(int) (int, string) { return 0, "" }

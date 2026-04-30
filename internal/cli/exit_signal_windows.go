//go:build windows

package cli

import "os"

// extractSignal is a Windows no-op. Windows has no POSIX signal model;
// the equivalent diagnostic information (NTSTATUS code, e.g.
// 0xC0000005 access violation) is already encoded in
// ProcessState.ExitCode() — when reinterpreted as a signed int it
// becomes a large negative number that maps back to the NTSTATUS via
// the standard Win32 status code table.
func extractSignal(_ *os.ProcessState) string {
	return ""
}

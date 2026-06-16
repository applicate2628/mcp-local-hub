//go:build !windows

package oneapirun

// commandExtensions on non-Windows hosts is just the bare command name —
// POSIX executables carry no implicit extension and the executable bit
// (which isRegularFile does not check, by design — matching the existing
// fileExists semantics in this package) is the relevant gate. The env
// argument is unused here but kept for signature parity with the Windows
// build so resolveCommandPath stays platform-agnostic.
func commandExtensions(_ string, _ []string) []string {
	return []string{""}
}

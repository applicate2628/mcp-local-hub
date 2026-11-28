//go:build windows

package cli

import "mcp-local-hub/internal/process"

// ResolveWindowsConsolePolicy recognizes only an exact first argument after
// argv[0]. On a match it removes exactly that element; every other string is
// preserved byte-for-byte, in order, with duplicates intact.
func ResolveWindowsConsolePolicy(args []string) (WindowsConsolePolicy, []string) {
	debug, normalized := process.NormalizeWindowsConsoleArgv(args)
	if !debug {
		return WindowsConsoleDisabled, normalized
	}
	return WindowsConsoleDebugExplicit, normalized
}

func windowsConsoleStartupUsage() string {
	return "\n\nWindows debug console (startup prefix only):\n  mcphub " + WindowsDebugConsolePrefix + " [command ...]"
}

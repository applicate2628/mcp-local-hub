//go:build windows

package cli

// ResolveWindowsConsolePolicy recognizes only an exact first argument after
// argv[0]. On a match it removes exactly that element; every other string is
// preserved byte-for-byte, in order, with duplicates intact.
func ResolveWindowsConsolePolicy(args []string) (WindowsConsolePolicy, []string) {
	if len(args) < 2 || args[1] != WindowsDebugConsolePrefix {
		return WindowsConsoleDisabled, args
	}
	normalized := make([]string, 0, len(args)-1)
	normalized = append(normalized, args[0])
	normalized = append(normalized, args[2:]...)
	return WindowsConsoleDebugExplicit, normalized
}

func windowsConsoleStartupUsage() string {
	return "\n\nWindows debug console (startup prefix only):\n  mcphub " + WindowsDebugConsolePrefix + " [command ...]"
}

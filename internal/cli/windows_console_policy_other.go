//go:build !windows

package cli

// ResolveWindowsConsolePolicy is a no-op outside Windows. The Windows-only
// startup token remains ordinary Cobra-owned input on these platforms.
func ResolveWindowsConsolePolicy(args []string) (WindowsConsolePolicy, []string) {
	return WindowsConsoleDisabled, args
}

func windowsConsoleStartupUsage() string { return "" }

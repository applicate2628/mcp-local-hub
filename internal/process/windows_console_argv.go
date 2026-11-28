package process

import "runtime"

// WindowsDebugConsolePrefix is the sole startup opt-in to a Windows console.
const WindowsDebugConsolePrefix = "--debug-console"

// NormalizeWindowsConsoleArgv recognizes and removes one exact Windows
// startup prefix immediately after argv[0]. A raw native invocation and its
// parser must agree on the resulting command; all other arguments stay intact.
func NormalizeWindowsConsoleArgv(args []string) (debug bool, normalized []string) {
	if runtime.GOOS != "windows" || len(args) < 2 || args[1] != WindowsDebugConsolePrefix {
		return false, args
	}
	normalized = make([]string, 0, len(args)-1)
	normalized = append(normalized, args[0])
	normalized = append(normalized, args[2:]...)
	return true, normalized
}

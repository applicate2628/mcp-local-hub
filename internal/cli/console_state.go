package cli

import "sync/atomic"

// consoleAttachedAtStartup records whether THIS process became a client of
// a Windows console when it started (cmd/mcphub's
// attachParentConsoleIfAvailable), i.e. whether a closing terminal can
// deliver CTRL_CLOSE_EVENT to it.
//
// It is set once by the composition root via SetConsoleAttached and read
// by the GUI command when it resolves its console-lifetime policy. atomic
// rather than a plain bool so a test that sets it concurrently with a
// running GUI goroutine stays race-detector clean.
var consoleAttachedAtStartup atomic.Bool

// SetConsoleAttached injects the process's console-client state from
// cmd/mcphub's main(), sibling to SetBuildInfo.
//
// Only main() can determine this: the console attach is the first
// statement of main, long before cobra parses a flag, and the answer is
// ambient process state rather than anything a command can derive. Lower
// layers consume the injected value; they must not probe the console
// themselves and must not re-derive "am I a background app" from an
// unrelated flag.
func SetConsoleAttached(attached bool) { consoleAttachedAtStartup.Store(attached) }

// ConsoleAttached reports the injected console-client state. Defaults to
// false, which is the safe default for every non-main entry point (tests,
// library embedding): no console is claimed, so nothing releases one.
func ConsoleAttached() bool { return consoleAttachedAtStartup.Load() }

// resolveReleaseConsole is the pure decision behind `mcphub gui`'s
// console-lifetime policy, split out so it is table-testable without a
// real console.
//
// Release the console only when there is one to release AND the operator
// did not ask to keep it. Both halves matter:
//
//   - consoleAttached false (Explorer double-click, the autostart shim,
//     a detached spawn) means there is nothing to release, so releasing
//     would be a pointless syscall that still kills stdio.
//   - foreground / noTray is the operator asking for terminal-coupled
//     lifetime with Ctrl-C intact.
func resolveReleaseConsole(consoleAttached, foreground, noTray bool) bool {
	return consoleAttached && !foreground && !noTray
}

package cli

import "sync/atomic"

// debugConsoleAcquired records whether THIS process explicitly attached or
// allocated a Windows debug console during startup.
//
// It is set once by the composition root via SetDebugConsoleAcquired and read
// by the GUI command when it resolves its console-lifetime policy. atomic
// rather than a plain bool so a test that sets it concurrently with a
// running GUI goroutine stays race-detector clean.
var debugConsoleAcquired atomic.Bool

// SetDebugConsoleAcquired injects the process-local explicit policy result.
//
// Only main() can determine this from the startup-prefix policy result. Lower
// layers consume the injected value and never re-derive console intent from an
// unrelated command flag.
func SetDebugConsoleAcquired(acquired bool) { debugConsoleAcquired.Store(acquired) }

// DebugConsoleAcquired reports the injected explicit-policy result. Its safe
// zero value means ordinary launches never release a console they do not own.
func DebugConsoleAcquired() bool { return debugConsoleAcquired.Load() }

// resolveReleaseConsole is the pure decision behind `mcphub gui`'s
// console-lifetime policy, split out so it is table-testable without a
// real console.
//
// Release the console only when there is one to release AND the operator
// did not ask to keep it. Both halves matter:
//
//   - debugConsoleAcquired false (Explorer double-click, the autostart shim,
//     a detached spawn) means there is nothing to release, so releasing
//     would be a pointless syscall that still kills stdio.
//   - foreground / noTray preserves the existing terminal-coupled lifetime
//     when the explicit debug console is present.
func resolveReleaseConsole(debugConsoleAcquired, foreground, noTray bool) bool {
	return debugConsoleAcquired && !foreground && !noTray
}

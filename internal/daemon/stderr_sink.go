// internal/daemon/stderr_sink.go — centralized "is mcphub's stderr a real
// terminal?" gate used by every daemon-side fallback write.
//
// Background: commit 401885b ("fix(daemon): suppress upstream stderr leak
// when mcphub's stderr is a pipe") gated the happy path of
// openStderrSink + openLogWriters on term.IsTerminal but left the fallback
// paths (mkdir fail, file-open fail, LogPath == "") + the daemon's own
// diagnostic emissions (LogSupervisorEvent, fmt.Fprintf(os.Stderr, "warn:
// ...")) unconditionally writing to os.Stderr. When mcphub's stderr is a
// pipe (scheduler-spawned, OR mcphub spawned as a stdio child by an MCP
// client) those bytes leak into the parent's inherited stdio and corrupt
// the terminal of any operator running Codex/Claude Code subagents.
//
// Issue #162 closure: route every daemon-side stderr emission through
// daemonDiagWriter() (file-or-discard, never raw os.Stderr in non-TTY
// contexts). The terminal probe is an injectable var so tests can pin
// both branches without touching real file descriptors.

package daemon

import (
	"io"
	"os"
	"sync"

	"golang.org/x/term"
)

// stderrIsTerminalMu guards the stderrIsTerminal package var so
// SetStderrIsTerminalForTest is safe to call while concurrent tests
// (or background daemon goroutines that touch daemonDiagWriter)
// are running. Required because the helper is read on hot paths
// (every fmt.Fprintf(daemonDiagWriter(), ...) call site).
//
// codex deep-sec PR #164 P3 closure.
var stderrIsTerminalMu sync.RWMutex

// stderrIsTerminal reports whether mcphub's own os.Stderr is connected
// to a real terminal. Pipe / file / dev/null all return false; only an
// actual TTY returns true. Injectable for tests via SetStderrIsTerminalForTest.
// Read under stderrIsTerminalMu.RLock.
var stderrIsTerminal = func() bool {
	return term.IsTerminal(int(os.Stderr.Fd()))
}

// daemonDiagWriter returns the writer that fallback paths + diagnostic
// emissions should use. On a real terminal we keep tee'ing to os.Stderr
// (the operator ran the daemon interactively for debug). On a pipe /
// file we return io.Discard so upstream "SUCCESS: ..." chatter, mkdir
// warnings, and supervisor events never reach the parent's stdio.
//
// The durable channel for diagnostics is the daemon's LogPath (rotated,
// 10MB cap, 5 keep). Anything that needs durability MUST also write to
// LogPath; daemonDiagWriter is the BEST-EFFORT mirror.
func daemonDiagWriter() io.Writer {
	stderrIsTerminalMu.RLock()
	probe := stderrIsTerminal
	stderrIsTerminalMu.RUnlock()
	if probe() {
		return os.Stderr
	}
	return io.Discard
}

// DaemonDiagWriter is the exported equivalent of daemonDiagWriter for
// cross-package callers (CLI lazy-proxy supervisor, workspace daemon
// SIGINT/SIGTERM path). Behavior is identical.
//
// codex deep-sec PR #164 P2 closure: the CLI workspace-daemon path was
// still writing `warn: proxy stop: %v` directly to os.Stderr, which
// bypassed the contract for scheduler-spawned daemons whose stderr
// is a pipe.
func DaemonDiagWriter() io.Writer { return daemonDiagWriter() }

// SetStderrIsTerminalForTest replaces the terminal probe for the
// duration of a test. Returns a restore closure. Mutex-guarded so
// t.Parallel() callers stay race-free.
//
// codex deep-sec PR #164 P3 closure: prior version mutated the
// package var without synchronization.
func SetStderrIsTerminalForTest(b bool) func() {
	stderrIsTerminalMu.Lock()
	prev := stderrIsTerminal
	stderrIsTerminal = func() bool { return b }
	stderrIsTerminalMu.Unlock()
	return func() {
		stderrIsTerminalMu.Lock()
		stderrIsTerminal = prev
		stderrIsTerminalMu.Unlock()
	}
}

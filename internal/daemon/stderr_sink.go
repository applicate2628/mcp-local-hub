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

	"golang.org/x/term"
)

// stderrIsTerminal reports whether mcphub's own os.Stderr is connected
// to a real terminal. Pipe / file / dev/null all return false; only an
// actual TTY returns true. Injectable for tests via SetStderrIsTerminalForTest.
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
	if stderrIsTerminal() {
		return os.Stderr
	}
	return io.Discard
}

// SetStderrIsTerminalForTest replaces the terminal probe for the
// duration of a test. Returns a restore closure. Used by tests that
// pin the non-TTY silence contract — see TestOpenStderrSink_NonTTYReturnsDiscard
// and TestOpenLogWriters_NonTTYReturnsLogFileOnly.
func SetStderrIsTerminalForTest(b bool) func() {
	prev := stderrIsTerminal
	stderrIsTerminal = func() bool { return b }
	return func() { stderrIsTerminal = prev }
}

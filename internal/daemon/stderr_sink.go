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
// codex deep-sec PR #164 r1 P3 closure.
var stderrIsTerminalMu sync.RWMutex

// stderrIsTerminal reports whether mcphub's own os.Stderr is connected
// to a real terminal. Pipe / file / dev/null all return false; only an
// actual TTY returns true. Injectable for tests via SetStderrIsTerminalForTest.
// Read ONLY via isStderrTerminal() so the mutex is honored.
var stderrIsTerminal = func() bool {
	return term.IsTerminal(int(os.Stderr.Fd()))
}

// stderrIsTerminalActiveOverride tracks whether a test currently
// holds the probe-override. SetStderrIsTerminalForTest panics if a
// second Set is attempted while one is active, because the restore
// closure pattern (`defer restore()`) cannot make overlapping
// overrides converge to the original probe in all orderings — the
// "test A sets X, test B sets Y, A restores first" scenario would
// leak X past A's lifetime if we silently allowed overlap.
//
// codex deep-sec PR #164 r2 P2 closure.
var stderrIsTerminalActiveOverride bool

// isStderrTerminal is the canonical mutex-honoring read path for the
// probe. Every internal call site (daemonDiagWriter, openStderrSink,
// openLogWriters, …) MUST use this helper instead of calling
// stderrIsTerminal() directly — direct calls bypass the RLock and
// race with SetStderrIsTerminalForTest.
//
// codex deep-sec PR #164 r2 P2 closure: direct reads at host.go:605
// + http_host.go:339 were flagged because they undermined the
// race-safety the mutex was added to provide.
func isStderrTerminal() bool {
	stderrIsTerminalMu.RLock()
	probe := stderrIsTerminal
	stderrIsTerminalMu.RUnlock()
	return probe()
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
	if isStderrTerminal() {
		return os.Stderr
	}
	return io.Discard
}

// DaemonDiagWriter is the exported equivalent of daemonDiagWriter for
// cross-package callers (CLI lazy-proxy supervisor, workspace daemon
// SIGINT/SIGTERM path). Behavior is identical.
//
// codex deep-sec PR #164 r1 P2 closure: the CLI workspace-daemon path
// was still writing `warn: proxy stop: %v` directly to os.Stderr,
// which bypassed the contract for scheduler-spawned daemons whose
// stderr is a pipe.
func DaemonDiagWriter() io.Writer { return daemonDiagWriter() }

// SetStderrIsTerminalForTest replaces the terminal probe for the
// duration of a test. Returns a restore closure that MUST be
// invoked (use `defer`).
//
// Concurrency contract: ONE override at a time. The helper panics
// if called while another override is still active; callers that
// run in parallel must use a different injection seam (e.g. inject
// the probe at construction). Tests in this package run serially
// and use the `defer restore()` pattern; that pattern is correct
// here, including when a single test calls Set + restore + Set
// + restore in sequence.
//
// codex deep-sec PR #164 r1 P3 + r2 P2 closure.
func SetStderrIsTerminalForTest(b bool) func() {
	stderrIsTerminalMu.Lock()
	if stderrIsTerminalActiveOverride {
		stderrIsTerminalMu.Unlock()
		panic("daemon.SetStderrIsTerminalForTest: another override is already active; this helper does not support overlapping calls — release the prior restore() before setting a new override, or use a per-instance injection seam for parallel tests")
	}
	prev := stderrIsTerminal
	stderrIsTerminal = func() bool { return b }
	stderrIsTerminalActiveOverride = true
	stderrIsTerminalMu.Unlock()
	var released bool
	return func() {
		stderrIsTerminalMu.Lock()
		defer stderrIsTerminalMu.Unlock()
		if released {
			return
		}
		stderrIsTerminal = prev
		stderrIsTerminalActiveOverride = false
		released = true
	}
}

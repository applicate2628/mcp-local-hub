package daemon

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestDaemonDiagWriter_NonTTYIsDiscard pins issue #162 P2 closure: when
// mcphub's own stderr is a pipe / file / dev-null, the daemon-side
// diag writer (used by every fallback path + by LogSupervisorEvent +
// by every fmt.Fprintf(daemonDiagWriter(), "warn: ...") call) must
// be io.Discard — NOT raw os.Stderr. Otherwise upstream subprocess
// chatter and daemon warnings would leak into the parent's inherited
// stdio (the regression behavior 401885b only partially fixed).
func TestDaemonDiagWriter_NonTTYIsDiscard(t *testing.T) {
	restore := SetStderrIsTerminalForTest(false)
	defer restore()
	if w := daemonDiagWriter(); w != io.Discard {
		t.Errorf("non-TTY daemonDiagWriter() = %T, want io.Discard", w)
	}
}

// TestDaemonDiagWriter_TTYIsStderr pins the operator-interactive
// branch: when mcphub is run from a real terminal, the diag writer
// stays as os.Stderr so the operator sees warnings + supervisor
// events on the console.
func TestDaemonDiagWriter_TTYIsStderr(t *testing.T) {
	restore := SetStderrIsTerminalForTest(true)
	defer restore()
	if w := daemonDiagWriter(); w != os.Stderr {
		t.Errorf("TTY daemonDiagWriter() = %T, want os.Stderr", w)
	}
}

// TestOpenStderrSink_LogPathEmptyNonTTY pins the new fallback shape:
// when LogPath is empty AND stderr is not a terminal, openStderrSink
// returns io.Discard. Before issue #162 it returned raw os.Stderr.
func TestOpenStderrSink_LogPathEmptyNonTTY(t *testing.T) {
	restore := SetStderrIsTerminalForTest(false)
	defer restore()
	h := &StdioHost{cfg: HostConfig{LogPath: ""}}
	if got := h.openStderrSink(); got != io.Discard {
		t.Errorf("LogPath='', non-TTY → openStderrSink() = %T, want io.Discard", got)
	}
}

// TestOpenStderrSink_LogPathEmptyTTY pins the interactive-debug
// branch: empty LogPath + TTY → os.Stderr.
func TestOpenStderrSink_LogPathEmptyTTY(t *testing.T) {
	restore := SetStderrIsTerminalForTest(true)
	defer restore()
	h := &StdioHost{cfg: HostConfig{LogPath: ""}}
	if got := h.openStderrSink(); got != os.Stderr {
		t.Errorf("LogPath='', TTY → openStderrSink() = %T, want os.Stderr", got)
	}
}

// TestOpenStderrSink_LogPathSetNonTTY pins: LogPath set + non-TTY →
// rotating file writer ONLY (no os.Stderr leg). The "log file is the
// sole sink" property is what protects the parent's stdio.
func TestOpenStderrSink_LogPathSetNonTTY(t *testing.T) {
	restore := SetStderrIsTerminalForTest(false)
	defer restore()
	logPath := filepath.Join(t.TempDir(), "stdio-host.log")
	h := &StdioHost{cfg: HostConfig{LogPath: logPath}}
	got := h.openStderrSink()
	if _, isRfw := got.(*rotatingFileWriter); !isRfw {
		t.Errorf("LogPath set, non-TTY → openStderrSink() = %T, want *rotatingFileWriter", got)
	}
	if h.logFile != nil {
		_ = h.logFile.Close()
	}
}

// TestOpenLogWriters_LogPathEmptyNonTTY pins the HTTPHost variant of
// the same contract: both stdout and stderr writers are io.Discard,
// closer is nil.
func TestOpenLogWriters_LogPathEmptyNonTTY(t *testing.T) {
	restore := SetStderrIsTerminalForTest(false)
	defer restore()
	h := &HTTPHost{cfg: HTTPHostConfig{LogPath: ""}}
	stdout, stderr, closer := h.openLogWriters()
	if stdout != io.Discard {
		t.Errorf("LogPath='', non-TTY → stdout writer = %T, want io.Discard", stdout)
	}
	if stderr != io.Discard {
		t.Errorf("LogPath='', non-TTY → stderr writer = %T, want io.Discard", stderr)
	}
	if closer != nil {
		t.Errorf("LogPath='', non-TTY → closer != nil; want nil")
	}
}

// TestOpenStderrSink_FileOpenFailureFallsBackToDiscard pins codex
// deep-sec PR #164 P3 closure: when LogPath is set but the file
// cannot be opened (parent dir is a regular file, not a directory,
// so MkdirAll fails), openStderrSink must NOT fall back to raw
// os.Stderr. The non-TTY branch returns io.Discard so the warn
// line about the failed open + any subsequent subprocess output
// stays out of the parent's stdio.
func TestOpenStderrSink_FileOpenFailureFallsBackToDiscard(t *testing.T) {
	restore := SetStderrIsTerminalForTest(false)
	defer restore()
	// Create a regular file where the helper would expect a directory.
	// MkdirAll on the LogPath parent will fail because that parent is
	// not a directory.
	root := t.TempDir()
	notADir := filepath.Join(root, "blocker")
	if err := os.WriteFile(notADir, []byte("file-not-dir"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	// LogPath that needs a child of `blocker` — mkdir will fail because
	// blocker is a regular file.
	logPath := filepath.Join(notADir, "child", "stdio-host.log")
	h := &StdioHost{cfg: HostConfig{LogPath: logPath}}
	got := h.openStderrSink()
	if got != io.Discard {
		t.Errorf("mkdir failure path → openStderrSink() = %T, want io.Discard", got)
	}
	if h.logFile != nil {
		t.Errorf("h.logFile was set despite mkdir failure; want nil")
	}
}

// TestOpenLogWriters_FileOpenFailureFallsBackToDiscard pins the
// HTTPHost variant of the same fallback: mkdir failure on a non-TTY
// stderr returns (io.Discard, io.Discard, nil).
func TestOpenLogWriters_FileOpenFailureFallsBackToDiscard(t *testing.T) {
	restore := SetStderrIsTerminalForTest(false)
	defer restore()
	root := t.TempDir()
	notADir := filepath.Join(root, "blocker")
	if err := os.WriteFile(notADir, []byte("file-not-dir"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	logPath := filepath.Join(notADir, "child", "http-host.log")
	h := &HTTPHost{cfg: HTTPHostConfig{LogPath: logPath}}
	stdout, stderr, closer := h.openLogWriters()
	if stdout != io.Discard {
		t.Errorf("mkdir failure → stdout writer = %T, want io.Discard", stdout)
	}
	if stderr != io.Discard {
		t.Errorf("mkdir failure → stderr writer = %T, want io.Discard", stderr)
	}
	if closer != nil {
		t.Errorf("mkdir failure → closer != nil; want nil")
	}
}

// TestLogSupervisorEvent_NonTTYDiscardsStderrButAppendsToLogPath pins
// codex deep-sec PR #164 P3 closure: the durable file-side write
// continues even when stderr is non-TTY. The stderr leg is discarded;
// the LogPath append happens.
func TestLogSupervisorEvent_NonTTYDiscardsStderrButAppendsToLogPath(t *testing.T) {
	restore := SetStderrIsTerminalForTest(false)
	defer restore()
	logPath := filepath.Join(t.TempDir(), "supervisor.log")
	h := &StdioHost{cfg: HostConfig{LogPath: logPath}}
	h.LogSupervisorEvent("non-tty event payload")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if want := "supervisor: non-tty event payload"; !contains(string(data), want) {
		t.Errorf("LogPath missing %q after LogSupervisorEvent (non-TTY); content=%q", want, string(data))
	}
}

// TestLogSupervisorEvent_NoLogPathNonTTYIsBenign pins the no-op
// branch: when LogPath is empty + non-TTY, the call must not panic
// and must not leave any persistent state (the event is genuinely
// dropped — nothing the operator can read, by design).
func TestLogSupervisorEvent_NoLogPathNonTTYIsBenign(t *testing.T) {
	restore := SetStderrIsTerminalForTest(false)
	defer restore()
	h := &StdioHost{cfg: HostConfig{LogPath: ""}}
	// Should not panic.
	h.LogSupervisorEvent("dropped event")
}

// contains is a tiny helper for the LogPath assertion. Kept local
// to avoid pulling strings into this file.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestOpenLogWriters_LogPathSetNonTTY pins: HTTPHost LogPath set +
// non-TTY → both writers are the log file directly (no io.MultiWriter
// wrapping os.Stderr).
func TestOpenLogWriters_LogPathSetNonTTY(t *testing.T) {
	restore := SetStderrIsTerminalForTest(false)
	defer restore()
	logPath := filepath.Join(t.TempDir(), "http-host.log")
	h := &HTTPHost{cfg: HTTPHostConfig{LogPath: logPath}}
	stdout, stderr, closer := h.openLogWriters()
	if closer == nil {
		t.Fatal("LogPath set, non-TTY → closer is nil; want log-file Closer")
	}
	// stdout and stderr should both be the SAME log file (not an
	// io.MultiWriter wrapping os.Stderr; that path is reserved for
	// TTY). Identity check via *os.File: the non-TTY return shape is
	// `return logFile, logFile, logFile` so all three references are
	// the same underlying *os.File.
	if stdout != stderr {
		t.Errorf("LogPath set, non-TTY → stdout/stderr writers differ; want same *os.File")
	}
	stdoutFile, ok1 := stdout.(*os.File)
	closerFile, ok2 := closer.(*os.File)
	if !ok1 || !ok2 || stdoutFile != closerFile {
		t.Errorf("LogPath set, non-TTY → stdout (%T) and closer (%T) are not the same *os.File; non-TTY path must return the file directly without MultiWriter", stdout, closer)
	}
	_ = closer.Close()
}

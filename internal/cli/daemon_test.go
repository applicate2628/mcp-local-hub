package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestWriteLaunchFailure_AppendsTimestampedLine asserts the DM-3 helper
// writes a grep-able diagnostic line to the daemon log path. The line
// must include the server, daemon, and the underlying error so
// `mcphub status` users can find the cause when Task Scheduler shows
// last_result=1 with no other context.
func TestWriteLaunchFailure_AppendsTimestampedLine(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "logs", "serena-claude.log")

	writeLaunchFailure(logPath, "serena", "claude", errors.New("port 9121 already in use"))

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("expected log file at %s: %v", logPath, err)
	}
	got := string(data)
	for _, want := range []string{
		"[mcphub-launch-failure",
		"server=serena",
		"daemon=claude",
		"port 9121 already in use",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q; got: %q", want, got)
		}
	}
}

// TestWriteLaunchFailure_AppendsToExistingLog confirms a second call
// appends rather than truncates — important for the multi-retry
// scenario where Task Scheduler's RestartOnFailure fires the daemon
// 3 times in 3 minutes and we want every failure recorded.
func TestWriteLaunchFailure_AppendsToExistingLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "memory-default.log")

	// Pre-populate the log with arbitrary prior content (e.g. previous
	// healthy run's child stdout).
	priorContent := "prior child output line\n"
	if err := os.WriteFile(logPath, []byte(priorContent), 0600); err != nil {
		t.Fatal(err)
	}

	writeLaunchFailure(logPath, "memory", "default", errors.New("first failure"))
	writeLaunchFailure(logPath, "memory", "default", errors.New("second failure"))

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.HasPrefix(got, priorContent) {
		t.Errorf("prior content was overwritten; got: %q", got)
	}
	if !strings.Contains(got, "first failure") {
		t.Errorf("missing first failure line; got: %q", got)
	}
	if !strings.Contains(got, "second failure") {
		t.Errorf("missing second failure line; got: %q", got)
	}
}

// TestWriteLaunchFailure_SilentOnUnwritablePath asserts the helper
// does not panic and returns silently when the log directory cannot
// be created. The deferred wrapper must never compound the original
// launch error — its only job is best-effort diagnostic recording.
//
// On Unix we create an unwritable parent and try to mkdir under it.
// On Windows we use a path with NUL characters that os.MkdirAll
// rejects unconditionally. Either way: no panic, no return value.
func TestWriteLaunchFailure_SilentOnUnwritablePath(t *testing.T) {
	var bogusPath string
	switch runtime.GOOS {
	case "windows":
		// Path containing NUL character — illegal on every Windows API.
		bogusPath = "C:\\bogus\x00path\\daemon.log"
	default:
		// Read-only parent (chmod 0500 means no write permission for
		// the owner, so MkdirAll under it fails).
		parent := t.TempDir()
		if err := os.Chmod(parent, 0500); err != nil {
			t.Skipf("chmod not honored on this filesystem: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(parent, 0700) })
		bogusPath = filepath.Join(parent, "subdir", "daemon.log")
	}

	// Defer/recover: the helper must not panic even on a bad path.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("writeLaunchFailure panicked on bad path: %v", r)
		}
	}()
	writeLaunchFailure(bogusPath, "x", "y", errors.New("err"))
}

// TestWriteLaunchFailure_CreatesParentDir asserts the helper creates
// a missing parent directory rather than failing silently when only
// the parent is missing — the daemon log dir may not exist on the
// very first launch after install.
func TestWriteLaunchFailure_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "deeply", "nested", "path", "log.log")

	writeLaunchFailure(logPath, "s", "d", fmt.Errorf("boom"))

	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("expected log file to exist after writeLaunchFailure: %v", err)
	}
}

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestRunProcess_RunsInCwdAndSurfacesCleanExit proves the two load-bearing
// companion-spawn behaviors: (1) the child runs FROM WorkingDir — a command that
// writes a RELATIVE marker file lands it in cwd; (2) a clean self-exit returns
// non-nil so the supervisor respawns (a long-lived companion is not expected to
// exit on its own).
func TestRunProcess_RunsInCwdAndSurfacesCleanExit(t *testing.T) {
	cwd := t.TempDir()
	var cfg ProcessConfig
	if runtime.GOOS == "windows" {
		cfg = ProcessConfig{Command: "cmd", Args: []string{"/c", "echo ok> marker.txt"}, WorkingDir: cwd}
	} else {
		cfg = ProcessConfig{Command: "sh", Args: []string{"-c", "echo ok > marker.txt"}, WorkingDir: cwd}
	}
	err := RunProcess(context.Background(), cfg)
	if err == nil {
		t.Fatal("RunProcess on a clean self-exit must return non-nil so the supervisor respawns the companion")
	}
	if _, statErr := os.Stat(filepath.Join(cwd, "marker.txt")); statErr != nil {
		t.Errorf("companion did not run from WorkingDir (relative marker.txt absent in cwd): %v", statErr)
	}
}

// TestRunProcess_CtxCancelReturnsNil proves a supervisor stop (ctx cancel) is a
// graceful shutdown, not a failure: RunProcess kills the child and returns nil.
// Uses a DIRECT long-running command (no shell wrapper) so ctx-cancel kills the
// process itself — RunProcess has no Job Object of its own (in production the Job
// Object lives on the parent mcphub-daemon via the supervisor's StartWithJob), so
// a shell wrapper's grandchild would survive the kill and is out of test scope.
func TestRunProcess_CtxCancelReturnsNil(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var cfg ProcessConfig
	if runtime.GOOS == "windows" {
		cfg = ProcessConfig{Command: "ping", Args: []string{"-n", "30", "127.0.0.1"}}
	} else {
		cfg = ProcessConfig{Command: "sleep", Args: []string{"30"}}
	}
	done := make(chan error, 1)
	go func() { done <- RunProcess(ctx, cfg) }()
	time.Sleep(250 * time.Millisecond) // let the child start
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ctx cancel (supervisor stop) must return nil; got %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("RunProcess did not return after ctx cancel")
	}
}

// TestRunProcess_KillsWrapperGrandchildOnCancel proves the Job-Object tree-kill
// (Codex #381): a wrapper (cmd /c) that forks a long-running grandchild (ping) is
// FULLY reaped on ctx cancel — Wait returns promptly (within WaitDelay) instead of
// wedging on the inherited pipe, and the grandchild is terminated. If the
// grandchild leaked it would keep holding the temp WorkingDir, and t.TempDir's
// deferred RemoveAll cleanup would fail the test. Windows-only: the Job is a no-op
// stub on POSIX (process-group reaping there is a separate follow-up).
func TestRunProcess_KillsWrapperGrandchildOnCancel(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Job-Object tree-kill is Windows-only; POSIX process-group reaping is a follow-up")
	}
	cwd := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cfg := ProcessConfig{Command: "cmd", Args: []string{"/c", "ping -n 60 127.0.0.1 > nul"}, WorkingDir: cwd}
	done := make(chan error, 1)
	go func() { done <- RunProcess(ctx, cfg) }()
	time.Sleep(600 * time.Millisecond) // let cmd fork the ping grandchild
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ctx cancel must return nil; got %v", err)
		}
		if elapsed := time.Since(start); elapsed > companionWaitDelay+5*time.Second {
			t.Errorf("Wait took %v after cancel — the wrapper grandchild may have wedged it", elapsed)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("RunProcess did not return after cancel — wrapper grandchild wedged Wait")
	}
}

// TestRunProcess_EmptyCommandErrors guards the obvious misconfiguration.
func TestRunProcess_EmptyCommandErrors(t *testing.T) {
	if err := RunProcess(context.Background(), ProcessConfig{}); err == nil {
		t.Error("RunProcess with empty Command must return an error")
	}
}

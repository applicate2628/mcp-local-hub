package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// MCP-Reliability smoke harness. These tests build the real `mcphub`
// binary once and exercise it via OS subprocess so regressions in the
// build / launch / cobra wiring / defer chain that DON'T surface in
// in-process tests get caught before merge.
//
// Coverage scope (β-α hybrid; full happy-path bridge is a future
// follow-up — needs a `--manifest-path` hook into the daemon command
// or a PATH-shim fixture and is out of scope for this regression
// gate):
//
//   - Binary builds AND launches AND exits 0 on a known-good cmd
//     (catches go-build regressions, main.go os.Exit chain,
//     SetBuildInfo wiring).
//   - Bad-server daemon command exits non-zero within 5s AND writes
//     the launch-failure diagnostic to the per-daemon log
//     (catches DM-3a defer-wrap regression in real OS context;
//     in-process TestDaemonCmd_RunFailure_AppendsToLog covers the
//     same logic but cannot catch regressions in main()'s exit-code
//     translation, cobra's SilenceUsage interaction, or signal
//     handler installation in the real binary).
//   - Help system reachable on a real subcommand (catches cobra
//     subcommand registration regressions in NewRootCmd).
//
// Build-time impact: ~5-10s on cold cache for the one-time
// `go build`. Subsequent scenarios reuse mcphubBin via sync.Once.

var (
	mcphubBin       string
	mcphubBuildOnce sync.Once
	mcphubBuildErr  error
)

// ensureMcphubBinary builds cmd/mcphub once per test process and
// returns the binary path. Build errors fail the calling test rather
// than panic the process so the TestMain-less package contract holds.
func ensureMcphubBinary(t *testing.T) string {
	t.Helper()
	mcphubBuildOnce.Do(func() {
		f, err := os.CreateTemp("", "mcphub-reliability-*"+exeSuffix())
		if err != nil {
			mcphubBuildErr = fmt.Errorf("create tempfile: %w", err)
			return
		}
		binPath := f.Name()
		_ = f.Close()
		repoRoot, err := findRepoRootFromCWD()
		if err != nil {
			mcphubBuildErr = fmt.Errorf("locate repo root: %w", err)
			return
		}
		cmd := exec.Command("go", "build", "-o", binPath, "./cmd/mcphub")
		cmd.Dir = repoRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			mcphubBuildErr = fmt.Errorf("go build ./cmd/mcphub: %w\n%s", err, out)
			return
		}
		mcphubBin = binPath
	})
	if mcphubBuildErr != nil {
		t.Fatalf("mcphub binary build: %v", mcphubBuildErr)
	}
	return mcphubBin
}

// findRepoRootFromCWD walks up from the test's working directory
// (which `go test` sets to the package dir) looking for go.mod. The
// 8-level cap is loose insurance against accidental infinite walks
// from a corrupted filesystem layout.
func findRepoRootFromCWD() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := wd
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("go.mod not found above %s", wd)
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// TestReliability_VersionCommandExitsZero asserts the smallest
// possible end-to-end smoke: built binary launches and prints
// version output. If this fails, every downstream daemon usage is
// also broken, so put it first to fail fast on build/launch
// regressions before more elaborate scenarios run.
func TestReliability_VersionCommandExitsZero(t *testing.T) {
	bin := ensureMcphubBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("`mcphub version` exit: %v\nOutput:\n%s", err, out)
	}
	if !strings.Contains(string(out), "mcp-local-hub") {
		t.Errorf("version output missing 'mcp-local-hub' marker; got:\n%s", out)
	}
}

// TestReliability_DaemonCmdUnknownServerExitsNonZeroQuickly covers
// the DM-3a defer-wrap launch-failure path through a real OS
// subprocess. The 5s ContextWithTimeout catches cobra/defer-chain
// hang regressions; the log-file assertion catches defer-wrap
// regressions where the closure stops firing or stops writing.
//
// In-process TestDaemonCmd_RunFailure_AppendsToLog covers the same
// logic via cmd.Execute() but does not exercise main.go's os.Exit
// translation, cobra's SilenceErrors/SilenceUsage in the real launch
// path, or the OS-level exit-code propagation Task Scheduler
// observes. This subprocess version closes that gap.
func TestReliability_DaemonCmdUnknownServerExitsNonZeroQuickly(t *testing.T) {
	bin := ensureMcphubBinary(t)
	tmpHome := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin,
		"daemon",
		"--server", "definitely-no-such-server",
		"--daemon", "x",
	)
	// Redirect log destination to test-controlled tempdir so the
	// real %LOCALAPPDATA% / $XDG_STATE_HOME isn't polluted.
	cmd.Env = append(os.Environ(),
		"LOCALAPPDATA="+tmpHome,
		"XDG_STATE_HOME="+tmpHome,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit, got success. Output:\n%s", out)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("daemon command hung past 5s budget. Output:\n%s", out)
	}
	logPath := filepath.Join(tmpHome, "mcp-local-hub", "logs",
		"definitely-no-such-server-x.log")
	data, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("expected launch-failure log at %s: %v\nsubprocess output:\n%s",
			logPath, readErr, out)
	}
	if !strings.Contains(string(data), "[mcphub-launch-failure") {
		t.Errorf("launch-failure marker missing from log; got:\n%s", data)
	}
}

// TestReliability_HelpDaemonExitsZero asserts the help system reaches
// a real subcommand. cobra auto-wires help when RunE is nil, but a
// regression that breaks subcommand registration in NewRootCmd or
// panics inside a Long-text template would surface here. Cheap
// safety net for binary integrity.
func TestReliability_HelpDaemonExitsZero(t *testing.T) {
	bin := ensureMcphubBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "help", "daemon").CombinedOutput()
	if err != nil {
		t.Fatalf("`mcphub help daemon` exit: %v\nOutput:\n%s", err, out)
	}
	if !strings.Contains(string(out), "daemon") {
		t.Errorf("help output missing 'daemon' marker; got:\n%s", out)
	}
}

package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLaunch_EchoOutputIntoLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "out.log")
	spec := LaunchSpec{
		Command: func() string {
			if runtime.GOOS == "windows" {
				return "cmd"
			}
			return "sh"
		}(),
		Args: func() []string {
			if runtime.GOOS == "windows" {
				return []string{"/C", "echo hello-launcher"}
			}
			return []string{"-c", "echo hello-launcher"}
		}(),
		LogPath: logPath,
	}
	code, err := Launch(spec)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	raw, _ := os.ReadFile(logPath)
	if !strings.Contains(string(raw), "hello-launcher") {
		t.Errorf("log missing output, got: %q", raw)
	}
}

func TestLaunch_PropagatesExitCode(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "out.log")
	spec := LaunchSpec{
		Command: func() string {
			if runtime.GOOS == "windows" {
				return "cmd"
			}
			return "sh"
		}(),
		Args: func() []string {
			if runtime.GOOS == "windows" {
				return []string{"/C", "exit 7"}
			}
			return []string{"-c", "exit 7"}
		}(),
		LogPath: logPath,
	}
	code, _ := Launch(spec)
	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
}

package gui

import (
	"runtime"
	"strings"
	"testing"
)

func TestAppDataDir_ReturnsUserWriteablePath(t *testing.T) {
	got, err := AppDataDir()
	if err != nil {
		t.Fatalf("AppDataDir: %v", err)
	}
	if got == "" {
		t.Fatal("empty path")
	}
	if runtime.GOOS == "windows" && !strings.Contains(strings.ToLower(got), "appdata") {
		t.Errorf("windows path should include AppData: %q", got)
	}
	if !strings.HasSuffix(got, "mcp-local-hub") {
		t.Errorf("path should end with mcp-local-hub: %q", got)
	}
}

func TestPidportPath_IsUnderAppData(t *testing.T) {
	got, err := PidportPath()
	if err != nil {
		t.Fatalf("PidportPath: %v", err)
	}
	if !strings.HasSuffix(got, "gui.pidport") {
		t.Errorf("path should end with gui.pidport: %q", got)
	}
}

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/gui"
)

func TestResolveInstallGUIPortReadsRedirectedPidport(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", root)
	t.Setenv("XDG_STATE_HOME", root)
	t.Setenv("HOME", root)

	if got := resolveInstallGUIPort(); got != 0 {
		t.Fatalf("resolveInstallGUIPort() with no pidport = %d, want 0", got)
	}

	appDir := filepath.Join(root, "mcp-local-hub")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatalf("mkdir app dir: %v", err)
	}
	pidport := filepath.Join(appDir, "gui.pidport")
	if err := gui.WritePidport(pidport, os.Getpid(), 9125); err != nil {
		t.Fatalf("write pidport: %v", err)
	}

	if got := resolveInstallGUIPort(); got != 9125 {
		t.Fatalf("resolveInstallGUIPort() = %d, want 9125", got)
	}
}

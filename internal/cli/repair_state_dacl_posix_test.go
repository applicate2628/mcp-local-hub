//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

func TestRepairStateDACL_PathSymlinkEscapeRefusedAndOutsideModeUnchanged(t *testing.T) {
	stateDir := t.TempDir()
	restore := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restore)

	outsideRoot := t.TempDir()
	outsideDir := filepath.Join(outsideRoot, "nested")
	if err := os.Mkdir(outsideDir, 0o700); err != nil {
		t.Fatalf("mkdir outside nested dir: %v", err)
	}
	outside := filepath.Join(outsideDir, "workspaces.yaml")
	if err := os.WriteFile(outside, []byte("version: 1\nworkspaces: []\n"), 0o666); err != nil {
		t.Fatalf("write outside target: %v", err)
	}
	if err := os.Chmod(outside, 0o666); err != nil {
		t.Fatalf("chmod outside target: %v", err)
	}
	if err := os.Symlink(outsideRoot, filepath.Join(stateDir, "link")); err != nil {
		t.Fatalf("symlink escape fixture: %v", err)
	}

	stdout, stderr, err := runRepairStateDACLCmd(t, "", "--path", filepath.Join("link", "nested", "workspaces.yaml"), "--yes")
	if err == nil {
		t.Fatalf("repair-state-dacl accepted symlink escape; stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(stderr, "refused:") {
		t.Fatalf("stderr = %q, want refused line", stderr)
	}
	info, statErr := os.Stat(outside)
	if statErr != nil {
		t.Fatalf("stat outside target: %v", statErr)
	}
	if got := info.Mode().Perm(); got != 0o666 {
		t.Fatalf("outside target mode changed to %04o, want unchanged 0666", got)
	}
}

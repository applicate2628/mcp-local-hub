//go:build windows

package api

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/autostart"
)

func TestGlobalInstallAutostartUsesCanonicalProductEntrypoint(t *testing.T) {
	canonical := filepath.Join(t.TempDir(), MCPHubBinaryName())
	if err := os.WriteFile(canonical, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(SetTestCanonicalMcphubPath(canonical))
	fb := &fakeInstallAutostartBackend{statusReturn: autostart.StateDrifted}
	installFakeAutostartBackend(t, fb)
	ensureGlobalInstallAutostartOwner(io.Discard, false, OwnerModeGUI)
	want := canonical
	if len(fb.statusOpts) != 1 || len(fb.enableOpts) != 1 || fb.statusOpts[0].MCPHubPath != want || fb.enableOpts[0].MCPHubPath != want {
		t.Fatalf("status=%v enable=%v want=%q", fb.statusOpts, fb.enableOpts, want)
	}
}

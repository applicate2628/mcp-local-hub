package portresolution

import (
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestR20IndividualPortOverlayWinsBeforeBuiltin(t *testing.T) {
	root := t.TempDir()
	overlayPort := filepath.Join(root, "overlays", "zlib")
	builtinPort := filepath.Join(root, "ports", "zlib")
	for _, dir := range []string{overlayPort, builtinPort} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "portfile.cmake"), []byte("# port\n"), 0o644); err != nil {
			t.Fatalf("write portfile: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "vcpkg.json"), []byte(`{"name":"zlib","version":"1"}`), 0o644); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
	}
	res := ResolvePort(Args{Port: "zlib", VcpkgRoot: root, OverlayPorts: []string{overlayPort}}, DefaultDeps())
	if res.Status != evidence.StatusOK || res.Winner == nil || res.Winner.Directory != overlayPort {
		t.Fatalf("individual overlay did not win: %+v", res)
	}
}

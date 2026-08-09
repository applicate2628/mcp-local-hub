package discovery

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestR20PathLookupResolvesExecutableSymlinkBeforeRoot(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "opt", "vcpkg")
	shimDir := filepath.Join(base, "usr", "local", "bin")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatalf("mkdir real root: %v", err)
	}
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatalf("mkdir shim dir: %v", err)
	}
	realBinary := filepath.Join(realRoot, vcpkgBinaryName(runtimeGOOSForTest()))
	if err := os.WriteFile(realBinary, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write real executable: %v", err)
	}
	shim := filepath.Join(shimDir, vcpkgBinaryName(runtimeGOOSForTest()))
	if err := os.Symlink(realBinary, shim); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	deps := DefaultDeps()
	deps.Getenv = func(string) string { return "" }
	deps.LookPath = func(string) (string, error) { return shim, nil }
	deps.Getwd = func() (string, error) { return "", errors.New("no cwd") }
	deps.UserHomeDir = func() (string, error) { return "", errors.New("no home") }
	res := DiscoverRoot("", deps)
	expectedRoot, err := filepath.EvalSymlinks(realRoot)
	if err != nil {
		t.Fatalf("resolve expected root: %v", err)
	}
	if res.Status != evidence.StatusOK || res.RuleFired != RulePath || res.Root != expectedRoot {
		t.Fatalf("PATH symlink resolved %+v, want root %q", res, expectedRoot)
	}
}

func runtimeGOOSForTest() string {
	return DefaultDeps().GOOS
}

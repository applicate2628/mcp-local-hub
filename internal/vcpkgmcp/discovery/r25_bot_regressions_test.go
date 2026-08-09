package discovery

import (
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestR25PathLookupRootIsMadeAbsolute(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "workspace")
	deps := Deps{
		Getenv:       func(string) string { return "" },
		LookPath:     func(string) (string, error) { return "vcpkg", nil },
		EvalSymlinks: func(path string) (string, error) { return path, nil },
		Getwd:        func() (string, error) { return cwd, nil },
		Stat: func(path string) (os.FileInfo, error) {
			if path == filepath.Join(cwd, "vcpkg") {
				return fakeFileInfo{name: "vcpkg"}, nil
			}
			return nil, os.ErrNotExist
		},
	}
	result := DiscoverRoot("", deps)
	if result.Status != evidence.StatusOK || result.RuleFired != RulePath || result.Root != cwd || !filepath.IsAbs(result.Root) {
		t.Fatalf("result=%+v, want absolute PATH root %q", result, cwd)
	}
}

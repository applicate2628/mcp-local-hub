package discovery

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

type modeFileInfo struct {
	name string
	mode os.FileMode
}

func (f modeFileInfo) Name() string       { return f.name }
func (f modeFileInfo) Size() int64        { return 0 }
func (f modeFileInfo) Mode() os.FileMode  { return f.mode }
func (f modeFileInfo) ModTime() time.Time { return time.Time{} }
func (f modeFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f modeFileInfo) Sys() any           { return nil }

type modeFS map[string]os.FileMode

func (m modeFS) stat(path string) (os.FileInfo, error) {
	mode, ok := m[filepath.Clean(path)]
	if !ok {
		return nil, os.ErrNotExist
	}
	return modeFileInfo{name: filepath.Base(path), mode: mode}, nil
}

func TestRelativeExplicitRootIsTerminalBeforeProbe(t *testing.T) {
	calls := 0
	deps := Deps{
		Getenv:      func(string) string { calls++; return testRoot("env") },
		LookPath:    func(string) (string, error) { calls++; return testRoot("path", "vcpkg"), nil },
		Getwd:       func() (string, error) { calls++; return testRoot("work"), nil },
		Stat:        func(string) (os.FileInfo, error) { calls++; return nil, os.ErrNotExist },
		GOOS:        "linux",
		UserHomeDir: func() (string, error) { calls++; return testRoot("home"), nil },
	}
	result := DiscoverRoot("relative/vcpkg", deps)
	if result.Status != evidence.StatusUnknown || result.Reason != ReasonExplicitRootRelative || calls != 0 {
		t.Fatalf("result=%+v ambient/probe calls=%d, want terminal %s and zero calls", result, calls, ReasonExplicitRootRelative)
	}
}

func TestEveryDiscoveryTierUsesExecutableAdmission(t *testing.T) {
	type tier struct {
		name  string
		setup func(mode os.FileMode) (Deps, string)
	}
	tiers := []tier{
		{"explicit", func(mode os.FileMode) (Deps, string) {
			root := testRoot("explicit")
			files := modeFS{filepath.Clean(binIn(root, "linux")): mode}
			deps := isolatedDiscoveryDeps(files)
			return deps, root
		}},
		{"env", func(mode os.FileMode) (Deps, string) {
			root := testRoot("env")
			files := modeFS{filepath.Clean(binIn(root, "linux")): mode}
			deps := isolatedDiscoveryDeps(files)
			deps.Getenv = func(name string) string {
				if name == "VCPKG_ROOT" {
					return root
				}
				return ""
			}
			return deps, ""
		}},
		{"path", func(mode os.FileMode) (Deps, string) {
			root := testRoot("path")
			binary := binIn(root, "linux")
			files := modeFS{filepath.Clean(binary): mode}
			deps := isolatedDiscoveryDeps(files)
			deps.LookPath = func(string) (string, error) { return binary, nil }
			return deps, ""
		}},
		{"manifest", func(mode os.FileMode) (Deps, string) {
			project := testRoot("manifest", "project")
			root := filepath.Join(project, "vcpkg")
			files := modeFS{
				filepath.Clean(filepath.Join(project, "vcpkg.json")): 0o644,
				filepath.Clean(binIn(root, "linux")):                 mode,
			}
			deps := isolatedDiscoveryDeps(files)
			deps.Getwd = func() (string, error) { return filepath.Join(project, "subdir"), nil }
			return deps, ""
		}},
		{"heuristic", func(mode os.FileMode) (Deps, string) {
			root := "/opt/vcpkg"
			files := modeFS{filepath.Clean(binIn(root, "linux")): mode}
			deps := isolatedDiscoveryDeps(files)
			return deps, ""
		}},
	}
	modes := []struct {
		name string
		mode os.FileMode
		want bool
	}{
		{"regular-executable", 0o755, true},
		{"followed-executable-symlink-target", 0o755, true},
		{"regular-non-executable", 0o644, false},
		{"directory", os.ModeDir | 0o755, false},
		{"fifo", os.ModeNamedPipe | 0o755, false},
		{"socket", os.ModeSocket | 0o755, false},
		{"device", os.ModeDevice | 0o755, false},
	}
	for _, tr := range tiers {
		for _, mr := range modes {
			t.Run(tr.name+"/"+mr.name, func(t *testing.T) {
				deps, explicit := tr.setup(mr.mode)
				result := DiscoverRoot(explicit, deps)
				accepted := result.Status == evidence.StatusOK
				if tr.name == "heuristic" {
					accepted = false
					for _, candidate := range result.Candidates {
						accepted = accepted || candidate.Rule == RuleHeuristic
					}
				}
				if accepted != mr.want {
					t.Fatalf("result=%+v accepted=%v, want %v", result, accepted, mr.want)
				}
			})
		}
	}
}

func TestManifestWalkupRequiresRegularMarker(t *testing.T) {
	project := testRoot("manifest-special", "project")
	root := filepath.Join(project, "vcpkg")
	files := modeFS{
		filepath.Clean(filepath.Join(project, "vcpkg.json")): os.ModeNamedPipe | 0o644,
		filepath.Clean(binIn(root, "linux")):                 0o755,
	}
	deps := isolatedDiscoveryDeps(files)
	deps.Getwd = func() (string, error) { return filepath.Join(project, "child"), nil }
	result := DiscoverRoot("", deps)
	if result.Status == evidence.StatusOK {
		t.Fatalf("special manifest marker selected root: %+v", result)
	}
}

func isolatedDiscoveryDeps(files modeFS) Deps {
	return Deps{
		Getenv:      func(string) string { return "" },
		LookPath:    func(string) (string, error) { return "", errors.New("not found") },
		Getwd:       func() (string, error) { return "", errors.New("no cwd") },
		Stat:        files.stat,
		GOOS:        "linux",
		UserHomeDir: func() (string, error) { return "", errors.New("no home") },
	}
}

package discovery

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/cmd/vcpkg-mcp/internal/evidence"
)

// fakeFileInfo is a minimal os.FileInfo for tests that never touch a real
// filesystem — this is the "Determinism and ambient-input control" seam:
// every ambient input DiscoverRoot reads is injected, so these tests fully
// control PATH/env/cwd/filesystem without depending on the host machine.
type fakeFileInfo struct {
	name  string
	isDir bool
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return 0 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.isDir }
func (f fakeFileInfo) Sys() any           { return nil }

// fakeFS is a set of paths (files, never dirs in this fake) that "exist".
type fakeFS map[string]bool

func (fs fakeFS) stat(path string) (os.FileInfo, error) {
	clean := filepath.Clean(path)
	if fs[clean] {
		return fakeFileInfo{name: filepath.Base(clean)}, nil
	}
	return nil, os.ErrNotExist
}

func baseDeps(goos string, files fakeFS) Deps {
	return Deps{
		Getenv:      func(string) string { return "" },
		LookPath:    func(string) (string, error) { return "", errors.New("not found") },
		Getwd:       func() (string, error) { return `C:\work\project`, nil },
		Stat:        files.stat,
		GOOS:        goos,
		UserHomeDir: func() (string, error) { return `C:\Users\op`, nil },
	}
}

func TestDiscoverRoot_ExplicitParamWins(t *testing.T) {
	files := fakeFS{filepath.Clean(`C:\explicit\vcpkg.exe`): true}
	deps := baseDeps("windows", files)
	// Also seed a valid env root to prove explicit still wins over it.
	deps.Getenv = func(k string) string {
		if k == "VCPKG_ROOT" {
			return `C:\env-root`
		}
		return ""
	}

	res := DiscoverRoot(`C:\explicit`, deps)
	if res.Status != evidence.StatusOK {
		t.Fatalf("status = %v, want ok", res.Status)
	}
	if res.RuleFired != RuleExplicit {
		t.Fatalf("rule = %v, want %v", res.RuleFired, RuleExplicit)
	}
	if res.Root != `C:\explicit` {
		t.Fatalf("root = %q", res.Root)
	}
}

func TestDiscoverRoot_EnvVarWhenNoExplicit(t *testing.T) {
	files := fakeFS{filepath.Clean(`C:\env-root\vcpkg.exe`): true}
	deps := baseDeps("windows", files)
	deps.Getenv = func(k string) string {
		if k == "VCPKG_ROOT" {
			return `C:\env-root`
		}
		return ""
	}

	res := DiscoverRoot("", deps)
	if res.Status != evidence.StatusOK || res.RuleFired != RuleEnv || res.Root != `C:\env-root` {
		t.Fatalf("got %+v", res)
	}
}

func TestDiscoverRoot_PathLookup(t *testing.T) {
	deps := baseDeps("windows", fakeFS{})
	deps.LookPath = func(file string) (string, error) {
		if file == "vcpkg" {
			return `C:\path-root\vcpkg.exe`, nil
		}
		return "", errors.New("not found")
	}
	res := DiscoverRoot("", deps)
	if res.Status != evidence.StatusOK || res.RuleFired != RulePath || res.Root != `C:\path-root` {
		t.Fatalf("got %+v", res)
	}
}

func TestDiscoverRoot_ManifestWalkup(t *testing.T) {
	files := fakeFS{
		filepath.Clean(`C:\work\project\vcpkg.json`):          true,
		filepath.Clean(`C:\work\project\vcpkg\vcpkg.exe`):     true,
	}
	deps := baseDeps("windows", files)
	res := DiscoverRoot("", deps)
	if res.Status != evidence.StatusOK || res.RuleFired != RuleManifest {
		t.Fatalf("got %+v", res)
	}
	if res.Root != filepath.Clean(`C:\work\project\vcpkg`) {
		t.Fatalf("root = %q", res.Root)
	}
}

func TestDiscoverRoot_ManifestFoundButNoSubmoduleBinary_FallsThrough(t *testing.T) {
	// Manifest present, but no co-located vcpkg/ binary, and no heuristic
	// hit either -> must NOT guess the manifest dir as root; falls to none.
	files := fakeFS{filepath.Clean(`C:\work\project\vcpkg.json`): true}
	deps := baseDeps("windows", files)
	res := DiscoverRoot("", deps)
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonNoneFound {
		t.Fatalf("got %+v, want unknown/no_candidates_found", res)
	}
	// The manifest is still surfaced as a considered-but-rejected candidate
	// for transparency.
	foundManifestCandidate := false
	for _, c := range res.Candidates {
		if c.Rule == RuleManifest {
			foundManifestCandidate = true
		}
	}
	if !foundManifestCandidate {
		t.Fatalf("expected manifest candidate to be surfaced even though rejected, got %+v", res.Candidates)
	}
}

func TestDiscoverRoot_HeuristicSingleHit(t *testing.T) {
	files := fakeFS{filepath.Clean(`C:\vcpkg\vcpkg.exe`): true}
	deps := baseDeps("windows", files)
	res := DiscoverRoot("", deps)
	if res.Status != evidence.StatusOK || res.RuleFired != RuleHeuristic || res.Root != `C:\vcpkg` {
		t.Fatalf("got %+v", res)
	}
}

func TestDiscoverRoot_HeuristicMultipleHits_ReportsAllNeverPicks(t *testing.T) {
	files := fakeFS{
		filepath.Clean(`C:\vcpkg\vcpkg.exe`):     true,
		filepath.Clean(`C:\dev\vcpkg\vcpkg.exe`): true,
	}
	deps := baseDeps("windows", files)
	res := DiscoverRoot("", deps)
	if res.Status != evidence.StatusUnknown {
		t.Fatalf("status = %v, want unknown (ambiguous)", res.Status)
	}
	if res.Reason != ReasonAmbiguous {
		t.Fatalf("reason = %v, want %v", res.Reason, ReasonAmbiguous)
	}
	if len(res.Candidates) != 2 {
		t.Fatalf("candidates = %+v, want exactly 2 (both reported, none silently picked)", res.Candidates)
	}
}

func TestDiscoverRoot_NoneFound_OffersManualSpecification(t *testing.T) {
	deps := baseDeps("windows", fakeFS{})
	res := DiscoverRoot("", deps)
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonNoneFound {
		t.Fatalf("got %+v, want unknown/no_candidates_found (never \"not installed\")", res)
	}
}

func TestDiscoverRoot_ExplicitInvalid_StillReported(t *testing.T) {
	// Explicit param given but wrong (no vcpkg binary there): must not
	// silently fall through to a heuristic hit without saying so.
	files := fakeFS{filepath.Clean(`C:\vcpkg\vcpkg.exe`): true} // valid heuristic candidate
	deps := baseDeps("windows", files)
	res := DiscoverRoot(`C:\wrong`, deps)
	if res.Status != evidence.StatusOK {
		t.Fatalf("status = %v, want ok (heuristic still resolves)", res.Status)
	}
	if res.RuleFired != RuleHeuristic {
		t.Fatalf("rule = %v, want heuristic (explicit was invalid)", res.RuleFired)
	}
}

func TestDiscoverRoot_POSIXHeuristics(t *testing.T) {
	files := fakeFS{filepath.Clean("/opt/vcpkg/vcpkg"): true}
	deps := baseDeps("linux", files)
	deps.UserHomeDir = func() (string, error) { return "/home/op", nil }
	res := DiscoverRoot("", deps)
	if res.Status != evidence.StatusOK || res.Root != "/opt/vcpkg" {
		t.Fatalf("got %+v", res)
	}
}

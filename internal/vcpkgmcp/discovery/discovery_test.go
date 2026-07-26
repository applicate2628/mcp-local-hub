package discovery

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
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

// errPermission is a non-ErrNotExist Stat failure, the shape a permission
// denial / sharing violation / transient I/O error takes. It must never be
// read as "absent".
var errPermission = fs.ErrPermission

// unreadableFS makes every Stat under one prefix fail with a non-ENOENT
// error, so tests can exercise the "the OS refused to tell us" branch.
type unreadableFS struct {
	base   fakeFS
	prefix string
}

func (u unreadableFS) stat(path string) (os.FileInfo, error) {
	if strings.HasPrefix(filepath.Clean(path), filepath.Clean(u.prefix)) {
		return nil, errPermission
	}
	return u.base.stat(path)
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
		filepath.Clean(`C:\work\project\vcpkg.json`):      true,
		filepath.Clean(`C:\work\project\vcpkg\vcpkg.exe`): true,
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

// TestDiscoverRoot_HeuristicSingleHit_NeverSelected is the mutation proof
// for "a heuristic NEVER selects": a single hardcoded machine-layout match
// is a candidate to confirm, not an authoritative root. Before this, one
// unrelated C:\vcpkg on the box made the tool answer ok and sent every
// downstream tool to analyse that installation.
func TestDiscoverRoot_HeuristicSingleHit_NeverSelected(t *testing.T) {
	files := fakeFS{filepath.Clean(`C:\vcpkg\vcpkg.exe`): true}
	deps := baseDeps("windows", files)
	res := DiscoverRoot("", deps)
	if res.Status != evidence.StatusUnknown {
		t.Fatalf("status = %v, want unknown — a heuristic match is never an authoritative root", res.Status)
	}
	if res.Reason != ReasonHeuristicOnly {
		t.Fatalf("reason = %v, want %v", res.Reason, ReasonHeuristicOnly)
	}
	if res.Root != "" {
		t.Fatalf("root = %q, want empty — nothing was SELECTED", res.Root)
	}
	if res.RuleFired != "" {
		t.Fatalf("rule_fired = %q, want empty — no rule concluded", res.RuleFired)
	}
	// The candidate must still be surfaced so the caller can confirm it.
	if len(res.Candidates) != 1 || res.Candidates[0].Path != `C:\vcpkg` ||
		res.Candidates[0].Rule != RuleHeuristic {
		t.Fatalf("candidates = %+v, want the single heuristic candidate reported", res.Candidates)
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

// TestDiscoverRoot_ExplicitInvalid_IsTerminal_NeverFallsThrough is the
// mutation proof for "an explicit root is TERMINAL". The caller asks about
// C:\wanted; a valid VCPKG_ROOT AND a valid heuristic hit both point
// somewhere else. Answering ok for either would silently redirect every
// downstream tool to an installation the caller never named.
func TestDiscoverRoot_ExplicitInvalid_IsTerminal_NeverFallsThrough(t *testing.T) {
	files := fakeFS{
		filepath.Clean(`D:\other\vcpkg.exe`): true, // valid env root
		filepath.Clean(`C:\vcpkg\vcpkg.exe`): true, // valid heuristic candidate
	}
	deps := baseDeps("windows", files)
	deps.Getenv = func(k string) string {
		if k == "VCPKG_ROOT" {
			return `D:\other`
		}
		return ""
	}
	deps.LookPath = func(string) (string, error) { return `D:\other\vcpkg.exe`, nil }

	res := DiscoverRoot(`C:\wanted`, deps)
	if res.Status != evidence.StatusUnknown {
		t.Fatalf("status = %v, want unknown — an invalid EXPLICIT root is terminal", res.Status)
	}
	if res.Reason != ReasonExplicitRootInvalid {
		t.Fatalf("reason = %v, want %v", res.Reason, ReasonExplicitRootInvalid)
	}
	if res.Root != "" {
		t.Fatalf("root = %q, want empty — resolving ANY other installation answers a question nobody asked", res.Root)
	}
	// The rejected explicit value must be the one thing reported back.
	if len(res.Candidates) != 1 || res.Candidates[0].Path != `C:\wanted` ||
		res.Candidates[0].Rule != RuleExplicit {
		t.Fatalf("candidates = %+v, want exactly the rejected explicit root", res.Candidates)
	}
	for _, c := range res.Candidates {
		if c.Rule == RuleEnv || c.Rule == RulePath || c.Rule == RuleHeuristic {
			t.Fatalf("lower-precedence rule %v was evaluated despite an explicit root", c.Rule)
		}
	}
}

// TestDiscoverRoot_ExplicitUnreadable_DistinctFromInvalid: a probe that
// FAILS is not evidence of absence, and the two cases have different
// remedies (fix access vs. correct the path), so they get different reasons.
func TestDiscoverRoot_ExplicitUnreadable_DistinctFromInvalid(t *testing.T) {
	deps := baseDeps("windows", fakeFS{})
	deps.Stat = unreadableFS{base: fakeFS{}, prefix: `C:\locked`}.stat

	res := DiscoverRoot(`C:\locked`, deps)
	if res.Status != evidence.StatusUnknown {
		t.Fatalf("status = %v, want unknown", res.Status)
	}
	if res.Reason != ReasonExplicitRootUnreadable {
		t.Fatalf("reason = %v, want %v (an unreadable probe must never be reported as an invalid/absent root)",
			res.Reason, ReasonExplicitRootUnreadable)
	}
}

func TestDiscoverRoot_POSIXHeuristics_NeverSelected(t *testing.T) {
	files := fakeFS{filepath.Clean("/opt/vcpkg/vcpkg"): true}
	deps := baseDeps("linux", files)
	deps.UserHomeDir = func() (string, error) { return "/home/op", nil }
	res := DiscoverRoot("", deps)
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonHeuristicOnly {
		t.Fatalf("got status=%v reason=%v, want unknown/heuristic_only", res.Status, res.Reason)
	}
	if len(res.Candidates) != 1 || res.Candidates[0].Path != "/opt/vcpkg" {
		t.Fatalf("candidates = %+v, want /opt/vcpkg reported as a candidate", res.Candidates)
	}
}

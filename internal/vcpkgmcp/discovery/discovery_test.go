package discovery

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// PORTABILITY (PR #591 P1)
//
// These tests inject GOOS, so they deliberately exercise the WINDOWS rules
// (vcpkg.exe, the C:\vcpkg heuristic list) on whatever host runs them — that
// injected-GOOS logic is the thing under test and it is worth covering on the
// ubuntu-latest leg of the build-and-test matrix, not just the windows one.
//
// What is NOT injected is `path/filepath`, which follows the HOST. Fixture
// paths were previously hand-typed with backslashes (`C:\explicit\vcpkg.exe`),
// so on Linux `filepath.Clean` left them as ONE opaque component while the
// production code computed `filepath.Join(root, "vcpkg.exe")` = a different
// string — the probe missed and the tests failed. `filepath.Dir` on such a
// path returns "." on Linux, which also broke the manifest walk-up.
//
// The fix is to build every fixture path through the SAME filepath helpers the
// production code uses, from an OS-appropriate absolute base (testRoot). Both
// sides then agree on any host, and no coverage is lost.
//
// Build tags were rejected: `//go:build windows` would silence these tests on
// Linux entirely, deleting the ubuntu coverage of injected-GOOS logic that has
// nothing host-specific about it. The defect was hand-typed separators, not the
// tests' subject matter.
//
// The heuristic literals (`C:\vcpkg`, `/opt/vcpkg`) are deliberately kept
// verbatim where a test asserts a CANDIDATE PATH: those strings are emitted by
// heuristicPathsFor itself, so the assertion must compare against the same
// literal. Only the derived BINARY path is built with filepath.Join.

// testRoot builds an absolute fixture path that is well-formed on the host, so
// the package's own filepath.Join/Dir/Clean calls agree with it.
func testRoot(parts ...string) string {
	base := "/fixture"
	if runtime.GOOS == "windows" {
		base = `C:\fixture`
	}
	return filepath.Join(append([]string{base}, parts...)...)
}

// binIn returns the path the package will probe for a vcpkg binary under dir,
// computed exactly as probeVcpkgBinary computes it.
func binIn(dir, goos string) string {
	return filepath.Join(dir, vcpkgBinaryName(goos))
}

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

// newFakeFS builds a fakeFS whose keys are cleaned the same way stat cleans
// its lookups, so a caller never has to remember to do it.
func newFakeFS(paths ...string) fakeFS {
	out := fakeFS{}
	for _, p := range paths {
		out[filepath.Clean(p)] = true
	}
	return out
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
		Getwd:       func() (string, error) { return testRoot("work", "project"), nil },
		Stat:        files.stat,
		GOOS:        goos,
		UserHomeDir: func() (string, error) { return testRoot("Users", "op"), nil },
	}
}

func TestDiscoverRoot_ExplicitParamWins(t *testing.T) {
	explicit := testRoot("explicit")
	envRoot := testRoot("env-root")
	files := newFakeFS(binIn(explicit, "windows"))
	deps := baseDeps("windows", files)
	// Also seed a valid env root to prove explicit still wins over it.
	deps.Getenv = func(k string) string {
		if k == "VCPKG_ROOT" {
			return envRoot
		}
		return ""
	}

	res := DiscoverRoot(explicit, deps)
	if res.Status != evidence.StatusOK {
		t.Fatalf("status = %v, want ok", res.Status)
	}
	if res.RuleFired != RuleExplicit {
		t.Fatalf("rule = %v, want %v", res.RuleFired, RuleExplicit)
	}
	if res.Root != explicit {
		t.Fatalf("root = %q, want %q", res.Root, explicit)
	}
}

func TestDiscoverRoot_EnvVarWhenNoExplicit(t *testing.T) {
	envRoot := testRoot("env-root")
	files := newFakeFS(binIn(envRoot, "windows"))
	deps := baseDeps("windows", files)
	deps.Getenv = func(k string) string {
		if k == "VCPKG_ROOT" {
			return envRoot
		}
		return ""
	}

	res := DiscoverRoot("", deps)
	if res.Status != evidence.StatusOK || res.RuleFired != RuleEnv || res.Root != envRoot {
		t.Fatalf("got %+v, want ok/vcpkg_root_env/%q", res, envRoot)
	}
}

func TestDiscoverRoot_PathLookup(t *testing.T) {
	pathRoot := testRoot("path-root")
	deps := baseDeps("windows", newFakeFS())
	deps.LookPath = func(file string) (string, error) {
		if file == "vcpkg" {
			return binIn(pathRoot, "windows"), nil
		}
		return "", errors.New("not found")
	}
	res := DiscoverRoot("", deps)
	if res.Status != evidence.StatusOK || res.RuleFired != RulePath || res.Root != pathRoot {
		t.Fatalf("got %+v, want ok/path_lookup/%q", res, pathRoot)
	}
}

func TestDiscoverRoot_ManifestWalkup(t *testing.T) {
	projectDir := testRoot("work", "project")
	submodule := filepath.Join(projectDir, "vcpkg")
	files := newFakeFS(
		filepath.Join(projectDir, "vcpkg.json"),
		binIn(submodule, "windows"),
	)
	deps := baseDeps("windows", files)
	res := DiscoverRoot("", deps)
	if res.Status != evidence.StatusOK || res.RuleFired != RuleManifest {
		t.Fatalf("got %+v, want ok/manifest_walkup", res)
	}
	if res.Root != submodule {
		t.Fatalf("root = %q, want %q", res.Root, submodule)
	}
}

func TestDiscoverRoot_ManifestFoundButNoSubmoduleBinary_FallsThrough(t *testing.T) {
	// Manifest present, but no co-located vcpkg/ binary, and no heuristic
	// hit either -> must NOT guess the manifest dir as root; falls to none.
	projectDir := testRoot("work", "project")
	files := newFakeFS(filepath.Join(projectDir, "vcpkg.json"))
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
	// `C:\vcpkg` is the literal heuristicPathsFor itself emits, so the
	// candidate assertion must compare against it verbatim; only the derived
	// binary path is joined.
	const heuristic = `C:\vcpkg`
	files := newFakeFS(binIn(heuristic, "windows"))
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
	if len(res.Candidates) != 1 || res.Candidates[0].Path != heuristic ||
		res.Candidates[0].Rule != RuleHeuristic {
		t.Fatalf("candidates = %+v, want the single heuristic candidate reported", res.Candidates)
	}
}

func TestDiscoverRoot_HeuristicMultipleHits_ReportsAllNeverPicks(t *testing.T) {
	files := newFakeFS(
		binIn(`C:\vcpkg`, "windows"),
		binIn(`C:\dev\vcpkg`, "windows"),
	)
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
	deps := baseDeps("windows", newFakeFS())
	res := DiscoverRoot("", deps)
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonNoneFound {
		t.Fatalf("got %+v, want unknown/no_candidates_found (never \"not installed\")", res)
	}
}

// TestDiscoverRoot_ExplicitInvalid_IsTerminal_NeverFallsThrough is the
// mutation proof for "an explicit root is TERMINAL". The caller asks about
// a root; a valid VCPKG_ROOT AND a valid heuristic hit both point somewhere
// else. Answering ok for either would silently redirect every downstream tool
// to an installation the caller never named.
func TestDiscoverRoot_ExplicitInvalid_IsTerminal_NeverFallsThrough(t *testing.T) {
	wanted := testRoot("wanted")
	other := testRoot("other")
	files := newFakeFS(
		binIn(other, "windows"),      // valid env root
		binIn(`C:\vcpkg`, "windows"), // valid heuristic candidate
	)
	deps := baseDeps("windows", files)
	deps.Getenv = func(k string) string {
		if k == "VCPKG_ROOT" {
			return other
		}
		return ""
	}
	deps.LookPath = func(string) (string, error) { return binIn(other, "windows"), nil }

	res := DiscoverRoot(wanted, deps)
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
	if len(res.Candidates) != 1 || res.Candidates[0].Path != wanted ||
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
	locked := testRoot("locked")
	deps := baseDeps("windows", newFakeFS())
	deps.Stat = unreadableFS{base: newFakeFS(), prefix: locked}.stat

	res := DiscoverRoot(locked, deps)
	if res.Status != evidence.StatusUnknown {
		t.Fatalf("status = %v, want unknown", res.Status)
	}
	if res.Reason != ReasonExplicitRootUnreadable {
		t.Fatalf("reason = %v, want %v (an unreadable probe must never be reported as an invalid/absent root)",
			res.Reason, ReasonExplicitRootUnreadable)
	}
}

func TestDiscoverRoot_POSIXHeuristics_NeverSelected(t *testing.T) {
	// "/opt/vcpkg" is heuristicPathsFor's own literal for a non-windows GOOS.
	const heuristic = "/opt/vcpkg"
	files := newFakeFS(binIn(heuristic, "linux"))
	deps := baseDeps("linux", files)
	deps.UserHomeDir = func() (string, error) { return testRoot("home", "op"), nil }
	res := DiscoverRoot("", deps)
	if res.Status != evidence.StatusUnknown || res.Reason != ReasonHeuristicOnly {
		t.Fatalf("got status=%v reason=%v, want unknown/heuristic_only", res.Status, res.Reason)
	}
	if len(res.Candidates) != 1 || res.Candidates[0].Path != heuristic {
		t.Fatalf("candidates = %+v, want %q reported as a candidate", res.Candidates, heuristic)
	}
}

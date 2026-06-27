package clients

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestProjectScopes_VerifiedFormats pins the per-client project-scope formats
// against the design's VERIFIED table
// (work-items/decisions/2026-06-24-per-project-gui-design.md): claude-code →
// .mcp.json/mcpServers, cursor → .cursor/mcp.json/mcpServers, vscode →
// .vscode/mcp.json/servers (NOT mcpServers).
func TestProjectScopes_VerifiedFormats(t *testing.T) {
	want := map[string]struct {
		rel     string
		section string
	}{
		"claude-code": {filepath.Join(".mcp.json"), "mcpServers"},
		"cursor":      {filepath.Join(".cursor", "mcp.json"), "mcpServers"},
		"vscode":      {filepath.Join(".vscode", "mcp.json"), "servers"},
	}
	got := map[string]ProjectScope{}
	for _, ps := range ProjectScopes() {
		if !ps.Supported {
			t.Errorf("ProjectScopes() returned an unsupported row for %q", ps.Client)
		}
		got[ps.Client] = ps
	}
	if len(got) != len(want) {
		t.Fatalf("ProjectScopes() count = %d, want %d (%v)", len(got), len(want), got)
	}
	for client, w := range want {
		ps, ok := got[client]
		if !ok {
			t.Fatalf("ProjectScopes() missing client %q", client)
		}
		if ps.RelFile != w.rel {
			t.Errorf("%s RelFile = %q, want %q", client, ps.RelFile, w.rel)
		}
		if ps.SectionKey != w.section {
			t.Errorf("%s SectionKey = %q, want %q", client, ps.SectionKey, w.section)
		}
	}
}

// TestProjectScope_SectionKeyMatchesAdapter is the drift guard pinning each
// ProjectScope.SectionKey to the adapter's own section-key constant — so the
// documentary key in the registry can never silently diverge from the key the
// real scanner/adapter parses. claudeCodeMCPServersKey (claude_code.go) and
// vscodeServersKey (vscode.go) are the single owners; cursor reuses the
// canonical JSON-family `mcpServers`.
func TestProjectScope_SectionKeyMatchesAdapter(t *testing.T) {
	bySection := map[string]string{}
	for _, ps := range ProjectScopes() {
		bySection[ps.Client] = ps.SectionKey
	}
	if bySection["claude-code"] != claudeCodeMCPServersKey {
		t.Errorf("claude-code SectionKey = %q, want adapter const %q", bySection["claude-code"], claudeCodeMCPServersKey)
	}
	if bySection["vscode"] != vscodeServersKey {
		t.Errorf("vscode SectionKey = %q, want adapter const %q", bySection["vscode"], vscodeServersKey)
	}
	// cursor uses the canonical JSON-family key (no per-adapter const; it is a
	// jsonMCPClient over `mcpServers`).
	if bySection["cursor"] != "mcpServers" {
		t.Errorf("cursor SectionKey = %q, want canonical \"mcpServers\"", bySection["cursor"])
	}
}

// TestProjectScopeClients_AreRegistryClients ensures every project-scope client
// id is a real SupportedClientNames() client (no orphan / typo'd id).
func TestProjectScopeClients_AreRegistryClients(t *testing.T) {
	known := map[string]bool{}
	for _, n := range SupportedClientNames() {
		known[n] = true
	}
	for _, ps := range ProjectScopes() {
		if !known[ps.Client] {
			t.Errorf("ProjectScope client %q is not a SupportedClientNames() client", ps.Client)
		}
	}
}

// TestProjectScanConfigPaths_Valid resolves an existing temp directory and
// returns absolute, in-root paths for all three clients.
func TestProjectScanConfigPaths_Valid(t *testing.T) {
	root := t.TempDir()

	paths, err := ProjectScanConfigPaths(root)
	if err != nil {
		t.Fatalf("ProjectScanConfigPaths(%q) error = %v", root, err)
	}

	// EvalSymlinks may canonicalize the temp dir (e.g. /var→/private/var on
	// macOS, or 8.3 short-name expansion on Windows); resolve the expected base
	// the same way so the comparison is exact.
	realRoot, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", root, err)
	}

	want := map[string]string{
		"claude-code": filepath.Join(realRoot, ".mcp.json"),
		"cursor":      filepath.Join(realRoot, ".cursor", "mcp.json"),
		"vscode":      filepath.Join(realRoot, ".vscode", "mcp.json"),
	}
	if len(paths) != len(want) {
		t.Fatalf("got %d paths, want %d: %v", len(paths), len(want), paths)
	}
	for client, w := range want {
		got, ok := paths[client]
		if !ok {
			t.Fatalf("missing path for %q", client)
		}
		if got != w {
			t.Errorf("%s path = %q, want %q", client, got, w)
		}
		if !filepath.IsAbs(got) {
			t.Errorf("%s path %q is not absolute", client, got)
		}
	}
}

// TestProjectScanConfigPaths_PathSafety_Rejects is the security table: every
// hostile/degenerate root must be rejected with a non-nil error and NO returned
// map. The error message must not echo the raw input (leak-safety asserted at
// the handler boundary; here we assert rejection + structural safety).
func TestProjectScanConfigPaths_PathSafety_Rejects(t *testing.T) {
	existingDir := t.TempDir()
	// A regular file (non-directory) target.
	regFile := filepath.Join(existingDir, "afile")
	if err := os.WriteFile(regFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	cases := []struct {
		name string
		root string
	}{
		{"empty", ""},
		{"relative", "some/rel/path"},
		{"relative-traversal", "../../etc"},
		{"dot", "."},
		{"absolute-traversal-to-nonexistent", filepath.Join(existingDir, "..", "..", "etc")},
		{"nonexistent-absolute", filepath.Join(existingDir, "definitely-not-here-xyz")},
		{"non-directory-file", regFile},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			paths, err := ProjectScanConfigPaths(c.root)
			if err == nil {
				t.Fatalf("ProjectScanConfigPaths(%q) = %v, want error", c.root, paths)
			}
			if paths != nil {
				t.Errorf("ProjectScanConfigPaths(%q) returned non-nil map on error: %v", c.root, paths)
			}
		})
	}
}

// TestProjectScanConfigPaths_AbsoluteTraversalCollapses verifies that an
// absolute root carrying `..` segments is rejected by the clean round-trip
// guard rather than silently resolved — i.e. the caller cannot smuggle
// traversal through a not-yet-collapsed absolute path.
func TestProjectScanConfigPaths_AbsoluteTraversalCollapses(t *testing.T) {
	base := t.TempDir()
	// base/sub exists; base/sub/../sub cleans to base/sub but is NOT in clean
	// form, so it must be rejected by the round-trip equality guard.
	sub := filepath.Join(base, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Build the dirty path by raw separator concatenation (NOT filepath.Join,
	// which would pre-clean it) so the function actually receives the `..`
	// segments. filepath.Clean(dirty) == sub, but dirty != sub, so the
	// round-trip equality guard must reject it.
	sep := string(filepath.Separator)
	dirty := base + sep + "sub" + sep + ".." + sep + "sub"
	if dirty == filepath.Clean(dirty) {
		t.Fatalf("test bug: constructed path %q is already clean", dirty)
	}
	if _, err := ProjectScanConfigPaths(dirty); err == nil {
		t.Fatalf("ProjectScanConfigPaths(%q) accepted a non-clean absolute path", dirty)
	}
}

// TestProjectScanConfigPaths_SymlinkContainedToDir accepts a symlinked root
// that resolves to a real directory, and the returned per-client paths are
// joined onto the RESOLVED real target (not the link path).
func TestProjectScanConfigPaths_SymlinkContainedToDir(t *testing.T) {
	target := t.TempDir()
	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "projlink")
	if err := os.Symlink(target, link); err != nil {
		// Windows without privilege / dev mode cannot create directory
		// symlinks — skip rather than fail.
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unsupported on this host: %v", err)
		}
		t.Fatalf("symlink: %v", err)
	}

	paths, err := ProjectScanConfigPaths(link)
	if err != nil {
		t.Fatalf("ProjectScanConfigPaths(symlink→dir) error = %v", err)
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatalf("EvalSymlinks(target): %v", err)
	}
	wantClaude := filepath.Join(realTarget, ".mcp.json")
	if paths["claude-code"] != wantClaude {
		t.Errorf("symlinked root resolved to %q, want join onto real target %q", paths["claude-code"], wantClaude)
	}
}

// TestProjectScanConfigPaths_SymlinkToFileRejected rejects a symlinked root
// whose target is a regular file (resolves to a non-directory).
func TestProjectScanConfigPaths_SymlinkToFileRejected(t *testing.T) {
	dir := t.TempDir()
	targetFile := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(targetFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	link := filepath.Join(t.TempDir(), "filelink")
	if err := os.Symlink(targetFile, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unsupported on this host: %v", err)
		}
		t.Fatalf("symlink: %v", err)
	}
	if _, err := ProjectScanConfigPaths(link); err == nil {
		t.Fatalf("ProjectScanConfigPaths(symlink→file) accepted a non-directory target")
	}
}

// TestProjectScanConfigPaths_IntermediateSymlinkEscapeDropped is the headline
// finding-1 guard: an INTERMEDIATE per-client config dir (.cursor / .vscode)
// that is itself a symlink pointing OUTSIDE the root must NOT contribute a
// config path — the client is DROPPED (never read), so os.ReadFile can never
// follow the link to <outside>/mcp.json. claude-code (whose config sits
// directly at <root>/.mcp.json, parent == root) is unaffected and stays. A
// partial result is correct: claude-code still scans.
func TestProjectScanConfigPaths_IntermediateSymlinkEscapeDropped(t *testing.T) {
	for _, tc := range []struct {
		name    string // subtest
		client  string // the registry client whose intermediate dir we attack
		linkDir string // the in-root dir name that becomes a symlink (.cursor/.vscode)
	}{
		{"cursor-dir-symlinked-out", "cursor", ".cursor"},
		{"vscode-dir-symlinked-out", "vscode", ".vscode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			// An OUTSIDE directory holding an attacker-controlled mcp.json the
			// scan must never reach.
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "mcp.json"),
				[]byte(`{"mcpServers":{"evil":{"command":"x"}}}`), 0o600); err != nil {
				t.Fatalf("seed outside mcp.json: %v", err)
			}
			// <root>/.cursor (or .vscode) -> outside  (intermediate-dir symlink).
			link := filepath.Join(root, tc.linkDir)
			if err := os.Symlink(outside, link); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("symlink unsupported on this host: %v", err)
				}
				t.Fatalf("symlink: %v", err)
			}

			paths, err := ProjectScanConfigPaths(root)
			if err != nil {
				t.Fatalf("ProjectScanConfigPaths(%q) error = %v", root, err)
			}
			// The attacked client is DROPPED entirely.
			if got, ok := paths[tc.client]; ok {
				t.Errorf("client %q must be dropped (intermediate-dir symlink escape) but got path %q", tc.client, got)
			}
			// claude-code (parent == root) is unaffected — partial result.
			realRoot, err := filepath.EvalSymlinks(filepath.Clean(root))
			if err != nil {
				t.Fatalf("EvalSymlinks(root): %v", err)
			}
			wantClaude := filepath.Join(realRoot, ".mcp.json")
			if paths["claude-code"] != wantClaude {
				t.Errorf("claude-code path = %q, want %q (must survive a sibling-client escape)", paths["claude-code"], wantClaude)
			}
			// Defense-in-depth: NO returned path may resolve under `outside`.
			realOutside, err := filepath.EvalSymlinks(outside)
			if err != nil {
				t.Fatalf("EvalSymlinks(outside): %v", err)
			}
			for client, p := range paths {
				if rootContainsPath(realOutside, filepath.Dir(p)) {
					t.Errorf("client %q path %q escapes into outside root %q", client, p, realOutside)
				}
			}
		})
	}
}

// TestProjectScanConfigPaths_RealIntermediateDirIncluded is the negative
// control for finding 1: a NORMAL real (non-symlink) .cursor / .vscode dir is
// fully contained and the client is INCLUDED. Without this the escape guard
// could over-reject legitimate projects.
func TestProjectScanConfigPaths_RealIntermediateDirIncluded(t *testing.T) {
	root := t.TempDir()
	// Real in-root dirs (no symlinks).
	if err := os.MkdirAll(filepath.Join(root, ".cursor"), 0o755); err != nil {
		t.Fatalf("mkdir .cursor: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".vscode"), 0o755); err != nil {
		t.Fatalf("mkdir .vscode: %v", err)
	}

	paths, err := ProjectScanConfigPaths(root)
	if err != nil {
		t.Fatalf("ProjectScanConfigPaths(%q) error = %v", root, err)
	}
	realRoot, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		t.Fatalf("EvalSymlinks(root): %v", err)
	}
	want := map[string]string{
		"claude-code": filepath.Join(realRoot, ".mcp.json"),
		"cursor":      filepath.Join(realRoot, ".cursor", "mcp.json"),
		"vscode":      filepath.Join(realRoot, ".vscode", "mcp.json"),
	}
	for client, w := range want {
		got, ok := paths[client]
		if !ok {
			t.Errorf("client %q dropped despite a real (non-symlink) in-root dir", client)
			continue
		}
		if got != w {
			t.Errorf("%s path = %q, want %q", client, got, w)
		}
	}
}

// TestProjectScanConfigPaths_FinalFileSymlinkEscapeDropped is the finding-1
// residual guard: a per-client config FILE that is ITSELF a symlink to an
// OUTSIDE regular file must be DROPPED even with MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK=1
// set — the project scan must NOT inherit the presence gate's opt-in
// symlink-following. The parent .cursor dir is in-root (a real dir), so the
// parent check passes; only the LEAF link escapes, which the final-file check
// catches. claude-code (whose .mcp.json is absent) is unaffected and stays.
func TestProjectScanConfigPaths_FinalFileSymlinkEscapeDropped(t *testing.T) {
	// Set the opt-in that makes the downstream presence gate report a
	// symlink-to-regular-file as "ok" → the scan would otherwise follow it.
	t.Setenv("MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK", "1")

	root := t.TempDir()
	// A real in-root .cursor dir (parent check passes), but mcp.json inside it
	// is a symlink to an OUTSIDE file the scan must never read.
	cursorDir := filepath.Join(root, ".cursor")
	if err := os.MkdirAll(cursorDir, 0o755); err != nil {
		t.Fatalf("mkdir .cursor: %v", err)
	}
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "mcp.json")
	if err := os.WriteFile(outsideFile,
		[]byte(`{"mcpServers":{"evil":{"command":"x"}}}`), 0o600); err != nil {
		t.Fatalf("seed outside mcp.json: %v", err)
	}
	// <root>/.cursor/mcp.json -> /outside/mcp.json (final-FILE symlink).
	if err := os.Symlink(outsideFile, filepath.Join(cursorDir, "mcp.json")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unsupported on this host: %v", err)
		}
		t.Fatalf("symlink: %v", err)
	}

	paths, err := ProjectScanConfigPaths(root)
	if err != nil {
		t.Fatalf("ProjectScanConfigPaths(%q) error = %v", root, err)
	}
	// cursor is DROPPED — the leaf link escapes realRoot.
	if got, ok := paths["cursor"]; ok {
		t.Errorf("cursor must be dropped (final-file symlink escape under opt-in) but got path %q", got)
	}
	// Defense-in-depth: NO returned path may resolve under `outside`.
	realOutside, err := filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatalf("EvalSymlinks(outside): %v", err)
	}
	for client, p := range paths {
		resolved, rerr := filepath.EvalSymlinks(p)
		if rerr != nil {
			// A returned path that doesn't resolve is fine (absent leaf).
			continue
		}
		if rootContainsPath(realOutside, resolved) {
			t.Errorf("client %q path %q resolves into outside root %q", client, p, realOutside)
		}
	}
}

// TestProjectScanConfigPaths_FinalFileSymlinkContainedIncluded is the negative
// control for the final-file guard: a mcp.json that is a symlink to ANOTHER
// file still INSIDE realRoot resolves in-root and the client is INCLUDED — the
// guard is containment-based, not symlink-phobic.
func TestProjectScanConfigPaths_FinalFileSymlinkContainedIncluded(t *testing.T) {
	root := t.TempDir()
	cursorDir := filepath.Join(root, ".cursor")
	if err := os.MkdirAll(cursorDir, 0o755); err != nil {
		t.Fatalf("mkdir .cursor: %v", err)
	}
	inRootTarget := filepath.Join(root, "real-mcp.json")
	if err := os.WriteFile(inRootTarget, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatalf("seed in-root target: %v", err)
	}
	if err := os.Symlink(inRootTarget, filepath.Join(cursorDir, "mcp.json")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unsupported on this host: %v", err)
		}
		t.Fatalf("symlink: %v", err)
	}

	paths, err := ProjectScanConfigPaths(root)
	if err != nil {
		t.Fatalf("ProjectScanConfigPaths(%q) error = %v", root, err)
	}
	if _, ok := paths["cursor"]; !ok {
		t.Errorf("cursor dropped despite an IN-ROOT symlinked mcp.json (guard is containment-based, not symlink-phobic)")
	}
}

// TestProjectScanConfigPaths_ForwardSlashRoot is the finding-4 guard: the P1
// frontend canonicalizes Windows roots with FORWARD slashes (C:/dev/proj), and
// the endpoint must ACCEPT them (previously the clean round-trip rejected them
// on Windows because Clean produces backslashes). POSIX is unaffected
// (FromSlash is a no-op). A forward-slash TRAVERSAL (C:/dev/../etc) must STILL
// be REJECTED. GOOS-gated like the existing symlink tests.
func TestProjectScanConfigPaths_ForwardSlashRoot(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("forward-slash separator normalization is Windows-specific; POSIX roots already use /")
	}
	// A real temp dir, then re-expressed with forward slashes the way the P1
	// frontend canonicalizes it.
	root := t.TempDir()
	fwd := filepath.ToSlash(root) // e.g. C:/Users/.../Temp/xxxxx
	if fwd == root {
		t.Fatalf("test bug: ToSlash(%q) did not change separators on Windows", root)
	}

	paths, err := ProjectScanConfigPaths(fwd)
	if err != nil {
		t.Fatalf("ProjectScanConfigPaths(%q) (forward-slash) error = %v; want accepted", fwd, err)
	}
	realRoot, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		t.Fatalf("EvalSymlinks(root): %v", err)
	}
	want := filepath.Join(realRoot, ".mcp.json")
	if paths["claude-code"] != want {
		t.Errorf("forward-slash root: claude-code path = %q, want %q", paths["claude-code"], want)
	}
}

// TestProjectScanConfigPaths_ForwardSlashTraversalRejected proves the
// separator normalization PRESERVES the traversal-rejection guard: a
// forward-slash root carrying `..` (C:/dev/../etc) is FromSlash'd to backslash,
// Clean'd to a different path, and rejected by the round-trip equality guard.
func TestProjectScanConfigPaths_ForwardSlashTraversalRejected(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("forward-slash Windows traversal case is Windows-specific")
	}
	// Build an absolute forward-slash root with a `..` segment. After FromSlash
	// it becomes `C:\dev\..\etc`; Clean collapses to `C:\etc` ≠ normalized
	// input, so it must be rejected (never stat'd).
	dirty := "C:/dev/../etc"
	if _, err := ProjectScanConfigPaths(dirty); err == nil {
		t.Fatalf("ProjectScanConfigPaths(%q) accepted a forward-slash traversal root", dirty)
	}
}

// TestProjectScanConfigPaths_IntermediateSymlinkContainedIncluded proves the
// guard is CONTAINMENT-based, not symlink-phobic: a .cursor that is a symlink
// pointing to ANOTHER directory still INSIDE realRoot resolves in-root and the
// client is INCLUDED.
func TestProjectScanConfigPaths_IntermediateSymlinkContainedIncluded(t *testing.T) {
	root := t.TempDir()
	// An in-root real target dir, and .cursor -> that in-root dir.
	inRootTarget := filepath.Join(root, "real-cursor-store")
	if err := os.MkdirAll(inRootTarget, 0o755); err != nil {
		t.Fatalf("mkdir in-root target: %v", err)
	}
	if err := os.Symlink(inRootTarget, filepath.Join(root, ".cursor")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unsupported on this host: %v", err)
		}
		t.Fatalf("symlink: %v", err)
	}

	paths, err := ProjectScanConfigPaths(root)
	if err != nil {
		t.Fatalf("ProjectScanConfigPaths(%q) error = %v", root, err)
	}
	if _, ok := paths["cursor"]; !ok {
		t.Errorf("cursor dropped despite an IN-ROOT symlinked .cursor dir (guard is containment-based, not symlink-phobic)")
	}
}

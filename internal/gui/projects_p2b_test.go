// internal/gui/projects_p2b_test.go
//
// Per-project-GUI Phase 2b: GET /api/projects/scan claude-code LOCAL scope +
// .mcp.json enabled/disabled reconciliation (READ-ONLY), through the HTTP
// handler.
//
// STATE-SAFETY (CRITICAL — this phase reads the user's LIVE ~/.claude.json):
//   - Every test calls isolateHome(t) (projects_test.go), which t.Setenv's
//     HOME + USERPROFILE to a temp dir. os.UserHomeDir() then resolves the
//     ~/.claude.json the reader opens INTO THE TEMP DIR — the developer's REAL
//     ~/.claude.json is structurally unreachable for the duration of the test.
//   - The synthetic ~/.claude.json is written under that temp home.
//   - TestProjectsScanP2b_LiveClaudeJSON_Untouched additionally snapshots the
//     REAL ~/.claude.json bytes+mtime before/after a scan and asserts they are
//     byte-identical — a belt-and-suspenders proof that no scan path ever writes
//     the live file (it cannot, but the test documents + guards the contract).
package gui

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// writeSyntheticClaudeJSON writes ~/.claude.json into the (already-isolated)
// temp home. Call AFTER isolateHome(t). Returns the path.
func writeSyntheticClaudeJSON(t *testing.T, body string) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir (should be the isolated temp home): %v", err)
	}
	p := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write synthetic claude.json: %v", err)
	}
	return p
}

// fwdSlashKey returns the forward-slash + uppercase-drive projects.<key> form
// (the most common host form) for a root. On POSIX returns root unchanged.
func fwdSlashKey(root string) string {
	if runtime.GOOS != "windows" {
		return root
	}
	return strings.ReplaceAll(root, "\\", "/")
}

// jsonEscPath escapes a path for a JSON string literal.
func jsonEscPath(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// TestProjectsScanP2b_LocalScopeExposed seeds a project with a .mcp.json (Project
// scope) AND a ~/.claude.json projects.<key>.mcpServers (Local scope), and
// asserts the scan exposes BOTH distinctly: the .mcp.json server appears as an
// Entry, the Local-scope server set appears in ScanResult.ProjectScope.
func TestProjectsScanP2b_LocalScopeExposed(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()

	// Project scope: <root>/.mcp.json
	mustWrite(t, filepath.Join(root, ".mcp.json"),
		`{"mcpServers":{"projServer":{"command":"node","args":["a.js"]}}}`)

	// Local scope: ~/.claude.json projects.<key>.mcpServers (a SEPARATE set)
	key := fwdSlashKey(root)
	writeSyntheticClaudeJSON(t, `{"projects":{"`+jsonEscPath(key)+`":{`+
		`"mcpServers":{"localServerB":{},"localServerA":{}}}}}`)

	s := NewServer(Config{})
	rec := getProjectScan(t, s, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out api.ScanResult
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// .mcp.json Project-scope server is an Entry with claude-code presence.
	var foundProj bool
	for _, e := range out.Entries {
		if e.Name == "projServer" {
			foundProj = true
			if _, ok := e.ClientPresence["claude-code"]; !ok {
				t.Errorf("projServer has no claude-code presence")
			}
		}
	}
	if !foundProj {
		t.Errorf("projServer (.mcp.json Project scope) absent from entries (%v)", entryNames(out))
	}

	// Local-scope set is in ProjectScope.LocalServers (sorted, SEPARATE).
	if out.ProjectScope == nil {
		t.Fatalf("ProjectScope is nil; want the local-scope projection")
	}
	want := []string{"localServerA", "localServerB"}
	if !reflect.DeepEqual(out.ProjectScope.LocalServers, want) {
		t.Errorf("ProjectScope.LocalServers = %v, want %v (sorted)", out.ProjectScope.LocalServers, want)
	}
}

// TestProjectsScanP2b_Reconciliation: a .mcp.json server in disabledMcpjsonServers
// → ProjectEnabled=false; in enabledMcpjsonServers → true; in both → true
// (override); in neither → true (default).
func TestProjectsScanP2b_Reconciliation(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()

	// Four .mcp.json Project-scope servers.
	mustWrite(t, filepath.Join(root, ".mcp.json"), `{"mcpServers":{`+
		`"disabledOne":{"command":"n"},`+
		`"enabledOne":{"command":"n"},`+
		`"bothOne":{"command":"n"},`+
		`"defaultOne":{"command":"n"}}}`)

	key := fwdSlashKey(root)
	writeSyntheticClaudeJSON(t, `{"projects":{"`+jsonEscPath(key)+`":{`+
		`"mcpServers":{},`+
		`"disabledMcpjsonServers":["disabledOne","bothOne"],`+
		`"enabledMcpjsonServers":["enabledOne","bothOne"]}}}`)

	s := NewServer(Config{})
	rec := getProjectScan(t, s, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out api.ScanResult
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	byName := map[string]api.ScanEntry{}
	for _, e := range out.Entries {
		byName[e.Name] = e
	}
	check := func(name string, want bool) {
		e, ok := byName[name]
		if !ok {
			t.Fatalf("server %q absent (%v)", name, entryNames(out))
		}
		if e.ProjectEnabled == nil {
			t.Fatalf("server %q: ProjectEnabled is nil (reconciliation not applied)", name)
		}
		if *e.ProjectEnabled != want {
			t.Errorf("server %q: ProjectEnabled = %v, want %v", name, *e.ProjectEnabled, want)
		}
	}
	check("disabledOne", false) // in disabled, not enabled
	check("enabledOne", true)   // in enabled only
	check("bothOne", true)      // in both → enabled wins
	check("defaultOne", true)   // in neither → default enabled

	// The verbatim toggle arrays must be surfaced too.
	if out.ProjectScope == nil {
		t.Fatalf("ProjectScope nil; want verbatim toggle arrays")
	}
	if !reflect.DeepEqual(out.ProjectScope.DisabledMcpjsonServers, []string{"disabledOne", "bothOne"}) {
		t.Errorf("DisabledMcpjsonServers = %v", out.ProjectScope.DisabledMcpjsonServers)
	}
	if !reflect.DeepEqual(out.ProjectScope.EnabledMcpjsonServers, []string{"enabledOne", "bothOne"}) {
		t.Errorf("EnabledMcpjsonServers = %v", out.ProjectScope.EnabledMcpjsonServers)
	}
}

// TestProjectsScanP2b_NoLocalRecord_DefaultsEnabled: a project with a .mcp.json
// but NO ~/.claude.json local record → every .mcp.json server is ProjectEnabled
// (default), and ProjectScope is nil (no local scope matched).
func TestProjectsScanP2b_NoLocalRecord_DefaultsEnabled(t *testing.T) {
	isolateHome(t) // no ~/.claude.json written
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, ".mcp.json"),
		`{"mcpServers":{"only":{"command":"n"}}}`)

	s := NewServer(Config{})
	rec := getProjectScan(t, s, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out api.ScanResult
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ProjectScope != nil {
		t.Errorf("ProjectScope should be nil with no local record, got %+v", out.ProjectScope)
	}
	for _, e := range out.Entries {
		if e.Name == "only" {
			if e.ProjectEnabled == nil || !*e.ProjectEnabled {
				t.Errorf("server with no local record should default ProjectEnabled=true, got %v", e.ProjectEnabled)
			}
		}
	}
}

// TestProjectsScanP2b_NonClaudeEntriesNotReconciled: cursor/vscode-only entries
// (no claude-code presence) keep ProjectEnabled nil — the reconciliation rule is
// claude-specific.
func TestProjectsScanP2b_NonClaudeEntriesNotReconciled(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, ".cursor", "mcp.json"),
		`{"mcpServers":{"cursorOnly":{"url":"http://127.0.0.1:9300/mcp"}}}`)

	// A local record exists, but it must NOT touch cursor entries.
	key := fwdSlashKey(root)
	writeSyntheticClaudeJSON(t, `{"projects":{"`+jsonEscPath(key)+`":{`+
		`"disabledMcpjsonServers":["cursorOnly"]}}}`)

	s := NewServer(Config{})
	rec := getProjectScan(t, s, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out api.ScanResult
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, e := range out.Entries {
		if e.Name == "cursorOnly" {
			if _, isClaude := e.ClientPresence["claude-code"]; isClaude {
				t.Fatalf("cursorOnly unexpectedly has claude-code presence")
			}
			if e.ProjectEnabled != nil {
				t.Errorf("cursor-only entry should NOT be reconciled (ProjectEnabled nil), got %v", *e.ProjectEnabled)
			}
		}
	}
}

// TestProjectsScanP2b_GlobalScanUnchanged: the P2b enrichment must not perturb
// the GLOBAL DefaultScanConfigPaths scan output. (Source-level: scan.go has zero
// diff; this is the runtime confirmation including a synthetic ~/.claude.json
// present in the isolated home.)
func TestProjectsScanP2b_GlobalScanUnchanged(t *testing.T) {
	isolateHome(t)
	// A synthetic ~/.claude.json IS present in the isolated home — the global
	// scan must still be byte-identical before/after a project scan.
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, ".mcp.json"), `{"mcpServers":{"p":{"command":"n"}}}`)
	key := fwdSlashKey(root)
	writeSyntheticClaudeJSON(t, `{"projects":{"`+jsonEscPath(key)+`":{"mcpServers":{"l":{}},"disabledMcpjsonServers":["p"]}}}`)

	before, err := api.NewAPI().ScanFrom(api.ScanOpts{ConfigPaths: api.DefaultScanConfigPaths()})
	if err != nil {
		t.Fatalf("baseline global scan: %v", err)
	}

	s := NewServer(Config{})
	rec := getProjectScan(t, s, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("project scan status = %d, body=%s", rec.Code, rec.Body.String())
	}

	after, err := api.NewAPI().ScanFrom(api.ScanOpts{ConfigPaths: api.DefaultScanConfigPaths()})
	if err != nil {
		t.Fatalf("post global scan: %v", err)
	}

	before.At = after.At
	sortEntriesByName(before.Entries)
	sortEntriesByName(after.Entries)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("global scan changed across a P2b project scan:\nbefore=%+v\nafter=%+v", before, after)
	}
	// The global scan must NEVER carry the P2b ProjectScope projection.
	if after.ProjectScope != nil {
		t.Errorf("global scan leaked ProjectScope = %+v (must be project-scan-only)", after.ProjectScope)
	}
	for _, e := range after.Entries {
		if e.ProjectEnabled != nil {
			t.Errorf("global scan entry %q leaked ProjectEnabled (must be project-scan-only)", e.Name)
		}
	}
}

// TestProjectsScanP2b_SymlinkedRoot_LocalScopeViaRealPath is the finding-1
// proof: when the scanned ?root is an allowed SYMLINK, the scan resolves it to
// the REAL root and scans the real paths — and the ~/.claude.json LOCAL-scope
// lookup MUST be keyed off that SAME resolved real path. Claude Code writes the
// projects.<key> at the REAL filesystem path, so a lookup keyed off the
// unresolved symlink path would canonicalize to the link path and MISS the key,
// silently dropping the project's Local-scope + reconciliation.
//
// We key the synthetic ~/.claude.json projects.<key> at the RESOLVED real root,
// then scan via the SYMLINK. The Local-scope set + the .mcp.json reconciliation
// must be present — which is only true if the handler threaded the resolved real
// root into EnrichProjectClaudeLocalScope.
func TestProjectsScanP2b_SymlinkedRoot_LocalScopeViaRealPath(t *testing.T) {
	isolateHome(t)

	// Real project dir with a .mcp.json (Project scope).
	realRoot := t.TempDir()
	mustWrite(t, filepath.Join(realRoot, ".mcp.json"),
		`{"mcpServers":{"projServer":{"command":"node"}}}`)

	// A symlink that points at the real project dir; we scan THROUGH it.
	link := filepath.Join(t.TempDir(), "projlink")
	if err := os.Symlink(realRoot, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("directory symlink unsupported on this host: %v", err)
		}
		t.Fatalf("symlink: %v", err)
	}

	// The projects.<key> is written at the RESOLVED real root (what Claude Code
	// writes) — NOT the link path. EvalSymlinks mirrors the handler's resolution.
	resolvedReal, err := filepath.EvalSymlinks(realRoot)
	if err != nil {
		t.Fatalf("EvalSymlinks(realRoot): %v", err)
	}
	key := fwdSlashKey(resolvedReal)
	writeSyntheticClaudeJSON(t, `{"projects":{"`+jsonEscPath(key)+`":{`+
		`"mcpServers":{"localOnly":{}},`+
		`"disabledMcpjsonServers":["projServer"]}}}`)

	s := NewServer(Config{})
	rec := getProjectScan(t, s, link) // scan VIA the symlink
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out api.ScanResult
	if derr := json.NewDecoder(rec.Body).Decode(&out); derr != nil {
		t.Fatalf("decode: %v", derr)
	}

	// Local scope matched via the resolved real path → ProjectScope present.
	if out.ProjectScope == nil {
		t.Fatalf("ProjectScope is nil: the local-scope lookup did NOT match the projects.<key> " +
			"written at the resolved real path (symlinked root keyed off the unresolved link path)")
	}
	if !reflect.DeepEqual(out.ProjectScope.LocalServers, []string{"localOnly"}) {
		t.Errorf("ProjectScope.LocalServers = %v, want [localOnly]", out.ProjectScope.LocalServers)
	}
	// Reconciliation applied via the resolved-path match: projServer is disabled.
	var found bool
	for _, e := range out.Entries {
		if e.Name == "projServer" {
			found = true
			if e.ProjectEnabled == nil {
				t.Fatalf("projServer ProjectEnabled is nil: reconciliation NOT applied " +
					"(local-scope lookup missed the resolved-path key)")
			}
			if *e.ProjectEnabled {
				t.Errorf("projServer ProjectEnabled = true, want false (it is in disabledMcpjsonServers)")
			}
		}
	}
	if !found {
		t.Fatalf("projServer (.mcp.json via the symlinked root) absent from entries (%v)", entryNames(out))
	}
}

// TestProjectsScanP2b_NoWrite_ClaudeJSONUntouched: the scan must not write the
// synthetic ~/.claude.json — byte-identical + same mtime after a scan, and no
// new file under the home.
func TestProjectsScanP2b_NoWrite_ClaudeJSONUntouched(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, ".mcp.json"), `{"mcpServers":{"p":{"command":"n"}}}`)
	key := fwdSlashKey(root)
	claudePath := writeSyntheticClaudeJSON(t, `{"projects":{"`+jsonEscPath(key)+`":{"mcpServers":{"l":{}},"disabledMcpjsonServers":["p"]}},"top":1}`)

	beforeBytes, _ := os.ReadFile(claudePath)
	beforeInfo, _ := os.Stat(claudePath)

	s := NewServer(Config{})
	rec := getProjectScan(t, s, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	afterBytes, _ := os.ReadFile(claudePath)
	if !reflect.DeepEqual(beforeBytes, afterBytes) {
		t.Errorf("synthetic ~/.claude.json content changed after a read-only scan")
	}
	afterInfo, _ := os.Stat(claudePath)
	if beforeInfo != nil && afterInfo != nil && !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Errorf("synthetic ~/.claude.json mtime changed")
	}
}

// TestProjectsScanP2b_LiveClaudeJSON_Unreachable is the LIVE-config-safety proof,
// asserted STRUCTURALLY rather than by byte-comparing the operator's real file.
//
// The earlier form snapshotted the developer's REAL ~/.claude.json and asserted
// it was byte-identical (and same mtime) after a scan. That is flaky for a reason
// unrelated to P2b: Claude Code (the live session running this very test) may
// LEGITIMATELY write its own ~/.claude.json mid-test, tripping the byte/mtime
// compare even though the scan never touched the file. The scan's real read-only
// contract is already proven by TestProjectsScanP2b_NoWrite_ClaudeJSONUntouched
// (synthetic, fully isolated). What remains worth guarding here is that the scan,
// running under the redirected HOME, STRUCTURALLY cannot REACH the real path at
// all — so we assert that instead of comparing a file another process owns.
//
// The reader resolves ~/.claude.json via os.UserHomeDir(), so the load-bearing
// invariant is: under isolateHome(t), os.UserHomeDir() returns the isolated temp
// home, and the real ~/.claude.json is NOT under that isolated home. A regression
// that un-isolated the read (e.g. hardcoding the real home) fails the home-redirect
// assertion here instead of silently corrupting the operator's file.
func TestProjectsScanP2b_LiveClaudeJSON_Unreachable(t *testing.T) {
	// Resolve the REAL home BEFORE any isolation. os.UserHomeDir() here reads the
	// process env, which TestMain did NOT redirect for HOME/USERPROFILE, so this
	// is the operator's actual home.
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot resolve real home: %v", err)
	}
	realClaude := filepath.Join(realHome, ".claude.json")

	// Now isolate and run a scan that exercises the local-scope reader.
	isolatedHome := isolateHome(t)
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, ".mcp.json"), `{"mcpServers":{"p":{"command":"n"}}}`)
	key := fwdSlashKey(root)
	writeSyntheticClaudeJSON(t, `{"projects":{"`+jsonEscPath(key)+`":{"mcpServers":{"l":{}}}}}`)

	// STRUCTURAL guard 1: under isolation, os.UserHomeDir() (the exact resolver the
	// reader uses) must point at the isolated temp home, NOT the operator's home.
	resolvedHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir under isolation: %v", err)
	}
	if resolvedHome != isolatedHome {
		t.Fatalf("LIVE-CONFIG-SAFETY VIOLATION: os.UserHomeDir()=%q under isolation, want isolated home %q "+
			"(the reader would read the operator's real ~/.claude.json)", resolvedHome, isolatedHome)
	}

	// STRUCTURAL guard 2: the real ~/.claude.json path must NOT live under the
	// isolated home, i.e. the scan's read surface and the real file are disjoint.
	if pathUnder(isolatedHome, realClaude) {
		t.Fatalf("test premise broken: real ~/.claude.json %q is under the isolated home %q", realClaude, isolatedHome)
	}

	s := NewServer(Config{})
	rec := getProjectScan(t, s, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	// And the scan saw the SYNTHETIC local scope (proving it read the isolated
	// file, not the real one).
	var out api.ScanResult
	if derr := json.NewDecoder(rec.Body).Decode(&out); derr != nil {
		t.Fatalf("decode: %v", derr)
	}
	if out.ProjectScope == nil || !reflect.DeepEqual(out.ProjectScope.LocalServers, []string{"l"}) {
		t.Fatalf("scan did not read the SYNTHETIC isolated ~/.claude.json (ProjectScope=%+v); "+
			"a read of the real file would not carry the synthetic server 'l'", out.ProjectScope)
	}
}

// pathUnder reports whether path is equal to, or under, base. Both are compared
// after Clean; case-insensitively on Windows.
func pathUnder(base, path string) bool {
	base = filepath.Clean(base)
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		base = strings.ToLower(base)
		path = strings.ToLower(path)
	}
	if base == path {
		return true
	}
	return strings.HasPrefix(path, base+string(filepath.Separator))
}

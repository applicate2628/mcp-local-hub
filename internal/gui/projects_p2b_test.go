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

// TestProjectsScanP2b_LiveClaudeJSON_Untouched is the LIVE-config-safety proof:
// it captures the developer's REAL ~/.claude.json (resolved BEFORE isolation),
// then runs a full project scan under the isolated home, then re-checks the REAL
// file is byte-identical. The reader CANNOT reach the real file (HOME/USERPROFILE
// are redirected during the scan), so this guards the contract rather than the
// mechanism — but it makes a regression that un-isolated the read immediately
// fail here instead of silently corrupting the operator's file.
func TestProjectsScanP2b_LiveClaudeJSON_Untouched(t *testing.T) {
	// Resolve the REAL home BEFORE any isolation. os.UserHomeDir() here reads the
	// process env, which TestMain did NOT redirect for HOME/USERPROFILE, so this
	// is the operator's actual home.
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot resolve real home: %v", err)
	}
	realClaude := filepath.Join(realHome, ".claude.json")

	var hadFile bool
	var beforeBytes []byte
	var beforeMod int64
	if info, statErr := os.Stat(realClaude); statErr == nil && !info.IsDir() {
		hadFile = true
		beforeMod = info.ModTime().UnixNano()
		beforeBytes, err = os.ReadFile(realClaude)
		if err != nil {
			t.Skipf("cannot read real ~/.claude.json for the safety snapshot: %v", err)
		}
	}

	// Now isolate and run a scan that exercises the local-scope reader.
	isolateHome(t)
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, ".mcp.json"), `{"mcpServers":{"p":{"command":"n"}}}`)
	key := fwdSlashKey(root)
	writeSyntheticClaudeJSON(t, `{"projects":{"`+jsonEscPath(key)+`":{"mcpServers":{"l":{}}}}}`)

	s := NewServer(Config{})
	rec := getProjectScan(t, s, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	// Re-check the REAL file is byte-identical (and same mtime / still absent).
	if hadFile {
		afterBytes, rerr := os.ReadFile(realClaude)
		if rerr != nil {
			t.Fatalf("real ~/.claude.json became unreadable after scan: %v", rerr)
		}
		if !reflect.DeepEqual(beforeBytes, afterBytes) {
			t.Fatalf("LIVE-CONFIG-SAFETY VIOLATION: real ~/.claude.json content changed across a scan")
		}
		info, serr := os.Stat(realClaude)
		if serr != nil {
			t.Fatalf("real ~/.claude.json stat failed after scan: %v", serr)
		}
		if info.ModTime().UnixNano() != beforeMod {
			t.Fatalf("LIVE-CONFIG-SAFETY VIOLATION: real ~/.claude.json mtime changed across a scan")
		}
	} else {
		if _, serr := os.Stat(realClaude); serr == nil {
			t.Fatalf("LIVE-CONFIG-SAFETY VIOLATION: scan CREATED a real ~/.claude.json that did not exist")
		}
	}
}

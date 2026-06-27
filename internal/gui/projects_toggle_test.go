// internal/gui/projects_toggle_test.go
//
// Per-project-GUI Phase 3a: POST /api/projects/toggle (the write backend),
// end-to-end through the HTTP handler. Tests:
//   - per-model dispatch effect: B object-member (cursor) add/remove; B-claude
//     Local array-move (never deletes mcpServers); C group servers add/remove.
//   - the classifier dispatches via clients.ProjectToggleOwner (an unsupported
//     (client, scope) is refused with no write).
//   - path-safety: a traversal/relative/nonexistent root is rejected (400), no
//     write outside realRoot, leak-safe body.
//   - no-write-on-block: a blocked/invalid toggle writes nothing.
//
// STATE-SAFETY (CRITICAL — these tests WRITE):
//   - isolateHome(t) fences HOME/USERPROFILE/LOCALAPPDATA/XDG to a temp dir, so
//     ~/.claude.json resolves into the temp home and the LIVE file is
//     structurally unreachable. The internal/gui TestMain additionally fences
//     the binary off the live state dir.
//   - api.SetClientWriteFallbackForTest() reverts clients.WriteConfigFile to a
//     plain os.WriteFile for the test (the secure-write parent-dir DACL gate
//     rejects %TEMP%-backed paths on Windows). t.Cleanup restores it.
//   - Group toggles use api.SetDaemonStateRootForTest so groups.yaml lands in a
//     temp state dir, NEVER the live one.
package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// postToggle issues a same-origin POST /api/projects/toggle with the given body.
func postToggle(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/projects/toggle", bytes.NewReader([]byte(body)))
	req.Host = "127.0.0.1:9081"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.port.Store(9081) // make allowedHost accept the request host
	s.mux.ServeHTTP(rec, req)
	return rec
}

func decodeToggleResp(t *testing.T, rec *httptest.ResponseRecorder) projectToggleResponse {
	t.Helper()
	var out projectToggleResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode toggle resp: %v (body=%s)", err, rec.Body.String())
	}
	return out
}

// TestProjectsToggle_ObjectMember_Cursor: enable then disable a cursor project
// server; the .cursor/mcp.json member is added then removed.
func TestProjectsToggle_ObjectMember_Cursor(t *testing.T) {
	isolateHome(t)
	t.Cleanup(api.SetClientWriteFallbackForTest())
	root := t.TempDir()
	s := NewServer(Config{})

	// ENABLE: member must be written with the supplied value.
	body := `{"root":"` + jsonEscPath(root) + `","client":"cursor","scope":"project-object-member","server":"beta","enable":true,"value":{"url":"http://127.0.0.1:9200/mcp"}}`
	rec := postToggle(t, s, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeToggleResp(t, rec); !got.Enabled {
		t.Errorf("enable read-back Enabled=false, want true")
	}
	cfg := filepath.Join(root, ".cursor", "mcp.json")
	if _, err := os.Stat(cfg); err != nil {
		t.Fatalf(".cursor/mcp.json not created: %v", err)
	}

	// DISABLE: member removed.
	body = `{"root":"` + jsonEscPath(root) + `","client":"cursor","scope":"project-object-member","server":"beta","enable":false}`
	rec = postToggle(t, s, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeToggleResp(t, rec); got.Enabled {
		t.Errorf("disable read-back Enabled=true, want false")
	}
}

// TestProjectsToggle_ObjectMember_EnableRequiresValue: enabling without a value
// is rejected (400) and writes nothing.
func TestProjectsToggle_ObjectMember_EnableRequiresValue(t *testing.T) {
	isolateHome(t)
	t.Cleanup(api.SetClientWriteFallbackForTest())
	root := t.TempDir()
	s := NewServer(Config{})

	before := snapshotTree(t, root)
	body := `{"root":"` + jsonEscPath(root) + `","client":"cursor","scope":"project-object-member","server":"beta","enable":true}`
	rec := postToggle(t, s, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	after := snapshotTree(t, root)
	if len(after) != len(before) {
		t.Errorf("invalid enable wrote files: before=%v after=%v", before, after)
	}
}

// TestProjectsToggle_ClaudeLocal_NeverDeletesMcpServers is the catastrophic-
// corruption guard at the HTTP level: a disable moves the name to
// disabledMcpjsonServers but the project's ~/.claude.json mcpServers definition
// survives.
func TestProjectsToggle_ClaudeLocal_NeverDeletesMcpServers(t *testing.T) {
	isolateHome(t)
	t.Cleanup(api.SetClientWriteFallbackForTest())
	root := t.TempDir()
	key := fwdSlashKey(root)
	claudeBody := `{"projects":{"` + jsonEscPath(key) + `":{` +
		`"mcpServers":{"keepme":{"command":"node"}},` +
		`"enabledMcpjsonServers":["keepme"]}}}`
	claudePath := writeSyntheticClaudeJSON(t, claudeBody)

	s := NewServer(Config{})
	body := `{"root":"` + jsonEscPath(root) + `","client":"claude-code","scope":"claude-local-membership","server":"keepme","enable":false}`
	rec := postToggle(t, s, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeToggleResp(t, rec); got.Enabled {
		t.Errorf("read-back Enabled=true after disable, want false")
	}

	// The mcpServers DEFINITION must still be present.
	data, _ := os.ReadFile(claudePath)
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse claude.json: %v (body=%s)", err, data)
	}
	projects, _ := m["projects"].(map[string]any)
	var entry map[string]any
	for _, v := range projects {
		entry, _ = v.(map[string]any)
	}
	if entry == nil {
		t.Fatalf("project entry lost")
	}
	ms, ok := entry["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers DELETED (data-loss!)")
	}
	if _, ok := ms["keepme"]; !ok {
		t.Errorf("mcpServers[keepme] definition deleted by a disable (decision-5 violation)")
	}
}

// TestProjectsToggle_GroupServers: enable then disable a server in a group via
// the C owner (api.ReadModifyWriteGroups). groups.yaml is sandboxed.
func TestProjectsToggle_GroupServers(t *testing.T) {
	isolateHome(t)
	stateDir := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))
	t.Cleanup(api.SetClientWriteFallbackForTest())

	// Seed a group via the real write path (sandboxed state dir).
	if err := api.WriteGroups(api.GroupsConfig{Version: 1, Groups: []api.Group{{Name: "frontend", Servers: []string{"existing"}}}}); err != nil {
		t.Fatalf("seed group: %v", err)
	}

	s := NewServer(Config{})
	enable := `{"scope":"group-servers","group":"frontend","server":"memory","enable":true}`
	rec := postToggle(t, s, enable)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeToggleResp(t, rec); !got.Enabled {
		t.Errorf("group enable read-back Enabled=false")
	}
	// Verify on disk.
	cfg, err := api.LoadGroups()
	if err != nil {
		t.Fatalf("load groups: %v", err)
	}
	if !groupContains(cfg, "frontend", "memory") || !groupContains(cfg, "frontend", "existing") {
		t.Errorf("group servers after enable wrong: %+v", cfg.Groups)
	}

	disable := `{"scope":"group-servers","group":"frontend","server":"memory","enable":false}`
	rec = postToggle(t, s, disable)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeToggleResp(t, rec); got.Enabled {
		t.Errorf("group disable read-back Enabled=true")
	}
	cfg, _ = api.LoadGroups()
	if groupContains(cfg, "frontend", "memory") {
		t.Errorf("memory still in group after disable")
	}
	if !groupContains(cfg, "frontend", "existing") {
		t.Errorf("existing member lost after disabling a different one")
	}
}

func groupContains(cfg api.GroupsConfig, group, server string) bool {
	for _, g := range cfg.Groups {
		if g.Name == group {
			for _, s := range g.Servers {
				if s == server {
					return true
				}
			}
		}
	}
	return false
}

// TestProjectsToggle_GroupNotFound: a toggle against a missing group is 404 and
// writes no new group.
func TestProjectsToggle_GroupNotFound(t *testing.T) {
	isolateHome(t)
	stateDir := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))
	t.Cleanup(api.SetClientWriteFallbackForTest())

	s := NewServer(Config{})
	// Use a ROUTABLE server name ("memory" is in the default routable set) so the
	// bot-PR#433-finding-2 ENABLE name-gate PASSES and the toggle reaches the
	// group-existence check — proving "group not found" → 404 (not the
	// unknown-server 400). An unknown server would short-circuit to 400 first.
	body := `{"scope":"group-servers","group":"ghost","server":"memory","enable":true}`
	rec := postToggle(t, s, body)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestProjectsToggle_Unsupported: an unsupported (client, scope) is refused
// (400) with no write — the classifier returns OwnerUnsupported.
func TestProjectsToggle_Unsupported(t *testing.T) {
	isolateHome(t)
	t.Cleanup(api.SetClientWriteFallbackForTest())
	root := t.TempDir()
	s := NewServer(Config{})

	before := snapshotTree(t, root)
	// claude-local-membership is claude-only; cursor here → unsupported.
	body := `{"root":"` + jsonEscPath(root) + `","client":"cursor","scope":"claude-local-membership","server":"x","enable":true}`
	rec := postToggle(t, s, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var env struct{ Code string }
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Code != "PROJECT_TOGGLE_UNSUPPORTED" {
		t.Errorf("code=%q, want PROJECT_TOGGLE_UNSUPPORTED", env.Code)
	}
	after := snapshotTree(t, root)
	if len(after) != len(before) {
		t.Errorf("unsupported toggle wrote files: %v", after)
	}
}

// TestProjectsToggle_PathSafety: a hostile/degenerate root for a project-config
// toggle is rejected (400), leak-safe, and writes nothing outside.
func TestProjectsToggle_PathSafety(t *testing.T) {
	isolateHome(t)
	t.Cleanup(api.SetClientWriteFallbackForTest())
	existing := t.TempDir()
	s := NewServer(Config{})

	cases := []struct{ name, root string }{
		{"relative", "some/rel/dir"},
		{"relative-traversal", "../../etc"},
		{"dot", "."},
		{"nonexistent", filepath.Join(existing, "nope-xyz")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := `{"root":"` + jsonEscPath(c.root) + `","client":"cursor","scope":"project-object-member","server":"x","enable":true,"value":{"url":"http://127.0.0.1:1/mcp"}}`
			rec := postToggle(t, s, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("root=%q status=%d, want 400; body=%s", c.root, rec.Code, rec.Body.String())
			}
			b := rec.Body.String()
			if strings.Contains(b, existing) {
				t.Errorf("body leaks internal temp path: %q", b)
			}
			var env struct{ Error, Code string }
			_ = json.Unmarshal(rec.Body.Bytes(), &env)
			if env.Code != "PROJECT_ROOT_INVALID" {
				t.Errorf("code=%q, want PROJECT_ROOT_INVALID", env.Code)
			}
			if env.Error != "internal error" {
				t.Errorf("error=%q, want redacted body", env.Error)
			}
		})
	}
}

// TestProjectsToggle_MissingServer: a toggle with no server is 400, no write.
func TestProjectsToggle_MissingServer(t *testing.T) {
	isolateHome(t)
	t.Cleanup(api.SetClientWriteFallbackForTest())
	root := t.TempDir()
	s := NewServer(Config{})
	before := snapshotTree(t, root)
	body := `{"root":"` + jsonEscPath(root) + `","client":"cursor","scope":"project-object-member","enable":true,"value":{}}`
	rec := postToggle(t, s, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if after := snapshotTree(t, root); len(after) != len(before) {
		t.Errorf("missing-server toggle wrote files: %v", after)
	}
}

// --- bot PR #433 round 2 ---

// TestProjectsToggle_GroupServers_RepublishesWhenHubLive pins finding 1: a
// group-servers toggle re-publishes the live hub snapshot via the SAME
// republishGroupsSnapshot seam /api/groups uses (so a membership change is
// effective in the running hub immediately). Republish must fire on BOTH the
// enable and the disable.
func TestProjectsToggle_GroupServers_RepublishesWhenHubLive(t *testing.T) {
	isolateHome(t)
	stateDir := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))
	t.Cleanup(api.SetClientWriteFallbackForTest())

	if err := api.WriteGroups(api.GroupsConfig{Version: 1, Groups: []api.Group{{Name: "frontend", Servers: []string{}}}}); err != nil {
		t.Fatalf("seed group: %v", err)
	}

	s := NewServer(Config{})
	// Deterministic routable set so the ENABLE name-gate passes for "memory".
	s.groups = diskTestGroupsAPI{available: []string{"memory", "time"}}
	// Mark the gate-ON hub listener live so the republish path runs.
	comp := &HubListenerComponents{port: 9300}
	comp.alive.Store(true)
	s.hubMcpComp.Store(comp)
	republishCalls := 0
	s.groupsRepublishFn = func(_ context.Context, _ *api.API) error { republishCalls++; return nil }

	rec := postToggle(t, s, `{"scope":"group-servers","group":"frontend","server":"memory","enable":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable status=%d body=%s", rec.Code, rec.Body.String())
	}
	if republishCalls != 1 {
		t.Fatalf("republish fired %d times on ENABLE, want 1 (finding 1: same seam as /api/groups)", republishCalls)
	}

	rec = postToggle(t, s, `{"scope":"group-servers","group":"frontend","server":"memory","enable":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", rec.Code, rec.Body.String())
	}
	if republishCalls != 2 {
		t.Fatalf("republish fired %d times after DISABLE, want 2 (republish on every membership change)", republishCalls)
	}
}

// TestProjectsToggle_GroupServers_NoRepublishWhenHubOff: with the hub gate-OFF
// the republish is skipped (no spurious call) and the durable write still lands.
func TestProjectsToggle_GroupServers_NoRepublishWhenHubOff(t *testing.T) {
	isolateHome(t)
	stateDir := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))
	t.Cleanup(api.SetClientWriteFallbackForTest())

	if err := api.WriteGroups(api.GroupsConfig{Version: 1, Groups: []api.Group{{Name: "frontend", Servers: []string{}}}}); err != nil {
		t.Fatalf("seed group: %v", err)
	}

	s := NewServer(Config{})
	s.groups = diskTestGroupsAPI{available: []string{"memory"}}
	// No hubMcpComp stored → HubMcpEndpointActive() false → gate-OFF.
	republishCalls := 0
	s.groupsRepublishFn = func(_ context.Context, _ *api.API) error { republishCalls++; return nil }

	rec := postToggle(t, s, `{"scope":"group-servers","group":"frontend","server":"memory","enable":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if republishCalls != 0 {
		t.Fatalf("republish fired %d times with hub gate-OFF, want 0", republishCalls)
	}
	cfg, err := api.LoadGroups()
	if err != nil {
		t.Fatalf("LoadGroups: %v", err)
	}
	if !groupContains(cfg, "frontend", "memory") {
		t.Fatalf("durable write missing after gate-OFF enable: %+v", cfg.Groups)
	}
}

// TestProjectsToggle_GroupServers_EnableRejectsUnknownServer pins finding 2: an
// ENABLE of a server NOT in the routable-server set is a redacted 400 and is
// NEVER persisted to groups.yaml.
func TestProjectsToggle_GroupServers_EnableRejectsUnknownServer(t *testing.T) {
	isolateHome(t)
	stateDir := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))
	t.Cleanup(api.SetClientWriteFallbackForTest())

	if err := api.WriteGroups(api.GroupsConfig{Version: 1, Groups: []api.Group{{Name: "frontend", Servers: []string{}}}}); err != nil {
		t.Fatalf("seed group: %v", err)
	}

	s := NewServer(Config{})
	// "ghost" is NOT in the routable set.
	s.groups = diskTestGroupsAPI{available: []string{"memory", "time"}}
	s.groupsRepublishFn = func(_ context.Context, _ *api.API) error { return nil }

	rec := postToggle(t, s, `{"scope":"group-servers","group":"frontend","server":"ghost","enable":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400 for unknown server", rec.Code, rec.Body.String())
	}
	var env struct{ Error, Code string }
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope %q: %v", rec.Body.String(), err)
	}
	if env.Code != "PROJECT_TOGGLE_UNKNOWN_SERVER" {
		t.Errorf("code=%q, want PROJECT_TOGGLE_UNKNOWN_SERVER", env.Code)
	}
	// The unknown name must NOT be in groups.yaml.
	cfg, err := api.LoadGroups()
	if err != nil {
		t.Fatalf("LoadGroups: %v", err)
	}
	if groupContains(cfg, "frontend", "ghost") {
		t.Fatalf("unknown server leaked into groups.yaml: %+v", cfg.Groups)
	}
}

// TestProjectsToggle_GroupServers_DisableNotNameGated confirms finding 2 is
// ENABLE-only: a DISABLE of a now-non-routable stale member still succeeds —
// cleanup must always be allowed.
func TestProjectsToggle_GroupServers_DisableNotNameGated(t *testing.T) {
	isolateHome(t)
	stateDir := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))
	t.Cleanup(api.SetClientWriteFallbackForTest())

	// Group already carries a stale, now-non-routable server "ghost".
	if err := api.WriteGroups(api.GroupsConfig{Version: 1, Groups: []api.Group{{Name: "frontend", Servers: []string{"ghost"}}}}); err != nil {
		t.Fatalf("seed group: %v", err)
	}

	s := NewServer(Config{})
	s.groups = diskTestGroupsAPI{available: []string{"memory"}} // "ghost" absent
	s.groupsRepublishFn = func(_ context.Context, _ *api.API) error { return nil }

	rec := postToggle(t, s, `{"scope":"group-servers","group":"frontend","server":"ghost","enable":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 (disable of a stale member must be allowed)", rec.Code, rec.Body.String())
	}
	cfg, err := api.LoadGroups()
	if err != nil {
		t.Fatalf("LoadGroups: %v", err)
	}
	if groupContains(cfg, "frontend", "ghost") {
		t.Fatalf("stale member not removed by disable: %+v", cfg.Groups)
	}
}

// TestProjectsScan_PreservesObjectMemberToggleValue pins finding 5: the project
// scan exposes a per-entry `toggle_value` for OBJECT-MEMBER substrates carrying
// the verbatim member value, while still stripping `raw`. A secret:<key> ref
// survives verbatim (no resolution) — the no-leak posture.
func TestProjectsScan_PreservesObjectMemberToggleValue(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, ".cursor", "mcp.json"),
		`{"mcpServers":{"beta":{"command":"node","args":["b.js"],"env":{"TOKEN":"secret:beta_token"}}}}`)

	s := NewServer(Config{})
	rec := getProjectScan(t, s, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out api.ScanResult
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var beta *api.ScanEntry
	for i := range out.Entries {
		if out.Entries[i].Name == "beta" {
			beta = &out.Entries[i]
			break
		}
	}
	if beta == nil {
		t.Fatalf("beta entry absent (names=%v)", entryNames(out))
	}
	ce, ok := beta.ClientPresence["cursor"]
	if !ok {
		t.Fatalf("beta has no cursor presence: %+v", beta.ClientPresence)
	}
	if ce.Raw != nil {
		t.Errorf("cursor entry leaked Raw on the wire: %+v", ce.Raw)
	}
	if ce.ToggleValue == nil {
		t.Fatalf("cursor object-member entry missing toggle_value (finding 5: P3b re-enable source)")
	}
	if got, _ := ce.ToggleValue["command"].(string); got != "node" {
		t.Errorf("toggle_value.command=%q, want node (verbatim member)", got)
	}
	env2, _ := ce.ToggleValue["env"].(map[string]any)
	if env2 == nil {
		t.Fatalf("toggle_value missing env: %+v", ce.ToggleValue)
	}
	if got, _ := env2["TOKEN"].(string); got != "secret:beta_token" {
		t.Errorf("toggle_value env.TOKEN=%q, want verbatim secret:beta_token (NO resolution)", got)
	}
}

// TestProjectsScan_ToggleValueForVscodeServersKey confirms finding 5 also covers
// the vscode `servers`-key object-member substrate.
func TestProjectsScan_ToggleValueForVscodeServersKey(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, ".vscode", "mcp.json"),
		`{"servers":{"gamma":{"command":"uvx","args":["g"]}}}`)

	s := NewServer(Config{})
	rec := getProjectScan(t, s, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out api.ScanResult
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var gamma *api.ScanEntry
	for i := range out.Entries {
		if out.Entries[i].Name == "gamma" {
			gamma = &out.Entries[i]
			break
		}
	}
	if gamma == nil {
		t.Fatalf("gamma entry absent (names=%v)", entryNames(out))
	}
	ce, ok := gamma.ClientPresence["vscode"]
	if !ok {
		t.Fatalf("gamma has no vscode presence: %+v", gamma.ClientPresence)
	}
	if ce.ToggleValue == nil {
		t.Fatalf("vscode object-member entry missing toggle_value")
	}
	if got, _ := ce.ToggleValue["command"].(string); got != "uvx" {
		t.Errorf("toggle_value.command=%q, want uvx", got)
	}
}

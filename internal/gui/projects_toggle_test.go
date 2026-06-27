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

// TestProjectsToggle_ClaudeObjectMember_RejectedNeverMemberDeletes is the
// security falsifier for bug 2026-06-27 at the HTTP boundary: a direct
// /api/projects/toggle caller sending {client:"claude-code",
// scope:"project-object-member", enable:false} against a project's checked-in
// .mcp.json is REJECTED (400 PROJECT_TOGGLE_UNSUPPORTED, no write) and the
// mcpServers[<server>] DEFINITION SURVIVES — the member-delete data-loss the P3b
// frontend fix prevented is now closed at the backend classifier too.
func TestProjectsToggle_ClaudeObjectMember_RejectedNeverMemberDeletes(t *testing.T) {
	isolateHome(t)
	t.Cleanup(api.SetClientWriteFallbackForTest())
	root := t.TempDir()
	s := NewServer(Config{})

	// Seed a checked-in .mcp.json with a claude server definition (the shared,
	// collaborator-visible file a member-delete would destroy).
	mcpPath := filepath.Join(root, ".mcp.json")
	seed := `{"mcpServers":{"keepme":{"command":"node","args":["server.js"]}}}`
	if err := os.WriteFile(mcpPath, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed .mcp.json: %v", err)
	}
	before, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatalf("read seeded .mcp.json: %v", err)
	}

	body := `{"root":"` + jsonEscPath(root) + `","client":"claude-code","scope":"project-object-member","server":"keepme","enable":false}`
	rec := postToggle(t, s, body)

	// 1) The destructive route is refused (no guess, no write).
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (claude-code object-member toggle must be rejected); body=%s", rec.Code, rec.Body.String())
	}
	var env struct{ Code string }
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Code != "PROJECT_TOGGLE_UNSUPPORTED" {
		t.Errorf("code=%q, want PROJECT_TOGGLE_UNSUPPORTED", env.Code)
	}

	// 2) The shared .mcp.json must be byte-for-byte UNCHANGED (the reject path
	//    makes NO write at all) AND the mcpServers[keepme] DEFINITION must STILL
	//    be present (no member-delete data-loss — the exact bug 2026-06-27).
	after, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatalf("re-read .mcp.json: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("rejected claude-code object-member toggle REWROTE the shared .mcp.json (must be byte-unchanged)\nbefore=\n%s\nafter=\n%s", before, after)
	}
	var m map[string]any
	if err := json.Unmarshal(after, &m); err != nil {
		t.Fatalf("parse .mcp.json: %v (file=\n%s)", err, after)
	}
	ms, ok := m["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers section DELETED (data-loss!); file=\n%s", after)
	}
	if _, ok := ms["keepme"]; !ok {
		t.Errorf("mcpServers[keepme] definition member-DELETED by a claude-code object-member disable (the exact bug 2026-06-27 data-loss); file=\n%s", after)
	}
}

// TestProjectsToggle_CursorVscodeObjectMember_StillWork confirms the narrowing
// did NOT break the clients that legitimately use object-member: cursor and
// vscode each enable then disable a project server through the object-member
// owner (they have no approval array, so member add/remove IS their correct
// semantic).
func TestProjectsToggle_CursorVscodeObjectMember_StillWork(t *testing.T) {
	isolateHome(t)
	t.Cleanup(api.SetClientWriteFallbackForTest())
	s := NewServer(Config{})

	cases := []struct {
		client  string
		relFile string
	}{
		{"cursor", filepath.Join(".cursor", "mcp.json")},
		{"vscode", filepath.Join(".vscode", "mcp.json")},
	}
	for _, c := range cases {
		t.Run(c.client, func(t *testing.T) {
			root := t.TempDir()
			enable := `{"root":"` + jsonEscPath(root) + `","client":"` + c.client +
				`","scope":"project-object-member","server":"beta","enable":true,"value":{"url":"http://127.0.0.1:9200/mcp"}}`
			rec := postToggle(t, s, enable)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s enable status=%d body=%s", c.client, rec.Code, rec.Body.String())
			}
			if got := decodeToggleResp(t, rec); !got.Enabled {
				t.Errorf("%s enable read-back Enabled=false, want true", c.client)
			}
			if _, err := os.Stat(filepath.Join(root, c.relFile)); err != nil {
				t.Fatalf("%s project config not created: %v", c.client, err)
			}

			disable := `{"root":"` + jsonEscPath(root) + `","client":"` + c.client +
				`","scope":"project-object-member","server":"beta","enable":false}`
			rec = postToggle(t, s, disable)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s disable status=%d body=%s", c.client, rec.Code, rec.Body.String())
			}
			if got := decodeToggleResp(t, rec); got.Enabled {
				t.Errorf("%s disable read-back Enabled=true, want false", c.client)
			}
		})
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

// --- bot PR #433 round 3 ---

// groupsYamlPath returns the on-disk groups.yaml path under the active
// (test-overridden) daemon state dir.
func groupsYamlPath(t *testing.T) string {
	t.Helper()
	dir, err := api.DaemonStateDirReadOnly()
	if err != nil {
		t.Fatalf("resolve state dir: %v", err)
	}
	return filepath.Join(dir, "groups.yaml")
}

// TestProjectsToggle_GroupNotFound_NoWriteWhenNoFile pins finding 2 (bot PR #433
// r3): a toggle of a NON-EXISTENT group when NO groups.yaml exists yet returns
// 404 AND creates NO groups.yaml. Before the fix the RMW callback returned
// nil,nil even on a miss, so ReadModifyWriteGroups normalized/CREATED the file
// before the handler 404'd.
func TestProjectsToggle_GroupNotFound_NoWriteWhenNoFile(t *testing.T) {
	isolateHome(t)
	stateDir := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))
	t.Cleanup(api.SetClientWriteFallbackForTest())

	gpath := groupsYamlPath(t)
	if _, err := os.Stat(gpath); err == nil {
		t.Fatalf("precondition: groups.yaml must NOT exist before the toggle")
	}

	s := NewServer(Config{})
	// "memory" is routable so the ENABLE name-gate passes and the toggle reaches
	// the group-existence check (proving 404 from a missing GROUP, not a 400 from
	// an unknown server).
	rec := postToggle(t, s, `{"scope":"group-servers","group":"ghost","server":"memory","enable":true}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	// The missing-group toggle must NOT have created groups.yaml (finding 2).
	if _, err := os.Stat(gpath); err == nil {
		t.Fatalf("missing-group toggle CREATED groups.yaml (finding 2: must make NO write): %s", gpath)
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected stat error on %s: %v", gpath, err)
	}
}

// TestProjectsToggle_GroupNotFound_FileByteUnchanged pins finding 2's
// existing-file case: a toggle of a non-existent group leaves an EXISTING
// groups.yaml byte-for-byte unchanged (no normalize/rewrite on the not-found
// path).
func TestProjectsToggle_GroupNotFound_FileByteUnchanged(t *testing.T) {
	isolateHome(t)
	stateDir := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))
	t.Cleanup(api.SetClientWriteFallbackForTest())

	// Seed an existing groups.yaml with one real group, via the real write path.
	if err := api.WriteGroups(api.GroupsConfig{Version: 1, Groups: []api.Group{{Name: "frontend", Servers: []string{"existing"}}}}); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	gpath := groupsYamlPath(t)
	before, err := os.ReadFile(gpath)
	if err != nil {
		t.Fatalf("read seeded groups.yaml: %v", err)
	}

	s := NewServer(Config{})
	s.groups = diskTestGroupsAPI{available: []string{"memory"}}
	rec := postToggle(t, s, `{"scope":"group-servers","group":"ghost","server":"memory","enable":true}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	after, err := os.ReadFile(gpath)
	if err != nil {
		t.Fatalf("read groups.yaml after toggle: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("missing-group toggle rewrote groups.yaml (finding 2: must be byte-unchanged)\nbefore=\n%s\nafter=\n%s", before, after)
	}
}

// TestProjectsToggle_GroupServers_DisableDropsToolsHidden pins finding 5 (bot PR
// #433 r3): disabling a server from a group removes it from BOTH the servers list
// AND the group's tools_hidden map — so groups.yaml is never left in the
// editor-rejected state /api/groups forbids (a tools_hidden key for a non-member),
// and re-adding the server later does not silently re-apply the stale filter.
func TestProjectsToggle_GroupServers_DisableDropsToolsHidden(t *testing.T) {
	isolateHome(t)
	stateDir := t.TempDir()
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))
	t.Cleanup(api.SetClientWriteFallbackForTest())

	// Seed a group whose member "memory" carries a per-server hidden-tool filter.
	if err := api.WriteGroups(api.GroupsConfig{Version: 1, Groups: []api.Group{{
		Name:        "frontend",
		Servers:     []string{"memory", "time"},
		ToolsHidden: map[string][]string{"memory": {"delete_file"}},
	}}}); err != nil {
		t.Fatalf("seed group: %v", err)
	}

	s := NewServer(Config{})
	s.groups = diskTestGroupsAPI{available: []string{"memory", "time"}}
	s.groupsRepublishFn = func(_ context.Context, _ *api.API) error { return nil }

	// DISABLE "memory": dropped from servers AND tools_hidden.
	rec := postToggle(t, s, `{"scope":"group-servers","group":"frontend","server":"memory","enable":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", rec.Code, rec.Body.String())
	}
	cfg, err := api.LoadGroups()
	if err != nil {
		t.Fatalf("LoadGroups: %v", err)
	}
	g := findGroup(cfg, "frontend")
	if g == nil {
		t.Fatalf("group lost after disable")
	}
	if groupHasServer(g.Servers, "memory") {
		t.Errorf("memory still in servers after disable: %+v", g.Servers)
	}
	if _, ok := g.ToolsHidden["memory"]; ok {
		t.Errorf("tools_hidden[memory] survived a member removal (finding 5): %+v", g.ToolsHidden)
	}
	// The other member's filter (none here) / membership must be intact.
	if !groupHasServer(g.Servers, "time") {
		t.Errorf("unrelated member 'time' lost: %+v", g.Servers)
	}

	// RE-ENABLE "memory": it returns as a plain member with NO stale filter.
	rec = postToggle(t, s, `{"scope":"group-servers","group":"frontend","server":"memory","enable":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-enable status=%d body=%s", rec.Code, rec.Body.String())
	}
	cfg, _ = api.LoadGroups()
	g = findGroup(cfg, "frontend")
	if g == nil || !groupHasServer(g.Servers, "memory") {
		t.Fatalf("memory not re-added: %+v", cfg.Groups)
	}
	if _, ok := g.ToolsHidden["memory"]; ok {
		t.Errorf("re-adding memory resurrected a stale tools_hidden filter (finding 5): %+v", g.ToolsHidden)
	}
}

// findGroup returns a pointer to the named group in cfg, or nil.
func findGroup(cfg api.GroupsConfig, name string) *api.Group {
	for i := range cfg.Groups {
		if cfg.Groups[i].Name == name {
			return &cfg.Groups[i]
		}
	}
	return nil
}

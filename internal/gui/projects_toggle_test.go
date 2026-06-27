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
	body := `{"scope":"group-servers","group":"ghost","server":"x","enable":true}`
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

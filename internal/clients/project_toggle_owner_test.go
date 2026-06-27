// internal/clients/project_toggle_owner_test.go
//
// Per-project-GUI Phase 3a: the toggle CLASSIFIER + the Model-B object-member
// leaf writer.
//
//   - ProjectToggleOwner dispatch: (client, scope) → the right write owner; the
//     GUI never re-derives this mapping.
//   - ToggleProjectObjectMember: enable adds the member under the client's
//     section key (mcpServers for cursor/claude, servers for vscode); disable
//     removes it. Comments/unrelated keys preserved (hujson). NEVER touches the
//     adapter GLOBAL paths — it writes the supplied PROJECT config path.
//
// STATE-SAFETY: writes target a t.TempDir() project config path; the test
// default WriteConfigFile (plain os.WriteFile) handles it. No HOME/state dir is
// touched (the object-member writer has no ambient path).
package clients

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectToggleOwner_Dispatch(t *testing.T) {
	cases := []struct {
		name   string
		client string
		scope  ProjectToggleScope
		want   ProjectToggleOwnerKind
	}{
		{"workspace-A", "anything", ScopeWorkspaceLSP, OwnerWorkspaceRegister},
		{"cursor-object-member", "cursor", ScopeProjectObjectMember, OwnerProjectObjectMember},
		{"vscode-object-member", "vscode", ScopeProjectObjectMember, OwnerProjectObjectMember},
		{"claude-project-object-member", "claude-code", ScopeProjectObjectMember, OwnerProjectObjectMember},
		{"unknown-client-object-member", "emacs", ScopeProjectObjectMember, OwnerUnsupported},
		{"claude-local-membership", "claude-code", ScopeClaudeLocalMembership, OwnerClaudeLocalMembership},
		{"non-claude-local-membership-unsupported", "cursor", ScopeClaudeLocalMembership, OwnerUnsupported},
		{"group-servers", "", ScopeGroupServers, OwnerGroupServers},
		{"empty-scope-unsupported", "cursor", ProjectToggleScope(""), OwnerUnsupported},
		{"bogus-scope-unsupported", "cursor", ProjectToggleScope("nope"), OwnerUnsupported},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ProjectToggleOwner(c.client, c.scope); got != c.want {
				t.Errorf("ProjectToggleOwner(%q,%q) = %d, want %d", c.client, c.scope, got, c.want)
			}
		})
	}
}

func TestToggleProjectObjectMember_AddRemove(t *testing.T) {
	cases := []struct {
		client     string
		relFile    string
		sectionKey string
	}{
		{"cursor", filepath.Join(".cursor", "mcp.json"), "mcpServers"},
		{"vscode", filepath.Join(".vscode", "mcp.json"), "servers"},
		{"claude-code", ".mcp.json", "mcpServers"},
	}
	for _, c := range cases {
		t.Run(c.client, func(t *testing.T) {
			root := t.TempDir()
			cfg := filepath.Join(root, c.relFile)

			// ENABLE: member added under the correct section key.
			val := map[string]any{"command": "node", "args": []any{"x.js"}}
			if err := ToggleProjectObjectMember(c.client, cfg, "srv", val, true); err != nil {
				t.Fatalf("enable: %v", err)
			}
			present, err := ProjectObjectMemberPresent(c.client, cfg, "srv")
			if err != nil {
				t.Fatalf("present read-back: %v", err)
			}
			if !present {
				t.Fatalf("member 'srv' absent after enable")
			}
			// The member must live under the CLIENT's section key, not some other.
			assertSectionMember(t, cfg, c.sectionKey, "srv", true)

			// DISABLE: member removed; idempotent re-disable is a no-op.
			if err := ToggleProjectObjectMember(c.client, cfg, "srv", nil, false); err != nil {
				t.Fatalf("disable: %v", err)
			}
			present, _ = ProjectObjectMemberPresent(c.client, cfg, "srv")
			if present {
				t.Errorf("member 'srv' still present after disable")
			}
			if err := ToggleProjectObjectMember(c.client, cfg, "srv", nil, false); err != nil {
				t.Errorf("idempotent re-disable errored: %v", err)
			}
		})
	}
}

// TestToggleProjectObjectMember_PreservesUnrelatedAndComments: enabling one
// member must keep the operator's other members + JSONC comments intact.
func TestToggleProjectObjectMember_PreservesUnrelatedAndComments(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `{
  // operator comment
  "mcpServers": { "existing": { "url": "http://127.0.0.1:9200/mcp" } },
  "unrelatedTop": 7
}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := ToggleProjectObjectMember("cursor", cfg, "added", map[string]any{"command": "deno"}, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	raw, _ := os.ReadFile(cfg)
	if !containsBytes(raw, "operator comment") {
		t.Errorf("comment lost; file=\n%s", raw)
	}
	if !containsBytes(raw, "unrelatedTop") {
		t.Errorf("unrelated top-level key lost; file=\n%s", raw)
	}
	assertSectionMember(t, cfg, "mcpServers", "existing", true)
	assertSectionMember(t, cfg, "mcpServers", "added", true)
}

// TestToggleProjectObjectMember_UnsupportedClient refuses (no write) for a
// client absent from the project-scope registry.
func TestToggleProjectObjectMember_UnsupportedClient(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "x.json")
	if err := ToggleProjectObjectMember("emacs", cfg, "srv", map[string]any{}, true); err == nil {
		t.Errorf("expected error for unsupported client, got nil")
	}
	if _, err := os.Stat(cfg); err == nil {
		t.Errorf("unsupported-client toggle wrote a file (must be no-write)")
	}
}

func assertSectionMember(t *testing.T, cfg, sectionKey, name string, want bool) {
	t.Helper()
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("read %s: %v", cfg, err)
	}
	m, err := parseJSONCBytes(data)
	if err != nil {
		t.Fatalf("parse %s: %v", cfg, err)
	}
	section, _ := m[sectionKey].(map[string]any)
	_, present := section[name]
	if present != want {
		t.Errorf("section %q member %q present=%v, want %v (file=\n%s)", sectionKey, name, present, want, data)
	}
}

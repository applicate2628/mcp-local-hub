package api

import (
	"testing"
	"time"
)

// TestEnrichStatusWithRegistry_OrphanWorkspaceTaskPreservesRawState guards
// the "missing registry entry" edge case for a workspace-scoped scheduler
// task. Without this guard, deriveState saw Port=0 → alive=false → the
// raw "Running" would flip to "Starting", misreporting a healthy orphan
// proxy as still starting. Keep the raw scheduler state when no matching
// registry row exists so the operator sees the truth and can investigate.
func TestEnrichStatusWithRegistry_OrphanWorkspaceTaskPreservesRawState(t *testing.T) {
	dir := t.TempDir()
	regPath := dir + "/ws.yaml"
	// Registry is EMPTY — the task exists in scheduler but has no matching
	// registry entry (corruption / stale scheduler task).
	rows := []DaemonStatus{
		{
			TaskName: "mcp-local-hub-lsp-deadbeef-python",
			State:    "Running",
			NextRun:  "",
		},
	}
	enrichStatusWithRegistry(rows, "", regPath)
	if rows[0].State != "Running" {
		t.Errorf("orphan workspace-scoped task: State = %q, want %q (raw scheduler state must be preserved when registry has no entry)",
			rows[0].State, "Running")
	}
	if rows[0].Port != 0 {
		t.Errorf("expected Port=0 (no registry entry to resolve from); got %d", rows[0].Port)
	}
}

// TestEnrichStatusWithRegistry_WorkspaceScoped seeds a registry entry for a
// lazy-proxy task name and asserts enrichStatusWithRegistry populates every
// workspace-scoped field (Workspace, Language, Backend, Lifecycle,
// LastMaterializedAt, LastToolsCallAt, LastError, Port).
func TestEnrichStatusWithRegistry_WorkspaceScoped(t *testing.T) {
	dir := t.TempDir()
	regPath := dir + "/ws.yaml"
	reg := NewRegistry(regPath)
	now := time.Now().UTC().Truncate(time.Second)
	reg.Put(WorkspaceEntry{
		WorkspaceKey:       "abcd1234",
		WorkspacePath:      "/home/u/project",
		Language:           "python",
		Backend:            "mcp-language-server",
		Port:               9217,
		TaskName:           "mcp-local-hub-lsp-abcd1234-python",
		Lifecycle:          LifecycleActive,
		LastMaterializedAt: now.Add(-30 * time.Minute),
		LastToolsCallAt:    now.Add(-5 * time.Minute),
		LastError:          "",
	})
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}

	rows := []DaemonStatus{
		{TaskName: `\mcp-local-hub-lsp-abcd1234-python`},
	}
	enrichStatusWithRegistry(rows, "", regPath)
	r := rows[0]
	if r.Workspace != "/home/u/project" {
		t.Errorf("Workspace = %q, want /home/u/project", r.Workspace)
	}
	if r.Language != "python" {
		t.Errorf("Language = %q, want python", r.Language)
	}
	if r.Backend != "mcp-language-server" {
		t.Errorf("Backend = %q, want mcp-language-server", r.Backend)
	}
	if r.Lifecycle != LifecycleActive {
		t.Errorf("Lifecycle = %q, want %q", r.Lifecycle, LifecycleActive)
	}
	if !r.LastMaterializedAt.Equal(now.Add(-30 * time.Minute)) {
		t.Errorf("LastMaterializedAt = %v, want %v", r.LastMaterializedAt, now.Add(-30*time.Minute))
	}
	if !r.LastToolsCallAt.Equal(now.Add(-5 * time.Minute)) {
		t.Errorf("LastToolsCallAt = %v, want %v", r.LastToolsCallAt, now.Add(-5*time.Minute))
	}
	if r.Port != 9217 {
		t.Errorf("Port = %d, want 9217", r.Port)
	}
}

// TestEnrichStatusWithRegistry_FailedEntryCarriesLastError asserts a
// missing-or-failed entry's LastError round-trips through enrichment.
func TestEnrichStatusWithRegistry_FailedEntryCarriesLastError(t *testing.T) {
	dir := t.TempDir()
	regPath := dir + "/ws.yaml"
	reg := NewRegistry(regPath)
	reg.Put(WorkspaceEntry{
		WorkspaceKey: "deadbeef",
		Language:     "go",
		Backend:      "gopls-mcp",
		Port:         9220,
		TaskName:     "mcp-local-hub-lsp-deadbeef-go",
		Lifecycle:    LifecycleMissing,
		LastError:    "gopls not on PATH",
	})
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	rows := []DaemonStatus{
		{TaskName: "mcp-local-hub-lsp-deadbeef-go"},
	}
	enrichStatusWithRegistry(rows, "", regPath)
	if rows[0].Lifecycle != LifecycleMissing {
		t.Errorf("Lifecycle = %q, want %q", rows[0].Lifecycle, LifecycleMissing)
	}
	if rows[0].LastError != "gopls not on PATH" {
		t.Errorf("LastError = %q, want %q", rows[0].LastError, "gopls not on PATH")
	}
}

// TestEnrichStatusWithRegistry_GlobalRowUntouched asserts a non-lazy-proxy
// task name (e.g. `mcp-local-hub-serena-claude`) leaves the workspace-scoped
// fields empty, preserving the stable global-daemon output contract.
func TestEnrichStatusWithRegistry_GlobalRowUntouched(t *testing.T) {
	dir := t.TempDir()
	regPath := dir + "/ws.yaml"
	reg := NewRegistry(regPath)
	reg.Put(WorkspaceEntry{
		WorkspaceKey: "abcd1234",
		Language:     "python",
		TaskName:     "mcp-local-hub-lsp-abcd1234-python",
		Lifecycle:    LifecycleActive,
	})
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	rows := []DaemonStatus{
		{TaskName: `\mcp-local-hub-serena-claude`},
	}
	enrichStatusWithRegistry(rows, "", regPath)
	if rows[0].Lifecycle != "" {
		t.Errorf("global row got lifecycle = %q; must stay empty", rows[0].Lifecycle)
	}
	if rows[0].Workspace != "" || rows[0].Language != "" || rows[0].Backend != "" {
		t.Errorf("global row got workspace fields populated: %+v", rows[0])
	}
	// parseTaskName still runs for the global row.
	if rows[0].Server != "serena" || rows[0].Daemon != "claude" {
		t.Errorf("global row parse broke: Server=%q Daemon=%q", rows[0].Server, rows[0].Daemon)
	}
}

// TestEnrichStatusWithRegistry_NoRegistryFileIsSilentNoop asserts a missing
// registry file does not break enrichment — workspace-scoped rows get their
// task-name split done but fields stay empty.
func TestEnrichStatusWithRegistry_NoRegistryFileIsSilentNoop(t *testing.T) {
	dir := t.TempDir()
	regPath := dir + "/nonexistent.yaml"
	rows := []DaemonStatus{
		{TaskName: "mcp-local-hub-lsp-abcd1234-python"},
	}
	enrichStatusWithRegistry(rows, "", regPath)
	if rows[0].Lifecycle != "" {
		t.Errorf("missing registry should not populate lifecycle; got %q", rows[0].Lifecycle)
	}
	// Server / Daemon still get parsed.
	if rows[0].Server != "mcp-language-server" {
		t.Errorf("Server = %q, want mcp-language-server", rows[0].Server)
	}
}

// TestParseLazyProxyTaskName exercises the pattern classifier.
func TestParseLazyProxyTaskName(t *testing.T) {
	cases := []struct {
		in       string
		wantKey  string
		wantLang string
		wantOK   bool
	}{
		{`mcp-local-hub-lsp-abcd1234-python`, "abcd1234", "python", true},
		{`\mcp-local-hub-lsp-abcd1234-python`, "abcd1234", "python", true},
		{`mcp-local-hub-lsp-deadbeef-vscode-css`, "deadbeef", "vscode-css", true},
		// wrong prefix
		{`mcp-local-hub-serena-claude`, "", "", false},
		{`mcp-local-hub-weekly-refresh`, "", "", false},
		// too-short key (must be exactly 8 hex)
		{`mcp-local-hub-lsp-abc-python`, "", "", false},
		// non-hex key
		{`mcp-local-hub-lsp-ZZZZZZZZ-python`, "", "", false},
		// missing language
		{`mcp-local-hub-lsp-abcd1234-`, "", "", false},
		{`mcp-local-hub-lsp-abcd1234`, "", "", false},
	}
	for _, tc := range cases {
		gotKey, gotLang, gotOK := parseLazyProxyTaskName(tc.in)
		if gotKey != tc.wantKey || gotLang != tc.wantLang || gotOK != tc.wantOK {
			t.Errorf("parseLazyProxyTaskName(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.in, gotKey, gotLang, gotOK, tc.wantKey, tc.wantLang, tc.wantOK)
		}
	}
}

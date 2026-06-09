package api

import (
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api/apitest"
)

// lspOrphanDescriptor builds an LSP workspace-proxy supervisor-intent
// descriptor the way BuildSupervisorDaemonForLSP does (flat argv with
// --workspace / --language).
func lspOrphanDescriptor(wsPath, lang string, port int) SupervisorDaemon {
	return SupervisorDaemon{
		TaskName: `\mcp-local-hub-lsp-` + WorkspaceKey(wsPath) + "-" + lang,
		Server:   "mcp-language-server",
		Daemon:   "lsp-" + lang,
		Command:  "mcphub",
		Args: []string{
			"daemon", "workspace-proxy",
			"--port", "9200",
			"--workspace", wsPath,
			"--language", lang,
		},
		Workspace: wsPath,
		Port:      port,
	}
}

// TestLSPRegistryRowBacksDescriptor_TrueWhenRowPresent_FalseWhenOrphaned is the
// orphaned-LSP-daemon quarantine fix at the registry-membership layer. The
// supervisor reconciler calls this predicate (via Reconciler.LSPRegistryHasRow)
// to EXCLUDE an LSP workspace-proxy descriptor whose backing registry row was
// removed (e.g. by `mcphub workspace unregister`) without the paired intent
// descriptor being removed. It must return true while a (workspace_key,
// language) row exists and false once it is gone.
func TestLSPRegistryRowBacksDescriptor_TrueWhenRowPresent_FalseWhenOrphaned(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	regPath := filepath.Join(dir, "workspaces.yaml")
	prev := defaultRegistryPathFn
	defaultRegistryPathFn = func() (string, error) { return regPath, nil }
	t.Cleanup(func() { defaultRegistryPathFn = prev })

	// Use a canonical path so WorkspaceKey here matches the predicate's
	// CanonicalWorkspacePathForCleanup-derived key.
	canonical, err := CanonicalWorkspacePathForCleanup(dir)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	d := lspOrphanDescriptor(canonical, "python", 9200)

	reg := NewRegistry(regPath)
	if err := reg.PutLSP(WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      "python",
		Backend:       "mcp-language-server",
		Port:          9200,
		TaskName:      "mcp-local-hub-lsp-" + wsKey + "-python",
	}); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Row present → backed.
	if !LSPRegistryRowBacksDescriptor(d) {
		t.Fatalf("descriptor with a live registry row must be reported as backed")
	}

	// Remove the row WITHOUT removing the descriptor — the exact orphan
	// the bug produced — then re-check.
	reg2 := NewRegistry(regPath)
	if err := reg2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	reg2.Remove(wsKey, "python")
	if err := reg2.Save(); err != nil {
		t.Fatalf("Save after remove: %v", err)
	}

	if LSPRegistryRowBacksDescriptor(d) {
		t.Fatalf("descriptor whose registry row was removed must be reported as orphaned (unbacked)")
	}
}

// TestLSPRegistryRowBacksDescriptor_FailsOpen pins the fail-OPEN posture: a
// descriptor missing its --language arg (a malformed / non-LSP shape) or any
// other parse miss must report backed=true so the guard never suppresses a
// legitimate spawn on a degenerate input.
func TestLSPRegistryRowBacksDescriptor_FailsOpen(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	regPath := filepath.Join(dir, "workspaces.yaml")
	prev := defaultRegistryPathFn
	defaultRegistryPathFn = func() (string, error) { return regPath, nil }
	t.Cleanup(func() { defaultRegistryPathFn = prev })

	noLang := SupervisorDaemon{
		TaskName: `\mcp-local-hub-lsp-deadbeef-python`,
		Server:   "mcp-language-server",
		Args:     []string{"daemon", "workspace-proxy", "--workspace", dir},
	}
	if !LSPRegistryRowBacksDescriptor(noLang) {
		t.Fatalf("descriptor missing --language must fail OPEN (backed=true)")
	}
}

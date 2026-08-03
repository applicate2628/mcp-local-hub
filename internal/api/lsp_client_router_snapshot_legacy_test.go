// internal/api/lsp_client_router_snapshot_legacy_test.go
//
// Regression coverage for the codex bot PR #588 P1 finding that the
// "complete" LSP pre-state snapshot did not model every client shape the
// forward reconcile MUTATES.
//
// The forward pass rewrites the canonical `mcp-language-server-<language>`
// entry AND DELETES every legacy registry-backed per-workspace entry that
// still points at a registry-owned proxy port
// (the forward Plan/Apply pipeline). The snapshot captured only the
// canonical name, so those deletions had no recovery row and the rollback
// could not put them back.
package api

import (
	"testing"

	"mcp-local-hub/internal/clients"
)

// seedLegacyLSPWorkspace registers one legacy per-workspace LSP row whose
// client-config entry name is `entryName` and whose proxy port is 9200.
func seedLegacyLSPWorkspace(t *testing.T, clientName, entryName string) {
	t.Helper()
	reg := NewRegistry(testRegistryPathOverride)
	if err := reg.PutLSP(WorkspaceEntry{
		WorkspaceKey:  "abcd1234",
		WorkspacePath: "D:/dev/project",
		Language:      "go",
		Backend:       "gopls-mcp",
		Port:          9200,
		TaskName:      "mcp-local-hub-lsp-abcd1234-go",
		ClientEntries: map[string]string{clientName: entryName},
	}); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save registry: %v", err)
	}
}

// TestSnapshotLSPRouterClientEntries_CapturesLegacyPerWorkspaceEntries is the
// direct guard: the entry the forward pass will delete must appear in the
// snapshot, with its real pre-state.
func TestSnapshotLSPRouterClientEntries_CapturesLegacyPerWorkspaceEntries(t *testing.T) {
	seedLSPRouterManifest(t, []string{"go"})
	restoreRegistry := overrideLSPRouterRegistry(t)
	defer restoreRegistry()
	seedLegacyLSPWorkspace(t, "codex-cli", "mcp-language-server-go-abcd")

	codex := newLSPRouterFakeClient(t, "codex-cli", true)
	codex.entries["mcp-language-server-go-abcd"] = clients.MCPEntry{
		Name: "mcp-language-server-go-abcd",
		URL:  "http://localhost:9200/mcp",
	}

	snapshot, err := NewAPI().SnapshotLSPRouterClientEntries(LSPClientRouterOpts{
		Clients: map[string]clients.Client{"codex-cli": codex},
	})
	if err != nil {
		t.Fatalf("SnapshotLSPRouterClientEntries: %v", err)
	}

	var legacy *LSPRouterEntrySnapshot
	for i := range snapshot {
		if snapshot[i].EntryName == "mcp-language-server-go-abcd" {
			legacy = &snapshot[i]
		}
	}
	if legacy == nil {
		t.Fatalf("the snapshot omitted the LEGACY per-workspace entry the forward reconcile deletes; without a row for it, `--rollback` cannot put it back and the client is left with no LSP entry at all. rows=%+v", snapshot)
	}
	if !legacy.Present {
		t.Fatalf("legacy row must record Present=true (the entry exists right now); a row recorded absent makes the rollback try to REMOVE it instead of restoring it: %+v", *legacy)
	}
	if legacy.URL != "http://localhost:9200/mcp" {
		t.Fatalf("legacy row must carry the pre-state URL the rollback restores to; got %q", legacy.URL)
	}
	if legacy.Language != "go" {
		t.Fatalf("legacy row language = %q, want go", legacy.Language)
	}
}

// TestMCPFrontLegacyLSP_ForwardThenRollbackRestoresTheLegacyEntry is the
// end-to-end property, and it reproduces the exact mechanism the reviewers
// described: legacy entry present, canonical ABSENT.
//
// Forward adds the canonical entry and removes the legacy one. Before the fix
// the record knew only "canonical was absent", so rollback removed the
// canonical entry and retired the record — leaving the client with NEITHER
// entry and no evidence anything had been lost.
func TestMCPFrontLegacyLSP_ForwardThenRollbackRestoresTheLegacyEntry(t *testing.T) {
	seedLSPRouterManifest(t, []string{"go"})
	restoreRegistry := overrideLSPRouterRegistry(t)
	defer restoreRegistry()
	seedLegacyLSPWorkspace(t, "codex-cli", "mcp-language-server-go-abcd")

	codex := newLSPRouterFakeClient(t, "codex-cli", true)
	codex.entries["mcp-language-server-go-abcd"] = clients.MCPEntry{
		Name: "mcp-language-server-go-abcd",
		URL:  "http://localhost:9200/mcp",
	}
	clientMap := map[string]clients.Client{"codex-cli": codex}
	a := NewAPI()

	// Capture the pre-state exactly as the reconcile command does.
	snapshot, serr := a.SnapshotLSPRouterClientEntries(LSPClientRouterOpts{Clients: clientMap})
	if serr != nil {
		t.Fatalf("snapshot: %v", serr)
	}

	if _, ferr := a.EnsureLSPRouterClientEntries(LSPClientRouterOpts{
		GUIPort: 9137,
		Clients: clientMap,
	}); ferr != nil {
		t.Fatalf("forward EnsureLSPRouterClientEntries: %v", ferr)
	}
	// Precondition for the assertion below: the forward pass really did both
	// things. If either stops happening this test must fail loudly rather than
	// silently proving nothing.
	if _, ok := codex.entries["mcp-language-server-go"]; !ok {
		t.Fatalf("test precondition broken: forward pass did not add the canonical entry; entries=%+v", codex.entries)
	}
	if _, ok := codex.entries["mcp-language-server-go-abcd"]; ok {
		t.Fatalf("test precondition broken: forward pass did not remove the legacy entry, so this test is not exercising the deletion it is named for; entries=%+v", codex.entries)
	}

	if _, rerr := a.RestoreLSPRouterClientEntriesSnapshot(snapshot, LSPClientRouterOpts{
		GUIPort: 9137,
		Clients: clientMap,
	}); rerr != nil {
		t.Fatalf("rollback RestoreLSPRouterClientEntriesSnapshot: %v", rerr)
	}

	legacy, ok := codex.entries["mcp-language-server-go-abcd"]
	if !ok {
		t.Fatalf("rollback did NOT restore the legacy per-workspace entry the forward pass deleted; the client is left with no LSP entry at all, which is worse than before the cutover. entries=%+v", codex.entries)
	}
	if legacy.URL != "http://localhost:9200/mcp" {
		t.Fatalf("legacy entry restored to the wrong URL: got %q, want http://localhost:9200/mcp", legacy.URL)
	}
	if _, ok := codex.entries["mcp-language-server-go"]; ok {
		t.Fatalf("rollback left the canonical entry the forward pass created; it should have been removed (it was recorded absent). entries=%+v", codex.entries)
	}
}

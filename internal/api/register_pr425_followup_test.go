package api

import (
	"testing"

	"mcp-local-hub/internal/clients"
)

// This file pins the bot PR #425 FOLLOW-UP GAP 2 CALLER-side workspace scope
// (architect GATE PASS): the destructive gopls / LSP cleanup's post-removal
// survivor recheck moved OUT of the workspace-blind adapter and INTO
// matchingDirectGoplsMCPEntries / matchingDirectLanguageServerEntries, where the
// canonical workspace is known. A same-name lower-layer entry for a DIFFERENT
// workspace must NOT block removal of the real workspace-A entry; a same-workspace
// re-emergence (a removal the hub cannot fully clear) MUST block.
//
// These tests drive the caller functions directly with a fake that implements the
// two OPTIONAL post-removal readers, so the workspace-scoping decision is exercised
// in isolation from the mimo merge (the adapter-side reader correctness is pinned
// in internal/clients/mimocode_pr425_followup_test.go).

// followupReaderFake implements registerClient PLUS the single post-removal
// ALL-STDIO active-reader interface that FINDING 2 made both caller families
// consume. Only the methods the caller functions touch are real; the rest satisfy
// registerClient with trivial bodies.
type followupReaderFake struct {
	stdioSurvivors map[string][]clients.StdioEntry
}

func (f *followupReaderFake) Exists() bool                               { return true }
func (f *followupReaderFake) BackupKeep(int) (string, error)             { return "/b", nil }
func (f *followupReaderFake) AddEntry(clients.MCPEntry) error            { return nil }
func (f *followupReaderFake) RemoveEntry(string) error                   { return nil }
func (f *followupReaderFake) GetEntry(string) (*clients.MCPEntry, error) { return nil, nil }
func (f *followupReaderFake) AllStdioEntries() ([]clients.StdioEntry, error) {
	return nil, nil
}
func (f *followupReaderFake) FindStdioLanguageServerEntries() ([]clients.LanguageServerStdioEntry, error) {
	return nil, nil
}
func (f *followupReaderFake) ActiveStdioEntriesExcludingWriteTarget(name string) ([]clients.StdioEntry, error) {
	return f.stdioSurvivors[name], nil
}

func goplsArgs(workspace string) []string {
	return []string{"mcp", "--workspace", workspace}
}

func lsArgs(workspace string) []string {
	return []string{"--lsp", "go", "--workspace", workspace}
}

// goplsStdio / lsStdio build the ALL-STDIO survivor shapes the single reader now
// returns: a gopls-mcp entry and an mcp-language-server --lsp <lang> entry.
func goplsStdio(name, workspace string) clients.StdioEntry {
	return clients.StdioEntry{Name: name, Command: "gopls", Args: goplsArgs(workspace)}
}

func lsStdio(name, lang, workspace string) clients.StdioEntry {
	return clients.StdioEntry{Name: name, Command: "mcp-language-server", Args: []string{"--lsp", lang, "--workspace", workspace}}
}

// CLAIM 3 — gopls(workspace A) write-target + a lower same-name gopls(workspace B)
// OR an npx → the write-target gopls(A) IS removed (re-emergent different-workspace
// / non-gopls survivors do NOT block); a same-name gopls(workspace A) re-emergence
// DOES block.
func TestFollowup_GoplsCaller_WorkspaceScopedSurvivor(t *testing.T) {
	wsA := t.TempDir()
	wsB := t.TempDir()
	canonicalA, err := CanonicalWorkspacePathForCleanup(wsA)
	if err != nil {
		t.Fatal(err)
	}

	candidate := clients.StdioEntry{Name: "gopls", Command: "gopls", Args: goplsArgs(wsA)}

	cases := []struct {
		name       string
		survivors  []clients.StdioEntry
		wantRemove bool
	}{
		{
			name:       "no survivor -> removed",
			survivors:  nil,
			wantRemove: true,
		},
		{
			name:       "different-workspace gopls(B) survivor -> still removed",
			survivors:  []clients.StdioEntry{{Name: "gopls", Command: "gopls", Args: goplsArgs(wsB)}},
			wantRemove: true,
		},
		{
			name:       "npx survivor (not gopls) -> still removed",
			survivors:  []clients.StdioEntry{{Name: "gopls", Command: "npx", Args: []string{"-y", "some-lsp", "--workspace", wsA}}},
			wantRemove: true,
		},
		{
			name:       "same-workspace gopls(A) survivor -> BLOCKED",
			survivors:  []clients.StdioEntry{{Name: "gopls", Command: "gopls", Args: goplsArgs(wsA)}},
			wantRemove: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &followupReaderFake{stdioSurvivors: map[string][]clients.StdioEntry{"gopls": tc.survivors}}
			out, err := matchingDirectGoplsMCPEntries(fake, []clients.StdioEntry{candidate}, map[string]bool{"go": true, "gopls": true}, canonicalA)
			if err != nil {
				t.Fatalf("matchingDirectGoplsMCPEntries: %v", err)
			}
			got := len(out) == 1 && out[0].Name == "gopls"
			if got != tc.wantRemove {
				t.Fatalf("removable=%v, want %v (survivors=%v)", got, tc.wantRemove, tc.survivors)
			}
		})
	}
}

// CLAIM 4 — mcp-language-server(workspace A) write-target + a lower same-name LSP
// for workspace B → A IS removed; same-workspace re-emergence DOES block. After
// FINDING 2 the survivor source is the SINGLE all-stdio reader (stdioSurvivors),
// not the dropped LSP-only reader.
func TestFollowup_LSPCaller_WorkspaceScopedSurvivor(t *testing.T) {
	wsA := t.TempDir()
	wsB := t.TempDir()
	canonicalA, err := CanonicalWorkspacePathForCleanup(wsA)
	if err != nil {
		t.Fatal(err)
	}
	aliases := map[string]bool{"go": true}

	candidate := clients.LanguageServerStdioEntry{Name: "mls", Command: "mcp-language-server", Language: "go", Args: lsArgs(wsA)}

	cases := []struct {
		name       string
		survivors  []clients.StdioEntry
		wantRemove bool
	}{
		{
			name:       "no survivor -> removed",
			survivors:  nil,
			wantRemove: true,
		},
		{
			name:       "different-workspace mcp-language-server(B) survivor -> still removed",
			survivors:  []clients.StdioEntry{lsStdio("mls", "go", wsB)},
			wantRemove: true,
		},
		{
			name:       "different-language survivor (rust) -> still removed",
			survivors:  []clients.StdioEntry{lsStdio("mls", "rust", wsA)},
			wantRemove: true,
		},
		{
			name:       "same-workspace+alias survivor -> BLOCKED",
			survivors:  []clients.StdioEntry{lsStdio("mls", "go", wsA)},
			wantRemove: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &followupReaderFake{stdioSurvivors: map[string][]clients.StdioEntry{"mls": tc.survivors}}
			out, err := matchingDirectLanguageServerEntries(fake, []clients.LanguageServerStdioEntry{candidate}, aliases, canonicalA)
			if err != nil {
				t.Fatalf("matchingDirectLanguageServerEntries: %v", err)
			}
			got := len(out) == 1 && out[0].Name == "mls"
			if got != tc.wantRemove {
				t.Fatalf("removable=%v, want %v (survivors=%v)", got, tc.wantRemove, tc.survivors)
			}
		})
	}
}

// A non-mimo / test fake that does NOT implement the optional readers falls back to
// an empty survivor set → the caller never blocks (behavior-unchanged for every
// single-file adapter). Pins the Q4 fallback contract.
func TestFollowup_Caller_NoReaderFallback_NeverBlocks(t *testing.T) {
	wsA := t.TempDir()
	canonicalA, err := CanonicalWorkspacePathForCleanup(wsA)
	if err != nil {
		t.Fatal(err)
	}
	// fakeClient (the existing harness fake) implements registerClient but NOT the
	// optional readers.
	parent := &fakeClientsMap{
		entries:         map[string]map[string]string{"x": {}},
		stdioEntries:    map[string]map[string]clients.LanguageServerStdioEntry{"x": {}},
		allStdioEntries: map[string]map[string]clients.StdioEntry{"x": {}},
		backupKeepCalls: map[string]int{},
		exists:          map[string]bool{"x": true},
	}
	fc := &fakeClient{parent: parent, name: "x"}

	gopls := clients.StdioEntry{Name: "gopls", Command: "gopls", Args: goplsArgs(wsA)}
	out, err := matchingDirectGoplsMCPEntries(fc, []clients.StdioEntry{gopls}, map[string]bool{"go": true, "gopls": true}, canonicalA)
	if err != nil {
		t.Fatalf("matchingDirectGoplsMCPEntries: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("a client without the optional reader must fall back to an empty survivor set and never block; got %d matches", len(out))
	}

	ls := clients.LanguageServerStdioEntry{Name: "mls", Command: "mcp-language-server", Language: "go", Args: lsArgs(wsA)}
	lspOut, err := matchingDirectLanguageServerEntries(fc, []clients.LanguageServerStdioEntry{ls}, map[string]bool{"go": true}, canonicalA)
	if err != nil {
		t.Fatalf("matchingDirectLanguageServerEntries: %v", err)
	}
	if len(lspOut) != 1 {
		t.Fatalf("LSP path: a client without the optional reader must never block; got %d matches", len(lspOut))
	}
}

// FINDING 2 — the CROSS-KIND survivor gap. Before the fix each caller family read
// its OWN reader with its OWN recheck, blind to the OTHER family:
//   - the gopls path consumed all-stdio but its inline filter only matched gopls;
//     an mcp-language-server --lsp go survivor for the same workspace was invisible
//     → the write-target gopls(A) was wrongly removed.
//   - the mcp-language-server path consumed the LSP-only reader (which DROPS
//     gopls-mcp); a gopls mcp survivor for the same workspace was invisible → the
//     write-target mcp-language-server(A) was wrongly removed.
// Both families now consume the single all-stdio reader and run the shared
// directLSPSurvivorMatchesWorkspace predicate, so EITHER family's survivor for THIS
// workspace blocks removal. These pin BOTH directions plus the different-workspace
// negatives.

// Direction A: gopls candidate × a lower same-name mcp-language-server-go-for-W
// survivor → BLOCKED (the formerly-invisible cross-kind survivor).
func TestFollowup_GoplsCaller_CrossKind_MlsGoSurvivor_Blocks(t *testing.T) {
	wsA := t.TempDir()
	wsB := t.TempDir()
	canonicalA, err := CanonicalWorkspacePathForCleanup(wsA)
	if err != nil {
		t.Fatal(err)
	}
	aliases := map[string]bool{"go": true, "gopls": true}
	candidate := goplsStdio("dev", wsA)

	cases := []struct {
		name       string
		survivors  []clients.StdioEntry
		wantRemove bool
	}{
		{
			name:       "cross-kind mcp-language-server-go(A) survivor -> BLOCKED",
			survivors:  []clients.StdioEntry{lsStdio("dev", "go", wsA)},
			wantRemove: false,
		},
		{
			name:       "cross-kind mcp-language-server-go(B) survivor (diff workspace) -> still removed",
			survivors:  []clients.StdioEntry{lsStdio("dev", "go", wsB)},
			wantRemove: true,
		},
		{
			name:       "cross-kind mcp-language-server-rust(A) survivor (non-alias lang) -> still removed",
			survivors:  []clients.StdioEntry{lsStdio("dev", "rust", wsA)},
			wantRemove: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &followupReaderFake{stdioSurvivors: map[string][]clients.StdioEntry{"dev": tc.survivors}}
			out, err := matchingDirectGoplsMCPEntries(fake, []clients.StdioEntry{candidate}, aliases, canonicalA)
			if err != nil {
				t.Fatalf("matchingDirectGoplsMCPEntries: %v", err)
			}
			got := len(out) == 1 && out[0].Name == "dev"
			if got != tc.wantRemove {
				t.Fatalf("removable=%v, want %v (survivors=%v)", got, tc.wantRemove, tc.survivors)
			}
		})
	}
}

// Direction B: mcp-language-server-go candidate × a lower same-name gopls-mcp-for-W
// survivor → BLOCKED (the formerly-invisible cross-kind survivor; the LSP-only
// reader used to DROP gopls-mcp entirely).
func TestFollowup_LSPCaller_CrossKind_GoplsSurvivor_Blocks(t *testing.T) {
	wsA := t.TempDir()
	wsB := t.TempDir()
	canonicalA, err := CanonicalWorkspacePathForCleanup(wsA)
	if err != nil {
		t.Fatal(err)
	}
	aliases := map[string]bool{"go": true, "gopls": true}
	candidate := clients.LanguageServerStdioEntry{Name: "dev", Command: "mcp-language-server", Language: "go", Args: lsArgs(wsA)}

	cases := []struct {
		name       string
		survivors  []clients.StdioEntry
		wantRemove bool
	}{
		{
			name:       "cross-kind gopls-mcp(A) survivor -> BLOCKED",
			survivors:  []clients.StdioEntry{goplsStdio("dev", wsA)},
			wantRemove: false,
		},
		{
			name:       "cross-kind gopls-mcp(B) survivor (diff workspace) -> still removed",
			survivors:  []clients.StdioEntry{goplsStdio("dev", wsB)},
			wantRemove: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &followupReaderFake{stdioSurvivors: map[string][]clients.StdioEntry{"dev": tc.survivors}}
			out, err := matchingDirectLanguageServerEntries(fake, []clients.LanguageServerStdioEntry{candidate}, aliases, canonicalA)
			if err != nil {
				t.Fatalf("matchingDirectLanguageServerEntries: %v", err)
			}
			got := len(out) == 1 && out[0].Name == "dev"
			if got != tc.wantRemove {
				t.Fatalf("removable=%v, want %v (survivors=%v)", got, tc.wantRemove, tc.survivors)
			}
		})
	}
}

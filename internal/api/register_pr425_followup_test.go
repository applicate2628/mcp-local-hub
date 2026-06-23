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

// followupReaderFake implements registerClient PLUS the two optional post-removal
// active-reader interfaces. Only the methods the caller functions touch are real;
// the rest satisfy registerClient with trivial bodies.
type followupReaderFake struct {
	stdioSurvivors map[string][]clients.StdioEntry
	lspSurvivors   map[string][]clients.LanguageServerStdioEntry
}

func (f *followupReaderFake) Exists() bool                                 { return true }
func (f *followupReaderFake) BackupKeep(int) (string, error)               { return "/b", nil }
func (f *followupReaderFake) AddEntry(clients.MCPEntry) error              { return nil }
func (f *followupReaderFake) RemoveEntry(string) error                     { return nil }
func (f *followupReaderFake) GetEntry(string) (*clients.MCPEntry, error)   { return nil, nil }
func (f *followupReaderFake) AllStdioEntries() ([]clients.StdioEntry, error) {
	return nil, nil
}
func (f *followupReaderFake) FindStdioLanguageServerEntries() ([]clients.LanguageServerStdioEntry, error) {
	return nil, nil
}
func (f *followupReaderFake) ActiveStdioEntriesExcludingWriteTarget(name string) ([]clients.StdioEntry, error) {
	return f.stdioSurvivors[name], nil
}
func (f *followupReaderFake) ActiveLanguageServerEntriesExcludingWriteTarget(name string) ([]clients.LanguageServerStdioEntry, error) {
	return f.lspSurvivors[name], nil
}

func goplsArgs(workspace string) []string {
	return []string{"mcp", "--workspace", workspace}
}

func lsArgs(workspace string) []string {
	return []string{"--lsp", "go", "--workspace", workspace}
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
			out, err := matchingDirectGoplsMCPEntries(fake, []clients.StdioEntry{candidate}, canonicalA)
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
// for workspace B → A IS removed; same-workspace re-emergence DOES block.
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
		survivors  []clients.LanguageServerStdioEntry
		wantRemove bool
	}{
		{
			name:       "no survivor -> removed",
			survivors:  nil,
			wantRemove: true,
		},
		{
			name:       "different-workspace mcp-language-server(B) survivor -> still removed",
			survivors:  []clients.LanguageServerStdioEntry{{Name: "mls", Command: "mcp-language-server", Language: "go", Args: lsArgs(wsB)}},
			wantRemove: true,
		},
		{
			name:       "different-language survivor (rust) -> still removed",
			survivors:  []clients.LanguageServerStdioEntry{{Name: "mls", Command: "mcp-language-server", Language: "rust", Args: []string{"--lsp", "rust", "--workspace", wsA}}},
			wantRemove: true,
		},
		{
			name:       "same-workspace+alias survivor -> BLOCKED",
			survivors:  []clients.LanguageServerStdioEntry{{Name: "mls", Command: "mcp-language-server", Language: "go", Args: lsArgs(wsA)}},
			wantRemove: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &followupReaderFake{lspSurvivors: map[string][]clients.LanguageServerStdioEntry{"mls": tc.survivors}}
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
	out, err := matchingDirectGoplsMCPEntries(fc, []clients.StdioEntry{gopls}, canonicalA)
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

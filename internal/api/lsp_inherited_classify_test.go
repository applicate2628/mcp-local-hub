package api

import "testing"

// TestClassifyLSPEntries_InheritedHTTP is the precise regression guard for bot
// finding #2 (PR #422): classifyLSPEntries Rule 1 (the http LSP row → "via-hub"
// promotion) MUST NOT force an INHERITED hub LSP cell back into the demigratable
// "via-hub" bucket. An inherited (import / below-write-target source the hub
// never wrote — ClientEntry.Inherited, only the mimocode scan path sets it) http
// LSP cell whose row has no OWNED hub cell must stay "via-hub-inherited" (the
// classify() result) with Managed == false. An OWNED http LSP cell always wins.
//
// classifyLSPEntries runs AFTER the main classify() loop in ScanFrom and
// overwrites e.Status for http LSP rows, so this is the only place the inherited
// status could be lost. The test calls classifyLSPEntries directly with a
// pre-seeded entries map (reg=nil is supported — the registry is a soft sanity
// gate, see the `_ = reg` line in classifyLSPEntries) so the assertion isolates
// the fix surface without the full mimocode scan plumbing.
func TestClassifyLSPEntries_InheritedHTTP(t *testing.T) {
	t.Run("inherited-only http LSP row -> via-hub-inherited (Managed false)", func(t *testing.T) {
		entries := map[string]*ScanEntry{
			"mcp-language-server-go": {
				Name:   "mcp-language-server-go",
				Status: "via-hub-inherited", // what classify() produced
				ClientPresence: map[string]ClientEntry{
					"mimocode": {Transport: "http", Endpoint: "http://localhost:9300/mcp", Inherited: true},
				},
			},
		}
		classifyLSPEntries(entries, nil)
		got := entries["mcp-language-server-go"]
		if got.Status != "via-hub-inherited" {
			t.Errorf("inherited http LSP row Status: got %q, want via-hub-inherited", got.Status)
		}
		if got.Managed {
			t.Errorf("via-hub-inherited LSP row must keep Managed == false, got true")
		}
	})

	t.Run("owned http LSP row -> via-hub (Managed true) [regression guard]", func(t *testing.T) {
		entries := map[string]*ScanEntry{
			"mcp-language-server-go": {
				Name:   "mcp-language-server-go",
				Status: "via-hub",
				ClientPresence: map[string]ClientEntry{
					"codex-cli": {Transport: "http", Endpoint: "http://localhost:9300/mcp"},
				},
			},
		}
		classifyLSPEntries(entries, nil)
		got := entries["mcp-language-server-go"]
		if got.Status != "via-hub" {
			t.Errorf("owned http LSP row Status: got %q, want via-hub", got.Status)
		}
		if !got.Managed {
			t.Errorf("owned via-hub LSP row must have Managed == true, got false")
		}
	})

	t.Run("owned cell wins over inherited cell on the same row -> via-hub", func(t *testing.T) {
		entries := map[string]*ScanEntry{
			"mcp-language-server-go": {
				Name: "mcp-language-server-go",
				ClientPresence: map[string]ClientEntry{
					"mimocode":  {Transport: "http", Endpoint: "http://localhost:9300/mcp", Inherited: true},
					"codex-cli": {Transport: "http", Endpoint: "http://localhost:9300/mcp"},
				},
			},
		}
		classifyLSPEntries(entries, nil)
		got := entries["mcp-language-server-go"]
		if got.Status != "via-hub" {
			t.Errorf("row with an OWNED http LSP cell must be via-hub, got %q", got.Status)
		}
		if !got.Managed {
			t.Errorf("an owned LSP row must have Managed == true, got false")
		}
	})
}

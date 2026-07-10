package clients

import "testing"

// TestEntryPresentInBytesJSONAdapters proves each JSON-family adopt adapter keys
// "present in bytes" on its OWN section (the same key its GetEntry reads), parsing
// the passed bytes directly with no disk access.
func TestEntryPresentInBytesJSONAdapters(t *testing.T) {
	cases := []struct {
		name    string
		client  Client
		section string // the top-level object key this adapter's entries live under
	}{
		{"cursor-base", &jsonMCPClient{clientName: "cursor", urlField: "url"}, "mcpServers"},
		{"claude-code", &claudeCode{}, claudeCodeMCPServersKey}, // "mcpServers"
		{"vscode", &vscodeClient{}, vscodeServersKey},           // "servers"
		{"opencode", &openCodeClient{}, openCodeMCPKey},         // "mcp"
		{"mimocode", &mimoCodeClient{}, mimoCodeMCPKey},         // "mcp" (write-target)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checker, ok := tc.client.(EntryBytesChecker)
			if !ok {
				t.Fatalf("%s does not implement EntryBytesChecker", tc.name)
			}
			present := []byte(`{"` + tc.section + `":{"srv":{"url":"http://x"}}}`)
			if got, err := checker.EntryPresentInBytes(present, "srv"); err != nil || !got {
				t.Errorf("entry present in its own section: got (%v,%v), want (true,nil)", got, err)
			}
			if got, err := checker.EntryPresentInBytes(present, "absent"); err != nil || got {
				t.Errorf("absent name in populated section: got (%v,%v), want (false,nil)", got, err)
			}
			// An entry under a DIFFERENT top-level key reads absent — proves the
			// adapter keys on its own section, not a generic whole-file scan (e.g.
			// vscode must read "servers", NOT "mcpServers").
			wrongSection := []byte(`{"someUnrelatedKey":{"srv":{}}}`)
			if got, err := checker.EntryPresentInBytes(wrongSection, "srv"); err != nil || got {
				t.Errorf("entry under a foreign section: got (%v,%v), want (false,nil)", got, err)
			}
			// Malformed config => error, which the capture caller treats as fail-closed.
			if _, err := checker.EntryPresentInBytes([]byte("{ this is not json"), "srv"); err == nil {
				t.Errorf("malformed config bytes: want an error")
			}
		})
	}
}

// TestEntryPresentInBytesCodexTOML covers the TOML adapter (codex-cli) whose section
// is [mcp_servers.*].
func TestEntryPresentInBytesCodexTOML(t *testing.T) {
	checker, ok := Client(&codexCLI{}).(EntryBytesChecker)
	if !ok {
		t.Fatal("codexCLI does not implement EntryBytesChecker")
	}
	present := []byte("[mcp_servers.srv]\ncommand = \"go\"\nargs = [\"version\"]\n")
	if got, err := checker.EntryPresentInBytes(present, "srv"); err != nil || !got {
		t.Errorf("present toml entry: got (%v,%v), want (true,nil)", got, err)
	}
	if got, err := checker.EntryPresentInBytes(present, "absent"); err != nil || got {
		t.Errorf("absent toml name: got (%v,%v), want (false,nil)", got, err)
	}
	if got, err := checker.EntryPresentInBytes([]byte("[mcp_servers]\n"), "srv"); err != nil || got {
		t.Errorf("empty mcp_servers table: got (%v,%v), want (false,nil)", got, err)
	}
	if _, err := checker.EntryPresentInBytes([]byte("this is not toml {{{"), "srv"); err == nil {
		t.Errorf("malformed toml bytes: want an error")
	}
}

// TestEntryPresentInBytesLockingClientForwards proves the wrapper every AllClients()
// adapter is wrapped in forwards to the concrete adapter (production always wraps).
func TestEntryPresentInBytesLockingClientForwards(t *testing.T) {
	wrapped := newLockingClient(&jsonMCPClient{clientName: "cursor", urlField: "url"})
	checker, ok := wrapped.(EntryBytesChecker)
	if !ok {
		t.Fatal("lockingClient must implement EntryBytesChecker")
	}
	present := []byte(`{"mcpServers":{"srv":{"url":"http://x"}}}`)
	if got, err := checker.EntryPresentInBytes(present, "srv"); err != nil || !got {
		t.Errorf("wrapped present: got (%v,%v), want (true,nil)", got, err)
	}
	if got, err := checker.EntryPresentInBytes(present, "absent"); err != nil || got {
		t.Errorf("wrapped absent: got (%v,%v), want (false,nil)", got, err)
	}
}

package clients

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func newPhysicalCleanupMutator(t *testing.T, client Client, original []byte, identity DirectCleanupIdentity) (*directCleanupMutator, *DirectCleanupTarget, string) {
	t.Helper()
	path := client.ConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	wrapped := newLockingClient(client)
	capability, ok := AsDirectCleanupMutator(wrapped)
	if !ok {
		t.Fatalf("%T was not admitted to direct cleanup", client)
	}
	mutator := capability.(*directCleanupMutator)
	mutator.writer = fallbackWriteConfigFile
	target, err := mutator.CaptureDirectCleanupTarget(identity)
	if err != nil {
		t.Fatalf("CaptureDirectCleanupTarget: %v", err)
	}
	return mutator, target, path
}

func runPhysicalCleanup(t *testing.T, mutator *directCleanupMutator, target *DirectCleanupTarget) (DirectCleanupReceipt, string, error) {
	t.Helper()
	var receipt DirectCleanupReceipt
	backup, err := mutator.CleanupDirectEntryAtomically(target, 3, func(got DirectCleanupReceipt) error {
		receipt = got
		return nil
	}, func(DirectCleanupCheckpoint) error { return nil })
	return receipt, backup, err
}

func TestDirectCleanupJSONC_LexicalDriftConflictsBeforeBackup(t *testing.T) {
	identity := DirectCleanupIdentity{Name: "legacy-go", Command: "mcp-language-server", Args: []string{"--lsp", "go"}}
	base := `{
  "mcpServers": {
    "legacy-go": {"command":"mcp-language-server","args":["--lsp","go"],"timeout":10},
    "sibling": {"command":"operator-tool"}
  }
}
`
	cases := map[string]string{
		"comment-only": `{
  "mcpServers": {
    "legacy-go": {/* owned */"command":"mcp-language-server","args":["--lsp","go"],"timeout":10},
    "sibling": {"command":"operator-tool"}
  }
}
`,
		"whitespace-only": `{
  "mcpServers": {
    "legacy-go": { "command": "mcp-language-server", "args": ["--lsp", "go"], "timeout": 10 },
    "sibling": {"command":"operator-tool"}
  }
}
`,
		"number-spelling": `{
  "mcpServers": {
    "legacy-go": {"command":"mcp-language-server","args":["--lsp","go"],"timeout":1e1},
    "sibling": {"command":"operator-tool"}
  }
}
`,
		"key-order": `{
  "mcpServers": {
    "legacy-go": {"timeout":10,"args":["--lsp","go"],"command":"mcp-language-server"},
    "sibling": {"command":"operator-tool"}
  }
}
`,
		"trailing-comma": `{
  "mcpServers": {
    "legacy-go": {"command":"mcp-language-server","args":["--lsp","go"],"timeout":10,},
    "sibling": {"command":"operator-tool"}
  }
}
`,
		"nested-comment": `{
  "mcpServers": {
    "legacy-go": {"command":"mcp-language-server","args":["--lsp",/* nested */"go"],"timeout":10},
    "sibling": {"command":"operator-tool"}
  }
}
`,
	}
	for name, changed := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "claude.json")
			mutator, target, _ := newPhysicalCleanupMutator(t, &claudeCode{path: path}, []byte(base), identity)
			if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
				t.Fatal(err)
			}
			receipt, backup, err := runPhysicalCleanup(t, mutator, target)
			if !errors.Is(err, ErrCASConflict) {
				t.Fatalf("cleanup error = %v, want ErrCASConflict", err)
			}
			if receipt != nil || backup != "" {
				t.Fatalf("lexical conflict armed receipt=%v backup=%q", receipt != nil, backup)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(after, []byte(changed)) {
				t.Fatal("lexical conflict changed config bytes")
			}
		})
	}
}

func TestDirectCleanupJSONC_SiblingEditSurvivesRemoveAndExactRestore(t *testing.T) {
	original := []byte(`{
  "mcpServers": {
    // target-owned lead
    "legacy-go": {"command":"mcp-language-server","args":["--lsp","go"]},
    "sibling": {"command":"operator-tool","marker":"before"}
  }
}
`)
	identity := DirectCleanupIdentity{Name: "legacy-go", Command: "mcp-language-server", Args: []string{"--lsp", "go"}}
	path := filepath.Join(t.TempDir(), "claude.json")
	mutator, target, _ := newPhysicalCleanupMutator(t, &claudeCode{path: path}, original, identity)
	changed := bytes.Replace(original, []byte(`"marker":"before"`), []byte(`"marker" : "after" /* sibling-owned */`), 1)
	siblingBytes := []byte(`"sibling": {"command":"operator-tool","marker" : "after" /* sibling-owned */}`)
	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	receipt, _, err := runPhysicalCleanup(t, mutator, target)
	if err != nil {
		t.Fatalf("cleanup after sibling edit: %v", err)
	}
	removed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(removed, target.physicalToken.memberBytes) || !bytes.Contains(removed, siblingBytes) {
		t.Fatalf("remove did not preserve only the sibling bytes:\n%s", removed)
	}
	if err := receipt.Restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(restored, target.physicalToken.memberBytes) || !bytes.Contains(restored, siblingBytes) {
		t.Fatalf("restore did not preserve exact target and current sibling bytes:\n%s", restored)
	}
}

func TestDirectCleanupJSONC_AddedSiblingMayChangeOnlyStructuralDelimiter(t *testing.T) {
	original := []byte(`{
  "mcpServers": {
    "first": {"command":"operator-first"},
    "legacy-go": {"command":"mcp-language-server","args":["--lsp","go"]}
  }
}
`)
	identity := DirectCleanupIdentity{Name: "legacy-go", Command: "mcp-language-server", Args: []string{"--lsp", "go"}}
	path := filepath.Join(t.TempDir(), "claude.json")
	mutator, target, _ := newPhysicalCleanupMutator(t, &claudeCode{path: path}, original, identity)
	changed := bytes.Replace(original,
		[]byte("\"legacy-go\": {\"command\":\"mcp-language-server\",\"args\":[\"--lsp\",\"go\"]}\n"),
		[]byte("\"legacy-go\": {\"command\":\"mcp-language-server\",\"args\":[\"--lsp\",\"go\"]},\n    \"new-sibling\": {\"command\":\"operator-new\"}\n"), 1)
	if bytes.Equal(changed, original) {
		t.Fatal("test fixture did not add sibling")
	}
	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	receipt, _, err := runPhysicalCleanup(t, mutator, target)
	if err != nil {
		t.Fatalf("cleanup after sibling insertion: %v", err)
	}
	removed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(removed, []byte(`"legacy-go"`)) || !bytes.Contains(removed, []byte(`"new-sibling"`)) {
		t.Fatalf("cleanup did not preserve added sibling:\n%s", removed)
	}
	if err := receipt.Restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(restored, target.physicalToken.targetBytes) || !bytes.Contains(restored, []byte(`"new-sibling"`)) {
		t.Fatalf("restore did not preserve target and added sibling:\n%s", restored)
	}
}

func TestDirectCleanupTOML_LexicalDriftConflictsBeforeBackup(t *testing.T) {
	identity := DirectCleanupIdentity{Name: "legacy-go", Command: "mcp-language-server", Args: []string{"--lsp", "go"}}
	base := `[mcp_servers.legacy-go]
command = "mcp-language-server"
args = ["--lsp", "go"]
startup_timeout_sec = 10

[mcp_servers.sibling]
command = "operator-tool"
`
	cases := map[string]string{
		"comment": `[mcp_servers.legacy-go]
command = "mcp-language-server" # owned
args = ["--lsp", "go"]
startup_timeout_sec = 10

[mcp_servers.sibling]
command = "operator-tool"
`,
		"quote-style": `[mcp_servers.legacy-go]
command = 'mcp-language-server'
args = ['--lsp', 'go']
startup_timeout_sec = 10

[mcp_servers.sibling]
command = "operator-tool"
`,
		"numeric-representation": `[mcp_servers.legacy-go]
command = "mcp-language-server"
args = ["--lsp", "go"]
startup_timeout_sec = 1_0

[mcp_servers.sibling]
command = "operator-tool"
`,
		"key-order": `[mcp_servers.legacy-go]
startup_timeout_sec = 10
args = ["--lsp", "go"]
command = "mcp-language-server"

[mcp_servers.sibling]
command = "operator-tool"
`,
	}
	for name, changed := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			mutator, target, _ := newPhysicalCleanupMutator(t, &codexCLI{path: path}, []byte(base), identity)
			if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
				t.Fatal(err)
			}
			receipt, backup, err := runPhysicalCleanup(t, mutator, target)
			if !errors.Is(err, ErrCASConflict) {
				t.Fatalf("cleanup error = %v, want ErrCASConflict", err)
			}
			if receipt != nil || backup != "" {
				t.Fatalf("lexical conflict armed receipt=%v backup=%q", receipt != nil, backup)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(after, []byte(changed)) {
				t.Fatal("lexical conflict changed TOML bytes")
			}
		})
	}
}

func TestDirectCleanupTOML_SiblingEditSurvivesRemoveAndExactRestore(t *testing.T) {
	original := []byte(`# file-owned header
[mcp_servers.legacy-go]
command = "mcp-language-server"
args = ["--lsp", "go"]

# sibling-owned lead
[mcp_servers.sibling]
command = "operator-tool"
marker = "before"
`)
	identity := DirectCleanupIdentity{Name: "legacy-go", Command: "mcp-language-server", Args: []string{"--lsp", "go"}}
	path := filepath.Join(t.TempDir(), "config.toml")
	mutator, target, _ := newPhysicalCleanupMutator(t, &codexCLI{path: path}, original, identity)
	changed := bytes.Replace(original, []byte(`marker = "before"`), []byte(`marker = 'after' # sibling-owned`), 1)
	siblingBytes := []byte("# sibling-owned lead\n[mcp_servers.sibling]\ncommand = \"operator-tool\"\nmarker = 'after' # sibling-owned\n")
	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	receipt, _, err := runPhysicalCleanup(t, mutator, target)
	if err != nil {
		t.Fatalf("cleanup after sibling edit: %v", err)
	}
	removed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(removed, target.physicalToken.targetBytes) || !bytes.Contains(removed, siblingBytes) {
		t.Fatalf("TOML remove did not preserve only the sibling bytes:\n%s", removed)
	}
	if err := receipt.Restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(restored, target.physicalToken.targetBytes) || !bytes.Contains(restored, siblingBytes) {
		t.Fatalf("TOML restore did not preserve exact target and current sibling bytes:\n%s", restored)
	}
}

func TestDirectCleanupTOML_FirstTargetRestoresBeforeRemainingSiblingWhenNextAnchorWasRemoved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	originalTarget := "[mcp_servers.target]\ncommand = \"mcphub\"\nargs = [\"relay\"]\n\n"
	original := originalTarget + "[mcp_servers.next]\ncommand = \"next\"\n\n[mcp_servers.remaining]\ncommand = \"remaining\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	client := &codexCLI{path: path}
	mutator := &directCleanupMutator{client: &lockingClient{Client: client}}
	target, err := mutator.CaptureDirectCleanupTarget(DirectCleanupIdentity{
		Name: "target", Command: "mcphub", Args: []string{"relay"},
	})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	var receipt DirectCleanupReceipt
	if _, err := mutator.CleanupDirectEntryAtomically(target, 5, func(r DirectCleanupReceipt) error {
		receipt = r
		return nil
	}, func(DirectCleanupCheckpoint) error { return nil }); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	withoutNext := "[mcp_servers.remaining]\ncommand = \"remaining\"\n"
	if err := os.WriteFile(path, []byte(withoutNext), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := receipt.Restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The blank separator belonged to the removed next table's leading prefix,
	// not to target's byte-exact block.
	want := strings.TrimSuffix(originalTarget, "\n") + withoutNext
	if string(got) != want {
		t.Fatalf("first target was not restored before the remaining sibling\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestDirectCleanupReceipt_MalformedCurrentBytesFailClosed(t *testing.T) {
	original := []byte(`{"mcpServers":{"legacy-go":{"command":"mcp-language-server","args":["--lsp","go"]}}}`)
	identity := DirectCleanupIdentity{Name: "legacy-go", Command: "mcp-language-server", Args: []string{"--lsp", "go"}}
	path := filepath.Join(t.TempDir(), "claude.json")
	mutator, target, _ := newPhysicalCleanupMutator(t, &claudeCode{path: path}, original, identity)
	receipt, _, err := runPhysicalCleanup(t, mutator, target)
	if err != nil {
		t.Fatal(err)
	}
	malformed := []byte(`{"mcpServers":`)
	if err := os.WriteFile(path, malformed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := receipt.Restore(); !errors.Is(err, ErrCleanupRestoreConflict) {
		t.Fatalf("restore error = %v, want ErrCleanupRestoreConflict", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(after)) != strings.TrimSpace(string(malformed)) {
		t.Fatal("malformed foreign bytes were changed")
	}
}

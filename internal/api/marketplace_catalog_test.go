package api

import (
	"strings"
	"testing"
)

func TestParseCatalog_HappyPath(t *testing.T) {
	raw := `{
  "schema_version": "1",
  "entries": [
    {"id": "filesystem", "name": "Filesystem MCP server",
     "transport": "stdio", "command": "npx",
     "args": ["-y", "@modelcontextprotocol/server-filesystem"]}
  ]
}`
	cat, err := ParseMarketplaceCatalog([]byte(raw))
	if err != nil {
		t.Fatalf("ParseMarketplaceCatalog: %v", err)
	}
	if len(cat.Entries) != 1 || cat.Entries[0].ID != "filesystem" {
		t.Fatalf("round-trip failed: %+v", cat)
	}
}

func TestParseCatalog_RejectsBadSchema(t *testing.T) {
	for _, raw := range []string{
		`{"schema_version": "9999", "entries": []}`,
		`{"schema_version": "1", "entries": [{"name": "no-id", "transport": "stdio", "command": "npx"}]}`,
		`{"schema_version": "1", "entries": [
			{"id": "dup", "name": "a", "transport": "stdio", "command": "npx"},
			{"id": "dup", "name": "b", "transport": "stdio", "command": "npx"}]}`,
		`{"schema_version": "1", "entries": [{"id": "x", "name": "X", "transport": "websocket", "command": "npx"}]}`,
		`{"schema_version": "1", "entries": [{"id": "nocmd", "name": "no command", "transport": "stdio"}]}`,
		`{"schema_version": "1", "entries": [{"id": "x", "name": "X", "transport": "http", "url": "http://insecure.example/mcp"}]}`,
	} {
		if _, err := ParseMarketplaceCatalog([]byte(raw)); err == nil {
			t.Errorf("expected rejection for %s", raw)
		}
	}
}

// TestParseCatalog_RejectsInvalidIDViaCheckManifestName pins codex
// r1 P2 closure: entry.id must pass the same gate `mcphub manifest
// create <name>` uses, so the draft will not fail later at create.
func TestParseCatalog_RejectsInvalidIDViaCheckManifestName(t *testing.T) {
	for _, badID := range []string{
		"UPPERCASE",       // CheckManifestName rejects non-lowercase
		"has space",       // CheckManifestName rejects whitespace
		"-leading-dash",   // regex rejects leading dash
		".leading-dot",    // regex rejects leading dot
		"mcphub-hub",      // reserved aggregate entry name (r15)
		"con",             // Windows device name
		"nul",             // Windows device name
	} {
		raw := `{"schema_version": "1", "entries": [{"id": "` + badID +
			`", "name": "X", "transport": "stdio", "command": "npx"}]}`
		if _, err := ParseMarketplaceCatalog([]byte(raw)); err == nil ||
			!strings.Contains(err.Error(), badID) {
			t.Errorf("expected rejection naming %q; got %v", badID, err)
		}
	}
}

func TestParseCatalog_HttpEntryAllowedNoCommand(t *testing.T) {
	raw := `{"schema_version": "1", "entries": [
		{"id": "ctx7", "name": "Context7", "transport": "http", "url": "https://mcp.context7.com/mcp"}
	]}`
	cat, err := ParseMarketplaceCatalog([]byte(raw))
	if err != nil {
		t.Fatalf("http entry should parse without command: %v", err)
	}
	if cat.Entries[0].URL != "https://mcp.context7.com/mcp" {
		t.Errorf("url round-trip failed")
	}
}

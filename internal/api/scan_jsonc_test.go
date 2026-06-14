package api

import (
	"os"
	"path/filepath"
	"testing"
)

// The Servers scan turned into a 500 because Zed's (and VS Code's)
// settings.json is JSONC — comments + trailing commas — which strict
// encoding/json rejects with "invalid character '/' looking for beginning of
// value". scanZed/scanVSCode now route bytes through the JSONC preprocessor.

func TestScanZed_TolerateJSONC(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "settings.json")
	jsonc := []byte(`{
  // a leading comment, which Zed allows and strict JSON rejects
  "context_servers": {
    "serena": { "command": "serena-mcp", }, /* trailing comma + block comment */
  },
}`)
	if err := os.WriteFile(cfg, jsonc, 0o600); err != nil {
		t.Fatal(err)
	}
	entries := map[string]*ScanEntry{}
	if err := scanZed(entries, cfg); err != nil {
		t.Fatalf("scanZed must tolerate JSONC (comments + trailing commas), got: %v", err)
	}
	if _, ok := entries["serena"]; !ok {
		t.Fatalf("expected the serena entry parsed from the JSONC config; got %v", entries)
	}
}

func TestScanVSCode_TolerateJSONC(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "settings.json")
	jsonc := []byte("{\n  // vscode settings.json is JSONC too\n  \"servers\": {\n    \"mem\": { \"url\": \"http://x\" },\n  },\n}")
	if err := os.WriteFile(cfg, jsonc, 0o600); err != nil {
		t.Fatal(err)
	}
	entries := map[string]*ScanEntry{}
	if err := scanVSCode(entries, cfg); err != nil {
		t.Fatalf("scanVSCode must tolerate JSONC, got: %v", err)
	}
	if _, ok := entries["mem"]; !ok {
		t.Fatalf("expected the mem entry parsed from the JSONC config; got %v", entries)
	}
}

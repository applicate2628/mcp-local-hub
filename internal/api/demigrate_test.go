package api

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
)

// setupTmpHomeAndClaude redirects UserHomeDir to tmp and seeds
// .claude.json with the given body. Returns the claude config path.
func setupTmpHomeAndClaude(t *testing.T, body string) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	claude := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(claude, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return claude
}

// TestDemigrate_RestoresStdioPerEntry round-trips a claude-code stdio
// entry through migrate → demigrate using a real manifest (so the
// client-bindings iteration is exercised, not the naive
// "iterate every installed adapter" pattern).
func TestDemigrate_RestoresStdioPerEntry(t *testing.T) {
	claudePath := setupTmpHomeAndClaude(t,
		`{"mcpServers":{"memory":{"type":"stdio","command":"npx","args":["-y","mem"]}}}`)

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	if err := os.MkdirAll(memDir, 0700); err != nil {
		t.Fatal(err)
	}
	manifestBody := `name: memory
kind: global
transport: stdio-bridge
command: npx
base_args:
  - "-y"
  - "mem"

daemons:
  - name: default
    port: 9200

client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`
	if err := os.WriteFile(filepath.Join(memDir, "manifest.yaml"), []byte(manifestBody), 0600); err != nil {
		t.Fatal(err)
	}

	cc, _ := clients.NewClaudeCode()
	if _, err := cc.Backup(); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if err := os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}

	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:  []string{"memory"},
		ScanOpts: ScanOpts{ManifestDir: manifestDir},
		Writer:   io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Failed) > 0 {
		t.Fatalf("unexpected failures: %+v", report.Failed)
	}
	if len(report.Restored) != 1 {
		t.Fatalf("expected 1 restored row, got %d", len(report.Restored))
	}

	live, _ := os.ReadFile(claudePath)
	var m map[string]any
	if err := json.Unmarshal(live, &m); err != nil {
		t.Fatal(err)
	}
	entry := m["mcpServers"].(map[string]any)["memory"].(map[string]any)
	if entry["type"] != "stdio" {
		t.Errorf("type=%v, want stdio", entry["type"])
	}
}

func TestDemigrate_OnlyIteratesManifestBindings(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)

	claudePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://x/mcp"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	geminiDir := filepath.Join(tmp, ".gemini")
	if err := os.MkdirAll(geminiDir, 0700); err != nil {
		t.Fatal(err)
	}
	geminiPath := filepath.Join(geminiDir, "settings.json")
	geminiBefore := `{"mcpServers":{"memory":{"url":"http://x/mcp","type":"http","timeout":10000}}}`
	if err := os.WriteFile(geminiPath, []byte(geminiBefore), 0600); err != nil {
		t.Fatal(err)
	}

	ccBackup := claudePath + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(ccBackup, []byte(
		`{"mcpServers":{"memory":{"type":"stdio","command":"npx","args":["-y","mem"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	_ = os.MkdirAll(memDir, 0700)
	if err := os.WriteFile(filepath.Join(memDir, "manifest.yaml"), []byte(
		`name: memory
kind: global
transport: stdio-bridge
command: npx
base_args: ["-y","mem"]
daemons:
  - name: default
    port: 9200
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`), 0600); err != nil {
		t.Fatal(err)
	}

	a := NewAPI()
	_, err := a.Demigrate(DemigrateOpts{
		Servers:  []string{"memory"},
		ScanOpts: ScanOpts{ManifestDir: manifestDir},
		Writer:   io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}

	geminiAfter, _ := os.ReadFile(geminiPath)
	if string(geminiAfter) != geminiBefore {
		t.Errorf("gemini config was touched — manifest bindings only mention claude-code.\nbefore: %s\nafter:  %s",
			geminiBefore, string(geminiAfter))
	}
}

func TestDemigrate_ClientsIncludeFilter(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	claudePath := filepath.Join(tmp, ".claude.json")
	_ = os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://x/mcp"}}}`), 0600)
	_ = os.WriteFile(claudePath+".bak-mcp-local-hub-20260101-000000", []byte(
		`{"mcpServers":{"memory":{"type":"stdio","command":"npx"}}}`), 0600)

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	_ = os.MkdirAll(memDir, 0700)
	_ = os.WriteFile(filepath.Join(memDir, "manifest.yaml"), []byte(
		`name: memory
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9200
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
  - client: gemini-cli
    daemon: default
    url_path: /mcp
`), 0600)

	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:        []string{"memory"},
		ClientsInclude: []string{"claude-code"},
		ScanOpts:       ScanOpts{ManifestDir: manifestDir},
		Writer:         io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Restored) != 1 || report.Restored[0].Client != "claude-code" {
		t.Errorf("expected single claude-code restore, got %+v", report.Restored)
	}
}

func TestDemigrate_MultiServerNewestFirstSucceeds(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	claudePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://x/mcp"},"fs":{"type":"http","url":"http://y/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	latest := claudePath + ".bak-mcp-local-hub-20260201-120000"
	if err := os.WriteFile(latest, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://x/mcp"},"fs":{"type":"stdio","command":"npx","args":["-y","fs"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}

	manifestDir := t.TempDir()
	fsDir := filepath.Join(manifestDir, "fs")
	_ = os.MkdirAll(fsDir, 0700)
	_ = os.WriteFile(filepath.Join(fsDir, "manifest.yaml"), []byte(
		`name: fs
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9201
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`), 0600)

	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:  []string{"fs"},
		ScanOpts: ScanOpts{ManifestDir: manifestDir},
		Writer:   io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Failed) > 0 {
		t.Fatalf("unexpected failures: %+v", report.Failed)
	}
	if len(report.Restored) != 1 {
		t.Fatalf("expected 1 restored row, got %d", len(report.Restored))
	}
}

func TestDemigrate_MultiServerRejectsOlderMigrationClearly(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	claudePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://x/mcp"},"fs":{"type":"http","url":"http://y/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	latest := claudePath + ".bak-mcp-local-hub-20260201-120000"
	if err := os.WriteFile(latest, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://x/mcp"},"fs":{"type":"stdio","command":"npx"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	_ = os.MkdirAll(memDir, 0700)
	_ = os.WriteFile(filepath.Join(memDir, "manifest.yaml"), []byte(
		`name: memory
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9200
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`), 0600)

	a := NewAPI()
	report, err := a.Demigrate(DemigrateOpts{
		Servers:  []string{"memory"},
		ScanOpts: ScanOpts{ManifestDir: manifestDir},
		Writer:   io.Discard,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Restored) != 0 {
		t.Fatalf("expected 0 restored (backup holds already-migrated entry), got %+v", report.Restored)
	}
	if len(report.Failed) != 1 {
		t.Fatalf("expected 1 failure, got %d: %+v", len(report.Failed), report.Failed)
	}
	if !strings.Contains(strings.ToLower(report.Failed[0].Err), "already") &&
		!strings.Contains(strings.ToLower(report.Failed[0].Err), "newest-first") {
		t.Errorf("failure message should mention already-migrated/newest-first: got %q", report.Failed[0].Err)
	}
}

func TestDemigrate_NoBackupReportsFailure(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	claudePath := filepath.Join(tmp, ".claude.json")
	_ = os.WriteFile(claudePath, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://x/mcp"}}}`), 0600)

	manifestDir := t.TempDir()
	memDir := filepath.Join(manifestDir, "memory")
	_ = os.MkdirAll(memDir, 0700)
	_ = os.WriteFile(filepath.Join(memDir, "manifest.yaml"), []byte(
		`name: memory
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9200
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`), 0600)

	a := NewAPI()
	buf := &bytes.Buffer{}
	report, err := a.Demigrate(DemigrateOpts{
		Servers:  []string{"memory"},
		ScanOpts: ScanOpts{ManifestDir: manifestDir},
		Writer:   buf,
	})
	if err != nil {
		t.Fatalf("Demigrate: %v", err)
	}
	if len(report.Failed) != 1 {
		t.Errorf("expected 1 failure, got %d: %+v", len(report.Failed), report.Failed)
	}
	if len(report.Restored) != 0 {
		t.Errorf("expected 0 restored, got %d", len(report.Restored))
	}
}

package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteConfigBundle_Composition(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))

	// Seed fake artifacts.
	dataDir := filepath.Join(tmp, "mcp-local-hub")
	stateDir := filepath.Join(tmp, "state", "mcp-local-hub")
	for _, d := range []string{dataDir, stateDir, filepath.Join(dataDir, "servers", "wolfram")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	must := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must(filepath.Join(dataDir, "servers", "wolfram", "manifest.yaml"), "name: wolfram\nport: 9001\n")
	must(filepath.Join(dataDir, "secrets.json"), `{"ciphertext":"AAA"}`)
	must(filepath.Join(dataDir, "gui-preferences.yaml"), "theme: dark\n")
	must(filepath.Join(stateDir, "workspaces.yaml"), "version: 1\nworkspaces: []\n")

	var buf bytes.Buffer
	if err := WriteConfigBundle(&buf); err != nil {
		t.Fatalf("WriteConfigBundle: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = string(b)
	}
	for _, expectedName := range []string{
		"servers/wolfram/manifest.yaml",
		"secrets.json",
		"gui-preferences.yaml",
		"workspaces.yaml",
		"bundle-meta.json",
	} {
		if _, ok := got[expectedName]; !ok {
			t.Errorf("bundle missing %q", expectedName)
		}
	}

	// Memo D11: hostname literal "redacted".
	var meta struct {
		Hostname   string `json:"hostname"`
		ExportTime string `json:"export_time"`
		MCPHubVer  string `json:"mcphub_version"`
		Platform   string `json:"platform"`
	}
	if err := json.Unmarshal([]byte(got["bundle-meta.json"]), &meta); err != nil {
		t.Fatalf("bundle-meta.json: %v", err)
	}
	if meta.Hostname != "redacted" {
		t.Errorf("hostname = %q, want literal %q (memo D11)", meta.Hostname, "redacted")
	}
	if meta.ExportTime == "" {
		t.Error("export_time missing")
	}
}

func TestWriteConfigBundle_ExcludesBackupFiles(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	dataDir := filepath.Join(tmp, "mcp-local-hub")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bak := filepath.Join(dataDir, "secrets.json.bak.20260101120000")
	if err := os.WriteFile(bak, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "secrets.json"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	_ = WriteConfigBundle(&buf)
	zr, _ := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	for _, f := range zr.File {
		if strings.Contains(f.Name, ".bak.") {
			t.Errorf("bundle includes backup file %q (must be excluded per memo D11)", f.Name)
		}
	}
}

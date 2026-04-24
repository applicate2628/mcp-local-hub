package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestListReturnsAllYAML(t *testing.T) {
	tmp := t.TempDir()
	_ = os.MkdirAll(filepath.Join(tmp, "foo"), 0755)
	_ = os.WriteFile(filepath.Join(tmp, "foo", "manifest.yaml"),
		[]byte("name: foo\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9200\n"), 0644)
	_ = os.MkdirAll(filepath.Join(tmp, "bar"), 0755)
	_ = os.WriteFile(filepath.Join(tmp, "bar", "manifest.yaml"),
		[]byte("name: bar\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9201\n"), 0644)
	_ = os.MkdirAll(filepath.Join(tmp, "draft"), 0755)
	// draft dir has no manifest.yaml — should be skipped.

	a := NewAPI()
	names, err := a.ManifestListIn(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Errorf("expected 2 manifests, got %v", names)
	}
}

func TestManifestValidateCatchesMissingFields(t *testing.T) {
	a := NewAPI()
	warnings := a.ManifestValidate("name: foo\n") // missing kind, transport, command, daemons
	if len(warnings) == 0 {
		t.Error("expected warnings for incomplete manifest, got none")
	}
}

func TestManifestCreateWritesYAML(t *testing.T) {
	tmp := t.TempDir()
	a := NewAPI()
	body := "name: newsrv\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9202\nclient_bindings: []\nweekly_refresh: false\n"
	if err := a.ManifestCreateIn(tmp, "newsrv", body); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tmp, "newsrv", "manifest.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "name: newsrv") {
		t.Error("manifest content not written")
	}
}

func TestManifestDeleteRemovesDir(t *testing.T) {
	tmp := t.TempDir()
	_ = os.MkdirAll(filepath.Join(tmp, "doomed"), 0755)
	_ = os.WriteFile(filepath.Join(tmp, "doomed", "manifest.yaml"),
		[]byte("name: doomed\nkind: global\ntransport: stdio-bridge\ncommand: x\ndaemons:\n  - name: default\n    port: 9203\n"), 0644)

	a := NewAPI()
	if err := a.ManifestDeleteIn(tmp, "doomed"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "doomed")); !os.IsNotExist(err) {
		t.Error("manifest dir not removed")
	}
}

// TestManifestGetIn_ReturnsContentHash verifies that ManifestGetInWithHash
// returns the same YAML as ManifestGetIn and a hash consistent with
// ManifestHashContent applied to those bytes.
func TestManifestGetIn_ReturnsContentHash(t *testing.T) {
	tmp := t.TempDir()
	yaml := "name: hashsrv\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9210\n"
	if err := os.MkdirAll(filepath.Join(tmp, "hashsrv"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "hashsrv", "manifest.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	a := NewAPI()
	gotYAML, gotHash, err := a.ManifestGetInWithHash(tmp, "hashsrv")
	if err != nil {
		t.Fatal(err)
	}
	if gotYAML != yaml {
		t.Errorf("YAML mismatch:\ngot  %q\nwant %q", gotYAML, yaml)
	}
	wantHash := ManifestHashContent([]byte(yaml))
	if gotHash != wantHash {
		t.Errorf("hash mismatch: got %q, want %q", gotHash, wantHash)
	}
}

// TestManifestGetIn_HashChangesOnExternalWrite is the load-bearing case
// for A2b D3 stale-file detection: if a second actor writes the manifest
// between the GUI's Load and Save calls, the hash must change so
// ManifestEdit can detect the conflict.
func TestManifestGetIn_HashChangesOnExternalWrite(t *testing.T) {
	tmp := t.TempDir()
	yaml1 := "name: stalesrv\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9211\n"
	if err := os.MkdirAll(filepath.Join(tmp, "stalesrv"), 0755); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(tmp, "stalesrv", "manifest.yaml")
	if err := os.WriteFile(manifestPath, []byte(yaml1), 0644); err != nil {
		t.Fatal(err)
	}

	a := NewAPI()
	_, h1, err := a.ManifestGetInWithHash(tmp, "stalesrv")
	if err != nil {
		t.Fatal(err)
	}

	// Simulate an external write between Load and Save.
	yaml2 := "name: stalesrv\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9212\n"
	if err := os.WriteFile(manifestPath, []byte(yaml2), 0644); err != nil {
		t.Fatal(err)
	}

	_, h2, err := a.ManifestGetInWithHash(tmp, "stalesrv")
	if err != nil {
		t.Fatal(err)
	}

	if h1 == h2 {
		t.Error("hash must differ after external write, but h1 == h2")
	}
}

// TestManifestCRUD_RejectsPathTraversalNames guards the regression
// where an attacker-controlled (or typo'd) name like "..",
// "../escaped", or an absolute path could escape the manifest root —
// for ManifestDeleteIn that would have meant os.RemoveAll on an
// arbitrary directory. The name validator rejects anything outside
// [a-z0-9][a-z0-9._-]*.
func TestManifestCRUD_RejectsPathTraversalNames(t *testing.T) {
	a := NewAPI()
	tmp := t.TempDir()

	cases := []string{
		"..",
		"../escaped",
		"../../etc",
		"/abs/path",
		`\abs\path`,
		"name/with/slash",
		"name\\with\\bs",
		"CapitalLetters",
		".leading-dot",
		"-leading-dash",
		"",
		" space",
		"name with spaces",
	}

	for _, bad := range cases {
		if err := a.ManifestDeleteIn(tmp, bad); err == nil {
			t.Errorf("ManifestDeleteIn(_, %q): expected rejection, got nil", bad)
		}
		if err := a.ManifestCreateIn(tmp, bad, "name: x\nkind: global\ntransport: stdio-bridge\ncommand: x\ndaemons: [{name: default, port: 9999}]\n"); err == nil {
			t.Errorf("ManifestCreateIn(_, %q): expected rejection, got nil", bad)
		}
		if err := a.ManifestEditIn(tmp, bad, "name: x\nkind: global\ntransport: stdio-bridge\ncommand: x\n"); err == nil {
			t.Errorf("ManifestEditIn(_, %q): expected rejection, got nil", bad)
		}
		if _, err := a.ManifestGet(bad); err == nil {
			t.Errorf("ManifestGet(%q): expected rejection, got nil", bad)
		}
	}
}

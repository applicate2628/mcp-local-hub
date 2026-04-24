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

func TestManifestGetIn_ReturnsContentHash(t *testing.T) {
	dir := t.TempDir()
	a := &API{}
	name := "memory"
	// Must satisfy api.ManifestValidate (which ManifestCreateIn gates on):
	// requires kind, transport, command, and at least one daemon.
	yaml := "name: memory\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9210\n"
	if err := a.ManifestCreateIn(dir, name, yaml); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, hash, err := a.ManifestGetInWithHash(dir, name)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != yaml {
		t.Errorf("yaml = %q, want %q", got, yaml)
	}
	want := ManifestHashContent([]byte(yaml))
	if hash != want {
		t.Errorf("hash = %q, want %q", hash, want)
	}
}

func TestManifestGetIn_HashChangesOnExternalWrite(t *testing.T) {
	dir := t.TempDir()
	a := &API{}
	name := "demo"
	initial := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9211\n"
	if err := a.ManifestCreateIn(dir, name, initial); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, h1, _ := a.ManifestGetInWithHash(dir, name)
	// External write — different bytes (port change) to simulate another
	// editor touching the file between Load and Save.
	path := filepath.Join(dir, name, "manifest.yaml")
	mutated := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9212\n"
	if err := os.WriteFile(path, []byte(mutated), 0600); err != nil {
		t.Fatalf("external write: %v", err)
	}
	_, h2, _ := a.ManifestGetInWithHash(dir, name)
	if h1 == h2 {
		t.Errorf("hash unchanged after external write: %q", h1)
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

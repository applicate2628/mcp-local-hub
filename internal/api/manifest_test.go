package api

import (
	"errors"
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

// Test YAML invariant: all strings passed to ManifestCreateIn AND to
// ManifestEditInWithHash (with non-empty expectedHash matching, or empty
// expectedHash — any path that reaches ManifestValidate) must pass
// api.ManifestValidate, which requires name + kind + transport + command
// + at least one daemon. See Task 2 for the same validator gate.
func TestManifestEditIn_RejectsHashMismatch(t *testing.T) {
	dir := t.TempDir()
	a := &API{}
	name := "demo"
	if err := a.ManifestCreateIn(dir, name, "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9220\n"); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, hash, _ := a.ManifestGetInWithHash(dir, name)
	path := filepath.Join(dir, name, "manifest.yaml")
	// External write bypasses validate (direct os.WriteFile); needs to
	// differ from the original bytes so the hash check trips.
	if err := os.WriteFile(path, []byte("name: demo\nkind: workspace-scoped\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9220\n"), 0600); err != nil {
		t.Fatalf("external write: %v", err)
	}
	// Edit yaml never reaches ManifestValidate because the hash-check
	// short-circuits first; still kept well-formed for clarity.
	_, err := a.ManifestEditInWithHash(dir, name, "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9220\n", hash)
	if err == nil {
		t.Fatalf("expected hash-mismatch error, got nil")
	}
	if !errors.Is(err, ErrManifestHashMismatch) {
		t.Errorf("err = %v, want ErrManifestHashMismatch", err)
	}
}

func TestManifestEditIn_AcceptsMatchingHash_ReturnsNewHash(t *testing.T) {
	dir := t.TempDir()
	a := &API{}
	name := "demo"
	orig := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9221\n"
	if err := a.ManifestCreateIn(dir, name, orig); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, hash, _ := a.ManifestGetInWithHash(dir, name)
	// Edit path reaches ManifestValidate — yaml must be well-formed.
	updated := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9221\n"
	newHash, err := a.ManifestEditInWithHash(dir, name, updated, hash)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	wantHash := ManifestHashContent([]byte(updated))
	if newHash != wantHash {
		t.Errorf("returned hash = %q, want %q", newHash, wantHash)
	}
	got, diskHash, _ := a.ManifestGetInWithHash(dir, name)
	if got != updated {
		t.Errorf("yaml = %q, want %q", got, updated)
	}
	if diskHash != newHash {
		t.Errorf("disk hash = %q does not match returned newHash %q", diskHash, newHash)
	}
}

func TestManifestEditIn_EmptyExpectedHash_SkipsCheck(t *testing.T) {
	dir := t.TempDir()
	a := &API{}
	name := "demo"
	orig := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9222\n"
	if err := a.ManifestCreateIn(dir, name, orig); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Empty expectedHash skips the check but still runs ManifestValidate
	// on the new yaml — must remain well-formed.
	updated := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9222\n"
	if _, err := a.ManifestEditInWithHash(dir, name, updated, ""); err != nil {
		t.Fatalf("empty-hash edit should succeed: %v", err)
	}
}

func TestManifestEditIn_AtomicWrite_TargetUnchangedOnFailure(t *testing.T) {
	dir := t.TempDir()
	a := &API{}
	name := "demo"
	orig := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9223\n"
	if err := a.ManifestCreateIn(dir, name, orig); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Inject write failure between tmp-close and rename.
	ManifestSetFailWriteHook(func() bool { return true })
	defer ManifestSetFailWriteHook(nil)
	updated := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9223\n"
	_, err := a.ManifestEditInWithHash(dir, name, updated, "")
	if err == nil {
		t.Fatalf("expected injected failure, got nil")
	}
	// Target content must be UNCHANGED.
	got, _, _ := a.ManifestGetInWithHash(dir, name)
	if got != orig {
		t.Errorf("target yaml changed on failure: %q, want %q", got, orig)
	}
	// No stale tmp file left.
	files, _ := os.ReadDir(filepath.Join(dir, name))
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".tmp") {
			t.Errorf("stale tmp file left: %q", f.Name())
		}
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

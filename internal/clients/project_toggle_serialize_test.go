// internal/clients/project_toggle_serialize_test.go
//
// Per-project-GUI P3a round-2 (bot PR #433) findings 3 + 4 for the Model-B
// object-member leaf writer (ToggleProjectObjectMember):
//
//   - finding 3: the read-modify-write is serialized through withConfigLock (the
//     SAME per-path mutex the adapter decorator wraps every mutating method in),
//     so two concurrent toggles of DIFFERENT members on the SAME project config
//     do not lost-update each other.
//   - finding 4: an ENABLE into a first-time project whose config PARENT dir does
//     not exist yet succeeds, because withConfigLock creates the parent via the
//     hardened SecureCreateParentDir BEFORE the write — even when the WRITER
//     itself does NOT create the parent (the production SecureWriteClientConfig
//     opens the immediate parent and does not mkdir). A DISABLE into a
//     missing-parent project is a pure no-op that creates no stray dir.
//
// STATE-SAFETY: all writes target t.TempDir() project config paths; no HOME /
// state dir is touched. Tests that override WriteConfigFile / SecureCreateParentDir
// capture+restore the originals.
package clients

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestToggleProjectObjectMember_ConcurrentNoLostUpdate pins finding 3: two
// concurrent ENABLE toggles of DIFFERENT members against the SAME project config
// file both survive. Without the withConfigLock serialization each goroutine
// reads the same (empty) baseline and the later write clobbers the earlier —
// exactly the lost update the per-path lock prevents.
func TestToggleProjectObjectMember_ConcurrentNoLostUpdate(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, ".cursor", "mcp.json")
	// Seed the dir + an empty config so both racers start from the same baseline.
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(cfg, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const n = 24
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines together to maximize contention
			name := fmt.Sprintf("srv%02d", i)
			errs[i] = ToggleProjectObjectMember("cursor", cfg, name,
				map[string]any{"command": "node", "args": []any{name}}, true)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Fatalf("concurrent enable %d errored: %v", i, e)
		}
	}

	// Every member must be present — none lost to a torn read-modify-write.
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	m, err := parseJSONCBytes(data)
	if err != nil {
		t.Fatalf("parse: %v (file=\n%s)", err, data)
	}
	section, _ := m["mcpServers"].(map[string]any)
	if section == nil {
		t.Fatalf("mcpServers section missing after concurrent enables (file=\n%s)", data)
	}
	if len(section) != n {
		t.Fatalf("concurrent enables lost updates: %d members survived, want %d (file=\n%s)", len(section), n, data)
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("srv%02d", i)
		if _, ok := section[name]; !ok {
			t.Errorf("member %q lost to a concurrent write", name)
		}
	}
}

// TestToggleProjectObjectMember_EnableCreatesMissingParent pins finding 4: an
// ENABLE into a fresh project whose .cursor/ parent does not exist succeeds
// because withConfigLock creates the parent via SecureCreateParentDir BEFORE the
// write — proven by overriding WriteConfigFile with a writer that REFUSES to
// create the parent itself (the production SecureWriteClientConfig posture). If
// withConfigLock did not create the parent first, the write would fail.
func TestToggleProjectObjectMember_EnableCreatesMissingParent(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, ".cursor", "mcp.json") // .cursor/ does NOT exist yet

	// Simulate the production writer: open-immediate-parent, NO mkdir. It fails
	// if the parent dir is absent — so the test only passes if withConfigLock
	// created the parent (via the fallback SecureCreateParentDir) beforehand.
	origWrite := WriteConfigFile
	WriteConfigFile = func(path string, contents []byte) error {
		if dir := filepath.Dir(path); dir != "" {
			if _, statErr := os.Stat(dir); statErr != nil {
				return fmt.Errorf("simulated SecureWriteClientConfig: parent dir absent (writer does NOT mkdir): %w", statErr)
			}
		}
		return os.WriteFile(path, contents, 0o600)
	}
	t.Cleanup(func() { WriteConfigFile = origWrite })

	// SecureCreateParentDir stays the in-package fallback (MkdirAll 0o700), which
	// is what withConfigLock invokes. This is the seam that must create .cursor/.
	if _, err := os.Stat(filepath.Dir(cfg)); err == nil {
		t.Fatalf("precondition: .cursor/ must NOT exist before the toggle")
	}

	val := map[string]any{"command": "node", "args": []any{"b.js"}}
	if err := ToggleProjectObjectMember("cursor", cfg, "beta", val, true); err != nil {
		t.Fatalf("enable into missing-parent project failed (finding 4: parent must be created hardened): %v", err)
	}

	// The parent + the member must now exist.
	if _, err := os.Stat(filepath.Dir(cfg)); err != nil {
		t.Fatalf(".cursor/ not created by the enable: %v", err)
	}
	present, err := ProjectObjectMemberPresent("cursor", cfg, "beta")
	if err != nil {
		t.Fatalf("read-back: %v", err)
	}
	if !present {
		t.Fatalf("member 'beta' absent after enable into a fresh project")
	}
}

// TestToggleProjectObjectMember_DisableMissingParentNoStrayDir pins the finding-4
// disable carve-out: a DISABLE into a project with no config dir is a pure no-op
// that creates NO file and NO stray empty .cursor/ directory (the member can't
// exist without the dir, and a writer needs the dir to exist — so we short-
// circuit before taking the lock / creating the parent).
func TestToggleProjectObjectMember_DisableMissingParentNoStrayDir(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, ".cursor", "mcp.json") // .cursor/ does NOT exist

	// A writer override that FAILS if it is ever called — a correct disable-of-
	// absent must not write anything at all.
	origWrite := WriteConfigFile
	WriteConfigFile = func(path string, contents []byte) error {
		return fmt.Errorf("disable-of-absent must not write, but WriteConfigFile was called for %s", path)
	}
	t.Cleanup(func() { WriteConfigFile = origWrite })

	if err := ToggleProjectObjectMember("cursor", cfg, "beta", nil, false); err != nil {
		t.Fatalf("disable into missing-parent project errored (must be a clean no-op): %v", err)
	}

	// No stray .cursor/ directory created.
	if _, err := os.Stat(filepath.Dir(cfg)); err == nil {
		t.Fatalf("disable-of-absent created a stray empty config dir %s", filepath.Dir(cfg))
	}
	// No config file created either.
	if _, err := os.Stat(cfg); err == nil {
		t.Fatalf("disable-of-absent created a config file %s", cfg)
	}
}

// TestToggleClaudeLocal_ConcurrentNoLostUpdate pins bot PR #433 r3 finding 1: the
// claude-local array-move RMW is now serialized through withConfigLock (the SAME
// per-path lock the object-member path and the adapter decorator use). Two
// concurrent ENABLE toggles of DIFFERENT .mcp.json servers — which both
// read-modify-write the SINGLE shared ~/.claude.json — must BOTH land their array
// move. Before the fix each goroutine read the same snapshot and the later
// whole-file WriteConfigFile replacement clobbered the earlier (lost update).
//
// STATE-SAFETY: setSyntheticHome t.Setenv's HOME/USERPROFILE to a temp dir and
// seeds a synthetic ~/.claude.json there; the live host file is unreachable. The
// in-package test-default WriteConfigFile (plain os.WriteFile) +
// SecureCreateParentDir (MkdirAll, on the always-existing temp home) are used.
func TestToggleClaudeLocal_ConcurrentNoLostUpdate(t *testing.T) {
	root := toggleTestRoot()
	key := projectKey("fwd-upper", root)
	keyEsc := jsonEscapeForTest(key)
	// Seed the project entry with an empty enabled array so every racer starts
	// from the same baseline (a single shared ~/.claude.json, one projects.<key>).
	body := `{"projects":{"` + keyEsc + `":{"enabledMcpjsonServers":[]}}}`
	_, claudePath := setSyntheticHome(t, body)

	const n = 24
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release together to maximize contention
			server := fmt.Sprintf("srv%02d", i)
			errs[i] = ToggleClaudeMcpjsonMembership(root, server, true /*enable*/, false /*allowSymlink*/)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Fatalf("concurrent claude-local enable %d errored: %v", i, e)
		}
	}

	// Every server name must survive in enabledMcpjsonServers — none lost to a
	// torn read-modify-write of the shared ~/.claude.json.
	_, entry := readClaudeProjectsMap(t, claudePath, root)
	if entry == nil {
		t.Fatalf("project entry lost after concurrent toggles")
	}
	enabled := stringSliceFromAny(entry["enabledMcpjsonServers"])
	got := make(map[string]bool, len(enabled))
	for _, s := range enabled {
		got[s] = true
	}
	if len(got) != n {
		t.Fatalf("concurrent claude-local enables lost updates: %d distinct servers survived, want %d (enabled=%v)", len(got), n, enabled)
	}
	for i := 0; i < n; i++ {
		server := fmt.Sprintf("srv%02d", i)
		if !got[server] {
			t.Errorf("server %q lost to a concurrent claude-local write", server)
		}
	}
}

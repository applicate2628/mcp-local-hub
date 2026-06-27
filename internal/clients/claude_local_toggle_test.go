// internal/clients/claude_local_toggle_test.go
//
// Per-project-GUI Phase 3a: ToggleClaudeMcpjsonMembership unit tests — the
// CATASTROPHIC-corruption surface (design T2). These pin the load-bearing
// guarantees of the claude-local array-move writer:
//
//   - ROUND-TRIP-PRESERVES-EVERYTHING: a realistic multi-key ~/.claude.json
//     (mcpServers + N projects.<key> entries + unrelated top-level keys + JSONC
//     comments) survives an enable-then-disable with ONLY the toggled arrays
//     changed — every other top-level key, every sibling projects.<key>, the
//     project's own mcpServers, and the comments stay intact.
//   - NEVER DELETES mcpServers (decision 5): after a disable, the project's
//     projects.<key>.mcpServers map still has the server definition — the toggle
//     is an APPROVAL move, never a definition delete.
//   - the enabled-state read-back goes through the SINGLE-owner predicate
//     IsMcpjsonServerEnabled.
//
// STATE-SAFETY (CRITICAL): every test t.Setenv's HOME + USERPROFILE to a temp
// dir and writes a SYNTHETIC ~/.claude.json there (via setSyntheticHome from
// claude_local_scope_test.go). The developer's REAL ~/.claude.json is NEVER
// read or written — os.UserHomeDir() resolves to the temp dir under the
// redirected env. The writes go through the test-default WriteConfigFile
// (plain os.WriteFile; production swaps it to SecureWriteClientConfig in
// api.init), so the in-package test never needs the hardened pipeline.
package clients

import (
	"os"
	"reflect"
	"runtime"
	"sort"
	"testing"
)

// toggleTestRoot returns an absolute project root for the running OS.
func toggleTestRoot() string {
	if runtime.GOOS == "windows" {
		return `C:\dev\toggleproj`
	}
	return "/dev/toggleproj"
}

// readClaudeProjectsMap parses the on-disk ~/.claude.json and returns its
// top-level map + the matched project entry for root (via the shared reader
// path), failing the test on any parse error.
func readClaudeProjectsMap(t *testing.T, claudePath, root string) (top map[string]any, entry map[string]any) {
	t.Helper()
	data, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatalf("read claude.json: %v", err)
	}
	m, err := parseJSONCBytes(data)
	if err != nil {
		t.Fatalf("parse claude.json: %v", err)
	}
	projects, _ := m["projects"].(map[string]any)
	rawKey, ok := matchClaudeProjectRawKey(projects, root)
	if !ok {
		t.Fatalf("no projects entry matched root %q (keys=%v)", root, mapKeys(projects))
	}
	e, _ := projects[rawKey].(map[string]any)
	return m, e
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestToggleClaudeLocal_RoundTripPreservesEverything is THE corruption test.
func TestToggleClaudeLocal_RoundTripPreservesEverything(t *testing.T) {
	root := toggleTestRoot()
	key := projectKey("fwd-upper", root) // a real-world form Claude writes
	keyEsc := jsonEscapeForTest(key)

	// A realistic multi-key file: a top-level mcpServers, an unrelated top-level
	// key, two sibling projects, JSONC comments, and our target project carrying
	// its own mcpServers + an existing toggle array.
	siblingKey := jsonEscapeForTest(siblingProjectKey())
	body := `{
  // top-of-file comment must survive
  "mcpServers": { "globalServer": { "command": "node" } },
  "numStartups": 42,
  "tipsHistory": { "x": 1 },
  "projects": {
    "` + keyEsc + `": {
      // this project's own mcpServers (a Local-scope definition) must NEVER be deleted
      "mcpServers": { "localOnly": { "command": "uvx" } },
      "enabledMcpjsonServers": ["already"],
      "disabledMcpjsonServers": ["target"]
    },
    "` + siblingKey + `": {
      "mcpServers": { "siblingSrv": { "command": "deno" } },
      "enabledMcpjsonServers": ["siblingEnabled"]
    }
  }
}`
	_, claudePath := setSyntheticHome(t, body)

	// ENABLE "target": must move it out of disabled and into enabled.
	if err := ToggleClaudeMcpjsonMembership(root, "target", true, false); err != nil {
		t.Fatalf("enable target: %v", err)
	}
	// DISABLE "target": must move it back to disabled.
	if err := ToggleClaudeMcpjsonMembership(root, "target", false, false); err != nil {
		t.Fatalf("disable target: %v", err)
	}

	top, entry := readClaudeProjectsMap(t, claudePath, root)

	// --- Every UNRELATED top-level key survives, byte-for-byte (value-for-value).
	if _, ok := top["mcpServers"].(map[string]any); !ok {
		t.Errorf("top-level mcpServers lost")
	}
	if got, _ := top["numStartups"].(float64); got != 42 {
		t.Errorf("numStartups = %v, want 42", top["numStartups"])
	}
	if _, ok := top["tipsHistory"].(map[string]any); !ok {
		t.Errorf("tipsHistory lost")
	}

	// --- The SIBLING project survives untouched.
	projects, _ := top["projects"].(map[string]any)
	sib, _ := matchClaudeProjectRawKey(projects, siblingProjectRoot())
	if sib == "" {
		t.Fatalf("sibling project lost (keys=%v)", mapKeys(projects))
	}
	sibEntry, _ := projects[sib].(map[string]any)
	if _, ok := sibEntry["mcpServers"].(map[string]any); !ok {
		t.Errorf("sibling mcpServers lost")
	}
	if en := stringSliceFromAny(sibEntry["enabledMcpjsonServers"]); !reflect.DeepEqual(en, []string{"siblingEnabled"}) {
		t.Errorf("sibling enabled array changed: %v", en)
	}

	// --- The TARGET project's own mcpServers (Local-scope DEFINITION) is intact
	//     — NEVER deleted by a toggle (decision 5).
	if ms, ok := entry["mcpServers"].(map[string]any); !ok {
		t.Errorf("target project mcpServers DELETED by toggle (data-loss!)")
	} else if _, ok := ms["localOnly"]; !ok {
		t.Errorf("target project mcpServers.localOnly definition lost")
	}

	// --- The toggle landed: after enable→disable, "target" ends DISABLED and the
	//     pre-existing "already" enable entry is preserved.
	disabled := stringSliceFromAny(entry["disabledMcpjsonServers"])
	if !containsStr(disabled, "target") {
		t.Errorf("target not in disabledMcpjsonServers after disable: %v", disabled)
	}
	enabled := stringSliceFromAny(entry["enabledMcpjsonServers"])
	if !containsStr(enabled, "already") {
		t.Errorf("pre-existing 'already' enable entry lost: %v", enabled)
	}
	if containsStr(enabled, "target") {
		t.Errorf("target still in enabledMcpjsonServers after disable: %v", enabled)
	}

	// --- Comments survive (the file is still JSONC with our comment text).
	raw, _ := os.ReadFile(claudePath)
	if !containsBytes(raw, "top-of-file comment must survive") {
		t.Errorf("top-of-file comment lost after toggle round-trip; file=\n%s", raw)
	}
	if !containsBytes(raw, "must NEVER be deleted") {
		t.Errorf("project mcpServers comment lost; file=\n%s", raw)
	}
}

// TestToggleClaudeLocal_NeverDeletesMcpServers is the explicit decision-5 guard:
// disabling a server moves it to disabledMcpjsonServers but leaves the project's
// own mcpServers DEFINITION (and the top-level one) intact.
func TestToggleClaudeLocal_NeverDeletesMcpServers(t *testing.T) {
	root := toggleTestRoot()
	keyEsc := jsonEscapeForTest(projectKey("back-upper", root))
	body := `{
  "projects": {
    "` + keyEsc + `": {
      "mcpServers": { "keepme": { "command": "node" }, "alsoKeep": {} },
      "enabledMcpjsonServers": ["keepme"]
    }
  }
}`
	_, claudePath := setSyntheticHome(t, body)

	if err := ToggleClaudeMcpjsonMembership(root, "keepme", false, false); err != nil {
		t.Fatalf("disable keepme: %v", err)
	}

	_, entry := readClaudeProjectsMap(t, claudePath, root)
	ms, ok := entry["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers map DELETED (data-loss!)")
	}
	if _, ok := ms["keepme"]; !ok {
		t.Errorf("mcpServers[keepme] DEFINITION deleted by a disable (decision-5 violation)")
	}
	if _, ok := ms["alsoKeep"]; !ok {
		t.Errorf("mcpServers[alsoKeep] (unrelated def) lost")
	}
	disabled := stringSliceFromAny(entry["disabledMcpjsonServers"])
	if !containsStr(disabled, "keepme") {
		t.Errorf("keepme not moved into disabledMcpjsonServers: %v", disabled)
	}
	enabled := stringSliceFromAny(entry["enabledMcpjsonServers"])
	if containsStr(enabled, "keepme") {
		t.Errorf("keepme not removed from enabledMcpjsonServers: %v", enabled)
	}
}

// TestToggleClaudeLocal_EnableThenDisableState pins the read-back through the
// single-owner predicate IsMcpjsonServerEnabled across enable/disable.
func TestToggleClaudeLocal_EnableThenDisableState(t *testing.T) {
	root := toggleTestRoot()
	keyEsc := jsonEscapeForTest(projectKey("fwd-lower", root))
	body := `{"projects":{"` + keyEsc + `":{"enableAllProjectMcpServers":true}}}`
	setSyntheticHome(t, body)

	read := func() ClaudeLocalScope {
		t.Helper()
		s, err := ReadClaudeLocalScope(root, false)
		if err != nil {
			t.Fatalf("read scope: %v", err)
		}
		return s
	}

	// DISABLE wins even with enableAll true (deny is absolute).
	if err := ToggleClaudeMcpjsonMembership(root, "srv", false, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if read().IsMcpjsonServerEnabled("srv") {
		t.Errorf("after disable with enableAll=true, srv still enabled (deny must win)")
	}
	// ENABLE: must override the disabled membership.
	if err := ToggleClaudeMcpjsonMembership(root, "srv", true, false); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !read().IsMcpjsonServerEnabled("srv") {
		t.Errorf("after enable, srv not enabled")
	}
}

// TestToggleClaudeLocal_FreshFileCreatesEntry: with NO ~/.claude.json present, a
// toggle builds a fresh, well-formed file with only the project entry + array.
func TestToggleClaudeLocal_FreshFileCreatesEntry(t *testing.T) {
	root := toggleTestRoot()
	_, claudePath := setSyntheticHome(t, "") // no file

	if err := ToggleClaudeMcpjsonMembership(root, "newsrv", true, false); err != nil {
		t.Fatalf("enable on fresh file: %v", err)
	}
	if _, err := os.Stat(claudePath); err != nil {
		t.Fatalf("fresh ~/.claude.json not created: %v", err)
	}
	s, err := ReadClaudeLocalScope(root, false)
	if err != nil {
		t.Fatalf("read fresh scope: %v", err)
	}
	if !s.IsMcpjsonServerEnabled("newsrv") {
		t.Errorf("newsrv not enabled in fresh file")
	}
	if !s.Matched {
		t.Errorf("fresh file's project entry not matched")
	}
}

// TestToggleClaudeLocal_NoFileNoSiblings_PreservesOtherProjects: creating a NEW
// project entry must not clobber an EXISTING sibling project.
func TestToggleClaudeLocal_CreateEntryPreservesSibling(t *testing.T) {
	root := toggleTestRoot()
	sibKey := jsonEscapeForTest(siblingProjectKey())
	// File has only the SIBLING; our target project has NO entry yet.
	body := `{"projects":{"` + sibKey + `":{"mcpServers":{"s":{}},"enabledMcpjsonServers":["e"]}}}`
	_, claudePath := setSyntheticHome(t, body)

	if err := ToggleClaudeMcpjsonMembership(root, "added", true, false); err != nil {
		t.Fatalf("enable (creates new entry): %v", err)
	}

	top, entry := readClaudeProjectsMap(t, claudePath, root)
	// New entry present + correct.
	if !containsStr(stringSliceFromAny(entry["enabledMcpjsonServers"]), "added") {
		t.Errorf("new project entry missing the enabled toggle")
	}
	// Sibling untouched.
	projects, _ := top["projects"].(map[string]any)
	sib, _ := matchClaudeProjectRawKey(projects, siblingProjectRoot())
	if sib == "" {
		t.Fatalf("sibling project clobbered when creating a new entry")
	}
	sibEntry, _ := projects[sib].(map[string]any)
	if !containsStr(stringSliceFromAny(sibEntry["enabledMcpjsonServers"]), "e") {
		t.Errorf("sibling toggle array changed: %v", sibEntry["enabledMcpjsonServers"])
	}
}

// TestComputeMcpjsonToggleMove pins the pure move semantics in isolation.
func TestComputeMcpjsonToggleMove(t *testing.T) {
	cases := []struct {
		name                   string
		server                 string
		enabled, disabled      []string
		enable                 bool
		wantEnabled, wantDisab []string
	}{
		{"enable-from-disabled", "x", nil, []string{"x"}, true, []string{"x"}, nil},
		{"enable-already-enabled", "x", []string{"x"}, nil, true, []string{"x"}, nil},
		{"disable-from-enabled", "x", []string{"x"}, nil, false, nil, []string{"x"}},
		{"disable-already-disabled", "x", nil, []string{"x"}, false, nil, []string{"x"}},
		{"enable-preserves-others", "x", []string{"a"}, []string{"x", "b"}, true, []string{"a", "x"}, []string{"b"}},
		{"disable-preserves-others", "x", []string{"x", "a"}, []string{"b"}, false, []string{"a"}, []string{"b", "x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotEn, gotDis := computeMcpjsonToggleMove(c.server, c.enabled, c.disabled, c.enable)
			if !reflect.DeepEqual(gotEn, c.wantEnabled) {
				t.Errorf("enabled = %v, want %v", gotEn, c.wantEnabled)
			}
			if !reflect.DeepEqual(gotDis, c.wantDisab) {
				t.Errorf("disabled = %v, want %v", gotDis, c.wantDisab)
			}
		})
	}
}

// --- small local helpers (no dependency on the api package) ---

func siblingProjectRoot() string {
	if runtime.GOOS == "windows" {
		return `C:\dev\siblingproj`
	}
	return "/dev/siblingproj"
}

func siblingProjectKey() string {
	return projectKey("fwd-upper", siblingProjectRoot())
}

func containsStr(in []string, s string) bool {
	for _, v := range in {
		if v == s {
			return true
		}
	}
	return false
}

func containsBytes(b []byte, sub string) bool {
	return len(sub) == 0 || (len(b) >= len(sub) && indexBytes(b, sub) >= 0)
}

func indexBytes(b []byte, sub string) int {
	for i := 0; i+len(sub) <= len(b); i++ {
		if string(b[i:i+len(sub)]) == sub {
			return i
		}
	}
	return -1
}

// jsonEscapeForTest escapes a key for embedding inside a JSON string literal in
// a test fixture (backslashes + quotes). Windows backslash keys are the case
// that needs it.
func jsonEscapeForTest(s string) string {
	out := make([]byte, 0, len(s)+4)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			out = append(out, '\\', '\\')
		case '"':
			out = append(out, '\\', '"')
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}

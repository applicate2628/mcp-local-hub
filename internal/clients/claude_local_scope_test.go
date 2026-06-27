// internal/clients/claude_local_scope_test.go
//
// Per-project-GUI Phase 2b: ReadClaudeLocalScope unit tests (READ-ONLY reader
// of ~/.claude.json → projects.<key>).
//
// STATE-SAFETY (CRITICAL): every test t.Setenv's HOME + USERPROFILE to a temp
// dir and writes a SYNTHETIC ~/.claude.json there. The developer's REAL
// ~/.claude.json is NEVER read or written by these tests — os.UserHomeDir()
// resolves to the temp dir under the redirected env. A snapshot test
// additionally proves the synthetic file is byte-identical after a read (the
// reader writes nothing).
package clients

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// setSyntheticHome points HOME + USERPROFILE at a fresh temp dir and writes the
// given ~/.claude.json body into it. Returns the home dir and the claude.json
// path. An empty body means "do not create the file" (the absent-file case).
func setSyntheticHome(t *testing.T, claudeJSON string) (home, claudePath string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	claudePath = filepath.Join(home, ".claude.json")
	if claudeJSON != "" {
		if err := os.WriteFile(claudePath, []byte(claudeJSON), 0o600); err != nil {
			t.Fatalf("write synthetic claude.json: %v", err)
		}
	}
	return home, claudePath
}

// projectKey returns a projects.<key> string in one of the THREE forms observed
// on the host (fwd+upper, fwd+lower, back+upper). The reader must match all of
// them to the same root via canonicalClaudeProjectKey. On POSIX, fall back to a
// POSIX-shaped key (the case/separator variance is Windows-specific).
func projectKey(form, root string) string {
	if runtime.GOOS != "windows" {
		return root // POSIX: single canonical form
	}
	switch form {
	case "fwd-upper":
		return strings.ReplaceAll(root, "\\", "/")
	case "fwd-lower":
		k := strings.ReplaceAll(root, "\\", "/")
		if len(k) >= 1 {
			k = strings.ToLower(k[:1]) + k[1:]
		}
		return k
	case "back-upper":
		return strings.ReplaceAll(root, "/", "\\")
	default:
		return root
	}
}

// TestReadClaudeLocalScope_MissingFile: absent ~/.claude.json → empty, no error.
func TestReadClaudeLocalScope_MissingFile(t *testing.T) {
	setSyntheticHome(t, "") // no file
	got, err := ReadClaudeLocalScope(`C:\dev\proj`)
	if err != nil {
		t.Fatalf("missing file should not error, got: %v", err)
	}
	if got.Matched {
		t.Errorf("missing file should yield Matched=false, got %+v", got)
	}
	if len(got.LocalServers) != 0 || len(got.Disabled) != 0 || len(got.Enabled) != 0 {
		t.Errorf("missing file should yield empty scope, got %+v", got)
	}
}

// TestReadClaudeLocalScope_NoProjectsMap: file present but no `projects` key →
// empty, no error.
func TestReadClaudeLocalScope_NoProjectsMap(t *testing.T) {
	setSyntheticHome(t, `{"mcpServers":{"x":{"url":"http://h/mcp"}}}`)
	got, err := ReadClaudeLocalScope(`C:\dev\proj`)
	if err != nil {
		t.Fatalf("no projects map should not error, got: %v", err)
	}
	if got.Matched {
		t.Errorf("no projects map → Matched=false, got %+v", got)
	}
}

// TestReadClaudeLocalScope_NoMatchingKey: projects map present but no key for
// the requested root → empty, no error.
func TestReadClaudeLocalScope_NoMatchingKey(t *testing.T) {
	body := `{"projects":{"C:/some/other/proj":{"mcpServers":{"z":{}}}}}`
	setSyntheticHome(t, body)
	got, err := ReadClaudeLocalScope(`C:\dev\proj`)
	if err != nil {
		t.Fatalf("no matching key should not error, got: %v", err)
	}
	if got.Matched {
		t.Errorf("no matching key → Matched=false, got %+v", got)
	}
}

// TestReadClaudeLocalScope_LocalServers: a matching projects.<key>.mcpServers is
// returned (sorted) for the matching root.
func TestReadClaudeLocalScope_LocalServers(t *testing.T) {
	root := osRoot(t, "dev", "proj")
	key := projectKey("fwd-upper", root)
	body := `{"projects":{"` + jsonEsc(key) + `":{"mcpServers":{"zeta":{"command":"node"},"alpha":{"url":"http://h/mcp"}}}}}`
	setSyntheticHome(t, body)

	got, err := ReadClaudeLocalScope(root)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !got.Matched {
		t.Fatalf("expected Matched=true for root %q vs key %q", root, key)
	}
	want := []string{"alpha", "zeta"} // sorted
	if !reflect.DeepEqual(got.LocalServers, want) {
		t.Errorf("LocalServers = %v, want %v (sorted)", got.LocalServers, want)
	}
}

// TestReadClaudeLocalScope_KeyFormVariants: the SAME root matches the
// projects.<key> across the separator/case forms the host actually writes
// (fwd+upper, fwd+lower, back+upper). This is the load-bearing robustness test.
func TestReadClaudeLocalScope_KeyFormVariants(t *testing.T) {
	root := osRoot(t, "dev", "myproj")
	forms := []string{"fwd-upper", "fwd-lower", "back-upper"}
	for _, form := range forms {
		t.Run(form, func(t *testing.T) {
			key := projectKey(form, root)
			body := `{"projects":{"` + jsonEsc(key) + `":{"mcpServers":{"s1":{}},"disabledMcpjsonServers":[],"enabledMcpjsonServers":[]}}}`
			setSyntheticHome(t, body)

			got, err := ReadClaudeLocalScope(root)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !got.Matched {
				t.Errorf("form %q: root %q did NOT match key %q (canonical mismatch)", form, root, key)
			}
			if !reflect.DeepEqual(got.LocalServers, []string{"s1"}) {
				t.Errorf("form %q: LocalServers = %v, want [s1]", form, got.LocalServers)
			}
		})
	}
}

// TestReadClaudeLocalScope_ToggleArrays: disabled/enabled arrays are returned
// verbatim and the reconciliation rule is applied correctly (disabled, enabled,
// enabled-override, neither).
func TestReadClaudeLocalScope_ToggleArrays(t *testing.T) {
	root := osRoot(t, "dev", "proj")
	key := projectKey("fwd-upper", root)
	body := `{"projects":{"` + jsonEsc(key) + `":{` +
		`"mcpServers":{"local-only":{}},` +
		`"disabledMcpjsonServers":["disabledServer","bothServer"],` +
		`"enabledMcpjsonServers":["enabledServer","bothServer"]}}}`
	setSyntheticHome(t, body)

	got, err := ReadClaudeLocalScope(root)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !got.Matched {
		t.Fatalf("expected match")
	}
	if !reflect.DeepEqual(got.Disabled, []string{"disabledServer", "bothServer"}) {
		t.Errorf("Disabled = %v", got.Disabled)
	}
	if !reflect.DeepEqual(got.Enabled, []string{"enabledServer", "bothServer"}) {
		t.Errorf("Enabled = %v", got.Enabled)
	}

	// Reconciliation rule:
	cases := []struct {
		name string
		want bool
	}{
		{"disabledServer", false}, // in disabled, not in enabled → disabled
		{"enabledServer", true},   // in enabled only → enabled
		{"bothServer", true},      // in BOTH → enabled wins (override)
		{"unlistedServer", true},  // in neither → default enabled
	}
	for _, c := range cases {
		if got.IsMcpjsonServerEnabled(c.name) != c.want {
			t.Errorf("IsMcpjsonServerEnabled(%q) = %v, want %v", c.name, !c.want, c.want)
		}
	}
}

// TestReadClaudeLocalScope_ReadOnly_ByteIdentical proves the reader writes
// NOTHING: the synthetic ~/.claude.json is byte-identical (and same mtime) after
// a read, and no sibling file is created.
func TestReadClaudeLocalScope_ReadOnly_ByteIdentical(t *testing.T) {
	root := osRoot(t, "dev", "proj")
	key := projectKey("fwd-upper", root)
	body := `{"projects":{"` + jsonEsc(key) + `":{"mcpServers":{"s":{}},"disabledMcpjsonServers":["d"]}},"otherTopLevel":42}`
	home, claudePath := setSyntheticHome(t, body)

	beforeBytes, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatalf("pre-read: %v", err)
	}
	beforeInfo, _ := os.Stat(claudePath)
	beforeHomeListing := listDir(t, home)

	if _, err := ReadClaudeLocalScope(root); err != nil {
		t.Fatalf("read: %v", err)
	}

	afterBytes, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatalf("post-read: %v", err)
	}
	if !reflect.DeepEqual(beforeBytes, afterBytes) {
		t.Errorf("~/.claude.json content changed after a read-only scope read")
	}
	afterInfo, _ := os.Stat(claudePath)
	if beforeInfo != nil && afterInfo != nil && !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Errorf("~/.claude.json mtime changed: before=%v after=%v", beforeInfo.ModTime(), afterInfo.ModTime())
	}
	afterHomeListing := listDir(t, home)
	if !reflect.DeepEqual(beforeHomeListing, afterHomeListing) {
		t.Errorf("home dir listing changed (file created/removed): before=%v after=%v", beforeHomeListing, afterHomeListing)
	}
}

// TestReadClaudeLocalScope_MalformedJSON: an unparseable file is a genuine
// error (not silently treated as empty), so corruption is surfaced.
func TestReadClaudeLocalScope_MalformedJSON(t *testing.T) {
	setSyntheticHome(t, `{"projects": {`) // truncated
	_, err := ReadClaudeLocalScope(`C:\dev\proj`)
	if err == nil {
		t.Errorf("malformed ~/.claude.json should error, got nil")
	}
}

// --- helpers ---

// osRoot builds an absolute project root that is valid for the current OS:
// `C:\<parts...>` on Windows, `/<parts...>` on POSIX.
func osRoot(t *testing.T, parts ...string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return `C:\` + filepath.Join(parts...)
	}
	return "/" + filepath.Join(parts...)
}

// jsonEsc escapes a path for embedding inside a JSON string literal (backslash
// + quote). Windows back-slash keys must be doubled in the JSON source.
func jsonEsc(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// listDir returns the sorted base names of the entries directly under dir.
func listDir(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

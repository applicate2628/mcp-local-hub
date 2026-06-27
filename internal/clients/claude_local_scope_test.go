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

// TestReadClaudeLocalScope_DuplicateKeyDeterministic proves the finding-2 fix:
// when ~/.claude.json has TWO projects.<key> entries that canonicalize to the
// SAME key, the reader ALWAYS picks the first by SORTED raw key (not a random
// map-iteration winner). Each colliding entry carries a DISTINCT server set so
// we can tell which one was chosen, and the read is repeated several times to
// prove the pick is stable across Go's randomized map iteration order.
//
// Cross-platform collision: a trailing-slash vs no-trailing-slash key both
// Clean to the same canonical key. Sorted, the shorter (no trailing slash) raw
// key wins ("/dev/proj" < "/dev/proj/"; "C:/dev/proj" < "C:/dev/proj/").
func TestReadClaudeLocalScope_DuplicateKeyDeterministic(t *testing.T) {
	root := osRoot(t, "dev", "proj")
	// Two raw keys that Clean to the same canonical key but differ by a trailing
	// slash. The forward-slash form keeps the JSON source simple on both OSes.
	base := strings.ReplaceAll(root, "\\", "/") // e.g. C:/dev/proj or /dev/proj
	keyNoSlash := base                          // sorted-FIRST (shorter prefix)
	keyWithSlash := base + "/"                  // sorted-second

	// keyNoSlash → server "first-wins"; keyWithSlash → server "second-loses".
	body := `{"projects":{` +
		`"` + jsonEsc(keyNoSlash) + `":{"mcpServers":{"first-wins":{}}},` +
		`"` + jsonEsc(keyWithSlash) + `":{"mcpServers":{"second-loses":{}}}` +
		`}}`

	// Sanity: the two raw keys must canonicalize identically (a real collision).
	if canonicalClaudeProjectKey(keyNoSlash) != canonicalClaudeProjectKey(keyWithSlash) {
		t.Fatalf("test bug: %q and %q do not collide canonically", keyNoSlash, keyWithSlash)
	}

	// Repeat the read; Go randomizes map iteration so an order-dependent bug would
	// surface as a flaky winner across iterations. The sorted-first rule must pick
	// "first-wins" every time.
	for i := 0; i < 16; i++ {
		setSyntheticHome(t, body)
		got, err := ReadClaudeLocalScope(root)
		if err != nil {
			t.Fatalf("iteration %d: read: %v", i, err)
		}
		if !got.Matched {
			t.Fatalf("iteration %d: expected a match for colliding keys", i)
		}
		if !reflect.DeepEqual(got.LocalServers, []string{"first-wins"}) {
			t.Fatalf("iteration %d: LocalServers = %v, want [first-wins] (sorted-first raw key %q must win deterministically)",
				i, got.LocalServers, keyNoSlash)
		}
	}
}

// TestReadClaudeLocalScope_DuplicateKeyDeterministic_WindowsCaseFold is the
// Windows-specific collision: two keys differing ONLY by drive-letter case
// (`C:/dev/proj` vs `c:/dev/proj`) both case-fold to the same canonical key.
// Sorted, the uppercase-drive key wins ('C' 0x43 < 'c' 0x63).
func TestReadClaudeLocalScope_DuplicateKeyDeterministic_WindowsCaseFold(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("drive-letter case-fold collision is Windows-specific")
	}
	root := osRoot(t, "dev", "proj")
	keyUpper := projectKey("fwd-upper", root) // C:/dev/proj — sorted-FIRST
	keyLower := projectKey("fwd-lower", root) // c:/dev/proj — sorted-second

	body := `{"projects":{` +
		`"` + jsonEsc(keyUpper) + `":{"mcpServers":{"upper-wins":{}}},` +
		`"` + jsonEsc(keyLower) + `":{"mcpServers":{"lower-loses":{}}}` +
		`}}`

	if canonicalClaudeProjectKey(keyUpper) != canonicalClaudeProjectKey(keyLower) {
		t.Fatalf("test bug: %q and %q do not collide canonically", keyUpper, keyLower)
	}

	for i := 0; i < 16; i++ {
		setSyntheticHome(t, body)
		got, err := ReadClaudeLocalScope(root)
		if err != nil {
			t.Fatalf("iteration %d: read: %v", i, err)
		}
		if !reflect.DeepEqual(got.LocalServers, []string{"upper-wins"}) {
			t.Fatalf("iteration %d: LocalServers = %v, want [upper-wins] (sorted-first raw key %q must win)",
				i, got.LocalServers, keyUpper)
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

// TestReadClaudeLocalScope_SpecialFile_Directory is the security-DoS regression
// guard (codex-bot P2): when ~/.claude.json is NOT a regular file, the reader
// must Lstat-gate it BEFORE the unconditional os.ReadFile so a special live path
// can never block or read unbounded data on /api/projects/scan. A DIRECTORY at
// the path is the cross-platform non-regular case (a FIFO needs mkfifo, absent
// on Windows). The reader must return an EMPTY scope — NOT an error, NOT a hang
// — mirroring the client-config presence gate's skip-on-non-regular verdict.
func TestReadClaudeLocalScope_SpecialFile_Directory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	// Create a DIRECTORY exactly where ~/.claude.json would be a file.
	claudePath := filepath.Join(home, ".claude.json")
	if err := os.Mkdir(claudePath, 0o700); err != nil {
		t.Fatalf("mkdir special-file dir: %v", err)
	}

	got, err := ReadClaudeLocalScope(osRoot(t, "dev", "proj"))
	if err != nil {
		t.Fatalf("directory at ~/.claude.json must NOT error (skip → empty), got: %v", err)
	}
	if got.Matched || len(got.LocalServers) != 0 || len(got.Disabled) != 0 || len(got.Enabled) != 0 {
		t.Errorf("directory at ~/.claude.json must yield an EMPTY scope (special-file skip), got %+v", got)
	}
}

// TestReadClaudeLocalScope_Symlink mirrors the presence-gate symlink policy
// (scan.go probeClientConfigPresence): a symlink-to-regular-file is REFUSED by
// default (→ empty scope) and only honored when the operator opts in via
// MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK on a non-strict host. Strict mode
// (MCPHUB_REQUIRE_SINGLE_USER_HOME=1) keeps the refusal even with the opt-in.
// Skips where the OS/permissions cannot create a symlink (unprivileged Windows).
func TestReadClaudeLocalScope_Symlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Real target: a regular ~/.claude.json body living OUTSIDE the symlink path.
	root := osRoot(t, "dev", "proj")
	key := projectKey("fwd-upper", root)
	body := `{"projects":{"` + jsonEsc(key) + `":{"mcpServers":{"linked":{}}}}}`
	target := filepath.Join(home, "real-claude.json")
	if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	link := filepath.Join(home, ".claude.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink on this host (need privilege/Developer Mode): %v", err)
	}

	// DEFAULT (no opt-in): refuse → empty scope, no error, no hang.
	t.Run("default_refuses", func(t *testing.T) {
		t.Setenv("MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK", "")
		t.Setenv("MCPHUB_REQUIRE_SINGLE_USER_HOME", "")
		got, err := ReadClaudeLocalScope(root)
		if err != nil {
			t.Fatalf("default symlink refusal must NOT error, got: %v", err)
		}
		if got.Matched {
			t.Errorf("default: symlink-to-regular-file must be REFUSED (empty scope), got %+v", got)
		}
	})

	// OPT-IN on a non-strict host: follow the symlink → read the target.
	t.Run("optin_follows", func(t *testing.T) {
		t.Setenv("MCPHUB_REQUIRE_SINGLE_USER_HOME", "")
		t.Setenv("MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK", "1")
		got, err := ReadClaudeLocalScope(root)
		if err != nil {
			t.Fatalf("opt-in symlink read: %v", err)
		}
		if !got.Matched || !reflect.DeepEqual(got.LocalServers, []string{"linked"}) {
			t.Errorf("opt-in: symlink-to-regular-file must be FOLLOWED, got %+v", got)
		}
	})

	// STRICT mode overrides the opt-in: refuse even with the opt-in set.
	t.Run("strict_overrides_optin", func(t *testing.T) {
		t.Setenv("MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK", "1")
		t.Setenv("MCPHUB_REQUIRE_SINGLE_USER_HOME", "1")
		got, err := ReadClaudeLocalScope(root)
		if err != nil {
			t.Fatalf("strict-mode symlink refusal must NOT error, got: %v", err)
		}
		if got.Matched {
			t.Errorf("strict mode must REFUSE the symlink even with the opt-in set, got %+v", got)
		}
	})
}

// TestCanonicalClaudeProjectKey exercises canonicalClaudeProjectKey directly:
//
//   - Windows-gated: C:/dev/Proj, c:/dev/proj, C:\dev\proj all normalize to the
//     SAME key (case-fold + separator normalization; trailing slash trimmed).
//   - POSIX-gated: /dev/Proj and /dev/proj normalize to DIFFERENT keys (POSIX is
//     case-sensitive). Forward-slash paths normalise to themselves; trailing
//     slash trimmed.
//   - Common (both OS): dot/dotdot segments collapse via Clean; empty input → "".
func TestCanonicalClaudeProjectKey(t *testing.T) {
	t.Run("empty_input", func(t *testing.T) {
		if got := canonicalClaudeProjectKey(""); got != "" {
			t.Errorf("empty input: got %q, want %q", got, "")
		}
	})

	t.Run("dot_dotdot_collapse", func(t *testing.T) {
		// filepath.Clean collapses these on any OS
		var input, want string
		if runtime.GOOS == "windows" {
			input = `C:\dev\..\dev\proj`
			want = "c:/dev/proj"
		} else {
			input = "/dev/../dev/proj"
			want = "/dev/proj"
		}
		if got := canonicalClaudeProjectKey(input); got != want {
			t.Errorf("dot/dotdot collapse: got %q, want %q", got, want)
		}
	})

	t.Run("trailing_slash_trimmed", func(t *testing.T) {
		var input, want string
		if runtime.GOOS == "windows" {
			input = `C:\dev\proj\`
			want = "c:/dev/proj"
		} else {
			input = "/dev/proj/"
			want = "/dev/proj"
		}
		if got := canonicalClaudeProjectKey(input); got != want {
			t.Errorf("trailing slash: got %q, want %q", got, want)
		}
	})

	if runtime.GOOS == "windows" {
		t.Run("windows_case_fold_and_sep_normalize", func(t *testing.T) {
			// All three observed key forms must produce the identical canonical key.
			cases := []struct {
				name  string
				input string
			}{
				{"fwd_upper_drive", `C:/dev/Proj`},
				{"fwd_lower_drive", `c:/dev/proj`},
				{"back_upper_drive", `C:\dev\proj`},
				{"mixed_case_segment", `C:/dev/PROJ`},
				{"trailing_slash_fwd", `C:/dev/proj/`},
				{"trailing_slash_back", `C:\dev\proj\`},
			}
			want := "c:/dev/proj"
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					if got := canonicalClaudeProjectKey(c.input); got != want {
						t.Errorf("input %q: got %q, want %q", c.input, got, want)
					}
				})
			}
		})
	} else {
		t.Run("posix_case_preserved_no_collision", func(t *testing.T) {
			// /dev/Proj and /dev/proj MUST NOT produce the same key on POSIX —
			// the filesystem treats them as distinct directories.
			upperKey := canonicalClaudeProjectKey("/dev/Proj")
			lowerKey := canonicalClaudeProjectKey("/dev/proj")
			if upperKey == lowerKey {
				t.Errorf("/dev/Proj and /dev/proj both yielded %q — POSIX paths must NOT be case-folded (case-sensitivity violated)", upperKey)
			}
		})

		t.Run("posix_already_clean_identity", func(t *testing.T) {
			// A clean forward-slash POSIX path normalizes to itself.
			input := "/dev/proj"
			if got := canonicalClaudeProjectKey(input); got != input {
				t.Errorf("clean POSIX path: got %q, want %q", got, input)
			}
		})

		t.Run("posix_case_preserved_value", func(t *testing.T) {
			// The exact canonical form for an uppercase POSIX segment is
			// the same string with forward slashes (no case folding).
			input := "/dev/MyProject"
			if got := canonicalClaudeProjectKey(input); got != input {
				t.Errorf("POSIX uppercase segment: got %q, want %q", got, input)
			}
		})
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

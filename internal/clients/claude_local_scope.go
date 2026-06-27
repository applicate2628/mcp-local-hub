package clients

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// claude_local_scope.go — per-project-GUI Phase 2b.
//
// claude-code has a DUAL per-project substrate (design doc
// work-items/decisions/2026-06-24-per-project-gui-design.md, the "VERIFIED
// client project-scope formats" table):
//
//   - PROJECT scope: <root>/.mcp.json, top-level `mcpServers` — checked-in,
//     shared with the repo. P2a reads this via the ProjectScope registry +
//     ProjectScanConfigPaths.
//   - LOCAL scope: ~/.claude.json → projects.<canonicalKey(root)> — private to
//     the user. Carries that project's own `mcpServers` map (the Local-scope
//     servers) PLUS the `disabledMcpjsonServers` / `enabledMcpjsonServers`
//     []string toggle arrays that gate the PROJECT-scope (.mcp.json) servers.
//
// THIS FILE owns the LOCAL-scope reader. It is strictly READ-ONLY: os.ReadFile
// of ~/.claude.json only — it NEVER writes the file (writes are P3, behind the
// security-reviewer gate). The host's live ~/.claude.json is therefore never
// touched by a scan.
//
// VERIFIED projects.<key> form (probed on the host 2026-06-27, redacted): the
// key is the project's absolute filesystem path, but Claude Code does NOT write
// it in one canonical form — across 21 keys on the dev host the forms were
// `C:/dev/...` (forward-slash + UPPERCASE drive, 13), `c:/dev/...`
// (forward-slash + lowercase drive, 6), and `C:\dev\...` (BACKSLASH + uppercase
// drive, 2). So a single-form assumption would silently miss the divergent
// keys. canonicalClaudeProjectKey normalizes BOTH the project root AND every
// projects.<key> to one comparison form (forward-slash + Clean + case-fold on
// Windows) before matching, so all three observed shapes resolve to the same
// project root robustly.

// ClaudeLocalScope is the read-only projection of one project's claude-code
// LOCAL-scope entry (~/.claude.json → projects.<key>). All fields are empty
// when ~/.claude.json is absent, has no `projects` map, or has no entry whose
// canonicalized key matches the requested root — that is the NORMAL "this
// project has no local-scope config" case, NOT an error.
type ClaudeLocalScope struct {
	// Matched reports whether a projects.<key> entry matched the root. False
	// means no local-scope config exists for this project (or no file at all).
	Matched bool
	// LocalServers is the SORTED list of server names from
	// projects.<key>.mcpServers (the Local-scope server set — distinct from the
	// .mcp.json Project-scope set P2a scans).
	LocalServers []string
	// Disabled is projects.<key>.disabledMcpjsonServers verbatim (the names of
	// .mcp.json Project-scope servers the user has disabled for this project).
	Disabled []string
	// Enabled is projects.<key>.enabledMcpjsonServers verbatim (names the user
	// has explicitly re-enabled, overriding Disabled).
	Enabled []string
}

// IsMcpjsonServerEnabled applies claude-code's enabled/disabled reconciliation
// rule for ONE .mcp.json (Project-scope) server name:
//
//	a server is ENABLED unless it is in disabledMcpjsonServers AND not
//	overridden in enabledMcpjsonServers.
//
// enabledMcpjsonServers wins on conflict (the explicit re-enable). A name in
// neither array is enabled (the default). This is the single owner of the rule
// so the reader and any future consumer can never re-derive it divergently.
func (s ClaudeLocalScope) IsMcpjsonServerEnabled(name string) bool {
	for _, e := range s.Enabled {
		if e == name {
			return true // explicit re-enable wins over a disable entry
		}
	}
	for _, d := range s.Disabled {
		if d == name {
			return false
		}
	}
	return true
}

// ReadClaudeLocalScope reads ~/.claude.json (the same single-file user config
// the production claudeCode adapter binds, resolved via os.UserHomeDir so a
// test's t.Setenv(HOME/USERPROFILE) redirects it to a synthetic file) and
// returns the LOCAL-scope projection for the given project root.
//
// READ-ONLY: os.ReadFile only (via readRawConfig). It NEVER writes
// ~/.claude.json. A missing file, missing `projects` map, or no key matching
// the root all yield an empty ClaudeLocalScope{} with a nil error — the
// "no local-scope config for this project" case is normal, not a failure. An
// error is returned ONLY for a genuine read failure (permission, I/O) or an
// unparseable file.
//
// root is matched against each projects.<key> via canonicalClaudeProjectKey
// (forward-slash + Clean + case-fold on Windows), so a forward-slash root from
// the GUI matches a backslash key (or a mixed-case drive letter) and vice
// versa. root is NOT required to be pre-validated here — it is only used as a
// comparison key against the fixed home file; the path-traversal threat surface
// is the project config files (guarded in ProjectScanConfigPaths), not this
// read of the fixed ~/.claude.json.
func ReadClaudeLocalScope(root string) (ClaudeLocalScope, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return ClaudeLocalScope{}, err
	}
	path := filepath.Join(home, ".claude.json")

	data, err := readRawConfig(path) // absent → (nil, nil)
	if err != nil {
		return ClaudeLocalScope{}, err
	}
	if len(data) == 0 {
		return ClaudeLocalScope{}, nil // no file → no local scope
	}
	m, err := parseJSONCBytes(data)
	if err != nil {
		return ClaudeLocalScope{}, err
	}

	projects, _ := m["projects"].(map[string]any)
	if len(projects) == 0 {
		return ClaudeLocalScope{}, nil // no projects map → no local scope
	}

	wantKey := canonicalClaudeProjectKey(root)
	if wantKey == "" {
		return ClaudeLocalScope{}, nil
	}

	// DETERMINISTIC match. Two raw projects.<key> entries can canonicalize to the
	// same key (e.g. `C:/dev/proj` and `c:/dev/proj` both case-fold to the same
	// key on Windows). Go map iteration order is randomized, so a bare
	// `for k := range projects` would pick a colliding entry non-deterministically
	// — the same scan could surface a different Local-scope set across runs. Rule:
	// iterate the raw keys in SORTED order and take the FIRST whose canonical form
	// matches (first-by-sorted-raw-key on a canonical collision). The pick is
	// stable across runs and across hosts.
	rawKeys := make([]string, 0, len(projects))
	for k := range projects {
		rawKeys = append(rawKeys, k)
	}
	sort.Strings(rawKeys)

	var matchedEntry map[string]any
	for _, k := range rawKeys {
		if canonicalClaudeProjectKey(k) == wantKey {
			matchedEntry, _ = projects[k].(map[string]any)
			break
		}
	}
	if matchedEntry == nil {
		return ClaudeLocalScope{}, nil // no entry for this project
	}

	out := ClaudeLocalScope{Matched: true}

	if servers, ok := matchedEntry["mcpServers"].(map[string]any); ok {
		out.LocalServers = make([]string, 0, len(servers))
		for name := range servers {
			out.LocalServers = append(out.LocalServers, name)
		}
		sort.Strings(out.LocalServers) // deterministic order (map iteration is random)
	}
	out.Disabled = stringSliceFromAny(matchedEntry["disabledMcpjsonServers"])
	out.Enabled = stringSliceFromAny(matchedEntry["enabledMcpjsonServers"])

	return out, nil
}

// stringSliceFromAny coerces a parsed JSON []any of strings into a []string,
// silently dropping any non-string element. A nil/absent/non-array input
// returns nil (so an absent toggle array stays nil, not [] — preserving the
// omitempty wire shape downstream).
func stringSliceFromAny(v any) []string {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// canonicalClaudeProjectKey normalizes an absolute project path (a project root
// OR a raw projects.<key> as written by Claude Code) to ONE comparison form so
// the three observed key shapes (forward-slash+upper-drive, forward-slash+
// lower-drive, backslash+upper-drive) all reduce to the same string for a given
// project. The form is: separator-normalized to the OS native form, Clean'd
// (collapsing `.`/`..`/redundant separators), converted to FORWARD slashes, and
// case-folded on Windows (NTFS is case-insensitive; the dev host showed
// mixed-case drive letters in the keys). On POSIX it is case-sensitive and a
// no-op on separators.
//
// It is deliberately tolerant — it does NOT validate, stat, or resolve symlinks
// (that is ProjectScanConfigPaths' job for the config-file read surface). It is
// purely a string-normalization join key for matching the fixed ~/.claude.json
// against a root. An empty input returns "".
func canonicalClaudeProjectKey(p string) string {
	if p == "" {
		return ""
	}
	// FromSlash makes the separators native (/ → \ on Windows; no-op POSIX) so
	// filepath.Clean collapses segments uniformly regardless of the input's
	// separator style. ToSlash then forces forward slashes for a stable,
	// separator-agnostic comparison string.
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
	cleaned = strings.TrimRight(cleaned, "/") // drop a trailing slash if any survived
	if runtime.GOOS == "windows" {
		cleaned = strings.ToLower(cleaned)
	}
	return cleaned
}

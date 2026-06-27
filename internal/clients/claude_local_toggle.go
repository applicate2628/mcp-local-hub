// claude_local_toggle.go — per-project-GUI Phase 3a.
//
// The claude-code LOCAL-scope WRITE owner: it toggles a .mcp.json
// (Project-scope) server's approval for ONE project by MOVING the server name
// between the projects.<key>.enabledMcpjsonServers and
// projects.<key>.disabledMcpjsonServers arrays in ~/.claude.json.
//
// CATASTROPHIC-CORRUPTION SURFACE — the design's T2. ~/.claude.json holds the
// operator's ENTIRE Claude Code config: top-level keys, dozens of
// projects.<key> entries, and (for hand-editors) JSONC comments. A wrong write
// here corrupts everything. The guarantees this file enforces:
//
//   - NEVER deletes from mcpServers (design decision 5). The toggle is a pure
//     APPROVAL move between two []string arrays. The server DEFINITION in
//     projects.<key>.mcpServers (LOCAL-scope) and the .mcp.json (Project-scope)
//     definition are both left untouched. A disable that deleted the definition
//     would be data-loss.
//   - comment + unknown-key preserving: the byte computation goes through
//     hujson (the SAME applyJSONCObjectMemberPath family the object-member
//     writers use), so every comment, every unrelated top-level key, and every
//     SIBLING projects.<key> entry survive byte-for-byte; only the two toggle
//     arrays under THIS project's key change value.
//   - the projects.<key> nesting is created when absent WITHOUT clobbering
//     siblings (top-down `add` of only the missing intermediates).
//   - the write routes through WriteConfigFile → SecureWriteClientConfig in
//     production (handle-relative, O_NOFOLLOW, atomic temp+rename, DACL-before-
//     bytes — TOCTOU-safe by construction; T1).
//   - the enabled-state PREDICATE is the single owner IsMcpjsonServerEnabled
//     (its precedence rule drives the target membership); the writer never
//     re-derives "what enabled means".
package clients

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tailscale/hujson"
)

// claude-code projects.<key> toggle-array keys. Single owners so the reader
// (ReadClaudeLocalScope) and this writer reference one literal each.
const (
	claudeProjectsSectionKey      = "projects"
	claudeEnabledMcpjsonArrayKey  = "enabledMcpjsonServers"
	claudeDisabledMcpjsonArrayKey = "disabledMcpjsonServers"
)

// claudeJSONPath returns the absolute path to ~/.claude.json, resolved via
// os.UserHomeDir so a test's t.Setenv(HOME/USERPROFILE) redirects it to a
// synthetic file (the live host file is never touched under that redirect). It
// is the SAME resolution ReadClaudeLocalScope uses.
func claudeJSONPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

// matchClaudeProjectRawKey returns the RAW projects.<key> string (as written by
// Claude Code) whose canonical form matches root, plus whether it was found. It
// is the SINGLE owner of the "which projects.<key> entry is this project"
// resolution, shared by the reader and the writer so they can never key off
// different raw keys on a canonical collision.
//
// DETERMINISTIC match: two raw keys can canonicalize to the same key (e.g.
// `C:/dev/proj` and `c:/dev/proj` both case-fold the same on Windows). Go map
// iteration is randomized, so the keys are sorted and the FIRST whose canonical
// form matches wins — stable across runs and hosts. This MIRRORS the reader's
// rule (claude_local_scope.go) exactly.
func matchClaudeProjectRawKey(projects map[string]any, root string) (string, bool) {
	wantKey := CanonicalProjectKey(root)
	if wantKey == "" || len(projects) == 0 {
		return "", false
	}
	rawKeys := make([]string, 0, len(projects))
	for k := range projects {
		rawKeys = append(rawKeys, k)
	}
	sort.Strings(rawKeys)
	for _, k := range rawKeys {
		if CanonicalProjectKey(k) == wantKey {
			return k, true
		}
	}
	return "", false
}

// ToggleClaudeMcpjsonMembership flips a .mcp.json (Project-scope) server's
// approval for the project rooted at `root` by MOVING `server` between the
// enabled/disabled toggle arrays under ~/.claude.json
// projects.<canonicalKey(root)>. It is the B-claude Local write owner
// (design decision 5).
//
// Semantics (driven by the IsMcpjsonServerEnabled precedence rule):
//
//   - enable:  remove `server` from disabledMcpjsonServers (deny wins absolute)
//     AND ensure it is present in enabledMcpjsonServers → guaranteed approved
//     regardless of enableAllProjectMcpServers.
//   - disable: remove `server` from enabledMcpjsonServers AND ensure it is
//     present in disabledMcpjsonServers → guaranteed NOT approved even when
//     enableAllProjectMcpServers is true.
//
// It NEVER touches projects.<key>.mcpServers (no definition delete — decision
// 5) nor any other key/comment/sibling-project. `allowSymlink` is the symlink-
// follow policy INJECTED from above (the single owner
// api.OperatorAllowsClientConfigSymlink); the writer never re-derives it.
//
// Idempotent: when the membership already reflects the requested state, the
// arrays are written back unchanged (whitespace may normalize via Format, but
// the approval result is stable) — the read-back the handler does confirms it.
func ToggleClaudeMcpjsonMembership(root, server string, enable, allowSymlink bool) error {
	if strings.TrimSpace(root) == "" {
		return fmt.Errorf("claude-local toggle: project root is required")
	}
	if strings.TrimSpace(server) == "" {
		return fmt.Errorf("claude-local toggle: server name is required")
	}
	wantKey := CanonicalProjectKey(root)
	if wantKey == "" {
		return fmt.Errorf("claude-local toggle: project root did not normalize to a key")
	}

	path, err := claudeJSONPath()
	if err != nil {
		return err
	}

	// SPECIAL-FILE / symlink gate BEFORE the read — mirror the reader's
	// claudeLocalPathReadable contract so a non-regular / refused-symlink
	// ~/.claude.json is never read or written through. An ABSENT file is the
	// normal "first ever toggle for this user" case and is allowed: the write
	// builds a fresh { "projects": { "<key>": { ... } } } file.
	if !claudeLocalTogglePathWritable(path, allowSymlink) {
		return fmt.Errorf("claude-local toggle: ~/.claude.json is not a writable regular file (a non-regular special file, or a refused symlink)")
	}

	// SERIALIZE THE READ-MODIFY-WRITE (bot PR #433 r3 finding 1): wrap the whole
	// read→compute→write of ~/.claude.json in withConfigLock — the SAME per-path
	// in-process mutex + cross-process advisory flock the adapter decorator
	// (lockingClient) and the object-member toggle (ToggleProjectObjectMember)
	// already serialize their RMWs on. The object-member r2 fix wrapped its own
	// path but MISSED this one, which RMWs the SINGLE shared ~/.claude.json across
	// ALL projects: two concurrent /api/projects/toggle on DIFFERENT claude-local
	// servers (even in different projects) each read the same snapshot, and the
	// later whole-file WriteConfigFile replacement clobbers the earlier array move
	// (lost update). Because ~/.claude.json is ONE path, EVERY claude-local toggle
	// serializes on the same lock key, so no two array moves can torn-write.
	//
	// withConfigLock also runs SecureCreateParentDir on the home dir before the
	// flock — idempotent for the (always-existing) home parent, so this adds the
	// serialization the finding asks for without changing the no-parent-create
	// posture the finding notes (~/.claude.json's parent always exists).
	return withConfigLock(path, func() error {
		original, err := readRawConfig(path) // absent → (nil, nil)
		if err != nil {
			return err
		}

		// Resolve the existing toggle arrays + the raw projects.<key> (when the file
		// and the project entry already exist) so the move is computed against the
		// CURRENT membership. A missing file / projects map / project entry all yield
		// empty arrays + an empty rawKey (a fresh entry under wantKey is created).
		//
		// CRITICAL (corruption-surface defense): the value-read parse runs on a COPY
		// of `original`, NOT the slice itself. parseJSONCBytes → hujson.Standardize
		// MUTATES its input slice in place (it overwrites `//` and `/* */` comment
		// bytes with spaces). If we parsed `original` directly here, the subsequent
		// comment-preserving write (applyClaudeMcpjsonToggleArrays, which re-parses
		// the SAME `original` with hujson.Parse) would see a comment-stripped buffer
		// and silently drop EVERY comment from ~/.claude.json. The copy keeps the
		// write's view of `original` pristine.
		curEnabled, curDisabled, rawKey := currentClaudeToggleArrays(copyBytes(original), root)

		newEnabled, newDisabled := computeMcpjsonToggleMove(server, curEnabled, curDisabled, enable)

		// The raw key to write under: an existing matched raw key (preserve the
		// operator's exact key spelling) or, when none, the canonical key.
		writeKey := rawKey
		if writeKey == "" {
			writeKey = wantKey
		}

		out, err := applyClaudeMcpjsonToggleArrays(original, writeKey, newEnabled, newDisabled)
		if err != nil {
			return err
		}
		return WriteConfigFile(path, out)
	})
}

// copyBytes returns a fresh copy of b (nil-safe). Used to protect a write
// buffer from parseJSONCBytes / hujson.Standardize's in-place comment-stripping
// mutation of its input slice.
func copyBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// currentClaudeToggleArrays parses `original` (~/.claude.json bytes; may be
// empty/absent) and returns the project's current enabled/disabled toggle
// arrays plus the raw projects.<key> that matched `root` (empty when no file /
// no projects map / no matching entry). It NEVER errors on a missing file or a
// missing entry — those are the normal "fresh toggle" cases yielding empty
// arrays. A genuine parse failure is swallowed to empty here (the subsequent
// applyClaudeMcpjsonToggleArrays re-parses with hujson and surfaces a real parse
// error there with full context), so this helper has no error return.
//
// IMPORTANT: callers MUST pass a COPY of the write buffer — parseJSONCBytes
// mutates its input slice in place (hujson.Standardize strips comments to
// spaces). See ToggleClaudeMcpjsonMembership for the rationale.
func currentClaudeToggleArrays(original []byte, root string) (enabled, disabled []string, rawKey string) {
	if len(strings.TrimSpace(string(original))) == 0 {
		return nil, nil, ""
	}
	m, err := parseJSONCBytes(original)
	if err != nil {
		return nil, nil, ""
	}
	projects, _ := m[claudeProjectsSectionKey].(map[string]any)
	if len(projects) == 0 {
		return nil, nil, ""
	}
	k, ok := matchClaudeProjectRawKey(projects, root)
	if !ok {
		return nil, nil, ""
	}
	entry, _ := projects[k].(map[string]any)
	if entry == nil {
		return nil, nil, k
	}
	return stringSliceFromAny(entry[claudeEnabledMcpjsonArrayKey]),
		stringSliceFromAny(entry[claudeDisabledMcpjsonArrayKey]),
		k
}

// computeMcpjsonToggleMove returns the new enabled/disabled arrays after moving
// `server` to the requested side. It de-duplicates and PRESERVES the existing
// order of survivors (appending a newly-added name at the end), so the write is
// minimal and stable. It is a pure function (no I/O) — unit-testable in
// isolation and the single place the move semantics live.
func computeMcpjsonToggleMove(server string, curEnabled, curDisabled []string, enable bool) (newEnabled, newDisabled []string) {
	if enable {
		// Approve: drop from disabled (deny wins absolute), ensure in enabled.
		newDisabled = removeStringPreserveOrder(curDisabled, server)
		newEnabled = ensureStringPresent(curEnabled, server)
		return newEnabled, newDisabled
	}
	// Disapprove: drop from enabled, ensure in disabled (deny wins even if
	// enableAllProjectMcpServers is true).
	newEnabled = removeStringPreserveOrder(curEnabled, server)
	newDisabled = ensureStringPresent(curDisabled, server)
	return newEnabled, newDisabled
}

// removeStringPreserveOrder returns a copy of in with EVERY occurrence of s
// removed, preserving the order of the survivors. A nil/empty input returns nil.
func removeStringPreserveOrder(in []string, s string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v != s {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ensureStringPresent returns in with s appended iff s is not already present,
// preserving order. A nil input with a new s returns a one-element slice.
func ensureStringPresent(in []string, s string) []string {
	for _, v := range in {
		if v == s {
			// Already present — return a copy so callers never alias the input.
			out := make([]string, len(in))
			copy(out, in)
			return out
		}
	}
	out := make([]string, 0, len(in)+1)
	out = append(out, in...)
	out = append(out, s)
	return out
}

// applyClaudeMcpjsonToggleArrays computes the new ~/.claude.json bytes that set
// projects.<rawKey>.{enabled,disabled}McpjsonServers to newEnabled/newDisabled,
// preserving EVERY comment, unrelated top-level key, sibling projects.<key>
// entry, and the project's own mcpServers/other fields. The ONLY values that
// change are the two toggle arrays under THIS project's key.
//
//   - When `original` is empty/absent, a fresh clean-indented
//     { "projects": { "<rawKey>": { ...arrays... } } } is built (omitting an
//     empty array entirely, matching the reader's nil-on-absent contract).
//   - Otherwise the mutation is expressed as RFC-6902 ops on the hujson AST:
//     the projects + projects.<rawKey> intermediates are `add`ed top-down only
//     when missing (never clobbering siblings); each toggle array is `add`ed
//     when its new value is non-empty (creates or replaces), or `remove`d when
//     the new value is empty AND it currently exists (so an emptied array does
//     not leave `[]` clutter and round-trips to the reader's nil). hujson
//     preserves the comments/whitespace of everything else.
//
// rawKey is the EXACT projects key to write under (an existing matched raw key,
// or the canonical key for a fresh entry) — never re-canonicalized here, so a
// pre-existing entry's spelling is preserved.
func applyClaudeMcpjsonToggleArrays(original []byte, rawKey string, newEnabled, newDisabled []string) ([]byte, error) {
	projectPtr := "/" + jsonPointerEscape(claudeProjectsSectionKey) + "/" + jsonPointerEscape(rawKey)
	enabledPtr := projectPtr + "/" + jsonPointerEscape(claudeEnabledMcpjsonArrayKey)
	disabledPtr := projectPtr + "/" + jsonPointerEscape(claudeDisabledMcpjsonArrayKey)

	if len(strings.TrimSpace(string(original))) == 0 {
		// Fresh file: build only the project entry with the non-empty arrays.
		entry := map[string]any{}
		if len(newEnabled) > 0 {
			entry[claudeEnabledMcpjsonArrayKey] = newEnabled
		}
		if len(newDisabled) > 0 {
			entry[claudeDisabledMcpjsonArrayKey] = newDisabled
		}
		fresh := map[string]any{
			claudeProjectsSectionKey: map[string]any{rawKey: entry},
		}
		out, err := json.MarshalIndent(fresh, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(out, '\n'), nil
	}

	v, err := hujson.Parse(original)
	if err != nil {
		return nil, fmt.Errorf("parse ~/.claude.json: %w", err)
	}

	var ops []map[string]any
	// Ensure projects + projects.<rawKey> objects exist (top-down), creating
	// only the MISSING intermediates so existing siblings are never replaced.
	projectsPtr := "/" + jsonPointerEscape(claudeProjectsSectionKey)
	if v.Find(projectsPtr) == nil {
		ops = append(ops, map[string]any{"op": "add", "path": projectsPtr, "value": map[string]any{}})
	}
	if v.Find(projectPtr) == nil {
		ops = append(ops, map[string]any{"op": "add", "path": projectPtr, "value": map[string]any{}})
	}

	// Each toggle array: add (create/replace) when non-empty; remove when empty
	// AND present; no-op when empty AND absent.
	ops = append(ops, toggleArrayOps(v, enabledPtr, newEnabled)...)
	ops = append(ops, toggleArrayOps(v, disabledPtr, newDisabled)...)

	if len(ops) == 0 {
		// Nothing to change (both arrays already empty/absent). Return the
		// re-packed original unchanged.
		return v.Pack(), nil
	}
	if err := patchValue(&v, ops); err != nil {
		return nil, fmt.Errorf("rewrite ~/.claude.json toggle arrays: %w", err)
	}
	v.Format()
	return v.Pack(), nil
}

// toggleArrayOps returns the RFC-6902 op(s) to bring the array at ptr to
// `value`: an `add` (create or replace) when value is non-empty; a `remove`
// when value is empty AND the array currently exists; nothing when value is
// empty AND the array is absent.
func toggleArrayOps(v hujson.Value, ptr string, value []string) []map[string]any {
	if len(value) > 0 {
		return []map[string]any{{"op": "add", "path": ptr, "value": value}}
	}
	if v.Find(ptr) != nil {
		return []map[string]any{{"op": "remove", "path": ptr}}
	}
	return nil
}

// claudeLocalTogglePathWritable mirrors claudeLocalPathReadable (the reader's
// special-file/symlink gate) for the WRITE path: an ABSENT file is allowed (the
// normal first-toggle case — the write creates a fresh file), a regular file is
// allowed, a non-regular special file is refused, and a symlink is refused
// UNLESS the injected allowSymlink policy is true AND it resolves to a regular
// file. This keeps the read of ~/.claude.json (in ToggleClaudeMcpjsonMembership)
// and the subsequent SecureWriteClientConfig rename from following a refused
// symlink. The injected policy is the single owner the reader also honors.
func claudeLocalTogglePathWritable(path string, allowSymlink bool) bool {
	lst, lerr := os.Lstat(path)
	if lerr != nil {
		// Missing → allowed (fresh-file create). Any other Lstat fault → refuse
		// (do not write through a faulty/uncertain path).
		return os.IsNotExist(lerr)
	}
	isSymlink := lst.Mode()&os.ModeSymlink != 0
	if !lst.Mode().IsRegular() && !isSymlink {
		return false // directory / FIFO / device / junction
	}
	if isSymlink {
		if allowSymlink {
			if rst, rstErr := os.Stat(path); rstErr == nil && rst.Mode().IsRegular() {
				return true
			}
		}
		return false
	}
	return true // regular file
}

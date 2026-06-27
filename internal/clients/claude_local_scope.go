package clients

import (
	"os"
	"path/filepath"
	"sort"
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
	// Enabled is projects.<key>.enabledMcpjsonServers verbatim (the user's
	// explicit per-server APPROVE list).
	Enabled []string
	// EnableAll is projects.<key>.enableAllProjectMcpServers — the project-wide
	// "approve every .mcp.json server" flag. When true, an unlisted server (in
	// neither Enabled nor Disabled) is APPROVED; when false (the default,
	// including absent/non-bool), an unlisted server is PENDING = NOT approved.
	// Disabled always wins over EnableAll (deny is absolute).
	EnableAll bool
}

// IsMcpjsonServerEnabled applies claude-code's .mcp.json (Project-scope)
// approval reconciliation rule for ONE server name. Claude's model is OPT-IN: a
// .mcp.json server is NOT loaded until the user approves it (verified against the
// Claude Code settings docs — `enableAllProjectMcpServers`,
// `enabledMcpjsonServers`, `disabledMcpjsonServers`). An unlisted server with no
// approve-all is PENDING the trust prompt, i.e. NOT enabled.
//
// TOTAL precedence (highest to lowest):
//
//  1. name in disabledMcpjsonServers → FALSE (deny wins, absolute — overrides
//     both an explicit enable entry and enableAll).
//  2. else name in enabledMcpjsonServers → TRUE (explicit per-server approve).
//  3. else EnableAll (enableAllProjectMcpServers) → TRUE (project-wide approve).
//  4. else → FALSE (unlisted + no approve-all = un-approved / pending — the
//     opt-IN default).
//
// This is the single owner of the rule so the reader and any future consumer can
// never re-derive it divergently.
func (s ClaudeLocalScope) IsMcpjsonServerEnabled(name string) bool {
	for _, d := range s.Disabled {
		if d == name {
			return false // deny wins, absolute
		}
	}
	for _, e := range s.Enabled {
		if e == name {
			return true // explicit per-server approve
		}
	}
	if s.EnableAll {
		return true // project-wide approve-all
	}
	return false // unlisted + no approve-all → un-approved (opt-IN default)
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
//
// allowSymlink is the symlink-follow policy INJECTED FROM ABOVE — the SINGLE
// owner is api.OperatorAllowsClientConfigSymlink (the canonical predicate the
// client-config presence gate uses), which honors BOTH the env vars
// (MCPHUB_REQUIRE_SINGLE_USER_HOME strict-override + MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK
// opt-in) AND the persisted supervisor-intent strict_mode bit. This package
// cannot import internal/api (cyclic), so the caller computes the policy and
// passes the resolved bool — the reader never re-derives it (no local env read),
// guaranteeing the presence gate and the local reader can never diverge. When
// true, a symlink-to-regular-file ~/.claude.json is followed; when false, a
// symlink is refused (→ empty scope).
func ReadClaudeLocalScope(root string, allowSymlink bool) (ClaudeLocalScope, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return ClaudeLocalScope{}, err
	}
	path := filepath.Join(home, ".claude.json")

	// SPECIAL-FILE / DoS gate (mirrors the client-config PRESENCE policy in
	// internal/api/scan.go probeClientConfigPresence:226-268). readRawConfig
	// below does an UNCONDITIONAL os.ReadFile, which — if ~/.claude.json is NOT
	// a regular file (a FIFO, device, named pipe, or a symlink to a stream) —
	// can BLOCK or read unbounded data on EVERY /api/projects/scan, hanging /
	// DoS-ing the Projects scan. So we Lstat-classify the path FIRST and read
	// ONLY a regular file, applying the SAME accept/reject the presence gate
	// uses:
	//
	//   - missing / Lstat error → empty scope (the normal "no local config"
	//     case; readRawConfig already coerces a missing file to empty, but we
	//     also treat any Lstat failure as "skip" so a transient/perm fault
	//     never falls through to an unconditional ReadFile).
	//   - non-regular AND non-symlink (directory / FIFO / device / junction) →
	//     skip → empty scope (presence gate's "error" verdict; here we never
	//     surface an error for the home file — a malformed live path simply
	//     yields no local scope, never a hung scan).
	//   - symlink → REFUSE (skip → empty scope) UNLESS the INJECTED allowSymlink
	//     policy is true AND the symlink resolves to a regular file — the SAME
	//     policy the presence gate honors. The policy is computed by the single
	//     owner (api.OperatorAllowsClientConfigSymlink) and passed in, so the
	//     local reader never re-derives it and cannot diverge from the presence
	//     gate (it honors both the env vars AND the persisted strict_mode bit).
	//   - regular file → read (the normal path).
	if !claudeLocalPathReadable(path, allowSymlink) {
		return ClaudeLocalScope{}, nil
	}

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

	// DETERMINISTIC first-by-sorted-raw-key match (handles canonical collisions
	// like `C:/dev/proj` vs `c:/dev/proj`). The match rule is the SINGLE owner
	// matchClaudeProjectRawKey (claude_local_toggle.go), shared with the writer
	// so reader and writer can never key off different raw entries.
	rawKey, ok := matchClaudeProjectRawKey(projects, root)
	if !ok {
		return ClaudeLocalScope{}, nil // no entry for this project (or empty root)
	}
	matchedEntry, _ := projects[rawKey].(map[string]any)
	if matchedEntry == nil {
		return ClaudeLocalScope{}, nil // entry present but not an object
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
	// enableAllProjectMcpServers is the project-wide approve-all flag; absent or
	// non-bool coerces to false (the opt-IN default — no blanket approval).
	out.EnableAll, _ = matchedEntry["enableAllProjectMcpServers"].(bool)

	return out, nil
}

// claudeLocalPathReadable reports whether path (~/.claude.json) is safe to
// os.ReadFile, mirroring the client-config PRESENCE gate
// (internal/api/scan.go probeClientConfigPresence:226-268). It returns false
// — meaning "skip, treat as empty local scope, do NOT read" — for a missing /
// unstatable path, a non-regular special file (directory / FIFO / device /
// junction), or a refused symlink. It returns true ONLY for a regular file, or
// for a symlink-to-regular-file when allowSymlink (injected from above by the
// single policy owner) is true (the same accept the presence gate grants).
//
// allowSymlink is the resolved symlink-follow policy passed down from
// ReadClaudeLocalScope's caller (api.OperatorAllowsClientConfigSymlink); this
// gate never re-derives it from the environment.
//
// READ-ONLY: it only Lstat/Stat-s the path; it never opens or writes it. A
// missing file is the common "no local-scope config" case, so it returns false
// silently (the caller maps that to an empty ClaudeLocalScope, never an error).
func claudeLocalPathReadable(path string, allowSymlink bool) bool {
	lst, lerr := os.Lstat(path)
	if lerr != nil {
		// Missing → normal no-config case. Any other Lstat fault → still skip
		// (never fall through to an unconditional ReadFile on a faulty path).
		return false
	}
	isSymlink := lst.Mode()&os.ModeSymlink != 0
	if !lst.Mode().IsRegular() && !isSymlink {
		// Directory / FIFO / device / junction — the DoS surface this gate
		// closes. Presence gate returns "error"; here we skip → empty scope.
		return false
	}
	if isSymlink {
		// Presence-gate symlink policy: refuse UNLESS the injected allowSymlink
		// policy is true AND the link resolves to a regular file. os.Stat
		// (kernel-level symlink follow) classifies the target, matching
		// probeClientConfigPresence's use of os.Stat over filepath.EvalSymlinks.
		if allowSymlink {
			if rst, rstErr := os.Stat(path); rstErr == nil && rst.Mode().IsRegular() {
				return true
			}
		}
		return false
	}
	return true // regular file
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

// canonicalClaudeProjectKey is the claude-code-specific caller of the SINGLE
// general join-key owner CanonicalProjectKey (canonical_project_key.go). It
// matches a project root (or a raw ~/.claude.json projects.<key> as written by
// Claude Code) against the three observed key shapes (forward-slash+upper-drive,
// forward-slash+lower-drive, backslash+upper-drive) by reducing both sides to
// the same normalized comparison string.
//
// It is a thin alias — NOT a second normalizer — so the claude-local reader and
// the P3a per-project A+B+C composition can never re-derive the join form
// divergently (P3 design decision 4 + T2 "no 4th normalizer"). All semantics
// (FromSlash/Clean/ToSlash/trailing-slash-trim/Windows case-fold, the
// absolute-path caller contract, no stat/symlink resolution) live in
// CanonicalProjectKey.
func canonicalClaudeProjectKey(p string) string {
	return CanonicalProjectKey(p)
}

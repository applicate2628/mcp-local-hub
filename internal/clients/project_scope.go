package clients

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ProjectScope is one row of the per-project (Model B) config-scope registry:
// for a supported client, WHERE under a project root its project-local MCP
// config lives (RelFile) and WHICH top-level JSON key holds the server map
// (SectionKey).
//
// This is a DATA registry — deliberately NOT a new method on the Client
// interface. The accepted design
// (work-items/decisions/2026-06-24-per-project-gui-design.md, decision 2)
// rejects a Client.ScopedConfigPath() method because that would force editing
// all 46 adapters; the registry feeds the EXISTING path-parameterized scan
// without touching the Client interface or any adapter.
//
// SectionKey is documentary / cross-check only here. The actual parse of a
// project config file is done by the EXISTING per-client scanner in
// internal/api/scan.go (scanClaude / scanCursor / scanVSCode), each of which
// already owns its section key. We supply only the per-client PATH; the
// scanner that runs against that path uses its own (verified-matching) key. A
// QA golden/cross-check test pins SectionKey against the adapter constants so
// the two can never silently diverge.
type ProjectScope struct {
	// Client is the registry client id (matches SupportedClientNames()).
	Client string
	// RelFile is the project-relative config path, joined onto the project
	// root. It is a FIXED CONSTANT, never derived from caller input — that is
	// the load-bearing property for the path-traversal threat model (see
	// ProjectScanConfigPaths).
	RelFile string
	// SectionKey is the top-level JSON key holding the server map in RelFile.
	SectionKey string
	// Supported reports whether this client has a project-local config scope.
	Supported bool
}

// projectScopeRegistry is the single owner of the per-client project-scope
// formats. VERIFIED against the adapters (cited file:line below) and the
// design's "VERIFIED client project-scope formats" table:
//
//   - claude-code: <root>/.mcp.json, top-level `mcpServers`
//     (claudeCodeMCPServersKey = "mcpServers", claude_code.go:28). This is the
//     repo-root PROJECT scope substrate. The ~/.claude.json nested
//     projects."<key>".mcpServers LOCAL scope is a SEPARATE substrate deferred
//     to P2b — NOT read here.
//   - cursor: <root>/.cursor/mcp.json, top-level `mcpServers`
//     (cursorClient is a jsonMCPClient over the canonical `mcpServers` shape;
//     scanCursor parses json:"mcpServers", scan.go:1007). Pure path-reparam of
//     the same JSON shape, relocated from ~/.cursor/mcp.json to the repo root.
//   - vscode: <root>/.vscode/mcp.json, top-level `servers` (NOT mcpServers!)
//     (vscodeServersKey = "servers", vscode.go:32; scanVSCode parses
//     json:"servers", scan.go:1040). Pure path-reparam, but a DIFFERENT key.
//
// Clients without a project-local scope are simply absent from this registry
// (equivalently Supported:false) and never contribute a project config path.
var projectScopeRegistry = []ProjectScope{
	// claude-code reads the .mcp.json Project scope and reports every server in
	// its `mcpServers` as CONFIGURED. The enabled/disabledMcpjsonServers
	// reconciliation lives in the ~/.claude.json Local scope (a SEPARATE
	// substrate) and is deferred to P2b per the design doc — a known scoped
	// boundary, not a defect.
	{Client: "claude-code", RelFile: ".mcp.json", SectionKey: "mcpServers", Supported: true},
	{Client: "cursor", RelFile: filepath.Join(".cursor", "mcp.json"), SectionKey: "mcpServers", Supported: true},
	{Client: "vscode", RelFile: filepath.Join(".vscode", "mcp.json"), SectionKey: "servers", Supported: true},
}

// ProjectScopes returns the supported per-client project-scope rows in stable
// registry order. Exposed for cross-check tests and any caller that needs the
// declarative format set without resolving paths.
func ProjectScopes() []ProjectScope {
	out := make([]ProjectScope, 0, len(projectScopeRegistry))
	for _, ps := range projectScopeRegistry {
		if ps.Supported {
			out = append(out, ps)
		}
	}
	return out
}

// ProjectScanConfigPaths maps a project root + the ProjectScope registry to a
// per-client client-id → absolute config-path map, ready to hand to
// api.ScanFrom(api.ScanOpts{ConfigPaths: ...}). It performs NO I/O on the
// per-client files (it only validates the root); a missing per-client file is
// the scanner's job to absorb (os.IsNotExist → no entries), so a project that
// has only a .cursor/mcp.json still returns paths for all three clients and the
// scanner reports the absent ones as having no servers.
//
// # Threat model (the security surface for P2a)
//
// `root` is UNTRUSTED — it arrives from the GUI's GET /api/projects/scan?root=
// HTTP query parameter, i.e. attacker-influenceable input. Project-path
// traversal on that param is the headline risk this function defends against.
// The defenses:
//
//   - root MUST be an ABSOLUTE path. A relative path (or one that does not
//     survive a Clean+Abs round-trip equality check) is rejected. Requiring
//     absolute denies "use my CWD as the base" ambiguity and denies bare
//     "../../etc" style relative escapes outright.
//   - root is filepath.Clean'd. A path that is not already in clean form (e.g.
//     embeds `..` or redundant separators) fails the round-trip equality and
//     is rejected, so a caller cannot smuggle traversal segments through a
//     not-yet-collapsed path.
//   - root MUST stat as an existing DIRECTORY. A non-existent path, a file, or
//     a special node is rejected before any per-client path is built — no read
//     happens against a bogus root.
//   - SYMLINK containment: the root is resolved with filepath.EvalSymlinks and
//     re-checked to be a directory, so a symlinked root that ultimately points
//     at a directory is allowed but resolves to its real target; a root that
//     resolves to a non-directory is rejected. Per-client files are then joined
//     onto the RESOLVED real root.
//   - The per-client join is filepath.Join(cleanRoot, ps.RelFile) where
//     ps.RelFile is a FIXED CONSTANT from the registry above, NEVER from input.
//     With a fixed, traversal-free relative component and an
//     already-validated clean+absolute+real root, no join can escape the root.
//   - INTERMEDIATE-COMPONENT symlink containment: the root-level EvalSymlinks
//     above only proves the ROOT itself does not redirect outside its real
//     target. It does NOT catch a symlink created at an INTERMEDIATE path
//     component INSIDE the valid root — e.g. `<root>/.cursor -> /other/dir`.
//     The downstream scanner only Lstats the FINAL config file, so os.ReadFile
//     would FOLLOW the `.cursor` link and read `/other/dir/mcp.json`, OUTSIDE
//     realRoot. To close that, each candidate's PARENT directory is resolved
//     with filepath.EvalSymlinks (when it exists) and the resolved parent is
//     re-checked to be contained within realRoot (rootContainsPath). A client
//     whose parent escapes realRoot is DROPPED from the returned map — never
//     read. A missing parent (the common no-config case) is NOT an escape:
//     the candidate stays in, and the scanner absorbs the absent file. A
//     partial result is correct: the other clients still scan.
//
// On any rejection it returns a non-nil error whose message is SAFE TO SHOW —
// it never echoes the caller's raw root path nor any resolved internal path, so
// the HTTP handler can surface it without leaking filesystem layout (the
// handler still maps it to a 400 with a generic body; see the gui handler).
func ProjectScanConfigPaths(root string) (map[string]string, error) {
	if root == "" {
		return nil, fmt.Errorf("project root is required")
	}
	// Separator normalization BEFORE the IsAbs + clean round-trip guard. The
	// P1 Projects frontend canonicalizes roots with FORWARD slashes
	// (`C:/dev/proj`), but on Windows filepath.Clean produces BACKSLASHES, so a
	// forward-slash Windows root would fail the round-trip equality guard below
	// and the endpoint would reject the frontend's OWN roots
	// (PROJECT_ROOT_INVALID), breaking the feature end-to-end. FromSlash
	// converts `/`→`\` on Windows and is a NO-OP on POSIX (so a POSIX
	// `/home/proj` is unaffected). The guard's security property is PRESERVED:
	// FromSlash only swaps the separator, it does NOT collapse `..` segments —
	// `C:/dev/../etc` → FromSlash → `C:\dev\..\etc` → Clean → `C:\etc` ≠
	// normalized input → still REJECTED by the round-trip guard.
	root = filepath.FromSlash(root)
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("project root must be an absolute path")
	}
	// Clean + round-trip equality: reject any root that is not already in
	// canonical clean form (embeds `..`, `.`, or redundant separators). This
	// denies traversal segments smuggled through a not-yet-collapsed path.
	cleanRoot := filepath.Clean(root)
	if cleanRoot != root {
		return nil, fmt.Errorf("project root must be a clean absolute path")
	}

	st, err := os.Stat(cleanRoot)
	if err != nil {
		// Do NOT wrap err — an *os.PathError embeds the absolute path, which
		// would leak the (attacker-supplied, but possibly host-revealing) root
		// back to the caller. A flat message keeps it leak-safe.
		return nil, fmt.Errorf("project root does not exist or is not accessible")
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("project root is not a directory")
	}

	// Symlink containment: resolve the real target and re-validate it is a
	// directory. EvalSymlinks fails closed (a broken/looping link errors). The
	// resolved path becomes the join base, so per-client files are read from
	// the real directory, not through a redirecting link.
	realRoot, err := filepath.EvalSymlinks(cleanRoot)
	if err != nil {
		return nil, fmt.Errorf("project root could not be resolved")
	}
	rst, err := os.Stat(realRoot)
	if err != nil || !rst.IsDir() {
		return nil, fmt.Errorf("project root does not resolve to a directory")
	}

	out := map[string]string{}
	for _, ps := range projectScopeRegistry {
		if !ps.Supported {
			continue
		}
		// ps.RelFile is a fixed registry constant (never input). Joined onto a
		// validated clean+absolute+real-directory root, the LEXICAL result
		// cannot escape realRoot.
		candidate := filepath.Join(realRoot, ps.RelFile)

		// Intermediate-component symlink defense: resolve the candidate's
		// PARENT dir and verify it is still contained within realRoot. This
		// catches `<root>/.cursor -> /other/dir`, which a lexical join cannot.
		// DROP (do not read) a client whose parent escapes; a partial result
		// is correct.
		if !parentContainedInRoot(realRoot, candidate) {
			continue
		}

		// FINAL-FILE symlink defense (independent of the parent check above):
		// the candidate config FILE may ITSELF be a symlink whose target is a
		// regular file OUTSIDE realRoot — e.g. `<root>/.cursor/mcp.json ->
		// /outside/mcp.json`. The downstream presence gate only Lstats the
		// final file and reports "ok" for a symlink-to-regular-file when the
		// operator has opted in via MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK on a
		// non-strict host (scan.go probeClientConfigPresence), after which
		// os.ReadFile FOLLOWS the link and reads the outside target — an escape
		// the parent check cannot see (the parent .cursor is in-root; only the
		// leaf link escapes). Resolving the final candidate here makes the
		// clients-package contract SELF-CONTAINED: the project scan never
		// inherits that opt-in symlink-following regardless of the presence
		// gate's posture. A NON-EXISTENT candidate is the common no-config case
		// — keep it (nothing is read; the presence gate skips an absent file).
		// Any OTHER resolution failure fails CLOSED (drop).
		if !finalFileContainedInRoot(realRoot, candidate) {
			continue
		}
		out[ps.Client] = candidate
	}
	return out, nil
}

// parentContainedInRoot reports whether the PARENT directory of candidate,
// after symlink resolution, is still contained within realRoot. realRoot is
// the already-symlink-resolved project root (the EvalSymlinks output above).
//
// The parent — not the final file — is the symlink surface that matters: the
// downstream scanner Lstats the final config file but os.ReadFile follows a
// symlinked intermediate DIRECTORY (e.g. a `.cursor` link). So we resolve the
// parent and require it to live inside realRoot.
//
// A MISSING parent is NOT an escape (the overwhelmingly common no-config
// case): the candidate is kept and the scanner absorbs the absent file. A
// parent that EXISTS but cannot be resolved (EvalSymlinks error other than
// not-exist) fails CLOSED — treated as an escape and dropped. A resolved
// parent that escapes realRoot is an escape.
func parentContainedInRoot(realRoot, candidate string) bool {
	parent := filepath.Dir(candidate)

	// Fast path: the parent IS realRoot itself (e.g. <root>/.mcp.json). realRoot
	// is already resolved + validated, so no per-candidate resolution is needed.
	if pathsEqual(parent, realRoot) {
		return true
	}

	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		// Missing parent → keep (scanner absorbs the absent file). Any other
		// resolution failure → fail closed (drop).
		return os.IsNotExist(err)
	}
	return rootContainsPath(realRoot, resolvedParent)
}

// finalFileContainedInRoot reports whether the candidate config FILE, after
// symlink resolution, is still contained within realRoot. realRoot is the
// already-symlink-resolved project root.
//
// Unlike parentContainedInRoot (which guards an intermediate symlinked
// DIRECTORY), this guards the LEAF: a `<root>/.cursor/mcp.json` that is itself
// a symlink to an OUTSIDE regular file. The downstream presence gate
// (scan.go probeClientConfigPresence) reports such a leaf as "ok" under the
// MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK opt-in, after which the scanner's
// os.ReadFile follows it outside realRoot. Resolving the leaf here keeps the
// project scan from inheriting that opt-in symlink-following.
//
// A NON-EXISTENT candidate is NOT an escape (the overwhelmingly common
// no-config case): the candidate is kept and the presence gate skips the
// absent file. A candidate that EXISTS but cannot be resolved (EvalSymlinks
// error other than not-exist — e.g. a looping/broken link) fails CLOSED
// (dropped). A resolved candidate that escapes realRoot is an escape.
func finalFileContainedInRoot(realRoot, candidate string) bool {
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		// Missing leaf → keep (no read happens). Any other resolution failure
		// → fail closed (drop).
		return os.IsNotExist(err)
	}
	return rootContainsPath(realRoot, resolved)
}

// rootContainsPath reports whether candidate is equal to, or a true
// subdirectory of, root. Both MUST already be absolute + symlink-resolved.
//
// Separator-aware (NOT a bare string prefix) so "/dev" does not match
// "/developer" and "C:\\proj" does not match "C:\\project2". Comparison is
// case-insensitive on Windows (NTFS is case-preserving but case-insensitive),
// case-sensitive elsewhere — mirroring the repo convention (api.rootContains
// in lsp_trusted_roots.go; install.go image comparison) of strings.EqualFold
// for Windows path comparison. It is a clients-package local copy because
// project_scope.go (clients) cannot import internal/api.
func rootContainsPath(root, candidate string) bool {
	if root == "" || candidate == "" {
		return false
	}
	eq := func(a, b string) bool { return a == b }
	hasPrefix := strings.HasPrefix
	if runtime.GOOS == "windows" {
		eq = strings.EqualFold
		hasPrefix = func(s, prefix string) bool {
			return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
		}
	}
	if eq(root, candidate) {
		return true
	}
	prefix := strings.TrimRight(root, string(os.PathSeparator))
	if prefix == "" {
		// root was only separators (POSIX "/"): everything is under it.
		return hasPrefix(candidate, string(os.PathSeparator))
	}
	return hasPrefix(candidate, prefix+string(os.PathSeparator))
}

// pathsEqual compares two absolute paths with the same OS-aware case folding
// as rootContainsPath.
func pathsEqual(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

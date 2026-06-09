// lsp_trusted_roots.go — the authorization boundary for the GUI LSP
// router's first-touch auto-register path.
//
// THREAT (the vulnerability this file closes). Before this gate,
// internal/gui/lsp_router.go's workspaceFromResolvedLSPPath would
// AUTO-REGISTER a brand-new supervised LSP daemon for ANY local
// directory an UNTRUSTED MCP `tools/call` named, as long as that
// directory happened to carry a language project marker (go.mod,
// package.json, pyproject.toml, …). Project markers are extremely
// common in arbitrary directories, so a malicious or compromised MCP
// client could spawn a supervised daemon rooted at an attacker-chosen
// path (resource-exhaustion + arbitrary-local-path process spawn,
// localhost-scoped but real). PR #269 closed it bluntly by deleting
// auto-register entirely; the operator rejected that because it kills
// the out-of-the-box convenience that PR #266 shipped.
//
// THE DECIDED DESIGN — trusted-root containment.
//
//   - A "trusted root" is either an operator-configured allowed root
//     path OR the canonical WorkspaceRoot of a workspace that was
//     registered through an EXPLICIT operator action (the `mcphub
//     register` CLI command or the GUI "Enable" handler). The latter
//     is "auto-bless on first explicit register" — see
//     BlessTrustedRoot below; it is invoked ONLY at the explicit
//     register call sites, NEVER from the router's auto-register seam.
//
//   - The router auto-registers a resolved workspace ONLY IF its
//     canonical WorkspaceRoot equals, or is a true subdirectory of,
//     some trusted root (LSPWorkspaceRootTrusted). Otherwise it
//     refuses exactly as PR #269 did.
//
//   - Net effect: the FIRST workspace under any tree must be
//     registered explicitly (that seeds trust for the tree), after
//     which sibling/child workspaces under that blessed root
//     auto-register transparently. The operator can also pre-trust a
//     broad root by hand-editing lsp-trusted-roots.json. An
//     attacker-named arbitrary path with no trusted-root ancestor is
//     refused.
//
// OPERATOR SURFACE. The store is a single owner-only state-dir file:
//
//	<state-dir>/lsp-trusted-roots.json
//	{ "version": 1, "roots": ["<canonical-abs-path>", ...] }
//
// It is operator-editable. A missing file means "no trusted roots"
// (every first-touch auto-register is refused until the first explicit
// register or a hand-added root). Writes go through the hardened
// state-file pipeline (WriteStateFileBytesAtomic → SecureWriteClientConfig
// chain); reads tolerate an absent file and apply the same parent-DACL
// read gate the supervisor-intent reader applies.
//
// This file supersedes PR #269 (codex/fix-lsp-router-vulnerability):
// same refusal for untrusted paths, but trusted trees keep the
// auto-register convenience.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/gofrs/flock"
)

// LSPTrustedRootsFileLeaf is the canonical basename of the trusted-roots
// store under the per-user state directory.
const LSPTrustedRootsFileLeaf = "lsp-trusted-roots.json"

// lspTrustedRootsVersion is the on-disk schema version. Bumped only on a
// breaking shape change; additive fields do not require a bump.
const lspTrustedRootsVersion = 1

// LSPTrustedRootsFile is the on-disk shape of lsp-trusted-roots.json.
//
// Roots holds canonical (filepath.Abs + EvalSymlinks + Clean, Windows
// drive-letter lowercased) absolute paths. Both operator-hand-added
// roots and auto-blessed explicit-register roots live in this one slice
// and are treated identically by the containment check — the union is
// the file itself.
type LSPTrustedRootsFile struct {
	Version int      `json:"version"`
	Roots   []string `json:"roots"`
}

// DefaultLSPTrustedRootsPath returns the absolute path of
// lsp-trusted-roots.json under the resolved per-user state directory.
// Honors the daemonStateRootOverride test seam
// (api.SetDaemonStateRootForTest) so cross-package tests redirect it
// without env vars, mirroring DefaultSupervisorIntentPath.
func DefaultLSPTrustedRootsPath() (string, error) {
	stateDir, err := DaemonStateDir()
	if err != nil {
		return "", err
	}
	return joinStateFilePath(stateDir, LSPTrustedRootsFileLeaf), nil
}

// lspTrustedRootsLockSuffix is the flock leaf suffix for the
// read-modify-write of the trusted-roots store. Mirrors the
// supervisor-intent flock convention (path + suffix).
const lspTrustedRootsLockSuffix = ".lock"

// canonicalizeTrustedRoot resolves p to the same canonical form used as
// a WorkspaceRoot key everywhere else (CanonicalWorkspacePath /
// CanonicalWorkspacePathForCleanup): filepath.Abs(filepath.Clean(p)),
// then filepath.EvalSymlinks over the full path so aliased parents do
// not produce two distinct keys for the same directory, then Windows
// drive-letter lowercasing so "C:\proj" and "c:\proj" unify.
//
// Unlike CanonicalWorkspacePath this does NOT require the directory to
// exist: a trusted root may legitimately name a not-yet-created or
// temporarily-unavailable tree (an operator pre-trusting a drive that
// is not mounted, or a registered workspace whose dir was removed). When
// EvalSymlinks cannot resolve (path or a parent is gone), it falls back
// to the best-effort symlink resolution used by the cleanup path so the
// stored key still matches what an explicit register persisted. This is
// the same forgiving convention CanonicalWorkspacePathForCleanup uses.
func canonicalizeTrustedRoot(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("trusted root: empty path")
	}
	abs, err := filepath.Abs(filepath.Clean(p))
	if err != nil {
		return "", fmt.Errorf("trusted root %q: %w", p, err)
	}
	// EvalSymlinks when possible (matches CanonicalWorkspacePath); fall
	// back to the best-effort walk for gone/aliased components so the
	// key matches the register-time canonical form even when the dir is
	// not currently resolvable.
	abs = resolveSymlinksBestEffort(abs)
	if runtime.GOOS == "windows" && len(abs) >= 2 && abs[1] == ':' {
		abs = strings.ToLower(string(abs[0])) + abs[1:]
	}
	return abs, nil
}

// LoadLSPTrustedRoots reads the trusted-roots store from path. An absent
// file is NOT an error — it returns an empty file (Version set to the
// current schema, Roots nil) so callers treat "no file" as "no trusted
// roots". A present file is parsed strictly enough to reject malformed
// JSON, but unknown top-level fields are tolerated (forward-compat with
// a future writer that adds fields) by using the default decoder.
//
// The read applies the SAME parent-directory DACL gate the
// supervisor-intent / supervisor-state readers apply
// (checkStateDirParentWriteSafe), unless the operator has opted into the
// relax lane via MCPHUB_ALLOW_UNHARDENED_STATE_READ=1. A parent that
// grants write/delete to a non-allowlisted principal is a swap risk: an
// attacker who can replace lsp-trusted-roots.json could inject an
// attacker-chosen trusted root and re-open the very hole this file
// closes. The gate refuses such a parent on read, symmetric with the
// write side.
func LoadLSPTrustedRoots(path string) (*LSPTrustedRootsFile, error) {
	if !operatorAllowsUnhardenedStateRead() {
		if err := checkStateDirParentWriteSafe(filepath.Dir(path)); err != nil {
			return nil, fmt.Errorf("read %s: insecure parent directory (set %s=1 to opt into the relax lane on operator-managed Windows hosts whose %%LOCALAPPDATA%% inherits AD-pushed groups, or tighten the parent's DACL): %w",
				path, AllowUnhardenedStateReadEnv, err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &LSPTrustedRootsFile{Version: lspTrustedRootsVersion}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f LSPTrustedRootsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if f.Version == 0 {
		f.Version = lspTrustedRootsVersion
	}
	return &f, nil
}

// LoadDefaultLSPTrustedRoots resolves the default store path and loads
// it (tolerating an absent file).
func LoadDefaultLSPTrustedRoots() (*LSPTrustedRootsFile, error) {
	path, err := DefaultLSPTrustedRootsPath()
	if err != nil {
		return nil, err
	}
	return LoadLSPTrustedRoots(path)
}

// canonicalizedRootSet returns the file's roots canonicalized + deduped.
// A root that fails canonicalization is dropped (it cannot match a
// canonical WorkspaceRoot anyway) but does not fail the whole load — an
// operator hand-editing the file should not break every other root by
// fat-fingering one entry. The drop is silent here; the containment
// check is the only consumer and a non-canonicalizable root can never
// contain a canonical workspace root.
func (f *LSPTrustedRootsFile) canonicalizedRootSet() map[string]struct{} {
	out := make(map[string]struct{}, len(f.Roots))
	for _, r := range f.Roots {
		c, err := canonicalizeTrustedRoot(r)
		if err != nil {
			continue
		}
		out[c] = struct{}{}
	}
	return out
}

// rootContains reports whether candidate is equal to, or a true
// subdirectory of, trustedRoot. Both arguments MUST already be in
// canonical form (canonicalizeTrustedRoot output).
//
// The test is separator-aware, NOT a bare string prefix: candidate is
// contained iff candidate == trustedRoot, OR candidate begins with
// trustedRoot + os.PathSeparator. This is what stops "/dev" from
// matching "/developer" and "C:\proj" from matching "C:\project2".
//
// Comparison is case-insensitive on Windows (NTFS is case-preserving
// but case-insensitive, and canonicalizeTrustedRoot already lowercases
// only the drive letter, not the rest of the path), case-sensitive
// elsewhere. This matches the repo convention of strings.EqualFold for
// Windows path comparison (e.g. install.go's image comparison, the
// watchdog XML validator's path equality).
func rootContains(trustedRoot, candidate string) bool {
	if trustedRoot == "" || candidate == "" {
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
	if eq(trustedRoot, candidate) {
		return true
	}
	// A true subdirectory: candidate must start with trustedRoot plus a
	// separator. Strip any trailing separator on trustedRoot first so a
	// stored "C:\" (drive root, already ends in a separator) still
	// composes a correct single-separator boundary.
	prefix := strings.TrimRight(trustedRoot, string(os.PathSeparator))
	if prefix == "" {
		// trustedRoot was only separators (e.g. POSIX "/"): everything
		// is under it.
		return hasPrefix(candidate, string(os.PathSeparator))
	}
	return hasPrefix(candidate, prefix+string(os.PathSeparator))
}

// LSPWorkspaceRootTrusted reports whether workspaceRoot is authorized
// for first-touch auto-register: true iff its canonical form is equal
// to, or a true subdirectory of, at least one trusted root in the
// store. This is the authorization boundary the GUI LSP router consults
// before calling its AutoRegisterFn.
//
// workspaceRoot is canonicalized before comparison so a router-supplied
// WorkspaceRoot (already canonical in production via the resolver, but
// canonicalized again here defensively) matches stored roots regardless
// of symlink aliasing or Windows drive-letter case. An empty
// workspaceRoot, or one that fails canonicalization, is NOT trusted
// (fail-closed).
//
// A nil receiver (no store loaded) is treated as an empty store → not
// trusted, so a load failure upstream that passes nil never silently
// authorizes.
func (f *LSPTrustedRootsFile) LSPWorkspaceRootTrusted(workspaceRoot string) bool {
	if f == nil {
		return false
	}
	canonical, err := canonicalizeTrustedRoot(workspaceRoot)
	if err != nil {
		return false
	}
	for trusted := range f.canonicalizedRootSet() {
		if rootContains(trusted, canonical) {
			return true
		}
	}
	return false
}

// LSPWorkspaceRootTrusted loads the default trusted-roots store and
// reports whether workspaceRoot is authorized for first-touch
// auto-register. This is the production seam the GUI router wires (via
// SetLSPRouterProduction) so the gate reads the live on-disk store on
// each first-touch decision rather than caching a snapshot — an
// operator who hand-edits the file, or a concurrent explicit register
// that just blessed a root, takes effect on the next router request.
//
// A load failure (corrupt JSON, insecure-parent gate rejection)
// propagates as an error so the router fails CLOSED (refuses
// auto-register) rather than silently trusting. The router maps the
// error to the same refusal it would emit for an untrusted path.
func LSPWorkspaceRootTrusted(workspaceRoot string) (bool, error) {
	f, err := LoadDefaultLSPTrustedRoots()
	if err != nil {
		return false, err
	}
	return f.LSPWorkspaceRootTrusted(workspaceRoot), nil
}

// BlessTrustedRoot canonicalizes workspaceRoot and idempotently appends
// it to the trusted-roots store at path under a cross-process flock.
// This is "auto-bless on first explicit register": it MUST be called
// ONLY from the explicit operator-driven register entry points (the
// `mcphub register` CLI command and the GUI "Enable" / lsp-register
// handler), NEVER from the GUI router's auto-register seam. Blessing on
// the router path would re-open the vulnerability — an untrusted
// tool-call path would bless itself and then pass the containment check
// on the very next request.
//
// Idempotent: a root already present (after canonicalization + the same
// case-fold/separator semantics the containment check uses) is a no-op
// and does not rewrite the file. Roots are kept sorted for a stable,
// reviewable on-disk file. The write goes through the hardened
// state-file pipeline while the flock is held (WriteStateFileBytesLockHeld),
// so the published file is owner-only and the read-modify-write is
// atomic across processes.
//
// A blessing failure is the caller's to decide on: explicit register
// should treat it as non-fatal (the workspace IS registered; the worst
// case is that a SIBLING workspace under the same tree will need its own
// explicit register because the bless did not land). Callers log it
// rather than failing the register.
func BlessTrustedRoot(path, workspaceRoot string) error {
	canonical, err := canonicalizeTrustedRoot(workspaceRoot)
	if err != nil {
		return fmt.Errorf("bless trusted root: canonicalize %q: %w", workspaceRoot, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("bless trusted root: mkdir state dir: %w", err)
	}
	lock := flock.New(path + lspTrustedRootsLockSuffix)
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("bless trusted root: flock %s: %w", path+lspTrustedRootsLockSuffix, err)
	}
	defer func() { _ = lock.Unlock() }()

	f, err := LoadLSPTrustedRoots(path)
	if err != nil {
		return fmt.Errorf("bless trusted root: load %s: %w", path, err)
	}

	// Idempotency: if the canonical root already EXACTLY matches a
	// stored canonical root, no write. (Use exact canonical equality,
	// not containment — a child of an existing root is still recorded
	// as its own entry so that removing the broader root later does not
	// silently de-trust the explicitly-registered child.)
	for stored := range f.canonicalizedRootSet() {
		if storedEqualsCanonical(stored, canonical) {
			return nil
		}
	}

	f.Version = lspTrustedRootsVersion
	f.Roots = append(f.Roots, canonical)
	// Re-canonicalize + dedup the whole slice so a hand-edited file with
	// duplicate or non-canonical entries is normalized on the next
	// bless, and keep it sorted for a stable diff.
	f.Roots = normalizedSortedRoots(f.Roots)

	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("bless trusted root: marshal: %w", err)
	}
	if err := WriteStateFileBytesLockHeld(path, raw); err != nil {
		return fmt.Errorf("bless trusted root: write %s: %w", path, err)
	}
	return nil
}

// BlessDefaultTrustedRoot resolves the default store path and blesses
// workspaceRoot there. The explicit register call sites use this.
func BlessDefaultTrustedRoot(workspaceRoot string) error {
	path, err := DefaultLSPTrustedRootsPath()
	if err != nil {
		return err
	}
	return BlessTrustedRoot(path, workspaceRoot)
}

// RemoveTrustedRoot canonicalizes root and idempotently removes every
// stored root whose canonical form equals it from the trusted-roots
// store at path, under the SAME cross-process flock and hardened
// state-file pipeline BlessTrustedRoot uses. It is the inverse of
// BlessTrustedRoot and is the operator-driven "un-trust this root"
// management surface the GUI Settings panel drives (it is NOT reachable
// from the router's auto-register seam — removing a root only ever
// SHRINKS trust, so it cannot re-open the vulnerability).
//
// Removal is by EXACT canonical equality (same case-fold/separator
// semantics storedEqualsCanonical uses), not containment: removing a
// broad root does NOT silently de-trust an explicitly-registered child
// that was recorded as its own entry, matching the bless-side comment
// that children are stored independently.
//
// Idempotent: removing a root that is not present (or an empty store)
// is a no-op success and does not rewrite the file, so the published
// file's mtime only changes when something actually changed. The
// surviving roots are re-normalized + sorted so a hand-edited file is
// also cleaned up on the next remove.
func RemoveTrustedRoot(path, root string) error {
	canonical, err := canonicalizeTrustedRoot(root)
	if err != nil {
		return fmt.Errorf("remove trusted root: canonicalize %q: %w", root, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("remove trusted root: mkdir state dir: %w", err)
	}
	lock := flock.New(path + lspTrustedRootsLockSuffix)
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("remove trusted root: flock %s: %w", path+lspTrustedRootsLockSuffix, err)
	}
	defer func() { _ = lock.Unlock() }()

	f, err := LoadLSPTrustedRoots(path)
	if err != nil {
		return fmt.Errorf("remove trusted root: load %s: %w", path, err)
	}

	// Drop every stored entry whose canonical form equals the target.
	// Compare against each entry's canonical form so a non-canonical
	// hand-edited entry that still resolves to the target is removed too.
	// A stored entry that fails canonicalization is retained verbatim (it
	// cannot match the target and dropping it would be a silent edit).
	kept := make([]string, 0, len(f.Roots))
	removedAny := false
	for _, stored := range f.Roots {
		c, cerr := canonicalizeTrustedRoot(stored)
		if cerr == nil && storedEqualsCanonical(c, canonical) {
			removedAny = true
			continue
		}
		kept = append(kept, stored)
	}
	if !removedAny {
		// No matching entry: no-op success, leave the file untouched so
		// the mtime is not churned on a removing-an-absent-root call.
		return nil
	}

	f.Version = lspTrustedRootsVersion
	// Re-canonicalize + dedup the survivors so a hand-edited file with
	// duplicate or non-canonical entries is normalized on remove, keeping
	// the on-disk file stable and reviewable (symmetric with bless).
	f.Roots = normalizedSortedRoots(kept)

	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("remove trusted root: marshal: %w", err)
	}
	if err := WriteStateFileBytesLockHeld(path, raw); err != nil {
		return fmt.Errorf("remove trusted root: write %s: %w", path, err)
	}
	return nil
}

// RemoveDefaultTrustedRoot resolves the default store path and removes
// root there. The GUI Settings "Trusted Roots" panel uses this.
func RemoveDefaultTrustedRoot(root string) error {
	path, err := DefaultLSPTrustedRootsPath()
	if err != nil {
		return err
	}
	return RemoveTrustedRoot(path, root)
}

// storedEqualsCanonical compares two already-canonical roots with the
// same case semantics rootContains uses (EqualFold on Windows, exact
// elsewhere).
func storedEqualsCanonical(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// normalizedSortedRoots canonicalizes every entry, drops the
// non-canonicalizable ones and case/path duplicates, and returns the
// survivors sorted. Used by BlessTrustedRoot to keep the on-disk file
// normalized and stable.
func normalizedSortedRoots(roots []string) []string {
	seen := make(map[string]string, len(roots)) // dedup-key -> canonical-as-stored
	for _, r := range roots {
		c, err := canonicalizeTrustedRoot(r)
		if err != nil {
			continue
		}
		key := c
		if runtime.GOOS == "windows" {
			key = strings.ToLower(c)
		}
		if _, dup := seen[key]; !dup {
			seen[key] = c
		}
	}
	out := make([]string, 0, len(seen))
	for _, c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

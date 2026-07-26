package serena_routing

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
)

// serenaProjectMarker is the relative path inside a workspace that
// identifies it as a serena project. AncestorWalk searches upward for
// this file to map an absolute path back to its workspace root.
const serenaProjectMarker = ".serena/project.yml"

// WorkspaceResolver maps an inbound tool-argument path to a registered
// per-workspace serena entry (Language == api.SerenaLanguageSentinel).
//
// It owns a *api.Registry handle plus the on-disk registry path, and
// re-reads the on-disk view when workspaces.yaml mtime advances. The
// reload is best-effort: a failed reload leaves the previously-loaded
// view in place rather than flipping the resolver into an error state
// on every call -- transient filesystem hiccups should not knock
// routing offline.
//
// Concurrency: all exported methods are safe for parallel use. The
// internal state mutex guards both the cached workspace list and the
// last-seen mtime.
type WorkspaceResolver struct {
	reg          *api.Registry
	registryPath string
	// readOnly, when true, makes refresh() reload the registry WITHOUT ever
	// taking Registry.Lock() (the cross-process exclusive flock, which also
	// CREATES <registry>.lock + the state directory on first acquire). See
	// NewReadOnlyWorkspaceResolver's doc comment for the safety argument and
	// the P2-3 finding this closes.
	readOnly bool

	mu        sync.RWMutex
	lastMtime time.Time
	loaded    bool                 // true after first successful Load, even with zero serena rows
	cached    []api.WorkspaceEntry // snapshot of reg.SerenaEntries()

	// refreshMu serializes the reload-and-publish critical section across
	// concurrent refresh() calls (A2 fix, architecture-adversarial-
	// reverify.md + qa-adversarial-falsifiers.md, work-items/active/2026-07-
	// 25-mcp-front-daemon/). Without it, two overlapping refreshes could
	// each Load() a DIFFERENT complete registry generation and then race to
	// publish under r.mu.Lock() independently — whichever happens to
	// acquire r.mu.Lock() LAST wins regardless of which generation is
	// actually newer, so an older generation could overwrite a newer one
	// already served to callers. Holding refreshMu across the WHOLE reload
	// (Load through publish), not just the final r.mu section, makes
	// concurrent reloads mutually exclusive: whichever acquires refreshMu
	// second always re-stats and re-loads AFTER the first one's publish
	// completed, so the generation it observes is guaranteed to be at
	// least as new as what was already published — publication order
	// becomes monotonic with real time.
	//
	// This is a purely in-process mutex. It does NOT reintroduce the
	// cross-process Registry.Lock() flock the read-only variant exists to
	// avoid (see NewReadOnlyWorkspaceResolver's doc comment) — the
	// readOnly branch below still never touches that lock, so the route
	// daemon still cannot contend with (or block) a concurrent GUI writer.
	//
	// The cheap "nothing changed" fast path in staleness() stays reachable
	// WITHOUT acquiring refreshMu, so the common steady-state case (no
	// reload needed) pays no serialization cost.
	refreshMu sync.Mutex
}

// refreshLoadedHookForTest, when non-nil, is invoked synchronously with the
// freshly-loaded entries slice right after a reload has read complete
// registry content but BEFORE that content is published to the resolver's
// cache (still inside the refreshMu critical section). Package-internal
// tests use it to deterministically engineer two overlapping reloads
// without relying on a naturally-occurring race window staying observable
// (see TestNewReadOnlyWorkspaceResolver_ConcurrentRefreshesCannotRegress).
// Always nil in production.
var refreshLoadedHookForTest func(entries []api.WorkspaceEntry)

// NewWorkspaceResolver returns a resolver bound to reg and the on-disk
// path of the registry file. The caller is responsible for calling
// reg.Load() at least once before the resolver serves requests;
// subsequent reloads happen implicitly inside the resolver when
// workspaces.yaml mtime advances.
//
// Pass the path explicitly so production callers using
// api.DefaultRegistryPath() and test callers using a tempdir path
// share the same resolver type without a Registry getter.
func NewWorkspaceResolver(reg *api.Registry, registryPath string) *WorkspaceResolver {
	return &WorkspaceResolver{reg: reg, registryPath: registryPath}
}

// NewReadOnlyWorkspaceResolver returns a resolver identical to
// NewWorkspaceResolver except its refresh NEVER takes Registry.Lock() — for
// the standalone `mcphub route` front daemon (internal/cli/route.go), which
// must never contend with the GUI's own registry writers for the SAME
// cross-process exclusive lock (P2-3 finding, adversarial cross-family
// review of Increment 1): a blocking Lock() there could stall route traffic
// behind a slow GUI mutation, or (more importantly) stall a GUI mutation
// behind route traffic, and Lock() unconditionally creates <registry>.lock +
// the state directory on first acquire even when nothing needs writing.
//
// Safety argument for the unlocked reload: every registry write goes through
// the hardened state-file pipeline (api.SecureWriteClientConfig-backed,
// documented in workspace_registry.go's Save()), which publishes via an
// atomic rename across a held file handle. A concurrent unlocked reader
// therefore always observes either the complete pre-write or the complete
// post-write content — never a torn/partial file — so skipping the lock
// cannot corrupt the in-memory cache. At worst, a reload that races a
// concurrent write sees the OLDER complete snapshot and re-reads on the
// NEXT mtime-triggered refresh; every other production/GUI caller
// (NewWorkspaceResolver) is completely unaffected — this is a strictly
// reduced-guarantee, additive variant.
func NewReadOnlyWorkspaceResolver(reg *api.Registry, registryPath string) *WorkspaceResolver {
	return &WorkspaceResolver{reg: reg, registryPath: registryPath, readOnly: true}
}

// NewDefaultWorkspaceResolver constructs a resolver bound to reg using
// api.DefaultRegistryPath() as the on-disk path. Returns an error if
// the default path cannot be resolved (e.g. UserHomeDir failure).
func NewDefaultWorkspaceResolver(reg *api.Registry) (*WorkspaceResolver, error) {
	p, err := api.DefaultRegistryPath()
	if err != nil {
		return nil, fmt.Errorf("serena_routing: default registry path: %w", err)
	}
	return NewWorkspaceResolver(reg, p), nil
}

// snapshot returns the current view of serena workspaces, refreshing
// the in-memory cache if workspaces.yaml mtime has advanced.
//
// A reload failure (registry file vanished mid-flight, parse error)
// returns the existing cache unchanged -- operationally the resolver
// must keep serving traffic across transient registry-write races.
func (r *WorkspaceResolver) snapshot() []api.WorkspaceEntry {
	r.refresh()
	r.mu.RLock()
	defer r.mu.RUnlock()
	// Return a shallow copy so callers cannot mutate the cached slice.
	out := make([]api.WorkspaceEntry, len(r.cached))
	copy(out, r.cached)
	return out
}

// refresh re-reads the registry file when its mtime has advanced past
// the last observed value. Callers must NOT hold r.mu when entering.
//
// Locking: the cheap "nothing changed" check (staleness()) is unserialized
// so the common no-change path stays lock-light. An actual reload is
// serialized end-to-end (Load through publish) under refreshMu — see the
// struct's own doc comment for why this is required (A2 fix) and why it
// does not reintroduce the cross-process registry lock the read-only
// variant avoids.
func (r *WorkspaceResolver) refresh() {
	if r.reg == nil || r.registryPath == "" {
		return
	}
	if stale, _, _ := r.staleness(); !stale {
		return
	}

	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()

	// Re-check now that we hold refreshMu: another goroutine may have
	// already completed an equivalent-or-newer reload while we waited for
	// the lock. This re-stats rather than reusing the pre-lock result, so
	// it reflects the registry's actual state at this moment.
	stale, fi, statErr := r.staleness()
	if !stale {
		return
	}

	if statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			r.mu.Lock()
			r.cached = nil
			r.lastMtime = time.Time{}
			r.loaded = false
			r.mu.Unlock()
			return
		}
		fmt.Fprintf(os.Stderr, "serena_routing: stat registry %s: %v; preserving cached snapshot\n", r.registryPath, statErr)
		return
	}

	mtime := fi.ModTime()

	if r.readOnly {
		// P2-3 fix: never take Registry.Lock() (see
		// NewReadOnlyWorkspaceResolver's doc comment for the safety
		// argument). Load() alone performs no locking and creates no
		// directory/lock file of its own.
		if err := r.reg.Load(); err != nil {
			fmt.Fprintf(os.Stderr, "serena_routing: load registry %s (read-only, unlocked): %v; preserving cached snapshot\n", r.registryPath, err)
			return
		}
	} else {
		unlock, err := r.reg.Lock()
		if err != nil {
			// The registry file exists (stat succeeded) but its cross-process lock
			// could not be acquired. This is NOT a transient missing-file case — it
			// is a genuine error (lock contention, permission, or a stuck holder), so
			// emit the same operator-visible diagnostic the stat-error path above
			// uses rather than returning silently. We still preserve the cached
			// snapshot (best-effort routing-availability posture, documented above)
			// instead of flipping into an error state; the diagnostic just makes the
			// degraded reload observable so an operator's workspaces.yaml edit not
			// taking effect is not invisible.
			fmt.Fprintf(os.Stderr, "serena_routing: lock registry %s: %v; preserving cached snapshot\n", r.registryPath, err)
			return
		}
		defer unlock()
		if err := r.reg.Load(); err != nil {
			// The registry file exists and was locked, but parsing/loading it failed
			// (e.g. a malformed YAML after an operator hand-edit). Same fail-loud
			// diagnostic as the lock-error and stat-error paths: surface the load
			// failure so a stale cached view being served is operator-visible,
			// while still preserving the prior snapshot for routing availability.
			fmt.Fprintf(os.Stderr, "serena_routing: load registry %s: %v; preserving cached snapshot\n", r.registryPath, err)
			return
		}
	}
	entries := r.reg.SerenaEntries()

	if refreshLoadedHookForTest != nil {
		refreshLoadedHookForTest(entries)
	}

	r.mu.Lock()
	r.cached = entries
	r.loaded = true
	r.lastMtime = mtime
	r.mu.Unlock()
}

// staleness reports whether the registry file's current mtime differs from
// the resolver's last-observed generation (or nothing has ever loaded
// successfully), alongside the stat result that produced the answer so a
// caller does not have to re-stat to act on it.
func (r *WorkspaceResolver) staleness() (stale bool, fi os.FileInfo, statErr error) {
	fi, statErr = os.Stat(r.registryPath)
	r.mu.RLock()
	lastMtime := r.lastMtime
	cacheEmpty := !r.loaded
	r.mu.RUnlock()
	if statErr != nil {
		return true, fi, statErr
	}
	if !cacheEmpty && fi.ModTime().Equal(lastMtime) {
		return false, fi, nil
	}
	return true, fi, nil
}

// ListWorkspaces returns a snapshot of every registered serena workspace
// entry as fresh pointer copies, refreshing the in-memory cache if
// workspaces.yaml mtime has advanced (same reload discipline as
// ResolveByPath).
//
// It exists so the /serena/mcp router can satisfy a workspace-agnostic
// MCP lifecycle request (tools/list) by proxying to ANY live serena
// daemon: the router picks an entry from this list, forwards one
// tools/list to it, and caches the result. The returned pointers are
// value-copies from the cached snapshot, so callers may read their
// fields without holding any resolver lock and cannot mutate the cache.
//
// Returns an empty (non-nil-distinguishing) slice when no serena
// workspace is registered; the router treats that as the empty-pool
// case and answers with an explicit JSON-RPC error rather than a
// fabricated empty tool list.
func (r *WorkspaceResolver) ListWorkspaces() []*api.WorkspaceEntry {
	entries := r.snapshot()
	out := make([]*api.WorkspaceEntry, len(entries))
	for i := range entries {
		e := entries[i]
		out[i] = &e
	}
	return out
}

// ResolveByPath maps an inbound MCP tool argument path to a registered
// serena workspace entry.
//
// Semantics:
//
//   - path is empty -> ErrInvalidPath
//   - path is absolute -> AncestorWalk to a directory containing
//     .serena/project.yml, then match that directory against
//     registered serena entries by canonicalized WorkspacePath. If
//     ancestor-walk finds no marker file, OR the found directory is
//     not in the registry, ErrWorkspaceNotFound.
//   - path is relative -> iterate serena entries in alphabetic order by
//     WorkspacePath, joining each with path. The first join whose
//     result resolves to an existing filesystem entry wins. No match
//     -> ErrWorkspaceNotFound.
//
// The returned entry is a value-copy from the cached snapshot; callers
// are free to read its fields without holding any resolver lock.
func (r *WorkspaceResolver) ResolveByPath(path string) (*api.WorkspaceEntry, error) {
	if path == "" {
		return nil, ErrInvalidPath
	}
	if isWindowsUNCPath(path) {
		return nil, ErrInvalidPath
	}
	entries := r.snapshot()
	if len(entries) == 0 {
		return nil, ErrWorkspaceNotFound
	}
	if filepath.IsAbs(path) {
		return r.resolveAbsolute(path, entries)
	}
	return r.resolveRelative(path, entries)
}

// isWindowsUNCPath returns true when path looks like a UNC/network
// share root - any two-leading-separator permutation including
// canonical `\\server\share\...` and `//server/share/...` plus the
// mixed forms `\/server\share` and `/\server/share`.
//
// The rejection is Windows-specific. On Windows both `/` and `\` are
// path separators, so any permutation of two leading separators is a
// UNC root, and a missed spelling can fall through to `os.Lstat` on an
// attacker-controlled network path - exactly the credential-leak
// filesystem probe this helper is meant to prevent.
//
// On non-Windows hosts the helper returns false so paths like
// `//tmp/project/file.go` (a valid POSIX absolute local path - Go's
// filepath treats it as absolute and `os.Lstat` resolves locally)
// continue to reach the normal resolution branches. Applying the
// rejection unconditionally would regress every Unix workspace whose
// canonical form happens to carry a double-slash prefix.
func isWindowsUNCPath(path string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	if len(path) < 2 {
		return false
	}
	isSep := func(c byte) bool { return c == '\\' || c == '/' }
	return isSep(path[0]) && isSep(path[1])
}

// resolveAbsolute handles the absolute-path branch of ResolveByPath.
//
// Step 1: AncestorWalk(path) finds the workspace directory that owns
// path (the nearest ancestor containing .serena/project.yml).
//
// Step 2: canonicalize that directory and look it up against the
// canonical WorkspacePath of each cached serena entry.
//
// An unregistered workspace directory (marker present but no row in
// workspaces.yaml) returns ErrWorkspaceNotFound -- Phase E will own
// auto-register on miss; the resolver itself stays write-free.
func (r *WorkspaceResolver) resolveAbsolute(path string, entries []api.WorkspaceEntry) (*api.WorkspaceEntry, error) {
	wsDir, err := r.AncestorWalk(path)
	if err != nil {
		return nil, err
	}
	canon, err := api.CanonicalWorkspacePath(wsDir)
	if err != nil {
		return nil, fmt.Errorf("%w: canonicalize ancestor %s: %v", ErrWorkspaceNotFound, wsDir, err)
	}
	for i := range entries {
		entryPath := canonicalizeWorkspacePath(entries[i].WorkspacePath)
		if entryPath == canon {
			e := entries[i]
			return &e, nil
		}
	}
	return nil, ErrWorkspaceNotFound
}

// resolveRelative handles the relative-path branch of ResolveByPath.
//
// Serena entries are iterated in alphabetic order by WorkspacePath so
// the resolution is deterministic across registry-row insertion order
// changes. The first entry whose WorkspacePath+rel exists wins.
//
// The "exists" check uses os.Lstat (not Stat) to count broken symlinks
// as "exists" -- the tool itself decides how to handle the broken link;
// the routing layer job is to deliver the call to the right daemon.
func (r *WorkspaceResolver) resolveRelative(rel string, entries []api.WorkspaceEntry) (*api.WorkspaceEntry, error) {
	sorted := make([]api.WorkspaceEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].WorkspacePath < sorted[j].WorkspacePath
	})
	for i := range sorted {
		ws := sorted[i]
		if ws.WorkspacePath == "" {
			continue
		}
		candidate := filepath.Join(ws.WorkspacePath, rel)
		cleanCandidate := filepath.Clean(candidate)
		cleanWorkspace := filepath.Clean(ws.WorkspacePath)
		relativeToWorkspace, err := filepath.Rel(cleanWorkspace, cleanCandidate)
		if err != nil || filepath.IsAbs(relativeToWorkspace) ||
			relativeToWorkspace == ".." ||
			strings.HasPrefix(relativeToWorkspace, ".."+string(os.PathSeparator)) {
			continue
		}
		if _, err := os.Lstat(candidate); err == nil {
			e := sorted[i]
			return &e, nil
		}
	}
	return nil, ErrWorkspaceNotFound
}

// AncestorWalk walks up from absPath until a directory containing
// .serena/project.yml is found; returns that directory.
//
// If absPath itself is a file, the walk starts from its parent. If
// absPath is a directory, the walk checks absPath first.
//
// Stops at the filesystem root (filepath.Dir returns its own input);
// returns ErrWorkspaceNotFound when no marker is found.
//
// absPath must already be absolute; relative inputs return
// ErrInvalidPath. (ResolveByPath is the entry point that distinguishes
// abs vs. rel; callers using AncestorWalk directly are expected to
// pass an absolute path.)
func (r *WorkspaceResolver) AncestorWalk(absPath string) (string, error) {
	if absPath == "" {
		return "", ErrInvalidPath
	}
	if !filepath.IsAbs(absPath) {
		return "", fmt.Errorf("%w: AncestorWalk requires an absolute path, got %q", ErrInvalidPath, absPath)
	}
	dir := absPath
	if fi, err := os.Lstat(absPath); err != nil || !fi.IsDir() {
		dir = filepath.Dir(absPath)
	}
	for {
		marker := filepath.Join(dir, serenaProjectMarker)
		if _, err := os.Lstat(marker); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrWorkspaceNotFound
		}
		dir = parent
	}
}

// canonicalizeWorkspacePath returns a comparison-friendly form of a
// stored WorkspacePath. It mirrors the normalization rules in
// api.CanonicalWorkspacePath but does NOT require the directory to
// exist on disk -- registry entries can outlive their workspace dir
// (manual deletion, drive unmount), and we still want a sane compare.
//
// Rules: Abs+Clean, then lowercase the Windows drive letter. Symlink
// evaluation is skipped because the stored value was canonicalized at
// registration time; re-resolving symlinks on every routing call is
// unnecessary and would surface transient FS errors on dropped network
// drives.
func canonicalizeWorkspacePath(p string) string {
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(filepath.Clean(p))
	if err != nil {
		return filepath.Clean(p)
	}
	if len(abs) >= 2 && abs[1] == ':' && isASCIILetter(abs[0]) {
		abs = strings.ToLower(string(abs[0])) + abs[1:]
	}
	return abs
}

// isASCIILetter is a Windows-drive-letter helper kept local to avoid
// pulling in unicode/utf8 for a one-char range check.
func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

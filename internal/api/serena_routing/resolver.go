package serena_routing

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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

	mu        sync.RWMutex
	lastMtime time.Time
	loaded    bool                 // true after first successful Load, even with zero serena rows
	cached    []api.WorkspaceEntry // snapshot of reg.SerenaEntries()
}

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
// Locking: refresh acquires r.mu twice -- first as RLock to read the
// stored mtime, then as Lock to install a new snapshot. Splitting the
// critical sections keeps the common no-change path cheap. Between the
// two critical sections we acquire the registry cross-process lock so
// the load is consistent with any concurrent writer.
func (r *WorkspaceResolver) refresh() {
	if r.reg == nil || r.registryPath == "" {
		return
	}
	fi, statErr := os.Stat(r.registryPath)

	r.mu.RLock()
	lastMtime := r.lastMtime
	cacheEmpty := !r.loaded
	r.mu.RUnlock()

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
	if !cacheEmpty && mtime.Equal(lastMtime) {
		return
	}

	unlock, err := r.reg.Lock()
	if err != nil {
		return
	}
	defer unlock()
	if err := r.reg.Load(); err != nil {
		return
	}
	entries := r.reg.SerenaEntries()

	r.mu.Lock()
	r.cached = entries
	r.loaded = true
	r.lastMtime = mtime
	r.mu.Unlock()
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

// isWindowsUNCPath returns true for paths that look like UNC/network
// paths (\\server\share\... or //server/share/...) so the resolver can
// reject them before any filesystem probe.
func isWindowsUNCPath(path string) bool {
	return strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, "//")
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

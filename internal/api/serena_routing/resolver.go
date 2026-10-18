package serena_routing

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"mcp-local-hub/internal/api"
)

// serenaProjectMarker is the relative path inside a workspace that
// identifies it as a serena project. AncestorWalk searches upward for
// this file to map an absolute path back to its workspace root.
const serenaProjectMarker = ".serena/project.yml"

// WorkspaceResolver maps an inbound tool-argument path to a registered
// per-workspace serena entry (Language == api.SerenaLanguageSentinel).
//
// Both Serena and LSP consume the same API-owned content-generation observer
// in production. Legacy direct constructors allocate a private observer with
// identical freshness and failure semantics.
type WorkspaceResolver struct {
	observer     *api.RegistrySnapshotObserver
	registryPath string       // used by the independently owned default-workspace marker
	mu           sync.RWMutex // protects only the default-marker diagnostic sink
	auditFn      func(level, event string, fields map[string]any) error
}

// NewWorkspaceResolver returns a resolver bound to reg and the on-disk
// path of the registry file. The observer reads on the first lookup; callers
// need not preload reg. Changed metadata triggers a read, and equal metadata
// still gets an exact-byte comparison within 32 registry-dependent lookups
// while the file remains readable.
//
// Pass the path explicitly so production callers using
// api.DefaultRegistryPath() and test callers using a tempdir path
// share the same resolver type without a Registry getter.
func NewWorkspaceResolver(reg *api.Registry, registryPath string) *WorkspaceResolver {
	if reg == nil {
		return NewWorkspaceResolverWithObserver(nil, registryPath)
	}
	return NewWorkspaceResolverWithObserver(api.NewRegistrySnapshotObserver(reg, registryPath, false), registryPath)
}

// NewWorkspaceResolverWithObserver injects the one snapshot owner shared with
// LSP at the process composition root.
func NewWorkspaceResolverWithObserver(observer *api.RegistrySnapshotObserver, registryPath string) *WorkspaceResolver {
	return &WorkspaceResolver{observer: observer, registryPath: registryPath}
}

// NewReadOnlyWorkspaceResolver returns a resolver identical to
// NewWorkspaceResolver except its observer NEVER takes Registry.Lock() — for
// the standalone `mcphub route` front daemon (internal/cli/route.go), which
// must never contend with the GUI's own registry writers for the SAME
// cross-process exclusive lock (P2-3 finding, adversarial cross-family
// review of Increment 1): a blocking Lock() there could stall route traffic
// behind a slow GUI mutation, or (more importantly) stall a GUI mutation
// behind route traffic, and Lock() unconditionally creates <registry>.lock +
// the state directory on first acquire even when nothing needs writing.
//
// Safety argument for the unlocked observation: every registry write goes through
// the hardened state-file pipeline (api.SecureWriteClientConfig-backed,
// documented in workspace_registry.go's Save()), which publishes via an
// atomic rename across a held file handle. A concurrent unlocked reader
// therefore always observes either the complete pre-write or the complete
// post-write content — never a torn/partial file — so skipping the lock
// cannot corrupt the observer's immutable generation. A read racing a write
// may return the older complete generation; the next distinguishable metadata
// observation, or the exact-byte comparison within 32 lookups when metadata
// aliases on a readable file, makes the durable generation visible. GUI mode
// still takes the lock.
func NewReadOnlyWorkspaceResolver(reg *api.Registry, registryPath string) *WorkspaceResolver {
	if reg == nil {
		return NewWorkspaceResolverWithObserver(nil, registryPath)
	}
	return NewWorkspaceResolverWithObserver(api.NewRegistrySnapshotObserver(reg, registryPath, true), registryPath)
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

// SetAuditSink supplies the optional operator diagnostic route for default
// marker reads. The GUI and standalone route daemon intentionally choose
// different sinks because they own different state files.
func (r *WorkspaceResolver) SetAuditSink(auditFn func(level, event string, fields map[string]any) error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.auditFn = auditFn
	r.mu.Unlock()
}

func (r *WorkspaceResolver) auditDefaultWorkspace(level, event string, fields map[string]any) {
	if r == nil {
		return
	}
	r.mu.RLock()
	auditFn := r.auditFn
	r.mu.RUnlock()
	if auditFn != nil {
		_ = auditFn(level, event, fields)
	}
}

func (r *WorkspaceResolver) auditSink() func(level, event string, fields map[string]any) error {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.auditFn
}

// observe returns one immutable generation without cloning unrelated rows.
func (r *WorkspaceResolver) observe() api.RegistrySnapshotView {
	if r == nil || r.observer == nil {
		return api.RegistrySnapshotView{}
	}
	view, _, _ := r.observer.ObserveView()
	return view
}

// ListWorkspaces returns fresh detached pointer copies of every registered
// Serena row from one observer generation. Like ResolveByPath, it checks
// content within 32 lookups on a stable readable file, even when metadata is unchanged.
//
// It exists so the /serena/mcp router can satisfy a workspace-agnostic
// MCP lifecycle request (tools/list) by proxying to ANY live serena
// daemon: the router picks an entry from this list, forwards one
// tools/list to it, and caches the result. The returned pointers are
// deep-copies from the observer-owned generation, so callers may read or
// mutate nested fields without changing later results.
//
// Returns an empty (non-nil-distinguishing) slice when no serena
// workspace is registered; the router treats that as the empty-pool
// case and answers with an explicit JSON-RPC error rather than a
// fabricated empty tool list.
func (r *WorkspaceResolver) ListWorkspaces() []*api.WorkspaceEntry {
	view := r.observe()
	out := make([]*api.WorkspaceEntry, 0, view.SerenaCount())
	for i := 0; i < view.SerenaCount(); i++ {
		candidate, _ := view.SerenaCandidate(i)
		entry, _ := view.Materialize(candidate)
		out = append(out, &entry)
	}
	return out
}

// ResolveDefaultWorkspace returns the operator-selected default Serena row.
// It reads the existing marker only, observes the current Serena generation,
// and matches the marker's canonical path exactly. It never auto-registers,
// selects an arbitrary row, or rewrites the marker.
func (r *WorkspaceResolver) ResolveDefaultWorkspace() (*api.WorkspaceEntry, error) {
	if r == nil || r.observer == nil || r.registryPath == "" {
		r.auditDefaultWorkspace("warn", "serena-default-workspace-unavailable", map[string]any{"reason": "resolver_unwired"})
		return nil, ErrDefaultWorkspaceUnavailable
	}

	markedPath, err := api.ReadDefaultWorkspaceWithAuditSink(filepath.Dir(r.registryPath), r.auditSink())
	if err != nil {
		r.auditDefaultWorkspace("warn", "serena-default-workspace-unavailable", map[string]any{"reason": "marker_read_failed"})
		return nil, fmt.Errorf("%w: marker read", ErrDefaultWorkspaceUnavailable)
	}
	if markedPath == "" {
		return nil, ErrDefaultWorkspaceNotConfigured
	}
	canonical, err := api.CanonicalWorkspacePath(markedPath)
	if err != nil {
		r.auditDefaultWorkspace("info", "serena-default-workspace-stale", map[string]any{"reason": "marker_path_not_current"})
		return nil, ErrDefaultWorkspaceStale
	}
	view := r.observe()
	for i := 0; i < view.SerenaCount(); i++ {
		candidate, _ := view.SerenaCandidate(i)
		if candidate.WorkspacePath == canonical {
			entry, _ := view.Materialize(candidate)
			return &entry, nil
		}
	}
	r.auditDefaultWorkspace("info", "serena-default-workspace-stale", map[string]any{"reason": "registration_missing"})
	return nil, ErrDefaultWorkspaceStale
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
// The returned entry deep-copies only the selected row from one immutable
// observer generation; callers may mutate nested fields safely.
func (r *WorkspaceResolver) ResolveByPath(path string) (*api.WorkspaceEntry, error) {
	if path == "" {
		return nil, ErrInvalidPath
	}
	if isWindowsUNCPath(path) {
		return nil, ErrInvalidPath
	}
	view := r.observe()
	if view.SerenaCount() == 0 {
		return nil, ErrWorkspaceNotFound
	}
	if filepath.IsAbs(path) {
		return r.resolveAbsolute(path, view)
	}
	return r.resolveRelative(path, view)
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
// canonical WorkspacePath of each observer-owned Serena candidate.
//
// An unregistered workspace directory (marker present but no row in
// workspaces.yaml) returns ErrWorkspaceNotFound -- Phase E will own
// auto-register on miss; the resolver itself stays write-free.
func (r *WorkspaceResolver) resolveAbsolute(path string, view api.RegistrySnapshotView) (*api.WorkspaceEntry, error) {
	wsDir, err := r.AncestorWalk(path)
	if err != nil {
		return nil, err
	}
	canon, err := api.CanonicalWorkspacePath(wsDir)
	if err != nil {
		return nil, fmt.Errorf("%w: canonicalize ancestor %s: %v", ErrWorkspaceNotFound, wsDir, err)
	}
	for i := 0; i < view.SerenaCount(); i++ {
		candidate, _ := view.SerenaCandidate(i)
		entryPath := canonicalizeWorkspacePath(candidate.WorkspacePath)
		if entryPath == canon {
			entry, _ := view.Materialize(candidate)
			return &entry, nil
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
func (r *WorkspaceResolver) resolveRelative(rel string, view api.RegistrySnapshotView) (*api.WorkspaceEntry, error) {
	for i := 0; i < view.SerenaCount(); i++ {
		candidateRow, _ := view.SerenaCandidateByPath(i)
		if candidateRow.WorkspacePath == "" {
			continue
		}
		candidate := filepath.Join(candidateRow.WorkspacePath, rel)
		cleanCandidate := filepath.Clean(candidate)
		cleanWorkspace := filepath.Clean(candidateRow.WorkspacePath)
		relativeToWorkspace, err := filepath.Rel(cleanWorkspace, cleanCandidate)
		if err != nil || filepath.IsAbs(relativeToWorkspace) ||
			relativeToWorkspace == ".." ||
			strings.HasPrefix(relativeToWorkspace, ".."+string(os.PathSeparator)) {
			continue
		}
		if _, err := os.Lstat(candidate); err == nil {
			entry, _ := view.Materialize(candidateRow)
			return &entry, nil
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

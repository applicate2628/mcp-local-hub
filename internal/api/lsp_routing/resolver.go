package lsp_routing

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
)

const gitMarker = ".git"

// ResolveResult is the write-free path inference result for one
// (workspace, language) routing decision.
type ResolveResult struct {
	WorkspaceRoot string
	WorkspaceKey  string
	Registered    bool
	Entry         *api.WorkspaceEntry
	// ProjectMarker is true only when WorkspaceRoot was selected by one of
	// the language-specific project_markers from the manifest. A .git fallback
	// can resolve an already-registered row, but first-touch auto-register must
	// require this stronger marker.
	ProjectMarker bool
}

// WorkspaceResolver maps an absolute LSP tool-argument path plus language to a
// canonical workspace root and, when present, the matching LSP registry row.
//
// Project roots are discovered by language-specific marker files/directories
// declared in config.LanguageSpec.ProjectMarkers. If no language marker exists
// above the input path, the nearest .git ancestor is used as a universal VCS
// fallback.
//
// Concurrency: exported methods are safe for parallel use. Registry snapshots
// are refreshed when the registry file mtime advances, mirroring the serena
// resolver's best-effort reload behavior.
type WorkspaceResolver struct {
	reg          *api.Registry
	registryPath string
	markers      map[string][]string

	mu        sync.RWMutex
	lastMtime time.Time
	loaded    bool
	cached    []api.WorkspaceEntry
}

// NewWorkspaceResolver returns a resolver bound to reg, registryPath, and the
// manifest language specs that define project markers.
func NewWorkspaceResolver(reg *api.Registry, registryPath string, languages []config.LanguageSpec) *WorkspaceResolver {
	return &WorkspaceResolver{
		reg:          reg,
		registryPath: registryPath,
		markers:      markerMap(languages),
	}
}

// NewDefaultWorkspaceResolver constructs a resolver using api.DefaultRegistryPath.
func NewDefaultWorkspaceResolver(reg *api.Registry, languages []config.LanguageSpec) (*WorkspaceResolver, error) {
	p, err := api.DefaultRegistryPath()
	if err != nil {
		return nil, fmt.Errorf("lsp_routing: default registry path: %w", err)
	}
	return NewWorkspaceResolver(reg, p, languages), nil
}

func markerMap(languages []config.LanguageSpec) map[string][]string {
	out := make(map[string][]string, len(languages))
	for _, l := range languages {
		name := strings.TrimSpace(l.Name)
		if name == "" {
			continue
		}
		seen := map[string]bool{}
		for _, marker := range l.ProjectMarkers {
			marker = strings.TrimSpace(marker)
			if marker == "" || marker == gitMarker || seen[marker] {
				continue
			}
			out[name] = append(out[name], marker)
			seen[marker] = true
		}
	}
	return out
}

// ResolveByPath maps an absolute file or directory path plus language to a
// canonical workspace root. The result is returned even when no matching
// registry row exists yet, allowing the future router to auto-register on miss.
func (r *WorkspaceResolver) ResolveByPath(path, language string) (*ResolveResult, error) {
	if path == "" || strings.TrimSpace(language) == "" {
		return nil, ErrInvalidPath
	}
	if isWindowsUNCPath(path) {
		return nil, ErrInvalidPath
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%w: ResolveByPath requires an absolute path, got %q", ErrInvalidPath, path)
	}
	wsDir, err := r.AncestorWalk(path, language)
	if err != nil {
		return nil, err
	}
	canon, err := api.CanonicalWorkspacePath(wsDir)
	if err != nil {
		return nil, fmt.Errorf("%w: canonicalize ancestor %s: %v", ErrWorkspaceNotFound, wsDir, err)
	}
	wsKey := api.WorkspaceKey(canon)
	result := &ResolveResult{
		WorkspaceRoot: canon,
		WorkspaceKey:  wsKey,
		ProjectMarker: r.hasProjectMarker(wsDir, language),
	}
	if entry, ok := r.matchRegistrationForResolvedWorkspace(wsKey, language, canon, wsDir); ok {
		result.Registered = true
		result.Entry = &entry
		result.WorkspaceKey = entry.WorkspaceKey
	}
	return result, nil
}

func (r *WorkspaceResolver) hasProjectMarker(dir, language string) bool {
	for _, marker := range r.markersFor(language) {
		if markerExists(filepath.Join(dir, marker)) {
			return true
		}
	}
	return false
}

// HasProjectMarker reports whether root contains one of language's configured
// project markers. It intentionally does not fall back to .git.
func (r *WorkspaceResolver) HasProjectMarker(root, language string) bool {
	if root == "" || strings.TrimSpace(language) == "" {
		return false
	}
	return r.hasProjectMarker(root, language)
}

// RegisteredWorkspace reports whether the registry currently contains a row
// for the exact (workspaceKey, language) tuple.
func (r *WorkspaceResolver) RegisteredWorkspace(workspaceKey, language string) (*api.WorkspaceEntry, bool) {
	entry, ok := r.matchRegistration(workspaceKey, language)
	if !ok {
		return nil, false
	}
	return &entry, true
}

// AncestorWalk walks upward from absPath and returns the directory containing
// the nearest marker for language. If no language marker exists, it returns the
// nearest .git ancestor. Relative input returns ErrInvalidPath.
func (r *WorkspaceResolver) AncestorWalk(absPath, language string) (string, error) {
	if absPath == "" || strings.TrimSpace(language) == "" {
		return "", ErrInvalidPath
	}
	if !filepath.IsAbs(absPath) {
		return "", fmt.Errorf("%w: AncestorWalk requires an absolute path, got %q", ErrInvalidPath, absPath)
	}
	dir := absPath
	if fi, err := os.Lstat(absPath); err != nil || !fi.IsDir() {
		dir = filepath.Dir(absPath)
	}

	markers := r.markersFor(language)
	var gitRoot string
	for {
		for _, marker := range markers {
			if markerExists(filepath.Join(dir, marker)) {
				return dir, nil
			}
		}
		if gitRoot == "" && markerExists(filepath.Join(dir, gitMarker)) {
			gitRoot = dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			if gitRoot != "" {
				return gitRoot, nil
			}
			return "", ErrWorkspaceNotFound
		}
		dir = parent
	}
}

func (r *WorkspaceResolver) markersFor(language string) []string {
	if r == nil || r.markers == nil {
		return nil
	}
	markers := r.markers[language]
	out := make([]string, len(markers))
	copy(out, markers)
	return out
}

func markerExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func (r *WorkspaceResolver) matchRegistration(workspaceKey, language string) (api.WorkspaceEntry, bool) {
	snapshot := r.snapshotRegistry()
	return matchRegistrationInSnapshot(snapshot, workspaceKey, language)
}

func (r *WorkspaceResolver) matchRegistrationForResolvedWorkspace(workspaceKey, language, canonicalRoot, discoveredRoot string) (api.WorkspaceEntry, bool) {
	snapshot := r.snapshotRegistry()
	if entry, ok := matchRegistrationInSnapshot(snapshot, workspaceKey, language); ok {
		return entry, true
	}
	legacyRoot, err := api.CanonicalWorkspacePathLegacyCompat(discoveredRoot)
	if err == nil {
		legacyKey := api.WorkspaceKey(legacyRoot)
		if legacyKey != workspaceKey {
			if entry, ok := matchRegistrationInSnapshot(snapshot, legacyKey, language); ok {
				return entry, true
			}
		}
	}
	return matchRegistrationByWorkspacePathInSnapshot(snapshot, language, canonicalRoot, legacyRoot)
}

func matchRegistrationInSnapshot(snapshot *api.Registry, workspaceKey, language string) (api.WorkspaceEntry, bool) {
	for _, e := range snapshot.ListByWorkspaceLSP(workspaceKey) {
		if e.Language == language {
			return e, true
		}
	}
	return api.WorkspaceEntry{}, false
}

func matchRegistrationByWorkspacePathInSnapshot(snapshot *api.Registry, language string, workspacePaths ...string) (api.WorkspaceEntry, bool) {
	pathSet := make(map[string]bool, len(workspacePaths))
	for _, p := range workspacePaths {
		if key := workspacePathMatchKey(p); key != "" {
			pathSet[key] = true
		}
	}
	if len(pathSet) == 0 {
		return api.WorkspaceEntry{}, false
	}

	var found api.WorkspaceEntry
	matched := false
	for _, e := range snapshot.LSPEntries() {
		if e.Language != language || e.WorkspacePath == "" {
			continue
		}
		if !pathSet[workspacePathMatchKey(e.WorkspacePath)] {
			continue
		}
		if matched {
			return api.WorkspaceEntry{}, false
		}
		found = e
		matched = true
	}
	return found, matched
}

func workspacePathMatchKey(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" && len(path) >= 2 && path[1] == ':' {
		path = strings.ToLower(string(path[0])) + path[1:]
	}
	return path
}

func (r *WorkspaceResolver) snapshotRegistry() *api.Registry {
	if r == nil {
		return &api.Registry{}
	}
	r.refresh()
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]api.WorkspaceEntry, len(r.cached))
	copy(out, r.cached)
	return &api.Registry{Workspaces: out}
}

// refresh re-reads the registry file when its mtime has advanced past the last
// observed value. Reload failures preserve the previous cache.
func (r *WorkspaceResolver) refresh() {
	if r == nil || r.reg == nil || r.registryPath == "" {
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
		fmt.Fprintf(os.Stderr, "lsp_routing: stat registry %s: %v; preserving cached snapshot\n", r.registryPath, statErr)
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
	entries := r.reg.LSPEntries()

	r.mu.Lock()
	r.cached = entries
	r.loaded = true
	r.lastMtime = mtime
	r.mu.Unlock()
}

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

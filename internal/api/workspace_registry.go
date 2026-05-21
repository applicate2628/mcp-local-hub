package api

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"gopkg.in/yaml.v3"

	"mcp-local-hub/internal/config"
)

// Lifecycle enumerates the 5 observable states of a workspace-scoped daemon.
// Written by the lazy proxy; read by Status and CLI output.
const (
	LifecycleConfigured = "configured" // registry entry exists, proxy running, backend NOT spawned
	LifecycleStarting   = "starting"   // materialization in-flight (singleflight call active)
	LifecycleActive     = "active"     // backend materialized and healthy
	LifecycleMissing    = "missing"    // materialization attempted; LSP binary not on PATH
	LifecycleFailed     = "failed"     // materialization attempted; failed for any non-missing-binary reason
)

// MaxLastErrorBytes caps LastError to keep the YAML file compact and
// readable in `workspaces` output. Truncated mid-UTF8 is OK because the
// field is diagnostic-only.
const MaxLastErrorBytes = 200

// SerenaLanguageSentinel is the reserved Language value that marks a
// WorkspaceEntry as a serena (per-workspace dynamic-pool) row instead of
// a per-LSP-language row. The leading "@" prefix is invalid for LSP
// language names (the manifest validator and PutLSP both refuse the
// prefix per the B.1 dual-gate defense) which guarantees the sentinel
// cannot collide with a real LSP language.
const SerenaLanguageSentinel = "@serena"

// WorkspaceEntry is one (workspace_key, language) tuple in the registry.
// The tuple is unique; WorkspaceKey+Language is the primary key.
//
// A row with Language == SerenaLanguageSentinel is a per-workspace serena
// daemon row (B.1). Such rows carry the same TaskName / Port / Backend
// surface as LSP rows but use the optional Languages snapshot field to
// describe which languages the serena project covers. LSP-only consumers
// (membership UI, weekly-refresh index, `mcphub stop --daemon`, register's
// ListByWorkspace lookup) must filter sentinel rows.
type WorkspaceEntry struct {
	WorkspaceKey  string            `yaml:"workspace_key"`
	WorkspacePath string            `yaml:"workspace_path"`
	Language      string            `yaml:"language"`
	Backend       string            `yaml:"backend"` // "mcp-language-server", "gopls-mcp", or "serena"
	Port          int               `yaml:"port"`
	TaskName      string            `yaml:"task_name"`
	ClientEntries map[string]string `yaml:"client_entries"` // client-name -> entry-name-in-that-config
	WeeklyRefresh bool              `yaml:"weekly_refresh"`

	// Lazy-mode fields. All omitempty so earlier schemas round-trip safely.
	Lifecycle          string    `yaml:"lifecycle,omitempty"`
	LastMaterializedAt time.Time `yaml:"last_materialized_at,omitempty"`
	LastToolsCallAt    time.Time `yaml:"last_tools_call_at,omitempty"`
	LastError          string    `yaml:"last_error,omitempty"`

	// B.1 sentinel-row fields. Only meaningful when Language ==
	// SerenaLanguageSentinel. All omitempty so LSP rows and pre-B.1
	// schemas round-trip cleanly.
	RegisteredAt  time.Time `yaml:"registered_at,omitempty"`
	RegisteredVia string    `yaml:"registered_via,omitempty"` // "manual" | "auto-detect" | "migration"
	Languages     []string  `yaml:"languages,omitempty"`      // snapshot of .serena/project.yml at register time
}

// Registry is the on-disk source of truth for workspace-scoped daemons.
// Path is typically %LOCALAPPDATA%\mcp-local-hub\workspaces.yaml (Windows) or
// $XDG_STATE_HOME/mcp-local-hub/workspaces.yaml (Linux/macOS).
type Registry struct {
	path       string
	Version    int              `yaml:"version"`
	Workspaces []WorkspaceEntry `yaml:"workspaces"`
}

const registryVersion = 1

// NewRegistry returns a Registry bound to path. Caller must Load() before use.
func NewRegistry(path string) *Registry {
	return &Registry{path: path, Version: registryVersion}
}

// defaultRegistryPathFn is the test seam used by Codex deep-sec PR #135
// Finding 5 to inject a synthetic resolver failure (e.g. UserHomeDir error
// on a CI host that has no $HOME) without manipulating real env vars.
// Production callers reach the actual resolver through DefaultRegistryPath.
var defaultRegistryPathFn func() (string, error)

// DefaultRegistryPath returns the platform-appropriate registry path.
// Windows: %LOCALAPPDATA%\mcp-local-hub\workspaces.yaml
// Linux/macOS: $XDG_STATE_HOME/mcp-local-hub/workspaces.yaml
//
//	(fallback ~/.local/state/mcp-local-hub/workspaces.yaml)
func DefaultRegistryPath() (string, error) {
	if defaultRegistryPathFn != nil {
		return defaultRegistryPathFn()
	}
	if runtime.GOOS == "windows" {
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "mcp-local-hub", "workspaces.yaml"), nil
		}
	}
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "mcp-local-hub", "workspaces.yaml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "mcp-local-hub", "workspaces.yaml"), nil
}

// Load reads the registry file. A missing file is not an error — the registry
// stays empty, ready for the first Save.
func (r *Registry) Load() error {
	data, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			r.Version = registryVersion
			r.Workspaces = nil
			return nil
		}
		return fmt.Errorf("read registry %s: %w", r.path, err)
	}
	if len(data) == 0 {
		r.Version = registryVersion
		r.Workspaces = nil
		return nil
	}
	var parsed struct {
		Version    int              `yaml:"version"`
		Workspaces []WorkspaceEntry `yaml:"workspaces"`
	}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return fmt.Errorf("parse registry %s: %w", r.path, err)
	}
	r.Version = parsed.Version
	if r.Version == 0 {
		r.Version = registryVersion
	}
	r.Workspaces = parsed.Workspaces
	return nil
}

// Save writes the registry atomically: backup existing file to .bak, write to
// a temp file, rename into place. A crash between temp-write and rename leaves
// the previous file intact (os.Rename is atomic on same filesystem).
func (r *Registry) Save() error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0700); err != nil {
		return fmt.Errorf("mkdir registry dir: %w", err)
	}
	// Backup existing file (overwrite previous .bak — one rolling copy).
	if existing, err := os.ReadFile(r.path); err == nil {
		if werr := os.WriteFile(r.path+".bak", existing, 0600); werr != nil {
			return fmt.Errorf("write .bak: %w", werr)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read existing: %w", err)
	}
	if r.Version == 0 {
		r.Version = registryVersion
	}
	out, err := yaml.Marshal(struct {
		Version    int              `yaml:"version"`
		Workspaces []WorkspaceEntry `yaml:"workspaces"`
	}{Version: r.Version, Workspaces: r.Workspaces})
	if err != nil {
		return fmt.Errorf("marshal registry: %w", err)
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0600); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	// On Windows, os.Rename fails if the destination is open. Registry
	// callers hold no concurrent file handles across Save (Load closes
	// the file before returning), so a plain Rename is sufficient.
	if err := os.Rename(tmp, r.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename tmp -> live: %w", err)
	}
	return nil
}

// Lock acquires a cross-process exclusive file lock on <registry>.lock.
// The returned function releases the lock; callers must defer it. Prevents
// two concurrent `mcphub register` invocations from racing on port allocation
// or client-config writes.
func (r *Registry) Lock() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(r.path), 0700); err != nil {
		return nil, fmt.Errorf("mkdir registry dir: %w", err)
	}
	fl := flock.New(r.path + ".lock")
	if err := fl.Lock(); err != nil {
		return nil, fmt.Errorf("lock %s: %w", r.path+".lock", err)
	}
	return func() { _ = fl.Unlock() }, nil
}

// Put upserts an entry (primary key = workspace_key + language).
func (r *Registry) Put(e WorkspaceEntry) {
	for i := range r.Workspaces {
		if r.Workspaces[i].WorkspaceKey == e.WorkspaceKey && r.Workspaces[i].Language == e.Language {
			r.Workspaces[i] = e
			return
		}
	}
	r.Workspaces = append(r.Workspaces, e)
}

// Get returns the entry for (workspaceKey, language) or (zero, false).
func (r *Registry) Get(workspaceKey, language string) (WorkspaceEntry, bool) {
	for _, e := range r.Workspaces {
		if e.WorkspaceKey == workspaceKey && e.Language == language {
			return e, true
		}
	}
	return WorkspaceEntry{}, false
}

// Remove deletes the entry for (workspaceKey, language). No-op if absent.
func (r *Registry) Remove(workspaceKey, language string) {
	kept := r.Workspaces[:0]
	for _, e := range r.Workspaces {
		if e.WorkspaceKey == workspaceKey && e.Language == language {
			continue
		}
		kept = append(kept, e)
	}
	r.Workspaces = kept
}

// AllocatedPorts returns the set of ports currently assigned across all entries.
func (r *Registry) AllocatedPorts() map[int]bool {
	out := map[int]bool{}
	for _, e := range r.Workspaces {
		if e.Port > 0 {
			out[e.Port] = true
		}
	}
	return out
}

// ListByWorkspace returns every entry with the given workspace_key.
func (r *Registry) ListByWorkspace(workspaceKey string) []WorkspaceEntry {
	var out []WorkspaceEntry
	for _, e := range r.Workspaces {
		if e.WorkspaceKey == workspaceKey {
			out = append(out, e)
		}
	}
	return out
}

// PutLSP upserts an LSP-row entry. It rejects any Language value that
// begins with the reserved "@" prefix; @serena rows must use PutSerena
// (the dual-gate defense per plan B.1). Existing Put remains callable
// for low-level paths (rollback restoration of a prior row, lifecycle
// updates that re-write an already-validated entry).
func (r *Registry) PutLSP(e WorkspaceEntry) error {
	if strings.HasPrefix(e.Language, "@") {
		return fmt.Errorf("PutLSP: language %q is reserved (use PutSerena for sentinel rows)", e.Language)
	}
	r.Put(e)
	return nil
}

// PutSerena upserts a serena (sentinel) row. It refuses any Language
// value other than SerenaLanguageSentinel exactly — the explicit gate
// ensures no caller can write a different "@<x>" prefix and slip past
// PutLSP's check.
func (r *Registry) PutSerena(e WorkspaceEntry) error {
	if e.Language != SerenaLanguageSentinel {
		return fmt.Errorf("PutSerena: language must be %q exactly, got %q", SerenaLanguageSentinel, e.Language)
	}
	r.Put(e)
	return nil
}

// SerenaEntries returns every serena (sentinel) row.
func (r *Registry) SerenaEntries() []WorkspaceEntry {
	var out []WorkspaceEntry
	for _, e := range r.Workspaces {
		if e.Language == SerenaLanguageSentinel {
			out = append(out, e)
		}
	}
	return out
}

// LSPEntries returns every non-sentinel row (per-LSP-language rows).
func (r *Registry) LSPEntries() []WorkspaceEntry {
	var out []WorkspaceEntry
	for _, e := range r.Workspaces {
		if e.Language != SerenaLanguageSentinel {
			out = append(out, e)
		}
	}
	return out
}

// GetSerena returns the serena row for workspaceKey or (zero, false).
func (r *Registry) GetSerena(workspaceKey string) (WorkspaceEntry, bool) {
	return r.Get(workspaceKey, SerenaLanguageSentinel)
}

// ListByWorkspaceLSP returns every LSP-row entry for workspaceKey,
// filtering out the serena sentinel row.
func (r *Registry) ListByWorkspaceLSP(workspaceKey string) []WorkspaceEntry {
	var out []WorkspaceEntry
	for _, e := range r.Workspaces {
		if e.WorkspaceKey != workspaceKey {
			continue
		}
		if e.Language == SerenaLanguageSentinel {
			continue
		}
		out = append(out, e)
	}
	return out
}

// RemoveSerena drops the serena (sentinel) row for workspaceKey. No-op
// if no serena row is present.
func (r *Registry) RemoveSerena(workspaceKey string) {
	r.Remove(workspaceKey, SerenaLanguageSentinel)
}

// AllocateSerenaPort returns the first free port in pool that is NOT in
// the registry's AllocatedPorts set. Unlike AllocatePort, it does not
// attempt an OS-level bind probe — the serena daemon will bind the port
// itself at spawn time, and the in-flight CLI does not need the probe to
// reserve a port slot in workspaces.yaml. Returns ErrPortPoolExhausted
// when every port in pool is taken.
func (r *Registry) AllocateSerenaPort(pool config.PortPool) (int, error) {
	if pool.Start <= 0 || pool.End < pool.Start {
		return 0, fmt.Errorf("AllocateSerenaPort: invalid port pool {start=%d,end=%d}", pool.Start, pool.End)
	}
	taken := r.AllocatedPorts()
	for p := pool.Start; p <= pool.End; p++ {
		if taken[p] {
			continue
		}
		return p, nil
	}
	return 0, fmt.Errorf("%w: pool {%d..%d} fully allocated (%d registry entries)",
		ErrPortPoolExhausted, pool.Start, pool.End, len(taken))
}

// RemoveByBackend drops rows for workspaceKey filtered by backendFilter.
// Semantics align with `mcphub unregister <workspace> --backend <value>`:
//
//   - backendFilter == ""           → remove every LSP row (Language != SerenaLanguageSentinel); leaves serena row in place. This is the v5 default.
//   - backendFilter == "all"        → remove every row for workspaceKey (legacy pre-v5 semantic).
//   - backendFilter == "serena"     → remove only the serena (sentinel) row.
//   - any other value (e.g. "mcp-language-server" / "go" / "gopls-mcp") → remove only LSP rows whose Backend or Language field equals backendFilter.
//
// Returns the count of rows actually removed.
func (r *Registry) RemoveByBackend(workspaceKey string, backendFilter string) int {
	removed := 0
	kept := r.Workspaces[:0]
	for _, e := range r.Workspaces {
		if e.WorkspaceKey != workspaceKey {
			kept = append(kept, e)
			continue
		}
		drop := false
		switch backendFilter {
		case "":
			drop = e.Language != SerenaLanguageSentinel
		case "all":
			drop = true
		case "serena":
			drop = e.Language == SerenaLanguageSentinel
		default:
			drop = e.Language != SerenaLanguageSentinel &&
				(e.Backend == backendFilter || e.Language == backendFilter)
		}
		if drop {
			removed++
			continue
		}
		kept = append(kept, e)
	}
	r.Workspaces = kept
	return removed
}

// PutLifecycle loads the registry under lock, updates the lifecycle state +
// LastError for (workspaceKey, language), and saves. LastError is truncated
// to MaxLastErrorBytes. For transitions that need to stamp a timestamp
// (e.g., -> Active), use PutLifecycleWithTimestamps instead.
//
// Implementation note: delegates to PutLifecycleWithTimestamps with zero
// timestamps; the zero-time guards inside skip the timestamp assignments,
// preserving the "state + error only" contract.
func (r *Registry) PutLifecycle(workspaceKey, language, state, lastError string) error {
	return r.PutLifecycleWithTimestamps(workspaceKey, language, state, lastError, time.Time{}, time.Time{})
}

// PutLifecycleWithTimestamps is the richer variant used by the proxy at
// materialization edges: state transition + timestamps in one atomic save.
// Zero-valued materializedAt / toolsCallAt leave the existing stored values
// unchanged; non-zero values are coerced to UTC before write.
func (r *Registry) PutLifecycleWithTimestamps(workspaceKey, language, state, lastError string, materializedAt, toolsCallAt time.Time) error {
	unlock, err := r.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := r.Load(); err != nil {
		return err
	}
	e, ok := r.Get(workspaceKey, language)
	if !ok {
		// Silent no-op: Unregister removed the row while the proxy
		// process was still running and now emits a late lifecycle
		// write. Resurrecting a bare entry (no port/task/bindings)
		// would leave a ghost record in workspaces.yaml and status
		// output — breaks the Unregister contract.
		return nil
	}
	e.Lifecycle = state
	if len(lastError) > MaxLastErrorBytes {
		lastError = lastError[:MaxLastErrorBytes]
	}
	e.LastError = lastError
	if !materializedAt.IsZero() {
		e.LastMaterializedAt = materializedAt.UTC()
	}
	if !toolsCallAt.IsZero() {
		e.LastToolsCallAt = toolsCallAt.UTC()
	}
	r.Put(e)
	return r.Save()
}

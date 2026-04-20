package api

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/gofrs/flock"
	"gopkg.in/yaml.v3"
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

// WorkspaceEntry is one (workspace_key, language) tuple in the registry.
// The tuple is unique; WorkspaceKey+Language is the primary key.
type WorkspaceEntry struct {
	WorkspaceKey  string            `yaml:"workspace_key"`
	WorkspacePath string            `yaml:"workspace_path"`
	Language      string            `yaml:"language"`
	Backend       string            `yaml:"backend"` // "mcp-language-server" or "gopls-mcp"
	Port          int               `yaml:"port"`
	TaskName      string            `yaml:"task_name"`
	ClientEntries map[string]string `yaml:"client_entries"` // client-name -> entry-name-in-that-config
	WeeklyRefresh bool              `yaml:"weekly_refresh"`

	// Lazy-mode fields. All omitempty so earlier schemas round-trip safely.
	Lifecycle          string    `yaml:"lifecycle,omitempty"`
	LastMaterializedAt time.Time `yaml:"last_materialized_at,omitempty"`
	LastToolsCallAt    time.Time `yaml:"last_tools_call_at,omitempty"`
	LastError          string    `yaml:"last_error,omitempty"`
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

// DefaultRegistryPath returns the platform-appropriate registry path.
// Windows: %LOCALAPPDATA%\mcp-local-hub\workspaces.yaml
// Linux/macOS: $XDG_STATE_HOME/mcp-local-hub/workspaces.yaml
//
//	(fallback ~/.local/state/mcp-local-hub/workspaces.yaml)
func DefaultRegistryPath() (string, error) {
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

// PutLifecycle loads the registry under lock, updates the lifecycle state +
// LastError for (workspaceKey, language), and saves. LastError is truncated
// to MaxLastErrorBytes. If Lifecycle transitions to Active, the caller
// should also set LastMaterializedAt via PutLifecycleWithTimestamps.
func (r *Registry) PutLifecycle(workspaceKey, language, state, lastError string) error {
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
		e = WorkspaceEntry{WorkspaceKey: workspaceKey, Language: language}
	}
	e.Lifecycle = state
	if len(lastError) > MaxLastErrorBytes {
		lastError = lastError[:MaxLastErrorBytes]
	}
	e.LastError = lastError
	r.Put(e)
	return r.Save()
}

// PutLifecycleWithTimestamps is the richer variant used by the proxy at
// materialization edges: state transition + timestamps in one atomic save.
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
		e = WorkspaceEntry{WorkspaceKey: workspaceKey, Language: language}
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

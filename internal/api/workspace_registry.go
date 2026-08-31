package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

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

// DefaultLSPMaterializedHardCap is the shared process-wide cap for
// materialized LSP backends. The daemon enforces it for organic tools/call
// materialization; status force-materialize uses it as a probe concurrency
// limit so explicit all-row probes do not self-deny against the same cap.
const DefaultLSPMaterializedHardCap = 16

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

	// PendingSerenaRemoval, its timestamp, and generation form one tuple.
	// Registry.BeginSerenaPendingRemoval stages that exact tuple after fence
	// acquisition and generation publication and immediately before intent
	// teardown. Successful teardown deletes the row. Any begin, intent-teardown,
	// or row-delete failure invokes the returned ownership-aware rollback, which
	// restores the exact prior tuple only while the attempt still owns it, skips
	// deleted rows, and preserves a later writer with a typed conflict. The fence
	// is released only after row deletion or rollback has reached its verdict.
	//
	// A pending tuple is not an unconditional skip. Repair consults the fence: a
	// held owner is preserved; a free matching generation is reclaimable;
	// unattributed legacy state follows the lease fallback; probe failure or
	// mismatched fresh provenance remains incomplete and fail-closed.
	PendingSerenaRemoval bool `yaml:"pending_serena_removal,omitempty"`

	// PendingSerenaRemovalAt is the UTC timestamp in the pending-removal tuple.
	PendingSerenaRemovalAt time.Time `yaml:"pending_serena_removal_at,omitempty"`

	// PendingSerenaRemovalGeneration is the removal-fence generation in the
	// pending-removal tuple. Empty preserves unattributed legacy state.
	PendingSerenaRemovalGeneration string `yaml:"pending_serena_removal_generation,omitempty"`
}

// Registry is the on-disk source of truth for workspace-scoped daemons.
// Path is typically %LOCALAPPDATA%\mcp-local-hub\workspaces.yaml (Windows) or
// $XDG_STATE_HOME/mcp-local-hub/workspaces.yaml (Linux/macOS).
type Registry struct {
	path       string
	Version    int              `yaml:"version"`
	Workspaces []WorkspaceEntry `yaml:"workspaces"`
	// lockFn is an instance-local test seam for deterministic release-error
	// settlement. Production registries leave it nil and use the ledgered leaf.
	lockFn func(string) (func() error, error)
	// savePendingRemovalFn is an internal test seam for the commit-unknown
	// registry writer case: a durable rename can succeed before reopening the
	// replacement file reports an error.
	savePendingRemovalFn func(*Registry) error

	// auditSink, when non-nil, is the destination Load() passes to the
	// shared inode-anchored state-file reader for its relax-fallback
	// diagnostic (finding 1, work-items/bugs/2026-07-26-route-daemon-
	// state-read-unhardened-parent-fallback-writes-hub-mcp-log.md). Nil
	// (the zero value every existing NewRegistry caller gets) preserves
	// today's default: LogHubMcpEvent, the shared hub-mcp.log. Purely
	// additive — see SetAuditSink.
	auditSink func(level, event string, fields map[string]any) error
	// beforeSerenaActivityCommitLockFn is an internal deterministic test hook
	// for the pre-lock replacement race. Production leaves it nil.
	beforeSerenaActivityCommitLockFn func()
	// afterSerenaActivityRegistryLockFn exposes the established lock-order
	// checkpoint to the lock-order regression test. Production leaves it nil.
	afterSerenaActivityRegistryLockFn func()
	// afterSerenaActivityIntentReadBeforeSaveFn is an internal deterministic
	// test hook for the intent-to-registry-save window. Production leaves it nil.
	afterSerenaActivityIntentReadBeforeSaveFn func()
}

// SetAuditSink overrides the diagnostic sink Load() uses for the shared
// state-file reader's relax-fallback event. A nil sink restores the default
// (LogHubMcpEvent). The read-only `mcphub route` front daemon
// (internal/cli/route.go) calls this with RouteReadOnlyStderrSink right
// after NewRegistry, before the first Load(), so its registry reads never
// reach the GUI-owned shared hub-mcp.log even under a default-relax
// broadened parent — every other caller that never calls SetAuditSink keeps
// today's behavior unchanged.
func (r *Registry) SetAuditSink(sink func(level, event string, fields map[string]any) error) {
	r.auditSink = sink
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
	data, err := ReadStateFileInodeAnchoredWithAuditSink(r.path, r.auditSink)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || isHubMcpStateMissingErr(err) {
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

// Save writes the registry atomically through a temp file and rename into
// place. A crash between temp-write and rename leaves the previous file intact
// (os.Rename is atomic on same filesystem).
func (r *Registry) Save() error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0700); err != nil {
		return fmt.Errorf("mkdir registry dir: %w", err)
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
	// Route the live-file write through the single hardened state-file owner
	// (G9 P3): handle-bound DACL + parent-dir relax-gate + audit event, instead
	// of the prior hand-rolled os.WriteFile(0600)+os.Rename. Use the LOCK-HELD
	// variant: Save() takes NO internal flock (its original contract), because
	// the registry's own Lock() (below) flocks the SAME leaf, r.path+".lock",
	// that WriteStateFileBytesAtomic would acquire — and many callers
	// (PutLifecycle*, membership updates, register/unregister, migrate rollback)
	// already hold r.Lock() before calling Save(). gofrs/flock is non-reentrant,
	// so acquiring it again here self-deadlocks (codex review of the Phase 2a
	// integration; TestRegistry_LockPreventsSimultaneousWriters). Exclusivity
	// stays the caller's job via r.Lock(), exactly as before this hardening.
	if err := WriteStateFileBytesLockHeld(r.path, out); err != nil {
		return fmt.Errorf("write registry: %w", err)
	}
	return nil
}

// RemoveSerenaRowsAndSave removes the named Serena rows and persists the
// registry while the caller holds r.Lock. If the hardened writer reports an
// error after publication, it re-reads the live file under that same lock and
// distinguishes a committed deletion from an uncertain mutation.
func (r *Registry) RemoveSerenaRowsAndSave(workspaceKeys ...string) (removed int, committed bool, err error) {
	for _, key := range workspaceKeys {
		removed += r.RemoveByBackend(key, "serena")
	}
	if err := r.Save(); err != nil {
		live := NewRegistry(r.path)
		if loadErr := live.Load(); loadErr != nil {
			return 0, false, errors.Join(err, fmt.Errorf("re-read registry after commit-unknown Serena removal: %w", loadErr))
		}
		for _, key := range workspaceKeys {
			if _, present := live.GetSerena(key); present {
				return 0, false, err
			}
		}
		return removed, true, err
	}
	return removed, true, nil
}

// LockPath returns the registry's single cross-process lock leaf.
func (r *Registry) LockPath() string { return r.path + ".lock" }

// Lock acquires a cross-process exclusive file lock on <registry>.lock.
// The returned one-shot release reports and records failure.
func (r *Registry) Lock() (func() error, error) {
	lockPath := r.LockPath()
	if err := os.MkdirAll(filepath.Dir(r.path), 0700); err != nil {
		return nil, fmt.Errorf("mkdir registry dir: %w", err)
	}
	if r.lockFn != nil {
		return r.lockFn(lockPath)
	}
	release, err := lockLeafLedgered(lockPath)
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", lockPath, err)
	}
	return release, nil
}

// TryLock is the non-blocking variant of Lock: it attempts to acquire the
// cross-process exclusive lock on <registry>.lock WITHOUT waiting. When the
// lock is free it returns (unlock, true, nil) — the caller must defer the
// unlock. When another process already holds the lock it returns
// (nil, false, nil) so a best-effort caller can SKIP rather than block on a
// hung holder. A real filesystem error (mkdir / flock syscall failure)
// returns (nil, false, err).
//
// Used by RepairSerenaIntentFromRegistry: a supervisor-startup self-heal must
// never stall on a registry lock held by a concurrent auto-register / migrate
// (that holder self-heals the orphan anyway), so it TryLocks and skips on
// contention.
func (r *Registry) TryLock() (func() error, bool, error) {
	lockPath := r.LockPath()
	if err := os.MkdirAll(filepath.Dir(r.path), 0700); err != nil {
		return nil, false, fmt.Errorf("mkdir registry dir: %w", err)
	}
	release, locked, err := tryLockLeafLedgered(lockPath)
	if err != nil {
		return nil, false, fmt.Errorf("try-lock %s: %w", lockPath, err)
	}
	if !locked {
		return nil, false, nil
	}
	return release, true, nil
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

// ErrSerenaPendingRemovalRollbackConflict reports that a pending-removal tuple
// changed after this transaction staged it. The later writer owns that tuple,
// so rollback deliberately leaves it untouched.
var ErrSerenaPendingRemovalRollbackConflict = errors.New("serena pending-removal rollback ownership conflict")

// ErrSerenaPendingRemovalTargetAbsent reports that no canonical or legacy
// Serena row remained when teardown tried to stage its ownership tuple. The
// caller must stop before removing supervisor intent: another actor may have
// deleted and re-registered the workspace since the earlier presence check.
var ErrSerenaPendingRemovalTargetAbsent = errors.New("serena pending-removal target absent")

// SerenaPendingRemovalRollbackConflict identifies the row a rollback could not
// restore because its tuple no longer belongs to the staging transaction.
type SerenaPendingRemovalRollbackConflict struct {
	WorkspaceKey string
}

func (e *SerenaPendingRemovalRollbackConflict) Error() string {
	return fmt.Sprintf("%v: workspace key %q", ErrSerenaPendingRemovalRollbackConflict, e.WorkspaceKey)
}

func (e *SerenaPendingRemovalRollbackConflict) Unwrap() error {
	return ErrSerenaPendingRemovalRollbackConflict
}

type serenaPendingRemovalTuple struct {
	pending    bool
	at         time.Time
	generation string
}

func serenaPendingRemovalTupleFor(e WorkspaceEntry) serenaPendingRemovalTuple {
	return serenaPendingRemovalTuple{
		pending:    e.PendingSerenaRemoval,
		at:         e.PendingSerenaRemovalAt,
		generation: e.PendingSerenaRemovalGeneration,
	}
}

func (t serenaPendingRemovalTuple) equal(other serenaPendingRemovalTuple) bool {
	return t.pending == other.pending && t.at.Equal(other.at) && t.generation == other.generation
}

// BeginSerenaPendingRemoval stages one exact pending-removal tuple for the
// canonical workspace key and, when distinct, its legacy key. It snapshots the
// exact prior tuples under the registry lock and returns a rollback that restores
// only rows still carrying this attempt's tuple. Save errors are commit-unknown:
// the atomic rename may already have published the staged rows, so staging has a
// usable rollback even when this method returns an error.
// This method is the sole production owner of active-teardown tuple staging and
// rollback.
//
// A missing row is never recreated and returns
// ErrSerenaPendingRemovalTargetAbsent so teardown cannot continue without a
// row-owned mark. A row whose tuple differs from both the snapshot and this
// attempt belongs to another writer; rollback reports a typed conflict and
// preserves it.
func (r *Registry) BeginSerenaPendingRemoval(wsKey, legacyWSKey, generation string) (rollback func() error, err error) {
	if generation != "" && !validSerenaRemovalFenceGeneration(generation) {
		return nil, fmt.Errorf("begin pending serena removal generation: malformed generation")
	}

	unlock, err := r.Lock()
	if err != nil {
		return nil, err
	}
	defer ReleaseAndJoin(&err, unlock, "begin pending serena removal: release registry lock")
	if err := r.Load(); err != nil {
		return nil, err
	}

	attempt := serenaPendingRemovalTuple{
		pending:    true,
		at:         time.Now().UTC(),
		generation: generation,
	}
	before := make(map[string]serenaPendingRemovalTuple)
	for _, key := range dedupeWorkspaceKeys(wsKey, legacyWSKey) {
		e, ok := r.GetSerena(key)
		if !ok {
			continue
		}
		before[key] = serenaPendingRemovalTupleFor(e)
		e.PendingSerenaRemoval = attempt.pending
		e.PendingSerenaRemovalAt = attempt.at
		e.PendingSerenaRemovalGeneration = attempt.generation
		r.Put(e)
	}
	if len(before) == 0 {
		return nil, ErrSerenaPendingRemovalTargetAbsent
	}

	rollback = func() (err error) {
		current := NewRegistry(r.path)
		unlock, err := current.Lock()
		if err != nil {
			return err
		}
		defer ReleaseAndJoin(&err, unlock, "rollback pending serena removal: release registry lock")
		if err := current.Load(); err != nil {
			return err
		}

		changed := false
		var conflicts []error
		for key, prior := range before {
			e, ok := current.GetSerena(key)
			if !ok {
				continue // a successful row delete must not be undone.
			}
			got := serenaPendingRemovalTupleFor(e)
			switch {
			case got.equal(attempt):
				e.PendingSerenaRemoval = prior.pending
				e.PendingSerenaRemovalAt = prior.at
				e.PendingSerenaRemovalGeneration = prior.generation
				current.Put(e)
				changed = true
			case got.equal(prior):
				// Already restored by an earlier rollback invocation.
			default:
				conflicts = append(conflicts, &SerenaPendingRemovalRollbackConflict{WorkspaceKey: key})
			}
		}
		if changed {
			if err := current.Save(); err != nil {
				conflicts = append(conflicts, err)
			}
		}
		return errors.Join(conflicts...)
	}

	if err := r.savePendingRemoval(); err != nil {
		return rollback, err
	}
	return rollback, nil
}

func (r *Registry) savePendingRemoval() error {
	if r.savePendingRemovalFn != nil {
		return r.savePendingRemovalFn(r)
	}
	return r.Save()
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

// PutLastToolsCallAt loads the registry under lock, updates only
// LastToolsCallAt for (workspaceKey, language), and saves. It preserves
// Lifecycle and LastError because callers may stamp successful activity without
// implying a lifecycle transition or clearing a diagnostic. A zero timestamp is
// a no-op, and a missing row is silently ignored to match PutLifecycle's
// unregistered-late-write behavior.
func (r *Registry) PutLastToolsCallAt(workspaceKey, language string, toolsCallAt time.Time) (err error) {
	if toolsCallAt.IsZero() {
		return nil
	}
	unlock, err := r.Lock()
	if err != nil {
		return err
	}
	defer ReleaseAndJoin(&err, unlock, "put last tools-call timestamp for "+workspaceKey+"/"+language+": write stands, but could not release the registry lock")
	if err := r.Load(); err != nil {
		return err
	}
	e, ok := r.Get(workspaceKey, language)
	if !ok {
		return nil
	}
	e.LastToolsCallAt = toolsCallAt.UTC()
	r.Put(e)
	return r.Save()
}

// ErrSerenaActivityTargetStale means the workspace generation or its matching
// supervisor descriptor changed before a route-originated activity commit could
// acquire the registry lock. The caller must not forward as that activity no
// longer belongs to the resolved daemon generation.
var ErrSerenaActivityTargetStale = errors.New("serena activity target stale")

// CommitSerenaActivity performs the complete Serena activity decision under
// two locks in the established registry-then-supervisor-intent order: reload,
// validate the exact workspace generation and its matching descriptor, perform
// a monotonic timestamp update, then save and return a deterministic receipt.
// It intentionally does not reuse PutLastToolsCallAt after a separate precheck:
// that split would let a replacement row with the same key receive a stale
// activity timestamp.
func (r *Registry) CommitSerenaActivity(intentPath string, request SerenaActivityCommitRequestV1) (receipt SerenaActivityCommitReceiptV1, err error) {
	if err := validateSerenaActivityCommitRequest(request); err != nil {
		return SerenaActivityCommitReceiptV1{}, err
	}
	if r.beforeSerenaActivityCommitLockFn != nil {
		r.beforeSerenaActivityCommitLockFn()
	}
	release, err := r.Lock()
	if err != nil {
		return SerenaActivityCommitReceiptV1{}, err
	}
	defer ReleaseAndJoin(&err, release, "commit serena activity: release registry lock")
	if r.afterSerenaActivityRegistryLockFn != nil {
		r.afterSerenaActivityRegistryLockFn()
	}
	intentRelease, err := lockSupervisorIntent(intentPath)
	if err != nil {
		return SerenaActivityCommitReceiptV1{}, fmt.Errorf("lock supervisor intent: %w", err)
	}
	defer releaseSupervisorIntentAndJoin(&err, intentRelease, "commit serena activity: release supervisor-intent lock")
	if err := r.Load(); err != nil {
		return SerenaActivityCommitReceiptV1{}, err
	}
	entry, ok := r.Get(request.WorkspaceKey, SerenaLanguageSentinel)
	if !ok || !serenaActivityTargetMatches(entry, request) {
		return SerenaActivityCommitReceiptV1{}, ErrSerenaActivityTargetStale
	}
	intent, readErr := ReadSupervisorIntent(intentPath)
	if readErr != nil {
		return SerenaActivityCommitReceiptV1{}, fmt.Errorf("read supervisor intent: %w", readErr)
	}
	if !serenaActivityIntentMatches(intent, request) {
		return SerenaActivityCommitReceiptV1{}, ErrSerenaActivityTargetStale
	}
	state := "committed"
	if !entry.LastToolsCallAt.IsZero() && !entry.LastToolsCallAt.Before(request.ActivityAt) {
		state = "already_committed"
	} else {
		if r.afterSerenaActivityIntentReadBeforeSaveFn != nil {
			r.afterSerenaActivityIntentReadBeforeSaveFn()
		}
		// The held intent lock excludes normal writers. Re-read before the
		// registry save as a fail-closed guard for a damaged/bypassing writer
		// observed by the deterministic test seam; never compensate after save.
		intent, readErr = ReadSupervisorIntent(intentPath)
		if readErr != nil {
			return SerenaActivityCommitReceiptV1{}, fmt.Errorf("re-read supervisor intent: %w", readErr)
		}
		if !serenaActivityIntentMatches(intent, request) {
			return SerenaActivityCommitReceiptV1{}, ErrSerenaActivityTargetStale
		}
		entry.LastToolsCallAt = request.ActivityAt.UTC()
		r.Put(entry)
		if err := r.Save(); err != nil {
			return SerenaActivityCommitReceiptV1{}, err
		}
	}
	return SerenaActivityCommitReceiptV1{
		ProtocolVersion: 1,
		WorkspaceKey:    request.WorkspaceKey,
		TaskName:        request.TaskName,
		RegisteredAt:    request.RegisteredAt.UTC(),
		ActivityAt:      request.ActivityAt.UTC(),
		State:           state,
	}, nil
}

func serenaActivityTargetMatches(entry WorkspaceEntry, request SerenaActivityCommitRequestV1) bool {
	return entry.Language == SerenaLanguageSentinel && entry.Backend == SerenaServerName &&
		entry.WorkspaceKey == request.WorkspaceKey && entry.WorkspacePath == request.WorkspacePath &&
		entry.TaskName == request.TaskName && entry.Port == request.ExpectedPort && entry.RegisteredAt.Equal(request.RegisteredAt)
}

func serenaActivityIntentMatches(intent *SupervisorIntentFile, request SerenaActivityCommitRequestV1) bool {
	if intent == nil {
		return false
	}
	for _, daemon := range intent.Daemons {
		if daemon.TaskName == request.TaskName && daemon.Workspace == request.WorkspacePath && daemon.Port == request.ExpectedPort {
			return true
		}
	}
	return false
}

// PutLifecycleWithTimestamps is the richer variant used by the proxy at
// materialization edges: state transition + timestamps in one atomic save.
// Zero-valued materializedAt / toolsCallAt leave the existing stored values
// unchanged; non-zero values are coerced to UTC before write.
func (r *Registry) PutLifecycleWithTimestamps(workspaceKey, language, state, lastError string, materializedAt, toolsCallAt time.Time) (err error) {
	unlock, err := r.Lock()
	if err != nil {
		return err
	}
	defer ReleaseAndJoin(&err, unlock, "put lifecycle for "+workspaceKey+"/"+language+": write stands, but could not release the registry lock")
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

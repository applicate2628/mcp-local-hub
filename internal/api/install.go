package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/process"
	"mcp-local-hub/internal/scheduler"
	"mcp-local-hub/internal/secrets"
)

// mcphubShortName is the bare executable name. Used for Antigravity relay
// entries (subprocess spawners like Node's child_process do honor PATH) and
// for the install preflight "is mcphub on PATH?" check.
var mcphubShortName = func() string {
	if runtime.GOOS == "windows" {
		return "mcphub.exe"
	}
	return "mcphub"
}()

// canonicalMcphubPath returns the absolute path at which `mcphub setup`
// installs the binary: ~/.local/bin/mcphub.exe (Windows) or
// ~/.local/bin/mcphub (Linux/macOS). Scheduler tasks use this path as their
// <Command> because Windows Task Scheduler's CreateProcess call sets
// lpApplicationName — which skips PATH search entirely — so a bare
// "mcphub.exe" Command fails with ERROR_FILE_NOT_FOUND even when PATH
// contains the canonical dir. The path is user-canonical (depends only on
// $HOME / %USERPROFILE%), not dev-location-specific: moving or rebuilding
// the binary and re-running `mcphub setup` keeps scheduler tasks valid
// without any rewrite.
// testCanonicalMcphubPathOverride is the test seam for canonicalMcphubPath.
// Production leaves it empty; unit tests that need a deterministic (or
// deliberately missing) binary path set it in their setup and restore
// in defer.
var testCanonicalMcphubPathOverride string

func canonicalMcphubPath() (string, error) {
	if testCanonicalMcphubPathOverride != "" {
		return testCanonicalMcphubPathOverride, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".local", "bin", mcphubShortName), nil
}

func ensureCanonicalMcphubPresent() (string, error) {
	canonicalPath, err := canonicalMcphubPath()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(canonicalPath); err != nil {
		return "", fmt.Errorf("%s not present — run `mcphub setup` once to install the canonical binary: %w", canonicalPath, err)
	}
	return canonicalPath, nil
}

// Plan describes the side effects that `mcp install --server X` would produce.
// Returned by BuildPlan and rendered by `install --dry-run`.
//
// v0.5.0 Phase 12: SupervisorIntent is the new authoritative seam that
// downstream consumers (migration journal, install reconciler, the
// status-seam IPC backing in health.go) read for "what daemons should
// the supervisor own?". SchedulerTasks is preserved for backward compat
// during the v0.5.x transition — existing call sites (executeInstallTo
// Step 1 at install.go:1589-1632, the prune-set construction at
// install.go:1638-1652, printPlanTo at install.go:1540-1553) continue
// using SchedulerTasks unchanged. A later release removes the legacy
// field once all consumers migrate.
//
// Spec §"Q12 CLI/GUI status seam" + plan §2611-2644.
type Plan struct {
	Server           string
	SchedulerTasks   []ScheduledTaskPlan     // DEPRECATED: kept for v0.5.x backward compat; replaced by SupervisorIntent in v0.6+.
	SupervisorIntent []SupervisorIntentEntry // v0.5.0 Phase 12 — authoritative for new supervisor-intent.json consumers.
	ClientUpdates    []ClientUpdatePlan
	// FullInstall is true when BuildPlan was called with an empty daemonFilter
	// — i.e. the plan covers the whole manifest. Only a full install can
	// safely reconcile (prune) obsolete sibling scheduler tasks from prior
	// installs; a partial install targets one daemon and must leave others
	// alone.
	FullInstall bool
}

type ScheduledTaskPlan struct {
	Name    string
	Command string
	Args    []string
	Trigger string // human-readable
}

// ClientUpdateAction is the typed enum for ClientUpdatePlan.Action. The
// raw string values are kept stable (`add/replace` / `remove`) so logs,
// dry-run output, and JSON debug dumps remain wire-compatible with the
// pre-Phase 5 plan shape. Spec §"Bidirectional install reconciler".
type ClientUpdateAction string

const (
	// ClientUpdateAddReplace adds the entry (or replaces it in place if
	// already present). Used both by the per-server install planner
	// (per-daemon entries) and the full-reconcile gate-ON planner
	// (mcphub-hub aggregate entry).
	ClientUpdateAddReplace ClientUpdateAction = "add/replace"

	// ClientUpdateRemove removes the named entry. ONLY the full-
	// reconcile planner (BuildHubReconcilePlan) emits this; per-server
	// install paths must leave existing entries alone — including the
	// mcphub-hub aggregate (codex r3 general F2 closure).
	ClientUpdateRemove ClientUpdateAction = "remove"
)

// ClientUpdatePlan describes one client-config side effect produced by
// either the per-server install planner (BuildPlan / BuildPlanWithOpts)
// or the full-reconcile planner (BuildHubReconcilePlan). The applier
// (ApplyHubReconcileInOrder for the reconcile path; executeInstallTo
// for the per-server path) consumes the same shape.
//
// EntryName carries the server-name (per-daemon entries) or the
// constant "mcphub-hub" (aggregate entry created on gate ON). The
// applier routes the write to the right adapter method using
// (Action, EntryName).
//
// Headers is populated only for the aggregate entry on gate ON; it
// carries the per-client X-Mcphub-Hub-Token plus the X-Mcphub-Instance-Id
// header the Phase 4 auth gate validates. Per-daemon entries leave
// Headers empty — they hit the daemon directly with no auth header.
//
// DisplayURL carries a redacted form of URL suitable for plan + install
// stdout. For local manifests it equals URL. For transport=remote-http
// it is the manifest's literal pre-expansion URL (with ${secret:KEY}
// placeholders intact) so a path/query token never reaches the
// operator's terminal even when URL embeds it (codex cumulative G6
// review P2 closure — URL-as-secret-bearer is rare but real).
type ClientUpdatePlan struct {
	Client     string
	Path       string
	Action     ClientUpdateAction
	EntryName  string            // "mcphub-hub" for aggregate; "<server>" for per-daemon
	URL        string            // empty for Remove
	DisplayURL string            // safe-to-print form of URL; falls back to URL when not set
	Headers    map[string]string // F-G5: token + instance id; empty for per-daemon
	DaemonName string            // legacy; only meaningful for per-daemon entries
}

// InstallOpts controls an install invocation.
type InstallOpts struct {
	Server            string
	DaemonFilter      string   // empty = all daemons in the manifest
	ClientsInclude    []string // empty = default install clients
	IncludeAllClients bool
	DryRun            bool
	Writer            io.Writer // progress output destination; nil = os.Stderr
}

// InstallAllOpts controls a bulk install.
type InstallAllOpts struct {
	ManifestDir       string
	ClientsInclude    []string
	IncludeAllClients bool
	DryRun            bool
	Writer            io.Writer
}

// InstallResult is one row in an InstallAll report.
type InstallResult struct {
	Server string
	Err    error
}

// UninstallReport summarizes what Uninstall actually did. Callers (CLI/GUI)
// render this however they like; the API itself does not print.
type UninstallReport struct {
	Server          string
	TasksDeleted    []string
	TaskDeleteWarns []string
	ClientsUpdated  []string
	ClientWarns     []string
}

// refuseWorkspaceScopedInstall returns a clear error when someone tries to
// `mcphub install --server mcp-language-server`. Workspace-scoped manifests
// require `mcphub register <workspace> [language...]` — there is no implicit
// install semantic for them because the (workspace, language) tuples that a
// workspace-scoped server needs cannot be inferred from the manifest alone.
// Callers may pass a writer for a human-friendly surface; the returned error
// is the machine-readable signal.
func refuseWorkspaceScopedInstall(m *config.ServerManifest, w io.Writer) error {
	if m.Kind != config.KindWorkspaceScoped {
		return nil
	}
	if w != nil {
		fmt.Fprintf(w, "Server %q is workspace-scoped; use `mcphub register <workspace> [language...]` instead of `mcphub install`.\n", m.Name)
	}
	return fmt.Errorf("server %q is workspace-scoped; use `mcphub register <workspace> [language...]`", m.Name)
}

// Install performs the full install flow for one server: reads manifest,
// runs preflight, builds plan, creates scheduler tasks, writes client configs,
// starts daemons.
//
// Task 10 wiring (plan v13 §62 audit-first canonical timing): on a
// non-dry-run path the function emits one Action=server-install audit
// entry per planned scheduler task BEFORE any scheduler / intent / client
// mutation. If audit append fails (incl. ErrIdentityOversize per §51 +
// §62), the install is REJECTED with a wrapped error and end-state is
// identical to never-attempted install. After a successful install the
// function records Desired=running intent for each created task; intent
// failures there are logged + tolerated because the install already
// happened.
func (a *API) Install(opts InstallOpts) error {
	w := opts.Writer
	if w == nil {
		w = os.Stderr
	}
	// 1. Load manifest (embed FS first, disk fallback for dev flow).
	//    The canonical installed binary resolves manifests from its
	//    embedded FS so an install launched from any cwd finds the same
	//    10 servers the daemon sees — previously install opened disk
	//    and failed or saw a stale subset.
	data, err := loadManifestYAMLEmbedFirst(opts.Server)
	if err != nil {
		return fmt.Errorf("load manifest %s: %w", opts.Server, err)
	}
	m, err := parseManifestForName(opts.Server, data)
	if err != nil {
		return err
	}
	// 1a. Reject workspace-scoped manifests at the Install entrypoint.
	// These require the explicit per-workspace `mcphub register` flow.
	if err := refuseWorkspaceScopedInstall(m, w); err != nil {
		return err
	}
	// 2. Preflight.
	if err := Preflight(m, opts.DaemonFilter); err != nil {
		return err
	}
	// 3. Build plan.
	plan, err := BuildPlanWithOpts(m, BuildPlanOpts{
		DaemonFilter:      opts.DaemonFilter,
		ClientsInclude:    opts.ClientsInclude,
		IncludeAllClients: opts.IncludeAllClients,
	})
	if err != nil {
		return err
	}
	// 4. Dry-run + audit-first + execute via the shared core. v0.6 Phase F:
	// global daemons spawn from supervisor-intent.json (installPlanCore writes
	// the descriptor rows + defers the spawn to the supervisor reconcile loop)
	// rather than from per-daemon scheduler tasks.
	return a.installPlanCore(context.Background(), m, plan, opts.DaemonFilter, opts.DryRun, w)
}

// InstallAll is the production entry point for bulk install. Reads
// the authoritative server list from the embed FS (with disk union
// for dev flow) so the canonical installed binary behaves identically
// regardless of cwd or whether a dev source tree sits nearby.
//
// Workspace-scoped manifests are skipped silently — not a failure, just
// not this command's job. Such servers require the explicit per-workspace
// `mcphub register` flow; a notice is emitted to w so the user knows why
// those names were omitted.
func (a *API) InstallAll(dryRun bool, w io.Writer) []InstallResult {
	return a.InstallAllWithOpts(InstallAllOpts{DryRun: dryRun, Writer: w})
}

func (a *API) InstallAllWithOpts(opts InstallAllOpts) []InstallResult {
	names, err := listManifestNamesEmbedFirst()
	if err != nil {
		return []InstallResult{{Err: err}}
	}
	var results []InstallResult
	var skipped []string
	for _, name := range names {
		// Probe the manifest kind cheaply. A parse error here is not
		// fatal — the normal install path will surface the same error
		// with its usual wrapping — so we fall through on failure.
		if data, derr := loadManifestYAMLEmbedFirst(name); derr == nil {
			if mf, perr := parseManifestForName(name, data); perr == nil && mf.Kind == config.KindWorkspaceScoped {
				skipped = append(skipped, name)
				continue
			}
		}
		err := a.installUsingEmbedFirst(InstallOpts{
			Server:            name,
			ClientsInclude:    opts.ClientsInclude,
			IncludeAllClients: opts.IncludeAllClients,
			DryRun:            opts.DryRun,
			Writer:            opts.Writer,
		})
		results = append(results, InstallResult{Server: name, Err: err})
	}
	if len(skipped) > 0 && opts.Writer != nil {
		fmt.Fprintf(opts.Writer, "Skipped %d workspace-scoped manifest(s); use `mcphub register` instead: %v\n",
			len(skipped), skipped)
	}
	return results
}

// InstallAllFrom installs every manifest under the explicit opts.ManifestDir.
// Retained for test hermetic-filesystem use and legacy callers that pass
// a tempdir; production uses InstallAll which consults the embed FS.
//
// Workspace-scoped manifests in the directory are skipped silently (same
// contract as InstallAll).
func (a *API) InstallAllFrom(opts InstallAllOpts) []InstallResult {
	var results []InstallResult
	entries, err := os.ReadDir(opts.ManifestDir)
	if err != nil {
		return []InstallResult{{Err: err}}
	}
	var skipped []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		manifestPath := filepath.Join(opts.ManifestDir, e.Name(), "manifest.yaml")
		if _, err := os.Stat(manifestPath); err != nil {
			continue
		}
		// Skip workspace-scoped manifests — same contract as InstallAll.
		if data, oerr := os.ReadFile(manifestPath); oerr == nil {
			if mf, perr := parseManifestForName(e.Name(), data); perr == nil && mf.Kind == config.KindWorkspaceScoped {
				skipped = append(skipped, e.Name())
				continue
			}
		}
		err := a.installFromManifestDir(InstallOpts{
			Server:            e.Name(),
			ClientsInclude:    opts.ClientsInclude,
			IncludeAllClients: opts.IncludeAllClients,
			DryRun:            opts.DryRun,
			Writer:            opts.Writer,
		}, opts.ManifestDir)
		results = append(results, InstallResult{Server: e.Name(), Err: err})
	}
	if len(skipped) > 0 && opts.Writer != nil {
		fmt.Fprintf(opts.Writer, "Skipped %d workspace-scoped manifest(s); use `mcphub register` instead: %v\n",
			len(skipped), skipped)
	}
	return results
}

// installUsingEmbedFirst is the install entry that loads the manifest
// via loadManifestYAMLEmbedFirst. Mirrors Install's audit-first +
// intent-after wiring per Task 10 (plan §62 audit-first canonical).
func (a *API) installUsingEmbedFirst(opts InstallOpts) error {
	w := opts.Writer
	if w == nil {
		w = os.Stderr
	}
	data, err := loadManifestYAMLEmbedFirst(opts.Server)
	if err != nil {
		return fmt.Errorf("load manifest %s: %w", opts.Server, err)
	}
	m, err := parseManifestForName(opts.Server, data)
	if err != nil {
		return err
	}
	if err := Preflight(m, opts.DaemonFilter); err != nil {
		return err
	}
	plan, err := BuildPlanWithOpts(m, BuildPlanOpts{
		DaemonFilter:      opts.DaemonFilter,
		ClientsInclude:    opts.ClientsInclude,
		IncludeAllClients: opts.IncludeAllClients,
	})
	if err != nil {
		return err
	}
	return a.installPlanCore(context.Background(), m, plan, opts.DaemonFilter, opts.DryRun, w)
}

// installFromManifestDir is Install-like but with an explicit manifestDir
// override. Used by InstallAllFrom so tests can point at a tempdir without
// mutating global executable-path state. Mirrors Install's audit-first +
// intent-after wiring per Task 10.
func (a *API) installFromManifestDir(opts InstallOpts, manifestDir string) error {
	w := opts.Writer
	if w == nil {
		w = os.Stderr
	}
	manifestPath := filepath.Join(manifestDir, opts.Server, "manifest.yaml")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", manifestPath, err)
	}
	m, err := parseManifestForName(opts.Server, data)
	if err != nil {
		return err
	}
	if err := Preflight(m, opts.DaemonFilter); err != nil {
		return err
	}
	plan, err := BuildPlanWithOpts(m, BuildPlanOpts{
		DaemonFilter:      opts.DaemonFilter,
		ClientsInclude:    opts.ClientsInclude,
		IncludeAllClients: opts.IncludeAllClients,
	})
	if err != nil {
		return err
	}
	return a.installPlanCore(context.Background(), m, plan, opts.DaemonFilter, opts.DryRun, w)
}

// Status returns the slice of MCP daemons under supervisor management
// (v0.5.0+) — same source DaemonStatusSnapshot uses for /api/status,
// so CLI `mcphub status` and the GUI poller (which feeds the tray
// icon) see the canonical 13-daemon view rather than scheduler.List's
// single supervisor-task row.
//
// Fallback contract: when the supervisor IPC is unreachable
// (ErrSupervisorIPCUnavailable — no lock owner sidecar, no pipe), the
// legacy scheduler scan via StatusWithOpts is used. Hosts mid-
// migration or running v0.4.x compat tooling still get a meaningful
// response that way.
//
// PR #215 fix: before this routing, poller + CLI both saw only the
// supervisor scheduler task, deriveState classified it as Failed
// (no port → alive=false), tray went StateError while Dashboard
// (via DaemonStatusSnapshot's IPC-first path) showed 11 Running
// daemons. Two divergent code paths produced two views of reality;
// unifying them through the IPC seam closes the divergence.
func (a *API) Status() ([]DaemonStatus, error) {
	return a.statusInternal(context.Background())
}

// statusInternal is the IPC-first routing implementation shared by
// Status() (with Background ctx) and StatusContext (with caller ctx).
// The IPC dial deadline is derived from the supplied ctx so that
// caller cancellation (HTTP request cancel, server shutdown, Ctrl+C
// via cobra cmd.Context()) propagates immediately to the supervisor
// pipe read instead of always waiting the full 5s under outage.
// Mirrors the established pattern at health.go:392-401.
//
// PR #215 r2 fix (codex review Finding 2): pre-r2 the IPC ctx was
// derived from context.Background() which severed the caller's
// cancellation chain — a CLI Ctrl+C or HTTP request cancel could
// not interrupt a stalled supervisor IPC dial mid-call.
func (a *API) statusInternal(ctx context.Context) ([]DaemonStatus, error) {
	ipcCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := DialSupervisorIPCStatus(ipcCtx)
	if err == nil {
		return rows, nil
	}
	if !errors.Is(err, ErrSupervisorIPCUnavailable) {
		return nil, err
	}
	// Supervisor not reachable; fall back to legacy scheduler scan.
	// StatusWithOpts is schtasks-driven and ctx-blind; callers that
	// need best-effort cancellation during the fallback path go
	// through StatusContext (api_surfaces.go) which wraps in a
	// goroutine per §32.
	return a.StatusWithOpts(StatusOpts{})
}

// StatusWithHealth is the pre-M5 shim kept for backwards compatibility. New
// callers should prefer StatusWithOpts.
func (a *API) StatusWithHealth(probeHealth bool) ([]DaemonStatus, error) {
	return a.StatusWithOpts(StatusOpts{ProbeHealth: probeHealth})
}

// StatusOpts bundles the flags governing Status enrichment. ProbeHealth
// toggles the synthetic initialize + tools/list round-trip. When
// ForceMaterialize is also true, workspace-scoped rows receive an
// additional no-op tools/call that triggers real backend materialization
// (the proxy writes LifecycleActive/Missing/Failed to the registry, which
// the enrichment then reloads onto the row).
//
// ForceMaterialize requires ProbeHealth. StatusWithOpts returns
// ErrForceMaterializeRequiresHealth when that invariant is violated —
// both the CLI help and the `--force-materialize` flag description promise
// this dependency. Allowing the flag in isolation would either be a
// silent no-op (confusing) or trigger materialization without the
// operator asking for the accompanying probe.
type StatusOpts struct {
	ProbeHealth      bool
	ForceMaterialize bool
}

// ErrForceMaterializeRequiresHealth enforces the documented `--health`
// prerequisite for `--force-materialize`. Callers should surface this
// verbatim to the end user.
var ErrForceMaterializeRequiresHealth = errors.New("--force-materialize requires --health")

// StatusWithOpts is Status + optional MCP-level probes. When
// opts.ProbeHealth is true, the function POSTs initialize + tools/list to
// each Running daemon's /mcp endpoint and fills DaemonStatus.Health. When
// opts.ForceMaterialize is also true, workspace-scoped rows get an
// additional no-op tools/call that drives the lazy proxy through
// materialization and records the resulting 5-state lifecycle in the
// registry (which this function then reloads onto the row).
//
// Disabled by default because the probe adds 1-3 s of per-daemon HTTP
// round-trips — acceptable for an interactive command but wasteful for
// repeated polling.
func (a *API) StatusWithOpts(opts StatusOpts) ([]DaemonStatus, error) {
	if opts.ForceMaterialize && !opts.ProbeHealth {
		return nil, ErrForceMaterializeRequiresHealth
	}
	tasks, err := statusSchedulerTasks()
	if err != nil {
		return nil, err
	}
	result := make([]DaemonStatus, 0, len(tasks))
	for _, t := range tasks {
		result = append(result, DaemonStatus{
			TaskName:   t.Name,
			State:      t.State,
			LastResult: int32(t.LastResult),
			NextRun:    t.NextRun,
		})
	}
	// Empty dir → enrichStatus uses the embed-first resolution path so
	// `mcphub status` from %TEMP% sees the same server set that the
	// daemon sees. Registry path is best-effort; if DefaultRegistryPath
	// errors (no $HOME, etc.), workspace-scoped rows get the task-name
	// parse only — their Lifecycle column shows "-" rather than a value.
	regPath, regErr := DefaultRegistryPath()
	if regErr != nil {
		regPath = ""
	}
	// Track every task name already seeded (scheduler tasks, then each merge
	// below) so the supervised/registry/intent merges never double-emit a row.
	// Bare-form key (leading "\" trimmed) so a scheduler-emitted "\mcp-..." row
	// dedups against a registry/intent "mcp-..." entry. Declared OUT of the
	// regPath block so the supervisor-intent merge below dedups even when the
	// registry path can't be resolved.
	seen := make(map[string]bool, len(result))
	for i := range result {
		seen[strings.TrimPrefix(result[i].TaskName, "\\")] = true
	}
	// Merge supervised/registry-backed LSP proxies that have NO scheduler task.
	// The v0.5.x supervised path (register_supervisor.go) writes these as
	// supervisor-intent children only, so sch.List never surfaces them — without
	// this merge they vanish from --workspace-scoped / --health / --force-materialize
	// even though they are registered and running. enrichStatusWithRegistry then
	// overlays Port/Language/Lifecycle and the alive-probe derives their State.
	registryOnlyLive := map[string]bool{}
	if regPath != "" {
		reg := NewRegistry(regPath)
		if err := reg.Load(); err == nil {
			for _, e := range reg.LSPEntries() {
				if e.TaskName == "" {
					continue
				}
				if !IsLazyProxyTaskName(e.TaskName) {
					continue
				}
				bare := strings.TrimPrefix(e.TaskName, "\\")
				if seen[bare] {
					continue
				}
				seen[bare] = true
				registryOnlyLive[bare] = e.Port != 0 && registryOnlyStatusPortLiveFn != nil && registryOnlyStatusPortLiveFn(e.Port)
				result = append(result, DaemonStatus{TaskName: e.TaskName, State: "Stopped"})
			}
		}
	}
	// Merge Phase-F supervisor-only GLOBAL daemons that have NO scheduler task.
	// In v0.6 every GLOBAL install uses SkipSchedulerTasks=true
	// (install_parsed_manifest.go), so a newly installed/migrated global daemon
	// (e.g. `fetch`) exists ONLY in supervisor-intent.json — statusSchedulerTasks
	// never surfaces it, and the LSP merge above only covers workspace-scoped
	// lazy-proxy rows. Without this seed the daemon DISAPPEARS from the
	// --health / --force-materialize / ProbeHealth table even though bare
	// `mcphub status` shows it (that path goes through the supervisor IPC seam).
	//
	// Best-effort + fail-open: a missing/unreadable intent file keeps the
	// scheduler-only behavior. Maintenance descriptors (watchdog/liveness/
	// weekly-refresh) are excluded — they are not probeable daemons — and any
	// descriptor already represented by a scheduler/LSP row is deduped away.
	// The per-row State is seeded "Ready" (the same neutral seed the scheduler
	// path uses) so enrichStatusWithRegistry's alive-probe derives the real
	// state: deriveState("Ready", alive) → "Running" when the port is bound,
	// "Stopped" otherwise. The descriptor carries the authoritative Server/
	// Daemon/Port, which enrichStatusWithRegistry preserves (the manifest-port
	// lookup only overwrites when the manifest actually has a matching port).
	mergeSupervisorOnlyDaemonRows(&result, seen)
	enrichStatusWithRegistry(result, "", regPath)
	finalizeRegistryOnlyWorkspaceStates(result, registryOnlyLive)
	if opts.ProbeHealth {
		probeDaemonHealth(result)
	}
	if opts.ForceMaterialize {
		forceMaterializeWorkspaceScoped(result, regPath)
	}
	return result, nil
}

// readSupervisorIntentForStatus loads the supervisor-intent.json descriptor
// set used to seed Phase-F supervisor-only daemon rows. Best-effort: a
// missing/unreadable intent file (no install yet, parent-dir gate rejection,
// decode error) returns (nil, err) and the caller keeps the scheduler-only
// behavior. Honors the daemonStateRootOverride state-dir test seam (via
// DefaultSupervisorIntentPath → DaemonStateDir), so tests redirect the read to
// a temp dir without env vars.
func readSupervisorIntentForStatus() (*SupervisorIntentFile, error) {
	path, err := DefaultSupervisorIntentPath()
	if err != nil {
		return nil, err
	}
	return ReadSupervisorIntent(path)
}

// mergeSupervisorOnlyDaemonRows appends a status row for every Phase-F
// supervisor-only GLOBAL daemon — a descriptor present in
// supervisor-intent.json with NO matching scheduler task (v0.6 global installs
// use SkipSchedulerTasks=true) and not already merged from the registry LSP
// path. Without this, such daemons (e.g. `fetch`) are invisible to the
// StatusWithOpts health/force-materialize path even though bare
// `mcphub status` (supervisor IPC seam) lists them.
//
// Dedup is by bare (leading-"\" trimmed) task name against the shared `seen`
// set the caller already populated from the scheduler + LSP-merge rows, so a
// daemon present in BOTH scheduler and intent appears exactly once.
//
// Excluded:
//   - descriptors with an empty TaskName (cannot dedup or probe),
//   - maintenance descriptors (watchdog/liveness/weekly-refresh) — not
//     probeable daemons; IsMaintenanceTaskName is the single source of truth,
//   - any descriptor already represented by a scheduler/LSP row.
//
// Seeded State is "Ready" (the neutral scheduler seed) so the downstream
// enrichStatusWithRegistry alive-probe derives the real state from port
// liveness. The descriptor's Server/Daemon/Port are carried onto the row;
// enrichStatusWithRegistry re-derives Server/Daemon from the (global) task
// name and only overwrites Port when the manifest actually has a matching
// entry, so the descriptor's authoritative Port survives an embed miss.
//
// Best-effort + fail-open: a read error keeps the scheduler-only result.
func mergeSupervisorOnlyDaemonRows(result *[]DaemonStatus, seen map[string]bool) {
	intent, err := readSupervisorIntentForStatus()
	if err != nil || intent == nil {
		return
	}
	for _, d := range intent.Daemons {
		if d.TaskName == "" {
			continue
		}
		if IsMaintenanceTaskName(d.TaskName) {
			continue
		}
		bare := strings.TrimPrefix(canonicalIntentTaskKey(d.TaskName), "\\")
		if seen[bare] {
			continue
		}
		seen[bare] = true
		*result = append(*result, DaemonStatus{
			TaskName: canonicalIntentTaskKey(d.TaskName),
			Server:   d.Server,
			Daemon:   d.Daemon,
			Port:     d.Port,
			State:    "Ready",
		})
	}
}

// forceMaterializeProbe is the hook StatusWithOpts uses when
// opts.ForceMaterialize is true. Production path is sendForceMaterializeTools
// which actually POSTs a no-op tools/call over HTTP; tests replace this
// variable so they can assert "Materialize was triggered" without spinning
// up a real HTTP server.
//
// The result string is captured into the registry as LastError when
// non-empty; a nil error + empty string means the probe returned a valid
// JSON-RPC response (either success or JSON-RPC error — classification
// happens inside the hook).
var forceMaterializeProbe = sendForceMaterializeTools

var statusSchedulerFactory = scheduler.New
var restartSchedulerFactory = scheduler.New

// stopSchedulerFactory mirrors restartSchedulerFactory for the Stop /
// StopAll kill paths so tests can swap in a fake scheduler instead of
// touching the OS scheduler (spec §4 Phase A.1).
var stopSchedulerFactory = scheduler.New

func statusSchedulerTasks() ([]scheduler.TaskStatus, error) {
	sch, err := statusSchedulerFactory()
	if err != nil {
		if schedulerUnavailableError(err) {
			return nil, nil
		}
		return nil, err
	}
	tasks, err := sch.List("mcp-local-hub-")
	if err != nil {
		if schedulerUnavailableError(err) {
			return nil, nil
		}
		return nil, err
	}
	return tasks, nil
}

func schedulerUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, scheduler.ErrNotImplemented)
}

func SchedulerUnavailableError(err error) bool {
	return schedulerUnavailableError(err)
}

// forceMaterializeWorkspaceScoped walks rows and for every workspace-scoped
// entry (non-empty Language), sends a real no-op tools/call via the
// forceMaterializeProbe hook. The proxy will drive the backend through
// materialization and record the 5-state lifecycle in the registry.
// After every row is probed, the registry is reloaded and the rows'
// Lifecycle + LastError + timestamp fields are refreshed.
func forceMaterializeWorkspaceScoped(rows []DaemonStatus, regPath string) {
	// Collect tool-call errors per row so we can surface them after
	// the registry reload. LifecycleActive only promises "MCP handshake
	// completed" — the backend may still return JSON-RPC errors at the
	// tool-call layer (e.g., workspace-load failures in gopls or missing
	// file context for pyright). Without this capture, --force-materialize
	// showed "active" while the tool actually errored, misleading
	// operators during incidents.
	toolErr := make([]string, len(rows))
	var probeRows []int
	for i := range rows {
		if rows[i].Language == "" || rows[i].Port == 0 {
			continue
		}
		probeRows = append(probeRows, i)
	}
	if len(probeRows) > 0 {
		workerCount := DefaultLSPMaterializedHardCap
		if workerCount <= 0 || workerCount > len(probeRows) {
			workerCount = len(probeRows)
		}
		jobs := make(chan int)
		var wg sync.WaitGroup
		wg.Add(workerCount)
		for range workerCount {
			go func() {
				defer wg.Done()
				for idx := range jobs {
					toolErr[idx] = forceMaterializeProbe(rows[idx].Port, rows[idx].Backend)
				}
			}()
		}
		for _, idx := range probeRows {
			jobs <- idx
		}
		close(jobs)
		wg.Wait()
	}
	// Always propagate tool-level errors to rows first — this must
	// happen even when the registry reload path below is skipped
	// (empty regPath / unreadable registry). Without this, callers
	// observing rows[i].LastError would see the stale registry value
	// (or empty) instead of the actual --force-materialize outcome.
	for i := range rows {
		if toolErr[i] != "" {
			rows[i].LastError = toolErr[i]
		}
	}
	// Reload the registry once so every workspace-scoped row sees the
	// proxy's post-probe lifecycle + timestamp writes.
	if regPath == "" {
		return
	}
	reg := NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		return
	}
	// Normalize leading "\" on both sides: Windows Task Scheduler's List
	// returns tasks with a leading "\" (e.g. "\mcp-local-hub-lsp-abc-python"),
	// whereas the registry stores the bare "mcp-local-hub-lsp-abc-python" form.
	// Without this the post-probe refresh silently misses every workspace-scoped
	// row on Windows even though the proxy has already written the new state.
	normalizeTaskName := func(s string) string { return strings.TrimPrefix(s, "\\") }
	byTask := make(map[string]WorkspaceEntry, len(reg.Workspaces))
	for _, e := range reg.Workspaces {
		byTask[normalizeTaskName(e.TaskName)] = e
	}
	for i := range rows {
		if rows[i].Language == "" {
			continue
		}
		if e, ok := byTask[normalizeTaskName(rows[i].TaskName)]; ok {
			rows[i].Lifecycle = e.Lifecycle
			rows[i].LastMaterializedAt = e.LastMaterializedAt
			rows[i].LastToolsCallAt = e.LastToolsCallAt
			// Preserve the probe's tool-error if one was captured
			// above; otherwise take the registry's value.
			if toolErr[i] == "" {
				rows[i].LastError = e.LastError
			}
		}
	}
}

// sendForceMaterializeTools POSTs a safe no-op tools/call to
// http://127.0.0.1:<port>/mcp so the lazy proxy triggers its own backend
// materialization. Tool choice per backend:
//
//	mcp-language-server → "hover" (safe, read-only; diagnostics alternative
//	  requires a loaded file that may not exist in the workspace yet)
//	gopls-mcp           → "go_workspace" (no-arg diagnostic equivalent)
//	other               → "tools/call" with a benign empty name; the proxy
//	  materializes before validating the tool name, so any valid
//	  JSON-RPC shape is enough to drive materialization
//
// Returns a non-empty backend-error string when the JSON-RPC response
// carries an "error" field. The proxy's own registry write marks
// LifecycleActive on MCP handshake success — which can diverge from
// the tool-call outcome when the backend speaks MCP but cannot serve
// tool calls (workspace-load errors in gopls, missing file in pyright,
// etc.). The caller surfaces the returned string as LastError so the
// CLI cell reads "OK (synth); backend ERR: <tool response err>"
// instead of hiding a tool-call failure behind LifecycleActive.
//
// Empty return on transport error (unreachable): we already return
// without touching the registry, and the subsequent registry reload
// will show whatever lifecycle the proxy last wrote.
func sendForceMaterializeTools(port int, backend string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	toolName := "hover"
	switch backend {
	case "gopls-mcp":
		toolName = "go_workspace"
	}
	body := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":101,"method":"tools/call","params":{"name":%q,"arguments":{}}}`,
		toolName)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return ""
	}
	// Streamable HTTP can deliver the response as JSON directly or as a
	// text/event-stream frame. extractSSEPayload (sse.go) pulls the JSON
	// envelope out of an SSE frame (multi-line data:, CRLF, optional space
	// after the colon all handled) and returns the body unchanged when it
	// is plain application/json. One shared owner with singleHealthProbe +
	// liveCapabilitySubSection.
	jsonBytes := extractSSEPayload(payload)
	var env struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(jsonBytes, &env); err != nil {
		return ""
	}
	if env.Error != nil && env.Error.Message != "" {
		// Truncate to match LastError capping.
		msg := env.Error.Message
		if len(msg) > MaxLastErrorBytes {
			msg = msg[:MaxLastErrorBytes] + "…"
		}
		return fmt.Sprintf("backend %s: %s", toolName, msg)
	}
	return ""
}

var healthProbeLivePortFn = portInUse
var preflightPortInUse = portInUse
var registryOnlyStatusPortLiveFn = portInUse

func finalizeRegistryOnlyWorkspaceStates(rows []DaemonStatus, liveByTask map[string]bool) {
	for i := range rows {
		live, ok := liveByTask[strings.TrimPrefix(rows[i].TaskName, "\\")]
		if !ok {
			continue
		}
		if live {
			rows[i].State = "Running"
		} else {
			rows[i].State = "Stopped"
		}
	}
}

// probeDaemonHealth fills DaemonStatus.Health for every Running row
// with a Port. Registry-only workspace-scoped rows may be seeded as
// Stopped before process lookup can prove liveness; for those rows, a
// live TCP port is enough to probe and promote the row to Running.
// The protocol: POST initialize (stream OR json Accept),
// capture Mcp-Session-Id, POST tools/list, count tools in the
// response. Any transport or JSON-RPC error is captured as Err with
// OK=false. Runs concurrently across rows to keep total time bounded.
//
// Workspace-scoped lazy proxies answer both initialize and tools/list
// synthetically from the embedded tool catalog without spawning the
// heavy backend. The probe therefore verifies the proxy is alive but
// says nothing about the underlying LSP binary. We tag those rows with
// Source="proxy-synthetic" so the CLI layer can distinguish them from
// a global-daemon row where a successful probe implies the upstream
// MCP server is also alive.
func probeDaemonHealth(rows []DaemonStatus) {
	var wg sync.WaitGroup
	for i := range rows {
		if rows[i].Port == 0 {
			continue
		}
		if rows[i].State != "Running" {
			if !rows[i].IsWorkspaceScoped || healthProbeLivePortFn == nil || !healthProbeLivePortFn(rows[i].Port) {
				continue
			}
			rows[i].State = "Running"
		}
		if rows[i].State != "Running" {
			continue
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			h := singleHealthProbeFn(rows[idx].Port)
			// Mark lazy-proxy probes by task-name structure, not by
			// registry-populated Language. Language can be empty when
			// registry enrichment fails (missing/corrupt file) even
			// though the task is clearly a lazy proxy. Without the
			// Source tag the CLI would show "OK (N)" as if a real
			// backend validated, when only the synthetic tools/list
			// responded — misleading during incidents.
			if h != nil && IsLazyProxyTaskName(rows[idx].TaskName) {
				h.Source = "proxy-synthetic"
			}
			rows[idx].Health = h
		}(i)
	}
	wg.Wait()
}

// singleHealthProbe does the initialize → tools/list sequence against
// 127.0.0.1:<port>/mcp with a 3 s total deadline. Returns a
// populated HealthProbe either way — the CLI decides whether OK or
// Err is the user-visible signal.
// singleHealthProbeFn is the test seam for singleHealthProbe. Production
// callers go through it; tests swap the var so the HTTP round-trip path
// is not exercised and the probe result is deterministic.
var singleHealthProbeFn = singleHealthProbe

const maxHealthProbeResponseBytes = 1 << 20 // 1 MiB

func singleHealthProbe(port int) *HealthProbe {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	url := fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
	client := &http.Client{Timeout: 3 * time.Second}

	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"mcphub-health","version":"1"}}}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(initBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return &HealthProbe{Err: "initialize: " + err.Error()}
	}
	sessionID := resp.Header.Get("Mcp-Session-Id")
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 {
		return &HealthProbe{Err: fmt.Sprintf("initialize: HTTP %d", resp.StatusCode)}
	}

	listBody := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	req2, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(listBody))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req2.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp2, err := client.Do(req2)
	if err != nil {
		return &HealthProbe{Err: "tools/list: " + err.Error()}
	}
	defer resp2.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp2.Body, maxHealthProbeResponseBytes+1))
	if err != nil {
		return &HealthProbe{Err: "tools/list: read: " + err.Error()}
	}
	if len(raw) > maxHealthProbeResponseBytes {
		return &HealthProbe{Err: fmt.Sprintf("tools/list: response too large (> %d bytes)", maxHealthProbeResponseBytes)}
	}
	// SSE-or-JSON: extractSSEPayload (sse.go) pulls the JSON envelope out
	// of a text/event-stream frame and returns the body unchanged when it
	// is plain application/json. One shared owner with
	// liveCapabilitySubSection + sendForceMaterializeTools.
	payload := extractSSEPayload(raw)
	var parsed struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return &HealthProbe{Err: "tools/list: parse: " + err.Error()}
	}
	if len(parsed.Error) > 0 {
		return &HealthProbe{Err: "tools/list: " + string(parsed.Error)}
	}
	return &HealthProbe{OK: true, ToolCount: len(parsed.Result.Tools)}
}

// Uninstall removes all scheduler tasks and client entries for a server.
// It never prints; the returned UninstallReport carries the outcome for
// CLI/GUI rendering.
// retiredServerNames is the allowlist of server names whose manifests
// have been removed in prior upgrades but may still have stale scheduler
// tasks and client entries on user machines. Only these names trigger
// the manifest-less uninstall fallback; any other unknown name fails
// fast so a typo cannot delete unrelated tasks by prefix overlap.
var retiredServerNames = map[string]bool{
	"gdb": true, // removed in the gdb shared-manifest retirement (PR #13)
}

func (a *API) Uninstall(server string) (*UninstallReport, error) {
	data, err := loadManifestYAMLEmbedFirst(server)
	if err != nil {
		if os.IsNotExist(err) && retiredServerNames[server] {
			return a.uninstallWithoutManifest(server)
		}
		return nil, fmt.Errorf("load manifest %s: %w", server, err)
	}
	m, err := parseManifestForName(server, data)
	if err != nil {
		return nil, err
	}
	report := &UninstallReport{Server: m.Name}
	// Delete tasks scoped to THIS server. The trailing dash narrows
	// "mcp-local-hub-foo-*" without sweeping "mcp-local-hub-foobar-*"
	// (PR #126). The retired-manifest path uninstallWithoutManifest
	// uses the same shape.
	sch, tasks, err := uninstallSchedulerTasksForServer(m.Name)
	if err != nil {
		return nil, err
	}
	// Task 10 plan §65: BEFORE deleting tasks, mark each as
	// Desired=stopped + Reason=uninstalled and append a
	// server-uninstalled audit entry. Audit / intent failures are
	// logged + tolerated — uninstall is idempotent and must remove
	// tasks regardless.
	taskNamesForIntent := make([]string, 0, len(tasks))
	for _, t := range tasks {
		taskNamesForIntent = append(taskNamesForIntent, strings.TrimPrefix(t.Name, "\\"))
	}
	a.recordUninstallIntentForTasks(taskNamesForIntent, nil)
	if sch != nil {
		for _, t := range tasks {
			if err := sch.Delete(t.Name); err != nil {
				report.TaskDeleteWarns = append(report.TaskDeleteWarns, fmt.Sprintf("delete %s: %v", t.Name, err))
			} else {
				report.TasksDeleted = append(report.TasksDeleted, t.Name)
			}
		}
	}
	// v0.6 Phase F (bot PR #288 F1): a global daemon lives in
	// supervisor-intent.json, not in a scheduler task — the sch.List/sch.Delete
	// above removes NOTHING for it. Remove the server's descriptor rows,
	// server-weekly-refresh timer, and stop entries from supervisor-intent.json
	// (under the intent flock, preserving every sibling) and nudge a running
	// supervisor to reconcile so the now-descriptorless daemon is terminated
	// promptly instead of being respawned forever. Best-effort: a cleanup
	// failure is a warning, never a hard error — uninstall is idempotent.
	a.removeServerFromSupervisorIntentBestEffortForManifest(m, report)
	// Remove client entries — but ONLY entries that are unambiguously
	// hub-managed. PR #94's check was too permissive: it treated any
	// loopback HTTP URL as hub-managed, so a user's own MCP server
	// happening to share the manifest name (e.g. user already had a
	// `serena` entry pointing at their own loopback service) would be
	// deleted by uninstall. Use the relay-tuple identity (RelayServer,
	// RelayDaemon, RelayExePath) plus a URL prefix check; only when
	// ALL of those match what this manifest would have installed do we
	// remove the entry.
	allClients := clients.AllClients()
	for _, b := range m.ClientBindings {
		client := allClients[b.Client]
		if client == nil || !client.Exists() {
			continue
		}
		entry, err := client.GetEntry(m.Name)
		if err != nil {
			report.ClientWarns = append(report.ClientWarns, fmt.Sprintf("read %s entry from %s: %v", m.Name, b.Client, err))
			continue
		}
		if entry == nil {
			// No entry under this name in this client; nothing to
			// remove. Not a warning — the binding may have been
			// removed manually or the client never received it.
			continue
		}
		expectedURL := expectedHubURL(m, b)
		if !isHubOwnedEntry(entry, m.Name, b.Daemon, expectedURL) {
			report.ClientWarns = append(report.ClientWarns, fmt.Sprintf("refusing to remove %s from %s: entry is not hub-managed (neither relay tuple nor URL matches what this manifest would install)", m.Name, b.Client))
			continue
		}
		if err := client.RemoveEntry(m.Name); err != nil {
			report.ClientWarns = append(report.ClientWarns, fmt.Sprintf("remove %s from %s: %v", m.Name, b.Client, err))
			continue
		}
		report.ClientsUpdated = append(report.ClientsUpdated, b.Client)
	}
	return report, nil
}

// expectedHubURL returns the URL that BuildPlan would install for this
// (manifest, binding) combination. Used by uninstall to recognize
// HTTP-native client entries (codex-cli, claude-code, cursor, gemini-cli,
// qwen-cli, vscode) that the hub installed but cannot mark with relay
// metadata because their adapters persist only Name + URL. Returns ""
// if the binding's daemon is unresolvable; callers must treat empty as
// "no URL match available".
//
// G6 remote-http branch (bot r1 P1 closure on PR #170): for
// transport=remote-http the entry URL came from the manifest's URL
// field (after ${secret:KEY} expansion). To recognize the entry on
// uninstall, re-expand the manifest URL and return it. Limitation:
// if a secret was rotated BETWEEN install and uninstall, the entry's
// URL embeds the OLD expansion while this function returns the NEW
// one — uninstall would skip the stale entry. Acceptable trade-off
// vs adding new state-file machinery; URL-as-secret-bearer is rare
// (headers are the dominant credential surface).
func expectedHubURL(m *config.ServerManifest, b config.ClientBinding) string {
	if m.Transport == config.TransportRemoteHTTP {
		expanded, err := ExpandSecrets(m.URL, nil)
		if err != nil {
			return "" // missing secrets at uninstall — caller treats as no-match
		}
		return expanded
	}
	daemon, ok := findDaemon(m, b.Daemon)
	if !ok {
		return ""
	}
	urlPath := b.URLPath
	if urlPath == "" {
		urlPath = "/mcp"
	}
	return fmt.Sprintf("http://localhost:%d%s", daemon.Port, urlPath)
}

// isHubOwnedEntry reports whether the client entry was placed by this
// hub for the given (server, daemon) binding. Two ownership signals are
// accepted:
//
//  1. Relay-tuple match: RelayExePath set + RelayServer == manifest name
//     + RelayDaemon == binding daemon. Antigravity (the only relay-style
//     adapter today) persists this triple in client config.
//
//  2. Exact URL match: entry.URL equals what BuildPlan would install
//     (http://localhost:<port><urlPath>). HTTP-native adapters
//     (codex-cli, claude-code, cursor, gemini-cli, qwen-cli, vscode)
//     persist only Name + URL — relay-tuple recognition would leave
//     their entries behind on uninstall (Codex finding on PR #128).
//
// Either signal is sufficient. An entry that matches NEITHER (e.g. a
// user-owned MCP server happening to share the manifest name and
// pointing at a different URL or running externally) is preserved.
func isHubOwnedEntry(entry *clients.MCPEntry, server, daemon, expectedURL string) bool {
	if entry == nil {
		return false
	}
	// Signal 1: relay-tuple match.
	if entry.RelayExePath != "" && entry.RelayServer == server && entry.RelayDaemon == daemon {
		return true
	}
	// Signal 2: URL match against what BuildPlan would install.
	if expectedURL != "" && entry.URL == expectedURL {
		return true
	}
	return false
}

// uninstallWithoutManifest cleans up stale scheduler tasks and client
// entries for a retired server whose manifest is no longer shipped.
// Only called by Uninstall for names in retiredServerNames — a typo or
// unknown server name must never reach here, because the task-prefix
// match would delete any task whose name starts with
// "mcp-local-hub-<server>-" (e.g. uninstalling "se" would otherwise
// sweep up "mcp-local-hub-serena-*"). Best-effort: task deletion by
// prefix + RemoveEntry across every known client. An entry that does
// not exist is not an error.
func (a *API) uninstallWithoutManifest(server string) (*UninstallReport, error) {
	report := &UninstallReport{Server: server}
	// Trailing dash matches the main Uninstall path's narrowing from
	// PR #31 so "mcp-local-hub-gdb-default" matches but "mcp-local-hub-
	// gdbtool-*" does not.
	sch, tasks, err := uninstallSchedulerTasksForServer(server)
	if err != nil {
		return nil, err
	}
	// Task 10 plan §65: same uninstall intent + audit recording as
	// the manifest-backed path.
	retiredTaskNames := make([]string, 0, len(tasks))
	for _, t := range tasks {
		retiredTaskNames = append(retiredTaskNames, strings.TrimPrefix(t.Name, "\\"))
	}
	a.recordUninstallIntentForTasks(retiredTaskNames, nil)
	if sch != nil {
		for _, t := range tasks {
			if err := sch.Delete(t.Name); err != nil {
				report.TaskDeleteWarns = append(report.TaskDeleteWarns, fmt.Sprintf("delete %s: %v", t.Name, err))
			} else {
				report.TasksDeleted = append(report.TasksDeleted, t.Name)
			}
		}
	}
	// v0.6 Phase F (bot PR #288 F1): also clear any supervisor-intent
	// descriptor rows / timer / stops a retired server still owns and nudge
	// reconcile — same symmetric cleanup as the manifest-backed path.
	a.removeServerFromSupervisorIntentBestEffort(server, report)
	for name, client := range clients.AllClients() {
		if client == nil || !client.Exists() {
			continue
		}
		// Only touch clients that actually had an entry; RemoveEntry would
		// otherwise rewrite every client config file (generating a backup)
		// just to delete a nonexistent key.
		entry, err := client.GetEntry(server)
		if err != nil {
			report.ClientWarns = append(report.ClientWarns, fmt.Sprintf("lookup %s in %s: %v", server, name, err))
			continue
		}
		if entry == nil {
			continue
		}
		if err := client.RemoveEntry(server); err != nil {
			report.ClientWarns = append(report.ClientWarns, fmt.Sprintf("remove %s from %s: %v", server, name, err))
			continue
		}
		report.ClientsUpdated = append(report.ClientsUpdated, name)
	}
	return report, nil
}

// installedServerNameSet returns the set of currently-installed server
// names (the manifest catalog the longest-installed-prefix disambiguator
// consults). Best-effort by contract: on any catalog read error it returns
// an EMPTY set. blankServerRowOwnedByLongestInstalledPrefix treats an empty
// set as "no sibling proof exists", which makes it claim any prefix-matching
// task — the documented safe full-cleanup fallback (r33-2). This is the
// single source of `installedServers` threaded into the four hyphen-family
// ownership guards (uninstall delete, full-reinstall prune, stop/restart
// per-daemon gate, and the listTasksForServer scope filter).
func installedServerNameSet() map[string]struct{} {
	names, _ := listManifestNamesEmbedFirst()
	out := make(map[string]struct{}, len(names))
	for _, n := range names {
		out[n] = struct{}{}
	}
	return out
}

// taskOwnedByServerExactOrLongestPrefix reports whether a managed scheduler
// task name belongs to `server` under the same disambiguation the
// supervisor-intent ownership uses: an EXACT parsed-server match, OR (for a
// blank/legacy daemon row) the longest-installed-prefix rule. Workspace
// lazy-proxy tasks (`mcp-local-hub-lsp-<key>-<lang>`, no server slug in the
// name) are NOT owned by any global server name and always return false here;
// callers handle those via the dedicated workspace branch instead.
func taskOwnedByServerExactOrLongestPrefix(taskName, server string, installedServers map[string]struct{}) bool {
	if IsLazyProxyTaskName(taskName) {
		return false
	}
	if parsedServer, _ := ParseManagedTaskName(taskName); parsedServer == server {
		return true
	}
	return blankServerRowOwnedByLongestInstalledPrefix(canonicalIntentTaskKey(taskName), server, installedServers)
}

func uninstallSchedulerTasksForServer(server string) (scheduler.Scheduler, []scheduler.TaskStatus, error) {
	sch, err := newScheduler()
	if err != nil {
		if schedulerUnavailableError(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	prefix := "mcp-local-hub-" + server + "-"
	tasks, err := sch.List(prefix)
	if err != nil {
		if schedulerUnavailableError(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	// FIX 1 (bot PR #288 hyphen-family): sch.List is a raw HasPrefix match, so
	// uninstalling `demo` would otherwise sweep sibling `demo-alpha`'s task
	// `\mcp-local-hub-demo-alpha-beta` and DELETE it. Filter to tasks this
	// server actually owns under the longest-installed-prefix disambiguator
	// (the helper expects the canonical leading-backslash form) so both DELETE
	// callers (Uninstall, uninstallWithoutManifest) skip foreign tasks.
	installed := installedServerNameSet()
	owned := tasks[:0]
	for _, t := range tasks {
		if taskOwnedByServerExactOrLongestPrefix(t.Name, server, installed) {
			owned = append(owned, t)
		}
	}
	return sch, owned, nil
}

// BuildPlanOpts controls plan-time filtering.
type BuildPlanOpts struct {
	DaemonFilter      string
	ClientsInclude    []string
	IncludeAllClients bool
}

// BuildPlan translates a manifest into concrete intended actions using the
// default install clients.
func BuildPlan(m *config.ServerManifest, daemonFilter string) (*Plan, error) {
	return BuildPlanWithOpts(m, BuildPlanOpts{DaemonFilter: daemonFilter})
}

// BuildPlanWithOpts translates a manifest into concrete intended actions.
// If DaemonFilter is non-empty, only that daemon and its referencing client
// bindings are included; weekly refresh is skipped because a partial install
// does not imply a full-server restart. An unknown DaemonFilter or client
// selector is an error surfaced before any side effects.
func BuildPlanWithOpts(m *config.ServerManifest, opts BuildPlanOpts) (*Plan, error) {
	// G6 remote-http branch (sub-PR 2): no daemons, no scheduler
	// tasks, no per-daemon ports. Bind URL + expanded Headers
	// directly into each client's config; the client connects
	// straight to the remote endpoint without going through the
	// local hub.
	if m.Transport == config.TransportRemoteHTTP {
		return buildRemoteHTTPPlan(m, opts)
	}
	if opts.DaemonFilter != "" {
		if _, ok := findDaemon(m, opts.DaemonFilter); !ok {
			return nil, fmt.Errorf("no daemon %q in manifest %s", opts.DaemonFilter, m.Name)
		}
	}
	includeClient, err := installClientPredicate(opts)
	if err != nil {
		return nil, err
	}
	// Scheduler tasks reference the canonical ~/.local/bin/mcphub.exe
	// path (not dev location). See canonicalMcphubPath for the rationale.
	canonicalPath, err := canonicalMcphubPath()
	if err != nil {
		return nil, err
	}
	workDir := filepath.Dir(canonicalPath)
	p := &Plan{Server: m.Name, FullInstall: opts.DaemonFilter == ""}
	// Scheduler tasks — one per daemon (global) or lazy (workspace-scoped).
	// SupervisorIntent mirrors SchedulerTasks during the v0.5.x transition
	// (plan §2611-2644). Both fields stay in sync until the supervisor pivot
	// completes and the legacy field is removed in v0.6+.
	for _, d := range m.Daemons {
		if opts.DaemonFilter != "" && d.Name != opts.DaemonFilter {
			continue
		}
		name := "mcp-local-hub-" + m.Name + "-" + d.Name
		args := []string{"daemon", "--server", m.Name, "--daemon", d.Name}
		p.SchedulerTasks = append(p.SchedulerTasks, ScheduledTaskPlan{
			Name:    name,
			Command: canonicalPath,
			Args:    args,
			Trigger: "At logon",
		})
		p.SupervisorIntent = append(p.SupervisorIntent, SupervisorIntentEntry{
			Name:       name,
			Command:    canonicalPath,
			Args:       args,
			WorkingDir: workDir,
			Trigger:    "At logon",
		})
	}
	// Weekly refresh restarts the whole server, so it only makes sense for full installs.
	if m.WeeklyRefresh && opts.DaemonFilter == "" {
		name := "mcp-local-hub-" + m.Name + "-weekly-refresh"
		args := []string{"restart", "--server", m.Name}
		p.SchedulerTasks = append(p.SchedulerTasks, ScheduledTaskPlan{
			Name:    name,
			Command: canonicalPath,
			Args:    args,
			Trigger: "Weekly Sun 03:00",
		})
		p.SupervisorIntent = append(p.SupervisorIntent, SupervisorIntentEntry{
			Name:       name,
			Command:    canonicalPath,
			Args:       args,
			WorkingDir: workDir,
			Trigger:    "Weekly Sun 03:00",
		})
	}
	// Client updates — one per binding; with a filter, only bindings pointing at the chosen daemon.
	for _, b := range m.ClientBindings {
		if opts.DaemonFilter != "" && b.Daemon != opts.DaemonFilter {
			continue
		}
		if !includeClient(b.Client) {
			continue
		}
		daemon, ok := findDaemon(m, b.Daemon)
		if !ok {
			return nil, fmt.Errorf("binding references unknown daemon %q", b.Daemon)
		}
		path, err := clientConfigPath(b.Client)
		if err != nil {
			return nil, err
		}
		urlPath := b.URLPath
		if urlPath == "" {
			urlPath = "/mcp"
		}
		if err := validateClientURLPath(urlPath); err != nil {
			return nil, fmt.Errorf("invalid url_path for client %q: %w", b.Client, err)
		}
		url := fmt.Sprintf("http://localhost:%d%s", daemon.Port, urlPath)
		// Per-server install path NEVER emits Remove (including a Remove
		// of the mcphub-hub aggregate). The full-reconcile pipeline
		// (BuildHubReconcilePlan / ApplyHubReconcileInOrder) owns gate
		// transitions; per-server installs only refresh their own
		// per-(server, client) bindings.
		p.ClientUpdates = append(p.ClientUpdates, ClientUpdatePlan{
			Client:     b.Client,
			Path:       path,
			Action:     ClientUpdateAddReplace,
			EntryName:  m.Name,
			URL:        url,
			DaemonName: b.Daemon,
		})
	}
	return p, nil
}

func installClientPredicate(opts BuildPlanOpts) (func(string) bool, error) {
	if opts.IncludeAllClients && len(opts.ClientsInclude) > 0 {
		return nil, fmt.Errorf("IncludeAllClients is mutually exclusive with ClientsInclude")
	}
	if opts.IncludeAllClients {
		return func(string) bool { return true }, nil
	}
	var names []string
	if len(opts.ClientsInclude) > 0 {
		names = opts.ClientsInclude
	} else {
		names = clients.DefaultInstallClientNames()
	}
	supported := map[string]bool{}
	for _, name := range clients.SupportedClientNames() {
		supported[name] = true
	}
	selected := map[string]bool{}
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		if !supported[trimmed] {
			return nil, fmt.Errorf("unknown client %q (expected %s)", trimmed, strings.Join(clients.SupportedClientNames(), " | "))
		}
		selected[trimmed] = true
	}
	return func(name string) bool { return selected[name] }, nil
}

func validateClientURLPath(urlPath string) error {
	if !strings.HasPrefix(urlPath, "/") {
		return fmt.Errorf("must start with '/'")
	}
	if strings.HasPrefix(urlPath, "//") {
		return fmt.Errorf("must not start with '//'")
	}
	u, err := url.Parse(urlPath)
	if err != nil {
		return fmt.Errorf("parse url_path: %w", err)
	}
	if u.Scheme != "" || u.Host != "" || u.User != nil {
		return fmt.Errorf("must be a path-only URL")
	}
	return nil
}

func findDaemon(m *config.ServerManifest, name string) (config.DaemonSpec, bool) {
	for _, d := range m.Daemons {
		if d.Name == name {
			return d, true
		}
	}
	return config.DaemonSpec{}, false
}

// buildRemoteHTTPPlan builds the install plan for a G6
// transport=remote-http manifest. No local daemon → no scheduler
// tasks; no per-daemon port → URL comes from manifest.URL directly.
// ${secret:KEY} placeholders in URL + Headers expand at this stage
// against the encrypted vault; missing secrets fail BEFORE any
// client config is touched (G6 spec §"Install path" step 2).
//
// Adapter capability matrix (G6 spec §"Adapter compatibility"):
//   - claude-code, codex-cli, cursor, vscode, gemini-cli: header
//     support confirmed; bind directly.
//   - antigravity: stdio-relay only → install refuses with WARN
//     (handled earlier in Preflight; defense-in-depth check here too).
//
// codex bot r2 P1 closure on PR #169 (the implementation-pending
// gate from sub-PR 1 lands its real handler here).
func buildRemoteHTTPPlan(m *config.ServerManifest, opts BuildPlanOpts) (*Plan, error) {
	// Bot r1 P2 closure on PR #170: remote-http has no daemons,
	// so `--daemon X` makes no sense. Reject explicitly instead
	// of silently treating it as a partial install (which would
	// leave the FullInstall=false flag and skip the full-server
	// reconciliation later).
	if opts.DaemonFilter != "" {
		return nil, fmt.Errorf("manifest %s: transport=remote-http has no daemons; --daemon flag is not applicable (got --daemon=%q)", m.Name, opts.DaemonFilter)
	}
	includeClient, err := installClientPredicate(opts)
	if err != nil {
		return nil, err
	}
	// Resolve ${secret:KEY} placeholders. Missing secrets fail
	// BEFORE any client config write so the operator sees a clear
	// error rather than half-installed entries with placeholder
	// strings.
	expandedURL, err := ExpandSecrets(m.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("install remote-http manifest %s: expand url: %w", m.Name, err)
	}
	expandedHeaders, err := ExpandSecretsMap(m.Headers, nil)
	if err != nil {
		return nil, fmt.Errorf("install remote-http manifest %s: expand headers: %w", m.Name, err)
	}
	p := &Plan{
		Server:      m.Name,
		FullInstall: opts.DaemonFilter == "",
	}
	for _, b := range m.ClientBindings {
		if b.Daemon != "" && b.Daemon != "default" {
			// remote-http manifests have no daemons; a binding
			// that names a non-empty / non-default daemon is a
			// manifest authoring mistake.
			return nil, fmt.Errorf("manifest %s: remote-http binding for client %q names daemon=%q but no daemons are declared (remove the daemon: line or use daemon: default)", m.Name, b.Client, b.Daemon)
		}
		if !includeClient(b.Client) {
			continue
		}
		// Defense-in-depth: enforce the canonical remote-http adapter
		// capability matrix (see remote_http_matrix.go). Antigravity
		// is the obvious stdio-relay case; this guard also catches
		// any future binding that names a client not on the matrix.
		// Preflight already rejected antigravity earlier — this
		// ensures direct BuildPlanWithOpts callers (tests, future
		// API consumers) and other off-matrix names see the same
		// rejection at plan build, before any client config is
		// touched.
		if !isRemoteHTTPCapableClient(b.Client) {
			return nil, fmt.Errorf("manifest %s: client=%q is not on the remote-http adapter capability matrix (supported: %v)", m.Name, b.Client, remoteHTTPCapableClients)
		}
		path, err := clientConfigPath(b.Client)
		if err != nil {
			return nil, err
		}
		p.ClientUpdates = append(p.ClientUpdates, ClientUpdatePlan{
			Client:    b.Client,
			Path:      path,
			Action:    ClientUpdateAddReplace,
			EntryName: m.Name,
			URL:       expandedURL,
			// DisplayURL keeps the manifest's literal pre-expansion
			// URL so plan + install stdout never echo path/query
			// tokens that may have come from ${secret:KEY}
			// placeholders. The wire URL above is still expanded.
			DisplayURL: m.URL,
			Headers:    expandedHeaders,
			// DaemonName intentionally empty — remote-http has none.
		})
	}
	return p, nil
}

// Preflight verifies install preconditions. Returns first error found.
// Called by Install before any side effects.
//
// daemonFilter must match the same filter used by BuildPlan — only daemons
// that the install will actually (re)create have their ports checked. Without
// this alignment, a partial install would fail preflight whenever sibling
// daemons (already running from a prior install) occupy their assigned ports,
// even though those ports are not being touched by the current invocation.
func Preflight(m *config.ServerManifest, daemonFilter string) error {
	// G6 remote-http branch (sub-PR 2): remote endpoints have no
	// local subprocess to LookPath, no ports to check, no scheduler
	// task to plan. Preflight short-circuits with the adapter
	// capability matrix check — antigravity is stdio-relay only
	// and cannot accept a remote URL as a direct entry. Other
	// adapter-specific gates (header schema) run later in
	// BuildPlanWithOpts where the binding set is built.
	if m.Transport == config.TransportRemoteHTTP {
		// canonical mcphub still needs to exist because client
		// configs reference the rotated-token reload-trigger path
		// through it. Keep that gate.
		if _, err := ensureCanonicalMcphubPresent(); err != nil {
			return err
		}
		// Bot r3 P2 closure on PR #170: the antigravity adapter
		// matrix check used to fire here unconditionally, blocking
		// filtered installs (`--clients claude-code`) of mixed-
		// binding manifests even when the operator explicitly
		// excluded antigravity. The check now lives in
		// buildRemoteHTTPPlan where the includeClient predicate is
		// known and the gate fires only against bindings actually
		// in scope for THIS install. Preflight stays narrow on
		// remote-http.
		return nil
	}
	// 1. Command available.
	if _, err := exec.LookPath(m.Command); err != nil {
		return fmt.Errorf("command %q not found on PATH: %w", m.Command, err)
	}
	// 2. Canonical mcphub must exist — scheduler tasks reference
	// ~/.local/bin/mcphub.exe by absolute path because Windows Task
	// Scheduler's CreateProcess call skips PATH lookup. Antigravity
	// relay entries also use this canonical absolute path to avoid
	// PATH/CWD resolution and binary planting.
	if _, err := ensureCanonicalMcphubPresent(); err != nil {
		return err
	}
	// 3. Ports free — only for daemons in the filtered scope.
	//
	// For native-http transports the daemon binds TWO ports: the external
	// client-facing spec.Port, and the internal spec.Port+10000 where the
	// upstream subprocess listens (see cli/daemon.go native-http branch).
	// Both must be free at install time; otherwise the install writes
	// scheduler and client-config entries that immediately fail to start.
	for _, d := range m.Daemons {
		if daemonFilter != "" && d.Name != daemonFilter {
			continue
		}
		if preflightPortInUse(d.Port) {
			// Bug-bash A6 (#6) closure: distinguish "our own running
			// daemon already holds this port" (idempotent reinstall;
			// tolerate and continue) from "a foreign process stole
			// the port we need" (real collision; fail loud).
			if !portHeldByOurDaemonForPortArm(d.Port, m.Name, d.Name, false) {
				return fmt.Errorf("port %d already in use (needed for daemon %s/%s)", d.Port, m.Name, d.Name)
			}
		}
		if m.Transport == config.TransportNativeHTTP {
			internal := d.Port + config.NativeHTTPInternalPortOffset
			if preflightPortInUse(internal) {
				if !portHeldByOurDaemonForPortArm(internal, m.Name, d.Name, true) {
					return fmt.Errorf("internal port %d already in use (needed for native-http upstream of %s/%s; external=%d, internal=external+%d)",
						internal, m.Name, d.Name, d.Port, config.NativeHTTPInternalPortOffset)
				}
			}
		}
	}
	// 4. Secret references resolve. Any `secret:<key>` in manifest.Env
	// must already exist in the vault — otherwise the daemon would
	// spawn, fail to start on the missing env var, and the user would
	// chase a cryptic subprocess error. Failing here surfaces the real
	// cause (missing secret) before any side effect is applied.
	if err := checkSecretRefs(m.Env); err != nil {
		return err
	}
	return nil
}

// checkSecretRefs resolves every manifest env value and fails fast on
// the first missing secret. Only probes secret: refs — file:/literal/
// $VAR values are left to the resolver at daemon launch (they have
// different failure modes we don't want to pre-empt here).
func checkSecretRefs(env map[string]string) error {
	vault, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		// No vault yet — only fail if at least one secret: ref is
		// declared. Manifests with no secret refs should install
		// cleanly on a fresh machine without any secrets setup.
		for k, v := range env {
			if strings.HasPrefix(v, "secret:") {
				return fmt.Errorf("env[%s]=%q requires a secrets vault; run `mcphub secrets set %s` first (vault open failed: %v)",
					k, v, strings.TrimPrefix(v, "secret:"), err)
			}
		}
		return nil
	}
	resolver := secrets.NewResolver(vault, nil)
	for k, v := range env {
		if !strings.HasPrefix(v, "secret:") {
			continue
		}
		if _, err := resolver.Resolve(v); err != nil {
			return fmt.Errorf("env[%s]=%q: %w (run `mcphub secrets set %s` to provide it)",
				k, v, err, strings.TrimPrefix(v, "secret:"))
		}
	}
	return nil
}

// portHeldByOurDaemon reports whether `port` is held by THIS server's
// own daemon, distinguishing "idempotent reinstall while my daemon is
// already running" from "foreign process stole the port we need".
// Bug-bash A6 (#6) closure: pre-fix, the Preflight port-in-use check
// raised the SAME error in both cases — operator saw `port 9129 already
// in use` for every server during `install --all` even when those
// daemons were our own running tasks.
//
// v0.6 Phase F moved global daemons from per-daemon scheduler tasks to
// supervisor-intent.json. Accept that supervisor-owned path first: the
// intent must contain THIS server+daemon row with the same port and
// supervisor IPC must report a live PID for that exact task. When the OS
// port-owner lookup is available, the listener PID must also match the
// supervisor-reported live PID.
//
// Three-part identity gate (bot r1 P1 + r2 P1 closure on PR #180):
// scheduler task name alone is not enough — a stale orphan / foreign
// PID can hold the port while a task with the matching name is also
// in "Running" state (recovery race, watchdog restart in flight,
// etc.). We require ALL of:
//
//  1. port-to-PID lookup succeeds with a non-zero PID
//  2. process identity matches our binary, by EITHER:
//     - image basename == "mcphub.exe" (stdio-bridge listener;
//     native-http external port held by our in-process http.Server)
//     - parent image basename == "mcphub.exe" (native-http internal
//     port held by the upstream child spawned by our daemon)
//     If neither matches, a foreign process owns the port → collision.
//  3. scheduler task `\mcp-local-hub-<server>-<daemon>` exists and
//     is in State == "Running"
//
// Test seam: lookupProcess, processIdentityByPID, and
// schedulerStatusForOwnPort are package vars in production wired to
// the processes.go init helper, the wmic-or-PowerShell identity
// lookup, and scheduler.New().Status respectively. Tests assign
// fakes for each.
func portHeldByOurDaemon(port int, server, daemon string) bool {
	return portHeldByOurDaemonForPortArm(port, server, daemon, true)
}

func portHeldByOurDaemonForPortArm(port int, server, daemon string, allowSupervisorInternalPortMatch bool) bool {
	if portHeldBySupervisorIntentDaemonForPortArm(port, server, daemon, allowSupervisorInternalPortMatch) {
		return true
	}
	if lookupProcess == nil {
		return false
	}
	pid, _, _, ok := lookupProcess(port)
	if !ok || pid == 0 {
		return false
	}
	// Image-identity gate. Accept ownership when EITHER the port-
	// holder's image is mcphub.exe (stdio-bridge / native-http
	// external) OR its parent image is mcphub.exe (native-http
	// internal port held by upstream child spawned by our daemon).
	if processIdentityByPID == nil {
		return false
	}
	image, parentImage, ok := processIdentityByPID(pid)
	if !ok {
		return false
	}
	if !isMcphubProcessImage(image) && !isMcphubProcessImage(parentImage) {
		return false
	}
	if schedulerStatusForOwnPort == nil {
		return false
	}
	taskName := fmt.Sprintf("\\mcp-local-hub-%s-%s", server, daemon)
	st, err := schedulerStatusForOwnPort(taskName)
	if err != nil {
		return false
	}
	if st.State != "Running" {
		return false
	}
	return statusOwnedByCurrentUser(st.Owner)
}

// portHeldBySupervisorIntentDaemon recognizes ports owned by the v0.6
// supervisor-intent path before falling back to legacy scheduler-task
// ownership checks.
//
// Trust ladder:
//   - Native-http internal port: require the descriptor row plus a live
//     supervisor-reported wrapper PID. When the port-owner lookup and process
//     parent walk are available, also prove the internal listener's parent chain
//     reaches that wrapper PID within a bounded depth; a resolvable chain that
//     does not reach it is a foreign listener and is rejected. If the lookup or
//     walk surface is unavailable, keep the previous live-wrapper-PID downgrade:
//     this matches the best-effort identity-gate posture used elsewhere on
//     hosts without process probes, and avoids breaking valid installs when the
//     OS cannot expose ancestry.
//   - External port:
//     1. Port-owner lookup available: require the live listener PID to equal
//     the live supervisor-reported PID for the matching task.
//     2. Port-owner lookup unavailable: fail closed. Supervisor IPC can prove
//     the descriptor task is live, but it cannot prove that the live task owns
//     the already-occupied client-facing port.
//     3. Supervisor IPC unreachable or no live task PID: fail closed.
//
// Callers reach this only after preflightPortInUse(port) is already true, so a
// missing proof surface cannot mean "port is free"; it leaves the normal port
// collision error in place.
func portHeldBySupervisorIntentDaemon(port int, server, daemon string) bool {
	return portHeldBySupervisorIntentDaemonForPortArm(port, server, daemon, true)
}

func portHeldBySupervisorIntentDaemonForPortArm(port int, server, daemon string, allowInternalPortMatch bool) bool {
	if port == 0 || server == "" || daemon == "" {
		return false
	}
	// Read-only resolver: this ownership probe runs inside Preflight, which a
	// --dry-run also exercises on a port collision — DaemonStateDir would
	// MkdirAll the state directory on a first-run host as a dry-run side
	// effect (bot PR #288 r23). A missing dir simply means no intent file →
	// the read below fails → false (the normal collision error stands).
	stateDir, err := daemonStateDirReadOnly()
	if err != nil {
		return false
	}
	intent, err := ReadSupervisorIntent(joinStateFilePath(stateDir, supervisorIntentFileLeaf))
	if err != nil || intent == nil {
		return false
	}
	row, matchedInternalPort, ok := supervisorIntentDaemonForPort(intent, port, server, daemon, allowInternalPortMatch)
	if !ok {
		return false
	}
	livePIDs, reachable := supervisorOwnedLivePIDsWithReachability(context.Background())
	if !reachable {
		return false
	}
	taskKey := strings.TrimPrefix(supervisorIntentDaemonTaskName(row, server, daemon), `\`)
	livePID, ok := livePIDs[taskKey]
	if !ok || livePID <= 0 {
		return false
	}
	if matchedInternalPort {
		owned, resolved := internalPortListenerChainsToWrapperPID(port, livePID)
		if resolved {
			return owned
		}
		// No usable port-owner or ancestry proof. Keep the documented downgrade:
		// descriptor row + live supervisor wrapper PID is the best available
		// identity signal on hosts where process lookup is unavailable.
		return true
	}
	portPID, havePortPID := supervisorOwnedPortPID(port)
	if !havePortPID {
		return false
	}
	return livePID == portPID
}

const internalPortParentWalkDepth = 3

func internalPortListenerChainsToWrapperPID(port int, wrapperPID int) (owned bool, resolved bool) {
	if wrapperPID <= 0 {
		return false, true
	}
	listenerPID, havePortPID := supervisorOwnedPortPID(port)
	if !havePortPID {
		return false, false
	}
	if listenerPID == wrapperPID {
		return true, true
	}
	if processNameAndParentByPID == nil {
		return false, false
	}
	cur := listenerPID
	for depth := 0; depth < internalPortParentWalkDepth; depth++ {
		_, parentPID, ok := processNameAndParentByPID(cur)
		if !ok {
			return false, false
		}
		if parentPID == wrapperPID {
			return true, true
		}
		if parentPID <= 0 || parentPID == cur {
			return false, true
		}
		cur = parentPID
	}
	return false, true
}

func supervisorIntentDaemonForPort(intent *SupervisorIntentFile, port int, server, daemon string, allowInternalPortMatch bool) (SupervisorDaemon, bool, bool) {
	if intent == nil {
		return SupervisorDaemon{}, false, false
	}
	for _, row := range intent.Daemons {
		matchedExternalPort := row.Port == port
		var matchedInternalPort bool
		if allowInternalPortMatch {
			matchedInternalPort = row.Port > 0 && row.Port+config.NativeHTTPInternalPortOffset == port
			if row.RuntimeSpec != nil && row.RuntimeSpec.UpstreamPort == port {
				matchedInternalPort = true
			}
		}
		if !matchedExternalPort && !matchedInternalPort {
			continue
		}
		if supervisorIntentRowMatchesServerDaemon(row, server, daemon) {
			return row, matchedInternalPort && !matchedExternalPort, true
		}
	}
	return SupervisorDaemon{}, false, false
}

func supervisorIntentRowMatchesServerDaemon(row SupervisorDaemon, server, daemon string) bool {
	if server == "" || daemon == "" {
		return false
	}
	// Both identity components are KNOWN here, so a blank-field legacy row is
	// matched by the exact canonical task name — never by ParseManagedTaskName,
	// whose last-hyphen split misattributes hyphenated daemon names
	// (\mcp-local-hub-demo-alpha-beta parses as demo-alpha/beta and a v0.6
	// global, having no scheduler-task fallback, then fails Preflight on its
	// OWN port; bot PR #288 r26 — third member of the r19-F1/r20-F4 family).
	if row.Server == "" || row.Daemon == "" {
		want := canonicalIntentTaskKey("mcp-local-hub-" + server + "-" + daemon)
		if canonicalIntentTaskKey(row.TaskName) == want {
			return true
		}
	}
	return row.Server == server && row.Daemon == daemon
}

func supervisorIntentDaemonTaskName(row SupervisorDaemon, server, daemon string) string {
	if strings.TrimSpace(row.TaskName) != "" {
		return canonicalIntentTaskKey(row.TaskName)
	}
	return canonicalIntentTaskKey("mcp-local-hub-" + server + "-" + daemon)
}

func supervisorOwnedPortPID(port int) (int, bool) {
	if lookupProcess == nil {
		return 0, false
	}
	pid, _, _, ok := lookupProcess(port)
	if !ok || pid == 0 {
		return 0, false
	}
	return pid, true
}

func statusOwnedByCurrentUser(owner string) bool {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return false
	}
	cur, err := user.Current()
	if err != nil || cur == nil {
		return false
	}
	// Task Scheduler often reports owner as DOMAIN\user, while Go may
	// return either `user` or `DOMAIN\user` depending on environment.
	u := strings.ToLower(strings.TrimSpace(cur.Username))
	o := strings.ToLower(owner)
	if u == o {
		return true
	}
	if i := strings.LastIndex(u, `\`); i >= 0 && i+1 < len(u) {
		u = u[i+1:]
	}
	if i := strings.LastIndex(o, `\`); i >= 0 && i+1 < len(o) {
		o = o[i+1:]
	}
	return u != "" && u == o
}

// processIdentityByPID is a function-pointer seam returning the image
// basename and parent-process image basename for a PID (e.g.,
// ("mcphub.exe", "svchost.exe") for our own daemon spawned by
// scheduler; ("python.exe", "mcphub.exe") for a native-http upstream
// child spawned by our daemon).
//
// Production init in processes.go wires it via wmic Name+ParentProcessId
// (with PowerShell Get-CimInstance fallback for Windows 11 24H2+ where
// wmic is removed — bot r2 P2 closure). Tests supply fakes. Stays nil
// on non-Windows hosts (matching the lookupProcess pattern) —
// portHeldByOurDaemon then returns false and the Preflight collision
// check fails as before.
var processIdentityByPID func(pid int) (image, parentImage string, ok bool)

// processNameAndParentByPID returns the process image basename plus parent PID
// for callers that need a bounded ancestry walk rather than only the direct
// parent image. Production is wired in processes.go next to processIdentityByPID;
// tests supply fakes.
var processNameAndParentByPID func(pid int) (image string, parentPID int, ok bool)

const mcphubProcessImageName = "mcphub.exe"

func isMcphubProcessImage(image string) bool {
	return strings.EqualFold(strings.TrimSpace(image), mcphubProcessImageName)
}

// mcphubPIDImageVerified probes the identity of pid. The identity gate is
// BEST-EFFORT hardening only when the lookup surface is structurally
// unavailable (nil seam, e.g. non-Windows hosts where no PID-image probe is
// wired): the caller may proceed in that case to preserve stop --force on
// hosts without the lookup. Once a probe is wired, however, an ok=false result
// means verification was attempted and failed (lookup/parsing failure, exited
// process, PID reuse race, etc.) and must fail closed rather than authorize a
// kill without a positive mcphub image verdict.
func mcphubPIDImageVerified(pid int) (image, parentImage string, verified, lookupAvailable, lookupOK bool) {
	if processIdentityByPID == nil {
		return "", "", false, false, false
	}
	image, parentImage, ok := processIdentityByPID(pid)
	if !ok {
		return image, parentImage, false, true, false
	}
	return image, parentImage, isMcphubProcessImage(image), true, true
}

func requireMcphubPIDImage(pid int) error {
	image, parentImage, verified, lookupAvailable, lookupOK := mcphubPIDImageVerified(pid)
	if !lookupAvailable {
		return nil // best-effort gate: no probe surface → proceed (see doc above)
	}
	if !lookupOK {
		return fmt.Errorf("process identity lookup failed for pid %d", pid)
	}
	if !verified {
		return fmt.Errorf("pid %d image %q parent %q is not %s", pid, image, parentImage, mcphubProcessImageName)
	}
	return nil
}

func requireMcphubPortOwnerPID(port, pid int) error {
	image, parentImage, verified, lookupAvailable, lookupOK := mcphubPIDImageVerified(pid)
	if !lookupAvailable {
		return nil // best-effort gate: no probe surface → proceed (see doc above)
	}
	if !lookupOK {
		return fmt.Errorf("process identity lookup failed for port %d pid %d", port, pid)
	}
	if !verified {
		return fmt.Errorf("port owned by foreign process: port %d pid %d image %q parent %q", port, pid, image, parentImage)
	}
	return nil
}

// schedulerStatusForOwnPort is a function-pointer seam for the
// own-port detection helper above. Production init wires it to
// scheduler.New().Status; tests replace it with a fake that returns
// canned TaskStatus values.
var schedulerStatusForOwnPort = defaultSchedulerStatusForOwnPort

func defaultSchedulerStatusForOwnPort(taskName string) (scheduler.TaskStatus, error) {
	sch, err := scheduler.New()
	if err != nil {
		return scheduler.TaskStatus{}, err
	}
	return sch.Status(taskName)
}

// portInUse returns true if a listener on the given port accepts connections.
func portInUse(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func printPlanTo(w io.Writer, p *Plan) error {
	fmt.Fprintf(w, "Install plan for server %q (dry-run):\n\n", p.Server)
	fmt.Fprintf(w, "  Scheduler tasks to create (%d):\n", len(p.SchedulerTasks))
	for _, t := range p.SchedulerTasks {
		fmt.Fprintf(w, "    \u2022 %s  [%s]\n        %s %v\n", t.Name, t.Trigger, t.Command, t.Args)
	}
	printClientUpdatesTo(w, p)
	fmt.Fprintln(w, "\nNo changes made.")
	_ = clients.Client(nil) // keep import live for later tasks
	return nil
}

func printClientUpdatesTo(w io.Writer, p *Plan) {
	fmt.Fprintf(w, "\n  Client configs to update (%d):\n", len(p.ClientUpdates))
	for _, u := range p.ClientUpdates {
		fmt.Fprintf(w, "    \u2022 %s (%s)\n        %s  \u2192  %s\n", u.Client, u.Path, u.Action, displayURLOf(u))
	}
}

// installPlanOpts carries the per-caller knobs installPlan needs that are
// not derivable from (m, plan). DaemonFilter drives audit-entry emission;
// DryRun short-circuits to a plan print; StartTasks gates Pass B inside
// executeInstallTo. Writer is the progress sink.
type installPlanOpts struct {
	Writer       io.Writer
	DaemonFilter string
	DryRun       bool
	StartTasks   bool
	// Intermediate, when non-nil, is forwarded to executeInstallTo and runs
	// inside its rollback scope between the client-config block and Pass B.
	// Used by InstallParsedManifest to fold the supervisor-intent write into
	// the same rollback stack; legacy callers leave it nil.
	Intermediate intentWriteStep
	// SkipSchedulerPrune, when true, suppresses the full-install obsolete-task
	// reconcile (pruneObsoleteServerTasks) inside executeInstallTo. Set by
	// InstallParsedManifest for workspace-scoped (DaemonTemplate) manifests,
	// which produce ZERO SchedulerTasks: pruning against an empty planned set
	// would delete every registered mcp-local-hub-<server>-* scheduler task
	// for that server. Those daemons live in supervisor-intent.json, not in
	// scheduler tasks, so there is nothing to reconcile here. Legacy callers
	// leave it false (zero value), preserving their reconcile behavior exactly.
	SkipSchedulerPrune bool
	// SkipSchedulerTasks, when true, suppresses Pass A scheduler-task creation
	// (sch.Create per planned daemon) AND the Pass B start inside
	// executeInstallTo. v0.6 Phase F sets it for GLOBAL manifest installs so a
	// fresh install writes supervisor-intent daemon rows (via Intermediate) and
	// defers every daemon spawn to the supervisor reconcile loop — no per-daemon
	// `\mcp-local-hub-<server>-<daemon>` Task Scheduler task is created. The plan
	// still carries SchedulerTasks (for audit/intent task-name derivation), but
	// they are never materialized as OS tasks. Legacy/workspace-scoped callers
	// leave it false; workspace-scoped plans already carry zero SchedulerTasks,
	// so the flag is a no-op there. When true the caller must also set
	// SkipSchedulerPrune=true (the daemon set now lives in supervisor-intent,
	// not in scheduler tasks) and pass a non-nil Intermediate that writes the
	// supervisor-intent rows.
	SkipSchedulerTasks bool
	// AuditTaskNames, when non-empty, OVERRIDES the manifest-derived task list
	// the pre-mutation audit (recordInstallAuditPreMutation) fail-closes on.
	// Set by InstallParsedManifest's workspace-scoped fan-out path: a
	// DaemonTemplate manifest has an EMPTY m.Daemons, so installAuditTaskNames
	// would yield zero entries and the fan-out install would commit
	// per-workspace supervisor-intent daemon rows WITHOUT any fail-closed
	// server-install audit entry. The fan-out path feeds the MATERIALIZED
	// per-workspace task names here so the audit fail-closes BEFORE any
	// intent/scheduler mutation. Legacy callers leave it nil and keep the
	// manifest-derived audit task list (installAuditTaskNames) exactly.
	AuditTaskNames []string
}

// installPlan is the shared materialization core between api.Install (and
// its embed/dir siblings) and InstallParsedManifest: emit the audit-first
// entries per planned task, then execute the plan with the requested
// daemon-start gating. It deliberately does NOT own dry-run-vs-execute
// policy beyond the short-circuit, the post-success intent record
// (recordInstallIntentPostSuccess for the legacy path / WriteSupervisorIntent
// for the parsed-manifest path), or any rollback ownership — those stay
// caller-specific because they diverge between the two entrypoints.
//
// ctx is accepted for symmetry with the exported InstallParsedManifest
// seam (and to give future cancellation a home); the underlying audit +
// scheduler steps are synchronous and do not yet observe it.
func (a *API) installPlan(ctx context.Context, m *config.ServerManifest, plan *Plan, opts installPlanOpts) error {
	_ = ctx
	w := opts.Writer
	if w == nil {
		w = os.Stderr
	}
	if opts.DryRun {
		return printPlanTo(w, plan)
	}
	// AUDIT-FIRST per plan §62 v12 canonical timing: emit one
	// server-install audit entry per planned task BEFORE any mutation.
	// Failure here (incl. ErrIdentityOversize per §51) aborts with an
	// end-state identical to never-attempted install.
	//
	// opts.AuditTaskNames overrides the manifest-derived list for the
	// workspace-scoped fan-out (whose m.Daemons is empty); legacy callers
	// leave it nil and audit the installAuditTaskNames(m, filter) set.
	if err := a.recordInstallAuditForTasks(installAuditTaskNamesOrOverride(m, opts.DaemonFilter, opts.AuditTaskNames)); err != nil {
		return err
	}
	return executeInstallTo(w, m, plan, a.effectiveBackupKeepN(), opts.StartTasks, opts.Intermediate, opts.SkipSchedulerPrune, opts.SkipSchedulerTasks)
}

// createdTask pairs a scheduler.TaskSpec created in Pass A with the
// human-readable Trigger string from its source ScheduledTaskPlan. Pass B
// iterates the created-tasks slice (not p.SchedulerTasks) and re-applies
// the SAME literal "At logon" filter the single-pass version used, so the
// daemon-start gate is bit-for-bit identical.
type createdTask struct {
	spec    scheduler.TaskSpec
	trigger string
}

// intentWriteStep is the typed shape of executeInstallTo's intermediate
// hook. Runs after scheduler-task creation + per-client-config writes and
// BEFORE Pass B task-start. Returns an idempotent compensator that is
// pushed onto executeInstallTo's single shared rollback stack and runs (in
// reverse) on any later-step failure. MUST NOT mutate the manifest or the
// workspace registry (those are the outer migrate-driver's rollback scope).
// Naming the type keeps the core rollback scope from inviting arbitrary
// future side-effects through a bare func() (func(), error) param.
type intentWriteStep func() (rollback func(), err error)

// executeInstallTo materializes the plan in two passes. Pass A creates
// every scheduler task (no Run); the per-client config block runs between
// the passes, preserving the scheduler-create → client-write → run
// ordering callers depend on; Pass B (gated by startTasks) starts the
// logon-triggered tasks. keepN caps the rolling timestamped-backup set per
// client (older copies are pruned in-place by the adapter); 0 disables
// pruning. Production callers feed it via a.effectiveBackupKeepN().
//
// startTasks=true reproduces the pre-two-pass behavior exactly (start
// logon-triggered daemons immediately). The migrate driver passes false to
// create tasks + write intent while deferring daemon spawn to the
// supervisor.
//
// intermediate, when non-nil, runs in the gap between the per-client config
// block and Pass B, INSIDE the rollback scope. It is the supervisor-intent
// write step for InstallParsedManifest (plan §"v7 executeInstallTo two-pass"
// — intent write is step 6 of the inner rollback). It returns a compensating
// undo that is pushed onto the same rollback stack as the scheduler-task and
// client-config undos; if it returns an error, the whole stack runs and
// executeInstallTo returns that error with every side effect undone. Legacy
// api.Install callers pass nil and observe bit-for-bit identical behavior
// (they record intent via recordInstallIntentPostSuccess AFTER this returns).
//
// skipPrune suppresses the full-install obsolete-task reconcile (step 1b). It
// is true only for workspace-scoped (DaemonTemplate) installs through
// InstallParsedManifest, whose plan carries ZERO SchedulerTasks — pruning
// against an empty planned set would delete every registered
// mcp-local-hub-<server>-* scheduler task for that server. Legacy callers pass
// false and keep the reconcile.
func executeInstallTo(w io.Writer, m *config.ServerManifest, p *Plan, keepN int, startTasks bool, intermediate intentWriteStep, skipPrune bool, skipTasks bool) error {
	// v0.6 Phase F: when skipTasks is set (GLOBAL manifest install routed to the
	// supervisor model) NEITHER Pass A scheduler-task creation NOR Pass B start
	// runs — the daemons live in supervisor-intent.json and the supervisor
	// reconcile loop spawns them. The caller also sets skipPrune (the daemon set
	// is owned by supervisor-intent, not scheduler tasks) and passes a non-nil
	// intermediate that writes those rows. Folding skipTasks into the Pass A /
	// Pass B / needsScheduler conditions keeps the client-config block + the
	// intermediate supervisor-intent write inside the SAME rollback scope.
	createTasks := !skipTasks
	// Acquire the scheduler only when there is real scheduler work: tasks to
	// create, an obsolete-task prune to run, or a Pass B start. A
	// workspace-scoped InstallParsedManifest (zero SchedulerTasks,
	// skipPrune=true, startTasks=false) — and a Phase-F global install
	// (skipTasks=true) — has none. On Linux/macOS scheduler.New() returns "not
	// implemented", so acquiring it unconditionally would fail serena/workspace
	// installs on those hosts before the supervisor-intent write — even though
	// this path has no scheduler dependency and defers daemon starts to the
	// reconciler. sch stays nil in that case and is never dereferenced: every
	// use below is inside the SchedulerTasks loop, the prune block (FullInstall
	// && !skipPrune), or Pass B (startTasks) — exactly the conditions in
	// needsScheduler.
	needsScheduler := (createTasks && len(p.SchedulerTasks) > 0) || (p.FullInstall && !skipPrune) || (startTasks && createTasks)
	var sch scheduler.Scheduler
	if needsScheduler {
		s, serr := newScheduler()
		if serr != nil {
			return fmt.Errorf("scheduler: %w", serr)
		}
		sch = s
	}
	// WorkingDirectory for the scheduler task: anchor at ~/.local/bin/
	// (same directory as the canonical mcphub binary). Using os.Getwd()
	// baked the dev-checkout cwd into the task XML — later invocations
	// from any other cwd broke because scheduler-spawned processes
	// inherited a stale cwd that no longer existed (e.g. R:\Temp\build
	// from a throwaway install run). ~/.local/bin is guaranteed to
	// exist (canonicalMcphubPath just confirmed it) and doesn't rot.
	canonical, err := canonicalMcphubPath()
	if err != nil {
		return err
	}
	workDir := filepath.Dir(canonical)

	// Rollback stack: accumulate compensating operations as side effects
	// are applied. On mid-sequence failure, pop and run them in reverse
	// so a failed install does not leave the system in a half-configured
	// state (scheduler tasks for a server whose client entries were never
	// written, or vice-versa).
	var rollback []func()
	runRollback := func() {
		for i := len(rollback) - 1; i >= 0; i-- {
			rollback[i]()
		}
	}

	// Pass A — create scheduler tasks ONLY (no Run). Collect each created
	// spec + its source trigger so Pass B can start the logon-triggered
	// subset without re-deriving from p.SchedulerTasks.
	//
	// v0.6 Phase F: when createTasks is false (GLOBAL manifest routed to the
	// supervisor model) this whole loop is skipped — no per-daemon
	// `\mcp-local-hub-<server>-<daemon>` Task Scheduler task is created. The
	// daemons are written into supervisor-intent.json by the intermediate hook
	// below and spawned by the supervisor reconcile loop. createdTasks stays
	// empty, so Pass B's start loop is a no-op too.
	var createdTasks []createdTask
	for _, t := range p.SchedulerTasks {
		if !createTasks {
			break
		}
		spec := scheduler.TaskSpec{
			Name:             t.Name,
			Description:      "mcp-local-hub: " + m.Name,
			Command:          t.Command,
			Args:             t.Args,
			WorkingDir:       workDir,
			RestartOnFailure: true,
		}
		if t.Trigger == "At logon" {
			spec.LogonTrigger = true
		} else if t.Trigger == "Weekly Sun 03:00" {
			spec.WeeklyTrigger = &scheduler.WeeklyTrigger{DayOfWeek: 0, HourLocal: 3, MinuteLocal: 0}
		}
		// Snapshot any existing task before replacing it so rollback can
		// put it back. Prior to this, Delete-before-Create made install
		// idempotent but a mid-sequence failure left the user with
		// NOTHING — the old task was gone and the new one never got
		// created. ExportXML gives us the full Task Scheduler XML of
		// whatever was there; rollback feeds it to ImportXML to restore.
		var priorXML []byte
		if xml, err := sch.ExportXML(spec.Name); err == nil {
			priorXML = xml
		}
		_ = sch.Delete(spec.Name)
		if err := sch.Create(spec); err != nil {
			runRollback()
			return fmt.Errorf("create task %s: %w", spec.Name, err)
		}
		taskName := spec.Name
		savedXML := priorXML // capture for closure
		rollback = append(rollback, func() {
			_ = sch.Delete(taskName)
			if len(savedXML) > 0 {
				if err := sch.ImportXML(taskName, savedXML); err == nil {
					fmt.Fprintf(w, "  rollback: restored prior scheduler task %s\n", taskName)
					return
				}
			}
			fmt.Fprintf(w, "  rollback: deleted scheduler task %s\n", taskName)
		})
		createdTasks = append(createdTasks, createdTask{spec: spec, trigger: t.Trigger})
		fmt.Fprintf(w, "\u2713 Scheduler task created: %s\n", spec.Name)
	}
	// 1b. Reconcile scheduler tasks: prune obsolete tasks left from prior
	// installs that this plan no longer covers (e.g. a `-weekly-refresh`
	// task from a manifest whose `weekly_refresh` flipped to false). Only
	// safe for full installs; partial installs target one daemon and must
	// not touch sibling tasks for daemons outside the filter.
	//
	// skipPrune additionally suppresses this for workspace-scoped
	// (DaemonTemplate) installs: their plan has ZERO SchedulerTasks, so an
	// empty planned set would prune every registered mcp-local-hub-<server>-*
	// task. Those daemons are tracked in supervisor-intent.json, not scheduler
	// tasks — there is nothing for this reconcile to own.
	if p.FullInstall && !skipPrune {
		planned := make(map[string]struct{}, len(p.SchedulerTasks))
		for _, t := range p.SchedulerTasks {
			planned[t.Name] = struct{}{}
		}
		pruneRollbacks, perr := pruneObsoleteServerTasks(sch, m.Name, planned, w)
		if perr != nil {
			// Listing failed — fall through with a warning. Reconciliation
			// is a nice-to-have; a reinstall that can't enumerate prior
			// tasks is still strictly better than aborting the install.
			fmt.Fprintf(w, "\u26a0 Task reconciliation skipped: %v\n", perr)
		} else {
			rollback = append(rollback, pruneRollbacks...)
		}
	}
	// 2. Backup + update client configs.
	// Populate relay-related fields so adapters for stdio-only clients
	// (e.g. Antigravity) can produce their `command`+`args` entry shape
	// invoking `mcphub.exe relay`. HTTP-native adapters ignore these fields.
	// Use an absolute canonical path to avoid PATH/CWD lookup when clients
	// spawn the relay command.
	allClients := clients.AllClients()
	for _, u := range p.ClientUpdates {
		client := allClients[u.Client]
		if client == nil {
			runRollback()
			return fmt.Errorf("unknown client %q in binding", u.Client)
		}
		if !client.Exists() {
			fmt.Fprintf(w, "\u26a0 Client %s not installed on this machine \u2014 skipping\n", u.Client)
			continue
		}
		// Snapshot the prior entry BEFORE backing up the file or adding
		// the new one. This is the piece that makes rollback atomic on
		// reinstall/replace: if the install fails downstream and the
		// entry already existed with a different URL or relay config,
		// we AddEntry(prior) to restore — instead of RemoveEntry, which
		// would leave the client with no entry at all.
		priorEntry, _ := client.GetEntry(m.Name)

		// keepN is the user's `backups.keep_n` setting (default 5). The
		// adapter writes a fresh timestamped backup, then prunes older
		// timestamped copies in-place so the on-disk set stays bounded.
		// The pristine `-original` sentinel is never affected.
		bak, err := client.BackupKeep(keepN)
		if err != nil {
			runRollback()
			return fmt.Errorf("backup %s: %w", u.Client, err)
		}
		fmt.Fprintf(w, "  backup: %s\n", bak)
		entry := clients.MCPEntry{
			Name:         m.Name,
			URL:          u.URL,
			Headers:      u.Headers,
			RelayServer:  m.Name,
			RelayDaemon:  u.DaemonName,
			RelayExePath: canonical,
		}
		if err := client.AddEntry(entry); err != nil {
			runRollback()
			return fmt.Errorf("add entry to %s: %w", u.Client, err)
		}
		// Compensating op: restore the PRIOR entry (if any) or remove
		// the entry we just added (if this was a first-time install).
		// Wholesale-restoring the backup file is still avoided — a
		// concurrent install of a different server would lose its entry
		// if we did that. Entry-level capture+restore keeps the rollback
		// surgical while preserving the full prior state of THIS server.
		clientRef := client
		entryName := m.Name
		savedPrior := priorEntry
		rollback = append(rollback, func() {
			if savedPrior != nil {
				if err := clientRef.AddEntry(*savedPrior); err == nil {
					fmt.Fprintf(w, "  rollback: restored prior %s entry in %s\n", entryName, u.Client)
					return
				}
			}
			_ = clientRef.RemoveEntry(entryName)
			fmt.Fprintf(w, "  rollback: removed %s entry from %s\n", entryName, u.Client)
		})
		fmt.Fprintf(w, "\u2713 %s \u2192 %s\n", u.Client, displayURLOf(u))
	}
	// Intermediate step (InstallParsedManifest only): write supervisor
	// intent INSIDE the rollback scope so a write failure undoes the
	// scheduler tasks + client configs already applied. Legacy api.Install
	// callers pass a nil hook and skip this entirely.
	if intermediate != nil {
		undo, err := intermediate()
		if err != nil {
			runRollback()
			return err
		}
		if undo != nil {
			rollback = append(rollback, undo)
		}
	}
	// Pass B - start daemons immediately (without waiting for next logon).
	// Gated by startTasks: the migrate driver passes false to defer daemon
	// spawn to the supervisor. A Run failure here is warning-only and must
	// NOT trigger rollback - the install (tasks + client configs) stands;
	// regressing this to a rollback-on-Run-failure would be a P0.
	if startTasks {
		for _, ct := range createdTasks {
			// Skip weekly refresh: triggered on schedule, not on install.
			if ct.trigger != "At logon" {
				continue
			}
			if err := sch.Run(ct.spec.Name); err != nil {
				fmt.Fprintf(w, "\u26a0 failed to start %s immediately: %v (will start at next logon)\n", ct.spec.Name, err)
			} else {
				fmt.Fprintf(w, "\u2713 Started: %s\n", ct.spec.Name)
			}
		}
	}
	fmt.Fprintln(w, "\nInstall complete.")
	return nil
}

// schedulerLister is the narrow scheduler subset pruneObsoleteServerTasks
// needs. scheduler.Scheduler satisfies it via its full method set; keeping
// the interface narrow lets tests supply a minimal fake without implementing
// Create / Run / Status / Stop.
type schedulerLister interface {
	List(prefix string) ([]scheduler.TaskStatus, error)
	Delete(name string) error
	ExportXML(name string) ([]byte, error)
	ImportXML(name string, xml []byte) error
}

// buildPruneSetForReconcile derives the set of "planned" task names from a
// supervisor-intent.json snapshot in the same BARE-key shape that
// pruneObsoleteServerTasks expects (see install.go:1773 — `strings.TrimPrefix(
// task.Name, "\\")` strips the canonical leading backslash before the
// prefix/equality check).
//
// supervisor-intent.json stores `task_name` in canonical leading-backslash
// form (e.g. `\mcp-local-hub-memory-default`); the install reconciler and
// the prune-set comparator both work in BARE form (without the backslash).
// This helper bridges the two shapes so reconcile-prune stays identical
// to the established install.go:1639-1642 planned-map invariant.
//
// Nil intent returns an empty (non-nil) map so callers never need a nil-deref
// guard before lookup.
//
// Spec §"Q12 CLI/GUI status seam" + plan §2611-2644.
func buildPruneSetForReconcile(intent *SupervisorIntentFile) map[string]struct{} {
	planned := make(map[string]struct{})
	if intent == nil {
		return planned
	}
	for _, d := range intent.Daemons {
		bare := strings.TrimPrefix(d.TaskName, "\\")
		if bare == "" {
			continue
		}
		planned[bare] = struct{}{}
	}
	for _, t := range intent.MaintenanceTimers {
		bare := strings.TrimPrefix(t.Name, "\\")
		if bare == "" {
			continue
		}
		planned[bare] = struct{}{}
	}
	return planned
}

// pruneObsoleteServerTasks deletes scheduler tasks whose Name starts with
// "mcp-local-hub-<server>-" and are absent from `planned`. Returns one
// rollback closure per successfully-pruned task that restores the task from
// its pre-delete XML snapshot. Callers must append these to their own
// rollback stack so a later install-sequence failure undoes the pruning
// together with the rest of the install.
//
// Only safe to call for full installs. Partial installs (one daemon) would
// see sibling-daemon tasks as "not in plan" and incorrectly prune them.
//
// Failures that are safe to continue past (per-task ExportXML errors,
// per-task Delete errors) are logged as warnings and skipped; only an
// initial List error is returned as fatal because we cannot reason about
// what to prune without an enumeration.
func pruneObsoleteServerTasks(sch schedulerLister, server string, planned map[string]struct{}, w io.Writer) ([]func(), error) {
	prefix := "mcp-local-hub-" + server + "-"
	existing, err := sch.List(prefix)
	if err != nil {
		return nil, fmt.Errorf("list tasks for %s: %w", server, err)
	}
	// FIX 2 (bot PR #288 hyphen-family): `planned` holds only THIS server's own
	// tasks, so a sibling's task (e.g. `\mcp-local-hub-demo-alpha-beta`, owned by
	// installed server `demo-alpha`) is absent from `planned` and a raw-prefix
	// prune of `demo` would DELETE it. Build the installed-server catalog once
	// and skip any task a LONGER installed server name also prefixes — the
	// longest-installed-prefix disambiguator owns it.
	installed := installedServerNameSet()
	var rollbacks []func()
	for _, task := range existing {
		// Windows Task Scheduler prefixes names with a leading backslash
		// (the task-folder separator). Strip it before prefix/equality
		// checks so "\mcp-local-hub-X-foo" matches "mcp-local-hub-X-foo".
		name := strings.TrimPrefix(task.Name, "\\")
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if _, keep := planned[name]; keep {
			continue
		}
		// Ownership guard: only prune tasks `server` actually owns. A sibling
		// task whose longest installed prefix is a DIFFERENT server survives.
		if !taskOwnedByServerExactOrLongestPrefix(task.Name, server, installed) {
			continue
		}
		// Snapshot XML for rollback. Best-effort: if the backend can't
		// export (platform limitation, ACL, etc.), we still delete but
		// rollback can only log that it cannot recreate.
		var priorXML []byte
		if xml, xerr := sch.ExportXML(name); xerr == nil {
			priorXML = xml
		}
		if derr := sch.Delete(name); derr != nil {
			fmt.Fprintf(w, "\u26a0 Failed to prune obsolete scheduler task %s: %v\n", name, derr)
			continue
		}
		fmt.Fprintf(w, "\u2713 Scheduler task removed (obsolete): %s\n", name)
		taskName := name
		savedXML := priorXML
		rollbacks = append(rollbacks, func() {
			if len(savedXML) == 0 {
				fmt.Fprintf(w, "  rollback: cannot recreate pruned task %s (no XML snapshot)\n", taskName)
				return
			}
			if ierr := sch.ImportXML(taskName, savedXML); ierr != nil {
				fmt.Fprintf(w, "  rollback: restore pruned task %s failed: %v\n", taskName, ierr)
				return
			}
			fmt.Fprintf(w, "  rollback: restored obsolete scheduler task %s\n", taskName)
		})
	}
	return rollbacks, nil
}

// clientConfigPath returns the absolute path to the named client's config.
// Private helper owned by the api package; a parallel copy lives in cli for
// commands that do not yet call through api (secrets, rollback).
func clientConfigPath(name string) (string, error) {
	return clients.ConfigPathForName(name)
}

// Stop stops a running daemon without removing its scheduler task or client
// entries. For each matching task it kills the daemon process by port
// FIRST (schtasks /End only terminates the task's launch action; the
// spawned daemon process keeps the port bound until killed) and then
// calls sch.Stop to clean up the scheduler state. Returns a per-task
// result set so callers can surface partial failures without bailing
// after the first row.
//
// Task 10 wiring (plan §8 + §11 + §51): Stop is the back-compat entry
// for the no-force path. It records Desired=stopped intent + a
// user-stop audit entry BEFORE killing; intent OR audit failure
// (incl. ErrIdentityOversize) returns the error verbatim and skips
// the kill. New callers that need --force should use StopWithOpts.
func (a *API) Stop(server, daemonFilter string) ([]RestartResult, error) {
	taskNames, err := stopIntentTaskNamesForServer(server, daemonFilter)
	if err != nil {
		// Codex deep-sec PR #135 Finding 3: same forensic-trail emission as
		// StopWithOpts (no-force path).
		recordStopFailedNoKill(server, false, err)
		return nil, err
	}
	if err := a.recordStopIntent(taskNames, false); err != nil {
		return nil, err
	}
	return a.stopSupervisorAwareKill(server, daemonFilter)
}

// stopSupervisorAwareKill runs the supervisor reconcile pass followed by
// the legacy kill path, skipping any task the supervisor pass already
// handled (spec §4 Phase A.1 — the same combine-and-skip shape Restart
// uses). PRECONDITION: recordStopIntent must already have written
// Desired=stopped for the in-scope tasks; the supervisor reconcile reads
// that intent from disk.
func (a *API) stopSupervisorAwareKill(server, daemonFilter string) ([]RestartResult, error) {
	supResults, supervisorHandled, err := stopSupervisorOwnedDaemons(context.Background(), server, daemonFilter)
	if err != nil {
		return nil, err
	}
	// Reuse the restart skip-set builder: rows with Code ==
	// SUPERVISOR_UNAVAILABLE are dropped (the legacy path should retry
	// them); every other row — success OR reconcile-failed — is skipped
	// so the kill path never taskkills a daemon a live supervisor would
	// respawn.
	handledTasks := schedulerBlockedRestartTaskNames(supResults)
	killResults, err := a.stopKillCore(server, daemonFilter, handledTasks)
	if err != nil {
		// Mixed install where the supervisor handled every owned row and
		// the host has no usable scheduler (POSIX beta) — mirror
		// Restart's tolerance.
		if supervisorHandled && schedulerUnavailableError(err) {
			return supResults, nil
		}
		return nil, err
	}
	return append(supResults, killResults...), nil
}

// stopKillCore is the original kill body of Stop. Extracted so Stop
// (no-force) and StopWithOpts (Force toggle) can share the kill path
// after each one has run its own intent/audit recording. handledTasks
// holds bare (no leading backslash) task names the supervisor reconcile
// pass already stopped; those are skipped here so the legacy path never
// taskkills a supervisor-owned daemon (the reaper would observe the
// non-clean exit and respawn it — the churn spec §4 kills).
func (a *API) stopKillCore(server, daemonFilter string, handledTasks map[string]struct{}) ([]RestartResult, error) {
	sch, err := stopSchedulerFactory()
	if err != nil {
		return nil, err
	}
	tasks, err := listTasksForServer(sch, server)
	if err != nil {
		return nil, err
	}
	ports := manifestPortMap("")
	wsByTask := workspaceTasksByName()
	installed := installedServerNameSet()
	var results []RestartResult
	for _, t := range tasks {
		normalized := strings.TrimPrefix(t.Name, "\\")
		if !isHubDaemonSchedulerTaskName(normalized) {
			continue
		}
		if _, already := handledTasks[normalized]; already {
			continue
		}
		if !taskMatchesServerDaemonGate(normalized, server, daemonFilter, installed) {
			continue
		}
		port := portForTask(normalized, ports, wsByTask)
		if port != 0 {
			if err := killByPortFn(port, 5*time.Second); err != nil {
				results = append(results, RestartResult{TaskName: t.Name, Err: "kill daemon: " + err.Error()})
				continue
			}
		}
		_ = sch.Stop(t.Name)
		results = append(results, RestartResult{TaskName: t.Name})
	}
	return results, nil
}

// taskMatchesServerDaemonGate is the exact-identity per-daemon gate shared by
// the stopKillCore and Restart legacy scheduler loops (FIX 3, bot PR #288
// hyphen-family). The previous gate was a bare
// `strings.HasSuffix(normalized, "-"+daemonFilter)`, which matched sibling
// tasks: `stop --server demo --daemon beta` matched
// `\mcp-local-hub-demo-alpha-beta` (server demo-alpha / daemon beta) and killed
// demo-alpha's REAL port.
//
// The gate distinguishes three cases:
//
//   - Workspace lazy-proxy tasks (`mcp-local-hub-lsp-<key>-<lang>`) carry no
//     server slug, so ParseManagedTaskName would mis-split them. They keep the
//     original suffix semantics: an empty daemonFilter targets every workspace
//     proxy listTasksForServer surfaced (preserving `stop --server
//     mcp-language-server`); a non-empty daemonFilter matches the proxy whose
//     `lsp-<key>-<lang>` daemon segment equals it.
//
//   - A NON-EMPTY daemonFilter is matched by exact task-name reconstruction:
//     the task must equal `mcp-local-hub-<server>-<daemonFilter>` verbatim. This
//     is unambiguous even for hyphenated daemon names (`vscode-css`), where
//     ParseManagedTaskName's greedy LastIndex('-') split would otherwise
//     misattribute server/daemon. It rejects `mcp-local-hub-demo-alpha-beta`
//     for (demo, beta) because the exact name is `mcp-local-hub-demo-beta`.
//
//   - An EMPTY daemonFilter (whole-server stop/restart) defers to the SAME
//     longest-installed-prefix ownership used by listTasksForServer's scope
//     filter, so the two layers agree: a sibling-owned task is excluded, a
//     genuinely-owned (exact or longest-prefix) task is included.
func taskMatchesServerDaemonGate(normalized, server, daemonFilter string, installedServers map[string]struct{}) bool {
	if IsLazyProxyTaskName(normalized) {
		if daemonFilter == "" {
			return true
		}
		return strings.HasSuffix(normalized, "-"+daemonFilter)
	}
	if daemonFilter != "" {
		want := strings.TrimPrefix(canonicalIntentTaskKey("mcp-local-hub-"+server+"-"+daemonFilter), `\`)
		return strings.TrimPrefix(normalized, `\`) == want
	}
	return taskOwnedByServerExactOrLongestPrefix(normalized, server, installedServers)
}

// Restart kills the live daemons for one server (+ optional daemon
// filter) by port and re-runs their scheduler tasks. The --server
// counterpart of RestartAll: same semantics, narrower scope.
//
// Task 10 wiring (plan §65 fail-handling table): per task, AFTER a
// successful sch.Run, the function records Desired=running intent +
// a server-restarted audit entry. Audit / intent failures are logged
// to opts.Writer (or os.Stderr by default) and never propagate — the
// restart already happened.
func (a *API) Restart(server, daemonFilter string) ([]RestartResult, error) {
	results, supervisorHandled, err := restartSupervisorOwnedDaemons(context.Background(), server, daemonFilter)
	if err != nil {
		return nil, err
	}
	// Mixed install: a server can have supervisor-owned rows AND legacy
	// scheduler tasks. Don't short-circuit after the supervisor pass —
	// fall through to restart the remaining scheduler tasks for this
	// server, skipping any task name already respawned via the supervisor
	// (same combine-and-skip behavior as RestartAll). Bot PR #268 r3.
	handledTasks := schedulerBlockedRestartTaskNames(results)
	sch, err := restartSchedulerFactory()
	if err != nil {
		if supervisorHandled && schedulerUnavailableError(err) {
			return results, nil
		}
		return nil, err
	}
	tasks, err := listTasksForServer(sch, server)
	if err != nil {
		if supervisorHandled && schedulerUnavailableError(err) {
			return results, nil
		}
		return nil, err
	}
	ports := manifestPortMap("")
	wsByTask := workspaceTasksByName()
	installed := installedServerNameSet()
	for _, t := range tasks {
		normalized := strings.TrimPrefix(t.Name, "\\")
		if !isHubDaemonSchedulerTaskName(normalized) {
			continue
		}
		if _, already := handledTasks[normalized]; already {
			continue
		}
		if !taskMatchesServerDaemonGate(normalized, server, daemonFilter, installed) {
			continue
		}
		port := portForTask(normalized, ports, wsByTask)
		if port != 0 {
			if err := killDaemonByPort(port, 5*time.Second); err != nil {
				results = append(results, RestartResult{TaskName: t.Name, Err: "kill daemon: " + err.Error()})
				continue
			}
			// DM-3: wait for the OS to actually release the listening
			// socket before triggering /Run. killDaemonByPort confirms
			// process exit, but TIME_WAIT and Windows' AFD socket-close
			// path can briefly hold the bind. Without this, the new
			// daemon's bind fails immediately and Task Scheduler records
			// last_result=1 with no useful diagnostic. 3s is small enough
			// to be invisible in normal restarts and large enough to ride
			// out the typical TIME_WAIT window.
			if err := waitForPortFree(port, 3*time.Second); err != nil {
				results = append(results, RestartResult{TaskName: t.Name, Err: "wait for port: " + err.Error()})
				continue
			}
		}
		_ = sch.Stop(t.Name)
		if err := sch.Run(t.Name); err != nil {
			results = append(results, RestartResult{TaskName: t.Name, Err: err.Error()})
			continue
		}
		// Task 10 plan §65: AFTER /Run success, record Desired=running
		// intent + restart audit entry. Failures are logged + tolerated.
		// We pass `normalized` (no leading backslash) — the canonical
		// task-key normalization in WriteDaemonIntent (Codex deep-sec
		// PR #135 Finding 1) prepends "\" before storage so the entry
		// lands under the same key the supervisor reconcile loop
		// (internal/cli/supervise_reconcile.go) indexes via row.TaskName.
		a.recordRestartIntentForTask(normalized, nil)
		results = append(results, RestartResult{TaskName: t.Name})
	}
	return results, nil
}

// listTasksForServer returns every scheduler task whose name maps to
// the given server. For global servers that means the classic
// "mcp-local-hub-<server>-" prefix. For workspace-scoped servers (the
// manifest's Kind == workspace-scoped) the per-(workspace, language)
// proxy tasks use "mcp-local-hub-lsp-<key>-<lang>" with NO server slug
// in the name — this helper also queries that prefix so `mcphub stop
// --server mcp-language-server` and `mcphub restart --server
// mcp-language-server` actually target those daemons. Without the
// extended match, workspace-scoped proxies survive every server-scoped
// maintenance command.
func listTasksForServer(sch scheduler.Scheduler, server string) ([]scheduler.TaskStatus, error) {
	primary, err := sch.List("mcp-local-hub-" + server + "-")
	if err != nil {
		return nil, err
	}
	// FIX 4 (bot PR #288 hyphen-family): sch.List is a raw HasPrefix match, so
	// `--server demo` over-captures sibling `demo-alpha`'s task
	// `\mcp-local-hub-demo-alpha-beta`; portForTask would then resolve
	// demo-alpha's REAL port and stop/restart would kill its live daemon. Filter
	// the primary list to tasks `demo` actually owns (exact parsed-server match,
	// or the longest-installed-prefix rule for a blank/legacy daemon row). The
	// workspace lsp-* branch below is left UNTOUCHED — those proxy tasks carry no
	// server slug in the name and are owned via the registry/workspace-path
	// handling in portForTask, not by this server-name filter.
	installed := installedServerNameSet()
	owned := primary[:0]
	for _, t := range primary {
		if taskOwnedByServerExactOrLongestPrefix(t.Name, server, installed) {
			owned = append(owned, t)
		}
	}
	primary = owned
	if !serverIsWorkspaceScoped(server) {
		return primary, nil
	}
	lsp, err := sch.List("mcp-local-hub-lsp-")
	if err != nil {
		// Surface the error. Swallowing would let Stop/Restart
		// report "success" while silently skipping every workspace
		// proxy — a scheduler/service glitch would then leave stale
		// daemons running with no signal to the operator. Wrap so
		// the caller can distinguish primary vs secondary list
		// failure when debugging.
		return nil, fmt.Errorf("list workspace-scoped lazy-proxy tasks: %w", err)
	}
	return append(primary, lsp...), nil
}

// serverIsWorkspaceScoped returns true iff the given server name refers
// to a manifest with Kind == workspace-scoped. Misses (unknown manifest,
// load error) return false — classic behavior.
func serverIsWorkspaceScoped(server string) bool {
	data, err := loadManifestYAMLEmbedFirst(server)
	if err != nil {
		return false
	}
	m, err := parseManifestForName(server, data)
	if err != nil {
		return false
	}
	return m.Kind == config.KindWorkspaceScoped
}

// workspaceTasksByName returns a (taskName → WorkspaceEntry) map from
// the current registry. Used by Stop/Restart to find the right port for
// workspace-scoped lazy-proxy tasks (their ports live in the registry,
// not the manifest). Nil on registry load failure — callers treat it as
// empty and fall back to manifest ports.
func workspaceTasksByName() map[string]WorkspaceEntry {
	regPath, err := DefaultRegistryPath()
	if err != nil {
		return nil
	}
	reg := NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		return nil
	}
	out := make(map[string]WorkspaceEntry, len(reg.Workspaces))
	for _, e := range reg.Workspaces {
		out[strings.TrimPrefix(e.TaskName, "\\")] = e
	}
	return out
}

// portForTask resolves the port for a scheduler task name, trying the
// workspace-scoped registry first (for lazy-proxy tasks) and then
// falling back to the manifest port map (for global daemons).
func portForTask(taskName string, ports map[string]map[string]int, wsByTask map[string]WorkspaceEntry) int {
	if e, ok := wsByTask[taskName]; ok && e.Port != 0 {
		return e.Port
	}
	srv, dmn := parseTaskName(taskName)
	if p, ok := ports[srv][dmn]; ok {
		return p
	}
	return ports[srv]["default"]
}

// RestartResult is one row in a RestartAll/Restart report. JSON tags
// added in Phase 3B-II A3-a (memo D9): the GUI restart handler now
// emits per-task results in JSON, and `error` is NOT omitempty —
// empty-string is the success discriminator the frontend parses.
type RestartResult struct {
	TaskName string `json:"task_name"`
	Err      string `json:"error"`
	Warning  string `json:"warning,omitempty"`
	Code     string `json:"-"`
}

func schedulerBlockedRestartTaskNames(results []RestartResult) map[string]struct{} {
	handledTasks := make(map[string]struct{}, len(results))
	for _, result := range results {
		name := strings.TrimPrefix(result.TaskName, "\\")
		if name == "" {
			continue
		}
		if result.Err != "" && result.Code == "SUPERVISOR_UNAVAILABLE" {
			continue
		}
		handledTasks[name] = struct{}{}
	}
	return handledTasks
}

const supervisorAutostartTaskLeaf = "mcp-local-hub-supervisor"

func isHubInfrastructureTaskName(taskName string) bool {
	name := strings.TrimPrefix(strings.TrimSpace(taskName), "\\")
	// The autostart constant lives in internal/autostart as
	// WindowsTaskName. Keep the leaf local here to avoid an api ->
	// autostart import cycle.
	if name == supervisorAutostartTaskLeaf {
		return true
	}
	return isMaintenanceTaskName(name)
}

func isHubDaemonSchedulerTaskName(taskName string) bool {
	name := strings.TrimPrefix(strings.TrimSpace(taskName), "\\")
	if name == "" || isHubInfrastructureTaskName(name) {
		return false
	}
	server, daemon := ParseManagedTaskName(name)
	return server != "" && daemon != ""
}

// RestartAll stops+starts every scheduler task under our prefix. Returns a
// per-task result list with any errors.
//
// Why we don't rely on scheduler.Stop alone: the task's action (spawning
// the daemon) finishes in milliseconds; the scheduler immediately flips
// the task back to "Ready". `schtasks /End` therefore finds no running
// task instance and silently succeeds, while the daemon process keeps
// running. A subsequent `schtasks /Run` tries to spawn a second daemon,
// hits the bound port, and dies — so the user ends up with the original
// stale daemon they wanted to replace. We have to kill the daemon
// process by port first.
func (a *API) RestartAll() ([]RestartResult, error) {
	results, supervisorHandled, err := restartSupervisorOwnedDaemons(context.Background(), "", "")
	if err != nil {
		return nil, err
	}
	handledTasks := schedulerBlockedRestartTaskNames(results)

	sch, err := restartSchedulerFactory()
	if err != nil {
		if supervisorHandled && schedulerUnavailableError(err) {
			return results, nil
		}
		return nil, err
	}
	tasks, err := sch.List("mcp-local-hub-")
	if err != nil {
		if supervisorHandled && schedulerUnavailableError(err) {
			return results, nil
		}
		return nil, err
	}
	ports := manifestPortMap("")
	wsByTask := workspaceTasksByName()
	for _, t := range tasks {
		normalized := strings.TrimPrefix(t.Name, "\\")
		if !isHubDaemonSchedulerTaskName(normalized) {
			continue
		}
		if _, alreadyHandled := handledTasks[normalized]; alreadyHandled {
			continue
		}
		port := portForTask(normalized, ports, wsByTask)
		if err := killDaemonByPort(port, 5*time.Second); err != nil {
			results = append(results, RestartResult{TaskName: t.Name, Err: "kill daemon: " + err.Error()})
			continue
		}
		if port != 0 {
			// DM-3: see Restart() comment. Same TIME_WAIT race applies
			// to RestartAll's per-task kill-then-Run.
			if err := waitForPortFree(port, 3*time.Second); err != nil {
				results = append(results, RestartResult{TaskName: t.Name, Err: "wait for port: " + err.Error()})
				continue
			}
		}
		_ = sch.Stop(t.Name) // no-op for completed actions; preserve for the edge case of a mid-launch task
		if err := sch.Run(t.Name); err != nil {
			results = append(results, RestartResult{TaskName: t.Name, Err: err.Error()})
			continue
		}
		results = append(results, RestartResult{TaskName: t.Name})
	}
	return results, nil
}

// waitForPortFree probes the local 127.0.0.1:port until net.Listen
// succeeds or timeout elapses. Used by Restart paths between
// killDaemonByPort and schtasks /Run to avoid the OS-level TIME_WAIT /
// AFD socket-close race where a new daemon's bind fails immediately
// after the old daemon exited (DM-3). The probe is loopback-only — we
// only ever bind on 127.0.0.1, so a 0.0.0.0 listener probe would race
// with unrelated services and add false negatives.
func waitForPortFree(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	var lastErr error
	for {
		l, err := net.Listen("tcp", addr)
		if err == nil {
			_ = l.Close()
			return nil
		}
		lastErr = err
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if lastErr != nil {
		return fmt.Errorf("port %d still in use after %v: %w", port, timeout, lastErr)
	}
	return fmt.Errorf("port %d still in use after %v", port, timeout)
}

// StopAll stops every running scheduler task under our prefix. Leaves tasks
// in place (scheduler will relaunch them at next LogonTrigger unless also
// uninstalled). Kills the daemon process by port (see RestartAll comment
// on why scheduler.Stop alone isn't enough). Returns per-task results so
// the CLI can report failures.
//
// Spec §4 Phase A.1: supervisor-owned daemons are stopped through the
// supervisor IPC reconcile instead of taskkill. Unlike Stop, StopAll
// historically recorded NO stop intent at all — without a Desired=stopped
// entry in daemon-intent.json the reconcile would see desired=running and
// could not stop anything — so the supervisor pass here records intent
// for its own targets FIRST. Legacy scheduler tasks keep the historical
// no-intent kill behavior.
func (a *API) StopAll() ([]RestartResult, error) {
	supTargets, err := loadSupervisorOwnedTargets("", "")
	if err != nil {
		return nil, err
	}
	if len(supTargets) > 0 {
		names := make([]string, 0, len(supTargets))
		for _, d := range supTargets {
			names = append(names, strings.TrimPrefix(d.TaskName, `\`))
		}
		// Intent MUST be on disk before the reconcile reads it; a
		// failed intent write fail-closes the supervisor pass the same
		// way Stop's recordStopIntent does. Use the --all who label so
		// the forensic audit log distinguishes a bulk "stop everything"
		// from a targeted `mcphub stop X` (fable F3).
		if err := a.recordStopIntentAs(names, false, auditWhoMcphubStopAll); err != nil {
			return nil, err
		}
	}
	supResults, supervisorHandled, err := stopSupervisorOwnedDaemons(context.Background(), "", "")
	if err != nil {
		return nil, err
	}
	handledTasks := schedulerBlockedRestartTaskNames(supResults)
	sch, err := stopSchedulerFactory()
	if err != nil {
		if supervisorHandled && schedulerUnavailableError(err) {
			return supResults, nil
		}
		return nil, err
	}
	tasks, err := sch.List("mcp-local-hub-")
	if err != nil {
		if supervisorHandled && schedulerUnavailableError(err) {
			return supResults, nil
		}
		return nil, err
	}
	ports := manifestPortMap("")
	wsByTask := workspaceTasksByName()
	results := supResults
	for _, t := range tasks {
		normalized := strings.TrimPrefix(t.Name, "\\")
		if !isHubDaemonSchedulerTaskName(normalized) {
			continue
		}
		if _, already := handledTasks[normalized]; already {
			continue
		}
		port := portForTask(normalized, ports, wsByTask)
		if err := killByPortFn(port, 5*time.Second); err != nil {
			results = append(results, RestartResult{TaskName: t.Name, Err: "kill daemon: " + err.Error()})
			continue
		}
		_ = sch.Stop(t.Name)
		results = append(results, RestartResult{TaskName: t.Name})
	}
	return results, nil
}

// killByPortFn is a test seam for killDaemonByPort. Production callers go
// through this indirection so the Unregister and WeeklyRefreshAll code
// paths can be unit-tested without spawning real processes bound to ports.
// Tests assign a fake in their setup and restore the default in defer.
var killByPortFn = killDaemonByPort

var errPortKillUnsupported = errors.New("port kill unsupported: process lookup unavailable")

const daemonPortReleasePollInterval = 200 * time.Millisecond

type portKillOutcome int

const (
	portKillNoPort portKillOutcome = iota
	portKillLookupUnavailable
	portKillNoListener
	portKillKilled
	portKillIdentityMismatch
)

var forceKillByPortFn = killDaemonByPortOutcome

var taskkillProcessTreeByPIDFn = taskkillProcessTreeByPID

func taskkillProcessTreeByPID(pid int) error {
	cmd := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/F", "/T")
	process.NoConsole(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("taskkill %d: %w: %s", pid, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// killDaemonByPort finds the process listening on 127.0.0.1:port, kills
// its whole tree with taskkill /F /T, and polls until the port is free.
// Returns nil when nothing is listening (nothing to kill), or when the host has
// no process-lookup hook for port-owner discovery.
//
// /T is critical: our hub.exe spawns npx/uvx which spawn node/python.
// Killing only hub.exe leaves the grandchildren running and occupying
// the child-stdin side of the pipe.
func killDaemonByPort(port int, timeout time.Duration) error {
	_, err := killDaemonByPortOutcome(port, timeout)
	return err
}

func killDaemonByPortOutcome(port int, timeout time.Duration) (portKillOutcome, error) {
	if port == 0 {
		return portKillNoPort, nil
	}
	if lookupProcess == nil {
		return portKillLookupUnavailable, nil
	}
	pid, _, _, ok := lookupProcess(port)
	if !ok {
		return portKillNoListener, nil
	}
	if err := requireMcphubPortOwnerPID(port, pid); err != nil {
		return portKillIdentityMismatch, err
	}
	if err := taskkillProcessTreeByPIDFn(pid); err != nil {
		return portKillNoListener, err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, _, _, stillUp := lookupProcess(port); !stillUp {
			return portKillKilled, nil
		}
		time.Sleep(daemonPortReleasePollInterval)
	}
	return portKillNoListener, fmt.Errorf("port %d still bound after %s", port, timeout)
}

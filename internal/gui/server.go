package gui

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
)

// Config drives Server construction. Zero values are sensible defaults.
type Config struct {
	// Port to bind on 127.0.0.1. Zero lets the OS pick one from the
	// ephemeral range; the chosen port is reported via Server.Port().
	Port int
	// Version is surfaced by /api/ping so the GUI's About screen and the
	// second-instance probe can confirm identity across releases.
	Version string
	// PID is surfaced by /api/ping so the second-instance probe can
	// verify the pidport file's PID matches the live process. Zero
	// means "use os.Getpid()" (the normal production path).
	PID int
}

// scanner is the narrow interface that the /api/scan handler needs.
// realScanner is the production adapter; tests inject their own.
type scanner interface {
	Scan() (*api.ScanResult, error)
}

type realScanner struct{}

func (realScanner) Scan() (*api.ScanResult, error) {
	return api.NewAPI().Scan()
}

// statusProvider is the narrow interface the /api/status handler needs.
type statusProvider interface {
	Status() ([]api.DaemonStatus, error)
}

type realStatusProvider struct{}

func (realStatusProvider) Status() ([]api.DaemonStatus, error) {
	return api.NewAPI().Status()
}

// snapshotStatusProvider is the production poller's statusProvider. It
// routes the SSE StatusPoller through the SAME supervisor-IPC-owned,
// fail-loud daemons snapshot that GET /api/status uses
// (healthBackend.DaemonStatusSnapshot), rather than api.Status()'s
// IPC-first-but-scheduler-fallback path.
//
// v0.6 Workstream B (§3.1) — fail loud on the SSE channel too. The
// /api/status route was converted to surface api.ErrSupervisorDown when
// the supervisor IPC is unreachable (so the Dashboard renders the
// degraded banner), but the StatusPoller was a SEPARATE channel feeding
// the SAME Dashboard state. RealStatusProvider.Status() →
// api.NewAPI().Status() → statusInternal STILL fell back to the legacy
// scheduler scan on ErrSupervisorIPCUnavailable, so a down supervisor
// produced stale scheduler rows (migrated daemons painted
// failed/Restarting) which the poller published as `daemon-state`
// deltas; the frontend's onDelta then cleared the degraded error and
// painted those stale cards — re-introducing the exact false-negative
// this phase removes, just via the SSE channel. Routing the poller
// through DaemonStatusSnapshot means a down supervisor yields
// ErrSupervisorDown and the poller emits a `poller-error` event instead
// of stale `daemon-state` deltas.
//
// The fix has two complementary effects on the Dashboard banner:
//  1. By OMISSION — the poller no longer emits banner-CLEARING
//     `daemon-state` deltas on a down supervisor, so onDelta's
//     setError(null) can no longer wipe the degraded banner.
//  2. By POSITIVE SIGNAL — the Dashboard subscribes to `poller-error`
//     (Dashboard.tsx useEventSource map) and calls setError(...), so a
//     down supervisor surfaces the banner within one poll cycle (5s)
//     rather than waiting up to 30s for the separate `/api/status` 500
//     poll. (The 30s HTTP poll remains the durable backstop.)
//
// DaemonStatusSnapshot derives its own bounded IPC deadline internally,
// so context.Background() here matches the prior api.Status() ctx
// semantics (no caller-cancellation propagation through the 5s poll
// cadence, which is acceptable for a fixed-interval background pump).
type snapshotStatusProvider struct {
	health healthBackend
}

func (p snapshotStatusProvider) Status() ([]api.DaemonStatus, error) {
	return p.health.DaemonStatusSnapshot(context.Background())
}

// healthBackend is the narrow interface the /api/health and /api/status
// handlers need. Wired in NewServer to a realHealthBackend whose `api`
// field references the long-lived Server.api instance — Phase G2's
// TTL+singleflight cache lives on that *API, so per-request api.NewAPI()
// would defeat it.
//
// Phase 6 of G2: /api/status now also routes through this backend (via
// DaemonStatusSnapshot) so both endpoints share one cache. Adding a
// method here MUST be matched by both realHealthBackend (production)
// and fakeHealth (test seam in health_test.go).
type healthBackend interface {
	HealthSnapshot(ctx context.Context, opts api.HealthOpts) (api.HealthSnapshot, error)
	// DaemonStatusSnapshot returns the canonical []DaemonStatus that
	// /api/status emits. Shares the daemons-section cache with
	// HealthSnapshot — one StatusWithOpts call serves both surfaces.
	DaemonStatusSnapshot(ctx context.Context) ([]api.DaemonStatus, error)
}

type realHealthBackend struct {
	api *api.API // long-lived; populated from Server.api during NewServer.
}

func (r realHealthBackend) HealthSnapshot(ctx context.Context, opts api.HealthOpts) (api.HealthSnapshot, error) {
	return r.api.HealthSnapshot(ctx, opts)
}

func (r realHealthBackend) DaemonStatusSnapshot(ctx context.Context) ([]api.DaemonStatus, error) {
	return r.api.DaemonStatusSnapshot(ctx)
}

// migrator is the narrow interface the /api/migrate handler needs.
// Returns the structured MigrateReport so the handler can surface per-
// row failures via 207 Multi-Status instead of flattening them into a
// single 500 error blob (bug-bash B1 closure #7 symmetry with demigrater
// — pre-fix, multi-cell Apply produced a wall-of-text error chain that
// operators couldn't tell which row was which or retry individually).
//
// Error is reserved for SETUP failures (e.g., manifest load failures
// that prevent any (server, client) iteration). Per-row failures live
// inside report.Failed and never propagate as Go errors.
//
// realMigrator is the production adapter; tests inject their own.
type migrator interface {
	Migrate(servers, clients []string) (*api.MigrateReport, error)
}

type realMigrator struct{}

// Migrate delegates to api.MigrateFrom. ScanOpts is left zero so
// ManifestDir defaults to "", which api.loadManifestForServer documents as
// the production embed-first path — this mirrors the CLI's scanManifestDir()
// returning "". The ScanOpts client-path fields are documented as unused by
// the migrate flow (see internal/api/migrate.go), so we do not populate them.
// clients is forwarded into MigrateOpts.ClientsInclude; an empty slice
// preserves the original "all clients bound in the manifest" behavior.
//
// Returns the api.MigrateReport verbatim so the handler can surface per-
// row failures structurally. Aggregation into a single error is no longer
// performed here — that was the source of the bug-bash B1 wall-of-text UX.
func (realMigrator) Migrate(servers, clients []string) (*api.MigrateReport, error) {
	return api.NewAPI().MigrateFrom(api.MigrateOpts{
		Servers:        servers,
		ClientsInclude: clients,
	})
}

// demigrater is the narrow interface the /api/demigrate handler needs.
// Returns the structured DemigrateReport so the handler can surface
// per-row failures via 207 Multi-Status instead of flattening them into
// a single 500 error blob (bug-bash B1 closure #7: pre-fix, multi-cell
// Apply produced a wall-of-text error like `1 demigrate row(s) failed:
// server/client: sentinel ... unreadable: ...` chained together with
// `; ` separators — operators couldn't tell which row was which or
// retry individual rows).
//
// Error is reserved for SETUP failures (e.g., manifest load failures
// that prevent any (server, client) iteration). Per-row failures live
// inside report.Failed and never propagate as Go errors.
//
// realDemigrater is the production adapter; tests inject their own.
type demigrater interface {
	Demigrate(servers, clients []string) (*api.DemigrateReport, error)
}

type realDemigrater struct{}

// Demigrate delegates to api.Demigrate. ScanOpts left zero (embed-first
// manifest path, like realMigrator). clients is forwarded into
// DemigrateOpts.ClientsInclude; empty slice preserves the "all bindings
// configured in the manifest" shape.
//
// Returns the api.DemigrateReport verbatim so the handler can surface
// per-row failures structurally. Aggregation into a single error is no
// longer performed here — that was the source of the bug-bash B1 wall-
// of-text UX. Callers (the handler + tests) decide how to render
// partial failures.
func (realDemigrater) Demigrate(servers, clients []string) (*api.DemigrateReport, error) {
	return api.NewAPI().Demigrate(api.DemigrateOpts{
		Servers:        servers,
		ClientsInclude: clients,
	})
}

// dismisser is the narrow interface both /api/dismiss (POST) and
// /api/dismissed (GET) need. One interface for both directions keeps
// the injection shape small; the POST handler uses DismissUnknown,
// the GET handler uses ListDismissedUnknown.
// realDismisser forwards to api.DismissUnknown / api.ListDismissedUnknown
// (persistent JSON file).
type dismisser interface {
	DismissUnknown(name string) error
	ListDismissedUnknown() (map[string]struct{}, error)
}

type realDismisser struct{}

func (realDismisser) DismissUnknown(name string) error {
	return api.DismissUnknown(name)
}

func (realDismisser) ListDismissedUnknown() (map[string]struct{}, error) {
	return api.ListDismissedUnknown()
}

// realManifestCreator is the production adapter for /api/manifest/create.
// Matches the realDemigrater / realDismisser idiom: empty value receiver,
// lazy api.NewAPI() per call so tests can swap the interface without
// needing to stub a constructor.
type realManifestCreator struct{}

func (realManifestCreator) ManifestCreate(name, yaml string) error {
	return api.NewAPI().ManifestCreate(name, yaml)
}

// realManifestValidator is the production adapter for /api/manifest/validate.
// Same shape as realManifestCreator above.
type realManifestValidator struct{}

func (realManifestValidator) ManifestValidate(yaml string) []string {
	return api.NewAPI().ManifestValidate(yaml)
}

// realManifestGetter is the production adapter for /api/manifest/get.
type realManifestGetter struct{}

func (realManifestGetter) ManifestGetWithHash(name string) (string, string, error) {
	return api.NewAPI().ManifestGetWithHash(name)
}

// realManifestEditor is the production adapter for /api/manifest/edit.
type realManifestEditor struct{}

func (realManifestEditor) ManifestEditWithHash(name, yaml, expectedHash string) (string, error) {
	return api.NewAPI().ManifestEditWithHash(name, yaml, expectedHash)
}

// realManifestLister is the production adapter for GET /api/manifests.
type realManifestLister struct{}

func (realManifestLister) ManifestList() ([]string, error) {
	return api.NewAPI().ManifestList()
}

// realManifestDeleter is the production adapter for DELETE /api/manifest/:name.
type realManifestDeleter struct{}

func (realManifestDeleter) ManifestDelete(name string) error {
	return api.NewAPI().ManifestDelete(name)
}

// realCatalogLister is the production adapter for GET /api/catalog.
type realCatalogLister struct{}

func (realCatalogLister) CatalogList() ([]config.CatalogFields, error) {
	return api.NewAPI().CatalogList()
}

// (realMarketplaceLister, the production adapter for GET /api/marketplace,
// lives in marketplace.go alongside the marketplaceLister interface +
// handler — §10 v2b read-only marketplace browse.)

// restarter is the narrow interface the /api/servers/:name/restart
// handler needs. Per memo D9 (Codex R8 P1), it now returns the
// per-task RestartResult slice (existing api.RestartResult{TaskName, Err}
// shape) plus an orchestration-level error. Handler maps:
//
//	results all empty Err  → 200 {restart_results:[…]}
//	results any non-empty  → 207 {restart_results:[…]}
//	err != nil             → 500 + RESTART_FAILED, body has partial
//	                         results (memo §D9).
type restarter interface {
	Restart(server, daemon string) ([]api.RestartResult, error)
	RestartAll() ([]api.RestartResult, error)
}

type realRestarter struct{}

// Restart delegates to api.Restart(server, daemonFilter). When daemon is
// empty, every daemon of the server is restarted (existing contract).
// When daemon is non-empty, api.Restart filters to a task whose name has
// the suffix "-<daemon>" — multi-daemon servers like serena (claude/codex)
// rely on this to avoid restarting siblings on a single-card click.
// Per-task results are returned as-is; the handler inspects each Err field
// to decide 200 vs 207 (all empty vs any non-empty).
func (realRestarter) Restart(server, daemon string) ([]api.RestartResult, error) {
	results, err := api.NewAPI().Restart(server, daemon)
	if results == nil {
		results = []api.RestartResult{}
	}
	return results, err
}

// RestartAll restarts every scheduler-tracked daemon under our prefix.
// Backs the Dashboard "Run all" / "Restart all" header button. Same
// 200/207/500 contract as Restart — empty results means "no scheduler
// tasks at all," which is normal on a fresh setup and stays 200.
func (realRestarter) RestartAll() ([]api.RestartResult, error) {
	results, err := api.NewAPI().RestartAll()
	if results == nil {
		results = []api.RestartResult{}
	}
	return results, err
}

// stopper is the narrow interface the /api/servers/:name/stop handler
// needs. Same shape as restarter — api.Stop returns the same per-task
// RestartResult slice plus orchestration error. Handler maps:
//
//	results all empty Err  → 200 {stop_results:[…]}
//	results any non-empty  → 207 {stop_results:[…]}
//	err != nil             → 500 + STOP_FAILED, body has partial results
type stopper interface {
	Stop(server, daemon string) ([]api.RestartResult, error)
	StopAll() ([]api.RestartResult, error)
}

type realStopper struct{}

// Stop delegates to api.Stop(server, daemonFilter). Empty daemon = all
// daemons of the server (existing contract); non-empty = a single
// daemon by suffix match. Mirrors realRestarter.Restart shape since
// api.Stop and api.Restart share the RestartResult schema.
func (realStopper) Stop(server, daemon string) ([]api.RestartResult, error) {
	results, err := api.NewAPI().Stop(server, daemon)
	if results == nil {
		results = []api.RestartResult{}
	}
	return results, err
}

// StopAll stops every running daemon under our prefix. Backs the
// Dashboard "Stop all" header button and the tray "Quit and stop all
// daemons" menu item.
func (realStopper) StopAll() ([]api.RestartResult, error) {
	results, err := api.NewAPI().StopAll()
	if results == nil {
		results = []api.RestartResult{}
	}
	return results, err
}

// secretsAPI is the narrow interface the /api/secrets/* handlers need.
// Wraps api.API methods so tests can inject a fake. Per memo §5.6.
type secretsAPI interface {
	Init() (api.SecretsInitResult, error)
	List() (api.SecretsEnvelope, error)
	Set(name, value string) error
	Rotate(name, value string, restart bool) (api.SecretsRotateResult, error)
	Restart(name string) ([]api.RestartResult, error)
	Delete(name string, confirm bool) error
}

// cleanupAPI is the narrow interface the /api/cleanup/* routes need.
// Wraps `internal/api/cleanup.go`'s API.CleanupOrphans so tests can
// inject a fake without spinning up real Win32_Process scans.
type cleanupAPI interface {
	// CleanupOrphansSupported reports whether the underlying backend
	// can run the orphan-MCP scan on this OS. Production checks
	// runtime.GOOS; tests can return true unconditionally so the
	// handler's JSON/auth/seam paths are exercised cross-platform.
	// Codex Cloud bot P1 on PR #131 (commit 460e7ff) — moved here from
	// a hard-coded handler check that short-circuited the test seam.
	CleanupOrphansSupported() bool
	CleanupOrphans(opts api.CleanupOpts) ([]api.OrphanProcess, error)
	CleanupLogWatchers(opts api.LogWatcherCleanupOpts) ([]api.LogWatcher, error)
}

type realCleanupAPI struct{}

func (realCleanupAPI) CleanupOrphansSupported() bool {
	return runtime.GOOS == "windows"
}

func (realCleanupAPI) CleanupOrphans(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
	return api.NewAPI().CleanupOrphans(opts)
}

func (realCleanupAPI) CleanupLogWatchers(opts api.LogWatcherCleanupOpts) ([]api.LogWatcher, error) {
	return api.NewAPI().CleanupLogWatchers(opts)
}

type realSecretsAPI struct{}

func (realSecretsAPI) Init() (api.SecretsInitResult, error) { return api.NewAPI().SecretsInit() }
func (realSecretsAPI) List() (api.SecretsEnvelope, error)   { return api.NewAPI().SecretsListWithUsage() }
func (realSecretsAPI) Set(name, value string) error         { return api.NewAPI().SecretsSet(name, value) }
func (realSecretsAPI) Rotate(name, value string, restart bool) (api.SecretsRotateResult, error) {
	return api.NewAPI().SecretsRotate(name, value, restart)
}
func (realSecretsAPI) Restart(name string) ([]api.RestartResult, error) {
	return api.NewAPI().SecretsRestart(name)
}
func (realSecretsAPI) Delete(name string, confirm bool) error {
	return api.NewAPI().SecretsDelete(name, confirm)
}

// logsProvider is the narrow interface the /api/logs/:server handler needs.
// The handler converts the stored log text to either a plain GET body or an
// SSE tail-follow stream. The daemon parameter threads a specific daemon
// name through to api.LogsGet; an empty string resolves to "default" in
// realLogs below, preserving single-daemon-per-server behavior while
// letting multi-daemon servers (serena ships claude + codex with no
// "default") pick the correct log file.
type logsProvider interface {
	Logs(server, daemon string, tail int) (string, error)
}

type realLogs struct{}

// Logs delegates to api.LogsGet. An empty daemon falls back to "default",
// which matches the single-daemon-per-server manifest shape used by most
// Phase 3B servers. Multi-daemon servers (e.g. serena, which exposes
// claude + codex daemons and no "default") can pass the explicit daemon
// name the GUI picker selected — see /api/status rows for daemon values.
func (realLogs) Logs(server, daemon string, tail int) (string, error) {
	if daemon == "" {
		daemon = "default"
	}
	return api.NewAPI().LogsGet(server, daemon, tail)
}

type lspRegistrar interface {
	RegisterLSP(workspacePath string, languages []string) (*lspRegisterReport, error)
}

type realLSPRegistrar struct{}

// RealStatusProvider is the production-default statusProvider. Tests inject
// their own; callers outside the package construct this one.
type RealStatusProvider = realStatusProvider

// Server is the GUI HTTP server. It owns a net/http.Server bound to
// 127.0.0.1, a ready-to-register mux, and a best-effort shutdown path.
type Server struct {
	cfg  Config
	mux  *http.ServeMux
	srv  *http.Server
	port atomic.Int32 // set after Listen, read by Port()
	// api is the long-lived shared *api.API handle. Phase G2 places the
	// HealthSnapshot TTL+singleflight cache on this struct, so the
	// healthBackend adapter MUST reuse this instance — per-request
	// api.NewAPI() would create a fresh cache every call and defeat the
	// caching machinery. Other handlers still use the per-request
	// api.NewAPI() pattern (out of scope for this task; can adopt later
	// if their workloads benefit).
	api                  *api.API
	onActivateWindow     func() error
	scanner              scanner
	status               statusProvider
	health               healthBackend
	migrator             migrator
	demigrater           demigrater
	dismisser            dismisser
	manifestCreator      manifestCreator
	manifestValidator    manifestValidator
	manifestGetter       manifestGetter
	manifestEditor       manifestEditor
	manifestLister       manifestLister
	manifestDeleter      manifestDeleter
	catalogLister        catalogLister
	marketplaceLister    marketplaceLister
	marketplaceRefresher marketplaceRefresher
	// Marketplace one-click install (POST /api/marketplace/install) seams —
	// all behind interfaces so handler tests inject fakes and never touch the
	// live fleet / live client configs.
	marketplaceInstallLoader marketplaceEntryLoader
	marketplacePortPicker    globalPortPicker
	marketplaceNamePresence  serverNamePresence
	marketplaceDirectWriter  directClientWriter
	installer                installer
	uninstaller              uninstaller
	installBulk              installBulkAPI
	restart                  restarter
	stop                     stopper
	logs                     logsProvider
	extractor                extractor
	events                   *Broadcaster
	secrets                  secretsAPI
	settings                 settingsAPI
	backups                  backupsAPI
	backupActions            backupActionsAPI
	cleanup                  cleanupAPI
	clientInit               clientInitializer
	lspRegistrar             lspRegistrar
	groups                   groupsAPI

	// groupsRepublishFn is the live hub-snapshot re-publish seam used by the
	// /api/groups mutation tail (republishGroupsSnapshot). Production: nil —
	// the handler calls publishResolverSnapshotForHubBind(s.api) directly (the
	// same choke point startHubMcpListener uses at gate-ON bind). Tests set a
	// fake to drive the seam deterministically without standing up a real hub
	// listener, and to assert it fired (or not). A per-Server field (not a
	// package-level var) so concurrent tests can't race a shared global and so
	// the seam is owned by the Server it belongs to (mirrors swapForRoute /
	// probeForRoute above).
	groupsRepublishFn func(ctx context.Context, a *api.API) error

	// Weekly-schedule swap test seams (memo D8). Production: nil — the
	// handler falls back to api.SwapWeeklyTrigger and a real
	// scheduler.New() ExportXML adapter. Tests inject closures to drive
	// deterministic outcomes without touching real Task Scheduler.
	swapForRoute      func(spec *api.ScheduleSpec, priorXML []byte) (string, error)
	exportXMLForRoute func(taskName string) ([]byte, error)

	// Force-kill HTTP wrapper test seams (memo D12 + D13). Production:
	// nil — the handlers fall back to gui.Probe / gui.KillRecordedHolder
	// against the real PidportPath(). Tests inject closures to drive
	// deterministic outcomes without touching real file locks or
	// processes. probeForRoute backs POST /api/force-kill/probe; the
	// returned Verdict is encoded as JSON. killForRoute backs POST
	// /api/force-kill; the returned (Verdict, error) tuple is mapped
	// onto HTTP status by Verdict.Class:
	//   - VerdictKilledRecovered / VerdictHealthy   -> 200
	//   - VerdictKillRefused (identity gate failed) -> 403
	//   - VerdictRaceLost   (lock changed mid-flight) -> 412
	//   - other err != nil  (kill failed/unknown)  -> 500
	probeForRoute func() Verdict
	killForRoute  func() (Verdict, error)

	// Phase 4 (G4) — hub-mcp listener bundle. Nil-stored when the
	// gate is off OR when bind failed. Owned by Start, drained by
	// the same cancel/error branches that drain the gui-server
	// listener. Atomic-stored so the test goroutine waiting on
	// ready can race-safely observe whether the hub came up
	// (codex bot phase4 r8 P1 closure on PR #158 — close(ready) now
	// happens BEFORE the hub init writes hubMcpComp, so a plain
	// pointer read would race the assignment).
	hubMcpComp atomic.Pointer[HubListenerComponents]

	// Phase C.2 (v0.5.x serena routing) -- holds the resolver +
	// session-router bundle wired by SetSerenaRouterDeps. Atomic so a
	// future hot-reload of the workspace registry can swap the bundle
	// without restarting the gui-server.
	serenaRouterDeps atomic.Pointer[serenaRouterDeps]

	// ROUTER-COMPLETION phase -- process-local cache of the
	// workspace-agnostic tools/list result the router synthesizes by
	// proxying one tools/list to any live serena daemon (see
	// serena_router_lifecycle.go). The serena tool surface is identical
	// across workspace daemons, so a single cached entry serves every
	// client; the cache is keyed by nothing and TTL-bounded.
	serenaToolsListCache toolsListCache

	// ROUTER-COMPLETION phase -- router-owned map of client-facing
	// Mcp-Session-Id -> the real upstream daemon session it is
	// multiplexed onto (see serena_router_handshake.go). Because the
	// router synthesizes `initialize` itself and mints the client
	// session id, it must perform a SEPARATE MCP handshake with the
	// workspace daemon (which issues its own session id) before
	// forwarding tool calls. This store records that binding so
	// subsequent calls forward the daemon-issued id, not the
	// router-minted client id. Distinct from serenaRouterDeps.Sessions
	// (sticky client-session -> workspace routing) -- the daemon session
	// id is a new concern this store owns. Thread-safe.
	serenaDaemonSessions daemonSessionStore

	// ROUTER-COMPLETION phase (P2 findings 4 + 5 + 7) -- router-owned
	// registry of client sessions minted by a prior `initialize` at this
	// router, plus the protocol version each negotiated (see
	// serena_router_session.go). It is the authoritative "this client
	// session was initialized here" record AND the source of each
	// session's negotiated version, consumed by the tools/list session
	// gate (Finding 4), the version-keyed tools/list cache (Finding 5),
	// and the tool-call protocol-version enforcement (Finding 7).
	// Distinct from serenaRouterDeps.Sessions (sticky session -> workspace)
	// and serenaDaemonSessions (session -> upstream daemon session).
	// Thread-safe.
	serenaRouterSessions routerSessionStore

	// §3 fail-loud IPC reconcile fallback state. serenaBackendPIDMu guards
	// serenaBackendLastPID, the per-workspace-PATH snapshot of the supervisor
	// IPC status's CurrentPID from the PREVIOUS reconcile tick. A workspace
	// whose PID changed (restart) or that disappeared from the status between
	// ticks is a backend-loss signal, so reconcileSerenaBackendLossViaIPC
	// tears down that workspace's router sessions. Owned solely by the
	// reconcile goroutine + its tests; guarded so a future caller is safe.
	serenaBackendPIDMu   sync.Mutex
	serenaBackendLastPID map[string]int
	// serenaBackendIdlePaths tracks workspace paths that were intentionally
	// idle-stopped and the remaining post-clear grace ticks. The first running
	// PID after that phase is a wake respawn, not backend loss, so reconcile
	// refreshes the baseline once and then clears this marker. A cleared idle
	// stop followed by PID0/Stopped consumes the bounded grace before normal
	// backend-loss teardown resumes. Guarded by serenaBackendPIDMu.
	serenaBackendIdlePaths map[string]int
	// serenaBackendDropGen increments whenever baseline tracking is dropped for
	// a workspace path. Reconcile snapshots this alongside serenaBackendLastPID
	// and refuses to persist stale pre-drop state if the generation advanced
	// while its IPC status read was in flight. Entries intentionally live for
	// the Server lifetime; the map has the same workspace-path cardinality as
	// serenaBackendPathByKey (workspaces ever observed), so cleanup would add a
	// second lifecycle without meaningfully improving bounds.
	// Guarded by serenaBackendPIDMu.
	serenaBackendDropGen map[string]uint64
	// serenaBackendPathByKey maps router reverse-index WorkspaceKey values to
	// the WorkspacePath keys used by serenaBackendLastPID/IdlePaths. It lets
	// last-unbind deletion avoid blockable resolver scans on hot teardown paths.
	// Guarded by serenaBackendPIDMu.
	serenaBackendPathByKey map[string]string

	// v0.6 idle-shutdown (#6, spec §6) per-daemon LAST-ACTIVITY tracking.
	// serenaActivityMu guards serenaLastActivity: WorkspaceKey -> the wall
	// time of the daemon's most recent /serena/mcp tool-call forward
	// (recordSerenaActivity, called from the router handler). The 60s idle
	// sweeper (serena_idle_sweeper.go) reads it to decide which RUNNING serena
	// pool daemons have been idle longer than the operator-configured
	// threshold. CRITICAL: this is LAST-ACTIVITY, not wall-clock-since-spawn —
	// a daemon mid-call or recently-active has a fresh timestamp and is never
	// idled (the falsification test). The map is the only owner; entries are
	// pruned when a daemon is idle-stopped so it does not accumulate stale keys.
	serenaActivityMu   sync.Mutex
	serenaLastActivity map[string]time.Time

	// serenaPersistMu guards serenaLastPersist: WorkspaceKey -> the last time we
	// DEBOUNCE-WROTE the @serena row's LastToolsCallAt to the registry. The
	// in-memory serenaLastActivity above is wiped on a GUI restart; persisting a
	// debounced copy to the registry (Phase 3) gives the idle-prune sweeper a
	// durable serena activity signal so a restart does not falsely read a serena
	// workspace as idle. Mirrors the LSP lazy proxy's debounced LastToolsCallAt
	// write (internal/daemon/lazy_proxy.go).
	serenaPersistMu   sync.Mutex
	serenaLastPersist map[string]time.Time

	// guiProcessStart is the wall time this GUI process started (set once in
	// NewServer). The idle sweeper's never-called fallback caps a daemon's
	// idle-duration at time-since-GUI-start so a GUI RESTART cannot
	// immediately idle-kill a daemon that was active just before the restart:
	// the restart wipes serenaLastActivity (in-memory), and a daemon whose
	// supervisor uptime already exceeds the threshold (e.g. 3h) would
	// otherwise be killed on the very first post-restart sweep even though it
	// serviced a call seconds before. Capping at time-since-GUI-start gives
	// every daemon a full fresh threshold window after a GUI restart (FIX-1,
	// fable's coupled-hazard insight). Read-only after construction.
	guiProcessStart time.Time

	// serenaInFlightMu guards serenaInFlight: WorkspaceKey -> count of
	// /serena/mcp forwards currently in flight to that workspace's daemon
	// (incremented around the WHOLE forward, including the SSE stream copy;
	// decremented on completion). The idle sweeper SKIPS any daemon with an
	// open forward so a single long-lived streaming call past the threshold is
	// never idle-killed MID-CALL (FIX-3). A zero/absent count means no forward
	// is open. The map prunes entries back to absent when the count hits 0.
	serenaInFlightMu sync.Mutex
	serenaInFlight   map[string]int

	// pruneEnoentMu guards pruneEnoentTicks: WorkspaceKey -> number of
	// CONSECUTIVE prune-sweep ticks that observed the workspace directory as
	// definitively gone (ENOENT). The workspace-daemon auto-prune sweeper
	// (workspace_prune_sweeper.go) prunes a deleted-dir registration only after
	// it crosses the 2-consecutive-tick threshold, absorbing a transient
	// unmount; the counter resets to 0 (entry pruned) the moment the directory
	// reappears or the row is removed. In-memory only — NOT persisted (Phase 1
	// adds no new persisted state); a GUI restart resets the counters, which is
	// safe (a still-deleted dir simply re-accrues two ticks before pruning).
	// Agent-worktree hits do NOT use this counter (they prune immediately).
	pruneEnoentMu    sync.Mutex
	pruneEnoentTicks map[string]int

	// LSP router dependencies for /lsp/<language>/mcp. This route is
	// intentionally separate from the Serena router because LSP workspace
	// proxies are sessionless upstreams and need no daemon-session handshake.
	lspRouterDeps atomic.Pointer[lspRouterDeps]
}

// NewServer constructs the Server. It registers the ping handler
// immediately so even a minimal Server answers /api/ping.
func NewServer(cfg Config) *Server {
	if cfg.PID == 0 {
		cfg.PID = os.Getpid()
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux(), guiProcessStart: time.Now(), pruneEnoentTicks: map[string]int{}}
	s.serenaRouterSessions.onWorkspaceEmpty = s.handleSerenaRouterWorkspaceEmpty
	// Long-lived shared *API handle. Phase G2 (/api/health) places the
	// TTL+singleflight HealthSnapshot cache here so concurrent requests
	// reuse the same cache; the healthBackend adapter below references
	// THIS instance. Other handlers continue to use api.NewAPI() per
	// request — out of scope for this task.
	s.api = api.NewAPI()
	s.scanner = realScanner{}
	s.status = realStatusProvider{}
	s.health = realHealthBackend{api: s.api}
	s.migrator = realMigrator{}
	s.demigrater = realDemigrater{}
	s.dismisser = realDismisser{}
	s.manifestCreator = realManifestCreator{}
	s.manifestValidator = realManifestValidator{}
	s.manifestGetter = realManifestGetter{}
	s.manifestEditor = realManifestEditor{}
	s.manifestLister = realManifestLister{}
	s.manifestDeleter = realManifestDeleter{}
	s.catalogLister = realCatalogLister{}
	s.marketplaceLister = realMarketplaceLister{}
	s.marketplaceRefresher = realMarketplaceLister{}
	s.marketplaceInstallLoader = realMarketplaceEntryLoader{}
	s.marketplacePortPicker = realGlobalPortPicker{}
	s.marketplaceNamePresence = realServerNamePresence{}
	s.marketplaceDirectWriter = realDirectClientWriter{}
	s.installer = realInstaller{}
	s.uninstaller = realUninstaller{}
	s.installBulk = realInstallBulkAPI{}
	s.restart = realRestarter{}
	s.stop = realStopper{}
	s.logs = realLogs{}
	s.extractor = realExtractor{}
	s.events = NewBroadcaster()
	s.secrets = realSecretsAPI{}
	s.settings = realSettingsAPI{}
	s.backups = realBackupsAPI{}
	s.backupActions = realBackupActionsAPI{}
	s.cleanup = realCleanupAPI{}
	s.clientInit = realClientInitializer{}
	s.lspRegistrar = realLSPRegistrar{}
	s.groups = realGroupsAPI{}
	registerPingRoutes(s)
	registerAssetRoutes(s)
	registerScanRoutes(s)
	registerStatusRoutes(s)
	registerHealthRoute(s)
	registerMigrateRoutes(s)
	registerDemigrateRoutes(s)
	registerDismissRoutes(s)
	registerManifestRoutes(s)
	registerMarketplaceRoutes(s)
	registerMarketplaceInstallRoutes(s)
	registerInstallRoutes(s)
	registerServerRoutes(s)
	registerEventsRoutes(s)
	registerLogsRoutes(s)
	registerExtractManifestRoutes(s)
	registerSecretsRoutes(s)
	registerSettingsRoutes(s)
	registerPathValidateRoutes(s)       // Wave 2: TypePath "Browse…" path-exists/is-dir validate
	registerClientInstallPrefsRoutes(s) // Wave 2: Settings → Clients default-install override
	registerBackupsRoutes(s)
	registerBackupsActionsRoutes(s) // Wave 2: per-timestamp backup restore + delete
	registerVersionRoutes(s)
	registerDaemonsRoutes(s)
	registerExportBundleRoutes(s)
	registerCleanupRoutes(s)
	registerForceKillRoutes(s)
	registerInitClientConfigRoutes(s)
	registerDaemonEnvRoutes(s)
	registerWorkspacesRoutes(s)
	registerLSPRegisterRoutes(s)
	registerLSPTrustedRootsRoutes(s)
	registerSupervisorRestartRoutes(s)
	registerGUISelfRestartRoutes(s) // Wave 3: POST /api/gui/restart self re-exec with lock handoff
	registerStateRelaxSettingRoutes(s)
	registerSerenaRouterRoutes(s)
	registerLSPRouterRoutes(s)
	registerGroupsRoutes(s) // groups Phase 5b-1: /api/groups CRUD authoring endpoint
	return s
}

// Broadcaster exposes the SSE event bus. Tests publish into it directly;
// production callers (poller goroutine in Task 12+) use it the same way.
func (s *Server) Broadcaster() *Broadcaster { return s.events }

// StatusProvider returns the statusProvider the production SSE
// StatusPoller must poll. It is backed by the server's long-lived
// healthBackend so the poller shares the daemons-section TTL cache with
// GET /api/status AND inherits its fail-loud contract: when the
// supervisor IPC is unreachable, DaemonStatusSnapshot returns
// api.ErrSupervisorDown rather than falling back to the legacy
// scheduler scan, so the poller emits a `poller-error` event instead of
// stale `daemon-state` deltas. The stale deltas would have CLEARED the
// Dashboard's degraded banner (onDelta → setError(null)); the
// `poller-error` event is now consumed by the Dashboard's useEventSource
// map to SET the banner (PR #281 round-2 P3), and also drives the tray
// icon to StateError via the poller's error channel (PR #281 round-2 P2,
// wired in cli.go). (v0.6 Workstream B §3.1).
func (s *Server) StatusProvider() statusProvider {
	return snapshotStatusProvider{health: s.health}
}

// OnActivateWindow registers the callback invoked when POST
// /api/activate-window is received. A second `mcphub gui` invocation
// posts here after handshaking with the incumbent; the callback is the
// hook that the tray + main window use to come to front.
//
// The callback returns an error so the handler can surface a non-success
// status when activation is genuinely impossible (e.g. headless session
// — no window to focus, no browser to launch). nil → 204 No Content,
// ErrActivationNoTarget → 503 Service Unavailable, other → 500. Codex
// bot review on PR #26 P2 (headless activate-window no-op masks failure).
func (s *Server) OnActivateWindow(fn func() error) { s.onActivateWindow = fn }

// Port returns the actual TCP port the server is bound to. Zero until
// Start has signaled ready.
func (s *Server) Port() int { return int(s.port.Load()) }

// HubMcpEndpointActive returns true when the hub-mcp listener is
// CURRENTLY running — published in this process AND its serve
// goroutine has not exited.
//
// Issue #161 P2 closure (persisted-vs-runtime hub gate badge): the
// settings DTO emits this as `actual_hub_endpoint_enabled` so the
// frontend can render the same "restart required" badge convention
// established for `actual_port` (when persisted != runtime).
//
// Codex bot r2 P2 closure on PR #168: the prior implementation
// returned `s.hubMcpComp.Load() != nil` alone, which reported
// "ever-published" rather than "currently live". A post-startup
// listener death (accept-loop fatal, etc.) would leave the
// hub-mcp.log "hub-listener-down" event behind but the badge
// would stay hidden because hubMcpComp was still non-nil. Now we
// also consult the bundle's Alive() flag, which the serve
// goroutine clears on any exit path.
func (s *Server) HubMcpEndpointActive() bool {
	comp := s.hubMcpComp.Load()
	if comp == nil {
		return false
	}
	return comp.Alive()
}

// HubMcpBoundPort returns the live hub listener's actually-bound TCP port and
// true when the gate-ON hub listener is currently running; (0, false)
// otherwise. The port is the one the listener bound at startup (the
// authoritative runtime value, not the persisted endpoint file which may lag a
// reset). B4 (bot R3): the Groups GET path uses this to build a usable
// /g/<group>/mcp connection URL only when the hub is actually serving it.
func (s *Server) HubMcpBoundPort() (int, bool) {
	comp := s.hubMcpComp.Load()
	if comp == nil || !comp.Alive() {
		return 0, false
	}
	return comp.port, true
}

// Start binds 127.0.0.1:<cfg.Port>, signals `ready` once the listener
// is accepting, then blocks in ListenAndServe. Returns when ctx is
// canceled (graceful shutdown, 5s deadline) or the listener errors.
// http.ErrServerClosed is returned as nil.
//
// Phase 4 (G4): after the gui-server listener is up and BEFORE
// `ready` is signaled, this method ALSO binds the hub-mcp listener
// when `gui_server.hub_endpoint_enabled=true` (separate socket at the
// persisted hub-mcp port). Bind failure is non-fatal to the gui-server
// — the operator still gets the GUI; the hub side stays gate-OFF until
// the operator runs the documented rotation chain.
func (s *Server) Start(ctx context.Context, ready chan<- struct{}) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.cfg.Port))
	if err != nil {
		return fmt.Errorf("bind 127.0.0.1:%d: %w", s.cfg.Port, err)
	}
	s.port.Store(int32(ln.Addr().(*net.TCPAddr).Port))
	s.srv = &http.Server{
		Handler:           s.httpHandler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Phase 4 (G4) — hub listener wiring. The gate is read from
	// gui-preferences.yaml directly (Phase 5 adds the registry entry +
	// Settings UI toggle). Bind failure surfaces via LogHubMcpEvent but
	// does NOT abort gui-server startup; an operator can still reach
	// the GUI to investigate.
	//
	// codex bot phase4 r8 P1 closure on PR #158: signal GUI readiness
	// BEFORE starting hub startup. BindHubMcpListener acquires
	// hub-mcp.lock via blocking flock; under contention (e.g. a
	// concurrent `mcphub hub-mcp regenerate-token` CLI run) the hub
	// startup can block for several seconds. The pre-r8 order made
	// callers waiting on `ready` block on that same flock even though
	// the GUI listener was already bound — defeating the
	// "hub failure is non-fatal to gui-server" contract. Close ready
	// + start srv.Serve FIRST; run hub startup AFTER. Shutdown still
	// handles a nil s.hubMcpComp cleanly.
	close(ready)
	errCh := make(chan error, 1)
	go func() { errCh <- s.srv.Serve(ln) }()

	// codex bot phase4 r9 P1 closure on PR #158: run hub startup in
	// a goroutine so ctx.Done() during the bind transaction (e.g. a
	// sibling `mcphub hub-mcp regenerate-token` is holding
	// hub-mcp.lock under blocking flock acquisition) can still tear
	// down the gui-server promptly. We wait up to 2s for the init
	// to settle at shutdown time so a successful bind doesn't leak
	// its listener; beyond that the process-exit flock release
	// unblocks the stuck acquireHubMcpLock anyway.
	//
	// codex bot phase4 r10 P1 closure on PR #158: hubInitCtx is a
	// derived cancel context, NOT the parent ctx, so the shutdown
	// path can issue an explicit hubInitCancel() to unwind a stuck
	// goroutine BEFORE the 2s wait elapses. startHubMcpListener now
	// honors ctx — its lock acquisition uses
	// acquireHubMcpLockContext, so hubInitCancel() unblocks the
	// flock acquisition within ~10 ms. Even the errCh-shutdown path
	// (gui-server listener died unexpectedly) needs to cancel the
	// goroutine so the goroutine does NOT later store a live
	// listener into s.hubMcpComp after Start has already returned —
	// the explicit cancel + defer below covers both shutdown paths.
	hubEnabled := readHubEndpointGateFromSettings()
	hubInitCtx, hubInitCancel := context.WithCancel(ctx)
	defer hubInitCancel()
	hubInitDone := make(chan struct{})
	go func() {
		defer close(hubInitDone)
		hubComp, hubErr := startHubMcpListener(hubInitCtx, hubEnabled, s.api)
		if hubErr != nil {
			// codex bot phase4 r1 P2 closure on PR #158: surface
			// non-bind hub failures (token gen/persist, endpoint
			// load/write, manifest pre-gate refusal) on the gui-
			// server log so operators get an actionable signal
			// without tailing hub-mcp.log. The error is also
			// already structured-logged via LogHubMcpEvent inside
			// startHubMcpListener.
			log.Printf("hub-mcp listener startup failed (gate-OFF for this process): %v", hubErr)
			return
		}
		if hubComp == nil {
			return
		}
		// codex deep-sec phase4 r24 P1 closure on PR #158 (lane #1):
		// CAS-based ownership transfer between hub-init goroutine
		// and shutdown path. Pre-r24 code did:
		//   if hubInitCtx.Err() != nil { tear down } else { Store }
		// which is NOT one atomic transition: the goroutine could be
		// descheduled between the Err() check and the Store, letting
		// the shutdown path Load() == nil, return, THEN the goroutine
		// publishes a live listener after Start has already returned.
		//
		// New protocol:
		//   1. Goroutine attempts CAS(nil -> hubComp). If it fails,
		//      something else already owns the slot (impossible in
		//      practice, but defensive) — tear down our bundle.
		//   2. Re-check ctx after the publish. If canceled NOW,
		//      attempt CAS(hubComp -> nil) to take ownership back. If
		//      THAT CAS succeeds, we still own the bundle and tear it
		//      down. If it fails, the shutdown path already
		//      atomically Swap'd to nil and will tear down itself.
		//
		// The shutdown path uses Swap(nil) (below) — single atomic
		// take-the-bundle-or-take-nothing.
		if !s.hubMcpComp.CompareAndSwap(nil, hubComp) {
			ShutdownHubListener(context.Background(), hubComp)
			return
		}
		if hubInitCtx.Err() != nil {
			if s.hubMcpComp.CompareAndSwap(hubComp, nil) {
				ShutdownHubListener(context.Background(), hubComp)
			}
			// else: shutdown path already swapped — it owns teardown.
		}
	}()

	select {
	case <-ctx.Done():
		// codex bot phase4 r10 P1 closure on PR #158: cancel hub init
		// BEFORE the 2s wait so a flock-stuck goroutine unwinds via
		// context.Canceled within ~10 ms (the
		// acquireHubMcpLockContext retry cadence). The wait then
		// serves only to give a goroutine in late-stage bind enough
		// time to publish its listener for the subsequent
		// ShutdownHubListener call.
		hubInitCancel()
		select {
		case <-hubInitDone:
		case <-time.After(2 * time.Second):
		}
		// codex bot phase4 r3 P2 closure on PR #158: each shutdown
		// phase gets its OWN 5s budget. The earlier code shared one
		// shutdownCtx between ShutdownHubListener and s.srv.Shutdown;
		// if a slow hub drain consumed most of the budget, the gui-
		// server Shutdown would return "context deadline exceeded"
		// even on a healthy gui server, turning a normal cancellation
		// into an error and skipping graceful close under load.
		// Drain hub listener BEFORE the gui-server so any racing
		// internal-reload writes complete via the still-flockable
		// state-dir.
		//
		// codex deep-sec phase4 r24 P1 closure on PR #158 (lane #1):
		// atomic.Swap takes ownership of the bundle (or nil). If the
		// goroutine raced past us and stored a bundle, we get it
		// here and tear it down. If we get nil, the goroutine either
		// hasn't stored yet (its later CAS will tear down its own
		// bundle when it sees ctx canceled) or it never produced
		// a bundle (errored out). No double-shutdown.
		hubCtx, hubCancel := context.WithTimeout(context.Background(), 5*time.Second)
		ShutdownHubListener(hubCtx, s.hubMcpComp.Swap(nil))
		hubCancel()

		guiCtx, guiCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer guiCancel()
		if err := s.srv.Shutdown(guiCtx); err != nil {
			// codex deep-sec phase4 r24 P2 closure on PR #158 (lane #3):
			// graceful gui-server shutdown failed (typically context
			// deadline exceeded). Force-close active connections so
			// request goroutines unwind before we return — without
			// this, hung requests survive Start's return.
			_ = s.srv.Close()
			s.events.Close()
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		// G9: stop the persist worker after HTTP is quiesced, so
		// any pending events queued by in-flight handlers are
		// flushed to gui-events.log before the goroutine exits
		// (Codex P2 on PR #150 line 101 — without this, every
		// Start/Stop cycle leaks one drain goroutine).
		s.events.Close()
		return nil
	case err := <-errCh:
		// gui-server listener died; wait briefly for hub init to
		// settle so we capture its components, then drain.
		//
		// codex bot phase4 r10 P1 closure on PR #158: cancel hub init
		// here too. The defer hubInitCancel above guarantees
		// eventual cancellation, but an explicit call ensures the
		// goroutine unwinds DURING the 2s wait window (not after),
		// avoiding the race where the goroutine stores a live
		// listener into s.hubMcpComp after Start has returned.
		hubInitCancel()
		select {
		case <-hubInitDone:
		case <-time.After(2 * time.Second):
		}
		// codex deep-sec phase4 r24 P1 closure on PR #158 (lane #1):
		// same atomic Swap ownership transfer as the ctx.Done branch.
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
		ShutdownHubListener(drainCtx, s.hubMcpComp.Swap(nil))
		drainCancel()
		s.events.Close()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

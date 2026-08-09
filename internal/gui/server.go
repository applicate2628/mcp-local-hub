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
	"mcp-local-hub/internal/daemonrecovery"
)

// GUIReadHeaderTimeout is the single HTTP header-read policy shared by normal
// Server.Start and restart-child listener owners. Bind deadlines govern only
// listener acquisition and must not alter the lifetime HTTP policy.
const GUIReadHeaderTimeout = 10 * time.Second

const defaultGUIShutdownDrainTimeout = 5 * time.Second

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
	// RestartV3Enabled is the composition-root-resolved GUI restart gate.
	RestartV3Enabled bool
	// GUIShutdownDrainTimeout bounds ordinary HTTP draining during process
	// shutdown. Zero preserves the production five-second drain.
	GUIShutdownDrainTimeout time.Duration
	// RecoverySettlementPostCommitBudget is the injected maximum duration of
	// post-commit recovery work. Zero uses daemonrecovery's production bound.
	RecoverySettlementPostCommitBudget time.Duration
	// RecoverySettlementTerminalizationBudget is the injected occurrence
	// terminalization allowance added to the post-commit recovery bound. Zero
	// uses the audit-lock adapter's production lock budget.
	RecoverySettlementTerminalizationBudget time.Duration
}

// scanner is the narrow interface that the /api/scan handler needs.
// realScanner is the production adapter; tests inject their own.
type scanner interface {
	Scan() (*api.ScanResult, error)
}

type realScanner struct {
	guiPort func() int
}

func (r realScanner) Scan() (*api.ScanResult, error) {
	guiPort := 0
	if r.guiPort != nil {
		guiPort = r.guiPort()
	}
	return api.NewAPI().ScanFrom(api.ScanOpts{
		ConfigPaths: api.DefaultScanConfigPaths(),
		ManifestDir: "",
		GUIPort:     guiPort,
	})
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

type realMigrator struct {
	// guiPort returns the LIVE bound GUI/hub listener port (Server.Port). It is
	// read lazily at Migrate time (not at wiring time) because the port is only
	// assigned after the listener binds. Passed into MigrateOpts.GUIPort so the
	// dynamic-pool serena server is written with its /serena/mcp router URL.
	guiPort func() int
}

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
func (r realMigrator) Migrate(servers, clients []string) (*api.MigrateReport, error) {
	guiPort := 0
	if r.guiPort != nil {
		guiPort = r.guiPort()
	}
	return api.NewAPI().MigrateFrom(api.MigrateOpts{
		Servers:        servers,
		ClientsInclude: clients,
		GUIPort:        guiPort,
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

type realManifestPresence struct{}

func (realManifestPresence) ManifestExists(name string) (bool, error) {
	return api.NewAPI().ManifestExists(name)
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

// realCatalogManifestGetter is the production adapter for GET
// /api/catalog/manifest (D2 r2). It routes to api.CatalogManifestGet,
// the EMBED-ONLY reader whose membership gate excludes any disk-only name
// BEFORE the loader, so the cold-re-enable Re-add prefill can never echo a
// hand-planted disk manifest carrying a literal secret.
type realCatalogManifestGetter struct{}

func (realCatalogManifestGetter) CatalogManifestGet(name string) (string, error) {
	return api.NewAPI().CatalogManifestGet(name)
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

	// CleanupAggressiveSupported / AggressiveCleanup back the
	// /api/cleanup/aggressive route (the operator-confirmed override
	// that kills the live-rooted MCP-stdio fan-out the default safe
	// sweep correctly refuses to touch). Same Windows-only gate as the
	// orphan sweep — production checks runtime.GOOS, the test seam can
	// return true to exercise the handler cross-platform.
	CleanupAggressiveSupported() bool
	AggressiveCleanup(opts api.CleanupOpts) ([]api.OrphanProcess, error)
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

func (realCleanupAPI) CleanupAggressiveSupported() bool {
	return runtime.GOOS == "windows"
}

func (realCleanupAPI) AggressiveCleanup(opts api.CleanupOpts) ([]api.OrphanProcess, error) {
	return api.NewAPI().AggressiveCleanup(opts)
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

type restartCoordinatorStarter interface {
	Start() (RestartCoordinatorStart, error)
}

const hubProducerJoinTimeout = 2 * time.Second

// hubProducerShutdownBarrier is the one Server-owned lifetime boundary for
// every goroutine that can publish hubMcpComp. Shutdown closes admission before
// cancellation, waits up to the bounded producer-join budget, takes the current
// component, and permanently rejects late publication. A producer that outlives
// the wait must self-shut down when its publication is rejected.
type hubProducerShutdownBarrier struct {
	mu           sync.Mutex
	started      bool
	stopping     bool
	closed       bool
	cancel       context.CancelFunc
	producers    sync.WaitGroup
	shutdownOnce sync.Once
}

func (b *hubProducerShutdownBarrier) begin(parent context.Context) (context.Context, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.started || b.stopping || b.closed {
		ctx, cancel := context.WithCancel(parent)
		cancel()
		return ctx, false
	}
	b.started = true
	ctx, cancel := context.WithCancel(parent)
	b.cancel = cancel
	return ctx, true
}

func (b *hubProducerShutdownBarrier) launch(producer func()) bool {
	b.mu.Lock()
	if b.stopping || b.closed {
		b.mu.Unlock()
		return false
	}
	b.producers.Add(1)
	b.mu.Unlock()
	go func() {
		defer b.producers.Done()
		producer()
	}()
	return true
}

func (b *hubProducerShutdownBarrier) publish(slot *atomic.Pointer[HubListenerComponents], comp *HubListenerComponents, shutdown func(context.Context, *HubListenerComponents)) bool {
	if comp == nil {
		return false
	}
	b.mu.Lock()
	accepted := !b.stopping && !b.closed && slot.CompareAndSwap(nil, comp)
	b.mu.Unlock()
	if !accepted {
		shutdown(context.Background(), comp)
	}
	return accepted
}

func (b *hubProducerShutdownBarrier) shutdown(ctx context.Context, slot *atomic.Pointer[HubListenerComponents]) {
	if ctx == nil {
		ctx = context.Background()
	}
	b.shutdownOnce.Do(func() {
		b.mu.Lock()
		b.stopping = true
		cancel := b.cancel
		b.mu.Unlock()
		if cancel != nil {
			cancel()
		}

		producersDone := make(chan struct{})
		go func() {
			b.producers.Wait()
			close(producersDone)
		}()
		joinCtx, cancelJoin := context.WithTimeout(ctx, hubProducerJoinTimeout)
		select {
		case <-producersDone:
		case <-joinCtx.Done():
		}
		cancelJoin()

		ShutdownHubListener(ctx, slot.Swap(nil))
		b.mu.Lock()
		b.closed = true
		b.mu.Unlock()
	})
}

// RealStatusProvider is the production-default statusProvider. Tests inject
// their own; callers outside the package construct this one.
type RealStatusProvider = realStatusProvider

// Server is the GUI HTTP server. It owns the long-lived application state and
// delegates its restartable loopback HTTP lifecycle to GUIListenerOwner.
type Server struct {
	cfg         Config
	mux         *http.ServeMux
	guiListener *GUIListenerOwner
	// restartCoordinator is composed by the CLI for every gate-ON server,
	// including an activated RestartV3 child. Gate-OFF servers leave it nil.
	restartCoordinator restartCoordinatorStarter
	port               atomic.Int32 // set after Listen, read by Port()
	// api is the long-lived shared *api.API handle. Phase G2 places the
	// HealthSnapshot TTL+singleflight cache on this struct, so the
	// healthBackend adapter MUST reuse this instance — per-request
	// api.NewAPI() would create a fresh cache every call and defeat the
	// caching machinery. Other handlers still use the per-request
	// api.NewAPI() pattern (out of scope for this task; can adopt later
	// if their workloads benefit).
	api                   *api.API
	onActivateWindow      func() error
	scanner               scanner
	status                statusProvider
	health                healthBackend
	migrator              migrator
	demigrater            demigrater
	dismisser             dismisser
	manifestCreator       manifestCreator
	manifestValidator     manifestValidator
	manifestGetter        manifestGetter
	manifestPresence      manifestPresence
	manifestEditor        manifestEditor
	manifestLister        manifestLister
	manifestDeleter       manifestDeleter
	catalogLister         catalogLister
	catalogManifestGetter catalogManifestGetter
	marketplaceLister     marketplaceLister
	marketplaceRefresher  marketplaceRefresher
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
	daemonRecover            daemonRecoverer
	auditLock                *auditLockAdapter
	recoverySettlements      *recoverySettlementRegistry
	shutdownDrainTimeout     time.Duration
	// shutdownDrainObserved is a test-only lifecycle seam invoked after the
	// normal GUI HTTP drain finishes (or its ordinary deadline expires). It
	// makes the post-drain recovery-settlement join observable without turning
	// tests into elapsed-time probes. Production leaves it nil.
	shutdownDrainObserved func(error)
	stop                  stopper
	logs                  logsProvider
	extractor             extractor
	events                *Broadcaster
	hubHealth             *hubHealthTracker
	secrets               secretsAPI
	settings              settingsAPI
	backups               backupsAPI
	backupActions         backupActionsAPI
	cleanup               cleanupAPI
	clientInit            clientInitializer
	symlinkWriter         symlinkResolveWriter
	lspRegistrar          lspRegistrar
	groups                groupsAPI

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

	// hubRestartCh is the buffered-1 signal channel from the detect-only
	// HubListenerHealthWatcher to the Server-owned restart driver. The
	// restart counters and last-success timestamp are owned by that driver
	// goroutine; they discriminate flapping from a fresh outage and cap
	// rolling restart attempts.
	hubRestartCh             chan hubListenerRestartRequest
	hubRestartDriverAlive    atomic.Bool
	hubRestartConsecutive    int
	hubRestartLastSuccess    time.Time
	hubRestartWindowStart    time.Time
	hubRestartWindowAttempts int
	hubProducerShutdown      hubProducerShutdownBarrier

	// Hub startup test seams. Production leaves both nil and uses the settings
	// registry plus startHubMcpListenerWithOptions directly.
	hubEndpointGateFn     func(*api.API) bool
	startHubMcpListenerFn func(context.Context, bool, *api.API, startHubMcpListenerOptions) (*HubListenerComponents, error)

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
	// serenaBackendLossTrigger is a coalesced wake signal for the IPC
	// backend-loss reconcile ticker. The event subscriber owns writes; the GUI
	// lifecycle ticker owns reads. Nil preserves the pure interval floor for
	// bare test Servers that bypass NewServer.
	serenaBackendLossTrigger chan struct{}

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

	// serenaStopGate owns the per-workspace stop/forward gate. A path-bound
	// /serena/mcp request enters it immediately after workspace resolution and
	// before wake/daemon-session resolution; the idle sweeper starts the same
	// workspace gate while deciding to stop and releases later entrants only
	// after the stop write plus stale daemon-session invalidation finish. A
	// non-zero count means a request has started for that workspace, even if it
	// has not reached the upstream POST yet.
	serenaStopGate serenaWorkspaceStopGate

	// pruneEnoentMu guards pruneEnoentTicks: workspace path -> the orphan REASON
	// last observed on this path plus the number of CONSECUTIVE prune-sweep ticks
	// that observed the SAME reason. The workspace-daemon auto-prune sweeper
	// (workspace_prune_sweeper.go) prunes a deleted-dir OR dead-worktree
	// registration only after it crosses the 2-consecutive-SAME-reason-tick
	// threshold, absorbing a transient unmount; the entry is dropped (counter
	// reset) the moment the workspace becomes healthy, the row is removed, or the
	// prune fires. The reason is tracked alongside the count so a path that flips
	// signal between ticks (deleted-dir on tick 1, dead-worktree on tick 2)
	// RESETS to 1 — only the SAME signal observed on two consecutive ticks prunes,
	// so neither grace window is defeated by a reason flip (Finding 2). In-memory
	// only — NOT persisted (Phase 1 adds no new persisted state); a GUI restart
	// resets the counters, which is safe (a still-dead path simply re-accrues two
	// ticks before pruning). Agent-worktree and idle hits do NOT use this counter
	// (they prune immediately).
	pruneEnoentMu    sync.Mutex
	pruneEnoentTicks map[string]pruneEnoentEntry

	// LSP router dependencies for /lsp/<language>/mcp. This route is
	// intentionally separate from the Serena router because LSP workspace
	// proxies are sessionless upstreams and need no daemon-session handshake.
	lspRouterDeps atomic.Pointer[lspRouterDeps]

	// revealSepProcessWarned guards the once-per-process
	// SeparateProcess=1 detection warning
	// (separate_process_detect_windows.go). Windows-only; the !windows
	// detectSeparateProcessOnce shim never touches it.
	revealSepProcessWarned sync.Once
}

// NewServer constructs the Server. It registers the ping handler
// immediately so even a minimal Server answers /api/ping.
func NewServer(cfg Config) *Server {
	if cfg.PID == 0 {
		cfg.PID = os.Getpid()
	}
	s := &Server{
		cfg:                      cfg,
		mux:                      http.NewServeMux(),
		guiListener:              NewGUIListenerOwner(GUIReadHeaderTimeout),
		hubRestartCh:             make(chan hubListenerRestartRequest, 1),
		serenaBackendLossTrigger: make(chan struct{}, 1),
		guiProcessStart:          time.Now(),
		pruneEnoentTicks:         map[string]pruneEnoentEntry{},
	}
	s.serenaRouterSessions.onWorkspaceEmpty = s.handleSerenaRouterWorkspaceEmpty
	// Long-lived shared *API handle. Phase G2 (/api/health) places the
	// TTL+singleflight HealthSnapshot cache here so concurrent requests
	// reuse the same cache; the healthBackend adapter below references
	// THIS instance. Other handlers continue to use api.NewAPI() per
	// request — out of scope for this task.
	s.api = api.NewAPI()
	s.scanner = realScanner{guiPort: s.Port}
	s.status = realStatusProvider{}
	s.health = realHealthBackend{api: s.api}
	s.migrator = realMigrator{guiPort: s.Port}
	s.demigrater = realDemigrater{}
	s.dismisser = realDismisser{}
	s.manifestCreator = realManifestCreator{}
	s.manifestValidator = realManifestValidator{}
	s.manifestGetter = realManifestGetter{}
	s.manifestPresence = realManifestPresence{}
	s.manifestEditor = realManifestEditor{}
	s.manifestLister = realManifestLister{}
	s.manifestDeleter = realManifestDeleter{}
	s.catalogLister = realCatalogLister{}
	s.catalogManifestGetter = realCatalogManifestGetter{}
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
	s.daemonRecover = realDaemonRecoverer{}
	s.stop = realStopper{}
	s.logs = realLogs{}
	s.extractor = realExtractor{}
	s.events = NewBroadcaster()
	shutdownDrainTimeout := cfg.GUIShutdownDrainTimeout
	if shutdownDrainTimeout <= 0 {
		shutdownDrainTimeout = defaultGUIShutdownDrainTimeout
	}
	postCommitBudget := cfg.RecoverySettlementPostCommitBudget
	if postCommitBudget <= 0 {
		postCommitBudget = daemonrecovery.MaximumPostCommitDuration()
	}
	terminalizationBudget := cfg.RecoverySettlementTerminalizationBudget
	if terminalizationBudget <= 0 {
		terminalizationBudget = auditLockStoreLockTimeout
	}
	s.auditLock = newUnclaimedAuditLockAdapterWithTerminalizationBudget(s.events, terminalizationBudget)
	s.shutdownDrainTimeout = shutdownDrainTimeout
	s.recoverySettlements = newRecoverySettlementRegistry(postCommitBudget, terminalizationBudget, func(event Event) {
		if s.events != nil {
			s.events.Publish(event)
		}
	})
	s.hubHealth = newHubHealthTracker(func(e Event) {
		if s.events != nil {
			s.events.Publish(e)
		}
	})
	s.secrets = realSecretsAPI{}
	s.settings = realSettingsAPI{}
	s.backups = realBackupsAPI{}
	s.backupActions = realBackupActionsAPI{}
	s.cleanup = realCleanupAPI{}
	s.clientInit = realClientInitializer{}
	s.symlinkWriter = realSymlinkResolveWriter{}
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
	registerAdoptRoutes(s)
	registerDeAdoptRoutes(s)
	registerInstallRoutes(s)
	registerServerRoutes(s)
	registerReadinessRoutes(s)
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
	registerHubHealthRoutes(s)
	registerDaemonsRoutes(s)
	registerExportBundleRoutes(s)
	registerCleanupRoutes(s)
	registerForceKillRoutes(s)
	registerInitClientConfigRoutes(s)
	registerResolveSymlinkWriteRoutes(s) // A3 PR-2: guided symlink-consent write
	registerDaemonEnvRoutes(s)
	registerDaemonRecoverRoutes(s)
	registerWorkspacesRoutes(s)
	registerLSPRegisterRoutes(s)
	registerLSPTrustedRootsRoutes(s)
	registerSupervisorRestartRoutes(s)
	registerGUISelfRestartRoutes(s) // Wave 3: POST /api/gui/restart self re-exec with lock handoff
	registerStateRelaxSettingRoutes(s)
	registerSerenaRouterRoutes(s)
	registerLSPRouterRoutes(s)
	registerGroupsRoutes(s)               // groups Phase 5b-1: /api/groups CRUD authoring endpoint
	registerProjectsRoutes(s)             // per-project-GUI P2a: GET /api/projects/scan (read-only, Model B)
	registerProjectsToggleRoutes(s)       // per-project-GUI P3a: POST /api/projects/toggle (write backend)
	registerProjectsAggregateRoutes(s)    // per-project-GUI P3a: GET /api/projects (A+B+C aggregate)
	registerProjectsGroupBindingRoutes(s) // per-project-GUI P3c: POST /api/projects/group-binding (group↔project bind/unbind)
	return s
}

// Broadcaster exposes the SSE event bus. Tests publish into it directly;
// production callers (poller goroutine in Task 12+) use it the same way.
func (s *Server) Broadcaster() *Broadcaster { return s.events }

// GUIListenerOwner exposes the restartable GUI listener lifecycle. It is the
// sole binder/closer/rebinder; hub and event ownership remains on Server.
func (s *Server) GUIListenerOwner() *GUIListenerOwner { return s.guiListener }

// ConfigureRestartCoordinator completes the GUI-owned half of the parent
// restart protocol. In particular, it fixes the irreversible boundary to this
// Server's own hub listener: RestartCoordinator calls this owner-side close
// immediately before releasing the CLI-owned single-instance lease.
func (s *Server) ConfigureRestartCoordinator(deps RestartCoordinatorDependencies) error {
	deps.Listener = s.guiListener
	deps.FullHandler = s.httpHandler()
	deps.Events = s.events
	deps.CloseHub = s.closeOwnHubListenerForRestart
	coordinator, err := NewRestartCoordinator(deps)
	if err != nil {
		return err
	}
	s.restartCoordinator = coordinator
	return nil
}

func (s *Server) closeOwnHubListenerForRestart(ctx context.Context) {
	s.hubProducerShutdown.shutdown(ctx, &s.hubMcpComp)
}

func (s *Server) publishHubMcpComponent(comp *HubListenerComponents, shutdown func(context.Context, *HubListenerComponents)) bool {
	return s.hubProducerShutdown.publish(&s.hubMcpComp, comp, shutdown)
}

// Activated closes when the full GUI handler is serving. CLI-owned poller,
// tray, and browser work waits on this gate.
func (s *Server) Activated() <-chan struct{} { return s.guiListener.Activated() }

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
// — the operator still gets the GUI while the existing bounded hub
// restart driver retries the initial bind from a nil component.
func (s *Server) Start(ctx context.Context, ready chan<- struct{}) error {
	ln, err := s.guiListener.bind(ctx, s.cfg.Port)
	if err != nil {
		return err
	}
	return s.continueWithGUIListener(ctx, ready, s.guiListener, ln)
}

// ContinueWithGUIListener activates a listener already bound and serving in
// standby, then enters the exact post-bind lifecycle used by Start. The caller
// transfers owner and bound to Server for the remainder of the process.
func (s *Server) ContinueWithGUIListener(ctx context.Context, ready chan<- struct{}, owner *GUIListenerOwner, bound net.Listener) error {
	if owner == nil {
		return errors.New("continue GUI server: nil listener owner")
	}
	if bound == nil {
		return errors.New("continue GUI server: nil bound listener")
	}
	s.guiListener = owner
	return s.continueWithGUIListener(ctx, ready, owner, bound)
}

func (s *Server) continueWithGUIListener(ctx context.Context, ready chan<- struct{}, owner *GUIListenerOwner, ln net.Listener) error {
	if err := s.auditLock.activateStore(ctx); err != nil {
		_ = ln.Close()
		return fmt.Errorf("activate daemon recovery occurrence store: %w", err)
	}
	s.port.Store(int32(ln.Addr().(*net.TCPAddr).Port))

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
	if err := owner.ServeFull(ln, s.httpHandler()); err != nil {
		_ = ln.Close()
		return err
	}
	return s.runActivatedGUIListener(ctx, owner.Errors())
}

// runActivatedGUIListener is the shared hub-init, restart-driver, event, and
// shutdown continuation. Both normal Start and restart-child standby activation
// enter this single implementation after ServeFull.
func (s *Server) runActivatedGUIListener(ctx context.Context, errCh <-chan error) error {

	// Reveal-window flood advisory (bug
	// 2026-06-22-explorer-folder-window-orphan-flood): warn once if the
	// Windows "Launch folder windows in a separate process" setting is on,
	// since that is the precondition for the orphan explorer.exe flood that
	// `mcphub gui --force --reveal` can leave behind (one un-reapable
	// window per invocation). Run AFTER close(ready)+Serve and in a
	// goroutine: the warn path takes a blocking hub-mcp.log.lock flock, so
	// running it on the readiness-critical path could let a concurrent log
	// writer stall GUI readiness (codex bot #423 P2). No-op off-Windows;
	// fail-soft on read error; idempotent via its own sync.Once.
	go detectSeparateProcessOnce(s)

	// Hub initialization and the restart driver are admitted through one
	// Server-owned producer barrier. Closing the hub shuts admission, cancels
	// their shared context, and waits a bounded interval for both producers before
	// taking the current component. startHubMcpListener honors that context through
	// acquireHubMcpLockContext, so a blocked hub-mcp.lock acquisition unwinds
	// promptly rather than publishing after parent shutdown.
	hubEnabled := readHubEndpointGateFromSettings(s.api)
	if s.hubEndpointGateFn != nil {
		hubEnabled = s.hubEndpointGateFn(s.api)
	}
	if hubEnabled {
		s.hubHealth.set(HubHealthRecovering, "")
	}
	hubStartFn := startHubMcpListenerWithOptions
	if s.startHubMcpListenerFn != nil {
		hubStartFn = s.startHubMcpListenerFn
	}
	hubInitCtx, producersAdmitted := s.hubProducerShutdown.begin(ctx)
	if producersAdmitted {
		s.hubProducerShutdown.launch(func() {
			hubComp, hubErr := hubStartFn(hubInitCtx, hubEnabled, s.api, startHubMcpListenerOptions{
				server:         s,
				onUnresponsive: s.signalHubListenerRestart,
				onRecovered:    s.onHubListenerRecovered,
			})
			if hubErr != nil {
				// codex bot phase4 r1 P2 closure on PR #158: surface
				// non-bind hub failures (token gen/persist, endpoint
				// load/write, manifest pre-gate refusal) on the gui-
				// server log so operators get an actionable signal
				// without tailing hub-mcp.log. The error is also
				// already structured-logged via LogHubMcpEvent inside
				// startHubMcpListener.
				log.Printf("hub-mcp listener startup failed; retry scheduled through existing restart driver: %v", hubErr)
				if hubEnabled {
					s.signalInitialHubStartupFailure(hubErr)
				}
				return
			}
			if hubComp == nil {
				if hubEnabled {
					s.hubHealth.set(HubHealthDown, "")
				}
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
			if !s.publishHubMcpComponent(hubComp, ShutdownHubListener) {
				if hubEnabled {
					s.hubHealth.set(HubHealthDown, "")
				}
				return
			}
			if hubInitCtx.Err() != nil {
				if s.hubMcpComp.CompareAndSwap(hubComp, nil) {
					ShutdownHubListener(context.Background(), hubComp)
				}
				// else: shutdown path already swapped — it owns teardown.
				return
			}
			// BindHubMcpListener loaded and persisted this endpoint while holding
			// hub-mcp.lock. Hydrate only from that accepted component so startup has
			// no unlocked pre-bind read and cannot latch stale marker state.
			if hubComp.reconcilePending {
				s.hubHealth.markReconcilePending()
			}
			if hubComp.alive.Load() {
				s.hubHealth.markHealthy()
			}
		})
		s.hubRestartDriverAlive.Store(true)
		s.hubProducerShutdown.launch(func() {
			runHubListenerRestartDriver(hubInitCtx, s, hubListenerRestartDriverOptions{})
		})
	}

	select {
	case <-ctx.Done():
		s.recoverySettlements.closeAdmission()
		s.auditLock.beginClose()
		// Cancel producers and wait only up to the bounded join budget before
		// taking the current component. Publication admission is already closed,
		// so a producer that finishes late must shut down its own rejected component.
		// codex bot phase4 r3 P2 closure on PR #158: each shutdown
		// phase gets its OWN 5s budget. The earlier code shared one
		// shutdownCtx between ShutdownHubListener and GUIListenerOwner.Shutdown;
		// if a slow hub drain consumed most of the budget, the gui-
		// server Shutdown would return "context deadline exceeded"
		// even on a healthy gui server, turning a normal cancellation
		// into an error and skipping graceful close under load.
		// Drain hub listener BEFORE the gui-server so any racing
		// internal-reload writes complete via the still-flockable
		// state-dir.
		//
		hubCtx, hubCancel := context.WithTimeout(context.Background(), 5*time.Second)
		s.closeOwnHubListenerForRestart(hubCtx)
		hubCancel()

		guiCtx, guiCancel := context.WithTimeout(context.Background(), s.shutdownDrainTimeout)
		shutdownErr := s.guiListener.Shutdown(guiCtx)
		guiCancel()
		if s.shutdownDrainObserved != nil {
			s.shutdownDrainObserved(shutdownErr)
		}
		if shutdownErr != nil {
			// codex deep-sec phase4 r24 P2 closure on PR #158 (lane #3):
			// graceful gui-server shutdown failed (typically context
			// deadline exceeded). Force-close active connections so
			// request goroutines unwind before we return — without
			// this, hung requests survive Start's return.
			shutdownErr = fmt.Errorf("graceful shutdown: %w", shutdownErr)
		}
		settlementErr := s.recoverySettlements.wait()
		// G9: stop the persist worker after HTTP is quiesced, so
		// any pending events queued by in-flight handlers are
		// flushed to gui-events.log before the goroutine exits
		// (Codex P2 on PR #150 line 101 — without this, every
		// Start/Stop cycle leaks one drain goroutine).
		s.auditLock.close()
		s.events.Close()
		return errors.Join(shutdownErr, settlementErr)
	case err := <-errCh:
		s.recoverySettlements.closeAdmission()
		s.auditLock.beginClose()
		// A failed GUI listener uses the same cancel/join/take hub boundary as
		// normal cancellation, so neither producer can republish after return.
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
		s.closeOwnHubListenerForRestart(drainCtx)
		drainCancel()
		guiDrainCtx, guiDrainCancel := context.WithTimeout(context.Background(), s.shutdownDrainTimeout)
		guiDrainErr := s.guiListener.Shutdown(guiDrainCtx)
		guiDrainCancel()
		if s.shutdownDrainObserved != nil {
			s.shutdownDrainObserved(guiDrainErr)
		}
		settlementErr := s.recoverySettlements.wait()
		s.auditLock.close()
		s.events.Close()
		if errors.Is(err, http.ErrServerClosed) {
			return settlementErr
		}
		return errors.Join(err, settlementErr)
	}
}

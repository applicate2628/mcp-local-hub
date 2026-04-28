package gui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"mcp-local-hub/internal/api"
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

// migrator is the narrow interface the /api/migrate handler needs.
// The handler treats the operation as a bulk "apply changes" — any failed
// (server, client) row inside the MigrateReport is surfaced as an error.
// clients narrows the set of client bindings that get rewritten (empty means
// "every binding configured for these servers"); it maps directly onto
// api.MigrateOpts.ClientsInclude. The GUI matrix is per-cell (server × client),
// so the handler must be able to address a single cell without touching the
// server's other client bindings — hence clients rides alongside servers.
// realMigrator is the production adapter; tests inject their own.
type migrator interface {
	Migrate(servers, clients []string) error
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
// Per-row failures are aggregated into a single error that mirrors the CLI's
// behavior in internal/cli/migrate.go: api.MigrateFrom returns nil error when
// only per-row writes fail (the outer run is considered complete), recording
// each failure in MigrateReport.Failed. Dropping that slice would let
// /api/migrate return 204 after partial failures, so the GUI would clear its
// pending changes even though some client config rewrites were not applied.
func (realMigrator) Migrate(servers, clients []string) error {
	report, err := api.NewAPI().MigrateFrom(api.MigrateOpts{
		Servers:        servers,
		ClientsInclude: clients,
	})
	if err != nil {
		return err
	}
	if report != nil && len(report.Failed) > 0 {
		msgs := make([]string, 0, len(report.Failed))
		for _, f := range report.Failed {
			msgs = append(msgs, f.Server+"/"+f.Client+": "+f.Err)
		}
		return fmt.Errorf("%d migration row(s) failed: %s", len(report.Failed), strings.Join(msgs, "; "))
	}
	return nil
}

// demigrater is the narrow interface the /api/demigrate handler needs.
// Semantics mirror migrator: per-row failures inside the DemigrateReport
// are aggregated into a single error so partial failures cannot silently
// 204 and mislead the GUI into thinking the rollback succeeded.
// realDemigrater is the production adapter; tests inject their own.
type demigrater interface {
	Demigrate(servers, clients []string) error
}

type realDemigrater struct{}

// Demigrate delegates to api.Demigrate. ScanOpts left zero (embed-first
// manifest path, like realMigrator). clients is forwarded into
// DemigrateOpts.ClientsInclude; empty slice preserves the "all bindings
// configured in the manifest" shape.
//
// Per-row failures are aggregated into a single error for the same
// reason realMigrator aggregates: api.Demigrate returns nil error when
// only per-row writes fail, and dropping that slice would let the GUI
// clear its pending state after partial success.
func (realDemigrater) Demigrate(servers, clients []string) error {
	report, err := api.NewAPI().Demigrate(api.DemigrateOpts{
		Servers:        servers,
		ClientsInclude: clients,
	})
	if err != nil {
		return err
	}
	if report != nil && len(report.Failed) > 0 {
		msgs := make([]string, 0, len(report.Failed))
		for _, f := range report.Failed {
			msgs = append(msgs, f.Server+"/"+f.Client+": "+f.Err)
		}
		return fmt.Errorf("%d demigrate row(s) failed: %s", len(report.Failed), strings.Join(msgs, "; "))
	}
	return nil
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
	Restart(server string) ([]api.RestartResult, error)
}

type realRestarter struct{}

// Restart delegates to api.Restart(server, daemonFilter). The GUI handler
// targets "restart all daemons for one server," so daemonFilter is "".
// Per-task results are returned as-is; the handler inspects each Err field
// to decide 200 vs 207 (all empty vs any non-empty).
func (realRestarter) Restart(server string) ([]api.RestartResult, error) {
	results, err := api.NewAPI().Restart(server, "")
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

// RealStatusProvider is the production-default statusProvider. Tests inject
// their own; callers outside the package construct this one.
type RealStatusProvider = realStatusProvider

// Server is the GUI HTTP server. It owns a net/http.Server bound to
// 127.0.0.1, a ready-to-register mux, and a best-effort shutdown path.
type Server struct {
	cfg              Config
	mux              *http.ServeMux
	srv              *http.Server
	port             atomic.Int32 // set after Listen, read by Port()
	onActivateWindow func()
	scanner          scanner
	status           statusProvider
	migrator         migrator
	demigrater       demigrater
	dismisser        dismisser
	manifestCreator   manifestCreator
	manifestValidator manifestValidator
	manifestGetter    manifestGetter
	manifestEditor    manifestEditor
	installer        installer
	restart          restarter
	logs             logsProvider
	extractor        extractor
	events           *Broadcaster
	secrets          secretsAPI
	settings         settingsAPI
}

// NewServer constructs the Server. It registers the ping handler
// immediately so even a minimal Server answers /api/ping.
func NewServer(cfg Config) *Server {
	if cfg.PID == 0 {
		cfg.PID = os.Getpid()
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.scanner = realScanner{}
	s.status = realStatusProvider{}
	s.migrator = realMigrator{}
	s.demigrater = realDemigrater{}
	s.dismisser = realDismisser{}
	s.manifestCreator = realManifestCreator{}
	s.manifestValidator = realManifestValidator{}
	s.manifestGetter = realManifestGetter{}
	s.manifestEditor = realManifestEditor{}
	s.installer = realInstaller{}
	s.restart = realRestarter{}
	s.logs = realLogs{}
	s.extractor = realExtractor{}
	s.events = NewBroadcaster()
	s.secrets = realSecretsAPI{}
	s.settings = realSettingsAPI{}
	registerPingRoutes(s)
	registerAssetRoutes(s)
	registerScanRoutes(s)
	registerStatusRoutes(s)
	registerMigrateRoutes(s)
	registerDemigrateRoutes(s)
	registerDismissRoutes(s)
	registerManifestRoutes(s)
	registerInstallRoutes(s)
	registerServerRoutes(s)
	registerEventsRoutes(s)
	registerLogsRoutes(s)
	registerExtractManifestRoutes(s)
	registerSecretsRoutes(s)
	registerSettingsRoutes(s)
	return s
}

// Broadcaster exposes the SSE event bus. Tests publish into it directly;
// production callers (poller goroutine in Task 12+) use it the same way.
func (s *Server) Broadcaster() *Broadcaster { return s.events }

// OnActivateWindow registers the callback invoked when POST
// /api/activate-window is received. A second `mcphub gui` invocation
// posts here after handshaking with the incumbent; the callback is the
// hook that the tray + main window use to come to front.
func (s *Server) OnActivateWindow(fn func()) { s.onActivateWindow = fn }

// Port returns the actual TCP port the server is bound to. Zero until
// Start has signaled ready.
func (s *Server) Port() int { return int(s.port.Load()) }

// Start binds 127.0.0.1:<cfg.Port>, signals `ready` once the listener
// is accepting, then blocks in ListenAndServe. Returns when ctx is
// canceled (graceful shutdown, 5s deadline) or the listener errors.
// http.ErrServerClosed is returned as nil.
func (s *Server) Start(ctx context.Context, ready chan<- struct{}) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.cfg.Port))
	if err != nil {
		return fmt.Errorf("bind 127.0.0.1:%d: %w", s.cfg.Port, err)
	}
	s.port.Store(int32(ln.Addr().(*net.TCPAddr).Port))
	s.srv = &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	close(ready)

	errCh := make(chan error, 1)
	go func() { errCh <- s.srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

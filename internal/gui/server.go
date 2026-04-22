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
// realMigrator is the production adapter; tests inject their own.
type migrator interface {
	Migrate(servers []string) error
}

type realMigrator struct{}

// Migrate delegates to api.MigrateFrom. ScanOpts is left zero so
// ManifestDir defaults to "", which api.loadManifestForServer documents as
// the production embed-first path — this mirrors the CLI's scanManifestDir()
// returning "". The ScanOpts client-path fields are documented as unused by
// the migrate flow (see internal/api/migrate.go), so we do not populate them.
func (realMigrator) Migrate(servers []string) error {
	_, err := api.NewAPI().MigrateFrom(api.MigrateOpts{Servers: servers})
	return err
}

// restarter is the narrow interface the /api/servers/:name/restart handler
// needs. realRestarter is the production adapter; tests inject their own.
type restarter interface {
	Restart(server string) error
}

type realRestarter struct{}

// Restart delegates to api.Restart(server, daemonFilter). The GUI handler
// targets "restart all daemons for one server," so daemonFilter is "".
// Per-task failures are aggregated into a single error to match the CLI's
// "one or more daemons failed to restart" semantics (see internal/cli/restart.go).
func (realRestarter) Restart(server string) error {
	results, err := api.NewAPI().Restart(server, "")
	if err != nil {
		return err
	}
	var failed []string
	for _, r := range results {
		if r.Err != "" {
			failed = append(failed, r.TaskName+": "+r.Err)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("restart failed: %s", strings.Join(failed, "; "))
	}
	return nil
}

// logsProvider is the narrow interface the /api/logs/:server handler needs.
// The handler converts the stored log text to either a plain GET body or an
// SSE tail-follow stream. The daemon name is not exposed at this seam —
// realLogs pins it to "default", matching the single-daemon-per-server
// manifest shape the GUI scans in Phase 3B. Multi-daemon selection is a
// Phase 3B-II concern and would surface a new field on logsProvider.
type logsProvider interface {
	Logs(server string, tail int) (string, error)
}

type realLogs struct{}

// Logs delegates to api.LogsGet with daemon "default". This mirrors the
// single-daemon assumption the rest of Phase 3B makes (scan + status +
// migrate all address the default daemon) and keeps the GUI seam narrow.
// If a server is later packaged with multiple daemons, the GUI would need
// to surface a daemon picker and logsProvider would gain a second arg.
func (realLogs) Logs(server string, tail int) (string, error) {
	return api.NewAPI().LogsGet(server, "default", tail)
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
	restart          restarter
	logs             logsProvider
	events           *Broadcaster
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
	s.restart = realRestarter{}
	s.logs = realLogs{}
	s.events = NewBroadcaster()
	registerPingRoutes(s)
	registerAssetRoutes(s)
	registerScanRoutes(s)
	registerStatusRoutes(s)
	registerMigrateRoutes(s)
	registerServerRoutes(s)
	registerEventsRoutes(s)
	registerLogsRoutes(s)
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

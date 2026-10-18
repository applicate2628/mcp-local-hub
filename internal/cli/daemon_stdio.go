package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"mcp-local-hub/internal/daemon"
)

// The CLI owns only its listener/Serve worker. Child custody stays in the host.
// An incomplete Serve receipt remains attached to the returned error.
type stdioListenerOutcome struct {
	server                                  *http.Server
	listener                                net.Listener
	serveDone                               chan struct{}
	serveError                              error
	shutdownError, closeError, receiveError error
}

type stdioListenerError struct {
	owner *stdioListenerOutcome
	cause error
}

func (e *stdioListenerError) Error() string { return e.cause.Error() }
func (e *stdioListenerError) Unwrap() error { return e.cause }

func formatStdioExit(h *daemon.StdioHost) string {
	identity, wait, native := h.NativeExit()
	if !native {
		return formatChildExit(h.ExitState())
	}
	if !wait.Known {
		return " (original exit outcome unavailable)"
	}
	hex := ""
	if wait.ExitCode < 0 || wait.ExitCode > 255 {
		hex = fmt.Sprintf(" exit_hex=0x%x", uint32(wait.ExitCode))
	}
	return fmt.Sprintf(" (pid=%d exit_code=%d%s)", identity.PID, wait.ExitCode, hex)
}

func serveStdioHost(ctx context.Context, h *daemon.StdioHost, srv *http.Server) error {
	listener, bindErr := net.Listen("tcp", srv.Addr)
	if bindErr != nil {
		stopErr := h.Stop()
		if stopErr != nil {
			h.LogSupervisorEvent(fmt.Sprintf("stop after http bind error: %v", stopErr))
		}
		return errors.Join(fmt.Errorf("http server: %w", bindErr), stopErr)
	}
	outcome := &stdioListenerOutcome{server: srv, listener: listener, serveDone: make(chan struct{})}
	go func() { outcome.serveError = srv.Serve(listener); close(outcome.serveDone) }()
	var primary error
	trigger := "http server error"
	select {
	case <-outcome.serveDone:
		if !errors.Is(outcome.serveError, http.ErrServerClosed) {
			primary = fmt.Errorf("http server: %w", outcome.serveError)
		}
	case <-ctx.Done():
		trigger = "ctx.Done graceful shutdown"
	case <-h.ChildExited():
		trigger = "child exit"
		primary = fmt.Errorf("stdio child exited unexpectedly%s", formatStdioExit(h))
		h.LogSupervisorEvent(primary.Error())
	}
	stopErr := h.Stop()
	if stopErr != nil {
		h.LogSupervisorEvent(fmt.Sprintf("stop after %s: %v", trigger, stopErr))
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	outcome.shutdownError = srv.Shutdown(shutdown)
	if outcome.shutdownError != nil {
		// Close receives the same server/listener; it is not another child owner.
		outcome.closeError = srv.Close()
	}
	select {
	case <-outcome.serveDone:
		if !errors.Is(outcome.serveError, http.ErrServerClosed) {
			primary = errors.Join(primary, outcome.serveError)
		}
	case <-shutdown.Done():
		outcome.receiveError = fmt.Errorf("stdio HTTP Serve receipt: %w", shutdown.Err())
	}
	err := errors.Join(primary, stopErr, outcome.shutdownError, outcome.closeError, outcome.receiveError)
	if err != nil {
		return &stdioListenerError{owner: outcome, cause: err}
	}
	return nil
}

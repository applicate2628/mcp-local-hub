// daemon_restart_watcher.go — hot-swap (b) event-driven restart detection.
//
// The hub caches a per-daemon MCP session (Mcp-Session-Id) keyed by daemon port.
// When the supervisor restarts a daemon, that cached session goes stale. (a)'s
// failure-driven self-heal recovers it reactively (the first post-restart call
// fails, then re-inits + retries). This watcher is the (b) PROACTIVE path: it
// observes the supervisor's reported daemon state and marks the cached session
// stale the moment a restart is observed, so the NEXT call re-initializes BEFORE
// dispatching and the client never sees the stale-session failure.
//
// The restart signal is a per-port change in the supervisor's reported
// current_pid (DaemonStatus.PID, sourced from the IPC status the hub already
// polls for /api/status — no new channel, no new dependency). This is a
// best-effort STATE-OBSERVER, not a retry-backoff timer: the poll detects the
// REAL pid change (the actual restart); it never guesses a restart duration. The
// supervisor owns daemon lifecycle and reports it; the hub (session owner)
// observes and converges — the clean ownership direction.
//
// SOUNDNESS: the raw current_pid is a best-effort signal. A stop→restart in
// which the OS recycles the IDENTICAL PID is observed as no change and skipped —
// astronomically unlikely on the restart timescale, and FULLY COVERED by (a):
// the first post-restart call hits the stale session, dial-fails, and self-heals.
// So (b) is a latency optimization (no one-call lag) layered on (a)'s soundness,
// not a correctness dependency. (The supervisor's monotonic, PID-recycle-immune
// pid_generation is NOT on the IPC status wire; threading it through is a
// possible future hardening, unnecessary given the (a) backstop.)
package api

import (
	"context"
	"time"
)

// DaemonRestartWatcher observes supervisor daemon state and marks the hub's
// cached sessions stale on a per-port current_pid change (a restart). markStale
// is HubSessionStore.MarkPortStale; statusFn is the supervisor status seam
// (a.DaemonStatusSnapshot — cached + singleflight, shared with /api/status).
type DaemonRestartWatcher struct {
	statusFn  func(context.Context) ([]DaemonStatus, error)
	markStale func(port int) int
	interval  time.Duration
	// lastPID maps daemon port -> last-observed current_pid. Mutated only on the
	// Run/checkOnce goroutine, so it needs no lock.
	lastPID map[int]int
	// consecutiveErrors tracks status-fetch failures so a sustained supervisor-IPC
	// outage (which silently disables the proactive path) is logged ONCE on
	// transition, not per tick.
	consecutiveErrors int
}

// DefaultRestartWatchInterval is the observation cadence. It is the state-poll
// interval, NOT a retry backoff — it bounds how soon after a restart the
// proactive invalidation fires; (a)'s reactive self-heal covers anything that
// races inside this window.
const DefaultRestartWatchInterval = 2 * time.Second

// restartWatcherErrorLogThreshold is the number of consecutive status-fetch
// failures before the watcher logs a degraded warning (one per transition).
const restartWatcherErrorLogThreshold = 3

// NewDaemonRestartWatcher wires a watcher. statusFn returns the current
// per-daemon status (Port + PID); markStale flags every session's cached entry
// for a port (returns the count marked). A zero interval gets the default.
func NewDaemonRestartWatcher(statusFn func(context.Context) ([]DaemonStatus, error), markStale func(port int) int, interval time.Duration) *DaemonRestartWatcher {
	if interval <= 0 {
		interval = DefaultRestartWatchInterval
	}
	return &DaemonRestartWatcher{
		statusFn:  statusFn,
		markStale: markStale,
		interval:  interval,
		lastPID:   map[int]int{},
	}
}

// Run polls until ctx is cancelled. The first observation of each port only
// records its PID (a fresh watcher start must not mark every daemon stale); a
// subsequent DIFFERENT PID for the same port is the restart that marks stale.
func (w *DaemonRestartWatcher) Run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	// Seed once immediately so a restart right after start is caught on the next
	// tick rather than after a full interval of blind baseline.
	w.checkOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.checkOnce(ctx)
		}
	}
}

// checkOnce reads the current status and marks stale any port whose live
// current_pid changed since the last observation. A status-fetch error is
// non-fatal (the next tick retries) but is logged once on transition into a
// sustained-error state so a silent outage is operator-visible. A zero port or
// zero PID (stopped / not-yet-bound) is skipped so a stopped→started transition
// is observed as a fresh PID, not a spurious change against a 0 baseline.
func (w *DaemonRestartWatcher) checkOnce(ctx context.Context) {
	rows, err := w.statusFn(ctx)
	if err != nil {
		w.consecutiveErrors++
		if w.consecutiveErrors == restartWatcherErrorLogThreshold {
			_ = LogHubMcpEvent("warn", "restart-watcher-status-degraded", map[string]any{
				"consecutive_errors": w.consecutiveErrors,
				"err":                err.Error(),
			})
		}
		return
	}
	if w.consecutiveErrors >= restartWatcherErrorLogThreshold {
		_ = LogHubMcpEvent("info", "restart-watcher-status-recovered", map[string]any{
			"after_consecutive_errors": w.consecutiveErrors,
		})
	}
	w.consecutiveErrors = 0

	for _, r := range rows {
		if r.Port == 0 || r.PID == 0 {
			continue
		}
		prev, seen := w.lastPID[r.Port]
		w.lastPID[r.Port] = r.PID
		if seen && prev != r.PID {
			marked := w.markStale(r.Port)
			_ = LogHubMcpEvent("info", "restart-watcher-marked-stale", map[string]any{
				"port":            r.Port,
				"pid_old":         prev,
				"pid_new":         r.PID,
				"sessions_marked": marked,
			})
		}
	}
}

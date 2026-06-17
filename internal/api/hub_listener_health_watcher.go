// hub_listener_health_watcher.go — B1 footgun: hub-listener-hang
// observability (partial; full auto-recovery deferred).
//
// Background (work-items/backlog/2026-06-16-hub-listener-hang-no-recovery.md):
// under gate-ON, the hub aggregate listener is a fire-and-forget
// goroutine inside the GUI process and is the SINGLE path for all of a
// client's aggregated MCP servers. A serve-loop DEATH (fatal accept
// error) already logs `hub-listener-down` + flips the live badge. But a
// HANG — wedged accept loop, stuck handler, deadlock — with the GUI
// process still alive leaves all aggregated servers unreachable with NO
// automatic recovery and NO observability: `\mcp-local-hub-liveness`
// probes the SUPERVISOR lock, not the hub listener, so a live GUI with a
// hung listener passes the liveness probe.
//
// This watcher closes the OBSERVABILITY gap (not the recovery gap): it
// periodically TCP-dials the bound hub port and, on a bounded number of
// consecutive failed dials, emits ONE structured `warn` event so the
// previously-silent failure appears in the same hub-mcp.log stream as
// bind/lifecycle events. On recovery it emits an `info` event. It does
// NOT restart the listener.
//
// SCOPE / SOUNDNESS. A TCP dial detects a CLOSED or unreachable socket
// (the OS-level listener is gone, or the accept loop is wedged such that
// the kernel backlog fills and connects start failing). It does NOT
// detect a handler-level deadlock where accept still completes but the
// request hangs — that needs a full authed HTTP round-trip, which is
// itself heavyweight and could wedge, and is DEFERRED with full
// auto-recovery (see CLAUDE.md "Hub listener hang — observability"). The
// dial is read-only (no auth, no JSON-RPC, no state mutation) so it
// cannot itself destabilize the running hub.

package api

import (
	"context"
	"fmt"
	"net"
	"time"
)

// DefaultHubHealthProbeInterval is the dial cadence. It bounds how soon
// after a socket-level outage the watcher emits the warn event.
const DefaultHubHealthProbeInterval = 15 * time.Second

// hubHealthProbeTimeout caps a single TCP dial. A loopback connect to a
// live listener completes in well under a millisecond; this generous
// budget keeps a transient scheduler stall from being misread as an
// outage.
const hubHealthProbeTimeout = 2 * time.Second

// hubHealthUnresponsiveThreshold is the number of CONSECUTIVE failed
// dials before the watcher declares the listener unresponsive and emits
// the warn event (once, on transition). Requiring N>1 in a row absorbs a
// single spurious dial failure (e.g. momentary backlog saturation under
// a legitimate burst) so the warn fires only on a sustained outage.
const hubHealthUnresponsiveThreshold = 3

// HubListenerHealthWatcher periodically TCP-dials the bound hub port and
// emits structured observability events when the listener becomes
// unresponsive (and when it recovers). It is the B1 observability
// backstop for a HUNG (not crashed) hub listener.
type HubListenerHealthWatcher struct {
	// addr is the loopback hostport the hub listener is bound to, e.g.
	// "127.0.0.1:3439". Captured at construction from the bound port.
	addr string
	// port is the numeric port, carried in event bodies for correlation
	// with bind/lifecycle events.
	port     int
	interval time.Duration
	timeout  time.Duration

	// dialFn is the TCP-dial seam. Production uses net.Dialer.DialContext;
	// tests inject a deterministic stub. Returns nil when the socket is
	// reachable.
	dialFn func(ctx context.Context, network, addr string, timeout time.Duration) error

	// emit is the event seam. Production uses LogHubMcpEvent; tests
	// capture emitted events. Signature matches LogHubMcpEvent.
	emit func(level, event string, fields map[string]any) error

	// consecutiveFailures counts failed dials in a row. Mutated only on
	// the Run/probeOnce goroutine, so it needs no lock.
	consecutiveFailures int
	// unresponsive records whether we are currently in the
	// "warn already emitted" state, so the warn fires once per outage
	// (on transition) and a recovery info fires once on the way back.
	unresponsive bool
}

// NewHubListenerHealthWatcher wires a watcher for the listener bound at
// `port`. A zero/negative interval gets DefaultHubHealthProbeInterval.
// Production callers leave dialFn/emit nil to use the real
// net.Dialer + LogHubMcpEvent; tests inject both seams.
func NewHubListenerHealthWatcher(port int, interval time.Duration) *HubListenerHealthWatcher {
	if interval <= 0 {
		interval = DefaultHubHealthProbeInterval
	}
	return &HubListenerHealthWatcher{
		addr:     fmt.Sprintf("127.0.0.1:%d", port),
		port:     port,
		interval: interval,
		timeout:  hubHealthProbeTimeout,
		dialFn:   defaultHubHealthDial,
		emit:     LogHubMcpEvent,
	}
}

// defaultHubHealthDial is the production TCP-dial. Returns nil iff the
// connect succeeds (socket reachable); the connection is closed
// immediately — this is a liveness probe, not a request.
func defaultHubHealthDial(ctx context.Context, network, addr string, timeout time.Duration) error {
	d := net.Dialer{Timeout: timeout}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := d.DialContext(dctx, network, addr)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

// Run probes until ctx is cancelled (hub stop / GUI shutdown). The first
// probe fires immediately so a listener already wedged at watcher start
// is caught on the first interval rather than after a full cadence of
// blind silence. Fire-and-forget on the listener ctx — it unwinds solely
// on ctx cancellation and needs no explicit join (same lifecycle as
// DaemonRestartWatcher).
func (w *HubListenerHealthWatcher) Run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	w.probeOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.probeOnce(ctx)
		}
	}
}

// probeOnce performs one TCP dial and updates the consecutive-failure
// counter + unresponsive state, emitting a warn on the transition into
// unresponsive and an info on the transition back to reachable.
//
// A dial failure caused by ctx cancellation (shutdown in flight) is NOT
// counted as an outage — it is the expected teardown path, so we return
// early without mutating state.
func (w *HubListenerHealthWatcher) probeOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	err := w.dialFn(ctx, "tcp", w.addr, w.timeout)
	if ctx.Err() != nil {
		// Cancellation raced the dial; treat as teardown, not outage.
		return
	}
	if err != nil {
		w.consecutiveFailures++
		if !w.unresponsive && w.consecutiveFailures >= hubHealthUnresponsiveThreshold {
			w.unresponsive = true
			_ = w.emit("warn", "hub-listener-unresponsive", map[string]any{
				"port":                 w.port,
				"consecutive_failures": w.consecutiveFailures,
				"err":                  err.Error(),
				"note":                 "hub aggregate listener not accepting connections; restart the GUI to recover (auto-recovery is a deferred follow-up — see CLAUDE.md \"Hub listener hang\")",
			})
		}
		return
	}
	// Reachable. If we had previously declared unresponsive, emit a
	// recovery info event on the transition back.
	if w.unresponsive {
		w.unresponsive = false
		_ = w.emit("info", "hub-listener-probe-recovered", map[string]any{
			"port": w.port,
		})
	}
	w.consecutiveFailures = 0
}

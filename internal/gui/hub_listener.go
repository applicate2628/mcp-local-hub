// hub_listener.go — Phase 4 Task 4.4 (G4 unified hub MCP).
//
// Owns the hub-mcp listener lifecycle, separate from the gui-server
// listener. Called by Server.Start after the gui-server listener
// binds and after the gui_server.hub_endpoint_enabled gate evaluates
// to true. The hub listener is a SEPARATE socket at the operator-
// chosen port (persisted in hub-mcp.endpoint.json across restarts);
// the existing per-daemon URLs are unaffected by Phase 4.
//
// Bind ordering (spec §"Bind ordering"):
//
//   1. Validate <state-dir> sanity (handled by DaemonStateDir).
//   2. flock(hub-mcp.lock) — implicit in the EnsureHubTokens +
//      EnsureHubEndpoint helpers.
//   3. Load OR generate hub-mcp-tokens.json (EnsureHubTokens).
//   4. Load existing hub-mcp.endpoint.json (LoadHubEndpoint).
//   5. Validate participating manifests in strict mode
//      (ManifestValidateForHubBind).
//   6. listener := NewListenerWithSOExclusive("127.0.0.1:<port>").
//   7. Write hub-mcp.endpoint.json with the OS-assigned port if
//      step 4 returned port=0 (EnsureHubEndpoint).
//   8. funlock — implicit.
//   9. Serve.
//
// Step 7 is the load-bearing crash-safety hop: if it fails AFTER the
// listener exists, we Close the listener so no traffic is accepted
// without a published endpoint file.
//
// Bind-failure handling: a port-in-use error during step 6 triggers
// the credential-rotation warning per spec §"Pre-bind handling" —
// a same-user local process may have pre-bound the port to harvest
// the token bytes a future client would have sent.
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"Bind ordering".
// Plan: docs/superpowers/plans/2026-05-12-g4-unified-hub-mcp.md Task 4.4.

package gui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

const (
	hubMcpReadHeaderTimeout = 10 * time.Second
	hubMcpReadTimeout       = 15 * time.Second
	hubMcpIdleTimeout       = 120 * time.Second
)

// HubListenerComponents bundles the resources the hub listener owns
// across its lifetime. Bound to Server in Server.Start so Shutdown
// can tear them down on context cancellation.
type HubListenerComponents struct {
	srv     *http.Server
	store   *api.HubSessionStore
	handler *api.HubMcpHandler
	reload  *api.InternalReloadHandler
	port    int

	// listenerCancel stops per-listener background watchers. It is
	// derived from the long-lived hub init context so shutdown/restart can
	// cancel the old listener's watchers without stopping the restart driver.
	listenerCancel context.CancelFunc

	// alive is true between the serve goroutine's start and exit.
	// Server.HubMcpEndpointActive consults this AND the
	// hubMcpComp.Load() result so the `actual_hub_endpoint_enabled`
	// settings DTO field reflects CURRENT liveness, not "ever-
	// published". codex bot r2 P2 closure on PR #168: pre-fix, a
	// post-startup listener death (accept-loop fatal, etc.) was
	// only logged via LogHubMcpEvent; hubMcpComp stayed non-nil so
	// the badge stayed hidden even though the runtime endpoint
	// was actually down.
	alive atomic.Bool
}

// Alive reports whether the serve goroutine is currently running.
// Set to true at construction; cleared by a defer in the serve
// goroutine when Serve returns (any path: clean shutdown or
// fatal accept-loop error). Exported for cross-package access by
// Server.HubMcpEndpointActive.
func (c *HubListenerComponents) Alive() bool {
	if c == nil {
		return false
	}
	return c.alive.Load()
}

const (
	hubListenerRestartBaseBackoff            = 5 * time.Second
	hubListenerRestartSamePortRebindBackoff  = 30 * time.Second
	hubListenerRestartSamePortRebindMaxWait  = 6 * time.Minute
	hubListenerRestartMaxBackoff             = 5 * time.Minute
	hubListenerRestartMaxConsecutiveRestarts = 5
	hubListenerRestartRollingWindow          = 30 * time.Minute
	hubListenerRestartMaxAttemptsPerWindow   = 20
	hubListenerRestartStableWindow           = api.DefaultHubHealthProbeInterval*time.Duration(api.DefaultHubHealthUnresponsiveThreshold) + api.DefaultHubHealthProbeInterval
)

type hubListenerRestartDriverOptions struct {
	startFn    func(context.Context) (*HubListenerComponents, error)
	shutdownFn func(context.Context, *HubListenerComponents)
	emitFn     func(level, event string, fields map[string]any) error
	sleepFn    func(context.Context, time.Duration) bool
	nowFn      func() time.Time
}

type hubListenerRestartOutcome int

const (
	hubListenerRestartStopDriver hubListenerRestartOutcome = iota
	hubListenerRestartOutageExhausted
	hubListenerRestartSucceeded
)

func (s *Server) signalHubListenerRestart() {
	if s == nil || s.hubRestartCh == nil {
		return
	}
	select {
	case s.hubRestartCh <- struct{}{}:
	default:
	}
}

func runHubListenerRestartDriver(ctx context.Context, s *Server, opts hubListenerRestartDriverOptions) {
	if s == nil || s.hubRestartCh == nil {
		return
	}
	opts = normalizeHubListenerRestartDriverOptions(s, opts)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.hubRestartCh:
			if restartHubListenerWithOutcome(ctx, s, opts) == hubListenerRestartStopDriver {
				return
			}
		}
	}
}

func normalizeHubListenerRestartDriverOptions(s *Server, opts hubListenerRestartDriverOptions) hubListenerRestartDriverOptions {
	if opts.startFn == nil {
		opts.startFn = func(ctx context.Context) (*HubListenerComponents, error) {
			return startHubMcpListenerWithOptions(ctx, true, s.api, startHubMcpListenerOptions{
				preservePortOnReloadHandlerFailure: true,
				onUnresponsive:                     s.signalHubListenerRestart,
			})
		}
	}
	if opts.shutdownFn == nil {
		opts.shutdownFn = ShutdownHubListener
	}
	if opts.emitFn == nil {
		opts.emitFn = api.LogHubMcpEvent
	}
	if opts.sleepFn == nil {
		opts.sleepFn = hubListenerRestartSleep
	}
	if opts.nowFn == nil {
		opts.nowFn = time.Now
	}
	return opts
}

func restartHubListener(ctx context.Context, s *Server, opts hubListenerRestartDriverOptions) bool {
	return restartHubListenerWithOutcome(ctx, s, opts) == hubListenerRestartSucceeded
}

func restartHubListenerWithOutcome(ctx context.Context, s *Server, opts hubListenerRestartDriverOptions) hubListenerRestartOutcome {
	if ctx.Err() != nil {
		return hubListenerRestartStopDriver
	}

	oldTaken := false
	restartPort := 0
	var retryDelay time.Duration
	var samePortRebindWait time.Duration
	var previousInstanceID string
	var previousInstanceIDErr error
	for {
		if ctx.Err() != nil {
			return hubListenerRestartStopDriver
		}
		if !s.hubRestartLastSuccess.IsZero() && opts.nowFn().Sub(s.hubRestartLastSuccess) >= hubListenerRestartStableWindow {
			s.hubRestartConsecutive = 0
			s.hubRestartLastSuccess = time.Time{}
		}
		if s.hubRestartConsecutive >= hubListenerRestartMaxConsecutiveRestarts {
			if restartPort == 0 {
				if comp := s.hubMcpComp.Load(); comp != nil {
					restartPort = comp.port
				}
			}
			fields := map[string]any{
				"attempts":                 s.hubRestartConsecutive,
				"max_consecutive_restarts": hubListenerRestartMaxConsecutiveRestarts,
				"port":                     restartPort,
			}
			if oldTaken && s.hubMcpComp.Load() == nil {
				fields["no_signal_retry_scheduled"] = true
				fields["retry_delay"] = hubListenerRestartStableWindow.String()
			}
			_ = opts.emitFn("error", "hub-listener-restart-exhausted", fields)
			if ctx.Err() != nil {
				return hubListenerRestartStopDriver
			}
			if oldTaken && s.hubMcpComp.Load() == nil {
				if !opts.sleepFn(ctx, hubListenerRestartStableWindow) {
					return hubListenerRestartStopDriver
				}
				s.hubRestartConsecutive = 0
				retryDelay = 0
				continue
			}
			return hubListenerRestartOutageExhausted
		}
		if retryDelay > 0 {
			if !opts.sleepFn(ctx, retryDelay) {
				return hubListenerRestartStopDriver
			}
			if ctx.Err() != nil {
				return hubListenerRestartStopDriver
			}
			retryDelay = 0
		}

		if !oldTaken {
			preview := s.hubMcpComp.Load()
			if preview == nil {
				return hubListenerRestartStopDriver
			}
			if restartPort == 0 {
				restartPort = preview.port
			}
			if !hubListenerRestartCanAttempt(s, opts.nowFn()) {
				hubListenerRestartEmitAbandoned(opts, s, restartPort)
				return hubListenerRestartStopDriver
			}
			old := s.hubMcpComp.Swap(nil)
			if old == nil {
				return hubListenerRestartStopDriver
			}
			oldTaken = true
			restartPort = old.port
			previousInstanceID, previousInstanceIDErr = loadHubListenerRestartInstanceID()
			opts.shutdownFn(ctx, old)
			if ctx.Err() != nil {
				return hubListenerRestartStopDriver
			}
		} else if !hubListenerRestartCanAttempt(s, opts.nowFn()) {
			hubListenerRestartEmitAbandoned(opts, s, restartPort)
			return hubListenerRestartStopDriver
		}

		hubListenerRestartRecordAttempt(s)
		s.hubRestartConsecutive++
		attempt := s.hubRestartConsecutive

		newComp, err := opts.startFn(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return hubListenerRestartStopDriver
			}
			samePortRebindPending := isHubListenerSamePortRebindPendingErr(err)
			_ = opts.emitFn("warn", "hub-listener-restart-failed", map[string]any{
				"port":                     restartPort,
				"attempt":                  attempt,
				"err":                      err.Error(),
				"same_port_rebind_pending": samePortRebindPending,
			})
			if samePortRebindPending {
				s.hubRestartConsecutive--
				samePortRebindWait += hubListenerRestartSamePortRebindBackoff
				if samePortRebindWait >= hubListenerRestartSamePortRebindMaxWait {
					fields := map[string]any{
						"attempts":                         s.hubRestartConsecutive,
						"max_consecutive_restarts":         hubListenerRestartMaxConsecutiveRestarts,
						"port":                             restartPort,
						"same_port_rebind_wait":            samePortRebindWait.String(),
						"max_same_port_rebind_wait":        hubListenerRestartSamePortRebindMaxWait.String(),
						"same_port_rebind_pending_timeout": true,
					}
					if oldTaken && s.hubMcpComp.Load() == nil {
						fields["no_signal_retry_scheduled"] = true
						fields["retry_delay"] = hubListenerRestartStableWindow.String()
					}
					_ = opts.emitFn("error", "hub-listener-restart-exhausted", fields)
					if ctx.Err() != nil {
						return hubListenerRestartStopDriver
					}
					if oldTaken && s.hubMcpComp.Load() == nil {
						if !opts.sleepFn(ctx, hubListenerRestartStableWindow) {
							return hubListenerRestartStopDriver
						}
						s.hubRestartConsecutive = 0
						samePortRebindWait = 0
						retryDelay = 0
						continue
					}
					return hubListenerRestartOutageExhausted
				}
				retryDelay = hubListenerRestartSamePortRebindBackoff
				continue
			}
			if s.hubRestartConsecutive < hubListenerRestartMaxConsecutiveRestarts {
				retryDelay = hubListenerRestartBackoff(attempt)
			}
			continue
		}
		if newComp == nil {
			_ = opts.emitFn("warn", "hub-listener-restart-failed", map[string]any{
				"port":    restartPort,
				"attempt": attempt,
				"err":     "startHubMcpListener returned nil bundle",
			})
			if s.hubRestartConsecutive < hubListenerRestartMaxConsecutiveRestarts {
				retryDelay = hubListenerRestartBackoff(attempt)
			}
			continue
		}
		if !s.hubMcpComp.CompareAndSwap(nil, newComp) {
			opts.shutdownFn(context.Background(), newComp)
			return hubListenerRestartStopDriver
		}
		if ctx.Err() != nil {
			if s.hubMcpComp.CompareAndSwap(newComp, nil) {
				opts.shutdownFn(context.Background(), newComp)
			}
			return hubListenerRestartStopDriver
		}
		restartPort = newComp.port
		currentInstanceID, currentInstanceIDErr := loadHubListenerRestartInstanceID()
		instanceIDPreserved := previousInstanceIDErr == nil && currentInstanceIDErr == nil && previousInstanceID != "" && previousInstanceID == currentInstanceID
		instanceIDChanged := previousInstanceIDErr == nil && currentInstanceIDErr == nil && previousInstanceID != "" && currentInstanceID != "" && previousInstanceID != currentInstanceID
		fields := map[string]any{
			"port":                  restartPort,
			"attempt":               attempt,
			"instance_id_preserved": instanceIDPreserved,
		}
		if previousInstanceIDErr != nil {
			fields["previous_instance_id_err"] = previousInstanceIDErr.Error()
		}
		if currentInstanceIDErr != nil {
			fields["current_instance_id_err"] = currentInstanceIDErr.Error()
		}
		if instanceIDChanged {
			s.hubRestartLastSuccess = time.Time{}
			fields["client_impact"] = "installed clients will receive HTTP 401 until hub-mode configs are reconciled"
			fields["operator_action"] = "mcphub install --reconcile-hub-mode"
			_ = opts.emitFn("error", "hub-listener-restart-instance-id-changed", fields)
			return hubListenerRestartSucceeded
		}
		s.hubRestartLastSuccess = opts.nowFn()
		eventLevel := "warn"
		if instanceIDPreserved {
			eventLevel = "info"
		}
		_ = opts.emitFn(eventLevel, "hub-listener-restarted", fields)
		return hubListenerRestartSucceeded
	}
}

func hubListenerRestartCanAttempt(s *Server, now time.Time) bool {
	if s.hubRestartWindowStart.IsZero() || now.Sub(s.hubRestartWindowStart) >= hubListenerRestartRollingWindow {
		s.hubRestartWindowStart = now
		s.hubRestartWindowAttempts = 0
	}
	return s.hubRestartWindowAttempts < hubListenerRestartMaxAttemptsPerWindow
}

func hubListenerRestartRecordAttempt(s *Server) {
	s.hubRestartWindowAttempts++
}

func hubListenerRestartEmitAbandoned(opts hubListenerRestartDriverOptions, s *Server, port int) {
	_ = opts.emitFn("error", "hub-listener-restart-abandoned", map[string]any{
		"attempts":                        s.hubRestartWindowAttempts,
		"max_attempts_per_rolling_window": hubListenerRestartMaxAttemptsPerWindow,
		"rolling_window":                  hubListenerRestartRollingWindow.String(),
		"port":                            port,
		"operator_action":                 "manual hub-listener intervention required",
		"reason":                          "restart attempt rolling window exhausted",
	})
}

func loadHubListenerRestartInstanceID() (string, error) {
	ep, err := api.LoadHubEndpoint()
	if err != nil {
		return "", err
	}
	return ep.InstanceID, nil
}

func hubListenerRestartBackoff(attempt int) time.Duration {
	if attempt <= 1 {
		return hubListenerRestartBaseBackoff
	}
	d := hubListenerRestartBaseBackoff
	for i := 1; i < attempt; i++ {
		if d >= hubListenerRestartMaxBackoff/2 {
			return hubListenerRestartMaxBackoff
		}
		d *= 2
	}
	if d > hubListenerRestartMaxBackoff {
		return hubListenerRestartMaxBackoff
	}
	return d
}

func hubListenerRestartSleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// readHubEndpointGateFromSettings reads gui_server.hub_endpoint_enabled
// from the gui-preferences.yaml file directly. The settings registry
// does not yet carry this key (Phase 5 adds it); we bypass the
// registry to keep Phase 4 free of cross-task settings work.
//
// Returns false when the file is absent, the key is absent, the value
// is not exactly "true", or the YAML is malformed. This is the
// fail-closed posture spec §"Settings + CLI surface" demands — a
// corrupt settings file MUST NOT silently flip the gate on.
func readHubEndpointGateFromSettings() bool {
	path := api.SettingsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	raw := map[string]string{}
	if uerr := yaml.Unmarshal(data, &raw); uerr != nil {
		return false
	}
	return raw["gui_server.hub_endpoint_enabled"] == "true"
}

// startHubMcpListener implements the spec §"Bind ordering" steps 1-9.
// Called from Server.Start AFTER the gui-server listener is up. The
// `enabled` argument lets the caller short-circuit without inspecting
// the settings file (tests inject the gate directly).
//
// Returns a HubListenerComponents bundle on success. The returned
// srv is already serving in a background goroutine; the store has its
// own sweep goroutine. The caller MUST call ShutdownHubListener on
// ctx cancellation to drain both.
//
// On bind failure during step 6, surfaces the underlying net error +
// emits the credential-rotation warning via LogHubMcpEvent. The
// caller's gui-server keeps running (gate-OFF semantics for the rest
// of this process lifetime); the operator must investigate and run
// the rotation CLI before the next start.
//
// On step-7 write failure, closes the listener before returning the
// error so no traffic is accepted without a published endpoint file
// (spec §"Bind ordering" rollback note).
func startHubMcpListener(ctx context.Context, enabled bool, a *api.API) (*HubListenerComponents, error) {
	return startHubMcpListenerWithOptions(ctx, enabled, a, startHubMcpListenerOptions{})
}

type startHubMcpListenerOptions struct {
	preservePortOnReloadHandlerFailure bool
	reloadHandlerFn                    func(context.Context) (*api.InternalReloadHandler, error)
	onUnresponsive                     func()
	healthProbeInterval                time.Duration
}

func startHubMcpListenerWithOptions(ctx context.Context, enabled bool, a *api.API, opts startHubMcpListenerOptions) (*HubListenerComponents, error) {
	if !enabled {
		return nil, nil
	}
	if opts.reloadHandlerFn == nil {
		opts.reloadHandlerFn = api.NewInternalReloadHandler
	}

	// codex bot phase4 r10 P1 closure on PR #158: short-circuit if
	// ctx is already canceled BEFORE we touch hub-mcp.lock. The pre-r10
	// code would still try to acquire the lock (blocking flock) even
	// though the caller (Server.Start) already gave up — the goroutine
	// could come back later, bind a listener, and store it in
	// hubMcpComp AFTER Server.Start returned. Fail fast instead.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("hub-mcp: %w", err)
	}

	// Steps 3-7 — lock-atomic bind transaction. codex deep-sec P1
	// closure on PR #158: the pre-r2 implementation called
	// EnsureHubTokens (locked), LoadHubEndpoint (no lock),
	// validateManifests (no lock), bind (no lock), EnsureHubEndpoint
	// (locked) — leaving four lock-free windows in between. Two
	// concurrent gui-server starts could both clear the same
	// persisted port=0 record, both bind distinct OS-assigned
	// ports, and race the endpoint persist. BindHubMcpListener
	// holds hub-mcp.lock across the entire sequence so concurrent
	// starts serialize. SO_EXCLUSIVEADDRUSE (Windows) + ordinary
	// loopback semantics (POSIX) make the second caller's bind fail
	// fast against the first caller's already-persisted port.
	//
	// codex bot phase4 r10 P1 closure on PR #158: ctx is threaded
	// into BindHubMcpListener so the lock acquisition itself is
	// bounded — under contention with a sibling CLI holding
	// hub-mcp.lock, ctx.Done() unwinds the bind transaction within
	// ~10 ms (acquireHubMcpLockContext retry cadence). Without this,
	// the goroutine would block on flock indefinitely past Start's
	// shutdown budget.
	res, berr := api.BindHubMcpListener(
		ctx,
		clients.SupportedClientNames(),
		func() error { return validateParticipatingManifestsForHubBind(a) },
	)
	if berr != nil {
		return nil, fmt.Errorf("hub-mcp: %w", berr)
	}
	ln := res.Listener
	ep := res.Endpoint
	port := ep.Port

	// Groups/namespaces Phase 3 (decision §"Phase 3 diagnostic finding",
	// Defect A): publish the ResolverSnapshot from the participating
	// manifests NOW that the gate-ON listener has bound. Before this
	// wiring NOTHING in production published a snapshot, so the live hub
	// LoadResolverSnapshot()'d nil at every session initialize → empty
	// IntendedParticipants → AggregateInitialize fanned out to nothing →
	// the aggregate exposed no tools (dormant). This is the choke point
	// the design names: build from the SAME manifest source the bind
	// pre-gate (validateParticipatingManifestsForHubBind) just validated.
	//
	// Gate-ON only by construction: this code is past the `enabled` short-
	// circuit, so a gate-OFF process never reaches it (no publish at
	// gate-OFF — the listener never runs, so a published snapshot would be
	// read by no one anyway).
	//
	// A publish failure is NON-fatal to the bind: the listener is already
	// up and a published-nil snapshot degrades to the same dormant
	// behavior that existed before this wiring (empty participants), not a
	// crash. Surface it as a structured warn so the dormant-aggregate
	// condition is observable rather than silent.
	if perr := publishResolverSnapshotForHubBind(ctx, a); perr != nil {
		_ = api.LogHubMcpEvent("warn", "resolver-snapshot-publish-failed", map[string]any{
			"port": port,
			"err":  perr.Error(),
		})
	}

	// Step 9 — set up mux + serve. Session store is per-listener so
	// a future "stop hub mode" path can tear down sessions cleanly.
	store := api.NewHubSessionStore(api.SessionStoreOpts{})
	handler := api.NewHubMcpHandler(store)
	handler.SetEndpoint(ep)
	// codex bot phase4 r5 P2 closure on PR #158: control-token
	// persistence failure surfaces as a returned error so the hub
	// listener refuses to come up with a silently-broken
	// /internal/reload-tokens. Rollback the bound listener so we
	// don't leave a half-initialized hub server running.
	reload, reloadErr := opts.reloadHandlerFn(ctx)
	if reloadErr != nil {
		_ = ln.Close()
		store.Close()
		// codex bot phase4 r8 P2 closure on PR #158: roll back the
		// persisted endpoint port so status / consumers of
		// hub-mcp.endpoint.json don't discover a dead port + PID
		// pair until the next successful startup. ResetHubPort sets
		// Port=0 and preserves StartedAt + InstanceID (port-only
		// mutation per its own r2 P2 closure). If the rollback
		// itself fails (e.g. flock contention with a sibling CLI),
		// surface that as an additional log line — the operator
		// must then manually `mcphub gui --reset-port` to clear the
		// stale endpoint.
		//
		// codex bot phase4 r12 P1 closure on PR #158: use the
		// ctx-aware variant so a sibling holder of hub-mcp.lock can
		// not freeze the rollback past Server.Start's shutdown
		// budget. If ctx is already canceled (the typical r10 race
		// — gui-server gave up on hub init), the rollback fails
		// fast with context.Canceled; the dead port + PID pair stays
		// on disk and the operator runs `mcphub gui --reset-port`
		// manually. That's strictly better than the goroutine
		// blocking past Start's return and mutating state after.
		//
		// Auto-restart opts out of this rollback. Installed client
		// configs already hold /clients/ and /g/ URLs with the
		// persisted port; clearing it would make the retry choose a
		// fresh OS port and orphan those gated URLs. The restart
		// driver emits hub-listener-restart-failed and retries against
		// the same persisted port instead.
		if !opts.preservePortOnReloadHandlerFailure {
			if rerr := api.ResetHubPortContext(ctx); rerr != nil {
				_ = api.LogHubMcpEvent("error", "hub-endpoint-rollback-failed", map[string]any{
					"err": rerr.Error(),
				})
			}
		}
		return nil, fmt.Errorf("hub-mcp: %w", reloadErr)
	}

	mux := http.NewServeMux()
	// /clients/ catches everything under /clients/<id>/mcp; the
	// handler's path parser validates the trailing /mcp + the client
	// id (gate 2).
	mux.Handle("/clients/", handler)
	// Groups/namespaces Phase 4b: the SAME handler serves /g/<group>/mcp
	// (the design's structurally-separate-prefix decision). The handler's
	// parseHubPathFromURL recognizes BOTH prefixes and maps a group to the
	// kind-namespaced "g:<group>" scope key; gate 2 rejects an unknown
	// group with the same empty-body 404 the unknown-client path uses. No
	// /clients/ behavior changes — the client branch is byte-identical.
	mux.Handle("/g/", handler)
	// codex bot phase4 r12 P2 closure on PR #158: also register the
	// bare /clients pattern (no trailing slash) so ServeMux does NOT
	// auto-301 /clients → /clients/ before the auth/path gates run.
	// The 301 would (a) emit a redirect response outside the
	// handler's 404-empty contract and (b) some redirect-following
	// clients may rewrite POST as GET on the redirect step,
	// silently dropping the JSON-RPC body. Returning 404 directly
	// matches the handler's gate-2 path-shape contract.
	//
	// codex bot phase4 r13 P2 closure on PR #158: use plain
	// WriteHeader(404) with no body. http.NotFound writes
	// "404 page not found\n" which breaks the handler's empty-body
	// 404 contract for all other gate-2 path-shape failures. Now
	// every gate-2 path-shape rejection returns the SAME shape:
	// empty body, status 404. Eliminates route-fingerprinting and
	// keeps callers from differentiating /clients vs /clients/foo.
	mux.HandleFunc("/clients", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	// Mirror the bare-prefix 404 guard for /g (same anti-301 +
	// empty-body-404 contract as /clients above).
	mux.HandleFunc("/g", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.Handle("/internal/reload-tokens", reload)

	// codex bot phase4 r23 P2 closure on PR #158: pre-filter the
	// request path BEFORE http.ServeMux gets a chance to apply its
	// built-in URL normalization (cleanPath collapsing `//`, `/./`,
	// `/../`). ServeMux would otherwise issue a 301 redirect for
	// any path that differs from path.Clean(path) — emitting a
	// Location header for malformed POSTs that the handler contract
	// says should return a quiet 404. Redirect-following POSTs may
	// also rewrite to GET on the second hop, silently dropping the
	// JSON-RPC body. Reject path-traversal / collapsed-slash inputs
	// directly with empty-body 404, matching every other gate-2
	// path-shape failure surface.
	muxedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cleaned := path.Clean(r.URL.Path); cleaned != r.URL.Path {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mux.ServeHTTP(w, r)
	})

	srv := newHubMcpHTTPServer(muxedHandler)

	// Allocate the components bundle BEFORE starting the serve
	// goroutine so the goroutine can mark `alive=false` on exit
	// (codex bot r2 P2 closure on PR #168 — accurate live-state
	// for the persisted-vs-runtime hub-endpoint badge).
	listenerCtx, listenerCancel := context.WithCancel(ctx)
	comp := &HubListenerComponents{
		srv:            srv,
		store:          store,
		handler:        handler,
		reload:         reload,
		port:           port,
		listenerCancel: listenerCancel,
	}
	comp.alive.Store(true)

	// Hot-swap (b) event-driven proactive re-init: watch the supervisor's daemon
	// state and mark a cached hub session stale the moment a daemon restart (a
	// per-port current_pid change) is observed, so the NEXT tools/call re-inits
	// BEFORE dispatching instead of failing first. Sourced off a.DaemonStatusSnapshot
	// — the SAME cached + singleflight-collapsed status /api/status already polls
	// (one fetch, two consumers; no redundant supervisor-IPC dial). Fire-and-forget
	// on the listener ctx: it unwinds solely on ctx cancellation (hub stop /
	// shutdown), so it needs no explicit join. (a)'s failure-driven self-heal
	// remains the backstop for the window between a restart and the next
	// observation (and for the astronomically-unlikely same-PID-recycle case).
	go api.NewDaemonRestartWatcher(a.DaemonStatusSnapshot, store.MarkPortStale, 0).Run(listenerCtx)

	// B1 footgun observability + v1 auto-restart trigger:
	// a HUNG (not crashed) hub listener with the GUI process still alive
	// is otherwise SILENT — the serve goroutine's `hub-listener-down`
	// only fires on a serve-loop DEATH, and `\mcp-local-hub-liveness`
	// probes the supervisor lock, not this listener. This watcher TCP-
	// dials the bound port on a cadence and emits a structured
	// `hub-listener-unresponsive` warn (once, on transition). The
	// optional callback only signals the Server-owned restart driver; the
	// watcher remains detect-only. v1 is intentionally TCP-dial only.
	// A handler-deadlock-specific authed round-trip probe is the named v2
	// follow-up because that probe can itself wedge or consume session cap.
	// Fire-and-forget on the listener ctx: unwinds solely on ctx
	// cancellation (hub stop / shutdown), same as the watcher above.
	healthWatcher := api.NewHubListenerHealthWatcher(port, opts.healthProbeInterval)
	healthWatcher.SetOnUnresponsive(opts.onUnresponsive)
	go healthWatcher.Run(listenerCtx)

	go func() {
		// Mark the listener dead on any exit path (clean shutdown via
		// ErrServerClosed AND fatal accept-loop errors). The badge
		// consumer (Server.HubMcpEndpointActive) re-reads this on
		// each /api/settings request, so a post-startup death is
		// reflected within one snapshot tick.
		defer comp.alive.Store(false)

		// codex bot phase4 r2 P2 closure on PR #158: surface non-
		// shutdown serve errors so operators can diagnose a hub
		// listener that died after startup reported `hub-listener-up`.
		// http.ErrServerClosed is the normal shutdown signal (returned
		// by Serve after Shutdown / Close) and not load-bearing here.
		// Anything else (accept loop fatal, unexpected close, etc.)
		// is structured-logged via LogHubMcpEvent so it appears in
		// the same hub-mcp.log stream as bind / lifecycle events.
		if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			_ = api.LogHubMcpEvent("error", "hub-listener-down", map[string]any{
				"port": port,
				"err":  serveErr.Error(),
			})
		}
	}()

	_ = api.LogHubMcpEvent("info", "hub-listener-up", map[string]any{
		"port": port,
	})

	return comp, nil
}

func newHubMcpHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: hubMcpReadHeaderTimeout,
		ReadTimeout:       hubMcpReadTimeout,
		IdleTimeout:       hubMcpIdleTimeout,
	}
}

// ShutdownHubListener drains the hub listener under a 5s timeout +
// closes the session store's sweep goroutine + removes the control
// token file. Idempotent: calling with a nil components is a no-op.
//
// Step-7 write failure means components may be nil; the caller's
// gui-server shutdown still runs but the hub side has nothing to
// drain.
func ShutdownHubListener(parentCtx context.Context, c *HubListenerComponents) {
	if c == nil {
		return
	}
	if c.listenerCancel != nil {
		c.listenerCancel()
	}
	shutCtx, cancel := context.WithTimeout(parentCtx, 5*time.Second)
	defer cancel()
	if c.srv != nil {
		// codex bot phase4 r6 P2 closure on PR #158: surface a
		// graceful-shutdown failure (typically context.DeadlineExceeded
		// when an in-flight /clients/.../mcp request did not finish
		// within the budget). We still proceed to tear down the
		// session store and reload handler — keeping them alive
		// indefinitely would leak goroutines and the control-token
		// file. But the operator now sees a structured log line
		// indicating graceful shutdown did not complete cleanly,
		// which can correlate with later "request timed out mid-
		// flight" diagnostics on the client side.
		if err := c.srv.Shutdown(shutCtx); err != nil {
			_ = api.LogHubMcpEvent("warn", "hub-shutdown-incomplete", map[string]any{
				"port": c.port,
				"err":  err.Error(),
			})
			// codex deep-sec phase4 r24 P2 closure on PR #158 (lane #3):
			// Shutdown returned an error (typically context deadline
			// exceeded) — graceful drain failed. The pre-r24 code
			// continued to close store/reload, leaving active hub
			// request goroutines alive (a long tools/call inside
			// the 60s daemon POST keeps the request handler alive
			// past the 5s graceful budget). Call srv.Close() to
			// force-close active connections so request goroutines
			// can unwind before the process exits.
			_ = c.srv.Close()
		}
	}
	if c.store != nil {
		c.store.Close()
	}
	if c.reload != nil {
		// codex bot phase4 r10 P1 closure on PR #158: pass shutCtx
		// (the 5s shutdown budget) so the reload-token flock
		// acquisition cannot block past that budget. A sibling
		// process holding hub-mcp.lock surfaces as
		// context.DeadlineExceeded here; the in-memory control token
		// is already cleared, so leaving the on-disk file behind is
		// harmless — the next hub start regenerates it under flock.
		if err := c.reload.Shutdown(shutCtx); err != nil {
			_ = api.LogHubMcpEvent("warn", "hub-control-token-cleanup-incomplete", map[string]any{
				"port": c.port,
				"err":  err.Error(),
			})
		}
	}
}

// validateParticipatingManifestsForHubBind iterates every server
// manifest known to this API instance + checks strict-mode validity.
// Returns the first failing manifest's error (one violation is
// enough to refuse the bind per spec §"Pre-gate"). An empty manifest
// set is acceptable (operator may be running gui without manifests).
//
// Wraps api.API.ManifestValidateForHubBind so callers in this file
// share the same gate.
func validateParticipatingManifestsForHubBind(a *api.API) error {
	if a == nil {
		return nil
	}
	scan, err := a.Scan()
	if err != nil {
		return fmt.Errorf("scan manifests: %w", err)
	}
	for _, entry := range scan.Entries {
		if !entry.ManifestExists {
			continue
		}
		// codex deep-sec P2 closure on PR #158: read with ManifestGet
		// (embedded-first) so manifests baked into the binary AND
		// disk overrides are validated against the same strict gate.
		// ManifestGetWithHash was disk-only — an embedded-only
		// manifest hit os.ErrNotExist and got silently skipped,
		// bypassing the bind pre-gate for the shipped manifest set.
		yaml, gerr := a.ManifestGet(entry.Name)
		if gerr != nil {
			// codex bot phase4 r1 P1 closure on PR #158: only the
			// scan-window race (manifest deleted between scan and read)
			// is benign. Permission errors, schema errors, or any
			// other read failure MUST fail the bind so the strict
			// pre-gate isn't silently bypassed.
			if errors.Is(gerr, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("manifest %q: read: %w", entry.Name, gerr)
		}
		if verr := a.ManifestValidateForHubBind(yaml); verr != nil {
			return fmt.Errorf("manifest %q: %w", entry.Name, verr)
		}
	}
	return nil
}

// publishResolverSnapshotForHubBind builds + publishes the hub's
// ResolverSnapshot from the participating manifests. This is the
// production publish choke point the groups/namespaces design names
// (decision §"Phase 3 diagnostic finding", Defect A): pre-this-wiring
// the snapshot publish path (PublishResolverSnapshot /
// BumpResolverOnManifestChange) had ZERO production callers, so the
// gate-ON hub aggregate read a nil snapshot and exposed no tools.
//
// Manifest source: the SAME set validateParticipatingManifestsForHubBind
// validates — a.Scan() entries with ManifestExists, loaded via
// a.ManifestGet (embed-first). Building from the validated set keeps the
// published topology consistent with what passed the bind pre-gate.
//
// Each manifest's client_bindings rows become Bindings[client] entries
// (keyed by the same client id parseClientPathFromURL yields), via the
// existing BumpResolverOnManifestChange atomic-swap publish. A manifest
// whose client_bindings is empty contributes nothing — additive by
// omission, identical to today when the file carries no bindings.
//
// A per-manifest parse failure is a hard error here (not a silent skip):
// the bind pre-gate already validated every manifest, so a parse failure
// at this point is a genuine inconsistency the operator should see, not a
// routine missing-server case. The caller treats the returned error as
// non-fatal to the bind (logs a warn) so a single bad manifest degrades
// to the dormant-aggregate behavior, never a failed gui startup.
func publishResolverSnapshotForHubBind(ctx context.Context, a *api.API) error {
	if a == nil {
		return nil
	}
	// R4-3 (bot R4): the manifest scan is now run UNDER hub-mcp.lock, inside
	// PublishGroupsSnapshotLocked, via this closure — so scan+publish is one
	// atomic critical section and concurrent manifest mutations serialize on
	// the flock, each publishing the LATEST on-disk manifest set (no stale
	// scan can clobber a newer publish). The closure reads client config, the
	// workspace registry, process snapshots, and embed-first manifests — NONE
	// of which acquire hub-mcp.lock — so running it under the held lock is
	// deadlock-free (verified: a.Scan / a.ManifestGet / config.ParseManifest
	// have no path to acquireHubMcpLock).
	scanManifests := func() ([]config.ServerManifest, error) {
		scan, err := a.Scan()
		if err != nil {
			return nil, fmt.Errorf("scan manifests: %w", err)
		}
		manifests := make([]config.ServerManifest, 0, len(scan.Entries))
		for _, entry := range scan.Entries {
			if !entry.ManifestExists {
				continue
			}
			// Embed-first read, matching validateParticipatingManifestsForHubBind.
			yamlStr, gerr := a.ManifestGet(entry.Name)
			if gerr != nil {
				// Scan-window race (manifest deleted between scan and read) is
				// benign — skip it. Any other read error is surfaced.
				if errors.Is(gerr, os.ErrNotExist) {
					continue
				}
				return nil, fmt.Errorf("manifest %q: read: %w", entry.Name, gerr)
			}
			m, perr := config.ParseManifest(strings.NewReader(yamlStr))
			if perr != nil {
				return nil, fmt.Errorf("manifest %q: parse: %w", entry.Name, perr)
			}
			manifests = append(manifests, *m)
		}
		return manifests, nil
	}
	// Groups/namespaces Phase 4a (DATA layer): fold groups.yaml into the
	// SAME published snapshot under kind-namespaced "g:<group>" keys, and
	// ensure a per-group hub-token row (loopback-only — the §D auth seam).
	//
	// P3-2 (opus-arch) + R4-3/R4-4 (bot R4): the scan → groups read →
	// token-ensure → publish span is serialized under ONE held hub-mcp.lock
	// inside PublishGroupsSnapshotLocked so a concurrent GUI groups OR manifest
	// mutation can never make this read an intermediate config and publish a
	// torn / out-of-order topology. The flock is acquired ctx-bounded (R4-4) so
	// a stuck sibling holder cannot freeze GUI startup/shutdown past its budget.
	// No deadlock: this runs AFTER any RMW / bind lock was released, the scan
	// closure does not acquire hub-mcp.lock, and the helper only calls in-flock
	// ("…Locked") halves that do not re-acquire.
	//
	// A bad / unreadable groups.yaml must NEVER fault the client snapshot
	// publish (additive-by-omission, decision claim 5): the helper degrades a
	// load error to "no groups" with a structured warn, so the bare-<client>
	// bindings still publish exactly as before. A token-ensure failure is
	// surfaced (deferred — the snapshot publishes first) so the GUI mutation
	// tail reports restart_required rather than a false full success.
	return api.PublishGroupsSnapshotLocked(ctx, scanManifests)
}

// isMissingHubEndpoint is true if err describes a "hub-mcp.endpoint.json
// does not yet exist" case. Endpoint helpers return wrapped errors;
// the wrapper inspects the underlying cause without leaking the
// secure-write error chain (which carries paths we'd otherwise need
// to redact).
func isMissingHubEndpoint(err error) bool {
	// Best-effort: rely on os.IsNotExist via errors.Is for the most
	// common case. The endpoint helpers wrap os.PathError via
	// fmt.Errorf("%w", ...) so the Is chain matches.
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	return false
}

// hubControlTokenPath returns the absolute path to
// <state-dir>/hub-mcp-control.token. Test helpers use this to verify
// the file exists post-construction and disappears post-Shutdown.
// Production callers should NOT read this file outside the rotation
// CLI's flock-protected pipeline.
func hubControlTokenPath() (string, error) {
	dir, err := api.DaemonStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hub-mcp-control.token"), nil
}

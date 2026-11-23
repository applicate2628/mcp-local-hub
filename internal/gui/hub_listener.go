package gui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/mcproute"
)

// HubListenerComponents is the GUI registration and health projection for a
// listener whose transport is owned by mcproute.
type HubListenerComponents struct {
	listener         *mcproute.HubListener
	port             int
	reconcilePending bool
	listenerCancel   context.CancelFunc
	alive            atomic.Bool
}

// Alive reports current transport liveness.
func (c *HubListenerComponents) Alive() bool {
	if c == nil {
		return false
	}
	if c.listener != nil {
		return c.listener.Alive()
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
	hubInitialBindPortOwnerProbeTimeout      = 3 * time.Second
)

type hubListenerRestartDriverOptions struct {
	startFn    func(context.Context) (*HubListenerComponents, error)
	shutdownFn func(context.Context, *HubListenerComponents)
	emitFn     func(level, event string, fields map[string]any) error
	sleepFn    func(context.Context, time.Duration) bool
	nowFn      func() time.Time
	cause      hubListenerRestartCause
	// initialBindPort is populated only for a confirmed persisted-port socket
	// bind refusal. Pre-bind startup failures leave it zero and never probe.
	initialBindPort int

	initialBindPortNeedsRotationFn func(context.Context, int) bool
	rotateHubInstanceIDFn          func(context.Context) (api.HubEndpoint, error)
}

type hubInitialBindPortOwnerDependencies struct {
	portOwner       func(context.Context, int) (pid int, ok bool, err error)
	processIdentity func(int) (ProcessIdentity, error)
}

type hubListenerRestartCause uint8

const (
	hubListenerRestartCauseUnresponsive hubListenerRestartCause = iota
	hubListenerRestartCauseInitialStartupFailed
	hubListenerRestartCauseInitialBindFailed
)

func (c hubListenerRestartCause) String() string {
	switch c {
	case hubListenerRestartCauseUnresponsive:
		return "unresponsive"
	case hubListenerRestartCauseInitialStartupFailed:
		return "initial-startup-failed"
	case hubListenerRestartCauseInitialBindFailed:
		return "initial-bind-failed"
	default:
		return fmt.Sprintf("unknown-%d", c)
	}
}

type hubListenerRestartRequest struct {
	cause           hubListenerRestartCause
	initialBindPort int
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
	// Only promise recovery while the once-started restart driver can consume the
	// signal. A non-shutdown driver exit is permanent, so a later watcher signal
	// must report down instead of an absorbing "recovering" state.
	if s.hubRestartDriverAlive.Load() {
		s.hubHealth.set(HubHealthRecovering, "")
	} else {
		s.hubHealth.set(HubHealthDown, "")
	}
	s.enqueueHubListenerRestart(hubListenerRestartRequest{cause: hubListenerRestartCauseUnresponsive})
}

// signalInitialHubStartupFailure classifies the startup error at its producer.
// Only a confirmed persisted-port socket refusal gets the adversarial bind
// cause; every other setup/startup error keeps the existing nil-component retry
// path without port-owner inspection or InstanceID rotation.
func (s *Server) signalInitialHubStartupFailure(err error) {
	if port, ok := api.HubMcpListenerBindFailurePort(err); ok {
		s.signalInitialHubBindFailure(port)
		return
	}
	if s == nil || s.hubRestartCh == nil {
		return
	}
	s.hubHealth.set(HubHealthRecovering, "")
	s.enqueueHubListenerRestart(hubListenerRestartRequest{cause: hubListenerRestartCauseInitialStartupFailed})
}

// signalInitialHubBindFailure is the confirmed-bind nil-component admission
// into the restart driver. The exact failed persisted port travels with the
// request so the later owner probe cannot drift to an unrelated endpoint value.
func (s *Server) signalInitialHubBindFailure(port int) {
	if s == nil || s.hubRestartCh == nil {
		return
	}
	s.hubHealth.set(HubHealthRecovering, "")
	s.enqueueHubListenerRestart(hubListenerRestartRequest{
		cause:           hubListenerRestartCauseInitialBindFailed,
		initialBindPort: port,
	})
}

func (s *Server) enqueueHubListenerRestart(request hubListenerRestartRequest) {
	select {
	case s.hubRestartCh <- request:
	default:
	}
}

func runHubListenerRestartDriver(ctx context.Context, s *Server, opts hubListenerRestartDriverOptions) {
	if s == nil || s.hubRestartCh == nil {
		return
	}
	s.hubRestartDriverAlive.Store(true)
	defer func() {
		// Shutdown cancels the watcher and no later signal can legitimately fire.
		// Preserve the flag on that path so a racing teardown callback cannot
		// publish a false down; every permanent non-shutdown exit clears it.
		if ctx.Err() == nil {
			s.hubRestartDriverAlive.Store(false)
		}
	}()
	opts = normalizeHubListenerRestartDriverOptions(s, opts)
	for {
		select {
		case <-ctx.Done():
			return
		case request := <-s.hubRestartCh:
			requestOpts := opts
			requestOpts.cause = request.cause
			requestOpts.initialBindPort = request.initialBindPort
			if restartHubListenerWithOutcome(ctx, s, requestOpts) == hubListenerRestartStopDriver {
				return
			}
		}
	}
}

func normalizeHubListenerRestartDriverOptions(s *Server, opts hubListenerRestartDriverOptions) hubListenerRestartDriverOptions {
	if opts.startFn == nil {
		hubStartFn := startHubMcpListenerWithOptions
		if s.startHubMcpListenerFn != nil {
			hubStartFn = s.startHubMcpListenerFn
		}
		opts.startFn = func(ctx context.Context) (*HubListenerComponents, error) {
			return hubStartFn(ctx, true, s.api, startHubMcpListenerOptions{
				preservePortOnReloadHandlerFailure: true,
				server:                             s,
				onUnresponsive:                     s.signalHubListenerRestart,
				onRecovered:                        s.onHubListenerRecovered,
			})
		}
	}
	if opts.shutdownFn == nil {
		opts.shutdownFn = ShutdownHubListener
	}
	if opts.emitFn == nil {
		// Wrap the log emit so hub-health-relevant restart events also drive the
		// tracker state (recovering / needs-reconcile / healthy / down) that the
		// Dashboard + Groups render; the underlying hub-mcp.log audit is unchanged.
		opts.emitFn = s.hubHealthEmitWrapper(api.LogHubMcpEvent)
	}
	if opts.sleepFn == nil {
		opts.sleepFn = hubListenerRestartSleep
	}
	if opts.nowFn == nil {
		opts.nowFn = time.Now
	}
	if opts.initialBindPortNeedsRotationFn == nil {
		opts.initialBindPortNeedsRotationFn = hubInitialBindPortNeedsInstanceIDRotation
	}
	if opts.rotateHubInstanceIDFn == nil {
		opts.rotateHubInstanceIDFn = api.RotateHubInstanceIDContext
	}
	return opts
}

func hubInitialBindPortNeedsInstanceIDRotation(ctx context.Context, port int) bool {
	return hubInitialBindPortNeedsInstanceIDRotationWithDeps(ctx, port, hubInitialBindPortOwnerDependencies{
		portOwner:       api.LoopbackPortOwnerPIDContext,
		processIdentity: processID,
	})
}

func hubInitialBindPortNeedsInstanceIDRotationWithDeps(ctx context.Context, port int, deps hubInitialBindPortOwnerDependencies) bool {
	if port <= 0 {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, hubInitialBindPortOwnerProbeTimeout)
	defer cancel()

	// Accepted bounded multi-tenant residual (PR #562 P1-1/P1-2): this probe
	// runs after the bind failure, so a different-user holder can drop the port
	// before inspection, and basename + GUI argv identity is forgeable by a
	// different-user copied/renamed binary. Both are moot for the primary
	// single-user host because a same-user actor already owns the credential
	// files; MCPHUB_REQUIRE_SINGLE_USER_HOME plus the owner-only DACL bounds the
	// fable-accepted residual. Reliable rebind-time detection is impossible
	// (TOCTOU + forgeable identity), so keep the own-mcphub-GUI no-rotation gate.
	if deps.portOwner == nil {
		return true
	}
	ownerPID, bound, err := deps.portOwner(probeCtx, port)
	if err != nil {
		return true
	}
	if !bound {
		return false
	}
	if ownerPID <= 0 || probeCtx.Err() != nil || deps.processIdentity == nil {
		return true
	}

	identity, err := deps.processIdentity(ownerPID)
	defer identity.Close()
	if err != nil || !identity.Alive || identity.Denied {
		return true
	}
	return !matchBasename(identity.ImagePath) || !cmdlineIsGui(identity.Cmdline)
}

func restartHubListener(ctx context.Context, s *Server, opts hubListenerRestartDriverOptions) bool {
	return restartHubListenerWithOutcome(ctx, s, opts) == hubListenerRestartSucceeded
}

func restartHubListenerWithOutcome(ctx context.Context, s *Server, opts hubListenerRestartDriverOptions) hubListenerRestartOutcome {
	if ctx.Err() != nil || s.cfg.SupervisedAggregate {
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
				if opts.cause != hubListenerRestartCauseInitialBindFailed && opts.cause != hubListenerRestartCauseInitialStartupFailed {
					return hubListenerRestartStopDriver
				}
				if opts.cause == hubListenerRestartCauseInitialBindFailed {
					restartPort = opts.initialBindPort
				}
				// Seed the telemetry port from the persisted endpoint so the
				// initial-bind restart events (-failed/-exhausted/-abandoned)
				// carry the real port instead of 0 — there is no old component
				// to read it from, but this is the exact outage class that
				// correlates with gated client URLs, so the port anchor matters.
				if restartPort == 0 {
					if ep, epErr := api.LoadHubEndpoint(); epErr == nil {
						restartPort = ep.Port
					}
				}
				if !hubListenerRestartCanAttempt(s, opts.nowFn()) {
					hubListenerRestartEmitAbandoned(opts, s, restartPort)
					return hubListenerRestartStopDriver
				}
				oldTaken = true
				previousInstanceID, previousInstanceIDErr = loadHubListenerRestartInstanceID()
				if opts.cause == hubListenerRestartCauseInitialBindFailed && restartPort > 0 && opts.initialBindPortNeedsRotationFn(ctx, restartPort) {
					if ctx.Err() != nil {
						return hubListenerRestartStopDriver
					}
					if _, rotateErr := opts.rotateHubInstanceIDFn(ctx); rotateErr != nil {
						if ctx.Err() != nil {
							return hubListenerRestartStopDriver
						}
						_ = opts.emitFn("error", "hub-listener-restart-exhausted", map[string]any{
							"attempts":                    s.hubRestartConsecutive,
							"max_consecutive_restarts":    hubListenerRestartMaxConsecutiveRestarts,
							"port":                        restartPort,
							"instance_id_rotation_failed": true,
							"err":                         rotateErr.Error(),
						})
						return hubListenerRestartOutageExhausted
					}
					s.hubHealth.markReconcilePending()
				}
			} else {
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
		if !s.publishHubMcpComponent(newComp, opts.shutdownFn) {
			return hubListenerRestartStopDriver
		}
		if ctx.Err() != nil {
			if s.hubMcpComp.CompareAndSwap(newComp, nil) {
				opts.shutdownFn(context.Background(), newComp)
			}
			return hubListenerRestartStopDriver
		}
		if !newComp.Alive() {
			if s.hubMcpComp.CompareAndSwap(newComp, nil) {
				opts.shutdownFn(context.Background(), newComp)
			}
			_ = opts.emitFn("warn", "hub-listener-restart-failed", map[string]any{
				"port":    newComp.port,
				"attempt": attempt,
				"err":     "new hub listener component is dead on arrival",
			})
			if s.hubRestartConsecutive < hubListenerRestartMaxConsecutiveRestarts {
				retryDelay = hubListenerRestartBackoff(attempt)
			}
			continue
		}
		restartPort = newComp.port
		// Hydrate the durable reconcile marker from the accepted component the
		// SAME way the initial-bind accept path does. BindHubMcpListener loaded
		// newComp.reconcilePending under hub-mcp.lock, so a sibling
		// `regenerate-instance-id` that set the marker while this GUI was live
		// (no in-process latch) is caught here — even when this restart's
		// instance-id reads both land AFTER the rotation (instanceIDPreserved,
		// so the instance-id-changed emit never fires). Idempotent with that
		// emit's markReconcilePending; markHealthy below re-reads the latch.
		if newComp.reconcilePending {
			s.hubHealth.markReconcilePending()
		}
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
// through the settings registry (the single owner of this setting —
// internal/api/settings_registry.go's SettingsRegistry — via
// (*api.API).SettingsGet, which resolves SettingsPath(), validates the
// persisted value against the registry's TypeBool definition, and falls
// back to the registry default ("false") on a missing file, an absent
// key, or an out-of-domain persisted value). a is the caller's shared
// *api.API handle (Server.Start passes s.api); a nil a falls back to a
// throwaway instance since SettingsGet has no dependency on API's
// in-memory state — it is a pure passthrough to SettingsPath()-rooted
// disk reads.
//
// Returns false when the file is absent, the key is absent, the
// persisted value is not exactly "true" (SettingsGet falls back to the
// registry default on any other TypeBool value, including "True"/"1"/
// "yes"/""), or the YAML is malformed (SettingsGet's read errors are
// treated as gate-off here). This is the fail-closed posture spec
// §"Settings + CLI surface" demands — a corrupt settings file MUST NOT
// silently flip the gate on.
func readHubEndpointGateFromSettings(a *api.API) bool {
	if a == nil {
		a = api.NewAPI()
	}
	v, err := a.SettingsGet("gui_server.hub_endpoint_enabled")
	if err != nil {
		return false
	}
	return v == "true"
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
	server                             *Server
	onUnresponsive                     func()
	onRecovered                        func()
	healthProbeInterval                time.Duration
}

func startHubMcpListenerWithOptions(ctx context.Context, enabled bool, a *api.API, opts startHubMcpListenerOptions) (*HubListenerComponents, error) {
	if !enabled || (opts.server != nil && opts.server.cfg.SupervisedAggregate) {
		return nil, nil
	}
	comp := &HubListenerComponents{}
	listener, err := mcproute.StartHubListener(ctx, a, mcproute.HubListenerOptions{
		PreservePortOnReloadHandlerFailure: opts.preservePortOnReloadHandlerFailure,
		ReloadHandler:                      opts.reloadHandlerFn,
		HealthProbeInterval:                opts.healthProbeInterval,
		OnUnresponsive:                     hubListenerComponentCallback(opts.server, comp, opts.onUnresponsive),
		OnRecovered:                        hubListenerComponentCallback(opts.server, comp, opts.onRecovered),
		OnServeFailure:                     func(error) { hubListenerServeFailed(opts.server, comp) },
	})
	if err != nil {
		return nil, err
	}
	ep := listener.Endpoint()
	comp.listener, comp.port, comp.reconcilePending = listener, ep.Port, ep.ReconcilePending

	// Keep the process-local reconcile latch aligned with a successful CLI
	// reconcile. This watcher is detect-only: read failures are ignored for the
	// current tick, and listener cancellation is its sole lifetime boundary.
	if server := opts.server; server != nil {
		listenerCtx, cancel := context.WithCancel(ctx)
		comp.listenerCancel = cancel
		interval := opts.healthProbeInterval
		if interval <= 0 {
			interval = api.DefaultHubHealthProbeInterval
		}
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-listenerCtx.Done():
					return
				case <-ticker.C:
					ep, err := api.LoadHubEndpoint()
					if err != nil {
						continue
					}
					// Bidirectional re-sync with the durable marker: re-latch when
					// a sibling rotation set it while this GUI was live (fable F1
					// backstop) AND self-heal a marker a stale-read tick wrongly
					// cleared (fable F2 TOCTOU). markReconcilePending is idempotent;
					// clearReconcilePendingIfResolved only downgrades NeedsReconcile.
					if ep.ReconcilePending {
						server.hubHealth.markReconcilePending()
					} else {
						server.hubHealth.clearReconcilePendingIfResolved(false)
					}
				}
			}
		}()
	}

	return comp, nil
}

func hubListenerServeFailed(s *Server, comp *HubListenerComponents) {
	comp.alive.Store(false)
	if s != nil {
		registered := s.hubMcpComp.Load()
		if registered == nil || registered == comp {
			if s.hubRestartDriverAlive.Load() {
				s.hubHealth.set(HubHealthRecovering, "")
			} else {
				s.hubHealth.set(HubHealthDown, "")
			}
		}
	}
}

func hubListenerComponentCallback(s *Server, comp *HubListenerComponents, callback func()) func() {
	if callback == nil || s == nil {
		return callback
	}
	return func() {
		if s.hubMcpComp.Load() == comp {
			callback()
		}
	}
}

// ShutdownHubListener settles the GUI health watcher and the common transport.
func ShutdownHubListener(parentCtx context.Context, c *HubListenerComponents) {
	if c == nil {
		return
	}
	if c.listenerCancel != nil {
		c.listenerCancel()
	}
	c.listener.Shutdown(parentCtx)
}

// Group and manifest changes share the resolver publisher used at listener startup.
func publishResolverSnapshotForHubBind(ctx context.Context, a *api.API) error {
	return mcproute.PublishHubResolverSnapshot(ctx, a)
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

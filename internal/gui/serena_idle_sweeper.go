// internal/gui/serena_idle_sweeper.go
//
// v0.6 idle-shutdown (#6, spec §6) — the 60s in-GUI idle sweeper + the
// per-daemon LAST-ACTIVITY tracking it reads.
//
// The sweeper lives in the GUI process (not the supervisor) because the GUI
// process is the only place /serena/mcp activity is observable — the
// supervisor is a SEPARATE process that never sees a /serena/mcp request. Each
// tick it:
//
//  1. reads the operator-configured idle threshold (daemons.serena_idle_shutdown,
//     GUI-settable; "off" → disabled, the sweep is a no-op);
//  2. lists the registered serena pool daemons (resolver.ListWorkspaces, the
//     Backend=="serena" sentinel rows) and the supervisor IPC status to learn
//     which are currently RUNNING;
//  3. for each running serena daemon, reads its LAST-ACTIVITY (recordSerenaActivity
//     stamps it on every forwarded /serena/mcp tool-call) — NOT wall-clock since
//     spawn — and, if idle longer than the threshold, writes Desired=stopped +
//     IntentReasonIdle onto the UNIFIED supervisor-intent stops sub-block
//     (api.WriteSerenaIdleStop → WriteStopIntent), the §4/Phase-E corrected stop
//     path. The supervisor's existing IntentWatcher + reconcile then terminates
//     the daemon. No second stop path.
//
// CRITICAL mid-call/teardown invariant (the falsification test, spec §6) — TWO
// layers, because last-activity alone is insufficient for a SINGLE long-lived
// call:
//
//  1. Last-activity is stamped ONCE at request START (serena_router.go), so a
//     recently-FINISHED call has a fresh timestamp and is never idled. But a
//     single tool call that STREAMS (SSE) past the threshold would leave a stale
//     start-of-call timestamp and could be idled MID-CALL on its own. So:
//  2. The router tracks IN-FLIGHT requests per workspace (enterSerenaForward /
//     exitSerenaForward from workspace resolution through the whole upstream
//     forward incl. the SSE stream), and the sweep SKIPS any daemon with a
//     started request (hasSerenaForwardInFlight). The forward's completion ALSO
//     re-stamps last-activity, so a just-finished long call resets the idle
//     clock. The same gate also claims prune teardown before registry/intent
//     mutation. Together these GUARANTEE a daemon being woken, handshaken,
//     forwarded to, or pruned is never concurrently idle-killed/pruned/entered,
//     even with the threshold at its 15m minimum.
//
// A daemon with NO recorded activity yet (just spawned, never called) uses its
// supervisor uptime as the idle baseline, CAPPED at time-since-GUI-process-start
// (serenaIdleDuration) so a GUI restart — which wipes the in-memory
// last-activity map — cannot immediately idle-kill a daemon that was active just
// before the restart; every daemon gets a full fresh threshold window after a
// GUI restart (FIX-1).
package gui

import (
	"context"
	"strings"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
)

// serenaIdleStopFn is the seam over api.(*API).WriteSerenaIdleStopResult the
// sweeper uses to record the idle stop. Production wires it (SetSerenaIdleShutdownFns)
// to a live api.API; tests inject a fake to assert the exact (taskName) it is
// called with WITHOUT touching the real state dir. A nil seam disables the
// stop-write half of the sweep (the GUI is unwired).
var serenaIdleStopFn func(taskName string, now time.Time) (bool, error)

// serenaIdleThresholdFn is the seam over the GUI-settable threshold read.
// Production reads daemons.serena_idle_shutdown via api.(*API).SettingsGet and
// parses it with api.SerenaIdleShutdownThreshold; tests inject a fixed
// (threshold, enabled). A nil seam disables the sweep (treated as "off").
var serenaIdleThresholdFn func() (time.Duration, bool)

// SetSerenaIdleShutdownFns wires the production idle-shutdown seams the GUI
// idle sweeper uses. CLI boot (internal/cli/gui.go) calls it with a live
// api.API-backed threshold reader + stop writer. Passing nil for either
// disables that half (the sweep is then a no-op for the missing half).
func SetSerenaIdleShutdownFns(threshold func() (time.Duration, bool), stop func(taskName string, now time.Time) (bool, error)) {
	serenaIdleThresholdFn = threshold
	serenaIdleStopFn = stop
}

// recordSerenaActivity stamps wsKey's in-memory last-activity to now. Called
// from the serena router handler on /serena/mcp tool-calls (the resolved
// workspace the call routes to), so the sweeper sees a fresh timestamp for any
// daemon that is being used or attempted. Idempotent + cheap (one map write
// under a dedicated mutex). An empty wsKey is ignored.
func (s *Server) recordSerenaActivity(wsKey string, now time.Time) {
	if s == nil || wsKey == "" {
		return
	}
	s.serenaActivityMu.Lock()
	if s.serenaLastActivity == nil {
		s.serenaLastActivity = make(map[string]time.Time)
	}
	s.serenaLastActivity[wsKey] = now
	s.serenaActivityMu.Unlock()
}

// serenaActivityPersistDebounce bounds how often maybePersistSerenaActivity writes the
// @serena row's LastToolsCallAt to the registry — at most one write per window
// per workspace, mirroring the LSP lazy proxy's debounce.
const serenaActivityPersistDebounce = 5 * time.Second

// maybePersistSerenaActivity debounce-writes wsKey's @serena registry row
// LastToolsCallAt after a request reaches the daemon. A silent no-op if the row
// was already unregistered (the registry write fails closed on a missing row).
func (s *Server) maybePersistSerenaActivity(wsKey string, now time.Time) {
	s.serenaPersistMu.Lock()
	if s.serenaLastPersist == nil {
		s.serenaLastPersist = make(map[string]time.Time)
	}
	due := now.Sub(s.serenaLastPersist[wsKey]) >= serenaActivityPersistDebounce
	if due {
		s.serenaLastPersist[wsKey] = now
	}
	s.serenaPersistMu.Unlock()
	if !due {
		return
	}
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		return
	}
	_ = api.NewRegistry(regPath).PutLastToolsCallAt(wsKey, api.SerenaLanguageSentinel, now.UTC())
}

// lastSerenaActivity returns wsKey's recorded last-activity time and whether
// any activity has been recorded for it. Used by the sweeper.
func (s *Server) lastSerenaActivity(wsKey string) (time.Time, bool) {
	if s == nil {
		return time.Time{}, false
	}
	s.serenaActivityMu.Lock()
	defer s.serenaActivityMu.Unlock()
	t, ok := s.serenaLastActivity[wsKey]
	return t, ok
}

// dropSerenaActivity removes wsKey's activity entry. Called after the sweeper
// idle-stops a daemon so the map does not retain a stale baseline for a now-
// stopped daemon (the next wake re-establishes activity on the next call).
func (s *Server) dropSerenaActivity(wsKey string) {
	if s == nil || wsKey == "" {
		return
	}
	s.serenaActivityMu.Lock()
	defer s.serenaActivityMu.Unlock()
	delete(s.serenaLastActivity, wsKey)
}

type serenaWorkspaceStopGate struct {
	mu          sync.Mutex
	byWorkspace map[string]*serenaWorkspaceStopGateEntry
}

type serenaWorkspaceStopGatePhase uint8

const (
	serenaWorkspaceStopGatePhaseNone serenaWorkspaceStopGatePhase = iota
	serenaWorkspaceStopGatePhaseIdleStop
	serenaWorkspaceStopGatePhasePrune
)

type serenaWorkspaceStopGateEntry struct {
	inFlight int
	phase    serenaWorkspaceStopGatePhase
	waiters  int
	ready    *sync.Cond
}

type serenaWorkspaceStopGateEnterResult struct {
	entered            bool
	waitedThroughPrune bool
	phaseActive        bool
}

type serenaWorkspaceGatePolicy uint8

const (
	serenaWorkspaceGatePolicyBlock serenaWorkspaceGatePolicy = iota
	serenaWorkspaceGatePolicyTryOnly
)

type serenaWorkspaceGateOutcome struct {
	ws          *api.WorkspaceEntry
	upstreamURL string
	rewoke      bool
	gate        serenaWorkspaceStopGateEnterResult
}

func (g *serenaWorkspaceStopGate) enter(wsKey string) serenaWorkspaceStopGateEnterResult {
	return g.enterCtx(context.Background(), wsKey)
}

func (g *serenaWorkspaceStopGate) enterCtx(ctx context.Context, wsKey string) serenaWorkspaceStopGateEnterResult {
	if g == nil || wsKey == "" {
		return serenaWorkspaceStopGateEnterResult{entered: true}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return serenaWorkspaceStopGateEnterResult{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	entry := g.entryLocked(wsKey)
	var stopWake func() bool
	waitedThroughPrune := false
	phaseActive := false
	for entry.phase != serenaWorkspaceStopGatePhaseNone {
		phaseActive = true
		if entry.phase == serenaWorkspaceStopGatePhasePrune {
			waitedThroughPrune = true
		}
		if err := ctx.Err(); err != nil {
			if stopWake != nil {
				stopWake()
			}
			g.pruneIdleEntryLocked(wsKey, entry)
			return serenaWorkspaceStopGateEnterResult{waitedThroughPrune: waitedThroughPrune, phaseActive: phaseActive}
		}
		if stopWake == nil {
			stopWake = context.AfterFunc(ctx, func() {
				g.mu.Lock()
				entry.ready.Broadcast()
				g.mu.Unlock()
			})
			defer stopWake()
		}
		entry.waiters++
		entry.ready.Wait()
		entry.waiters--
	}
	if err := ctx.Err(); err != nil {
		g.pruneIdleEntryLocked(wsKey, entry)
		return serenaWorkspaceStopGateEnterResult{waitedThroughPrune: waitedThroughPrune, phaseActive: phaseActive}
	}
	entry.inFlight++
	return serenaWorkspaceStopGateEnterResult{entered: true, waitedThroughPrune: waitedThroughPrune, phaseActive: phaseActive}
}

func (g *serenaWorkspaceStopGate) tryEnter(wsKey string) serenaWorkspaceStopGateEnterResult {
	if g == nil || wsKey == "" {
		return serenaWorkspaceStopGateEnterResult{entered: true}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	entry := g.entryLocked(wsKey)
	if entry.phase != serenaWorkspaceStopGatePhaseNone {
		return serenaWorkspaceStopGateEnterResult{
			phaseActive:        true,
			waitedThroughPrune: entry.phase == serenaWorkspaceStopGatePhasePrune,
		}
	}
	entry.inFlight++
	return serenaWorkspaceStopGateEnterResult{entered: true}
}

func (g *serenaWorkspaceStopGate) exit(wsKey string) {
	if g == nil || wsKey == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	entry := g.byWorkspace[wsKey]
	if entry == nil {
		return
	}
	if entry.inFlight > 0 {
		entry.inFlight--
	}
	g.pruneIdleEntryLocked(wsKey, entry)
}

func (g *serenaWorkspaceStopGate) hasInFlight(wsKey string) bool {
	if g == nil || wsKey == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	entry := g.byWorkspace[wsKey]
	return entry != nil && entry.inFlight > 0
}

func (g *serenaWorkspaceStopGate) beginIdleStop(wsKey string) bool {
	return g.beginPhase(wsKey, serenaWorkspaceStopGatePhaseIdleStop)
}

func (g *serenaWorkspaceStopGate) endIdleStop(wsKey string) {
	g.endPhase(wsKey, serenaWorkspaceStopGatePhaseIdleStop)
}

func (g *serenaWorkspaceStopGate) beginPrune(wsKey string) bool {
	return g.beginPhase(wsKey, serenaWorkspaceStopGatePhasePrune)
}

func (g *serenaWorkspaceStopGate) endPrune(wsKey string) {
	g.endPhase(wsKey, serenaWorkspaceStopGatePhasePrune)
}

func (g *serenaWorkspaceStopGate) beginPhase(wsKey string, phase serenaWorkspaceStopGatePhase) bool {
	if g == nil || wsKey == "" || phase == serenaWorkspaceStopGatePhaseNone {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	entry := g.entryLocked(wsKey)
	if entry.inFlight > 0 || entry.waiters > 0 || entry.phase != serenaWorkspaceStopGatePhaseNone {
		return false
	}
	entry.phase = phase
	return true
}

func (g *serenaWorkspaceStopGate) endPhase(wsKey string, phase serenaWorkspaceStopGatePhase) {
	if g == nil || wsKey == "" || phase == serenaWorkspaceStopGatePhaseNone {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	entry := g.entryLocked(wsKey)
	if entry.phase == phase {
		entry.phase = serenaWorkspaceStopGatePhaseNone
		entry.ready.Broadcast()
		g.pruneIdleEntryLocked(wsKey, entry)
	}
}

func (g *serenaWorkspaceStopGate) entryLocked(wsKey string) *serenaWorkspaceStopGateEntry {
	if g.byWorkspace == nil {
		g.byWorkspace = make(map[string]*serenaWorkspaceStopGateEntry)
	}
	entry := g.byWorkspace[wsKey]
	if entry == nil {
		entry = &serenaWorkspaceStopGateEntry{}
		entry.ready = sync.NewCond(&g.mu)
		g.byWorkspace[wsKey] = entry
	}
	return entry
}

func (g *serenaWorkspaceStopGate) pruneIdleEntryLocked(wsKey string, entry *serenaWorkspaceStopGateEntry) {
	if g == nil || wsKey == "" || entry == nil {
		return
	}
	if entry.inFlight == 0 && entry.phase == serenaWorkspaceStopGatePhaseNone && entry.waiters == 0 {
		delete(g.byWorkspace, wsKey)
	}
}

// enterSerenaForward marks the start of a daemon-bound Serena request for wsKey,
// including tool-call forwards, tools/list candidate probes, cancel forwards,
// and client DELETE forwards (increments the in-flight counter). The matching
// exitSerenaForward MUST be deferred so the count is decremented on EVERY return
// path including a panic. The sweeper uses the same per-workspace gate to make
// "no in-flight request" atomic with starting idle-stop/prune teardown; new
// requests wait for that phase to finish before wake/handshake/forward. An empty
// wsKey is ignored.
func (s *Server) enterSerenaForward(wsKey string) serenaWorkspaceStopGateEnterResult {
	if s == nil || wsKey == "" {
		return serenaWorkspaceStopGateEnterResult{entered: true}
	}
	return s.serenaStopGate.enter(wsKey)
}

// enterSerenaForwardCtx is the bounded form of enterSerenaForward. The result
// reports whether the request entered before ctx expired and whether it waited
// across a prune phase, which means callers must re-read workspace state.
func (s *Server) enterSerenaForwardCtx(ctx context.Context, wsKey string) serenaWorkspaceStopGateEnterResult {
	if s == nil || wsKey == "" {
		return serenaWorkspaceStopGateEnterResult{entered: true}
	}
	return s.serenaStopGate.enterCtx(ctx, wsKey)
}

// tryEnterSerenaForward is the non-blocking form used by tools/list candidate
// selection. A candidate already in idle-stop/prune is reported through
// phaseActive and is not marked in-flight.
func (s *Server) tryEnterSerenaForward(wsKey string) serenaWorkspaceStopGateEnterResult {
	if s == nil || wsKey == "" {
		return serenaWorkspaceStopGateEnterResult{entered: true}
	}
	return s.serenaStopGate.tryEnter(wsKey)
}

// exitSerenaForward marks the end of a /serena/mcp request to wsKey's daemon
// (decrements the in-flight counter). An empty wsKey is ignored; a decrement of
// an absent/zero key is a defensive no-op (the count never goes negative).
func (s *Server) exitSerenaForward(wsKey string) {
	if s == nil || wsKey == "" {
		return
	}
	s.serenaStopGate.exit(wsKey)
}

func (s *Server) withSerenaWorkspaceGate(
	ctx context.Context,
	wsKey string,
	policy serenaWorkspaceGatePolicy,
	resolve func(string) *api.WorkspaceEntry,
	urlFn func(*api.WorkspaceEntry) string,
	onPhaseActive func(*serenaWorkspaceGateOutcome) bool,
	fn func(*serenaWorkspaceGateOutcome) error,
) (entered bool, aborted bool, err error) {
	ctxProvided := ctx != nil
	if ctx == nil {
		ctx = context.Background()
	}
	currentKey := wsKey
	for {
		var gate serenaWorkspaceStopGateEnterResult
		switch policy {
		case serenaWorkspaceGatePolicyTryOnly:
			gate = s.tryEnterSerenaForward(currentKey)
		default:
			if ctxProvided {
				gate = s.enterSerenaForwardCtx(ctx, currentKey)
			} else {
				gate = s.enterSerenaForward(currentKey)
			}
		}
		if !gate.entered {
			if policy != serenaWorkspaceGatePolicyTryOnly {
				return false, false, nil
			}
			out := &serenaWorkspaceGateOutcome{gate: gate}
			if resolve != nil {
				out.ws = resolve(currentKey)
			}
			if urlFn != nil {
				out.upstreamURL = urlFn(out.ws)
			}
			if gate.phaseActive && onPhaseActive != nil {
				aborted = onPhaseActive(out)
			}
			return false, aborted, nil
		}

		entered = true
		exitKey := currentKey
		retry, innerAborted, innerErr := func() (bool, bool, error) {
			defer s.exitSerenaForward(exitKey)
			out := &serenaWorkspaceGateOutcome{gate: gate}
			if resolve != nil {
				out.ws = resolve(currentKey)
				if out.ws == nil {
					return false, true, nil
				}
				if out.ws.WorkspaceKey != "" && out.ws.WorkspaceKey != currentKey {
					currentKey = out.ws.WorkspaceKey
					return true, false, nil
				}
			}
			if urlFn != nil {
				out.upstreamURL = urlFn(out.ws)
			}
			if gate.phaseActive && !gate.waitedThroughPrune && onPhaseActive != nil {
				if onPhaseActive(out) {
					return false, true, nil
				}
			}
			if fn != nil {
				return false, false, fn(out)
			}
			return false, false, nil
		}()
		if retry {
			continue
		}
		return true, innerAborted, innerErr
	}
}

// hasSerenaForwardInFlight reports whether at least one /serena/mcp forward is
// currently open or preparing to open to wsKey's daemon. The sweeper consults
// it to skip a daemon with an in-flight call or pre-forward request.
func (s *Server) hasSerenaForwardInFlight(wsKey string) bool {
	if s == nil || wsKey == "" {
		return false
	}
	return s.serenaStopGate.hasInFlight(wsKey)
}

func (s *Server) beginSerenaIdleStop(wsKey string) bool {
	if s == nil || wsKey == "" {
		return false
	}
	return s.serenaStopGate.beginIdleStop(wsKey)
}

func (s *Server) endSerenaIdleStop(wsKey string) {
	if s == nil || wsKey == "" {
		return
	}
	s.serenaStopGate.endIdleStop(wsKey)
}

func (s *Server) beginSerenaPrune(wsKey string) bool {
	if s == nil || wsKey == "" {
		return false
	}
	return s.serenaStopGate.beginPrune(wsKey)
}

func (s *Server) endSerenaPrune(wsKey string) {
	if s == nil || wsKey == "" {
		return
	}
	s.serenaStopGate.endPrune(wsKey)
}

func serenaTaskHasActiveIdleStop(taskName string, now time.Time) bool {
	if taskName == "" {
		return false
	}
	stop, err := api.ReadSerenaUnifiedStopForTask(taskName)
	if err != nil {
		return false
	}
	active, reason := stop.IsActiveStop(now.UTC())
	return active && reason == api.IntentReasonIdle
}

// SweepIdleSerenaDaemons is the per-tick idle-shutdown sweep (spec §6 #6). It
// stops every RUNNING serena pool daemon that has been idle longer than the
// operator-configured threshold by writing Desired=stopped+IntentReasonIdle on
// the unified supervisor-intent stops sub-block. Returns the number of daemons
// it idle-stopped this tick.
//
// `now` is the evaluation clock (injected for tests; production passes
// time.Now()). It is a no-op when: idle-shutdown is "off" / the seams are
// unwired / there is no resolver / the supervisor IPC status is unreachable
// (do NOT stop daemons on a status-read failure — that would be a false
// positive; the next successful tick retries).
func (s *Server) SweepIdleSerenaDaemons(ctx context.Context, now time.Time) int {
	if s == nil {
		return 0
	}
	thresholdFn := serenaIdleThresholdFn
	stopFn := serenaIdleStopFn
	if thresholdFn == nil || stopFn == nil {
		return 0
	}
	threshold, enabled := thresholdFn()
	if !enabled || threshold <= 0 {
		return 0 // "off" — idle-shutdown disabled.
	}

	deps := s.serenaRouterDepsProd()
	if deps == nil || deps.Resolver == nil {
		return 0
	}
	lister, ok := deps.Resolver.(workspaceLister)
	if !ok {
		return 0
	}

	// Collect the serena pool daemons: WorkspacePath -> (WorkspaceKey, TaskName).
	// The supervisor IPC status keys workspace by PATH (Workspace = WorkspacePath).
	type serenaRow struct {
		key      string
		taskName string
	}
	byPath := map[string]serenaRow{}
	for _, ws := range lister.ListWorkspaces() {
		if ws == nil || ws.TaskName == "" {
			continue
		}
		if !isSerenaWorkspaceEntry(ws) {
			continue
		}
		byPath[ws.WorkspacePath] = serenaRow{key: ws.WorkspaceKey, taskName: ws.TaskName}
	}
	if len(byPath) == 0 {
		return 0
	}

	statusFn := serenaBackendStatusFn
	if statusFn == nil {
		return 0
	}
	rows, err := statusFn(ctx)
	if err != nil {
		// IPC unavailable / transient: do NOT idle-stop on a status-read
		// failure (false-positive risk — the daemons may be fine and only the
		// supervisor momentarily unreachable). Skip this tick.
		return 0
	}

	stopped := 0
	for _, row := range rows {
		sr, want := byPath[row.Workspace]
		if !want {
			continue // not a serena pool daemon the router knows.
		}
		if !isRunningDaemonState(row.State) {
			continue // already stopped / failed / restarting — nothing to idle.
		}

		// The per-workspace stop/forward gate makes the "no started request"
		// observation atomic with beginning an idle-stop phase. A request that
		// already resolved this workspace increments the counter before
		// wake/handshake, so the sweep skips it; a request that arrives after this
		// point waits until the stop write and stale-session invalidation finish,
		// then observes/wakes the just-stopped daemon. The gate mutex is held only
		// for that small state transition, never across the stop writer or other
		// lock-taking cleanup calls.
		type idleStopResult struct {
			gated   bool
			idleFor time.Duration
			wrote   bool
			err     error
		}
		result := func() idleStopResult {
			if !s.beginSerenaIdleStop(sr.key) {
				return idleStopResult{}
			}
			defer s.endSerenaIdleStop(sr.key)

			// Compute idle duration from LAST-ACTIVITY, not wall-clock since spawn.
			// If activity was recorded, that is the baseline; otherwise fall back to
			// the daemon's supervisor uptime so a freshly-spawned-but-never-called
			// daemon is not idled until it has been up at least `threshold`.
			result := idleStopResult{
				gated:   true,
				idleFor: serenaIdleDuration(s, sr.key, row.UptimeSec, now),
			}
			if result.idleFor < threshold {
				return result
			}
			result.wrote, result.err = stopFn(sr.taskName, now)
			if result.err != nil || !result.wrote {
				return result
			}
			// Drop the activity baseline so a stale timestamp does not linger for a
			// now-stopped daemon. The wake re-establishes it on the next call.
			s.dropSerenaActivity(sr.key)
			// The successful stop write is the durable fact: invalidate the local
			// daemon-session binding unconditionally while later requests are still
			// held at the workspace gate. Re-reading the idle marker here races a
			// concurrent wake clearing that marker.
			s.serenaDaemonSessions.unbindWorkspace(sr.key)
			return result
		}()
		if !result.gated {
			continue
		}
		idleFor := result.idleFor
		if idleFor < threshold {
			continue
		}
		if result.err != nil {
			// Non-fatal: a transient stop-write failure is retried next tick.
			// Emit a best-effort audit so the failure is diagnosable.
			_ = api.LogHubMcpEvent("warn", "serena-idle-stop-failed", map[string]any{
				"task_name":     sr.taskName,
				"workspace_key": sr.key,
				"idle_secs":     int(idleFor / time.Second),
				"err":           result.err.Error(),
			})
			continue
		}
		if !result.wrote {
			_ = api.LogHubMcpEvent("info", "serena-idle-skipped-operator-stop-active", map[string]any{
				"task_name":      sr.taskName,
				"workspace_key":  sr.key,
				"idle_secs":      int(idleFor / time.Second),
				"threshold_secs": int(threshold / time.Second),
			})
			continue
		}
		stopped++
		_ = api.LogHubMcpEvent("info", "serena-idle-stopped", map[string]any{
			"task_name":      sr.taskName,
			"workspace_key":  sr.key,
			"idle_secs":      int(idleFor / time.Second),
			"threshold_secs": int(threshold / time.Second),
		})
	}
	return stopped
}

// serenaIdleDuration computes how long a serena daemon has been idle. It uses
// the recorded LAST-ACTIVITY when present; otherwise it falls back to the
// supervisor-reported uptime, CAPPED at time-since-GUI-process-start (see
// below). Both paths read LAST-ACTIVITY/uptime, NOT wall-clock-since-spawn-
// baseline, so a daemon mid-call or recently active is never classified idle.
//
// GUI-restart baseline (FIX-1, fable's coupled-hazard insight): the never-called
// fallback caps the uptime-derived idle at time-since-GUI-process-start. A GUI
// restart wipes serenaLastActivity (it is in-memory), so a daemon that was
// active seconds before the restart would, with real supervisor uptime now
// populated (FIX-1 IPC decode), read as idle == uptime (e.g. 3h) and be killed
// on the very first post-restart sweep. Capping at time-since-GUI-start gives
// every daemon a full fresh threshold window after a GUI restart: until the GUI
// has itself been up at least `threshold`, no never-called daemon can be idled.
// The cap is skipped only when guiProcessStart is the zero value (test servers
// that construct a bare &Server{} without NewServer), where it would otherwise
// be a ~2000-year span that never binds anyway.
func serenaIdleDuration(s *Server, wsKey string, uptimeSec int64, now time.Time) time.Duration {
	if last, ok := s.lastSerenaActivity(wsKey); ok {
		d := now.Sub(last)
		if d < 0 {
			d = 0 // clock skew guard — never report negative idle.
		}
		return d
	}
	// No recorded activity: use supervisor uptime as the idle baseline. A
	// negative/zero uptime (unknown) is treated as just-spawned (idle 0).
	if uptimeSec <= 0 {
		return 0
	}
	idle := time.Duration(uptimeSec) * time.Second
	// Cap at time-since-GUI-start so a GUI restart cannot immediately idle a
	// daemon (see docstring). Only when guiProcessStart is set (production).
	if !s.guiProcessStart.IsZero() {
		if sinceGUIStart := now.Sub(s.guiProcessStart); sinceGUIStart >= 0 && sinceGUIStart < idle {
			idle = sinceGUIStart
		}
	}
	return idle
}

// isSerenaWorkspaceEntry reports whether a registry entry is a serena pool
// daemon row (the sentinel-language serena backend). Mirrors the serena
// classification in internal/gui/daemons.go (Language == SerenaLanguageSentinel)
// and the registry's "serena" backend.
func isSerenaWorkspaceEntry(ws *api.WorkspaceEntry) bool {
	if ws == nil {
		return false
	}
	return ws.Backend == "serena" || ws.Language == api.SerenaLanguageSentinel
}

// isRunningDaemonState reports whether a supervisor IPC status State string
// indicates a live daemon worth idle-stopping. "Running" and "Ready" are live;
// "Stopped" / "Failed" / "Restarting" / "Quarantined" / "Idle" are not (nothing
// to idle, or already in flux). Case-insensitive to be robust against surface
// wording drift.
//
// NOTE (FIX-6c): the CURRENT wired status source never emits "Ready" — the IPC
// normalizer (normalizeSupervisorIPCStatusState) and the supervisor's
// supervisorStatusGUIState both only produce Running/Stopped/Restarting/
// Quarantined/Idle. "Ready" is retained DEFENSIVELY because DaemonStatus.State's
// own doc-comment (internal/api/types.go) advertises "Ready" as a legal value;
// keeping it means a future source that does surface "Ready" is treated as live
// (the safe direction — a live daemon classified as not-running would simply not
// be idled, never wrongly killed). Removing it would be a silent behavior bet on
// the current emitters never changing.
func isRunningDaemonState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "running", "ready":
		return true
	default:
		return false
	}
}

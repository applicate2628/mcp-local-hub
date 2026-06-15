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
// CRITICAL mid-call invariant (the falsification test, spec §6) — TWO layers,
// because last-activity alone is insufficient for a SINGLE long-lived call:
//
//  1. Last-activity is stamped ONCE at request START (serena_router.go), so a
//     recently-FINISHED call has a fresh timestamp and is never idled. But a
//     single tool call that STREAMS (SSE) past the threshold would leave a stale
//     start-of-call timestamp and could be idled MID-CALL on its own. So:
//  2. The router tracks IN-FLIGHT forwards per workspace (enterSerenaForward /
//     exitSerenaForward around the WHOLE forward incl. the SSE stream), and the
//     sweep SKIPS any daemon with an open forward (hasSerenaForwardInFlight).
//     The forward's completion ALSO re-stamps last-activity, so a just-finished
//     long call resets the idle clock. Together these GUARANTEE a daemon mid-call
//     is never idle-killed, even with the threshold at its 15m minimum (FIX-3).
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

// enterSerenaForward marks the start of a /serena/mcp forward to wsKey's daemon
// (increments the in-flight counter). The matching exitSerenaForward MUST be
// deferred so the count is decremented on EVERY return path including a panic.
// The sweeper skips any daemon with a non-zero in-flight count so a long-lived
// streaming call past the idle threshold is never killed MID-CALL (FIX-3). An
// empty wsKey is ignored.
func (s *Server) enterSerenaForward(wsKey string) {
	if s == nil || wsKey == "" {
		return
	}
	s.serenaInFlightMu.Lock()
	defer s.serenaInFlightMu.Unlock()
	if s.serenaInFlight == nil {
		s.serenaInFlight = make(map[string]int)
	}
	s.serenaInFlight[wsKey]++
}

// exitSerenaForward marks the end of a /serena/mcp forward to wsKey's daemon
// (decrements the in-flight counter, pruning the entry to absent at zero). An
// empty wsKey is ignored; a decrement of an absent/zero key is a defensive
// no-op (the count never goes negative).
func (s *Server) exitSerenaForward(wsKey string) {
	if s == nil || wsKey == "" {
		return
	}
	s.serenaInFlightMu.Lock()
	defer s.serenaInFlightMu.Unlock()
	if s.serenaInFlight == nil {
		return
	}
	if n := s.serenaInFlight[wsKey]; n > 1 {
		s.serenaInFlight[wsKey] = n - 1
	} else {
		delete(s.serenaInFlight, wsKey)
	}
}

// hasSerenaForwardInFlight reports whether at least one /serena/mcp forward is
// currently open to wsKey's daemon. The sweeper consults it to skip a daemon
// with an in-flight call (FIX-3 mid-call protection).
func (s *Server) hasSerenaForwardInFlight(wsKey string) bool {
	if s == nil || wsKey == "" {
		return false
	}
	s.serenaInFlightMu.Lock()
	defer s.serenaInFlightMu.Unlock()
	return s.serenaInFlight[wsKey] > 0
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

		// FIX-3 mid-call protection: NEVER idle-stop a daemon with an open
		// /serena/mcp forward. A single tool call that streams (SSE) past the
		// threshold (min 15m) keeps an in-flight counter incremented around the
		// WHOLE forward (recordSerenaActivity is stamped once at request start,
		// so last-activity alone would go stale mid-stream). Skipping an in-flight
		// daemon guarantees the documented invariant: a daemon mid-call is never
		// idle-killed. The forward's completion re-stamps activity, so the daemon
		// also gets a fresh threshold window after the call finishes.
		if s.hasSerenaForwardInFlight(sr.key) {
			continue
		}

		// Compute idle duration from LAST-ACTIVITY, not wall-clock since spawn.
		// If activity was recorded, that is the baseline; otherwise fall back to
		// the daemon's supervisor uptime so a freshly-spawned-but-never-called
		// daemon is not idled until it has been up at least `threshold`.
		idleFor := serenaIdleDuration(s, sr.key, row.UptimeSec, now)
		if idleFor < threshold {
			continue // mid-call / recently active / freshly spawned — keep alive.
		}

		wrote, err := stopFn(sr.taskName, now)
		if err != nil {
			// Non-fatal: a transient stop-write failure is retried next tick.
			// Emit a best-effort audit so the failure is diagnosable.
			_ = api.LogHubMcpEvent("warn", "serena-idle-stop-failed", map[string]any{
				"task_name":     sr.taskName,
				"workspace_key": sr.key,
				"idle_secs":     int(idleFor / time.Second),
				"err":           err.Error(),
			})
			continue
		}
		if !wrote {
			_ = api.LogHubMcpEvent("info", "serena-idle-skipped-operator-stop-active", map[string]any{
				"task_name":      sr.taskName,
				"workspace_key":  sr.key,
				"idle_secs":      int(idleFor / time.Second),
				"threshold_secs": int(threshold / time.Second),
			})
			continue
		}
		// Drop the activity baseline so a stale timestamp does not linger for a
		// now-stopped daemon. The wake re-establishes it on the next call.
		s.dropSerenaActivity(sr.key)
		if serenaTaskHasActiveIdleStop(sr.taskName, now) {
			s.serenaDaemonSessions.unbindWorkspace(sr.key)
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

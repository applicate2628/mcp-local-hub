// Package api — v0.6 idle-shutdown (#6, spec §6) wake + threshold helpers.
//
// serena_idle_shutdown.go owns the API-side surface of feature #6:
//
//   - SerenaIdleShutdownThreshold: parse the GUI-settable
//     daemons.serena_idle_shutdown enum into a time.Duration (0 = disabled).
//   - SerenaIdleStopCandidate / WriteSerenaIdleStop: the 60s sweeper's
//     write step — mark a serena pool daemon stopped with IntentReasonIdle on
//     the UNIFIED supervisor-intent stops sub-block (WriteStopIntent), the
//     §4/Phase-E corrected stop-propagation path. No second stop path.
//   - WakeIdleSerenaDaemon: the router's next-request wake — clear ONLY an
//     IntentReasonIdle stop (ClearStopIntent), nudge the supervisor to
//     reconcile NOW (so it respawns), and probe readiness; an operator stop
//     (user-stop / user-disabled / chronic-failure) is REFUSED so the
//     operator's stop wins and an idle wake never resurrects a disabled
//     daemon (spec §6: "a user-disabled daemon is never woken by an idle
//     wake").
//
// The sweeper itself (which reads per-daemon LAST-ACTIVITY, not wall-clock
// since spawn) lives in the GUI process where /serena/mcp activity is
// observed (internal/gui/serena_idle_sweeper.go); it calls into these
// helpers. The supervisor (a separate process) sees the stop/clear via its
// existing supervisor-intent.json IntentWatcher + reconcile, so the
// stop-propagation path is unchanged from §4/Phase-E.
package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// SerenaIdleShutdownSettingKey is the GUI-settable enum that drives the idle
// threshold (SettingsRegistry: daemons.serena_idle_shutdown).
const SerenaIdleShutdownSettingKey = "daemons.serena_idle_shutdown"

// ErrWakeRefusedOperatorStop is returned by WakeIdleSerenaDaemon when the
// daemon's active stop is an OPERATOR stop (IntentReasonUserStop /
// IntentReasonUserDisabled) or a chronic-failure quarantine — NOT an idle
// stop. The wake refuses to clear it and refuses to spawn: the operator's
// stop wins (spec §6). The router maps this to its existing not-found/down
// 503 rather than resurrecting a daemon the operator deliberately suppressed.
var ErrWakeRefusedOperatorStop = errors.New("serena idle wake refused: daemon has an operator stop, not an idle stop")

// SerenaIdleShutdownThreshold parses the daemons.serena_idle_shutdown enum
// value into its idle threshold. "off" (or any unrecognized value) returns
// (0, false) → idle-shutdown disabled. Recognized values mirror
// SettingsRegistry's enum exactly: 15m / 30m / 1h / 2h.
//
// Pure: no I/O. The GUI sweeper reads the setting value (api.SettingsGet)
// once per tick and passes it here.
func SerenaIdleShutdownThreshold(settingValue string) (time.Duration, bool) {
	switch settingValue {
	case "15m":
		return 15 * time.Minute, true
	case "30m":
		return 30 * time.Minute, true
	case "1h":
		return time.Hour, true
	case "2h":
		return 2 * time.Hour, true
	default:
		// "off" and any out-of-domain value disable idle-shutdown. The
		// registry already validates persisted values back to the default on
		// read (SettingsListIn), so an out-of-domain value here only happens if
		// a caller passes a raw string; failing closed to "disabled" is the
		// safe posture.
		return 0, false
	}
}

// WriteSerenaIdleStop records Desired=stopped + IntentReasonIdle for taskName
// on the UNIFIED supervisor-intent.json stops sub-block — the SOLE stop write
// path after Phase 4-E2. The supervisor's IntentWatcher observes the
// supervisor-intent.json mtime bump, refreshes its UnifiedStopsFile-derived
// intent cache, and the reconcile terminates the running daemon. No second stop
// path is authored (spec §6 / §5.1-E).
//
// FIX-2a: it routes through WriteStopIntentIdleGuarded (NOT the unconditional
// WriteStopIntent) so an idle stop NEVER overwrites an ACTIVE operator stop
// (user-stop / user-disabled / chronic-failure) written between the sweeper's
// status snapshot and this write. The arbitration is performed atomically under
// the supervisor-intent flock, so an operator stop always wins — an idle write
// from a stale snapshot can never resurrect a deliberately-suppressed daemon on
// the next request.
//
// `now` is the evaluation clock (injected for tests); production passes
// time.Now().UTC(). IsActiveStop(now) classifies the idle stop as ACTIVE (it
// never TTL-expires), so the entry is SET when no operator stop blocks it.
func (a *API) WriteSerenaIdleStop(taskName string, now time.Time) error {
	_, err := a.WriteSerenaIdleStopResult(taskName, now)
	return err
}

func (a *API) WriteSerenaIdleStopResult(taskName string, now time.Time) (bool, error) {
	return a.WriteStopIntentIdleGuardedResult(taskName, DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonIdle,
		UpdatedAt: now.UTC(),
	}, "serena-idle-sweeper", now)
}

// serenaWakeReconcileFn is the seam over DialSupervisorIPCReconcile used by
// WakeIdleSerenaDaemon. Default nudges the running supervisor to reconcile NOW
// so the just-cleared idle daemon respawns immediately (the 60s IntentWatcher
// poll is the backstop); tests override it. Mirrors autoRegisterReconcileFn.
var serenaWakeReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
	return DialSupervisorIPCReconcile(ctx, apply)
}

// serenaWakeReadinessFn is the seam over the full wake readiness proof used by
// WakeIdleSerenaDaemon. Default first proves the daemon port's OS owner matches
// the supervisor-reported child PID, then runs the existing serena initialize
// probe. Tests that focus on stop/clear behavior override the whole proof.
var serenaWakeReadinessFn = func(ctx context.Context, taskName string, port int, timeout time.Duration) error {
	return verifySerenaWakeReady(ctx, taskName, port, timeout)
}

// serenaWakeReadStopFn is the seam over the unified-intent stop read used by
// WakeIdleSerenaDaemon's operator-stop guard. Default reads the on-disk
// supervisor-intent.json stops sub-block; tests override it to inject a
// specific prior stop without a real state dir.
var serenaWakeReadStopFn = readSerenaUnifiedStopForTaskWithAuditSink

func (a *API) serenaWakeIsInFlight(taskName string) bool {
	if a == nil {
		return false
	}
	taskName = canonicalIntentTaskKey(taskName)
	// Hold the in-flight mutex across compare-and-clear plus mark. Otherwise a
	// second request can read "no active stop" in the tiny window after the file
	// clear but before the wake is registered, which is exactly the blind-forward
	// race this registry closes.
	a.serenaWakeInFlightMu.Lock()
	defer a.serenaWakeInFlightMu.Unlock()
	return a.serenaWakeInFlight[taskName]
}

func (a *API) clearSerenaWakeInFlight(taskName string) {
	if a == nil {
		return
	}
	taskName = canonicalIntentTaskKey(taskName)
	a.serenaWakeInFlightMu.Lock()
	defer a.serenaWakeInFlightMu.Unlock()
	delete(a.serenaWakeInFlight, taskName)
}

func serenaWakeReadyTimeout(ctx context.Context) time.Duration {
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := serenaWakeReadinessTimeout
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem < timeout {
			timeout = rem
		}
	}
	return timeout
}

func runSerenaWakeReadiness(ctx context.Context, taskName string, port int, readyContext string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := serenaWakeReadyTimeout(ctx)
	if timeout <= 0 {
		return fmt.Errorf("serena idle wake: %s, but the wake deadline expired before the daemon on port %d became ready (retry — the supervisor is bringing it up)", readyContext, port)
	}
	if rdErr := serenaWakeReadinessFn(ctx, taskName, port, timeout); rdErr != nil {
		return fmt.Errorf("serena idle wake: %s, but the daemon on port %d was not ready in time: %w (retry — the supervisor is bringing it up)", readyContext, port, rdErr)
	}
	return nil
}

// serenaStopReadCache is the FIX-5 hot-path guard. Every /serena/mcp request
// classifies the daemon's stop via readSerenaUnifiedStopForTask, which used to
// read+parse the full supervisor-intent.json (and walk the secure-read DACL
// posture) UNCONDITIONALLY — even in steady state when no stop exists. This
// cache memoizes the parsed stops sub-block keyed by the file's (mtime, size):
// each request does only a cheap os.Stat, and re-reads+parses ONLY when the
// stat key changed (any writer commits via atomic temp+rename, so any stop
// write/clear bumps mtime and/or size and busts the cache).
//
// Correctness under concurrent writes: this cache backs ONLY the read-only stop
// CLASSIFICATION (the lock-free read in WakeIdleSerenaDaemon). Every MUTATION
// (WriteStopIntentIdleGuarded SET, ClearStopIntentIfReason compare-and-clear)
// re-reads the file FRESH under the supervisor-intent flock and stays
// authoritative — the cache never participates in a write decision. The only
// effect of a one-request-stale cache is a benign retry: a just-written stop the
// cache hasn't picked up yet means one extra forward attempt against a
// down/up daemon (the next request restats and sees the new mtime). The cache is
// therefore an optimization with no impact on the FIX-2 arbitration invariants.
var serenaStopReadCache struct {
	mu    sync.Mutex
	path  string
	mtime time.Time
	size  int64
	stops map[string]DaemonIntent // the parsed UnifiedStopsFile().Tasks; nil when the file is absent
	valid bool
}

// ReadSerenaUnifiedStopForTask returns the current stop directive for taskName
// from the UNIFIED supervisor-intent.json stops sub-block via the same
// mtime/size-keyed cache WakeIdleSerenaDaemon uses. A missing intent file or
// absent entry returns the zero DaemonIntent (Desired==""), which IsActiveStop
// treats as "not a stop" (default-running). Read-only.
//
// FIX-5: it consults serenaStopReadCache. On a cache hit (file (mtime,size)
// unchanged since the last read) it returns the cached entry WITHOUT a parse; on
// a miss (or first call, or a changed stat) it re-reads+parses and refreshes the
// cache under serenaStopReadCache.mu.
func ReadSerenaUnifiedStopForTask(taskName string) (DaemonIntent, error) {
	return readSerenaUnifiedStopForTask(canonicalIntentTaskKey(taskName))
}

// readSerenaUnifiedStopForTask expects a canonical taskName and implements the
// cached stop read behind ReadSerenaUnifiedStopForTask.
func readSerenaUnifiedStopForTask(taskName string) (DaemonIntent, error) {
	return readSerenaUnifiedStopForTaskWithAuditSink(taskName, LogHubMcpEvent)
}

// readSerenaUnifiedStopForTaskWithAuditSink is the per-call route-owned
// variant. It keeps the shared cache, but any cache miss that has to inspect
// supervisor-intent.json routes broadened-parent diagnostics through sink.
func readSerenaUnifiedStopForTaskWithAuditSink(taskName string, sink func(level, event string, fields map[string]any) error) (DaemonIntent, error) {
	if sink == nil {
		sink = LogHubMcpEvent
	}
	path, err := DefaultSupervisorIntentPath()
	if err != nil {
		return DaemonIntent{}, fmt.Errorf("serena idle wake: resolve supervisor-intent path: %w", err)
	}

	// Cheap stat for the cache key. A missing file is a valid state (no stops
	// recorded yet) — cache it as an empty map keyed on a zero stat so repeated
	// no-file requests do not re-stat-miss every time.
	var statMtime time.Time
	var statSize int64
	fileExists := true
	if fi, statErr := os.Stat(path); statErr != nil {
		if !os.IsNotExist(statErr) {
			return DaemonIntent{}, fmt.Errorf("serena idle wake: stat supervisor-intent.json: %w", statErr)
		}
		fileExists = false
	} else {
		statMtime = fi.ModTime()
		statSize = fi.Size()
	}

	serenaStopReadCache.mu.Lock()
	defer serenaStopReadCache.mu.Unlock()

	if serenaStopReadCache.valid &&
		serenaStopReadCache.path == path &&
		serenaStopReadCache.size == statSize &&
		serenaStopReadCache.mtime.Equal(statMtime) {
		// Hit: the file is unchanged since the last parse. Return the cached
		// entry (absent task → zero DaemonIntent → "not a stop").
		return serenaStopReadCache.stops[taskName], nil
	}

	// Miss: (re)read + parse and refresh the cache.
	var stops map[string]DaemonIntent
	if fileExists {
		raw, rerr := ReadStateFileInodeAnchoredWithAuditSink(path, sink)
		if rerr != nil {
			return DaemonIntent{}, fmt.Errorf("serena idle wake: read supervisor-intent.json: %w", rerr)
		}
		intent, _, rerr := decodeSupervisorIntentFile(path, raw)
		if rerr != nil {
			return DaemonIntent{}, fmt.Errorf("serena idle wake: decode supervisor-intent.json: %w", rerr)
		}
		if intent != nil {
			stops = UnifiedStopsFile(intent, nil).Tasks
		}
	}
	serenaStopReadCache.path = path
	serenaStopReadCache.mtime = statMtime
	serenaStopReadCache.size = statSize
	serenaStopReadCache.stops = stops
	serenaStopReadCache.valid = true
	return stops[taskName], nil
}

// serenaWakeReadinessTimeout bounds the post-respawn readiness probe in
// WakeIdleSerenaDaemon. Generous enough for a serena child whose `.serena/`
// cache is warm (the idle daemon was alive minutes ago, so the cache is on
// disk) but bounded so a wedged respawn returns to the router (→ 503 + client
// retry) rather than blocking the call. Mirrors the auto-register budget.
const serenaWakeReadinessTimeout = 20 * time.Second

const serenaWakeReadinessPollInterval = 50 * time.Millisecond

func verifySerenaWakeReady(ctx context.Context, taskName string, port int, timeout time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if time.Until(deadline) <= 0 {
		return fmt.Errorf("serena idle wake: readiness deadline expired before probing port %d", port)
	}

	if serenaWakePortOwnerProofSupported() {
		expectedPID, err := waitForSerenaWakeSupervisorPID(ctx, taskName, deadline)
		if err != nil {
			return err
		}
		if err := waitForSerenaWakePortOwner(ctx, taskName, port, expectedPID, deadline); err != nil {
			return err
		}
		if rem := time.Until(deadline); rem <= 0 {
			return fmt.Errorf("serena idle wake: readiness deadline expired before serena initialize probe on port %d", port)
		} else if err := verifySerenaProxyReady(port, rem); err != nil {
			return err
		}
		if ok, err := verifySerenaWakePortOwnerNow(taskName, port, expectedPID); err != nil {
			return fmt.Errorf("serena idle wake: port-owner proof failed after serena initialize probe: %w", err)
		} else if !ok {
			return fmt.Errorf("serena idle wake: port-owner proof failed after serena initialize probe: no process owns loopback LISTENING port %d for %s (supervisor-reported daemon PID %d is not listening)", port, canonicalIntentTaskKey(taskName), expectedPID)
		}
		return nil
	}

	// Windows and Linux have an OS-level loopback LISTENING-port owner lookup.
	// Other platforms keep the pre-existing protocol sanity probe until their
	// owner lookup is implemented; unsupported platforms do not claim the PID
	// identity boundary.
	return verifySerenaProxyReady(port, time.Until(deadline))
}

func serenaWakePortOwnerProofSupported() bool {
	return runtime.GOOS == "windows" || runtime.GOOS == "linux"
}

func waitForSerenaWakeSupervisorPID(ctx context.Context, taskName string, deadline time.Time) (int, error) {
	wantTask := canonicalIntentTaskKey(taskName)
	statusCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var lastErr error
	var lastRow DaemonStatus
	var sawRow bool
	for {
		rows, err := supervisorIPCStatusFn(statusCtx)
		if err != nil {
			lastErr = err
		} else {
			lastErr = nil
			if row, ok := serenaWakeStatusRowForTask(rows, wantTask); ok {
				sawRow = true
				lastRow = row
				if serenaWakeStatusRowHasLivePID(row) {
					return row.PID, nil
				}
			}
		}

		if time.Until(deadline) <= 0 {
			return 0, serenaWakeSupervisorPIDWaitError(wantTask, sawRow, lastRow, lastErr)
		}
		if err := sleepUntilNextSerenaWakePoll(statusCtx, deadline); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return 0, serenaWakeSupervisorPIDWaitError(wantTask, sawRow, lastRow, lastErr)
			}
			return 0, fmt.Errorf("serena idle wake: wait for supervisor status live PID for %s: %w", wantTask, err)
		}
	}
}

func serenaWakeStatusRowForTask(rows []DaemonStatus, taskName string) (DaemonStatus, bool) {
	for _, row := range rows {
		if canonicalIntentTaskKey(row.TaskName) == taskName {
			return row, true
		}
	}
	return DaemonStatus{}, false
}

func serenaWakeStatusRowHasLivePID(row DaemonStatus) bool {
	if row.PID <= 0 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(row.State)) {
	case "running", "ready":
		return true
	default:
		return false
	}
}

func serenaWakeSupervisorPIDWaitError(taskName string, sawRow bool, lastRow DaemonStatus, lastErr error) error {
	if lastErr != nil {
		return fmt.Errorf("serena idle wake: supervisor status for %s did not report a live PID before deadline: %w", taskName, lastErr)
	}
	if sawRow {
		return fmt.Errorf("serena idle wake: supervisor status for %s did not report a live PID before deadline (last state=%q pid=%d)", taskName, lastRow.State, lastRow.PID)
	}
	return fmt.Errorf("serena idle wake: supervisor status did not report task %s with a live PID before deadline", taskName)
}

func waitForSerenaWakePortOwner(ctx context.Context, taskName string, port, expectedPID int, deadline time.Time) error {
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	for {
		ok, err := verifySerenaWakePortOwnerNow(taskName, port, expectedPID)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Until(deadline) <= 0 {
			return fmt.Errorf("serena idle wake: no process owns loopback LISTENING port %d for %s before deadline (supervisor-reported daemon PID %d is not listening yet)", port, canonicalIntentTaskKey(taskName), expectedPID)
		}
		if sleepErr := sleepUntilNextSerenaWakePoll(waitCtx, deadline); sleepErr != nil {
			if errors.Is(sleepErr, context.DeadlineExceeded) {
				return fmt.Errorf("serena idle wake: no process owns loopback LISTENING port %d for %s before deadline (supervisor-reported daemon PID %d is not listening yet)", port, canonicalIntentTaskKey(taskName), expectedPID)
			}
			return fmt.Errorf("serena idle wake: wait for port-owner proof for %s on port %d: %w", canonicalIntentTaskKey(taskName), port, sleepErr)
		}
	}
}

func verifySerenaWakePortOwnerNow(taskName string, port, expectedPID int) (bool, error) {
	ownerPID, ok, err := loopbackPortOwnerFn(port)
	if err != nil {
		return false, fmt.Errorf("serena idle wake: resolve OS owner of loopback port %d for %s: %w", port, canonicalIntentTaskKey(taskName), err)
	}
	if !ok {
		return false, nil
	}
	if ownerPID != expectedPID {
		return false, fmt.Errorf("serena idle wake: loopback port %d is owned by PID %d, not the supervisor-reported daemon PID %d for %s", port, ownerPID, expectedPID, canonicalIntentTaskKey(taskName))
	}
	return true, nil
}

func sleepUntilNextSerenaWakePoll(ctx context.Context, deadline time.Time) error {
	sleep := serenaWakeReadinessPollInterval
	if rem := time.Until(deadline); rem < sleep {
		sleep = rem
	}
	if sleep <= 0 {
		return context.DeadlineExceeded
	}
	timer := time.NewTimer(sleep)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// WakeIdleSerenaDaemon is the router's next-request wake for an idle-stopped
// serena pool daemon (spec §6 #6). Contract (the router caller in
// internal/gui/serena_router.go relies on it):
//
//   - The daemon has NO active stop → fast no-op success (nil). The daemon is
//     presumed up; the caller forwards as usual. (This is the steady-state hot
//     path: a request to a live daemon does NOT pay the clear/nudge/probe
//     cost.)
//
//   - The daemon's active stop is IntentReasonIdle → CLEAR it (ClearStopIntent
//     on the unified sub-block), nudge the supervisor to reconcile NOW
//     (apply=true), then probe the external port for readiness. Returns nil
//     once ready; returns a wrapped error (→ router 503, client retries) if
//     the respawn is not ready within the bounded budget. The clear is
//     idempotent, so two concurrent wakes converge.
//
//   - The daemon's active stop is an OPERATOR stop (user-stop / user-disabled)
//     or a chronic-failure quarantine → return ErrWakeRefusedOperatorStop
//     WITHOUT clearing the stop or spawning. The operator's stop wins; an idle
//     wake never resurrects a deliberately-suppressed daemon.
//
// `port` is the daemon's external (client-facing) port (ws.Port). `who` is the
// audit attribution for the clear. ctx bounds the whole wake (the router
// passes its detached+bounded request context).
func (a *API) WakeIdleSerenaDaemon(ctx context.Context, taskName string, port int, who string) error {
	return a.WakeIdleSerenaDaemonWithAuditSink(ctx, taskName, port, who, LogHubMcpEvent)
}

// WakeIdleSerenaDaemonWithAuditSink applies the same wake transition while
// routing its best-effort warning through the caller-owned audit boundary.
// A nil sink preserves the ordinary API contract and uses LogHubMcpEvent.
func (a *API) WakeIdleSerenaDaemonWithAuditSink(ctx context.Context, taskName string, port int, who string, auditSink func(level, event string, fields map[string]any) error) error {
	if auditSink == nil {
		auditSink = LogHubMcpEvent
	}
	now := time.Now().UTC()
	taskKey := canonicalIntentTaskKey(taskName)

	prior, err := serenaWakeReadStopFn(taskKey, auditSink)
	if err != nil {
		return err
	}
	active, reason := prior.IsActiveStop(now)
	if !active {
		// No active stop → daemon presumed up. Fast no-op (steady-state hot
		// path). A future-dated clock-skew stop also lands here as active with
		// the synthetic clock-skew reason, which is NOT idle → refused below.
		if a.serenaWakeIsInFlight(taskKey) {
			return runSerenaWakeReadiness(ctx, taskName, port, fmt.Sprintf("%s wake already in progress", taskName))
		}
		return nil
	}
	if reason != IntentReasonIdle {
		// Operator stop / chronic-failure / clock-skew-suspect: the wake must
		// NOT clear it or spawn. The operator (or fail-closed) suppression
		// wins (spec §6).
		return ErrWakeRefusedOperatorStop
	}

	// Idle stop → clear it so the supervisor's reconcile re-includes the daemon
	// in the spawn-desired set, then nudge an immediate reconcile.
	//
	// FIX-2b: COMPARE-AND-CLEAR — delete the stop ONLY if the on-disk entry is
	// STILL an idle stop. The IsActiveStop classification above read the stop
	// LOCK-FREE; an operator stop (user-stop / user-disabled) written between
	// that read and this clear must NOT be erased by a blind delete. The
	// compare-and-clear is performed under the supervisor-intent flock, so it is
	// atomic against that operator write: if the entry is no longer idle, the
	// clear reports clearAllowed=false and the wake refuses immediately. An
	// absent entry remains clearAllowed=true so two concurrent idle wakes
	// converge without treating the second clear as an operator stop.
	a.serenaWakeInFlightMu.Lock()
	clearAllowed, err := a.ClearStopIntentIfReason(taskKey, IntentReasonIdle, who)
	ownsInFlight := false
	if err == nil && clearAllowed {
		if a.serenaWakeInFlight == nil {
			a.serenaWakeInFlight = make(map[string]bool)
		}
		if !a.serenaWakeInFlight[taskKey] {
			a.serenaWakeInFlight[taskKey] = true
			ownsInFlight = true
		}
	}
	a.serenaWakeInFlightMu.Unlock()
	if err != nil {
		return fmt.Errorf("serena idle wake: clear idle stop for %s: %w", taskName, err)
	}
	if !clearAllowed {
		return ErrWakeRefusedOperatorStop
	}
	if ownsInFlight {
		defer a.clearSerenaWakeInFlight(taskKey)
	}

	// Nudge the supervisor to reconcile NOW. A reachable-supervisor reconcile
	// error stays best-effort: the supervisor's 60s IntentWatcher feeds stop
	// deltas (installPlanCore documents that it does not discover new
	// descriptors, but this wake only changed the stops sub-block), and the
	// readiness probe below remains the success gate. ErrSupervisorIPCUnavailable
	// is different: no live watcher can observe the just-cleared stop, so restore
	// the idle directive before returning a retryable error.
	if _, recErr := serenaWakeReconcileFn(ctx, true); recErr != nil {
		if errors.Is(recErr, ErrSupervisorIPCUnavailable) {
			if ownsInFlight {
				if restoreErr := a.WriteStopIntentIdleGuarded(taskKey, prior, who, time.Now().UTC()); restoreErr != nil {
					return fmt.Errorf("serena idle wake: supervisor IPC unavailable after clearing idle stop for %s; restore idle stop failed: %v: %w", taskName, restoreErr, recErr)
				}
			}
			return fmt.Errorf("serena idle wake: supervisor IPC unavailable after clearing idle stop for %s: %w", taskName, recErr)
		}
		_ = auditSink("warn", "serena-idle-wake-reconcile-nudge-failed", map[string]any{
			"task_name": taskName,
			"err":       recErr.Error(),
		})
	}

	// Probe the external port for readiness, bounded by BOTH the fixed budget
	// AND the caller's remaining deadline so a slow respawn cannot block past
	// the router's advertised window.
	return runSerenaWakeReadiness(ctx, taskName, port, fmt.Sprintf("%s cleared and reconcile nudged", taskName))
}

// Package cli — Task 9.1 maintenance-timer scheduler + Task 9.2
// transient PID tracking with per-timer mutex.
//
// Spec §"Maintenance timer scheduler (detail)" + §"Single-run guard +
// transient tracking" + §"Graceful exit + quiesce drain"
// (docs/superpowers/specs/2026-05-16-v0.5.0-supervisor-architecture.md
// lines 452-483).
//
// Fixed-cadence in-process evaluator. Two kinds in v0.5.0:
//   - workspace-weekly-refresh — Sunday 03:00 local
//   - server-weekly-refresh    — Sunday 03:00 local
//
// Both fire weekly with DST + sleep catch-up. Persistence is
// `supervisor-state.maintenance_fired_at[kind]` serialised as
// RFC3339Nano UTC (Z-suffix) — comparison uses time.Time arithmetic
// only, never manual hour math.
//
// Task 9.2 extensions (additive to Task 9.1):
//
//   - `Spawner` interface abstracts the OS-level spawn so tests can
//     inject a synthetic process surface; production wiring (a later
//     task) uses os/exec.
//   - Per-timer in-process mutex serialises fires of the same `Kind`;
//     different Kinds run concurrently.
//   - "Record PID BEFORE syscall" invariant: a `PID=0` claim slot is
//     written to `supervisor-state.transient_pids` BEFORE
//     `Spawner.Start` is invoked, then the slot is rewritten to the
//     actual PID after spawn succeeds (or rolled back on failure).
//     The forensic property: a panic during Start leaves the PID=0
//     claim visible to outside observers.
//
// The Task 9.1 callers (`SetFireHook` only, no `SetSpawner`) keep
// working unchanged: when `spawner` is nil, Tick falls back to the
// bare fire-hook invocation with no transient pipeline.
package cli

import (
	"log"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
)

// StateStore decouples the scheduler from the on-disk
// SupervisorStateFile. Production wiring (a later task) supplies an
// adapter that persists the underlying map + transient slice. Tests
// use an in-memory implementation.
//
// Transient methods (Task 9.2 additions) MUST be safe for concurrent
// callers: the scheduler invokes them from goroutines spawned per
// fire. The production adapter is expected to take a short lock
// around each call and persist on every mutation (the on-disk file
// is the source of truth that QuiesceHandler.Drain consumes).
type StateStore interface {
	GetMaintenanceFiredAt(kind string) (string, bool)
	SetMaintenanceFiredAt(kind, rfc3339nanoUTC string)

	// AddTransientPID appends a TransientPID entry. The "claim slot"
	// pattern: a `PID=0` entry is written BEFORE the spawn syscall,
	// then this same method is called again with the real PID after
	// spawn succeeds. The store must accept both calls without
	// deduplication; the second call's caller will have already
	// removed the claim slot via RemoveTransientPID(0) and is now
	// installing the post-spawn entry.
	AddTransientPID(p api.TransientPID)

	// RemoveTransientPID removes ALL entries with the given PID.
	// Callers use this in two contexts: (a) the post-Wait drain
	// (removes the real PID); (b) the spawn-failure rollback
	// (removes the PID=0 claim slot). Implementations must tolerate
	// "no such PID" as a no-op rather than an error.
	RemoveTransientPID(pid int)
}

// Spawner abstracts the OS-level process launch so tests can inject a
// synthetic process surface. Production wiring (a later task) maps
// this to `os/exec.Cmd.Start` + `Wait`.
//
// Contract:
//
//   - Start launches the timer's command and returns the OS PID
//     (Windows ProcessId, POSIX pid_t) on success. The returned PID
//     MUST be > 0 (zero is reserved by the scheduler as the
//     pre-spawn claim sentinel).
//   - Start MUST NOT block; the spawn syscall returns immediately
//     once the process is created.
//   - Wait blocks until the process identified by `pid` exits. It
//     returns nil on clean exit (regardless of exit code) and a
//     non-nil error only when Wait itself fails (e.g. PID never
//     existed). The scheduler treats either return as "process is
//     gone, drain the TransientPID".
type Spawner interface {
	Start(t api.MaintenanceTimer) (pid int, err error)
	Wait(pid int) error
}

// MaintenanceScheduler evaluates maintenance timers and fans fire
// events to a caller-supplied hook + (Task 9.2) an optional Spawner.
// Construct via NewMaintenanceScheduler; the zero value is not usable.
type MaintenanceScheduler struct {
	state    StateStore
	fireHook func(api.MaintenanceTimer)

	// Task 9.2 additions:
	spawner  Spawner
	mu       sync.Mutex
	inflight map[string]struct{} // keyed by t.Kind; entry presence = "fire in progress"
	clock    func() time.Time    // UTC clock used for StartedAt timestamping in fire(); test-injectable
}

// NewMaintenanceScheduler builds a scheduler backed by the supplied
// state store.
func NewMaintenanceScheduler(state StateStore) *MaintenanceScheduler {
	return &MaintenanceScheduler{
		state:    state,
		fireHook: func(api.MaintenanceTimer) {}, // no-op default keeps Tick crash-free if caller forgets SetFireHook
		inflight: map[string]struct{}{},
		clock:    func() time.Time { return time.Now().UTC() },
	}
}

// SetFireHook installs the callback invoked once per fired timer.
// Task 9.1: production wires this to the transient-spawn path; tests
// record fires for assertions.
//
// Task 9.2: when a Spawner is also set, the hook fires AFTER
// `Spawner.Start` returns successfully and AFTER the PID is recorded
// in state. The hook is now an observability surface, not the spawn
// surface; the spawn happens through Spawner.
func (s *MaintenanceScheduler) SetFireHook(fn func(api.MaintenanceTimer)) {
	if fn == nil {
		s.fireHook = func(api.MaintenanceTimer) {}
		return
	}
	s.fireHook = fn
}

// SetSpawner installs the process-launch surface used by the Task 9.2
// transient pipeline. When nil, Tick falls back to the Task 9.1
// fire-hook-only path (no PID tracking, no per-timer mutex
// engagement). Wiring this is required for the supervisor's
// fire-and-forget transient lifecycle.
func (s *MaintenanceScheduler) SetSpawner(sp Spawner) {
	s.spawner = sp
}

// SetClock injects a clock used by `fire()` to stamp `StartedAt` on
// the recorded `api.TransientPID`. Defaults to `time.Now().UTC()`.
// Tests use this to make StartedAt deterministic for assertions.
//
// Note: this clock is independent of the `now` value passed to Tick
// (which drives cadence math). Tick keeps using its caller-supplied
// `now`; only the StartedAt timestamp of the recorded transient comes
// from this clock.
func (s *MaintenanceScheduler) SetClock(c func() time.Time) {
	if c == nil {
		s.clock = func() time.Time { return time.Now().UTC() }
		return
	}
	s.clock = c
}

// Tick is one evaluation pass. Called every 60s by the reconcile
// loop (Task 7.1 caller wiring lands later). Pure with respect to
// `now` and the state store for cadence math — no clock calls, no I/O.
//
// Per-timer algorithm (spec lines 458-462):
//
//   - Compute next_due = next Sun 03:00 local on or after
//     last_fired_at[kind] (synthetic baseline = most-recent-past
//     Sun 03:00 local when the entry is absent).
//   - If now >= next_due → fire, set last_fired_at = now.UTC().
//   - The spec's 6h split (within-6h vs. catch-up) is documentation:
//     both paths fire ONCE and update last_fired_at to `now`, so a
//     multi-week sleep produces exactly one catch-up fire on the
//     first post-sleep tick.
//
// "Fire" routing (Task 9.2):
//
//   - When `spawner` is set: route through `fire(t)` which engages
//     the per-timer mutex + claim-before-syscall pipeline + post-Wait
//     drain goroutine.
//   - When `spawner` is nil: fall back to the Task 9.1 fire-hook-only
//     path so existing tests + callers keep working.
//
// `last_fired_at` is set BEFORE the fire path runs (same as Task 9.1).
// This means a fire that is skipped by the per-timer mutex (overlap)
// still updates `last_fired_at` — which is correct: the timer's
// cadence advances regardless of whether the prior instance has
// finished, otherwise a slow handler would wedge weekly cadence.
func (s *MaintenanceScheduler) Tick(now time.Time, timers []api.MaintenanceTimer) {
	for _, t := range timers {
		switch t.Kind {
		case "workspace-weekly-refresh", "server-weekly-refresh":
			// known cadence — fall through to the weekly evaluator
		default:
			log.Printf("severity=warn event=unknown-maintenance-kind kind=%q name=%q", t.Kind, t.Name)
			continue
		}

		baseline := s.parseLastFiredOrSynthesise(t.Kind, now)
		nextDue := nextSunday0300Local(baseline)

		if !now.Before(nextDue) {
			// Fire once. Both within-6h and catch-up paths use the
			// same single-fire semantics; setting last_fired_at = now
			// rebases next_due past `now`, so the same Tick (and any
			// immediate follow-up Tick before another Sunday elapses)
			// cannot re-fire the same window.
			s.state.SetMaintenanceFiredAt(t.Kind, now.UTC().Format(time.RFC3339Nano))

			if s.spawner == nil {
				// Task 9.1 compat path: no spawner → fire the hook
				// inline. No PID tracking, no per-timer mutex
				// engagement, no spawn-failure rollback. Existing
				// Task 9.1 callers (test helpers) keep working.
				s.fireHook(t)
				continue
			}

			s.fire(t)
		}
	}
}

// fire is the Task 9.2 per-timer side-effect path. It runs the full
// claim-before-syscall pipeline + spawns a goroutine that waits for
// the transient to exit and drains it from state.
//
// Ordering (the load-bearing invariant):
//
//  1. Per-timer slot acquired (skip + warn-log if busy). The lock
//     covers ONLY the inflight-map check; we do NOT hold `s.mu` while
//     calling into the state store or the spawner.
//  2. PID=0 claim slot written to state. From this moment, an outside
//     observer (including a goroutine that survives a panic) can see
//     "fire was attempted" for this Kind.
//  3. `Spawner.Start` invoked. On panic, the deferred recovery rolls
//     back the inflight slot but PRESERVES the PID=0 claim (the
//     forensic record).
//  4. On Start success: rewrite the claim (Remove(0) then Add(real
//     PID with StartedAt)). The two-call dance is acceptable because
//     the production StateStore adapter takes a short lock per call;
//     no one else mutates this Kind's row mid-fire because we hold
//     the inflight slot.
//  5. fireHook invoked for observability AFTER spawn + record
//     succeeded.
//  6. Drain goroutine launched: `Spawner.Wait`, then
//     `RemoveTransientPID(realPID)`, then release the inflight slot.
//     Wait MUST return (the contract); the goroutine has a clear exit
//     condition.
//
// Spawn-error rollback (step 3 returns non-nil err): the PID=0 claim
// is removed AND the inflight slot is released so the next Tick can
// retry. The error is logged at warn severity.
func (s *MaintenanceScheduler) fire(t api.MaintenanceTimer) {
	// Step 1: acquire the per-timer slot.
	s.mu.Lock()
	if _, busy := s.inflight[t.Kind]; busy {
		s.mu.Unlock()
		log.Printf("severity=warn event=maintenance-fire-skipped-busy kind=%q", t.Kind)
		return
	}
	s.inflight[t.Kind] = struct{}{}
	s.mu.Unlock()

	// Step 2: write the PID=0 claim slot BEFORE the spawn syscall.
	// Spec §"Single-run guard + transient tracking": "Transient child
	// PID + kind + started_at recorded in supervisor-state.transient_pids
	// BEFORE the syscall."
	claim := api.TransientPID{
		PID:       0, // sentinel: claim made, no real PID yet
		Kind:      t.Kind,
		StartedAt: s.clock().UTC().Format(time.RFC3339Nano),
	}
	s.state.AddTransientPID(claim)

	// Step 3: spawn. Deferred recovery preserves the claim on panic
	// (the forensic property) while still releasing the inflight slot
	// so the system doesn't wedge.
	var pid int
	var startErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				// Preserve the PID=0 claim (don't roll back) for
				// forensic visibility, but release the slot and
				// re-raise the panic to the caller of Tick. The
				// scheduler is in-process; re-raising matches Go
				// convention for unexpected programmer error.
				s.mu.Lock()
				delete(s.inflight, t.Kind)
				s.mu.Unlock()
				panic(r)
			}
		}()
		pid, startErr = s.spawner.Start(t)
	}()

	if startErr != nil {
		// Step 3 failure: roll back the claim AND release the slot
		// so the next Tick can retry.
		s.state.RemoveTransientPID(0)
		s.mu.Lock()
		delete(s.inflight, t.Kind)
		s.mu.Unlock()
		log.Printf("severity=warn event=maintenance-spawn-error kind=%q err=%v", t.Kind, startErr)
		return
	}

	if pid <= 0 {
		// Defensive: spawner contract violation. Treat as error.
		s.state.RemoveTransientPID(0)
		s.mu.Lock()
		delete(s.inflight, t.Kind)
		s.mu.Unlock()
		log.Printf("severity=warn event=maintenance-spawn-bad-pid kind=%q pid=%d", t.Kind, pid)
		return
	}

	// Step 4: rewrite the claim into the real PID record. The
	// StartedAt timestamp from step 2 is preserved (re-stamped here
	// for clarity; same clock instance).
	s.state.RemoveTransientPID(0)
	s.state.AddTransientPID(api.TransientPID{
		PID:       pid,
		Kind:      t.Kind,
		StartedAt: claim.StartedAt,
	})

	// Step 5: fire the observability hook AFTER spawn + record.
	s.fireHook(t)

	// Step 6: drain goroutine. The goroutine has a single exit
	// condition (Spawner.Wait returning) and updates only state +
	// the inflight map under s.mu; no `for {}` loop.
	go func(pid int, kind string) {
		// Wait MUST return per the Spawner contract; we ignore its
		// error because either path means "process is gone, drain
		// the entry".
		_ = s.spawner.Wait(pid)

		s.state.RemoveTransientPID(pid)
		s.mu.Lock()
		delete(s.inflight, kind)
		s.mu.Unlock()
	}(pid, t.Kind)
}

// parseLastFiredOrSynthesise returns either the parsed last_fired_at
// entry or, when the entry is missing/empty/unparseable, a synthetic
// baseline = most-recent-past Sun 03:00 local on or before `now`.
//
// An unparseable stored value is treated identically to a missing
// entry — the alternative would be silently never firing the timer,
// which is the worse outcome (operator sees nothing happen and has
// no diagnostic). Caller flow on first install reaches this with no
// stored entry.
func (s *MaintenanceScheduler) parseLastFiredOrSynthesise(kind string, now time.Time) time.Time {
	if raw, ok := s.state.GetMaintenanceFiredAt(kind); ok && raw != "" {
		if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			return t
		}
		log.Printf("severity=warn event=maintenance-last-fired-unparseable kind=%q value=%q", kind, raw)
	}
	return mostRecentPastSunday0300Local(now)
}

// nextSunday0300Local returns the earliest Sunday 03:00 local time
// `t` such that `t >= after`. Uses time.Date + Weekday() exclusively;
// no manual hour/second arithmetic, so DST transitions are absorbed
// by the runtime's location handling.
//
// Implementation: convert `after` to its local-zone view, build a
// candidate at this week's Sunday 03:00, and walk forward day-by-day
// until the candidate is on a Sunday and at-or-past `after`.
func nextSunday0300Local(after time.Time) time.Time {
	loc := time.Local
	a := after.In(loc)

	// Start with the same calendar day at 03:00 local.
	candidate := time.Date(a.Year(), a.Month(), a.Day(), 3, 0, 0, 0, loc)
	// Walk forward up to 7 days to land on Sunday at-or-after `after`.
	// The bound is 8 iterations to cover the worst case where the
	// starting candidate is a Sunday 03:00 already-past `after` (then
	// we step exactly 7 days forward).
	for i := 0; i < 8; i++ {
		if candidate.Weekday() == time.Sunday && !candidate.Before(after) {
			return candidate
		}
		candidate = candidate.AddDate(0, 0, 1)
	}
	// Unreachable: AddDate always lands on a Sunday within 7 steps and
	// the candidate strictly advances toward `after`. Returning the
	// last computed value preserves total-function semantics.
	return candidate
}

// mostRecentPastSunday0300Local returns the latest Sunday 03:00
// local time `t` such that `t <= before`. Used to synthesise a
// baseline when no last_fired_at entry exists.
func mostRecentPastSunday0300Local(before time.Time) time.Time {
	loc := time.Local
	b := before.In(loc)

	candidate := time.Date(b.Year(), b.Month(), b.Day(), 3, 0, 0, 0, loc)
	for i := 0; i < 8; i++ {
		if candidate.Weekday() == time.Sunday && !candidate.After(before) {
			return candidate
		}
		candidate = candidate.AddDate(0, 0, -1)
	}
	return candidate
}

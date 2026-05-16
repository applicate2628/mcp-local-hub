// Package cli — Task 9.1 maintenance-timer scheduler.
//
// Spec §"Maintenance timer scheduler (detail)"
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
// The evaluator is pure: it takes `now` + `[]MaintenanceTimer` and
// the abstract StateStore, decides per-timer whether to fire, calls
// the fire hook, and writes the new last_fired_at back. Transient PID
// tracking, per-timer mutex, and quiesce drain are NOT implemented
// here — those land in Task 9.2.
package cli

import (
	"log"
	"time"

	"mcp-local-hub/internal/api"
)

// StateStore decouples the scheduler from the on-disk
// SupervisorStateFile. Production wiring (a later task) supplies an
// adapter that persists the underlying map. Tests use an in-memory
// implementation.
type StateStore interface {
	GetMaintenanceFiredAt(kind string) (string, bool)
	SetMaintenanceFiredAt(kind, rfc3339nanoUTC string)
}

// MaintenanceScheduler evaluates maintenance timers and fans
// fire events to a caller-supplied hook. Construct via
// NewMaintenanceScheduler; the zero value is not usable.
type MaintenanceScheduler struct {
	state    StateStore
	fireHook func(api.MaintenanceTimer)
}

// NewMaintenanceScheduler builds a scheduler backed by the supplied
// state store.
func NewMaintenanceScheduler(state StateStore) *MaintenanceScheduler {
	return &MaintenanceScheduler{
		state:    state,
		fireHook: func(api.MaintenanceTimer) {}, // no-op default keeps Tick crash-free if caller forgets SetFireHook
	}
}

// SetFireHook installs the callback invoked once per fired timer.
// Production wires this to the transient-spawn path (Task 9.2);
// tests record fires for assertions.
func (s *MaintenanceScheduler) SetFireHook(fn func(api.MaintenanceTimer)) {
	if fn == nil {
		s.fireHook = func(api.MaintenanceTimer) {}
		return
	}
	s.fireHook = fn
}

// Tick is one evaluation pass. Called every 60s by the reconcile
// loop (Task 7.1 caller wiring lands later). Pure with respect to
// `now` and the state store — no clock calls, no I/O.
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
			s.fireHook(t)
			s.state.SetMaintenanceFiredAt(t.Kind, now.UTC().Format(time.RFC3339Nano))
		}
	}
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

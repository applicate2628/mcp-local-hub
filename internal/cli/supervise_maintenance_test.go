// Tests for the maintenance-timer scheduler (Task 9.1).
//
// Spec §"Maintenance timer scheduler (detail)"
// (docs/superpowers/specs/2026-05-16-v0.5.0-supervisor-architecture.md
// lines 452-483); plan Task 9.1
// (docs/superpowers/plans/2026-05-16-v0.5.0-supervisor.md lines 2310-2362).
//
// TDD discipline: these tests were written and run BEFORE the
// implementation (supervise_maintenance.go) so the failure mode is
// observed first.
package cli

import (
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// testStateStore is the test-only in-memory StateStore used by the
// scheduler tests. Production wiring (a later task) will pass an
// adapter over the on-disk SupervisorStateFile.
//
// The name `newTestState` matches the plan-§2314 test snippet
// verbatim — see plan line 2326 (`state := newTestState(t)`).
type testStateStore struct {
	fired map[string]string
}

func newTestState(_ *testing.T) *testStateStore {
	return &testStateStore{fired: map[string]string{}}
}

func (s *testStateStore) GetMaintenanceFiredAt(kind string) (string, bool) {
	v, ok := s.fired[kind]
	return v, ok
}

func (s *testStateStore) SetMaintenanceFiredAt(kind, rfc3339nanoUTC string) error {
	s.fired[kind] = rfc3339nanoUTC
	return nil
}

// Task 9.2 interface methods — no-ops here. The Task 9.1 tests never
// wire a Spawner, so the transient pipeline never engages and these
// methods are never invoked. They exist only to satisfy the extended
// StateStore interface at compile time. The Task 9.2 transient tests
// use a separate state store (transientTestState) that exercises
// these methods.
func (s *testStateStore) AddTransientPID(_ api.TransientPID) error { return nil }
func (s *testStateStore) RemoveTransientPID(_ int)                 {}
func (s *testStateStore) RemoveTransientClaim(_, _ string)         {}

// failingStateStore is a StateStore stub that returns a fixed error
// from SetMaintenanceFiredAt. Used by TestMaintenance_StormPrevention
// to verify the scheduler's in-memory cache blocks re-fire when the
// underlying state write fails — closes consultant strategic concern
// on PR #243 (repeated-fire-storm at 60s tick cadence).
type failingStateStore struct {
	fired   map[string]string
	failErr error
	calls   int
}

func newFailingStateStore() *failingStateStore {
	return &failingStateStore{fired: map[string]string{}}
}

func (s *failingStateStore) GetMaintenanceFiredAt(kind string) (string, bool) {
	v, ok := s.fired[kind]
	return v, ok
}

func (s *failingStateStore) SetMaintenanceFiredAt(_, _ string) error {
	s.calls++
	return s.failErr
}
func (s *failingStateStore) AddTransientPID(_ api.TransientPID) error { return nil }
func (s *failingStateStore) RemoveTransientPID(_ int)                 {}
func (s *failingStateStore) RemoveTransientClaim(_, _ string)         {}

// TestMaintenance_FiresOnSunday03Local — verbatim from plan §2324-2341.
// Sunday 03:05 local with last_fired one week prior → fires once.
func TestMaintenance_FiresOnSunday03Local(t *testing.T) {
	timer := api.MaintenanceTimer{Name: `\mcp-local-hub-workspace-weekly-refresh`, Kind: "workspace-weekly-refresh", Command: "echo", Args: []string{"fired"}}
	state := newTestState(t)

	// Set last-fired to last Sunday.
	loc := time.Local
	now := time.Date(2026, 5, 17, 3, 5, 0, 0, loc) // Sunday 03:05 local
	state.SetMaintenanceFiredAt("workspace-weekly-refresh", now.AddDate(0, 0, -7).Format(time.RFC3339Nano))

	sched := NewMaintenanceScheduler(state)
	fired := []string{}
	sched.SetFireHook(func(t api.MaintenanceTimer) { fired = append(fired, t.Kind) })
	sched.Tick(now, []api.MaintenanceTimer{timer})

	if len(fired) != 1 {
		t.Fatalf("expected one fire, got %d", len(fired))
	}
}

// TestMaintenance_CatchUpAfterMultiWeekSleep — verbatim from plan §2343-2359.
// last_fired 22 days ago, now monday-after → fires ONCE (catch-up), not
// once-per-missed-Sunday.
func TestMaintenance_CatchUpAfterMultiWeekSleep(t *testing.T) {
	timer := api.MaintenanceTimer{Kind: "workspace-weekly-refresh"}
	state := newTestState(t)

	// last_fired 3 weeks ago.
	state.SetMaintenanceFiredAt("workspace-weekly-refresh", "2026-04-26T03:00:00Z")
	now := time.Date(2026, 5, 18, 14, 0, 0, 0, time.UTC) // Mon, 22 days later

	sched := NewMaintenanceScheduler(state)
	fired := 0
	sched.SetFireHook(func(t api.MaintenanceTimer) { fired++ })
	sched.Tick(now, []api.MaintenanceTimer{timer})

	if fired != 1 {
		t.Fatalf("catch-up should fire ONCE for missed window, got %d", fired)
	}
}

// TestMaintenance_NoFireBeforeSundayDue — Saturday 23:59 local must not
// trip the evaluator; next_due is still in the future.
func TestMaintenance_NoFireBeforeSundayDue(t *testing.T) {
	timer := api.MaintenanceTimer{Kind: "workspace-weekly-refresh"}
	state := newTestState(t)

	loc := time.Local
	// Saturday 23:59 local — one minute short of Sunday.
	now := time.Date(2026, 5, 16, 23, 59, 0, 0, loc)
	// last_fired was the prior Sunday 03:05 (just past 03:00, so next_due
	// is the NEXT Sunday 03:00).
	lastFired := time.Date(2026, 5, 10, 3, 5, 0, 0, loc)
	state.SetMaintenanceFiredAt("workspace-weekly-refresh", lastFired.UTC().Format(time.RFC3339Nano))

	sched := NewMaintenanceScheduler(state)
	fired := 0
	sched.SetFireHook(func(t api.MaintenanceTimer) { fired++ })
	sched.Tick(now, []api.MaintenanceTimer{timer})

	if fired != 0 {
		t.Fatalf("must not fire before Sun 03:00, got %d fires", fired)
	}
}

// TestMaintenance_NoFireWithin6hOfLastFire — last_fired one hour ago at
// Sun 04:00 local: next_due is the following Sun 03:00, six days out.
// `now` is one hour later (Sun 05:00) → no fire.
func TestMaintenance_NoFireWithin6hOfLastFire(t *testing.T) {
	timer := api.MaintenanceTimer{Kind: "workspace-weekly-refresh"}
	state := newTestState(t)

	loc := time.Local
	// `now` is Sunday 05:00 local; last_fired one hour earlier at 04:00.
	now := time.Date(2026, 5, 17, 5, 0, 0, 0, loc)
	lastFired := now.Add(-1 * time.Hour) // Sun 04:00 local
	state.SetMaintenanceFiredAt("workspace-weekly-refresh", lastFired.UTC().Format(time.RFC3339Nano))

	sched := NewMaintenanceScheduler(state)
	fired := 0
	sched.SetFireHook(func(t api.MaintenanceTimer) { fired++ })
	sched.Tick(now, []api.MaintenanceTimer{timer})

	if fired != 0 {
		t.Fatalf("must not re-fire within 6h of last fire (last=Sun04:00, now=Sun05:00); got %d fires", fired)
	}
}

// TestMaintenance_UnknownKindSkipped — unknown kind is logged at warn
// and skipped; state map remains unchanged.
func TestMaintenance_UnknownKindSkipped(t *testing.T) {
	timer := api.MaintenanceTimer{Kind: "future-monthly-thing"}
	state := newTestState(t)

	// Pre-populate something we can re-check stays exactly as written.
	const sentinel = "2020-01-01T00:00:00Z"
	state.SetMaintenanceFiredAt("future-monthly-thing", sentinel)

	loc := time.Local
	now := time.Date(2026, 5, 17, 4, 0, 0, 0, loc) // Sunday 04:00 local

	sched := NewMaintenanceScheduler(state)
	fired := 0
	sched.SetFireHook(func(t api.MaintenanceTimer) { fired++ })
	sched.Tick(now, []api.MaintenanceTimer{timer})

	if fired != 0 {
		t.Fatalf("unknown kind must not fire; got %d fires", fired)
	}
	got, ok := state.GetMaintenanceFiredAt("future-monthly-thing")
	if !ok || got != sentinel {
		t.Fatalf("unknown kind must not mutate state; got (%q,%v) want (%q,true)", got, ok, sentinel)
	}
}

// TestMaintenance_FirstFireWithoutPriorTimestamp — empty state, `now` is
// Sun 04:00 local → fires once, state populated.
func TestMaintenance_FirstFireWithoutPriorTimestamp(t *testing.T) {
	timer := api.MaintenanceTimer{Kind: "server-weekly-refresh"}
	state := newTestState(t)
	// No SetMaintenanceFiredAt call — state is empty.

	loc := time.Local
	now := time.Date(2026, 5, 17, 4, 0, 0, 0, loc) // Sunday 04:00 local

	sched := NewMaintenanceScheduler(state)
	fired := 0
	sched.SetFireHook(func(t api.MaintenanceTimer) { fired++ })
	sched.Tick(now, []api.MaintenanceTimer{timer})

	if fired != 1 {
		t.Fatalf("first fire without prior timestamp must fire once on Sun 03:00+; got %d", fired)
	}
	if _, ok := state.GetMaintenanceFiredAt("server-weekly-refresh"); !ok {
		t.Fatalf("first fire must populate state map for kind")
	}
}

// TestMaintenance_ServerWeeklyRefreshTimersKeyByServer covers the global
// multi-server case: server-weekly-refresh timers share a Kind but have
// distinct Server values, so firing one server must not suppress its sibling.
//
// Negative-control: pre-fix Tick keys maintenance_fired_at by Kind alone. The
// first timer writes server-weekly-refresh, then the second timer reads it as
// already fired and never refreshes.
func TestMaintenance_ServerWeeklyRefreshTimersKeyByServer(t *testing.T) {
	timers := []api.MaintenanceTimer{
		{Kind: "server-weekly-refresh", Server: "alpha"},
		{Kind: "server-weekly-refresh", Server: "beta"},
	}
	state := newTestState(t)

	loc := time.Local
	now := time.Date(2026, 5, 17, 4, 0, 0, 0, loc) // Sunday 04:00 local
	sched := NewMaintenanceScheduler(state)
	var fired []string
	sched.SetFireHook(func(t api.MaintenanceTimer) { fired = append(fired, t.Server) })
	sched.Tick(now, timers)

	if len(fired) != 2 || fired[0] != "alpha" || fired[1] != "beta" {
		t.Fatalf("fired servers = %v, want [alpha beta]", fired)
	}
	if _, ok := state.GetMaintenanceFiredAt("server-weekly-refresh:alpha"); !ok {
		t.Fatalf("missing alpha fired_at entry; state=%+v", state.fired)
	}
	if _, ok := state.GetMaintenanceFiredAt("server-weekly-refresh:beta"); !ok {
		t.Fatalf("missing beta fired_at entry; state=%+v", state.fired)
	}
	if _, ok := state.GetMaintenanceFiredAt("server-weekly-refresh"); ok {
		t.Fatalf("server-scoped timer wrote legacy Kind-only fired_at entry; state=%+v", state.fired)
	}
}

func TestMaintenance_BlankServerWeeklyRefreshTimersKeyByParsedName(t *testing.T) {
	timers := []api.MaintenanceTimer{
		{Name: `\mcp-local-hub-alpha-weekly-refresh`, Kind: "server-weekly-refresh"},
		{Name: `\mcp-local-hub-beta-weekly-refresh`, Kind: "server-weekly-refresh"},
	}
	state := newTestState(t)

	loc := time.Local
	now := time.Date(2026, 5, 17, 4, 0, 0, 0, loc)
	sched := NewMaintenanceScheduler(state)
	var fired []string
	sched.SetFireHook(func(t api.MaintenanceTimer) { fired = append(fired, t.Name) })
	sched.Tick(now, timers)

	if len(fired) != 2 || fired[0] != timers[0].Name || fired[1] != timers[1].Name {
		t.Fatalf("fired timers = %v, want both blank-Server canonical timer names", fired)
	}
	if _, ok := state.GetMaintenanceFiredAt("server-weekly-refresh:alpha"); !ok {
		t.Fatalf("missing alpha parsed-name fired_at entry; state=%+v", state.fired)
	}
	if _, ok := state.GetMaintenanceFiredAt("server-weekly-refresh:beta"); !ok {
		t.Fatalf("missing beta parsed-name fired_at entry; state=%+v", state.fired)
	}
	if _, ok := state.GetMaintenanceFiredAt("server-weekly-refresh"); ok {
		t.Fatalf("blank-Server server timer wrote shared kind-only fired_at entry; state=%+v", state.fired)
	}
	if got := maintenanceTimerIdentityKey(api.MaintenanceTimer{
		Name:   `\mcp-local-hub-alpha-weekly-refresh`,
		Kind:   "server-weekly-refresh",
		Server: "alpha",
	}); got != "server-weekly-refresh:alpha" {
		t.Fatalf("populated Server identity key = %q, want server-weekly-refresh:alpha", got)
	}
}

// TestMaintenance_FirstFireSkippedBeforeFirstDue — empty state but `now`
// is mid-week (Wednesday) → no fire yet because the synthetic baseline
// is most-recent past Sun 03:00 local and next_due is the NEXT Sun
// 03:00. Without this, a fresh install on Wednesday would fire
// immediately on the first tick. The spec language "due immediately if
// now is on/past most recent Sun 03:00 local" must be reconciled with
// the same algorithm used for the populated path; the canonical reading
// is: synthesise baseline = most-recent-past Sun 03:00, then compute
// next_due relative to that baseline. The next_due is therefore
// most-recent-past Sun 03:00 itself, which is in the past, so fire
// fires on the first tick after install — but the test below pins down
// the boundary: on Wednesday the most-recent-past Sun 03:00 was three
// days ago, which exceeds the 6h window — that's still a single fire
// per spec "fire ONCE for the missed window". So Wednesday DOES fire.
//
// To cover the genuine "no fire" pre-install case we instead pin Sat
// morning: most-recent-past Sun 03:00 is six days ago; that still
// fires (catch-up). The only way to NOT fire from empty state is for
// `now` to be before the very first Sun 03:00 the algorithm can
// synthesise — which Go's time.Time can always synthesise (year 1).
// Therefore: from empty state the scheduler ALWAYS fires on the first
// tick. This test pins that invariant so future refactors don't
// silently re-introduce a "wait 7 days from cold install" behavior
// that operators would experience as no maintenance running for a week.
func TestMaintenance_FirstFireOnAnyDay(t *testing.T) {
	timer := api.MaintenanceTimer{Kind: "workspace-weekly-refresh"}
	state := newTestState(t)

	loc := time.Local
	// Wednesday 14:00 local — empty state.
	now := time.Date(2026, 5, 13, 14, 0, 0, 0, loc)

	sched := NewMaintenanceScheduler(state)
	fired := 0
	sched.SetFireHook(func(t api.MaintenanceTimer) { fired++ })
	sched.Tick(now, []api.MaintenanceTimer{timer})

	if fired != 1 {
		t.Fatalf("empty state must catch-up fire once on first tick; got %d", fired)
	}
}

// TestMaintenance_PersistedTimestampSerializedAsRFC3339NanoUTC — after
// fire, the value written into state ends with `Z` and parses cleanly
// back via time.RFC3339Nano (spec §"Serialized as RFC3339Nano UTC").
func TestMaintenance_PersistedTimestampSerializedAsRFC3339NanoUTC(t *testing.T) {
	timer := api.MaintenanceTimer{Kind: "workspace-weekly-refresh"}
	state := newTestState(t)

	loc := time.Local
	now := time.Date(2026, 5, 17, 3, 5, 0, 0, loc) // Sunday 03:05 local
	state.SetMaintenanceFiredAt("workspace-weekly-refresh", now.AddDate(0, 0, -7).Format(time.RFC3339Nano))

	sched := NewMaintenanceScheduler(state)
	sched.SetFireHook(func(api.MaintenanceTimer) {})
	sched.Tick(now, []api.MaintenanceTimer{timer})

	got, ok := state.GetMaintenanceFiredAt("workspace-weekly-refresh")
	if !ok {
		t.Fatalf("state missing kind entry after fire")
	}
	if len(got) == 0 || got[len(got)-1] != 'Z' {
		t.Fatalf("persisted timestamp must be UTC (Z-suffix); got %q", got)
	}
	if _, err := time.Parse(time.RFC3339Nano, got); err != nil {
		t.Fatalf("persisted timestamp must parse as RFC3339Nano; got %q err=%v", got, err)
	}
}

// TestMaintenance_DisabledTimerSkipsFire covers the operator-supported
// off-switch (consultant strategic blocker on PR #243). A timer with
// Enabled=&false must NOT fire even when nextDue has elapsed; nil and
// &true must fire normally (default-on for legacy intent files).
func TestMaintenance_DisabledTimerSkipsFire(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 5, 17, 3, 5, 0, 0, loc)
	tru, fal := true, false

	cases := []struct {
		name      string
		enabled   *bool
		wantFires int
	}{
		{name: "nil-default-on", enabled: nil, wantFires: 1},
		{name: "true-explicit-on", enabled: &tru, wantFires: 1},
		{name: "false-disabled", enabled: &fal, wantFires: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := newTestState(t)
			state.SetMaintenanceFiredAt("workspace-weekly-refresh", now.AddDate(0, 0, -7).Format(time.RFC3339Nano))
			timer := api.MaintenanceTimer{Kind: "workspace-weekly-refresh", Enabled: tc.enabled}

			sched := NewMaintenanceScheduler(state)
			fires := 0
			sched.SetFireHook(func(api.MaintenanceTimer) { fires++ })
			sched.Tick(now, []api.MaintenanceTimer{timer})

			if fires != tc.wantFires {
				t.Errorf("Enabled=%v: got %d fires, want %d", tc.enabled, fires, tc.wantFires)
			}
		})
	}
}

// TestMaintenance_StormPreventionOnStateWriteFailure covers the
// consultant strategic blocker on PR #243 (repeated-fire-storm). When
// the StateStore.SetMaintenanceFiredAt returns an error (transient
// disk I/O failure, AV scanner, quota policy denial), the scheduler's
// in-process cache must capture the fire-at timestamp and authoritatively
// block re-fire on the very next 60s tick — otherwise a single persist
// failure produces a fire every 60s for the duration of the disk
// error (60 fires/hour at supervisor's tick cadence).
func TestMaintenance_StormPreventionOnStateWriteFailure(t *testing.T) {
	loc := time.Local
	tickAt := time.Date(2026, 5, 17, 3, 5, 0, 0, loc) // Sunday 03:05 local
	timer := api.MaintenanceTimer{Kind: "workspace-weekly-refresh"}

	state := newFailingStateStore()
	state.failErr = errSyntheticDiskFull
	// Seed: last fire one week ago, so nextDue check passes on the
	// first Tick (forcing a fire attempt that will hit the failing
	// state write).
	state.fired["workspace-weekly-refresh"] = tickAt.AddDate(0, 0, -7).Format(time.RFC3339Nano)

	sched := NewMaintenanceScheduler(state)
	fires := 0
	sched.SetFireHook(func(api.MaintenanceTimer) { fires++ })

	// Tick 1: nextDue passes, fire fires, SetMaintenanceFiredAt
	// FAILS, scheduler caches in-process.
	sched.Tick(tickAt, []api.MaintenanceTimer{timer})
	if fires != 1 {
		t.Fatalf("Tick 1: got %d fires, want 1 (first fire must proceed; cache engaged only after persist error)", fires)
	}
	if state.calls != 1 {
		t.Fatalf("Tick 1: state.SetMaintenanceFiredAt called %d times, want 1", state.calls)
	}

	// Tick 2: state is still STALE (returns the original 1-week-ago
	// value because the failing SetMaintenanceFiredAt did not
	// persist). Without the cache, the scheduler would re-fire here.
	// With the cache, the cache returns the recent fire-at and
	// nextDue moves 7 days forward — no re-fire.
	sched.Tick(tickAt.Add(60*time.Second), []api.MaintenanceTimer{timer})
	if fires != 1 {
		t.Errorf("Tick 2 (60s after failed persist): got %d fires, want 1 — STORM regression: in-process cache failed to block re-fire of failing-persist kind", fires)
	}

	// Tick 3: 10 minutes later, still nothing changed on disk.
	// Cache must still hold.
	sched.Tick(tickAt.Add(10*time.Minute), []api.MaintenanceTimer{timer})
	if fires != 1 {
		t.Errorf("Tick 3 (10min after failed persist): got %d fires, want 1 — STORM regression: cache eviction broke storm prevention", fires)
	}
}

// flakyStateStore stores fired-at values on success but can be told to
// fail the next SetMaintenanceFiredAt (toggling failNext). Unlike
// failingStateStore it WRITES the value through on success, so a test
// can drive the fail→recover→read-back sequence the cache-invariant
// test (PR #243 bot P1#1) needs.
type flakyStateStore struct {
	fired    map[string]string
	failNext bool
}

func newFlakyStateStore() *flakyStateStore { return &flakyStateStore{fired: map[string]string{}} }

func (s *flakyStateStore) GetMaintenanceFiredAt(kind string) (string, bool) {
	v, ok := s.fired[kind]
	return v, ok
}
func (s *flakyStateStore) SetMaintenanceFiredAt(kind, v string) error {
	if s.failNext {
		return errSyntheticDiskFull
	}
	s.fired[kind] = v
	return nil
}
func (s *flakyStateStore) AddTransientPID(_ api.TransientPID) error { return nil }
func (s *flakyStateStore) RemoveTransientPID(_ int)                 {}
func (s *flakyStateStore) RemoveTransientClaim(_, _ string)         {}

// TestMaintenance_CacheClearedAfterPersistRecovers covers PR #243 bot
// P1#1. The storm-prevention cache is populated on a persist FAILURE so
// the next 60s tick does not re-fire while disk stays stale. But once a
// later persist SUCCEEDS, the cache must be cleared — otherwise the
// stale cached timestamp (frozen at the failed-fire moment) keeps
// winning over disk, and once it ages past a week every tick looks due
// → a fire storm that only a supervisor restart ends.
func TestMaintenance_CacheClearedAfterPersistRecovers(t *testing.T) {
	loc := time.Local
	timer := api.MaintenanceTimer{Kind: "workspace-weekly-refresh"}
	state := newFlakyStateStore()
	sched := NewMaintenanceScheduler(state)
	fires := 0
	sched.SetFireHook(func(api.MaintenanceTimer) { fires++ })

	week1 := time.Date(2026, 5, 17, 4, 0, 0, 0, loc) // Sunday 04:00
	week2 := time.Date(2026, 5, 24, 4, 0, 0, 0, loc) // next Sunday 04:00

	// Tick 1: due (synthetic baseline), persist FAILS → cache engaged.
	state.failNext = true
	sched.Tick(week1, []api.MaintenanceTimer{timer})
	if fires != 1 {
		t.Fatalf("Tick 1: want 1 fire, got %d", fires)
	}

	// Persist recovers for all subsequent ticks.
	state.failNext = false

	// Tick 1b (+60s): cache (week1) blocks re-fire — no storm yet.
	sched.Tick(week1.Add(60*time.Second), []api.MaintenanceTimer{timer})
	if fires != 1 {
		t.Fatalf("Tick 1b: cache must block re-fire 60s after failed persist; got %d fires", fires)
	}

	// Tick 2: a week later the window is due again; this fire's persist
	// SUCCEEDS and must clear the cache so disk becomes authoritative.
	sched.Tick(week2, []api.MaintenanceTimer{timer})
	if fires != 2 {
		t.Fatalf("Tick 2: want 2 fires (new weekly window), got %d", fires)
	}

	// Tick 2b (+60s): with the cache cleared, the read falls through to
	// disk (week2) → next_due is 7 days out → NO fire. If the cache had
	// NOT been cleared it would still hold week1, making this tick (and
	// every tick after) look due → storm (fires would climb to 3+).
	sched.Tick(week2.Add(60*time.Second), []api.MaintenanceTimer{timer})
	if fires != 2 {
		t.Fatalf("Tick 2b: stale cache not cleared after successful persist — STORM regression: got %d fires, want 2", fires)
	}
}

// TestMaintenance_NoRefireOnExactSunday0300 covers PR #243 bot round-2
// P2. A fire that lands exactly on Sunday 03:00:00 local persists that
// instant as last_fired_at; with an inclusive next-due boundary the
// same instant looked due again on the next 60s tick → a double fire.
// The strictly-after boundary for real fires must advance a full week.
func TestMaintenance_NoRefireOnExactSunday0300(t *testing.T) {
	loc := time.Local
	exact := time.Date(2026, 5, 17, 3, 0, 0, 0, loc) // Sunday 03:00:00 exactly
	timer := api.MaintenanceTimer{Kind: "workspace-weekly-refresh"}
	state := newTestState(t) // compat path (no spawner): deterministic fire count
	sched := NewMaintenanceScheduler(state)
	fires := 0
	sched.SetFireHook(func(api.MaintenanceTimer) { fires++ })

	// First tick exactly at Sun 03:00:00 — fires (synthetic baseline due).
	sched.Tick(exact, []api.MaintenanceTimer{timer})
	if fires != 1 {
		t.Fatalf("exact Sun 03:00:00 first tick: want 1 fire, got %d", fires)
	}
	// Next 60s tick — must NOT re-fire: last_fired == Sun 03:00:00, so
	// the strictly-after boundary puts next_due a full week out.
	sched.Tick(exact.Add(60*time.Second), []api.MaintenanceTimer{timer})
	if fires != 1 {
		t.Fatalf("exact-03:00 refire regression: tick +60s re-fired; want 1 got %d", fires)
	}
	// Several minutes later still no re-fire.
	sched.Tick(exact.Add(5*time.Minute), []api.MaintenanceTimer{timer})
	if fires != 1 {
		t.Fatalf("exact-03:00 refire regression: tick +5m re-fired; want 1 got %d", fires)
	}
}

// errSyntheticDiskFull is the synthetic error injected by the storm
// prevention test to simulate a state-write failure.
var errSyntheticDiskFull = newSyntheticErr("synthetic disk full (test-only)")

type syntheticErr struct{ msg string }

func newSyntheticErr(msg string) *syntheticErr { return &syntheticErr{msg: msg} }
func (e *syntheticErr) Error() string          { return e.msg }

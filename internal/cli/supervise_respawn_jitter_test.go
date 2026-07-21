package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// The tests below cover the thundering-herd amplifier: the six serena workspace
// proxies fail within ~250 ms of each other and then re-arm on an IDENTICAL
// deterministic ladder, so every retry wave re-collides at full width and
// reproduces the contention that caused the first failure.
//
// NON-VACUITY: against the pre-fix tree armRespawnBackoffTimer armed
// computeRespawnBackoff(failures) verbatim, so TestRespawnBackoff_HerdDoesNotRearmInLockstep
// sees one distinct delay and fails. See the RED evidence in the delivery report.

// TestRespawnBackoff_HerdDoesNotRearmInLockstep is the herd regression: a fleet
// of same-server daemons that crash simultaneously must NOT all re-arm at the
// same instant. Drives the production arm path (armRespawnBackoffTimer) for a
// six-daemon fleet at the same failure count and asserts the armed delays are
// actually spread.
func TestRespawnBackoff_HerdDoesNotRearmInLockstep(t *testing.T) {
	ctrl, _, _ := armGenController(t)

	// The observed fleet: six serena workspace proxies, same failure count, so
	// the deterministic ladder hands all six an identical base.
	const fleet = 6
	const failures = 3

	base := computeRespawnBackoff(failures)
	distinct := map[time.Duration]struct{}{}
	for i := 0; i < fleet; i++ {
		d := serenaReadyBudgetDescriptor()
		// Distinct task names so the arm-generation guard treats them as six
		// independent daemons rather than superseding each other.
		d.TaskName = `\mcp-local-hub-serena-herd-` + string(rune('a'+i))
		armed := ctrl.armRespawnBackoffTimer(*d, d.TaskName, base)

		if armed < base {
			t.Fatalf("daemon %d armed %v, which is SOONER than the ladder's %v — downward jitter would accelerate the sliding-window crash count toward quarantine", i, armed, base)
		}
		distinct[armed] = struct{}{}
	}

	if len(distinct) == 1 {
		t.Fatalf("all %d daemons armed the identical delay %v — they re-enter cold start in lockstep and reproduce the contention that failed them (this is the herd the fix must break)", fleet, base)
	}
}

// TestRespawnScheduledAudit_ReportsSubSecondJitterPrecisely is the regression
// for the audit row rounding away the very precision it was added to expose.
//
// Jitter is sub-second across most of the ladder — the FIRST respawn's 1 s base
// takes 0-500 ms — so a seconds-granularity field truncates the whole spread:
// an armed 1.2 s and an unjittered 1.0 s both report `backoff_seconds: 1`,
// making the row useless for correlating against an observed respawn time,
// which is the stated reason the armed value is emitted at all.
//
// The jitter fraction is PINNED here; with the production rand source the armed
// delay would be random and this assertion would be a coin flip.
//
// NON-VACUITY: against a mutated emit that carries only the seconds fields
// (`backoff_seconds` / `backoff_base_seconds`, the pre-fix shape) this test
// fails — the precise fields are absent. See the RED evidence in the report.
func TestRespawnScheduledAudit_ReportsSubSecondJitterPrecisely(t *testing.T) {
	tmp := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmp, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	t.Cleanup(func() { _ = events.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	descriptor := api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-test-jitter-precision`,
		Server:   "test",
		Daemon:   "default",
	}

	var spawns atomic.Int32
	ctrl := &supervisorController{
		intentCache:         newIntentCache(),
		tracker:             NewDaemonRuntimeTracker(),
		events:              events,
		daemonIntent:        newDaemonIntentCache(),
		spawn:               func(api.SupervisorDaemon) error { spawns.Add(1); return nil },
		ctx:                 ctx,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
		// Pinned: base 1s + 0.4 * (1s * 0.5 span) = 1.2s armed. Deliberately
		// sub-second jitter, which is the band the seconds field destroys.
		jitterSource: func() float64 { return 0.4 },
	}
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: []api.SupervisorDaemon{descriptor}})
	ctrl.smStates.Store(descriptor.TaskName, api.StRunning)

	ctrl.handleLoopEvent(api.LoopEvent{
		Kind:     api.EvChildExit,
		TaskName: descriptor.TaskName,
		Body:     map[string]any{"exit_code": 1},
	})

	body := findRespawnScheduledBody(t, eventsPath)

	const wantBaseMS, wantArmedMS = 1000, 1200
	gotArmed := jsonNumField(t, body, "backoff_ms")
	gotBase := jsonNumField(t, body, "backoff_base_ms")

	if gotArmed != wantArmedMS {
		t.Errorf("backoff_ms = %v, want %d (base 1000ms + 0.4 of the 500ms span)", gotArmed, wantArmedMS)
	}
	if gotBase != wantBaseMS {
		t.Errorf("backoff_base_ms = %v, want %d", gotBase, wantBaseMS)
	}
	if gotArmed == gotBase {
		t.Errorf("backoff_ms (%v) == backoff_base_ms (%v): the row does not distinguish the armed delay from the ladder value, so an operator cannot correlate it against an observed respawn", gotArmed, gotBase)
	}

	// The pre-existing coarse field is preserved AND is demonstrably unable to
	// carry this distinction — which is exactly why the ms fields exist. If
	// this stops holding, the seconds field silently became the precise one and
	// the doc comment above it is stale.
	if got := jsonNumField(t, body, "backoff_seconds"); got != 1 {
		t.Errorf("backoff_seconds = %v, want 1 (pre-existing truncated-seconds contract preserved)", got)
	}
}

// findRespawnScheduledBody returns the body of the single
// daemon-respawn-scheduled row in the event log.
func findRespawnScheduledBody(t *testing.T, eventsPath string) map[string]any {
	t.Helper()
	assertEventInLog(t, eventsPath, `"event":"daemon-respawn-scheduled"`)
	raw, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, `"event":"daemon-respawn-scheduled"`) {
			continue
		}
		var row struct {
			Event string         `json:"event"`
			Body  map[string]any `json:"body"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("decode event row %q: %v", line, err)
		}
		return row.Body
	}
	t.Fatalf("no daemon-respawn-scheduled row in:\n%s", raw)
	return nil
}

// jsonNumField reads a numeric body field, failing the test when it is absent —
// which is what makes the "report rounded seconds again" mutation fail rather
// than silently skip the assertion.
func jsonNumField(t *testing.T, body map[string]any, key string) float64 {
	t.Helper()
	v, ok := body[key]
	if !ok {
		t.Fatalf("audit body has no %q field (keys present: %v); the row cannot express the armed delay precisely", key, sortedKeys(body))
	}
	n, ok := v.(float64)
	if !ok {
		t.Fatalf("audit field %q = %v (%T), want a number", key, v, v)
	}
	return n
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestJitteredRespawnBackoff_Bounds pins the spread's contract directly on the
// pure function, across the whole ladder including the capped plateau, without
// depending on randomness.
func TestJitteredRespawnBackoff_Bounds(t *testing.T) {
	for _, base := range []time.Duration{
		respawnBackoffStep,
		4 * time.Second,
		respawnBackoffMax,
	} {
		span := time.Duration(float64(base) * respawnBackoffJitterFraction)
		if span > respawnBackoffJitterMax {
			span = respawnBackoffJitterMax
		}
		for _, r := range []float64{0, 0.25, 0.5, 0.75, 1} {
			got := jitteredRespawnBackoff(base, r)
			// Upward-only: never sooner than the ladder.
			if got < base {
				t.Fatalf("base=%v r=%v: armed %v < base — jitter must never accelerate a retry", base, r, got)
			}
			if got > base+span {
				t.Fatalf("base=%v r=%v: armed %v exceeds base+span (%v) — the recovery-latency tail is unbounded", base, r, got, base+span)
			}
		}
	}
}

// TestJitteredRespawnBackoff_ZeroBaseUnchanged: a zero backoff means "respawn
// now" (the failures<=0 case). Delaying it would change spawn semantics.
func TestJitteredRespawnBackoff_ZeroBaseUnchanged(t *testing.T) {
	if got := jitteredRespawnBackoff(0, 1); got != 0 {
		t.Fatalf("jitteredRespawnBackoff(0, 1) = %v, want 0 — an immediate respawn must stay immediate", got)
	}
}

// TestJitteredRespawnBackoff_OutOfRangeFractionClamped keeps the spread bounded
// even if a caller supplies a fraction outside [0,1].
func TestJitteredRespawnBackoff_OutOfRangeFractionClamped(t *testing.T) {
	base := 4 * time.Second
	span := time.Duration(float64(base) * respawnBackoffJitterFraction)
	if got := jitteredRespawnBackoff(base, -5); got != base {
		t.Fatalf("negative fraction produced %v, want the bare base %v", got, base)
	}
	if got := jitteredRespawnBackoff(base, 5); got != base+span {
		t.Fatalf("over-range fraction produced %v, want the capped %v", got, base+span)
	}
}

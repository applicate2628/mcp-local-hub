// internal/cli/builtin_route_daemon_generation_test.go
//
// Finding A3 (architecture-adversarial-reverify.md,
// work-items/active/2026-07-25-mcp-front-daemon/), runtime-proven by
// qa-adversarial-falsifiers.md: ensureBuiltinRouteDaemonAtStartup had two
// independent defects in its return-value contract.
//
//  1. On a nil input, a strict-port or persistence pre-commit failure
//     returned a FABRICATED non-nil empty intent instead of preserving nil —
//     because the function allocated `&api.SupervisorIntentFile{Version: 1}`
//     and reassigned its own `intent` parameter BEFORE those failure checks
//     ran, so both failure paths returned the freshly-allocated value instead
//     of the true (nil) original.
//  2. On success, the function reapplied its own mutation to the CALLER's
//     pre-lock in-memory copy instead of adopting the exact generation
//     api.MutateSupervisorIntentIfChangedReturning observed under the flock
//     — so a concurrent writer's disk-committed change (another daemon, a
//     stop, a strict-mode flip) landed on disk but was silently absent from
//     the returned value the caller feeds into the controller-cache seed and
//     initial reconcile pass.
//
// These tests reproduce both defects deterministically (no goroutines or
// pause hooks needed for #2 — the staleness is a simple point-in-time
// ordering, not a live race: writing generation B to disk BEFORE calling the
// seeder with the caller's stale generation A reproduces exactly the
// "another writer committed between load and lock" scenario every time).
package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// TestEnsureBuiltinRouteDaemonAtStartup_AdoptsObservedGenerationOverStaleCaller
// is the A3 falsifying test for defect #2. It seeds a stale caller intent A,
// writes a NEWER, disk-only generation B (an extra daemon, an extra stop,
// and StrictMode=true — none of which the caller's stale A carries) directly
// to disk, then calls the seeder with A. The returned intent must carry
// every part of B, exactly one canonical built-in route row, and must be
// deeply equal to a fresh independent disk re-read.
//
// Mutation-proven: reverting the seeder to reapply its mutation to
// `workingIntent`/`originalIntent` and return that (the pre-A3-fix shape)
// makes this test fail — the returned intent carries stale A's
// StrictMode=false and omits B's extra daemon and stop. See the commit
// message for the exact reverted-and-restored transcript.
func TestEnsureBuiltinRouteDaemonAtStartup_AdoptsObservedGenerationOverStaleCaller(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")

	eventsPath := filepath.Join(stateDir, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	// Stale caller generation A: what this boot's own loadIntentFiles read
	// before the seeder's locked mutation. Deliberately carries none of
	// generation B's content below.
	staleA := &api.SupervisorIntentFile{
		Version:    1,
		StrictMode: false,
	}

	const extraDaemonTask = `\qa-a3-generation-b-extra`
	const stoppedTask = `\qa-a3-generation-b-stopped`

	// A concurrent writer commits generation B to disk between the caller's
	// load and the seeder's lock acquisition — simulated deterministically
	// by writing B directly before calling the seeder (the seeder's own
	// fresh disk read happens strictly inside the call below, so whatever
	// is on disk at that point is what it will observe).
	genB := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{
				TaskName: extraDaemonTask,
				Server:   "qa-extra",
				Daemon:   "extra",
				Command:  "/bin/qa-extra",
			},
		},
		Stops: map[string]api.DaemonIntent{
			stoppedTask: {Desired: "stopped", Reason: "operator"},
		},
		StrictMode: true,
	}
	if err := api.WriteSupervisorIntent(intentPath, genB); err != nil {
		t.Fatalf("seed generation B directly to disk: %v", err)
	}

	got := ensureBuiltinRouteDaemonAtStartup(stateDir, staleA, events)
	if got == nil {
		t.Fatalf("ensureBuiltinRouteDaemonAtStartup returned nil")
	}

	if !got.StrictMode {
		t.Fatalf("returned intent StrictMode = false, want true (generation B's value) — the return adopted stale caller A instead of the disk-observed generation")
	}

	foundExtra := false
	routeRows := 0
	for _, d := range got.Daemons {
		if d.TaskName == extraDaemonTask {
			foundExtra = true
		}
		if d.TaskName == api.BuiltinRouteTaskName {
			routeRows++
		}
	}
	if !foundExtra {
		t.Fatalf("returned intent is missing generation B's extra daemon %s — a concurrent writer's committed change was silently dropped", extraDaemonTask)
	}
	if routeRows != 1 {
		t.Fatalf("returned intent has %d canonical built-in route rows, want exactly 1: %+v", routeRows, got.Daemons)
	}
	if _, ok := got.Stops[stoppedTask]; !ok {
		t.Fatalf("returned intent is missing generation B's stop %s — a concurrent writer's committed stop was silently dropped", stoppedTask)
	}

	// The returned value must be indistinguishable from a fresh, independent
	// disk re-read: both should reflect the exact same committed generation.
	disk, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("re-read %s: %v", intentPath, err)
	}
	if !reflect.DeepEqual(got, disk) {
		t.Fatalf("returned intent diverges from a fresh disk re-read:\nreturned=%+v\ndisk=%+v", got, disk)
	}
}

// TestEnsureBuiltinRouteDaemonAtStartup_NilInputStrictPortFailurePreservesNil
// is the A3 falsifying test for defect #1's strict-port-failure branch: a
// nil input must return nil on a strict-port resolution failure, not a
// fabricated non-nil empty intent (which would wrongly enable the caller's
// `intent != nil`-gated initial reconcile branch on a resolution failure).
//
// Mutation-proven: reverting to the pre-A3-fix ordering (allocate
// `&api.SupervisorIntentFile{Version: 1}` and reassign the `intent`
// parameter BEFORE the strict-port check) makes this test fail — the
// returned value is non-nil.
func TestEnsureBuiltinRouteDaemonAtStartup_NilInputStrictPortFailurePreservesNil(t *testing.T) {
	stateDir := t.TempDir()
	eventsPath := filepath.Join(stateDir, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	prevPortFn := resolveMCPFrontPortStrictFn
	resolveMCPFrontPortStrictFn = func() (int, error) {
		return 0, os.ErrInvalid
	}
	t.Cleanup(func() { resolveMCPFrontPortStrictFn = prevPortFn })

	got := ensureBuiltinRouteDaemonAtStartup(stateDir, nil, events)
	if got != nil {
		t.Fatalf("ensureBuiltinRouteDaemonAtStartup(nil, ...) with a forced strict-port failure returned %+v, want nil", got)
	}

	if _, statErr := os.Stat(filepath.Join(stateDir, "supervisor-intent.json")); statErr == nil {
		t.Fatalf("supervisor-intent.json was created despite a port-resolution failure on a nil input")
	}
}

// TestEnsureBuiltinRouteDaemonAtStartup_NilInputPersistFailurePreservesNil is
// the A3 falsifying test for defect #1's persist-failure branch: a nil input
// must return nil on a locked-persist failure too, not just on a strict-port
// failure. Forces the persist to fail deterministically the same way
// TestEnsureBuiltinRouteDaemonAtStartup_PersistFailurePreservesExistingRow
// does: stateDir is a FILE, not a directory, so
// MutateSupervisorIntentIfChangedReturning's os.MkdirAll(filepath.Dir(path))
// fails before ever touching the flock or the caller's intent.
//
// Mutation-proven: reverting to the pre-A3-fix ordering makes this test fail
// — the returned value is non-nil.
func TestEnsureBuiltinRouteDaemonAtStartup_NilInputPersistFailurePreservesNil(t *testing.T) {
	parent := t.TempDir()
	stateDir := filepath.Join(parent, "not-a-directory")
	if err := os.WriteFile(stateDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("seed non-directory stateDir: %v", err)
	}

	eventsPath := filepath.Join(parent, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	got := ensureBuiltinRouteDaemonAtStartup(stateDir, nil, events)
	if got != nil {
		t.Fatalf("ensureBuiltinRouteDaemonAtStartup(nil, ...) with a forced persist failure returned %+v, want nil", got)
	}
}

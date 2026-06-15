package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// withFakePortOwnersSnapshot swaps the package-level
// loopbackPortOwnersSnapshotFn for the duration of a test and returns a pointer
// to a call counter so the test can assert the snapshot is taken EXACTLY ONCE
// per supervisorStatusDaemons refresh (the core perf guarantee — N daemons,
// ONE OS port-owner query, not N).
func withFakePortOwnersSnapshot(t *testing.T, fn func() (map[int]int, error)) *int {
	t.Helper()
	var calls int
	prev := loopbackPortOwnersSnapshotFn
	loopbackPortOwnersSnapshotFn = func() (map[int]int, error) {
		calls++
		return fn()
	}
	t.Cleanup(func() { loopbackPortOwnersSnapshotFn = prev })
	return &calls
}

// seedRunningDaemons writes a state-safe supervisor-intent.json with n Running
// daemons (ports preset so the status producer never reaches
// ResolveManifestDaemonPort / the registry) and returns the stateDir, the
// populated tracker, and a task -> (pid, port) map for assertions. StartedAt is
// set well past the bind grace so a not-live classification is not masked by
// the grace window.
func seedRunningDaemons(t *testing.T, n int) (string, *DaemonRuntimeTracker, map[string]struct{ pid, port int }) {
	t.Helper()
	stateDir := apitest.HardenedTempDir(t)
	intent := &api.SupervisorIntentFile{Version: 1}
	meta := map[string]struct{ pid, port int }{}
	tracker := NewDaemonRuntimeTracker()
	startedAt := time.Now().UTC().Add(-1 * time.Hour) // far past the 5s bind grace
	for i := 0; i < n; i++ {
		task := fmt.Sprintf(`\mcp-local-hub-srv%d-default`, i)
		pid := 500000 + i
		port := 9300 + i
		intent.Daemons = append(intent.Daemons, api.SupervisorDaemon{
			TaskName: task,
			Server:   fmt.Sprintf("srv%d", i),
			Daemon:   "default",
			Port:     port,
		})
		tracker.MarkSpawned(task, pid, startedAt)
		meta[task] = struct{ pid, port int }{pid, port}
	}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	return stateDir, tracker, meta
}

// installOwnsPortProbe sets the GLOBAL liveness probe so the status path takes
// the snapshot branch (PortOwnerPID must be non-nil to enable it) with
// PIDAlive=true and NO PIDIdentity proof (so the per-daemon liveness reduces to
// the port-owner check resolved from the injected snapshot). The PortOwnerPID
// installed here is never actually called by the status path — it is replaced
// by the snapshot-backed closure inside supervisorStatusDaemons — but it must
// be non-nil to gate the snapshot branch on.
func installOwnsPortProbe(t *testing.T) {
	t.Helper()
	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive:     func(pid int) bool { return true },
		PortLive:     func(port int) bool { return true },
		PortOwnerPID: func(port int) (int, bool, error) { return 0, false, errors.New("must not be called directly") },
	})
	t.Cleanup(restore)
}

// TestSupervisorStatusTakesPortOwnerSnapshotExactlyOnce is the core perf
// guarantee: resolving N(>=3) running daemons takes ONE OS port-owner snapshot,
// not N per-port netstat spawns. It also asserts every daemon resolves Running
// against the shared snapshot (each port maps to its own tracked PID).
func TestSupervisorStatusTakesPortOwnerSnapshotExactlyOnce(t *testing.T) {
	const n = 4
	stateDir, tracker, meta := seedRunningDaemons(t, n)
	installOwnsPortProbe(t)

	// Build the snapshot: every daemon's port maps to its OWN tracked PID, so
	// every row resolves live (port_owner == current_pid).
	snap := map[int]int{}
	for _, m := range meta {
		snap[m.port] = m.pid
	}
	calls := withFakePortOwnersSnapshot(t, func() (map[int]int, error) { return snap, nil })

	rows, err := supervisorStatusDaemons(stateDir, tracker)
	if err != nil {
		t.Fatalf("supervisorStatusDaemons: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("port-owner snapshot taken %d times, want EXACTLY 1 (one query for all %d daemons)", *calls, n)
	}
	if len(rows) != n {
		t.Fatalf("rows len = %d, want %d", len(rows), n)
	}
	for _, row := range rows {
		if row["state"] != "Running" {
			t.Fatalf("daemon %v resolved %q, want Running (port owner == tracked pid): %+v", row["task_name"], row["state"], row)
		}
		if _, hasStale := row["stale_pid"]; hasStale {
			t.Fatalf("daemon %v should not carry stale_pid when owned by its tracked pid: %+v", row["task_name"], row)
		}
	}
}

// TestSupervisorStatusSnapshotErrorStaysRunning asserts a snapshot ERROR is
// treated exactly like a per-port netstat failure: the daemon classifies
// port_owner_unverified, which the status producer keeps as Running (not
// Restarting). A probe error must NEVER flip the row to a restart state.
func TestSupervisorStatusSnapshotErrorStaysRunning(t *testing.T) {
	const n = 3
	stateDir, tracker, _ := seedRunningDaemons(t, n)
	installOwnsPortProbe(t)

	calls := withFakePortOwnersSnapshot(t, func() (map[int]int, error) {
		return nil, errors.New("netstat policy-blocked")
	})

	rows, err := supervisorStatusDaemons(stateDir, tracker)
	if err != nil {
		t.Fatalf("supervisorStatusDaemons: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("snapshot taken %d times on error path, want 1", *calls)
	}
	if len(rows) != n {
		t.Fatalf("rows len = %d, want %d", len(rows), n)
	}
	for _, row := range rows {
		if row["state"] != "Running" {
			t.Fatalf("daemon %v resolved %q on snapshot error, want Running (port_owner_unverified is observe-only): %+v", row["task_name"], row["state"], row)
		}
		if _, hasStale := row["stale_pid"]; hasStale {
			t.Fatalf("daemon %v carried stale_pid on a probe error, want none: %+v", row["task_name"], row)
		}
	}
}

// TestSupervisorStatusSnapshotOwnerMismatchFlipsRestarting asserts a daemon
// whose port is owned by a DIFFERENT live PID in the shared snapshot flips to
// Restarting and surfaces stale_pid — the port_owner_mismatch path. Other
// daemons whose ports map to their own tracked PID stay Running. Still ONE
// snapshot for the whole refresh.
func TestSupervisorStatusSnapshotOwnerMismatchFlipsRestarting(t *testing.T) {
	const n = 3
	stateDir, tracker, meta := seedRunningDaemons(t, n)
	installOwnsPortProbe(t)

	// Pin the supervisor self-PID to a value that cannot collide with any
	// tracked daemon PID (500000+) or the foreign owner (pid+999999) so the
	// squatted port resolves port_owner_mismatch deterministically, never the
	// port_owner_self branch.
	prevSelf := supervisorSelfPIDFn
	supervisorSelfPIDFn = func() int { return 1 }
	t.Cleanup(func() { supervisorSelfPIDFn = prevSelf })

	// Pick one daemon to be squatted: its port maps to a FOREIGN pid in the
	// snapshot; the rest map to their own tracked pid.
	var squattedTask string
	var squattedPID, squattedPort int
	snap := map[int]int{}
	for task, m := range meta {
		if squattedTask == "" {
			squattedTask = task
			squattedPID = m.pid
			squattedPort = m.port
			snap[m.port] = m.pid + 999999 // foreign owner
			continue
		}
		snap[m.port] = m.pid
	}
	calls := withFakePortOwnersSnapshot(t, func() (map[int]int, error) { return snap, nil })

	rows, err := supervisorStatusDaemons(stateDir, tracker)
	if err != nil {
		t.Fatalf("supervisorStatusDaemons: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("snapshot taken %d times, want 1", *calls)
	}

	var sawSquatted bool
	for _, row := range rows {
		canonTask := row["task_name"].(string)
		if canonTask == squattedTask {
			sawSquatted = true
			if row["state"] != "Restarting" {
				t.Fatalf("squatted daemon %q (port %d owned by foreign pid) resolved %q, want Restarting: %+v", squattedTask, squattedPort, row["state"], row)
			}
			if got := row["stale_pid"]; got != squattedPID {
				t.Fatalf("squatted daemon stale_pid = %v, want tracked pid %d: %+v", got, squattedPID, row)
			}
			if row["current_pid"] != 0 {
				t.Fatalf("squatted daemon current_pid = %v, want 0 (zeroed on Restarting): %+v", row["current_pid"], row)
			}
			continue
		}
		if row["state"] != "Running" {
			t.Fatalf("non-squatted daemon %q resolved %q, want Running: %+v", canonTask, row["state"], row)
		}
	}
	if !sawSquatted {
		t.Fatalf("squatted task %q not found in rows", squattedTask)
	}
}

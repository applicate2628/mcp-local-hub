package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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
// daemons (ports preset so the status producer's port resolver short-circuits on
// Port>0 and never reaches the manifest / the registry) and returns the stateDir,
// the populated tracker, and a task -> (pid, port) map for assertions. StartedAt is
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

	rows, err := supervisorStatusDaemons(stateDir, tracker, nil)
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

	rows, err := supervisorStatusDaemons(stateDir, tracker, nil)
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

	rows, err := supervisorStatusDaemons(stateDir, tracker, nil)
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

func TestStatusPortOwnersCoalescerCoalescesWithinTTL(t *testing.T) {
	var calls int32
	var gen atomic.Uint64
	now := time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC)
	c := &statusPortOwnersCoalescer{
		snapshotFn: func(context.Context) (map[int]int, error) {
			atomic.AddInt32(&calls, 1)
			return map[int]int{9123: 1234}, nil
		},
		genFn:   gen.Load,
		nowFn:   func() time.Time { return now },
		timeout: time.Second,
	}

	for i := 0; i < 5; i++ {
		snap, err := c.Get()
		if err != nil {
			t.Fatalf("Get #%d: %v", i, err)
		}
		if snap[9123] != 1234 {
			t.Fatalf("Get #%d snapshot = %+v, want port 9123 owner 1234", i, snap)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("snapshotFn calls = %d, want 1 within TTL", got)
	}
}

func TestStatusPortOwnersCoalescerSingleflightsConcurrentMiss(t *testing.T) {
	const callers = 16
	var calls int32
	var gen atomic.Uint64
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	now := time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC)
	want := map[int]int{9123: 1234}
	c := &statusPortOwnersCoalescer{
		snapshotFn: func(context.Context) (map[int]int, error) {
			atomic.AddInt32(&calls, 1)
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return want, nil
		},
		genFn:   gen.Load,
		nowFn:   func() time.Time { return now },
		timeout: time.Second,
	}

	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap, err := c.Get()
			if err != nil {
				errs <- err
				return
			}
			if snap[9123] != 1234 {
				errs <- fmt.Errorf("snapshot = %+v, want port 9123 owner 1234", snap)
			}
		}()
	}
	<-started
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("snapshotFn calls = %d, want 1 for concurrent miss", got)
	}
}

func TestStatusPortOwnersCoalescerSlowProbeCachesAtCompletionForWaiters(t *testing.T) {
	const callers = 8
	var calls int32
	var gen atomic.Uint64
	var nowCalls int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	base := time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC)
	c := &statusPortOwnersCoalescer{
		snapshotFn: func(context.Context) (map[int]int, error) {
			atomic.AddInt32(&calls, 1)
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return map[int]int{9123: 1234}, nil
		},
		genFn: gen.Load,
		nowFn: func() time.Time {
			if atomic.AddInt32(&nowCalls, 1) == 1 {
				return base
			}
			return base.Add(statusPortOwnersTTL + time.Second)
		},
		timeout: time.Second,
	}

	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap, err := c.Get()
			if err != nil {
				t.Errorf("Get: %v", err)
				return
			}
			if snap[9123] != 1234 {
				t.Errorf("snapshot = %+v, want port 9123 owner 1234", snap)
			}
		}()
	}
	<-started
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("snapshotFn calls = %d, want 1 even when the first probe spans the TTL", got)
	}
}

func TestStatusPortOwnersCoalescerRefreshesAfterTTL(t *testing.T) {
	var calls int32
	var gen atomic.Uint64
	now := time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC)
	c := &statusPortOwnersCoalescer{
		snapshotFn: func(context.Context) (map[int]int, error) {
			call := int(atomic.AddInt32(&calls, 1))
			return map[int]int{9123: call}, nil
		},
		genFn:   gen.Load,
		nowFn:   func() time.Time { return now },
		timeout: time.Second,
	}

	snap, err := c.Get()
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if snap[9123] != 1 {
		t.Fatalf("first snapshot = %+v, want call marker 1", snap)
	}
	now = now.Add(statusPortOwnersTTL + time.Nanosecond)
	snap, err = c.Get()
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if snap[9123] != 2 {
		t.Fatalf("second snapshot = %+v, want refreshed call marker 2", snap)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("snapshotFn calls = %d, want 2 after TTL", got)
	}
}

// A fleet-generation change must force the very next Get to re-probe even WITHIN
// the TTL — the read-your-writes guarantee for any respawn path (manual or
// automatic): status must not combine fresh tracker state with a pre-respawn
// owner map.
func TestStatusPortOwnersCoalescerRefreshesWithinTTLWhenGenerationChanges(t *testing.T) {
	var calls int32
	var gen atomic.Uint64
	now := time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC)
	c := &statusPortOwnersCoalescer{
		snapshotFn: func(context.Context) (map[int]int, error) {
			call := int(atomic.AddInt32(&calls, 1))
			return map[int]int{9123: call}, nil
		},
		genFn:   gen.Load,
		nowFn:   func() time.Time { return now },
		timeout: time.Second,
	}

	if snap, err := c.Get(); err != nil || snap[9123] != 1 {
		t.Fatalf("first Get = (%+v, %v), want call marker 1", snap, err)
	}
	// Still within the TTL (now unchanged): only the generation bump should force
	// the fresh probe.
	gen.Add(1)
	snap, err := c.Get()
	if err != nil {
		t.Fatalf("post-generation-bump Get: %v", err)
	}
	if snap[9123] != 2 {
		t.Fatalf("post-generation-bump snapshot = %+v, want a fresh re-probe (marker 2) despite same-TTL", snap)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("snapshotFn calls = %d, want 2 (generation bump forced a re-probe within TTL)", got)
	}
}

func TestStatusPortOwnersCoalescerCachesErrorAndReprobesAfterTTL(t *testing.T) {
	var calls int32
	var gen atomic.Uint64
	now := time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC)
	c := &statusPortOwnersCoalescer{
		snapshotFn: func(context.Context) (map[int]int, error) {
			call := atomic.AddInt32(&calls, 1)
			return nil, fmt.Errorf("netstat failed %d", call)
		},
		genFn:   gen.Load,
		nowFn:   func() time.Time { return now },
		timeout: time.Second,
	}

	_, err1 := c.Get()
	_, err2 := c.Get()
	if err1 == nil || err2 == nil {
		t.Fatalf("cached error path returned err1=%v err2=%v, want errors", err1, err2)
	}
	if err1.Error() != err2.Error() {
		t.Fatalf("within-TTL errors differ: %q vs %q", err1, err2)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("snapshotFn calls within TTL = %d, want 1", got)
	}

	now = now.Add(statusPortOwnersTTL + time.Nanosecond)
	_, err3 := c.Get()
	if err3 == nil || err3.Error() == err1.Error() {
		t.Fatalf("after TTL error = %v, want fresh reprobe error distinct from %v", err3, err1)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("snapshotFn calls after TTL = %d, want 2", got)
	}
}

func TestStatusPortOwnersCoalescerBoundsSnapshotWithContext(t *testing.T) {
	c := &statusPortOwnersCoalescer{
		snapshotFn: func(ctx context.Context) (map[int]int, error) {
			<-ctx.Done()
			return nil, fmt.Errorf("snapshot stopped: %w", ctx.Err())
		},
		nowFn:   func() time.Time { return time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC) },
		timeout: 20 * time.Millisecond,
	}

	start := time.Now()
	_, err := c.Get()
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Get err = %v, want context deadline exceeded", err)
	}
	if elapsed > time.Second {
		t.Fatalf("Get took %s, want bounded by coalescer timeout", elapsed)
	}
}

func TestSupervisorStatusDaemonsFailLoudAndSnapshotErrorWithCoalescer(t *testing.T) {
	t.Run("intent read failure is still top-level error", func(t *testing.T) {
		stateDir := apitest.HardenedTempDir(t)
		if err := os.Mkdir(filepath.Join(stateDir, "supervisor-intent.json"), 0o700); err != nil {
			t.Fatalf("mkdir supervisor-intent.json directory: %v", err)
		}
		c := &statusPortOwnersCoalescer{
			snapshotFn: func(context.Context) (map[int]int, error) {
				t.Fatal("snapshotFn must not run when intent read fails")
				return nil, nil
			},
			nowFn:   func() time.Time { return time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC) },
			timeout: time.Second,
		}

		rows, err := supervisorStatusDaemons(stateDir, NewDaemonRuntimeTracker(), c)
		if err == nil {
			t.Fatalf("supervisorStatusDaemons rows=%+v err=nil, want intent read error", rows)
		}
	})

	t.Run("snapshot error keeps rows and no top-level error", func(t *testing.T) {
		stateDir, tracker, _ := seedRunningDaemons(t, 1)
		installOwnsPortProbe(t)
		c := &statusPortOwnersCoalescer{
			snapshotFn: func(context.Context) (map[int]int, error) {
				return nil, errors.New("netstat deadline exceeded")
			},
			nowFn:   func() time.Time { return time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC) },
			timeout: time.Second,
		}

		rows, err := supervisorStatusDaemons(stateDir, tracker, c)
		if err != nil {
			t.Fatalf("supervisorStatusDaemons returned top-level err on snapshot failure: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("rows len = %d, want 1", len(rows))
		}
		if rows[0]["state"] != "Running" {
			t.Fatalf("snapshot error row state = %v, want Running via port_owner_unverified: %+v", rows[0]["state"], rows[0])
		}
		if _, hasStale := rows[0]["stale_pid"]; hasStale {
			t.Fatalf("snapshot error row carried stale_pid, want none: %+v", rows[0])
		}
	})
}

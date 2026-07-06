package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// bindDeadlineTestHarness spins up a sweep-ready in-memory fixture: an intent
// with one daemon, a tracker, and a real event loop whose handler records posted
// events on a channel. The caller injects the liveness probe and drives
// sweepSupervisorLivenessOnce with its own bindLatch to exercise the P1b
// first-bind-deadline logic.
type bindDeadlineFixture struct {
	stateDir string
	intent   *api.SupervisorIntentFile
	tracker  *DaemonRuntimeTracker
	loop     *api.EventLoop
	events   chan api.LoopEvent
	taskName string
}

func newBindDeadlineFixture(t *testing.T, d api.SupervisorDaemon) *bindDeadlineFixture {
	t.Helper()
	stateDir := apitest.HardenedTempDir(t)
	intent := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{d}}
	tracker := NewDaemonRuntimeTracker()
	loop := api.NewEventLoop(16)
	events := make(chan api.LoopEvent, 4)
	loop.RegisterHandler(func(e api.LoopEvent) { events <- e })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go loop.Run(ctx)
	return &bindDeadlineFixture{
		stateDir: stateDir,
		intent:   intent,
		tracker:  tracker,
		loop:     loop,
		events:   events,
		taskName: canonicalSupervisorTaskName(d.TaskName),
	}
}

// expectNoEvent asserts no loop event is posted within a short window.
func (f *bindDeadlineFixture) expectNoEvent(t *testing.T, msg string) {
	t.Helper()
	select {
	case ev := <-f.events:
		t.Fatalf("%s: unexpected event %+v", msg, ev)
	case <-time.After(300 * time.Millisecond):
	}
}

// expectManualRestart asserts an EvManualRestart is posted for the task.
func (f *bindDeadlineFixture) expectManualRestart(t *testing.T) api.LoopEvent {
	t.Helper()
	select {
	case ev := <-f.events:
		if ev.Kind != api.EvManualRestart || ev.TaskName != f.taskName {
			t.Fatalf("event = %+v, want EvManualRestart for %s", ev, f.taskName)
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("sweep did not post EvManualRestart")
		return api.LoopEvent{}
	}
}

// TestSweep_PortUnboundWithinStartupDeadlineNoRestart is spec test 10: a
// freshly-spawned daemon whose port is not yet bound, well within its startup
// deadline (120s), with no prior latch → the sweep must NOT restart it.
func TestSweep_PortUnboundWithinStartupDeadlineNoRestart(t *testing.T) {
	// serena-proxy descriptor → 120s deadline (explicit field also 120).
	d := api.SupervisorDaemon{
		TaskName:                   `\mcp-local-hub-serena-abc`,
		Server:                     "serena",
		Daemon:                     "abc",
		Port:                       9151,
		Args:                       []string{"daemon", "serena-proxy"},
		StartupBindDeadlineSeconds: 120,
	}
	f := newBindDeadlineFixture(t, d)
	f.tracker.MarkSpawned(d.TaskName, 22036, time.Now().UTC().Add(-30*time.Second)) // 30s < 120s

	probe := supervisorLivenessProbe{
		PIDAlive:     func(int) bool { return true },
		PortOwnerPID: func(int) (int, bool, error) { return 0, false, nil }, // unbound
	}
	restore := setSupervisorLivenessProbeForTest(probe)
	defer restore()

	sweepSupervisorLivenessOnce(f.stateDir, f.intent, f.tracker, f.loop, nil, map[string]int{}, nil, nil)
	f.expectNoEvent(t, "unbound within startup deadline")
}

// TestSweep_PortUnboundPastStartupDeadlineRestartsAndEmitsBindTimeout is spec
// test 11: an unbound daemon PAST its startup deadline (StartedAt=now-121s,
// deadline 120s) → the sweep restarts it AND emits daemon-bind-timeout.
func TestSweep_PortUnboundPastStartupDeadlineRestartsAndEmitsBindTimeout(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(stateDir, "supervisor-events.log")
	auditLog, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer auditLog.Close()

	d := api.SupervisorDaemon{
		TaskName:                   `\mcp-local-hub-serena-abc`,
		Server:                     "serena",
		Daemon:                     "abc",
		Port:                       9151,
		Args:                       []string{"daemon", "serena-proxy"},
		StartupBindDeadlineSeconds: 120,
	}
	intent := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{d}}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(d.TaskName, 22036, time.Now().UTC().Add(-121*time.Second)) // past 120s
	loop := api.NewEventLoop(16)
	events := make(chan api.LoopEvent, 4)
	loop.RegisterHandler(func(e api.LoopEvent) { events <- e })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	probe := supervisorLivenessProbe{
		PIDAlive:     func(int) bool { return true },
		PortOwnerPID: func(int) (int, bool, error) { return 0, false, nil }, // unbound
	}
	restore := setSupervisorLivenessProbeForTest(probe)
	defer restore()

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, auditLog, map[string]int{}, nil, nil)

	select {
	case ev := <-events:
		if ev.Kind != api.EvManualRestart {
			t.Fatalf("event = %+v, want EvManualRestart", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("past-deadline unbound did not restart")
	}

	body := waitForEventMarker(t, eventsPath, "daemon-bind-timeout", 2*time.Second)
	if !strings.Contains(body, "daemon-bind-timeout") {
		t.Fatalf("daemon-bind-timeout not emitted; log:\n%s", body)
	}
	// The deadline_seconds field must reflect the resolved 120s.
	if !strings.Contains(body, `"deadline_seconds":120`) {
		t.Fatalf("daemon-bind-timeout missing deadline_seconds:120; log:\n%s", body)
	}
}

// TestSweep_PostFirstBindUnboundRestartsAt5s is spec test 12: after the port has
// been observed bound by the current PID (sweep 1 latches), a subsequent unbound
// port past the 5s post-bind grace (but well within the startup deadline) DOES
// restart — the post-bind rule applies once latched.
func TestSweep_PostFirstBindUnboundRestartsAt5s(t *testing.T) {
	d := api.SupervisorDaemon{
		TaskName:                   `\mcp-local-hub-serena-abc`,
		Server:                     "serena",
		Daemon:                     "abc",
		Port:                       9151,
		Args:                       []string{"daemon", "serena-proxy"},
		StartupBindDeadlineSeconds: 120,
	}
	f := newBindDeadlineFixture(t, d)
	// StartedAt 30s ago: within the 120s startup deadline but PAST the 5s
	// post-bind grace, so once latched the unbound port restarts.
	f.tracker.MarkSpawned(d.TaskName, 22036, time.Now().UTC().Add(-30*time.Second))

	latch := map[string]int{}

	// Sweep 1: port owned by the current PID → live, latches the bind.
	boundProbe := supervisorLivenessProbe{
		PIDAlive:     func(int) bool { return true },
		PortOwnerPID: func(int) (int, bool, error) { return 22036, true, nil }, // owned by current PID
	}
	restore1 := setSupervisorLivenessProbeForTest(boundProbe)
	sweepSupervisorLivenessOnce(f.stateDir, f.intent, f.tracker, f.loop, nil, latch, nil, nil)
	restore1()
	f.expectNoEvent(t, "sweep 1 bound is live")
	if latch[f.taskName] != 1 {
		t.Fatalf("bind latch = %v, want generation 1 latched after first bind", latch)
	}

	// Sweep 2: port now UNBOUND. Latched at the current generation → the 5s
	// post-bind grace applies; StartedAt is 30s ago (> 5s) → restart fires.
	unboundProbe := supervisorLivenessProbe{
		PIDAlive:     func(int) bool { return true },
		PortOwnerPID: func(int) (int, bool, error) { return 0, false, nil },
	}
	restore2 := setSupervisorLivenessProbeForTest(unboundProbe)
	defer restore2()
	sweepSupervisorLivenessOnce(f.stateDir, f.intent, f.tracker, f.loop, nil, latch, nil, nil)
	f.expectManualRestart(t)
}

// TestSweep_BindLatchResetsOnNewGeneration is spec test 13: a latch set at
// generation 1 must NOT carry over to generation 2 — after a respawn bumps the
// generation, the startup deadline re-applies so an unbound port within the
// deadline does NOT restart.
func TestSweep_BindLatchResetsOnNewGeneration(t *testing.T) {
	d := api.SupervisorDaemon{
		TaskName:                   `\mcp-local-hub-serena-abc`,
		Server:                     "serena",
		Daemon:                     "abc",
		Port:                       9151,
		Args:                       []string{"daemon", "serena-proxy"},
		StartupBindDeadlineSeconds: 120,
	}
	f := newBindDeadlineFixture(t, d)
	// Pre-latch generation 1 manually (as if a prior sweep had observed a bind).
	f.tracker.MarkSpawned(d.TaskName, 22036, time.Now().UTC().Add(-30*time.Second))
	latch := map[string]int{f.taskName: 1}

	// Respawn → generation 2, fresh StartedAt.
	f.tracker.MarkSpawned(d.TaskName, 33000, time.Now().UTC())

	unboundProbe := supervisorLivenessProbe{
		PIDAlive:     func(int) bool { return true },
		PortOwnerPID: func(int) (int, bool, error) { return 0, false, nil },
	}
	restore := setSupervisorLivenessProbeForTest(unboundProbe)
	defer restore()

	// The latch is stale (gen1 != gen2), so the startup deadline re-applies. The
	// fresh gen2 StartedAt is well within 120s → no restart.
	sweepSupervisorLivenessOnce(f.stateDir, f.intent, f.tracker, f.loop, nil, latch, nil, nil)
	f.expectNoEvent(t, "unbound within re-applied startup deadline after new generation")
}

// TestSupervisorStartupBindDeadline_Resolution is spec test 14: 0 → 60s;
// serena-proxy descriptor → 120s; explicit 300 → 300s.
func TestSupervisorStartupBindDeadline_Resolution(t *testing.T) {
	cases := []struct {
		name string
		d    api.SupervisorDaemon
		want time.Duration
	}{
		{
			name: "global default zero",
			d:    api.SupervisorDaemon{TaskName: `\mcp-local-hub-memory-default`, Args: []string{"daemon", "--server", "memory", "--daemon", "default"}},
			want: 60 * time.Second,
		},
		{
			// §4b: a field-0 serena-proxy pool row (real rows carry Server/Daemon +
			// --server in args) resolves serena by SERVER IDENTITY → 120s, covering
			// the workspace-hash daemon name the manifest never declares. This is the
			// #234→#488 population the old argv-arm gave 120s and §4a would have
			// dropped to 60s.
			name: "serena proxy zero field (workspace-hash identity)",
			d: api.SupervisorDaemon{TaskName: `\mcp-local-hub-serena-abc`, Server: "serena", Daemon: "6935d24c",
				Args: []string{"daemon", "serena-proxy", "--server", "serena", "--workspace", "d:\\dev\\x", "--port", "9150", "--task-name", `\mcp-local-hub-serena-abc`}},
			want: 120 * time.Second,
		},
		{
			// legacy-unified serena (args `daemon --server serena --daemon unified`,
			// field 0): the old argv-shape arm MISSED this (args[1] != serena-proxy)
			// and gave 60s — the PR #504 regression. §4b's server-identity keying
			// gives it 120s.
			name: "legacy-unified serena identity",
			d: api.SupervisorDaemon{TaskName: `\mcp-local-hub-serena-unified`, Server: "serena", Daemon: "unified",
				Args: []string{"daemon", "--server", "serena", "--daemon", "unified"}},
			want: 120 * time.Second,
		},
		{
			name: "explicit field wins",
			d:    api.SupervisorDaemon{TaskName: `\mcp-local-hub-serena-abc`, Server: "serena", Daemon: "abc", Args: []string{"daemon", "serena-proxy"}, StartupBindDeadlineSeconds: 300},
			want: 300 * time.Second,
		},
		{
			name: "explicit field on global",
			d:    api.SupervisorDaemon{TaskName: `\mcp-local-hub-memory-default`, StartupBindDeadlineSeconds: 45},
			want: 45 * time.Second,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := supervisorStartupBindDeadline(tc.d); got != tc.want {
				t.Fatalf("supervisorStartupBindDeadline = %v, want %v", got, tc.want)
			}
		})
	}
}

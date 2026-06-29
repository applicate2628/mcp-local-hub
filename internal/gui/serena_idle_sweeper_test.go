package gui

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// withIdleSweeperSeams wires the threshold + stop seams and a backend status
// reader, restoring all three on cleanup. capturedStops receives every
// task_name the sweeper idle-stops.
func withIdleSweeperSeams(t *testing.T, threshold time.Duration, enabled bool, status func(context.Context) ([]api.DaemonStatus, error)) *[]string {
	t.Helper()
	origThresh, origStop, origStatus := serenaIdleThresholdFn, serenaIdleStopFn, serenaBackendStatusFn
	captured := &[]string{}
	serenaIdleThresholdFn = func() (time.Duration, bool) { return threshold, enabled }
	serenaIdleStopFn = func(taskName string, _ time.Time) (bool, error) {
		*captured = append(*captured, taskName)
		return true, nil
	}
	serenaBackendStatusFn = status
	t.Cleanup(func() {
		serenaIdleThresholdFn, serenaIdleStopFn, serenaBackendStatusFn = origThresh, origStop, origStatus
	})
	return captured
}

// serenaSweeperServer builds a router server with a single serena workspace
// (sentinel-language, serena backend) and the lister/resolver wired.
func serenaSweeperServer(t *testing.T, ws *api.WorkspaceEntry) *Server {
	t.Helper()
	deps := &serenaRouterDeps{
		Resolver:      &listerStubResolver{stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}}, list: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(*api.WorkspaceEntry) string { return "http://unused" },
		AuditFn:       func(string, string, map[string]any) error { return nil },
		// WakeIdleFn deliberately nil — these tests exercise the SWEEPER, not
		// the handler wake.
	}
	return newSerenaTestServer(t, deps)
}

func serenaWS(key, path string, port int) *api.WorkspaceEntry {
	return &api.WorkspaceEntry{
		WorkspaceKey:  key,
		WorkspacePath: path,
		Port:          port,
		Backend:       "serena",
		Language:      api.SerenaLanguageSentinel,
		TaskName:      `\mcp-local-hub-serena-` + key,
	}
}

// FALSIFICATION (spec §6): with the idle threshold at 1 minute, a daemon that
// just had activity recorded (the "mid-call / recently-active" case) is NOT
// idle-stopped. The sweeper reads LAST-ACTIVITY, not wall-clock since spawn.
func TestIdleSweeper_RecentlyActive_NotKilled(t *testing.T) {
	const wsPath = "/proj/alpha"
	ws := serenaWS("alpha", wsPath, 9201)
	s := serenaSweeperServer(t, ws)

	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	// The daemon has a LONG uptime (2h) but activity 10s ago — last-activity is
	// the baseline, so it is NOT idle despite the long uptime.
	captured := withIdleSweeperSeams(t, time.Minute, true,
		func(context.Context) ([]api.DaemonStatus, error) {
			return []api.DaemonStatus{{Server: "serena", Workspace: wsPath, State: "Running", PID: 1000, Port: 9201, UptimeSec: 7200}}, nil
		},
	)
	s.recordSerenaActivity("alpha", now.Add(-10*time.Second))

	if n := s.SweepIdleSerenaDaemons(context.Background(), now); n != 0 {
		t.Fatalf("recently-active daemon was idle-stopped (%d); the sweeper must read last-activity, not uptime-since-spawn", n)
	}
	if len(*captured) != 0 {
		t.Fatalf("recently-active daemon must NOT be stopped; captured=%v", *captured)
	}
}

// A daemon idle longer than the threshold (last-activity well in the past) IS
// idle-stopped via the unified-intent stop writer.
func TestIdleSweeper_Idle_Stopped(t *testing.T) {
	const wsPath = "/proj/beta"
	ws := serenaWS("beta", wsPath, 9202)
	s := serenaSweeperServer(t, ws)

	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	captured := withIdleSweeperSeams(t, 30*time.Minute, true,
		func(context.Context) ([]api.DaemonStatus, error) {
			return []api.DaemonStatus{{Server: "serena", Workspace: wsPath, State: "Running", PID: 1000, Port: 9202, UptimeSec: 7200}}, nil
		},
	)
	// Last activity 45 minutes ago — past the 30m threshold.
	s.recordSerenaActivity("beta", now.Add(-45*time.Minute))

	if n := s.SweepIdleSerenaDaemons(context.Background(), now); n != 1 {
		t.Fatalf("idle daemon must be stopped; got n=%d", n)
	}
	if len(*captured) != 1 || (*captured)[0] != `\mcp-local-hub-serena-beta` {
		t.Fatalf("expected idle stop for \\mcp-local-hub-serena-beta; captured=%v", *captured)
	}
	// The activity baseline must be dropped after the idle-stop.
	if _, ok := s.lastSerenaActivity("beta"); ok {
		t.Fatalf("activity baseline must be dropped after idle-stop")
	}
}

// "off" disables idle-shutdown: nothing is stopped even when long-idle.
func TestIdleSweeper_Off_Disabled(t *testing.T) {
	const wsPath = "/proj/gamma"
	ws := serenaWS("gamma", wsPath, 9203)
	s := serenaSweeperServer(t, ws)

	now := time.Now().UTC()
	captured := withIdleSweeperSeams(t, 0, false, // "off"
		func(context.Context) ([]api.DaemonStatus, error) {
			return []api.DaemonStatus{{Server: "serena", Workspace: wsPath, State: "Running", PID: 1000, Port: 9203, UptimeSec: 99999}}, nil
		},
	)
	s.recordSerenaActivity("gamma", now.Add(-99*time.Hour))

	if n := s.SweepIdleSerenaDaemons(context.Background(), now); n != 0 {
		t.Fatalf("off must disable idle-shutdown; got n=%d", n)
	}
	if len(*captured) != 0 {
		t.Fatalf("off must stop nothing; captured=%v", *captured)
	}
}

// A freshly-spawned daemon with NO recorded activity is NOT idled until its
// uptime exceeds the threshold (the never-called fallback baseline).
func TestIdleSweeper_NeverCalled_FreshUptime_NotKilled(t *testing.T) {
	const wsPath = "/proj/delta"
	ws := serenaWS("delta", wsPath, 9204)
	s := serenaSweeperServer(t, ws)

	now := time.Now().UTC()
	// Uptime 20s, threshold 1m, NO activity recorded → not idle yet.
	captured := withIdleSweeperSeams(t, time.Minute, true,
		func(context.Context) ([]api.DaemonStatus, error) {
			return []api.DaemonStatus{{Server: "serena", Workspace: wsPath, State: "Running", PID: 1000, Port: 9204, UptimeSec: 20}}, nil
		},
	)
	if n := s.SweepIdleSerenaDaemons(context.Background(), now); n != 0 {
		t.Fatalf("freshly-spawned daemon (uptime < threshold, no activity) must not be idled; got n=%d", n)
	}
	if len(*captured) != 0 {
		t.Fatalf("fresh daemon must not be stopped; captured=%v", *captured)
	}
}

// A never-called daemon whose uptime exceeds the threshold IS idled (uptime
// fallback baseline) — provided the GUI itself has been up past the threshold
// (the GUI-restart baseline cap, FIX-1; here the GUI started 1h ago).
func TestIdleSweeper_NeverCalled_LongUptime_Killed(t *testing.T) {
	const wsPath = "/proj/epsilon"
	ws := serenaWS("epsilon", wsPath, 9205)
	s := serenaSweeperServer(t, ws)

	now := time.Now().UTC()
	// GUI has been up 1h (> the 1m threshold), so the GUI-restart cap does not
	// suppress the idle-kill of a long-uptime never-called daemon.
	s.guiProcessStart = now.Add(-time.Hour)
	captured := withIdleSweeperSeams(t, time.Minute, true,
		func(context.Context) ([]api.DaemonStatus, error) {
			return []api.DaemonStatus{{Server: "serena", Workspace: wsPath, State: "Running", PID: 1000, Port: 9205, UptimeSec: 600}}, nil
		},
	)
	if n := s.SweepIdleSerenaDaemons(context.Background(), now); n != 1 {
		t.Fatalf("never-called daemon up 10m past a 1m threshold must be idled; got n=%d", n)
	}
	if len(*captured) != 1 {
		t.Fatalf("expected one idle stop; captured=%v", *captured)
	}
}

// FIX-1 (fable's coupled-hazard insight): after a GUI RESTART, a daemon with a
// LONG supervisor uptime (3h) that was active just before the restart must NOT
// be idle-killed on the first post-restart sweep. The restart wiped
// serenaLastActivity, so the sweeper falls back to uptime — but the
// GUI-restart baseline caps idle at time-since-GUI-start. With the GUI up only
// 30s and a 1m threshold, no daemon is idled even though uptime is 3h. Without
// the cap, real uptime (now populated by FIX-1's IPC decode) would kill it.
func TestIdleSweeper_GUIRestartBaseline_LongUptimeNotKilled(t *testing.T) {
	const wsPath = "/proj/restart"
	ws := serenaWS("restart", wsPath, 9210)
	s := serenaSweeperServer(t, ws)

	now := time.Now().UTC()
	// The GUI just restarted 30s ago; the activity map is empty (simulating the
	// wipe). The daemon has a 3h supervisor uptime.
	s.guiProcessStart = now.Add(-30 * time.Second)
	captured := withIdleSweeperSeams(t, time.Minute, true,
		func(context.Context) ([]api.DaemonStatus, error) {
			return []api.DaemonStatus{{Server: "serena", Workspace: wsPath, State: "Running", PID: 1000, Port: 9210, UptimeSec: 3 * 3600}}, nil
		},
	)
	if n := s.SweepIdleSerenaDaemons(context.Background(), now); n != 0 {
		t.Fatalf("a 3h-uptime daemon must NOT be idle-killed when the GUI restarted 30s ago (threshold 1m); got n=%d — the GUI-restart baseline is missing", n)
	}
	if len(*captured) != 0 {
		t.Fatalf("GUI-restart baseline must keep the daemon alive; captured=%v", *captured)
	}

	// Once the GUI has been up PAST the threshold (here 2m), the same
	// never-called long-uptime daemon IS idled — the cap no longer binds.
	s.guiProcessStart = now.Add(-2 * time.Minute)
	if n := s.SweepIdleSerenaDaemons(context.Background(), now); n != 1 {
		t.Fatalf("after the GUI has been up past the threshold, the long-uptime never-called daemon must be idled; got n=%d", n)
	}
}

// FIX-3 (mid-call protection): a daemon with an OPEN /serena/mcp forward is
// NEVER idle-stopped, even when its last-activity is well past the threshold
// (the activity was stamped ONCE at call-start; a single long streaming call
// keeps the in-flight counter incremented). Once the forward completes, the same
// long-idle daemon IS idled.
func TestIdleSweeper_InFlightForward_NotKilledMidCall(t *testing.T) {
	const wsPath = "/proj/inflight"
	ws := serenaWS("inflight", wsPath, 9211)
	s := serenaSweeperServer(t, ws)

	now := time.Now().UTC()
	captured := withIdleSweeperSeams(t, time.Minute, true,
		func(context.Context) ([]api.DaemonStatus, error) {
			return []api.DaemonStatus{{Server: "serena", Workspace: wsPath, State: "Running", PID: 1000, Port: 9211, UptimeSec: 7200}}, nil
		},
	)
	// Activity was stamped at call-start, 45m ago (well past the 1m threshold) —
	// the call is STILL streaming (mid-call), so the in-flight counter is open.
	s.recordSerenaActivity("inflight", now.Add(-45*time.Minute))
	s.enterSerenaForward("inflight")

	if n := s.SweepIdleSerenaDaemons(context.Background(), now); n != 0 {
		t.Fatalf("a daemon with an OPEN forward must NOT be idle-killed mid-call; got n=%d", n)
	}
	if len(*captured) != 0 {
		t.Fatalf("mid-call daemon must not be stopped; captured=%v", *captured)
	}

	// The call completes: the forward closes AND last-activity is re-stamped to
	// now (the production defer does both). The daemon is no longer mid-call and
	// its activity is fresh → still not idled.
	s.exitSerenaForward("inflight")
	s.recordSerenaActivity("inflight", now)
	if n := s.SweepIdleSerenaDaemons(context.Background(), now); n != 0 {
		t.Fatalf("a just-finished call re-stamps activity, so the daemon must not be immediately idled; got n=%d", n)
	}

	// Time advances past the threshold with no further activity → NOW it idles.
	later := now.Add(2 * time.Minute)
	if n := s.SweepIdleSerenaDaemons(context.Background(), later); n != 1 {
		t.Fatalf("after the forward completes AND the threshold elapses, the daemon must be idled; got n=%d", n)
	}
	if len(*captured) != 1 {
		t.Fatalf("expected exactly one idle stop after the call finished + threshold; captured=%v", *captured)
	}
}

// FIX-3 counter discipline: nested forwards (two concurrent calls) keep the
// daemon protected until BOTH complete; the counter never goes negative.
func TestIdleSweeper_InFlightCounter_Balanced(t *testing.T) {
	const wsPath = "/proj/counter"
	ws := serenaWS("counter", wsPath, 9212)
	s := serenaSweeperServer(t, ws)

	now := time.Now().UTC()
	captured := withIdleSweeperSeams(t, time.Minute, true,
		func(context.Context) ([]api.DaemonStatus, error) {
			return []api.DaemonStatus{{Server: "serena", Workspace: wsPath, State: "Running", PID: 1000, Port: 9212, UptimeSec: 7200}}, nil
		},
	)
	s.recordSerenaActivity("counter", now.Add(-45*time.Minute))

	// Two concurrent forwards open.
	s.enterSerenaForward("counter")
	s.enterSerenaForward("counter")
	if !s.hasSerenaForwardInFlight("counter") {
		t.Fatalf("two opens must register in-flight")
	}
	// One closes — still protected.
	s.exitSerenaForward("counter")
	if !s.hasSerenaForwardInFlight("counter") {
		t.Fatalf("one of two forwards still open must keep the daemon protected")
	}
	if n := s.SweepIdleSerenaDaemons(context.Background(), now); n != 0 {
		t.Fatalf("a daemon with one forward still open must not be idled; got n=%d", n)
	}
	// Second closes — no longer in-flight.
	s.exitSerenaForward("counter")
	if s.hasSerenaForwardInFlight("counter") {
		t.Fatalf("after both forwards close, the daemon must not be in-flight")
	}
	// A defensive extra exit must not drive the count negative / re-create state.
	s.exitSerenaForward("counter")
	if s.hasSerenaForwardInFlight("counter") {
		t.Fatalf("an extra exit must stay a no-op (count never negative)")
	}
	// With no forward open and stale activity, the daemon now idles.
	if n := s.SweepIdleSerenaDaemons(context.Background(), now); n != 1 {
		t.Fatalf("after all forwards close, the long-idle daemon must idle; got n=%d", n)
	}
	if len(*captured) != 1 {
		t.Fatalf("expected one idle stop; captured=%v", *captured)
	}
}

func TestSerenaStopGate_PrunesIdleEntryAfterLastExit(t *testing.T) {
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})

	s.enterSerenaForward("zero-count")
	s.exitSerenaForward("zero-count")

	s.serenaStopGate.mu.Lock()
	_, exists := s.serenaStopGate.byWorkspace["zero-count"]
	size := len(s.serenaStopGate.byWorkspace)
	s.serenaStopGate.mu.Unlock()

	if exists {
		t.Fatalf("stop gate retained zero-count idle entry after last exit; map size=%d", size)
	}
}

func TestSerenaStopGate_WaitingForwardBlocksNewPhase(t *testing.T) {
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	const wsKey = "waiting-forward"

	if !s.beginSerenaIdleStop(wsKey) {
		t.Fatalf("precondition: could not hold idle-stop gate for %s", wsKey)
	}

	entered := make(chan serenaWorkspaceStopGateEnterResult, 1)
	go func() {
		entered <- s.enterSerenaForwardCtx(context.Background(), wsKey)
	}()
	waitForSerenaStopGateWaiters(t, s, wsKey, 1)

	s.serenaStopGate.mu.Lock()
	entry := s.serenaStopGate.byWorkspace[wsKey]
	if entry == nil {
		s.serenaStopGate.mu.Unlock()
		t.Fatalf("missing stop-gate entry for %s", wsKey)
	}
	entry.phase = serenaWorkspaceStopGatePhaseNone
	s.serenaStopGate.mu.Unlock()

	if s.beginSerenaIdleStop(wsKey) {
		s.endSerenaIdleStop(wsKey)
		releaseWaitingSerenaForward(t, s, wsKey, entered)
		t.Fatalf("beginSerenaIdleStop succeeded while a forward was waiting on the gate")
	}
	if s.beginSerenaPrune(wsKey) {
		s.endSerenaPrune(wsKey)
		releaseWaitingSerenaForward(t, s, wsKey, entered)
		t.Fatalf("beginSerenaPrune succeeded while a forward was waiting on the gate")
	}

	releaseWaitingSerenaForward(t, s, wsKey, entered)

	if !s.beginSerenaIdleStop(wsKey) {
		t.Fatalf("beginSerenaIdleStop should succeed after the waiter enters and exits")
	}
	s.endSerenaIdleStop(wsKey)
	if !s.beginSerenaPrune(wsKey) {
		t.Fatalf("beginSerenaPrune should succeed after the waiter enters and exits")
	}
	s.endSerenaPrune(wsKey)
}

func TestWithSerenaWorkspaceGatePolicies(t *testing.T) {
	type gateResult struct {
		entered bool
		aborted bool
		err     error
	}

	tests := []struct {
		name               string
		policy             serenaWorkspaceGatePolicy
		phase              serenaWorkspaceStopGatePhase
		resolveNil         bool
		onPhaseActiveAbort bool
		wantEntered        bool
		wantAborted        bool
		wantResolveCalls   int
		wantOnPhaseCalls   int
		wantFnCalls        int
	}{
		{
			name:             "block phaseActive rewakes then runs",
			policy:           serenaWorkspaceGatePolicyBlock,
			phase:            serenaWorkspaceStopGatePhaseIdleStop,
			wantEntered:      true,
			wantResolveCalls: 1,
			wantOnPhaseCalls: 1,
			wantFnCalls:      1,
		},
		{
			name:             "block waitedThroughPrune resolve nil aborts",
			policy:           serenaWorkspaceGatePolicyBlock,
			phase:            serenaWorkspaceStopGatePhasePrune,
			resolveNil:       true,
			wantEntered:      true,
			wantAborted:      true,
			wantResolveCalls: 1,
		},
		{
			name:               "tryOnly gated excludes candidate",
			policy:             serenaWorkspaceGatePolicyTryOnly,
			phase:              serenaWorkspaceStopGatePhaseIdleStop,
			onPhaseActiveAbort: true,
			wantAborted:        true,
			wantResolveCalls:   1,
			wantOnPhaseCalls:   1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
			wsKey := "gate-" + strings.ReplaceAll(tc.name, " ", "-")
			ws := serenaWS(wsKey, "/proj/"+wsKey, 9126)
			switch tc.phase {
			case serenaWorkspaceStopGatePhaseIdleStop:
				if !s.beginSerenaIdleStop(wsKey) {
					t.Fatalf("precondition: could not hold idle-stop gate for %s", wsKey)
				}
				defer s.endSerenaIdleStop(wsKey)
			case serenaWorkspaceStopGatePhasePrune:
				if !s.beginSerenaPrune(wsKey) {
					t.Fatalf("precondition: could not hold prune gate for %s", wsKey)
				}
				defer s.endSerenaPrune(wsKey)
			default:
				t.Fatalf("unsupported test phase %d", tc.phase)
			}

			resolveCalls := 0
			onPhaseCalls := 0
			fnCalls := 0
			resolve := func(gotKey string) *api.WorkspaceEntry {
				resolveCalls++
				if gotKey != wsKey {
					t.Fatalf("resolve key = %q, want %q", gotKey, wsKey)
				}
				if tc.resolveNil {
					return nil
				}
				return ws
			}
			urlFn := func(got *api.WorkspaceEntry) string {
				if got == nil {
					return ""
				}
				return "http://127.0.0.1:9126"
			}
			onPhaseActive := func(out *serenaWorkspaceGateOutcome) bool {
				onPhaseCalls++
				if out.ws != ws {
					t.Fatalf("onPhaseActive ws = %#v, want %#v", out.ws, ws)
				}
				if out.upstreamURL == "" {
					t.Fatalf("onPhaseActive missing upstreamURL")
				}
				out.rewoke = true
				return tc.onPhaseActiveAbort
			}
			fn := func(out *serenaWorkspaceGateOutcome) error {
				fnCalls++
				if out.ws != ws {
					t.Fatalf("fn ws = %#v, want %#v", out.ws, ws)
				}
				if out.upstreamURL == "" {
					t.Fatalf("fn missing upstreamURL")
				}
				if tc.wantOnPhaseCalls > 0 && !out.rewoke {
					t.Fatalf("fn saw rewoke=false after onPhaseActive")
				}
				return nil
			}

			var got gateResult
			if tc.policy == serenaWorkspaceGatePolicyBlock {
				done := make(chan gateResult, 1)
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				go func() {
					entered, aborted, err := s.withSerenaWorkspaceGate(ctx, wsKey, tc.policy, resolve, urlFn, onPhaseActive, fn)
					done <- gateResult{entered: entered, aborted: aborted, err: err}
				}()
				waitForSerenaStopGateWaiters(t, s, wsKey, 1)
				switch tc.phase {
				case serenaWorkspaceStopGatePhaseIdleStop:
					s.endSerenaIdleStop(wsKey)
				case serenaWorkspaceStopGatePhasePrune:
					s.endSerenaPrune(wsKey)
				}
				select {
				case got = <-done:
				case <-time.After(2 * time.Second):
					t.Fatalf("withSerenaWorkspaceGate did not return after phase release")
				}
			} else {
				entered, aborted, err := s.withSerenaWorkspaceGate(context.Background(), wsKey, tc.policy, resolve, urlFn, onPhaseActive, fn)
				got = gateResult{entered: entered, aborted: aborted, err: err}
			}

			if got.err != nil {
				t.Fatalf("withSerenaWorkspaceGate err = %v", got.err)
			}
			if got.entered != tc.wantEntered {
				t.Fatalf("entered = %v, want %v", got.entered, tc.wantEntered)
			}
			if got.aborted != tc.wantAborted {
				t.Fatalf("aborted = %v, want %v", got.aborted, tc.wantAborted)
			}
			if resolveCalls != tc.wantResolveCalls {
				t.Fatalf("resolve calls = %d, want %d", resolveCalls, tc.wantResolveCalls)
			}
			if onPhaseCalls != tc.wantOnPhaseCalls {
				t.Fatalf("onPhaseActive calls = %d, want %d", onPhaseCalls, tc.wantOnPhaseCalls)
			}
			if fnCalls != tc.wantFnCalls {
				t.Fatalf("fn calls = %d, want %d", fnCalls, tc.wantFnCalls)
			}
		})
	}
}

func TestWithSerenaWorkspaceGateTimeoutSkipsCallbacksAndBody(t *testing.T) {
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	const wsKey = "gate-timeout"

	if !s.beginSerenaIdleStop(wsKey) {
		t.Fatalf("precondition: could not hold idle-stop gate for %s", wsKey)
	}
	defer s.endSerenaIdleStop(wsKey)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	resolveCalls := 0
	urlCalls := 0
	onPhaseCalls := 0
	fnCalls := 0
	entered, aborted, err := s.withSerenaWorkspaceGate(
		ctx,
		wsKey,
		serenaWorkspaceGatePolicyBlock,
		func(string) *api.WorkspaceEntry {
			resolveCalls++
			return serenaWS(wsKey, "/proj/"+wsKey, 9126)
		},
		func(*api.WorkspaceEntry) string {
			urlCalls++
			return "http://127.0.0.1:9126"
		},
		func(*serenaWorkspaceGateOutcome) bool {
			onPhaseCalls++
			return true
		},
		func(*serenaWorkspaceGateOutcome) error {
			fnCalls++
			return nil
		},
	)

	if err != nil {
		t.Fatalf("withSerenaWorkspaceGate err = %v", err)
	}
	if entered {
		t.Fatalf("entered = true, want false after gate timeout")
	}
	if aborted {
		t.Fatalf("aborted = true, want false when timeout skips callbacks")
	}
	if resolveCalls != 0 {
		t.Fatalf("resolve calls = %d, want 0", resolveCalls)
	}
	if urlCalls != 0 {
		t.Fatalf("url calls = %d, want 0", urlCalls)
	}
	if onPhaseCalls != 0 {
		t.Fatalf("onPhaseActive calls = %d, want 0", onPhaseCalls)
	}
	if fnCalls != 0 {
		t.Fatalf("fn calls = %d, want 0", fnCalls)
	}
	if s.hasSerenaForwardInFlight(wsKey) {
		t.Fatalf("gate timeout must not leave an in-flight forward")
	}
}

func waitForSerenaStopGateWaiters(t *testing.T, s *Server, wsKey string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.serenaStopGate.mu.Lock()
		got := 0
		if entry := s.serenaStopGate.byWorkspace[wsKey]; entry != nil {
			got = entry.waiters
		}
		s.serenaStopGate.mu.Unlock()
		if got >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("stop gate waiters for %s did not reach %d", wsKey, want)
}

func releaseWaitingSerenaForward(t *testing.T, s *Server, wsKey string, entered <-chan serenaWorkspaceStopGateEnterResult) {
	t.Helper()
	s.serenaStopGate.mu.Lock()
	if entry := s.serenaStopGate.byWorkspace[wsKey]; entry != nil {
		entry.phase = serenaWorkspaceStopGatePhaseNone
		entry.ready.Broadcast()
	}
	s.serenaStopGate.mu.Unlock()
	select {
	case gate := <-entered:
		if !gate.entered {
			t.Fatalf("waiting forward did not enter after release: %#v", gate)
		}
		if !gate.phaseActive {
			t.Fatalf("waiting forward did not report phaseActive after waiting")
		}
		s.exitSerenaForward(wsKey)
	case <-time.After(2 * time.Second):
		t.Fatalf("waiting forward did not return after release")
	}
}

// A non-running (Stopped/Restarting) daemon is never idle-stopped.
func TestIdleSweeper_NotRunning_Skipped(t *testing.T) {
	const wsPath = "/proj/zeta"
	ws := serenaWS("zeta", wsPath, 9206)
	s := serenaSweeperServer(t, ws)

	now := time.Now().UTC()
	captured := withIdleSweeperSeams(t, time.Minute, true,
		func(context.Context) ([]api.DaemonStatus, error) {
			return []api.DaemonStatus{{Server: "serena", Workspace: wsPath, State: "Stopped", PID: 0, Port: 9206, UptimeSec: 0}}, nil
		},
	)
	s.recordSerenaActivity("zeta", now.Add(-99*time.Hour))
	if n := s.SweepIdleSerenaDaemons(context.Background(), now); n != 0 {
		t.Fatalf("a non-running daemon must not be idle-stopped; got n=%d", n)
	}
	if len(*captured) != 0 {
		t.Fatalf("stopped daemon must not be re-stopped; captured=%v", *captured)
	}
}

// A status-read failure must NOT idle-stop anything (false-positive guard).
func TestIdleSweeper_StatusReadError_NoStop(t *testing.T) {
	const wsPath = "/proj/eta"
	ws := serenaWS("eta", wsPath, 9207)
	s := serenaSweeperServer(t, ws)

	now := time.Now().UTC()
	captured := withIdleSweeperSeams(t, time.Minute, true,
		func(context.Context) ([]api.DaemonStatus, error) {
			return nil, context.DeadlineExceeded
		},
	)
	s.recordSerenaActivity("eta", now.Add(-99*time.Hour))
	if n := s.SweepIdleSerenaDaemons(context.Background(), now); n != 0 {
		t.Fatalf("status-read failure must not idle-stop (false-positive guard); got n=%d", n)
	}
	if len(*captured) != 0 {
		t.Fatalf("status error must stop nothing; captured=%v", *captured)
	}
}

// ---------------------------------------------------------------------------
// Router-handler wake integration.
// ---------------------------------------------------------------------------

// On a tool-call, the handler records activity for the resolved workspace AND
// invokes WakeIdleFn with the daemon's task name + port BEFORE the forward; the
// forward succeeds when the wake returns nil.
func TestRouterWake_InvokedAndRecordsActivity(t *testing.T) {
	const wsPath = "/proj/wake"
	ws := serenaWS("wake", wsPath, 9301)

	daemon := newFakeSerenaDaemon("wake")
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	var wakeMu sync.Mutex
	var gotTask string
	var gotPort int
	wakeCalls := 0
	deps := &serenaRouterDeps{
		Resolver:      &listerStubResolver{stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}}, list: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(*api.WorkspaceEntry) string { return ts.URL },
		AuditFn:       func(string, string, map[string]any) error { return nil },
		WakeIdleFn: func(_ context.Context, taskName string, port int, _ string) error {
			wakeMu.Lock()
			defer wakeMu.Unlock()
			wakeCalls++
			gotTask, gotPort = taskName, port
			return nil // daemon is up
		},
	}
	s := newSerenaTestServer(t, deps)

	sid := mintRouterSession(t, s, "2025-11-25")
	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": wsPath + "/src/main.go"})
	rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": sid})
	if rr.Code != http.StatusOK {
		t.Fatalf("tool call status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	wakeMu.Lock()
	defer wakeMu.Unlock()
	if wakeCalls != 1 {
		t.Fatalf("WakeIdleFn called %d times, want 1", wakeCalls)
	}
	if gotTask != `\mcp-local-hub-serena-wake` || gotPort != 9301 {
		t.Fatalf("wake called with (%q, %d), want (\\mcp-local-hub-serena-wake, 9301)", gotTask, gotPort)
	}
	if _, ok := s.lastSerenaActivity("wake"); !ok {
		t.Fatalf("handler must record last-activity for the resolved workspace")
	}
}

// FIX-3 (router integration): while a /serena/mcp forward is mid-flight to the
// daemon, hasSerenaForwardInFlight reports true; once the forward completes the
// counter is balanced back to zero AND last-activity is re-stamped. Drives the
// REAL handler forward path with a fake daemon whose tool handler blocks.
func TestRouterForward_InFlightCounterAndRestamp(t *testing.T) {
	// Hermetic state: the successful-forward path now persists LastToolsCallAt
	// to the registry. Redirect LOCALAPPDATA (Windows-authoritative, checked
	// first by DefaultRegistryPath) + XDG_STATE_HOME so the persist never
	// touches the developer's real registry (it is a no-op on the synthetic
	// key today, but stays hermetic if a real row ever shares the key).
	tmpState := t.TempDir()
	t.Setenv("LOCALAPPDATA", filepath.Join(tmpState, "AppData", "Local"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpState, "state"))
	const wsPath = "/proj/forwardflight"
	ws := serenaWS("forwardflight", wsPath, 9311)

	entered := make(chan struct{})
	release := make(chan struct{})
	daemon := newFakeSerenaDaemon("forwardflight")
	daemon.tool = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		close(entered) // the forward is now in-flight in the handler.
		<-release      // block until the test releases it (mid-call).
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	deps := &serenaRouterDeps{
		Resolver:      &listerStubResolver{stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}}, list: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(*api.WorkspaceEntry) string { return ts.URL },
		AuditFn:       func(string, string, map[string]any) error { return nil },
		// WakeIdleFn nil — this test exercises the forward in-flight tracking.
	}
	s := newSerenaTestServer(t, deps)

	sid := mintRouterSession(t, s, "2025-11-25")
	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": wsPath + "/src/main.go"})

	done := make(chan struct{})
	go func() {
		defer close(done)
		rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": sid})
		if rr.Code != http.StatusOK {
			t.Errorf("forward status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
	}()

	// Wait until the handler is mid-forward, then assert in-flight is observed.
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("forward never reached the daemon tool handler")
	}
	if !s.hasSerenaForwardInFlight(ws.WorkspaceKey) {
		t.Fatalf("the daemon must be marked in-flight WHILE the forward is open")
	}

	// Release the daemon; the forward completes.
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("forward did not complete after release")
	}

	// After completion: counter balanced to zero, activity re-stamped fresh.
	if s.hasSerenaForwardInFlight(ws.WorkspaceKey) {
		t.Fatalf("the in-flight counter must be balanced back to zero after the forward completes")
	}
	if _, ok := s.lastSerenaActivity(ws.WorkspaceKey); !ok {
		t.Fatalf("forward completion must re-stamp last-activity")
	}
}

func TestSerenaRouter_PreForwardWakeWindowBlocksIdleStop(t *testing.T) {
	tmpState := t.TempDir()
	t.Setenv("LOCALAPPDATA", filepath.Join(tmpState, "AppData", "Local"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpState, "state"))

	const wsPath = "/proj/pre-forward-wake"
	ws := serenaWS("pre-forward-wake", wsPath, 9312)

	daemon := newFakeSerenaDaemon("pre-forward-wake")
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	wakeStarted := make(chan struct{})
	releaseWake := make(chan struct{})
	var wakeOnce sync.Once
	deps := &serenaRouterDeps{
		Resolver: &listerStubResolver{
			stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}},
			list:         []*api.WorkspaceEntry{ws},
		},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(*api.WorkspaceEntry) string { return ts.URL },
		AuditFn:       func(string, string, map[string]any) error { return nil },
		WakeIdleFn: func(context.Context, string, int, string) error {
			wakeOnce.Do(func() { close(wakeStarted) })
			<-releaseWake
			return nil
		},
	}
	s := newSerenaTestServer(t, deps)

	captured := withIdleSweeperSeams(t, time.Nanosecond, true,
		func(context.Context) ([]api.DaemonStatus, error) {
			return []api.DaemonStatus{{Server: "serena", Workspace: wsPath, TaskName: ws.TaskName, State: "Running", PID: 1000, Port: ws.Port, UptimeSec: 7200}}, nil
		},
	)

	sid := mintRouterSession(t, s, "2025-11-25")
	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": wsPath + "/src/main.go"})

	done := make(chan struct{})
	go func() {
		defer close(done)
		rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": sid})
		if rr.Code != http.StatusOK {
			t.Errorf("tool call status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
	}()

	select {
	case <-wakeStarted:
	case <-time.After(5 * time.Second):
		close(releaseWake)
		t.Fatal("request never reached the injected slow wake")
	}

	n := s.SweepIdleSerenaDaemons(context.Background(), time.Now().UTC().Add(time.Minute))
	stoppedDuringWake := n != 0 || len(*captured) != 0

	close(releaseWake)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tool call did not complete after releasing wake")
	}

	if stoppedDuringWake {
		t.Fatalf("idle sweep stopped a workspace with a request already in its pre-forward wake window: n=%d captured=%v", n, *captured)
	}
}

func TestSerenaRouter_ToolsListWakeWindowBlocksIdleStop(t *testing.T) {
	tmpState := t.TempDir()
	t.Setenv("LOCALAPPDATA", filepath.Join(tmpState, "AppData", "Local"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpState, "state"))

	const wsPath = "/proj/tools-list-pre-forward-wake"
	daemon := newFakeSerenaDaemon("tools-list-pre-forward-wake")
	daemon.tool = func(w http.ResponseWriter, _ *http.Request, b []byte) {
		if !strings.Contains(string(b), `"tools/list"`) {
			t.Errorf("upstream body did not carry tools/list; got %s", string(b))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"find_symbol"}]}}`))
	}
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	ws := serenaWS("tools-list-pre-forward-wake", wsPath, testServerPort(t, ts))
	wakeStarted := make(chan struct{})
	releaseWake := make(chan struct{})
	var wakeOnce sync.Once
	deps := &serenaRouterDeps{
		Resolver: &listerStubResolver{
			stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}},
			list:         []*api.WorkspaceEntry{ws},
		},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(*api.WorkspaceEntry) string { return ts.URL },
		AuditFn:       func(string, string, map[string]any) error { return nil },
		WakeIdleFn: func(context.Context, string, int, string) error {
			wakeOnce.Do(func() { close(wakeStarted) })
			<-releaseWake
			return nil
		},
	}
	s := newSerenaTestServer(t, deps)

	captured := withIdleSweeperSeams(t, time.Nanosecond, true,
		func(context.Context) ([]api.DaemonStatus, error) {
			return []api.DaemonStatus{{Server: "serena", Workspace: wsPath, TaskName: ws.TaskName, State: "Running", PID: 1000, Port: ws.Port, UptimeSec: 7200}}, nil
		},
	)

	sid := mintRouterSession(t, s, "2025-11-25")
	body := buildLifecycleBody(t, "tools/list", map[string]any{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": sid})
		if rr.Code != http.StatusOK {
			t.Errorf("tools/list status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
	}()

	select {
	case <-wakeStarted:
	case <-time.After(5 * time.Second):
		close(releaseWake)
		t.Fatal("tools/list request never reached the injected slow wake")
	}

	markedDuringWake := s.hasSerenaForwardInFlight(ws.WorkspaceKey)
	n := s.SweepIdleSerenaDaemons(context.Background(), time.Now().UTC().Add(time.Minute))
	stoppedDuringWake := n != 0 || len(*captured) != 0

	close(releaseWake)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tools/list did not complete after releasing wake")
	}

	if !markedDuringWake {
		t.Fatalf("tools/list must mark workspace %q in-flight before WakeIdleFn blocks", ws.WorkspaceKey)
	}
	if stoppedDuringWake {
		t.Fatalf("idle sweep stopped a workspace with tools/list already in its wake window: n=%d captured=%v", n, *captured)
	}
}

// FALSIFICATION: when the daemon has an operator stop, WakeIdleFn returns
// ErrWakeRefusedOperatorStop. The handler must treat that as terminal instead
// of forwarding to whatever process happens to own the descriptor port.
func TestRouterWake_OperatorStopRefused_ReturnsStopped503WithoutForward(t *testing.T) {
	// DefaultRegistryPath checks LOCALAPPDATA FIRST on Windows (the project's GA
	// platform), falling through to XDG_STATE_HOME only when LOCALAPPDATA is
	// empty — so redirecting only XDG_STATE_HOME is a no-op on Windows and this
	// test would seed + Save into the developer's REAL registry. Redirect both.
	tmpState := t.TempDir()
	t.Setenv("LOCALAPPDATA", filepath.Join(tmpState, "AppData", "Local"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpState, "state"))
	const wsPath = "/proj/disabled"
	ws := serenaWS("disabled", wsPath, 9302)
	seedLastToolsCallAt := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("DefaultRegistryPath: %v", err)
	}
	reg := api.NewRegistry(regPath)
	if err := reg.PutSerena(api.WorkspaceEntry{
		WorkspaceKey:       ws.WorkspaceKey,
		WorkspacePath:      ws.WorkspacePath,
		Language:           api.SerenaLanguageSentinel,
		Backend:            ws.Backend,
		Port:               ws.Port,
		TaskName:           ws.TaskName,
		Lifecycle:          api.LifecycleFailed,
		LastError:          "operator stopped diagnostic",
		LastToolsCallAt:    seedLastToolsCallAt,
		ClientEntries:      map[string]string{},
		RegisteredVia:      "test",
		RegisteredAt:       seedLastToolsCallAt,
		WeeklyRefresh:      false,
		LastMaterializedAt: seedLastToolsCallAt,
	}); err != nil {
		t.Fatalf("seed PutSerena: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	daemon := newFakeSerenaDaemon("disabled")
	var forwardCalls int
	daemon.tool = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		forwardCalls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`))
	}
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	deps := &serenaRouterDeps{
		Resolver:      &listerStubResolver{stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}}, list: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(*api.WorkspaceEntry) string { return ts.URL },
		AuditFn:       func(string, string, map[string]any) error { return nil },
		WakeIdleFn: func(context.Context, string, int, string) error {
			return api.ErrWakeRefusedOperatorStop
		},
	}
	s := newSerenaTestServer(t, deps)

	sid := mintRouterSession(t, s, "2025-11-25")
	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": wsPath + "/src/main.go"})
	rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": sid})

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("operator-stop-refused wake status = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}
	if forwardCalls != 0 {
		t.Fatalf("operator-stop-refused wake forwarded to upstream %d time(s); want no forward", forwardCalls)
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode JSON-RPC error body: %v; body=%s", err, rr.Body.String())
	}
	if !strings.Contains(envelope.Error.Message, "operator stop") {
		t.Fatalf("error message = %q, want operator stop named", envelope.Error.Message)
	}
	gotReg := api.NewRegistry(regPath)
	if err := gotReg.Load(); err != nil {
		t.Fatalf("reload registry: %v", err)
	}
	got, ok := gotReg.Get(ws.WorkspaceKey, api.SerenaLanguageSentinel)
	if !ok {
		t.Fatal("seeded serena registry row missing")
	}
	if got.Lifecycle != api.LifecycleFailed || got.LastError != "operator stopped diagnostic" {
		t.Fatalf("refused wake mutated lifecycle/error to lifecycle=%q last_error=%q", got.Lifecycle, got.LastError)
	}
	if !got.LastToolsCallAt.Equal(seedLastToolsCallAt) {
		t.Fatalf("refused wake refreshed LastToolsCallAt to %v, want preserved %v", got.LastToolsCallAt, seedLastToolsCallAt)
	}
}

// When WakeIdleFn returns a NON-refusal error (respawn not ready in time), the
// handler 503s so the client retries.
func TestRouterWake_NotReady_503(t *testing.T) {
	const wsPath = "/proj/notready"
	ws := serenaWS("notready", wsPath, 9303)
	deps := &serenaRouterDeps{
		Resolver:      &listerStubResolver{stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}}, list: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(*api.WorkspaceEntry) string { return "http://unused" },
		AuditFn:       func(string, string, map[string]any) error { return nil },
		WakeIdleFn: func(context.Context, string, int, string) error {
			return context.DeadlineExceeded // respawn not ready
		},
	}
	s := newSerenaTestServer(t, deps)

	sid := mintRouterSession(t, s, "2025-11-25")
	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": wsPath + "/src/main.go"})
	rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": sid})
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("a not-ready wake must 503 so the client retries; got %d. body=%s", rr.Code, rr.Body.String())
	}
}

// FIX-4 (codex-unique P1): an all-idle serena pool with an empty tools-list
// cache must WAKE a candidate daemon on tools/list, not fail permanently. The
// fake daemon REFUSES tools/list until WakeIdleFn is invoked (modelling an
// idle-stopped daemon whose port is unbound until the supervisor respawns it on
// wake). Without the FIX-4 wake, tools/list would return the permanent
// "no daemon answered" error and the pool would stay asleep forever.
func TestRouterToolsList_AllIdle_WakesCandidateAndSucceeds(t *testing.T) {
	const wsPath = "/proj/toolswake"
	ws := serenaWS("toolswake", wsPath, 9312)

	var stateMu sync.Mutex
	awake := false

	daemon := newFakeSerenaDaemon("toolswake")
	daemon.tool = func(w http.ResponseWriter, _ *http.Request, b []byte) {
		stateMu.Lock()
		up := awake
		stateMu.Unlock()
		if !up {
			// Idle-stopped: as if the port were unbound — refuse so the fetch
			// loop records a candidate failure.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if !strings.Contains(string(b), `"tools/list"`) {
			t.Errorf("upstream body did not carry tools/list; got %s", string(b))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"find_symbol"}]}}`))
	}
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	wakeCalls := 0
	var wokeTask string
	var wokePort int
	deps := &serenaRouterDeps{
		Resolver:      &listerStubResolver{stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}}, list: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(*api.WorkspaceEntry) string { return ts.URL },
		AuditFn:       func(string, string, map[string]any) error { return nil },
		WakeIdleFn: func(_ context.Context, taskName string, port int, _ string) error {
			// The wake "respawns" the daemon: it now answers tools/list.
			stateMu.Lock()
			awake = true
			stateMu.Unlock()
			wakeCalls++
			wokeTask, wokePort = taskName, port
			return nil
		},
	}
	s := newSerenaTestServer(t, deps)

	sid := mintRouterSession(t, s, "2025-11-25")
	hdr := map[string]string{"Mcp-Session-Id": sid}
	body := buildLifecycleBody(t, "tools/list", map[string]any{})

	rr := postSerena(t, s, body, hdr)
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/list against an all-idle pool must wake a daemon and succeed; status=%d body=%s", rr.Code, rr.Body.String())
	}
	if wakeCalls != 1 {
		t.Fatalf("tools/list must wake exactly one candidate; wakeCalls=%d", wakeCalls)
	}
	if wokeTask != `\mcp-local-hub-serena-toolswake` || wokePort != 9312 {
		t.Fatalf("wake called with (%q,%d), want (\\mcp-local-hub-serena-toolswake, 9312)", wokeTask, wokePort)
	}
	assertToolsListNames(t, rr.Body.Bytes(), []string{"find_symbol"})
}

func TestRouterToolsList_ConfirmedIdleWakeDeletesBaselineAndIdleMarker(t *testing.T) {
	withSerenaIdleReconcileGlobals(t)
	withTempSerenaStateRoot(t)

	daemon := newFakeSerenaDaemon("toolswake-baseline")
	ts := newSafeSerenaHTTPTestServer(t, daemon.handler())
	port := testServerPort(t, ts)

	const wsPath = "/proj/toolswake-baseline"
	ws := serenaWS("toolswake-baseline", wsPath, port)
	wakeCalls := 0
	deps := &serenaRouterDeps{
		Resolver: &listerStubResolver{
			stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}},
			list:         []*api.WorkspaceEntry{ws},
		},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(*api.WorkspaceEntry) string { return ts.URL },
		AuditFn:       func(string, string, map[string]any) error { return nil },
		WakeIdleFn: func(_ context.Context, taskName string, _ int, _ string) error {
			wakeCalls++
			allowed, err := api.NewAPI().ClearStopIntentIfReason(taskName, api.IntentReasonIdle, "test-tools-list-wake")
			if err != nil {
				return err
			}
			if !allowed {
				t.Fatalf("tools/list wake did not clear active idle stop for %s", taskName)
			}
			return nil
		},
	}
	s := newSerenaTestServer(t, deps)
	seedSerenaBackendBaseline(s, wsPath, 1000)
	seedSerenaBackendIdleMarker(s, wsPath, 2)
	if err := api.NewAPI().WriteSerenaIdleStop(ws.TaskName, time.Now().UTC()); err != nil {
		t.Fatalf("seed idle stop: %v", err)
	}

	sid := mintRouterSession(t, s, "2025-11-25")
	rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/list wake status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if wakeCalls != 1 {
		t.Fatalf("WakeIdleFn calls = %d, want 1", wakeCalls)
	}
	if _, ok := serenaBackendBaselineForTest(s, wsPath); ok {
		t.Fatalf("tools/list confirmed idle wake left backend PID baseline for %q; want it deleted", wsPath)
	}
	if _, ok := serenaBackendIdleMarkerForTest(s, wsPath); ok {
		t.Fatalf("tools/list confirmed idle wake left backend idle marker for %q; want it deleted", wsPath)
	}
}

// TestRouterToolsList_AllIdle_FirstWakeRefusedTriesNextCandidate covers the
// multi-workspace pool case: a user-stopped row may refuse wake, but a later
// merely-idle sibling can still be woken and satisfy tools/list.
//
// Negative-control: wake only the first eligible candidate and this test leaves
// the second daemon asleep, so tools/list returns "no daemon answered".
func TestRouterToolsList_AllIdle_FirstWakeRefusedTriesNextCandidate(t *testing.T) {
	wsStopped := serenaWS("stopped", "/proj/stopped", 9313)
	wsIdle := serenaWS("idle", "/proj/idle", 9314)

	firstDaemon := newFakeSerenaDaemon("stopped")
	firstDaemon.tool = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	firstTS := httptest.NewServer(firstDaemon.handler())
	t.Cleanup(firstTS.Close)

	var stateMu sync.Mutex
	secondAwake := false
	secondDaemon := newFakeSerenaDaemon("idle")
	secondDaemon.tool = func(w http.ResponseWriter, _ *http.Request, b []byte) {
		stateMu.Lock()
		up := secondAwake
		stateMu.Unlock()
		if !up {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if !strings.Contains(string(b), `"tools/list"`) {
			t.Errorf("upstream body did not carry tools/list; got %s", string(b))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"find_symbol"}]}}`))
	}
	secondTS := httptest.NewServer(secondDaemon.handler())
	t.Cleanup(secondTS.Close)

	var wakeOrder []string
	deps := &serenaRouterDeps{
		Resolver: &listerStubResolver{
			stubResolver: stubResolver{entries: []*api.WorkspaceEntry{wsStopped, wsIdle}},
			list:         []*api.WorkspaceEntry{wsStopped, wsIdle},
		},
		Sessions: NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string {
			if ws.WorkspaceKey == wsStopped.WorkspaceKey {
				return firstTS.URL
			}
			return secondTS.URL
		},
		AuditFn: func(string, string, map[string]any) error { return nil },
		WakeIdleFn: func(_ context.Context, taskName string, _ int, _ string) error {
			wakeOrder = append(wakeOrder, taskName)
			if taskName == wsStopped.TaskName {
				return api.ErrWakeRefusedOperatorStop
			}
			stateMu.Lock()
			secondAwake = true
			stateMu.Unlock()
			return nil
		},
	}
	s := newSerenaTestServer(t, deps)

	sid := mintRouterSession(t, s, "2025-11-25")
	body := buildLifecycleBody(t, "tools/list", map[string]any{})
	rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": sid})
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/list should wake the second eligible candidate after first refusal; status=%d body=%s", rr.Code, rr.Body.String())
	}
	wantOrder := []string{wsStopped.TaskName, wsIdle.TaskName}
	if len(wakeOrder) != len(wantOrder) || wakeOrder[0] != wantOrder[0] || wakeOrder[1] != wantOrder[1] {
		t.Fatalf("wake order = %v, want %v", wakeOrder, wantOrder)
	}
	assertToolsListNames(t, rr.Body.Bytes(), []string{"find_symbol"})
}

// TestRouterToolsList_NoOpWakeDeadPortTriesNextCandidate covers the case where
// WakeIdleFn returns nil through its fast no-op path for a stale/dead first row.
// A nil wake is only terminal if that candidate is actually serving; otherwise
// tools/list must continue to a later wakeable idle-stopped sibling.
//
// Negative-control: pre-fix wakeOneSerenaCandidateForToolsList returns after the
// first nil wake, leaving the second candidate asleep and tools/list returns
// "no daemon answered".
func TestRouterToolsList_NoOpWakeDeadPortTriesNextCandidate(t *testing.T) {
	deadLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate dead candidate port: %v", err)
	}
	deadPort := deadLn.Addr().(*net.TCPAddr).Port
	if err := deadLn.Close(); err != nil {
		t.Fatalf("close dead candidate listener: %v", err)
	}

	wsDead := serenaWS("dead-noop", "/proj/dead-noop", deadPort)
	wsIdle := serenaWS("idle-wakeable", "/proj/idle-wakeable", 0)

	var stateMu sync.Mutex
	secondAwake := false
	secondDaemon := newFakeSerenaDaemon("idle-wakeable")
	secondDaemon.tool = func(w http.ResponseWriter, _ *http.Request, b []byte) {
		stateMu.Lock()
		up := secondAwake
		stateMu.Unlock()
		if !up {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if !strings.Contains(string(b), `"tools/list"`) {
			t.Errorf("upstream body did not carry tools/list; got %s", string(b))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"find_symbol"}]}}`))
	}
	secondTS := httptest.NewServer(secondDaemon.handler())
	t.Cleanup(secondTS.Close)
	wsIdle.Port = secondTS.Listener.Addr().(*net.TCPAddr).Port

	var probedPorts []int
	origPortLive := serenaToolsListPortLiveFn
	serenaToolsListPortLiveFn = func(_ context.Context, port int) bool {
		probedPorts = append(probedPorts, port)
		return port == wsIdle.Port
	}
	t.Cleanup(func() { serenaToolsListPortLiveFn = origPortLive })

	var wakeOrder []string
	deps := &serenaRouterDeps{
		Resolver: &listerStubResolver{
			stubResolver: stubResolver{entries: []*api.WorkspaceEntry{wsDead, wsIdle}},
			list:         []*api.WorkspaceEntry{wsDead, wsIdle},
		},
		Sessions: NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string {
			if ws.WorkspaceKey == wsIdle.WorkspaceKey {
				return secondTS.URL
			}
			return "http://127.0.0.1:1"
		},
		AuditFn: func(string, string, map[string]any) error { return nil },
		WakeIdleFn: func(_ context.Context, taskName string, _ int, _ string) error {
			wakeOrder = append(wakeOrder, taskName)
			if taskName == wsIdle.TaskName {
				stateMu.Lock()
				secondAwake = true
				stateMu.Unlock()
			}
			return nil
		},
	}
	s := newSerenaTestServer(t, deps)

	sid := mintRouterSession(t, s, "2025-11-25")
	rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/list should continue after dead no-op wake and wake the second candidate; status=%d body=%s wakeOrder=%v", rr.Code, rr.Body.String(), wakeOrder)
	}
	wantOrder := []string{wsDead.TaskName, wsIdle.TaskName}
	if len(wakeOrder) != len(wantOrder) || wakeOrder[0] != wantOrder[0] || wakeOrder[1] != wantOrder[1] {
		t.Fatalf("wake order = %v, want %v", wakeOrder, wantOrder)
	}
	wantProbes := []int{wsDead.Port, wsIdle.Port}
	if len(probedPorts) != len(wantProbes) || probedPorts[0] != wantProbes[0] || probedPorts[1] != wantProbes[1] {
		t.Fatalf("probed ports = %v, want %v", probedPorts, wantProbes)
	}
	assertToolsListNames(t, rr.Body.Bytes(), []string{"find_symbol"})
}

func TestRouterToolsList_ConfirmedWakeReseedsExistingBaseline(t *testing.T) {
	withTempSerenaStateRoot(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)
	port := testServerPort(t, ts)
	if (port >= 9121 && port <= 9299) || (port >= 9400 && port <= 9599) {
		t.Skipf("httptest selected live mcphub port %d", port)
	}

	const wsPath = "/proj/tools-list-wake-reseed"
	const pid = 3579
	ws := serenaWS("tools-list-wake-reseed", wsPath, port)
	wakeCalls := 0
	deps := &serenaRouterDeps{
		Resolver: &listerStubResolver{
			stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}},
			list:         []*api.WorkspaceEntry{ws},
		},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(*api.WorkspaceEntry) string { return ts.URL },
		AuditFn:       func(string, string, map[string]any) error { return nil },
		WakeIdleFn: func(_ context.Context, taskName string, _ int, _ string) error {
			wakeCalls++
			allowed, err := api.NewAPI().ClearStopIntentIfReason(taskName, api.IntentReasonIdle, "test-tools-list-wake")
			if err != nil {
				return err
			}
			if !allowed {
				t.Fatalf("ClearStopIntentIfReason returned false for active idle stop")
			}
			return nil
		},
	}
	s := newSerenaTestServer(t, deps)
	seedSerenaBackendBaseline(s, wsPath, pid)
	seedSerenaBackendIdleMarker(s, wsPath, serenaBackendPostIdleGraceTicks)

	prevStatusFn := serenaBackendStatusFn
	serenaBackendStatusFn = func(context.Context) ([]api.DaemonStatus, error) {
		return []api.DaemonStatus{
			{Server: "serena", Workspace: wsPath, TaskName: ws.TaskName, State: "Running", PID: pid, Port: port},
		}, nil
	}
	t.Cleanup(func() { serenaBackendStatusFn = prevStatusFn })
	if err := api.NewAPI().WriteSerenaIdleStop(ws.TaskName, time.Now().UTC()); err != nil {
		t.Fatalf("WriteSerenaIdleStop: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/serena/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	s.wakeOneSerenaCandidateForToolsList(req, deps, []*api.WorkspaceEntry{ws}, deps.AuditFn)

	if wakeCalls != 1 {
		t.Fatalf("WakeIdleFn calls = %d, want 1", wakeCalls)
	}
	// No router session is bound to the workspace in this test, so the
	// confirmed wake must DROP the baseline rather than reseed it (PR #291 bot
	// r9 — a baseline recorded during an unbound window outlives a later
	// restart and the first real session's establish-only seed then keeps the
	// stale PID, producing a false backend-loss teardown).
	if got, ok := serenaBackendBaselineForTest(s, wsPath); ok {
		t.Fatalf("tools/list post-wake baseline with no bound sessions = (%d,true), want absent", got)
	}
	if ticks, ok := serenaBackendIdleMarkerForTest(s, wsPath); ok {
		t.Fatalf("tools/list post-wake idle marker = (%d,true), want absent", ticks)
	}

	// BOUND variant: with a live router session indexed to the workspace the
	// confirmed wake reseeds the baseline to the observed live PID.
	sid := mintRouterSession(t, s, "2025-11-25")
	s.serenaRouterSessions.bindWorkspace(sid, ws.WorkspaceKey)
	seedSerenaBackendBaseline(s, wsPath, pid-1) // stale pre-wake value
	seedSerenaBackendIdleMarker(s, wsPath, serenaBackendPostIdleGraceTicks)
	if err := api.NewAPI().WriteSerenaIdleStop(ws.TaskName, time.Now().UTC()); err != nil {
		t.Fatalf("WriteSerenaIdleStop (bound variant): %v", err)
	}
	s.wakeOneSerenaCandidateForToolsList(req, deps, []*api.WorkspaceEntry{ws}, deps.AuditFn)
	if wakeCalls != 2 {
		t.Fatalf("WakeIdleFn calls after bound-variant wake = %d, want 2", wakeCalls)
	}
	if got, ok := serenaBackendBaselineForTest(s, wsPath); !ok || got != pid {
		t.Fatalf("bound-variant post-wake baseline = (%d,%v), want (%d,true)", got, ok, pid)
	}
	if ticks, ok := serenaBackendIdleMarkerForTest(s, wsPath); ok {
		t.Fatalf("bound-variant post-wake idle marker = (%d,true), want absent", ticks)
	}
}

// A non-serena row (LSP workspace proxy) in the status is never idle-stopped by
// this serena sweeper.
func TestIdleSweeper_NonSerenaRow_Ignored(t *testing.T) {
	const wsPath = "/proj/theta"
	// An LSP (non-serena) workspace entry: the sweeper must not target it.
	lsp := &api.WorkspaceEntry{
		WorkspaceKey:  "theta",
		WorkspacePath: wsPath,
		Port:          9208,
		Backend:       "mcp-language-server",
		Language:      "go",
		TaskName:      `\mcp-local-hub-lsp-theta-go`,
	}
	s := serenaSweeperServer(t, lsp)

	now := time.Now().UTC()
	captured := withIdleSweeperSeams(t, time.Minute, true,
		func(context.Context) ([]api.DaemonStatus, error) {
			return []api.DaemonStatus{{Server: "mcp-language-server", Workspace: wsPath, State: "Running", PID: 1000, Port: 9208, UptimeSec: 99999}}, nil
		},
	)
	s.recordSerenaActivity("theta", now.Add(-99*time.Hour))
	if n := s.SweepIdleSerenaDaemons(context.Background(), now); n != 0 {
		t.Fatalf("a non-serena (LSP) daemon must not be idle-stopped by the serena sweeper; got n=%d", n)
	}
	if len(*captured) != 0 {
		t.Fatalf("non-serena row must not be stopped; captured=%v", *captured)
	}
}

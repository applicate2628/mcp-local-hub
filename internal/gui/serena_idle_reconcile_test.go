package gui

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

func withSerenaIdleReconcileGlobals(t *testing.T) {
	t.Helper()
	prevStatus := serenaBackendStatusFn
	prevThreshold := serenaIdleThresholdFn
	prevStop := serenaIdleStopFn
	prevRouterSeam := serenaRouterTestSeam
	serenaRouterTestSeam = nil
	t.Cleanup(func() {
		serenaBackendStatusFn = prevStatus
		serenaIdleThresholdFn = prevThreshold
		serenaIdleStopFn = prevStop
		serenaRouterTestSeam = prevRouterSeam
	})
}

func withTempSerenaStateRoot(t *testing.T) {
	t.Helper()
	t.Cleanup(api.SetDaemonStateRootForTest(apitest.HardenedTempDir(t)))
}

func newSerenaStoreSeedServer(t *testing.T, ws *api.WorkspaceEntry) (*Server, *InMemorySessionRouter) {
	t.Helper()
	sessions := NewInMemorySessionRouter()
	s := &Server{}
	s.SetSerenaRouterDeps(&serenaRouterDeps{
		Resolver: &listerStubResolver{
			stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}},
			list:         []*api.WorkspaceEntry{ws},
		},
		Sessions: sessions,
		AuditFn:  func(string, string, map[string]any) error { return nil },
	})
	return s, sessions
}

func seedBoundSerenaSession(s *Server, sessions *InMemorySessionRouter, ws *api.WorkspaceEntry, sid, daemonSessionID string) {
	s.serenaRouterSessions.store(sid, "2025-11-25")
	s.serenaRouterSessions.bindWorkspace(sid, ws.WorkspaceKey)
	if daemonSessionID != "" {
		s.serenaDaemonSessions.store(sid, ws.WorkspaceKey, daemonSessionID, "2025-11-25")
	}
	sessions.BindSession(sid, ws)
}

func seedSerenaBackendBaseline(s *Server, wsPath string, pid int) {
	s.serenaBackendPIDMu.Lock()
	defer s.serenaBackendPIDMu.Unlock()
	s.serenaBackendLastPID = map[string]int{wsPath: pid}
}

func seedSerenaBackendIdleMarker(s *Server, wsPath string, ticks int) {
	s.serenaBackendPIDMu.Lock()
	defer s.serenaBackendPIDMu.Unlock()
	if s.serenaBackendIdlePaths == nil {
		s.serenaBackendIdlePaths = map[string]int{}
	}
	s.serenaBackendIdlePaths[wsPath] = ticks
}

func serenaBackendBaselineForTest(s *Server, wsPath string) (int, bool) {
	s.serenaBackendPIDMu.Lock()
	defer s.serenaBackendPIDMu.Unlock()
	pid, ok := s.serenaBackendLastPID[wsPath]
	return pid, ok
}

func serenaBackendIdleMarkerForTest(s *Server, wsPath string) (int, bool) {
	s.serenaBackendPIDMu.Lock()
	defer s.serenaBackendPIDMu.Unlock()
	ticks, ok := s.serenaBackendIdlePaths[wsPath]
	return ticks, ok
}

func assertSerenaSessionLive(t *testing.T, s *Server, sessions *InMemorySessionRouter, wsKey, sid string) {
	t.Helper()
	if !s.serenaRouterSessions.known(sid) {
		t.Fatalf("router session %q was torn down; want it alive", sid)
	}
	if got := s.serenaRouterSessions.sessionsForWorkspace(wsKey); len(got) != 1 || got[0] != sid {
		t.Fatalf("workspace %q sessions = %v, want [%s]", wsKey, got, sid)
	}
	if got := sessions.LookupSession(sid); got == nil || got.WorkspaceKey != wsKey {
		t.Fatalf("sticky session %q = %+v, want workspace %q", sid, got, wsKey)
	}
}

func assertSerenaSessionGone(t *testing.T, s *Server, sessions *InMemorySessionRouter, sid string) {
	t.Helper()
	if s.serenaRouterSessions.known(sid) {
		t.Fatalf("router session %q survived backend loss; want it gone", sid)
	}
	if _, _, _, ok := s.serenaDaemonSessions.bindingFor(sid); ok {
		t.Fatalf("daemon-session binding for %q survived backend loss; want it gone", sid)
	}
	if got := sessions.LookupSession(sid); got != nil {
		t.Fatalf("sticky session %q survived backend loss: %+v", sid, got)
	}
}

func TestSerenaRouter_BackendLoss_SeedNeverOverwritesExistingBaseline(t *testing.T) {
	withSerenaIdleReconcileGlobals(t)

	const wsPath = "/proj/establish-only"
	ws := serenaWS("establish-only", wsPath, 9301)
	s, _ := newSerenaStoreSeedServer(t, ws)
	seedSerenaBackendBaseline(s, wsPath, 1000)
	serenaBackendStatusFn = func(context.Context) ([]api.DaemonStatus, error) {
		return []api.DaemonStatus{
			{Server: "serena", Workspace: wsPath, TaskName: ws.TaskName, State: "Running", PID: 2000, Port: ws.Port},
		}, nil
	}

	s.seedSerenaBackendPIDBaseline(context.Background(), ws)

	if got, ok := serenaBackendBaselineForTest(s, wsPath); !ok || got != 1000 {
		t.Fatalf("seed overwrote existing baseline = (%d,%v), want (1000,true)", got, ok)
	}
}

func TestSerenaRouter_BackendLoss_LastUnbindDropsBaselineAndIdleMarker(t *testing.T) {
	withSerenaIdleReconcileGlobals(t)
	withTempSerenaStateRoot(t)

	const wsPath = "/proj/last-unbind"
	const sid = "sid-last-unbind"
	ws := serenaWS("last-unbind", wsPath, 9301)
	s, sessions := newSerenaStoreSeedServer(t, ws)
	seedBoundSerenaSession(s, sessions, ws, sid, "daemon-before-unbind")
	seedSerenaBackendBaseline(s, wsPath, 1000)
	seedSerenaBackendIdleMarker(s, wsPath, 2)

	s.coordinateBackendLossUnbind(sid, sessions)

	assertSerenaSessionGone(t, s, sessions, sid)
	if _, ok := serenaBackendBaselineForTest(s, wsPath); ok {
		t.Fatalf("last unbind left backend PID baseline for %q; want it deleted", wsPath)
	}
	if _, ok := serenaBackendIdleMarkerForTest(s, wsPath); ok {
		t.Fatalf("last unbind left backend idle marker for %q; want it deleted", wsPath)
	}
}

func TestSerenaRouter_BackendLoss_OneOfTwoUnbindKeepsBaselineAndIdleMarker(t *testing.T) {
	withSerenaIdleReconcileGlobals(t)
	withTempSerenaStateRoot(t)

	const wsPath = "/proj/two-unbind"
	ws := serenaWS("two-unbind", wsPath, 9302)
	s, sessions := newSerenaStoreSeedServer(t, ws)
	seedBoundSerenaSession(s, sessions, ws, "sid-a", "daemon-a")
	seedBoundSerenaSession(s, sessions, ws, "sid-b", "daemon-b")
	seedSerenaBackendBaseline(s, wsPath, 1000)
	seedSerenaBackendIdleMarker(s, wsPath, 2)

	s.coordinateBackendLossUnbind("sid-a", sessions)

	assertSerenaSessionGone(t, s, sessions, "sid-a")
	if got := s.serenaRouterSessions.sessionsForWorkspace(ws.WorkspaceKey); len(got) != 1 || got[0] != "sid-b" {
		t.Fatalf("workspace sessions after one unbind = %v, want [sid-b]", got)
	}
	if got, ok := serenaBackendBaselineForTest(s, wsPath); !ok || got != 1000 {
		t.Fatalf("one-of-two unbind baseline = (%d,%v), want (1000,true)", got, ok)
	}
	if got, ok := serenaBackendIdleMarkerForTest(s, wsPath); !ok || got != 2 {
		t.Fatalf("one-of-two unbind idle marker = (%d,%v), want (2,true)", got, ok)
	}
}

func TestSerenaRouter_BackendLoss_IPCReconcileFirstTickAbsentRowTearsDownBoundSession(t *testing.T) {
	withSerenaIdleReconcileGlobals(t)
	withTempSerenaStateRoot(t)

	const wsPath = "/proj/absent-alpha"
	const sid = "sid-absent-alpha"
	ws := serenaWS("absent-alpha", wsPath, 9301)
	s, sessions := newSerenaStoreSeedServer(t, ws)
	seedBoundSerenaSession(s, sessions, ws, sid, "daemon-before-loss")
	serenaBackendStatusFn = func(context.Context) ([]api.DaemonStatus, error) {
		return nil, nil
	}

	if _, ok := serenaBackendBaselineForTest(s, wsPath); ok {
		t.Fatalf("precondition: backend PID baseline for %q should be absent on the first tick", wsPath)
	}
	if n := s.ReconcileSerenaBackendLossViaIPC(context.Background()); n != 1 {
		t.Fatalf("first tick with absent IPC row tore down %d sessions; want 1 because the bound daemon vanished before the first observation", n)
	}
	assertSerenaSessionGone(t, s, sessions, sid)
	if _, ok := serenaBackendBaselineForTest(s, wsPath); ok {
		t.Fatalf("absent-row backend loss persisted a PID baseline for %q; want no persisted entry", wsPath)
	}
}

func TestSerenaRouter_BackendLoss_IPCReconcileFirstTickAbsentRowWithIdleStopSurvives(t *testing.T) {
	withSerenaIdleReconcileGlobals(t)
	withTempSerenaStateRoot(t)

	const wsPath = "/proj/idle-absent-alpha"
	const sid = "sid-idle-absent-alpha"
	ws := serenaWS("idle-absent-alpha", wsPath, 9301)
	s, sessions := newSerenaStoreSeedServer(t, ws)
	seedBoundSerenaSession(s, sessions, ws, sid, "daemon-before-idle")
	if err := api.NewAPI().WriteSerenaIdleStop(ws.TaskName, time.Now().UTC()); err != nil {
		t.Fatalf("seed idle stop: %v", err)
	}
	serenaBackendStatusFn = func(context.Context) ([]api.DaemonStatus, error) {
		return nil, nil
	}

	if n := s.ReconcileSerenaBackendLossViaIPC(context.Background()); n != 0 {
		t.Fatalf("first tick with absent IPC row plus active idle stop tore down %d sessions; want 0 so idle wake can preserve the session", n)
	}
	assertSerenaSessionLive(t, s, sessions, ws.WorkspaceKey, sid)
	if _, ok := serenaBackendBaselineForTest(s, wsPath); ok {
		t.Fatalf("idle absent-row first tick persisted a PID baseline for %q; want no PID baseline while idle-stopped", wsPath)
	}
}

func TestSerenaRouter_BackendLoss_IPCReconcileFirstTickRunningRowEstablishesBaseline(t *testing.T) {
	withSerenaIdleReconcileGlobals(t)
	withTempSerenaStateRoot(t)

	const wsPath = "/proj/running-alpha"
	const sid = "sid-running-alpha"
	const pid = 3333
	ws := serenaWS("running-alpha", wsPath, 9301)
	s, sessions := newSerenaStoreSeedServer(t, ws)
	seedBoundSerenaSession(s, sessions, ws, sid, "daemon-running")
	serenaBackendStatusFn = func(context.Context) ([]api.DaemonStatus, error) {
		return []api.DaemonStatus{
			{Server: "serena", Workspace: wsPath, TaskName: ws.TaskName, State: "Running", PID: pid, Port: ws.Port},
		}, nil
	}

	if n := s.ReconcileSerenaBackendLossViaIPC(context.Background()); n != 0 {
		t.Fatalf("first running-row observation tore down %d sessions; want 0 because it should establish the baseline", n)
	}
	assertSerenaSessionLive(t, s, sessions, ws.WorkspaceKey, sid)
	if got, ok := serenaBackendBaselineForTest(s, wsPath); !ok || got != pid {
		t.Fatalf("baseline after first running-row observation = (%d,%v), want (%d,true)", got, ok, pid)
	}
}

func TestSerenaRouter_BackendLoss_IPCReconcilePreservesIdleStoppedAndPostWakePID(t *testing.T) {
	withSerenaIdleReconcileGlobals(t)
	withTempSerenaStateRoot(t)

	const wsPath = "/proj/idle-alpha"
	const sid = "sid-idle-alpha"
	const pidA = 1111
	const pidB = 2222
	ws := serenaWS("idle-alpha", wsPath, 9301)
	s, sessions := newSerenaStoreSeedServer(t, ws)
	seedBoundSerenaSession(s, sessions, ws, sid, "daemon-before-idle")
	seedSerenaBackendBaseline(s, wsPath, pidA)

	row := api.DaemonStatus{Server: "serena", Workspace: wsPath, TaskName: ws.TaskName, State: "Running", PID: pidA, Port: ws.Port}
	serenaBackendStatusFn = func(context.Context) ([]api.DaemonStatus, error) {
		return []api.DaemonStatus{row}, nil
	}

	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	if err := api.NewAPI().WriteSerenaIdleStop(ws.TaskName, now); err != nil {
		t.Fatalf("seed idle stop: %v", err)
	}

	row = api.DaemonStatus{Server: "serena", Workspace: wsPath, TaskName: ws.TaskName, State: "Stopped", PID: 0, StalePID: 0, Port: ws.Port}
	if n := s.ReconcileSerenaBackendLossViaIPC(context.Background()); n != 0 {
		t.Fatalf("idle-stopped daemon tore down %d sessions; want 0 so next request can wake transparently", n)
	}
	assertSerenaSessionLive(t, s, sessions, ws.WorkspaceKey, sid)
	if got, ok := serenaBackendBaselineForTest(s, wsPath); !ok || got != pidA {
		t.Fatalf("baseline after idle-stopped tick = (%d,%v), want (%d,true)", got, ok, pidA)
	}

	clearAllowed, err := api.NewAPI().ClearStopIntentIfReason(ws.TaskName, api.IntentReasonIdle, "test")
	if err != nil || !clearAllowed {
		t.Fatalf("clear idle stop = (%v,%v), want (nil,true)", err, clearAllowed)
	}
	row = api.DaemonStatus{Server: "serena", Workspace: wsPath, TaskName: ws.TaskName, State: "Stopped", PID: 0, StalePID: 0, Port: ws.Port}
	if n := s.ReconcileSerenaBackendLossViaIPC(context.Background()); n != 0 {
		t.Fatalf("post-clear wake window tore down %d sessions; want 0 while the idle grace preserves the waking session", n)
	}
	assertSerenaSessionLive(t, s, sessions, ws.WorkspaceKey, sid)
	if got, ok := serenaBackendBaselineForTest(s, wsPath); !ok || got != pidA {
		t.Fatalf("baseline during post-clear wake window = (%d,%v), want (%d,true)", got, ok, pidA)
	}

	row = api.DaemonStatus{Server: "serena", Workspace: wsPath, TaskName: ws.TaskName, State: "Running", PID: pidB, Port: ws.Port}
	if n := s.ReconcileSerenaBackendLossViaIPC(context.Background()); n != 0 {
		t.Fatalf("first post-idle wake PID change tore down %d sessions; want 0 and baseline refresh", n)
	}
	assertSerenaSessionLive(t, s, sessions, ws.WorkspaceKey, sid)
	if got, ok := serenaBackendBaselineForTest(s, wsPath); !ok || got != pidB {
		t.Fatalf("baseline after post-wake running tick = (%d,%v), want (%d,true)", got, ok, pidB)
	}

	row = api.DaemonStatus{Server: "serena", Workspace: wsPath, TaskName: ws.TaskName, State: "Stopped", PID: 0, StalePID: 0, Port: ws.Port}
	if n := s.ReconcileSerenaBackendLossViaIPC(context.Background()); n != 1 {
		t.Fatalf("stopped daemon without an idle stop tore down %d sessions; want 1 real backend-loss teardown", n)
	}
	assertSerenaSessionGone(t, s, sessions, sid)
}

func TestSerenaRouter_BackendLoss_IPCReconcileIdleGraceExpiresWithoutRespawn(t *testing.T) {
	withSerenaIdleReconcileGlobals(t)
	withTempSerenaStateRoot(t)

	const wsPath = "/proj/idle-never-respawns"
	const sid = "sid-idle-never-respawns"
	const pidA = 3333
	ws := serenaWS("idle-never-respawns", wsPath, 9302)
	s, sessions := newSerenaStoreSeedServer(t, ws)
	seedBoundSerenaSession(s, sessions, ws, sid, "daemon-before-idle")
	seedSerenaBackendBaseline(s, wsPath, pidA)

	row := api.DaemonStatus{Server: "serena", Workspace: wsPath, TaskName: ws.TaskName, State: "Stopped", PID: 0, StalePID: 0, Port: ws.Port}
	serenaBackendStatusFn = func(context.Context) ([]api.DaemonStatus, error) {
		return []api.DaemonStatus{row}, nil
	}

	now := time.Date(2026, 6, 11, 12, 30, 0, 0, time.UTC)
	if err := api.NewAPI().WriteSerenaIdleStop(ws.TaskName, now); err != nil {
		t.Fatalf("seed idle stop: %v", err)
	}
	if n := s.ReconcileSerenaBackendLossViaIPC(context.Background()); n != 0 {
		t.Fatalf("idle-stopped daemon tore down %d sessions; want 0", n)
	}
	assertSerenaSessionLive(t, s, sessions, ws.WorkspaceKey, sid)

	clearAllowed, err := api.NewAPI().ClearStopIntentIfReason(ws.TaskName, api.IntentReasonIdle, "test")
	if err != nil || !clearAllowed {
		t.Fatalf("clear idle stop = (%v,%v), want (nil,true)", err, clearAllowed)
	}
	if n := s.ReconcileSerenaBackendLossViaIPC(context.Background()); n != 0 {
		t.Fatalf("first post-clear dead tick tore down %d sessions; want 0 during bounded idle grace", n)
	}
	assertSerenaSessionLive(t, s, sessions, ws.WorkspaceKey, sid)

	if n := s.ReconcileSerenaBackendLossViaIPC(context.Background()); n != 1 {
		t.Fatalf("second post-clear dead tick tore down %d sessions; want 1 after idle grace is exhausted", n)
	}
	assertSerenaSessionGone(t, s, sessions, sid)
}

func TestIdleSweeper_InvalidatesDaemonSessionOnlyAfterIdleStop(t *testing.T) {
	withSerenaIdleReconcileGlobals(t)
	withTempSerenaStateRoot(t)

	const wsPath = "/proj/idle-s2"
	const sid = "sid-idle-s2"
	ws := serenaWS("idle-s2", wsPath, 9302)
	s, sessions := newSerenaStoreSeedServer(t, ws)
	seedBoundSerenaSession(s, sessions, ws, sid, "daemon-session-before-idle")

	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	serenaIdleThresholdFn = func() (time.Duration, bool) { return 30 * time.Minute, true }
	serenaIdleStopFn = api.NewAPI().WriteSerenaIdleStop
	serenaBackendStatusFn = func(context.Context) ([]api.DaemonStatus, error) {
		return []api.DaemonStatus{{Server: "serena", Workspace: wsPath, TaskName: ws.TaskName, State: "Running", PID: 1000, Port: ws.Port, UptimeSec: 7200}}, nil
	}
	s.recordSerenaActivity(ws.WorkspaceKey, now.Add(-45*time.Minute))

	if n := s.SweepIdleSerenaDaemons(context.Background(), now); n != 1 {
		t.Fatalf("idle sweeper stopped %d daemons; want 1", n)
	}
	if _, _, _, ok := s.serenaDaemonSessions.bindingFor(sid); ok {
		t.Fatalf("daemon-session binding survived idle stop; next request would reuse a stale upstream session id")
	}
	if dsid, _, ok := s.serenaDaemonSessions.lookup(sid, ws.WorkspaceKey); ok {
		t.Fatalf("daemon-session lookup after idle stop = %q; want miss and re-handshake", dsid)
	}
	assertSerenaSessionLive(t, s, sessions, ws.WorkspaceKey, sid)
}

func TestSerenaRouter_BackendLoss_UnboundWindowRestartSeedsNewGeneration(t *testing.T) {
	withSerenaIdleReconcileGlobals(t)
	withTempSerenaStateRoot(t)

	daemon := newFakeSerenaDaemon("unbound-restart")
	ts := newSafeSerenaHTTPTestServer(t, daemon.handler())
	port := testServerPort(t, ts)

	const wsPath = "/proj/unbound-restart"
	const oldSID = "sid-before-unbound-window"
	const pidA = 1000
	const pidB = 2000
	ws := serenaWS("unbound-restart", wsPath, port)
	sessions := NewInMemorySessionRouter()
	deps := &serenaRouterDeps{
		Resolver: &listerStubResolver{
			stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}},
			list:         []*api.WorkspaceEntry{ws},
		},
		Sessions:        sessions,
		UpstreamURLFn:   func(*api.WorkspaceEntry) string { return ts.URL },
		AuditFn:         func(string, string, map[string]any) error { return nil },
		UpstreamTimeout: 2 * time.Second,
	}
	s := newSerenaTestServer(t, deps)
	seedBoundSerenaSession(s, sessions, ws, oldSID, "daemon-before-unbound-window")
	seedSerenaBackendBaseline(s, wsPath, pidA)

	s.coordinateBackendLossUnbind(oldSID, sessions)
	if _, ok := serenaBackendBaselineForTest(s, wsPath); ok {
		t.Fatalf("unbound window left stale backend PID baseline for %q; want it deleted before the next first bind", wsPath)
	}

	serenaBackendStatusFn = func(context.Context) ([]api.DaemonStatus, error) {
		return []api.DaemonStatus{
			{Server: "serena", Workspace: wsPath, TaskName: ws.TaskName, State: "Running", PID: pidB, Port: port},
		}, nil
	}
	sid := mintRouterSession(t, s, "2025-11-25")
	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": wsPath + "/src/main.py"})
	if rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": sid}); rr.Code != http.StatusOK {
		t.Fatalf("post-restart first bind status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got, ok := serenaBackendBaselineForTest(s, wsPath); !ok || got != pidB {
		t.Fatalf("post-unbound-window seed baseline = (%d,%v), want (%d,true)", got, ok, pidB)
	}
	if n := s.ReconcileSerenaBackendLossViaIPC(context.Background()); n != 0 {
		t.Fatalf("reconcile after unbound-window restart tore down %d sessions; want 0", n)
	}
	assertSerenaSessionLive(t, s, sessions, ws.WorkspaceKey, sid)
}

func TestSerenaRouter_BackendLoss_FreshHandshakeDoesNotOverwriteExistingBaseline(t *testing.T) {
	withSerenaIdleReconcileGlobals(t)

	daemon := newFakeSerenaDaemon("fresh-handshake")
	ts := newSafeSerenaHTTPTestServer(t, daemon.handler())
	port := testServerPort(t, ts)

	const wsPath = "/proj/fresh-handshake"
	const pidA = 1000
	const pidB = 2000
	ws := serenaWS("fresh-handshake", wsPath, port)
	sessions := NewInMemorySessionRouter()
	deps := &serenaRouterDeps{
		Resolver: &listerStubResolver{
			stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}},
			list:         []*api.WorkspaceEntry{ws},
		},
		Sessions:        sessions,
		UpstreamURLFn:   func(*api.WorkspaceEntry) string { return ts.URL },
		AuditFn:         func(string, string, map[string]any) error { return nil },
		UpstreamTimeout: 2 * time.Second,
	}
	s := newSerenaTestServer(t, deps)
	seedSerenaBackendBaseline(s, wsPath, pidA)
	serenaBackendStatusFn = func(context.Context) ([]api.DaemonStatus, error) {
		return []api.DaemonStatus{
			{Server: "serena", Workspace: wsPath, TaskName: ws.TaskName, State: "Running", PID: pidB, Port: port},
		}, nil
	}

	sid := mintRouterSession(t, s, "2025-11-25")
	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": wsPath + "/src/main.py"})
	if rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": sid}); rr.Code != http.StatusOK {
		t.Fatalf("fresh-handshake request status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got, ok := serenaBackendBaselineForTest(s, wsPath); !ok || got != pidA {
		t.Fatalf("fresh handshake overwrote existing baseline = (%d,%v), want (%d,true)", got, ok, pidA)
	}
	if n := s.ReconcileSerenaBackendLossViaIPC(context.Background()); n != 1 {
		t.Fatalf("reconcile after preserved %d -> %d baseline tore down %d sessions; want 1", pidA, pidB, n)
	}
	assertSerenaSessionGone(t, s, sessions, sid)
}

func TestSerenaRouter_BackendLoss_ConfirmedIdleWakeWithTwoSessionsLetsReconcileEstablishPID(t *testing.T) {
	withSerenaIdleReconcileGlobals(t)
	withTempSerenaStateRoot(t)

	daemon := newFakeSerenaDaemon("idle-two-sessions")
	ts := newSafeSerenaHTTPTestServer(t, daemon.handler())
	port := testServerPort(t, ts)

	const wsPath = "/proj/idle-two-sessions"
	const pidA = 1000
	const pidB = 2000
	ws := serenaWS("idle-two-sessions", wsPath, port)
	sessions := NewInMemorySessionRouter()
	wakeCalls := 0
	deps := &serenaRouterDeps{
		Resolver: &listerStubResolver{
			stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}},
			list:         []*api.WorkspaceEntry{ws},
		},
		Sessions:        sessions,
		UpstreamURLFn:   func(*api.WorkspaceEntry) string { return ts.URL },
		AuditFn:         func(string, string, map[string]any) error { return nil },
		UpstreamTimeout: 2 * time.Second,
		WakeIdleFn: func(_ context.Context, taskName string, _ int, _ string) error {
			wakeCalls++
			allowed, err := api.NewAPI().ClearStopIntentIfReason(taskName, api.IntentReasonIdle, "test-wake")
			if err != nil {
				return err
			}
			if !allowed {
				t.Fatalf("wake did not clear active idle stop for %s", taskName)
			}
			return nil
		},
	}
	s := newSerenaTestServer(t, deps)
	seedBoundSerenaSession(s, sessions, ws, "sid-a", "daemon-before-idle-a")
	seedBoundSerenaSession(s, sessions, ws, "sid-b", "daemon-before-idle-b")
	seedSerenaBackendBaseline(s, wsPath, pidA)
	seedSerenaBackendIdleMarker(s, wsPath, 2)
	s.serenaDaemonSessions.unbindWorkspace(ws.WorkspaceKey)
	if err := api.NewAPI().WriteSerenaIdleStop(ws.TaskName, time.Now().UTC()); err != nil {
		t.Fatalf("seed idle stop: %v", err)
	}
	serenaBackendStatusFn = func(context.Context) ([]api.DaemonStatus, error) {
		return nil, nil
	}

	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": wsPath + "/src/main.py"})
	if rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": "sid-a"}); rr.Code != http.StatusOK {
		t.Fatalf("idle-wake request status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if wakeCalls != 1 {
		t.Fatalf("WakeIdleFn calls = %d, want 1", wakeCalls)
	}
	if _, ok := serenaBackendBaselineForTest(s, wsPath); ok {
		t.Fatalf("confirmed idle wake left pre-idle PID baseline for %q; want it deleted before reconcile", wsPath)
	}
	if _, ok := serenaBackendIdleMarkerForTest(s, wsPath); ok {
		t.Fatalf("confirmed idle wake left idle marker for %q; want it deleted", wsPath)
	}
	if got := s.serenaRouterSessions.sessionsForWorkspace(ws.WorkspaceKey); len(got) != 2 {
		t.Fatalf("confirmed idle wake changed router session count = %d (%v), want 2", len(got), got)
	}

	serenaBackendStatusFn = func(context.Context) ([]api.DaemonStatus, error) {
		return []api.DaemonStatus{
			{Server: "serena", Workspace: wsPath, TaskName: ws.TaskName, State: "Running", PID: pidB, Port: port},
		}, nil
	}
	if n := s.ReconcileSerenaBackendLossViaIPC(context.Background()); n != 0 {
		t.Fatalf("first post-wake reconcile tore down %d sessions; want 0 and baseline establishment", n)
	}
	if got, ok := serenaBackendBaselineForTest(s, wsPath); !ok || got != pidB {
		t.Fatalf("post-wake reconcile baseline = (%d,%v), want (%d,true)", got, ok, pidB)
	}
	for _, sid := range []string{"sid-a", "sid-b"} {
		if !s.serenaRouterSessions.known(sid) {
			t.Fatalf("router session %q was torn down by confirmed idle wake", sid)
		}
		if got := sessions.LookupSession(sid); got == nil || got.WorkspaceKey != ws.WorkspaceKey {
			t.Fatalf("sticky session %q = %+v, want workspace %q", sid, got, ws.WorkspaceKey)
		}
	}
	if got := s.serenaRouterSessions.sessionsForWorkspace(ws.WorkspaceKey); len(got) != 2 {
		t.Fatalf("workspace sessions after post-wake reconcile = %v, want two sessions", got)
	}
}

func newSafeSerenaHTTPTestServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	for i := 0; i < 32; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen on ephemeral loopback port: %v", err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		if port >= 9121 && port <= 9299 {
			_ = ln.Close()
			continue
		}
		ts := httptest.NewUnstartedServer(h)
		ts.Listener = ln
		ts.Start()
		t.Cleanup(ts.Close)
		return ts
	}
	t.Fatal("could not allocate an httptest port outside 9121-9299")
	return nil
}

func postSerenaOnGUIOrigin(t *testing.T, s *Server, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/serena/mcp", bytes.NewReader(body))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", "http://127.0.0.1:9300")
	if _, hasCT := headers["Content-Type"]; !hasCT {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	return rr
}

func TestSerenaRouter_IdleShutdownWakeReconcilesAndRehandshakes(t *testing.T) {
	withSerenaIdleReconcileGlobals(t)
	withTempSerenaStateRoot(t)

	daemon := newFakeSerenaDaemon("postwake")
	ts := newSafeSerenaHTTPTestServer(t, daemon.handler())
	port := testServerPort(t, ts)

	const wsPath = "/proj/coupled-alpha"
	const sid = "sid-coupled-alpha"
	const pidA = 3333
	const pidB = 4444
	ws := serenaWS("coupled-alpha", wsPath, port)
	sessions := NewInMemorySessionRouter()
	wakeCalls := 0
	deps := &serenaRouterDeps{
		Resolver: &listerStubResolver{
			stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}},
			list:         []*api.WorkspaceEntry{ws},
		},
		Sessions:        sessions,
		UpstreamURLFn:   func(*api.WorkspaceEntry) string { return ts.URL },
		AuditFn:         func(string, string, map[string]any) error { return nil },
		UpstreamTimeout: 2 * time.Second,
		WakeIdleFn: func(context.Context, string, int, string) error {
			wakeCalls++
			return nil
		},
	}
	s := NewServer(Config{Port: 9300, Version: "test", PID: 1})
	s.SetSerenaRouterDeps(deps)
	seedBoundSerenaSession(s, sessions, ws, sid, "stale-daemon-session-before-idle")
	seedSerenaBackendBaseline(s, wsPath, pidA)

	statusRow := api.DaemonStatus{Server: "serena", Workspace: wsPath, TaskName: ws.TaskName, State: "Running", PID: pidA, Port: port, UptimeSec: 7200}
	serenaBackendStatusFn = func(context.Context) ([]api.DaemonStatus, error) {
		return []api.DaemonStatus{statusRow}, nil
	}
	serenaIdleThresholdFn = func() (time.Duration, bool) { return 30 * time.Minute, true }
	serenaIdleStopFn = api.NewAPI().WriteSerenaIdleStop

	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	s.recordSerenaActivity(ws.WorkspaceKey, now.Add(-45*time.Minute))
	if n := s.SweepIdleSerenaDaemons(context.Background(), now); n != 1 {
		t.Fatalf("idle sweep stopped %d daemons; want 1", n)
	}
	if _, _, _, ok := s.serenaDaemonSessions.bindingFor(sid); ok {
		t.Fatalf("idle sweep left stale daemon-session binding for %q", sid)
	}
	assertSerenaSessionLive(t, s, sessions, ws.WorkspaceKey, sid)

	statusRow = api.DaemonStatus{Server: "serena", Workspace: wsPath, TaskName: ws.TaskName, State: "Stopped", PID: 0, StalePID: 0, Port: port}
	if n := s.ReconcileSerenaBackendLossViaIPC(context.Background()); n != 0 {
		t.Fatalf("idle-stopped reconcile tore down %d sessions; want 0", n)
	}
	assertSerenaSessionLive(t, s, sessions, ws.WorkspaceKey, sid)

	clearAllowed, err := api.NewAPI().ClearStopIntentIfReason(ws.TaskName, api.IntentReasonIdle, "test")
	if err != nil || !clearAllowed {
		t.Fatalf("clear idle stop = (%v,%v), want (nil,true)", err, clearAllowed)
	}
	statusRow = api.DaemonStatus{Server: "serena", Workspace: wsPath, TaskName: ws.TaskName, State: "Running", PID: pidB, Port: port}
	if n := s.ReconcileSerenaBackendLossViaIPC(context.Background()); n != 0 {
		t.Fatalf("post-wake PID reconcile tore down %d sessions; want 0", n)
	}
	assertSerenaSessionLive(t, s, sessions, ws.WorkspaceKey, sid)
	if got, ok := serenaBackendBaselineForTest(s, wsPath); !ok || got != pidB {
		t.Fatalf("post-wake baseline = (%d,%v), want (%d,true)", got, ok, pidB)
	}

	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": wsPath + "/src/main.py"})
	rr := postSerenaOnGUIOrigin(t, s, body, map[string]string{"Mcp-Session-Id": sid})
	if rr.Code != http.StatusOK {
		t.Fatalf("post-wake request status = %d, want 200 after re-handshake; body=%s", rr.Code, rr.Body.String())
	}
	assertSerenaSessionLive(t, s, sessions, ws.WorkspaceKey, sid)
	if dsid, _, ok := s.serenaDaemonSessions.lookup(sid, ws.WorkspaceKey); !ok || dsid == "stale-daemon-session-before-idle" {
		t.Fatalf("post-wake daemon-session lookup = (%q,%v), want fresh re-handshaken id", dsid, ok)
	}

	daemon.mu.Lock()
	mintCount := daemon.mintCount
	lastToolSession := daemon.lastToolSession
	daemon.mu.Unlock()
	if mintCount != 1 {
		t.Fatalf("daemon initialize count = %d, want 1 fresh handshake after idle wake", mintCount)
	}
	if lastToolSession == "" || lastToolSession == "stale-daemon-session-before-idle" {
		t.Fatalf("forward used daemon session %q, want fresh post-wake session", lastToolSession)
	}
	if wakeCalls != 1 {
		t.Fatalf("WakeIdleFn calls = %d, want 1 request-path wake check", wakeCalls)
	}
}

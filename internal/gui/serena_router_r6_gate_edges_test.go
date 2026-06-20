package gui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

type revalidateAfterGateResolver struct {
	mu      sync.Mutex
	stale   *api.WorkspaceEntry
	fresh   *api.WorkspaceEntry
	calls   int
	onFirst func()
}

func (r *revalidateAfterGateResolver) ResolveByPath(string) (*api.WorkspaceEntry, error) {
	r.mu.Lock()
	r.calls++
	call := r.calls
	var ws *api.WorkspaceEntry
	if call == 1 {
		ws = r.stale
	} else {
		ws = r.fresh
	}
	onFirst := r.onFirst
	r.mu.Unlock()
	if call == 1 && onFirst != nil {
		onFirst()
	}
	if ws == nil {
		return nil, ErrWorkspaceNotFound
	}
	return ws, nil
}

func (r *revalidateAfterGateResolver) ListWorkspaces() []*api.WorkspaceEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls == 0 && r.stale != nil {
		return []*api.WorkspaceEntry{r.stale}
	}
	if r.fresh != nil {
		return []*api.WorkspaceEntry{r.fresh}
	}
	return nil
}

func TestSerenaRouter_RevalidatesWorkspaceAfterGateEntry(t *testing.T) {
	stale := serenaWS("revalidate-stale", "/proj/revalidate", 0)
	fresh := serenaWS("revalidate-fresh", "/proj/revalidate", 0)

	var staleHits int
	staleDaemon := newFakeSerenaDaemon("revalidate-stale")
	staleDaemon.tool = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		staleHits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"stale"}]}}`))
	}
	staleTS := httptest.NewServer(staleDaemon.handler())
	t.Cleanup(staleTS.Close)
	stale.Port = testServerPort(t, staleTS)

	var freshHits int
	freshDaemon := newFakeSerenaDaemon("revalidate-fresh")
	freshDaemon.tool = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		freshHits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"fresh"}]}}`))
	}
	freshTS := httptest.NewServer(freshDaemon.handler())
	t.Cleanup(freshTS.Close)
	fresh.Port = testServerPort(t, freshTS)

	resolver := &revalidateAfterGateResolver{stale: stale, fresh: fresh}
	deps := &serenaRouterDeps{
		Resolver: resolver,
		Sessions: NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string {
			switch {
			case ws != nil && ws.WorkspaceKey == stale.WorkspaceKey:
				return staleTS.URL
			case ws != nil && ws.WorkspaceKey == fresh.WorkspaceKey:
				return freshTS.URL
			default:
				return ""
			}
		},
		AuditFn: func(string, string, map[string]any) error { return nil },
	}
	s := newSerenaTestServer(t, deps)
	resolver.onFirst = func() {
		if !s.beginSerenaPrune(stale.WorkspaceKey) {
			t.Fatalf("precondition: could not start prune for %s", stale.WorkspaceKey)
		}
		s.endSerenaPrune(stale.WorkspaceKey)
	}

	sid := mintRouterSession(t, s, "2025-11-25")
	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/revalidate/src/main.go"})
	rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": sid})
	if rr.Code != http.StatusOK {
		t.Fatalf("tool call status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if staleHits != 0 {
		t.Fatalf("stale workspace daemon was hit %d time(s); want 0 after post-gate revalidation", staleHits)
	}
	if freshHits != 1 {
		t.Fatalf("fresh workspace daemon hits = %d, want 1", freshHits)
	}
}

func TestRouterToolsList_WakeSkipsCandidateInActivePhase(t *testing.T) {
	blocked := serenaWS("tools-list-wake-active-phase", "/proj/tools-list-wake-active-phase", 9341)
	live := serenaWS("tools-list-wake-live", "/proj/tools-list-wake-live", 9342)

	origPortLive := serenaToolsListPortLiveFn
	serenaToolsListPortLiveFn = func(_ context.Context, port int) bool {
		return port == live.Port
	}
	t.Cleanup(func() { serenaToolsListPortLiveFn = origPortLive })

	var mu sync.Mutex
	var wakeOrder []string
	liveWoke := make(chan struct{})
	var liveOnce sync.Once
	deps := &serenaRouterDeps{
		Resolver: &listerStubResolver{
			stubResolver: stubResolver{entries: []*api.WorkspaceEntry{blocked, live}},
			list:         []*api.WorkspaceEntry{blocked, live},
		},
		Sessions: NewInMemorySessionRouter(),
		AuditFn:  func(string, string, map[string]any) error { return nil },
		WakeIdleFn: func(_ context.Context, taskName string, _ int, _ string) error {
			mu.Lock()
			wakeOrder = append(wakeOrder, taskName)
			mu.Unlock()
			if taskName == live.TaskName {
				liveOnce.Do(func() { close(liveWoke) })
			}
			return nil
		},
	}
	s := newSerenaTestServer(t, deps)

	if !s.beginSerenaIdleStop(blocked.WorkspaceKey) {
		t.Fatalf("precondition: could not hold idle-stop gate for %s", blocked.WorkspaceKey)
	}
	gateHeld := true
	defer func() {
		if gateHeld {
			s.endSerenaIdleStop(blocked.WorkspaceKey)
		}
	}()

	req := httptest.NewRequest(http.MethodPost, "/serena/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	done := make(chan map[string]struct{}, 1)
	go func() {
		done <- s.wakeOneSerenaCandidateForToolsList(req, deps, []*api.WorkspaceEntry{blocked, live}, deps.AuditFn)
	}()

	select {
	case <-liveWoke:
	case <-time.After(300 * time.Millisecond):
		s.endSerenaIdleStop(blocked.WorkspaceKey)
		gateHeld = false
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatalf("tools/list wake blocked on active-phase candidate %s instead of skipping to %s", blocked.WorkspaceKey, live.WorkspaceKey)
	}

	excluded := <-done
	if _, ok := excluded[blocked.TaskName]; !ok {
		t.Fatalf("active-phase candidate %s was not excluded from fetch candidates: %v", blocked.TaskName, excluded)
	}
	mu.Lock()
	gotOrder := append([]string(nil), wakeOrder...)
	mu.Unlock()
	if len(gotOrder) != 1 || gotOrder[0] != live.TaskName {
		t.Fatalf("wake order = %v, want only %s", gotOrder, live.TaskName)
	}
	s.endSerenaIdleStop(blocked.WorkspaceKey)
	gateHeld = false
}

func TestRouterToolsList_WakePruneCandidateExcludedFromFetch(t *testing.T) {
	pruned := serenaWS("tools-list-wake-pruned", "/proj/tools-list-wake-pruned", 9343)
	live := serenaWS("tools-list-wake-pruned-live", "/proj/tools-list-wake-pruned-live", 9344)

	var prunedHits int
	prunedDaemon := newFakeSerenaDaemon("tools-list-wake-pruned")
	prunedDaemon.tool = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		prunedHits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"forbidden_pruned_tool"}]}}`))
	}
	prunedTS := httptest.NewServer(prunedDaemon.handler())
	t.Cleanup(prunedTS.Close)
	pruned.Port = testServerPort(t, prunedTS)

	liveDaemon := newFakeSerenaDaemon("tools-list-wake-pruned-live")
	liveDaemon.tool = func(w http.ResponseWriter, _ *http.Request, b []byte) {
		if !strings.Contains(string(b), `"tools/list"`) {
			t.Errorf("upstream body did not carry tools/list; got %s", string(b))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"find_symbol"}]}}`))
	}
	liveTS := httptest.NewServer(liveDaemon.handler())
	t.Cleanup(liveTS.Close)
	live.Port = testServerPort(t, liveTS)

	origPortLive := serenaToolsListPortLiveFn
	serenaToolsListPortLiveFn = func(_ context.Context, port int) bool {
		return port == live.Port
	}
	t.Cleanup(func() { serenaToolsListPortLiveFn = origPortLive })

	deps := &serenaRouterDeps{
		Resolver: &listerStubResolver{
			stubResolver: stubResolver{entries: []*api.WorkspaceEntry{pruned, live}},
			list:         []*api.WorkspaceEntry{pruned, live},
		},
		Sessions: NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string {
			if ws != nil && ws.WorkspaceKey == pruned.WorkspaceKey {
				return prunedTS.URL
			}
			return liveTS.URL
		},
		AuditFn:    func(string, string, map[string]any) error { return nil },
		WakeIdleFn: func(context.Context, string, int, string) error { return nil },
	}
	s := newSerenaTestServer(t, deps)
	if !s.beginSerenaPrune(pruned.WorkspaceKey) {
		t.Fatalf("precondition: could not hold prune gate for %s", pruned.WorkspaceKey)
	}
	gateHeld := true
	defer func() {
		if gateHeld {
			s.endSerenaPrune(pruned.WorkspaceKey)
		}
	}()

	sid := mintRouterSession(t, s, "2025-11-25")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	}()

	select {
	case rr := <-done:
		if rr.Code != http.StatusOK {
			t.Fatalf("tools/list status = %d, want 200 from live candidate; body=%s", rr.Code, rr.Body.String())
		}
		assertToolsListNames(t, rr.Body.Bytes(), []string{"find_symbol"})
	case <-time.After(300 * time.Millisecond):
		s.endSerenaPrune(pruned.WorkspaceKey)
		gateHeld = false
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatalf("tools/list wake blocked on pruned candidate %s instead of dropping it and using %s", pruned.WorkspaceKey, live.WorkspaceKey)
	}
	if prunedHits != 0 {
		t.Fatalf("pruned candidate was fetched %d time(s); want 0", prunedHits)
	}
	s.endSerenaPrune(pruned.WorkspaceKey)
	gateHeld = false
}

func TestRouterToolsList_RewakesCandidateIdleStoppedAfterWake(t *testing.T) {
	ws := serenaWS("tools-list-rewake-after-idle-stop", "/proj/tools-list-rewake-after-idle-stop", 9345)

	var s *Server
	var mu sync.Mutex
	awake := false
	wakeCalls := 0
	var stopOnce sync.Once

	daemon := newFakeSerenaDaemon("tools-list-rewake-after-idle-stop")
	daemon.tool = func(w http.ResponseWriter, _ *http.Request, b []byte) {
		if !strings.Contains(string(b), `"tools/list"`) {
			t.Errorf("upstream body did not carry tools/list; got %s", string(b))
		}
		mu.Lock()
		up := awake
		mu.Unlock()
		if !up {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"find_symbol"}]}}`))
	}
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)
	ws.Port = testServerPort(t, ts)

	origPortLive := serenaToolsListPortLiveFn
	serenaToolsListPortLiveFn = func(_ context.Context, port int) bool {
		return port == ws.Port
	}
	t.Cleanup(func() { serenaToolsListPortLiveFn = origPortLive })

	deps := &serenaRouterDeps{
		Resolver: &listerStubResolver{
			stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}},
			list:         []*api.WorkspaceEntry{ws},
		},
		Sessions: NewInMemorySessionRouter(),
		UpstreamURLFn: func(*api.WorkspaceEntry) string {
			mu.Lock()
			wakes := wakeCalls
			mu.Unlock()
			if wakes == 1 {
				stopOnce.Do(func() {
					mu.Lock()
					awake = false
					mu.Unlock()
					if !s.beginSerenaIdleStop(ws.WorkspaceKey) {
						t.Errorf("precondition: could not start idle-stop for %s", ws.WorkspaceKey)
						return
					}
					go func() {
						time.Sleep(50 * time.Millisecond)
						s.endSerenaIdleStop(ws.WorkspaceKey)
					}()
				})
			}
			return ts.URL
		},
		AuditFn: func(string, string, map[string]any) error { return nil },
		WakeIdleFn: func(context.Context, string, int, string) error {
			mu.Lock()
			wakeCalls++
			awake = true
			mu.Unlock()
			return nil
		},
	}
	s = newSerenaTestServer(t, deps)

	sid := mintRouterSession(t, s, "2025-11-25")
	rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/list status = %d, want 200 after re-wake; body=%s", rr.Code, rr.Body.String())
	}
	assertToolsListNames(t, rr.Body.Bytes(), []string{"find_symbol"})
	mu.Lock()
	gotWakeCalls := wakeCalls
	mu.Unlock()
	if gotWakeCalls != 2 {
		t.Fatalf("WakeIdleFn calls = %d, want 2 (initial wake plus re-wake after waited idle-stop)", gotWakeCalls)
	}
}

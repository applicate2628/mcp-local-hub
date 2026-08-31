// internal/cli/route_session_expiry_test.go
//
// Regression coverage for the codex bot PR #588 P2 finding that MCP sessions
// created by the STANDALONE `mcphub route` front daemon were never expired.
//
// The daemon builds its own serena and LSP SessionRouters (buildRouteServer),
// but the sweeps that expire sessions live in the GUI's lifecycle and drive
// the GUI's own routers. Nothing expired these, so a supervisor-managed,
// always-on route daemon accumulated one binding per MCP session for its
// entire uptime — unbounded growth in exactly the process designed to run
// forever.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/gui"

	"github.com/spf13/cobra"
)

// TestRouteDaemon_SessionStoresAreReachableForExpiry pins the structural
// precondition: the route daemon's session routers must be handed back by its
// construction path at all. Before the fix they were local variables that fell
// out of scope, so no caller could ever sweep them.
func TestRouteDaemon_SessionStoresAreReachableForExpiry(t *testing.T) {
	redirectMCPFrontTestEnv(t)

	_, stores, err := buildRouteServer(&cobra.Command{}, 0)
	if err != nil {
		t.Fatalf("buildRouteServer: %v", err)
	}
	if stores == nil {
		t.Fatalf("buildRouteServer returned no session stores; the route daemon's session maps are unreachable and therefore un-expirable")
	}
	if stores.serena == nil {
		t.Fatalf("the route daemon's serena session router was not handed back, so nothing can expire it")
	}
	// The LSP router is only wired when the mcp-language-server manifest loads
	// and parses. It is embedded, so it should be wired here — and if it ever
	// is not, the sweep wiring must not be what silently breaks.
	if stores.lsp == nil {
		t.Fatalf("the route daemon's LSP session router was not handed back; the embedded mcp-language-server manifest should always wire it")
	}
}

// TestRouteDaemon_SessionExpiryActuallyReclaimsBoundSessions drives the real
// wiring (runRouteSessionExpiry, the same function runRoute calls) and proves
// a bound session is actually reclaimed.
//
// interval and ttl are compressed so the sweep completes inside the test; the
// production call passes sessionCleanupInterval and the 24h idle TTL.
func TestRouteDaemon_SessionExpiryActuallyReclaimsBoundSessions(t *testing.T) {
	redirectMCPFrontTestEnv(t)

	s, stores, err := buildRouteServer(&cobra.Command{}, 0)
	if err != nil {
		t.Fatalf("buildRouteServer: %v", err)
	}

	// Bind one serena session and one LSP session, the way a client handshake
	// does.
	stores.serena.BindSession("serena-session-1", &api.WorkspaceEntry{
		WorkspaceKey: "abcd1234", WorkspacePath: "/tmp/project", Language: "go",
	})
	stores.lsp.EnsureSession("lsp-session-1")
	if stores.serena.Len() == 0 {
		t.Fatalf("test precondition broken: no serena session was bound, so expiry cannot be observed")
	}
	if stores.lsp.Len() == 0 {
		t.Fatalf("test precondition broken: no LSP session was bound, so expiry cannot be observed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// ttl=0 makes every existing binding immediately idle-expired, so the very
	// next tick must reclaim it.
	runRouteSessionExpiry(ctx, s, stores, 5*time.Millisecond, 0)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if stores.serena.Len() == 0 && stores.lsp.Len() == 0 {
			return // both reclaimed
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the route daemon's sessions were never expired (serena=%d lsp=%d after 5s of sweeps); a long-lived route daemon grows these maps for its entire uptime",
		stores.serena.Len(), stores.lsp.Len())
}

// TestRouteDaemon_SessionExpiryStopsWithContext proves the sweeps cannot
// outlive the daemon: they are started as bare goroutines, so ctx is the only
// thing that stops them.
func TestRouteDaemon_SessionExpiryStopsWithContext(t *testing.T) {
	redirectMCPFrontTestEnv(t)

	s, stores, err := buildRouteServer(&cobra.Command{}, 0)
	if err != nil {
		t.Fatalf("buildRouteServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runRouteSessionExpiry(ctx, s, stores, 5*time.Millisecond, 0)
	cancel()

	// After cancellation a newly-bound session must survive: no sweeper is
	// running any more.
	time.Sleep(50 * time.Millisecond)
	stores.serena.BindSession("post-cancel", &api.WorkspaceEntry{
		WorkspaceKey: "abcd1234", WorkspacePath: "/tmp/project", Language: "go",
	})
	time.Sleep(50 * time.Millisecond)
	if stores.serena.Len() == 0 {
		t.Fatalf("a sweep ran after its context was cancelled; the route daemon's expiry goroutines must stop with the daemon")
	}
}

// TestRouteDaemon_BackendLossReconcileUsesRouteOwnedStores drives the route
// construction's own Server, sticky router, and reconciler. It verifies the
// route process observes first-generation and transient-status states without
// teardown, then removes the route-owned client/sticky state only when the
// supervisor confirms a replacement generation. The GUI package's lifecycle
// tests cover the same reconciler's private router+daemon-store teardown; this
// test proves `mcphub route` composes that one owner for ITS stores.
func TestRouteDaemon_BackendLossReconcileUsesRouteOwnedStores(t *testing.T) {
	tmp := redirectMCPFrontTestEnv(t)

	var daemonMu sync.Mutex
	issued := map[string]bool{}
	mintCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
			Params struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch request.Method {
		case "initialize":
			daemonMu.Lock()
			mintCount++
			daemonSID := "route-daemon-" + strconv.Itoa(mintCount)
			issued[daemonSID] = true
			daemonMu.Unlock()
			w.Header().Set("Mcp-Session-Id", daemonSID)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","serverInfo":{"name":"serena","version":"test"},"capabilities":{"tools":{}}}}`))
			return
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		}
		daemonMu.Lock()
		known := issued[r.Header.Get("Mcp-Session-Id")]
		daemonMu.Unlock()
		if !known {
			http.Error(w, "unknown daemon session", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	t.Cleanup(upstream.Close)
	parsedUpstream, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	upstreamPort, err := strconv.Atoi(parsedUpstream.Port())
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}

	workspace := i6MakeSerenaWorkspace(t, filepath.Join(tmp, "workspace-root"), "RouteReconcile")
	toolFile := i6WriteWorkspaceFile(t, workspace, "src", "main.go")
	registryPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("resolve registry path: %v", err)
	}
	registry := api.NewRegistry(registryPath)
	entry := api.WorkspaceEntry{
		WorkspaceKey:  api.WorkspaceKey(workspace),
		WorkspacePath: workspace,
		Language:      api.SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          upstreamPort,
		TaskName:      "mcp-local-hub-serena-route-reconcile",
		RegisteredAt:  time.Date(2026, 8, 31, 16, 0, 0, 0, time.UTC),
	}
	if err := registry.PutSerena(entry); err != nil {
		t.Fatalf("put serena workspace: %v", err)
	}
	if err := registry.Save(); err != nil {
		t.Fatalf("save registry: %v", err)
	}

	s, stores, err := buildRouteServer(&cobra.Command{}, mcpFrontI6TestPort)
	if err != nil {
		t.Fatalf("buildRouteServer: %v", err)
	}
	activityCommits := 0
	s.SetSerenaActivityCommitterForTest(func(_ context.Context, request api.SerenaActivityCommitRequestV1) (api.SerenaActivityCommitReceiptV1, error) {
		activityCommits++
		if request.LegacyGenerationUnspecified || !request.RegisteredAt.Equal(entry.RegisteredAt) {
			t.Fatalf("route commit request = %+v", request)
		}
		return api.SerenaActivityCommitReceiptV1{ProtocolVersion: 1, WorkspaceKey: request.WorkspaceKey, TaskName: request.TaskName, RegisteredAt: request.RegisteredAt, ActivityAt: request.ActivityAt, State: "committed"}, nil
	})

	var statusMu sync.Mutex
	statusCalls := 0
	currentPID := 1000
	statusErr := error(nil)
	var auditEvents []string
	previousDial := routeDialSupervisorIPCStatus
	previousAudit := routeLifecycleAuditFn
	routeDialSupervisorIPCStatus = func(context.Context) ([]api.DaemonStatus, error) {
		statusMu.Lock()
		defer statusMu.Unlock()
		statusCalls++
		if statusErr != nil {
			return nil, statusErr
		}
		return []api.DaemonStatus{{
			Server: "serena", Workspace: workspace, TaskName: entry.TaskName, State: "Running", PID: currentPID, Port: upstreamPort,
		}}, nil
	}
	routeLifecycleAuditFn = func(level, event string, fields map[string]any) error {
		statusMu.Lock()
		auditEvents = append(auditEvents, event)
		statusMu.Unlock()
		return nil
	}
	t.Cleanup(func() {
		routeDialSupervisorIPCStatus = previousDial
		routeLifecycleAuditFn = previousAudit
		gui.SetSerenaBackendStatusFn(nil)
	})

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`
	initRR := routePost(t, s, initialize, "")
	if initRR.Code != http.StatusOK {
		t.Fatalf("route initialize status = %d, want 200; body=%s", initRR.Code, initRR.Body.String())
	}
	clientSID := initRR.Header().Get("Mcp-Session-Id")
	if clientSID == "" {
		t.Fatal("route initialize did not mint a client session")
	}
	toolRR := routePost(t, s, i6BuildToolCallBody(t, "find_symbol", map[string]any{"relative_path": toolFile}), clientSID)
	if toolRR.Code != http.StatusOK {
		t.Fatalf("route tool call status = %d, want 200; body=%s", toolRR.Code, toolRR.Body.String())
	}
	if stores.serena.Len() != 1 {
		t.Fatalf("route sticky store length = %d, want 1 after a route-owned session bind", stores.serena.Len())
	}
	if activityCommits != 1 {
		t.Fatalf("route activity commits = %d, want 1 before forwarded tool call", activityCommits)
	}

	ctx, cancel := context.WithCancel(context.Background())
	startRouteSerenaBackendLossReconcile(ctx, s, 5*time.Millisecond)
	waitRouteCondition(t, "first route status observation", func() bool {
		statusMu.Lock()
		defer statusMu.Unlock()
		return statusCalls >= 1
	})
	if stores.serena.Len() != 1 {
		t.Fatalf("first status observation tore down route-owned sticky store; length=%d want=1", stores.serena.Len())
	}

	statusMu.Lock()
	statusErr = errors.New("transient supervisor status read failure")
	callsBeforeError := statusCalls
	statusMu.Unlock()
	waitRouteCondition(t, "transient route status read", func() bool {
		statusMu.Lock()
		defer statusMu.Unlock()
		return statusCalls > callsBeforeError
	})
	if stores.serena.Len() != 1 {
		t.Fatalf("transient status read error tore down route-owned stores; length=%d want=1", stores.serena.Len())
	}
	statusMu.Lock()
	if len(auditEvents) == 0 || auditEvents[len(auditEvents)-1] != "serena-supervisor-status-read-failed" {
		statusMu.Unlock()
		t.Fatalf("status-read failure audit = %v, want redacted structured lifecycle event", auditEvents)
	}
	statusErr = nil
	currentPID = 2000
	statusMu.Unlock()
	waitRouteCondition(t, "confirmed route daemon PID replacement", func() bool { return stores.serena.Len() == 0 })

	// The known client session is gone too, proving the route process used its
	// own Server stores rather than any GUI-owned router instance.
	terminated := routePost(t, s, i6BuildToolCallBody(t, "list_memories", map[string]any{}), clientSID)
	if terminated.Code == http.StatusOK {
		cancel()
		t.Fatalf("route session survived confirmed daemon generation change; body=%s", terminated.Body.String())
	}

	cancel()
	statusMu.Lock()
	callsAtCancel := statusCalls
	statusMu.Unlock()
	time.Sleep(40 * time.Millisecond)
	statusMu.Lock()
	callsAfterCancel := statusCalls
	statusMu.Unlock()
	if callsAfterCancel != callsAtCancel {
		t.Fatalf("route reconcile ticker continued after context cancellation: calls before=%d after=%d", callsAtCancel, callsAfterCancel)
	}
}

func routePost(t *testing.T, s *gui.Server, body, sessionID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/serena/mcp", strings.NewReader(body))
	req.Host = "127.0.0.1:19321"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", "http://127.0.0.1:19321")
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	rr := httptest.NewRecorder()
	s.RouteHandler().ServeHTTP(rr, req)
	return rr
}

func waitRouteCondition(t *testing.T, name string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", name)
}

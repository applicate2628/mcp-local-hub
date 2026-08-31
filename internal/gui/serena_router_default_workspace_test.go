package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/serena_routing"
)

type defaultWorkspaceStubResolver struct {
	*stubResolver
	mu           sync.Mutex
	defaultWS    *api.WorkspaceEntry
	defaultErr   error
	defaultCalls int
}

func (r *defaultWorkspaceStubResolver) ResolveDefaultWorkspace() (*api.WorkspaceEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defaultCalls++
	if r.defaultErr != nil {
		return nil, r.defaultErr
	}
	if r.defaultWS == nil {
		return nil, nil
	}
	copy := *r.defaultWS
	return &copy, nil
}

func (r *defaultWorkspaceStubResolver) ListWorkspaces() []*api.WorkspaceEntry {
	r.stubResolver.mu.Lock()
	defer r.stubResolver.mu.Unlock()
	out := make([]*api.WorkspaceEntry, 0, len(r.stubResolver.entries))
	for _, entry := range r.stubResolver.entries {
		if entry == nil {
			continue
		}
		copy := *entry
		out = append(out, &copy)
	}
	return out
}

// TestSerenaRouter_PathlessFirstCallUsesConfiguredDefault proves the actual
// first-call contract: a router-minted session with no sticky workspace may
// use only the resolver's configured default, then is sticky for later calls.
func TestSerenaRouter_PathlessFirstCallUsesConfiguredDefault(t *testing.T) {
	root := t.TempDir()
	alphaPath := makeSerenaWorkspace(t, root, "Alpha")
	betaPath := makeSerenaWorkspace(t, root, "Beta")
	registeredAt := time.Date(2026, 8, 31, 21, 0, 0, 0, time.UTC)
	alpha := api.WorkspaceEntry{WorkspaceKey: api.WorkspaceKey(alphaPath), WorkspacePath: alphaPath, Port: 9301, TaskName: "serena-alpha", Backend: api.SerenaServerName, Language: api.SerenaLanguageSentinel, RegisteredAt: registeredAt}
	beta := api.WorkspaceEntry{WorkspaceKey: api.WorkspaceKey(betaPath), WorkspacePath: betaPath, Port: 9302, TaskName: "serena-beta", Backend: api.SerenaServerName, Language: api.SerenaLanguageSentinel, RegisteredAt: registeredAt}
	registryPath := filepath.Join(root, "workspaces.yaml")
	registry := api.NewRegistry(registryPath)
	if err := registry.PutSerena(alpha); err != nil {
		t.Fatalf("register alpha: %v", err)
	}
	if err := registry.PutSerena(beta); err != nil {
		t.Fatalf("register beta: %v", err)
	}
	if err := registry.Save(); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	if err := registry.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if err := api.WriteDefaultWorkspace(root, betaPath); err != nil {
		t.Fatalf("write default marker: %v", err)
	}
	intentPath := filepath.Join(root, "supervisor-intent.json")
	if err := api.WriteSupervisorIntent(intentPath, &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{
		{TaskName: alpha.TaskName, Workspace: alpha.WorkspacePath, Port: alpha.Port},
		{TaskName: beta.TaskName, Workspace: beta.WorkspacePath, Port: beta.Port},
	}}); err != nil {
		t.Fatalf("write supervisor intent: %v", err)
	}
	resolver := serena_routing.NewWorkspaceResolver(registry, registryPath)
	daemon := newFakeSerenaDaemon("beta")
	server := httptest.NewServer(daemon.handler())
	t.Cleanup(server.Close)

	var events []string
	autoRegisterCalls := 0
	var commitMu sync.Mutex
	var receipt api.SerenaActivityCommitReceiptV1
	var committed bool
	daemon.tool = func(w http.ResponseWriter, _ *http.Request, body []byte) {
		if bytes.Contains(body, []byte(`"get_current_config"`)) {
			fresh := api.NewRegistry(registryPath)
			if err := fresh.Load(); err != nil {
				t.Errorf("load registry at upstream forward: %v", err)
			} else if current, ok := fresh.GetSerena(beta.WorkspaceKey); !ok || current.LastToolsCallAt.IsZero() {
				t.Errorf("upstream reached before durable activity receipt; beta row=%+v found=%v", current, ok)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}
	deps := &serenaRouterDeps{
		Resolver:      resolver,
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(*api.WorkspaceEntry) string { return server.URL },
		AutoRegisterFn: func(_ context.Context, _ string) (*api.WorkspaceEntry, error) {
			autoRegisterCalls++
			return nil, fmt.Errorf("auto-register must not run for default resolution")
		},
		CommitSerenaActivityFn: func(_ context.Context, request api.SerenaActivityCommitRequestV1) (api.SerenaActivityCommitReceiptV1, error) {
			if request.WorkspaceKey != beta.WorkspaceKey || request.WorkspacePath != beta.WorkspacePath || request.TaskName != beta.TaskName || request.ExpectedPort != beta.Port || !request.RegisteredAt.Equal(registeredAt) || request.LegacyGenerationUnspecified || request.ActivityAt.IsZero() {
				t.Fatalf("activity request = %+v, want current beta generation", request)
			}
			actual, err := registry.CommitSerenaActivity(intentPath, request)
			if err != nil {
				return api.SerenaActivityCommitReceiptV1{}, err
			}
			commitMu.Lock()
			receipt, committed = actual, true
			commitMu.Unlock()
			return actual, nil
		},
		AuditFn: func(_ string, event string, _ map[string]any) error {
			events = append(events, event)
			return nil
		},
	}
	s := newSerenaTestServer(t, deps)
	sid := mintRouterSession(t, s, "2025-11-25")
	list := postSerena(t, s, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`), map[string]string{"Mcp-Session-Id": sid})
	if list.Code != http.StatusOK {
		t.Fatalf("tools/list status = %d, want 200; body=%s", list.Code, list.Body.String())
	}
	if got := deps.Sessions.LookupSession(sid); got != nil {
		t.Fatalf("tools/list created sticky binding %+v, want none before first tools/call", got)
	}

	first := postSerena(t, s, buildToolCallBody(t, "get_current_config", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	if first.Code != http.StatusOK {
		t.Fatalf("first pathless call status = %d, want 200; body=%s", first.Code, first.Body.String())
	}
	if got := deps.Sessions.LookupSession(sid); got == nil || got.WorkspaceKey != beta.WorkspaceKey || !got.RegisteredAt.Equal(registeredAt) {
		t.Fatalf("sticky binding = %+v, want default beta with authoritative generation", got)
	}
	commitMu.Lock()
	gotReceipt, gotCommitted := receipt, committed
	commitMu.Unlock()
	if !gotCommitted || gotReceipt.WorkspaceKey != beta.WorkspaceKey || !gotReceipt.RegisteredAt.Equal(registeredAt) || gotReceipt.ActivityAt.IsZero() {
		t.Fatalf("durable activity receipt = %+v committed=%v, want beta generation before forward", gotReceipt, gotCommitted)
	}
	if autoRegisterCalls != 0 {
		t.Fatalf("auto-register calls = %d, want 0 for a default-resolved pathless call", autoRegisterCalls)
	}
	foundBound := false
	for _, event := range events {
		if event == "serena-default-workspace-bound" {
			foundBound = true
		}
	}
	if !foundBound {
		t.Fatalf("audit events = %v, want serena-default-workspace-bound after sticky bind", events)
	}

	if err := api.WriteDefaultWorkspace(root, ""); err != nil {
		t.Fatalf("clear marker after sticky bind: %v", err)
	}
	second := postSerena(t, s, buildToolCallBody(t, "list_memories", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	if second.Code != http.StatusOK {
		t.Fatalf("second sticky call status = %d, want 200; body=%s", second.Code, second.Body.String())
	}
}

func TestSerenaRouter_PathlessDefaultErrorsAreTypedAndPathFree(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"not configured", serena_routing.ErrDefaultWorkspaceNotConfigured, "NOT_CONFIGURED"},
		{"stale", fmt.Errorf("wrapped marker %s: %w", "/private/default", serena_routing.ErrDefaultWorkspaceStale), "STALE"},
		{"unavailable", serena_routing.ErrDefaultWorkspaceUnavailable, "UNAVAILABLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolver := &defaultWorkspaceStubResolver{stubResolver: &stubResolver{}, defaultErr: tc.err}
			s := newSerenaTestServer(t, &serenaRouterDeps{Resolver: resolver, Sessions: NewInMemorySessionRouter()})
			sid := mintRouterSession(t, s, "2025-11-25")
			rr := postSerena(t, s, buildToolCallBody(t, "get_current_config", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body=%s", rr.Code, rr.Body.String())
			}
			var response struct {
				Error struct {
					Code int `json:"code"`
					Data struct {
						Code string `json:"code"`
					} `json:"data"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if response.Error.Code != -32003 || response.Error.Data.Code != tc.want {
				t.Fatalf("error = %+v, want -32003 / %s", response.Error, tc.want)
			}
			if strings.Contains(rr.Body.String(), "/private/default") {
				t.Fatalf("default-resolution response leaked marker path: %s", rr.Body.String())
			}
		})
	}
}

func TestSerenaRouter_PathlessDefaultDoesNotApplyToUnknownOrMissingSession(t *testing.T) {
	workspace := &api.WorkspaceEntry{WorkspaceKey: "default", WorkspacePath: "/work/default", Port: 9301}
	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"missing", nil},
		{"unknown", map[string]string{"Mcp-Session-Id": "not-minted-here"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolver := &defaultWorkspaceStubResolver{stubResolver: &stubResolver{entries: []*api.WorkspaceEntry{workspace}}, defaultWS: workspace}
			s := newSerenaTestServer(t, &serenaRouterDeps{Resolver: resolver, Sessions: NewInMemorySessionRouter()})
			rr := postSerena(t, s, buildToolCallBody(t, "get_current_config", map[string]any{}), tc.headers)
			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want preserved 503 missing-session shape; body=%s", rr.Code, rr.Body.String())
			}
			if resolver.defaultCalls != 0 {
				t.Fatalf("default resolver calls = %d, want 0 for %s session", resolver.defaultCalls, tc.name)
			}
		})
	}
}

func TestSerenaRouter_PathlessDefaultDoesNotReadMarkerBeforeProtocolGate(t *testing.T) {
	workspace := &api.WorkspaceEntry{WorkspaceKey: "default", WorkspacePath: "/work/default", Port: 9301}
	resolver := &defaultWorkspaceStubResolver{stubResolver: &stubResolver{entries: []*api.WorkspaceEntry{workspace}}, defaultWS: workspace}
	s := newSerenaTestServer(t, &serenaRouterDeps{Resolver: resolver, Sessions: NewInMemorySessionRouter()})
	sid := mintRouterSession(t, s, "2025-11-25")
	rr := postSerena(t, s, buildToolCallBody(t, "get_current_config", map[string]any{}), map[string]string{
		"Mcp-Session-Id":       sid,
		"MCP-Protocol-Version": "2025-06-18",
	})
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "protocol-version mismatch") {
		t.Fatalf("protocol mismatch response = %d %s, want preserved 400 mismatch", rr.Code, rr.Body.String())
	}
	if resolver.defaultCalls != 0 {
		t.Fatalf("default resolver calls = %d, want 0 before protocol validation", resolver.defaultCalls)
	}
}

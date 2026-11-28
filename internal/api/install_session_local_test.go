package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/config"
)

func sessionLocalPlanFixture(t *testing.T) (*config.ServerManifest, *sessionLocalInstallTarget) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	m := &config.ServerManifest{Name: "private-fixture", Kind: config.KindGlobal, Transport: config.TransportStdioBridge, Command: "fixture",
		Daemons:        []config.DaemonSpec{{Name: "default", Port: 9124, MCPStdioState: config.MCPStdioStateSessionLocal}},
		ClientBindings: []config.ClientBinding{{Client: "claude-code", Daemon: "default", URLPath: "/mcp"}, {Client: "cursor", Daemon: "default", URLPath: "/mcp"}}}
	target := &sessionLocalInstallTarget{Endpoint: HubEndpoint{Port: 9191, InstanceID: strings.Repeat("a", 64)},
		Epoch: MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetFront, Generation: 4, Port: 9192},
		Route: BuildBuiltinRouteDaemon("fixture", 9192), Tokens: HubTokenTable{Tokens: map[string]string{"claude-code": strings.Repeat("b", 64), "cursor": strings.Repeat("c", 64)}},
		Providers: []HubStdioEndpoint{SessionLocalStdioEndpoint(m, &m.Daemons[0])}}
	return m, target
}

func TestSessionLocalPreflightDoesNotProbeConfiguredTCPPort(t *testing.T) {
	m, _ := sessionLocalPlanFixture(t)
	prior := preflightPortInUse
	t.Cleanup(func() { preflightPortInUse = prior })
	calls := 0
	preflightPortInUse = func(int) bool { calls++; return true }
	for _, finding := range AdmissionCheck(m, scopeForPreflight("default")) {
		if finding.ID == "external-port-free" {
			t.Fatal("private provider treated as a TCP listener")
		}
	}
	if calls != 0 {
		t.Fatal("private admission probed an unrelated configured port")
	}
}

func TestSessionLocalMissingSharedRowDoesNotDiscardPendingStopWatermark(t *testing.T) {
	m, target := sessionLocalPlanFixture(t)
	task := canonicalIntentTaskKey(supervisorTaskNameForManifestDaemon(m.Name, "default"))
	stamp := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUninstalled, UpdatedAt: time.Now().UTC()}
	intent := &SupervisorIntentFile{Version: 1, Stops: map[string]DaemonIntent{task: stamp}}
	if _, err := sessionLocalLegacySelection(intent, target.Providers); err == nil {
		t.Fatal("missing descriptor acknowledged pending original closure")
	}
	// Existing ClearStop preserves the departed record as a watermark; restart
	// normalization must not silently make a private install admissible.
	intent.Stops = nil
	intent.LegacyStopWatermarks = map[string]DaemonIntent{task: stamp}
	normalizeAbsentOnlyStopWatermarks(intent)
	if _, err := sessionLocalLegacySelection(intent, target.Providers); err == nil {
		t.Fatal("stop clear/restart erased receiving uncertainty")
	}
	delete(intent.LegacyStopWatermarks, task)
	if legacy, err := sessionLocalLegacySelection(intent, target.Providers); err != nil || len(legacy) != 0 {
		t.Fatal("settled absence refused")
	}
}

func TestSessionLocalPlanNeverProducesSharedIntentOrDirectPort(t *testing.T) {
	m, target := sessionLocalPlanFixture(t)
	if p, err := BuildPlanWithOpts(m, BuildPlanOpts{IncludeAllClients: true}); !errors.Is(err, ErrSessionLocalInstallUnavailable) || p != nil {
		t.Fatalf("unreceived owner produced plan: %#v %v", p, err)
	}
	p, err := BuildPlanWithOpts(m, BuildPlanOpts{IncludeAllClients: true, sessionLocal: target})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.SupervisorIntent) != 0 || len(p.SchedulerTasks) != 0 || len(p.ClientUpdates) != 2 {
		t.Fatalf("private plan leaked a permanent backend: %#v", p)
	}
	for _, update := range p.ClientUpdates {
		want := "http://127.0.0.1:9191/clients/" + update.Client + "/mcp"
		if update.URL != want || update.Headers["X-Mcphub-Hub-Token"] != target.Tokens.Tokens[update.Client] {
			t.Fatalf("wrong registered binding: %#v", update)
		}
	}
	bindPlanSupervisorIntentManifestHash(p, strings.Repeat("d", 64))
	receipt := frozenInstallMutationReceipt(p, InstallOpts{Server: m.Name})
	if len(receipt.ExpectedTasks) != 1 || receipt.ExpectedTasks[0] != BuiltinRouteTaskName {
		t.Fatalf("readiness invented provider task: %#v", receipt.ExpectedTasks)
	}
	if p.manifestHash != strings.Repeat("d", 64) {
		t.Fatal("private plan lost raw manifest binding")
	}
}

func TestSessionLocalReconcileRefusesGateOffAndUnreceivedGateOn(t *testing.T) {
	m, target := sessionLocalPlanFixture(t)
	for _, opts := range []HubReconcileOpts{{GateOn: false}, {GateOn: true}} {
		plan, err := BuildHubReconcilePlan([]config.ServerManifest{*m}, target.Endpoint, target.Tokens, opts)
		if !errors.Is(err, ErrSessionLocalInstallUnavailable) || len(plan) != 0 {
			t.Fatalf("unsafe reconcile emitted writes: %#v %v", plan, err)
		}
	}
	plan, err := BuildHubReconcilePlan([]config.ServerManifest{*m}, target.Endpoint, target.Tokens, HubReconcileOpts{GateOn: true, privateAdmitted: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range plan {
		if strings.Contains(op.URL, ":9124") {
			t.Fatal("gate-on plan exposed private configured port")
		}
	}
}

func TestProjectPrivatePlanHashBindsIssuanceOnlyAtCredentialSlot(t *testing.T) {
	raw := map[string]any{"url": "http://127.0.0.1:9191/g/reasoning/mcp", "http_headers": map[string]any{"X-Mcphub-Hub-Token": projectCredentialPlanMarker, "X-Mcphub-Instance-Id": strings.Repeat("a", 64)}, "tool_timeout_sec": 90}
	want, err := privateDesiredPlanHash(raw)
	if err != nil {
		t.Fatal(err)
	}
	headers := raw["http_headers"].(map[string]any)
	headers["X-Mcphub-Hub-Token"] = strings.Repeat("b", 64)
	if got, err := privateDesiredPlanHash(raw); err != nil || got != want {
		t.Fatalf("issued credential did not bind original plan: %s %v", got, err)
	}
	raw["url"] = "http://127.0.0.1:9191/g/another-project/mcp"
	if got, err := privateDesiredPlanHash(raw); err != nil || got == want {
		t.Fatalf("scope change bypassed digest: %s %v", got, err)
	}
	headers["X-Mcphub-Hub-Token"] = "attacker-selected-placeholder"
	if _, err := privateDesiredPlanHash(raw); err == nil {
		t.Fatal("arbitrary deferred credential accepted")
	}
}

type sessionLocalAdmissionTransport struct {
	endpoint     HubStdioEndpoint
	awaitEntered chan struct{}
	allowReceipt chan struct{}
	once         sync.Once
}

func (p *sessionLocalAdmissionTransport) Allows(e HubStdioEndpoint) bool { return e == p.endpoint }
func (p *sessionLocalAdmissionTransport) RoundTrip(r *http.Request, lifetime context.Context, e HubStdioEndpoint, current func() bool) (*http.Response, error) {
	if !p.Allows(e) || !current() || lifetime.Err() != nil {
		return nil, errHubDaemonAdmission
	}
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return nil, err
	}
	status := http.StatusOK
	body := ""
	header := make(http.Header)
	switch request.Method {
	case "initialize":
		header.Set("Mcp-Session-Id", "private-provider")
		body = `{"jsonrpc":"2.0","id":` + string(request.ID) + `,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}}}}`
	case "notifications/initialized":
		status = http.StatusAccepted
	case "tools/list":
		body = `{"jsonrpc":"2.0","id":` + string(request.ID) + `,"result":{"tools":[{"name":"reason","inputSchema":{"type":"object"}}]}}`
	default:
		return nil, errHubDaemonAdmission
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
}
func (p *sessionLocalAdmissionTransport) CaptureRetirement(context.Context, HubStdioEndpoint) (func(context.Context) error, error) {
	return func(ctx context.Context) error {
		p.once.Do(func() { close(p.awaitEntered) })
		select {
		case <-p.allowReceipt:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, nil
}

func TestSessionLocalControlAdmissionWaitsForOriginalReceipt(t *testing.T) {
	m, target := sessionLocalPlanFixture(t)
	priorSnapshot := LoadResolverSnapshot()
	priorTokens := liveTokenTable.Load()
	t.Cleanup(func() { PublishResolverSnapshot(priorSnapshot); liveTokenTable.Store(priorTokens) })
	PublishResolverSnapshot(BuildResolverSnapshotFromManifests([]config.ServerManifest{*m}))
	liveTokenTable.Store(&hubTokenAuthorization{tokens: target.Tokens, projects: hubProjectAuthorityRecord{Groups: map[string]hubProjectEnrollment{}}, verified: true})
	transport := &sessionLocalAdmissionTransport{endpoint: target.Providers[0], awaitEntered: make(chan struct{}), allowReceipt: make(chan struct{})}
	store := NewHubSessionStore(SessionStoreOpts{StdioSessions: transport})
	defer store.Close()
	h := NewHubMcpHandler(store)
	h.SetEndpoint(target.Endpoint)
	control := &InternalReloadHandler{StdioAdmission: h.AdmitSessionLocalStdio, StdioInstanceID: target.Endpoint.InstanceID}
	token := strings.Repeat("e", 64)
	control.controlTok.Store(&token)
	body, _ := json.Marshal(HubStdioAdmissionRequest{Endpoint: target.Providers[0], Scope: "claude-code", Probe: true})
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9191/internal/reload-tokens", strings.NewReader(string(body)))
	request.Header.Set("X-Mcphub-Control-Token", token)
	request.Header.Set("X-Mcphub-Control-Operation", "stdio-admission-v1")
	request.Header.Set("X-Mcphub-Instance-Id", target.Endpoint.InstanceID)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	requestCtx, cancel := context.WithCancel(request.Context())
	request = request.WithContext(requestCtx)
	var releaseOnce sync.Once
	releaseReceipt := func() { releaseOnce.Do(func() { close(transport.allowReceipt) }) }
	defer func() {
		cancel()
		releaseReceipt()
		<-done
	}()
	go func() { control.ServeHTTP(response, request); close(done) }()
	select {
	case <-transport.awaitEntered:
	case <-done:
		t.Fatal("admission returned before original receipt")
	}
	select {
	case <-done:
		t.Fatal("204 was emitted before settlement")
	default:
	}
	releaseReceipt()
	<-done
	if response.Code != http.StatusNoContent {
		t.Fatalf("settled admission: %d", response.Code)
	}
}

func TestSessionLocalControlCannotUseClientCredential(t *testing.T) {
	control := &InternalReloadHandler{StdioInstanceID: strings.Repeat("a", 64)}
	token := strings.Repeat("b", 64)
	control.controlTok.Store(&token)
	called := false
	control.StdioAdmission = func(context.Context, HubStdioAdmissionRequest) error { called = true; return nil }
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/internal/reload-tokens", strings.NewReader(`{}`))
	r.Header.Set("X-Mcphub-Hub-Token", token)
	r.Header.Set("X-Mcphub-Control-Operation", "stdio-admission-v1")
	r.Header.Set("X-Mcphub-Instance-Id", control.StdioInstanceID)
	w := httptest.NewRecorder()
	control.ServeHTTP(w, r)
	if called || w.Code != http.StatusUnauthorized {
		t.Fatalf("client credential reached allocation: called=%t status=%d", called, w.Code)
	}
}

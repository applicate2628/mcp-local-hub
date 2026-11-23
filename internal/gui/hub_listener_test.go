// hub_listener_test.go — Phase 4 Task 4.4 (G4 unified hub MCP).
//
// gui-package integration tests for the hub-mcp listener lifecycle in
// hub_listener.go.
//
// SCOPE NOTE: Windows runs of the on-disk bind tests are skipped here
// because SecureWriteClientConfig requires a parent-dir DACL that
// passes the allowlist gate, and the helper that builds such a dir
// (hardenedTempDir) lives in the api-package test files (cannot be
// reached from a sibling-package test). The canonical on-disk
// Windows e2e test lives at internal/api/hub_mcp_e2e_test.go
// (Task 4.5). What CAN be tested cross-package on every platform is
// the gate-read helper + the Server.Start wiring with the gate OFF.

package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mcp-local-hub/internal/api/apitest"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// setupGateOverrides routes LOCALAPPDATA/XDG_DATA_HOME at a per-test
// temp dir so api.SettingsPath() returns a path under it. Returns
// the absolute gui-preferences.yaml path so tests can write the
// gate value.
//
// On Windows api.SettingsPath joins LOCALAPPDATA + mcp-local-hub +
// gui-preferences.yaml. On Linux it joins XDG_DATA_HOME + same. The
// helper pre-creates the parent dir so writes succeed.
func setupGateOverrides(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	parent := filepath.Join(root, "mcp-local-hub")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	// Set both env vars so the lookup hits our temp regardless of
	// platform. The lookup is short-circuit; whichever comes first
	// matches.
	t.Setenv("LOCALAPPDATA", root)
	t.Setenv("XDG_DATA_HOME", root)
	return filepath.Join(parent, "gui-preferences.yaml")
}

// writeGate writes `gui_server.hub_endpoint_enabled: "<value>"` to
// the per-test settings file.
func writeGate(t *testing.T, path string, on bool) {
	t.Helper()
	v := "false"
	if on {
		v = "true"
	}
	content := fmt.Sprintf("gui_server.hub_endpoint_enabled: %q\n", v)
	if err := api.WriteStateFileBytesAtomic(path, []byte(content)); err != nil {
		t.Fatalf("write gui-preferences.yaml: %v", err)
	}
}

// TestHubListenerComponents_AliveFalseOnNilReceiver pins the
// nil-receiver contract on the Alive() helper. Server.HubMcpEndpointActive
// guards against a nil hubMcpComp via Load() == nil; this is an
// extra defense if a caller bypasses that check.
func TestHubListenerComponents_AliveFalseOnNilReceiver(t *testing.T) {
	var c *HubListenerComponents
	if c.Alive() {
		t.Error("nil receiver Alive() = true; want false (defensive)")
	}
}

func TestHubListenerFatalServeExitSetsHealthDownUnlessSuperseded(t *testing.T) {
	for _, tc := range []struct {
		name              string
		exitingLive       bool
		nothingRegistered bool
		driverAlive       bool
		want              HubHealthState
		wantAction        string
	}{
		{name: "live component with live driver", exitingLive: true, driverAlive: true, want: HubHealthRecovering},
		{name: "live component with dead driver", exitingLive: true, want: HubHealthDown},
		{name: "stale component", exitingLive: false, driverAlive: true, want: HubHealthNeedsReconcile, wantAction: hubReconcileOperatorAction},
		{name: "pre-registration component with dead driver", nothingRegistered: true, want: HubHealthDown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", t.TempDir())
			s := newEphemeralServer(t, Config{})
			s.hubHealth.markReconcilePending()
			s.hubRestartDriverAlive.Store(tc.driverAlive)

			exiting := &HubListenerComponents{}
			if !tc.nothingRegistered {
				current := exiting
				if !tc.exitingLive {
					current = &HubListenerComponents{}
				}
				s.hubMcpComp.Store(current)
			}

			hubListenerServeFailed(s, exiting)
			if got, action := s.hubHealth.snapshot(); got != tc.want || action != tc.wantAction {
				t.Fatalf("fatal Serve exit = state %q action %q, want %q action %q", got, action, tc.want, tc.wantAction)
			}

			if tc.exitingLive {
				// The fatal availability state must not erase the latched reconcile
				// requirement; a later recovery restores the operator guidance.
				s.hubHealth.markHealthy()
				if got, action := s.hubHealth.snapshot(); got != HubHealthNeedsReconcile || action != hubReconcileOperatorAction {
					t.Fatalf("recovery after fatal exit = state %q action %q, want needs-reconcile + action", got, action)
				}
			}
		})
	}
}

func TestHubListenerRecoveryCallbackRequiresCurrentComponent(t *testing.T) {
	s := newEphemeralServer(t, Config{})
	oldComp := liveRestartTestComp(3439)
	currentComp := liveRestartTestComp(3439)
	s.hubMcpComp.Store(currentComp)
	s.hubHealth.set(HubHealthRecovering, "")

	hubListenerComponentCallback(s, oldComp, s.onHubListenerRecovered)()
	if got, action := s.hubHealth.snapshot(); got != HubHealthRecovering || action != "" {
		t.Fatalf("stale component recovery = state %q action %q, want recovering", got, action)
	}

	hubListenerComponentCallback(s, currentComp, s.onHubListenerRecovered)()
	if got, action := s.hubHealth.snapshot(); got != HubHealthHealthy || action != "" {
		t.Fatalf("current component recovery = state %q action %q, want healthy", got, action)
	}
}

// TestHubListenerSkippedWithGateOff — gate OFF, no listener bound,
// no state files written. Runs on every platform.
func TestHubListenerSkippedWithGateOff(t *testing.T) {
	settingsPath := setupGateOverrides(t)
	writeGate(t, settingsPath, false)
	if readHubEndpointGateFromSettings(nil) {
		t.Fatal("test setup: gate-read returned true for false setting")
	}

	a := api.NewAPI()
	bundle, err := startHubMcpListener(context.Background(), false, a)
	if err != nil {
		t.Fatalf("startHubMcpListener returned err with gate OFF: %v", err)
	}
	if bundle != nil {
		ShutdownHubListener(context.Background(), bundle)
		t.Errorf("bundle non-nil with gate OFF")
	}
}

// TestHubListenerBindFailureOnWindows — guarded check that the
// listener factory's bind-error path surfaces on Windows. We pre-bind
// a port via plain net.Listen, then ask the factory to bind the same
// port — Windows' SO_EXCLUSIVEADDRUSE makes the second bind fail
// synchronously. POSIX has no equivalent so this test skips there.
//
// This test does NOT exercise startHubMcpListener (which needs
// hardened-tempdir state-dir support); it exercises the listener
// factory in isolation so the bind-error surface is verified at the
// gui-package layer regardless of DACL helper availability.
func TestHubListenerBindFailureOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("SO_EXCLUSIVEADDRUSE is windows-only; POSIX bind reuse is a different surface")
	}
	preLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pre-bind listener: %v", err)
	}
	defer preLn.Close()
	port := preLn.Addr().(*net.TCPAddr).Port

	ln, err := api.NewListenerWithSOExclusive(fmt.Sprintf("127.0.0.1:%d", port))
	if err == nil {
		ln.Close()
		t.Fatalf("second bind against pre-bound port %d succeeded; SO_EXCLUSIVEADDRUSE not enforced", port)
	}
}

// TestReadHubEndpointGateFromSettings_AbsentFileReturnsFalse —
// missing gui-preferences.yaml MUST evaluate the gate as false
// (fail-closed posture).
func TestReadHubEndpointGateFromSettings_AbsentFileReturnsFalse(t *testing.T) {
	// setupGateOverrides creates the parent dir but not the file.
	settingsPath := setupGateOverrides(t)
	if _, err := os.Stat(settingsPath); err == nil {
		t.Fatalf("test setup: settings file already exists: %s", settingsPath)
	}
	if readHubEndpointGateFromSettings(nil) {
		t.Errorf("gate evaluated as true with no settings file")
	}
}

// TestReadHubEndpointGateFromSettings_CorruptYAMLReturnsFalse —
// malformed YAML MUST evaluate the gate as false (fail-closed).
func TestReadHubEndpointGateFromSettings_CorruptYAMLReturnsFalse(t *testing.T) {
	settingsPath := setupGateOverrides(t)
	if err := api.WriteStateFileBytesAtomic(settingsPath, []byte("{ not valid: yaml: [")); err != nil {
		t.Fatalf("write corrupt yaml: %v", err)
	}
	if readHubEndpointGateFromSettings(nil) {
		t.Errorf("gate evaluated as true on corrupt YAML")
	}
}

func TestReadHubEndpointGateFromSettings_InodeAnchorRejectsSymlink(t *testing.T) {
	settingsPath := setupGateOverrides(t)
	target := filepath.Join(filepath.Dir(settingsPath), "target-gui-preferences.yaml")
	if err := os.WriteFile(target, []byte(`gui_server.hub_endpoint_enabled: "true"`+"\n"), 0o600); err != nil {
		t.Fatalf("write target settings: %v", err)
	}
	if err := os.Symlink(target, settingsPath); err != nil {
		t.Skipf("symlink unsupported in this environment: %v", err)
	}

	if readHubEndpointGateFromSettings(nil) {
		t.Fatalf("gate followed a symlinked true settings file; want fail-closed false")
	}
}

// TestReadHubEndpointGateFromSettings_OnlyTrueIsTrue — only the
// exact string "true" enables the gate. Other truthy-looking values
// ("True", "TRUE", "1", "yes") are rejected to keep the gate read
// deterministic.
func TestReadHubEndpointGateFromSettings_OnlyTrueIsTrue(t *testing.T) {
	settingsPath := setupGateOverrides(t)
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"true-string", `gui_server.hub_endpoint_enabled: "true"`, true},
		{"True-mixed", `gui_server.hub_endpoint_enabled: "True"`, false},
		{"yes", `gui_server.hub_endpoint_enabled: "yes"`, false},
		{"1", `gui_server.hub_endpoint_enabled: "1"`, false},
		{"empty", `gui_server.hub_endpoint_enabled: ""`, false},
		{"absent", `other_key: "true"`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := api.WriteStateFileBytesAtomic(settingsPath, []byte(c.raw+"\n")); err != nil {
				t.Fatalf("write: %v", err)
			}
			got := readHubEndpointGateFromSettings(nil)
			if got != c.want {
				t.Errorf("%s: got %v, want %v / raw=%q", c.name, got, c.want, c.raw)
			}
		})
	}
}

// TestServerStartContinuesWithGateOff — Server.Start runs cleanly
// with the hub gate OFF (the wiring doesn't break the existing path).
func TestServerStartContinuesWithGateOff(t *testing.T) {
	settingsPath := setupGateOverrides(t)
	writeGate(t, settingsPath, false)

	s := NewServer(Config{Port: 0})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready := make(chan struct{})

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Start(ctx, ready)
	}()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("Server.Start did not signal ready within 3s")
	}
	if s.Port() == 0 {
		t.Errorf("gui-server did not bind despite Start success")
	}
	if s.hubMcpComp.Load() != nil {
		t.Errorf("hub bundle non-nil with gate OFF")
	}

	// Probe gui-server /api/ping (smoke).
	url := fmt.Sprintf("http://127.0.0.1:%d/api/ping", s.Port())
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("gui-server ping: %v", err)
	}
	resp.Body.Close()

	cancel()
	if err := <-errCh; err != nil && !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("Server.Start returned err: %v", err)
	}
}

// TestServerStartContinuesAfterHubBindFailure — even if the hub
// listener bind fails (gate ON but state-dir unwriteable on
// Windows), Server.Start MUST still bring up gui-server. The hub
// path enters the existing bounded restart driver while gui-server
// remains available.
//
// Skipped on POSIX where the t.TempDir() state-dir is writeable
// and the bind succeeds — this test specifically targets the
// "hub setup failed, gui-server lives" branch.
func TestServerStartContinuesAfterHubBindFailure(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("test simulates DACL failure mode visible only on Windows")
	}
	settingsPath := setupGateOverrides(t)
	writeGate(t, settingsPath, true)

	// Reroute the state dir to a plain t.TempDir() — Windows DACL
	// gate refuses SecureWriteClientConfig writes there, simulating
	// the failed-bind path. The cleanup restore puts back the
	// production resolver so the process state is clean.
	restore := api.SetDaemonStateRootForTest(t.TempDir())
	t.Cleanup(restore)

	s := NewServer(Config{Port: 0})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready := make(chan struct{})

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Start(ctx, ready)
	}()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("Server.Start did not signal ready within 3s despite hub bind failure")
	}
	if s.Port() == 0 {
		t.Errorf("gui-server did not bind despite Start success")
	}
	if hc := s.hubMcpComp.Load(); hc != nil {
		ShutdownHubListener(context.Background(), hc)
		t.Errorf("hub bundle non-nil despite simulated bind failure")
	}
	// Probe gui-server /api/ping (smoke): the gui-server is still up.
	url := fmt.Sprintf("http://127.0.0.1:%d/api/ping", s.Port())
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("gui-server ping: %v", err)
	}
	resp.Body.Close()

	cancel()
	if err := <-errCh; err != nil && !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("Server.Start returned err: %v", err)
	}
}

func TestHubListenerOrdinaryAggregateLifecycle(t *testing.T) {
	state := apitest.HardenedTempDir(t)
	t.Cleanup(api.SetDaemonStateRootForTest(state))
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", state)
	resetResolverSnapshot(t)
	t.Cleanup(func() { api.PublishResolverSnapshot(nil) })

	received := make(chan string, 8)
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodDelete {
			received <- "delete"
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var req struct {
			Method string
			ID     json.RawMessage
			Params struct{ Name string }
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		received <- req.Method + ":" + req.Params.Name
		w.Header().Set("Content-Type", "application/json")
		var result string
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "fixture-downstream-session")
			result = "{\"protocolVersion\":\"2025-11-25\",\"capabilities\":{\"tools\":{}},\"serverInfo\":{\"name\":\"fixture\",\"version\":\"1\"}}"
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			result = "{\"tools\":[{\"name\":\"echo\",\"description\":\"Fixture echo\"}]}"
		case "tools/call":
			if req.Params.Name != "echo" {
				t.Errorf("downstream tool = %q", req.Params.Name)
			}
			result = "{\"content\":[{\"type\":\"text\",\"text\":\"did useful work\"}]}"
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, "{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":%s}", req.ID, result)
	}))
	t.Cleanup(downstream.Close)
	manifestRoot := t.TempDir()
	serverRoot := filepath.Join(manifestRoot, "fixture")
	if err := os.Mkdir(serverRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf("name: fixture\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: %d\nclient_bindings:\n  - client: claude-code\n    daemon: default\n    url_path: /mcp\n", downstream.Listener.Addr().(*net.TCPAddr).Port)
	if err := os.WriteFile(filepath.Join(serverRoot, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestRoot)

	startReadySupervisorFixture(t, state, []api.SupervisorDaemon{{
		TaskName: "fixture-status", Server: "fixture", Daemon: "default", Port: downstream.Listener.Addr().(*net.TCPAddr).Port,
	}})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The production GUI wires its supervisor status source before
	// activation. Use the real dial to this fixture's isolated peer,
	// through the per-API seam, so the watcher stays within the fixture.
	watcherStatus := make(chan error, 1)
	a := api.NewAPI()
	a.HealthStatusFn = func(api.StatusOpts) ([]api.DaemonStatus, error) {
		statusCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		rows, err := api.DialSupervisorIPCStatus(statusCtx)
		select {
		case watcherStatus <- err:
		default:
		}
		return rows, err
	}
	hub, err := startHubMcpListener(ctx, true, a)
	if err != nil {
		t.Fatal(err)
	}
	if hub.listener == nil {
		t.Fatal("GUI assembly did not receive the shared aggregate lifecycle")
	}
	shutdown := func() { ShutdownHubListener(context.Background(), hub) }
	defer shutdown()
	select {
	case err := <-watcherStatus:
		if err != nil {
			t.Fatalf("listener watcher did not receive isolated supervisor status: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listener watcher did not dial isolated supervisor status")
	}
	endpoint := hub.listener.Endpoint()
	if endpoint.Port == 0 || endpoint.InstanceID == "" || !hub.Alive() {
		t.Fatal("live listener did not receive its bound endpoint and generation")
	}
	if durable, err := api.LoadHubEndpoint(); err != nil || durable != endpoint {
		t.Fatalf("published endpoint differs from accepted bind: %v", err)
	}
	if snapshot := api.LoadResolverSnapshot(); snapshot == nil || len(snapshot.Bindings["claude-code"]) != 1 {
		t.Fatal("listener startup did not publish its participating manifest")
	}
	tokens, err := api.ReloadHubTokens()
	if err != nil {
		t.Fatal(err)
	}
	control, err := api.ReadHubMcpControlToken()
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	t.Cleanup(client.CloseIdleConnections)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", endpoint.Port)
	scope := api.HubClientPrefix + "claude-code/mcp"
	var sid string
	request := func(method, path, body, origin string, useControl bool, status int) (http.Header, string) {
		t.Helper()
		input, err := http.NewRequestWithContext(ctx, method, baseURL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		input.Header.Set("X-Mcphub-Hub-Token", tokens.Tokens["claude-code"])
		input.Header.Set("X-Mcphub-Instance-Id", endpoint.InstanceID)
		input.Header.Set("MCP-Protocol-Version", "2025-11-25")
		if sid != "" {
			input.Header.Set("Mcp-Session-Id", sid)
		}
		if origin != "" {
			input.Header.Set("Origin", origin)
		}
		if useControl {
			input.Header.Set("X-Mcphub-Control-Token", control)
		}
		resp, err := client.Do(input)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := io.ReadAll(resp.Body)
		if err := errors.Join(err, resp.Body.Close()); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != status {
			t.Fatalf("%s %s: status %d want %d", method, path, resp.StatusCode, status)
		}
		return resp.Header, string(payload)
	}
	for _, path := range []string{"/clients", "/g", "/clients//claude-code/mcp"} {
		if h, body := request(http.MethodPost, path, "", "", false, http.StatusNotFound); h.Get("Location") != "" || body != "" {
			t.Fatalf("malformed path %q returned a body or redirect", path)
		}
	}
	init := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-11-25\"}}"
	request(http.MethodPost, scope, init, "http://example.invalid", false, http.StatusForbidden)
	header, _ := request(http.MethodPost, scope, init, "", false, http.StatusOK)
	sid = header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("initialize did not issue a session")
	}
	request(http.MethodPost, scope, "{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}", "", false, http.StatusAccepted)
	_, list := request(http.MethodPost, scope, "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\"}", "", false, http.StatusOK)
	if !strings.Contains(list, "\"name\":\"fixture__echo\"") {
		t.Fatalf("tools/list missing fixture tool: %s", list)
	}
	_, call := request(http.MethodPost, scope, "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"fixture__echo\"}}", "", false, http.StatusOK)
	if !strings.Contains(call, "did useful work") {
		t.Fatalf("missing downstream result: %s", call)
	}
	request(http.MethodDelete, scope, "", "", false, http.StatusNoContent)
	request(http.MethodPost, scope, "{\"jsonrpc\":\"2.0\",\"id\":4,\"method\":\"ping\"}", "", false, http.StatusNotFound)
	want := []string{"initialize:", "notifications/initialized:", "tools/list:", "tools/call:echo", "delete"}
	for _, next := range want {
		select {
		case got := <-received:
			if got != next {
				t.Fatalf("downstream method = %s, want %s", got, next)
			}
		default:
			t.Fatalf("missing downstream method %s", next)
		}
	}
	if len(received) != 0 {
		t.Fatal("unexpected extra downstream dispatch")
	}

	request(http.MethodHead, "/internal/reload-tokens", "", "", false, http.StatusUnauthorized)
	request(http.MethodHead, "/internal/reload-tokens", "", "", true, http.StatusNoContent)
	request(http.MethodPost, "/internal/reload-tokens", "", "", true, http.StatusNoContent)
	h, _ := request(http.MethodPost, "/internal/reload-tokens", "", "", true, http.StatusTooManyRequests)
	if h.Get("Retry-After") != "5" {
		t.Fatal("missing reload cooldown")
	}

	client.CloseIdleConnections()
	shutdown()
	deadline := time.Now().Add(2 * time.Second)
	for hub.Alive() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if hub.Alive() {
		t.Fatal("listener reported alive after Shutdown")
	}
	if conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", endpoint.Port)); err == nil {
		_ = conn.Close()
		t.Fatal("listener accepted a fresh connection after Shutdown")
	}
	if _, err := api.ReadHubMcpControlToken(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control token cleanup: %v", err)
	}
	t.Log("loopback lifecycle: initialize, initialized, tools/list, useful downstream tools/call, DELETE, reload 204/429, listener settled")
}

func TestSupervisedAggregateGUICannotBindOrRestart(t *testing.T) {
	server := &Server{cfg: Config{SupervisedAggregate: true}}
	ctx := context.Background()
	components, err := startHubMcpListenerWithOptions(ctx, true, nil, startHubMcpListenerOptions{server: server})
	if err != nil || components != nil {
		t.Fatalf("supervised Product GUI admitted local aggregate bind: %v", err)
	}
	if got := restartHubListenerWithOutcome(ctx, server, hubListenerRestartDriverOptions{}); got != hubListenerRestartStopDriver {
		t.Fatalf("supervised Product GUI admitted aggregate restart: %v", got)
	}
}

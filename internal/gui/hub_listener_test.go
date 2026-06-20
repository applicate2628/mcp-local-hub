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
	"fmt"
	"net"
	"net/http"
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
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
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

// TestHubListenerComponents_AliveTransitionsOnExit pins codex bot
// r2 P2 closure on PR #168: the serve goroutine flips Alive() from
// true to false when it exits. We construct a minimal bundle with
// a closed listener so srv.Serve returns immediately, then poll
// Alive() until it flips false.
func TestHubListenerComponents_AliveTransitionsOnExit(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Close immediately so Serve returns http.ErrServerClosed.
	_ = ln.Close()

	srv := &http.Server{Handler: http.NewServeMux(), ReadHeaderTimeout: 1 * time.Second}
	comp := &HubListenerComponents{srv: srv}
	comp.alive.Store(true)
	go func() {
		defer comp.alive.Store(false)
		_ = srv.Serve(ln)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !comp.Alive() {
			return // SUCCESS — alive flipped false post-Serve
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("Alive() stuck true 2s after Serve exited; the defer must clear it on any exit path")
}

func TestHubMcpHTTPServerHasSlowClientTimeouts(t *testing.T) {
	srv := newHubMcpHTTPServer(http.NewServeMux())
	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Fatalf("ReadHeaderTimeout=%v want 10s", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout <= 0 {
		t.Fatalf("ReadTimeout must bound slow request bodies")
	}
	if srv.WriteTimeout < api.PerCallWallClockCap+10*time.Second {
		t.Fatalf("WriteTimeout=%v want at least PerCallWallClockCap+10s", srv.WriteTimeout)
	}
	if srv.IdleTimeout != 120*time.Second {
		t.Fatalf("IdleTimeout=%v want 120s", srv.IdleTimeout)
	}
}

// TestHubListenerSkippedWithGateOff — gate OFF, no listener bound,
// no state files written. Runs on every platform.
func TestHubListenerSkippedWithGateOff(t *testing.T) {
	settingsPath := setupGateOverrides(t)
	writeGate(t, settingsPath, false)
	if readHubEndpointGateFromSettings() {
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
	if readHubEndpointGateFromSettings() {
		t.Errorf("gate evaluated as true with no settings file")
	}
}

// TestReadHubEndpointGateFromSettings_CorruptYAMLReturnsFalse —
// malformed YAML MUST evaluate the gate as false (fail-closed).
func TestReadHubEndpointGateFromSettings_CorruptYAMLReturnsFalse(t *testing.T) {
	settingsPath := setupGateOverrides(t)
	if err := os.WriteFile(settingsPath, []byte("{ not valid: yaml: ["), 0o600); err != nil {
		t.Fatalf("write corrupt yaml: %v", err)
	}
	if readHubEndpointGateFromSettings() {
		t.Errorf("gate evaluated as true on corrupt YAML")
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
			if err := os.WriteFile(settingsPath, []byte(c.raw+"\n"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			got := readHubEndpointGateFromSettings()
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
// path stays gate-OFF for this process lifetime; operator runs the
// rotation chain to recover.
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

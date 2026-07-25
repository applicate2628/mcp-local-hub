// internal/cli/install_reconcile_mcp_front_test.go
//
// Sub-increment 2a: coverage for `mcphub install --reconcile-mcp-front[/--rollback]`.
//
// Safety note: every test here redirects LOCALAPPDATA/USERPROFILE to a
// throwaway t.TempDir() (so clients.AllClients() adapters resolve under an
// empty, non-existent home — zero real client configs are ever touched) AND
// overrides mcpFrontReconcileSerenaReportPathFn (so report persistence never
// depends on api.DaemonStateDir(), which on Windows production builds
// ignores LOCALAPPDATA/MCPHUB_STATE_DIR_OVERRIDE unless the WHOLE test
// binary is compiled with -tags test_state_path_env — this package's normal
// `go test` invocation is not guaranteed to carry that tag).
package cli

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

// startTestRouteServer binds a REAL loopback listener and serves the actual
// production route-daemon handler (buildRouteServer, the same construction
// runRoute uses) on it — so the reconcile command's liveness gate
// (defaultRouterReadinessPing -> mcpInitializeProbe, a real HTTP round-trip)
// is proven against the genuine router shape, not a hand-rolled fake HTTP
// responder. Returns the bound port and a cleanup func.
func startTestRouteServer(t *testing.T) (port int, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port
	s, berr := buildRouteServer(&cobra.Command{}, actualPort)
	if berr != nil {
		_ = ln.Close()
		t.Fatalf("buildRouteServer: %v", berr)
	}
	httpSrv := &http.Server{Handler: s.RouteHandler()}
	go func() { _ = httpSrv.Serve(ln) }()
	return actualPort, func() {
		_ = httpSrv.Close()
	}
}

// redirectMCPFrontTestEnv points every settings/client-config resolution at
// a throwaway temp home for the duration of the test.
func redirectMCPFrontTestEnv(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
}

// withMCPFrontReportPathSeam overrides mcpFrontReconcileSerenaReportPathFn to
// a temp path for the duration of the test and restores it afterward.
func withMCPFrontReportPathSeam(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp-front-reconcile-serena-report.json")
	orig := mcpFrontReconcileSerenaReportPathFn
	mcpFrontReconcileSerenaReportPathFn = func() (string, error) { return path, nil }
	t.Cleanup(func() { mcpFrontReconcileSerenaReportPathFn = orig })
	return path
}

func TestRunReconcileMCPFront_FailsClosedWhenRouteNotLive(t *testing.T) {
	redirectMCPFrontTestEnv(t)
	reportPath := withMCPFrontReportPathSeam(t)

	a := api.NewAPI()
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, "19399"); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}
	// Deliberately do NOT start a route server on 19399 — nothing is
	// listening there.

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err := runReconcileMCPFront(cmd, false)
	if err == nil {
		t.Fatalf("expected an error when the route port is not live, got nil")
	}

	if _, statErr := os.Stat(reportPath); !os.IsNotExist(statErr) {
		t.Fatalf("report file must NOT be written when the fail-closed gate refuses; stat err = %v", statErr)
	}
}

func TestRunReconcileMCPFront_ForwardThenRollback_RoundTrip(t *testing.T) {
	redirectMCPFrontTestEnv(t)
	reportPath := withMCPFrontReportPathSeam(t)

	port, cleanup := startTestRouteServer(t)
	defer cleanup()

	a := api.NewAPI()
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(port)); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(new(testWriter))
	if err := runReconcileMCPFront(cmd, false); err != nil {
		t.Fatalf("forward reconcile: %v", err)
	}
	if _, statErr := os.Stat(reportPath); statErr != nil {
		t.Fatalf("expected the serena report to be persisted at %s after a successful forward reconcile: %v", reportPath, statErr)
	}

	if err := runReconcileMCPFront(cmd, true); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, statErr := os.Stat(reportPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the persisted report to be removed after a successful rollback; stat err = %v", statErr)
	}
}

func TestRunReconcileMCPFront_Rollback_NoPriorReport_Errors(t *testing.T) {
	redirectMCPFrontTestEnv(t)
	withMCPFrontReportPathSeam(t)

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err := runReconcileMCPFront(cmd, true)
	if err == nil {
		t.Fatalf("expected an error rolling back with no prior forward-reconcile report, got nil")
	}
}

func TestRunReconcileMCPFront_RollbackFlagWithoutReconcileFlag_Errors(t *testing.T) {
	c := newInstallCmdReal()
	c.SetArgs([]string{"--rollback"})
	c.SetOut(new(testWriter))
	c.SetErr(new(testWriter))
	err := c.Execute()
	if err == nil {
		t.Fatalf("expected --rollback without --reconcile-mcp-front to error")
	}
}

// testWriter is a minimal io.Writer sink so cobra command output during
// tests does not spam stdout.
type testWriter struct{}

func (testWriter) Write(p []byte) (int, error) { return len(p), nil }

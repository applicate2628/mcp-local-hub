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
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"
)

// seedSupervisorOwnedRoutePort makes the SUPERVISOR-OWNERSHIP gate
// (api.AssertMCPFrontPortSupervisorOwned) pass for port, by materializing the
// state a real supervised front daemon would have produced.
//
// It does NOT stub the gate out. Every step of the gate runs its production
// code here: the supervisor-lock probe takes a real flock (held for the
// duration of the test, which is exactly what a live supervisor does), the
// canonical descriptor is read out of a real supervisor-intent.json, the
// runtime row out of a real supervisor-state.json, and the PID liveness check
// runs against a genuinely-live PID (this test process). Only the two OS
// primitives that cannot be satisfied in-process — "who owns this loopback
// socket" and "what image is that PID" — are injected, and they are injected
// TRUTHFULLY: the test binary really is the process holding the port that
// startTestRouteServer bound.
//
// That distinction is the whole point. A helper that disabled the gate would
// let every forward test pass while proving nothing about ownership; this one
// leaves the gate armed, so the refusal test
// (TestMCPFrontOwnership_ForwardRefusesUnsupervisedRouteListener) exercises
// the same code path with one fact changed.
// seededSupervisorLock holds the flock seedSupervisorOwnedRoutePort took, so
// a test can release it mid-run to simulate "no supervisor is running".
// Package-level rather than returned because every existing call site wants
// only the seeding side effect; these tests never run in parallel.
var seededSupervisorLock *flock.Flock

// releaseSeededSupervisorLock drops the supervisor-liveness evidence the seed
// installed, turning a fully-supervised host into one where the route listener
// is running with no supervisor behind it — the standalone `mcphub route`
// case finding 3 is about.
func releaseSeededSupervisorLock(t *testing.T) {
	t.Helper()
	if seededSupervisorLock == nil {
		t.Fatalf("no seeded supervisor lock to release")
	}
	if err := seededSupervisorLock.Unlock(); err != nil {
		t.Fatalf("release seeded supervisor lock: %v", err)
	}
	seededSupervisorLock = nil
}

func seedSupervisorOwnedRoutePort(t *testing.T, port int) {
	t.Helper()
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		t.Fatalf("resolve state dir: %v", err)
	}

	// A held flock is what SupervisorRunningUnderStateDir actually probes, so
	// the test holds one rather than faking the answer. gofrs/flock locks the
	// `<path>.lock` sibling, and both Windows (per-HANDLE LockFileEx) and
	// Linux (per-open-file-description flock(2)) refuse a second handle's
	// TryLock even from the same process, so the probe reads "running".
	lk := flock.New(filepath.Join(stateDir, "supervisor.lock.lock"))
	got, lerr := lk.TryLock()
	if lerr != nil || !got {
		t.Fatalf("hold the supervisor lock for the ownership gate: got=%v err=%v", got, lerr)
	}
	seededSupervisorLock = lk
	t.Cleanup(func() {
		if seededSupervisorLock != nil {
			_ = seededSupervisorLock.Unlock()
			seededSupervisorLock = nil
		}
	})

	intentPath, ierr := api.DefaultSupervisorIntentPath()
	if ierr != nil {
		t.Fatalf("resolve supervisor-intent path: %v", ierr)
	}
	if werr := api.WriteSupervisorIntent(intentPath, &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{api.BuildBuiltinRouteDaemon("mcphub.exe", port)},
	}); werr != nil {
		t.Fatalf("seed supervisor-intent: %v", werr)
	}

	// started_at is the REAL kernel-recorded creation time of the process the
	// state file names, exactly as a live supervisor records for the child it
	// spawned. It is not forged: the port really is held by this process, so
	// its true start time is the truthful value here.
	//
	// The ownership gate binds the port owner to this timestamp (step 5), which
	// is what makes a recycled PID fail. Seeding a bogus value would forge the
	// proof; seeding the real one leaves that leg armed, so
	// TestMCPFrontOwnership_ForwardRefusesPIDReuseImpostor can exercise it by
	// changing this ONE fact.
	selfStart, sok := process.ProcessStartTime(os.Getpid())
	if !sok {
		t.Skipf("this platform cannot read a process start time, so the supervisor-ownership gate is unavailable here by design")
	}
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	if werr := api.WriteSupervisorState(statePath, &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{
			api.BuiltinRouteTaskName: {
				State:         "running",
				CurrentPID:    os.Getpid(),
				PIDGeneration: 1,
				StartedAt:     selfStart.UTC().Format(time.RFC3339Nano),
			},
		},
	}); werr != nil {
		t.Fatalf("seed supervisor-state: %v", werr)
	}

	t.Cleanup(api.SetPortOwnerIdentityProbesForTest(
		func(p int) (int, bool, error) {
			if p != port {
				return 0, false, nil
			}
			return os.Getpid(), true, nil
		},
		func(int) (string, bool) { return "mcphub.exe", true },
	))
}

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
	s, _, berr := buildRouteServer(&cobra.Command{}, actualPort)
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
//
// It also pins the daemon state dir PER TEST. The package-global TestMain
// redirect already keeps every test off the operator's real state dir, but it
// is one SHARED directory: now that these tests seed supervisor-intent.json /
// supervisor-state.json for the ownership gate, sharing would let one test's
// seed satisfy (or contradict) another's. Per-test pinning keeps each seed
// invisible to its neighbours.
func redirectMCPFrontTestEnv(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("HOME", tmp)
	t.Cleanup(api.SetDaemonStateRootForTest(tmp))
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
	//
	// The supervisor-ownership gate runs BEFORE the liveness probe, so it is
	// satisfied here on purpose: otherwise it would refuse first and this test
	// would silently stop covering the LIVENESS refusal it is named for. The
	// injected OS probe is the only part that is fictional (it reports this
	// process as the port owner) — everything the gate reads off disk is real.
	seedSupervisorOwnedRoutePort(t, 19399)

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

	seedSupervisorOwnedRoutePort(t, port)

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

// TestRunReconcileMCPFront_Rollback_UsesPersistedPortNotLiveSetting is the F2
// hardening guard (adversarial-gate finding on sub-increment 2a): rollback
// must not depend on being able to re-resolve the LIVE mcp_front.port
// setting once a forward-run report already recorded a good port.
//
// Honest scoping note: the reviewer's literal scenario — the operator
// changes mcp_front.port to a DIFFERENT VALID port between forward and
// rollback, orphaning the OLD port's LSP router entry — does not reproduce
// in this codebase, because the LSP rollback's entry lookup is keyed by the
// entry's fixed canonical name rather than by the GUIPort value passed in
// (the port is ownership evidence only). That was true of the
// RollbackLSPRouterClientEntries routine this command used at the time this
// test was written, and remains true of
// api.RestoreLSPRouterClientEntriesSnapshot, which REPLACED it in the codex
// bot PR #588 P1 fix — see that function's doc comment, and
// TestMCPFrontPR588_RollbackRestoresPriorLSPRouterURL for why the swap was
// necessary. (The api-side guard
// TestRollbackLSPRouterClientEntries_RemovalIsNameKeyedNotPortKeyed still
// pins the property for the demotion routine, which remains the owner of its
// own, different operation.) Also,
// a.SettingsSet validates against the registry's [1024,65535] range on
// every write, so an actually out-of-range value can never reach the
// settings file through the normal write path; and a corrupted-but-in-range
// persisted value is silently resolved to the schema default by
// SettingsListIn's own graceful-degrade behavior (never an error) — so
// there is no way to make a.ResolveMCPFrontPort() itself fail via the
// normal settings surface either.
//
// What this hardening DOES fix, and what this test proves: before it,
// runRollbackMCPFront called a.ResolveMCPFrontPort() unconditionally, which
// reads and fully re-parses gui-preferences.yaml. If that file becomes
// unreadable/unparseable for any reason between the forward run and the
// rollback (this test simulates that by writing malformed YAML directly to
// the settings file, bypassing SettingsSet), the OLD rollback would abort
// with an error even though the forward run's report already recorded a
// perfectly good, usable port. The hardened rollback reads the port back
// from that persisted report instead and never needs to touch the live
// settings file at all in the common case.
//
// Mutation-proven: reverting runRollbackMCPFront to always call
// a.ResolveMCPFrontPort() (ignoring persisted.Port) makes this test fail
// with the settings-file parse error surfacing as the rollback's own error
// — confirmed manually during implementation (see this item's final
// report for the captured failing output), then reverted.
func TestRunReconcileMCPFront_Rollback_UsesPersistedPortNotLiveSetting(t *testing.T) {
	redirectMCPFrontTestEnv(t)
	reportPath := withMCPFrontReportPathSeam(t)

	port, cleanup := startTestRouteServer(t)
	defer cleanup()

	seedSupervisorOwnedRoutePort(t, port)

	a := api.NewAPI()
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(port)); err != nil {
		t.Fatalf("SettingsSet (forward port): %v", err)
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

	// Simulate the live settings file becoming unreadable between the
	// forward run and the rollback (malformed YAML, bypassing SettingsSet's
	// own validation — this is not reachable through the normal write path,
	// see the doc comment above for why a plain value-drift cannot be used
	// here). a.ResolveMCPFrontPort() must now fail if anything still calls
	// it unconditionally.
	settingsPath := api.SettingsPath()
	if err := os.WriteFile(settingsPath, []byte("mcp_front: [this is not valid yaml\n"), 0o600); err != nil {
		t.Fatalf("corrupt settings file: %v", err)
	}
	if _, verifyErr := a.ResolveMCPFrontPort(); verifyErr == nil {
		t.Fatalf("test precondition broken: ResolveMCPFrontPort should fail against the corrupted settings file, but it did not")
	}

	if err := runReconcileMCPFront(cmd, true); err != nil {
		t.Fatalf("rollback must succeed using the persisted forward-run port, independent of the (now-unreadable) live mcp_front.port setting: %v", err)
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

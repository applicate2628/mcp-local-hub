package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"

	"github.com/spf13/cobra"
)

func startJoinedMCPFrontRouteServer(t *testing.T) (int, func() error) {
	t.Helper()
	port, cleanup, err := startJoinedMCPFrontRouteServerAt(0)
	if err != nil {
		t.Fatalf("start route server: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup route server on port %d: %v", port, err)
		}
	})
	return port, cleanup
}

func startJoinedMCPFrontRouteServerAt(requestedPort int) (int, func() error, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(requestedPort)))
	if err != nil {
		return 0, nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	router, _, err := buildRouteServer(&cobra.Command{}, port)
	if err != nil {
		_ = ln.Close()
		return 0, nil, err
	}
	server := &http.Server{Handler: router.RouteHandler()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(ln)
	}()
	var once sync.Once
	var cleanupErr error
	cleanup := func() error {
		once.Do(func() {
			closeErr := server.Close()
			listenerErr := ln.Close()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				cleanupErr = errors.New("route server goroutine did not settle")
			}
			if cleanupErr == nil && closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
				cleanupErr = closeErr
			}
			if cleanupErr == nil && listenerErr != nil && !errors.Is(listenerErr, net.ErrClosed) {
				cleanupErr = listenerErr
			}
		})
		return cleanupErr
	}
	return port, cleanup, nil
}

func TestRunForwardReconcileMCPFront_CallerLossAfterActivationRequiresAutomaticPriorRestore(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	assertRedirectedStateDir(t, tmp)
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		t.Fatalf("resolve test state dir: %v", err)
	}
	reportPath := filepath.Join(stateDir, "mcp-front-reconcile-serena-report.json")
	originalReportPath := mcpFrontReconcileSerenaReportPathFn
	mcpFrontReconcileSerenaReportPathFn = func() (string, error) { return reportPath, nil }
	t.Cleanup(func() { mcpFrontReconcileSerenaReportPathFn = originalReportPath })

	configPath := seedClaudeCodeConfig(t, tmp, map[string]any{
		"serena": map[string]any{"url": "http://127.0.0.1:9125/serena/mcp"},
	})
	portA, closeA := startJoinedMCPFrontRouteServer(t)
	seedSupervisorOwnedRoutePort(t, portA)
	a := api.NewAPI()
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(portA)); err != nil {
		t.Fatalf("set initial front port A: %v", err)
	}
	if err := runReconcileMCPFront(newMCPFrontTestCmd(), false); err != nil {
		t.Fatalf("establish persisted version-4 A baseline: %v", err)
	}

	beforeReport, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read baseline A report: %v", err)
	}
	baseline := readPersistedMCPFrontReport(t, reportPath)
	if baseline.Version != mcpFrontReconcileVersion4 || baseline.ActivePlan == nil || baseline.ActivePlan.Port != portA {
		t.Fatalf("baseline report = %+v, want current version with active plan A=%d", baseline, portA)
	}
	routingA, err := a.MCPFrontRoutingTargetSnapshot()
	if err != nil {
		t.Fatalf("read baseline routing A: %v", err)
	}
	if routingA.State != api.MCPFrontRoutingTargetFront || routingA.Port != portA {
		t.Fatalf("baseline routing = %+v, want FRONT A=%d", routingA, portA)
	}
	beforeConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read baseline client config: %v", err)
	}

	portB, closeB := startJoinedMCPFrontRouteServer(t)
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(portB)); err != nil {
		t.Fatalf("set requested front port B: %v", err)
	}
	recoveryEntered := make(chan struct{})
	releaseRecovery := make(chan struct{})
	var recoveredServerMu sync.Mutex
	var recoveredServerCleanup func() error
	t.Cleanup(func() {
		recoveredServerMu.Lock()
		cleanup := recoveredServerCleanup
		recoveredServerMu.Unlock()
		if cleanup != nil {
			if err := cleanup(); err != nil {
				t.Errorf("cleanup recovered A route server: %v", err)
			}
		}
	})
	recoveryOps := newMCPFrontRouteActivationRecoveryOps(a, stateDir, nil)
	recoveryOps.projectRoute = func(ctx context.Context, activation mcpFrontRouteActivation) error {
		close(recoveryEntered)
		select {
		case <-releaseRecovery:
		case <-ctx.Done():
			return ctx.Err()
		}
		return projectMCPFrontRouteActivationSourceWithCommand(ctx, stateDir, a, activation, "mcphub-test")
	}
	recoveryOps.verifyDescriptor = func(activation mcpFrontRouteActivation, port int) error {
		return verifyMCPFrontRouteActivationDescriptorWithCommand(stateDir, activation, port, "mcphub-test")
	}
	recoveryOps.reconcile = func(context.Context) error {
		if err := closeB(); err != nil {
			return fmt.Errorf("stop B listener during recovery reconcile: %w", err)
		}
		_, cleanup, err := startJoinedMCPFrontRouteServerAt(portA)
		if err != nil {
			return fmt.Errorf("restart A listener during recovery reconcile: %w", err)
		}
		recoveredServerMu.Lock()
		recoveredServerCleanup = cleanup
		recoveredServerMu.Unlock()
		return nil
	}
	recoveryDriver := startMCPFrontRouteActivationRecoveryDriver(context.Background(), nil, func(ctx context.Context) {
		_ = recoverMCPFrontRouteActivation(ctx, stateDir, recoveryOps)
	})
	t.Cleanup(recoveryDriver.Stop)
	activationOps := mcpFrontRouteActivationOps{
		stage: func(ctx context.Context, port int) error {
			return stageMCPFrontRouteDaemonWithOps(ctx, stateDir, port, mcpFrontRouteStageOps{
				commandPath: func() string { return "mcphub-test" },
				reconcile: func(context.Context, bool) (api.ReconcileResponse, error) {
					if err := closeA(); err != nil {
						return api.ReconcileResponse{}, fmt.Errorf("stop A listener during supervisor reconcile: %w", err)
					}
					recoveryDriver.Trigger()
					return api.ReconcileResponse{AppliedCount: 1}, nil
				},
			})
		},
		waitListener: waitForMCPFrontRouteListener,
		preflight: func(ctx context.Context, port int) error {
			return api.AssertMCPFrontRoutesLive(ctx, port)
		},
	}
	admissionCallbacks := 0
	admissionOps := mcpFrontGenerationAdmissionOps{
		routingState: func() (api.MCPFrontRoutingTargetSnapshot, error) {
			admissionCallbacks++
			return routingA, errors.New("admission must be unreachable after caller loss")
		},
		transition: func(context.Context, api.MCPFrontRoutingTargetSnapshot, api.MCPFrontRoutingTargetSnapshot) error {
			admissionCallbacks++
			return errors.New("admission transition must be unreachable after caller loss")
		},
		persist: func(string, *mcpFrontReconcileReport) error {
			admissionCallbacks++
			return errors.New("admission persistence must be unreachable after caller loss")
		},
	}

	workerDone := make(chan struct{})
	workerErr := make(chan error, 2)
	go func() {
		defer close(workerDone)
		lock, lockErr := api.AcquireSupervisorLock(filepath.Join(stateDir, mcpFrontReconcileLockLeaf))
		if lockErr != nil {
			workerErr <- fmt.Errorf("acquire transaction lock: %w", lockErr)
			return
		}
		defer lock.Release()
		callErr := runForwardReconcileMCPFrontWithOps(
			newMCPFrontTestCmd(), a, reportPath, activationOps, admissionOps,
			func(ctx context.Context, port int, ops mcpFrontRouteActivationOps) error {
				if err := activateMCPFrontRoute(ctx, port, ops); err != nil {
					return err
				}
				runtime.Goexit()
				return nil
			},
		)
		workerErr <- fmt.Errorf("caller returned instead of terminating at cutpoint: %v", callErr)
	}()
	select {
	case <-workerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("caller-loss worker did not settle")
	}
	select {
	case workerFailure := <-workerErr:
		t.Fatal(workerFailure)
	default:
	}
	select {
	case <-recoveryEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("autonomous recovery did not acquire the released transaction lock")
	}

	phaseBytes, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read phase report after caller loss: %v", err)
	}
	phaseReport, err := readMCPFrontReconcileReport(reportPath)
	if err != nil {
		t.Fatalf("strict-read phase report after caller loss: %v", err)
	}
	afterRouting, err := a.MCPFrontRoutingTargetSnapshot()
	if err != nil {
		t.Fatalf("read routing after caller loss: %v", err)
	}
	intent, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
	if err != nil {
		t.Fatalf("read intent after caller loss: %v", err)
	}
	intentPort := 0
	for _, daemon := range intent.Daemons {
		if daemon.TaskName == api.BuiltinRouteTaskName {
			intentPort = daemon.Port
			break
		}
	}
	afterConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read client config after caller loss: %v", err)
	}
	phase := phaseReport.RouteActivation
	if bytes.Equal(phaseBytes, beforeReport) || phaseReport.Version != mcpFrontReconcileVersion5 || phaseReport.Generation != baseline.Generation || phaseReport.ActivePlan == nil || phaseReport.ActivePlan.Port != portA || phase == nil || phase.Phase != mcpFrontRouteActivationPrepared || phase.FromEpoch != routingA || phase.ToEpoch.Generation != routingA.Generation+1 || phase.ToEpoch.Port != portB || afterRouting != routingA || intentPort != portB || admissionCallbacks != 0 || !bytes.Equal(afterConfig, beforeConfig) {
		t.Fatalf("caller-loss discriminator mismatch: phase_v5=%t reportA=%t routingA=%t intentB=%t admission_callbacks=%d client_unchanged=%t", phaseReport.Version == mcpFrontReconcileVersion5 && phase != nil, phaseReport.ActivePlan != nil && phaseReport.ActivePlan.Port == portA, afterRouting == routingA, intentPort == portB, admissionCallbacks, bytes.Equal(afterConfig, beforeConfig))
	}
	if err := api.AssertMCPFrontRoutesLive(context.Background(), portB); err != nil {
		t.Fatalf("B is not live after successful activation: %v", err)
	}
	if conn, dialErr := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(portA)), 100*time.Millisecond); dialErr == nil {
		_ = conn.Close()
		t.Fatalf("A listener remained available after B activation on port %d", portA)
	}

	close(releaseRecovery)
	restoreCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := waitForMCPFrontRouteListener(restoreCtx, portA); err != nil {
		t.Fatalf("automatic A restoration missing after caller loss: persisted_A=true routing_A=true intent_B=true live_B=true A_unavailable=true admission_callbacks=0 client_unchanged=true: %v", err)
	}
	var finalReport *mcpFrontReconcileReport
	for restoreCtx.Err() == nil {
		finalReport, err = readMCPFrontReconcileReport(reportPath)
		if err == nil && finalReport != nil && finalReport.RouteActivation == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if finalReport == nil || finalReport.RouteActivation != nil {
		t.Fatalf("autonomous recovery did not durably clear the phase: report=%+v err=%v", finalReport, err)
	}
	wantFinal := baseline
	wantFinal.Version = mcpFrontReconcileVersion5
	if !reflect.DeepEqual(finalReport, &wantFinal) {
		t.Fatalf("final report changed the A baseline: got=%+v want=%+v", finalReport, &wantFinal)
	}
	finalRouting, err := a.MCPFrontRoutingTargetSnapshot()
	if err != nil || finalRouting != routingA {
		t.Fatalf("final routing=%+v err=%v, want A=%+v", finalRouting, err, routingA)
	}
	finalIntent, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
	if err != nil {
		t.Fatalf("read final intent: %v", err)
	}
	if err := validateMCPFrontRouteDescriptorAtPorts(finalIntent, "mcphub-test", portA); err != nil {
		t.Fatalf("final descriptor is not exact A: %v", err)
	}
	if err := assertMCPFrontRouteListenerAbsent(context.Background(), portB); err != nil {
		t.Fatalf("B remained served after recovery: %v", err)
	}
	finalConfig, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(finalConfig, beforeConfig) || admissionCallbacks != 0 {
		t.Fatalf("recovery touched client/admission state: config_unchanged=%t admission_callbacks=%d err=%v", bytes.Equal(finalConfig, beforeConfig), admissionCallbacks, err)
	}
	lockCtx, lockCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer lockCancel()
	release, err := acquireMCPFrontReconcileLockContext(lockCtx, filepath.Join(stateDir, mcpFrontReconcileLockLeaf))
	if err != nil {
		t.Fatalf("autonomous recovery retained transaction lock: %v", err)
	}
	release()
}

func TestProjectMCPFrontRouteActivation_RejectsThirdDescriptorWithoutMutation(t *testing.T) {
	tmp := mcpFrontPR588Env(t)
	assertRedirectedStateDir(t, tmp)
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		t.Fatal(err)
	}
	portA, _ := startJoinedMCPFrontRouteServer(t)
	seedSupervisorOwnedRoutePort(t, portA)
	a := api.NewAPI()
	if err := a.SettingsSet(api.MCPFrontPortSettingKey, strconv.Itoa(portA)); err != nil {
		t.Fatal(err)
	}
	if err := runReconcileMCPFront(newMCPFrontTestCmd(), false); err != nil {
		t.Fatalf("establish A: %v", err)
	}
	routingA, err := a.MCPFrontRoutingTargetSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	activation := mcpFrontRouteActivation{
		Phase:     mcpFrontRouteActivationPrepared,
		FromEpoch: routingA,
		ToEpoch:   api.MCPFrontRoutingTargetSnapshot{State: api.MCPFrontRoutingTargetFrontPreparing, Generation: routingA.Generation + 1, Port: portA + 1},
	}
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	if _, err := api.MutateSupervisorIntentIfChangedReturning(intentPath, func(intent *api.SupervisorIntentFile) (bool, error) {
		return api.EnsureBuiltinRouteDaemon(intent, canonicalMcphubPath(), portA+2)
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	err = projectMCPFrontRouteActivationSource(context.Background(), stateDir, a, activation)
	if err == nil || !strings.Contains(err.Error(), "descriptor-mismatch") {
		t.Fatalf("third descriptor recovery error=%v", err)
	}
	after, readErr := os.ReadFile(intentPath)
	if readErr != nil || !bytes.Equal(after, before) {
		t.Fatalf("third descriptor was mutated: bytes_unchanged=%t err=%v", bytes.Equal(after, before), readErr)
	}
}

func TestActivateMCPFrontRoute_StagesDescriptorBeforePreflight(t *testing.T) {
	var order []string
	err := activateMCPFrontRoute(context.Background(), 9555, mcpFrontRouteActivationOps{
		stage: func(context.Context, int) error {
			order = append(order, "stage")
			return nil
		},
		waitListener: func(context.Context, int) error {
			order = append(order, "wait-listener")
			return nil
		},
		preflight: func(context.Context, int) error {
			order = append(order, "preflight")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("activate route: %v", err)
	}
	if got := len(order); got != 3 || order[0] != "stage" || order[1] != "wait-listener" || order[2] != "preflight" {
		t.Fatalf("activation order=%v, want [stage wait-listener preflight]", order)
	}

	preflightCalled := false
	sentinel := errors.New("stage failed")
	err = activateMCPFrontRoute(context.Background(), 9555, mcpFrontRouteActivationOps{
		stage: func(context.Context, int) error { return sentinel },
		waitListener: func(context.Context, int) error {
			t.Fatal("listener wait ran after stage failure")
			return nil
		},
		preflight: func(context.Context, int) error {
			preflightCalled = true
			return nil
		},
	})
	if !errors.Is(err, sentinel) || preflightCalled {
		t.Fatalf("stage failure err=%v preflightCalled=%v, want failure before probe", err, preflightCalled)
	}
}

func TestActivateMCPFrontRoute_WaitsForReconciledListener(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve delayed-listener port: %v", err)
	}
	port := reservation.Addr().(*net.TCPAddr).Port
	if err := reservation.Close(); err != nil {
		t.Fatalf("release delayed-listener reservation: %v", err)
	}

	stageCalls := 0
	preflightCalls := 0
	listenerResult := make(chan net.Listener, 1)
	listenerErr := make(chan error, 1)
	err = activateMCPFrontRoute(ctx, port, mcpFrontRouteActivationOps{
		stage: func(context.Context, int) error {
			stageCalls++
			go func() {
				time.Sleep(100 * time.Millisecond)
				ln, listenErr := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
				if listenErr != nil {
					listenerErr <- listenErr
					return
				}
				listenerResult <- ln
			}()
			return nil
		},
		waitListener: waitForMCPFrontRouteListener,
		preflight: func(context.Context, int) error {
			preflightCalls++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("activate route after delayed listener: %v", err)
	}
	if stageCalls != 1 {
		t.Fatalf("stage calls=%d, want exactly 1", stageCalls)
	}
	if preflightCalls != 1 {
		t.Fatalf("preflight calls=%d, want one strict proof after listener wait", preflightCalls)
	}
	select {
	case ln := <-listenerResult:
		if closeErr := ln.Close(); closeErr != nil {
			t.Fatalf("close delayed listener: %v", closeErr)
		}
	case listenErr := <-listenerErr:
		t.Fatalf("bind delayed listener: %v", listenErr)
	case <-time.After(time.Second):
		t.Fatal("delayed listener was not returned for cleanup")
	}
}

func TestRestorePriorMCPFrontRoute_WaitsForListenerAndStrictProof(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	var order []string
	ops := mcpFrontRouteActivationOps{
		stage: func(ctx context.Context, port int) error {
			if err := ctx.Err(); err != nil {
				t.Fatalf("cleanup activation inherited canceled caller context: %v", err)
			}
			if port != 9554 {
				t.Fatalf("stage port=%d, want prior port 9554", port)
			}
			order = append(order, "stage")
			return nil
		},
		waitListener: func(_ context.Context, port int) error {
			if port != 9554 {
				t.Fatalf("listener port=%d, want prior port 9554", port)
			}
			order = append(order, "wait-listener")
			return nil
		},
		preflight: func(_ context.Context, port int) error {
			if port != 9554 {
				t.Fatalf("preflight port=%d, want prior port 9554", port)
			}
			order = append(order, "preflight")
			return nil
		},
	}
	if err := restorePriorMCPFrontRoute(parent, 9554, 9555, ops); err != nil {
		t.Fatalf("restore prior route: %v", err)
	}
	if got := order; len(got) != 3 || got[0] != "stage" || got[1] != "wait-listener" || got[2] != "preflight" {
		t.Fatalf("restore order=%v, want [stage wait-listener preflight]", got)
	}

	order = nil
	if err := restorePriorMCPFrontRoute(context.Background(), 9554, 9554, ops); err != nil {
		t.Fatalf("same-port restore: %v", err)
	}
	if len(order) != 0 {
		t.Fatalf("same-port restore invoked activation: %v", order)
	}

	waitErr := errors.New("prior listener did not recover")
	preflightCalled := false
	err := restorePriorMCPFrontRoute(context.Background(), 9554, 9555, mcpFrontRouteActivationOps{
		stage: func(context.Context, int) error { return nil },
		waitListener: func(context.Context, int) error {
			return waitErr
		},
		preflight: func(context.Context, int) error {
			preflightCalled = true
			return nil
		},
	})
	if !errors.Is(err, waitErr) {
		t.Fatalf("listener recovery error=%v, want sentinel", err)
	}
	if preflightCalled {
		t.Fatal("strict preflight ran after prior-listener recovery failure")
	}
}

func TestWaitForMCPFrontRouteListener_StopsAtCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := waitForMCPFrontRouteListener(ctx, 9557)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error=%v, want context.Canceled", err)
	}
}

func TestStageMCPFrontRouteDaemon_PersistsExactPortBeforeSupervisorReconcile(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	const port = 9555
	reconcileCalls := 0

	err := stageMCPFrontRouteDaemonWithOps(context.Background(), stateDir, port, mcpFrontRouteStageOps{
		commandPath: func() string { return "mcphub-test" },
		reconcile: func(context.Context, bool) (api.ReconcileResponse, error) {
			reconcileCalls++
			intent, err := api.ReadSupervisorIntent(intentPath)
			if err != nil {
				t.Fatalf("read staged intent: %v", err)
			}
			want := api.BuildBuiltinRouteDaemon("mcphub-test", port)
			if len(intent.Daemons) != 1 || intent.Daemons[0].TaskName != want.TaskName || intent.Daemons[0].Port != port {
				t.Fatalf("reconcile observed intent=%+v, want exact route descriptor at %d", intent.Daemons, port)
			}
			return api.ReconcileResponse{}, nil
		},
	})
	if err != nil {
		t.Fatalf("stage route daemon: %v", err)
	}
	if reconcileCalls != 1 {
		t.Fatalf("reconcile calls=%d, want 1", reconcileCalls)
	}
}

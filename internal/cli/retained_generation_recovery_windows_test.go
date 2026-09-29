//go:build windows

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

type retainedGenerationHeldFake struct {
	pid            int
	verifyCalls    int
	closeCalls     int
	terminateCalls int
	verifyErr      error
}

func (f *retainedGenerationHeldFake) PID() int { return f.pid }
func (f *retainedGenerationHeldFake) VerifyIdentity(process.PIDIdentityProof) error {
	f.verifyCalls++
	return f.verifyErr
}
func (f *retainedGenerationHeldFake) Terminate() (bool, error) {
	f.terminateCalls++
	return false, errors.New("retained-generation recovery must never terminate")
}
func (f *retainedGenerationHeldFake) Close() error { f.closeCalls++; return nil }

type retainedGenerationRunFixture struct {
	txn          *WindowsProductTxn
	paths        productPairPaths
	pairOrder    *[]string
	legacy       *fakeUpgradeDeps
	held         *retainedGenerationHeldFake
	frozen       []windowsProductIntentPortV1
	observation  retainedGenerationObservationV1
	observeCall  int
	sendCalls    []string
	waitLock     int
	verifyPorts  int
	publishCalls int
	lastEvent    retainedGenerationSettledEventV1
}

func newRetainedGenerationRunFixture(t *testing.T) *retainedGenerationRunFixture {
	t.Helper()
	txn, paths, order := partialPreventionUpgradePair(t)
	retainedPath := filepath.Join(filepath.Dir(paths.priorCLI), "retained", "mcphub.exe.old-20260910T035900Z")
	frozen := []windowsProductIntentPortV1{
		{TaskName: "task-unbound", Port: 9123, ArgvSHA256: strings.Repeat("a", 64)},
		{TaskName: "task-canonical", Port: 9124, ArgvSHA256: strings.Repeat("b", 64)},
		{TaskName: "task-retained", Port: 9125, ArgvSHA256: strings.Repeat("c", 64)},
	}
	observation := retainedGenerationObservationV1{
		ReceiptID:               "retained-observation",
		CanonicalCLIPath:        paths.priorCLI,
		CanonicalWindowlessPath: paths.windowless,
		SupervisorPID:           77,
		SupervisorStartedAt:     "2026-09-10T03:59:00Z",
		SupervisorImagePath:     retainedPath,
		SupervisorCommandSHA256: strings.Repeat("d", 64),
		GUIOwner:                windowsProductGUIOwnerConfirmedAbsent,
		Ports: []retainedGenerationPortObservationV1{
			{TaskName: "task-unbound", Port: 9123, State: windowsProductPortUnbound},
			{TaskName: "task-canonical", Port: 9124, State: windowsProductPortCanonicalOwned, PID: 88, StartedAt: "2026-09-10T03:59:01Z", ParentPID: 77, ObservedImagePath: paths.priorCLI, ArgvSHA256: strings.Repeat("b", 64)},
			{TaskName: "task-retained", Port: 9125, State: windowsProductPortRetainedOwned, PID: 99, StartedAt: "2026-09-10T03:59:02Z", ParentPID: 77, ObservedImagePath: retainedPath, ArgvSHA256: strings.Repeat("c", 64)},
		},
		ObservedAtUTC: time.Date(2026, 9, 10, 4, 0, 0, 0, time.UTC),
	}
	return &retainedGenerationRunFixture{
		txn: txn, paths: paths, pairOrder: order, legacy: &fakeUpgradeDeps{},
		held: &retainedGenerationHeldFake{pid: 77}, frozen: frozen, observation: observation,
	}
}

func (f *retainedGenerationRunFixture) lifecycle(observe func(int) (*retainedGenerationObservationV1, error), send func(string, int) (retainedGenerationIPCExchangeV1, error)) *windowsProductPairLifecycleHooks {
	return &windowsProductPairLifecycleHooks{
		FrozenPorts: f.frozen,
		Observe: func(context.Context, string, []windowsProductIntentPortV1) (windowsProductQuiescenceObservationV1, error) {
			return windowsProductQuiescenceObservationV1{}, errors.New("ordinary lifecycle observer must remain unreachable for retained generation")
		},
		Retained: &retainedGenerationLifecycleHooks{
			RequestVersion: 0,
			Observe: func(context.Context, string, string, []windowsProductIntentPortV1) (*retainedGenerationObservationV1, error) {
				f.observeCall++
				return observe(f.observeCall)
			},
			HoldSupervisor: func(pid int) (process.HeldPIDGeneration, error) {
				if pid != 77 {
					return nil, fmt.Errorf("hold PID=%d", pid)
				}
				return f.held, nil
			},
			Send: func(_ context.Context, _ api.SupervisorLockOwner, version int, cmd string, _ map[string]any, _ int) (retainedGenerationIPCExchangeV1, error) {
				f.sendCalls = append(f.sendCalls, cmd)
				return send(cmd, version)
			},
			PublishSettled: func(event retainedGenerationSettledEventV1) (string, error) {
				f.publishCalls++
				f.lastEvent = event
				return strings.Repeat("f", 64), nil
			},
		},
		Now: func() time.Time { return time.Date(2026, 9, 10, 4, 1, 0, 0, time.UTC) },
	}
}

func retainedGenerationValidExchange(cmd string, version int) retainedGenerationIPCExchangeV1 {
	id := int64(41)
	result := map[string]any{"still_running": []any{}}
	args := map[string]any{"timeout_ms": defaultQuiesceTimeoutMs}
	if cmd == "exit" {
		id = 42
		result = map[string]any{"graceful_exit_initiated": true}
		args = map[string]any{"graceful": true, "timeout_ms": defaultExitTimeoutMs}
	}
	return retainedGenerationIPCExchangeV1{
		Request:  api.IPCRequest{Version: version, ID: id, Cmd: cmd, Args: args},
		Hello:    api.IPCHello{Version: 1, PID: 77, StartedAt: "2026-09-10T03:59:00Z"},
		Response: api.IPCResponse{ID: id, OK: true, Result: result, Final: true},
	}
}

func (f *retainedGenerationRunFixture) run(t *testing.T, lifecycle *windowsProductPairLifecycleHooks) error {
	t.Helper()
	return RunInstallUpgrade(context.Background(), UpgradeOpts{
		WindowsProduct:              f.txn,
		WindowsProductPairLifecycle: lifecycle,
		BinaryPath:                  f.paths.priorCLI,
		PipePath:                    "retained-generation-test-pipe",
		ExpectedPorts:               []int{9123, 9124, 9125},
		WaitSupervisorLockReleased: func(context.Context, time.Duration) error {
			f.waitLock++
			return nil
		},
		VerifyPortsUnbound: func([]int, time.Duration) error {
			f.verifyPorts++
			return nil
		},
		Deps: f.legacy,
	})
}

func TestRetainedGenerationPortPopulationAtomic(t *testing.T) {
	f := newRetainedGenerationRunFixture(t)
	lifecycle := f.lifecycle(
		func(int) (*retainedGenerationObservationV1, error) {
			copy := f.observation
			copy.Ports = append([]retainedGenerationPortObservationV1(nil), f.observation.Ports...)
			return &copy, nil
		},
		func(cmd string, version int) (retainedGenerationIPCExchangeV1, error) {
			return retainedGenerationValidExchange(cmd, version), nil
		},
	)
	if err := f.run(t, lifecycle); err != nil {
		t.Fatalf("retained mixed-population upgrade: %v", err)
	}
	if f.observeCall != 2 || f.held.verifyCalls != 1 || f.held.closeCalls != 1 || f.held.terminateCalls != 0 || strings.Join(f.sendCalls, ",") != "quiesce-timers,exit" || f.waitLock != 1 || f.verifyPorts != 1 || f.publishCalls != 1 {
		t.Fatalf("retained lifecycle observe=%d verify=%d close=%d terminate=%d send=%v wait=%d ports=%d publish=%d", f.observeCall, f.held.verifyCalls, f.held.closeCalls, f.held.terminateCalls, f.sendCalls, f.waitLock, f.verifyPorts, f.publishCalls)
	}
	if f.lastEvent.ReasonID != "retained-generation-gracefully-settled" || !validLowerHexSHA256(f.lastEvent.ObservationSHA256) || len(f.lastEvent.Ports) != 3 {
		t.Fatalf("settled event=%+v", f.lastEvent)
	}
	if f.legacy.quiesceCalled || f.legacy.exitCalled || f.legacy.forceKillCalled {
		t.Fatalf("retained route reached ordinary reap: %v", f.legacy.calls)
	}
	if f.txn.Opts.ReplacementAuthority.Quiescence == nil || !containsPartialPreventionStep(*f.pairOrder, "promote-cli") {
		t.Fatalf("authority=%+v pair_order=%v", f.txn.Opts.ReplacementAuthority, *f.pairOrder)
	}
}

func TestRetainedGenerationIdentityAndPopulationRejectionsHaveZeroEffects(t *testing.T) {
	tests := []struct {
		name   string
		wantID string
		mutate func(*retainedGenerationObservationV1, int)
	}{
		{name: "same_basename_other_path", wantID: "E_RETAINED_GENERATION_IMAGE_MISMATCH", mutate: func(o *retainedGenerationObservationV1, _ int) {
			o.Ports[2].ObservedImagePath = filepath.Join(filepath.Dir(filepath.Dir(o.SupervisorImagePath)), filepath.Base(o.SupervisorImagePath))
		}},
		{name: "parent", wantID: "E_RETAINED_GENERATION_PARENT_MISMATCH", mutate: func(o *retainedGenerationObservationV1, _ int) { o.Ports[2].ParentPID = 78 }},
		{name: "argv", wantID: "E_RETAINED_GENERATION_ARGV_MISMATCH", mutate: func(o *retainedGenerationObservationV1, _ int) { o.Ports[2].ArgvSHA256 = strings.Repeat("e", 64) }},
		{name: "gui", wantID: "E_UPGRADE_GUI_OWNER_PRESENT", mutate: func(o *retainedGenerationObservationV1, _ int) { o.GUIOwner = "unknown" }},
		{name: "missing_row", wantID: "E_RETAINED_GENERATION_PORT_POPULATION_MISMATCH", mutate: func(o *retainedGenerationObservationV1, _ int) { o.Ports = o.Ports[:2] }},
		{name: "extra_row", wantID: "E_RETAINED_GENERATION_PORT_POPULATION_MISMATCH", mutate: func(o *retainedGenerationObservationV1, _ int) {
			o.Ports = append(o.Ports, retainedGenerationPortObservationV1{TaskName: "extra", Port: 9199, State: windowsProductPortUnbound})
		}},
		{name: "duplicate_row", wantID: "E_RETAINED_GENERATION_PORT_POPULATION_MISMATCH", mutate: func(o *retainedGenerationObservationV1, _ int) { o.Ports[2] = o.Ports[1] }},
		{name: "generation_changed", wantID: "E_RETAINED_GENERATION_CHANGED", mutate: func(o *retainedGenerationObservationV1, call int) {
			if call == 2 {
				o.SupervisorStartedAt = "2026-09-10T03:59:03Z"
			}
		}},
		{name: "port_changed_on_reprobe", wantID: "E_RETAINED_GENERATION_PORT_POPULATION_MISMATCH", mutate: func(o *retainedGenerationObservationV1, call int) {
			if call == 2 {
				o.Ports[2].PID = 100
			}
		}},
		{name: "same_pid_different_start_on_reprobe", wantID: "E_RETAINED_GENERATION_PORT_POPULATION_MISMATCH", mutate: func(o *retainedGenerationObservationV1, call int) {
			if call == 2 {
				o.Ports[2].StartedAt = "2026-09-10T03:59:02.0000001Z"
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetainedGenerationRunFixture(t)
			lifecycle := f.lifecycle(func(call int) (*retainedGenerationObservationV1, error) {
				copy := f.observation
				copy.Ports = append([]retainedGenerationPortObservationV1(nil), f.observation.Ports...)
				tc.mutate(&copy, call)
				return &copy, nil
			}, func(cmd string, version int) (retainedGenerationIPCExchangeV1, error) {
				return retainedGenerationValidExchange(cmd, version), nil
			})
			err := f.run(t, lifecycle)
			if err == nil || !strings.Contains(err.Error(), tc.wantID) {
				t.Fatalf("error=%v want=%s", err, tc.wantID)
			}
			if len(f.sendCalls) != 0 || f.publishCalls != 0 || f.held.terminateCalls != 0 || f.legacy.forceKillCalled || f.legacy.quiesceCalled || f.legacy.exitCalled || f.txn.Opts.ReplacementAuthority.Quiescence != nil {
				t.Fatalf("rejection effects send=%v legacy=%v authority=%+v", f.sendCalls, f.legacy.calls, f.txn.Opts.ReplacementAuthority)
			}
			for _, forbidden := range []string{"promote-cli", "promote-windowless"} {
				if containsPartialPreventionStep(*f.pairOrder, forbidden) {
					t.Fatalf("rejection reached %s: %v", forbidden, *f.pairOrder)
				}
			}
		})
	}
}

func TestRetainedGenerationQuiesceEmptyStateDoesNotProvePorts(t *testing.T) {
	f := newRetainedGenerationRunFixture(t)
	lifecycle := f.lifecycle(func(int) (*retainedGenerationObservationV1, error) {
		copy := f.observation
		copy.Ports = append([]retainedGenerationPortObservationV1(nil), f.observation.Ports...)
		return &copy, nil
	}, func(cmd string, version int) (retainedGenerationIPCExchangeV1, error) {
		return retainedGenerationValidExchange(cmd, version), nil
	})
	f.verifyPorts = 0
	err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		WindowsProduct: f.txn, WindowsProductPairLifecycle: lifecycle,
		BinaryPath: f.paths.priorCLI, PipePath: "retained-stale-state", ExpectedPorts: []int{9123, 9124, 9125},
		WaitSupervisorLockReleased: func(context.Context, time.Duration) error { f.waitLock++; return nil },
		VerifyPortsUnbound:         func([]int, time.Duration) error { f.verifyPorts++; return errors.New("port 9125 remains bound") },
		Deps:                       f.legacy,
	})
	if err == nil || !strings.Contains(err.Error(), "E_RETAINED_GENERATION_QUIESCE_UNSETTLED") {
		t.Fatalf("error=%v want retained quiesce unsettled", err)
	}
	if strings.Join(f.sendCalls, ",") != "quiesce-timers,exit" || f.waitLock != 1 || f.verifyPorts != 1 || f.legacy.forceKillCalled || f.legacy.startCalled {
		t.Fatalf("calls send=%v wait=%d verify=%d legacy=%v", f.sendCalls, f.waitLock, f.verifyPorts, f.legacy.calls)
	}
	if f.txn.Opts.ReplacementAuthority.Quiescence != nil || containsPartialPreventionStep(*f.pairOrder, "promote-cli") {
		t.Fatalf("unsettled retained route mutated authority/pair: authority=%+v order=%v", f.txn.Opts.ReplacementAuthority, *f.pairOrder)
	}
}

func TestRetainedGenerationProtocolRejectionNeverFallsThroughOrForces(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    func(*retainedGenerationIPCExchangeV1)
		wantCalls string
	}{
		{name: "quiesce", wantCalls: "quiesce-timers", mutate: func(exchange *retainedGenerationIPCExchangeV1) {
			exchange.Response.Result = map[string]any{"still_running": []any{float64(99)}}
		}},
		{name: "exit", wantCalls: "quiesce-timers,exit", mutate: func(exchange *retainedGenerationIPCExchangeV1) {
			exchange.Response.Result = map[string]any{"graceful_exit_initiated": false}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetainedGenerationRunFixture(t)
			lifecycle := f.lifecycle(func(int) (*retainedGenerationObservationV1, error) {
				copy := f.observation
				copy.Ports = append([]retainedGenerationPortObservationV1(nil), f.observation.Ports...)
				return &copy, nil
			}, func(cmd string, version int) (retainedGenerationIPCExchangeV1, error) {
				exchange := retainedGenerationValidExchange(cmd, version)
				if (tc.name == "quiesce" && cmd == "quiesce-timers") || tc.name == cmd {
					tc.mutate(&exchange)
				}
				return exchange, nil
			})
			err := f.run(t, lifecycle)
			if err == nil || !strings.Contains(err.Error(), "E_RETAINED_GENERATION_PROTOCOL_UNSUPPORTED") {
				t.Fatalf("error=%v", err)
			}
			if strings.Join(f.sendCalls, ",") != tc.wantCalls || f.legacy.forceKillCalled || f.legacy.quiesceCalled || f.legacy.exitCalled || f.legacy.startCalled || f.txn.Opts.ReplacementAuthority.Quiescence != nil {
				t.Fatalf("calls=%v legacy=%v authority=%+v", f.sendCalls, f.legacy.calls, f.txn.Opts.ReplacementAuthority)
			}
			if containsPartialPreventionStep(*f.pairOrder, "promote-cli") {
				t.Fatalf("protocol rejection reached pair transaction: %v", *f.pairOrder)
			}
		})
	}
}

func TestRetainedGenerationIPCEnvelopeCompatibility(t *testing.T) {
	owner := api.SupervisorLockOwner{PID: 77, StartedAt: "2026-09-10T03:59:00Z"}
	for _, version := range []int{0, 1} {
		for _, cmd := range []string{"quiesce-timers", "exit"} {
			if err := validateRetainedGenerationIPCExchange(owner, version, cmd, retainedGenerationValidExchange(cmd, version)); err != nil {
				t.Fatalf("version=%d cmd=%s: %v", version, cmd, err)
			}
		}
	}
	bad := []struct {
		name   string
		cmd    string
		mutate func(*retainedGenerationIPCExchangeV1)
	}{
		{name: "version", cmd: "quiesce-timers", mutate: func(e *retainedGenerationIPCExchangeV1) { e.Request.Version = 2 }},
		{name: "quiesce_args", cmd: "quiesce-timers", mutate: func(e *retainedGenerationIPCExchangeV1) { e.Request.Args = map[string]any{} }},
		{name: "exit_args", cmd: "exit", mutate: func(e *retainedGenerationIPCExchangeV1) {
			e.Request.Args = map[string]any{"graceful": false, "timeout_ms": defaultExitTimeoutMs}
		}},
		{name: "hello", cmd: "quiesce-timers", mutate: func(e *retainedGenerationIPCExchangeV1) { e.Hello.PID++ }},
		{name: "id", cmd: "quiesce-timers", mutate: func(e *retainedGenerationIPCExchangeV1) { e.Response.ID++ }},
		{name: "not_final", cmd: "quiesce-timers", mutate: func(e *retainedGenerationIPCExchangeV1) { e.Response.Final = false }},
		{name: "not_ok", cmd: "quiesce-timers", mutate: func(e *retainedGenerationIPCExchangeV1) { e.Response.OK = false }},
		{name: "mixed_error", cmd: "quiesce-timers", mutate: func(e *retainedGenerationIPCExchangeV1) {
			e.Response.Error = &api.IPCErr{Code: "mixed", Message: "mixed"}
		}},
		{name: "still_running", cmd: "quiesce-timers", mutate: func(e *retainedGenerationIPCExchangeV1) {
			e.Response.Result = map[string]any{"still_running": []any{float64(9)}}
		}},
		{name: "malformed_result", cmd: "quiesce-timers", mutate: func(e *retainedGenerationIPCExchangeV1) { e.Response.Result = "malformed" }},
		{name: "exit_result", cmd: "exit", mutate: func(e *retainedGenerationIPCExchangeV1) {
			e.Response.Result = map[string]any{"graceful_exit_initiated": false}
		}},
		{name: "unknown_command", cmd: "unknown", mutate: func(*retainedGenerationIPCExchangeV1) {}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			exchange := retainedGenerationValidExchange(tc.cmd, 0)
			tc.mutate(&exchange)
			if err := validateRetainedGenerationIPCExchange(owner, 0, tc.cmd, exchange); err == nil || !strings.Contains(err.Error(), "E_RETAINED_GENERATION_PROTOCOL_UNSUPPORTED") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestObserveRetainedGenerationClassifiesCompleteMixedPartition(t *testing.T) {
	observedAt := time.Date(2026, 9, 10, 4, 0, 0, 0, time.UTC)
	supervisorStarted := "2026-09-10T03:59:00Z"
	canonical := filepath.Join(t.TempDir(), "mcphub.exe")
	windowless := filepath.Join(filepath.Dir(canonical), "mcphub-windowless.exe")
	retained := filepath.Join(filepath.Dir(canonical), "retained", "mcphub.exe.old-20260910T035900Z")
	commands := map[int]string{
		77: `"` + retained + `" supervise`,
		88: `"` + canonical + `" daemon --server canonical --daemon default --port 9124`,
		99: `"` + canonical + `" daemon --server retained --daemon default --port 9125`,
	}
	frozen := []windowsProductIntentPortV1{
		{TaskName: "task-unbound", Port: 9123, ArgvSHA256: windowsProductArgvSHA256([]string{canonical, "daemon", "--server", "unbound", "--daemon", "default", "--port", "9123"})},
		{TaskName: "task-canonical", Port: 9124, ArgvSHA256: windowsProductArgvSHA256(process.TokenizeWindowsCommandLine(commands[88]))},
		{TaskName: "task-retained", Port: 9125, ArgvSHA256: windowsProductArgvSHA256(process.TokenizeWindowsCommandLine(commands[99]))},
	}
	identities := map[int]process.ProcessIdentity{
		77: {PID: 77, Basename: "mcphub.exe.old-20260910T035900Z", CommandLine: commands[77], ExecutablePath: retained, CreationDateUnix: observedAt.Add(-time.Minute).Unix()},
		88: {PID: 88, Basename: "mcphub.exe", CommandLine: commands[88], ExecutablePath: canonical, CreationDateUnix: observedAt.Add(-50 * time.Second).Unix()},
		99: {PID: 99, Basename: "mcphub.exe.old-20260910T035900Z", CommandLine: commands[99], ExecutablePath: retained, CreationDateUnix: observedAt.Add(-40 * time.Second).Unix()},
	}
	handleIdentities := map[int]process.PIDHandleIdentity{
		77: {PID: 77, ExecutablePath: retained, StartedAt: time.Date(2026, 9, 10, 3, 59, 0, 0, time.UTC)},
		88: {PID: 88, ExecutablePath: canonical, StartedAt: observedAt.Add(-50 * time.Second)},
		99: {PID: 99, ExecutablePath: retained, StartedAt: observedAt.Add(-40 * time.Second)},
	}
	parents := map[int]int{88: 77, 99: 77}
	portPIDs := map[int]int{9124: 88, 9125: 99}
	observation, err := observeRetainedGenerationWithDeps(context.Background(), t.TempDir(), canonical, windowless, frozen, retainedGenerationObservationDeps{
		TrySupervisorAbsent: func(string) (bool, error) { return false, nil },
		ReadSupervisorOwner: func(string) (api.SupervisorLockOwner, error) {
			return api.SupervisorLockOwner{PID: 77, StartedAt: supervisorStarted}, nil
		},
		LookupIdentity:      func(pid int) (process.ProcessIdentity, error) { return identities[pid], nil },
		OwnerMatchesCurrent: func(int) (bool, error) { return true, nil },
		ConfirmGUIAbsent:    func(string) error { return nil },
		HandleIdentity:      func(pid int) (process.PIDHandleIdentity, error) { return handleIdentities[pid], nil },
		ParentPID:           func(pid int) (int, error) { return parents[pid], nil },
		PortOwner: func(_ context.Context, port int) (int, bool, error) {
			pid, ok := portPIDs[port]
			return pid, ok, nil
		},
		CurrentPairReceipt: func(string) (windowsProductPreimageV1, error) {
			return windowsProductPreimageV1{Present: true, SHA256: strings.Repeat("e", 64)}, nil
		},
		Now: func() time.Time { return observedAt },
	})
	if err != nil {
		t.Fatalf("observe mixed retained generation: %v", err)
	}
	if observation == nil || observation.SupervisorPID != 77 || !samePath(observation.SupervisorImagePath, retained) || len(observation.Ports) != 3 {
		t.Fatalf("observation=%+v", observation)
	}
	wantStates := []windowsProductPortStateV1{windowsProductPortUnbound, windowsProductPortCanonicalOwned, windowsProductPortRetainedOwned}
	for i, want := range wantStates {
		if observation.Ports[i].State != want {
			t.Fatalf("port[%d] state=%s want=%s", i, observation.Ports[i].State, want)
		}
	}
}

func TestObserveRetainedGenerationRequiresPreciseChildStartIdentity(t *testing.T) {
	base := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	supervisorStarted := base.Add(250 * time.Millisecond)
	observedAt := base.Add(900 * time.Millisecond)
	canonical := filepath.Join(t.TempDir(), "mcphub.exe")
	windowless := filepath.Join(filepath.Dir(canonical), "mcphub-windowless.exe")
	retained := filepath.Join(filepath.Dir(canonical), "retained", "mcphub.exe.old-synthetic")
	supervisorCommand := `"` + retained + `" supervise`
	childCommand := `"` + retained + `" daemon --server synthetic --daemon default --port 9124`
	frozen := []windowsProductIntentPortV1{{
		TaskName: "synthetic-retained", Port: 9124,
		ArgvSHA256: windowsProductArgvSHA256(process.TokenizeWindowsCommandLine(childCommand)),
	}}

	observe := func(childHandle process.PIDHandleIdentity, childHandleErr error) (*retainedGenerationObservationV1, error) {
		identities := map[int]process.ProcessIdentity{
			1001: {PID: 1001, Basename: "mcphub.exe.old-synthetic", CommandLine: supervisorCommand, ExecutablePath: retained, CreationDateUnix: base.Unix()},
			1002: {PID: 1002, Basename: "mcphub.exe.old-synthetic", CommandLine: childCommand, ExecutablePath: retained, CreationDateUnix: base.Unix()},
		}
		return observeRetainedGenerationWithDeps(context.Background(), t.TempDir(), canonical, windowless, frozen, retainedGenerationObservationDeps{
			TrySupervisorAbsent: func(string) (bool, error) { return false, nil },
			ReadSupervisorOwner: func(string) (api.SupervisorLockOwner, error) {
				return api.SupervisorLockOwner{PID: 1001, StartedAt: supervisorStarted.Format(time.RFC3339Nano)}, nil
			},
			LookupIdentity:      func(pid int) (process.ProcessIdentity, error) { return identities[pid], nil },
			OwnerMatchesCurrent: func(int) (bool, error) { return true, nil },
			ConfirmGUIAbsent:    func(string) error { return nil },
			HandleIdentity: func(pid int) (process.PIDHandleIdentity, error) {
				if pid == 1001 {
					return process.PIDHandleIdentity{PID: 1001, ExecutablePath: retained, StartedAt: base}, nil
				}
				return childHandle, childHandleErr
			},
			ParentPID: func(int) (int, error) { return 1001, nil },
			PortOwner: func(context.Context, int) (int, bool, error) {
				return 1002, true, nil
			},
			CurrentPairReceipt: func(string) (windowsProductPreimageV1, error) {
				return windowsProductPreimageV1{Present: true, SHA256: strings.Repeat("a", 64)}, nil
			},
			Now: func() time.Time { return observedAt },
		})
	}

	observeChild := func(startedAt time.Time) process.PIDHandleIdentity {
		return process.PIDHandleIdentity{PID: 1002, ExecutablePath: retained, StartedAt: startedAt}
	}
	t.Run("same_second_child_after_fractional_supervisor", func(t *testing.T) {
		observation, err := observe(observeChild(base.Add(500*time.Millisecond)), nil)
		if err != nil || observation == nil {
			t.Fatalf("same-second precise child observation=(%+v, %v), want accepted", observation, err)
		}
		if got := observation.Ports[0].StartedAt; got != base.Add(500*time.Millisecond).Format(time.RFC3339Nano) {
			t.Fatalf("child started_at=%q, want precise handle time", got)
		}
	})
	t.Run("actual_older_same_second_child_rejected", func(t *testing.T) {
		observation, err := observe(observeChild(base.Add(200*time.Millisecond)), nil)
		if observation != nil || err == nil || !strings.Contains(err.Error(), "E_RETAINED_GENERATION_CHANGED") {
			t.Fatalf("older child observation=(%+v, %v), want changed rejection", observation, err)
		}
	})
	for _, tc := range []struct {
		name          string
		handle        process.PIDHandleIdentity
		handleErr     error
		wantFailureID string
	}{
		{name: "unknown_getter", handleErr: errors.New("injected handle failure"), wantFailureID: "E_RETAINED_GENERATION_CHANGED"},
		{name: "zero_start", handle: observeChild(time.Time{}), wantFailureID: "E_RETAINED_GENERATION_CHANGED"},
		{name: "empty_path", handle: process.PIDHandleIdentity{PID: 1002, StartedAt: base.Add(500 * time.Millisecond)}, wantFailureID: "E_RETAINED_GENERATION_CHANGED"},
		{name: "pid_mismatch", handle: process.PIDHandleIdentity{PID: 1003, ExecutablePath: retained, StartedAt: base.Add(500 * time.Millisecond)}, wantFailureID: "E_RETAINED_GENERATION_CHANGED"},
		{name: "foreign_path", handle: process.PIDHandleIdentity{PID: 1002, ExecutablePath: filepath.Join(filepath.Dir(retained), "other.exe"), StartedAt: base.Add(500 * time.Millisecond)}, wantFailureID: "E_RETAINED_GENERATION_IMAGE_MISMATCH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observation, err := observe(tc.handle, tc.handleErr)
			if observation != nil || err == nil || !strings.Contains(err.Error(), tc.wantFailureID) {
				t.Fatalf("child identity observation=(%+v, %v), want %s", observation, err, tc.wantFailureID)
			}
		})
	}
}

func TestObserveRetainedGenerationIncompleteIdentityDepsFailClosed(t *testing.T) {
	canonical := filepath.Join(t.TempDir(), "mcphub.exe")
	observation, err := observeRetainedGenerationWithDeps(context.Background(), t.TempDir(), canonical, filepath.Join(filepath.Dir(canonical), "mcphub-windowless.exe"), nil, retainedGenerationObservationDeps{
		ConfirmGUIAbsent: func(string) error { return nil },
	})
	if err == nil || observation != nil || !strings.Contains(err.Error(), "E_RETAINED_GENERATION_CHANGED") {
		t.Fatalf("observation=%+v error=%v", observation, err)
	}
}

func TestSendRetainedGenerationIPCRejectsWrongHelloBeforeRequest(t *testing.T) {
	stateDir := withTempStateDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	pipePath := api.SupervisorIPCAddress(stateDir)
	if !strings.Contains(pipePath, "mcphub-supervisor-test-") {
		t.Fatalf("refusing non-test pipe %q", pipePath)
	}
	listener, err := winio.ListenPipe(pipePath, &winio.PipeConfig{MessageMode: false, InputBufferSize: 4096, OutputBufferSize: 4096})
	if err != nil {
		t.Fatalf("listen isolated test pipe: %v", err)
	}
	defer listener.Close()
	receivedRequest := make(chan bool, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			receivedRequest <- false
			return
		}
		defer conn.Close()
		hello, _ := json.Marshal(map[string]any{"hello": api.IPCHello{Version: 1, PID: 78, StartedAt: "2026-09-10T03:59:00Z"}})
		_, _ = conn.Write(append(hello, '\n'))
		_ = conn.SetReadDeadline(time.Now().Add(750 * time.Millisecond))
		buffer := make([]byte, 4096)
		n, _ := conn.Read(buffer)
		receivedRequest <- n != 0
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = sendRetainedGenerationIPC(ctx, pipePath, api.SupervisorLockOwner{PID: 77, StartedAt: "2026-09-10T03:59:00Z"}, 0, "quiesce-timers", map[string]any{"timeout_ms": 100}, 100)
	if err == nil {
		t.Fatal("wrong hello unexpectedly accepted")
	}
	select {
	case sent := <-receivedRequest:
		if sent {
			t.Fatal("retained client sent a request after the supervisor hello generation mismatched")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake retained supervisor did not settle")
	}
}

func TestSendRetainedGenerationIPCAcceptsV0V1ExactFinal(t *testing.T) {
	for _, version := range []int{0, 1} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			stateDir := withTempStateDir(t)
			t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
			pipePath := api.SupervisorIPCAddress(stateDir)
			if !strings.Contains(pipePath, "mcphub-supervisor-test-") {
				t.Fatalf("refusing non-test pipe %q", pipePath)
			}
			listener, err := winio.ListenPipe(pipePath, &winio.PipeConfig{MessageMode: false, InputBufferSize: 4096, OutputBufferSize: 4096})
			if err != nil {
				t.Fatalf("listen isolated test pipe: %v", err)
			}
			defer listener.Close()
			owner := api.SupervisorLockOwner{PID: 77, StartedAt: "2026-09-10T03:59:00Z"}
			serverResult := make(chan struct {
				request    api.IPCRequest
				rawRequest map[string]json.RawMessage
				err        error
			}, 1)
			go func() {
				var result struct {
					request    api.IPCRequest
					rawRequest map[string]json.RawMessage
					err        error
				}
				conn, err := listener.Accept()
				if err != nil {
					result.err = err
					serverResult <- result
					return
				}
				defer conn.Close()
				hello, _ := json.Marshal(map[string]any{"hello": api.IPCHello{Version: 1, PID: owner.PID, StartedAt: owner.StartedAt}})
				if _, err := conn.Write(append(hello, '\n')); err != nil {
					result.err = err
					serverResult <- result
					return
				}
				line, err := readLine(conn, 4096)
				if err != nil {
					result.err = err
					serverResult <- result
					return
				}
				if err := json.Unmarshal(line, &result.request); err != nil {
					result.err = err
					serverResult <- result
					return
				}
				if err := json.Unmarshal(line, &result.rawRequest); err != nil {
					result.err = err
					serverResult <- result
					return
				}
				accepted, _ := json.Marshal(api.IPCResponse{ID: result.request.ID, OK: true, Result: map[string]any{"accepted": true}})
				final, _ := json.Marshal(api.IPCResponse{ID: result.request.ID, OK: true, Result: map[string]any{"drained": float64(0), "still_running": []any{}}, Final: true})
				if _, err := conn.Write(append(accepted, '\n')); err == nil {
					_, err = conn.Write(append(final, '\n'))
				}
				result.err = err
				serverResult <- result
			}()

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			exchange, err := sendRetainedGenerationIPC(ctx, pipePath, owner, version, "quiesce-timers", map[string]any{"timeout_ms": 100}, 100)
			if err != nil {
				t.Fatalf("send retained IPC v%d: %v", version, err)
			}
			if err := validateRetainedGenerationIPCExchange(owner, version, "quiesce-timers", exchange); err != nil {
				t.Fatalf("validate retained IPC v%d: %v", version, err)
			}
			select {
			case result := <-serverResult:
				if result.err != nil || result.request.Version != version || result.request.Cmd != "quiesce-timers" {
					t.Fatalf("server request=%+v err=%v", result.request, result.err)
				}
				wireVersion, present := result.rawRequest["version"]
				if version == 0 && present {
					t.Fatalf("v0 request encoded version=%s; the compatibility envelope must omit it", wireVersion)
				}
				if version == 1 {
					if !present {
						t.Fatal("v1 request omitted the explicit version field")
					}
					var got int
					if err := json.Unmarshal(wireVersion, &got); err != nil || got != 1 {
						t.Fatalf("v1 request encoded version=%s decode=%d error=%v", wireVersion, got, err)
					}
				}
			case <-time.After(2 * time.Second):
				t.Fatal("fake retained supervisor did not settle")
			}
		})
	}
}

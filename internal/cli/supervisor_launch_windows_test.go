//go:build windows

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/gui"
	"mcp-local-hub/internal/process"
)

func TestSupervisorLaunchCommittedStartupFailureKeepsForwardPending(t *testing.T) {
	f := newProductRecoveryV3Fixture(t, WindowsProductRecoveryCommitDecided)
	tasks := &recoveryTaskTxnFake{digest: f.journal.TaskSetSHA256}
	req := f.request(tasks)
	req.ObserveSuccessorReady = func(context.Context, string, string) (bool, error) { return false, nil }
	launch := &fixtureSupervisorLaunch{}
	req.StartSupervisor = func(string) (SupervisorLaunch, error) { return launch, nil }
	req.WaitSupervisorReady = func(context.Context, string, string) error { return errors.New("committed readiness failed") }
	req.Deps.AdmitProduct = func(in binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error) {
		in.SHA256 = f.journal.Product.NewSHA256
		return in, nil
	}
	_, err := reconcileWindowsProductRecovery(req)
	if !errors.Is(err, ErrWindowsProductCleanupPending) || launch.aborts != 1 || launch.handoffs != 0 {
		t.Fatalf("err=%v launch=%+v", err, launch)
	}
	assertPairFile(t, f.product, f.newProduct)
	if _, err := os.Stat(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)); err != nil {
		t.Fatal("committed journal lost")
	}
}

func TestSupervisorLaunchCommitDecisionAmbiguity(t *testing.T) {
	for _, mode := range []string{"prior", "commit", "missing", "foreign-transaction"} {
		t.Run(mode, func(t *testing.T) {
			txn, _, order := newProductTxnFixture(t)
			launch := &fixtureSupervisorLaunch{order: order}
			txn.Opts.Deps.StartSupervisor = func(string) (SupervisorLaunch, error) { return launch, nil }
			write := txn.Opts.Deps.WriteRecoveryJournal
			txn.Opts.Deps.WriteRecoveryJournal = func(path string, raw []byte) error {
				j, err := decodeWindowsProductRecoveryJournalV3(raw)
				if err != nil {
					return err
				}
				if j.Phase != WindowsProductRecoveryCommitDecided {
					return write(path, raw)
				}
				switch mode {
				case "commit":
					if err := write(path, raw); err != nil {
						return err
					}
				case "missing":
					if err := os.Remove(path); err != nil {
						return err
					}
				case "foreign-transaction":
					j.TransactionID = strings.Repeat("f", 32)
					foreign, err := encodeWindowsProductRecoveryJournalV3(j)
					if err != nil {
						return err
					}
					if err := write(path, foreign); err != nil {
						return err
					}
				}
				return errors.New("ambiguous commit writer outcome")
			}
			_, err := txn.Run(context.Background())
			if err == nil {
				t.Fatal("ambiguous persistence became success")
			}
			if mode == "prior" {
				if launch.aborts != 1 || launch.handoffs != 0 || !recoveryStepsOrdered(*order, []string{"abort-launch", "tasks-restore"}) {
					t.Fatalf("prior err=%v launch=%+v order=%v", err, launch, *order)
				}
			} else {
				if launch.aborts != 0 || launch.handoffs != 1 || strings.Contains(strings.Join(*order, ","), "tasks-restore") {
					t.Fatalf("forward/unknown err=%v launch=%+v order=%v", err, launch, *order)
				}
				if mode != "commit" && !strings.Contains(err.Error(), "commit decision is unknown") {
					t.Fatalf("unknown classified committed: %v", err)
				}
			}
		})
	}
	for _, snapshots := range [][2][]byte{{nil, nil}, {[]byte("same"), []byte("same")}} {
		txn := &WindowsProductTxn{}
		if decision, err := txn.readCommitDecision(snapshots[0], snapshots[1]); decision != productDecisionUnknown || err == nil {
			t.Fatalf("degraded snapshots: %v %v", decision, err)
		}
	}
}

func TestSupervisorLaunchPriorFailureBranches(t *testing.T) {
	for _, stage := range []string{"bind", "ready", "independent", "reservation", "gui-task", "health", "cancel", "success", "borrowed"} {
		t.Run(stage, func(t *testing.T) {
			launch := &fixtureSupervisorLaunch{}
			fail := errors.New(stage + " failed")
			lease := &upgradeGUITestLease{}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			guiStarted := false
			ops := guiOwnedPriorRecoveryOps{
				Readback: func(windowsProductRecoveryJournalV3) error { return nil },
				ObserveHealthy: func(context.Context, windowsProductRecoveryJournalV3) error {
					if stage == "borrowed" || (guiStarted && stage != "health" && stage != "cancel") {
						return nil
					}
					return fail
				},
				Reservation: func(context.Context) (gui.OwnedSingleInstanceLease, error) { return lease, nil },
				SupervisorState: func(context.Context, windowsProductRecoveryJournalV3) (guiOwnedPriorSupervisorState, error) {
					return guiOwnedPriorSupervisorAbsent, nil
				},
				SettleSame:      func(context.Context, windowsProductRecoveryJournalV3) error { return nil },
				ProveAbsent:     func(context.Context, windowsProductRecoveryJournalV3) error { return nil },
				StartSupervisor: func() (SupervisorLaunch, error) { return launch, nil },
				WaitSupervisorReady: func(context.Context) error {
					if stage == "ready" {
						return fail
					}
					return nil
				},
				ProveIndependent: func(context.Context, windowsProductRecoveryJournalV3) error {
					if stage == "independent" {
						return fail
					}
					return nil
				},
				StartGUI: func() error {
					if stage == "gui-task" {
						return fail
					}
					guiStarted = true
					if stage == "cancel" {
						cancel()
					}
					return nil
				},
				PollInterval: time.Millisecond,
			}
			if stage == "bind" {
				launch.bindErr = fail
			}
			if stage == "reservation" {
				lease.releaseErr = fail
			}
			err := recoverGUIOwnedPriorSettlementWithOps(ctx, windowsProductRecoveryJournalV3{PreSettlement: &windowsProductPreSettlementV3{GUIOwner: windowsProductGUIOwnerConfirmedPresent}}, ops)
			if stage == "success" {
				if err != nil || launch.aborts != 0 || launch.handoffs != 1 {
					t.Fatalf("err=%v launch=%+v", err, launch)
				}
			} else if stage == "borrowed" {
				if err != nil || launch.binds+launch.aborts+launch.handoffs != 0 {
					t.Fatalf("borrowed custody=%+v err=%v", launch, err)
				}
			} else if !errors.Is(err, ErrWindowsProductRecoveryPending) || launch.aborts != 1 || launch.handoffs != 0 {
				t.Fatalf("err=%v launch=%+v", err, launch)
			}
		})
	}
}

func TestSupervisorLaunchProductPhaseReceiving(t *testing.T) {
	for _, stage := range []WindowsProductStage{WindowsProductStageSupervisorStarted, WindowsProductStageSupervisorReady, WindowsProductStageCommitDecided, WindowsProductStageReceiptWritten} {
		t.Run(string(stage), func(t *testing.T) {
			txn, paths, order := newProductTxnFixture(t)
			launch := &fixtureSupervisorLaunch{order: order}
			txn.Opts.Deps.StartSupervisor = func(string) (SupervisorLaunch, error) { return launch, nil }
			txn.Opts.Deps.Fault = func(at WindowsProductStage) error {
				if at == stage {
					return errors.New("phase fault")
				}
				return nil
			}
			_, err := txn.Run(context.Background())
			if err == nil {
				t.Fatal("fault disappeared")
			}
			if stage == WindowsProductStageSupervisorStarted || stage == WindowsProductStageSupervisorReady {
				if launch.aborts != 1 || launch.handoffs != 0 || !recoveryStepsOrdered(*order, []string{"abort-launch", "tasks-restore"}) {
					t.Fatalf("predecision launch=%+v order=%v", launch, *order)
				}
			} else {
				if launch.aborts != 0 || launch.handoffs != 1 {
					t.Fatalf("committed launch=%+v order=%v", launch, *order)
				}
				if _, err := os.Stat(paths.product); err != nil {
					t.Fatal("committed Product removed")
				}
			}
		})
	}
}

func TestSupervisorLaunchChildFixture(t *testing.T) {
	if os.Getenv("MCPHUB_TEST_OWNED_PRECONTROL_CHILD") != "1" {
		return
	}
	// A deliberate pre-owner/IPC stall. Only its original creator can stop it.
	time.Sleep(time.Hour)
}

func TestSupervisorLaunchBindingCustody(t *testing.T) {
	releaseFailure := errors.New("synthetic original Release outcome")
	launch := &ownedSupervisorLaunch{handedOff: true, releaseErr: releaseFailure}
	launch.proof = &supervisorControlProof{issuer: launch}
	binding := SupervisorControlBinding{proof: launch.proof}
	// No creator is attached: any resumed destructive authority would panic.
	for i := 0; i < 2; i++ {
		if err := launch.Handoff(context.Background(), binding); !errors.Is(err, releaseFailure) {
			t.Fatalf("transferred Release outcome lost: %v", err)
		}
	}
	if transferred, err := launch.handoffDisposition(); !transferred || !errors.Is(err, releaseFailure) {
		t.Fatalf("actual transfer/Release disposition lost: transferred=%v error=%v", transferred, err)
	}
	if err := launch.Abort(context.Background()); err == nil {
		t.Fatal("managed actor regained Abort authority")
	}
	for _, bad := range []SupervisorControlBinding{{}, {proof: &supervisorControlProof{issuer: &ownedSupervisorLaunch{}}}} {
		if err := launch.Handoff(context.Background(), bad); err == nil {
			t.Fatal("missing/foreign binding accepted")
		}
	}
	settled := &ownedSupervisorLaunch{settled: true}
	settled.proof = &supervisorControlProof{issuer: settled}
	if err := settled.Handoff(context.Background(), SupervisorControlBinding{proof: settled.proof}); err == nil {
		t.Fatal("settled actor handed off")
	}
}

func TestSupervisorLaunchOriginalChildOutcome(t *testing.T) {
	waitFailure := errors.New("synthetic original Wait outcome")
	clearanceFailure := errors.New("synthetic namespace still occupied")
	launch := &ownedSupervisorLaunch{waitDone: make(chan struct{}), settleErr: clearanceFailure}
	if received, err := launch.originalChildOutcome(); received || err != nil {
		t.Fatalf("pending original Wait fabricated: received=%v error=%v", received, err)
	}
	// Publish the actual channel protocol synthetically; no second Wait or
	// native ownership is created. Namespace clearance remains unsuccessful.
	launch.waitErr = waitFailure
	close(launch.waitDone)
	for _, aggregateSettled := range []bool{false, true} {
		launch.settled = aggregateSettled
		if received, err := launch.originalChildOutcome(); !received || !errors.Is(err, waitFailure) {
			t.Fatalf("completed original Wait hidden by aggregate settled=%v: received=%v error=%v", aggregateSettled, received, err)
		}
	}
	if transferred, err := launch.handoffDisposition(); transferred || err != nil {
		t.Fatalf("child exit fabricated transfer: transferred=%v error=%v", transferred, err)
	}
}

func TestSupervisorLaunchDependencyCapture(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	adapter := buildV5UpgradeDeps(exe, stateDir)
	lockPath, pipePath := adapter.supervisorLockDir, adapter.pipePath
	owner := api.SupervisorLockOwner{PID: 1234, StartedAt: "2026-10-16T00:00:00Z"}
	peer := api.SupervisorProcessIdentityV1{PID: owner.PID}
	adapter.readSupervisorOwner = func(path string) (api.SupervisorLockOwner, error) {
		if path != lockPath {
			t.Fatalf("captured reader namespace changed: %s", path)
		}
		return owner, nil
	}
	adapter.sendIPCPeer = func(_ context.Context, pipe string, gotOwner api.SupervisorLockOwner, gotPeer api.SupervisorProcessIdentityV1, command string, _ map[string]any, _ time.Duration) (api.IPCResponse, error) {
		if pipe != pipePath || gotOwner != owner || gotPeer != peer || command != "exit" {
			t.Fatal("captured strict sender operands changed")
		}
		return api.IPCResponse{OK: true, Final: true}, nil
	}
	deps, err := adapter.supervisorLaunchDependencies()
	if err != nil {
		t.Fatal(err)
	}
	held, err := api.AcquireSupervisorLockQuiet(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(held.Release)
	// The composed value must not consult the later transaction again.
	adapter.exePath = filepath.Join(stateDir, "different.exe")
	adapter.supervisorLockDir = filepath.Join(stateDir, "different.lock")
	adapter.pipePath = "different-pipe"
	adapter.readSupervisorOwner = func(string) (api.SupervisorLockOwner, error) {
		t.Fatal("late reader resolution")
		return api.SupervisorLockOwner{}, nil
	}
	adapter.sendIPCPeer = func(context.Context, string, api.SupervisorLockOwner, api.SupervisorProcessIdentityV1, string, map[string]any, time.Duration) (api.IPCResponse, error) {
		t.Fatal("late strict sender resolution")
		return api.IPCResponse{}, nil
	}
	if deps.exePath != exe || deps.supervisorLockPath != lockPath || deps.pipePath != pipePath {
		t.Fatal("launch namespace was not captured by value")
	}
	if got, err := deps.readOwner(deps.supervisorLockPath); err != nil || got != owner {
		t.Fatalf("captured reader lost: %+v / %v", got, err)
	}
	if response, err := deps.sendIPCPeer(context.Background(), deps.pipePath, owner, peer, "exit", nil, time.Second); err != nil || !response.OK || !response.Final {
		t.Fatalf("captured sender lost: %+v / %v", response, err)
	}
	// The existing waiter probes before inspecting cancellation. The original
	// namespace is held; retargeting the free later namespace would return nil.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := deps.waitLockReleased(cancelled, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("captured waiter did not observe held original namespace: %v", err)
	}
	held.Release()
	if err := deps.waitLockReleased(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}

	for _, scenario := range []string{"missing-exe", "missing-lock", "wrong-lock", "missing-pipe", "foreign-pipe", "missing-reader", "missing-waiter", "missing-strict-sender"} {
		t.Run(scenario, func(t *testing.T) {
			missing := deps
			switch scenario {
			case "missing-exe":
				missing.exePath = ""
			case "missing-lock":
				missing.supervisorLockPath = ""
			case "wrong-lock":
				missing.supervisorLockPath = filepath.Join(stateDir, "other.lock")
			case "missing-pipe":
				missing.pipePath = ""
			case "foreign-pipe":
				missing.pipePath = "foreign-pipe"
			case "missing-reader":
				missing.readOwner = nil
			case "missing-waiter":
				missing.waitLockReleased = nil
			case "missing-strict-sender":
				missing.sendIPCPeer = nil
			}
			if err := missing.validate(); err == nil {
				t.Fatal("incomplete captured dependencies accepted")
			}
		})
	}
	if err := (supervisorLaunchDependencies{}).validate(); err == nil {
		t.Fatal("zero captured dependencies accepted")
	}
}

func TestSupervisorLaunchDependencyLegacyOnlyRefused(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	adapter := buildV5UpgradeDeps(exe, t.TempDir())
	adapter.sendIPCPeer = nil
	adapter.sendIPC = func(context.Context, string, api.SupervisorLockOwner, string, map[string]any, time.Duration) (api.IPCResponse, error) {
		t.Fatal("legacy sender used for strict launch")
		return api.IPCResponse{}, nil
	}
	if _, err := adapter.supervisorLaunchDependencies(); !errors.Is(err, errSupervisorPeerIPCSenderRequired) {
		t.Fatalf("legacy-only override silently bypassed: %v", err)
	}
	if launch, err := adapter.StartSupervisor(exe); launch != nil || !errors.Is(err, errSupervisorPeerIPCSenderRequired) {
		t.Fatalf("strict omission reached creation: launch=%v error=%v", launch, err)
	}
}

func TestSupervisorLaunchStalledCreatorCleanup(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	build := func() *exec.Cmd {
		cmd := exec.Command(exe, "-test.run=^TestSupervisorLaunchChildFixture$")
		cmd.Env = append(os.Environ(), "MCPHUB_TEST_OWNED_PRECONTROL_CHILD=1")
		process.NoConsole(cmd)
		return cmd
	}
	// Creator mechanics are contained by QA's existing Job; breakaway policy is
	// independently covered by the unchanged producer/native Product oracle.
	for _, scenario := range []string{"complete-dependencies", "missing-captured-reader"} {
		t.Run(scenario, func(t *testing.T) {
			stateDir := t.TempDir()
			deps, err := buildV5UpgradeDeps(exe, stateDir).supervisorLaunchDependencies()
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "missing-captured-reader" {
				deps.readOwner = nil
			}
			cmd := build()
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			launch, err := newOwnedSupervisorLaunch(cmd, deps, nil)
			if launch != nil {
				t.Cleanup(func() {
					if !launch.handedOff {
						if err := abortStartedSupervisor(launch); err != nil {
							t.Errorf("creator cleanup: %v", err)
						}
					}
				})
			} else {
				// Constructor failure to retain custody must not leak this fixture's
				// actually started original. This branch has no competing Wait owner.
				t.Cleanup(func() {
					if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
						t.Error(err)
					}
					if err := cmd.Wait(); err != nil {
						if _, expectedExit := err.(*exec.ExitError); !expectedExit {
							t.Error(err)
						}
					}
				})
				t.Fatal("constructor lost actual started creator")
			}
			if scenario == "missing-captured-reader" && err == nil {
				t.Fatal("post-creation dependency omission accepted")
			}
			if scenario == "complete-dependencies" && err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if _, err := launch.BindControl(ctx, supervisorExpectation(stateDir, exe, strings.Repeat("a", 64))); err == nil {
				t.Fatal("pre-owner child acquired control")
			}
			// A foreign replacement sidecar is diagnostic input, never termination authority.
			foreign := api.SupervisorLockOwner{PID: os.Getpid(), StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
			if err := api.WriteStateFileAtomic(filepath.Join(stateDir, "supervisor.lock.owner.json"), foreign); err != nil {
				t.Fatal(err)
			}
			var callers sync.WaitGroup
			results := make(chan error, 3)
			for i := 0; i < 3; i++ {
				callers.Add(1)
				go func() { defer callers.Done(); results <- abortStartedSupervisor(launch) }()
			}
			callers.Wait()
			close(results)
			for err := range results {
				if err != nil {
					t.Fatal(err)
				}
			}
			if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
				t.Fatalf("original creator was not reaped: %v", cmd.ProcessState)
			}
			owner, err := api.ReadSupervisorLockOwner(filepath.Join(stateDir, "supervisor.lock"))
			if err != nil || owner != foreign {
				t.Fatalf("foreign sidecar changed: %+v %v", owner, err)
			}
			if err := abortStartedSupervisor(launch); err != nil {
				t.Fatalf("repeated Abort: %v", err)
			}
			if err := launch.Handoff(context.Background(), SupervisorControlBinding{}); err == nil {
				t.Fatal("settled/zero handoff succeeded")
			}
		})
	}
}

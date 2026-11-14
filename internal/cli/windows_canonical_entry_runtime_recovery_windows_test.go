//go:build windows

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/process"
)

func TestCanonicalRuntimeRecoveryNonUpgradeSettlesWithoutPriorRuntime(t *testing.T) {
	for _, mode := range []WindowsProductPairMode{WindowsProductPairModeSetup, WindowsProductPairModeCanonicalize} {
		t.Run(string(mode), func(t *testing.T) {
			req, journal, _ := canonicalPendingValidationFixture(t, WindowsProductRecoveryPrepared)
			journal.Mode = mode
			path := filepath.Join(req.StateDir, WindowsProductRecoveryFileLeafV3)
			writeCanonicalPendingValidationJournal(t, req.StateDir, journal)
			runtimeRequest, err := windowsCanonicalEntryRecoveryRuntimeRequest(req.Context, req.StateDir, req.CanonicalEntryPath, req.LegacyWindowlessPath)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			req.RecoverPriorSettlementOriginal = func(ctx context.Context, j windowsProductRecoveryJournalV3, original *binaryadmission.WindowsCanonicalEntryPriorRuntimeV1) error {
				calls++
				if j.EntrySet.PriorRuntime != nil || original != nil {
					t.Fatal("non-upgrade fixture unexpectedly acquired a prior runtime")
				}
				return runtimeRequest.RecoverPriorSettlementOriginal(ctx, j, original)
			}
			req.Deps.DiscardTaskStore = func(WindowsProductPairTaskRecovery) error { return nil }
			tasks := &recoveryTaskTxnFake{}
			settled, err := rollbackCanonicalEntryRecovery(req, withWindowsProductRecoveryDefaults(req.Deps), path, journal, tasks)
			if err != nil || settled == nil || settled.Outcome != WindowsProductPairRecoverySettledRollback || calls != 1 || tasks.restoreCalls != 1 {
				t.Fatalf("non-upgrade rollback did not settle: result=%+v calls=%d tasks=%+v error=%v", settled, calls, tasks, err)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("settled non-upgrade journal remains: %v", err)
			}
		})
	}
}

// The runtime image and recovery owner are real native fixtures. Only process
// creation/readiness is substituted, so no installed service can be launched.
func canonicalPriorRuntimeRecoveryFixture(t *testing.T, live bool) (windowsProductRecoveryJournalV3, *binaryadmission.WindowsCanonicalEntryPriorRuntimeV1) {
	t.Helper()
	req, journal, _ := canonicalPendingValidationFixture(t, WindowsProductRecoveryPrepared)
	path := journal.EntrySet.CanonicalEntry.Target
	writeUpgradeGUIAdmittedProduct(t, path, "ORIGINAL-RECOVERY-RUNTIME")
	body := mustReadFile(t, path)
	journal.EntrySet.CanonicalEntry.PriorPresent = true
	journal.EntrySet.CanonicalEntry.PriorSHA256 = recoveryTestSHA(body)
	journal.EntrySet.PriorRuntime = &windowsCanonicalEntryRuntimePreimageV1{Path: path, Present: true, SHA256: recoveryTestSHA(body)}
	journal.Mode = WindowsProductPairModeUpgrade
	journal.FrozenPorts = []api.SupervisorFrozenPort{}
	journal.PreSettlement = &windowsProductPreSettlementV3{
		Supervisor: windowsProductSupervisorSettlementV1{State: windowsProductSupervisorKnownAbsent},
		GUIOwner:   windowsProductGUIOwnerConfirmedAbsent, SupervisorLockReleased: true,
		Ports: []windowsProductPortSettlementV1{},
	}
	if live {
		journal.PreSettlement.SupervisorLockReleased = false
		journal.PreSettlement.Supervisor = windowsProductSupervisorSettlementV1{State: windowsProductSupervisorGracefullySettled,
			Generation: &windowsProductSupervisorGenerationV1{PID: 4123, StartedAt: "2026-09-29T00:00:00Z"}}
		quiescence, digest, err := projectWindowsProductRecoveryQuiescence(windowsProductQuiescenceReceiptV1{
			ReceiptID: "original-runtime-stopped", Scope: windowsProductQuiescenceScopeV1,
			ObservationSHA256: strings.Repeat("a", 64),
			CanonicalCLIPath:  path, CanonicalWindowlessPath: journal.LegacyWindowless.Target,
			PriorCLI:   windowsProductPreimageV1{Present: true, SHA256: journal.EntrySet.PriorRuntime.SHA256},
			Supervisor: journal.PreSettlement.Supervisor, Ports: []windowsProductPortSettlementV1{},
			GUIOwner: windowsProductGUIOwnerConfirmedAbsent, SupervisorLockReleased: true,
			SettledAtUTC: time.Date(2026, 9, 29, 0, 0, 1, 0, time.UTC),
		})
		if err != nil {
			t.Fatal(err)
		}
		journal.Quiescence, journal.QuiescenceReceiptSHA256, journal.PriorFleetReaped = &quiescence, digest, true
	}
	writeCanonicalPendingValidationJournal(t, req.StateDir, journal)
	source, err := prepareCanonicalRecoverySource(req, withWindowsProductRecoveryDefaults(req.Deps), filepath.Join(req.StateDir, WindowsProductRecoveryFileLeafV3), journal, canonicalRecoveryRollback)
	if err != nil {
		t.Fatal(err)
	}
	var owner *binaryadmission.WindowsCanonicalEntryRecoverySetV1
	t.Cleanup(func() {
		if owner != nil {
			if err := owner.Close(); err != nil {
				t.Error(err)
			}
		}
		if source.directory != nil {
			if err := source.directory.Close(); err != nil {
				t.Error(err)
			}
		}
		source.active = false
	})
	if err := source.acquireDirectory(req.Context); err != nil {
		t.Fatal(err)
	}
	owner, err = binaryadmission.AcquireWindowsCanonicalEntryForRecoveryV1(req.Context, source, binaryadmission.WindowsCanonicalRecoveryPriorV1)
	if err != nil {
		t.Fatal(err)
	}
	original, err := owner.RuntimeProduct(req.Context)
	if err != nil || original == nil {
		t.Fatalf("original prior runtime unavailable: %v", err)
	}
	return journal, original
}

func TestCanonicalRuntimeRecoveryRestartsStoppedPriorAndReceivesReadiness(t *testing.T) {
	for _, test := range []struct {
		name      string
		failReady bool
	}{{"ready", false}, {"readiness-failure", true}} {
		t.Run(test.name, func(t *testing.T) {
			journal, original := canonicalPriorRuntimeRecoveryFixture(t, true)
			stateDir := apitest.HardenedTempDir(t)
			var order []string
			launch := &fixtureSupervisorLaunch{order: &order}
			readinessFailure := errors.New("prior readiness unconfirmed")
			err := recoverWindowsCanonicalEntryPriorRuntimeWithLaunch(context.Background(), stateDir, journal, original,
				func(path string) (SupervisorLaunch, error) {
					if path != journal.EntrySet.PriorRuntime.Path {
						t.Fatal("restart selected a different runtime")
					}
					order = append(order, "start")
					return launch, nil
				},
				func(_ context.Context, path, digest string) error {
					if path != journal.EntrySet.PriorRuntime.Path || digest != journal.EntrySet.PriorRuntime.SHA256 {
						t.Fatal("readiness was not bound to the prior runtime")
					}
					order = append(order, "ready")
					if test.failReady {
						return readinessFailure
					}
					return nil
				})
			want := []string{"start", "bind-launch", "ready", "handoff-launch"}
			if test.failReady {
				want[len(want)-1] = "abort-launch"
				if !errors.Is(err, readinessFailure) || launch.handoffs != 0 {
					t.Fatalf("readiness failure was accepted: error=%v launch=%+v", err, launch)
				}
			} else if err != nil || launch.aborts != 0 {
				t.Fatalf("stopped prior did not recover: error=%v launch=%+v", err, launch)
			}
			if !reflect.DeepEqual(order, want) {
				t.Fatalf("runtime restoration order=%v want=%v", order, want)
			}
		})
	}
}

func TestCanonicalRuntimeRecoveryAbsenceOnlySettlesOriginallyAbsent(t *testing.T) {
	journal, _ := canonicalPriorRuntimeRecoveryFixture(t, false)
	stateDir := apitest.HardenedTempDir(t)
	if err := recoverWindowsCanonicalEntryPriorRuntimeWithLaunch(context.Background(), stateDir, journal, nil, nil, nil); err != nil {
		t.Fatalf("unchanged prior absence did not settle: %v", err)
	}
	for _, guiOwner := range []string{windowsProductGUIOwnerConfirmedAbsent, windowsProductGUIOwnerConfirmedPresent} {
		journal, _ := canonicalPriorRuntimeRecoveryFixture(t, true)
		journal.PreSettlement.GUIOwner = guiOwner
		if err := recoverWindowsCanonicalEntryPriorRuntime(context.Background(), stateDir, journal, nil); !errors.Is(err, ErrWindowsProductRecoveryPending) {
			t.Fatalf("previously live runtime accepted without original owner: gui=%s error=%v", guiOwner, err)
		}
	}
}

func TestCanonicalRuntimeRecoveryCancellationPreventsStart(t *testing.T) {
	journal, original := canonicalPriorRuntimeRecoveryFixture(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := recoverWindowsCanonicalEntryPriorRuntimeWithLaunch(ctx, apitest.HardenedTempDir(t), journal, original,
		func(string) (SupervisorLaunch, error) {
			t.Fatal("cancelled recovery started a supervisor")
			return nil, nil
		},
		func(context.Context, string, string) error {
			t.Fatal("cancelled recovery waited for readiness")
			return nil
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestCanonicalRuntimeRecoveryStillRequiresUpgradeProjection(t *testing.T) {
	journal := windowsProductRecoveryJournalV3{
		Mode: WindowsProductPairModeUpgrade, EntrySet: &windowsCanonicalEntrySetJournalV1{},
		PreSettlement: &windowsProductPreSettlementV3{},
	}
	err := recoverWindowsCanonicalEntryPriorRuntimeWithLaunch(context.Background(), t.TempDir(), journal, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "complete-set recovery has no bound original prior runtime") {
		t.Fatalf("Upgrade without its runtime projection was accepted: %v", err)
	}
}

// This is a real native pipe/held-generation check for the read-only replay
// operation. It does not launch a Product or issue any service mutation.
func TestCanonicalRuntimeRecoveryReplayReadinessBindsNativePeer(t *testing.T) {
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("native recovery test requires a deadline")
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if os.Getenv(supervisorPeerSenderChildEnv) != "" {
		t.Fatal("unexpected sender helper sentinel")
	}
	for _, foreign := range []bool{false, true} {
		name := "original-native-peer"
		if foreign {
			name = "foreign-native-peer-with-copied-hello"
		}
		t.Run(name, func(t *testing.T) {
			peer, err := api.ObserveSupervisorProcessIdentityV1(os.Getpid())
			if err != nil {
				t.Fatal(err)
			}
			if foreign {
				_, peer = startSupervisorPeerSenderFixture(t, ctx, deadline)
				if peer.PID == os.Getpid() {
					t.Fatal("foreign-server fixture must have a distinct original process")
				}
			}
			owner := api.SupervisorLockOwner{PID: peer.PID, StartedAt: peer.CreationTime}
			held, err := process.HoldPIDForJobObservation(owner.PID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := held.Close(); err != nil {
					t.Error(err)
				}
			})
			requests := 0
			_, receive := supervisorPeerSenderPipe(t, ctx, func(conn net.Conn) error {
				helloErr := (&SupervisorIPCListener{pid: owner.PID, startedAt: owner.StartedAt}).WriteHello(conn)
				var request api.IPCRequest
				readErr := json.NewDecoder(conn).Decode(&request)
				if readErr != nil {
					if foreign && supervisorPeerSenderDisconnected(helloErr) && supervisorPeerSenderDisconnected(readErr) {
						return nil
					}
					return errors.Join(helloErr, readErr)
				}
				requests++
				if request.Cmd != "status" {
					return errors.New("read-only recovery sent a non-status request")
				}
				return errors.Join(helloErr, json.NewEncoder(conn).Encode(api.IPCResponse{
					ID: request.ID, OK: true, Final: true,
					Result: map[string]any{"state": "running", "daemons": []any{}, "reconcile_ready": true},
				}))
			})
			stateDir := os.Getenv("MCPHUB_STATE_DIR_OVERRIDE")
			if err := api.WriteStateFileAtomic(filepath.Join(stateDir, "supervisor.lock.owner.json"), owner); err != nil {
				t.Fatal(err)
			}
			err = waitCanonicalRecoveredSupervisorReady(ctx, stateDir, peer.ImagePath, owner, held)
			if foreign {
				if !errors.Is(err, api.ErrSupervisorControlPeerIdentity) {
					t.Fatalf("foreign readiness was not refused by native peer identity: %v", err)
				}
			} else if err != nil {
				t.Fatalf("original native readiness was refused: %v", err)
			}
			if receiveErr := receive(); receiveErr != nil {
				t.Fatal(receiveErr)
			}
			if foreign {
				if requests != 0 {
					t.Fatalf("foreign native peer received %d client requests", requests)
				}
			} else if requests != 1 {
				t.Fatalf("original native peer received %d client requests, want one", requests)
			}
			if err := held.VerifyIdentity(process.PIDIdentityProof{PID: owner.PID, ExecutablePath: peer.ImagePath, StartedAt: owner.StartedAt, StartTolerance: time.Nanosecond}); err != nil {
				t.Fatalf("read-only recovery disturbed the observed generation: %v", err)
			}
		})
	}
}

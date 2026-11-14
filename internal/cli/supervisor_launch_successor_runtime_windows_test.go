//go:build windows

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/binaryadmission"
)

// Unlike the command-order fixtures, this uses the actual production Product,
// native stage/prior FileObjects and supervisor creator/control/readiness. Only
// task storage is in-process; no GUI, task registration or daemon is started.
func TestCanonicalSuccessorNativeControlRetainsOriginalFiles(t *testing.T) {
	built := loadWindowsCanonicalEntryFixture(t, "MCPHUB_CANONICAL_ENTRY_FIXTURE_REQUIRED")
	requireCanonicalGUIReplayStateIsolation(t, built.Targets.ProductPath)
	for _, priorKind := range []string{"absent", "identical", "different-identity"} {
		t.Run(priorKind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			txn, deps := newNativeCanonicalSuccessorFixture(t, ctx, built, priorKind)
			assertHeld := func() {
				t.Helper()
				file, err := os.Open(txn.Opts.ProductPath)
				if file != nil {
					if closeErr := file.Close(); closeErr != nil {
						t.Error(closeErr)
					}
				}
				if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
					t.Fatalf("canonical Product lost its native original guard: %v", err)
				}
				if err := txn.verifyLiveCanonicalEntrySet(); err != nil {
					t.Fatal(err)
				}
			}
			assertHeld()
			launch, err := deps.startCanonicalEntrySuccessor(ctx, txn.Opts.ProductPath, txn.Opts.EntrySet)
			// Cleanup retains precisely the returned creator and runs before the
			// file owners close, including when the original control probe fails.
			retainCanonicalGUIReplayCleanup(t, launch)
			if err != nil || launch == nil {
				t.Fatalf("start actual promoted Product: %v", err)
			}
			expected := supervisorExpectation(txn.Opts.StateDir, txn.Opts.ProductPath, built.Input.ProductSHA256)
			bindCtx, bindCancel := context.WithTimeout(ctx, 10*time.Second)
			binding, err := launch.BindControl(bindCtx, expected)
			bindCancel()
			if err != nil || binding.proof == nil {
				t.Fatalf("native successor control while original files remain held: %v", err)
			}
			if err := deps.WaitSupervisorReady(ctx, 15*time.Second, txn.Opts.ProductPath, UpgradeCandidateV1{SHA256: built.Input.ProductSHA256}); err != nil {
				t.Fatalf("native successor readiness while original files remain held: %v", err)
			}
			wrong := strings.Repeat("0", 64)
			if wrong == built.Input.ProductSHA256 {
				wrong = strings.Repeat("1", 64)
			}
			if ready, err := deps.probeUpgradeReadyOnce(ctx, txn.Opts.ProductPath, UpgradeCandidateV1{SHA256: wrong}); ready || err == nil || !strings.Contains(err.Error(), "path/SHA-256 mismatch") {
				t.Fatalf("held successor accepted a different requested Product: ready=%v err=%v", ready, err)
			}
			assertHeld()
			if err := abortStartedSupervisor(launch); err != nil {
				t.Fatalf("settle exact test-created successor: %v", err)
			}
			if err := txn.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := deps.validateUpgradeSupervisorIdentity(ctx, txn.Opts.ProductPath, UpgradeCandidateV1{SHA256: built.Input.ProductSHA256}, binding.proof.owner); err == nil || !strings.Contains(err.Error(), "closed") {
				t.Fatalf("closed successor originals retained runtime authority: %v", err)
			}
		})
	}
}

func newNativeCanonicalSuccessorFixture(t *testing.T, ctx context.Context, built *windowsCanonicalEntryFixture, priorKind string) (*WindowsProductTxn, *v5UpgradeDeps) {
	t.Helper()
	root := apitest.HardenedTempDir(t)
	neutralizeClientConfigPathEnv(t, apitest.HardenedDir(t, filepath.Join(root, "home")))
	for _, assignment := range childStateEnv(root) {
		key, value, _ := strings.Cut(assignment, "=")
		t.Setenv(key, value)
	}
	t.Setenv("MCPHUB_E2E_SCHEDULER", "none")
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", apitest.HardenedDir(t, filepath.Join(root, "manifests")))
	stateDir := apitest.HardenedDir(t, childStateDirOverrideLeaf(root))
	apitest.HardenedDir(t, filepath.Join(root, "mcp-local-hub"))
	apitest.HardenedDir(t, filepath.Dir(api.SettingsPath()))
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))
	api.EnableSupervisorIPCTestPipeIsolation()
	if !strings.Contains(api.SupervisorIPCAddress(stateDir), "-test-") {
		t.Fatal("native successor fixture requires an isolated supervisor pipe")
	}
	targets, err := binaryadmission.CanonicalEntryTargetsV1(filepath.Join(apitest.HardenedDir(t, filepath.Join(root, "product")), "mcphub.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if priorKind != "absent" {
		apitest.HardenedDir(t, filepath.Dir(targets.ProductPath))
		for _, pair := range [][2]string{{built.Targets.CanonicalEntryPath, targets.CanonicalEntryPath}, {built.Targets.ProductPath, targets.ProductPath}, {built.Targets.IdentityPath, targets.IdentityPath}} {
			body, err := os.ReadFile(pair[0])
			if err != nil {
				t.Fatal(err)
			}
			if pair[1] == targets.IdentityPath && priorKind == "different-identity" {
				var value any
				if err := json.Unmarshal(body, &value); err != nil {
					t.Fatal(err)
				}
				body, err = json.MarshalIndent(value, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(pair[1], body, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	stage, err := built.Original.Stage(ctx, targets.CanonicalEntryPath)
	if stage != nil {
		t.Cleanup(func() {
			if err := stage.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil || stage == nil {
		t.Fatalf("stage native successor fixture: %v", err)
	}
	paths, err := stage.Targets()
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	txn := &WindowsProductTxn{Opts: WindowsProductTxnOpts{
		Context: ctx, StateDir: stateDir, Mode: WindowsProductPairModeUpgrade,
		EntrySet:           &stagedWindowsCanonicalEntrySetV1{original: stage},
		CanonicalEntryPath: targets.CanonicalEntryPath, ProductPath: targets.ProductPath, IdentityPath: targets.IdentityPath,
		StagedProduct: paths.ProductPath, LegacyWindowlessPath: filepath.Join(filepath.Dir(targets.CanonicalEntryPath), "mcphub-windowless.exe"),
		Tasks: &productPairTaskFake{order: &order}, FrozenPorts: []windowsProductIntentPortV1{},
		PreSettlement: &windowsProductQuiescenceObservationV1{GUIOwner: windowsProductGUIOwnerConfirmedAbsent,
			Supervisor: windowsProductSupervisorSettlementV1{State: windowsProductSupervisorKnownAbsent}, Ports: []windowsProductPortSettlementV1{}, SupervisorLockReleased: true},
		Deps: WindowsProductTxnDeps{PublishCommitted: windowsProductEventPublisher(stateDir)},
	}}
	t.Cleanup(func() {
		if err := txn.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := txn.Prepare(); err != nil {
		t.Fatalf("prepare original successor transaction: %v", err)
	}
	if priorKind == "absent" {
		if _, err := txn.promoteProductOriginal(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := txn.promoteCanonicalEntryCompanions(ctx); err != nil {
		t.Fatal(err)
	}
	reservation, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reservation.Addr().(*net.TCPAddr).Port
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	daemon := api.BuildBuiltinRouteDaemon(targets.ProductPath, port)
	if err := api.WriteStateFileAtomic(api.SettingsPath(), map[string]string{
		api.GUIRunDesiredKey: api.GUIRunRunning, api.MCPFrontPortSettingKey: strconv.Itoa(port),
		api.MCPFrontRoutingTargetSettingKey: string(api.MCPFrontRoutingTargetGUI),
	}); err != nil {
		t.Fatal(err)
	}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), &api.SupervisorIntentFile{
		Version: 1, IntentGeneration: 1, OwnerMode: api.OwnerModeGUI, Daemons: []api.SupervisorDaemon{daemon},
		Stops: map[string]api.DaemonIntent{daemon.TaskName: {Desired: api.IntentDesiredStopped, Reason: api.IntentReasonUserStop, UpdatedAt: time.Now().UTC()}},
	}); err != nil {
		t.Fatal(err)
	}
	return txn, buildV5UpgradeDeps(targets.ProductPath, stateDir)
}

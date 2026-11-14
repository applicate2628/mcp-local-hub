//go:build windows

package cli

import (
	"context"
	"debug/buildinfo"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/gui"
)

// This regression receives the actual Product and native supervisor generation.
// Only the GUI boundary is headless: it proves continuation to GUI start, not a
// running GUI, its health endpoint, or Windows Task Scheduler integration.
func TestCanonicalGUIRuntimeRecoveryNativeIndependentReplay(t *testing.T) {
	fixture := loadWindowsCanonicalEntryFixture(t, "MCPHUB_CANONICAL_ENTRY_FIXTURE_REQUIRED")
	requireCanonicalGUIReplayStateIsolation(t, fixture.Targets.ProductPath)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	root := apitest.HardenedTempDir(t)
	home := apitest.HardenedDir(t, filepath.Join(root, "home"))
	neutralizeClientConfigPathEnv(t, home)
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
		t.Fatal("native replay requires an isolated supervisor pipe")
	}

	targets, err := binaryadmission.CanonicalEntryTargetsV1(filepath.Join(root, "product", "mcphub.exe"))
	if err != nil {
		t.Fatal(err)
	}
	apitest.HardenedDir(t, filepath.Dir(targets.CanonicalEntryPath))
	apitest.HardenedDir(t, filepath.Dir(targets.ProductPath))
	for _, pair := range [][2]string{
		{fixture.Targets.CanonicalEntryPath, targets.CanonicalEntryPath},
		{fixture.Targets.ProductPath, targets.ProductPath},
		{fixture.Targets.IdentityPath, targets.IdentityPath},
	} {
		body, err := os.ReadFile(pair[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pair[1], body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Staging acquires the existing complete set as a native original prior.
	// The borrowed runtime remains backed by those handles throughout replay.
	stage, err := fixture.Original.Stage(ctx, targets.CanonicalEntryPath)
	if stage != nil {
		t.Cleanup(func() {
			if err := stage.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil || stage == nil {
		t.Fatalf("stage native replay fixture: %v", err)
	}
	prior, err := stage.AcquireOriginalPriorSet(ctx)
	if prior != nil {
		t.Cleanup(func() {
			if err := prior.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil || prior == nil {
		t.Fatalf("acquire native replay prior: %v", err)
	}
	original, err := prior.RuntimeProduct(ctx)
	if err != nil || original == nil {
		t.Fatalf("receive native replay Product: %v", err)
	}
	artifact := original.Artifact()
	if !samePath(artifact.Path, targets.ProductPath) || artifact.SHA256 != fixture.Input.ProductSHA256 {
		t.Fatal("native prior runtime differs from the qualified Product")
	}

	reservation, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reservation.Addr().(*net.TCPAddr).Port
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	// A real stopped descriptor makes production fleet observation meaningful
	// without starting a backend. The built-in startup seeder observes the same
	// Product/port and retains this durable stop before its first reconcile.
	daemon := api.BuildBuiltinRouteDaemon(targets.ProductPath, port)
	if err := api.WriteStateFileAtomic(api.SettingsPath(), map[string]string{
		api.GUIRunDesiredKey:                api.GUIRunRunning,
		api.MCPFrontPortSettingKey:          strconv.Itoa(port),
		api.MCPFrontRoutingTargetSettingKey: string(api.MCPFrontRoutingTargetGUI),
	}); err != nil {
		t.Fatal(err)
	}
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	if err := api.WriteSupervisorIntent(intentPath, &api.SupervisorIntentFile{
		Version: 1, IntentGeneration: 1, OwnerMode: api.OwnerModeGUI,
		Daemons: []api.SupervisorDaemon{daemon},
		Stops: map[string]api.DaemonIntent{daemon.TaskName: {
			Desired: api.IntentDesiredStopped, Reason: api.IntentReasonUserStop, UpdatedAt: time.Now().UTC(),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	deps := buildV5UpgradeDeps(targets.ProductPath, stateDir)
	deps.originalRuntime, deps.originalContext = original, ctx
	expected := supervisorExpectation(stateDir, targets.ProductPath, artifact.SHA256)
	start := func() (*ownedSupervisorLaunch, SupervisorControlBinding) {
		t.Helper()
		launch, err := deps.StartSupervisor(targets.ProductPath)
		retainCanonicalGUIReplayCleanup(t, launch)
		if err != nil || launch == nil {
			t.Fatalf("start native replay supervisor: %v", err)
		}
		owned, ok := launch.(*ownedSupervisorLaunch)
		if !ok {
			t.Fatalf("native creator capability has unexpected type %T", launch)
		}
		binding, err := launch.BindControl(ctx, expected)
		if err != nil || binding.proof == nil {
			t.Fatalf("bind native replay supervisor: %v", err)
		}
		if err := deps.WaitSupervisorReady(ctx, defaultSupervisorLockReleaseTimeout, targets.ProductPath, UpgradeCandidateV1{SHA256: artifact.SHA256}); err != nil {
			t.Fatalf("native replay supervisor readiness: %v", err)
		}
		return owned, binding
	}
	// Record an actual earlier generation, settle it through its creator, then
	// retain a separately created independent generation solely for test cleanup.
	first, firstBinding := start()
	firstOwner := firstBinding.proof.owner
	if err := abortStartedSupervisor(first); err != nil {
		t.Fatalf("settle earlier test-owned generation: %v", err)
	}
	creator, binding := start()
	owner := binding.proof.owner
	if owner == firstOwner {
		t.Fatal("native replay fixture did not create a distinct generation")
	}
	t.Run("original-control-rejects-different-hash", func(t *testing.T) {
		wrong := expected
		wrong.ProductSHA256 = strings.Repeat("0", 64)
		if wrong.ProductSHA256 == artifact.SHA256 {
			wrong.ProductSHA256 = strings.Repeat("1", 64)
		}
		creator.mu.Lock()
		_, err := creator.observeControl(ctx, wrong)
		creator.mu.Unlock()
		if err == nil || !strings.Contains(err.Error(), "original supervisor runtime path/SHA-256 mismatch") {
			t.Fatalf("creator accepted a different Product digest: %v", err)
		}
		ready, err := deps.probeUpgradeReadyOnce(ctx, targets.ProductPath, UpgradeCandidateV1{SHA256: wrong.ProductSHA256})
		if ready || err == nil || !strings.Contains(err.Error(), "original supervisor runtime path/SHA-256 mismatch") {
			t.Fatalf("prior runtime confirmed a successor digest at the same path: ready=%t err=%v", ready, err)
		}
	})
	t.Run("original-control-rejects-foreign-path-and-generation", func(t *testing.T) {
		if _, err := observeOriginalSupervisorRuntime(ctx, filepath.Join(root, "foreign", "mcphub.exe"), artifact.SHA256, owner, original); err == nil || !strings.Contains(err.Error(), "path/SHA-256 mismatch") {
			t.Fatalf("original runtime accepted a foreign Product path: %v", err)
		}
		changed := owner
		started, err := time.Parse(time.RFC3339Nano, owner.StartedAt)
		if err != nil {
			t.Fatal(err)
		}
		changed.StartedAt = started.Add(time.Second).Format(time.RFC3339Nano)
		if _, err := observeOriginalSupervisorRuntime(ctx, targets.ProductPath, artifact.SHA256, changed, original); err == nil || !strings.Contains(err.Error(), "verify original supervisor generation") {
			t.Fatalf("original runtime accepted a different process generation: %v", err)
		}
	})
	journal := windowsProductRecoveryJournalV3{
		Mode:    WindowsProductPairModeUpgrade,
		Product: windowsProductRecoveryArtifactV3{Target: targets.ProductPath, PriorPresent: true, PriorSHA256: artifact.SHA256},
		EntrySet: &windowsCanonicalEntrySetJournalV1{
			CanonicalEntry: windowsProductRecoveryArtifactV3{Target: targets.CanonicalEntryPath, PriorPresent: true, PriorSHA256: fixture.Input.CanonicalEntrySHA256},
			Identity:       windowsProductRecoveryArtifactV3{Target: targets.IdentityPath, PriorPresent: true, PriorSHA256: fixture.Input.IdentitySHA256},
			PriorRuntime:   &windowsCanonicalEntryRuntimePreimageV1{Path: targets.ProductPath, Present: true, SHA256: artifact.SHA256},
		},
		LegacyWindowless: windowsProductRecoveryLegacyWindowlessV3{Target: filepath.Join(root, "product", "mcphub-windowless.exe")},
		Receipts: windowsProductRecoveryReceiptsV3{
			V3: windowsProductPairRecoveryReceipt{Path: filepath.Join(stateDir, UpgradeReceiptSchemaV3+".json")},
			V2: windowsProductPairRecoveryReceipt{Path: filepath.Join(stateDir, UpgradeReceiptSchemaV2+".json")},
		},
		FrozenPorts: []api.SupervisorFrozenPort{{TaskName: daemon.TaskName, Port: port, ArgvSHA256: windowsProductArgvSHA256(append([]string{daemon.Command}, daemon.Args...))}},
		PreSettlement: &windowsProductPreSettlementV3{
			GUIOwner: windowsProductGUIOwnerConfirmedPresent,
			Supervisor: windowsProductSupervisorSettlementV1{State: windowsProductSupervisorGracefullySettled,
				Generation: &windowsProductSupervisorGenerationV1{PID: firstOwner.PID, StartedAt: firstOwner.StartedAt}},
			Ports: []windowsProductPortSettlementV1{{TaskName: daemon.TaskName, Port: port, PreState: windowsProductPortUnbound}},
		},
	}
	recorded := *journal.PreSettlement.Supervisor.Generation
	guiPath := filepath.Join(apitest.HardenedDir(t, filepath.Join(root, "headless-gui")), "gui.pidport")
	for _, failGUIStart := range []bool{true, false} {
		name := "continues-to-headless-start"
		if failGUIStart {
			name = "failed-headless-start-preserves-generation"
		}
		t.Run(name, func(t *testing.T) {
			ops, err := buildGUIOwnedPriorRecoveryOps(ctx, stateDir, targets.ProductPath, journal, nil, nil, original)
			if err != nil {
				t.Fatal(err)
			}
			var readbacks, classifications, fleetProofs, reservations, guiStarts, forbidden int
			readback, classify, proveFleet := ops.Readback, ops.SupervisorState, ops.ProveIndependent
			ops.Readback = func(j windowsProductRecoveryJournalV3) error { readbacks++; return readback(j) }
			ops.SupervisorState = func(ctx context.Context, j windowsProductRecoveryJournalV3) (guiOwnedPriorSupervisorState, error) {
				classifications++
				state, err := classify(ctx, j)
				if err == nil && state != guiOwnedPriorSupervisorRecoveredIndependent {
					t.Fatalf("native classifier returned %q, want recovered-independent", state)
				}
				return state, err
			}
			ops.ProveIndependent = func(ctx context.Context, j windowsProductRecoveryJournalV3) error {
				fleetProofs++
				return proveFleet(ctx, j)
			}
			settle, absent, startSupervisor, waitReady := ops.SettleSame, ops.ProveAbsent, ops.StartSupervisor, ops.WaitSupervisorReady
			ops.SettleSame = func(ctx context.Context, j windowsProductRecoveryJournalV3) error { forbidden++; return settle(ctx, j) }
			ops.ProveAbsent = func(ctx context.Context, j windowsProductRecoveryJournalV3) error { forbidden++; return absent(ctx, j) }
			ops.StartSupervisor = func() (SupervisorLaunch, error) {
				forbidden++
				launch, err := startSupervisor()
				retainCanonicalGUIReplayCleanup(t, launch)
				return launch, err
			}
			ops.WaitSupervisorReady = func(ctx context.Context) error { forbidden++; return waitReady(ctx) }
			// These three GUI operations alone are replaced. Native readback,
			// generation, same-user, image, Job, readiness, peer and fleet checks
			// above delegate unchanged to the production composition.
			ops.ObserveHealthy = func(context.Context, windowsProductRecoveryJournalV3) error {
				if guiStarts == 0 {
					return errors.New("headless fixture has no GUI yet")
				}
				return nil
			}
			ops.Reservation = func(context.Context) (gui.OwnedSingleInstanceLease, error) {
				reservations++
				lease, err := gui.AcquireSingleInstanceLockOnlyAt(guiPath)
				if lease != nil {
					t.Cleanup(func() {
						if err := lease.ReleaseErr(); err != nil {
							t.Error(err)
						}
					})
				}
				return lease, err
			}
			startFailure := errors.New("headless GUI start failed")
			ops.StartGUI = func() error {
				guiStarts++
				lease, err := gui.AcquireSingleInstanceLockOnlyAt(guiPath)
				if err != nil {
					return err
				}
				t.Cleanup(func() {
					if err := lease.ReleaseErr(); err != nil {
						t.Error(err)
					}
				})
				if err := lease.ReleaseErr(); err != nil {
					return err
				}
				if failGUIStart {
					return startFailure
				}
				return nil
			}
			err = recoverGUIOwnedPriorSettlementWithOps(ctx, journal, ops)
			wantReadbacks := 2
			if failGUIStart {
				wantReadbacks = 1
				var diagnostic *upgradeGUIPriorSettlementError
				if !errors.Is(err, startFailure) || !errors.As(err, &diagnostic) || diagnostic.operation != upgradeGUIPriorGUIStart {
					t.Fatalf("native replay failed before the headless GUI boundary: %v", err)
				}
			} else if err != nil {
				t.Fatalf("native replay did not continue to headless GUI start: %v", err)
			}
			if readbacks != wantReadbacks || classifications != 1 || fleetProofs != 1 || reservations != 1 || guiStarts != 1 || forbidden != 0 {
				t.Fatalf("replay effects differ: readbacks=%d classify=%d fleet=%d reserve=%d GUI=%d forbidden=%d", readbacks, classifications, fleetProofs, reservations, guiStarts, forbidden)
			}
			current, err := api.ReadSupervisorLockOwner(filepath.Join(stateDir, "supervisor.lock"))
			if err != nil || current != owner || *journal.PreSettlement.Supervisor.Generation != recorded {
				t.Fatalf("native replay changed the observed or journal generation: %v", err)
			}
			creator.mu.Lock()
			custodyChanged := creator.handedOff || creator.settling || creator.settled || creator.proof != binding.proof
			creator.mu.Unlock()
			if custodyChanged {
				t.Fatal("replay aborted or handed off an observation-only supervisor")
			}
			ready, err := api.ReadSupervisorControlReadinessForPeerFromStateDir(ctx, stateDir, owner, binding.proof.peer)
			if err != nil || !ready.ReconcileReadyPresent || !ready.ReconcileReadyValid || !ready.ReconcileReady {
				t.Fatalf("same native peer was not ready after replay: %v", err)
			}
			intent, err := api.ObserveSupervisorRuntimeIntent(ctx, intentPath, time.Now())
			if err != nil || intent == nil || len(intent.Ports) != 1 || !intent.Stopped[daemon.TaskName] {
				t.Fatalf("replay changed the stopped fixture fleet: %v", err)
			}
		})
	}
	t.Run("cleanup-uses-current-operation-context", func(t *testing.T) {
		cancel()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cleanupCancel()
		if err := creator.Abort(cleanupCtx); err != nil {
			t.Fatalf("original creator cleanup reused the cancelled launch context: %v", err)
		}
	})
	// Release the native prior only after its test-owned process is settled.
	// Its historical Artifact data must never remain sufficient for readiness.
	if err := abortStartedSupervisor(creator); err != nil {
		t.Fatalf("settle native creator before closing original admission: %v", err)
	}
	if err := prior.Close(); err != nil {
		t.Fatal(err)
	}
	t.Run("closed-original-refuses-runtime-proof", func(t *testing.T) {
		proofCtx, proofCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer proofCancel()
		_, err := deps.validateUpgradeSupervisorIdentity(proofCtx, targets.ProductPath, UpgradeCandidateV1{SHA256: artifact.SHA256}, owner)
		if err == nil || !strings.Contains(err.Error(), "validate original supervisor runtime") || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("closed original retained runtime authority: %v", err)
		}
	})
}

func requireCanonicalGUIReplayStateIsolation(t *testing.T, productPath string) {
	t.Helper()
	if os.Getenv("MCPHUB_CANONICAL_FIXTURE_TEST_PRODUCT") != "1" {
		t.Fatal("native replay requires the explicitly qualified state-isolated test Product")
	}
	info, err := buildinfo.ReadFile(productPath)
	if err != nil {
		t.Fatalf("read Product build settings before native launch: %v", err)
	}
	for _, setting := range info.Settings {
		if setting.Key != "-tags" {
			continue
		}
		for _, tag := range strings.FieldsFunc(setting.Value, func(r rune) bool { return r == ',' || r == ' ' }) {
			if tag == "test_state_path_env" {
				return
			}
		}
	}
	t.Fatal("native replay Product was not built with test_state_path_env; refusing launch")
}

func retainCanonicalGUIReplayCleanup(t *testing.T, launch SupervisorLaunch) {
	t.Helper()
	if launch == nil {
		return
	}
	t.Cleanup(func() {
		if err := abortStartedSupervisor(launch); err != nil {
			t.Errorf("settle test-created native replay supervisor: %v", err)
		}
	})
}

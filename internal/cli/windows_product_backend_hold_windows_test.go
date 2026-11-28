//go:build windows

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
)

func windowsProductBackendHoldState(t *testing.T) (context.Context, string) {
	t.Helper()
	deadline, finite := t.Deadline()
	if !finite {
		t.Fatal("backend hold requires a bounded fixture context")
	}
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	t.Cleanup(cancel)
	return ctx, withTempStateDir(t)
}

func seedWindowsProductBackendHold(t *testing.T, stateDir string, pending bool) []byte {
	t.Helper()
	hold := api.SupervisorBackendRunHold{
		Version: 1, Installation: strings.Repeat("a", 64), StopID: strings.Repeat("b", 64),
		Absent: true, Settled: true,
	}
	if pending {
		hold.Absent, hold.Settled = false, false
		hold.Original = api.SupervisorLockOwner{PID: 31, StartedAt: "2026-11-20T00:00:00Z"}
		hold.Work = []api.SupervisorBackendStopWork{{Operation: 1, Task: "fixture", Outcome: api.SupervisorBackendStopPending, PendingPhase: "fixture-settlement"}}
	}
	raw, err := json.Marshal(api.SupervisorIntentFile{Version: 1, OwnerMode: api.OwnerModeSupervise, BackendRunHold: hold})
	if err != nil {
		t.Fatal(err)
	}
	if err := api.WriteStateFileBytesAtomic(filepath.Join(stateDir, "supervisor-intent.json"), raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertWindowsProductBackendHoldBytes(t *testing.T, path string, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, expected) {
		t.Fatalf("protected preimage changed at %s: %v", filepath.Base(path), err)
	}
}

func TestWindowsProductBackendHoldRefusesFreshAndPreviouslyPreparedEffects(t *testing.T) {
	for _, name := range []string{"fresh_pending", "fresh_settled"} {
		t.Run(name, func(t *testing.T) {
			txn, paths, order := newProductTxnFixture(t)
			ctx, stateDir := windowsProductBackendHoldState(t)
			txn.Opts.Context, txn.Opts.StateDir, paths.stateDir = ctx, stateDir, stateDir
			raw := seedWindowsProductBackendHold(t, stateDir, name == "fresh_pending")
			if err := txn.Prepare(); !errors.Is(err, api.ErrSupervisorBackendHeld) {
				t.Fatalf("fresh hold admission: %v", err)
			}
			assertWindowsProductBackendHoldFreshNoEffects(t, txn, paths, order)
			assertWindowsProductBackendHoldBytes(t, filepath.Join(stateDir, "supervisor-intent.json"), raw)
		})
	}
	t.Run("prepared", func(t *testing.T) {
		txn, paths, order := newProductTxnFixture(t)
		ctx, stateDir := windowsProductBackendHoldState(t)
		txn.Opts.Context, txn.Opts.StateDir, paths.stateDir = ctx, stateDir, stateDir
		if err := txn.Prepare(); err != nil {
			t.Fatal(err)
		}
		journalPath := filepath.Join(paths.stateDir, WindowsProductRecoveryFileLeafV3)
		preimage, err := os.ReadFile(journalPath)
		if err != nil {
			t.Fatal(err)
		}
		baseline := strings.Join(*order, ",")
		raw := seedWindowsProductBackendHold(t, paths.stateDir, false)
		if result, err := txn.Run(ctx); !errors.Is(err, api.ErrSupervisorBackendHeld) || result.Outcome != "" {
			t.Fatalf("prepared hold admission: %+v, %v", result, err)
		}
		if strings.Join(*order, ",") != baseline {
			t.Fatalf("held transaction ran a forward or inverse effect: %v", *order)
		}
		assertWindowsProductBackendHoldBytes(t, journalPath, preimage)
		assertWindowsProductBackendHoldBytes(t, filepath.Join(paths.stateDir, "supervisor-intent.json"), raw)
	})
}

func TestWindowsProductBackendHoldRefusesBothV3RecoveryDirections(t *testing.T) {
	for _, phase := range []WindowsProductRecoveryPhase{
		WindowsProductRecoveryPrepared, WindowsProductRecoveryCommitDecided,
	} {
		t.Run(string(phase), func(t *testing.T) {
			ctx, stateDir := windowsProductBackendHoldState(t)
			fixture := newProductRecoveryV3Fixture(t, phase)
			fixture.stateDir = stateDir
			fixture.journal.Receipts.V3.Path = filepath.Join(stateDir, UpgradeReceiptSchemaV3+".json")
			fixture.journal.Receipts.V2.Path = filepath.Join(stateDir, UpgradeReceiptSchemaV2+".json")
			fixture.journal.Tasks.StoreRoot = filepath.Join(stateDir, "task-store")
			if err := persistWindowsProductRecoveryJournalV3(ctx, productJournalFixtureAllowance(),
				filepath.Join(stateDir, WindowsProductRecoveryFileLeafV3), fixture.journal,
				api.WriteStateFileBytesAtomic, api.ReadStateFileInodeAnchored); err != nil {
				t.Fatal(err)
			}
			tasks := &recoveryTaskTxnFake{digest: fixture.journal.TaskSetSHA256}
			req := fixture.request(tasks)
			req.Context = ctx
			req.Deps.ReopenTasks = func(context.Context, WindowsProductPairTaskRecovery) (WindowsProductTaskTxn, error) {
				t.Error("held recovery reached original Task reopen")
				return nil, errors.New("unexpected Task reopen")
			}
			req.Legacy = windowsProductPairRecoveryRequest{JournalAllowance: req.JournalAllowance, Context: req.Context, StateDir: req.StateDir}
			req.ObserveSuccessorReady = func(context.Context, string, string) (bool, error) {
				t.Fatal("held recovery reached running readiness")
				return false, nil
			}
			req.StartSupervisor = func(string) (SupervisorLaunch, error) {
				t.Fatal("held recovery attempted a successor")
				return nil, nil
			}
			journalPath := filepath.Join(fixture.stateDir, WindowsProductRecoveryFileLeafV3)
			preimage, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			raw := seedWindowsProductBackendHold(t, fixture.stateDir, false)
			if settled, err := reconcileWindowsProductRecovery(req); !errors.Is(err, api.ErrSupervisorBackendHeld) || settled != nil {
				t.Fatalf("held recovery produced a success: %+v, %v", settled, err)
			}
			assertWindowsProductBackendHoldBytes(t, journalPath, preimage)
			assertWindowsProductBackendHoldBytes(t, filepath.Join(fixture.stateDir, "supervisor-intent.json"), raw)
			assertWindowsProductBackendHoldBytes(t, fixture.product, fixture.newProduct)
			assertWindowsProductBackendHoldBytes(t, fixture.productAside, fixture.priorProduct)
			assertWindowsProductBackendHoldBytes(t, fixture.legacyAside, fixture.priorLegacy)
		})
	}
}

func assertWindowsProductBackendHoldFreshNoEffects(t *testing.T, txn *WindowsProductTxn, paths productTxnFixturePaths, order *[]string) {
	t.Helper()
	if len(*order) != 0 || txn.prepared != nil {
		t.Fatal("guard refusal entered Product or Task effects", *order)
	}
	for _, path := range []string{paths.product, filepath.Join(paths.stateDir, WindowsProductRecoveryFileLeafV3),
		filepath.Join(paths.stateDir, UpgradeReceiptSchemaV3+".json")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("guard created protected path %s: %v", filepath.Base(path), err)
		}
	}
	assertWindowsProductBackendHoldBytes(t, paths.staged, []byte("new-product"))
	assertWindowsProductBackendHoldBytes(t, paths.legacy, []byte("legacy-windowless"))
}

func TestWindowsProductBackendHoldRefusesMalformedIntent(t *testing.T) {
	for _, name := range []string{"null_hold", "omitted_settled", "null_settled"} {
		t.Run(name, func(t *testing.T) {
			txn, paths, order := newProductTxnFixture(t)
			ctx, stateDir := windowsProductBackendHoldState(t)
			txn.Opts.Context, txn.Opts.StateDir, paths.stateDir = ctx, stateDir, stateDir
			path := filepath.Join(stateDir, "supervisor-intent.json")
			raw := []byte("{\"version\":1,\"backend_run_hold\":null}")
			if name != "null_hold" {
				valid := seedWindowsProductBackendHold(t, stateDir, true)
				replacement := ""
				if name == "null_settled" {
					replacement = "\"settled\":null,"
				}
				raw = bytes.Replace(valid, []byte("\"settled\":false,"), []byte(replacement), 1)
				if bytes.Equal(raw, valid) {
					t.Fatal("malformed boolean fixture did not change")
				}
			}
			if err := api.WriteStateFileBytesAtomic(path, raw); err != nil {
				t.Fatal(err)
			}
			if err := txn.Prepare(); !errors.Is(err, api.ErrSupervisorBackendHoldInvalid) {
				t.Fatalf("malformed hold was treated as absent: %v", err)
			}
			assertWindowsProductBackendHoldFreshNoEffects(t, txn, paths, order)
			assertWindowsProductBackendHoldBytes(t, path, raw)
		})
	}
}

func TestWindowsProductBackendHoldRefusesIntentLeaseError(t *testing.T) {
	txn, paths, order := newProductTxnFixture(t)
	ctx, stateDir := windowsProductBackendHoldState(t)
	txn.Opts.Context, txn.Opts.StateDir, paths.stateDir = ctx, stateDir, stateDir
	if err := os.Mkdir(filepath.Join(stateDir, "supervisor-intent.json.lock"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := txn.Prepare(); err == nil || !strings.Contains(err.Error(), "inspect Product backend hold") {
		t.Fatalf("non-openable intent lease did not refuse at the Product guard: %v", err)
	}
	assertWindowsProductBackendHoldFreshNoEffects(t, txn, paths, order)
}

func TestWindowsProductBackendHoldRefusesLegacyRecoveryBeforeRestore(t *testing.T) {
	ctx, stateDir := windowsProductBackendHoldState(t)
	fixture := newRecoveryFixture(t)
	fixture.stateDir = stateDir
	fixture.receipt = filepath.Join(stateDir, UpgradeReceiptSchemaV2+".json")
	fixture.taskStore = filepath.Join(stateDir, "task-store")
	fixture.journal.Receipt.Path = fixture.receipt
	fixture.journal.Tasks.StoreRoot = fixture.taskStore
	fixture.persistJournal(t)
	journalPath := filepath.Join(fixture.stateDir, WindowsProductPairRecoveryFileLeaf)
	preimage, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	raw := seedWindowsProductBackendHold(t, fixture.stateDir, false)
	var order []string
	req := fixture.request(&recoveryTaskTxnFake{}, &order, nil)
	req.Context = ctx
	if settled, err := reconcileWindowsProductPairRecovery(req); !errors.Is(err, api.ErrSupervisorBackendHeld) || settled != nil {
		t.Fatalf("held legacy recovery result: %+v, %v", settled, err)
	}
	if len(order) != 0 {
		t.Fatalf("held legacy recovery entered an effect owner: %v", order)
	}
	assertWindowsProductBackendHoldBytes(t, journalPath, preimage)
	assertWindowsProductBackendHoldBytes(t, filepath.Join(fixture.stateDir, "supervisor-intent.json"), raw)
}

func TestWindowsProductBackendHoldAbsentRetainsNormalRunningReceipt(t *testing.T) {
	for _, name := range []string{"missing_intent", "legacy_intent"} {
		t.Run(name, func(t *testing.T) {
			txn, _, _ := newProductTxnFixture(t)
			ctx, stateDir := windowsProductBackendHoldState(t)
			txn.Opts.Context, txn.Opts.StateDir = ctx, stateDir
			if name == "legacy_intent" {
				path := filepath.Join(stateDir, "supervisor-intent.json")
				if err := api.WriteStateFileBytesAtomic(path, []byte("{\"version\":1}\n")); err != nil {
					t.Fatal(err)
				}
			}
			result, err := txn.Run(ctx)
			if err != nil || result.Outcome != WindowsProductPairCommittedOutcome {
				t.Fatalf("ordinary no-hold transaction: %+v, %v", result, err)
			}
		})
	}
}

func TestWindowsProductSetupAndCanonicalizeRequireNoBackendHoldBeforeStage(t *testing.T) {
	for _, mode := range []WindowsProductPairMode{WindowsProductPairModeSetup, WindowsProductPairModeCanonicalize} {
		t.Run(string(mode), func(t *testing.T) {
			ctx, stateDir := windowsProductBackendHoldState(t)
			raw := seedWindowsProductBackendHold(t, stateDir, false)
			binding, err := binaryadmission.NewWindowsCanonicalEntryBuildBindingV1(strings.Repeat("a", 32), strings.Repeat("b", 64), binaryadmission.WindowsCanonicalEntryReverseProjectionV1)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			target := filepath.Join(t.TempDir(), "mcphub.exe")
			if err := installWindowsCanonicalEntrySet(ctx, &out, target, mode, windowsCanonicalEntryCommandInput{binding: binding}); !errors.Is(err, api.ErrSupervisorBackendHeld) {
				t.Fatalf("expected hold rejection before original-image admission, got %v", err)
			}
			if out.Len() != 0 {
				t.Fatalf("held command reported success: %s", out.String())
			}
			if entries, err := os.ReadDir(filepath.Dir(target)); err != nil || len(entries) != 0 {
				t.Fatalf("held command created an entry-set staging path: %v, %v", entries, err)
			}
			assertWindowsProductBackendHoldBytes(t, filepath.Join(stateDir, "supervisor-intent.json"), raw)
		})
	}
}

func TestWindowsProductBackendHoldUpgradeRefusesBeforeStage(t *testing.T) {
	ctx, stateDir := windowsProductBackendHoldState(t)
	raw := seedWindowsProductBackendHold(t, stateDir, false)
	binding, err := binaryadmission.NewWindowsCanonicalEntryBuildBindingV1(strings.Repeat("a", 32), strings.Repeat("b", 64), binaryadmission.WindowsCanonicalEntryReverseProjectionV1)
	if err != nil {
		t.Fatal(err)
	}
	targetRoot := t.TempDir()
	target := filepath.Join(targetRoot, "mcphub.exe")
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err = runV5UpgradeWindowsWithPathsAndDepsAndInput(cmd, filepath.Join(targetRoot, "absent-fixture.exe"), target,
		upgradeGUIFencedDeps{}, windowsCanonicalEntryCommandInput{binding: binding})
	if !errors.Is(err, api.ErrSupervisorBackendHeld) || stdout.Len() != 0 {
		t.Fatalf("upgrade reached candidate admission, stage or no-op: %v", err)
	}
	if entries, err := os.ReadDir(targetRoot); err != nil || len(entries) != 0 {
		t.Fatalf("held upgrade touched its private Product target: %v, %v", entries, err)
	}
	assertWindowsProductBackendHoldBytes(t, filepath.Join(stateDir, "supervisor-intent.json"), raw)
}

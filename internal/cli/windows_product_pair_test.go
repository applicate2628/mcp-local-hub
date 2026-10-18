//go:build windows

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/scheduler"
)

var (
	productPairTestCLISHA        = strings.Repeat("c", sha256.Size*2)
	productPairTestWindowlessSHA = strings.Repeat("d", sha256.Size*2)
)

type productPairTaskFake struct {
	order        *[]string
	restored     bool
	closed       bool
	discarded    bool
	closeErr     error
	inventoryErr error
	restoreErr   error
}

func (f *productPairTaskFake) InventoryExport() error {
	*f.order = append(*f.order, "tasks-snapshot")
	return f.inventoryErr
}

func productPairTestGracefulLifecycle() *windowsProductPairLifecycleHooks {
	return &windowsProductPairLifecycleHooks{
		Observe: func(context.Context, string, []windowsProductIntentPortV1) (windowsProductQuiescenceObservationV1, error) {
			return windowsProductQuiescenceObservationV1{
				Supervisor: windowsProductSupervisorSettlementV1{State: windowsProductSupervisorGracefullySettled, Generation: &windowsProductSupervisorGenerationV1{PID: 71, StartedAt: "2026-09-01T00:00:00Z", IPCRequestVersion: 0}},
				GUIOwner:   windowsProductGUIOwnerConfirmedAbsent,
			}, nil
		},
		Now: func() time.Time { return time.Date(2026, 9, 1, 0, 0, 1, 0, time.UTC) },
	}
}
func (f *productPairTaskFake) RewriteCommand() error {
	*f.order = append(*f.order, "tasks-rewrite")
	return nil
}
func (f *productPairTaskFake) VerifyRuntime() error {
	*f.order = append(*f.order, "tasks-readback")
	return nil
}
func (f *productPairTaskFake) VerifyRuntimeSHA256() (string, error) {
	if err := f.VerifyRuntime(); err != nil {
		return "", err
	}
	return strings.Repeat("e", sha256.Size*2), nil
}
func (f *productPairTaskFake) RestoreImport() error {
	f.restored = true
	*f.order = append(*f.order, "tasks-restore")
	return f.restoreErr
}
func (f *productPairTaskFake) Close() error { f.closed = true; return f.closeErr }
func (f *productPairTaskFake) RecoveryDescriptor() (WindowsProductPairTaskRecovery, error) {
	return WindowsProductPairTaskRecovery{StoreRoot: filepath.Join(os.TempDir(), "fixture-task-store")}, nil
}
func (f *productPairTaskFake) DiscardRecoveryStore() error { f.discarded = true; return nil }

type productTaskCloseCounter struct {
	WindowsProductTaskTxn
	closes int
}

func (c *productTaskCloseCounter) Close() error {
	c.closes++
	return c.WindowsProductTaskTxn.Close()
}

func TestDecodeUpgradeReceiptReadsV1AndV2(t *testing.T) {
	v1, err := DecodeUpgradeReceipt([]byte(`{"schema":"upgrade-receipt-v1","admission":"a","version":"v","commit":"c","build_date":"d","sha256":"old","installed_at":"t"}`))
	if err != nil || v1.V1 == nil || v1.V2 != nil || v1.V1.SHA256 != "old" {
		t.Fatalf("v1=%+v err=%v", v1, err)
	}
	v2, err := DecodeUpgradeReceipt([]byte(`{"schema":"upgrade-receipt-v2","admission":"a","version":"v","commit":"c","build_date":"d","artifacts":{"cli":{"role":"cli","sha256":"c"},"windowless":{"role":"windowless","sha256":"w"}},"installed_at":"t"}`))
	if err != nil || v2.V2 == nil || v2.V1 != nil || v2.V2.Artifacts.Windowless.SHA256 != "w" {
		t.Fatalf("v2=%+v err=%v", v2, err)
	}
}

func TestDecodeUpgradeReceiptReadsV3ProductReceipt(t *testing.T) {
	raw := []byte(`{
		"schema":"upgrade-receipt-v3",
		"transaction_id":"0123456789abcdef0123456789abcdef",
		"mode":"upgrade",
		"admission":"local-product",
		"artifact":{
			"role":"product",
			"subsystem":2,
			"machine":34404,
			"version":"0.4.38",
			"commit":"0123456789abcdef0123456789abcdef01234567",
			"build_date":"2026-09-11T00:00:00Z",
			"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		},
		"task_set_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"retired":{
			"windowless_absent":true,
			"admission_helper_absent_from_payload":true,
			"rollback_images_absent":true
		},
		"installed_at":"2026-09-11T00:00:01Z"
	}`)

	if _, err := DecodeUpgradeReceipt(raw); err != nil {
		t.Fatalf("decode V3 product receipt: %v", err)
	}
}

func TestWindowsProductTxnCommitsV3AfterTaskReadbackAndLegacyRetirement(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	product := filepath.Join(dir, "mcphub.exe")
	legacy := filepath.Join(dir, "mcphub-windowless.exe")
	staged := filepath.Join(dir, "mcphub.exe.stage")
	writePairFixture(t, staged, []byte("new-product"))
	writePairFixture(t, legacy, []byte("legacy-windowless"))
	var order []string
	tasks := &productPairTaskFake{order: &order}
	deps := productTxnTestDeps(&order)
	txn := &WindowsProductTxn{Opts: WindowsProductTxnOpts{
		StateDir: stateDir, ProductPath: product, LegacyWindowlessPath: legacy,
		StagedProduct: staged, Mode: WindowsProductPairModeCanonicalize,
		ReplacementAuthority: windowsProductReplacementAuthority{
			Disposition: windowsProductPairDispositionV1{CLI: windowsProductTargetCreate, Windowless: windowsProductTargetIdentical},
		},
		Tasks: tasks, Deps: deps,
	}}
	committed, err := txn.Run(context.Background())
	if err != nil {
		t.Fatalf("run Product/V3 transaction: %v", err)
	}
	if committed.Outcome != WindowsProductPairCommittedOutcome || committed.Artifact.Role != binaryadmission.WindowsArtifactRoleProduct {
		t.Fatalf("committed=%+v", committed)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy target remains: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, UpgradeReceiptSchemaV3+".json"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeUpgradeReceipt(raw)
	if err != nil || decoded.V3 == nil || decoded.V3.Artifact.SHA256 != committed.Artifact.SHA256 {
		t.Fatalf("receipt=%+v err=%v", decoded, err)
	}
	if !recoveryStepsOrdered(order, []string{"tasks-snapshot", "promote-cli", "tasks-rewrite", "tasks-readback", "publish-product"}) {
		t.Fatalf("order=%v", order)
	}
}

func TestWindowsProductTxnPrePrepareStoreDisposition(t *testing.T) {
	stateDir := t.TempDir()
	req := windowsProductTxnRequest{
		Context:              context.Background(),
		StateDir:             stateDir,
		ProductPath:          filepath.Join(stateDir, "mcphub.exe"),
		LegacyWindowlessPath: filepath.Join(stateDir, "mcphub-windowless.exe"),
		StagedProduct:        writeAdmissionPEFixture(t, binaryadmission.WindowsCUISubsystem, false),
		Mode:                 WindowsProductPairModeCanonicalize,
	}
	txn, err := newWindowsProductTxn(req)
	if err != nil {
		t.Fatalf("begin task transaction: %v", err)
	}
	owner := txn.Opts.Tasks.(*windowsProductPairTaskOwner)
	counter := &productTaskCloseCounter{WindowsProductTaskTxn: owner}
	txn.Opts.Tasks = counter
	t.Cleanup(func() {
		_ = owner.Close()
		_ = owner.store.Close()
	})

	_, err = txn.Run(context.Background())
	var prePromotion *WindowsProductPairPrePromotionError
	if !errors.As(err, &prePromotion) || !strings.Contains(err.Error(), binaryadmission.WindowsPESubsystemErrorID) {
		t.Fatalf("Prepare error=%v, want typed PE admission refusal", err)
	}
	if counter.closes != 1 {
		t.Fatalf("task lock Close calls=%d, want exactly one in Run", counter.closes)
	}
	if _, err := os.Stat(owner.store.RootPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unjournaled Prepare failure retained task store: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	req.Context = ctx
	next, err := newWindowsProductTxn(req)
	if err != nil {
		t.Fatalf("reacquire task transaction after failed Prepare: %v", err)
	}
	if err := next.Close(); err != nil {
		t.Fatalf("close reacquired task transaction: %v", err)
	}
	if err := next.Opts.Tasks.(*windowsProductPairTaskOwner).store.Close(); err != nil {
		t.Fatalf("close reacquired task store: %v", err)
	}
}

func TestWindowsProductTxnClosePreservesStoreOnAmbiguousJournalPopulation(t *testing.T) {
	stateDir := t.TempDir()
	req := windowsProductTxnRequest{
		Context: context.Background(), StateDir: stateDir,
		ProductPath:          filepath.Join(stateDir, "mcphub.exe"),
		LegacyWindowlessPath: filepath.Join(stateDir, "mcphub-windowless.exe"),
		StagedProduct:        writeAdmissionPEFixture(t, binaryadmission.WindowsCUISubsystem, false),
		Mode:                 WindowsProductPairModeUpgrade,
	}
	txn, err := newWindowsProductTxn(req)
	if err != nil {
		t.Fatal(err)
	}
	store := txn.Opts.Tasks.(*windowsProductPairTaskOwner).store
	t.Cleanup(func() { _ = store.Close() })
	if err := os.WriteFile(filepath.Join(stateDir, WindowsProductRecoveryFileLeafV3), []byte("test-owned pending journal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(stateDir, WindowsProductPairRecoveryFileLeaf), 0o700); err != nil {
		t.Fatal(err)
	}
	err = txn.Close()
	if err == nil || !strings.Contains(err.Error(), "UPGRADE_GUI_TASK_STORE_CLEANUP_UNCONFIRMED") {
		t.Fatalf("ambiguous second journal Close error=%v, want typed uncertainty", err)
	}
	if _, statErr := os.Stat(store.RootPath()); statErr != nil {
		t.Fatalf("journal-bound store was discarded: %v", statErr)
	}
}

func TestWindowsProductTxnConstructorJoinsStoreCleanupFailure(t *testing.T) {
	t.Run("later task begin failure", func(t *testing.T) {
		stateDir := t.TempDir()
		beginErr := errors.New("test-owned task begin refusal")
		var storeRoot string
		_, err := newWindowsProductTxnWith(windowsProductTxnRequest{
			Context: context.Background(), StateDir: stateDir,
			ProductPath:          filepath.Join(stateDir, "mcphub.exe"),
			LegacyWindowlessPath: filepath.Join(stateDir, "mcphub-windowless.exe"),
			StagedProduct:        filepath.Join(stateDir, "candidate.exe"),
			Mode:                 WindowsProductPairModeUpgrade,
		}, func(root string) (*windowsProductPairTaskStore, error) {
			store, err := newWindowsProductPairTaskStore(root)
			if err != nil {
				return nil, err
			}
			storeRoot = store.RootPath()
			if err := os.WriteFile(filepath.Join(storeRoot, "test-owned-unrecognized-leaf"), []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}
			return store, nil
		}, func(context.Context, string, string, string, scheduler.RetainedTaskXMLStore) (*scheduler.OwnedEntrypointTaskTxn, error) {
			return nil, beginErr
		})
		if !errors.Is(err, beginErr) || !strings.Contains(err.Error(), "UPGRADE_GUI_TASK_STORE_CLEANUP_UNCONFIRMED") {
			t.Fatalf("constructor error=%v, want begin cause and cleanup failure", err)
		}
		if _, statErr := os.Stat(filepath.Join(storeRoot, "test-owned-unrecognized-leaf")); statErr != nil {
			t.Fatalf("failed cleanup erased unrecognized leaf: %v", statErr)
		}
	})

	t.Run("partial store acquisition error", func(t *testing.T) {
		stateDir := t.TempDir()
		acquisitionErr := errors.New("test-owned task store acquisition refusal")
		retainedXML := []byte("<Task><RegistrationInfo><URI>owned</URI></RegistrationInfo></Task>")
		var storeRoot, retainedRef string
		var partialStore *windowsProductPairTaskStore
		_, err := newWindowsProductTxnWith(windowsProductTxnRequest{
			Context: context.Background(), StateDir: stateDir,
			ProductPath:          filepath.Join(stateDir, "mcphub.exe"),
			LegacyWindowlessPath: filepath.Join(stateDir, "mcphub-windowless.exe"),
			StagedProduct:        filepath.Join(stateDir, "candidate.exe"),
			Mode:                 WindowsProductPairModeUpgrade,
		}, func(root string) (*windowsProductPairTaskStore, error) {
			store, err := newWindowsProductPairTaskStore(root)
			if err != nil {
				return nil, err
			}
			partialStore = store
			storeRoot = store.RootPath()
			retainedRef, err = store.Retain(context.Background(), retainedXML)
			if err != nil {
				t.Fatalf("materialize real retained task XML: %v", err)
			}
			if got, readErr := os.ReadFile(filepath.Join(storeRoot, retainedRef)); readErr != nil || !bytes.Equal(got, retainedXML) {
				t.Fatalf("real retained task XML was not present before constructor cleanup: bytes=%q error=%v", got, readErr)
			}
			if err := os.WriteFile(filepath.Join(storeRoot, "test-owned-unrecognized-leaf"), []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}
			return store, acquisitionErr
		}, func(context.Context, string, string, string, scheduler.RetainedTaskXMLStore) (*scheduler.OwnedEntrypointTaskTxn, error) {
			t.Fatal("task begin ran after store acquisition returned an error")
			return nil, nil
		})
		if err == nil || !errors.Is(err, acquisitionErr) || !strings.Contains(err.Error(), "UPGRADE_GUI_TASK_STORE_CLEANUP_UNCONFIRMED") {
			t.Fatalf("constructor error=%v, want original acquisition cause joined with cleanup uncertainty", err)
		}
		if partialStore == nil {
			t.Fatal("partial physical store was not returned to the constructor")
		}
		retainedPath := filepath.Join(storeRoot, retainedRef)
		if _, statErr := os.Lstat(retainedPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("constructor did not dispose selected retained XML material: stat error=%v", statErr)
		}
		unrecognizedPath := filepath.Join(storeRoot, "test-owned-unrecognized-leaf")
		if got, readErr := os.ReadFile(unrecognizedPath); readErr != nil || string(got) != "preserve" {
			t.Fatalf("constructor cleanup changed unrelated material: bytes=%q error=%v", got, readErr)
		}
		if _, statErr := os.Stat(storeRoot); statErr != nil {
			t.Fatalf("unrelated material's containing root was removed: %v", statErr)
		}
		if _, retainErr := partialStore.Retain(context.Background(), []byte("after-close")); retainErr == nil || !strings.Contains(retainErr.Error(), "closed") {
			t.Fatalf("partial store remained usable after selected cleanup: error=%v", retainErr)
		}
	})
}

func TestWindowsProductTxnPredecisionFaultRestoresProductTasksLegacyAndReceipt(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	product := filepath.Join(dir, "mcphub.exe")
	legacy := filepath.Join(dir, "mcphub-windowless.exe")
	staged := filepath.Join(dir, "mcphub.exe.stage")
	priorReceipt := filepath.Join(stateDir, UpgradeReceiptSchemaV2+".json")
	writePairFixture(t, staged, []byte("new-product"))
	writePairFixture(t, legacy, []byte("legacy-windowless"))
	writePairFixture(t, priorReceipt, []byte("prior-v2-receipt"))
	var order []string
	tasks := &productPairTaskFake{order: &order}
	deps := productTxnTestDeps(&order)
	deps.Fault = func(stage WindowsProductStage) error {
		if stage == WindowsProductStageLegacyRetired {
			return errors.New("injected legacy-retired fault")
		}
		return nil
	}
	txn := &WindowsProductTxn{Opts: WindowsProductTxnOpts{
		StateDir: stateDir, ProductPath: product, LegacyWindowlessPath: legacy,
		StagedProduct: staged, Mode: WindowsProductPairModeCanonicalize,
		ReplacementAuthority: windowsProductReplacementAuthority{
			Disposition: windowsProductPairDispositionV1{CLI: windowsProductTargetCreate, Windowless: windowsProductTargetIdentical},
		},
		Tasks: tasks, Deps: deps,
	}}
	if _, err := txn.Run(context.Background()); err == nil {
		t.Fatal("fault unexpectedly committed")
	}
	if _, err := os.Stat(product); !os.IsNotExist(err) {
		t.Fatalf("prior-absent product was not restored: %v", err)
	}
	assertPairFile(t, legacy, []byte("legacy-windowless"))
	assertPairFile(t, priorReceipt, []byte("prior-v2-receipt"))
	if !tasks.restored {
		t.Fatal("task XML was not restored")
	}
	for _, step := range order {
		if step == "publish-product" {
			t.Fatalf("predecision fault published committed event: %v", order)
		}
	}
}

func TestWindowsProductTxnPostdecisionEventFailureNeverRollsBack(t *testing.T) {
	txn, paths, order := newProductTxnFixture(t)
	txn.Opts.Deps.PublishCommitted = func(WindowsProductCommitEvent) (string, error) {
		*order = append(*order, "publish-product")
		return strings.Repeat("f", sha256.Size*2), errors.New("event sink unavailable")
	}
	committed, err := txn.Run(context.Background())
	if !errors.Is(err, ErrWindowsProductPairCommittedObservabilityFailed) {
		t.Fatalf("error=%v", err)
	}
	if committed.Outcome != WindowsProductPairCommittedObservabilityFailed {
		t.Fatalf("committed=%+v", committed)
	}
	assertPairFile(t, paths.product, []byte("new-product"))
	if _, statErr := os.Stat(paths.legacy); !os.IsNotExist(statErr) {
		t.Fatalf("postdecision event failure restored legacy target: %v", statErr)
	}
	raw, readErr := os.ReadFile(filepath.Join(paths.stateDir, WindowsProductRecoveryFileLeafV3))
	if readErr != nil {
		t.Fatal(readErr)
	}
	journal, decodeErr := decodeWindowsProductRecoveryJournalV3(raw)
	if decodeErr != nil || journal.Phase != WindowsProductRecoveryCommitDecided {
		t.Fatalf("journal=%+v err=%v", journal, decodeErr)
	}
}

func TestWindowsProductTxnPostdecisionCleanupFailureRetainsCommitDecision(t *testing.T) {
	txn, paths, _ := newProductTxnFixture(t)
	remove := txn.Opts.Deps.RemoveExact
	txn.Opts.Deps.RemoveExact = func(path string) error {
		if strings.Contains(filepath.Base(path), ".old-") {
			return errors.New("mapped retained image")
		}
		return remove(path)
	}
	committed, err := txn.Run(context.Background())
	if !errors.Is(err, ErrWindowsProductCleanupPending) {
		t.Fatalf("error=%v", err)
	}
	if committed.Outcome != WindowsProductPairCommittedCleanupFailed {
		t.Fatalf("committed=%+v", committed)
	}
	assertPairFile(t, paths.product, []byte("new-product"))
	if _, statErr := os.Stat(paths.legacy); !os.IsNotExist(statErr) {
		t.Fatalf("postdecision cleanup failure restored legacy target: %v", statErr)
	}
	raw, readErr := os.ReadFile(filepath.Join(paths.stateDir, WindowsProductRecoveryFileLeafV3))
	if readErr != nil {
		t.Fatal(readErr)
	}
	journal, decodeErr := decodeWindowsProductRecoveryJournalV3(raw)
	if decodeErr != nil || journal.Phase != WindowsProductRecoveryCommitDecided {
		t.Fatalf("journal=%+v err=%v", journal, decodeErr)
	}
}

type productTxnFixturePaths struct {
	stateDir string
	product  string
	legacy   string
	staged   string
}

func newProductTxnFixture(t *testing.T) (*WindowsProductTxn, productTxnFixturePaths, *[]string) {
	t.Helper()
	dir := t.TempDir()
	paths := productTxnFixturePaths{
		stateDir: filepath.Join(dir, "state"),
		product:  filepath.Join(dir, "mcphub.exe"),
		legacy:   filepath.Join(dir, "mcphub-windowless.exe"),
		staged:   filepath.Join(dir, "mcphub.exe.stage"),
	}
	if err := os.MkdirAll(paths.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writePairFixture(t, paths.staged, []byte("new-product"))
	writePairFixture(t, paths.legacy, []byte("legacy-windowless"))
	order := new([]string)
	tasks := &productPairTaskFake{order: order}
	deps := productTxnTestDeps(order)
	txn := &WindowsProductTxn{Opts: WindowsProductTxnOpts{
		StateDir: paths.stateDir, ProductPath: paths.product,
		LegacyWindowlessPath: paths.legacy, StagedProduct: paths.staged,
		Mode: WindowsProductPairModeCanonicalize,
		ReplacementAuthority: windowsProductReplacementAuthority{
			Disposition: windowsProductPairDispositionV1{CLI: windowsProductTargetCreate, Windowless: windowsProductTargetIdentical},
		},
		Tasks: tasks, Deps: deps,
	}}
	return txn, paths, order
}

func TestWindowsProductTxnPreparePersistsGUIOwnedLivePreimageBeforeStop(t *testing.T) {
	txn, paths, _ := newProductTxnFixture(t)
	writePairFixture(t, paths.product, []byte("prior-product"))
	txn.Opts.Mode = WindowsProductPairModeUpgrade
	txn.Opts.FrozenPorts = []windowsProductIntentPortV1{}
	txn.Opts.PreSettlement = &windowsProductQuiescenceObservationV1{
		Supervisor: windowsProductSupervisorSettlementV1{
			State:      windowsProductSupervisorGracefullySettled,
			Generation: &windowsProductSupervisorGenerationV1{PID: 4123, StartedAt: "2026-09-29T00:00:00Z"},
		},
		Ports: []windowsProductPortSettlementV1{}, GUIOwner: "confirmed-present",
	}
	if err := txn.Prepare(); err != nil {
		t.Fatalf("write-ahead prepare rejected live GUI/fleet: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(paths.stateDir, WindowsProductRecoveryFileLeafV3))
	if err != nil {
		t.Fatal(err)
	}
	j, err := decodeWindowsProductRecoveryJournalV3(raw)
	if err != nil || string(j.Phase) != "gui_owned_stop_pending" || j.PreSettlement == nil ||
		j.PreSettlement.GUIOwner != "confirmed-present" || j.PreSettlement.Supervisor.Generation == nil ||
		j.PreSettlement.Supervisor.Generation.PID != 4123 || j.PriorFleetReaped {
		t.Fatalf("pre-stop journal lost live restoration obligation: phase=%q pre=%+v reaped=%v err=%v", j.Phase, j.PreSettlement, j.PriorFleetReaped, err)
	}
}

func TestWindowsProductTxnGUIOwnedTransitionPreservesOriginalLivePreimage(t *testing.T) {
	txn, paths, _ := newProductTxnFixture(t)
	prior := []byte("prior-product")
	writePairFixture(t, paths.product, prior)
	txn.Opts.Mode = WindowsProductPairModeUpgrade
	txn.Opts.FrozenPorts = []windowsProductIntentPortV1{}
	live := windowsProductSupervisorSettlementV1{State: windowsProductSupervisorGracefullySettled,
		Generation: &windowsProductSupervisorGenerationV1{PID: 4123, StartedAt: "2026-09-29T00:00:00Z"}}
	txn.Opts.PreSettlement = &windowsProductQuiescenceObservationV1{
		Supervisor: live, Ports: []windowsProductPortSettlementV1{}, GUIOwner: windowsProductGUIOwnerConfirmedPresent,
	}
	if err := txn.Prepare(); err != nil {
		t.Fatal(err)
	}
	priorProductHash := sha256.Sum256(prior)
	priorLegacyHash := sha256.Sum256([]byte("legacy-windowless"))
	receipt, err := mintWindowsProductQuiescenceReceipt(paths.product, paths.legacy,
		windowsProductPairPreflightV1{
			PriorCLI:        windowsProductPreimageV1{Present: true, SHA256: fmt.Sprintf("%x", priorProductHash)},
			PriorWindowless: windowsProductPreimageV1{Present: true, SHA256: fmt.Sprintf("%x", priorLegacyHash)},
		}, windowsProductQuiescenceObservationV1{
			Supervisor: live, Ports: []windowsProductPortSettlementV1{},
			GUIOwner: windowsProductGUIOwnerConfirmedAbsent, SupervisorLockReleased: true,
		}, func() time.Time { return time.Date(2026, 9, 29, 0, 0, 1, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	if err := txn.SettleGUIOwnedStop(receipt); err != nil {
		t.Fatalf("settle GUI-owned stop: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(paths.stateDir, WindowsProductRecoveryFileLeafV3))
	if err != nil {
		t.Fatal(err)
	}
	j, err := decodeWindowsProductRecoveryJournalV3(raw)
	if err != nil || j.Phase != WindowsProductRecoveryPrepared || !j.PriorFleetReaped ||
		j.PreSettlement.GUIOwner != windowsProductGUIOwnerConfirmedPresent ||
		j.Quiescence == nil || j.Quiescence.GUIOwner != windowsProductGUIOwnerConfirmedAbsent ||
		j.PreSettlement.Supervisor.Generation.PID != 4123 {
		t.Fatalf("settled journal lost write-ahead preimage: phase=%q pre=%+v q=%+v err=%v", j.Phase, j.PreSettlement, j.Quiescence, err)
	}
}

func TestWindowsProductRecoveryLegacyPreparedRejectsPresentGUI(t *testing.T) {
	txn, paths, _ := newProductTxnFixture(t)
	writePairFixture(t, paths.product, []byte("prior-product"))
	txn.Opts.Mode = WindowsProductPairModeUpgrade
	txn.Opts.FrozenPorts = []windowsProductIntentPortV1{}
	txn.Opts.PreSettlement = &windowsProductQuiescenceObservationV1{
		Supervisor: windowsProductSupervisorSettlementV1{State: windowsProductSupervisorGracefullySettled,
			Generation: &windowsProductSupervisorGenerationV1{PID: 4123, StartedAt: "2026-09-29T00:00:00Z"}},
		Ports: []windowsProductPortSettlementV1{}, GUIOwner: windowsProductGUIOwnerConfirmedPresent,
	}
	if err := txn.Prepare(); err != nil {
		t.Fatal(err)
	}
	j := txn.prepared.journal
	j.Phase = WindowsProductRecoveryPrepared
	if err := validateWindowsProductRecoveryJournalV3(j); err == nil {
		t.Fatal("legacy prepared phase accepted an unsettled present GUI")
	}
}

func TestWindowsProductTxnGUIOwnedPrepareWriteOrReadFailureLeavesUnprepared(t *testing.T) {
	for _, failure := range []string{"write", "read"} {
		t.Run(failure, func(t *testing.T) {
			txn, paths, _ := newProductTxnFixture(t)
			writePairFixture(t, paths.product, []byte("prior-product"))
			txn.Opts.Mode = WindowsProductPairModeUpgrade
			txn.Opts.FrozenPorts = []windowsProductIntentPortV1{}
			txn.Opts.PreSettlement = &windowsProductQuiescenceObservationV1{
				Supervisor: windowsProductSupervisorSettlementV1{State: windowsProductSupervisorGracefullySettled,
					Generation: &windowsProductSupervisorGenerationV1{PID: 4123, StartedAt: "2026-09-29T00:00:00Z"}},
				Ports: []windowsProductPortSettlementV1{}, GUIOwner: windowsProductGUIOwnerConfirmedPresent,
			}
			if failure == "write" {
				txn.Opts.Deps.WriteRecoveryJournal = func(string, []byte) error { return errors.New("injected write failure") }
			} else {
				txn.Opts.Deps.ReadRecoveryJournal = func(string) ([]byte, error) { return nil, errors.New("injected read failure") }
			}
			if err := txn.Prepare(); err == nil || !errors.Is(err, ErrWindowsProductPairRecoveryPrepare) {
				t.Fatalf("%s failure was not reported as preparation failure: %v", failure, err)
			}
			if txn.prepared != nil {
				t.Fatalf("%s failure armed GUI stop", failure)
			}
			assertPairFile(t, paths.product, []byte("prior-product"))
		})
	}
}

func TestWindowsProductTxnGUIOwnedInProcessRollbackRetainsJournalUntilFleetProof(t *testing.T) {
	txn, paths, _ := newProductTxnFixture(t)
	writePairFixture(t, paths.product, []byte("prior-product"))
	txn.Opts.Mode = WindowsProductPairModeUpgrade
	txn.Opts.FrozenPorts = []windowsProductIntentPortV1{}
	txn.Opts.PreSettlement = &windowsProductQuiescenceObservationV1{
		Supervisor: windowsProductSupervisorSettlementV1{State: windowsProductSupervisorGracefullySettled,
			Generation: &windowsProductSupervisorGenerationV1{PID: 4123, StartedAt: "2026-09-29T00:00:00Z"}},
		Ports: []windowsProductPortSettlementV1{}, GUIOwner: windowsProductGUIOwnerConfirmedPresent,
	}
	if err := txn.Prepare(); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(paths.stateDir, WindowsProductRecoveryFileLeafV3)
	if err := txn.rollback(errors.New("injected stop failure")); !errors.Is(err, ErrWindowsProductRecoveryPending) {
		t.Fatalf("missing in-process fleet proof did not retain pending error: %v", err)
	}
	if _, err := os.Stat(journalPath); err != nil {
		t.Fatalf("pending rollback journal lost: %v", err)
	}
	txn.prepared.deps.RecoverPriorSettlement = func(j windowsProductRecoveryJournalV3) error {
		if j.PreSettlement == nil || j.PreSettlement.Supervisor.Generation == nil || j.PreSettlement.Supervisor.Generation.PID != 4123 {
			t.Fatalf("in-process callback lost live preimage: %+v", j.PreSettlement)
		}
		assertPairFile(t, paths.product, []byte("prior-product"))
		return nil
	}
	if err := txn.rollback(errors.New("injected stop failure")); errors.Is(err, ErrWindowsProductRecoveryPending) {
		t.Fatalf("verified prior fleet stayed pending: %v", err)
	}
	if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settled rollback journal remains: %v", err)
	}
}

func TestRunInstallUpgradePersistsInitialV3JournalBeforeLifecycleMutation(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	product := filepath.Join(dir, "mcphub.exe")
	legacy := filepath.Join(dir, "mcphub-windowless.exe")
	staged := filepath.Join(dir, "mcphub.exe.stage")
	writePairFixture(t, staged, []byte("new-product"))
	var order []string
	tasks := &productPairTaskFake{order: &order}
	deps := productTxnTestDeps(&order)
	writeJournal := deps.WriteRecoveryJournal
	firstJournal := true
	deps.WriteRecoveryJournal = func(path string, raw []byte) error {
		order = append(order, "journal")
		if firstJournal {
			firstJournal = false
			journal, err := decodeWindowsProductRecoveryJournalV3(raw)
			if err != nil {
				return err
			}
			if journal.Phase != WindowsProductRecoveryPrepared || journal.Quiescence != nil ||
				journal.PriorFleetReaped || journal.TaskSetSHA256 != "" {
				return fmt.Errorf("initial journal is not pre-settlement prepared: %+v", journal)
			}
		}
		return writeJournal(path, raw)
	}
	txn := &WindowsProductTxn{Opts: WindowsProductTxnOpts{
		StateDir: stateDir, ProductPath: product, LegacyWindowlessPath: legacy,
		StagedProduct: staged, Mode: WindowsProductPairModeUpgrade,
		Tasks: tasks, Deps: deps,
	}}
	lifecycle := &windowsProductPairLifecycleHooks{
		Observe: func(context.Context, string, []windowsProductIntentPortV1) (windowsProductQuiescenceObservationV1, error) {
			order = append(order, "observe")
			return windowsProductQuiescenceObservationV1{
				Supervisor: windowsProductSupervisorSettlementV1{State: windowsProductSupervisorKnownAbsent},
				GUIOwner:   windowsProductGUIOwnerConfirmedAbsent, SupervisorLockReleased: true,
			}, nil
		},
		Now: func() time.Time { return time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC) },
	}
	upgradeDeps := &fakeUpgradeDeps{}
	var preparedOutputErr error
	if err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		WindowsProduct: txn, WindowsProductPairLifecycle: lifecycle,
		OnProductPrepared: func() error {
			raw, err := os.ReadFile(filepath.Join(stateDir, WindowsProductRecoveryFileLeafV3))
			if err != nil {
				t.Fatalf("prepared stage before journal readback: %v", err)
			}
			journal, err := decodeWindowsProductRecoveryJournalV3(raw)
			if err != nil || journal.Phase != WindowsProductRecoveryPrepared {
				t.Fatalf("prepared stage journal=(%+v,%v)", journal, err)
			}
			order = append(order, "prepared-stage")
			return upgradeGUIPreparedStageReporter(upgradeGUIOutputFailure{err: errors.New("synthetic prepared output failure")}, &preparedOutputErr)()
		},
		BinaryPath: product, Deps: upgradeDeps,
	}); err != nil {
		t.Fatalf("run upgrade: %v", err)
	}
	if !recoveryStepsOrdered(order, []string{"observe", "tasks-snapshot", "journal", "prepared-stage", "promote-cli"}) {
		t.Fatalf("initial journal was not persisted before mutation: %v", order)
	}
	if preparedOutputErr == nil || !strings.Contains(preparedOutputErr.Error(), "UPGRADE_GUI_STAGE_OUTPUT_UNCONFIRMED") {
		t.Fatalf("Product did not retain prepared output failure after transaction: %v", preparedOutputErr)
	}
}

func productTxnTestDeps(order *[]string) WindowsProductTxnDeps {
	pair := productPairTestDeps(order)
	return withWindowsProductDefaults(WindowsProductTxnDeps{
		AdmitProduct: func(input binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error) {
			digest, err := hashRecoveryFile(input.Path)
			if err != nil {
				return binaryadmission.WindowsArtifact{}, err
			}
			if input.SHA256 != "" && input.SHA256 != digest {
				return binaryadmission.WindowsArtifact{}, errors.New("product SHA mismatch")
			}
			return binaryadmission.WindowsArtifact{
				Path: input.Path, Role: binaryadmission.WindowsArtifactRoleProduct,
				Machine: 34404, Subsystem: binaryadmission.WindowsGUISubsystem,
				Version: "0.4.38", Commit: strings.Repeat("a", 40),
				BuildDate: "2026-09-11T00:00:00Z", SHA256: digest,
			}, nil
		},
		AdmitLegacyWindowless: func(input binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error) {
			digest, err := hashRecoveryFile(input.Path)
			if err != nil {
				return binaryadmission.WindowsArtifact{}, err
			}
			if input.SHA256 != digest {
				return binaryadmission.WindowsArtifact{}, errors.New("legacy SHA mismatch")
			}
			input.SHA256 = digest
			return input, nil
		},
		Promote: pair.Promote, RestorePromotion: pair.RestorePromotion,
		ListGeneratedAsides: listGeneratedRenameAsideBasenames,
		PublishCommitted: func(WindowsProductCommitEvent) (string, error) {
			*order = append(*order, "publish-product")
			return strings.Repeat("f", sha256.Size*2), nil
		},
		Now: func() time.Time { return time.Date(2026, 9, 11, 0, 0, 1, 0, time.UTC) },
	})
}

type productPairPaths struct{ priorCLI, windowless, stagedCLI, stagedWindowless, receipt string }

func productPairFixturePaths(dir string) productPairPaths {
	return productPairPaths{
		priorCLI: filepath.Join(dir, "mcphub.exe"), windowless: filepath.Join(dir, "mcphub-windowless.exe"),
		stagedCLI: filepath.Join(dir, "staged", "mcphub.exe"), stagedWindowless: filepath.Join(dir, "staged", "mcphub-windowless.exe"),
		receipt: filepath.Join(dir, "state", "upgrade-receipt-v2.json"),
	}
}

func productPairTestDeps(order *[]string) WindowsProductPairTxnDeps {
	return WindowsProductPairTxnDeps{
		AdmitPair: func(cli, windowless binaryadmission.WindowsArtifact) (binaryadmission.WindowsProductPair, error) {
			if filepath.Base(filepath.Dir(cli.Path)) == "staged" {
				*order = append(*order, "admit-staged")
			} else {
				*order = append(*order, "readback-pair")
			}
			return binaryadmission.WindowsProductPair{
				CLI:        binaryadmission.WindowsArtifact{Path: cli.Path, Role: binaryadmission.WindowsArtifactRoleCLI, Version: "0.4.36", Commit: "abc1234", BuildDate: "2026-09-01T00:00:00Z", SHA256: productPairTestCLISHA},
				Windowless: binaryadmission.WindowsArtifact{Path: windowless.Path, Role: binaryadmission.WindowsArtifactRoleWindowless, Version: "0.4.36", Commit: "abc1234", BuildDate: "2026-09-01T00:00:00Z", SHA256: productPairTestWindowlessSHA},
			}, nil
		},
		Promote: func(src, dst, _ string) (WindowsProductPairPromotion, error) {
			if filepath.Base(dst) == "mcphub-windowless.exe" {
				*order = append(*order, "promote-windowless")
			} else {
				*order = append(*order, "promote-cli")
			}
			body, err := os.ReadFile(src)
			if err != nil {
				return WindowsProductPairPromotion{}, err
			}
			promotion := WindowsProductPairPromotion{Target: dst, NewSHA256: fmt.Sprintf("%x", sha256.Sum256(body))}
			if prior, err := os.ReadFile(dst); err == nil {
				promotion.PriorPresent = true
				promotion.RetainedPrior = dst + ".retained"
				if err := os.WriteFile(promotion.RetainedPrior, prior, 0o600); err != nil {
					return WindowsProductPairPromotion{}, err
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return WindowsProductPairPromotion{}, err
			}
			if err := os.WriteFile(dst, body, 0o600); err != nil {
				return WindowsProductPairPromotion{}, err
			}
			return promotion, nil
		},
		RestorePromotion: func(p WindowsProductPairPromotion) (string, error) {
			successor, err := os.ReadFile(p.Target)
			if err != nil {
				return "", err
			}
			body, err := os.ReadFile(p.RetainedPrior)
			if err != nil {
				return "", err
			}
			displaced := p.Target + ".old-" + time.Now().UTC().Format("20060102T150405Z")
			if err := os.WriteFile(displaced, successor, 0o600); err != nil {
				return "", err
			}
			if err := os.WriteFile(p.Target, body, 0o600); err != nil {
				return displaced, err
			}
			return displaced, nil
		},
		ReadBackWindowless: func(string, binaryadmission.WindowsArtifact) error {
			*order = append(*order, "readback-windowless")
			return nil
		},
		StartSupervisor: func(string) (SupervisorLaunch, error) {
			*order = append(*order, "start-supervisor")
			return &fixtureSupervisorLaunch{}, nil
		},
		SettleSuccessor: func(string) error { *order = append(*order, "settle-successor"); return nil },
		WaitSupervisorReady: func(context.Context, string, binaryadmission.WindowsProductPair) error {
			*order = append(*order, "supervisor-ready")
			return nil
		},
		WriteReceipt: func(path string, raw []byte) error {
			*order = append(*order, "receipt")
			return api.WriteStateFileBytesAtomic(path, raw)
		},
		ReadReceipt: func(path string) ([]byte, error) {
			*order = append(*order, "receipt-readback")
			return os.ReadFile(path)
		},
		PublishCommitted: func(event WindowsProductPairCommitEvent) (string, error) {
			*order = append(*order, "publish-committed")
			return "event-sha", nil
		},
		Now: func() time.Time { return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) },
	}
}

func TestWindowsProductPairCommitReconciliationIsReceiptBoundAndIdempotent(t *testing.T) {
	pair := binaryadmission.WindowsProductPair{
		CLI:        binaryadmission.WindowsArtifact{Role: binaryadmission.WindowsArtifactRoleCLI, Version: "0.4.36", Commit: "abc1234", BuildDate: "2026-09-01T00:00:00Z", SHA256: productPairTestCLISHA},
		Windowless: binaryadmission.WindowsArtifact{Role: binaryadmission.WindowsArtifactRoleWindowless, Version: "0.4.36", Commit: "abc1234", BuildDate: "2026-09-01T00:00:00Z", SHA256: productPairTestWindowlessSHA},
	}
	legacyReceipt := UpgradeReceiptV2{Schema: UpgradeReceiptSchemaV2, Admission: UpgradeAdmissionLocalProduct, Version: pair.CLI.Version, Commit: pair.CLI.Commit, BuildDate: pair.CLI.BuildDate, Artifacts: pair, InstalledAt: "2026-09-01T00:00:01Z"}
	receipt := legacyReceipt
	receipt.Mode = WindowsProductPairModeSetup
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var events []WindowsProductPairCommitEvent
	publish := func(event WindowsProductPairCommitEvent) (string, error) {
		events = append(events, event)
		return "same-digest-carrier", nil
	}
	for range 2 {
		reconciled, err := reconcileWindowsProductPairCommit(raw, pair, pair, WindowsProductPairModeSetup, publish)
		if err != nil || !reconciled {
			t.Fatalf("reconciled=%v err=%v", reconciled, err)
		}
	}
	if len(events) != 2 || !reflect.DeepEqual(events[0], events[1]) || events[0].ReceiptSHA256 != fmt.Sprintf("%x", sha256.Sum256(raw)) {
		t.Fatalf("reconciled events=%+v", events)
	}
	mismatch := pair
	mismatch.CLI.SHA256 = "different"
	if reconciled, err := reconcileWindowsProductPairCommit(raw, mismatch, pair, WindowsProductPairModeSetup, publish); err != nil || reconciled || len(events) != 2 {
		t.Fatalf("mismatch reconciled=%v err=%v events=%d", reconciled, err, len(events))
	}
	if reconciled, err := reconcileWindowsProductPairCommit(raw, pair, pair, WindowsProductPairModeUpgrade, publish); err != nil || reconciled || len(events) != 2 {
		t.Fatalf("cross-mode receipt reconciled=%v err=%v events=%d", reconciled, err, len(events))
	}
	legacyRaw, err := json.Marshal(legacyReceipt)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled, err := reconcileWindowsProductPairCommit(legacyRaw, pair, pair, WindowsProductPairModeSetup, publish); err != nil || reconciled || len(events) != 2 {
		t.Fatalf("mode-less receipt reconciled=%v err=%v events=%d", reconciled, err, len(events))
	}
	unsupportedReceipt := legacyReceipt
	unsupportedReceipt.Mode = "future-mode"
	unsupportedRaw, err := json.Marshal(unsupportedReceipt)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeUpgradeReceipt(unsupportedRaw)
	if err != nil || decoded.V2 == nil || decoded.V2.Mode != "future-mode" {
		t.Fatalf("unsupported mode receipt was not read-compatible: decoded=%+v err=%v", decoded, err)
	}
	if reconciled, err := reconcileWindowsProductPairCommit(unsupportedRaw, pair, pair, "future-mode", publish); err != nil || reconciled || len(events) != 2 {
		t.Fatalf("unsupported mode receipt reconciled=%v err=%v events=%d", reconciled, err, len(events))
	}
}

func writePairFixture(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}
func assertPairFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s bytes=%q err=%v want=%q", path, got, err, want)
	}
}

func mustReadPairFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestWindowsProductPairTaskStorePrivateJournalClosePreservesBytes(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprintf("uncertain=%t", uncertain), func(t *testing.T) {
			stateDir := withTempStateDir(t)
			store, err := newWindowsProductPairTaskStore(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			xml := []byte("<Task>synthetic journal retention</Task>")
			ref, err := store.Retain(context.Background(), xml)
			if err != nil {
				t.Fatal(err)
			}
			journal := filepath.Join(stateDir, WindowsProductRecoveryFileLeafV3)
			if uncertain {
				if err := os.Mkdir(journal, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(journal, []byte("synthetic pending journal"), 0o600); err != nil {
				t.Fatal(err)
			}
			tasks := &productPairTaskFake{}
			txn := &WindowsProductTxn{constructorStore: store, Opts: WindowsProductTxnOpts{StateDir: stateDir, Tasks: tasks}}
			closeErr := txn.Close()
			if uncertain && (closeErr == nil || !strings.Contains(closeErr.Error(), "UPGRADE_GUI_TASK_STORE_CLEANUP_UNCONFIRMED")) {
				t.Fatalf("uncertain journal cleanup was accepted: %v", closeErr)
			}
			if !uncertain && closeErr != nil {
				t.Fatalf("journal-preserving close: %v", closeErr)
			}
			if !tasks.closed {
				t.Fatal("coordinator did not close its task lease")
			}
			got, err := store.Load(context.Background(), ref)
			if err != nil || !bytes.Equal(got, xml) {
				t.Fatalf("journal-linked store bytes were discarded: %v", err)
			}
		})
	}
}

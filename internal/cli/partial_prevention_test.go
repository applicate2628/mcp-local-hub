//go:build windows

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// TestWindowsProductPairRejectsExistingTargetsWithoutQuiescence catches any
// direct transaction caller bypassing RunInstallUpgrade and replacing an
// existing different canonical target without lifecycle authority.
// TestSetupAndCanonicalizeRefuseDifferentExistingPairBeforeTransaction proves
// both non-upgrade callers stop before constructing the artifact/task owner.
func testWindowsProductPreimage(contents []byte) windowsProductPreimageV1 {
	return windowsProductPreimageV1{Present: true, SHA256: fmt.Sprintf("%x", sha256.Sum256(contents))}
}

func TestObserveWindowsProductPairQuiescenceClassifiesTempLockLifecycle(t *testing.T) {
	originalGUIProbe := findRunningGUIsOnTargetFn
	findRunningGUIsOnTargetFn = func(string) ([]api.ProcessInfo, error) { return nil, nil }
	t.Cleanup(func() { findRunningGUIsOnTargetFn = originalGUIProbe })
	stateDir := t.TempDir()
	target := filepath.Join(t.TempDir(), "mcphub.exe")

	absent, err := observeWindowsProductPairQuiescence(context.Background(), stateDir, target, nil)
	if err != nil || absent.Supervisor.State != windowsProductSupervisorKnownAbsent || !absent.SupervisorLockReleased {
		t.Fatalf("known-absent observation=%+v err=%v", absent, err)
	}

	lock, err := api.AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		t.Fatalf("acquire isolated supervisor lock: %v", err)
	}
	t.Cleanup(lock.Release)
	graceful, err := observeWindowsProductPairQuiescence(context.Background(), stateDir, target, nil)
	if err != nil || graceful.Supervisor.State != windowsProductSupervisorGracefullySettled || graceful.Supervisor.Generation == nil || graceful.Supervisor.Generation.PID <= 0 || graceful.SupervisorLockReleased {
		t.Fatalf("graceful observation=%+v err=%v", graceful, err)
	}
}

func partialPreventionUpgradePair(t *testing.T) (*WindowsProductTxn, productPairPaths, *[]string) {
	t.Helper()
	dir := t.TempDir()
	paths := productPairFixturePaths(dir)
	writePairFixture(t, paths.priorCLI, []byte("prior-cli"))
	writePairFixture(t, paths.windowless, []byte("prior-windowless"))
	cliBytes, windowlessBytes := []byte("new-product"), []byte("new-windowless")
	writePairFixture(t, paths.stagedCLI, cliBytes)
	writePairFixture(t, paths.stagedWindowless, windowlessBytes)
	order := &[]string{}
	deps := productTxnTestDeps(order)
	tasks := &productPairTaskFake{order: order}
	stateDir := filepath.Dir(paths.receipt)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return &WindowsProductTxn{Opts: WindowsProductTxnOpts{
		StateDir: stateDir, ProductPath: paths.priorCLI, LegacyWindowlessPath: paths.windowless,
		StagedProduct: paths.stagedCLI, ReceiptV2Path: paths.receipt,
		Mode: WindowsProductPairModeUpgrade, Tasks: tasks, Deps: deps,
	}}, paths, order
}

func containsPartialPreventionStep(order []string, step string) bool {
	for _, got := range order {
		if got == step {
			return true
		}
	}
	return false
}

func TestWindowsProductPairRecoveryJournalV2DispatchAndV1Refusal(t *testing.T) {
	f := newRecoveryFixture(t)
	receipt := windowsProductQuiescenceReceiptV1{
		ReceiptID: "q-v2", Scope: windowsProductQuiescenceScopeV1, ObservationSHA256: strings.Repeat("a", 64),
		CanonicalCLIPath: f.cli, CanonicalWindowlessPath: f.windowless,
		PriorCLI: testWindowsProductPreimage(f.priorCLI), PriorWindowless: testWindowsProductPreimage(f.priorWindowless),
		Supervisor: windowsProductSupervisorSettlementV1{State: windowsProductSupervisorKnownAbsent},
		Ports:      []windowsProductPortSettlementV1{{TaskName: "task", Port: 9123, PreState: windowsProductPortUnbound, PostUnbound: true}},
		GUIOwner:   windowsProductGUIOwnerConfirmedAbsent, SupervisorLockReleased: true,
		SettledAtUTC: time.Date(2026, 9, 10, 5, 0, 0, 0, time.UTC),
	}
	projection, digest, err := projectWindowsProductRecoveryQuiescence(receipt)
	if err != nil {
		t.Fatal(err)
	}
	f.journal.Schema = WindowsProductPairRecoverySchemaV2
	f.journal.PriorFleetReaped = true
	f.journal.Quiescence = &projection
	f.journal.QuiescenceReceiptSHA256 = digest
	raw, err := json.Marshal(f.journal)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeWindowsProductPairRecoveryJournal(raw)
	if err != nil || decoded.Schema != WindowsProductPairRecoverySchemaV2 || decoded.Quiescence == nil {
		t.Fatalf("current decoder = %+v err=%v", decoded, err)
	}
	if _, err := decodeWindowsProductPairRecoveryJournalV1(raw); err == nil || !strings.Contains(err.Error(), "E_WINDOWS_PRODUCT_PAIR_RECOVERY_SCHEMA_UNSUPPORTED") {
		t.Fatalf("strict V1 decoder error = %v", err)
	}

	tampered := f.journal
	tampered.QuiescenceReceiptSHA256 = strings.Repeat("f", 64)
	tamperedRaw, err := json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeWindowsProductPairRecoveryJournal(tamperedRaw); err == nil {
		t.Fatal("current decoder accepted a V2 quiescence digest mismatch")
	}

	v1 := newRecoveryFixture(t).journal
	v1Raw, err := json.Marshal(v1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeWindowsProductPairRecoveryJournal(v1Raw); err != nil {
		t.Fatalf("current decoder rejected V1: %v", err)
	}
	if _, err := decodeWindowsProductPairRecoveryJournalV1(v1Raw); err != nil {
		t.Fatalf("strict V1 decoder rejected V1: %v", err)
	}
}

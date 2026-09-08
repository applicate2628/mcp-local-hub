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
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
)

type productPairTaskFake struct {
	order        *[]string
	restored     bool
	closed       bool
	closeErr     error
	inventoryErr error
}

func (f *productPairTaskFake) InventoryExport() error {
	*f.order = append(*f.order, "tasks-snapshot")
	return f.inventoryErr
}

func TestRunInstallUpgradeRecoversTypedPrePromotionPairFailuresExactlyOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*WindowsProductPairTxn, *productPairTaskFake, *WindowsProductPairTxnDeps, string)
	}{
		{"missing publisher", func(txn *WindowsProductPairTxn, _ *productPairTaskFake, deps *WindowsProductPairTxnDeps, _ string) {
			deps.PublishCommitted = nil
			txn.Opts.Deps = *deps
		}},
		{"staged admission", func(txn *WindowsProductPairTxn, _ *productPairTaskFake, deps *WindowsProductPairTxnDeps, _ string) {
			deps.AdmitPair = func(binaryadmission.WindowsArtifact, binaryadmission.WindowsArtifact) (binaryadmission.WindowsProductPair, error) {
				return binaryadmission.WindowsProductPair{}, errors.New("staged admission")
			}
			txn.Opts.Deps = *deps
		}},
		{"cli snapshot", func(txn *WindowsProductPairTxn, _ *productPairTaskFake, _ *WindowsProductPairTxnDeps, dir string) {
			txn.Opts.CLIPath = dir
		}},
		{"windowless snapshot", func(txn *WindowsProductPairTxn, _ *productPairTaskFake, _ *WindowsProductPairTxnDeps, dir string) {
			txn.Opts.WindowlessPath = dir
		}},
		{"receipt snapshot", func(txn *WindowsProductPairTxn, _ *productPairTaskFake, _ *WindowsProductPairTxnDeps, dir string) {
			txn.Opts.ReceiptPath = dir
		}},
		{"task inventory", func(_ *WindowsProductPairTxn, tasks *productPairTaskFake, _ *WindowsProductPairTxnDeps, _ string) {
			tasks.inventoryErr = errors.New("task inventory")
		}},
		{"invalid mode", func(txn *WindowsProductPairTxn, _ *productPairTaskFake, _ *WindowsProductPairTxnDeps, _ string) {
			txn.Opts.Mode = "invalid"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			paths := productPairFixturePaths(dir)
			writePairFixture(t, paths.priorCLI, []byte("prior-cli"))
			writePairFixture(t, paths.stagedCLI, []byte("staged-cli"))
			writePairFixture(t, paths.stagedWindowless, []byte("staged-windowless"))
			var order []string
			tasks := &productPairTaskFake{order: &order}
			deps := productPairTestDeps(&order)
			promotions := 0
			pairRestarts := 0
			deps.Promote = func(string, string, string) (WindowsProductPairPromotion, error) {
				promotions++
				return WindowsProductPairPromotion{}, errors.New("promotion must not run")
			}
			deps.RestartPrior = func(string) error { pairRestarts++; return nil }
			txn := &WindowsProductPairTxn{Opts: WindowsProductPairTxnOpts{CLIPath: paths.priorCLI, WindowlessPath: paths.windowless, StagedCLI: paths.stagedCLI, StagedWindowless: paths.stagedWindowless, ReceiptPath: paths.receipt, Mode: WindowsProductPairModeUpgrade, Tasks: tasks, Deps: deps}}
			tc.mutate(txn, tasks, &deps, dir)

			legacy := &fakeUpgradeDeps{quiesceResult: api.IPCResponse{ID: 1, OK: true, Result: map[string]any{"still_running": []any{}}, Final: true}, exitResult: api.IPCResponse{ID: 2, OK: true, Final: true}}
			err := RunInstallUpgrade(context.Background(), UpgradeOpts{WindowsProductPair: txn, BinaryPath: paths.priorCLI, PipePath: "pair-prepromotion", Deps: legacy, WaitSupervisorLockReleased: func(context.Context, time.Duration) error { return nil }})
			var pre *WindowsProductPairPrePromotionError
			if !errors.As(err, &pre) {
				t.Fatalf("error=%v, want typed pre-promotion error", err)
			}
			if promotions != 0 || pairRestarts != 0 {
				t.Fatalf("pre-promotion mutated pair: promotions=%d pair_restarts=%d", promotions, pairRestarts)
			}
			starts := 0
			for _, call := range legacy.calls {
				if call == "start" {
					starts++
				}
			}
			if starts != 1 || !tasks.closed {
				t.Fatalf("outer recovery starts=%d closed=%v calls=%v", starts, tasks.closed, legacy.calls)
			}
		})
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
func (f *productPairTaskFake) RestoreImport() error {
	f.restored = true
	*f.order = append(*f.order, "tasks-restore")
	return nil
}
func (f *productPairTaskFake) Close() error { f.closed = true; return f.closeErr }

func TestWindowsProductPairTxnOrdersAdapterTasksCLIReadbackReadinessAndReceipt(t *testing.T) {
	dir := t.TempDir()
	paths := productPairFixturePaths(dir)
	writePairFixture(t, paths.priorCLI, []byte("old-cli"))
	writePairFixture(t, paths.stagedCLI, []byte("new-cli"))
	writePairFixture(t, paths.stagedWindowless, []byte("new-windowless"))
	priorReceipt := []byte("{\"schema\":\"upgrade-receipt-v1\",\"sha256\":\"prior\"}\n")
	writePairFixture(t, paths.receipt, priorReceipt)

	var order []string
	tasks := &productPairTaskFake{order: &order}
	deps := productPairTestDeps(&order)
	committed, err := (WindowsProductPairTxn{Opts: WindowsProductPairTxnOpts{
		CLIPath: paths.priorCLI, WindowlessPath: paths.windowless, StagedCLI: paths.stagedCLI,
		StagedWindowless: paths.stagedWindowless, ReceiptPath: paths.receipt,
		Mode: WindowsProductPairModeUpgrade, Tasks: tasks, Deps: deps,
	}}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if committed.Schema != WindowsProductPairCommittedSchemaV1 {
		t.Fatalf("commit schema=%q", committed.Schema)
	}
	wantOrder := []string{"admit-staged", "tasks-snapshot", "promote-windowless", "readback-windowless", "tasks-rewrite", "tasks-readback", "promote-cli", "readback-pair", "start-supervisor", "supervisor-ready", "receipt", "receipt-readback", "publish-committed"}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("order=%v want=%v", order, wantOrder)
	}
	raw, err := os.ReadFile(paths.receipt)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeUpgradeReceipt(raw)
	if err != nil || decoded.V2 == nil || decoded.V2.Artifacts.CLI.SHA256 != "cli-sha" || decoded.V2.Artifacts.Windowless.SHA256 != "windowless-sha" {
		t.Fatalf("receipt=%+v err=%v raw=%s", decoded, err, raw)
	}
}

func TestWindowsProductPairTxnFaultMatrixRestoresExactPriorPairTasksAndReceipt(t *testing.T) {
	stages := []WindowsProductPairStage{
		WindowsProductPairStageWindowlessPromoted,
		WindowsProductPairStageWindowlessReadBack,
		WindowsProductPairStageTasksRewritten,
		WindowsProductPairStageTasksReadBack,
		WindowsProductPairStageCLIPromoted,
		WindowsProductPairStagePairReadBack,
		WindowsProductPairStageSupervisorStarted,
		WindowsProductPairStageSupervisorReady,
		WindowsProductPairStageReceiptWritten,
	}
	for _, faultStage := range stages {
		t.Run(string(faultStage), func(t *testing.T) {
			dir := t.TempDir()
			paths := productPairFixturePaths(dir)
			priorCLI := []byte("old-0.4.35-gui-cli")
			priorReceipt := []byte("{ \"schema\": \"upgrade-receipt-v2\", \"sentinel\": true }\r\n")
			writePairFixture(t, paths.priorCLI, priorCLI)
			writePairFixture(t, paths.stagedCLI, []byte("new-cli"))
			writePairFixture(t, paths.stagedWindowless, []byte("new-windowless"))
			writePairFixture(t, paths.receipt, priorReceipt)

			var order []string
			tasks := &productPairTaskFake{order: &order}
			deps := productPairTestDeps(&order)
			deps.Fault = func(stage WindowsProductPairStage) error {
				if stage == faultStage {
					return errors.New("injected " + string(stage))
				}
				return nil
			}
			_, err := (WindowsProductPairTxn{Opts: WindowsProductPairTxnOpts{
				CLIPath: paths.priorCLI, WindowlessPath: paths.windowless, StagedCLI: paths.stagedCLI,
				StagedWindowless: paths.stagedWindowless, ReceiptPath: paths.receipt,
				Mode: WindowsProductPairModeUpgrade, Tasks: tasks, Deps: deps,
			}}).Run(context.Background())
			if err == nil {
				t.Fatal("fault unexpectedly committed")
			}
			assertPairFile(t, paths.priorCLI, priorCLI)
			if _, statErr := os.Stat(paths.windowless); !os.IsNotExist(statErr) {
				t.Fatalf("absent prior companion was not removed: %v", statErr)
			}
			assertPairFile(t, paths.receipt, priorReceipt)
			if !tasks.restored && faultStage != WindowsProductPairStageWindowlessPromoted && faultStage != WindowsProductPairStageWindowlessReadBack {
				t.Fatalf("task XML not restored after %s", faultStage)
			}
		})
	}
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

func TestRunInstallUpgradeDelegatesPairMutationAfterLegacyReleaseToSoleOwner(t *testing.T) {
	dir := t.TempDir()
	paths := productPairFixturePaths(dir)
	writePairFixture(t, paths.priorCLI, []byte("old-cli"))
	writePairFixture(t, paths.stagedCLI, []byte("new-cli"))
	writePairFixture(t, paths.stagedWindowless, []byte("new-windowless"))
	var order []string
	tasks := &productPairTaskFake{order: &order}
	released := false
	legacy := &fakeUpgradeDeps{
		quiesceResult: api.IPCResponse{ID: 1, OK: true, Result: map[string]any{"still_running": []any{}}, Final: true},
		exitResult:    api.IPCResponse{ID: 2, OK: true, Final: true},
	}
	deps := productPairTestDeps(&order)
	admitPair := deps.AdmitPair
	deps.AdmitPair = func(cli, windowless binaryadmission.WindowsArtifact) (binaryadmission.WindowsProductPair, error) {
		if !released {
			return binaryadmission.WindowsProductPair{}, errors.New("pair owner invoked before prior supervisor release")
		}
		return admitPair(cli, windowless)
	}
	txn := &WindowsProductPairTxn{Opts: WindowsProductPairTxnOpts{
		CLIPath: paths.priorCLI, WindowlessPath: paths.windowless, StagedCLI: paths.stagedCLI,
		StagedWindowless: paths.stagedWindowless, ReceiptPath: paths.receipt,
		Mode: WindowsProductPairModeUpgrade, Tasks: tasks, Deps: deps,
	}}
	if err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		WindowsProductPair: txn,
		BinaryPath:         paths.priorCLI,
		PipePath:           "test-pipe",
		Deps:               legacy,
		WaitSupervisorLockReleased: func(context.Context, time.Duration) error {
			released = true
			return nil
		},
	}); err != nil {
		t.Fatalf("RunInstallUpgrade pair path: %v", err)
	}
	if len(order) == 0 || order[0] != "admit-staged" {
		t.Fatalf("pair owner not invoked: %v", order)
	}
	if !legacy.quiesceCalled || !legacy.exitCalled || !released {
		t.Fatalf("legacy release not completed: quiesce=%v exit=%v released=%v", legacy.quiesceCalled, legacy.exitCalled, released)
	}
	if legacy.renameAsideCalled || legacy.startCalled {
		t.Fatalf("parallel legacy mutation owner invoked: rename=%v start=%v", legacy.renameAsideCalled, legacy.startCalled)
	}
}

func TestRunInstallUpgradeClosesPairOwnerWhenLegacyPreconditionsFail(t *testing.T) {
	var order []string
	tasks := &productPairTaskFake{order: &order}
	txn := &WindowsProductPairTxn{Opts: WindowsProductPairTxnOpts{Tasks: tasks}}
	err := RunInstallUpgrade(context.Background(), UpgradeOpts{WindowsProductPair: txn})
	if err == nil {
		t.Fatal("missing legacy dependencies unexpectedly succeeded")
	}
	if !tasks.closed {
		t.Fatal("pre-pair legacy failure leaked the pair task owner")
	}
}

func TestCanonicalizeWindowsProductPairDelegatesAllMutationToSoleOwner(t *testing.T) {
	withTempStateDir(t)
	sourceCLI := writeAdmissionProductPairFixture(t, "CANONICALIZE")
	targetDir := t.TempDir()
	targetCLI := filepath.Join(targetDir, "mcphub.exe")
	targetWindowless := filepath.Join(targetDir, "mcphub-windowless.exe")
	priorCLI := []byte("prior-cli-exact")
	writePairFixture(t, targetCLI, priorCLI)

	originalFactory := newWindowsProductPairTxnFn
	t.Cleanup(func() { newWindowsProductPairTxnFn = originalFactory })
	var order []string
	newWindowsProductPairTxnFn = func(req windowsProductPairTxnRequest) (*WindowsProductPairTxn, error) {
		return &WindowsProductPairTxn{Opts: WindowsProductPairTxnOpts{
			CLIPath: req.CLIPath, WindowlessPath: req.WindowlessPath,
			StagedCLI: req.StagedCLI, StagedWindowless: req.StagedWindowless,
			ReceiptPath: filepath.Join(req.StateDir, UpgradeReceiptSchemaV2+".json"),
			Mode:        WindowsProductPairModeCanonicalize,
			Tasks:       &productPairTaskFake{order: &order}, Deps: productPairTestDeps(&order),
		}}, nil
	}
	var out bytes.Buffer
	if err := canonicalizeBinaryToTarget(&out, sourceCLI, targetCLI); err != nil {
		t.Fatalf("canonicalize pair: %v", err)
	}
	assertPairFile(t, targetCLI, mustReadPairFile(t, sourceCLI))
	assertPairFile(t, targetWindowless, mustReadPairFile(t, filepath.Join(filepath.Dir(sourceCLI), "mcphub-windowless.exe")))
	if !strings.Contains(out.String(), "product pair canonicalized") || len(order) == 0 {
		t.Fatalf("output=%q order=%v", out.String(), order)
	}
	if bytes.Equal(mustReadPairFile(t, targetCLI), priorCLI) {
		t.Fatal("canonical CLI remained at prior bytes")
	}
}

func TestWindowsProductPairTaskStoreExactReadbackAndCleanup(t *testing.T) {
	stateDir := withTempStateDir(t)
	store, err := newWindowsProductPairTaskStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	root := store.root
	want := []byte("<Task><Actions><Exec><Command>exact</Command></Exec></Actions></Task>\r\n")
	ref, err := store.Retain(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(context.Background(), ref)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("load bytes=%q err=%v", got, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback store survived Close: %v", err)
	}
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
				CLI:        binaryadmission.WindowsArtifact{Path: cli.Path, Role: binaryadmission.WindowsArtifactRoleCLI, Version: "0.4.36", Commit: "abc1234", BuildDate: "2026-09-01T00:00:00Z", SHA256: "cli-sha"},
				Windowless: binaryadmission.WindowsArtifact{Path: windowless.Path, Role: binaryadmission.WindowsArtifactRoleWindowless, Version: "0.4.36", Commit: "abc1234", BuildDate: "2026-09-01T00:00:00Z", SHA256: "windowless-sha"},
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
		StartSupervisor: func(string) error { *order = append(*order, "start-supervisor"); return nil },
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

func TestWindowsProductPairTxnEventFailureAfterReceiptDoesNotRollbackCommittedPair(t *testing.T) {
	dir := t.TempDir()
	paths := productPairFixturePaths(dir)
	writePairFixture(t, paths.priorCLI, []byte("old-cli"))
	writePairFixture(t, paths.stagedCLI, []byte("new-cli"))
	writePairFixture(t, paths.stagedWindowless, []byte("new-windowless"))
	var order []string
	tasks := &productPairTaskFake{order: &order}
	deps := productPairTestDeps(&order)
	deps.PublishCommitted = func(WindowsProductPairCommitEvent) (string, error) {
		order = append(order, "publish-failed")
		return "event-sha", errors.New("injected event carrier failure")
	}
	result, err := (WindowsProductPairTxn{Opts: WindowsProductPairTxnOpts{
		CLIPath: paths.priorCLI, WindowlessPath: paths.windowless, StagedCLI: paths.stagedCLI,
		StagedWindowless: paths.stagedWindowless, ReceiptPath: paths.receipt,
		Mode: WindowsProductPairModeCanonicalize, Tasks: tasks, Deps: deps,
	}}).Run(context.Background())
	if err == nil || !errors.Is(err, ErrWindowsProductPairCommittedObservabilityFailed) {
		t.Fatalf("error=%v, want committed observability failure", err)
	}
	if result.Outcome != WindowsProductPairCommittedObservabilityFailed || result.ReceiptSHA256 == "" {
		t.Fatalf("result=%+v", result)
	}
	if tasks.restored {
		t.Fatal("receipt-authoritative committed pair was rolled back after event failure")
	}
	assertPairFile(t, paths.priorCLI, []byte("new-cli"))
	if _, decodeErr := DecodeUpgradeReceipt(mustReadPairFile(t, paths.receipt)); decodeErr != nil {
		t.Fatalf("authoritative receipt missing after event failure: %v", decodeErr)
	}
}

func TestWindowsProductPairCommitReconciliationIsReceiptBoundAndIdempotent(t *testing.T) {
	pair := binaryadmission.WindowsProductPair{
		CLI:        binaryadmission.WindowsArtifact{Role: binaryadmission.WindowsArtifactRoleCLI, Version: "0.4.36", Commit: "abc1234", BuildDate: "2026-09-01T00:00:00Z", SHA256: "cli-sha"},
		Windowless: binaryadmission.WindowsArtifact{Role: binaryadmission.WindowsArtifactRoleWindowless, Version: "0.4.36", Commit: "abc1234", BuildDate: "2026-09-01T00:00:00Z", SHA256: "windowless-sha"},
	}
	receipt := UpgradeReceiptV2{Schema: UpgradeReceiptSchemaV2, Admission: UpgradeAdmissionLocalProduct, Version: pair.CLI.Version, Commit: pair.CLI.Commit, BuildDate: pair.CLI.BuildDate, Artifacts: pair, InstalledAt: "2026-09-01T00:00:01Z"}
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
}

func TestWindowsProductPairTxnCleanupFailureAfterReceiptIsCommittedAndNeverRollsBack(t *testing.T) {
	dir := t.TempDir()
	paths := productPairFixturePaths(dir)
	writePairFixture(t, paths.priorCLI, []byte("old-cli"))
	writePairFixture(t, paths.stagedCLI, []byte("new-cli"))
	writePairFixture(t, paths.stagedWindowless, []byte("new-windowless"))
	var order []string
	tasks := &productPairTaskFake{order: &order, closeErr: errors.New("injected task owner cleanup failure")}
	result, err := (WindowsProductPairTxn{Opts: WindowsProductPairTxnOpts{
		CLIPath: paths.priorCLI, WindowlessPath: paths.windowless, StagedCLI: paths.stagedCLI,
		StagedWindowless: paths.stagedWindowless, ReceiptPath: paths.receipt,
		Mode: WindowsProductPairModeSetup, Tasks: tasks, Deps: productPairTestDeps(&order),
	}}).Run(context.Background())
	if !errors.Is(err, ErrWindowsProductPairCommittedCleanupFailed) || result.Outcome != WindowsProductPairCommittedCleanupFailed || result.ReceiptSHA256 == "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if tasks.restored {
		t.Fatal("committed pair rolled back after post-receipt cleanup failure")
	}
	assertPairFile(t, paths.priorCLI, []byte("new-cli"))
}

func TestWindowsProductPairTxnSweepsExactAdmittedTargetsAfterCommit(t *testing.T) {
	dir := t.TempDir()
	paths := productPairFixturePaths(dir)
	paths.priorCLI = filepath.Join(dir, "terminal-product.bin")
	paths.windowless = filepath.Join(dir, "explorer-product.payload")
	writePairFixture(t, paths.priorCLI, []byte("old-cli"))
	writePairFixture(t, paths.stagedCLI, []byte("new-cli"))
	writePairFixture(t, paths.stagedWindowless, []byte("new-windowless"))
	var order []string
	tasks := &productPairTaskFake{order: &order}
	deps := productPairTestDeps(&order)
	var swept []string
	deps.SweepOldTargets = func(targets []string, _ ...func(string, error)) error {
		swept = append([]string(nil), targets...)
		return nil
	}
	if _, err := (WindowsProductPairTxn{Opts: WindowsProductPairTxnOpts{CLIPath: paths.priorCLI, WindowlessPath: paths.windowless, StagedCLI: paths.stagedCLI, StagedWindowless: paths.stagedWindowless, ReceiptPath: paths.receipt, Mode: WindowsProductPairModeUpgrade, Tasks: tasks, Deps: deps}}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(swept, []string{paths.priorCLI, paths.windowless}) {
		t.Fatalf("swept targets=%v", swept)
	}
}

func TestWindowsProductPairRollbackCleanupFailureStillRestartsPrior(t *testing.T) {
	dir := t.TempDir()
	paths := productPairFixturePaths(dir)
	prior := []byte("old-cli")
	writePairFixture(t, paths.priorCLI, prior)
	writePairFixture(t, paths.stagedCLI, []byte("new-cli"))
	writePairFixture(t, paths.stagedWindowless, []byte("new-windowless"))
	var order []string
	tasks := &productPairTaskFake{order: &order}
	deps := productPairTestDeps(&order)
	restarts := 0
	deps.RestartPrior = func(string) error { restarts++; return nil }
	deps.RestorePromotion = func(p WindowsProductPairPromotion) (string, error) {
		body, err := os.ReadFile(p.RetainedPrior)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(p.Target, body, 0o600); err != nil {
			return "", err
		}
		return p.Target + ".not-a-generated-aside", nil
	}
	deps.Fault = func(stage WindowsProductPairStage) error {
		if stage == WindowsProductPairStageCLIPromoted {
			return errors.New("force rollback")
		}
		return nil
	}
	_, err := (WindowsProductPairTxn{Opts: WindowsProductPairTxnOpts{CLIPath: paths.priorCLI, WindowlessPath: paths.windowless, StagedCLI: paths.stagedCLI, StagedWindowless: paths.stagedWindowless, ReceiptPath: paths.receipt, Mode: WindowsProductPairModeUpgrade, Tasks: tasks, Deps: deps}}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "displaced successor is not an exact generated aside") {
		t.Fatalf("err=%v", err)
	}
	if restarts != 1 {
		t.Fatalf("prior restarts=%d, want 1", restarts)
	}
	assertPairFile(t, paths.priorCLI, prior)
}

func TestWindowsProductPairTxnRollsBackPartialPromotionResult(t *testing.T) {
	for _, tc := range []struct {
		name string
		role string
	}{
		{name: "windowless", role: "windowless"},
		{name: "cli", role: "cli"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			paths := productPairFixturePaths(dir)
			oldCLI, oldWindowless := []byte("old-cli"), []byte("old-windowless")
			writePairFixture(t, paths.priorCLI, oldCLI)
			writePairFixture(t, paths.windowless, oldWindowless)
			writePairFixture(t, paths.stagedCLI, []byte("new-cli"))
			writePairFixture(t, paths.stagedWindowless, []byte("new-windowless"))
			var order []string
			tasks := &productPairTaskFake{order: &order}
			deps := productPairTestDeps(&order)
			basePromote := deps.Promote
			deps.Promote = func(src, dst, sha string) (WindowsProductPairPromotion, error) {
				promotion, err := basePromote(src, dst, sha)
				if err != nil {
					return promotion, err
				}
				if (tc.role == "windowless" && dst == paths.windowless) || (tc.role == "cli" && dst == paths.priorCLI) {
					return promotion, errors.New("injected partial promotion after replace")
				}
				return promotion, nil
			}
			restarts := 0
			deps.RestartPrior = func(string) error { restarts++; return nil }
			_, err := (WindowsProductPairTxn{Opts: WindowsProductPairTxnOpts{CLIPath: paths.priorCLI, WindowlessPath: paths.windowless, StagedCLI: paths.stagedCLI, StagedWindowless: paths.stagedWindowless, ReceiptPath: paths.receipt, Mode: WindowsProductPairModeUpgrade, Tasks: tasks, Deps: deps}}).Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "injected partial promotion") {
				t.Fatalf("err=%v", err)
			}
			assertPairFile(t, paths.priorCLI, oldCLI)
			assertPairFile(t, paths.windowless, oldWindowless)
			if restarts != 1 {
				t.Fatalf("prior restarts=%d, want 1", restarts)
			}
		})
	}
}

func TestRunInstallUpgradeDoesNotOuterRecoverPostPromotionPairRollback(t *testing.T) {
	dir := t.TempDir()
	paths := productPairFixturePaths(dir)
	writePairFixture(t, paths.priorCLI, []byte("old-cli"))
	writePairFixture(t, paths.stagedCLI, []byte("new-cli"))
	writePairFixture(t, paths.stagedWindowless, []byte("new-windowless"))
	var order []string
	tasks := &productPairTaskFake{order: &order}
	deps := productPairTestDeps(&order)
	pairRestarts := 0
	deps.RestartPrior = func(string) error { pairRestarts++; return nil }
	deps.Fault = func(stage WindowsProductPairStage) error {
		if stage == WindowsProductPairStageCLIPromoted {
			return errors.New("post-promotion fault")
		}
		return nil
	}
	txn := &WindowsProductPairTxn{Opts: WindowsProductPairTxnOpts{CLIPath: paths.priorCLI, WindowlessPath: paths.windowless, StagedCLI: paths.stagedCLI, StagedWindowless: paths.stagedWindowless, ReceiptPath: paths.receipt, Mode: WindowsProductPairModeUpgrade, Tasks: tasks, Deps: deps}}
	legacy := &fakeUpgradeDeps{quiesceResult: api.IPCResponse{ID: 1, OK: true, Result: map[string]any{"still_running": []any{}}, Final: true}, exitResult: api.IPCResponse{ID: 2, OK: true, Final: true}}
	err := RunInstallUpgrade(context.Background(), UpgradeOpts{WindowsProductPair: txn, BinaryPath: paths.priorCLI, PipePath: "pair-postpromotion", Deps: legacy, WaitSupervisorLockReleased: func(context.Context, time.Duration) error { return nil }})
	if err == nil {
		t.Fatal("post-promotion failure unexpectedly succeeded")
	}
	if pairRestarts != 1 || legacy.startCalled {
		t.Fatalf("pair restarts=%d outer starts=%v calls=%v", pairRestarts, legacy.startCalled, legacy.calls)
	}
}

func TestWindowsProductPairDefaultPromotionRestoresMappedImage(t *testing.T) {
	if marker := os.Getenv("MCPHUB_PAIR_MAPPED_HELPER"); marker != "" {
		if err := os.WriteFile(marker, []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Second)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prior, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "mcphub.exe")
	staged := filepath.Join(dir, "mcphub.exe.stage")
	marker := filepath.Join(dir, "mapped-ready")
	if err := os.WriteFile(target, prior, 0o755); err != nil {
		t.Fatal(err)
	}
	newBytes := append(bytes.Clone(prior), []byte("new-image-marker")...)
	if err := os.WriteFile(staged, newBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(target, "-test.run=^TestWindowsProductPairDefaultPromotionRestoresMappedImage$")
	child.Env = append(os.Environ(), "MCPHUB_PAIR_MAPPED_HELPER="+marker)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("mapped-image helper did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	deps := withWindowsProductPairDefaults(WindowsProductPairTxnDeps{})
	newSHA := fmt.Sprintf("%x", sha256.Sum256(newBytes))
	promotion, err := deps.Promote(staged, target, newSHA)
	if err != nil {
		t.Fatalf("promote over mapped image: %v", err)
	}
	assertPairFile(t, promotion.RetainedPrior, prior)
	assertPairFile(t, target, newBytes)
	if _, err := deps.RestorePromotion(promotion); err != nil {
		t.Fatalf("restore mapped prior through rename-aside: %v", err)
	}
	assertPairFile(t, target, prior)
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

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
)

type productPairTaskFake struct {
	order    *[]string
	restored bool
	closed   bool
}

func (f *productPairTaskFake) InventoryExport() error {
	*f.order = append(*f.order, "tasks-snapshot")
	return nil
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
func (f *productPairTaskFake) Close() error { f.closed = true; return nil }

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
		Version: "0.4.36", Commit: "abc123", BuildDate: "2026-09-01", Tasks: tasks, Deps: deps,
	}}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if committed.Schema != WindowsProductPairCommittedSchemaV1 {
		t.Fatalf("commit schema=%q", committed.Schema)
	}
	wantOrder := []string{"admit-staged", "tasks-snapshot", "promote-windowless", "readback-windowless", "tasks-rewrite", "tasks-readback", "promote-cli", "readback-pair", "start-supervisor", "supervisor-ready", "committed", "receipt"}
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
		WindowsProductPairStageCommitted,
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
				Version: "0.4.36", Commit: "abc123", BuildDate: "2026-09-01", Tasks: tasks, Deps: deps,
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
		Version: "0.4.36", Commit: "abc123", BuildDate: "2026-09-01", Tasks: tasks, Deps: deps,
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
			Version:     "0.4.36", Commit: "abc123", BuildDate: "2026-09-01",
			Tasks: &productPairTaskFake{order: &order}, Deps: productPairTestDeps(&order),
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
				CLI:        binaryadmission.WindowsArtifact{Path: cli.Path, Role: binaryadmission.WindowsArtifactRoleCLI, Version: cli.Version, Commit: cli.Commit, BuildDate: cli.BuildDate, SHA256: "cli-sha"},
				Windowless: binaryadmission.WindowsArtifact{Path: windowless.Path, Role: binaryadmission.WindowsArtifactRoleWindowless, Version: windowless.Version, Commit: windowless.Commit, BuildDate: windowless.BuildDate, SHA256: "windowless-sha"},
			}, nil
		},
		Promote: func(src, dst string) error {
			if filepath.Base(dst) == "mcphub-windowless.exe" {
				*order = append(*order, "promote-windowless")
			} else {
				*order = append(*order, "promote-cli")
			}
			body, err := os.ReadFile(src)
			if err != nil {
				return err
			}
			return writeProductPairFileAtomic(dst, body, 0o755)
		},
		ReadBackWindowless: func(string, binaryadmission.WindowsArtifact) error {
			*order = append(*order, "readback-windowless")
			return nil
		},
		StartSupervisor: func(string) error { *order = append(*order, "start-supervisor"); return nil },
		WaitSupervisorReady: func(context.Context, string, binaryadmission.WindowsProductPair) error {
			*order = append(*order, "supervisor-ready")
			return nil
		},
		OnCommitted: func(WindowsProductPairCommitted) error { *order = append(*order, "committed"); return nil },
		WriteReceipt: func(path string, raw []byte) error {
			*order = append(*order, "receipt")
			return writeProductPairFileAtomic(path, raw, 0o600)
		},
		Now: func() time.Time { return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) },
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

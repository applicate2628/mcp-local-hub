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
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
)

type recoveryTaskTxnFake struct {
	order        *[]string
	restoreCalls int
	verifyCalls  int
	closeCalls   int
}

func (*recoveryTaskTxnFake) InventoryExport() error { return nil }
func (*recoveryTaskTxnFake) RecoveryDescriptor() (WindowsProductPairTaskRecovery, error) {
	return WindowsProductPairTaskRecovery{}, nil
}
func (*recoveryTaskTxnFake) RewriteCommand() error  { return nil }
func (f *recoveryTaskTxnFake) VerifyRuntime() error { f.verifyCalls++; return nil }
func (f *recoveryTaskTxnFake) RestoreImport() error {
	f.restoreCalls++
	if f.order != nil {
		*f.order = append(*f.order, "restore-tasks")
	}
	return nil
}
func (*recoveryTaskTxnFake) DiscardRecoveryStore() error { return nil }
func (f *recoveryTaskTxnFake) Close() error              { f.closeCalls++; return nil }

type recoveryFixture struct {
	stateDir, cli, windowless, receipt, taskStore string
	priorCLI, priorWindowless, priorReceipt       []byte
	newCLI, newWindowless                         []byte
	journal                                       windowsProductPairRecoveryJournal
}

func newRecoveryFixture(t *testing.T) recoveryFixture {
	t.Helper()
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	installDir := filepath.Join(root, "install")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(installDir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := recoveryFixture{
		stateDir:   stateDir,
		cli:        filepath.Join(installDir, "mcphub.exe"),
		windowless: filepath.Join(installDir, "mcphub-windowless.exe"),
		receipt:    filepath.Join(stateDir, UpgradeReceiptSchemaV2+".json"),
		taskStore:  filepath.Join(stateDir, "windows-product-pair-task-rollback-fixture"),
		priorCLI:   []byte("prior-cli"), priorWindowless: []byte("prior-windowless"), priorReceipt: []byte("prior-receipt\n"),
		newCLI: []byte("successor-cli"), newWindowless: []byte("successor-windowless"),
	}
	for path, body := range map[string][]byte{f.cli: f.priorCLI, f.windowless: f.priorWindowless, f.receipt: f.priorReceipt} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(f.taskStore, 0o700); err != nil {
		t.Fatal(err)
	}
	f.journal = windowsProductPairRecoveryJournal{
		Schema:        WindowsProductPairRecoverySchemaV1,
		TransactionID: strings.Repeat("a", 32),
		Phase:         WindowsProductPairRecoveryPrepared,
		Mode:          WindowsProductPairModeUpgrade,
		CLI:           windowsProductPairRecoveryArtifact{Target: f.cli, PriorPresent: true, PriorSHA256: recoveryTestSHA(f.priorCLI), NewSHA256: recoveryTestSHA(f.newCLI)},
		Windowless:    windowsProductPairRecoveryArtifact{Target: f.windowless, PriorPresent: true, PriorSHA256: recoveryTestSHA(f.priorWindowless), NewSHA256: recoveryTestSHA(f.newWindowless)},
		Receipt:       windowsProductPairRecoveryReceipt{Path: f.receipt, PriorPresent: true, PriorBytes: append([]byte(nil), f.priorReceipt...)},
		Tasks:         WindowsProductPairTaskRecovery{StoreRoot: f.taskStore},
		Staged: windowsProductPairRecoveryStaged{
			CLIPath: filepath.Join(installDir, "mcphub.exe.stage"), CLISHA256: recoveryTestSHA(f.newCLI),
			WindowlessPath: filepath.Join(installDir, "mcphub-windowless.exe.stage"), WindowlessSHA256: recoveryTestSHA(f.newWindowless),
		},
	}
	return f
}

func recoveryTestSHA(body []byte) string { return fmt.Sprintf("%x", sha256.Sum256(body)) }

func (f recoveryFixture) persistJournal(t *testing.T) {
	t.Helper()
	raw, err := json.MarshalIndent(f.journal, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.WriteStateFileBytesAtomic(filepath.Join(f.stateDir, WindowsProductPairRecoveryFileLeaf), append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
}

func recoveryAsidePath(target string, second int) string {
	return target + fmt.Sprintf(".old-20260908T1735%02dZ", second)
}

func moveRecoveryPriorAside(t *testing.T, target string, second int) string {
	t.Helper()
	aside := recoveryAsidePath(target, second)
	if err := os.Rename(target, aside); err != nil {
		t.Fatal(err)
	}
	return aside
}

func promoteRecoveryFixture(t *testing.T, target string, successor []byte, second int) {
	t.Helper()
	moveRecoveryPriorAside(t, target, second)
	if err := os.WriteFile(target, successor, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f recoveryFixture) request(tasks *recoveryTaskTxnFake, order *[]string, eventErr error) windowsProductPairRecoveryRequest {
	tasks.order = order
	defaults := withWindowsProductPairDefaults(WindowsProductPairTxnDeps{})
	return windowsProductPairRecoveryRequest{
		Context: context.Background(), StateDir: f.stateDir, CLIPath: f.cli, WindowlessPath: f.windowless,
		SettleSuccessor: func(string) error { *order = append(*order, "settle-successor"); return nil },
		ObservePriorReady: func(context.Context, string, string) (bool, error) {
			*order = append(*order, "observe-prior")
			return false, nil
		},
		RestartPrior:   func(string) error { *order = append(*order, "restart-prior"); return nil },
		WaitPriorReady: func(context.Context, string, string) error { *order = append(*order, "wait-prior"); return nil },
		ReconcileCommitted: func(windowsProductPairRecoveryJournal) error {
			*order = append(*order, "reconcile-committed")
			return eventErr
		},
		Deps: windowsProductPairRecoveryDeps{
			ReopenTasks: func(context.Context, WindowsProductPairTaskRecovery) (WindowsProductPairTaskTxn, error) {
				*order = append(*order, "reopen-tasks")
				return tasks, nil
			},
			DiscardTaskStore: func(WindowsProductPairTaskRecovery) error {
				*order = append(*order, "discard-store")
				if err := os.Remove(f.taskStore); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				return nil
			},
			AdmitPair: func(cli, windowless binaryadmission.WindowsArtifact) (binaryadmission.WindowsProductPair, error) {
				return binaryadmission.WindowsProductPair{
					CLI:        binaryadmission.WindowsArtifact{Path: cli.Path, Role: binaryadmission.WindowsArtifactRoleCLI, Version: "0.4.36", Commit: "fixture", BuildDate: "2026-09-08T00:00:00Z", SHA256: f.journal.CLI.NewSHA256},
					Windowless: binaryadmission.WindowsArtifact{Path: windowless.Path, Role: binaryadmission.WindowsArtifactRoleWindowless, Version: "0.4.36", Commit: "fixture", BuildDate: "2026-09-08T00:00:00Z", SHA256: f.journal.Windowless.NewSHA256},
				}, nil
			},
			RestorePromotion: func(p WindowsProductPairPromotion) (string, error) {
				if sameWindowsRecoveryPath(p.Target, f.cli) {
					*order = append(*order, "restore-cli")
				} else {
					*order = append(*order, "restore-windowless")
				}
				return defaults.RestorePromotion(p)
			},
			RestoreNoReplace: func(source, target string) error {
				if sameWindowsRecoveryPath(target, f.cli) {
					*order = append(*order, "restore-cli")
				} else {
					*order = append(*order, "restore-windowless")
				}
				return moveWindowsFileNoReplace(source, target)
			},
			WriteReceipt: func(path string, raw []byte) error {
				*order = append(*order, "restore-receipt")
				return api.WriteStateFileBytesAtomic(path, raw)
			},
			RemoveReceipt: func(path string) error {
				*order = append(*order, "restore-receipt")
				return removeRecoveryLeaf(path)
			},
			RemoveExact: func(path string) error {
				if sameWindowsRecoveryPath(path, f.cli) {
					*order = append(*order, "restore-cli")
				} else if sameWindowsRecoveryPath(path, f.windowless) {
					*order = append(*order, "restore-windowless")
				}
				return os.Remove(path)
			},
		},
	}
}

func TestWindowsProductPairRecoveryPriorAbsentCrashMatrix(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, recoveryFixture)
	}{
		{name: "prepared before first promotion"},
		{name: "windowless promoted", mutate: func(t *testing.T, f recoveryFixture) {
			if err := os.WriteFile(f.windowless, f.newWindowless, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "both promoted", mutate: func(t *testing.T, f recoveryFixture) {
			if err := os.WriteFile(f.windowless, f.newWindowless, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.cli, f.newCLI, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			for _, path := range []string{f.cli, f.windowless, f.receipt} {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			f.journal.CLI.PriorPresent, f.journal.CLI.PriorSHA256 = false, ""
			f.journal.Windowless.PriorPresent, f.journal.Windowless.PriorSHA256 = false, ""
			f.journal.Receipt.PriorPresent, f.journal.Receipt.PriorBytes = false, nil
			f.persistJournal(t)
			if tc.mutate != nil {
				tc.mutate(t, f)
			}
			var order []string
			tasks := &recoveryTaskTxnFake{}
			settled, err := reconcileWindowsProductPairRecovery(f.request(tasks, &order, nil))
			if err != nil || settled == nil || settled.Outcome != WindowsProductPairRecoverySettledRollback {
				t.Fatalf("settled=%+v err=%v order=%v", settled, err, order)
			}
			for _, path := range []string{f.cli, f.windowless, f.receipt} {
				if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("prior-absent path %s exists: %v", path, statErr)
				}
			}
			if tc.name == "both promoted" && !recoveryStepsOrdered(order, []string{"restore-cli", "restore-tasks", "restore-windowless", "restore-receipt"}) {
				t.Fatalf("prior-absent reverse order=%v", order)
			}
		})
	}
}

func TestWindowsProductPairRecoveryIncompleteReceiptRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(*testing.T, recoveryFixture)
	}{
		{name: "missing", write: func(t *testing.T, f recoveryFixture) {
			if err := os.Remove(f.receipt); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "old"},
		{name: "partial atomic payload", write: func(t *testing.T, f recoveryFixture) {
			if err := api.WriteStateFileBytesAtomic(f.receipt, []byte(`{"schema":"upgrade-receipt-v2"`)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			promoteRecoveryFixture(t, f.windowless, f.newWindowless, 16)
			promoteRecoveryFixture(t, f.cli, f.newCLI, 17)
			if tc.write != nil {
				tc.write(t, f)
			}
			f.persistJournal(t)
			var order []string
			tasks := &recoveryTaskTxnFake{}
			settled, err := reconcileWindowsProductPairRecovery(f.request(tasks, &order, nil))
			if err != nil || settled == nil || settled.Outcome != WindowsProductPairRecoverySettledRollback {
				t.Fatalf("settled=%+v err=%v order=%v", settled, err, order)
			}
			if containsRecoveryStep(order, "reconcile-committed") || tasks.restoreCalls != 1 {
				t.Fatalf("incomplete receipt committed: tasks=%+v order=%v", tasks, order)
			}
			if got, readErr := os.ReadFile(f.receipt); readErr != nil || !bytes.Equal(got, f.priorReceipt) {
				t.Fatalf("receipt=%q err=%v", got, readErr)
			}
		})
	}
}

func TestWindowsProductPairRecoveryFreshOwnerRollbackCrashMatrix(t *testing.T) {
	tests := []struct {
		name             string
		phase            WindowsProductPairRecoveryPhase
		priorFleetReaped bool
		mutate           func(*testing.T, recoveryFixture)
	}{
		{name: "after prepared before first rename"},
		{name: "during windowless target to old", mutate: func(t *testing.T, f recoveryFixture) { moveRecoveryPriorAside(t, f.windowless, 1) }},
		{name: "after windowless promotion", mutate: func(t *testing.T, f recoveryFixture) { promoteRecoveryFixture(t, f.windowless, f.newWindowless, 2) }},
		{name: "during task rewrite", mutate: func(t *testing.T, f recoveryFixture) { promoteRecoveryFixture(t, f.windowless, f.newWindowless, 3) }},
		{name: "during CLI target to old", mutate: func(t *testing.T, f recoveryFixture) {
			promoteRecoveryFixture(t, f.windowless, f.newWindowless, 4)
			moveRecoveryPriorAside(t, f.cli, 5)
		}},
		{name: "after both promotions before successor", mutate: func(t *testing.T, f recoveryFixture) {
			promoteRecoveryFixture(t, f.windowless, f.newWindowless, 6)
			promoteRecoveryFixture(t, f.cli, f.newCLI, 7)
		}},
		{name: "after durable successor starting", phase: WindowsProductPairRecoverySuccessorStarting, priorFleetReaped: true, mutate: func(t *testing.T, f recoveryFixture) {
			promoteRecoveryFixture(t, f.windowless, f.newWindowless, 8)
			promoteRecoveryFixture(t, f.cli, f.newCLI, 9)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			if tc.phase != "" {
				f.journal.Phase = tc.phase
			}
			f.journal.PriorFleetReaped = tc.priorFleetReaped
			f.persistJournal(t)
			if tc.mutate != nil {
				tc.mutate(t, f)
			}
			var order []string
			tasks := &recoveryTaskTxnFake{}
			settled, err := reconcileWindowsProductPairRecovery(f.request(tasks, &order, nil))
			if err != nil {
				t.Fatalf("fresh-owner recovery: %v; order=%v", err, order)
			}
			if settled == nil || settled.Outcome != WindowsProductPairRecoverySettledRollback {
				t.Fatalf("settled=%+v", settled)
			}
			for path, want := range map[string][]byte{f.cli: f.priorCLI, f.windowless: f.priorWindowless, f.receipt: f.priorReceipt} {
				got, readErr := os.ReadFile(path)
				if readErr != nil || !bytes.Equal(got, want) {
					t.Fatalf("restored %s=%q err=%v want=%q", filepath.Base(path), got, readErr, want)
				}
			}
			if tasks.restoreCalls != 1 || tasks.closeCalls != 1 {
				t.Fatalf("task restore=%d close=%d order=%v", tasks.restoreCalls, tasks.closeCalls, order)
			}
			if _, statErr := os.Stat(filepath.Join(f.stateDir, WindowsProductPairRecoveryFileLeaf)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("journal survived settlement: %v", statErr)
			}
			if tc.priorFleetReaped {
				want := []string{"settle-successor", "reopen-tasks", "observe-prior", "restart-prior", "wait-prior", "discard-store"}
				for _, step := range want {
					if !containsRecoveryStep(order, step) {
						t.Fatalf("missing %s in %v", step, order)
					}
				}
			} else if containsRecoveryStep(order, "restart-prior") {
				t.Fatalf("non-upgrade recovery restarted prior: %v", order)
			}
			if tc.name == "after both promotions before successor" || tc.name == "after durable successor starting" {
				wantOrder := []string{"restore-cli", "restore-tasks", "restore-windowless", "restore-receipt"}
				if !recoveryStepsOrdered(order, wantOrder) {
					t.Fatalf("reverse order=%v want subsequence=%v", order, wantOrder)
				}
			}
		})
	}
}

func TestWindowsProductPairRecoveryMatchingReceiptCommitsWithoutRollback(t *testing.T) {
	f := newRecoveryFixture(t)
	promoteRecoveryFixture(t, f.windowless, f.newWindowless, 10)
	promoteRecoveryFixture(t, f.cli, f.newCLI, 11)
	pair, err := f.request(&recoveryTaskTxnFake{}, new([]string), nil).Deps.AdmitPair(binaryadmission.WindowsArtifact{Path: f.cli}, binaryadmission.WindowsArtifact{Path: f.windowless})
	if err != nil {
		t.Fatal(err)
	}
	receipt := UpgradeReceiptV2{Schema: UpgradeReceiptSchemaV2, Admission: UpgradeAdmissionLocalProduct, Version: pair.CLI.Version, Commit: pair.CLI.Commit, BuildDate: pair.CLI.BuildDate, Artifacts: pair, InstalledAt: "2026-09-08T00:00:00Z"}
	raw, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.WriteStateFileBytesAtomic(f.receipt, append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
	f.persistJournal(t)
	var order []string
	tasks := &recoveryTaskTxnFake{}
	settled, err := reconcileWindowsProductPairRecovery(f.request(tasks, &order, nil))
	if err != nil {
		t.Fatalf("commit recovery: %v order=%v", err, order)
	}
	if settled == nil || settled.Outcome != WindowsProductPairRecoverySettledCommit {
		t.Fatalf("settled=%+v", settled)
	}
	if tasks.restoreCalls != 0 || tasks.verifyCalls != 1 || containsRecoveryStep(order, "restart-prior") || !containsRecoveryStep(order, "reconcile-committed") {
		t.Fatalf("commit polarity violated: tasks=%+v order=%v", tasks, order)
	}
	if got, _ := os.ReadFile(f.cli); !bytes.Equal(got, f.newCLI) {
		t.Fatalf("committed CLI rolled back: %q", got)
	}
}

func TestWindowsProductPairRecoveryCommittedEventFailureNeverRollsBack(t *testing.T) {
	f := newRecoveryFixture(t)
	promoteRecoveryFixture(t, f.windowless, f.newWindowless, 12)
	promoteRecoveryFixture(t, f.cli, f.newCLI, 13)
	pair, _ := f.request(&recoveryTaskTxnFake{}, new([]string), nil).Deps.AdmitPair(binaryadmission.WindowsArtifact{Path: f.cli}, binaryadmission.WindowsArtifact{Path: f.windowless})
	receipt := UpgradeReceiptV2{Schema: UpgradeReceiptSchemaV2, Admission: UpgradeAdmissionLocalProduct, Version: pair.CLI.Version, Commit: pair.CLI.Commit, BuildDate: pair.CLI.BuildDate, Artifacts: pair, InstalledAt: "2026-09-08T00:00:00Z"}
	raw, _ := json.MarshalIndent(receipt, "", "  ")
	if err := api.WriteStateFileBytesAtomic(f.receipt, append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
	f.persistJournal(t)
	var order []string
	tasks := &recoveryTaskTxnFake{}
	eventErr := fmt.Errorf("%w: fixture", ErrWindowsProductPairCommittedObservabilityFailed)
	settled, err := reconcileWindowsProductPairRecovery(f.request(tasks, &order, eventErr))
	if !errors.Is(err, ErrWindowsProductPairCommittedObservabilityFailed) || settled == nil || settled.Outcome != WindowsProductPairRecoverySettledCommit {
		t.Fatalf("settled=%+v err=%v", settled, err)
	}
	if tasks.restoreCalls != 0 || containsRecoveryStep(order, "restart-prior") {
		t.Fatalf("event failure rolled back: tasks=%+v order=%v", tasks, order)
	}
	if _, statErr := os.Stat(filepath.Join(f.stateDir, WindowsProductPairRecoveryFileLeaf)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("settled journal survived event failure: %v", statErr)
	}
}

func TestWindowsProductPairRecoveryRefusesAmbiguousPostJournalAside(t *testing.T) {
	f := newRecoveryFixture(t)
	baseline := recoveryAsidePath(f.cli, 14)
	if err := os.WriteFile(baseline, f.priorCLI, 0o600); err != nil {
		t.Fatal(err)
	}
	f.journal.CLI.BaselineAsideBasenames = []string{filepath.Base(baseline)}
	f.persistJournal(t)
	extra := recoveryAsidePath(f.cli, 15)
	if err := os.WriteFile(extra, f.priorCLI, 0o600); err != nil {
		t.Fatal(err)
	}
	var order []string
	tasks := &recoveryTaskTxnFake{}
	settled, err := reconcileWindowsProductPairRecovery(f.request(tasks, &order, nil))
	if settled != nil || !errors.Is(err, ErrWindowsProductPairRecoveryIdentity) {
		t.Fatalf("settled=%+v err=%v", settled, err)
	}
	if tasks.restoreCalls != 0 || len(order) != 0 {
		t.Fatalf("identity refusal mutated state: tasks=%+v order=%v", tasks, order)
	}
	if got, _ := os.ReadFile(f.cli); !bytes.Equal(got, f.priorCLI) {
		t.Fatalf("canonical CLI changed: %q", got)
	}
}

func TestWindowsProductPairRecoverySettledCleanupRetriesWithoutMutation(t *testing.T) {
	f := newRecoveryFixture(t)
	f.journal.Phase = WindowsProductPairRecoverySettledRollback
	f.persistJournal(t)
	var order []string
	removeCalls := 0
	req := f.request(&recoveryTaskTxnFake{}, &order, nil)
	req.Deps.RemoveJournal = func(path string) error {
		removeCalls++
		if removeCalls == 1 {
			return errors.New("injected remove failure")
		}
		return removeRecoveryLeaf(path)
	}
	req.Deps.ReopenTasks = func(context.Context, WindowsProductPairTaskRecovery) (WindowsProductPairTaskTxn, error) {
		return nil, errors.New("settled cleanup must not reopen tasks")
	}
	if _, err := reconcileWindowsProductPairRecovery(req); !errors.Is(err, ErrWindowsProductPairRecoveryCleanup) {
		t.Fatalf("first cleanup err=%v", err)
	}
	settled, err := reconcileWindowsProductPairRecovery(req)
	if err != nil || settled == nil || settled.Outcome != WindowsProductPairRecoverySettledRollback {
		t.Fatalf("retry settled=%+v err=%v", settled, err)
	}
	if containsRecoveryStep(order, "reopen-tasks") || containsRecoveryStep(order, "restart-prior") {
		t.Fatalf("cleanup replayed mutation: %v", order)
	}
}

func TestWindowsProductPairRecoveryWiringReopensExistingSchedulerStore(t *testing.T) {
	withNoopSchedulerEnv(t)
	f := newRecoveryFixture(t)
	if err := os.Remove(f.taskStore); err != nil {
		t.Fatal(err)
	}
	owner, err := newWindowsProductPairTxn(windowsProductPairTxnRequest{
		Context: context.Background(), StateDir: f.stateDir, CLIPath: f.cli, WindowlessPath: f.windowless,
		StagedCLI: f.journal.Staged.CLIPath, StagedWindowless: f.journal.Staged.WindowlessPath, Mode: WindowsProductPairModeSetup,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Opts.Tasks.InventoryExport(); err != nil {
		t.Fatal(err)
	}
	recovery, err := owner.Opts.Tasks.RecoveryDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Opts.Tasks.Close(); err != nil {
		t.Fatal(err)
	}
	f.journal.Tasks = recovery
	f.persistJournal(t)
	settled, err := reconcileWiredWindowsProductPairRecovery(windowsProductPairRecoveryRequest{
		Context: context.Background(), StateDir: f.stateDir, CLIPath: f.cli, WindowlessPath: f.windowless,
		ReconcileCommitted: func(windowsProductPairRecoveryJournal) error { return nil },
	})
	if err != nil || settled == nil || settled.Outcome != WindowsProductPairRecoverySettledRollback {
		t.Fatalf("settled=%+v err=%v", settled, err)
	}
	if _, statErr := os.Stat(recovery.StoreRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("retained store survived recovery: %v", statErr)
	}
}

func TestWindowsProductPairSetupAndCanonicalizeFenceBeforeRecoveryOrMutation(t *testing.T) {
	stateDir := withTempStateDir(t)
	holder, err := api.AcquireUpgradeFence(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Release() })
	originalFactory := newWindowsProductPairTxnFn
	t.Cleanup(func() { newWindowsProductPairTxnFn = originalFactory })
	called := false
	newWindowsProductPairTxnFn = func(windowsProductPairTxnRequest) (*WindowsProductPairTxn, error) {
		called = true
		return nil, errors.New("unreachable")
	}
	for _, run := range []struct {
		name string
		fn   func() error
	}{
		{name: "setup", fn: func() error {
			return bootstrapProductToTarget(new(bytes.Buffer), filepath.Join(t.TempDir(), "source.exe"), filepath.Join(t.TempDir(), "mcphub.exe"))
		}},
		{name: "canonicalize", fn: func() error {
			return canonicalizeProductToTarget(new(bytes.Buffer), filepath.Join(t.TempDir(), "source.exe"), filepath.Join(t.TempDir(), "mcphub.exe"))
		}},
	} {
		t.Run(run.name, func(t *testing.T) {
			if err := run.fn(); err == nil || !strings.Contains(err.Error(), "another product-pair transaction is active") {
				t.Fatalf("error=%v", err)
			}
		})
	}
	if called {
		t.Fatal("pair transaction factory ran behind held fence")
	}
}

const windowsProductPairCrashChildEnv = "MCPHUB_WINDOWS_PAIR_CRASH_CHILD"

type windowsProductPairCrashChildTasks struct {
	storeRoot string
	crashAt   string
}

func (*windowsProductPairCrashChildTasks) InventoryExport() error { return nil }
func (t *windowsProductPairCrashChildTasks) RecoveryDescriptor() (WindowsProductPairTaskRecovery, error) {
	return WindowsProductPairTaskRecovery{StoreRoot: t.storeRoot}, nil
}
func (t *windowsProductPairCrashChildTasks) RewriteCommand() error {
	if t.crashAt == "during-tasks-rewrite" {
		os.Exit(86)
	}
	return nil
}
func (*windowsProductPairCrashChildTasks) VerifyRuntime() error { return nil }
func (*windowsProductPairCrashChildTasks) RestoreImport() error { return nil }
func (t *windowsProductPairCrashChildTasks) DiscardRecoveryStore() error {
	if err := os.Remove(t.storeRoot); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
func (*windowsProductPairCrashChildTasks) Close() error { return nil }

func TestWindowsProductPairCrashChild(t *testing.T) {
	root := os.Getenv(windowsProductPairCrashChildEnv)
	if root == "" {
		return
	}
	crashAt := os.Getenv(windowsProductPairCrashChildEnv + "_POINT")
	stateDir := filepath.Join(root, "state")
	installDir := filepath.Join(root, "install")
	cliPath := filepath.Join(installDir, "mcphub.exe")
	windowlessPath := filepath.Join(installDir, "mcphub-windowless.exe")
	stagedCLI := filepath.Join(installDir, "mcphub.exe.stage")
	stagedWindowless := filepath.Join(installDir, "mcphub-windowless.exe.stage")
	tasks := &windowsProductPairCrashChildTasks{storeRoot: filepath.Join(stateDir, "windows-product-pair-task-rollback-fixture"), crashAt: crashAt}
	promote := func(src, dst, newSHA string) (WindowsProductPairPromotion, error) {
		prior, err := os.ReadFile(dst)
		if err != nil {
			return WindowsProductPairPromotion{}, err
		}
		second := 40
		if sameWindowsRecoveryPath(dst, cliPath) {
			second = 41
		}
		aside := recoveryAsidePath(dst, second)
		if err := os.Rename(dst, aside); err != nil {
			return WindowsProductPairPromotion{}, err
		}
		if (crashAt == "during-windowless-target-to-old" && sameWindowsRecoveryPath(dst, windowlessPath)) || (crashAt == "during-cli-target-to-old" && sameWindowsRecoveryPath(dst, cliPath)) {
			os.Exit(86)
		}
		if err := os.Rename(src, dst); err != nil {
			return WindowsProductPairPromotion{Target: dst, RetainedPrior: aside, PriorPresent: true, NewSHA256: newSHA}, err
		}
		if recoveryTestSHA(prior) == "" {
			return WindowsProductPairPromotion{}, errors.New("unreachable prior hash")
		}
		return WindowsProductPairPromotion{Target: dst, RetainedPrior: aside, PriorPresent: true, NewSHA256: newSHA}, nil
	}
	admit := func(cli, windowless binaryadmission.WindowsArtifact) (binaryadmission.WindowsProductPair, error) {
		return binaryadmission.WindowsProductPair{
			CLI:        binaryadmission.WindowsArtifact{Path: cli.Path, Role: binaryadmission.WindowsArtifactRoleCLI, Version: "0.4.36", Commit: "fixture", BuildDate: "2026-09-08T00:00:00Z", SHA256: recoveryTestSHA([]byte("successor-cli"))},
			Windowless: binaryadmission.WindowsArtifact{Path: windowless.Path, Role: binaryadmission.WindowsArtifactRoleWindowless, Version: "0.4.36", Commit: "fixture", BuildDate: "2026-09-08T00:00:00Z", SHA256: recoveryTestSHA([]byte("successor-windowless"))},
		}, nil
	}
	deps := WindowsProductPairTxnDeps{
		AdmitPair:          admit,
		Promote:            promote,
		ReadBackWindowless: func(string, binaryadmission.WindowsArtifact) error { return nil },
		StartSupervisor: func(string) error {
			if crashAt == "after-successor-starting" {
				os.Exit(86)
			}
			return nil
		},
		WaitSupervisorReady: func(context.Context, string, binaryadmission.WindowsProductPair) error { return nil },
		RestartPrior:        func(string) error { return nil },
		SettleSuccessor:     func(string) error { return nil },
		PublishCommitted:    func(WindowsProductPairCommitEvent) (string, error) { return "fixture", nil },
		Fault: func(stage WindowsProductPairStage) error {
			if crashAt == "after-windowless-readback" && stage == WindowsProductPairStageWindowlessReadBack {
				os.Exit(86)
			}
			if crashAt == "after-complete-receipt" && stage == WindowsProductPairStageReceiptWritten {
				os.Exit(86)
			}
			return nil
		},
	}
	if crashAt == "during-receipt-write" {
		deps.WriteReceipt = func(path string, _ []byte) error {
			if err := api.WriteStateFileBytesAtomic(path, []byte(`{"schema":"upgrade-receipt-v2"`)); err != nil {
				return err
			}
			os.Exit(86)
			return nil
		}
	}
	if crashAt == "settled-commit-cleanup" {
		deps.RemoveRecoveryJournal = func(string) error { os.Exit(86); return nil }
	}
	_, err := (WindowsProductPairTxn{Opts: WindowsProductPairTxnOpts{
		StateDir: stateDir, CLIPath: cliPath, WindowlessPath: windowlessPath,
		StagedCLI: stagedCLI, StagedWindowless: stagedWindowless, ReceiptPath: filepath.Join(stateDir, UpgradeReceiptSchemaV2+".json"),
		Mode: WindowsProductPairModeUpgrade, PriorFleetReaped: true, Tasks: tasks, Deps: deps,
	}}).Run(context.Background())
	t.Fatalf("crash child returned instead of exiting at %q: %v", crashAt, err)
}

func TestWindowsProductPairRecoveryRealProcessDeathFreshOwnerMatrix(t *testing.T) {
	for _, tc := range []struct {
		point   string
		outcome WindowsProductPairRecoveryPhase
	}{
		{point: "during-windowless-target-to-old", outcome: WindowsProductPairRecoverySettledRollback},
		{point: "after-windowless-readback", outcome: WindowsProductPairRecoverySettledRollback},
		{point: "during-tasks-rewrite", outcome: WindowsProductPairRecoverySettledRollback},
		{point: "during-cli-target-to-old", outcome: WindowsProductPairRecoverySettledRollback},
		{point: "after-successor-starting", outcome: WindowsProductPairRecoverySettledRollback},
		{point: "during-receipt-write", outcome: WindowsProductPairRecoverySettledRollback},
		{point: "after-complete-receipt", outcome: WindowsProductPairRecoverySettledCommit},
		{point: "settled-commit-cleanup", outcome: WindowsProductPairRecoverySettledCommit},
	} {
		t.Run(tc.point, func(t *testing.T) {
			f := newRecoveryFixture(t)
			for path, body := range map[string][]byte{f.journal.Staged.CLIPath: f.newCLI, f.journal.Staged.WindowlessPath: f.newWindowless} {
				if err := os.WriteFile(path, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsProductPairCrashChild$")
			cmd.Env = append(os.Environ(), windowsProductPairCrashChildEnv+"="+filepath.Dir(f.stateDir), windowsProductPairCrashChildEnv+"_POINT="+tc.point)
			err := cmd.Run()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
				t.Fatalf("child error=%v", err)
			}
			var order []string
			tasks := &recoveryTaskTxnFake{}
			settled, recoverErr := reconcileWindowsProductPairRecovery(f.request(tasks, &order, nil))
			if recoverErr != nil || settled == nil || settled.Outcome != tc.outcome {
				t.Fatalf("settled=%+v err=%v order=%v", settled, recoverErr, order)
			}
			if tc.outcome == WindowsProductPairRecoverySettledCommit {
				if tasks.restoreCalls != 0 || containsRecoveryStep(order, "restart-prior") {
					t.Fatalf("committed crash rolled back: tasks=%+v order=%v", tasks, order)
				}
			} else {
				if got, _ := os.ReadFile(f.cli); !bytes.Equal(got, f.priorCLI) {
					t.Fatalf("CLI=%q", got)
				}
				if got, _ := os.ReadFile(f.windowless); !bytes.Equal(got, f.priorWindowless) {
					t.Fatalf("windowless=%q", got)
				}
			}
		})
	}
}

func containsRecoveryStep(order []string, want string) bool {
	for _, step := range order {
		if step == want {
			return true
		}
	}
	return false
}

func recoveryStepsOrdered(order, want []string) bool {
	next := 0
	for _, step := range order {
		if next < len(want) && step == want[next] {
			next++
		}
	}
	return next == len(want)
}

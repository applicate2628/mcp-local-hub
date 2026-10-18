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
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/gui"
)

func committedRecoveryFixture(t *testing.T) (productRecoveryV3Fixture, string, upgradeCommittedRecoveryDeps) {
	t.Helper()
	f := newProductRecoveryV3Fixture(t, WindowsProductRecoveryCommitDecided)
	f.journal.Mode = WindowsProductPairModeUpgrade
	f.journal.PreSettlement = &windowsProductPreSettlementV3{
		Supervisor: windowsProductSupervisorSettlementV1{State: windowsProductSupervisorGracefullySettled,
			Generation: &windowsProductSupervisorGenerationV1{PID: 103, StartedAt: "2026-09-29T00:00:00Z"}},
		GUIOwner: windowsProductGUIOwnerConfirmedPresent,
	}
	receipt, err := mintWindowsProductQuiescenceReceipt(f.product, f.legacy,
		windowsProductPairPreflightV1{PriorCLI: windowsProductPreimageV1{Present: true, SHA256: f.journal.Product.PriorSHA256}, PriorWindowless: windowsProductPreimageV1{Present: true, SHA256: f.journal.LegacyWindowless.PriorSHA256}},
		windowsProductQuiescenceObservationV1{Supervisor: f.journal.PreSettlement.Supervisor, GUIOwner: windowsProductGUIOwnerConfirmedAbsent, SupervisorLockReleased: true},
		func() time.Time { return time.Date(2026, 9, 29, 0, 0, 1, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	projection, digest, err := projectWindowsProductRecoveryQuiescence(receipt)
	if err != nil {
		t.Fatal(err)
	}
	f.journal.Quiescence, f.journal.QuiescenceReceiptSHA256, f.journal.PriorFleetReaped = &projection, digest, true
	writeCommittedRecoveryJournal(t, f)

	executor := filepath.Join(filepath.Dir(f.product), "recovery", "mcphub.exe")
	if err := os.MkdirAll(filepath.Dir(executor), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executor, []byte("distinct-recovery-product"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash := func(path string) string {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%x", sha256.Sum256(raw))
	}
	canonicalHash, executorHash := hash(f.product), hash(executor)
	if canonicalHash == executorHash {
		t.Fatal("fixture executor must differ from canonical")
	}
	artifact := UpgradeReceiptArtifactV3{
		Role: binaryadmission.WindowsArtifactRoleProduct, Subsystem: binaryadmission.WindowsGUISubsystem,
		Machine: 0x8664, Version: "0.4.37", Commit: strings.Repeat("c", 40),
		BuildDate: "2026-09-29T00:00:00Z", SHA256: canonicalHash,
	}
	committedReceipt := UpgradeReceiptV3{
		Schema: UpgradeReceiptSchemaV3, TransactionID: f.journal.TransactionID,
		Mode: WindowsProductPairModeUpgrade, Admission: UpgradeAdmissionLocalProductV3,
		Artifact: artifact, TaskSetSHA256: f.journal.TaskSetSHA256,
		Retired:     UpgradeReceiptRetiredV3{WindowlessAbsent: true, AdmissionHelperAbsentFromPayload: true, RollbackImagesAbsent: true},
		InstalledAt: "2026-09-29T00:00:00Z",
	}
	raw, err := json.Marshal(committedReceipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.receiptV3, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := map[string]any{
		"schema": "windows-product-identity-v1", "role": "product", "filename": "mcphub.exe",
		"subsystem": 2, "machine": 0x8664, "version": "0.4.37",
		"commit": strings.Repeat("d", 40), "build_date": "2026-09-29T01:00:00Z", "sha256": executorHash,
	}
	raw, err = json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(executor), "windows-product-identity.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	deps := upgradeCommittedRecoveryDeps{
		AdmitProduct: func(in binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error) {
			if in.Role != binaryadmission.WindowsArtifactRoleProduct {
				return binaryadmission.WindowsArtifact{}, errors.New("wrong role")
			}
			meta := artifact
			if in.Path == executor {
				meta.Commit, meta.BuildDate, meta.SHA256 = strings.Repeat("d", 40), "2026-09-29T01:00:00Z", executorHash
			} else if in.Path != f.product {
				return binaryadmission.WindowsArtifact{}, errors.New("unexpected path")
			}
			return binaryadmission.WindowsArtifact{Path: in.Path, Role: in.Role, Machine: meta.Machine,
				Subsystem: meta.Subsystem, Version: meta.Version, Commit: meta.Commit,
				BuildDate: meta.BuildDate, SHA256: meta.SHA256}, nil
		},
		BuildInfo: func() (string, string, string) { return "0.4.37", strings.Repeat("d", 40), "2026-09-29T01:00:00Z" },
	}
	return f, executor, deps
}

func writeCommittedRecoveryJournal(t *testing.T, f productRecoveryV3Fixture) {
	t.Helper()
	if err := persistWindowsProductRecoveryJournalV3(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3), f.journal,
		api.WriteStateFileBytesAtomic, api.ReadStateFileInodeAnchored); err != nil {
		t.Fatal(err)
	}
}

func committedRecoveryCommand() (*cobra.Command, *bytes.Buffer) {
	out := &bytes.Buffer{}
	cmd := &cobra.Command{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	return cmd, out
}

func TestCommittedRecoveryExternalDifferentHashSettlesOnlyJournaledTransaction(t *testing.T) {
	f, executor, deps := committedRecoveryFixture(t)
	calls := 0
	deps.Recover = func(req windowsProductRecoveryRequest) (*WindowsProductPairRecoverySettled, error) {
		calls++
		if req.StateDir != f.stateDir || req.ProductPath != f.product || req.LegacyWindowlessPath != f.legacy {
			t.Fatal("wrong recovery target")
		}
		if err := os.Remove(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)); err != nil {
			t.Fatal(err)
		}
		return &WindowsProductPairRecoverySettled{TransactionID: f.journal.TransactionID, Outcome: WindowsProductPairRecoverySettledCommit}, nil
	}
	cmd, out := committedRecoveryCommand()
	if err := runCommittedRecoveryWindowsWithDeps(cmd, f.stateDir, executor, f.product, deps); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !strings.Contains(out.String(), "UPGRADE_COMMITTED_RECOVERY_SETTLED") {
		t.Fatalf("calls=%d output=%q", calls, out.String())
	}
	if got, err := hashRecoveryFile(f.product); err != nil || got != f.journal.Product.NewSHA256 {
		t.Fatalf("canonical changed: %s %v", got, err)
	}
	cmd, _ = committedRecoveryCommand()
	if err := runCommittedRecoveryWindowsWithDeps(cmd, f.stateDir, executor, f.product, deps); err == nil || !strings.Contains(err.Error(), "UPGRADE_COMMITTED_RECOVERY_NOT_ADMITTED") || calls != 1 {
		t.Fatalf("repeat error=%v calls=%d", err, calls)
	}
}

func TestCommittedRecoveryRejectsNonCommittedInputsWithoutOwnerCall(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *productRecoveryV3Fixture, *upgradeCommittedRecoveryDeps)
	}{
		{"no-journal", func(t *testing.T, f *productRecoveryV3Fixture, _ *upgradeCommittedRecoveryDeps) {
			if err := os.Remove(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)); err != nil {
				t.Fatal(err)
			}
		}},
		{"predecision", func(t *testing.T, f *productRecoveryV3Fixture, _ *upgradeCommittedRecoveryDeps) {
			f.journal.Phase = WindowsProductRecoveryGUIOwnedStopPending
			f.journal.TaskSetSHA256 = ""
			f.journal.Quiescence = nil
			f.journal.QuiescenceReceiptSHA256 = ""
			f.journal.PriorFleetReaped = false
			writeCommittedRecoveryJournal(t, *f)
		}},
		{"wrong-mode", func(t *testing.T, f *productRecoveryV3Fixture, _ *upgradeCommittedRecoveryDeps) {
			f.journal.Mode = WindowsProductPairModeCanonicalize
			f.journal.PreSettlement = nil
			f.journal.Quiescence = nil
			f.journal.QuiescenceReceiptSHA256 = ""
			f.journal.PriorFleetReaped = false
			writeCommittedRecoveryJournal(t, *f)
		}},
		{"wrong-executor-version", func(_ *testing.T, _ *productRecoveryV3Fixture, d *upgradeCommittedRecoveryDeps) {
			d.BuildInfo = func() (string, string, string) { return "0.4.38", strings.Repeat("d", 40), "2026-09-29T01:00:00Z" }
		}},
		{"legacy-coexists", func(t *testing.T, f *productRecoveryV3Fixture, _ *upgradeCommittedRecoveryDeps) {
			if err := os.WriteFile(filepath.Join(f.stateDir, WindowsProductPairRecoveryFileLeaf), []byte("legacy"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong-executor-sidecar", func(t *testing.T, _ *productRecoveryV3Fixture, d *upgradeCommittedRecoveryDeps) {
			base := d.AdmitProduct
			d.AdmitProduct = func(in binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error) {
				admitted, err := base(in)
				if strings.EqualFold(filepath.Base(in.Path), "mcphub.exe") && strings.Contains(in.Path, "recovery") {
					admitted.Role = binaryadmission.WindowsArtifactRoleCLI
				}
				return admitted, err
			}
		}},
		{"corrupt-journal", func(t *testing.T, f *productRecoveryV3Fixture, _ *upgradeCommittedRecoveryDeps) {
			if err := os.WriteFile(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, executor, deps := committedRecoveryFixture(t)
			calls := 0
			deps.Recover = func(windowsProductRecoveryRequest) (*WindowsProductPairRecoverySettled, error) {
				calls++
				return nil, nil
			}
			tc.mutate(t, &f, &deps)
			cmd, _ := committedRecoveryCommand()
			if err := runCommittedRecoveryWindowsWithDeps(cmd, f.stateDir, executor, f.product, deps); err == nil || calls != 0 {
				t.Fatalf("error=%v recovery calls=%d", err, calls)
			}
		})
	}
}

func TestCommittedRecoveryPendingKeepsJournalAndOmitsCompletion(t *testing.T) {
	f, executor, deps := committedRecoveryFixture(t)
	deps.Recover = func(windowsProductRecoveryRequest) (*WindowsProductPairRecoverySettled, error) {
		return nil, fmt.Errorf("%w: GUI health", ErrWindowsProductRecoveryPending)
	}
	cmd, out := committedRecoveryCommand()
	err := runCommittedRecoveryWindowsWithDeps(cmd, f.stateDir, executor, f.product, deps)
	if !errors.Is(err, ErrWindowsProductRecoveryPending) || strings.Contains(out.String(), "UPGRADE_COMMITTED_RECOVERY_SETTLED") {
		t.Fatalf("error=%v output=%q", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)); err != nil {
		t.Fatalf("pending journal lost: %v", err)
	}
}

func TestCommittedRecoveryGUIAlreadyPresentReportsLastPredicateWithoutPrivateValues(t *testing.T) {
	f, _, _ := committedRecoveryFixture(t)
	prior := findRunningGUIsOnTargetFn
	t.Cleanup(func() { findRunningGUIsOnTargetFn = prior })
	findRunningGUIsOnTargetFn = func(string) ([]api.ProcessInfo, error) { return []api.ProcessInfo{{PID: 123}}, nil }
	inner := errors.New("UPGRADE_GUI_IDENTITY_AMBIGUOUS: nested UPGRADE_GUI_TASK_DRIFT; private path C:\\private\\task.xml")
	releases := 0
	err := restoreGUIOwnedForwardWithObserver(context.Background(), f.journal, func() error {
		releases++
		return nil
	}, func(context.Context) error {
		return inner
	})
	var health *upgradeGUIForwardHealthError
	if !errors.Is(err, ErrWindowsProductRecoveryPending) || !errors.Is(err, inner) ||
		!errors.As(err, &health) || health.predicate != "UPGRADE_GUI_IDENTITY_AMBIGUOUS" ||
		!strings.Contains(err.Error(), "UPGRADE_GUI_IDENTITY_AMBIGUOUS") || strings.Contains(err.Error(), "UPGRADE_GUI_TASK_DRIFT") ||
		strings.Contains(err.Error(), "private") || releases != 0 {
		t.Fatalf("GUI-present failure = %v", err)
	}
}

func TestCommittedRecoverySlowHealthTimeoutReportsLastPredicateWithoutPrivateValues(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	inner := errors.New("UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN: nested UPGRADE_GUI_TASK_DRIFT; private SID S-1-5-21")
	err := waitUpgradeGUIForwardHealth(ctx, func() error {
		return inner
	})
	var health *upgradeGUIForwardHealthError
	if !errors.Is(err, ErrWindowsProductRecoveryPending) || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, inner) ||
		!errors.As(err, &health) || health.predicate != "UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN" ||
		!strings.Contains(err.Error(), "UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN") || strings.Contains(err.Error(), "UPGRADE_GUI_TASK_DRIFT") ||
		strings.Contains(err.Error(), "S-1-5-21") {
		t.Fatalf("timeout failure = %v", err)
	}
}

type emptyForwardHealthMultiError struct{}

func (emptyForwardHealthMultiError) Error() string   { return "" }
func (emptyForwardHealthMultiError) Unwrap() []error { return nil }

func TestUpgradeGUIForwardHealthPredicateUsesFinalImmediateOuterError(t *testing.T) {
	pair := errors.New("UPGRADE_GUI_PAIR_TOPOLOGY_UNPROVEN: nested UPGRADE_GUI_TASK_DRIFT")
	independent := errors.New("UPGRADE_GUI_IDENTITY_AMBIGUOUS: nested UPGRADE_GUI_TASK_DRIFT")
	unknown := errors.New("UPGRADE_GUI_UNKNOWN: nested UPGRADE_GUI_TASK_DRIFT")
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"outer-beats-nested", independent, "UPGRADE_GUI_IDENTITY_AMBIGUOUS"},
		{"suffix-is-not-exact", errors.New("UPGRADE_GUI_TASK_DRIFT_SUFFIX: private"), "GUI_FORWARD_HEALTH_UNCLASSIFIED"},
		{"missing-delimiter", errors.New("UPGRADE_GUI_TASK_DRIFT"), "GUI_FORWARD_HEALTH_UNCLASSIFIED"},
		{"unknown-beats-nested", unknown, "GUI_FORWARD_HEALTH_UNCLASSIFIED"},
		{"empty", errors.New(""), "GUI_FORWARD_HEALTH_UNCLASSIFIED"},
		{"joined-final-known", errors.Join(pair, independent), "UPGRADE_GUI_IDENTITY_AMBIGUOUS"},
		{"joined-final-unknown", errors.Join(pair, unknown), "GUI_FORWARD_HEALTH_UNCLASSIFIED"},
		{"nil", nil, "NONE"},
		{"joined-nil", errors.Join(nil, nil), "NONE"},
		{"non-nil-empty-multi-error", emptyForwardHealthMultiError{}, "GUI_FORWARD_HEALTH_UNCLASSIFIED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := upgradeGUIForwardHealthPredicate(tc.err); got != tc.want {
				t.Fatalf("predicate=%q, want=%q", got, tc.want)
			}
		})
	}
	limit, cancel := context.WithCancel(context.Background())
	cancel()
	healthErr := waitUpgradeGUIForwardHealth(limit, func() error { return errors.Join(pair, independent) })
	var health *upgradeGUIForwardHealthError
	if !errors.As(healthErr, &health) || health.predicate != "UPGRADE_GUI_IDENTITY_AMBIGUOUS" ||
		!errors.Is(healthErr, pair) || !errors.Is(healthErr, independent) || strings.Contains(healthErr.Error(), "TASK_DRIFT") {
		t.Fatalf("joined carrier failed: %v", healthErr)
	}
}

func TestCommittedRecoveryCommandIgnoresUntrustedPredicateTextAndIDs(t *testing.T) {
	private := "PRIVATE_USER_PATH_X\\SID-S-1-5-21\\argv-secret"
	for _, tc := range []struct {
		name, id, stage string
		cause           error
	}{
		{"text-spoof", "E_WINDOWS_PRODUCT_PAIR_COMMITTED_OBSERVABILITY_FAILED", "complete committed forward recovery",
			fmt.Errorf("%w: last_failed_predicate=UPGRADE_GUI_TASK_DRIFT %s", ErrWindowsProductPairCommittedObservabilityFailed, private)},
		{"unknown-id", "E_WINDOWS_PRODUCT_PAIR_COMMITTED_OBSERVABILITY_FAILED", "complete committed forward recovery",
			&upgradeGUIForwardHealthError{predicate: "UPGRADE_GUI_PRIVATE_" + private, summary: "forward health unconfirmed"}},
		{"none-id", "E_WINDOWS_PRODUCT_PAIR_COMMITTED_OBSERVABILITY_FAILED", "complete committed forward recovery",
			&upgradeGUIForwardHealthError{predicate: "NONE", summary: "forward health unconfirmed"}},
		{"missing-carrier", "E_WINDOWS_PRODUCT_PAIR_COMMITTED_OBSERVABILITY_FAILED", "complete committed forward recovery",
			(*upgradeGUIForwardHealthError)(nil)},
		{"wrong-stage", "E_WINDOWS_PRODUCT_PAIR_COMMITTED_OBSERVABILITY_FAILED", "admit recovery executor",
			&upgradeGUIForwardHealthError{predicate: "UPGRADE_GUI_TASK_DRIFT", summary: "forward health unconfirmed"}},
		{"wrong-id", "E_WINDOWS_PRODUCT_RECOVERY_PENDING", "complete committed forward recovery",
			&upgradeGUIForwardHealthError{predicate: "UPGRADE_GUI_TASK_DRIFT", summary: "forward health unconfirmed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := committedRecoveryCommandError(tc.id, tc.stage, tc.cause).Error()
			want := tc.id + ": " + tc.stage
			if got != want || strings.Contains(got, private) || strings.Contains(got, "last_failed_predicate") {
				t.Fatalf("public error=%q, want=%q", got, want)
			}
		})
	}
}

func TestCommittedRecoveryCommandProjectsWhitelistedForwardPredicate(t *testing.T) {
	f, executor, deps := committedRecoveryFixture(t)
	private := "PRIVATE_USER_PATH_X\\SID-S-1-5-21\\argv-secret"
	inner := errors.New("UPGRADE_GUI_IDENTITY_AMBIGUOUS: nested observer says UPGRADE_GUI_TASK_DRIFT; " + private)
	limit, cancel := context.WithCancel(context.Background())
	cancel()
	healthErr := waitUpgradeGUIForwardHealth(limit, func() error { return inner })
	txn, paths, _ := newProductTxnFixture(t)
	txn.Opts.Deps.PublishCommitted = func(WindowsProductCommitEvent) (string, error) {
		return "", healthErr
	}
	committed, ownerErr := txn.Run(context.Background())
	if committed.Outcome != WindowsProductPairCommittedObservabilityFailed || !errors.Is(ownerErr, ErrWindowsProductPairCommittedObservabilityFailed) {
		t.Fatalf("postdecision result: outcome=%s err=%v", committed.Outcome, ownerErr)
	}
	assertPairFile(t, paths.product, []byte("new-product"))
	deps.Recover = func(windowsProductRecoveryRequest) (*WindowsProductPairRecoverySettled, error) {
		return nil, ownerErr
	}
	cmd, out := committedRecoveryCommand()
	err := runCommittedRecoveryWindowsWithDeps(cmd, f.stateDir, executor, f.product, deps)
	want := "E_WINDOWS_PRODUCT_PAIR_COMMITTED_OBSERVABILITY_FAILED: complete committed forward recovery; last_failed_predicate=UPGRADE_GUI_IDENTITY_AMBIGUOUS"
	if err == nil || err.Error() != want || !errors.Is(err, inner) || out.Len() != 0 {
		t.Fatalf("public error=%v, want=%q; retained inner=%t; stdout=%q", err, want, errors.Is(err, inner), out.String())
	}
	if strings.Contains(err.Error(), private) {
		t.Fatalf("private observer data leaked: %q", err.Error())
	}
}

type committedRecoveryFenceFake struct {
	releaseErr error
	releases   int
}

func (f *committedRecoveryFenceFake) Release() error { f.releases++; return f.releaseErr }

func TestCommittedRecoveryFenceReleaseFailureSuppressesCompletion(t *testing.T) {
	f, executor, deps := committedRecoveryFixture(t)
	fence := &committedRecoveryFenceFake{releaseErr: errors.New("release unconfirmed")}
	deps.AcquireFence = func(context.Context, string) (upgradeCommittedFence, error) { return fence, nil }
	deps.Recover = func(windowsProductRecoveryRequest) (*WindowsProductPairRecoverySettled, error) {
		if err := os.Remove(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)); err != nil {
			t.Fatal(err)
		}
		return &WindowsProductPairRecoverySettled{TransactionID: f.journal.TransactionID, Outcome: WindowsProductPairRecoverySettledCommit}, nil
	}
	cmd, out := committedRecoveryCommand()
	err := runCommittedRecoveryWindowsWithDeps(cmd, f.stateDir, executor, f.product, deps)
	if err == nil || !strings.Contains(err.Error(), "UPGRADE_FENCE_RELEASE_UNCONFIRMED") || fence.releases != 1 || strings.Contains(out.String(), "UPGRADE_COMMITTED_RECOVERY_SETTLED") {
		t.Fatalf("error=%v releases=%d output=%q", err, fence.releases, out.String())
	}
}

func TestCommittedRecoveryReceiptAndCanonicalIdentityMismatchNeverCallsOwner(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, productRecoveryV3Fixture, *upgradeCommittedRecoveryDeps)
	}{
		{"canonical-hash", func(_ *testing.T, f productRecoveryV3Fixture, d *upgradeCommittedRecoveryDeps) {
			base := d.AdmitProduct
			d.AdmitProduct = func(in binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error) {
				admitted, err := base(in)
				if in.Path == f.product {
					admitted.SHA256 = strings.Repeat("0", 64)
				}
				return admitted, err
			}
		}},
		{"receipt-transaction", func(t *testing.T, f productRecoveryV3Fixture, _ *upgradeCommittedRecoveryDeps) {
			raw, err := os.ReadFile(f.receiptV3)
			if err != nil {
				t.Fatal(err)
			}
			var receipt UpgradeReceiptV3
			if err := json.Unmarshal(raw, &receipt); err != nil {
				t.Fatal(err)
			}
			receipt.TransactionID = strings.Repeat("b", 32)
			raw, err = json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.receiptV3, raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"receipt-task-set", func(t *testing.T, f productRecoveryV3Fixture, _ *upgradeCommittedRecoveryDeps) {
			raw, err := os.ReadFile(f.receiptV3)
			if err != nil {
				t.Fatal(err)
			}
			var receipt UpgradeReceiptV3
			if err := json.Unmarshal(raw, &receipt); err != nil {
				t.Fatal(err)
			}
			receipt.TaskSetSHA256 = strings.Repeat("0", 64)
			raw, err = json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.receiptV3, raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, executor, deps := committedRecoveryFixture(t)
			calls := 0
			deps.Recover = func(windowsProductRecoveryRequest) (*WindowsProductPairRecoverySettled, error) {
				calls++
				return nil, nil
			}
			tc.mutate(t, f, &deps)
			cmd, out := committedRecoveryCommand()
			err := runCommittedRecoveryWindowsWithDeps(cmd, f.stateDir, executor, f.product, deps)
			if !errors.Is(err, ErrWindowsProductRecoveryIdentity) || calls != 0 || out.Len() != 0 {
				t.Fatalf("error=%v calls=%d output=%q", err, calls, out.String())
			}
		})
	}
}

func TestCommittedRecoveryCommandRedactsExecutorAdmissionCause(t *testing.T) {
	f, executor, deps := committedRecoveryFixture(t)
	private := "PRIVATE_USER_PATH_X\\SID-S-1-5-21\\argv-secret"
	cause := errors.New(private)
	deps.AdmitProduct = func(binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error) {
		return binaryadmission.WindowsArtifact{}, fmt.Errorf("PE %s: %w", executor, cause)
	}
	deps.Recover = func(windowsProductRecoveryRequest) (*WindowsProductPairRecoverySettled, error) {
		t.Fatal("recovery owner called after failed executor admission")
		return nil, nil
	}
	prior := committedRecoveryFn
	t.Cleanup(func() { committedRecoveryFn = prior })
	committedRecoveryFn = func(cmd *cobra.Command) error {
		return runCommittedRecoveryWindowsWithDeps(cmd, f.stateDir, executor, f.product, deps)
	}
	root := NewRootCmd()
	root.SetArgs([]string{"upgrade", "--complete-committed-recovery"})
	out, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(stderr)
	root.SilenceUsage = true
	err := root.Execute()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
	} // cmd/mcphub composition-root rendering
	if err == nil || !errors.Is(err, cause) || !strings.Contains(err.Error(), "E_WINDOWS_PRODUCT_RECOVERY_IDENTITY") ||
		strings.Contains(err.Error(), private) || strings.Contains(err.Error(), executor) ||
		strings.Contains(stderr.String(), private) || strings.Contains(stderr.String(), executor) ||
		strings.Contains(out.String(), "UPGRADE_COMMITTED_RECOVERY_SETTLED") {
		t.Fatalf("returned=%v stderr=%q stdout=%q", err, stderr.String(), out.String())
	}
}

func TestCommittedRecoveryCommandRedactsOtherPathBearingFailures(t *testing.T) {
	for _, tc := range []struct {
		name, code, stage string
		mutate            func(*testing.T, productRecoveryV3Fixture, string, *upgradeCommittedRecoveryDeps, error)
	}{
		{"missing-journal", "UPGRADE_COMMITTED_RECOVERY_NOT_ADMITTED", "inspect committed journal", func(t *testing.T, f productRecoveryV3Fixture, _ string, _ *upgradeCommittedRecoveryDeps, _ error) {
			if err := os.Remove(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing-sidecar", "E_WINDOWS_PRODUCT_RECOVERY_IDENTITY", "admit recovery executor", func(t *testing.T, _ productRecoveryV3Fixture, executor string, _ *upgradeCommittedRecoveryDeps, _ error) {
			if err := os.Remove(filepath.Join(filepath.Dir(executor), "windows-product-identity.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing-receipt", "E_WINDOWS_PRODUCT_RECOVERY_IDENTITY", "verify committed Product targets and receipt", func(t *testing.T, f productRecoveryV3Fixture, _ string, _ *upgradeCommittedRecoveryDeps, _ error) {
			if err := os.Remove(f.receiptV3); err != nil {
				t.Fatal(err)
			}
		}},
		{"fence-acquire", "UPGRADE_COMMITTED_RECOVERY_FENCE_UNAVAILABLE", "acquire upgrade fence", func(_ *testing.T, _ productRecoveryV3Fixture, _ string, d *upgradeCommittedRecoveryDeps, cause error) {
			d.AcquireFence = func(context.Context, string) (upgradeCommittedFence, error) {
				return nil, fmt.Errorf("private fence path: %w", cause)
			}
		}},
		{"owner-pending", "E_WINDOWS_PRODUCT_RECOVERY_PENDING", "complete committed forward recovery", func(_ *testing.T, _ productRecoveryV3Fixture, _ string, d *upgradeCommittedRecoveryDeps, cause error) {
			d.Recover = func(windowsProductRecoveryRequest) (*WindowsProductPairRecoverySettled, error) {
				return nil, fmt.Errorf("%w: %w", ErrWindowsProductRecoveryPending, cause)
			}
		}},
		{"fence-release", "UPGRADE_FENCE_RELEASE_UNCONFIRMED", "release upgrade fence", func(t *testing.T, f productRecoveryV3Fixture, _ string, d *upgradeCommittedRecoveryDeps, cause error) {
			d.AcquireFence = func(context.Context, string) (upgradeCommittedFence, error) {
				return &committedRecoveryFenceFake{releaseErr: cause}, nil
			}
			d.Recover = func(windowsProductRecoveryRequest) (*WindowsProductPairRecoverySettled, error) {
				if err := os.Remove(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)); err != nil {
					t.Fatal(err)
				}
				return &WindowsProductPairRecoverySettled{TransactionID: f.journal.TransactionID, Outcome: WindowsProductPairRecoverySettledCommit}, nil
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, executor, deps := committedRecoveryFixture(t)
			private := "PRIVATE_USER_PATH_X\\SID-S-1-5-21\\argv-secret"
			cause := errors.New(private)
			deps.Recover = func(windowsProductRecoveryRequest) (*WindowsProductPairRecoverySettled, error) {
				t.Fatal("unexpected recovery owner call")
				return nil, nil
			}
			tc.mutate(t, f, executor, &deps, cause)
			cmd, out := committedRecoveryCommand()
			err := runCommittedRecoveryWindowsWithDeps(cmd, f.stateDir, executor, f.product, deps)
			stderr := &bytes.Buffer{}
			if err != nil {
				fmt.Fprintln(stderr, "error:", err)
			}
			if err == nil || !strings.Contains(err.Error(), tc.code) || !strings.Contains(err.Error(), tc.stage) ||
				strings.Contains(err.Error(), private) || strings.Contains(err.Error(), filepath.Dir(f.product)) ||
				strings.Contains(stderr.String(), private) || strings.Contains(stderr.String(), filepath.Dir(f.product)) ||
				strings.Contains(out.String(), "UPGRADE_COMMITTED_RECOVERY_SETTLED") {
				t.Fatalf("returned=%v stderr=%q stdout=%q", err, stderr.String(), out.String())
			}
			if tc.name == "fence-acquire" || tc.name == "owner-pending" || tc.name == "fence-release" {
				if !errors.Is(err, cause) {
					t.Fatalf("internal cause lost: %v", err)
				}
			}
			if tc.name == "missing-journal" || tc.name == "missing-sidecar" || tc.name == "missing-receipt" {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("underlying not-exist cause lost: %v", err)
				}
			}
			if tc.name == "missing-sidecar" || tc.name == "missing-receipt" {
				if !errors.Is(err, ErrWindowsProductRecoveryIdentity) {
					t.Fatalf("typed identity cause lost: %v", err)
				}
			}
			if tc.name == "missing-journal" && !errors.Is(err, ErrUpgradeCommittedRecoveryNotAdmitted) {
				t.Fatalf("typed no-journal cause lost: %v", err)
			}
		})
	}
}

func TestCommittedRecoveryTrailingSidecarSyntaxPreservesCause(t *testing.T) {
	f, executor, deps := committedRecoveryFixture(t)
	private := "PRIVATE_USER_PATH_X\\SID-S-1-5-21\\argv-secret"
	sidecar := filepath.Join(filepath.Dir(executor), "windows-product-identity.json")
	raw, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecar, append(raw, []byte("\n?"+private)...), 0o600); err != nil {
		t.Fatal(err)
	}
	deps.Recover = func(windowsProductRecoveryRequest) (*WindowsProductPairRecoverySettled, error) {
		t.Fatal("recovery owner called after malformed sidecar")
		return nil, nil
	}
	cmd, out := committedRecoveryCommand()
	err = runCommittedRecoveryWindowsWithDeps(cmd, f.stateDir, executor, f.product, deps)
	var syntax *json.SyntaxError
	stderr := &bytes.Buffer{}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
	}
	if err == nil || !errors.Is(err, ErrWindowsProductRecoveryIdentity) || !errors.As(err, &syntax) ||
		!strings.Contains(err.Error(), "E_WINDOWS_PRODUCT_RECOVERY_IDENTITY: admit recovery executor") ||
		strings.Contains(err.Error(), private) || strings.Contains(stderr.String(), private) ||
		strings.Contains(out.String(), "UPGRADE_COMMITTED_RECOVERY_SETTLED") {
		t.Fatalf("error=%v syntax=%v stderr=%q stdout=%q", err, syntax, stderr.String(), out.String())
	}
}

func TestCommittedRecoverySettledJournalProbePreservesOSError(t *testing.T) {
	f, executor, deps := committedRecoveryFixture(t)
	private := "PRIVATE_USER_PATH_X\\SID-S-1-5-21\\argv-secret"
	cause := &os.PathError{Op: "lstat", Path: private, Err: os.ErrPermission}
	fence := &committedRecoveryFenceFake{}
	deps.AcquireFence = func(context.Context, string) (upgradeCommittedFence, error) { return fence, nil }
	deps.LstatJournal = func(string) (os.FileInfo, error) { return nil, cause }
	deps.Recover = func(windowsProductRecoveryRequest) (*WindowsProductPairRecoverySettled, error) {
		if err := os.Remove(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)); err != nil {
			t.Fatal(err)
		}
		return &WindowsProductPairRecoverySettled{TransactionID: f.journal.TransactionID, Outcome: WindowsProductPairRecoverySettledCommit}, nil
	}
	cmd, out := committedRecoveryCommand()
	err := runCommittedRecoveryWindowsWithDeps(cmd, f.stateDir, executor, f.product, deps)
	stderr := &bytes.Buffer{}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
	}
	var pathErr *os.PathError
	if err == nil || !errors.Is(err, ErrWindowsProductRecoveryPending) || !errors.Is(err, os.ErrPermission) ||
		!errors.As(err, &pathErr) || pathErr != cause || fence.releases != 1 ||
		!strings.Contains(err.Error(), "E_WINDOWS_PRODUCT_RECOVERY_PENDING: verify settled journal removal") ||
		strings.Contains(err.Error(), private) || strings.Contains(stderr.String(), private) ||
		strings.Contains(out.String(), "UPGRADE_COMMITTED_RECOVERY_SETTLED") {
		t.Fatalf("error=%v pathErr=%v releases=%d stderr=%q stdout=%q", err, pathErr, fence.releases, stderr.String(), out.String())
	}
}

func TestPendingRecoveryResultsAndUnknownOwnerStayDistinctAndPrivate(t *testing.T) {
	private := "PRIVATE_PATH_X\\SID-S-1-5-21\\argv-secret\\<Task>PRIVATE_XML</Task>\\E_FAKE"
	for _, tc := range []struct {
		name   string
		result *WindowsProductPairRecoverySettled
		cause  error
		want   string
	}{
		{"missing", nil, nil, "RESULT_MISSING"},
		{"outcome", &WindowsProductPairRecoverySettled{Outcome: WindowsProductPairRecoverySettledCommit}, nil, "OUTCOME_DIFFERENT"},
		{"transaction", &WindowsProductPairRecoverySettled{Outcome: WindowsProductPairRecoverySettledRollback, TransactionID: private}, nil, "TRANSACTION_DIFFERENT"},
		{"unknown-owner", nil, fmt.Errorf("%w: %s", ErrWindowsProductRecoveryPending, private), "OWNER_ERROR_UNCLASSIFIED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, executor, deps := committedRecoveryFixture(t)
			f.journal.Phase = WindowsProductRecoverySuccessorStarting
			writeCommittedRecoveryJournal(t, f)
			fence := &committedRecoveryFenceFake{}
			deps.AcquireFence = func(context.Context, string) (upgradeCommittedFence, error) { return fence, nil }
			deps.ValidatePending = func(windowsProductRecoveryRequest, windowsProductRecoveryJournalV3) error { return nil }
			calls := 0
			deps.Recover = func(windowsProductRecoveryRequest) (*WindowsProductPairRecoverySettled, error) {
				calls++
				return tc.result, tc.cause
			}
			cmd, out := committedRecoveryCommand()
			err := runWindowsProductRecoveryCommandWithDeps(cmd, f.stateDir, executor, f.product, windowsRecoveryCommandPending, deps)
			if err == nil || calls != 1 || fence.releases != 1 || !errors.Is(err, ErrWindowsProductRecoveryPending) || err.Error() != "E_WINDOWS_PRODUCT_RECOVERY_PENDING: recover pending prior settlement; recovery_result="+tc.want || strings.Contains(err.Error(), private) || out.Len() != 0 {
				t.Fatalf("safe result receiving failed: %v", err)
			}
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Fatal("actual unknown owner error was lost")
			}
		})
	}
}

func TestPendingRecoveryPriorCauseSurvivesRollbackAndKeepsPublicID(t *testing.T) {
	f := newProductRecoveryV3Fixture(t, WindowsProductRecoverySuccessorStarting)
	f.journal.Mode = WindowsProductPairModeUpgrade
	f.journal.PreSettlement = &windowsProductPreSettlementV3{GUIOwner: windowsProductGUIOwnerConfirmedPresent}
	initial := errors.New("UPGRADE_GUI_TASK_DRIFT: PRIVATE_PATH_X nested E_FAKE")
	terminal := fmt.Errorf("%w: PRIVATE_PATH_X SID-S-1-5-21 argv-secret", ErrWindowsProductPairCommittedObservabilityFailed)
	ops, _, _ := guiPriorDiagnosticOpsFixture(t)
	ops.ObserveHealthy = func(context.Context, windowsProductRecoveryJournalV3) error { return initial }
	ops.Reservation = func(context.Context) (gui.OwnedSingleInstanceLease, error) { return nil, terminal }
	ownerErr := recoverGUIOwnedPriorSettlementWithOps(context.Background(), f.journal, ops)
	tasks := &recoveryTaskTxnFake{}
	req := f.request(tasks)
	req.RecoverPriorSettlement = func(context.Context, windowsProductRecoveryJournalV3) error { return ownerErr }
	settled, err := rollbackWindowsProductRecovery(req, withWindowsProductRecoveryDefaults(req.Deps), filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3), f.journal, tasks)
	var diagnostic *upgradeGUIPriorSettlementError
	if settled != nil || !errors.As(err, &diagnostic) || !errors.Is(err, terminal) || !errors.Is(err, initial) || !errors.Is(err, ErrWindowsProductPairCommittedObservabilityFailed) {
		t.Fatal("real rollback router dropped typed prior causes")
	}
	public := committedRecoveryCommandError(committedRecoveryOwnerFailureID(err), "recover pending prior settlement", err)
	if public.FailureID() != "E_WINDOWS_PRODUCT_RECOVERY_PENDING" || public.Error() != "E_WINDOWS_PRODUCT_RECOVERY_PENDING: recover pending prior settlement; recovery_result=OWNER_ERROR; failed_operation=RESERVATION; reservation_step=UNCLASSIFIED; initial_failed_predicate=UPGRADE_GUI_TASK_DRIFT" {
		t.Fatalf("child sentinel changed pending ID or private projection: %s", public.Error())
	}
}

func TestPendingRecoveryProjectionRefusesInjectedOperationAndText(t *testing.T) {
	private := "PRIVATE_PATH_X\\SID-S-1-5-21\\argv-secret\\E_FAKE"
	for _, cause := range []error{
		fmt.Errorf("%w: failed_operation=RESERVATION; %s", ErrWindowsProductRecoveryPending, private),
		&upgradeGUIPriorSettlementError{operation: upgradeGUIPriorOperation(private), cause: errors.New(private), initialHealth: errors.New("UPGRADE_GUI_TASK_DRIFT: " + private)},
	} {
		public := committedRecoveryCommandError("E_WINDOWS_PRODUCT_RECOVERY_PENDING", "recover pending prior settlement", cause)
		if public.Error() != "E_WINDOWS_PRODUCT_RECOVERY_PENDING: recover pending prior settlement; recovery_result=OWNER_ERROR_UNCLASSIFIED" || strings.Contains(public.Error(), private) {
			t.Fatal("untrusted operation/text crossed pending projection")
		}
		if !errors.Is(public, cause) {
			t.Fatal("redaction erased the original error")
		}
	}
}

func TestPendingRecoveryReservationSubstepsProjectOnlyActualClosedDetail(t *testing.T) {
	private := "PRIVATE_PATH_X\\SID-S-1-5-21\\argv-secret\\<Task>PRIVATE_XML</Task>"
	primary := errors.New(private)
	cleanup := errors.New("PRIVATE_RELEASE_X")
	for _, tc := range []struct {
		name                                  string
		firstFails, acquireFails, secondFails bool
		missingLease                          bool
		releaseErr                            error
		step                                  string
		cleanupSuffix                         string
	}{
		{name: "first", firstFails: true, step: "FIRST_ABSENCE"},
		{name: "acquire", acquireFails: true, step: "ACQUIRE_EXACT"},
		{name: "missing-lease", missingLease: true, step: "ACQUIRE_EXACT"},
		{name: "second", secondFails: true, step: "SECOND_ABSENCE"},
		{name: "second-release", secondFails: true, releaseErr: cleanup, step: "SECOND_ABSENCE", cleanupSuffix: "; reservation_cleanup=RELEASE_FAILED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks := 0
			reservationLease := &upgradeGUITestLease{releaseErr: tc.releaseErr}
			ops, _, _ := guiPriorDiagnosticOpsFixture(t)
			ops.ObserveHealthy = func(context.Context, windowsProductRecoveryJournalV3) error {
				return errors.New("UPGRADE_GUI_TASK_DRIFT: " + private)
			}
			ops.Reservation = func(ctx context.Context) (gui.OwnedSingleInstanceLease, error) {
				return acquireGUIOwnedPriorReservation(ctx, nil, guiOwnedPriorReservationOps{
					ProveNoGUIOrTray: func(context.Context) error {
						checks++
						if (checks == 1 && tc.firstFails) || (checks == 2 && tc.secondFails) {
							return primary
						}
						return nil
					},
					AcquireExact: func() (gui.OwnedSingleInstanceLease, error) {
						if tc.acquireFails {
							return nil, primary
						}
						if tc.missingLease {
							return nil, nil
						}
						return reservationLease, nil
					},
				})
			}
			journal := windowsProductRecoveryJournalV3{PreSettlement: &windowsProductPreSettlementV3{GUIOwner: windowsProductGUIOwnerConfirmedPresent}}
			ownerErr := recoverGUIOwnedPriorSettlementWithOps(context.Background(), journal, ops)
			public := committedRecoveryCommandError(committedRecoveryOwnerFailureID(ownerErr), "recover pending prior settlement", ownerErr)
			want := "E_WINDOWS_PRODUCT_RECOVERY_PENDING: recover pending prior settlement; recovery_result=OWNER_ERROR; failed_operation=RESERVATION; reservation_step=" + tc.step + tc.cleanupSuffix + "; initial_failed_predicate=UPGRADE_GUI_TASK_DRIFT"
			if public.Error() != want || !errors.Is(public, ErrWindowsProductRecoveryPending) || public.FailureID() != "E_WINDOWS_PRODUCT_RECOVERY_PENDING" {
				t.Fatal("actual reservation substep or original causal chain was lost")
			}
			if !tc.missingLease && !errors.Is(public, primary) {
				t.Fatal("original reservation primary cause was text-only")
			}
			var outer, inner *upgradeGUIPriorSettlementError
			if !errors.As(ownerErr, &outer) || !errors.As(outer.cause, &inner) {
				t.Fatal("original reservation carrier did not survive prior settlement wrapping")
			}
			if tc.releaseErr != nil {
				if inner.reservationRelease != cleanup || strings.Index(inner.cause.Error(), primary.Error()) > strings.Index(inner.cause.Error(), cleanup.Error()) {
					t.Fatal("actual release failure was lost or reordered before primary")
				}
			} else if inner.reservationRelease != nil {
				t.Fatal("cleanup failure was manufactured")
			}
			if tc.releaseErr != nil && !errors.Is(public, cleanup) {
				t.Fatal("actual cleanup cause was dropped")
			}
			if strings.Contains(public.Error(), private) || strings.Contains(public.Error(), "PRIVATE_RELEASE_X") || strings.Contains(fmt.Sprintf("%v %#v", ownerErr, ownerErr), private) {
				t.Fatal("private reservation detail escaped its closed projection")
			}
			wantReleases := 0
			if tc.secondFails {
				wantReleases = 1
			}
			if reservationLease.released != wantReleases {
				t.Fatal("diagnostics repeated or fabricated a release")
			}
		})
	}
}

func TestPendingRecoveryReservationProjectionRejectsInjectedStepAndCleanup(t *testing.T) {
	private := "PRIVATE_RESERVATION_STEP_X"
	cause := &upgradeGUIPriorSettlementError{operation: upgradeGUIPriorReservation, reservationStep: upgradeGUIPriorReservationStep(private), reservationRelease: errors.New(private), cause: errors.New(private)}
	public := committedRecoveryCommandError("E_WINDOWS_PRODUCT_RECOVERY_PENDING", "recover pending prior settlement", cause)
	want := "E_WINDOWS_PRODUCT_RECOVERY_PENDING: recover pending prior settlement; recovery_result=OWNER_ERROR; failed_operation=RESERVATION; reservation_step=UNCLASSIFIED"
	if public.Error() != want || strings.Contains(public.Error(), "RELEASE_FAILED") || strings.Contains(public.Error(), private) {
		t.Fatal("injected unknown step acquired a classified cleanup indication")
	}
}

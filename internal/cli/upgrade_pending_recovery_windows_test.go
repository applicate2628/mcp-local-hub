//go:build windows

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/gui"
	"mcp-local-hub/internal/process"
)

func pendingRecoveryFixture(t *testing.T, phase WindowsProductRecoveryPhase) (productRecoveryV3Fixture, string, upgradeCommittedRecoveryDeps) {
	f, executor, deps := committedRecoveryFixture(t)
	f.journal.Phase = phase
	if phase == WindowsProductRecoveryPrepared || phase == WindowsProductRecoveryGUIOwnedStopPending || phase == WindowsProductRecoverySettledRollback {
		f.journal.TaskSetSHA256 = ""
	}
	if phase == WindowsProductRecoveryPrepared {
		f.journal.PreSettlement = &windowsProductPreSettlementV3{Supervisor: windowsProductSupervisorSettlementV1{State: windowsProductSupervisorKnownAbsent}, GUIOwner: windowsProductGUIOwnerConfirmedAbsent, SupervisorLockReleased: true, Ports: []windowsProductPortSettlementV1{}}
		f.journal.PriorFleetReaped = false
		f.journal.Quiescence = nil
		f.journal.QuiescenceReceiptSHA256 = ""
	}
	if phase == WindowsProductRecoveryGUIOwnedStopPending {
		f.journal.PriorFleetReaped = false
		f.journal.Quiescence = nil
		f.journal.QuiescenceReceiptSHA256 = ""
	}
	writeCommittedRecoveryJournal(t, f)
	deps.ValidatePending = func(windowsProductRecoveryRequest, windowsProductRecoveryJournalV3) error { return nil }
	return f, executor, deps
}

func TestPendingRecoveryCommandAdmittedPhasesAndDifferentExecutor(t *testing.T) {
	for _, phase := range []WindowsProductRecoveryPhase{WindowsProductRecoveryPrepared, WindowsProductRecoveryGUIOwnedStopPending, WindowsProductRecoverySuccessorStarting, WindowsProductRecoverySettledRollback} {
		t.Run(string(phase), func(t *testing.T) {
			f, executor, deps := pendingRecoveryFixture(t, phase)
			calls := 0
			deps.Recover = func(req windowsProductRecoveryRequest) (*WindowsProductPairRecoverySettled, error) {
				calls++
				if !validLowerHexSHA256(req.ExpectedJournalSHA256) || !req.RequirePriorSettlementOnSettledRollback {
					t.Fatal("pending admission not propagated")
				}
				if err := os.Remove(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)); err != nil {
					return nil, err
				}
				return &WindowsProductPairRecoverySettled{TransactionID: f.journal.TransactionID, Outcome: WindowsProductPairRecoverySettledRollback}, nil
			}
			cmd, out := committedRecoveryCommand()
			if err := runWindowsProductRecoveryCommandWithDeps(cmd, f.stateDir, executor, f.product, windowsRecoveryCommandPending, deps); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || strings.Count(out.String(), "UPGRADE_PENDING_RECOVERY_SETTLED") != 1 || strings.Contains(out.String(), "COMMITTED_RECOVERY_SETTLED") {
				t.Fatalf("calls=%d out=%q", calls, out.String())
			}
		})
	}
}

func TestPendingRecoveryCommandRefusesForwardOrMissingAdmission(t *testing.T) {
	for _, phase := range []WindowsProductRecoveryPhase{WindowsProductRecoveryCommitDecided, WindowsProductRecoverySettledCommit} {
		t.Run(string(phase), func(t *testing.T) {
			f, executor, deps := committedRecoveryFixture(t)
			f.journal.Phase = phase
			writeCommittedRecoveryJournal(t, f)
			deps.Recover = func(windowsProductRecoveryRequest) (*WindowsProductPairRecoverySettled, error) {
				t.Fatal("pending command entered forward owner")
				return nil, nil
			}
			cmd, out := committedRecoveryCommand()
			if err := runWindowsProductRecoveryCommandWithDeps(cmd, f.stateDir, executor, f.product, windowsRecoveryCommandPending, deps); err == nil || !strings.Contains(err.Error(), "UPGRADE_PENDING_RECOVERY_NOT_ADMITTED") || out.Len() != 0 {
				t.Fatalf("error=%v out=%q", err, out.String())
			}
		})
	}
	f, executor, deps := pendingRecoveryFixture(t, WindowsProductRecoverySuccessorStarting)
	deps.ValidatePending = func(windowsProductRecoveryRequest, windowsProductRecoveryJournalV3) error {
		return errors.New("synthetic missing inverse")
	}
	deps.Recover = func(windowsProductRecoveryRequest) (*WindowsProductPairRecoverySettled, error) {
		t.Fatal("invalid inverse entered lifecycle")
		return nil, nil
	}
	cmd, out := committedRecoveryCommand()
	if err := runWindowsProductRecoveryCommandWithDeps(cmd, f.stateDir, executor, f.product, windowsRecoveryCommandPending, deps); err == nil || out.Len() != 0 {
		t.Fatalf("missing inverse error=%v out=%q", err, out.String())
	}
}

func TestPendingRecoveryEngineBindsOwnJournalReadBeforeEffects(t *testing.T) {
	f := newProductRecoveryV3Fixture(t, WindowsProductRecoverySuccessorStarting)
	tasks := &recoveryTaskTxnFake{}
	req := f.request(tasks)
	req.ExpectedJournalSHA256 = strings.Repeat("f", 64)
	req.Deps.ReopenTasks = func(context.Context, WindowsProductPairTaskRecovery) (WindowsProductTaskTxn, error) {
		t.Fatal("journal mismatch opened task/lifecycle owner")
		return nil, nil
	}
	if _, err := reconcileWindowsProductRecovery(req); !errors.Is(err, ErrWindowsProductRecoveryIdentity) {
		t.Fatalf("journal conflict=%v", err)
	}
	assertPairFile(t, f.product, f.newProduct)
}

func TestPendingRecoverySettledRollbackHealthPrecedesJournalCleanup(t *testing.T) {
	f := newProductRecoveryV3Fixture(t, WindowsProductRecoverySettledRollback)
	req := f.request(&recoveryTaskTxnFake{})
	req.RequirePriorSettlementOnSettledRollback = true
	raw, err := os.ReadFile(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3))
	if err != nil {
		t.Fatal(err)
	}
	req.ExpectedJournalSHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	discard := 0
	req.Deps.DiscardTaskStore = func(WindowsProductPairTaskRecovery) error { discard++; return nil }
	if _, err := reconcileWindowsProductRecovery(req); err == nil || discard != 0 {
		t.Fatalf("missing health err=%v discard=%d", err, discard)
	}
	req.RecoverPriorSettlement = func(context.Context, windowsProductRecoveryJournalV3) error {
		return errors.New("synthetic unhealthy prior")
	}
	if _, err := reconcileWindowsProductRecovery(req); err == nil || discard != 0 {
		t.Fatalf("unhealthy prior err=%v discard=%d", err, discard)
	}
	if _, err := os.Stat(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)); err != nil {
		t.Fatal("health failure removed journal")
	}
}

func TestPendingRecoveryEngineMissingExpectedDigestFailsClosed(t *testing.T) {
	f := newProductRecoveryV3Fixture(t, WindowsProductRecoverySettledRollback)
	req := f.request(&recoveryTaskTxnFake{})
	req.RequirePriorSettlementOnSettledRollback = true
	req.RecoverPriorSettlement = func(context.Context, windowsProductRecoveryJournalV3) error {
		t.Fatal("missing pending digest reached health/lifecycle")
		return nil
	}
	if _, err := reconcileWindowsProductRecovery(req); !errors.Is(err, ErrWindowsProductRecoveryIdentity) {
		t.Fatalf("missing pending digest=%v", err)
	}
}

func TestPendingRecoveryReadOnlyMissingPriorPayloadRefuses(t *testing.T) {
	f := newProductRecoveryV3Fixture(t, WindowsProductRecoverySuccessorStarting)
	if err := os.Remove(f.productAside); err != nil {
		t.Fatal(err)
	}
	req := f.request(&recoveryTaskTxnFake{})
	req.Deps.ReopenTasks = func(context.Context, WindowsProductPairTaskRecovery) (WindowsProductTaskTxn, error) {
		t.Fatal("missing artifact entered task/lifecycle owner")
		return nil, nil
	}
	if err := validateWindowsProductPendingRecovery(req, f.journal); !errors.Is(err, ErrWindowsProductRecoveryIdentity) {
		t.Fatalf("missing inverse=%v", err)
	}
	assertPairFile(t, f.product, f.newProduct)
}

type pendingRecoveryShortWriter struct{}

func (pendingRecoveryShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }
func TestPendingRecoveryOutputFailureCannotReportSuccess(t *testing.T) {
	f, executor, deps := pendingRecoveryFixture(t, WindowsProductRecoverySuccessorStarting)
	deps.Recover = func(windowsProductRecoveryRequest) (*WindowsProductPairRecoverySettled, error) {
		if err := os.Remove(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)); err != nil {
			return nil, err
		}
		return &WindowsProductPairRecoverySettled{TransactionID: f.journal.TransactionID, Outcome: WindowsProductPairRecoverySettledRollback}, nil
	}
	cmd, _ := committedRecoveryCommand()
	cmd.SetOut(pendingRecoveryShortWriter{})
	err := runWindowsProductRecoveryCommandWithDeps(cmd, f.stateDir, executor, f.product, windowsRecoveryCommandPending, deps)
	if !errors.Is(err, io.ErrShortWrite) || !strings.Contains(err.Error(), "UPGRADE_PENDING_RECOVERY_OUTPUT_FAILED") {
		t.Fatalf("short output=%v", err)
	}
}

func TestPendingRecoveryCommandInvalidJournalNeverInvokesOwner(t *testing.T) {
	for _, kind := range []string{"missing", "malformed", "unknown-phase", "wrong-mode", "wrong-target", "mixed-legacy", "inverse-failure", "journal-drift"} {
		t.Run(kind, func(t *testing.T) {
			f, executor, deps := pendingRecoveryFixture(t, WindowsProductRecoverySuccessorStarting)
			path := filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)
			calls := 0
			deps.Recover = func(windowsProductRecoveryRequest) (*WindowsProductPairRecoverySettled, error) {
				calls++
				return nil, nil
			}
			switch kind {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				if err := os.WriteFile(path, []byte("malformed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unknown-phase":
				f.journal.Phase = WindowsProductRecoveryPhase("unknown")
				raw, err := json.Marshal(f.journal)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong-mode":
				f.journal.Mode = WindowsProductPairModeCanonicalize
				raw, _ := json.Marshal(f.journal)
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong-target":
				f.journal.Product.Target = filepath.Join(filepath.Dir(f.product), "foreign", "mcphub.exe")
				raw, _ := json.Marshal(f.journal)
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "mixed-legacy":
				if err := os.WriteFile(filepath.Join(f.stateDir, WindowsProductPairRecoveryFileLeaf), []byte("legacy"), 0600); err != nil {
					t.Fatal(err)
				}
			case "inverse-failure":
				deps.ValidatePending = func(windowsProductRecoveryRequest, windowsProductRecoveryJournalV3) error {
					return errors.New("synthetic unavailable inverse")
				}
			case "journal-drift":
				deps.ValidatePending = func(windowsProductRecoveryRequest, windowsProductRecoveryJournalV3) error {
					return os.WriteFile(path, []byte("foreign rewritten journal"), 0600)
				}
			}
			cmd, out := committedRecoveryCommand()
			if err := runWindowsProductRecoveryCommandWithDeps(cmd, f.stateDir, executor, f.product, windowsRecoveryCommandPending, deps); err == nil || calls != 0 || out.Len() != 0 {
				t.Fatalf("kind=%s error=%v calls=%d out=%q", kind, err, calls, out.String())
			}
		})
	}
}

func TestPendingRecoveryEngineExpectedJournalRejectsLegacyFallback(t *testing.T) {
	f := newProductRecoveryV3Fixture(t, WindowsProductRecoverySuccessorStarting)
	req := f.request(&recoveryTaskTxnFake{})
	req.ExpectedJournalSHA256 = strings.Repeat("e", 64)
	if err := os.Remove(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.stateDir, WindowsProductPairRecoveryFileLeaf), []byte("legacy"), 0600); err != nil {
		t.Fatal(err)
	}
	req.Legacy.Deps.ReadJournal = func(string) ([]byte, error) { t.Fatal("expected V3 request entered legacy fallback"); return nil, nil }
	if _, err := reconcileWindowsProductRecovery(req); !errors.Is(err, ErrWindowsProductRecoveryIdentity) {
		t.Fatalf("legacy fallback=%v", err)
	}
}

func TestPendingRecoveryOwnerFailureRetainsJournalAndRedacts(t *testing.T) {
	f, executor, deps := pendingRecoveryFixture(t, WindowsProductRecoverySuccessorStarting)
	deps.Recover = func(windowsProductRecoveryRequest) (*WindowsProductPairRecoverySettled, error) {
		return nil, fmt.Errorf("%w: private-path-secret", ErrWindowsProductRecoveryPending)
	}
	cmd, out := committedRecoveryCommand()
	err := runWindowsProductRecoveryCommandWithDeps(cmd, f.stateDir, executor, f.product, windowsRecoveryCommandPending, deps)
	if !errors.Is(err, ErrWindowsProductRecoveryPending) || strings.Contains(err.Error(), "private-path-secret") || out.Len() != 0 {
		t.Fatalf("owner failure=%v out=%q", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)); err != nil {
		t.Fatal("owner failure removed pending journal")
	}
}

func TestPendingCurrentRuntimeQualificationPrecedesMaterialEffects(t *testing.T) {
	f, _, _ := pendingRecoveryFixture(t, WindowsProductRecoverySuccessorStarting)
	req := f.request(&recoveryTaskTxnFake{})
	calls := 0
	req.Deps.ReopenTasks = func(context.Context, WindowsProductPairTaskRecovery) (WindowsProductTaskTxn, error) {
		t.Fatal("unqualified current intent opened material owner")
		return nil, nil
	}
	req.PreparePendingRuntime = func(ctx context.Context, _ windowsProductRecoveryJournalV3, receiving *pendingRuntimeReceiving) (func(context.Context, windowsProductRecoveryJournalV3) error, error) {
		calls++
		_, err := capturePendingRuntimeExpectation(ctx, time.Now, func(context.Context, time.Time) (*api.SupervisorRuntimeIntent, error) {
			return nil, api.ErrSupervisorRuntimeIntentUnavailable
		}, func() (api.GUIOwnerControl, error) { return api.GUIOwnerControl{RunDesired: api.GUIRunRunning}, nil }, receiving)
		return nil, err
	}
	_, err := reconcileWindowsProductRecovery(req)
	var failure *pendingRuntimeError
	if calls != 1 || !errors.Is(err, ErrWindowsProductRecoveryPending) || !errors.As(err, &failure) || failure.kind != pendingRuntimeUnavailable {
		t.Fatalf("calls=%d failure=%v", calls, err)
	}
	assertPairFile(t, f.product, f.newProduct)
	if _, err := os.Stat(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)); err != nil {
		t.Fatal("qualification failure removed original journal")
	}
}

func TestPendingCurrentRuntimeUsesOriginalMaterialInverseAndResult(t *testing.T) {
	f, _, _ := pendingRecoveryFixture(t, WindowsProductRecoverySuccessorStarting)
	req := f.request(&recoveryTaskTxnFake{})
	basis := pendingRuntimeReceiving{intentSHA256: strings.Repeat("a", 64), stopSHA256: strings.Repeat("b", 64), guiSHA256: strings.Repeat("c", 64), generation: 8, observedAt: time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)}
	settledCalls := 0
	req.RecoverPriorSettlement = func(context.Context, windowsProductRecoveryJournalV3) error {
		t.Fatal("unbound old runtime comparator used")
		return nil
	}
	req.PreparePendingRuntime = func(_ context.Context, j windowsProductRecoveryJournalV3, receiving *pendingRuntimeReceiving) (func(context.Context, windowsProductRecoveryJournalV3) error, error) {
		assertPairFile(t, f.product, f.newProduct)
		if j.TransactionID != f.journal.TransactionID {
			t.Fatal("journal identity substituted")
		}
		*receiving = basis
		return func(_ context.Context, j windowsProductRecoveryJournalV3) error {
			settledCalls++
			assertPairFile(t, f.product, f.priorProduct)
			assertPairFile(t, f.legacy, f.priorLegacy)
			assertPairFile(t, f.receiptV2, f.priorReceiptV2)
			return nil
		}, nil
	}
	result, err := reconcileWindowsProductRecovery(req)
	if err != nil || settledCalls != 1 || result == nil || result.Runtime == nil || *result.Runtime != basis || result.TransactionID != f.journal.TransactionID || result.Outcome != WindowsProductPairRecoverySettledRollback {
		t.Fatalf("result=%+v calls=%d error=%v", result, settledCalls, err)
	}
}

func TestPendingCurrentRuntimeTypedProjectionAndSuccessEcho(t *testing.T) {
	basis := &pendingRuntimeReceiving{intentSHA256: strings.Repeat("a", 64), stopSHA256: strings.Repeat("b", 64), guiSHA256: strings.Repeat("c", 64), generation: 8, observedAt: time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)}
	for _, kind := range []pendingRuntimeCause{pendingRuntimeUnavailable, pendingRuntimeInvalid, pendingRuntimeChanged, pendingRuntimeMismatch, pendingRuntimeMaterial, pendingRuntimeReadiness, pendingRuntimeHistorical, pendingRuntimeCleanup} {
		private := errors.New("PRIVATE_PATH SID-S-1-5-21 injected runtime_failure=FAKE")
		cause := pendingRuntimeFailure(kind, basis, private)
		projected := committedRecoveryCommandError("E_WINDOWS_PRODUCT_RECOVERY_PENDING", "recover pending prior settlement", cause)
		projected.runtime = basis
		message := projected.Error()
		direct := cause.Error()
		if !strings.Contains(direct, "runtime_failure="+string(kind)) || strings.Contains(direct, "PRIVATE_PATH") || strings.Contains(direct, "FAKE") {
			t.Fatalf("direct current projection=%q", direct)
		}
		var typed *pendingRuntimeError
		if !errors.Is(projected, private) || !errors.As(projected, &typed) || !strings.Contains(message, "runtime_basis=current-intent") || !strings.Contains(message, "runtime_failure="+string(kind)) || strings.Contains(message, "PRIVATE_PATH") || strings.Contains(message, "FAKE") {
			t.Fatalf("typed projection=%q", message)
		}
	}
	f, executor, deps := pendingRecoveryFixture(t, WindowsProductRecoverySuccessorStarting)
	deps.Recover = func(windowsProductRecoveryRequest) (*WindowsProductPairRecoverySettled, error) {
		if err := os.Remove(filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3)); err != nil {
			return nil, err
		}
		return &WindowsProductPairRecoverySettled{TransactionID: f.journal.TransactionID, Outcome: WindowsProductPairRecoverySettledRollback, Runtime: basis}, nil
	}
	cmd, out := committedRecoveryCommand()
	if err := runWindowsProductRecoveryCommandWithDeps(cmd, f.stateDir, executor, f.product, windowsRecoveryCommandPending, deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "runtime_basis=current-intent") || !strings.Contains(out.String(), "runtime_intent_sha256="+basis.intentSHA256) {
		t.Fatalf("success receiving=%q", out.String())
	}
}

func TestPendingCurrentRuntimeStoppedGUIAndFatalObservationPreservePolicy(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	receiving := new(pendingRuntimeReceiving)
	intent := &api.SupervisorRuntimeIntent{SHA256: strings.Repeat("a", 64), StopSHA256: strings.Repeat("b", 64), ObservedAt: now, Ports: []api.SupervisorFrozenPort{}, Stopped: map[string]bool{}}
	_, err := capturePendingRuntimeExpectation(context.Background(), func() time.Time { return now }, func(context.Context, time.Time) (*api.SupervisorRuntimeIntent, error) { return intent, nil }, func() (api.GUIOwnerControl, error) {
		return api.GUIOwnerControl{RunDesired: api.GUIRunStopped, StopCause: "cli-quit"}, nil
	}, receiving)
	if !errors.Is(err, api.ErrGUIStoppedByUser) || receiving.intentSHA256 != intent.SHA256 {
		t.Fatalf("GUI stop policy/basis=%+v error=%v", receiving, err)
	}
	for _, kind := range []pendingRuntimeCause{pendingRuntimeChanged, pendingRuntimeCleanup} {
		ops, _, _ := guiPriorDiagnosticOpsFixture(t)
		ops.ObserveHealthy = func(context.Context, windowsProductRecoveryJournalV3) error {
			return pendingRuntimeFailure(kind, receiving, errors.New("retained cause"))
		}
		ops.Reservation = func(context.Context) (gui.OwnedSingleInstanceLease, error) {
			t.Fatal("fatal bound observation entered reservation")
			return nil, nil
		}
		journal := windowsProductRecoveryJournalV3{PreSettlement: &windowsProductPreSettlementV3{GUIOwner: windowsProductGUIOwnerConfirmedPresent}}
		if err := recoverGUIOwnedPriorSettlementWithOps(context.Background(), journal, ops); err == nil {
			t.Fatalf("fatal observation=%v", err)
		}
	}
}

func TestPendingHistoricalSettlementReusesGenuineJournalReceipt(t *testing.T) {
	f, _, _ := pendingRecoveryFixture(t, WindowsProductRecoverySuccessorStarting)
	if !f.journal.PriorFleetReaped || f.journal.Quiescence == nil {
		t.Fatal("fixture lacks original acquired closure receipt")
	}
	lookup := func(int) (process.PIDHandleIdentity, error) {
		t.Fatal("completed original closure was replaced by later PID/port reservation")
		return process.PIDHandleIdentity{}, nil
	}
	if err := validatePendingHistoricalCohort(context.Background(), f.journal, api.SupervisorLockOwner{}, nil, lookup); err != nil {
		t.Fatal(err)
	}
	f.journal.QuiescenceReceiptSHA256 = strings.Repeat("0", 64)
	if err := validatePendingHistoricalCohort(context.Background(), f.journal, api.SupervisorLockOwner{}, nil, lookup); err == nil {
		t.Fatal("invalid retained closure receipt accepted")
	}
}

func TestOrdinaryPendingRuntimeBoundaryKeepsNestedOwnerDiagnostics(t *testing.T) {
	for _, operation := range []upgradeGUIPriorOperation{upgradeGUIPriorReadiness, upgradeGUIPriorControl} {
		t.Run(string(operation), func(t *testing.T) {
			f := newProductRecoveryV3Fixture(t, WindowsProductRecoverySuccessorStarting)
			f.journal.Mode = WindowsProductPairModeUpgrade
			f.journal.PreSettlement = &windowsProductPreSettlementV3{GUIOwner: windowsProductGUIOwnerConfirmedPresent}
			basis := &pendingRuntimeReceiving{intentSHA256: strings.Repeat("a", 64), stopSHA256: strings.Repeat("b", 64), guiSHA256: strings.Repeat("c", 64), generation: 8, observedAt: time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)}
			private := errors.New("PRIVATE_PATH_X SID-S-1-5-21 argv-secret runtime_failure=FAKE failed_operation=FAKE")
			initial := errors.New("UPGRADE_GUI_TASK_DRIFT: PRIVATE_PATH_X")
			kind := pendingRuntimeReadiness
			if operation == upgradeGUIPriorControl {
				kind = pendingRuntimeMismatch
			}
			primary := pendingRuntimeFailure(kind, basis, private)
			cleanupCause := errors.New("PRIVATE_CLEANUP_X SID-S-1-5-21 argv-secret")
			cleanup := pendingRuntimeFailure(pendingRuntimeCleanup, basis, cleanupCause)
			ops, lease, launch := guiPriorDiagnosticOpsFixture(t)
			ops.ObserveHealthy = func(context.Context, windowsProductRecoveryJournalV3) error { return initial }
			if operation == upgradeGUIPriorControl {
				launch.bindErr = primary
			} else {
				ops.WaitSupervisorReady = func(context.Context) error { return primary }
			}
			launch.abortErr = cleanup
			ownerErr := recoverGUIOwnedPriorSettlementWithOps(context.Background(), f.journal, ops)
			if lease.released != 1 || launch.binds != 1 || launch.aborts != 1 || launch.handoffs != 0 {
				t.Fatalf("diagnostic changed cleanup: lease=%+v launch=%+v", lease, launch)
			}
			var prior *upgradeGUIPriorSettlementError
			if !errors.As(ownerErr, &prior) || prior.operation != operation {
				t.Fatal("producing operation lost")
			}
			outer, ok := prior.cause.(interface{ Unwrap() []error })
			if !ok || len(outer.Unwrap()) != 1 {
				t.Fatal("reservation cleanup wrapper changed")
			}
			joined, ok := outer.Unwrap()[0].(interface{ Unwrap() []error })
			if !ok || len(joined.Unwrap()) != 2 || !errors.Is(joined.Unwrap()[0], primary) || !errors.Is(joined.Unwrap()[1], cleanupCause) {
				t.Fatal("primary then cleanup ordering changed")
			}
			tasks := &recoveryTaskTxnFake{}
			req := f.request(tasks)
			req.RecoverPriorSettlement = func(context.Context, windowsProductRecoveryJournalV3) error { return ownerErr }
			settled, retained := rollbackWindowsProductRecovery(req, withWindowsProductRecoveryDefaults(req.Deps), filepath.Join(f.stateDir, WindowsProductRecoveryFileLeafV3), f.journal, tasks)
			if settled != nil {
				t.Fatal("failed prior recovery became success")
			}
			// This exact ordinary return boundary is used by runV5UpgradeWindowsWithPathsAndDeps.
			ordinary := ordinaryWindowsProductRecoveryError(retained)
			message := ordinary.Error()
			if !strings.HasPrefix(message, "v0.5 upgrade: recover Windows product pair: E_WINDOWS_PRODUCT_RECOVERY_PENDING: "+currentPendingRuntimeStage) || !strings.Contains(message, "failed_operation="+string(operation)) || !strings.Contains(message, "runtime_failure="+string(kind)) || !strings.Contains(message, "runtime_cleanup=FAILED") || !strings.Contains(message, basis.projection()) {
				t.Fatalf("ordinary boundary omitted retained diagnostics: %q", message)
			}
			for _, marker := range []string{"PRIVATE_", "SID-S-", "argv-secret", "=FAKE"} {
				if strings.Contains(message, marker) {
					t.Fatalf("private/injected value crossed ordinary stderr: %q", message)
				}
			}
			for _, cause := range []error{private, initial, cleanupCause, primary, cleanup, ErrWindowsProductRecoveryPending, ErrWindowsProductPairRecoveryRollback} {
				if !errors.Is(ordinary, cause) {
					t.Fatal("ordinary projection erased retained cause")
				}
			}
			exclusive := committedRecoveryCommandError("E_WINDOWS_PRODUCT_RECOVERY_PENDING", "recover pending prior settlement", retained)
			exclusive.runtime = basis
			want := "E_WINDOWS_PRODUCT_RECOVERY_PENDING: recover pending prior settlement; recovery_result=OWNER_ERROR; failed_operation=" + string(operation) + "; initial_failed_predicate=UPGRADE_GUI_TASK_DRIFT" + basis.projection() + "; runtime_cleanup=FAILED; runtime_failure=" + string(kind)
			if exclusive.Error() != want {
				t.Fatalf("exclusive projection changed: %q", exclusive.Error())
			}
			if lease.released != 1 || launch.aborts != 1 {
				t.Fatal("projection repeated cleanup")
			}
		})
	}
	private := errors.New("PRIVATE_PATH_X SID-S-1-5-21 argv-secret runtime_failure=FAKE")
	unknown := pendingRuntimeFailure(pendingRuntimeCause("FAKE"), nil, private)
	unclassified := ordinaryWindowsProductRecoveryError(unknown).Error()
	if !strings.Contains(unclassified, "recovery_result=OWNER_ERROR_UNCLASSIFIED") || !strings.Contains(unclassified, "runtime_snapshot=UNAVAILABLE") || strings.Contains(unclassified, "runtime_failure=") || strings.Contains(unclassified, "runtime_intent_sha256=") || strings.Contains(unclassified, "PRIVATE_") || strings.Contains(unclassified, "FAKE") {
		t.Fatalf("unknown/unavailable diagnostics fabricated detail: %q", unclassified)
	}
	unrelated := errors.New("ordinary unrelated failure")
	unchanged := ordinaryWindowsProductRecoveryError(unrelated)
	if unchanged.Error() != "v0.5 upgrade: recover Windows product pair: ordinary unrelated failure" || !errors.Is(unchanged, unrelated) {
		t.Fatal("unrelated ordinary behavior changed")
	}
}

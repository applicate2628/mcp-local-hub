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
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/process"
)

// The fixture supplies pre-stop observations; actual Product owners conduct
// preparation, promotion, inverse, close and fixed-leaf native transfer.
func v2TxnAssertServingRollback(t *testing.T, txn *WindowsProductTxn, seed binaryadmission.WindowsCanonicalEntrySetSnapshotV2, quiescence *windowsProductQuiescenceReceiptV1, journalPath, mode string) {
	t.Helper()
	o := &txn.Opts
	prior, err := o.EntryBundle.acquirePrior(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	view, err := prior.RuntimeProduct(t.Context())
	if err != nil || view == nil {
		t.Fatal(err)
	}
	evidence, err := prior.Observation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 32)
	bundle := &windowsCanonicalEntrySetReceiptV2{Schema: binaryadmission.WindowsCanonicalEntrySetSchemaV2, TransactionID: id,
		BuildID: seed.BuildID, RegistryProjectionSHA256: seed.RegistryProjectionSHA256, CanonicalEntryPath: o.CanonicalEntryPath,
		ProductPath: o.ProductPath, IdentityPath: o.IdentityPath, Machine: seed.Machine}
	for _, member := range evidence.Members {
		switch member.Member {
		case seed.IdentityRelativePath:
			bundle.IdentitySHA256 = member.File.SHA256
		case seed.CanonicalEntryRelativePath:
			bundle.CanonicalEntrySHA256 = member.File.SHA256
		}
	}
	artifact := view.Artifact()
	receipt := UpgradeReceiptV3{Schema: UpgradeReceiptSchemaV3, Mode: WindowsProductPairModeUpgrade, Admission: UpgradeAdmissionCanonicalEntryProductV2,
		TransactionID: id, EntryBundle: bundle, InstalledAt: time.Now().UTC().Format(time.RFC3339Nano), TaskSetSHA256: strings.Repeat("c", 64),
		Artifact: UpgradeReceiptArtifactV3{Role: artifact.Role, Subsystem: artifact.Subsystem, Machine: artifact.Machine, Version: artifact.Version, Commit: artifact.Commit, BuildDate: artifact.BuildDate, SHA256: artifact.SHA256},
		Retired:  UpgradeReceiptRetiredV3{WindowlessAbsent: true, AdmissionHelperAbsentFromPayload: true, RollbackImagesAbsent: true}}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeUpgradeReceipt(raw); err != nil {
		t.Fatal(err)
	}
	if err := api.WriteStateFileBytesAtomic(o.ReceiptV3Path, raw); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(123456, 0).UTC().Format(time.RFC3339Nano)
	observed := windowsProductPriorServingAggregateV1{Supervisor: api.SupervisorLockOwner{PID: 101, StartedAt: stamp}, Route: api.SupervisorLockOwner{PID: 102, StartedAt: stamp}, RouteGeneration: 2,
		PriorTransactionID: id, PriorReceiptSHA256: v2TxnFixtureDigest(raw), EndpointAuthoritySHA256: strings.Repeat("b", 64),
		EndpointRawSHA256: strings.Repeat("d", 64), EndpointProvenanceSHA256: strings.Repeat("f", 64), QualificationProvenanceSHA256: strings.Repeat("e", 64), EndpointIdentity: api.ConfigParentDirectoryIdentityV1{VolumeSerialNumber: 1, FileID: [16]byte{1}}}
	o.PriorServingAggregate = &observed
	o.PreSettlement.Supervisor = windowsProductSupervisorSettlementV1{State: windowsProductSupervisorGracefullySettled, Generation: &windowsProductSupervisorGenerationV1{PID: 101, StartedAt: stamp, IPCRequestVersion: 1}}
	o.PreSettlement.SupervisorLockReleased = false
	quiescence.Supervisor = o.PreSettlement.Supervisor
	ns, err := api.OpenWindowsProductStateNamespace(t.Context(), o.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := ns.Close(); err != nil {
			t.Error(err)
		}
	}()
	fence, err := api.AcquireUpgradeFenceBound(t.Context(), o.StateDir, ns)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := fence.Release(); err != nil {
			t.Error(err)
		}
	}()
	var marker *api.WindowsProductRollbackContinuation
	txn.transferAggregateRollback = func(ctx context.Context, raw []byte) error {
		var err error
		marker, err = ns.TransferAggregateRollback(ctx, fence, raw)
		return err
	}
	trigger := errors.New("interrupted serving prior transaction")
	o.Deps.RestartPrior = func(string) (SupervisorLaunch, error) {
		t.Fatal("old Product restarted before terminal V3 transfer")
		return nil, nil
	}
	reached := false
	o.Deps.Fault = func(stage WindowsProductStage) error {
		if stage == WindowsProductStageProductReadBack {
			reached = true
			return trigger
		}
		return nil
	}
	var borrower *binaryadmission.WindowsCanonicalEntryRecoverySetV2
	if mode == "serving-lender" {
		o.Deps.WriteRecoveryJournal = func(path string, body []byte) error {
			if err := api.WriteStateFileBytesAtomic(path, body); err != nil {
				return err
			}
			j, err := decodeWindowsProductRecoveryJournalV3(body)
			if err != nil || borrower != nil || j.EntryBundle.Bundle.Disposition.State != "rolled-back" || len(j.EntryBundle.Cleanup) == 0 {
				return err
			}
			for _, row := range j.EntryBundle.Cleanup {
				if row.State != "absence_received" {
					return nil
				}
			}
			// Keep a child of the original warm directory lender alive across the actual close.
			req := windowsProductRecoveryRequest{Context: t.Context(), StateDir: o.StateDir, JournalAllowance: o.JournalAllowance}
			source := &windowsCanonicalJournalRecoverySource{req: req, deps: boundWindowsProductRecoveryDefaults(req), journal: j, raw: bytes.Clone(body),
				path: path, purpose: canonicalRecoveryRollbackCleanup, active: true, memberDirectories: o.EntryBundle.original.Directories()}
			source.self = source
			borrower, err = binaryadmission.AcquireWindowsCanonicalEntryForRecoveryV2(t.Context(), source, binaryadmission.WindowsCanonicalRecoveryPriorV1)
			if borrower == nil {
				t.Fatal("original directory borrower unavailable", err)
			}
			return nil
		}
	}
	var prepared windowsProductRecoveryJournalV3
	checkPrepared := func() error {
		var err error
		prepared, err = v2TxnReadFixtureJournal(journalPath)
		if err != nil || prepared.PriorServingAggregate == nil || *prepared.PriorServingAggregate != observed || prepared.PriorFleetReaped {
			t.Fatal("initial journal lost original prior observation", err)
		}
		altered := *prepared.PriorServingAggregate
		for _, malformed := range []string{"", strings.Repeat("D", 64)} {
			altered.EndpointRawSHA256 = malformed
			invalid := prepared
			invalid.PriorServingAggregate = &altered
			if validatePriorServingAggregate(invalid) == nil {
				t.Fatal("journal accepted a serving observation without a canonical endpoint digest")
			}
		}
		return trigger
	}
	if strings.HasPrefix(mode, "serving-drift-") {
		tasks := o.Tasks.(*v2TxnFixtureTasks)
		rpc := &fakeUpgradeDeps{}
		generation := *o.PreSettlement
		var preparedBytes []byte
		foreign := []byte("external receipt after PREPARED")
		err := RunInstallUpgrade(t.Context(), UpgradeOpts{StateDir: o.StateDir, BinaryPath: o.ProductPath,
			WindowsProduct: txn, priorRuntime: view, Deps: rpc, OnProductPrepared: func() error {
				preparedBytes = mustReadFile(t, journalPath)
				switch mode {
				case "serving-drift-v2":
					return api.WriteStateFileBytesAtomic(o.ReceiptV2Path, foreign)
				case "serving-drift-v3":
					return api.WriteStateFileBytesAtomic(o.ReceiptV3Path, foreign)
				default:
					tasks.currentXML = "<Task>independent change</Task>"
					return nil
				}
			}, WindowsProductPairLifecycle: &windowsProductPairLifecycleHooks{FrozenPorts: []windowsProductIntentPortV1{},
				Observe: func(context.Context, string, []windowsProductIntentPortV1) (windowsProductQuiescenceObservationV1, error) {
					return generation, nil
				}}})
		check := func(err error) {
			t.Helper()
			expected := "prior Product receipt changed before rollback"
			if mode == "serving-drift-task" {
				expected = "fixture Task differs from original XML"
			}
			if !errors.Is(err, ErrWindowsProductRecoveryPending) || !strings.Contains(err.Error(), expected) || len(rpc.calls) != 0 ||
				tasks.restores != 0 || tasks.rewrites != 0 || marker != nil || len(preparedBytes) == 0 ||
				!bytes.Equal(preparedBytes, mustReadFile(t, journalPath)) {
				t.Fatal("changed PREPARED preimage was not preserved", rpc.calls, err)
			}
			switch mode {
			case "serving-drift-v2":
				if !bytes.Equal(mustReadFile(t, o.ReceiptV2Path), foreign) {
					t.Fatal("new V2 receipt removed")
				}
			case "serving-drift-v3":
				if !bytes.Equal(mustReadFile(t, o.ReceiptV3Path), foreign) {
					t.Fatal("changed V3 receipt overwritten")
				}
			default:
				if tasks.currentXML != "<Task>independent change</Task>" {
					t.Fatal("changed Task reimported")
				}
			}
		}
		check(err)
		req := windowsProductRecoveryRequest{Context: t.Context(), StateDir: o.StateDir, CanonicalEntryPath: o.CanonicalEntryPath,
			ProductPath: o.ProductPath, IdentityPath: o.IdentityPath, LegacyWindowlessPath: o.LegacyWindowlessPath, JournalAllowance: o.JournalAllowance,
			Legacy: windowsProductPairRecoveryRequest{JournalAllowance: o.JournalAllowance}, transferAggregateRollback: txn.transferAggregateRollback,
			Deps: windowsProductRecoveryDeps{
				ReopenEntryBundleTasks: func(context.Context, WindowsProductPairTaskRecovery, binaryadmission.WindowsCanonicalEntrySetTargetsV2) (WindowsProductTaskTxn, error) {
					return tasks, nil
				},
				DiscardTaskStore: func(WindowsProductPairTaskRecovery) error { t.Fatal("discard of changed preparation"); return nil },
			}}
		_, err = reconcileWindowsProductRecovery(req)
		check(err)
		return
	}
	if mode == "serving-before-reap" {
		generation := *o.PreSettlement
		err = RunInstallUpgrade(t.Context(), UpgradeOpts{StateDir: o.StateDir, BinaryPath: o.ProductPath,
			WindowsProduct: txn, priorRuntime: view, Deps: buildV5UpgradeDeps(o.ProductPath, o.StateDir), OnProductPrepared: checkPrepared,
			WindowsProductPairLifecycle: &windowsProductPairLifecycleHooks{FrozenPorts: []windowsProductIntentPortV1{},
				Observe: func(context.Context, string, []windowsProductIntentPortV1) (windowsProductQuiescenceObservationV1, error) {
					return generation, nil
				}}})
	} else {
		if err := txn.Prepare(); err != nil {
			t.Fatal(err)
		}
		_ = checkPrepared()
		txn.Opts.ReplacementAuthority.Quiescence = quiescence
		_, err = txn.Run(t.Context())
		if !reached {
			t.Fatal("promotion boundary not reached", err)
		}
	}
	if mode == "serving-lender" {
		if borrower == nil || marker != nil || !errors.Is(err, trigger) || !strings.Contains(err.Error(), "requires settled child borrowers") {
			t.Fatal("real close fault was not preserved", err)
		}
		defer func() {
			if err := borrower.Close(); err != nil {
				t.Error(err)
			}
			for _, directory := range orderedCurrentDirectories(txn.Opts.EntryBundle) {
				if err := directory.Close(); err != nil {
					t.Error(err)
				}
			}
		}()
		failed, err := v2TxnReadFixtureJournal(journalPath)
		if err != nil || failed.EntryBundle.Bundle.Disposition.CloseOwner == nil || failed.EntryBundle.Bundle.Disposition.CloseError == nil {
			t.Fatal("lender diagnostic did not reach active V3", err)
		}
		unrelated := failed
		unrelated.PriorServingAggregate = nil
		if err := validateCanonicalEntryBundleJournal(unrelated); err == nil {
			t.Fatal("serving close-owner diagnostic admitted for an unrelated journal")
		}
		req := windowsProductRecoveryRequest{Context: t.Context(), StateDir: o.StateDir, CanonicalEntryPath: o.CanonicalEntryPath, ProductPath: o.ProductPath,
			IdentityPath: o.IdentityPath, LegacyWindowlessPath: o.LegacyWindowlessPath, JournalAllowance: o.JournalAllowance,
			Legacy: windowsProductPairRecoveryRequest{JournalAllowance: o.JournalAllowance}, transferAggregateRollback: txn.transferAggregateRollback}
		before := mustReadFile(t, journalPath)
		if _, err := reconcileWindowsProductRecovery(req); !errors.Is(err, ErrWindowsProductCleanupPending) || marker != nil || !bytes.Equal(before, mustReadFile(t, journalPath)) {
			t.Fatal("fresh retry cleared active lender", err)
		}
		return
	}
	if !errors.Is(err, trigger) || !soleMaterialRollbackError(err) || marker == nil {
		t.Fatal("native material rollback/handoff", err)
	}
	if _, err = os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old receiver still blocked by V3", err)
	}
	j, err := receiveAggregateRollbackJournal(marker, o.StateDir, o.CanonicalEntryPath)
	if err != nil || j.TransactionID != prepared.TransactionID || j.PriorServingAggregate.PriorTransactionID != id {
		t.Fatal("terminal handoff", err)
	}
	if err := refuseAggregateRollback(o.StateDir); !errors.Is(err, ErrWindowsProductRecoveryPending) {
		t.Fatal("mutation accepted", err)
	}
	ordinary, err := inspectWindowsProductRecoveryLeaves(o.StateDir)
	if err != nil || ordinary {
		t.Fatal("restored receiver could not observe material settlement", err)
	}
	fresh, err := ns.ReceiveAggregateRollback(t.Context(), o.JournalAllowance.maxEncodedBytes)
	if err != nil || !bytes.Equal(fresh.Bytes(), marker.Bytes()) {
		t.Fatal("lost terminal copy on cold recovery", err)
	}
	if _, err := os.Stat(filepath.Join(o.StateDir, api.SupervisorEventLogFileLeaf)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rollback published commit", err)
	}
	if !bytes.Equal(mustReadFile(t, o.ReceiptV3Path), raw) {
		t.Fatal("restored prior receipt changed")
	}
	if mode == "serving-before-reap" {
		if o.Tasks.(*v2TxnFixtureTasks).restores != 0 {
			t.Fatal("untouched Tasks were reimported")
		}
		if err := fence.Release(); err != nil {
			t.Fatal(err)
		}
		heldIntent := seedWindowsProductBackendHold(t, o.StateDir, false)
		err := restoreProductPriorAggregate(t.Context(), o.StateDir, o.CanonicalEntryPath, o.JournalAllowance, fresh)
		if !errors.Is(err, api.ErrSupervisorBackendHeld) || ns.VerifyAggregateRollback(t.Context(), fresh) != nil {
			t.Fatal("intentional stop did not preserve the exact rollback continuation", err)
		}
		assertWindowsProductBackendHoldBytes(t, filepath.Join(o.StateDir, "supervisor-intent.json"), heldIntent)
	}
}

func TestWindowsProductAggregateRollbackRelease(t *testing.T) {
	trigger := errors.New("upgrade promotion failed")
	late := errors.New("release or apply failure")
	for _, mode := range []string{"warm original failure", "cold ready", "lost apply response", "GUI release refused", "additional owner close error"} {
		t.Run(mode, func(t *testing.T) {
			state := withTempStateDir(t)
			fence, err := api.AcquireUpgradeFence(t.Context(), state)
			if err != nil {
				t.Fatal(err)
			}
			var order []string
			guiLease := &upgradeGUICommandLease{stateDir: state, order: &order}
			var initial error = &productMaterialRollbackError{cause: trigger}
			wantCall := true
			switch mode {
			case "cold ready":
				initial = nil
			case "GUI release refused":
				guiLease.releaseErr, wantCall = late, false
			case "additional owner close error":
				initial, wantCall = errors.Join(initial, late), false
			}
			called := false
			restore := func() error {
				called = true
				next, acquired, err := api.TryAcquireUpgradeFence(t.Context(), state)
				if err != nil || !acquired || guiLease.released != 1 {
					t.Fatal("restoration entered before actual release", err)
				}
				if err := next.Release(); err != nil {
					t.Fatal(err)
				}
				if mode == "lost apply response" {
					return late
				}
				return nil
			}
			cmd := &cobra.Command{}
			var output bytes.Buffer
			cmd.SetOut(&output)
			err = finishV5UpgradeWindowsCommand(cmd, false, "success", initial, nil, guiLease, fence, upgradeProductContinuation{restore: restore})
			if called != wantCall || output.Len() != 0 || (mode == "cold ready") != (err == nil) {
				t.Fatal("incorrect recovery decision", called, output.String(), err)
			}
			if mode != "cold ready" && !errors.Is(err, trigger) {
				t.Fatal("lost initiating rollback failure", err)
			}
			if mode != "cold ready" && mode != "warm original failure" && !errors.Is(err, late) {
				t.Fatal("lost secondary failure", err)
			}
		})
	}
}

// The child owns the real singleton, pipe and kernel generation. Its Route
// status and fleet reconciliation flags are controlled protocol responses.
func TestRestoredAggregateFleetReadinessUsesCurrentPeer(t *testing.T) {
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("bounded fixture required")
	}
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	state := apitest.HardenedTempDir(t)
	var first api.SupervisorLockOwner
	t.Run("initial ready generation", func(t *testing.T) {
		child := startBoundStatusChildAt(t, ctx, deadline, 1, state, true)
		first = api.SupervisorLockOwner{PID: child.peer.PID, StartedAt: child.peer.CreationTime}
		err := requireRestoredFleetReady(ctx, state, first, child.peer, func(ctx context.Context) error { return ctx.Err() })
		if err != nil {
			t.Fatal(err)
		}
	})
	if err := os.Rename(filepath.Join(state, "peer-sender-ready"), filepath.Join(state, "first-peer-ready")); err != nil {
		t.Fatal(err)
	}
	child := startBoundStatusChildAt(t, ctx, deadline, 2, state, true)
	owner := api.SupervisorLockOwner{PID: child.peer.PID, StartedAt: child.peer.CreationTime}
	if owner == first {
		t.Fatal("second fixture generation required")
	}
	held, err := process.HoldPIDForObservation(owner.PID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := held.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := held.VerifyIdentity(process.PIDIdentityProof{PID: owner.PID, StartedAt: owner.StartedAt, ExecutablePath: child.peer.ImagePath, StartTolerance: time.Nanosecond}); err != nil {
		t.Fatal(err)
	}
	check := func(ctx context.Context) error {
		current, err := api.ReadOccupiedSupervisorLockOwner(state)
		if err != nil || current != owner {
			return errors.Join(api.ErrSupervisorControlPeerIdentity, err)
		}
		return errors.Join(ctx.Err(), requireRunningCommittedSupervisor(held))
	}
	status, err := api.QuerySupervisorAggregateStatus(ctx, state, owner, child.peer, check)
	if err != nil || status.Effective != "ready" || status.Ownership != api.RouteAggregateOwned {
		t.Fatal("second peer Route receipt", err)
	}
	if err := requireRestoredFleetReady(ctx, state, owner, child.peer, check); !errors.Is(err, ErrWindowsProductRecoveryPending) {
		t.Fatal("second peer's ready Route and first peer's ready fleet admitted", err)
	}
	if err := requireRestoredFleetReady(ctx, state, owner, child.peer, check); err != nil {
		t.Fatal("second peer ready", err)
	}
	// Exit after the strict reply, before receipt delivery. The last check sees
	// the original handle signalled; the earlier successful reply cannot settle.
	count := 0
	err = requireRestoredFleetReady(ctx, state, owner, child.peer, func(ctx context.Context) error {
		count++
		if count == 2 {
			if err := child.input(); err != nil {
				return err
			}
			until, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			for {
				wait, err := held.Wait()
				if err != nil {
					return err
				}
				if wait.Result == uint32(windows.WAIT_OBJECT_0) {
					break
				}
				if wait.Result != uint32(windows.WAIT_TIMEOUT) {
					return errors.New("fixture wait failed")
				}
				select {
				case <-until.Done():
					return until.Err()
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		return check(ctx)
	})
	if err == nil || count != 2 {
		t.Fatal("accepted an exited post-reply peer", count, err)
	}
}

func TestServingRollbackReceiptAbsenceMustBeConclusive(t *testing.T) {
	receipts := windowsProductRecoveryReceiptsV3{V3: windowsProductPairRecoveryReceipt{Path: "v3", PriorPresent: true, PriorBytes: []byte("prior")}, V2: windowsProductPairRecoveryReceipt{Path: "v2"}}
	fault := errors.New("unconfirmed read owner close")
	err := verifyServingRollbackReceipts(receipts, func(path string) ([]byte, error) {
		if path == "v3" {
			return []byte("prior"), nil
		}
		return nil, errors.Join(os.ErrNotExist, fault)
	})
	if !errors.Is(err, ErrWindowsProductRecoveryIdentity) || !errors.Is(err, fault) {
		t.Fatal("uncertain V2 absence admitted", err)
	}
}

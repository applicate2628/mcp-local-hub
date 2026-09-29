//go:build windows

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/scheduler"
)

type windowsProductTxnRequest struct {
	Context              context.Context
	StateDir             string
	ProductPath          string
	LegacyWindowlessPath string
	StagedProduct        string
	Mode                 WindowsProductPairMode
	ReplacementAuthority windowsProductReplacementAuthority
	StartSupervisor      func(string) error
	WaitSupervisorReady  func(context.Context, string, binaryadmission.WindowsArtifact) error
	RestartPrior         func(string) error
	SettleSuccessor      func(string) error
}

var newWindowsProductTxnFn = newWindowsProductTxn

func newWindowsProductTxn(req windowsProductTxnRequest) (*WindowsProductTxn, error) {
	return newWindowsProductTxnWith(req, newWindowsProductPairTaskStore, scheduler.BeginOwnedEntrypointTaskTxnWithStore)
}

func newWindowsProductTxnWith(
	req windowsProductTxnRequest,
	createStore func(string) (*windowsProductPairTaskStore, error),
	beginTasks func(context.Context, string, string, string, scheduler.RetainedTaskXMLStore) (*scheduler.OwnedEntrypointTaskTxn, error),
) (*WindowsProductTxn, error) {
	if req.Context == nil {
		req.Context = context.Background()
	}
	if req.StateDir == "" || req.ProductPath == "" || req.LegacyWindowlessPath == "" || req.StagedProduct == "" {
		return nil, errors.New("Windows product wiring: state and artifact paths are required")
	}
	store, err := createStore(req.StateDir)
	if err != nil {
		return nil, err
	}
	taskTxn, err := beginTasks(
		req.Context,
		filepath.Join(req.StateDir, "windows-product-entrypoint.lock"),
		req.LegacyWindowlessPath,
		req.ProductPath,
		store,
	)
	if err != nil {
		if cleanupErr := store.Close(); cleanupErr != nil {
			return nil, errors.Join(err, fmt.Errorf("UPGRADE_GUI_TASK_STORE_CLEANUP_UNCONFIRMED: constructor task store: %w", cleanupErr))
		}
		return nil, err
	}
	tasks := &windowsProductPairTaskOwner{txn: taskTxn, store: store}
	return &WindowsProductTxn{constructorStore: store, Opts: WindowsProductTxnOpts{
		StateDir: req.StateDir, ProductPath: req.ProductPath,
		LegacyWindowlessPath: req.LegacyWindowlessPath, StagedProduct: req.StagedProduct,
		ReceiptV3Path: filepath.Join(req.StateDir, UpgradeReceiptSchemaV3+".json"),
		ReceiptV2Path: filepath.Join(req.StateDir, UpgradeReceiptSchemaV2+".json"),
		Mode:          req.Mode, ReplacementAuthority: req.ReplacementAuthority, Tasks: tasks,
		Deps: WindowsProductTxnDeps{
			StartSupervisor: req.StartSupervisor, WaitSupervisorReady: req.WaitSupervisorReady,
			RestartPrior: req.RestartPrior, SettleSuccessor: req.SettleSuccessor,
			PublishCommitted: windowsProductEventPublisher(req.StateDir),
		},
	}}, nil
}

func windowsProductEventPublisher(stateDir string) func(WindowsProductCommitEvent) (string, error) {
	return func(event WindowsProductCommitEvent) (string, error) {
		log, err := api.OpenSupervisorEventLog(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
		if err != nil {
			return "", err
		}
		defer log.Close()
		prepared, err := api.PrepareSupervisorEvent(api.SupervisorEvent{
			SchemaVersion: api.SupervisorEventSchemaVersion,
			TS:            event.InstalledAt, Severity: api.SupervisorEventSeverityInfo,
			Source: api.SupervisorEventSourceMigration, Event: "windows-product-committed",
			Body: map[string]any{
				"schema": event.Schema, "transaction_id": event.TransactionID,
				"mode": string(event.Mode), "receipt_sha256": event.ReceiptSHA256,
				"artifact": map[string]any{
					"role": event.Artifact.Role, "subsystem": event.Artifact.Subsystem,
					"machine": event.Artifact.Machine,
					"version": event.Artifact.Version, "commit": event.Artifact.Commit,
					"build_date": event.Artifact.BuildDate, "sha256": event.Artifact.SHA256,
				},
				"task_set_sha256": event.TaskSetSHA256,
				"retired": map[string]any{
					"windowless_absent":                    event.Retired.WindowlessAbsent,
					"admission_helper_absent_from_payload": event.Retired.AdmissionHelperAbsentFromPayload,
					"rollback_images_absent":               event.Retired.RollbackImagesAbsent,
				},
			},
		})
		if err != nil {
			return "", err
		}
		digest, err := log.PersistPendingVerified(prepared)
		if err != nil {
			return digest, err
		}
		_ = log.TryReplayPending()
		return digest, nil
	}
}

type upgradeGUIProductOutcome string

const (
	upgradeGUIAlreadyCurrent         upgradeGUIProductOutcome = "UPGRADE_GUI_ALREADY_CURRENT"
	upgradeGUINewTransactionRequired upgradeGUIProductOutcome = "UPGRADE_GUI_NEW_TRANSACTION_REQUIRED"
)

type windowsProductReceiptEvidence struct {
	present                 bool
	match                   bool
	canonicalMatchesReceipt bool
	sourceMatchesCanonical  bool
	targetErr               error
	raw                     []byte
	receipt                 *UpgradeReceiptV3
}

// inspectWindowsProductReceiptEvidence shares the Product-owned read-only
// match with event reconciliation. Publication remains the existing writer.
func inspectWindowsProductReceiptEvidence(stateDir, sourceProduct, targetProduct string, mode WindowsProductPairMode, admit func(binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error)) (windowsProductReceiptEvidence, error) {
	raw, err := os.ReadFile(filepath.Join(stateDir, UpgradeReceiptSchemaV3+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return windowsProductReceiptEvidence{}, nil
	}
	if err != nil {
		return windowsProductReceiptEvidence{}, fmt.Errorf("read committed Windows product receipt: %w", err)
	}
	evidence := windowsProductReceiptEvidence{present: true}
	source, err := admit(binaryadmission.WindowsArtifact{
		Path: sourceProduct, Role: binaryadmission.WindowsArtifactRoleProduct,
	})
	if err != nil {
		return evidence, fmt.Errorf("admit source product for committed-event reconciliation: %w", err)
	}
	target, err := admit(binaryadmission.WindowsArtifact{
		Path: targetProduct, Role: binaryadmission.WindowsArtifactRoleProduct,
	})
	if err != nil {
		evidence.targetErr = err
		return evidence, nil
	}
	decoded, err := DecodeUpgradeReceipt(raw)
	if err != nil || decoded.V3 == nil {
		return evidence, fmt.Errorf("decode committed Windows product receipt: %w", err)
	}
	receipt := decoded.V3
	evidence.raw, evidence.receipt = raw, receipt
	evidence.sourceMatchesCanonical = sameWindowsProductIdentity(source, target)
	evidence.canonicalMatchesReceipt = target.SHA256 == receipt.Artifact.SHA256 &&
		target.Machine == receipt.Artifact.Machine &&
		target.Subsystem == receipt.Artifact.Subsystem &&
		target.Version == receipt.Artifact.Version &&
		target.Commit == receipt.Artifact.Commit &&
		target.BuildDate == receipt.Artifact.BuildDate
	evidence.match = windowsProductPairReceiptModeMatches(receipt.Mode, mode) && evidence.sourceMatchesCanonical && evidence.canonicalMatchesReceipt
	return evidence, nil
}

func inspectWindowsProductUpgradeReceiptWithAdmit(stateDir, sourceProduct, targetProduct string, admit func(binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error)) (upgradeGUIProductOutcome, error) {
	source, err := admit(binaryadmission.WindowsArtifact{Path: sourceProduct, Role: binaryadmission.WindowsArtifactRoleProduct})
	if err != nil {
		return "", fmt.Errorf("UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE: admit requested Product: %w", err)
	}
	target, err := admit(binaryadmission.WindowsArtifact{Path: targetProduct, Role: binaryadmission.WindowsArtifactRoleProduct})
	if err != nil {
		return "", fmt.Errorf("UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE: admit prior canonical Product: %w", err)
	}
	evidence, err := inspectWindowsProductReceiptEvidence(stateDir, sourceProduct, targetProduct, WindowsProductPairModeUpgrade, admit)
	if err != nil || evidence.targetErr != nil {
		return "", fmt.Errorf("UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE: receipt/canonical proof: %w", errors.Join(err, evidence.targetErr))
	}
	if evidence.match {
		return upgradeGUIAlreadyCurrent, nil
	}
	if evidence.present && !evidence.canonicalMatchesReceipt {
		return "", errors.New("UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE: prior canonical Product and V3 receipt disagree")
	}
	if !evidence.present && sameWindowsProductIdentity(source, target) {
		return "", errors.New("UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE: requested Product occupies canonical path without V3 receipt")
	}
	return upgradeGUINewTransactionRequired, nil
}

func inspectWindowsProductUpgradeReceipt(stateDir, sourceProduct, targetProduct string) (upgradeGUIProductOutcome, error) {
	return inspectWindowsProductUpgradeReceiptWithAdmit(stateDir, sourceProduct, targetProduct, binaryadmission.AdmitWindowsRole)
}

func reconcileWindowsProductReceipt(stateDir, sourceProduct, targetProduct string, mode WindowsProductPairMode) (bool, error) {
	evidence, err := inspectWindowsProductReceiptEvidence(stateDir, sourceProduct, targetProduct, mode, binaryadmission.AdmitWindowsRole)
	if err != nil || !evidence.match {
		return false, err
	}
	raw, receipt := evidence.raw, evidence.receipt
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	event := WindowsProductCommitEvent{
		Schema: receipt.Schema, TransactionID: receipt.TransactionID, Mode: receipt.Mode,
		Artifact: receipt.Artifact, TaskSetSHA256: receipt.TaskSetSHA256,
		Retired: receipt.Retired, ReceiptSHA256: digest, InstalledAt: receipt.InstalledAt,
	}
	if _, err := windowsProductEventPublisher(stateDir)(event); err != nil {
		return false, fmt.Errorf("%w: reconcile receipt_sha256=%s: %v", ErrWindowsProductPairCommittedObservabilityFailed, digest, err)
	}
	return true, nil
}

func reconcileWiredWindowsProductPairRecovery(req windowsProductPairRecoveryRequest) (*WindowsProductPairRecoverySettled, error) {
	if req.Context == nil {
		req.Context = context.Background()
	}
	if req.Deps.ReopenTasks == nil {
		req.Deps.ReopenTasks = func(ctx context.Context, recovery WindowsProductPairTaskRecovery) (WindowsProductPairTaskTxn, error) {
			store, err := reopenWindowsProductPairTaskStore(req.StateDir, recovery, true)
			if err != nil {
				return nil, err
			}
			backups := make([]scheduler.OwnedEntrypointTaskBackup, 0, len(recovery.Backups))
			for _, backup := range recovery.Backups {
				backups = append(backups, scheduler.OwnedEntrypointTaskBackup{TaskName: backup.TaskName, Ref: backup.Ref, SHA256: backup.SHA256})
			}
			txn, err := scheduler.ReopenOwnedEntrypointTaskTxnWithStore(
				ctx,
				filepath.Join(req.StateDir, "windows-product-pair-entrypoint.lock"),
				req.CLIPath,
				req.WindowlessPath,
				store,
				backups,
			)
			if err != nil {
				return nil, err
			}
			return &windowsProductPairTaskOwner{txn: txn, store: store}, nil
		}
	}
	if req.Deps.DiscardTaskStore == nil {
		req.Deps.DiscardTaskStore = func(recovery WindowsProductPairTaskRecovery) error {
			store, err := reopenWindowsProductPairTaskStore(req.StateDir, recovery, false)
			if err != nil {
				return err
			}
			return store.Close()
		}
	}
	if req.ReconcileCommitted == nil {
		req.ReconcileCommitted = func(journal windowsProductPairRecoveryJournal) error {
			raw, err := api.ReadStateFileInodeAnchored(journal.Receipt.Path)
			if err != nil {
				return fmt.Errorf("read recovered committed Windows product pair receipt: %w", err)
			}
			pair, err := binaryadmission.AdmitWindowsProductPair(
				binaryadmission.WindowsArtifact{Path: req.CLIPath, Role: binaryadmission.WindowsArtifactRoleCLI, SHA256: journal.CLI.NewSHA256},
				binaryadmission.WindowsArtifact{Path: req.WindowlessPath, Role: binaryadmission.WindowsArtifactRoleWindowless, SHA256: journal.Windowless.NewSHA256},
			)
			if err != nil {
				return fmt.Errorf("admit recovered committed Windows product pair: %w", err)
			}
			reconciled, err := reconcileWindowsProductPairCommit(raw, pair, pair, journal.Mode, windowsProductPairEventPublisher(req.StateDir))
			if err != nil {
				return err
			}
			if !reconciled {
				return errors.New("recovered committed Windows product pair receipt no longer matches its canonical pair")
			}
			return nil
		}
	}
	return reconcileWindowsProductPairRecovery(req)
}

func reconcileWiredWindowsProductRecovery(req windowsProductRecoveryRequest) (*WindowsProductPairRecoverySettled, error) {
	if req.Context == nil {
		req.Context = context.Background()
	}
	if req.Deps.ReopenTasks == nil {
		req.Deps.ReopenTasks = func(ctx context.Context, recovery WindowsProductPairTaskRecovery) (WindowsProductTaskTxn, error) {
			store, err := reopenWindowsProductPairTaskStore(req.StateDir, recovery, true)
			if err != nil {
				return nil, err
			}
			backups := make([]scheduler.OwnedEntrypointTaskBackup, 0, len(recovery.Backups))
			for _, backup := range recovery.Backups {
				backups = append(backups, scheduler.OwnedEntrypointTaskBackup{
					TaskName: backup.TaskName, Ref: backup.Ref, SHA256: backup.SHA256,
				})
			}
			txn, err := scheduler.ReopenOwnedEntrypointTaskTxnWithStore(
				ctx, filepath.Join(req.StateDir, "windows-product-entrypoint.lock"),
				req.LegacyWindowlessPath, req.ProductPath, store, backups,
			)
			if err != nil {
				return nil, err
			}
			return &windowsProductPairTaskOwner{txn: txn, store: store}, nil
		}
	}
	if req.Deps.DiscardTaskStore == nil {
		req.Deps.DiscardTaskStore = func(recovery WindowsProductPairTaskRecovery) error {
			store, err := reopenWindowsProductPairTaskStore(req.StateDir, recovery, false)
			if err != nil {
				return err
			}
			return store.Close()
		}
	}
	if req.Deps.PublishCommitted == nil {
		req.Deps.PublishCommitted = windowsProductEventPublisher(req.StateDir)
	}
	if req.ReconcileCommitted == nil {
		req.ReconcileCommitted = func(journal windowsProductRecoveryJournalV3) error {
			reconciled, err := reconcileWindowsProductReceipt(
				req.StateDir, req.ProductPath, req.ProductPath, journal.Mode,
			)
			if err != nil {
				return err
			}
			if !reconciled {
				return errors.New("recovered committed Windows product receipt no longer matches canonical product")
			}
			return nil
		}
	}
	return reconcileWindowsProductRecovery(req)
}

func windowsProductPairEventPublisher(stateDir string) func(WindowsProductPairCommitEvent) (string, error) {
	return func(event WindowsProductPairCommitEvent) (string, error) {
		log, err := api.OpenSupervisorEventLog(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
		if err != nil {
			return "", err
		}
		defer log.Close()
		prepared, err := api.PrepareSupervisorEvent(api.SupervisorEvent{
			SchemaVersion: api.SupervisorEventSchemaVersion,
			TS:            event.InstalledAt,
			Severity:      api.SupervisorEventSeverityInfo,
			Source:        api.SupervisorEventSourceMigration,
			Event:         "windows-product-pair-committed",
			Body: map[string]any{
				"schema": event.Schema, "mode": string(event.Mode), "receipt_schema": event.ReceiptSchema,
				"receipt_sha256": event.ReceiptSHA256, "version": event.Pair.CLI.Version,
				"commit": event.Pair.CLI.Commit, "build_date": event.Pair.CLI.BuildDate,
				"cli":        map[string]any{"role": event.Pair.CLI.Role, "subsystem": binaryadmission.WindowsCUISubsystem, "sha256": event.Pair.CLI.SHA256},
				"windowless": map[string]any{"role": event.Pair.Windowless.Role, "subsystem": binaryadmission.WindowsGUISubsystem, "sha256": event.Pair.Windowless.SHA256},
			},
		})
		if err != nil {
			return "", err
		}
		digest, err := log.PersistPendingVerified(prepared)
		if err != nil {
			return digest, err
		}
		_ = log.TryReplayPending()
		return digest, nil
	}
}

func reconcileWindowsProductPairCommit(raw []byte, source, target binaryadmission.WindowsProductPair, mode WindowsProductPairMode, publish func(WindowsProductPairCommitEvent) (string, error)) (bool, error) {
	decoded, err := DecodeUpgradeReceipt(raw)
	if err != nil || decoded.V2 == nil {
		return false, fmt.Errorf("decode committed Windows product pair receipt: %w", err)
	}
	if !windowsProductPairReceiptModeMatches(decoded.V2.Mode, mode) {
		return false, nil
	}
	if !sameProductPairIdentity(source, target) || !sameProductPairIdentity(target, decoded.V2.Artifacts) {
		return false, nil
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	event := WindowsProductPairCommitEvent{
		Schema: WindowsProductPairCommittedSchemaV1, Mode: mode, ReceiptSchema: UpgradeReceiptSchemaV2,
		ReceiptSHA256: digest, InstalledAt: decoded.V2.InstalledAt, Pair: target,
	}
	if _, err := publish(event); err != nil {
		return false, fmt.Errorf("%w: reconcile receipt_sha256=%s: %v", ErrWindowsProductPairCommittedObservabilityFailed, digest, err)
	}
	return true, nil
}

type windowsProductPairTaskOwner struct {
	txn   *scheduler.OwnedEntrypointTaskTxn
	store *windowsProductPairTaskStore
}

func (o *windowsProductPairTaskOwner) InventoryExport() error { return o.txn.InventoryExport() }
func (o *windowsProductPairTaskOwner) RecoveryDescriptor() (WindowsProductPairTaskRecovery, error) {
	backups := o.txn.Backups()
	converted := make([]WindowsProductPairTaskBackup, 0, len(backups))
	for _, backup := range backups {
		converted = append(converted, WindowsProductPairTaskBackup{TaskName: backup.TaskName, Ref: backup.Ref, SHA256: backup.SHA256})
	}
	return WindowsProductPairTaskRecovery{StoreRoot: o.store.root, Backups: converted}, nil
}
func (o *windowsProductPairTaskOwner) RewriteCommand() error { return o.txn.RewriteCommand() }
func (o *windowsProductPairTaskOwner) VerifyRuntime() error  { return o.txn.VerifyRuntime() }
func (o *windowsProductPairTaskOwner) VerifyRuntimeSHA256() (string, error) {
	return o.txn.VerifyRuntimeSHA256()
}
func (o *windowsProductPairTaskOwner) RestoreImport() error        { return o.txn.RestoreImport() }
func (o *windowsProductPairTaskOwner) DiscardRecoveryStore() error { return o.store.Close() }
func (o *windowsProductPairTaskOwner) Close() error                { return o.txn.Close() }

// windowsProductPairTaskStore keeps exact task XML only for the lifetime of one
// pair transaction. Writes and reads use the hardened state-file owner; Close
// removes only the exact leaves created by this instance.
type windowsProductPairTaskStore struct {
	mu     sync.Mutex
	root   string
	hashes map[string]string
	closed bool
}

func newWindowsProductPairTaskStore(stateDir string) (*windowsProductPairTaskStore, error) {
	root, err := os.MkdirTemp(stateDir, "windows-product-pair-task-rollback-")
	if err != nil {
		return nil, fmt.Errorf("create Windows product pair task rollback store: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		_ = os.Remove(root)
		return nil, fmt.Errorf("protect Windows product pair task rollback store: %w", err)
	}
	return &windowsProductPairTaskStore{root: root, hashes: make(map[string]string)}, nil
}

func reopenWindowsProductPairTaskStore(stateDir string, recovery WindowsProductPairTaskRecovery, requireRoot bool) (*windowsProductPairTaskStore, error) {
	root := filepath.Clean(recovery.StoreRoot)
	if !sameWindowsRecoveryPath(filepath.Dir(root), filepath.Clean(stateDir)) || !strings.HasPrefix(filepath.Base(root), "windows-product-pair-task-rollback-") {
		return nil, errors.New("Windows product pair task rollback store is outside the exact state-directory owner")
	}
	if info, err := os.Lstat(root); err != nil {
		if requireRoot || !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect retained task rollback store: %w", err)
		}
	} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("retained task rollback store is not an ordinary directory")
	}
	hashes := make(map[string]string, len(recovery.Backups))
	for _, backup := range recovery.Backups {
		if backup.Ref == "" || filepath.Base(backup.Ref) != backup.Ref || strings.ContainsAny(backup.Ref, `/\\`) || backup.SHA256 == "" {
			return nil, errors.New("retained task rollback descriptor contains an invalid opaque ref")
		}
		if prior, exists := hashes[backup.Ref]; exists && prior != backup.SHA256 {
			return nil, errors.New("retained task rollback descriptor repeats an opaque ref with different identity")
		}
		hashes[backup.Ref] = backup.SHA256
	}
	return &windowsProductPairTaskStore{root: root, hashes: hashes}, nil
}

func (s *windowsProductPairTaskStore) Retain(ctx context.Context, xml []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(xml))
	ref := "task-" + digest + ".xml"
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errors.New("Windows product pair task rollback store is closed")
	}
	if err := api.WriteStateFileBytesAtomic(filepath.Join(s.root, ref), xml); err != nil {
		return "", err
	}
	readBack, err := api.ReadStateFileBeneathRootNoFollow(ctx, s.root, []string{ref}, digest)
	if err != nil || !bytes.Equal(readBack, xml) {
		if err == nil {
			err = errors.New("retained task XML readback differs")
		}
		return "", err
	}
	s.hashes[ref] = digest
	return ref, nil
}

func (s *windowsProductPairTaskStore) Load(ctx context.Context, ref string) ([]byte, error) {
	s.mu.Lock()
	digest, ok := s.hashes[ref]
	closed := s.closed
	s.mu.Unlock()
	if closed || !ok {
		return nil, errors.New("Windows product pair task rollback ref is unavailable")
	}
	return api.ReadStateFileBeneathRootNoFollow(ctx, s.root, []string{ref}, digest)
}

func (s *windowsProductPairTaskStore) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	refs := make([]string, 0, len(s.hashes))
	for ref := range s.hashes {
		refs = append(refs, ref)
	}
	s.mu.Unlock()
	var errs []error
	for _, ref := range refs {
		for _, leaf := range []string{ref, ref + ".lock"} {
			if err := os.Remove(filepath.Join(s.root, leaf)); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	if err := os.Remove(s.root); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

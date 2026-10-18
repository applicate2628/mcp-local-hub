//go:build windows

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
)

type WindowsProductPairRecoverySettled struct {
	TransactionID     string
	Outcome           WindowsProductPairRecoveryPhase
	PairOrPriorHashes [2]string
	Runtime           *pendingRuntimeReceiving
}

type windowsProductPairRecoveryRequest struct {
	Context            context.Context
	StateDir           string
	CLIPath            string
	WindowlessPath     string
	SettleSuccessor    func(string) error
	ObservePriorReady  func(context.Context, string, string) (bool, error)
	RestartPrior       func(string) (SupervisorLaunch, error)
	WaitPriorReady     func(context.Context, string, string) error
	ReconcileCommitted func(windowsProductPairRecoveryJournal) error
	Deps               windowsProductPairRecoveryDeps
}

type windowsProductPairRecoveryDeps struct {
	ReadJournal      func(string) ([]byte, error)
	WriteJournal     func(string, []byte) error
	RemoveJournal    func(string) error
	ReadReceipt      func(string) ([]byte, error)
	WriteReceipt     func(string, []byte) error
	RemoveReceipt    func(string) error
	ListAsides       func(string) ([]string, error)
	HashFile         func(string) (string, error)
	AdmitPair        func(binaryadmission.WindowsArtifact, binaryadmission.WindowsArtifact) (binaryadmission.WindowsProductPair, error)
	ReopenTasks      func(context.Context, WindowsProductPairTaskRecovery) (WindowsProductPairTaskTxn, error)
	DiscardTaskStore func(WindowsProductPairTaskRecovery) error
	RestorePromotion func(WindowsProductPairPromotion) (string, error)
	RestoreNoReplace func(string, string) error
	RemoveExact      func(string) error
}

type windowsProductRecoveryRequest struct {
	Context                context.Context
	StateDir               string
	ProductPath            string
	LegacyWindowlessPath   string
	Legacy                 windowsProductPairRecoveryRequest
	RecoverPriorSettlement func(context.Context, windowsProductRecoveryJournalV3) error
	PreparePendingRuntime  func(context.Context, windowsProductRecoveryJournalV3, *pendingRuntimeReceiving) (func(context.Context, windowsProductRecoveryJournalV3) error, error)
	PendingRuntime         *pendingRuntimeReceiving
	ObserveSuccessorReady  func(context.Context, string, string) (bool, error)
	StartSupervisor        func(string) (SupervisorLaunch, error)
	WaitSupervisorReady    func(context.Context, string, string) error
	ReconcileCommitted     func(windowsProductRecoveryJournalV3) error
	Deps                   windowsProductRecoveryDeps
	// Optional command admission controls; legacy recovery retains its contract.
	ExpectedJournalSHA256                   string
	RequirePriorSettlementOnSettledRollback bool
}

type windowsProductRecoveryDeps struct {
	ReadJournal      func(string) ([]byte, error)
	WriteJournal     func(string, []byte) error
	RemoveJournal    func(string) error
	ReadReceipt      func(string) ([]byte, error)
	WriteReceipt     func(string, []byte) error
	RemoveReceipt    func(string) error
	ListAsides       func(string) ([]string, error)
	HashFile         func(string) (string, error)
	AdmitProduct     func(binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error)
	ReopenTasks      func(context.Context, WindowsProductPairTaskRecovery) (WindowsProductTaskTxn, error)
	DiscardTaskStore func(WindowsProductPairTaskRecovery) error
	RestorePromotion func(WindowsProductPairPromotion) (string, error)
	RestoreNoReplace func(string, string) error
	RemoveExact      func(string) error
	PublishCommitted func(WindowsProductCommitEvent) (string, error)
	Now              func() time.Time
}

// The recovery owner, shared by stored-pending admission and execution.
func windowsProductPriorRecoveryPending(j windowsProductRecoveryJournalV3) bool {
	if j.Mode != WindowsProductPairModeUpgrade || j.PreSettlement == nil {
		return false
	}
	switch j.Phase {
	case WindowsProductRecoveryPrepared, WindowsProductRecoveryGUIOwnedStopPending, WindowsProductRecoverySuccessorStarting, WindowsProductRecoverySettledRollback:
		return true
	default:
		return false
	}
}

func reconcileWindowsProductRecovery(req windowsProductRecoveryRequest) (result *WindowsProductPairRecoverySettled, retErr error) {
	if req.Context == nil {
		req.Context = context.Background()
	}
	if !cleanAbsolutePath(req.StateDir) || !cleanAbsolutePath(req.ProductPath) || !cleanAbsolutePath(req.LegacyWindowlessPath) {
		return nil, fmt.Errorf("%w: state and canonical product paths are required", ErrWindowsProductRecoveryIdentity)
	}
	if req.RequirePriorSettlementOnSettledRollback && !validLowerHexSHA256(req.ExpectedJournalSHA256) {
		return nil, fmt.Errorf("%w: pending command requires an admitted V3 journal digest", ErrWindowsProductRecoveryIdentity)
	}
	d := withWindowsProductRecoveryDefaults(req.Deps)
	v3Path := filepath.Join(req.StateDir, WindowsProductRecoveryFileLeafV3)
	legacyPath := filepath.Join(req.StateDir, WindowsProductPairRecoveryFileLeaf)
	_, v3Err := d.ReadJournal(v3Path)
	_, legacyErr := d.ReadJournal(legacyPath)
	v3Present := v3Err == nil
	legacyPresent := legacyErr == nil
	if req.ExpectedJournalSHA256 != "" {
		if !validLowerHexSHA256(req.ExpectedJournalSHA256) || !v3Present || legacyPresent || (legacyErr != nil && !errors.Is(legacyErr, os.ErrNotExist)) {
			return nil, fmt.Errorf("%w: expected V3 journal admission is unavailable or mixed", ErrWindowsProductRecoveryIdentity)
		}
	}
	if v3Present && legacyPresent {
		return nil, fmt.Errorf("%w: legacy and V3 recovery journals coexist", ErrWindowsProductRecoveryIdentity)
	}
	if !v3Present && v3Err != nil && !errors.Is(v3Err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: read V3 journal: %v", ErrWindowsProductRecoveryIdentity, v3Err)
	}
	if legacyPresent {
		if _, err := reconcileWindowsProductPairRecovery(req.Legacy); err != nil {
			return nil, err
		}
		_, err := d.ReadJournal(legacyPath)
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: legacy recovery journal did not settle", ErrWindowsProductRecoveryIdentity)
		}
		return nil, nil
	}
	if !v3Present {
		return nil, nil
	}
	raw, err := d.ReadJournal(v3Path)
	if err != nil {
		return nil, fmt.Errorf("%w: read V3 journal: %v", ErrWindowsProductRecoveryIdentity, err)
	}
	if req.ExpectedJournalSHA256 != "" {
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != req.ExpectedJournalSHA256 {
			return nil, fmt.Errorf("%w: expected V3 journal changed before recovery", ErrWindowsProductRecoveryIdentity)
		}
	}
	journal, err := decodeWindowsProductRecoveryJournalV3(raw)
	if err != nil || !sameWindowsRecoveryPath(journal.Product.Target, req.ProductPath) ||
		!sameWindowsRecoveryPath(journal.LegacyWindowless.Target, req.LegacyWindowlessPath) ||
		!sameWindowsRecoveryPath(journal.Receipts.V3.Path, filepath.Join(req.StateDir, UpgradeReceiptSchemaV3+".json")) ||
		!sameWindowsRecoveryPath(journal.Receipts.V2.Path, filepath.Join(req.StateDir, UpgradeReceiptSchemaV2+".json")) {
		return nil, fmt.Errorf("%w: V3 journal schema or resolved target mismatch", ErrWindowsProductRecoveryIdentity)
	}
	// Stored pre-decision GUI-owned recovery binds current desired state before
	// task opening or material inverse. Immediate rollback never enters this hook.
	if req.PreparePendingRuntime != nil && windowsProductPriorRecoveryPending(journal) && journal.PreSettlement.GUIOwner == windowsProductGUIOwnerConfirmedPresent {
		if req.PendingRuntime == nil {
			req.PendingRuntime = new(pendingRuntimeReceiving)
		}
		defer func() {
			if result != nil {
				snapshot := *req.PendingRuntime
				result.Runtime = &snapshot
			}
			if retErr != nil {
				var typed *pendingRuntimeError
				if !errors.As(retErr, &typed) {
					var kind pendingRuntimeCause
					if errors.Is(retErr, ErrWindowsProductRecoveryIdentity) || errors.Is(retErr, ErrWindowsProductPairRecoveryRollback) {
						kind = pendingRuntimeMaterial
					}
					retErr = pendingRuntimeFailure(kind, req.PendingRuntime, retErr)
				}
			}
		}()
		settle, err := req.PreparePendingRuntime(req.Context, journal, req.PendingRuntime)
		if err != nil {
			return nil, err
		}
		if settle == nil {
			return nil, pendingRuntimeFailure(pendingRuntimeUnavailable, req.PendingRuntime, nil)
		}
		req.RecoverPriorSettlement = settle
		if journal.Phase == WindowsProductRecoverySettledRollback {
			req.RequirePriorSettlementOnSettledRollback = true
		}
	}
	if journal.Phase == WindowsProductRecoverySettledCommit || journal.Phase == WindowsProductRecoverySettledRollback {
		if journal.Phase == WindowsProductRecoverySettledRollback && req.RequirePriorSettlementOnSettledRollback {
			if req.RecoverPriorSettlement == nil {
				return nil, fmt.Errorf("%w: settled rollback prior health owner is unavailable", ErrWindowsProductRecoveryPending)
			}
			if err := req.RecoverPriorSettlement(req.Context, journal); err != nil {
				return nil, err
			}
		}
		if journal.Phase == WindowsProductRecoverySettledCommit {
			if req.ReconcileCommitted == nil {
				return nil, errors.New("committed product recovery reconciliation owner is unavailable")
			}
			if err := req.ReconcileCommitted(journal); err != nil {
				return nil, err
			}
		}
		if d.DiscardTaskStore == nil {
			return nil, fmt.Errorf("%w: retained task-store cleanup owner is unavailable", ErrWindowsProductPairRecoveryCleanup)
		}
		if err := d.DiscardTaskStore(journal.Tasks); err != nil {
			return nil, fmt.Errorf("%w: discard retained task store: %w", ErrWindowsProductPairRecoveryCleanup, err)
		}
		if err := d.RemoveJournal(v3Path); err != nil {
			return nil, fmt.Errorf("%w: remove settled V3 journal: %v", ErrWindowsProductPairRecoveryCleanup, err)
		}
		outcome := WindowsProductPairRecoverySettledRollback
		if journal.Phase == WindowsProductRecoverySettledCommit {
			outcome = WindowsProductPairRecoverySettledCommit
		}
		return &WindowsProductPairRecoverySettled{TransactionID: journal.TransactionID, Outcome: outcome}, nil
	}
	if d.ReopenTasks == nil || d.DiscardTaskStore == nil {
		return nil, fmt.Errorf("%w: retained task recovery owner is unavailable", ErrWindowsProductRecoveryIdentity)
	}
	tasks, err := d.ReopenTasks(req.Context, journal.Tasks)
	if err != nil {
		return nil, fmt.Errorf("%w: reopen retained task transaction: %w", ErrWindowsProductRecoveryIdentity, err)
	}
	defer func() { retErr = errors.Join(retErr, tasks.Close()) }()
	if journal.Phase == WindowsProductRecoveryCommitDecided {
		return completeWindowsProductRecoveryForward(req, d, v3Path, journal, tasks)
	}
	return rollbackWindowsProductRecovery(req, d, v3Path, journal, tasks)
}

func rollbackWindowsProductRecovery(req windowsProductRecoveryRequest, d windowsProductRecoveryDeps, journalPath string, journal windowsProductRecoveryJournalV3, tasks WindowsProductTaskTxn) (*WindowsProductPairRecoverySettled, error) {
	productArtifact := windowsProductPairRecoveryArtifact{
		Target: journal.Product.Target, PriorPresent: journal.Product.PriorPresent,
		PriorSHA256: journal.Product.PriorSHA256, NewSHA256: journal.Product.NewSHA256,
		BaselineAsideBasenames: recoveryAsideBasenames(journal.Product.BaselineAsides),
	}
	productState, err := inspectWindowsProductPairRecoveryArtifact(productArtifact, pairRecoveryDepsFromProduct(d))
	if err != nil {
		return nil, fmt.Errorf("%w: canonical product: %v", ErrWindowsProductRecoveryIdentity, err)
	}
	legacyArtifact := windowsProductPairRecoveryArtifact{
		Target: journal.LegacyWindowless.Target, PriorPresent: journal.LegacyWindowless.PriorPresent,
		PriorSHA256:            journal.LegacyWindowless.PriorSHA256,
		BaselineAsideBasenames: recoveryAsideBasenames(journal.LegacyWindowless.BaselineAsides),
	}
	legacyState, err := inspectWindowsProductLegacyRecoveryArtifact(legacyArtifact, d)
	if err != nil {
		return nil, fmt.Errorf("%w: legacy windowless: %v", ErrWindowsProductRecoveryIdentity, err)
	}
	var restoreErrs []error
	guiOwnedPredecision := journal.PreSettlement != nil &&
		journal.PreSettlement.GUIOwner == windowsProductGUIOwnerConfirmedPresent
	if guiOwnedPredecision && req.RecoverPriorSettlement == nil {
		restoreErrs = append(restoreErrs, fmt.Errorf("%w: GUI-owned prior fleet settlement owner is unavailable", ErrWindowsProductRecoveryPending))
	}
	if journal.Mode == WindowsProductPairModeUpgrade &&
		journal.Phase == WindowsProductRecoverySuccessorStarting &&
		req.RecoverPriorSettlement == nil {
		restoreErrs = append(restoreErrs, errors.New("prior settlement recovery owner is unavailable"))
	}
	if err := restoreWindowsProductPairRecoveryArtifact(productArtifact, productState, pairRecoveryDepsFromProduct(d)); err != nil {
		restoreErrs = append(restoreErrs, fmt.Errorf("restore canonical product: %w", err))
	}
	if err := tasks.RestoreImport(); err != nil {
		restoreErrs = append(restoreErrs, fmt.Errorf("restore task XML: %w", err))
	}
	if err := restoreWindowsProductLegacyRecoveryArtifact(legacyArtifact, legacyState, d); err != nil {
		restoreErrs = append(restoreErrs, fmt.Errorf("restore legacy windowless: %w", err))
	}
	if err := restoreWindowsProductPairRecoveryReceipt(journal.Receipts.V3, pairRecoveryDepsFromProduct(d)); err != nil {
		restoreErrs = append(restoreErrs, fmt.Errorf("restore V3 receipt: %w", err))
	}
	if err := restoreWindowsProductPairRecoveryReceipt(journal.Receipts.V2, pairRecoveryDepsFromProduct(d)); err != nil {
		restoreErrs = append(restoreErrs, fmt.Errorf("restore V2 receipt: %w", err))
	}
	if req.RecoverPriorSettlement != nil {
		if err := req.RecoverPriorSettlement(req.Context, journal); err != nil {
			if guiOwnedPredecision {
				restoreErrs = append(restoreErrs, fmt.Errorf("%w: GUI-owned prior fleet settlement: %w", ErrWindowsProductRecoveryPending, err))
			} else {
				restoreErrs = append(restoreErrs, err)
			}
		}
	}
	if joined := errors.Join(restoreErrs...); joined != nil {
		return nil, fmt.Errorf("%w: %w", ErrWindowsProductPairRecoveryRollback, joined)
	}
	journal.Phase = WindowsProductRecoverySettledRollback
	journal.TaskSetSHA256 = ""
	if err := persistWindowsProductRecoveryJournalV3(journalPath, journal, d.WriteJournal, d.ReadJournal); err != nil {
		return nil, fmt.Errorf("%w: persist settled rollback: %v", ErrWindowsProductPairRecoveryCleanup, err)
	}
	if err := d.DiscardTaskStore(journal.Tasks); err != nil {
		return nil, fmt.Errorf("%w: discard retained task store: %w", ErrWindowsProductPairRecoveryCleanup, err)
	}
	if err := d.RemoveJournal(journalPath); err != nil {
		return nil, fmt.Errorf("%w: remove settled V3 journal: %v", ErrWindowsProductPairRecoveryCleanup, err)
	}
	return &WindowsProductPairRecoverySettled{TransactionID: journal.TransactionID, Outcome: WindowsProductPairRecoverySettledRollback}, nil
}

// Read-only pending-command admission reuses the recovery owner's physical
// preimage and task validators. It never restores, discards or starts anything.
func validateWindowsProductPendingRecovery(req windowsProductRecoveryRequest, j windowsProductRecoveryJournalV3) (retErr error) {
	if req.Context == nil {
		return fmt.Errorf("%w: recovery context is unavailable", ErrWindowsProductRecoveryIdentity)
	}
	if err := req.Context.Err(); err != nil {
		return err
	}
	d := withWindowsProductRecoveryDefaults(req.Deps)
	product := windowsProductPairRecoveryArtifact{Target: j.Product.Target, PriorPresent: j.Product.PriorPresent, PriorSHA256: j.Product.PriorSHA256, NewSHA256: j.Product.NewSHA256, BaselineAsideBasenames: recoveryAsideBasenames(j.Product.BaselineAsides)}
	state, err := inspectWindowsProductPairRecoveryArtifact(product, pairRecoveryDepsFromProduct(d))
	if err != nil {
		return fmt.Errorf("%w: prior Product inverse: %w", ErrWindowsProductRecoveryIdentity, err)
	}
	legacy := windowsProductPairRecoveryArtifact{Target: j.LegacyWindowless.Target, PriorPresent: j.LegacyWindowless.PriorPresent, PriorSHA256: j.LegacyWindowless.PriorSHA256, BaselineAsideBasenames: recoveryAsideBasenames(j.LegacyWindowless.BaselineAsides)}
	if _, err := inspectWindowsProductLegacyRecoveryArtifact(legacy, d); err != nil {
		return fmt.Errorf("%w: legacy inverse: %w", ErrWindowsProductRecoveryIdentity, err)
	}
	if j.Product.PriorPresent {
		path := j.Product.Target
		if state.canonical != j.Product.PriorSHA256 {
			path = state.priorAside
		}
		prior, err := d.AdmitProduct(binaryadmission.WindowsArtifact{Path: path, Role: binaryadmission.WindowsArtifactRoleProduct, SHA256: j.Product.PriorSHA256})
		if err != nil || prior.SHA256 != j.Product.PriorSHA256 || prior.Role != binaryadmission.WindowsArtifactRoleProduct || prior.Subsystem != binaryadmission.WindowsGUISubsystem || prior.Machine != 0x8664 {
			return fmt.Errorf("%w: prior Product admission differs", ErrWindowsProductRecoveryIdentity)
		}
	}
	if j.Phase == WindowsProductRecoverySettledRollback {
		return readbackGUIOwnedPriorProduct(j)
	}
	if d.ReopenTasks == nil {
		return fmt.Errorf("%w: task inverse validation owner unavailable", ErrWindowsProductRecoveryIdentity)
	}
	tasks, err := d.ReopenTasks(req.Context, j.Tasks)
	if err != nil {
		return fmt.Errorf("%w: task inverse validation: %w", ErrWindowsProductRecoveryIdentity, err)
	}
	if tasks == nil {
		return fmt.Errorf("%w: task inverse validation returned no owner", ErrWindowsProductRecoveryIdentity)
	}
	// Close releases only the reopened task transaction lock. DiscardRecoveryStore
	// is intentionally not invoked: retained XML remains recovery-owned.
	return tasks.Close()
}

func completeWindowsProductRecoveryForward(req windowsProductRecoveryRequest, d windowsProductRecoveryDeps, journalPath string, journal windowsProductRecoveryJournalV3, tasks WindowsProductTaskTxn) (*WindowsProductPairRecoverySettled, error) {
	product, err := d.AdmitProduct(binaryadmission.WindowsArtifact{
		Path: journal.Product.Target, Role: binaryadmission.WindowsArtifactRoleProduct, SHA256: journal.Product.NewSHA256,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: canonical product identity: %v", ErrWindowsProductCleanupPending, err)
	}
	digest, err := tasks.VerifyRuntimeSHA256()
	if err != nil || digest != journal.TaskSetSHA256 {
		return nil, fmt.Errorf("%w: task-set readback differs: %v", ErrWindowsProductCleanupPending, err)
	}
	if _, err := os.Stat(journal.LegacyWindowless.Target); !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: legacy windowless target is not absent: %v", ErrWindowsProductCleanupPending, err)
	}
	if req.ObserveSuccessorReady == nil || req.StartSupervisor == nil || req.WaitSupervisorReady == nil {
		return nil, fmt.Errorf("%w: successor readiness owner is unavailable", ErrWindowsProductCleanupPending)
	}
	ready, err := req.ObserveSuccessorReady(req.Context, journal.Product.Target, journal.Product.NewSHA256)
	if err != nil {
		return nil, fmt.Errorf("%w: observe successor readiness: %v", ErrWindowsProductCleanupPending, err)
	}
	if !ready {
		launch, err := req.StartSupervisor(journal.Product.Target)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("%w: restart committed successor: %v", ErrWindowsProductCleanupPending, err), abortStartedSupervisor(launch))
		}
		if err := receiveStartedSupervisor(req.Context, launch, supervisorExpectation(req.StateDir, journal.Product.Target, journal.Product.NewSHA256), func() error {
			return req.WaitSupervisorReady(req.Context, journal.Product.Target, journal.Product.NewSHA256)
		}); err != nil {
			return nil, fmt.Errorf("%w: committed successor readiness/control: %w", ErrWindowsProductCleanupPending, err)
		}
	}
	productPriorAside, err := inspectWindowsProductForwardAsides(journal.Product.Target,
		journal.Product.PriorPresent, journal.Product.PriorSHA256, journal.Product.BaselineAsides, d)
	if err != nil {
		return nil, fmt.Errorf("%w: product asides: %v", ErrWindowsProductCleanupPending, err)
	}
	legacyPriorAside, err := inspectWindowsProductForwardAsides(journal.LegacyWindowless.Target,
		journal.LegacyWindowless.PriorPresent, journal.LegacyWindowless.PriorSHA256, journal.LegacyWindowless.BaselineAsides, d)
	if err != nil {
		return nil, fmt.Errorf("%w: legacy asides: %v", ErrWindowsProductCleanupPending, err)
	}
	productTxn := &WindowsProductTxn{Opts: WindowsProductTxnOpts{
		StateDir: req.StateDir, ProductPath: req.ProductPath, LegacyWindowlessPath: req.LegacyWindowlessPath,
		StagedProduct: journal.Staged.Path, ReceiptV3Path: journal.Receipts.V3.Path, ReceiptV2Path: journal.Receipts.V2.Path,
		Mode: journal.Mode, Tasks: tasks,
	}}
	productTxn.prepared = &windowsProductPreparedTxn{
		deps: withWindowsProductDefaults(WindowsProductTxnDeps{
			AdmitProduct: d.AdmitProduct, RestorePromotion: d.RestorePromotion,
			WriteReceipt: d.WriteReceipt, ReadReceipt: d.ReadReceipt, RemoveReceipt: d.RemoveReceipt,
			WriteRecoveryJournal: d.WriteJournal, ReadRecoveryJournal: d.ReadJournal,
			RemoveRecoveryJournal: d.RemoveJournal, ListGeneratedAsides: d.ListAsides,
			HashFile: d.HashFile, RemoveExact: d.RemoveExact, PublishCommitted: d.PublishCommitted, Now: d.Now,
		}),
		staged: product, journal: journal,
		productPromotion: &WindowsProductPairPromotion{
			Target: journal.Product.Target, RetainedPrior: productPriorAside,
			PriorPresent: journal.Product.PriorPresent, NewSHA256: journal.Product.NewSHA256,
		},
		legacyRetained: legacyPriorAside,
	}
	committed, err := productTxn.finishProductCommit(req.Context, product, true)
	if err != nil {
		return nil, err
	}
	if committed.Outcome != WindowsProductPairCommittedOutcome {
		return nil, fmt.Errorf("%w: forward completion outcome %q", ErrWindowsProductCleanupPending, committed.Outcome)
	}
	return &WindowsProductPairRecoverySettled{TransactionID: journal.TransactionID, Outcome: WindowsProductPairRecoverySettledCommit}, nil
}

// A validated commit_decided journal may outlive any individual cleanup removal.
// Only its present, hash-bound asides are replayed; absence is not rollback evidence.
func inspectWindowsProductForwardAsides(target string, priorPresent bool, priorSHA string,
	baselineAsides []windowsProductRecoveryAsideV3, d windowsProductRecoveryDeps) (string, error) {
	current, err := d.ListAsides(target)
	if err != nil {
		return "", err
	}
	baseline := make(map[string]string, len(baselineAsides))
	for _, aside := range baselineAsides {
		baseline[aside.Basename] = aside.SHA256
	}
	seen := make(map[string]bool, len(current))
	priorAside := ""
	for _, name := range current {
		path := filepath.Join(filepath.Dir(target), name)
		if seen[name] || !api.IsGeneratedRenameAsideForTarget(target, path) {
			return "", errors.New("duplicate or unrecognized generated aside")
		}
		seen[name] = true
		actual, err := d.HashFile(path)
		if err != nil {
			return "", err
		}
		if expected, ok := baseline[name]; ok {
			if actual != expected {
				return "", errors.New("journaled baseline aside hash differs")
			}
			continue
		}
		if !priorPresent || actual != priorSHA || priorAside != "" {
			return "", errors.New("unrecognized or duplicate post-journal generated aside")
		}
		priorAside = path
	}
	return priorAside, nil
}

func recoveryAsideBasenames(asides []windowsProductRecoveryAsideV3) []string {
	names := make([]string, 0, len(asides))
	for _, aside := range asides {
		names = append(names, aside.Basename)
	}
	return names
}

func pairRecoveryDepsFromProduct(d windowsProductRecoveryDeps) windowsProductPairRecoveryDeps {
	return windowsProductPairRecoveryDeps{
		ReadReceipt: d.ReadReceipt, WriteReceipt: d.WriteReceipt, RemoveReceipt: d.RemoveReceipt,
		ListAsides: d.ListAsides, HashFile: d.HashFile, RestorePromotion: d.RestorePromotion,
		RestoreNoReplace: d.RestoreNoReplace, RemoveExact: d.RemoveExact,
	}
}

func inspectWindowsProductLegacyRecoveryArtifact(a windowsProductPairRecoveryArtifact, d windowsProductRecoveryDeps) (windowsProductPairRecoveryArtifactState, error) {
	state := windowsProductPairRecoveryArtifactState{}
	hash, err := d.HashFile(a.Target)
	if errors.Is(err, os.ErrNotExist) {
		hash = ""
	} else if err != nil {
		return state, err
	}
	if hash != "" && hash != a.PriorSHA256 {
		return state, errors.New("legacy target differs from recorded preimage")
	}
	state.canonical = hash
	current, err := d.ListAsides(a.Target)
	if err != nil {
		return state, err
	}
	baseline := make(map[string]struct{}, len(a.BaselineAsideBasenames))
	for _, name := range a.BaselineAsideBasenames {
		baseline[name] = struct{}{}
	}
	for _, name := range current {
		if _, ok := baseline[name]; ok {
			continue
		}
		path := filepath.Join(filepath.Dir(a.Target), name)
		digest, err := d.HashFile(path)
		if err != nil || digest != a.PriorSHA256 || state.priorAside != "" {
			return state, errors.New("ambiguous legacy retained aside")
		}
		state.priorAside = path
	}
	if a.PriorPresent {
		if state.canonical == "" && state.priorAside == "" {
			return state, errors.New("legacy preimage is missing")
		}
		if state.canonical != "" && state.priorAside != "" {
			return state, errors.New("legacy target and retained aside coexist")
		}
	} else if state.canonical != "" || state.priorAside != "" {
		return state, errors.New("prior-absent legacy target has residue")
	}
	return state, nil
}

func restoreWindowsProductLegacyRecoveryArtifact(a windowsProductPairRecoveryArtifact, state windowsProductPairRecoveryArtifactState, d windowsProductRecoveryDeps) error {
	if !a.PriorPresent {
		if state.canonical != "" || state.priorAside != "" {
			return errors.New("prior-absent legacy target has residue")
		}
		return nil
	}
	if state.canonical == "" {
		if state.priorAside == "" {
			return errors.New("legacy retained preimage is missing")
		}
		if err := d.RestoreNoReplace(state.priorAside, a.Target); err != nil {
			return err
		}
	}
	digest, err := d.HashFile(a.Target)
	if err != nil || digest != a.PriorSHA256 {
		return errors.New("restored legacy hash differs")
	}
	return nil
}

func withWindowsProductRecoveryDefaults(d windowsProductRecoveryDeps) windowsProductRecoveryDeps {
	product := withWindowsProductDefaults(WindowsProductTxnDeps{
		WriteReceipt: d.WriteReceipt, ReadReceipt: d.ReadReceipt, RemoveReceipt: d.RemoveReceipt,
		WriteRecoveryJournal: d.WriteJournal, ReadRecoveryJournal: d.ReadJournal,
		RemoveRecoveryJournal: d.RemoveJournal, ListGeneratedAsides: d.ListAsides,
		HashFile: d.HashFile, RemoveExact: d.RemoveExact, RestorePromotion: d.RestorePromotion,
		RestoreNoReplace: d.RestoreNoReplace, PublishCommitted: d.PublishCommitted, Now: d.Now,
	})
	d.ReadJournal, d.WriteJournal, d.RemoveJournal = product.ReadRecoveryJournal, product.WriteRecoveryJournal, product.RemoveRecoveryJournal
	d.ReadReceipt, d.WriteReceipt, d.RemoveReceipt = product.ReadReceipt, product.WriteReceipt, product.RemoveReceipt
	d.ListAsides, d.HashFile, d.RemoveExact = product.ListGeneratedAsides, product.HashFile, product.RemoveExact
	d.RestorePromotion, d.RestoreNoReplace = product.RestorePromotion, product.RestoreNoReplace
	d.PublishCommitted, d.Now = product.PublishCommitted, product.Now
	if d.AdmitProduct == nil {
		d.AdmitProduct = binaryadmission.AdmitWindowsRole
	}
	return d
}

func reconcileWindowsProductPairRecovery(req windowsProductPairRecoveryRequest) (result *WindowsProductPairRecoverySettled, retErr error) {
	if req.Context == nil {
		req.Context = context.Background()
	}
	if req.StateDir == "" || req.CLIPath == "" || req.WindowlessPath == "" {
		return nil, fmt.Errorf("%w: state and canonical pair paths are required", ErrWindowsProductPairRecoveryIdentity)
	}
	d := withWindowsProductPairRecoveryDefaults(req.Deps)
	journalPath := filepath.Join(req.StateDir, WindowsProductPairRecoveryFileLeaf)
	raw, err := d.ReadJournal(journalPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: read journal: %v", ErrWindowsProductPairRecoveryIdentity, err)
	}
	journal, err := decodeWindowsProductPairRecoveryJournal(raw)
	if err != nil || !sameWindowsRecoveryPath(journal.CLI.Target, req.CLIPath) || !sameWindowsRecoveryPath(journal.Windowless.Target, req.WindowlessPath) || !sameWindowsRecoveryPath(journal.Receipt.Path, filepath.Join(req.StateDir, UpgradeReceiptSchemaV2+".json")) {
		return nil, fmt.Errorf("%w: journal schema or resolved target mismatch", ErrWindowsProductPairRecoveryIdentity)
	}
	if journal.Phase == WindowsProductPairRecoverySettledCommit || journal.Phase == WindowsProductPairRecoverySettledRollback {
		if d.DiscardTaskStore == nil {
			return nil, fmt.Errorf("%w: retained task-store cleanup owner is unavailable", ErrWindowsProductPairRecoveryCleanup)
		}
		if err := cleanupSettledWindowsProductPairRecovery(journalPath, journal, d); err != nil {
			return nil, err
		}
		if journal.Phase == WindowsProductPairRecoverySettledCommit {
			if req.ReconcileCommitted == nil {
				return recoverySettledResult(journal), errors.New("committed recovery reconciliation owner is unavailable")
			}
			if err := req.ReconcileCommitted(journal); err != nil {
				return recoverySettledResult(journal), err
			}
		}
		return recoverySettledResult(journal), nil
	}

	cliState, err := inspectWindowsProductPairRecoveryArtifact(journal.CLI, d)
	if err != nil {
		return nil, fmt.Errorf("%w: canonical CLI: %v", ErrWindowsProductPairRecoveryIdentity, err)
	}
	windowlessState, err := inspectWindowsProductPairRecoveryArtifact(journal.Windowless, d)
	if err != nil {
		return nil, fmt.Errorf("%w: windowless adapter: %v", ErrWindowsProductPairRecoveryIdentity, err)
	}
	if d.ReopenTasks == nil || d.DiscardTaskStore == nil {
		return nil, fmt.Errorf("%w: retained task recovery owner is unavailable", ErrWindowsProductPairRecoveryIdentity)
	}
	tasks, err := d.ReopenTasks(req.Context, journal.Tasks)
	if err != nil {
		return nil, fmt.Errorf("%w: reopen retained task transaction: %w", ErrWindowsProductPairRecoveryIdentity, err)
	}
	defer func() {
		if closeErr := tasks.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close recovered task transaction: %w", closeErr))
		}
	}()

	if cliState.canonical == journal.CLI.NewSHA256 && windowlessState.canonical == journal.Windowless.NewSHA256 {
		if committed, _ := recoveryCommitProven(journal, tasks, d); committed {
			journal.Phase = WindowsProductPairRecoverySettledCommit
			if err := persistRecoveredWindowsProductPairJournal(journalPath, journal, d); err != nil {
				return nil, fmt.Errorf("%w: persist settled commit: %v", ErrWindowsProductPairCommittedCleanupFailed, err)
			}
			if err := cleanupSettledWindowsProductPairRecovery(journalPath, journal, d); err != nil {
				return nil, fmt.Errorf("%w: %w", ErrWindowsProductPairCommittedCleanupFailed, err)
			}
			if req.ReconcileCommitted == nil {
				return recoverySettledResult(journal), errors.New("committed recovery reconciliation owner is unavailable")
			}
			if err := req.ReconcileCommitted(journal); err != nil {
				return recoverySettledResult(journal), err
			}
			return recoverySettledResult(journal), nil
		}
	}

	var restoreErrs []error
	if journal.Phase == WindowsProductPairRecoverySuccessorStarting {
		if req.SettleSuccessor == nil {
			restoreErrs = append(restoreErrs, errors.New("successor settlement owner is unavailable"))
		} else if err := req.SettleSuccessor(req.CLIPath); err != nil {
			restoreErrs = append(restoreErrs, fmt.Errorf("settle successor: %w", err))
		}
	}
	if err := restoreWindowsProductPairRecoveryArtifact(journal.CLI, cliState, d); err != nil {
		restoreErrs = append(restoreErrs, fmt.Errorf("restore canonical CLI: %w", err))
	}
	if err := tasks.RestoreImport(); err != nil {
		restoreErrs = append(restoreErrs, fmt.Errorf("restore task XML: %w", err))
	}
	if err := restoreWindowsProductPairRecoveryArtifact(journal.Windowless, windowlessState, d); err != nil {
		restoreErrs = append(restoreErrs, fmt.Errorf("restore windowless adapter: %w", err))
	}
	if err := restoreWindowsProductPairRecoveryReceipt(journal.Receipt, d); err != nil {
		restoreErrs = append(restoreErrs, fmt.Errorf("restore receipt: %w", err))
	}
	if err := verifyWindowsProductPairRecoveryPrior(journal, tasks, d); err != nil {
		restoreErrs = append(restoreErrs, err)
	}
	if journal.PriorFleetReaped {
		if req.ObservePriorReady == nil || req.RestartPrior == nil || req.WaitPriorReady == nil {
			restoreErrs = append(restoreErrs, errors.New("prior fleet recovery owner is unavailable"))
		} else if ready, err := req.ObservePriorReady(req.Context, req.CLIPath, journal.CLI.PriorSHA256); err != nil {
			restoreErrs = append(restoreErrs, fmt.Errorf("observe prior readiness: %w", err))
		} else if !ready {
			launch, err := req.RestartPrior(req.CLIPath)
			if err != nil {
				restoreErrs = append(restoreErrs, errors.Join(fmt.Errorf("restart prior supervisor: %w", err), abortStartedSupervisor(launch)))
			} else if err := receiveStartedSupervisor(req.Context, launch, supervisorExpectation(req.StateDir, req.CLIPath, journal.CLI.PriorSHA256), func() error {
				return req.WaitPriorReady(req.Context, req.CLIPath, journal.CLI.PriorSHA256)
			}); err != nil {
				restoreErrs = append(restoreErrs, fmt.Errorf("prior supervisor readiness: %w", err))
			}
		}
	}
	if joined := errors.Join(restoreErrs...); joined != nil {
		return nil, fmt.Errorf("%w: %v", ErrWindowsProductPairRecoveryRollback, joined)
	}
	journal.Phase = WindowsProductPairRecoverySettledRollback
	if err := persistRecoveredWindowsProductPairJournal(journalPath, journal, d); err != nil {
		return nil, fmt.Errorf("%w: persist settled rollback: %v", ErrWindowsProductPairRecoveryCleanup, err)
	}
	if err := cleanupSettledWindowsProductPairRecovery(journalPath, journal, d); err != nil {
		return nil, err
	}
	return recoverySettledResult(journal), nil
}

type windowsProductPairRecoveryArtifactState struct {
	canonical  string
	priorAside string
	newAside   string
}

func inspectWindowsProductPairRecoveryArtifact(a windowsProductPairRecoveryArtifact, d windowsProductPairRecoveryDeps) (windowsProductPairRecoveryArtifactState, error) {
	state := windowsProductPairRecoveryArtifactState{}
	hash, err := d.HashFile(a.Target)
	if errors.Is(err, os.ErrNotExist) {
		hash = ""
	} else if err != nil {
		return state, err
	}
	if hash != "" && hash != a.PriorSHA256 && hash != a.NewSHA256 {
		return state, errors.New("canonical bytes match neither recorded prior nor successor")
	}
	state.canonical = hash
	current, err := d.ListAsides(a.Target)
	if err != nil {
		return state, err
	}
	baseline := make(map[string]struct{}, len(a.BaselineAsideBasenames))
	for _, name := range a.BaselineAsideBasenames {
		baseline[name] = struct{}{}
	}
	for _, name := range current {
		if _, existed := baseline[name]; existed {
			continue
		}
		candidate := filepath.Join(filepath.Dir(a.Target), name)
		digest, err := d.HashFile(candidate)
		if err != nil {
			return state, err
		}
		switch digest {
		case a.PriorSHA256:
			if !a.PriorPresent || state.priorAside != "" {
				return state, errors.New("ambiguous crash-created prior aside")
			}
			state.priorAside = candidate
		case a.NewSHA256:
			if state.newAside != "" {
				return state, errors.New("ambiguous crash-created successor aside")
			}
			state.newAside = candidate
		default:
			return state, errors.New("unrecognized post-journal generated aside")
		}
	}
	if a.PriorPresent {
		switch state.canonical {
		case a.PriorSHA256:
			if state.priorAside != "" {
				return state, errors.New("canonical prior and crash-created prior aside coexist")
			}
		case a.NewSHA256:
			if state.priorAside == "" || state.newAside != "" {
				return state, errors.New("canonical successor has ambiguous crash-created asides")
			}
		case "":
			if state.priorAside == "" {
				return state, errors.New("absent canonical has no crash-created prior aside")
			}
		}
	} else if state.priorAside != "" || state.newAside != "" {
		return state, errors.New("prior-absent target has an unexplained crash-created aside")
	}
	return state, nil
}

func restoreWindowsProductPairRecoveryArtifact(a windowsProductPairRecoveryArtifact, state windowsProductPairRecoveryArtifactState, d windowsProductPairRecoveryDeps) error {
	if a.PriorPresent {
		switch state.canonical {
		case a.PriorSHA256:
			if err := removeRecoverySuccessorAside(a, state.newAside, d); err != nil {
				return err
			}
		case a.NewSHA256:
			if state.priorAside == "" {
				return errors.New("unique crash-created prior aside is missing")
			}
			displaced, err := d.RestorePromotion(WindowsProductPairPromotion{Target: a.Target, RetainedPrior: state.priorAside, PriorPresent: true, NewSHA256: a.NewSHA256})
			if err != nil {
				return err
			}
			if err := cleanupDisplacedProductPairSuccessor(WindowsProductPairPromotion{Target: a.Target, NewSHA256: a.NewSHA256}, displaced); err != nil {
				return err
			}
		case "":
			if state.priorAside == "" {
				return errors.New("unique crash-created prior aside is missing")
			}
			if err := d.RestoreNoReplace(state.priorAside, a.Target); err != nil {
				return err
			}
			if err := removeRecoverySuccessorAside(a, state.newAside, d); err != nil {
				return err
			}
		default:
			return errors.New("invalid canonical recovery state")
		}
	} else {
		if state.priorAside != "" || state.newAside != "" {
			return errors.New("prior-absent target has an unexplained crash-created aside")
		}
		switch state.canonical {
		case "":
		case a.NewSHA256:
			if err := d.RemoveExact(a.Target); err != nil {
				return err
			}
		default:
			return errors.New("invalid prior-absent canonical state")
		}
	}
	return verifyRecoveryArtifactPrior(a, d)
}

func removeRecoverySuccessorAside(a windowsProductPairRecoveryArtifact, path string, d windowsProductPairRecoveryDeps) error {
	if path == "" {
		return nil
	}
	digest, err := d.HashFile(path)
	if err != nil || digest != a.NewSHA256 {
		return errors.New("crash-created successor aside identity differs")
	}
	return d.RemoveExact(path)
}

func recoveryCommitProven(j windowsProductPairRecoveryJournal, tasks WindowsProductPairTaskTxn, d windowsProductPairRecoveryDeps) (bool, binaryadmission.WindowsProductPair) {
	pair, err := d.AdmitPair(
		binaryadmission.WindowsArtifact{Path: j.CLI.Target, Role: binaryadmission.WindowsArtifactRoleCLI, SHA256: j.CLI.NewSHA256},
		binaryadmission.WindowsArtifact{Path: j.Windowless.Target, Role: binaryadmission.WindowsArtifactRoleWindowless, SHA256: j.Windowless.NewSHA256},
	)
	if err != nil || pair.CLI.SHA256 != j.CLI.NewSHA256 || pair.Windowless.SHA256 != j.Windowless.NewSHA256 {
		return false, binaryadmission.WindowsProductPair{}
	}
	raw, err := d.ReadReceipt(j.Receipt.Path)
	if err != nil {
		return false, binaryadmission.WindowsProductPair{}
	}
	decoded, err := DecodeUpgradeReceipt(raw)
	if err != nil || decoded.V2 == nil || !windowsProductPairReceiptModeMatches(decoded.V2.Mode, j.Mode) || !sameProductPairIdentity(decoded.V2.Artifacts, pair) || tasks.VerifyRuntime() != nil {
		return false, binaryadmission.WindowsProductPair{}
	}
	return true, pair
}

func restoreWindowsProductPairRecoveryReceipt(r windowsProductPairRecoveryReceipt, d windowsProductPairRecoveryDeps) error {
	if r.PriorPresent {
		if err := d.WriteReceipt(r.Path, r.PriorBytes); err != nil {
			return err
		}
		got, err := d.ReadReceipt(r.Path)
		if err != nil || !slices.Equal(got, r.PriorBytes) {
			return errors.New("prior receipt exact-byte readback differs")
		}
		return nil
	}
	return d.RemoveReceipt(r.Path)
}

func verifyWindowsProductPairRecoveryPrior(j windowsProductPairRecoveryJournal, tasks WindowsProductPairTaskTxn, d windowsProductPairRecoveryDeps) error {
	if err := verifyRecoveryArtifactPrior(j.CLI, d); err != nil {
		return fmt.Errorf("verify prior CLI: %w", err)
	}
	if err := verifyRecoveryArtifactPrior(j.Windowless, d); err != nil {
		return fmt.Errorf("verify prior windowless: %w", err)
	}
	if j.Receipt.PriorPresent {
		raw, err := d.ReadReceipt(j.Receipt.Path)
		if err != nil || !slices.Equal(raw, j.Receipt.PriorBytes) {
			return errors.New("verify prior receipt: exact bytes differ")
		}
	} else if _, err := d.ReadReceipt(j.Receipt.Path); !errors.Is(err, os.ErrNotExist) {
		return errors.New("verify prior receipt: expected absence")
	}
	return nil
}

func verifyRecoveryArtifactPrior(a windowsProductPairRecoveryArtifact, d windowsProductPairRecoveryDeps) error {
	digest, err := d.HashFile(a.Target)
	if a.PriorPresent {
		if err != nil || digest != a.PriorSHA256 {
			return errors.New("canonical prior hash differs")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return errors.New("prior-absent canonical target exists or is unreadable")
	}
	return nil
}

func cleanupSettledWindowsProductPairRecovery(path string, journal windowsProductPairRecoveryJournal, d windowsProductPairRecoveryDeps) error {
	if err := d.DiscardTaskStore(journal.Tasks); err != nil {
		return fmt.Errorf("%w: discard retained task store: %w", ErrWindowsProductPairRecoveryCleanup, err)
	}
	if err := d.RemoveJournal(path); err != nil {
		return fmt.Errorf("%w: remove settled journal: %v", ErrWindowsProductPairRecoveryCleanup, err)
	}
	return nil
}

func persistRecoveredWindowsProductPairJournal(path string, journal windowsProductPairRecoveryJournal, d windowsProductPairRecoveryDeps) error {
	txnDeps := withWindowsProductPairDefaults(WindowsProductPairTxnDeps{WriteRecoveryJournal: d.WriteJournal, ReadRecoveryJournal: d.ReadJournal})
	return persistWindowsProductPairRecoveryJournal(path, journal, txnDeps)
}

func recoverySettledResult(j windowsProductPairRecoveryJournal) *WindowsProductPairRecoverySettled {
	hashes := [2]string{j.CLI.PriorSHA256, j.Windowless.PriorSHA256}
	if j.Phase == WindowsProductPairRecoverySettledCommit {
		hashes = [2]string{j.CLI.NewSHA256, j.Windowless.NewSHA256}
	}
	return &WindowsProductPairRecoverySettled{TransactionID: j.TransactionID, Outcome: j.Phase, PairOrPriorHashes: hashes}
}

func withWindowsProductPairRecoveryDefaults(d windowsProductPairRecoveryDeps) windowsProductPairRecoveryDeps {
	if d.ReadJournal == nil {
		d.ReadJournal = api.ReadStateFileInodeAnchored
	}
	if d.WriteJournal == nil {
		d.WriteJournal = api.WriteStateFileBytesAtomic
	}
	if d.RemoveJournal == nil {
		d.RemoveJournal = removeRecoveryLeaf
	}
	if d.ReadReceipt == nil {
		d.ReadReceipt = api.ReadStateFileInodeAnchored
	}
	if d.WriteReceipt == nil {
		d.WriteReceipt = api.WriteStateFileBytesAtomic
	}
	if d.RemoveReceipt == nil {
		d.RemoveReceipt = removeRecoveryLeaf
	}
	if d.ListAsides == nil {
		d.ListAsides = listGeneratedRenameAsideBasenames
	}
	if d.HashFile == nil {
		d.HashFile = hashRecoveryFile
	}
	if d.AdmitPair == nil {
		d.AdmitPair = binaryadmission.AdmitWindowsProductPair
	}
	if d.RestorePromotion == nil {
		d.RestorePromotion = withWindowsProductPairDefaults(WindowsProductPairTxnDeps{}).RestorePromotion
	}
	if d.RestoreNoReplace == nil {
		d.RestoreNoReplace = moveWindowsFileNoReplace
	}
	if d.RemoveExact == nil {
		d.RemoveExact = os.Remove
	}
	return d
}

func hashRecoveryFile(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(body)), nil
}

func removeRecoveryLeaf(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func moveWindowsFileNoReplace(source, target string) error {
	sourceW, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	targetW, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(sourceW, targetW, 0)
}

func sameWindowsRecoveryPath(left, right string) bool {
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}

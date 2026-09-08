package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
)

const (
	UpgradeReceiptSchemaV2              = "upgrade-receipt-v2"
	WindowsProductPairCommittedSchemaV1 = "windows-product-pair-committed-v1"
)

// WindowsProductPairStage is the deterministic fault/readback boundary used by
// the one pair transaction. Production leaves Fault nil; tests inject one
// failure after each completed stage and require exact preimage restoration.
type WindowsProductPairStage string

const (
	WindowsProductPairStageWindowlessPromoted WindowsProductPairStage = "windowless-promoted"
	WindowsProductPairStageWindowlessReadBack WindowsProductPairStage = "windowless-read-back"
	WindowsProductPairStageTasksRewritten     WindowsProductPairStage = "tasks-rewritten"
	WindowsProductPairStageTasksReadBack      WindowsProductPairStage = "tasks-read-back"
	WindowsProductPairStageCLIPromoted        WindowsProductPairStage = "cli-promoted"
	WindowsProductPairStagePairReadBack       WindowsProductPairStage = "pair-read-back"
	WindowsProductPairStageSupervisorStarted  WindowsProductPairStage = "supervisor-started"
	WindowsProductPairStageSupervisorReady    WindowsProductPairStage = "supervisor-ready"
	WindowsProductPairStageReceiptWritten     WindowsProductPairStage = "receipt-written"
)

type WindowsProductPairMode string

const (
	WindowsProductPairModeSetup        WindowsProductPairMode = "setup"
	WindowsProductPairModeCanonicalize WindowsProductPairMode = "canonicalize"
	WindowsProductPairModeUpgrade      WindowsProductPairMode = "upgrade"
)

type WindowsProductPairOutcome string

const (
	WindowsProductPairCommittedOutcome             WindowsProductPairOutcome = "committed"
	WindowsProductPairCommittedObservabilityFailed WindowsProductPairOutcome = "committed-observability-failed"
	WindowsProductPairCommittedCleanupFailed       WindowsProductPairOutcome = "committed-cleanup-failed"
)

var ErrWindowsProductPairCommittedObservabilityFailed = errors.New("E_WINDOWS_PRODUCT_PAIR_COMMITTED_OBSERVABILITY_FAILED")
var ErrWindowsProductPairCommittedCleanupFailed = errors.New("E_WINDOWS_PRODUCT_PAIR_COMMITTED_CLEANUP_FAILED")

// WindowsProductPairPrePromotionError marks a direct pair failure before any
// artifact, task, or receipt promotion. Only the outer upgrade owner knows
// whether a prior fleet was already reaped and must be restarted.
type WindowsProductPairPrePromotionError struct{ Cause error }

func (e *WindowsProductPairPrePromotionError) Error() string {
	return fmt.Sprintf("Windows product pair pre-promotion failure: %v", e.Cause)
}
func (e *WindowsProductPairPrePromotionError) Unwrap() error { return e.Cause }

// WindowsProductPairTaskTxn is the narrow scheduler-owned command migration
// surface. The concrete Windows adapter is scheduler.OwnedEntrypointTaskTxn.
type WindowsProductPairTaskTxn interface {
	InventoryExport() error
	RewriteCommand() error
	VerifyRuntime() error
	RestoreImport() error
	Close() error
}

// UpgradeReceiptV2 binds both role-correct artifacts after pair readback.
// V1 remains a separate read-compatible historical shape.
type UpgradeReceiptV2 struct {
	Schema      string                             `json:"schema"`
	Admission   string                             `json:"admission"`
	Version     string                             `json:"version"`
	Commit      string                             `json:"commit"`
	BuildDate   string                             `json:"build_date"`
	Artifacts   binaryadmission.WindowsProductPair `json:"artifacts"`
	InstalledAt string                             `json:"installed_at"`
}

// UpgradeReceipt is a schema-discriminated compatibility result.
type UpgradeReceipt struct {
	V1 *UpgradeReceiptV1
	V2 *UpgradeReceiptV2
}

func DecodeUpgradeReceipt(raw []byte) (UpgradeReceipt, error) {
	var header struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return UpgradeReceipt{}, fmt.Errorf("decode upgrade receipt schema: %w", err)
	}
	switch header.Schema {
	case UpgradeReceiptSchemaV1:
		var v1 UpgradeReceiptV1
		if err := json.Unmarshal(raw, &v1); err != nil {
			return UpgradeReceipt{}, err
		}
		return UpgradeReceipt{V1: &v1}, nil
	case UpgradeReceiptSchemaV2:
		var v2 UpgradeReceiptV2
		if err := json.Unmarshal(raw, &v2); err != nil {
			return UpgradeReceipt{}, err
		}
		if v2.Artifacts.CLI.Role != binaryadmission.WindowsArtifactRoleCLI ||
			v2.Artifacts.Windowless.Role != binaryadmission.WindowsArtifactRoleWindowless ||
			v2.Artifacts.CLI.SHA256 == "" || v2.Artifacts.Windowless.SHA256 == "" {
			return UpgradeReceipt{}, errors.New("decode upgrade-receipt-v2: incomplete role-keyed artifacts")
		}
		return UpgradeReceipt{V2: &v2}, nil
	default:
		return UpgradeReceipt{}, fmt.Errorf("unsupported upgrade receipt schema %q", header.Schema)
	}
}

// WindowsProductPairCommitted is the sole successful transaction result. It is
// observable only after pair and scheduler readback plus supervisor readiness.
type WindowsProductPairCommitted struct {
	Schema        string
	Outcome       WindowsProductPairOutcome
	Pair          binaryadmission.WindowsProductPair
	ReceiptSHA256 string
	EventSHA256   string
}

type WindowsProductPairCommitEvent struct {
	Schema        string
	Mode          WindowsProductPairMode
	ReceiptSchema string
	ReceiptSHA256 string
	InstalledAt   string
	Pair          binaryadmission.WindowsProductPair
}

type WindowsProductPairPromotion struct {
	Target        string
	RetainedPrior string
	PriorPresent  bool
	NewSHA256     string
}

type WindowsProductPairTxnDeps struct {
	AdmitPair           func(cli, windowless binaryadmission.WindowsArtifact) (binaryadmission.WindowsProductPair, error)
	Promote             func(src, dst, newSHA256 string) (WindowsProductPairPromotion, error)
	RestorePromotion    func(WindowsProductPairPromotion) (string, error)
	SweepOldTargets     func([]string, ...func(string, error)) error
	ReadBackWindowless  func(path string, expected binaryadmission.WindowsArtifact) error
	StartSupervisor     func(cliPath string) error
	WaitSupervisorReady func(ctx context.Context, cliPath string, pair binaryadmission.WindowsProductPair) error
	RestartPrior        func(cliPath string) error
	SettleSuccessor     func(cliPath string) error
	WriteReceipt        func(path string, raw []byte) error
	ReadReceipt         func(path string) ([]byte, error)
	PublishCommitted    func(WindowsProductPairCommitEvent) (string, error)
	Fault               func(WindowsProductPairStage) error
	Now                 func() time.Time
}

type WindowsProductPairTxnOpts struct {
	CLIPath          string
	WindowlessPath   string
	StagedCLI        string
	StagedWindowless string
	ReceiptPath      string
	Mode             WindowsProductPairMode
	Tasks            WindowsProductPairTaskTxn
	Deps             WindowsProductPairTxnDeps
}

// WindowsProductPairTxn is the only commit owner for Windows product-pair
// mutation. It promotes windowless first, migrates and reads back exact-owned
// tasks second, promotes canonical CUI last, then proves pair/readiness before
// publishing the committed result and V2 receipt.
type WindowsProductPairTxn struct{ Opts WindowsProductPairTxnOpts }

func (t *WindowsProductPairTxn) Close() error {
	if t == nil || t.Opts.Tasks == nil {
		return nil
	}
	return t.Opts.Tasks.Close()
}

type productPairFileSnapshot struct {
	path    string
	body    []byte
	present bool
}

func (t WindowsProductPairTxn) Run(ctx context.Context) (result WindowsProductPairCommitted, retErr error) {
	o := t.Opts
	prePromotion := func(err error) error { return &WindowsProductPairPrePromotionError{Cause: err} }
	if o.Tasks == nil {
		return WindowsProductPairCommitted{}, prePromotion(errors.New("Windows product pair transaction: task transaction is required"))
	}
	if o.CLIPath == "" || o.WindowlessPath == "" || o.StagedCLI == "" || o.StagedWindowless == "" || o.ReceiptPath == "" {
		return WindowsProductPairCommitted{}, prePromotion(errors.New("Windows product pair transaction: all paths are required"))
	}
	if o.Mode != WindowsProductPairModeSetup && o.Mode != WindowsProductPairModeCanonicalize && o.Mode != WindowsProductPairModeUpgrade {
		return WindowsProductPairCommitted{}, prePromotion(errors.New("Windows product pair transaction: valid caller mode is required"))
	}
	defer func() {
		if closeErr := o.Tasks.Close(); closeErr != nil {
			if result.ReceiptSHA256 != "" {
				if result.Outcome == WindowsProductPairCommittedOutcome {
					result.Outcome = WindowsProductPairCommittedCleanupFailed
				}
				retErr = errors.Join(retErr, fmt.Errorf("%w: receipt_sha256=%s: %v", ErrWindowsProductPairCommittedCleanupFailed, result.ReceiptSHA256, closeErr))
			} else {
				retErr = errors.Join(retErr, closeErr)
			}
		}
	}()
	d := withWindowsProductPairDefaults(o.Deps)
	if d.PublishCommitted == nil {
		return WindowsProductPairCommitted{}, prePromotion(errors.New("Windows product pair transaction: committed publisher is required"))
	}
	staged, err := d.AdmitPair(
		binaryadmission.WindowsArtifact{Path: o.StagedCLI, Role: binaryadmission.WindowsArtifactRoleCLI},
		binaryadmission.WindowsArtifact{Path: o.StagedWindowless, Role: binaryadmission.WindowsArtifactRoleWindowless},
	)
	if err != nil {
		return WindowsProductPairCommitted{}, prePromotion(fmt.Errorf("admit staged Windows product pair: %w", err))
	}

	cliPrior, err := snapshotProductPairFile(o.CLIPath)
	if err != nil {
		return WindowsProductPairCommitted{}, prePromotion(err)
	}
	windowlessPrior, err := snapshotProductPairFile(o.WindowlessPath)
	if err != nil {
		return WindowsProductPairCommitted{}, prePromotion(err)
	}
	receiptPrior, err := snapshotProductPairFile(o.ReceiptPath)
	if err != nil {
		return WindowsProductPairCommitted{}, prePromotion(err)
	}
	if err := o.Tasks.InventoryExport(); err != nil {
		return WindowsProductPairCommitted{}, prePromotion(fmt.Errorf("snapshot exact-owned task XML: %w", err))
	}

	var windowlessPromotion, cliPromotion *WindowsProductPairPromotion
	successorStarted := false
	rollback := func(trigger error) error {
		var restoreErrs []error
		if successorStarted {
			if d.SettleSuccessor == nil {
				restoreErrs = append(restoreErrs, errors.New("settle transaction-started successor: no settlement owner"))
			} else if err := d.SettleSuccessor(o.CLIPath); err != nil {
				restoreErrs = append(restoreErrs, fmt.Errorf("settle transaction-started successor: %w", err))
			}
		}
		if err := restoreProductPairPromotion(d, cliPromotion, cliPrior); err != nil {
			restoreErrs = append(restoreErrs, fmt.Errorf("restore canonical CLI: %w", err))
		}
		if err := o.Tasks.RestoreImport(); err != nil {
			restoreErrs = append(restoreErrs, err)
		}
		if err := restoreProductPairPromotion(d, windowlessPromotion, windowlessPrior); err != nil {
			restoreErrs = append(restoreErrs, fmt.Errorf("restore windowless adapter: %w", err))
		}
		if err := restoreProductPairFile(receiptPrior); err != nil {
			restoreErrs = append(restoreErrs, err)
		}
		if cliPrior.present && d.RestartPrior != nil {
			if err := d.RestartPrior(o.CLIPath); err != nil {
				restoreErrs = append(restoreErrs, fmt.Errorf("restart prior supervisor: %w", err))
			}
		}
		if joined := errors.Join(restoreErrs...); joined != nil {
			return fmt.Errorf("%w; exact Windows product pair rollback failed: %v", trigger, joined)
		}
		return fmt.Errorf("%w; exact Windows product pair rollback completed", trigger)
	}
	fault := func(stage WindowsProductPairStage) error {
		if d.Fault == nil {
			return nil
		}
		if err := d.Fault(stage); err != nil {
			return fmt.Errorf("fault after %s: %w", stage, err)
		}
		return nil
	}

	promotion, err := d.Promote(o.StagedWindowless, o.WindowlessPath, staged.Windowless.SHA256)
	windowlessPromotion = new(WindowsProductPairPromotion)
	*windowlessPromotion = promotion
	if err != nil {
		return WindowsProductPairCommitted{}, rollback(fmt.Errorf("promote windowless adapter: %w", err))
	}
	if err := verifyProductPairPromotion(promotion, windowlessPrior); err != nil {
		return WindowsProductPairCommitted{}, rollback(fmt.Errorf("verify retained windowless prior: %w", err))
	}
	if err := fault(WindowsProductPairStageWindowlessPromoted); err != nil {
		return WindowsProductPairCommitted{}, rollback(err)
	}
	if err := d.ReadBackWindowless(o.WindowlessPath, staged.Windowless); err != nil {
		return WindowsProductPairCommitted{}, rollback(fmt.Errorf("read back windowless adapter: %w", err))
	}
	if err := fault(WindowsProductPairStageWindowlessReadBack); err != nil {
		return WindowsProductPairCommitted{}, rollback(err)
	}
	if err := o.Tasks.RewriteCommand(); err != nil {
		return WindowsProductPairCommitted{}, rollback(fmt.Errorf("rewrite exact-owned task commands: %w", err))
	}
	if err := fault(WindowsProductPairStageTasksRewritten); err != nil {
		return WindowsProductPairCommitted{}, rollback(err)
	}
	if err := o.Tasks.VerifyRuntime(); err != nil {
		return WindowsProductPairCommitted{}, rollback(fmt.Errorf("read back exact-owned task commands: %w", err))
	}
	if err := fault(WindowsProductPairStageTasksReadBack); err != nil {
		return WindowsProductPairCommitted{}, rollback(err)
	}
	promotion, err = d.Promote(o.StagedCLI, o.CLIPath, staged.CLI.SHA256)
	cliPromotion = new(WindowsProductPairPromotion)
	*cliPromotion = promotion
	if err != nil {
		return WindowsProductPairCommitted{}, rollback(fmt.Errorf("promote canonical CUI: %w", err))
	}
	if err := verifyProductPairPromotion(promotion, cliPrior); err != nil {
		return WindowsProductPairCommitted{}, rollback(fmt.Errorf("verify retained CLI prior: %w", err))
	}
	if err := fault(WindowsProductPairStageCLIPromoted); err != nil {
		return WindowsProductPairCommitted{}, rollback(err)
	}
	readBack, err := d.AdmitPair(
		binaryadmission.WindowsArtifact{Path: o.CLIPath, Role: binaryadmission.WindowsArtifactRoleCLI, SHA256: staged.CLI.SHA256},
		binaryadmission.WindowsArtifact{Path: o.WindowlessPath, Role: binaryadmission.WindowsArtifactRoleWindowless, SHA256: staged.Windowless.SHA256},
	)
	if err != nil || !sameProductPairIdentity(staged, readBack) {
		if err == nil {
			err = errors.New("read-back pair identity differs from staged pair")
		}
		return WindowsProductPairCommitted{}, rollback(fmt.Errorf("read back Windows product pair: %w", err))
	}
	if err := fault(WindowsProductPairStagePairReadBack); err != nil {
		return WindowsProductPairCommitted{}, rollback(err)
	}
	if d.StartSupervisor != nil {
		if err := d.StartSupervisor(o.CLIPath); err != nil {
			return WindowsProductPairCommitted{}, rollback(fmt.Errorf("start canonical supervisor: %w", err))
		}
		successorStarted = true
	}
	if err := fault(WindowsProductPairStageSupervisorStarted); err != nil {
		return WindowsProductPairCommitted{}, rollback(err)
	}
	if d.WaitSupervisorReady != nil {
		if err := d.WaitSupervisorReady(ctx, o.CLIPath, readBack); err != nil {
			return WindowsProductPairCommitted{}, rollback(fmt.Errorf("canonical supervisor readiness: %w", err))
		}
	}
	if err := fault(WindowsProductPairStageSupervisorReady); err != nil {
		return WindowsProductPairCommitted{}, rollback(err)
	}

	now := d.Now().UTC().Format(time.RFC3339Nano)
	receipt := UpgradeReceiptV2{Schema: UpgradeReceiptSchemaV2, Admission: UpgradeAdmissionLocalProduct, Version: readBack.CLI.Version, Commit: readBack.CLI.Commit, BuildDate: readBack.CLI.BuildDate, Artifacts: readBack, InstalledAt: now}
	raw, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return WindowsProductPairCommitted{}, rollback(err)
	}
	raw = append(raw, '\n')
	if err := d.WriteReceipt(o.ReceiptPath, raw); err != nil {
		return WindowsProductPairCommitted{}, rollback(fmt.Errorf("persist %s: %w", UpgradeReceiptSchemaV2, err))
	}
	if err := fault(WindowsProductPairStageReceiptWritten); err != nil {
		return WindowsProductPairCommitted{}, rollback(err)
	}
	readReceipt, err := d.ReadReceipt(o.ReceiptPath)
	if err != nil || string(readReceipt) != string(raw) {
		if err == nil {
			err = errors.New("receipt exact-byte readback differs")
		}
		return WindowsProductPairCommitted{}, rollback(fmt.Errorf("read back %s: %w", UpgradeReceiptSchemaV2, err))
	}
	decoded, err := DecodeUpgradeReceipt(readReceipt)
	if err != nil || decoded.V2 == nil || !sameProductPairIdentity(decoded.V2.Artifacts, readBack) {
		if err == nil {
			err = errors.New("receipt parsed identity differs from admitted pair")
		}
		return WindowsProductPairCommitted{}, rollback(fmt.Errorf("validate %s readback: %w", UpgradeReceiptSchemaV2, err))
	}
	receiptDigest := fmt.Sprintf("%x", sha256.Sum256(readReceipt))
	committed := WindowsProductPairCommitted{Schema: WindowsProductPairCommittedSchemaV1, Outcome: WindowsProductPairCommittedOutcome, Pair: readBack, ReceiptSHA256: receiptDigest}
	event := WindowsProductPairCommitEvent{
		Schema: WindowsProductPairCommittedSchemaV1, Mode: o.Mode, ReceiptSchema: UpgradeReceiptSchemaV2,
		ReceiptSHA256: receiptDigest, InstalledAt: receipt.InstalledAt, Pair: readBack,
	}
	eventDigest, eventErr := d.PublishCommitted(event)
	committed.EventSHA256 = eventDigest
	if eventErr != nil {
		committed.Outcome = WindowsProductPairCommittedObservabilityFailed
	}
	var cleanupErr error
	if err := d.SweepOldTargets([]string{committed.Pair.CLI.Path, committed.Pair.Windowless.Path}); err != nil {
		committed.Outcome = WindowsProductPairCommittedCleanupFailed
		cleanupErr = fmt.Errorf("%w: receipt_sha256=%s: %v", ErrWindowsProductPairCommittedCleanupFailed, receiptDigest, err)
	}
	if eventErr != nil {
		return committed, errors.Join(fmt.Errorf("%w: receipt_sha256=%s event_sha256=%s: %v", ErrWindowsProductPairCommittedObservabilityFailed, receiptDigest, eventDigest, eventErr), cleanupErr)
	}
	if cleanupErr != nil {
		return committed, cleanupErr
	}
	return committed, nil
}

func withWindowsProductPairDefaults(d WindowsProductPairTxnDeps) WindowsProductPairTxnDeps {
	if d.AdmitPair == nil {
		d.AdmitPair = binaryadmission.AdmitWindowsProductPair
	}
	if d.Promote == nil {
		d.Promote = func(src, dst, newSHA256 string) (WindowsProductPairPromotion, error) {
			if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
				if err := os.Rename(src, dst); err != nil {
					return WindowsProductPairPromotion{}, fmt.Errorf("promote into proven-absent target %s: %w", dst, err)
				}
				return WindowsProductPairPromotion{Target: dst, NewSHA256: newSHA256}, nil
			} else if err != nil {
				return WindowsProductPairPromotion{}, fmt.Errorf("probe promotion target %s: %w", dst, err)
			}
			result, err := api.RenameAsideReplaceWithResult(dst, src)
			if err != nil {
				return WindowsProductPairPromotion{Target: dst, RetainedPrior: result.RetainedPrior, PriorPresent: true, NewSHA256: newSHA256}, err
			}
			if !result.Promoted || result.RetainedPrior == "" {
				return WindowsProductPairPromotion{}, errors.New("rename-aside promotion returned incomplete retained-prior result")
			}
			return WindowsProductPairPromotion{Target: dst, RetainedPrior: result.RetainedPrior, PriorPresent: true, NewSHA256: newSHA256}, nil
		}
	}
	if d.RestorePromotion == nil {
		d.RestorePromotion = func(p WindowsProductPairPromotion) (string, error) {
			if !p.PriorPresent || p.RetainedPrior == "" {
				return "", errors.New("rename-aside restore requires an exact retained prior")
			}
			result, err := api.RenameAsideReplaceWithResult(p.Target, p.RetainedPrior)
			if err != nil {
				return result.RetainedPrior, err
			}
			if !result.Promoted || result.RetainedPrior == "" {
				return "", errors.New("rename-aside restore did not return displaced successor")
			}
			return result.RetainedPrior, nil
		}
	}
	if d.ReadBackWindowless == nil {
		d.ReadBackWindowless = func(path string, expected binaryadmission.WindowsArtifact) error {
			expected.Path = path
			actual, err := binaryadmission.AdmitWindowsRole(expected)
			if err != nil {
				return err
			}
			if actual.SHA256 != expected.SHA256 {
				return errors.New("windowless SHA-256 mismatch")
			}
			return nil
		}
	}
	if d.WriteReceipt == nil {
		d.WriteReceipt = api.WriteStateFileBytesAtomic
	}
	if d.ReadReceipt == nil {
		d.ReadReceipt = os.ReadFile
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.SweepOldTargets == nil {
		d.SweepOldTargets = api.SweepOldBinaryTargets
	}
	return d
}

func sameProductPairIdentity(a, b binaryadmission.WindowsProductPair) bool {
	return a.CLI.Role == b.CLI.Role && a.CLI.Version == b.CLI.Version && a.CLI.Commit == b.CLI.Commit && a.CLI.BuildDate == b.CLI.BuildDate && a.CLI.SHA256 == b.CLI.SHA256 &&
		a.Windowless.Role == b.Windowless.Role && a.Windowless.Version == b.Windowless.Version && a.Windowless.Commit == b.Windowless.Commit && a.Windowless.BuildDate == b.Windowless.BuildDate && a.Windowless.SHA256 == b.Windowless.SHA256
}

func snapshotProductPairFile(path string) (productPairFileSnapshot, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return productPairFileSnapshot{path: path}, nil
	}
	if err != nil {
		return productPairFileSnapshot{}, fmt.Errorf("snapshot %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return productPairFileSnapshot{}, fmt.Errorf("snapshot %s: not a regular file", path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return productPairFileSnapshot{}, fmt.Errorf("snapshot %s: %w", path, err)
	}
	return productPairFileSnapshot{path: path, body: body, present: true}, nil
}

func restoreProductPairFile(snapshot productPairFileSnapshot) error {
	if !snapshot.present {
		if err := os.Remove(snapshot.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return api.WriteStateFileBytesAtomic(snapshot.path, snapshot.body)
}

func verifyProductPairPromotion(promotion WindowsProductPairPromotion, prior productPairFileSnapshot) error {
	if promotion.Target != prior.path || promotion.PriorPresent != prior.present {
		return errors.New("promotion prior-presence/target result differs from snapshot")
	}
	if !prior.present {
		if promotion.RetainedPrior != "" {
			return errors.New("absent-target promotion unexpectedly retained a prior")
		}
		return nil
	}
	if promotion.RetainedPrior == "" {
		return errors.New("existing-target promotion did not return retained prior")
	}
	body, err := os.ReadFile(promotion.RetainedPrior)
	if err != nil {
		return err
	}
	if string(body) != string(prior.body) {
		return errors.New("retained prior bytes differ from exact snapshot")
	}
	return nil
}

func restoreProductPairPromotion(d WindowsProductPairTxnDeps, promotion *WindowsProductPairPromotion, prior productPairFileSnapshot) error {
	if promotion == nil {
		return nil
	}
	var displaced string
	if prior.present {
		var err error
		displaced, err = d.RestorePromotion(*promotion)
		if err != nil {
			return err
		}
	} else {
		body, err := os.ReadFile(promotion.Target)
		if err != nil {
			return err
		}
		actual := fmt.Sprintf("%x", sha256.Sum256(body))
		if actual != promotion.NewSHA256 {
			return errors.New("refusing to remove absent-prior target whose bytes are not transaction-owned")
		}
		if err := os.Remove(promotion.Target); err != nil {
			return err
		}
	}
	if prior.present {
		body, err := os.ReadFile(prior.path)
		if err != nil {
			return err
		}
		if string(body) != string(prior.body) {
			return errors.New("restored prior bytes differ from exact snapshot")
		}
		if err := cleanupDisplacedProductPairSuccessor(*promotion, displaced); err != nil {
			return err
		}
	} else if _, err := os.Stat(prior.path); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("prior-absent target still exists after rollback: %v", err)
	}
	return nil
}

func cleanupDisplacedProductPairSuccessor(promotion WindowsProductPairPromotion, displaced string) error {
	if !api.IsGeneratedRenameAsideForTarget(promotion.Target, displaced) {
		return errors.New("displaced successor is not an exact generated aside of the promotion target")
	}
	body, err := os.ReadFile(displaced)
	if err != nil {
		return err
	}
	if actual := fmt.Sprintf("%x", sha256.Sum256(body)); actual != promotion.NewSHA256 {
		return errors.New("displaced successor bytes are not transaction-owned")
	}
	if err := os.Remove(displaced); err != nil {
		return err
	}
	return nil
}

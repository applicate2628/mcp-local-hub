package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
)

const (
	UpgradeAdmissionLocalProductV3      = "local-product"
	UpgradeReceiptSchemaV3              = "upgrade-receipt-v3"
	UpgradeReceiptSchemaV2              = "upgrade-receipt-v2"
	WindowsProductRecoverySchemaV3      = "windows-product-recovery-v3"
	WindowsProductRecoveryFileLeafV3    = WindowsProductRecoverySchemaV3 + ".json"
	WindowsProductPairCommittedSchemaV1 = "windows-product-pair-committed-v1"
	WindowsProductPairRecoverySchemaV1  = "windows-product-pair-recovery-v1"
	WindowsProductPairRecoverySchemaV2  = "windows-product-pair-recovery-v2"
	WindowsProductPairRecoveryFileLeaf  = WindowsProductPairRecoverySchemaV1 + ".json"
)

type WindowsProductRecoveryPhase string

const (
	WindowsProductRecoveryPrepared            WindowsProductRecoveryPhase = "prepared"
	WindowsProductRecoveryGUIOwnedStopPending WindowsProductRecoveryPhase = "gui_owned_stop_pending"
	WindowsProductRecoverySuccessorStarting   WindowsProductRecoveryPhase = "successor_starting"
	WindowsProductRecoveryCommitDecided       WindowsProductRecoveryPhase = "commit_decided"
	WindowsProductRecoverySettledCommit       WindowsProductRecoveryPhase = "settled_commit"
	WindowsProductRecoverySettledRollback     WindowsProductRecoveryPhase = "settled_rollback"
)

type WindowsProductPairRecoveryPhase string

const (
	WindowsProductPairRecoveryPrepared          WindowsProductPairRecoveryPhase = "prepared"
	WindowsProductPairRecoverySuccessorStarting WindowsProductPairRecoveryPhase = "successor_starting"
	WindowsProductPairRecoverySettledCommit     WindowsProductPairRecoveryPhase = "settled_commit"
	WindowsProductPairRecoverySettledRollback   WindowsProductPairRecoveryPhase = "settled_rollback"
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
var ErrWindowsProductPairRecoveryPrepare = errors.New("E_WINDOWS_PRODUCT_PAIR_RECOVERY_PREPARE")
var ErrWindowsProductPairRecoveryIdentity = errors.New("E_WINDOWS_PRODUCT_PAIR_RECOVERY_IDENTITY")
var ErrWindowsProductPairRecoveryRollback = errors.New("E_WINDOWS_PRODUCT_PAIR_RECOVERY_ROLLBACK")
var ErrWindowsProductPairRecoveryCleanup = errors.New("E_WINDOWS_PRODUCT_PAIR_RECOVERY_CLEANUP")
var ErrWindowsProductPairRecoverySchemaUnsupported = errors.New("E_WINDOWS_PRODUCT_PAIR_RECOVERY_SCHEMA_UNSUPPORTED")
var ErrWindowsProductPairQuiescenceRequired = errors.New("E_WINDOWS_PRODUCT_PAIR_QUIESCENCE_REQUIRED")
var ErrWindowsProductRecoveryPending = errors.New("E_WINDOWS_PRODUCT_RECOVERY_PENDING")
var ErrWindowsProductPairReplacementRequiresUpgrade = errors.New("E_WINDOWS_PRODUCT_PAIR_REPLACEMENT_REQUIRES_UPGRADE")
var ErrWindowsProductPairSupervisorUnsettled = errors.New("E_WINDOWS_PRODUCT_PAIR_SUPERVISOR_UNSETTLED")

const windowsProductQuiescenceScopeV1 = "supervisor-lock-and-intent-ports-v1"
const windowsProductGUIOwnerConfirmedAbsent = "confirmed-absent"
const windowsProductGUIOwnerConfirmedPresent = "confirmed-present"

type windowsProductPreimageV1 struct {
	Present bool   `json:"present"`
	SHA256  string `json:"sha256"`
}

type windowsProductTargetDispositionV1 string

const (
	windowsProductTargetCreate    windowsProductTargetDispositionV1 = "create"
	windowsProductTargetIdentical windowsProductTargetDispositionV1 = "identical"
	windowsProductTargetReplace   windowsProductTargetDispositionV1 = "replace"
)

type windowsProductPairDispositionV1 struct {
	CLI        windowsProductTargetDispositionV1
	Windowless windowsProductTargetDispositionV1
}

type windowsProductPortStateV1 string

const (
	windowsProductPortUnbound        windowsProductPortStateV1 = "unbound"
	windowsProductPortCanonicalOwned windowsProductPortStateV1 = "canonical-owned"
	windowsProductPortRetainedOwned  windowsProductPortStateV1 = "retained-owned"
)

type windowsProductPortSettlementV1 struct {
	TaskName    string                    `json:"task_name"`
	Port        int                       `json:"port"`
	PreState    windowsProductPortStateV1 `json:"pre_state"`
	PrePID      int                       `json:"pre_pid"`
	PostUnbound bool                      `json:"post_unbound"`
}

type windowsProductSupervisorSettlementStateV1 string

const (
	windowsProductSupervisorKnownAbsent       windowsProductSupervisorSettlementStateV1 = "known-absent"
	windowsProductSupervisorGracefullySettled windowsProductSupervisorSettlementStateV1 = "gracefully-settled"
)

type windowsProductSupervisorGenerationV1 struct {
	PID               int    `json:"pid"`
	StartedAt         string `json:"started_at"`
	IPCRequestVersion int    `json:"ipc_request_version"`
}

type windowsProductSupervisorSettlementV1 struct {
	State      windowsProductSupervisorSettlementStateV1 `json:"state"`
	Generation *windowsProductSupervisorGenerationV1     `json:"generation"`
}

type windowsProductQuiescenceReceiptV1 struct {
	ReceiptID               string
	Scope                   string
	ObservationSHA256       string
	CanonicalCLIPath        string
	CanonicalWindowlessPath string
	PriorCLI                windowsProductPreimageV1
	PriorWindowless         windowsProductPreimageV1
	Supervisor              windowsProductSupervisorSettlementV1
	Ports                   []windowsProductPortSettlementV1
	GUIOwner                string
	SupervisorLockReleased  bool
	SettledAtUTC            time.Time
}

type windowsProductReplacementAuthority struct {
	Disposition windowsProductPairDispositionV1
	Quiescence  *windowsProductQuiescenceReceiptV1
}

type windowsProductIntentPortV1 struct {
	TaskName   string
	Port       int
	ArgvSHA256 string
}

type windowsProductQuiescenceObservationV1 struct {
	Supervisor             windowsProductSupervisorSettlementV1
	Ports                  []windowsProductPortSettlementV1
	GUIOwner               string
	SupervisorLockReleased bool
	ObservationSHA256      string
	Retained               *retainedGenerationObservationV1
}

type windowsProductPairLifecycleHooks struct {
	FrozenPorts []windowsProductIntentPortV1
	Observe     func(context.Context, string, []windowsProductIntentPortV1) (windowsProductQuiescenceObservationV1, error)
	Retained    *retainedGenerationLifecycleHooks
	Now         func() time.Time
}

type windowsProductPairPreflightV1 struct {
	Disposition     windowsProductPairDispositionV1
	PriorCLI        windowsProductPreimageV1
	PriorWindowless windowsProductPreimageV1
}

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
	RecoveryDescriptor() (WindowsProductPairTaskRecovery, error)
	RewriteCommand() error
	VerifyRuntime() error
	RestoreImport() error
	DiscardRecoveryStore() error
	Close() error
}

type WindowsProductPairTaskBackup struct {
	TaskName string `json:"task_name"`
	Ref      string `json:"opaque_ref"`
	SHA256   string `json:"sha256"`
}

type WindowsProductPairTaskRecovery struct {
	StoreRoot string                         `json:"retained_store_root"`
	Backups   []WindowsProductPairTaskBackup `json:"backups"`
}

type windowsProductPairRecoveryArtifact struct {
	Target                 string   `json:"target"`
	PriorPresent           bool     `json:"prior_present"`
	PriorSHA256            string   `json:"prior_sha256"`
	NewSHA256              string   `json:"new_sha256"`
	BaselineAsideBasenames []string `json:"baseline_generated_aside_basenames"`
}

type windowsProductPairRecoveryReceipt struct {
	Path         string `json:"path"`
	PriorPresent bool   `json:"prior_present"`
	PriorBytes   []byte `json:"prior_bytes"`
}

type windowsProductPairRecoveryStaged struct {
	CLIPath          string `json:"cli_path"`
	CLISHA256        string `json:"cli_sha256"`
	WindowlessPath   string `json:"windowless_path"`
	WindowlessSHA256 string `json:"windowless_sha256"`
}

type windowsProductPairRecoveryJournal struct {
	Schema                  string                              `json:"schema"`
	TransactionID           string                              `json:"transaction_id"`
	Phase                   WindowsProductPairRecoveryPhase     `json:"phase"`
	Mode                    WindowsProductPairMode              `json:"mode"`
	CLI                     windowsProductPairRecoveryArtifact  `json:"cli"`
	Windowless              windowsProductPairRecoveryArtifact  `json:"windowless"`
	Receipt                 windowsProductPairRecoveryReceipt   `json:"receipt"`
	Tasks                   WindowsProductPairTaskRecovery      `json:"tasks"`
	Staged                  windowsProductPairRecoveryStaged    `json:"staged"`
	PriorFleetReaped        bool                                `json:"prior_fleet_reaped"`
	Quiescence              *windowsProductRecoveryQuiescenceV2 `json:"quiescence,omitempty"`
	QuiescenceReceiptSHA256 string                              `json:"quiescence_receipt_sha256,omitempty"`
}

type windowsProductRecoveryQuiescenceV2 struct {
	ReceiptID                 string                               `json:"receipt_id"`
	Scope                     string                               `json:"scope"`
	ObservationSHA256         string                               `json:"observation_sha256"`
	CanonicalCLIPathSHA256    string                               `json:"canonical_cli_path_sha256"`
	CanonicalWindowlessSHA256 string                               `json:"canonical_windowless_path_sha256"`
	PriorCLI                  windowsProductPreimageV1             `json:"prior_cli"`
	PriorWindowless           windowsProductPreimageV1             `json:"prior_windowless"`
	Supervisor                windowsProductSupervisorSettlementV1 `json:"supervisor"`
	Ports                     []windowsProductPortSettlementV1     `json:"ports"`
	GUIOwner                  string                               `json:"gui_owner"`
	SupervisorLockReleased    bool                                 `json:"supervisor_lock_released"`
	SettledAtUTC              string                               `json:"settled_at"`
}

type windowsProductPairRecoveryJournalV1 struct {
	Schema           string                             `json:"schema"`
	TransactionID    string                             `json:"transaction_id"`
	Phase            WindowsProductPairRecoveryPhase    `json:"phase"`
	Mode             WindowsProductPairMode             `json:"mode"`
	CLI              windowsProductPairRecoveryArtifact `json:"cli"`
	Windowless       windowsProductPairRecoveryArtifact `json:"windowless"`
	Receipt          windowsProductPairRecoveryReceipt  `json:"receipt"`
	Tasks            WindowsProductPairTaskRecovery     `json:"tasks"`
	Staged           windowsProductPairRecoveryStaged   `json:"staged"`
	PriorFleetReaped bool                               `json:"prior_fleet_reaped"`
}

type windowsProductRecoveryAsideV3 struct {
	Basename string `json:"basename"`
	SHA256   string `json:"sha256"`
}

type windowsProductRecoveryArtifactV3 struct {
	Target         string                          `json:"target"`
	PriorPresent   bool                            `json:"prior_present"`
	PriorSHA256    string                          `json:"prior_sha256"`
	NewSHA256      string                          `json:"new_sha256"`
	BaselineAsides []windowsProductRecoveryAsideV3 `json:"baseline_asides"`
}

type windowsProductRecoveryLegacyWindowlessV3 struct {
	Target         string                          `json:"target"`
	PriorPresent   bool                            `json:"prior_present"`
	PriorSHA256    string                          `json:"prior_sha256"`
	BaselineAsides []windowsProductRecoveryAsideV3 `json:"baseline_asides"`
}

type windowsProductRecoveryReceiptsV3 struct {
	V3 windowsProductPairRecoveryReceipt `json:"v3"`
	V2 windowsProductPairRecoveryReceipt `json:"v2"`
}

type windowsProductRecoveryStagedV3 struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type windowsProductFrozenPortV3 struct {
	TaskName   string `json:"task_name"`
	Port       int    `json:"port"`
	ArgvSHA256 string `json:"argv_sha256"`
}

type windowsProductPreSettlementV3 struct {
	Supervisor             windowsProductSupervisorSettlementV1 `json:"supervisor"`
	Ports                  []windowsProductPortSettlementV1     `json:"ports"`
	GUIOwner               string                               `json:"gui_owner"`
	SupervisorLockReleased bool                                 `json:"supervisor_lock_released"`
}

type windowsProductRecoveryJournalV3 struct {
	Schema                  string                                   `json:"schema"`
	TransactionID           string                                   `json:"transaction_id"`
	Phase                   WindowsProductRecoveryPhase              `json:"phase"`
	Mode                    WindowsProductPairMode                   `json:"mode"`
	Product                 windowsProductRecoveryArtifactV3         `json:"product"`
	LegacyWindowless        windowsProductRecoveryLegacyWindowlessV3 `json:"legacy_windowless"`
	Receipts                windowsProductRecoveryReceiptsV3         `json:"receipts"`
	Tasks                   WindowsProductPairTaskRecovery           `json:"tasks"`
	Staged                  windowsProductRecoveryStagedV3           `json:"staged"`
	FrozenPorts             []windowsProductFrozenPortV3             `json:"frozen_ports"`
	PreSettlement           *windowsProductPreSettlementV3           `json:"pre_settlement"`
	PriorFleetReaped        bool                                     `json:"prior_fleet_reaped"`
	Quiescence              *windowsProductRecoveryQuiescenceV2      `json:"quiescence"`
	QuiescenceReceiptSHA256 string                                   `json:"quiescence_receipt_sha256"`
	TaskSetSHA256           string                                   `json:"task_set_sha256"`
}

// UpgradeReceiptV2 binds both role-correct artifacts after pair readback.
// V1 remains a separate read-compatible historical shape.
type UpgradeReceiptV2 struct {
	Schema      string                             `json:"schema"`
	Mode        WindowsProductPairMode             `json:"mode,omitempty"`
	Admission   string                             `json:"admission"`
	Version     string                             `json:"version"`
	Commit      string                             `json:"commit"`
	BuildDate   string                             `json:"build_date"`
	Artifacts   binaryadmission.WindowsProductPair `json:"artifacts"`
	InstalledAt string                             `json:"installed_at"`
}

type UpgradeReceiptArtifactV3 struct {
	Role      binaryadmission.WindowsArtifactRole `json:"role"`
	Subsystem uint16                              `json:"subsystem"`
	Machine   uint16                              `json:"machine"`
	Version   string                              `json:"version"`
	Commit    string                              `json:"commit"`
	BuildDate string                              `json:"build_date"`
	SHA256    string                              `json:"sha256"`
}

type UpgradeReceiptRetiredV3 struct {
	WindowlessAbsent                 bool `json:"windowless_absent"`
	AdmissionHelperAbsentFromPayload bool `json:"admission_helper_absent_from_payload"`
	RollbackImagesAbsent             bool `json:"rollback_images_absent"`
}

// UpgradeReceiptV3 is the only receipt written by the single-product
// transaction. V1 and V2 remain read-only compatibility shapes.
type UpgradeReceiptV3 struct {
	Schema        string                   `json:"schema"`
	TransactionID string                   `json:"transaction_id"`
	Mode          WindowsProductPairMode   `json:"mode"`
	Admission     string                   `json:"admission"`
	Artifact      UpgradeReceiptArtifactV3 `json:"artifact"`
	TaskSetSHA256 string                   `json:"task_set_sha256"`
	Retired       UpgradeReceiptRetiredV3  `json:"retired"`
	InstalledAt   string                   `json:"installed_at"`
}

// UpgradeReceipt is a schema-discriminated compatibility result.
type UpgradeReceipt struct {
	V1 *UpgradeReceiptV1
	V2 *UpgradeReceiptV2
	V3 *UpgradeReceiptV3
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
	case UpgradeReceiptSchemaV3:
		var v3 UpgradeReceiptV3
		if err := json.Unmarshal(raw, &v3); err != nil {
			return UpgradeReceipt{}, err
		}
		if err := validateUpgradeReceiptV3(v3); err != nil {
			return UpgradeReceipt{}, err
		}
		return UpgradeReceipt{V3: &v3}, nil
	default:
		return UpgradeReceipt{}, fmt.Errorf("unsupported upgrade receipt schema %q", header.Schema)
	}
}

func validateUpgradeReceiptV3(receipt UpgradeReceiptV3) error {
	if receipt.Schema != UpgradeReceiptSchemaV3 || !validRecoveryHex(receipt.TransactionID, 16) {
		return errors.New("decode upgrade-receipt-v3: invalid transaction identity")
	}
	if receipt.Mode != WindowsProductPairModeSetup && receipt.Mode != WindowsProductPairModeCanonicalize && receipt.Mode != WindowsProductPairModeUpgrade {
		return errors.New("decode upgrade-receipt-v3: invalid mode")
	}
	if receipt.Admission != UpgradeAdmissionLocalProductV3 ||
		receipt.Artifact.Role != binaryadmission.WindowsArtifactRoleProduct ||
		receipt.Artifact.Subsystem != binaryadmission.WindowsGUISubsystem ||
		receipt.Artifact.Machine == 0 ||
		strings.TrimSpace(receipt.Artifact.Version) == "" ||
		strings.TrimSpace(receipt.Artifact.Commit) == "" ||
		strings.TrimSpace(receipt.Artifact.BuildDate) == "" ||
		!validLowerHexSHA256(receipt.Artifact.SHA256) {
		return errors.New("decode upgrade-receipt-v3: invalid product identity")
	}
	if !validLowerHexSHA256(receipt.TaskSetSHA256) ||
		!receipt.Retired.WindowlessAbsent ||
		!receipt.Retired.AdmissionHelperAbsentFromPayload ||
		!receipt.Retired.RollbackImagesAbsent {
		return errors.New("decode upgrade-receipt-v3: incomplete cleanup proof")
	}
	installedAt, err := time.Parse(time.RFC3339Nano, receipt.InstalledAt)
	if err != nil || installedAt.Location() != time.UTC {
		return errors.New("decode upgrade-receipt-v3: invalid installed_at")
	}
	return nil
}

var ErrWindowsProductCleanupPending = errors.New("E_WINDOWS_PRODUCT_CLEANUP_PENDING")
var ErrWindowsProductRecoveryIdentity = errors.New("E_WINDOWS_PRODUCT_RECOVERY_IDENTITY")

type WindowsProductStage string

const (
	WindowsProductStageProductPromoted   WindowsProductStage = "product-promoted"
	WindowsProductStageProductReadBack   WindowsProductStage = "product-read-back"
	WindowsProductStageTasksRewritten    WindowsProductStage = "tasks-rewritten"
	WindowsProductStageTasksReadBack     WindowsProductStage = "tasks-read-back"
	WindowsProductStageSupervisorStarted WindowsProductStage = "supervisor-started"
	WindowsProductStageSupervisorReady   WindowsProductStage = "supervisor-ready"
	WindowsProductStageLegacyRetired     WindowsProductStage = "legacy-retired"
	WindowsProductStageCommitDecided     WindowsProductStage = "commit-decided"
	WindowsProductStageCleanupVerified   WindowsProductStage = "cleanup-verified"
	WindowsProductStageReceiptWritten    WindowsProductStage = "receipt-written"
)

type WindowsProductTaskTxn interface {
	InventoryExport() error
	RecoveryDescriptor() (WindowsProductPairTaskRecovery, error)
	RewriteCommand() error
	VerifyRuntimeSHA256() (string, error)
	RestoreImport() error
	DiscardRecoveryStore() error
	Close() error
}

type WindowsProductCommitted struct {
	Schema        string
	Outcome       WindowsProductPairOutcome
	Artifact      binaryadmission.WindowsArtifact
	TaskSetSHA256 string
	ReceiptSHA256 string
	EventSHA256   string
}

type WindowsProductCommitEvent struct {
	Schema        string
	TransactionID string
	Mode          WindowsProductPairMode
	Artifact      UpgradeReceiptArtifactV3
	TaskSetSHA256 string
	Retired       UpgradeReceiptRetiredV3
	ReceiptSHA256 string
	InstalledAt   string
}

type WindowsProductTxnDeps struct {
	AdmitProduct           func(binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error)
	AdmitLegacyWindowless  func(binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error)
	Promote                func(src, dst, newSHA256 string) (WindowsProductPairPromotion, error)
	RestorePromotion       func(WindowsProductPairPromotion) (string, error)
	RetainLegacy           func(string) (string, error)
	RestoreNoReplace       func(string, string) error
	ListGeneratedAsides    func(string) ([]string, error)
	HashFile               func(string) (string, error)
	RemoveExact            func(string) error
	StartSupervisor        func(string) error
	WaitSupervisorReady    func(context.Context, string, binaryadmission.WindowsArtifact) error
	RestartPrior           func(string) error
	RecoverPriorSettlement func(windowsProductRecoveryJournalV3) error
	SettleSuccessor        func(string) error
	WriteReceipt           func(string, []byte) error
	ReadReceipt            func(string) ([]byte, error)
	RemoveReceipt          func(string) error
	WriteRecoveryJournal   func(string, []byte) error
	ReadRecoveryJournal    func(string) ([]byte, error)
	RemoveRecoveryJournal  func(string) error
	PublishCommitted       func(WindowsProductCommitEvent) (string, error)
	Fault                  func(WindowsProductStage) error
	Now                    func() time.Time
}

type WindowsProductTxnOpts struct {
	StateDir             string
	ProductPath          string
	LegacyWindowlessPath string
	StagedProduct        string
	ReceiptV3Path        string
	ReceiptV2Path        string
	Mode                 WindowsProductPairMode
	FrozenPorts          []windowsProductIntentPortV1
	PreSettlement        *windowsProductQuiescenceObservationV1
	PriorFleetReaped     bool
	ReplacementAuthority windowsProductReplacementAuthority
	Tasks                WindowsProductTaskTxn
	Deps                 WindowsProductTxnDeps
}

type windowsProductPreparedTxn struct {
	deps             WindowsProductTxnDeps
	staged           binaryadmission.WindowsArtifact
	productPrior     productPairFileSnapshot
	legacyPrior      productPairFileSnapshot
	receiptV3Prior   productPairFileSnapshot
	receiptV2Prior   productPairFileSnapshot
	journal          windowsProductRecoveryJournalV3
	productPromotion *WindowsProductPairPromotion
	legacyRetained   string
	successorStarted bool
	disposition      windowsProductPairDispositionV1
}

// WindowsProductTxn is the sole V3 writer for canonical product, task,
// recovery, receipt, cleanup, and committed-event state.
type WindowsProductTxn struct {
	Opts             WindowsProductTxnOpts
	prepared         *windowsProductPreparedTxn
	constructorStore interface{ Close() error }
}

// inspectWindowsProductRecoveryLeaves observes both exact Product recovery
// owners without opening, parsing, creating, or settling either journal.
func inspectWindowsProductRecoveryLeaves(stateDir string) (present bool, err error) {
	root, err := os.Lstat(stateDir)
	if err != nil || !root.IsDir() || root.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("Windows Product state root is unverifiable: %v", err)
	}
	var uncertain []error
	for _, leaf := range []string{WindowsProductRecoveryFileLeafV3, WindowsProductPairRecoveryFileLeaf} {
		info, statErr := os.Lstat(filepath.Join(stateDir, leaf))
		switch {
		case statErr == nil && info.Mode().IsRegular():
			present = true
		case statErr == nil:
			uncertain = append(uncertain, fmt.Errorf("recovery journal leaf %s is not regular", leaf))
		case errors.Is(statErr, os.ErrNotExist):
		default:
			uncertain = append(uncertain, fmt.Errorf("inspect recovery journal %s: %w", leaf, statErr))
		}
	}
	if len(uncertain) != 0 {
		return present, errors.Join(uncertain...)
	}
	return present, nil
}

func (t *WindowsProductTxn) Close() error {
	if t == nil || t.Opts.Tasks == nil {
		return nil
	}
	lockErr := t.Opts.Tasks.Close()
	if t.constructorStore == nil {
		return lockErr
	}
	journalPresent, err := inspectWindowsProductRecoveryLeaves(t.Opts.StateDir)
	if err != nil {
		return errors.Join(lockErr, fmt.Errorf("UPGRADE_GUI_TASK_STORE_CLEANUP_UNCONFIRMED: journal state is unverifiable: %w", err))
	}
	if journalPresent {
		return lockErr // A durable journal may still need this exact store.
	}
	if err := t.constructorStore.Close(); err != nil {
		return errors.Join(lockErr, fmt.Errorf("UPGRADE_GUI_TASK_STORE_CLEANUP_UNCONFIRMED: discard unjournaled task store: %w", err))
	}
	return lockErr
}

func (t *WindowsProductTxn) Prepare() error {
	if t == nil {
		return errors.New("Windows product transaction is required")
	}
	if t.prepared != nil {
		return nil
	}
	o := t.Opts
	if o.StateDir == "" && o.ReceiptV3Path != "" {
		o.StateDir = filepath.Dir(o.ReceiptV3Path)
	}
	if o.ReceiptV3Path == "" {
		o.ReceiptV3Path = filepath.Join(o.StateDir, UpgradeReceiptSchemaV3+".json")
	}
	if o.ReceiptV2Path == "" {
		o.ReceiptV2Path = filepath.Join(o.StateDir, UpgradeReceiptSchemaV2+".json")
	}
	if o.Tasks == nil || !cleanAbsolutePath(o.StateDir) || !cleanAbsolutePath(o.ProductPath) ||
		!cleanAbsolutePath(o.LegacyWindowlessPath) || !cleanAbsolutePath(o.StagedProduct) ||
		!cleanAbsolutePath(o.ReceiptV3Path) || !cleanAbsolutePath(o.ReceiptV2Path) {
		return errors.New("Windows product transaction: state, task, and artifact paths are required")
	}
	if o.Mode != WindowsProductPairModeSetup && o.Mode != WindowsProductPairModeCanonicalize && o.Mode != WindowsProductPairModeUpgrade {
		return errors.New("Windows product transaction: valid caller mode is required")
	}
	d := withWindowsProductDefaults(o.Deps)
	if d.PublishCommitted == nil {
		return errors.New("Windows product transaction: committed publisher is required")
	}
	staged, err := d.AdmitProduct(binaryadmission.WindowsArtifact{Path: o.StagedProduct, Role: binaryadmission.WindowsArtifactRoleProduct})
	if err != nil {
		return fmt.Errorf("E_WINDOWS_PRODUCT_ADMISSION: staged product: %w", err)
	}
	productPrior, err := snapshotProductPairFile(o.ProductPath)
	if err != nil {
		return err
	}
	legacyPrior, err := snapshotProductPairFile(o.LegacyWindowlessPath)
	if err != nil {
		return err
	}
	if legacyPrior.present {
		legacyDigest := windowsProductPreimage(legacyPrior).SHA256
		if _, err := d.AdmitLegacyWindowless(binaryadmission.WindowsArtifact{
			Path: o.LegacyWindowlessPath, Role: binaryadmission.WindowsArtifactRoleWindowless, SHA256: legacyDigest,
		}); err != nil {
			return fmt.Errorf("E_WINDOWS_PRODUCT_ADMISSION: legacy windowless: %w", err)
		}
	}
	v3Prior, err := snapshotProductPairFile(o.ReceiptV3Path)
	if err != nil {
		return err
	}
	v2Prior, err := snapshotProductPairFile(o.ReceiptV2Path)
	if err != nil {
		return err
	}
	if err := o.Tasks.InventoryExport(); err != nil {
		return fmt.Errorf("snapshot exact-owned task XML: %w", err)
	}
	tasks, err := o.Tasks.RecoveryDescriptor()
	if err != nil || !cleanAbsolutePath(tasks.StoreRoot) {
		return fmt.Errorf("task recovery descriptor unavailable: %w", err)
	}
	productAsides, err := snapshotWindowsProductAsides(o.ProductPath, d)
	if err != nil {
		return fmt.Errorf("inventory canonical product asides: %w", err)
	}
	legacyAsides, err := snapshotWindowsProductAsides(o.LegacyWindowlessPath, d)
	if err != nil {
		return fmt.Errorf("inventory legacy windowless asides: %w", err)
	}
	id, err := newWindowsProductPairTransactionID()
	if err != nil {
		return err
	}
	productDisposition := windowsProductTargetDisposition(productPrior, staged.SHA256)
	disposition := windowsProductPairDispositionV1{
		CLI:        productDisposition,
		Windowless: windowsProductTargetIdentical,
	}
	journal := windowsProductRecoveryJournalV3{
		Schema: WindowsProductRecoverySchemaV3, TransactionID: id,
		Phase: WindowsProductRecoveryPrepared, Mode: o.Mode,
		Product: windowsProductRecoveryArtifactV3{
			Target: o.ProductPath, PriorPresent: productPrior.present,
			PriorSHA256: windowsProductPreimage(productPrior).SHA256,
			NewSHA256:   staged.SHA256, BaselineAsides: productAsides,
		},
		LegacyWindowless: windowsProductRecoveryLegacyWindowlessV3{
			Target: o.LegacyWindowlessPath, PriorPresent: legacyPrior.present,
			PriorSHA256:    windowsProductPreimage(legacyPrior).SHA256,
			BaselineAsides: legacyAsides,
		},
		Receipts: windowsProductRecoveryReceiptsV3{
			V3: recoveryReceiptFromSnapshot(v3Prior),
			V2: recoveryReceiptFromSnapshot(v2Prior),
		},
		Tasks:         tasks,
		Staged:        windowsProductRecoveryStagedV3{Path: o.StagedProduct, SHA256: staged.SHA256},
		FrozenPorts:   projectWindowsProductFrozenPorts(o.FrozenPorts),
		PreSettlement: projectWindowsProductPreSettlement(o.PreSettlement),
	}
	if o.Mode == WindowsProductPairModeUpgrade && o.PreSettlement != nil &&
		o.PreSettlement.GUIOwner == windowsProductGUIOwnerConfirmedPresent {
		journal.Phase = WindowsProductRecoveryGUIOwnedStopPending
	}
	if o.ReplacementAuthority.Quiescence != nil {
		projection, digest, err := projectWindowsProductRecoveryQuiescence(*o.ReplacementAuthority.Quiescence)
		if err != nil {
			return fmt.Errorf("project quiescence receipt: %w", err)
		}
		journal.PriorFleetReaped = true
		journal.Quiescence = &projection
		journal.QuiescenceReceiptSHA256 = digest
	}
	path := filepath.Join(o.StateDir, WindowsProductRecoveryFileLeafV3)
	if err := persistWindowsProductRecoveryJournalV3(path, journal, d.WriteRecoveryJournal, d.ReadRecoveryJournal); err != nil {
		return fmt.Errorf("%w: prepare V3 journal: %v", ErrWindowsProductPairRecoveryPrepare, err)
	}
	t.Opts = o
	t.prepared = &windowsProductPreparedTxn{
		deps: d, staged: staged, productPrior: productPrior, legacyPrior: legacyPrior,
		receiptV3Prior: v3Prior, receiptV2Prior: v2Prior, journal: journal,
		disposition: disposition,
	}
	return nil
}

func (t *WindowsProductTxn) Run(ctx context.Context) (result WindowsProductCommitted, retErr error) {
	var tasks WindowsProductTaskTxn
	if t != nil {
		tasks = t.Opts.Tasks
	}
	preserveRecovery := true
	if tasks != nil {
		defer func() {
			if closeErr := t.Close(); closeErr != nil {
				retErr = errors.Join(retErr, closeErr)
			}
			if !preserveRecovery && t.constructorStore == nil {
				if err := tasks.DiscardRecoveryStore(); err != nil {
					retErr = errors.Join(retErr, fmt.Errorf("discard Windows product recovery store: %w", err))
				}
			}
		}()
	}
	if err := t.Prepare(); err != nil {
		return WindowsProductCommitted{}, &WindowsProductPairPrePromotionError{Cause: err}
	}
	o, p := t.Opts, t.prepared
	if p.journal.Phase == WindowsProductRecoveryGUIOwnedStopPending {
		return WindowsProductCommitted{}, &WindowsProductPairPrePromotionError{Cause: fmt.Errorf("%w: GUI-owned stop has no durable settled observation", ErrWindowsProductRecoveryPending)}
	}
	if o.ReplacementAuthority.Quiescence != nil && p.journal.Quiescence == nil {
		projection, digest, err := projectWindowsProductRecoveryQuiescence(*o.ReplacementAuthority.Quiescence)
		if err != nil {
			return WindowsProductCommitted{}, t.rollback(fmt.Errorf("project quiescence receipt: %w", err))
		}
		p.journal.PriorFleetReaped = p.journal.PreSettlement != nil &&
			p.journal.PreSettlement.Supervisor.State == windowsProductSupervisorGracefullySettled
		p.journal.Quiescence = &projection
		p.journal.QuiescenceReceiptSHA256 = digest
		if err := t.persistProductJournal(); err != nil {
			return WindowsProductCommitted{}, t.rollback(fmt.Errorf("persist settled quiescence in V3 journal: %w", err))
		}
	}
	if err := validateWindowsSingleProductReplacementAuthority(o, p.disposition, p.productPrior, p.legacyPrior); err != nil {
		return WindowsProductCommitted{}, t.rollback(err)
	}
	fault := func(stage WindowsProductStage) error {
		if p.deps.Fault == nil {
			return nil
		}
		if err := p.deps.Fault(stage); err != nil {
			return fmt.Errorf("fault after %s: %w", stage, err)
		}
		return nil
	}
	if p.disposition.CLI != windowsProductTargetIdentical {
		promotion, err := p.deps.Promote(o.StagedProduct, o.ProductPath, p.staged.SHA256)
		p.productPromotion = rollbackProductPairPromotion(promotion, p.productPrior)
		if err != nil {
			return WindowsProductCommitted{}, t.rollback(fmt.Errorf("promote canonical product: %w", err))
		}
		if err := verifyProductPairPromotion(promotion, p.productPrior); err != nil {
			return WindowsProductCommitted{}, t.rollback(fmt.Errorf("verify retained product prior: %w", err))
		}
	}
	if err := fault(WindowsProductStageProductPromoted); err != nil {
		return WindowsProductCommitted{}, t.rollback(err)
	}
	readBack, err := p.deps.AdmitProduct(binaryadmission.WindowsArtifact{
		Path: o.ProductPath, Role: binaryadmission.WindowsArtifactRoleProduct, SHA256: p.staged.SHA256,
	})
	if err != nil || !sameWindowsProductIdentity(p.staged, readBack) {
		if err == nil {
			err = errors.New("canonical product identity differs from staged product")
		}
		return WindowsProductCommitted{}, t.rollback(fmt.Errorf("read back canonical product: %w", err))
	}
	if err := fault(WindowsProductStageProductReadBack); err != nil {
		return WindowsProductCommitted{}, t.rollback(err)
	}
	if err := o.Tasks.RewriteCommand(); err != nil {
		return WindowsProductCommitted{}, t.rollback(fmt.Errorf("rewrite exact-owned task commands: %w", err))
	}
	if err := fault(WindowsProductStageTasksRewritten); err != nil {
		return WindowsProductCommitted{}, t.rollback(err)
	}
	taskDigest, err := o.Tasks.VerifyRuntimeSHA256()
	if err != nil {
		return WindowsProductCommitted{}, t.rollback(fmt.Errorf("read back exact-owned task commands: %w", err))
	}
	p.journal.TaskSetSHA256 = taskDigest
	p.journal.Phase = WindowsProductRecoverySuccessorStarting
	if err := t.persistProductJournal(); err != nil {
		return WindowsProductCommitted{}, t.rollback(fmt.Errorf("persist successor-starting V3 recovery phase: %w", err))
	}
	if err := fault(WindowsProductStageTasksReadBack); err != nil {
		return WindowsProductCommitted{}, t.rollback(err)
	}
	if p.deps.StartSupervisor != nil {
		if err := p.deps.StartSupervisor(o.ProductPath); err != nil {
			return WindowsProductCommitted{}, t.rollback(fmt.Errorf("start canonical supervisor: %w", err))
		}
		p.successorStarted = true
	}
	if err := fault(WindowsProductStageSupervisorStarted); err != nil {
		return WindowsProductCommitted{}, t.rollback(err)
	}
	if p.deps.WaitSupervisorReady != nil {
		if err := p.deps.WaitSupervisorReady(ctx, o.ProductPath, readBack); err != nil {
			return WindowsProductCommitted{}, t.rollback(fmt.Errorf("canonical supervisor readiness: %w", err))
		}
	}
	if err := fault(WindowsProductStageSupervisorReady); err != nil {
		return WindowsProductCommitted{}, t.rollback(err)
	}
	if p.legacyPrior.present {
		retained, err := p.deps.RetainLegacy(o.LegacyWindowlessPath)
		if err != nil {
			return WindowsProductCommitted{}, t.rollback(fmt.Errorf("retire legacy windowless: %w", err))
		}
		p.legacyRetained = retained
		if err := verifyBoundWindowsProductFile(retained, p.legacyPrior, o.LegacyWindowlessPath, p.deps); err != nil {
			return WindowsProductCommitted{}, t.rollback(fmt.Errorf("verify retired legacy windowless: %w", err))
		}
	}
	if _, err := os.Stat(o.LegacyWindowlessPath); !errors.Is(err, os.ErrNotExist) {
		return WindowsProductCommitted{}, t.rollback(fmt.Errorf("legacy windowless target was not retired: %v", err))
	}
	if err := fault(WindowsProductStageLegacyRetired); err != nil {
		return WindowsProductCommitted{}, t.rollback(err)
	}
	p.journal.Phase = WindowsProductRecoveryCommitDecided
	if err := t.persistProductJournal(); err != nil {
		return WindowsProductCommitted{}, t.rollback(fmt.Errorf("persist commit-decided V3 recovery phase: %w", err))
	}
	if err := fault(WindowsProductStageCommitDecided); err != nil {
		return WindowsProductCommitted{}, fmt.Errorf("%w: %v", ErrWindowsProductCleanupPending, err)
	}
	committed, err := t.finishProductCommit(ctx, readBack, false)
	if err != nil {
		return committed, err
	}
	preserveRecovery = false
	return committed, nil
}

func validateWindowsSingleProductReplacementAuthority(o WindowsProductTxnOpts, actual windowsProductPairDispositionV1, productPrior, legacyPrior productPairFileSnapshot) error {
	authority := o.ReplacementAuthority
	if authority.Disposition != actual {
		if windowsProductPairDispositionNeedsReplacement(actual) {
			return ErrWindowsProductPairQuiescenceRequired
		}
		return errors.New("Windows product disposition authority does not match target preimages")
	}
	if o.Mode == WindowsProductPairModeUpgrade {
		if authority.Quiescence == nil {
			return ErrWindowsProductPairQuiescenceRequired
		}
		receipt := authority.Quiescence
		if receipt.CanonicalCLIPath != o.ProductPath ||
			receipt.CanonicalWindowlessPath != o.LegacyWindowlessPath ||
			receipt.PriorCLI != windowsProductPreimage(productPrior) ||
			receipt.PriorWindowless != windowsProductPreimage(legacyPrior) {
			return errors.New("E_WINDOWS_PRODUCT_PREIMAGE_CHANGED")
		}
		return validateWindowsProductQuiescenceReceipt(*receipt)
	}
	if windowsProductPairDispositionNeedsReplacement(actual) {
		return ErrWindowsProductPairReplacementRequiresUpgrade
	}
	if authority.Quiescence != nil {
		return errors.New("Windows product quiescence receipt is forbidden outside upgrade")
	}
	return nil
}

func withWindowsProductDefaults(d WindowsProductTxnDeps) WindowsProductTxnDeps {
	pair := withWindowsProductPairDefaults(WindowsProductPairTxnDeps{
		Promote: d.Promote, RestorePromotion: d.RestorePromotion,
		WriteReceipt: d.WriteReceipt, ReadReceipt: d.ReadReceipt,
		WriteRecoveryJournal: d.WriteRecoveryJournal, ReadRecoveryJournal: d.ReadRecoveryJournal,
		RemoveRecoveryJournal: d.RemoveRecoveryJournal,
		ListGeneratedAsides:   d.ListGeneratedAsides, Now: d.Now,
	})
	if d.AdmitProduct == nil {
		d.AdmitProduct = binaryadmission.AdmitWindowsRole
	}
	if d.AdmitLegacyWindowless == nil {
		d.AdmitLegacyWindowless = binaryadmission.AdmitWindowsRole
	}
	d.Promote = pair.Promote
	d.RestorePromotion = pair.RestorePromotion
	d.WriteReceipt = pair.WriteReceipt
	d.ReadReceipt = pair.ReadReceipt
	d.WriteRecoveryJournal = pair.WriteRecoveryJournal
	d.ReadRecoveryJournal = pair.ReadRecoveryJournal
	d.RemoveRecoveryJournal = pair.RemoveRecoveryJournal
	d.ListGeneratedAsides = pair.ListGeneratedAsides
	d.Now = pair.Now
	if d.RetainLegacy == nil {
		d.RetainLegacy = api.RetainBinaryTargetAside
	}
	if d.RestoreNoReplace == nil {
		d.RestoreNoReplace = moveWindowsFileNoReplace
	}
	if d.HashFile == nil {
		d.HashFile = hashRecoveryFile
	}
	if d.RemoveExact == nil {
		d.RemoveExact = os.Remove
	}
	if d.RemoveReceipt == nil {
		d.RemoveReceipt = removeRecoveryLeaf
	}
	return d
}

func recoveryReceiptFromSnapshot(snapshot productPairFileSnapshot) windowsProductPairRecoveryReceipt {
	var body []byte
	if snapshot.present {
		body = append([]byte(nil), snapshot.body...)
	}
	return windowsProductPairRecoveryReceipt{
		Path: snapshot.path, PriorPresent: snapshot.present, PriorBytes: body,
	}
}

func snapshotWindowsProductAsides(target string, d WindowsProductTxnDeps) ([]windowsProductRecoveryAsideV3, error) {
	names, err := d.ListGeneratedAsides(target)
	if err != nil {
		return nil, err
	}
	rows := make([]windowsProductRecoveryAsideV3, 0, len(names))
	for _, name := range names {
		path := filepath.Join(filepath.Dir(target), name)
		digest, err := d.HashFile(path)
		if err != nil {
			return nil, err
		}
		rows = append(rows, windowsProductRecoveryAsideV3{Basename: name, SHA256: digest})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Basename < rows[j].Basename })
	return rows, nil
}

func projectWindowsProductFrozenPorts(rows []windowsProductIntentPortV1) []windowsProductFrozenPortV3 {
	projected := make([]windowsProductFrozenPortV3, 0, len(rows))
	for _, row := range rows {
		projected = append(projected, windowsProductFrozenPortV3{
			TaskName: row.TaskName, Port: row.Port, ArgvSHA256: row.ArgvSHA256,
		})
	}
	sort.Slice(projected, func(i, j int) bool {
		if projected[i].TaskName != projected[j].TaskName {
			return projected[i].TaskName < projected[j].TaskName
		}
		if projected[i].Port != projected[j].Port {
			return projected[i].Port < projected[j].Port
		}
		return projected[i].ArgvSHA256 < projected[j].ArgvSHA256
	})
	return projected
}

func projectWindowsProductPreSettlement(observation *windowsProductQuiescenceObservationV1) *windowsProductPreSettlementV3 {
	if observation == nil {
		return nil
	}
	return &windowsProductPreSettlementV3{
		Supervisor:             observation.Supervisor,
		Ports:                  append(make([]windowsProductPortSettlementV1, 0, len(observation.Ports)), observation.Ports...),
		GUIOwner:               observation.GUIOwner,
		SupervisorLockReleased: observation.SupervisorLockReleased,
	}
}

func expandWindowsProductFrozenPorts(rows []windowsProductFrozenPortV3) []windowsProductIntentPortV1 {
	expanded := make([]windowsProductIntentPortV1, 0, len(rows))
	for _, row := range rows {
		expanded = append(expanded, windowsProductIntentPortV1{
			TaskName: row.TaskName, Port: row.Port, ArgvSHA256: row.ArgvSHA256,
		})
	}
	return expanded
}

func expandWindowsProductPreSettlement(pre *windowsProductPreSettlementV3) *windowsProductQuiescenceObservationV1 {
	if pre == nil {
		return nil
	}
	return &windowsProductQuiescenceObservationV1{
		Supervisor:             pre.Supervisor,
		Ports:                  append([]windowsProductPortSettlementV1(nil), pre.Ports...),
		GUIOwner:               pre.GUIOwner,
		SupervisorLockReleased: pre.SupervisorLockReleased,
	}
}

func sameWindowsProductIdentity(a, b binaryadmission.WindowsArtifact) bool {
	return a.Role == b.Role && a.Machine == b.Machine && a.Subsystem == b.Subsystem &&
		a.Version == b.Version && a.Commit == b.Commit && a.BuildDate == b.BuildDate &&
		a.SHA256 == b.SHA256
}

func (t *WindowsProductTxn) persistProductJournal() error {
	p := t.prepared
	return persistWindowsProductRecoveryJournalV3(
		filepath.Join(t.Opts.StateDir, WindowsProductRecoveryFileLeafV3),
		p.journal, p.deps.WriteRecoveryJournal, p.deps.ReadRecoveryJournal,
	)
}

// SettleGUIOwnedStop binds the observed absence to the original live preimage
// and reads the resulting prepared journal back before Product promotion.
func (t *WindowsProductTxn) SettleGUIOwnedStop(receipt windowsProductQuiescenceReceiptV1) error {
	if t == nil || t.prepared == nil || t.prepared.journal.Phase != WindowsProductRecoveryGUIOwnedStopPending {
		return fmt.Errorf("%w: no GUI-owned stop is pending", ErrWindowsProductRecoveryPending)
	}
	projection, digest, err := projectWindowsProductRecoveryQuiescence(receipt)
	if err != nil {
		return fmt.Errorf("%w: invalid GUI-owned settlement: %v", ErrWindowsProductRecoveryPending, err)
	}
	p := t.prepared
	if !sameWindowsProductPath(receipt.CanonicalCLIPath, p.journal.Product.Target) ||
		!sameWindowsProductPath(receipt.CanonicalWindowlessPath, p.journal.LegacyWindowless.Target) ||
		receipt.PriorCLI != (windowsProductPreimageV1{Present: p.journal.Product.PriorPresent, SHA256: p.journal.Product.PriorSHA256}) ||
		receipt.PriorWindowless != (windowsProductPreimageV1{Present: p.journal.LegacyWindowless.PriorPresent, SHA256: p.journal.LegacyWindowless.PriorSHA256}) {
		return fmt.Errorf("%w: GUI-owned settlement changed Product preimage", ErrWindowsProductRecoveryPending)
	}
	previous := p.journal
	p.journal.Phase = WindowsProductRecoveryPrepared
	p.journal.PriorFleetReaped = true
	p.journal.Quiescence = &projection
	p.journal.QuiescenceReceiptSHA256 = digest
	if err := t.persistProductJournal(); err != nil {
		p.journal = previous
		return fmt.Errorf("%w: persist GUI-owned settlement: %v", ErrWindowsProductRecoveryPending, err)
	}
	t.Opts.ReplacementAuthority.Quiescence = &receipt
	return nil
}

func (t *WindowsProductTxn) rollback(trigger error) error {
	o, p := t.Opts, t.prepared
	var restoreErrs []error
	if p.successorStarted {
		if p.deps.SettleSuccessor == nil {
			restoreErrs = append(restoreErrs, errors.New("settle transaction-started successor: no settlement owner"))
		} else if err := p.deps.SettleSuccessor(o.ProductPath); err != nil {
			restoreErrs = append(restoreErrs, fmt.Errorf("settle transaction-started successor: %w", err))
		}
	}
	pairDeps := withWindowsProductPairDefaults(WindowsProductPairTxnDeps{RestorePromotion: p.deps.RestorePromotion})
	if err := restoreProductPairPromotion(pairDeps, p.productPromotion, p.productPrior); err != nil {
		restoreErrs = append(restoreErrs, fmt.Errorf("restore canonical product: %w", err))
	}
	if err := o.Tasks.RestoreImport(); err != nil {
		restoreErrs = append(restoreErrs, fmt.Errorf("restore task XML: %w", err))
	}
	if err := restoreLegacyWindowlessPreimage(p, o.LegacyWindowlessPath); err != nil {
		restoreErrs = append(restoreErrs, fmt.Errorf("restore legacy windowless: %w", err))
	}
	for _, snapshot := range []productPairFileSnapshot{p.receiptV3Prior, p.receiptV2Prior} {
		if err := restoreProductPairFile(snapshot); err != nil {
			restoreErrs = append(restoreErrs, fmt.Errorf("restore receipt %s: %w", snapshot.path, err))
		}
	}
	if joined := errors.Join(restoreErrs...); joined != nil {
		return errors.Join(trigger, fmt.Errorf("%w: exact Windows product rollback failed: %v", ErrWindowsProductPairRecoveryRollback, joined))
	}
	if p.journal.PreSettlement != nil && p.journal.PreSettlement.GUIOwner == windowsProductGUIOwnerConfirmedPresent {
		if p.deps.RecoverPriorSettlement == nil {
			return errors.Join(trigger, fmt.Errorf("%w: GUI-owned prior settlement owner is unavailable", ErrWindowsProductRecoveryPending))
		}
		if err := p.deps.RecoverPriorSettlement(p.journal); err != nil {
			return errors.Join(trigger, fmt.Errorf("%w: GUI-owned prior settlement: %v", ErrWindowsProductRecoveryPending, err))
		}
	} else if p.journal.PriorFleetReaped && p.productPrior.present && p.deps.RestartPrior != nil {
		if err := p.deps.RestartPrior(o.ProductPath); err != nil {
			return errors.Join(trigger, fmt.Errorf("%w: restart prior supervisor: %v", ErrWindowsProductPairRecoveryRollback, err))
		}
	}
	p.journal.Phase = WindowsProductRecoverySettledRollback
	p.journal.TaskSetSHA256 = ""
	if err := t.persistProductJournal(); err != nil {
		return errors.Join(trigger, fmt.Errorf("%w: persist settled rollback: %v", ErrWindowsProductPairRecoveryCleanup, err))
	}
	if err := o.Tasks.DiscardRecoveryStore(); err != nil {
		return errors.Join(trigger, fmt.Errorf("%w: discard retained task store: %v", ErrWindowsProductPairRecoveryCleanup, err))
	}
	if err := p.deps.RemoveRecoveryJournal(filepath.Join(o.StateDir, WindowsProductRecoveryFileLeafV3)); err != nil {
		return errors.Join(trigger, fmt.Errorf("%w: remove settled V3 journal: %v", ErrWindowsProductPairRecoveryCleanup, err))
	}
	return fmt.Errorf("%w; exact Windows product rollback completed", trigger)
}

func restoreLegacyWindowlessPreimage(p *windowsProductPreparedTxn, target string) error {
	if !p.legacyPrior.present {
		if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("prior-absent legacy target exists or is unreadable: %v", err)
		}
		return nil
	}
	if p.legacyRetained != "" {
		if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("retired legacy target is not absent before restore: %v", err)
		}
		if err := p.deps.RestoreNoReplace(p.legacyRetained, target); err != nil {
			return err
		}
		p.legacyRetained = ""
	}
	digest, err := p.deps.HashFile(target)
	if err != nil || digest != windowsProductPreimage(p.legacyPrior).SHA256 {
		return errors.New("restored legacy windowless hash differs")
	}
	return nil
}

func verifyBoundWindowsProductFile(path string, prior productPairFileSnapshot, target string, d WindowsProductTxnDeps) error {
	if path == "" || !api.IsGeneratedRenameAsideForTarget(target, path) {
		return errors.New("retained path is not generated for its exact target")
	}
	digest, err := d.HashFile(path)
	if err != nil || digest != windowsProductPreimage(prior).SHA256 {
		return errors.New("retained bytes differ from journaled preimage")
	}
	return nil
}

func (t *WindowsProductTxn) finishProductCommit(_ context.Context, artifact binaryadmission.WindowsArtifact, forwardReplay bool) (WindowsProductCommitted, error) {
	o, p := t.Opts, t.prepared
	if err := cleanupWindowsProductTransactionFiles(o, p, forwardReplay); err != nil {
		return WindowsProductCommitted{Artifact: artifact, TaskSetSHA256: p.journal.TaskSetSHA256, Outcome: WindowsProductPairCommittedCleanupFailed},
			fmt.Errorf("%w: %v", ErrWindowsProductCleanupPending, err)
	}
	if p.deps.Fault != nil {
		if err := p.deps.Fault(WindowsProductStageCleanupVerified); err != nil {
			return WindowsProductCommitted{Artifact: artifact, TaskSetSHA256: p.journal.TaskSetSHA256, Outcome: WindowsProductPairCommittedCleanupFailed},
				fmt.Errorf("%w: fault after cleanup-verified: %v", ErrWindowsProductCleanupPending, err)
		}
	}
	now := p.deps.Now().UTC().Format(time.RFC3339Nano)
	retired := UpgradeReceiptRetiredV3{
		WindowlessAbsent: true, AdmissionHelperAbsentFromPayload: true, RollbackImagesAbsent: true,
	}
	receipt := UpgradeReceiptV3{
		Schema: UpgradeReceiptSchemaV3, TransactionID: p.journal.TransactionID,
		Mode: o.Mode, Admission: UpgradeAdmissionLocalProductV3,
		Artifact: UpgradeReceiptArtifactV3{
			Role: artifact.Role, Subsystem: artifact.Subsystem, Version: artifact.Version,
			Machine: artifact.Machine, Commit: artifact.Commit, BuildDate: artifact.BuildDate, SHA256: artifact.SHA256,
		},
		TaskSetSHA256: p.journal.TaskSetSHA256, Retired: retired, InstalledAt: now,
	}
	raw, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return WindowsProductCommitted{}, fmt.Errorf("%w: encode V3 receipt: %v", ErrWindowsProductCleanupPending, err)
	}
	raw = append(raw, '\n')
	if err := p.deps.WriteReceipt(o.ReceiptV3Path, raw); err != nil {
		return WindowsProductCommitted{}, fmt.Errorf("%w: persist V3 receipt: %v", ErrWindowsProductCleanupPending, err)
	}
	if p.deps.Fault != nil {
		if err := p.deps.Fault(WindowsProductStageReceiptWritten); err != nil {
			return WindowsProductCommitted{}, fmt.Errorf("%w: %v", ErrWindowsProductCleanupPending, err)
		}
	}
	readBack, err := p.deps.ReadReceipt(o.ReceiptV3Path)
	if err != nil || !bytes.Equal(readBack, raw) {
		return WindowsProductCommitted{}, fmt.Errorf("%w: V3 receipt exact-byte readback differs: %v", ErrWindowsProductCleanupPending, err)
	}
	decoded, err := DecodeUpgradeReceipt(readBack)
	if err != nil || decoded.V3 == nil || decoded.V3.TransactionID != p.journal.TransactionID ||
		decoded.V3.Artifact.SHA256 != artifact.SHA256 || decoded.V3.TaskSetSHA256 != p.journal.TaskSetSHA256 {
		return WindowsProductCommitted{}, fmt.Errorf("%w: V3 receipt identity readback differs: %v", ErrWindowsProductCleanupPending, err)
	}
	if err := p.deps.RemoveReceipt(o.ReceiptV2Path); err != nil {
		return WindowsProductCommitted{}, fmt.Errorf("%w: remove exact V2 receipt: %v", ErrWindowsProductCleanupPending, err)
	}
	receiptDigest := fmt.Sprintf("%x", sha256.Sum256(readBack))
	event := WindowsProductCommitEvent{
		Schema: UpgradeReceiptSchemaV3, TransactionID: p.journal.TransactionID,
		Mode: o.Mode, Artifact: receipt.Artifact, TaskSetSHA256: p.journal.TaskSetSHA256,
		Retired: retired, ReceiptSHA256: receiptDigest, InstalledAt: now,
	}
	eventDigest, err := p.deps.PublishCommitted(event)
	committed := WindowsProductCommitted{
		Schema: UpgradeReceiptSchemaV3, Outcome: WindowsProductPairCommittedOutcome,
		Artifact: artifact, TaskSetSHA256: p.journal.TaskSetSHA256,
		ReceiptSHA256: receiptDigest, EventSHA256: eventDigest,
	}
	if err != nil {
		committed.Outcome = WindowsProductPairCommittedObservabilityFailed
		return committed, fmt.Errorf("%w: receipt_sha256=%s: %w", ErrWindowsProductPairCommittedObservabilityFailed, receiptDigest, err)
	}
	p.journal.Phase = WindowsProductRecoverySettledCommit
	if err := t.persistProductJournal(); err != nil {
		committed.Outcome = WindowsProductPairCommittedCleanupFailed
		return committed, fmt.Errorf("%w: persist settled commit: %v", ErrWindowsProductCleanupPending, err)
	}
	if err := o.Tasks.DiscardRecoveryStore(); err != nil {
		committed.Outcome = WindowsProductPairCommittedCleanupFailed
		return committed, fmt.Errorf("%w: discard retained task store: %v", ErrWindowsProductCleanupPending, err)
	}
	if err := p.deps.RemoveRecoveryJournal(filepath.Join(o.StateDir, WindowsProductRecoveryFileLeafV3)); err != nil {
		committed.Outcome = WindowsProductPairCommittedCleanupFailed
		return committed, fmt.Errorf("%w: remove settled V3 journal: %v", ErrWindowsProductCleanupPending, err)
	}
	return committed, nil
}

func cleanupWindowsProductTransactionFiles(o WindowsProductTxnOpts, p *windowsProductPreparedTxn, forwardReplay bool) error {
	if err := removeBoundWindowsProductFileIfPresent(o.StagedProduct, p.journal.Product.NewSHA256, p.deps); err != nil {
		return err
	}
	for _, entry := range []struct {
		path   string
		digest string
	}{
		{path: retainedPromotionPath(p.productPromotion), digest: p.journal.Product.PriorSHA256},
		{path: p.legacyRetained, digest: p.journal.LegacyWindowless.PriorSHA256},
	} {
		if entry.path == "" {
			continue
		}
		remove := removeBoundWindowsProductFile
		if forwardReplay {
			remove = removeBoundWindowsProductFileIfPresent
		}
		if err := remove(entry.path, entry.digest, p.deps); err != nil {
			return err
		}
	}
	for _, owner := range []struct {
		target string
		asides []windowsProductRecoveryAsideV3
	}{
		{o.ProductPath, p.journal.Product.BaselineAsides},
		{o.LegacyWindowlessPath, p.journal.LegacyWindowless.BaselineAsides},
	} {
		for _, aside := range owner.asides {
			remove := removeBoundWindowsProductFile
			if forwardReplay {
				remove = removeBoundWindowsProductFileIfPresent
			}
			if err := remove(filepath.Join(filepath.Dir(owner.target), aside.Basename), aside.SHA256, p.deps); err != nil {
				return err
			}
		}
		current, err := p.deps.ListGeneratedAsides(owner.target)
		if err != nil {
			return err
		}
		if len(current) != 0 {
			return fmt.Errorf("unrecognized or mapped executable asides remain for %s: %v", owner.target, current)
		}
	}
	if _, err := os.Stat(o.LegacyWindowlessPath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("legacy windowless target remains: %v", err)
	}
	helper := filepath.Join(filepath.Dir(o.ProductPath), "mcphub-pe-admit.exe")
	if _, err := os.Stat(helper); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("admission helper target remains or is unreadable: %v", err)
	}
	return nil
}

func removeBoundWindowsProductFileIfPresent(path, digest string, d WindowsProductTxnDeps) error {
	actual, err := d.HashFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if actual != digest {
		return fmt.Errorf("bound cleanup hash mismatch for %s", path)
	}
	return d.RemoveExact(path)
}

func retainedPromotionPath(p *WindowsProductPairPromotion) string {
	if p == nil {
		return ""
	}
	return p.RetainedPrior
}

func removeBoundWindowsProductFile(path, digest string, d WindowsProductTxnDeps) error {
	actual, err := d.HashFile(path)
	if err != nil {
		return err
	}
	if actual != digest {
		return fmt.Errorf("bound cleanup hash mismatch for %s", path)
	}
	if err := d.RemoveExact(path); err != nil {
		return err
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("bound cleanup path remains: %s: %v", path, err)
	}
	return nil
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
	AdmitPair             func(cli, windowless binaryadmission.WindowsArtifact) (binaryadmission.WindowsProductPair, error)
	Promote               func(src, dst, newSHA256 string) (WindowsProductPairPromotion, error)
	RestorePromotion      func(WindowsProductPairPromotion) (string, error)
	SweepOldTargets       func([]string, ...func(string, error)) error
	ReadBackWindowless    func(path string, expected binaryadmission.WindowsArtifact) error
	StartSupervisor       func(cliPath string) error
	WaitSupervisorReady   func(ctx context.Context, cliPath string, pair binaryadmission.WindowsProductPair) error
	RestartPrior          func(cliPath string) error
	SettleSuccessor       func(cliPath string) error
	WriteReceipt          func(path string, raw []byte) error
	ReadReceipt           func(path string) ([]byte, error)
	WriteRecoveryJournal  func(path string, raw []byte) error
	ReadRecoveryJournal   func(path string) ([]byte, error)
	RemoveRecoveryJournal func(path string) error
	ListGeneratedAsides   func(target string) ([]string, error)
	PublishCommitted      func(WindowsProductPairCommitEvent) (string, error)
	Fault                 func(WindowsProductPairStage) error
	Now                   func() time.Time
}

type productPairFileSnapshot struct {
	path    string
	body    []byte
	present bool
}

func windowsProductPreimage(snapshot productPairFileSnapshot) windowsProductPreimageV1 {
	if !snapshot.present {
		return windowsProductPreimageV1{}
	}
	return windowsProductPreimageV1{Present: true, SHA256: fmt.Sprintf("%x", sha256.Sum256(snapshot.body))}
}

func windowsProductTargetDisposition(prior productPairFileSnapshot, stagedSHA256 string) windowsProductTargetDispositionV1 {
	if !prior.present {
		return windowsProductTargetCreate
	}
	if windowsProductPreimage(prior).SHA256 == stagedSHA256 {
		return windowsProductTargetIdentical
	}
	return windowsProductTargetReplace
}

func windowsProductPairDispositionNeedsReplacement(disposition windowsProductPairDispositionV1) bool {
	return disposition.CLI == windowsProductTargetReplace || disposition.Windowless == windowsProductTargetReplace
}

func inspectWindowsProductPreflight(txn *WindowsProductTxn) (windowsProductPairPreflightV1, error) {
	if txn == nil {
		return windowsProductPairPreflightV1{}, errors.New("Windows product transaction is required")
	}
	o := txn.Opts
	d := withWindowsProductDefaults(o.Deps)
	staged, err := d.AdmitProduct(binaryadmission.WindowsArtifact{
		Path: o.StagedProduct, Role: binaryadmission.WindowsArtifactRoleProduct,
	})
	if err != nil {
		return windowsProductPairPreflightV1{}, fmt.Errorf("E_WINDOWS_PRODUCT_ADMISSION: staged product before supervisor settlement: %w", err)
	}
	productPrior, err := snapshotProductPairFile(o.ProductPath)
	if err != nil {
		return windowsProductPairPreflightV1{}, err
	}
	legacyPrior, err := snapshotProductPairFile(o.LegacyWindowlessPath)
	if err != nil {
		return windowsProductPairPreflightV1{}, err
	}
	return windowsProductPairPreflightV1{
		Disposition: windowsProductPairDispositionV1{
			CLI:        windowsProductTargetDisposition(productPrior, staged.SHA256),
			Windowless: windowsProductTargetIdentical,
		},
		PriorCLI:        windowsProductPreimage(productPrior),
		PriorWindowless: windowsProductPreimage(legacyPrior),
	}, nil
}

func validateWindowsProductQuiescenceReceipt(receipt windowsProductQuiescenceReceiptV1) error {
	if strings.TrimSpace(receipt.ReceiptID) == "" || receipt.Scope != windowsProductQuiescenceScopeV1 {
		return errors.New("invalid Windows product pair quiescence receipt identity")
	}
	if !validLowerHexSHA256(receipt.ObservationSHA256) {
		return errors.New("invalid Windows product pair quiescence observation digest")
	}
	if !cleanAbsolutePath(receipt.CanonicalCLIPath) || !cleanAbsolutePath(receipt.CanonicalWindowlessPath) || pathsEqual(receipt.CanonicalCLIPath, receipt.CanonicalWindowlessPath) {
		return errors.New("invalid Windows product pair quiescence target paths")
	}
	if !validWindowsProductPreimage(receipt.PriorCLI) || !validWindowsProductPreimage(receipt.PriorWindowless) {
		return errors.New("invalid Windows product pair quiescence preimage")
	}
	if receipt.GUIOwner != windowsProductGUIOwnerConfirmedAbsent || !receipt.SupervisorLockReleased {
		return errors.New("invalid Windows product pair quiescence ownership settlement")
	}
	if receipt.SettledAtUTC.IsZero() || receipt.SettledAtUTC.Location() != time.UTC {
		return errors.New("invalid Windows product pair quiescence settlement time")
	}

	seenPorts := make(map[string]struct{}, len(receipt.Ports))
	allInitiallyUnbound := true
	for _, port := range receipt.Ports {
		if strings.TrimSpace(port.TaskName) == "" || port.Port < 1 || port.Port > 65535 || !port.PostUnbound {
			return errors.New("invalid Windows product pair quiescence port settlement")
		}
		key := fmt.Sprintf("%s\x00%d", port.TaskName, port.Port)
		if _, exists := seenPorts[key]; exists {
			return errors.New("duplicate Windows product pair quiescence port settlement")
		}
		seenPorts[key] = struct{}{}
		switch port.PreState {
		case windowsProductPortUnbound:
			if port.PrePID != 0 {
				return errors.New("invalid unbound Windows product pair port preimage")
			}
		case windowsProductPortCanonicalOwned, windowsProductPortRetainedOwned:
			allInitiallyUnbound = false
			if port.PrePID <= 0 {
				return errors.New("invalid owned Windows product pair port preimage")
			}
		default:
			return errors.New("invalid Windows product pair port pre-state")
		}
	}

	switch receipt.Supervisor.State {
	case windowsProductSupervisorKnownAbsent:
		if receipt.Supervisor.Generation != nil || !allInitiallyUnbound {
			return errors.New("invalid known-absent Windows product pair supervisor settlement")
		}
	case windowsProductSupervisorGracefullySettled:
		generation := receipt.Supervisor.Generation
		if generation == nil || generation.PID <= 0 || strings.TrimSpace(generation.StartedAt) == "" || (generation.IPCRequestVersion != 0 && generation.IPCRequestVersion != 1) {
			return errors.New("invalid gracefully-settled Windows product pair supervisor generation")
		}
	default:
		return errors.New("invalid Windows product pair supervisor settlement state")
	}
	return nil
}

func validWindowsProductPreimage(preimage windowsProductPreimageV1) bool {
	if !preimage.Present {
		return preimage.SHA256 == ""
	}
	return validLowerHexSHA256(preimage.SHA256)
}

func validLowerHexSHA256(value string) bool {
	return value == strings.ToLower(value) && validRecoveryHex(value, sha256.Size)
}

func cleanAbsolutePath(path string) bool {
	return path != "" && filepath.IsAbs(path) && path == filepath.Clean(path)
}

func windowsSingleProductNonUpgradeReplacementAuthority(stagedProduct, productTarget string) (windowsProductReplacementAuthority, error) {
	productSHA, err := hashFile(stagedProduct)
	if err != nil {
		return windowsProductReplacementAuthority{}, err
	}
	prior, err := snapshotProductPairFile(productTarget)
	if err != nil {
		return windowsProductReplacementAuthority{}, err
	}
	disposition := windowsProductPairDispositionV1{
		CLI:        windowsProductTargetDisposition(prior, hex.EncodeToString(productSHA)),
		Windowless: windowsProductTargetIdentical,
	}
	if windowsProductPairDispositionNeedsReplacement(disposition) {
		return windowsProductReplacementAuthority{}, ErrWindowsProductPairReplacementRequiresUpgrade
	}
	return windowsProductReplacementAuthority{Disposition: disposition}, nil
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
				return WindowsProductPairPromotion{Target: dst, RetainedPrior: result.RetainedPrior, PriorPresent: !result.PriorCanonical, NewSHA256: newSHA256}, err
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
		d.ReadReceipt = api.ReadStateFileInodeAnchored
	}
	if d.WriteRecoveryJournal == nil {
		d.WriteRecoveryJournal = api.WriteStateFileBytesAtomic
	}
	if d.ReadRecoveryJournal == nil {
		d.ReadRecoveryJournal = api.ReadStateFileInodeAnchored
	}
	if d.RemoveRecoveryJournal == nil {
		d.RemoveRecoveryJournal = func(path string) error {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		}
	}
	if d.ListGeneratedAsides == nil {
		d.ListGeneratedAsides = listGeneratedRenameAsideBasenames
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.SweepOldTargets == nil {
		d.SweepOldTargets = api.SweepOldBinaryTargets
	}
	return d
}

// rollbackProductPairPromotion returns a rollback token only when the
// promotion result proves that it changed the exact target snapshot. The
// rename-aside producer reports PriorCanonical when its attempted rename left
// the prior binary at the target; such an error is not restorable here.
func rollbackProductPairPromotion(promotion WindowsProductPairPromotion, prior productPairFileSnapshot) *WindowsProductPairPromotion {
	if promotion.Target != prior.path || promotion.NewSHA256 == "" || promotion.PriorPresent != prior.present {
		return nil
	}
	if prior.present && promotion.RetainedPrior == "" {
		return nil
	}
	return &promotion
}

func newWindowsProductPairTransactionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("create recovery transaction id: %w", err)
	}
	return fmt.Sprintf("%x", raw[:]), nil
}

func listGeneratedRenameAsideBasenames(target string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		return nil, err
	}
	basenames := make([]string, 0)
	for _, entry := range entries {
		candidate := filepath.Join(filepath.Dir(target), entry.Name())
		if api.IsGeneratedRenameAsideForTarget(target, candidate) {
			basenames = append(basenames, entry.Name())
		}
	}
	sort.Strings(basenames)
	return basenames, nil
}

func persistWindowsProductPairRecoveryJournal(path string, journal windowsProductPairRecoveryJournal, d WindowsProductPairTxnDeps) error {
	raw, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := d.WriteRecoveryJournal(path, raw); err != nil {
		return err
	}
	readBack, err := d.ReadRecoveryJournal(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(readBack, raw) {
		return errors.New("recovery journal exact-byte readback differs")
	}
	decoded, err := decodeWindowsProductPairRecoveryJournal(readBack)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(decoded, journal) {
		return errors.New("recovery journal decoded value differs")
	}
	return nil
}

func decodeWindowsProductPairRecoveryJournal(raw []byte) (windowsProductPairRecoveryJournal, error) {
	var header struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return windowsProductPairRecoveryJournal{}, err
	}
	switch header.Schema {
	case WindowsProductPairRecoverySchemaV1:
		return decodeWindowsProductPairRecoveryJournalV1(raw)
	case WindowsProductPairRecoverySchemaV2:
		return decodeWindowsProductPairRecoveryJournalV2(raw)
	default:
		return windowsProductPairRecoveryJournal{}, fmt.Errorf("%w: %q", ErrWindowsProductPairRecoverySchemaUnsupported, header.Schema)
	}
}

func decodeWindowsProductPairRecoveryJournalV1(raw []byte) (windowsProductPairRecoveryJournal, error) {
	var header struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return windowsProductPairRecoveryJournal{}, err
	}
	if header.Schema != WindowsProductPairRecoverySchemaV1 {
		return windowsProductPairRecoveryJournal{}, fmt.Errorf("%w: strict V1 reader refuses %q", ErrWindowsProductPairRecoverySchemaUnsupported, header.Schema)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var v1 windowsProductPairRecoveryJournalV1
	if err := dec.Decode(&v1); err != nil {
		return windowsProductPairRecoveryJournal{}, err
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return windowsProductPairRecoveryJournal{}, errors.New("recovery journal has trailing JSON")
	}
	journal := windowsProductPairRecoveryJournal{
		Schema: v1.Schema, TransactionID: v1.TransactionID, Phase: v1.Phase, Mode: v1.Mode,
		CLI: v1.CLI, Windowless: v1.Windowless, Receipt: v1.Receipt, Tasks: v1.Tasks, Staged: v1.Staged, PriorFleetReaped: v1.PriorFleetReaped,
	}
	if err := validateWindowsProductPairRecoveryJournal(journal); err != nil {
		return windowsProductPairRecoveryJournal{}, err
	}
	return journal, nil
}

func decodeWindowsProductPairRecoveryJournalV2(raw []byte) (windowsProductPairRecoveryJournal, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var journal windowsProductPairRecoveryJournal
	if err := dec.Decode(&journal); err != nil {
		return windowsProductPairRecoveryJournal{}, err
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return windowsProductPairRecoveryJournal{}, errors.New("recovery journal has trailing JSON")
	}
	if err := validateWindowsProductPairRecoveryJournal(journal); err != nil {
		return windowsProductPairRecoveryJournal{}, err
	}
	return journal, nil
}

func decodeWindowsProductRecoveryJournalV3(raw []byte) (windowsProductRecoveryJournalV3, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var journal windowsProductRecoveryJournalV3
	if err := dec.Decode(&journal); err != nil {
		return windowsProductRecoveryJournalV3{}, err
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return windowsProductRecoveryJournalV3{}, errors.New("V3 recovery journal has trailing JSON")
	}
	if err := validateWindowsProductRecoveryJournalV3(journal); err != nil {
		return windowsProductRecoveryJournalV3{}, err
	}
	return journal, nil
}

func validateWindowsProductRecoveryJournalV3(j windowsProductRecoveryJournalV3) error {
	if j.Schema != WindowsProductRecoverySchemaV3 || !validRecoveryHex(j.TransactionID, 16) {
		return errors.New("invalid V3 recovery schema or transaction identity")
	}
	switch j.Phase {
	case WindowsProductRecoveryPrepared, WindowsProductRecoveryGUIOwnedStopPending, WindowsProductRecoverySuccessorStarting,
		WindowsProductRecoveryCommitDecided, WindowsProductRecoverySettledCommit,
		WindowsProductRecoverySettledRollback:
	default:
		return errors.New("invalid V3 recovery phase")
	}
	if j.Mode != WindowsProductPairModeSetup && j.Mode != WindowsProductPairModeCanonicalize && j.Mode != WindowsProductPairModeUpgrade {
		return errors.New("invalid V3 recovery mode")
	}
	if !cleanAbsolutePath(j.Product.Target) ||
		!strings.EqualFold(filepath.Base(j.Product.Target), "mcphub.exe") ||
		!cleanAbsolutePath(j.LegacyWindowless.Target) ||
		!strings.EqualFold(filepath.Base(j.LegacyWindowless.Target), "mcphub-windowless.exe") ||
		sameWindowsProductPath(j.Product.Target, j.LegacyWindowless.Target) ||
		!validRecoveryPreimage(j.Product.PriorPresent, j.Product.PriorSHA256) ||
		!validLowerHexSHA256(j.Product.NewSHA256) ||
		!validRecoveryPreimage(j.LegacyWindowless.PriorPresent, j.LegacyWindowless.PriorSHA256) {
		return errors.New("invalid V3 recovery product identity")
	}
	if err := validateWindowsProductRecoveryAsides(j.Product.Target, j.Product.BaselineAsides); err != nil {
		return fmt.Errorf("invalid V3 product aside baseline: %w", err)
	}
	if err := validateWindowsProductRecoveryAsides(j.LegacyWindowless.Target, j.LegacyWindowless.BaselineAsides); err != nil {
		return fmt.Errorf("invalid V3 legacy-windowless aside baseline: %w", err)
	}
	if !cleanAbsolutePath(j.Receipts.V3.Path) ||
		!strings.EqualFold(filepath.Base(j.Receipts.V3.Path), UpgradeReceiptSchemaV3+".json") ||
		!cleanAbsolutePath(j.Receipts.V2.Path) ||
		!strings.EqualFold(filepath.Base(j.Receipts.V2.Path), UpgradeReceiptSchemaV2+".json") ||
		!sameWindowsProductPath(filepath.Dir(j.Receipts.V3.Path), filepath.Dir(j.Receipts.V2.Path)) ||
		!validRecoveryReceiptSnapshot(j.Receipts.V3) ||
		!validRecoveryReceiptSnapshot(j.Receipts.V2) {
		return errors.New("invalid V3 recovery receipt snapshots")
	}
	if !cleanAbsolutePath(j.Staged.Path) ||
		sameWindowsProductPath(j.Staged.Path, j.Product.Target) ||
		j.Staged.SHA256 != j.Product.NewSHA256 {
		return errors.New("invalid V3 staged product identity")
	}
	if !cleanAbsolutePath(j.Tasks.StoreRoot) {
		return errors.New("invalid V3 retained task store")
	}
	previousTask := ""
	for _, backup := range j.Tasks.Backups {
		key := strings.ToLower(strings.TrimSpace(backup.TaskName))
		if key == "" || (previousTask != "" && key <= previousTask) ||
			filepath.Base(backup.Ref) != backup.Ref || strings.ContainsAny(backup.Ref, "/\\") ||
			!validLowerHexSHA256(backup.SHA256) {
			return errors.New("invalid V3 recovery task descriptor")
		}
		previousTask = key
	}
	switch j.Phase {
	case WindowsProductRecoveryPrepared, WindowsProductRecoveryGUIOwnedStopPending, WindowsProductRecoverySettledRollback:
		if j.TaskSetSHA256 != "" {
			return errors.New("V3 predecision/rollback journal has a task-set digest")
		}
	default:
		if !validLowerHexSHA256(j.TaskSetSHA256) {
			return errors.New("V3 forward journal lacks a valid task-set digest")
		}
	}
	if j.Mode == WindowsProductPairModeUpgrade {
		if len(j.FrozenPorts) == 0 && j.FrozenPorts == nil {
			return errors.New("upgrade V3 recovery requires a non-null frozen port set")
		}
		for i, row := range j.FrozenPorts {
			if strings.TrimSpace(row.TaskName) == "" || row.Port < 1 || row.Port > 65535 ||
				!validLowerHexSHA256(row.ArgvSHA256) {
				return errors.New("invalid V3 frozen port")
			}
			if i > 0 {
				prev := j.FrozenPorts[i-1]
				if prev.TaskName > row.TaskName ||
					(prev.TaskName == row.TaskName && prev.Port > row.Port) ||
					(prev.TaskName == row.TaskName && prev.Port == row.Port && prev.ArgvSHA256 >= row.ArgvSHA256) {
					return errors.New("unsorted or duplicate V3 frozen port")
				}
			}
		}
		pre := expandWindowsProductPreSettlement(j.PreSettlement)
		if pre == nil {
			return errors.New("upgrade V3 recovery requires pre-settlement observation")
		}
		frozen := expandWindowsProductFrozenPorts(j.FrozenPorts)
		guiOwned := pre.GUIOwner == windowsProductGUIOwnerConfirmedPresent
		if guiOwned && (pre.Supervisor.State != windowsProductSupervisorGracefullySettled ||
			(j.Phase != WindowsProductRecoveryGUIOwnedStopPending && j.Phase != WindowsProductRecoverySettledRollback && j.Quiescence == nil) ||
			j.Phase == WindowsProductRecoveryGUIOwnedStopPending && (j.Quiescence != nil || j.PriorFleetReaped)) {
			return errors.New("invalid GUI-owned stop preimage or pending form")
		}
		if j.Phase == WindowsProductRecoveryGUIOwnedStopPending && !guiOwned {
			return errors.New("GUI-owned stop phase lacks a live GUI preimage")
		}
		if err := validateWindowsProductPreSettlementV3(*pre, frozen, guiOwned); err != nil {
			return fmt.Errorf("invalid V3 pre-settlement observation: %w", err)
		}
		if j.Quiescence == nil {
			if (j.Phase != WindowsProductRecoveryPrepared && j.Phase != WindowsProductRecoveryGUIOwnedStopPending &&
				!(guiOwned && j.Phase == WindowsProductRecoverySettledRollback)) || j.PriorFleetReaped || j.QuiescenceReceiptSHA256 != "" {
				return errors.New("invalid initial prepared V3 recovery form")
			}
			return nil
		}
		legacy := windowsProductPairRecoveryJournal{
			Schema: WindowsProductPairRecoverySchemaV2,
			Mode:   j.Mode, PriorFleetReaped: j.PriorFleetReaped,
			CLI:        windowsProductPairRecoveryArtifact{Target: j.Product.Target, PriorPresent: j.Product.PriorPresent, PriorSHA256: j.Product.PriorSHA256, NewSHA256: j.Product.NewSHA256},
			Windowless: windowsProductPairRecoveryArtifact{Target: j.LegacyWindowless.Target, PriorPresent: j.LegacyWindowless.PriorPresent, PriorSHA256: j.LegacyWindowless.PriorSHA256, NewSHA256: j.LegacyWindowless.PriorSHA256},
			Quiescence: j.Quiescence, QuiescenceReceiptSHA256: j.QuiescenceReceiptSHA256,
		}
		if err := validateWindowsProductRecoveryQuiescenceV3(legacy); err != nil {
			return err
		}
		q := j.Quiescence
		initialLive := pre.Supervisor.State == windowsProductSupervisorGracefullySettled
		if j.PriorFleetReaped != initialLive ||
			!reflect.DeepEqual(q.Supervisor, pre.Supervisor) ||
			(q.GUIOwner != pre.GUIOwner && !(guiOwned && q.GUIOwner == windowsProductGUIOwnerConfirmedAbsent)) ||
			!sameWindowsProductPortPreimages(q.Ports, pre.Ports) {
			return errors.New("settled V3 quiescence is not bound to pre-settlement observation")
		}
	} else if len(j.FrozenPorts) != 0 || j.PreSettlement != nil ||
		j.PriorFleetReaped || j.Quiescence != nil || j.QuiescenceReceiptSHA256 != "" {
		return errors.New("non-upgrade V3 recovery contains quiescence state")
	}
	return nil
}

func validateWindowsProductPreSettlementV3(observation windowsProductQuiescenceObservationV1, frozen []windowsProductIntentPortV1, guiOwned bool) error {
	if (observation.GUIOwner != windowsProductGUIOwnerConfirmedAbsent && !(guiOwned && observation.GUIOwner == windowsProductGUIOwnerConfirmedPresent)) || len(observation.Ports) != len(frozen) {
		return errors.New("incomplete Windows product lifecycle observation")
	}
	want := make(map[string]struct{}, len(frozen))
	for _, row := range frozen {
		want[fmt.Sprintf("%s\x00%d", row.TaskName, row.Port)] = struct{}{}
	}
	allUnbound := true
	for _, row := range observation.Ports {
		key := fmt.Sprintf("%s\x00%d", row.TaskName, row.Port)
		if _, exists := want[key]; !exists {
			return errors.New("observed port population differs from frozen intent")
		}
		delete(want, key)
		switch row.PreState {
		case windowsProductPortUnbound:
			if row.PrePID != 0 {
				return errors.New("unbound observed port has a PID")
			}
		case windowsProductPortCanonicalOwned, windowsProductPortRetainedOwned:
			allUnbound = false
			if row.PrePID <= 0 {
				return errors.New("owned observed port lacks a PID")
			}
		default:
			return errors.New("pre-settlement observation has unsupported port owner state")
		}
	}
	if len(want) != 0 {
		return errors.New("observed port population is incomplete")
	}
	switch observation.Supervisor.State {
	case windowsProductSupervisorKnownAbsent:
		if observation.Supervisor.Generation != nil || !observation.SupervisorLockReleased || !allUnbound {
			return errors.New("known-absent observation is contradictory")
		}
		for _, row := range observation.Ports {
			if !row.PostUnbound {
				return errors.New("known-absent port lacks positive unbound proof")
			}
		}
	case windowsProductSupervisorGracefullySettled:
		generation := observation.Supervisor.Generation
		if observation.SupervisorLockReleased || generation == nil || generation.PID <= 0 ||
			strings.TrimSpace(generation.StartedAt) == "" ||
			(generation.IPCRequestVersion != 0 && generation.IPCRequestVersion != 1) {
			return errors.New("live supervisor generation observation is invalid")
		}
		for _, row := range observation.Ports {
			if row.PostUnbound {
				return errors.New("live observation claims settlement before lifecycle execution")
			}
		}
	default:
		return errors.New("supervisor lifecycle state is unknown")
	}
	return nil
}

func sameWindowsProductPortPreimages(settled, initial []windowsProductPortSettlementV1) bool {
	if len(settled) != len(initial) {
		return false
	}
	byKey := make(map[string]windowsProductPortSettlementV1, len(initial))
	for _, row := range initial {
		byKey[fmt.Sprintf("%s\x00%d", row.TaskName, row.Port)] = row
	}
	for _, row := range settled {
		key := fmt.Sprintf("%s\x00%d", row.TaskName, row.Port)
		prior, ok := byKey[key]
		if !ok || row.PreState != prior.PreState || row.PrePID != prior.PrePID || !row.PostUnbound {
			return false
		}
		delete(byKey, key)
	}
	return len(byKey) == 0
}

func validRecoveryPreimage(present bool, digest string) bool {
	if !present {
		return digest == ""
	}
	return validLowerHexSHA256(digest)
}

func validRecoveryReceiptSnapshot(receipt windowsProductPairRecoveryReceipt) bool {
	return receipt.PriorPresent == (receipt.PriorBytes != nil)
}

func validateWindowsProductRecoveryAsides(target string, asides []windowsProductRecoveryAsideV3) error {
	previous := ""
	for _, aside := range asides {
		if filepath.Base(aside.Basename) != aside.Basename ||
			strings.ContainsAny(aside.Basename, "/\\") ||
			(previous != "" && aside.Basename <= previous) ||
			!api.IsGeneratedRenameAsideForTarget(target, filepath.Join(filepath.Dir(target), aside.Basename)) ||
			!validLowerHexSHA256(aside.SHA256) {
			return errors.New("invalid or unsorted aside identity")
		}
		previous = aside.Basename
	}
	return nil
}

func validateWindowsProductRecoveryQuiescenceV3(j windowsProductPairRecoveryJournal) error {
	if j.Quiescence == nil || !validLowerHexSHA256(j.QuiescenceReceiptSHA256) {
		return errors.New("invalid V3 recovery quiescence envelope")
	}
	q := *j.Quiescence
	settledAt, err := time.Parse(time.RFC3339Nano, q.SettledAtUTC)
	if err != nil {
		return errors.New("invalid V3 recovery quiescence time")
	}
	receipt := windowsProductQuiescenceReceiptV1{
		ReceiptID: q.ReceiptID, Scope: q.Scope, ObservationSHA256: q.ObservationSHA256,
		CanonicalCLIPath: j.CLI.Target, CanonicalWindowlessPath: j.Windowless.Target,
		PriorCLI: q.PriorCLI, PriorWindowless: q.PriorWindowless,
		Supervisor: q.Supervisor, Ports: q.Ports, GUIOwner: q.GUIOwner,
		SupervisorLockReleased: q.SupervisorLockReleased, SettledAtUTC: settledAt,
	}
	if err := validateWindowsProductQuiescenceReceipt(receipt); err != nil {
		return fmt.Errorf("invalid V3 recovery quiescence receipt: %w", err)
	}
	if q.CanonicalCLIPathSHA256 != hashWindowsProductPath(j.CLI.Target) ||
		q.CanonicalWindowlessSHA256 != hashWindowsProductPath(j.Windowless.Target) ||
		q.PriorCLI != (windowsProductPreimageV1{Present: j.CLI.PriorPresent, SHA256: j.CLI.PriorSHA256}) ||
		q.PriorWindowless != (windowsProductPreimageV1{Present: j.Windowless.PriorPresent, SHA256: j.Windowless.PriorSHA256}) {
		return errors.New("V3 recovery quiescence target/preimage mismatch")
	}
	raw, err := json.Marshal(q)
	if err != nil {
		return err
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != j.QuiescenceReceiptSHA256 {
		return errors.New("V3 recovery quiescence receipt digest mismatch")
	}
	return nil
}

func sameWindowsProductPath(left, right string) bool {
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}

func persistWindowsProductRecoveryJournalV3(path string, journal windowsProductRecoveryJournalV3, write func(string, []byte) error, read func(string) ([]byte, error)) error {
	if err := validateWindowsProductRecoveryJournalV3(journal); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := write(path, raw); err != nil {
		return err
	}
	readBack, err := read(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(readBack, raw) {
		return errors.New("V3 recovery journal exact-byte readback differs")
	}
	decoded, err := decodeWindowsProductRecoveryJournalV3(readBack)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(decoded, journal) {
		return errors.New("V3 recovery journal decoded value differs")
	}
	return nil
}

func validateWindowsProductPairRecoveryJournal(j windowsProductPairRecoveryJournal) error {
	if (j.Schema != WindowsProductPairRecoverySchemaV1 && j.Schema != WindowsProductPairRecoverySchemaV2) || !validRecoveryHex(j.TransactionID, 16) {
		return errors.New("invalid recovery schema or transaction identity")
	}
	if j.Schema == WindowsProductPairRecoverySchemaV1 {
		if j.Quiescence != nil || j.QuiescenceReceiptSHA256 != "" {
			return errors.New("V1 recovery journal contains V2 quiescence fields")
		}
	} else if err := validateWindowsProductRecoveryQuiescence(j); err != nil {
		return err
	}
	if j.Phase != WindowsProductPairRecoveryPrepared && j.Phase != WindowsProductPairRecoverySuccessorStarting && j.Phase != WindowsProductPairRecoverySettledCommit && j.Phase != WindowsProductPairRecoverySettledRollback {
		return errors.New("invalid recovery phase")
	}
	if j.Mode != WindowsProductPairModeSetup && j.Mode != WindowsProductPairModeCanonicalize && j.Mode != WindowsProductPairModeUpgrade {
		return errors.New("invalid recovery mode")
	}
	if !filepath.IsAbs(j.CLI.Target) || !filepath.IsAbs(j.Windowless.Target) || strings.EqualFold(filepath.Clean(j.CLI.Target), filepath.Clean(j.Windowless.Target)) || !filepath.IsAbs(j.Receipt.Path) || !filepath.IsAbs(j.Tasks.StoreRoot) || !filepath.IsAbs(j.Staged.CLIPath) || !filepath.IsAbs(j.Staged.WindowlessPath) {
		return errors.New("incomplete recovery paths")
	}
	for _, a := range []windowsProductPairRecoveryArtifact{j.CLI, j.Windowless} {
		if !validRecoveryHex(a.NewSHA256, sha256.Size) || (a.PriorPresent && !validRecoveryHex(a.PriorSHA256, sha256.Size)) || (!a.PriorPresent && a.PriorSHA256 != "") || !sort.StringsAreSorted(a.BaselineAsideBasenames) {
			return errors.New("invalid recovery artifact identity")
		}
		for i, name := range a.BaselineAsideBasenames {
			if filepath.Base(name) != name || !api.IsGeneratedRenameAsideForTarget(a.Target, filepath.Join(filepath.Dir(a.Target), name)) || (i > 0 && name == a.BaselineAsideBasenames[i-1]) {
				return errors.New("invalid recovery aside baseline")
			}
		}
	}
	if j.Staged.CLISHA256 != j.CLI.NewSHA256 || j.Staged.WindowlessSHA256 != j.Windowless.NewSHA256 || j.Receipt.PriorPresent != (j.Receipt.PriorBytes != nil) {
		return errors.New("inconsistent recovery preimage")
	}
	for _, backup := range j.Tasks.Backups {
		if strings.TrimSpace(backup.TaskName) == "" || filepath.Base(backup.Ref) != backup.Ref || strings.ContainsAny(backup.Ref, `/\\`) || !validRecoveryHex(backup.SHA256, sha256.Size) {
			return errors.New("invalid recovery task descriptor")
		}
	}
	return nil
}

func projectWindowsProductRecoveryQuiescence(receipt windowsProductQuiescenceReceiptV1) (windowsProductRecoveryQuiescenceV2, string, error) {
	if err := validateWindowsProductQuiescenceReceipt(receipt); err != nil {
		return windowsProductRecoveryQuiescenceV2{}, "", err
	}
	projection := windowsProductRecoveryQuiescenceV2{
		ReceiptID:                 receipt.ReceiptID,
		Scope:                     receipt.Scope,
		ObservationSHA256:         receipt.ObservationSHA256,
		CanonicalCLIPathSHA256:    hashWindowsProductPath(receipt.CanonicalCLIPath),
		CanonicalWindowlessSHA256: hashWindowsProductPath(receipt.CanonicalWindowlessPath),
		PriorCLI:                  receipt.PriorCLI,
		PriorWindowless:           receipt.PriorWindowless,
		Supervisor:                receipt.Supervisor,
		Ports:                     append([]windowsProductPortSettlementV1(nil), receipt.Ports...),
		GUIOwner:                  receipt.GUIOwner,
		SupervisorLockReleased:    receipt.SupervisorLockReleased,
		SettledAtUTC:              receipt.SettledAtUTC.Format(time.RFC3339Nano),
	}
	raw, err := json.Marshal(projection)
	if err != nil {
		return windowsProductRecoveryQuiescenceV2{}, "", err
	}
	return projection, fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

func validateWindowsProductRecoveryQuiescence(j windowsProductPairRecoveryJournal) error {
	if j.Quiescence == nil || !j.PriorFleetReaped || j.Mode != WindowsProductPairModeUpgrade || !validLowerHexSHA256(j.QuiescenceReceiptSHA256) {
		return errors.New("invalid V2 recovery quiescence envelope")
	}
	q := *j.Quiescence
	settledAt, err := time.Parse(time.RFC3339Nano, q.SettledAtUTC)
	if err != nil {
		return errors.New("invalid V2 recovery quiescence time")
	}
	receipt := windowsProductQuiescenceReceiptV1{
		ReceiptID: q.ReceiptID, Scope: q.Scope, ObservationSHA256: q.ObservationSHA256,
		CanonicalCLIPath: j.CLI.Target, CanonicalWindowlessPath: j.Windowless.Target,
		PriorCLI: q.PriorCLI, PriorWindowless: q.PriorWindowless,
		Supervisor: q.Supervisor, Ports: q.Ports, GUIOwner: q.GUIOwner,
		SupervisorLockReleased: q.SupervisorLockReleased, SettledAtUTC: settledAt,
	}
	if err := validateWindowsProductQuiescenceReceipt(receipt); err != nil {
		return fmt.Errorf("invalid V2 recovery quiescence receipt: %w", err)
	}
	if q.CanonicalCLIPathSHA256 != hashWindowsProductPath(j.CLI.Target) || q.CanonicalWindowlessSHA256 != hashWindowsProductPath(j.Windowless.Target) {
		return errors.New("V2 recovery quiescence target digest mismatch")
	}
	if q.PriorCLI != (windowsProductPreimageV1{Present: j.CLI.PriorPresent, SHA256: j.CLI.PriorSHA256}) || q.PriorWindowless != (windowsProductPreimageV1{Present: j.Windowless.PriorPresent, SHA256: j.Windowless.PriorSHA256}) {
		return errors.New("V2 recovery quiescence preimage mismatch")
	}
	if (!j.CLI.PriorPresent || j.CLI.PriorSHA256 == j.CLI.NewSHA256) && (!j.Windowless.PriorPresent || j.Windowless.PriorSHA256 == j.Windowless.NewSHA256) {
		return errors.New("V2 recovery quiescence journal has no replacement disposition")
	}
	raw, err := json.Marshal(q)
	if err != nil {
		return err
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != j.QuiescenceReceiptSHA256 {
		return errors.New("V2 recovery quiescence receipt digest mismatch")
	}
	return nil
}

func hashWindowsProductPath(path string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(path)))
}

func validRecoveryHex(value string, decodedBytes int) bool {
	if len(value) != decodedBytes*2 {
		return false
	}
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == decodedBytes
}

func sameProductPairIdentity(a, b binaryadmission.WindowsProductPair) bool {
	return a.CLI.Role == b.CLI.Role && a.CLI.Version == b.CLI.Version && a.CLI.Commit == b.CLI.Commit && a.CLI.BuildDate == b.CLI.BuildDate && a.CLI.SHA256 == b.CLI.SHA256 &&
		a.Windowless.Role == b.Windowless.Role && a.Windowless.Version == b.Windowless.Version && a.Windowless.Commit == b.Windowless.Commit && a.Windowless.BuildDate == b.Windowless.BuildDate && a.Windowless.SHA256 == b.Windowless.SHA256
}

func windowsProductPairReceiptModeMatches(receiptMode, expectedMode WindowsProductPairMode) bool {
	if receiptMode != WindowsProductPairModeSetup && receiptMode != WindowsProductPairModeCanonicalize && receiptMode != WindowsProductPairModeUpgrade {
		return false
	}
	return receiptMode == expectedMode
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

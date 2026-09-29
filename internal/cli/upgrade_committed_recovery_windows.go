//go:build windows

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/buildinfo"
	"mcp-local-hub/internal/scheduler"
)

type upgradeCommittedRecoveryDeps struct {
	AdmitProduct func(binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error)
	BuildInfo    func() (string, string, string)
	Recover      func(windowsProductRecoveryRequest) (*WindowsProductPairRecoverySettled, error)
	AcquireFence func(context.Context, string) (upgradeCommittedFence, error)
	LstatJournal func(string) (os.FileInfo, error)
}

type upgradeCommittedFence interface{ Release() error }

var ErrUpgradeCommittedRecoveryNotAdmitted = errors.New("UPGRADE_COMMITTED_RECOVERY_NOT_ADMITTED")

// This is the sole user-visible projection for the recovery command. The
// underlying error stays available to typed in-process diagnostics, but its
// path-, SID-, or argv-bearing text must not cross the CLI stderr boundary.
type upgradeCommittedCommandError struct {
	id, stage string
	cause     error
}

func (e *upgradeCommittedCommandError) Error() string {
	message := e.id + ": " + e.stage
	if e.id == "E_WINDOWS_PRODUCT_PAIR_COMMITTED_OBSERVABILITY_FAILED" && e.stage == "complete committed forward recovery" {
		var health *upgradeGUIForwardHealthError
		if errors.As(e.cause, &health) && health != nil && isUpgradeGUIForwardHealthPredicate(health.predicate) {
			message += "; last_failed_predicate=" + health.predicate
		}
	}
	return message
}
func (e *upgradeCommittedCommandError) Unwrap() error     { return e.cause }
func (e *upgradeCommittedCommandError) FailureID() string { return e.id }

func committedRecoveryCommandError(id, stage string, cause error) error {
	return &upgradeCommittedCommandError{id: id, stage: stage, cause: cause}
}

func committedRecoveryOwnerFailureID(err error) string {
	switch {
	case errors.Is(err, ErrWindowsProductPairCommittedObservabilityFailed):
		return "E_WINDOWS_PRODUCT_PAIR_COMMITTED_OBSERVABILITY_FAILED"
	case errors.Is(err, ErrWindowsProductRecoveryPending):
		return "E_WINDOWS_PRODUCT_RECOVERY_PENDING"
	case errors.Is(err, ErrWindowsProductRecoveryIdentity):
		return "E_WINDOWS_PRODUCT_RECOVERY_IDENTITY"
	case errors.Is(err, ErrWindowsProductCleanupPending):
		return "E_WINDOWS_PRODUCT_CLEANUP_PENDING"
	case errors.Is(err, ErrWindowsProductPairCommittedCleanupFailed):
		return "E_WINDOWS_PRODUCT_PAIR_COMMITTED_CLEANUP_FAILED"
	default:
		return "UPGRADE_COMMITTED_RECOVERY_OWNER_FAILED"
	}
}

func init() { committedRecoveryFn = runCommittedRecoveryWindows }

func runCommittedRecoveryWindows(cmd *cobra.Command) error {
	if runtime.GOARCH != "amd64" {
		return errors.New("UPGRADE_COMMITTED_RECOVERY_NOT_ADMITTED: Windows amd64 Product is required")
	}
	executor, canonical, err := resolveUpgradeSelfPaths()
	if err != nil {
		return committedRecoveryCommandError("UPGRADE_COMMITTED_RECOVERY_NOT_ADMITTED", "resolve Product paths", err)
	}
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return committedRecoveryCommandError("UPGRADE_COMMITTED_RECOVERY_NOT_ADMITTED", "resolve Product state", err)
	}
	return runCommittedRecoveryWindowsWithDeps(cmd, stateDir, executor, canonical, upgradeCommittedRecoveryDeps{
		AdmitProduct: binaryadmission.AdmitWindowsRole,
		BuildInfo:    buildinfo.Get,
		Recover:      reconcileWiredWindowsProductRecovery,
		AcquireFence: func(ctx context.Context, dir string) (upgradeCommittedFence, error) {
			return api.AcquireUpgradeFence(ctx, dir)
		},
	})
}

func readCommittedRecoveryRegular(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("expected ordinary regular file")
	}
	return os.ReadFile(path)
}

type upgradeRecoveryExecutorSidecar struct {
	Schema    string                              `json:"schema"`
	Role      binaryadmission.WindowsArtifactRole `json:"role"`
	Filename  string                              `json:"filename"`
	Subsystem uint16                              `json:"subsystem"`
	Machine   uint16                              `json:"machine"`
	Version   string                              `json:"version"`
	Commit    string                              `json:"commit"`
	BuildDate string                              `json:"build_date"`
	SHA256    string                              `json:"sha256"`
}

func admitCommittedRecoveryExecutor(path string, admit func(binaryadmission.WindowsArtifact) (binaryadmission.WindowsArtifact, error), buildInfo func() (string, string, string)) (binaryadmission.WindowsArtifact, string, error) {
	if !strings.EqualFold(filepath.Base(path), "mcphub.exe") {
		return binaryadmission.WindowsArtifact{}, "", errors.New("executor is not the Product image")
	}
	admitted, err := admit(binaryadmission.WindowsArtifact{Path: path, Role: binaryadmission.WindowsArtifactRoleProduct})
	if err != nil {
		return binaryadmission.WindowsArtifact{}, "", fmt.Errorf("executor admission: %w", err)
	}
	if admitted.Role != binaryadmission.WindowsArtifactRoleProduct || admitted.Subsystem != binaryadmission.WindowsGUISubsystem || admitted.Machine != 0x8664 || admitted.SHA256 == "" {
		return binaryadmission.WindowsArtifact{}, "", errors.New("executor role, GUI subsystem, machine or digest differs")
	}
	version, commit, date := buildInfo()
	if version != admitted.Version || commit != admitted.Commit || date != admitted.BuildDate || version == "dev" || commit == "unknown" || date == "unknown" {
		return binaryadmission.WindowsArtifact{}, "", errors.New("executor embedded build identity differs")
	}
	raw, err := readCommittedRecoveryRegular(filepath.Join(filepath.Dir(path), "windows-product-identity.json"))
	if err != nil {
		return binaryadmission.WindowsArtifact{}, "", fmt.Errorf("executor identity sidecar: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var sidecar upgradeRecoveryExecutorSidecar
	if err := dec.Decode(&sidecar); err != nil {
		return binaryadmission.WindowsArtifact{}, "", fmt.Errorf("executor identity sidecar: %w", err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err != nil {
			return binaryadmission.WindowsArtifact{}, "", fmt.Errorf("executor identity sidecar has trailing JSON: %w", err)
		}
		return binaryadmission.WindowsArtifact{}, "", errors.New("executor identity sidecar has trailing JSON")
	}
	if sidecar.Schema != "windows-product-identity-v1" || sidecar.Role != admitted.Role || sidecar.Filename != "mcphub.exe" ||
		sidecar.Subsystem != admitted.Subsystem || sidecar.Machine != admitted.Machine || sidecar.Version != admitted.Version ||
		sidecar.Commit != admitted.Commit || sidecar.BuildDate != admitted.BuildDate || sidecar.SHA256 != admitted.SHA256 {
		return binaryadmission.WindowsArtifact{}, "", errors.New("executor identity sidecar differs from admitted Product")
	}
	sum := sha256.Sum256(raw)
	return admitted, hex.EncodeToString(sum[:]), nil
}

func runCommittedRecoveryWindowsWithDeps(cmd *cobra.Command, stateDir, executor, canonical string, deps upgradeCommittedRecoveryDeps) (retErr error) {
	failureID, failureStage := "UPGRADE_COMMITTED_RECOVERY_NOT_ADMITTED", "initialize recovery"
	defer func() {
		if retErr != nil {
			retErr = committedRecoveryCommandError(failureID, failureStage, retErr)
		}
	}()
	if cmd == nil || deps.AdmitProduct == nil || deps.BuildInfo == nil || deps.Recover == nil {
		return errors.New("UPGRADE_COMMITTED_RECOVERY_NOT_ADMITTED: recovery dependencies incomplete")
	}
	if deps.AcquireFence == nil {
		deps.AcquireFence = func(ctx context.Context, dir string) (upgradeCommittedFence, error) {
			return api.AcquireUpgradeFence(ctx, dir)
		}
	}
	if deps.LstatJournal == nil {
		deps.LstatJournal = os.Lstat
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	acquireCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	failureID, failureStage = "UPGRADE_COMMITTED_RECOVERY_FENCE_UNAVAILABLE", "acquire upgrade fence"
	fence, err := deps.AcquireFence(acquireCtx, stateDir)
	if err != nil {
		return fmt.Errorf("UPGRADE_COMMITTED_RECOVERY_FENCE_UNAVAILABLE: %w", err)
	}
	if fence == nil {
		return errors.New("UPGRADE_COMMITTED_RECOVERY_FENCE_UNAVAILABLE: fence owner returned no lease")
	}
	completion := ""
	defer func() {
		if releaseErr := fence.Release(); releaseErr != nil {
			failureID, failureStage = "UPGRADE_FENCE_RELEASE_UNCONFIRMED", "release upgrade fence"
			retErr = errors.Join(retErr, fmt.Errorf("UPGRADE_FENCE_RELEASE_UNCONFIRMED: %w", releaseErr))
		}
		if retErr == nil && completion != "" {
			failureID, failureStage = "UPGRADE_COMMITTED_RECOVERY_OUTPUT_FAILED", "write completion marker"
			_, retErr = fmt.Fprintln(cmd.OutOrStdout(), completion)
		}
	}()

	failureID, failureStage = "E_WINDOWS_PRODUCT_RECOVERY_IDENTITY", "admit recovery executor"
	execIdentity, sidecarSHA, err := admitCommittedRecoveryExecutor(executor, deps.AdmitProduct, deps.BuildInfo)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrWindowsProductRecoveryIdentity, err)
	}
	failureID, failureStage = "E_WINDOWS_PRODUCT_RECOVERY_IDENTITY", "inspect committed journal"
	v3Path := filepath.Join(stateDir, WindowsProductRecoveryFileLeafV3)
	raw, err := readCommittedRecoveryRegular(v3Path)
	if errors.Is(err, os.ErrNotExist) {
		failureID = "UPGRADE_COMMITTED_RECOVERY_NOT_ADMITTED"
		return fmt.Errorf("%w: no V3 recovery journal: %w", ErrUpgradeCommittedRecoveryNotAdmitted, err)
	}
	if err != nil {
		return fmt.Errorf("%w: V3 journal unreadable: %w", ErrWindowsProductRecoveryIdentity, err)
	}
	if _, err := os.Lstat(filepath.Join(stateDir, WindowsProductPairRecoveryFileLeaf)); err == nil {
		failureID = "UPGRADE_COMMITTED_RECOVERY_NOT_ADMITTED"
		return errors.New("UPGRADE_COMMITTED_RECOVERY_NOT_ADMITTED: legacy recovery journal coexists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: legacy journal inspection: %w", ErrWindowsProductRecoveryIdentity, err)
	}
	journal, err := decodeWindowsProductRecoveryJournalV3(raw)
	if err != nil {
		return fmt.Errorf("%w: invalid V3 journal: %w", ErrWindowsProductRecoveryIdentity, err)
	}
	if journal.Mode != WindowsProductPairModeUpgrade || journal.Phase != WindowsProductRecoveryCommitDecided ||
		journal.PreSettlement == nil || journal.PreSettlement.GUIOwner != windowsProductGUIOwnerConfirmedPresent {
		failureID = "UPGRADE_COMMITTED_RECOVERY_NOT_ADMITTED"
		return errors.New("UPGRADE_COMMITTED_RECOVERY_NOT_ADMITTED: not a committed GUI-owned upgrade")
	}
	failureStage = "verify committed Product targets and receipt"
	legacy := scheduler.WindowsLegacyEntrypointPath(canonical)
	if !sameWindowsRecoveryPath(journal.Product.Target, canonical) || !sameWindowsRecoveryPath(journal.LegacyWindowless.Target, legacy) ||
		!sameWindowsRecoveryPath(journal.Receipts.V3.Path, filepath.Join(stateDir, UpgradeReceiptSchemaV3+".json")) ||
		!sameWindowsRecoveryPath(journal.Receipts.V2.Path, filepath.Join(stateDir, UpgradeReceiptSchemaV2+".json")) {
		return fmt.Errorf("%w: journal target paths differ", ErrWindowsProductRecoveryIdentity)
	}
	if _, err := readCommittedRecoveryRegular(journal.Receipts.V3.Path); err != nil {
		return fmt.Errorf("%w: V3 receipt unreadable: %w", ErrWindowsProductRecoveryIdentity, err)
	}
	evidence, err := inspectWindowsProductReceiptEvidence(stateDir, canonical, canonical, WindowsProductPairModeUpgrade, deps.AdmitProduct)
	if err != nil {
		return fmt.Errorf("%w: receipt admission: %w", ErrWindowsProductRecoveryIdentity, err)
	}
	if !evidence.present || !evidence.match || evidence.receipt == nil ||
		evidence.receipt.TransactionID != journal.TransactionID || evidence.receipt.Mode != journal.Mode ||
		evidence.receipt.Artifact.SHA256 != journal.Product.NewSHA256 || evidence.receipt.TaskSetSHA256 != journal.TaskSetSHA256 ||
		evidence.receipt.Artifact.Version != execIdentity.Version || evidence.receipt.Artifact.Machine != execIdentity.Machine ||
		evidence.receipt.Artifact.Subsystem != execIdentity.Subsystem {
		return fmt.Errorf("%w: canonical Product, receipt, journal or executor compatibility differs", ErrWindowsProductRecoveryIdentity)
	}
	failureID, failureStage = "UPGRADE_COMMITTED_RECOVERY_OWNER_FAILED", "complete committed forward recovery"
	req := windowsProductRecoveryRuntimeRequest(ctx, stateDir, canonical, legacy)
	settled, err := deps.Recover(req)
	if err != nil {
		failureID = committedRecoveryOwnerFailureID(err)
		return err
	}
	if settled == nil || settled.Outcome != WindowsProductPairRecoverySettledCommit || settled.TransactionID != journal.TransactionID {
		failureID = "E_WINDOWS_PRODUCT_RECOVERY_PENDING"
		return fmt.Errorf("%w: recovery did not settle the admitted transaction", ErrWindowsProductRecoveryPending)
	}
	if _, err := deps.LstatJournal(v3Path); !errors.Is(err, os.ErrNotExist) {
		failureID, failureStage = "E_WINDOWS_PRODUCT_RECOVERY_PENDING", "verify settled journal removal"
		if err != nil {
			return fmt.Errorf("%w: V3 journal absence unverifiable: %w", ErrWindowsProductRecoveryPending, err)
		}
		return fmt.Errorf("%w: V3 journal still present", ErrWindowsProductRecoveryPending)
	}
	completion = fmt.Sprintf("UPGRADE_COMMITTED_RECOVERY_SETTLED: transaction=%s executor_version=%s executor_commit=%s executor_sha256=%s executor_sidecar_sha256=%s", journal.TransactionID, execIdentity.Version, execIdentity.Commit, execIdentity.SHA256, sidecarSHA)
	return nil
}

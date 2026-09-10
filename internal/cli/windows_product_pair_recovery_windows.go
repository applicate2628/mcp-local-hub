//go:build windows

package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
)

type WindowsProductPairRecoverySettled struct {
	TransactionID     string
	Outcome           WindowsProductPairRecoveryPhase
	PairOrPriorHashes [2]string
}

type windowsProductPairRecoveryRequest struct {
	Context            context.Context
	StateDir           string
	CLIPath            string
	WindowlessPath     string
	SettleSuccessor    func(string) error
	ObservePriorReady  func(context.Context, string, string) (bool, error)
	RestartPrior       func(string) error
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
		return nil, fmt.Errorf("%w: reopen retained task transaction: %v", ErrWindowsProductPairRecoveryIdentity, err)
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
				return nil, fmt.Errorf("%w: %v", ErrWindowsProductPairCommittedCleanupFailed, err)
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
			if err := req.RestartPrior(req.CLIPath); err != nil {
				restoreErrs = append(restoreErrs, fmt.Errorf("restart prior supervisor: %w", err))
			} else if err := req.WaitPriorReady(req.Context, req.CLIPath, journal.CLI.PriorSHA256); err != nil {
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
		return fmt.Errorf("%w: discard retained task store: %v", ErrWindowsProductPairRecoveryCleanup, err)
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

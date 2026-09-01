package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

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
	WindowsProductPairStageCommitted          WindowsProductPairStage = "committed"
	WindowsProductPairStageReceiptWritten     WindowsProductPairStage = "receipt-written"
)

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
	Schema string
	Pair   binaryadmission.WindowsProductPair
}

type WindowsProductPairTxnDeps struct {
	AdmitPair           func(cli, windowless binaryadmission.WindowsArtifact) (binaryadmission.WindowsProductPair, error)
	Promote             func(src, dst string) error
	ReadBackWindowless  func(path string, expected binaryadmission.WindowsArtifact) error
	StartSupervisor     func(cliPath string) error
	WaitSupervisorReady func(ctx context.Context, cliPath string, pair binaryadmission.WindowsProductPair) error
	RestartPrior        func(cliPath string) error
	OnCommitted         func(WindowsProductPairCommitted) error
	WriteReceipt        func(path string, raw []byte) error
	Fault               func(WindowsProductPairStage) error
	Now                 func() time.Time
}

type WindowsProductPairTxnOpts struct {
	CLIPath          string
	WindowlessPath   string
	StagedCLI        string
	StagedWindowless string
	ReceiptPath      string
	Version          string
	Commit           string
	BuildDate        string
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
	mode    os.FileMode
	present bool
}

func (t WindowsProductPairTxn) Run(ctx context.Context) (_ WindowsProductPairCommitted, retErr error) {
	o := t.Opts
	if o.Tasks == nil {
		return WindowsProductPairCommitted{}, errors.New("Windows product pair transaction: task transaction is required")
	}
	if o.CLIPath == "" || o.WindowlessPath == "" || o.StagedCLI == "" || o.StagedWindowless == "" || o.ReceiptPath == "" {
		return WindowsProductPairCommitted{}, errors.New("Windows product pair transaction: all paths are required")
	}
	defer func() { retErr = errors.Join(retErr, o.Tasks.Close()) }()
	d := withWindowsProductPairDefaults(o.Deps)
	staged, err := d.AdmitPair(
		binaryadmission.WindowsArtifact{Path: o.StagedCLI, Role: binaryadmission.WindowsArtifactRoleCLI, Version: o.Version, Commit: o.Commit, BuildDate: o.BuildDate},
		binaryadmission.WindowsArtifact{Path: o.StagedWindowless, Role: binaryadmission.WindowsArtifactRoleWindowless, Version: o.Version, Commit: o.Commit, BuildDate: o.BuildDate},
	)
	if err != nil {
		return WindowsProductPairCommitted{}, fmt.Errorf("admit staged Windows product pair: %w", err)
	}

	cliPrior, err := snapshotProductPairFile(o.CLIPath)
	if err != nil {
		return WindowsProductPairCommitted{}, err
	}
	windowlessPrior, err := snapshotProductPairFile(o.WindowlessPath)
	if err != nil {
		return WindowsProductPairCommitted{}, err
	}
	receiptPrior, err := snapshotProductPairFile(o.ReceiptPath)
	if err != nil {
		return WindowsProductPairCommitted{}, err
	}
	if err := o.Tasks.InventoryExport(); err != nil {
		return WindowsProductPairCommitted{}, fmt.Errorf("snapshot exact-owned task XML: %w", err)
	}

	rollback := func(trigger error) error {
		var restoreErrs []error
		if err := o.Tasks.RestoreImport(); err != nil {
			restoreErrs = append(restoreErrs, err)
		}
		for _, snapshot := range []productPairFileSnapshot{receiptPrior, cliPrior, windowlessPrior} {
			if err := restoreProductPairFile(snapshot); err != nil {
				restoreErrs = append(restoreErrs, err)
			}
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

	if err := d.Promote(o.StagedWindowless, o.WindowlessPath); err != nil {
		return WindowsProductPairCommitted{}, rollback(fmt.Errorf("promote windowless adapter: %w", err))
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
	if err := d.Promote(o.StagedCLI, o.CLIPath); err != nil {
		return WindowsProductPairCommitted{}, rollback(fmt.Errorf("promote canonical CUI: %w", err))
	}
	if err := fault(WindowsProductPairStageCLIPromoted); err != nil {
		return WindowsProductPairCommitted{}, rollback(err)
	}
	readBack, err := d.AdmitPair(
		binaryadmission.WindowsArtifact{Path: o.CLIPath, Role: binaryadmission.WindowsArtifactRoleCLI, Version: o.Version, Commit: o.Commit, BuildDate: o.BuildDate, SHA256: staged.CLI.SHA256},
		binaryadmission.WindowsArtifact{Path: o.WindowlessPath, Role: binaryadmission.WindowsArtifactRoleWindowless, Version: o.Version, Commit: o.Commit, BuildDate: o.BuildDate, SHA256: staged.Windowless.SHA256},
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

	committed := WindowsProductPairCommitted{Schema: WindowsProductPairCommittedSchemaV1, Pair: readBack}
	if d.OnCommitted != nil {
		if err := d.OnCommitted(committed); err != nil {
			return WindowsProductPairCommitted{}, rollback(fmt.Errorf("publish WindowsProductPairCommitted: %w", err))
		}
	}
	if err := fault(WindowsProductPairStageCommitted); err != nil {
		return WindowsProductPairCommitted{}, rollback(err)
	}
	now := d.Now().UTC().Format(time.RFC3339Nano)
	receipt := UpgradeReceiptV2{Schema: UpgradeReceiptSchemaV2, Admission: UpgradeAdmissionLocalProduct, Version: o.Version, Commit: o.Commit, BuildDate: o.BuildDate, Artifacts: readBack, InstalledAt: now}
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
	return committed, nil
}

func withWindowsProductPairDefaults(d WindowsProductPairTxnDeps) WindowsProductPairTxnDeps {
	if d.AdmitPair == nil {
		d.AdmitPair = binaryadmission.AdmitWindowsProductPair
	}
	if d.Promote == nil {
		d.Promote = func(src, dst string) error {
			body, err := os.ReadFile(src)
			if err != nil {
				return err
			}
			return writeProductPairFileAtomic(dst, body, 0o755)
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
		d.WriteReceipt = func(path string, raw []byte) error { return writeProductPairFileAtomic(path, raw, 0o600) }
	}
	if d.Now == nil {
		d.Now = time.Now
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
	return productPairFileSnapshot{path: path, body: body, mode: info.Mode().Perm(), present: true}, nil
}

func restoreProductPairFile(snapshot productPairFileSnapshot) error {
	if !snapshot.present {
		if err := os.Remove(snapshot.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return writeProductPairFileAtomic(snapshot.path, snapshot.body, snapshot.mode)
}

func writeProductPairFileAtomic(path string, body []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return err
		}
		if err := os.Rename(tmpPath, path); err != nil {
			return err
		}
	}
	ok = true
	return nil
}

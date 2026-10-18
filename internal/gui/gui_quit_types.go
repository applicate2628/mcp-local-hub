package gui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"mcp-local-hub/internal/binaryadmission"
)

// GUIQuitRequest binds one cooperative request to the original active owner.
type GUIQuitRequest struct {
	ExpectedGeneration string `json:"expected_generation"`
	StopDaemons        bool   `json:"stop_daemons"`
}

// Accepted reports persisted intent, never process exit or settled shutdown.
type GUIQuitResult struct {
	Accepted    bool   `json:"accepted"`
	Generation  string `json:"generation"`
	StopDaemons bool   `json:"stop_daemons"`
}

type GUIQuitHandler func(context.Context, GUIQuitRequest) (GUIQuitResult, error)

var ErrGUIQuitGeneration = errors.New("GUI_QUIT_GENERATION_CONFLICT")
var ErrGUIQuitSettlementUnconfirmed = errors.New("GUI_QUIT_SETTLEMENT_UNCONFIRMED")
var ErrGUIQuitBreadthConflict = errors.New("GUI_QUIT_BREADTH_CONFLICT: retained Quit did not request daemon stop")
var ErrGUIQuitDaemonOutcomeUnconfirmed = errors.New("GUI_QUIT_DAEMON_OUTCOME_UNCONFIRMED: retained Quit policy does not prove a daemon stop outcome")

// WithActiveGeneration serializes Quit with handoff and terminal publication.
// fn runs under the genuine acquired lifecycle; it must not reenter it.
func (l *GUIOwnerLifecycle) WithActiveGeneration(expected string, fn func(GUIOwnerRecord) error) error {
	if l == nil || expected == "" || fn == nil {
		return ErrGUIQuitGeneration
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	record, err := ReadGUIOwnerRecord(l.path)
	if err != nil {
		return err
	}
	if !l.matches(record) || record.Generation != expected || record.State != guiOwnerStateActive {
		return ErrGUIQuitGeneration
	}
	return fn(record)
}

func (l *GUIOwnerLifecycle) Generation() string {
	if l == nil {
		return ""
	}
	return l.generation
}

// AcquireGUIQuitObservation retains the original native process BEFORE the
// request. Metadata and HTTP cannot replace this reference or retarget it.
// The returned identity always remains the caller's Close responsibility.
func AcquireGUIQuitObservation(ctx context.Context, path string) (record GUIOwnerRecord, identity ProcessIdentity, err error) {
	if ctx == nil {
		return record, identity, errors.New("GUI quit context is required")
	}
	record, err = ReadGUIOwnerRecord(path)
	if err != nil {
		return record, identity, err
	}
	if record.Legacy || record.State != guiOwnerStateActive || record.Port == 0 {
		return record, identity, errors.New("GUI_QUIT_UNSUPPORTED_INCUMBENT_OR_GENERATION")
	}
	identity, err = retainedProcessID(record.PID)
	if err != nil {
		return record, identity, err
	}
	if !identity.Alive || identity.Denied || identity.Indeterminate || identity.Handle == 0 || !identity.StartTime.Equal(record.StartTime) {
		return record, identity, errors.New("GUI_QUIT_NATIVE_IDENTITY_UNAVAILABLE")
	}
	info, err := os.Stat(path)
	if err != nil {
		return record, identity, err
	}
	verdict := Verdict{PID: record.PID, PIDImage: identity.ImagePath, PIDStart: identity.StartTime, Mtime: info.ModTime(), pidCmdlineRaw: identity.Cmdline}
	if EvaluateProcessIdentity(verdict).Class != ProcessIdentityMatch {
		return record, identity, errors.New("GUI_QUIT_NATIVE_IDENTITY_REFUSED")
	}
	if runtime.GOOS == "windows" {
		if _, err := binaryadmission.AdmitWindowsRole(binaryadmission.WindowsArtifact{Path: identity.ImagePath, Role: binaryadmission.WindowsArtifactRoleProduct}); err != nil {
			return record, identity, err
		}
	}
	if err := VerifyGUIOwnerListener(ctx, path, record.PID, record.Port); err != nil {
		return record, identity, err
	}
	current, err := ReadGUIOwnerRecord(path)
	if err != nil {
		return record, identity, err
	}
	if current != record {
		return record, identity, ErrGUIQuitGeneration
	}
	return record, identity, nil
}

// WithAbsentGUIOwner proves absence with the original flock and handoff owner.
// Busy is the only false/nil result; it never means absence or permission.
func WithAbsentGUIOwner(ctx context.Context, path string, operation func() error) (absent bool, err error) {
	if ctx == nil || operation == nil {
		return false, errors.New("GUI absent operation requires context and callback")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	lease, err := AcquireSingleInstanceLockOnlyAt(path)
	if errors.Is(err, ErrSingleInstanceBusy) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, lease.ReleaseErr()) }()
	deadlines := DefaultRestartDeadlines()
	marker, err := readValidatedHandoffMarker(NewHandoffMarkerStore(filepath.Dir(path), deadlines))
	if err != nil {
		return false, err
	}
	if reservationWindowOpen(marker, deadlines.Now()) {
		return false, ErrHandoffReserved
	}
	record, readErr := ReadGUIOwnerRecord(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return false, readErr
	}
	if readErr == nil {
		if record.State == guiOwnerStateHandoff {
			return false, ErrHandoffReserved
		}
		if record.State != guiOwnerStateTombstone {
			identity, observeErr := processID(record.PID)
			if observeErr != nil {
				return false, observeErr
			}
			if identity.Alive || identity.Denied || identity.Indeterminate {
				return false, errors.New("GUI_QUIT_ABSENCE_UNVERIFIED")
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return true, operation()
}

// VerifyGUIQuitSettlement is only called after the SAME retained process has
// signalled. The terminal record and actual free flock are separate outcomes.
func VerifyGUIQuitSettlement(path string, original GUIOwnerRecord) (err error) {
	record, err := ReadGUIOwnerRecord(path)
	if err != nil {
		return err
	}
	if record.Legacy || record.Generation != original.Generation || record.PID != original.PID || !record.StartTime.Equal(original.StartTime) || record.State != guiOwnerStateTombstone || record.Port != 0 {
		return fmt.Errorf("%w: original terminal record unavailable", ErrGUIQuitSettlementUnconfirmed)
	}
	lease, err := AcquireSingleInstanceLockOnlyAt(path)
	if err != nil {
		return errors.Join(ErrGUIQuitSettlementUnconfirmed, err)
	}
	defer func() { err = errors.Join(err, lease.ReleaseErr()) }()
	current, err := ReadGUIOwnerRecord(path)
	if err != nil {
		return err
	}
	if current != record {
		return ErrGUIQuitGeneration
	}
	return nil
}

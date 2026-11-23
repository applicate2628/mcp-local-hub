//go:build windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"golang.org/x/sys/windows"
	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/gui"
	"mcp-local-hub/internal/process"
)

type windowsProductQuiescencePhase uint8

const (
	windowsProductQuiescenceAcquiring windowsProductQuiescencePhase = iota
	windowsProductQuiescenceInverseReady
	windowsProductQuiescenceIntentInverse
	windowsProductQuiescenceControlsInverse
	windowsProductQuiescenceClosed
)

// This operation-local composition borrows native owners and owns only its
// actual reservations. It never issues stop, restart or file mutation authority.
type windowsProductSuccessorQuiescence struct {
	mu                                                 sync.Mutex
	phase                                              windowsProductQuiescencePhase
	entered                                            bool
	closeEntered                                       bool
	causes                                             []error
	closeErr                                           error
	source                                             binaryadmission.WindowsCanonicalRecoverySourceV2
	successor                                          *binaryadmission.WindowsCanonicalEntryRecoverySetV2
	executing                                          *binaryadmission.WindowsCanonicalEntrySetV2
	journal                                            windowsProductRecoveryJournalV3
	stateDir, guiControlPath, guiOwnerPath, intentPath string
	now                                                func() time.Time
	intent                                             *api.SupervisorRuntimeIntent
	guiReads                                           []api.RequiredGUIOwnerControlObservation
	guiLease                                           gui.OwnedSingleInstanceLease
	supervisor                                         *api.SupervisorLock
	acquisition                                        flock.AcquisitionObservation
	release                                            flock.ReleaseObservation
	intentLease                                        *api.StateFileObservedLease
	intentClose                                        *api.StateFileLeaseCloseObservation
}

type windowsProductQuiescenceAcquisitionStage string

const (
	windowsProductQuiescenceAtNativeIdentity   windowsProductQuiescenceAcquisitionStage = "NATIVE_IDENTITY"
	windowsProductQuiescenceAtNativeJournal    windowsProductQuiescenceAcquisitionStage = "NATIVE_JOURNAL"
	windowsProductQuiescenceAtSuccessorBank    windowsProductQuiescenceAcquisitionStage = "SUCCESSOR_BANK"
	windowsProductQuiescenceAtInitialControls  windowsProductQuiescenceAcquisitionStage = "INITIAL_CONTROLS"
	windowsProductQuiescenceAtReservation      windowsProductQuiescenceAcquisitionStage = "RESERVATION"
	windowsProductQuiescenceAtReservedControls windowsProductQuiescenceAcquisitionStage = "RESERVED_CONTROLS"
	windowsProductQuiescenceAtHeldRecheck      windowsProductQuiescenceAcquisitionStage = "HELD_RECHECK"
)

func isWindowsProductQuiescenceAcquisitionStage(stage windowsProductQuiescenceAcquisitionStage) bool {
	switch stage {
	case windowsProductQuiescenceAtNativeIdentity, windowsProductQuiescenceAtNativeJournal, windowsProductQuiescenceAtSuccessorBank,
		windowsProductQuiescenceAtInitialControls, windowsProductQuiescenceAtReservation,
		windowsProductQuiescenceAtReservedControls, windowsProductQuiescenceAtHeldRecheck:
		return true
	default:
		return false
	}
}

func (s *windowsProductQuiescenceAcquisitionStage) record(next windowsProductQuiescenceAcquisitionStage) {
	if s != nil {
		*s = next
	}
}

type windowsProductQuiescenceAcquisitionError struct {
	stage windowsProductQuiescenceAcquisitionStage
	cause error
}

func (e *windowsProductQuiescenceAcquisitionError) Error() string { return e.cause.Error() }
func (e *windowsProductQuiescenceAcquisitionError) Unwrap() error { return e.cause }

func acquireWindowsProductSuccessorQuiescence(ctx context.Context, stateDir, guiControlPath string, journal windowsProductRecoveryJournalV3, source binaryadmission.WindowsCanonicalRecoverySourceV2, successor *binaryadmission.WindowsCanonicalEntryRecoverySetV2, executing *binaryadmission.WindowsCanonicalEntrySetV2) (q *windowsProductSuccessorQuiescence, retErr error) {
	q = &windowsProductSuccessorQuiescence{source: source, successor: successor, executing: executing, journal: journal, stateDir: stateDir, guiControlPath: guiControlPath, intentPath: filepath.Join(stateDir, "supervisor-intent.json")}
	stage := windowsProductQuiescenceAtNativeIdentity
	defer func() {
		if retErr != nil {
			retErr = &windowsProductQuiescenceAcquisitionError{stage: stage, cause: retErr}
			q.causes = append(q.causes, retErr)
		}
	}()
	if ctx == nil || source == nil || successor == nil || journal.EntryBundle == nil || !cleanAbsolutePath(stateDir) || !cleanAbsolutePath(guiControlPath) {
		return q, errors.New("current successor quiescence inputs unavailable")
	}
	if executing == nil {
		return q, errors.New("executing-entry native lender unavailable")
	}
	original, ok := source.(*windowsCanonicalJournalRecoverySource)
	if !ok || original == nil || original.deps.Now == nil {
		return q, errors.New("original current recovery source/clock unavailable")
	}
	q.now = original.deps.Now
	limit, cancel := context.WithTimeout(ctx, time.Duration(api.DefaultSupervisorReapQuiesceTimeoutMs)*time.Millisecond)
	defer cancel()
	if err := q.verifyNative(limit, &stage); err != nil {
		return q, err
	}
	targets, err := binaryadmission.CanonicalEntryTargetsV2(journal.EntryBundle.CanonicalEntry.Target)
	if err != nil {
		return q, err
	}
	// The executing original denies links/reparse and retains native ancestor
	// guards. Its original root must not alias any selected mutation parent.
	directory, err := executing.BundleDirectorySnapshot()
	if err != nil {
		return q, err
	}
	for _, selected := range original.memberDirectorySnapshots {
		if directory.ParentIdentity == selected.DirectoryIdentity || directory.DirectoryIdentity == selected.DirectoryIdentity {
			return q, errors.New("executing-entry native parent aliases an inverse exclusion")
		}
	}
	if samePath(directory.Root, filepath.Dir(targets.CanonicalEntryPath)) {
		return q, errors.New("executing-entry aliases current inverse root")
	}
	stage = windowsProductQuiescenceAtInitialControls
	first, err := api.NewAPI().ObserveRequiredGUIOwnerControlContextIn(limit, guiControlPath)
	q.guiReads = append(q.guiReads, first)
	if err != nil || first.Control == nil {
		return q, errors.Join(err, errors.New("required GUI control unavailable"))
	}
	q.intent, err = api.ObserveSupervisorRuntimeIntent(limit, q.intentPath, q.now())
	if err != nil || q.intent == nil {
		return q, errors.Join(err, errors.New("current runtime intent unavailable"))
	}
	if !validLowerHexSHA256(q.intent.SHA256) || !validLowerHexSHA256(q.intent.StopSHA256) || q.intent.Ports == nil {
		return q, errors.New("current runtime scope incomplete")
	}
	q.guiOwnerPath, err = resolveUpgradeGUIOwnerPath()
	if err != nil {
		return q, err
	}
	if !samePath(filepath.Dir(q.guiOwnerPath), stateDir) {
		return q, errors.New("GUI reservation belongs to a different current namespace")
	}
	stage = windowsProductQuiescenceAtReservation
	_, err = acquireGUIOwnedPriorReservation(limit, nil, guiOwnedPriorReservationOps{
		ProveNoGUIOrTray: q.absent,
		AcquireExact: func() (gui.OwnedSingleInstanceLease, error) {
			lease, err := gui.AcquireSingleInstanceLockOnlyAt(q.guiOwnerPath)
			q.guiLease = lease // register even lease+error before the helper can lose it
			return lease, err
		},
	})
	if err != nil {
		return q, err
	}
	stage = windowsProductQuiescenceAtReservedControls
	q.supervisor, q.acquisition, err = api.AcquireSupervisorLockQuietObserved(limit, filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		return q, err
	}
	second, err := api.NewAPI().ObserveRequiredGUIOwnerControlContextIn(limit, guiControlPath)
	q.guiReads = append(q.guiReads, second)
	if err != nil || second.Control == nil {
		return q, errors.Join(err, errors.New("reserved GUI control unavailable"))
	}
	if !first.Control.Equal(*second.Control) {
		return q, errors.New("GUI control changed during quiescence receiving")
	}
	q.intentLease, err = api.AcquireStateFileObservedLease(limit, q.intentPath)
	if err != nil {
		return q, err
	}
	stage = windowsProductQuiescenceAtHeldRecheck
	if err = q.verifyHeld(limit, nil); err != nil {
		return q, err
	}
	q.phase = windowsProductQuiescenceInverseReady
	return q, nil
}

func (q *windowsProductSuccessorQuiescence) verifyNative(ctx context.Context, stage *windowsProductQuiescenceAcquisitionStage) (retErr error) {
	if ctx == nil {
		return errors.New("quiescence context required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if q.source == nil || q.successor == nil || q.executing == nil {
		return errors.New("original native quiescence lenders required")
	}
	stage.record(windowsProductQuiescenceAtNativeJournal)
	if err := q.source.ValidateOriginalRecovery(ctx); err != nil {
		return err
	}
	stage.record(windowsProductQuiescenceAtSuccessorBank)
	if err := q.successor.VerifyUnchanged(ctx); err != nil {
		return err
	}
	stage.record(windowsProductQuiescenceAtNativeIdentity)
	if err := q.executing.VerifyUnchanged(); err != nil {
		return err
	}
	currentProcess := windows.CurrentProcess()
	var originalProcess windows.Handle
	if err := windows.DuplicateHandle(currentProcess, currentProcess, currentProcess, &originalProcess, windows.PROCESS_QUERY_LIMITED_INFORMATION, false, 0); err != nil {
		return errors.Join(fmt.Errorf("duplicate current process query handle: %w", err), ctx.Err())
	}
	defer func() {
		if err := windows.CloseHandle(originalProcess); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close current process query handle: %w", err))
		}
	}()
	return errors.Join(q.executing.VerifyOriginalProductProcessHandle(uintptr(originalProcess)), ctx.Err())
}

type windowsProductAbsenceCheck string

const (
	windowsProductAbsenceContext          windowsProductAbsenceCheck = "CONTEXT"
	windowsProductAbsenceIssuedImages     windowsProductAbsenceCheck = "ISSUED_IMAGES"
	windowsProductAbsenceFrozenPorts      windowsProductAbsenceCheck = "FROZEN_PORTS"
	windowsProductAbsenceProcessInventory windowsProductAbsenceCheck = "PROCESS_INVENTORY"
	windowsProductAbsenceProcessIdentity  windowsProductAbsenceCheck = "PROCESS_IDENTITY"
	windowsProductAbsenceProcessPresent   windowsProductAbsenceCheck = "PROCESS_PRESENT"
	windowsProductAbsenceGUIOwnerRecord   windowsProductAbsenceCheck = "GUI_OWNER_RECORD"
	windowsProductAbsenceGUIEndpoint      windowsProductAbsenceCheck = "GUI_ENDPOINT"
)

func isWindowsProductAbsenceCheck(check windowsProductAbsenceCheck) bool {
	switch check {
	case windowsProductAbsenceContext, windowsProductAbsenceIssuedImages, windowsProductAbsenceFrozenPorts,
		windowsProductAbsenceProcessInventory, windowsProductAbsenceProcessIdentity, windowsProductAbsenceProcessPresent,
		windowsProductAbsenceGUIOwnerRecord, windowsProductAbsenceGUIEndpoint:
		return true
	default:
		return false
	}
}

type windowsProductAbsenceError struct {
	check windowsProductAbsenceCheck
	cause error
}

func (e *windowsProductAbsenceError) Error() string { return e.cause.Error() }
func (e *windowsProductAbsenceError) Unwrap() error { return e.cause }

func (q *windowsProductSuccessorQuiescence) absent(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return &windowsProductAbsenceError{check: windowsProductAbsenceContext, cause: err}
	}
	if q.intent == nil {
		return &windowsProductAbsenceError{check: windowsProductAbsenceFrozenPorts, cause: errors.New("complete current intent scope required")}
	}
	images, err := q.successor.IssuedImagePaths(ctx)
	if err != nil {
		return &windowsProductAbsenceError{check: windowsProductAbsenceIssuedImages, cause: err}
	}
	// The installed Product remains protected even if it was not returned
	// among the successor's issued images.
	images = append(images, q.journal.Product.Target)
	frozen := expandWindowsProductFrozenPorts(q.intent.Ports)
	if _, err := observeWindowsProductIntentPorts(ctx, q.journal.Product.Target, frozen, true); err != nil {
		return &windowsProductAbsenceError{check: windowsProductAbsenceFrozenPorts, cause: err}
	}
	names := []string{"mcphub.exe", "mcphub-windowless.exe"}
	for _, image := range images {
		names = append(names, filepath.Base(image))
	}
	pids, err := productImageSnapshotPIDs(ctx, names, nativeProductProcessSnapshotOps())
	if err != nil {
		return &windowsProductAbsenceError{check: windowsProductAbsenceProcessInventory, cause: err}
	}
	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return &windowsProductAbsenceError{check: windowsProductAbsenceContext, cause: err}
		}
		observation, err := process.ObservePIDHandleIdentity(ctx, pid)
		if err != nil {
			return &windowsProductAbsenceError{check: windowsProductAbsenceProcessIdentity, cause: err}
		}
		if observation.ConfirmedAbsent {
			continue
		}
		identity := observation.Identity
		for _, image := range images {
			if samePath(identity.ExecutablePath, image) {
				return &windowsProductAbsenceError{check: windowsProductAbsenceProcessPresent, cause: fmt.Errorf("%w: current selected image still has a process user", ErrWindowsProductRecoveryPending)}
			}
		}
	}
	record, err := gui.ReadGUIOwnerRecord(q.guiOwnerPath)
	if err != nil {
		return &windowsProductAbsenceError{check: windowsProductAbsenceGUIOwnerRecord, cause: err}
	}
	if err := proveUpgradeGUIOwnerEndpointAbsent(ctx, record, api.LoopbackPortOwnerPIDContext); err != nil {
		return &windowsProductAbsenceError{check: windowsProductAbsenceGUIEndpoint, cause: err}
	}
	return nil
}

// Toolhelp supplies executable basenames only. It preselects candidates for
// the held native process identity check and contributes no path authority.
func productImageSnapshotPIDs(ctx context.Context, names []string, ops productProcessSnapshotOps) ([]int, error) {
	var pids []int
	haveProcess := false
	err := walkProductProcessSnapshot(ctx, ops, func(entry *windows.ProcessEntry32) (bool, error) {
		if entry.ProcessID == 0 {
			return true, nil // The system idle pseudo-process has no image owner.
		}
		if !slices.Contains(entry.ExeFile[:], 0) {
			return false, errors.New("process snapshot contains an incomplete image name")
		}
		name := windows.UTF16ToString(entry.ExeFile[:])
		if name == "" {
			return false, errors.New("process snapshot contains a row without an image name")
		}
		haveProcess = true
		for _, image := range names {
			if strings.EqualFold(name, image) {
				pids = append(pids, int(entry.ProcessID))
				break
			}
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	if !haveProcess {
		return nil, errors.New("process snapshot contains no complete process rows")
	}
	return pids, nil
}

type currentInverseCheck string

const (
	currentInverseCheckPhase          currentInverseCheck = "PHASE"
	currentInverseCheckNativeIdentity currentInverseCheck = "NATIVE_IDENTITY"
	currentInverseCheckNativeJournal  currentInverseCheck = "NATIVE_JOURNAL"
	currentInverseCheckSuccessorBank  currentInverseCheck = "SUCCESSOR_BANK"
	currentInverseCheckReservations   currentInverseCheck = "RESERVATIONS"
	currentInverseCheckSupervisor     currentInverseCheck = "SUPERVISOR_RESERVATION"
	currentInverseCheckIntentLease    currentInverseCheck = "INTENT_RESERVATION"
	currentInverseCheckIntentBorrow   currentInverseCheck = "INTENT_BORROW"
	currentInverseCheckIntentObserve  currentInverseCheck = "INTENT_OBSERVATION"
	currentInverseCheckIntentCompare  currentInverseCheck = "INTENT_COMPARE"
	currentInverseCheckAbsence        currentInverseCheck = "ABSENCE"
)

func isCurrentInverseCheck(check currentInverseCheck) bool {
	switch check {
	case currentInverseCheckPhase, currentInverseCheckNativeIdentity, currentInverseCheckNativeJournal,
		currentInverseCheckSuccessorBank, currentInverseCheckReservations, currentInverseCheckSupervisor,
		currentInverseCheckIntentLease, currentInverseCheckIntentBorrow, currentInverseCheckIntentObserve,
		currentInverseCheckIntentCompare, currentInverseCheckAbsence:
		return true
	default:
		return false
	}
}

func (check *currentInverseCheck) record(next currentInverseCheck) {
	if check != nil {
		*check = next
	}
}

type currentInverseCheckError struct {
	check currentInverseCheck
	cause error
}

func (e *currentInverseCheckError) Error() string { return e.cause.Error() }
func (e *currentInverseCheckError) Unwrap() error { return e.cause }

func (q *windowsProductSuccessorQuiescence) verifyHeld(ctx context.Context, check *currentInverseCheck) error {
	var nativeStage *windowsProductQuiescenceAcquisitionStage
	if check != nil {
		stage := windowsProductQuiescenceAtNativeIdentity
		nativeStage = &stage
	}
	if err := q.verifyNative(ctx, nativeStage); err != nil {
		if nativeStage != nil {
			switch *nativeStage {
			case windowsProductQuiescenceAtNativeJournal:
				check.record(currentInverseCheckNativeJournal)
			case windowsProductQuiescenceAtSuccessorBank:
				check.record(currentInverseCheckSuccessorBank)
			default:
				check.record(currentInverseCheckNativeIdentity)
			}
		}
		return err
	}
	check.record(currentInverseCheckReservations)
	if q.guiLease == nil || q.supervisor == nil || q.intentLease == nil {
		return errors.New("original quiescence reservations incomplete")
	}
	check.record(currentInverseCheckSupervisor)
	if err := q.supervisor.ValidateQuietObservedHeld(ctx, filepath.Join(q.stateDir, "supervisor.lock")); err != nil {
		return err
	}
	check.record(currentInverseCheckIntentLease)
	if err := q.intentLease.ValidateHeld(ctx, q.intentPath); err != nil {
		return err
	}
	check.record(currentInverseCheckIntentBorrow)
	return q.intentLease.Borrow(ctx, q.intentPath, func() error {
		check.record(currentInverseCheckIntentObserve)
		observed, err := api.ObserveSupervisorRuntimeIntent(ctx, q.intentPath, q.now())
		if err != nil {
			return err
		}
		check.record(currentInverseCheckIntentCompare)
		if observed == nil || observed.SHA256 != q.intent.SHA256 || observed.StopSHA256 != q.intent.StopSHA256 || observed.Generation != q.intent.Generation || !reflect.DeepEqual(observed.Ports, q.intent.Ports) || !reflect.DeepEqual(observed.Stopped, q.intent.Stopped) {
			return errors.New("current runtime intent/cohort/stops changed")
		}
		check.record(currentInverseCheckAbsence)
		if err := q.absent(ctx); err != nil {
			return err
		}
		check.record(currentInverseCheckIntentBorrow)
		return nil
	})
}

func (q *windowsProductSuccessorQuiescence) enter(phase windowsProductQuiescencePhase) error {
	if q == nil {
		return errors.New("original successor quiescence required")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.phase != phase || q.entered || q.closeEntered || len(q.causes) > 0 {
		return errors.Join(errors.New("quiescence phase unavailable"), errors.Join(q.causes...))
	}
	q.entered = true
	return nil
}
func (q *windowsProductSuccessorQuiescence) leave(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.entered = false
	if err != nil {
		q.causes = append(q.causes, err)
	}
}
func (q *windowsProductSuccessorQuiescence) VerifyForInverse(ctx context.Context) (retErr error) {
	if err := q.enter(windowsProductQuiescenceInverseReady); err != nil {
		return &currentInverseCheckError{check: currentInverseCheckPhase, cause: err}
	}
	defer func() { q.leave(retErr) }()
	var check currentInverseCheck
	if err := q.verifyHeld(ctx, &check); err != nil {
		return &currentInverseCheckError{check: check, cause: err}
	}
	return nil
}
func (q *windowsProductSuccessorQuiescence) WithIntentInverse(ctx context.Context, body func(*api.StateFileObservedLease) error) (retErr error) {
	if body == nil {
		return errors.New("original conditional intent inverse required")
	}
	if err := q.enter(windowsProductQuiescenceInverseReady); err != nil {
		return err
	}
	defer func() { q.leave(retErr) }()
	q.mu.Lock()
	q.phase = windowsProductQuiescenceIntentInverse
	q.mu.Unlock()
	// No q mutex is held across original owner operations or the callback.
	retErr = q.verifyHeld(ctx, nil)
	if retErr == nil {
		retErr = q.intentLease.Borrow(ctx, q.intentPath, func() error { return body(q.intentLease) })
	}
	closed, closeErr := q.intentLease.CloseObserved()
	q.intentClose = closed
	if closeErr == nil {
		closeErr = q.intentClose.Validate(q.intentLease)
	}
	retErr = errors.Join(retErr, closeErr)
	if retErr == nil {
		q.mu.Lock()
		q.phase = windowsProductQuiescenceControlsInverse
		q.mu.Unlock()
	}
	return retErr
}
func (q *windowsProductSuccessorQuiescence) Close() error {
	if q == nil {
		return errors.New("original successor quiescence required")
	}
	q.mu.Lock()
	if q.entered {
		err := errors.New("quiescence close pending on entered operation")
		q.causes = append(q.causes, err)
		q.mu.Unlock()
		return err
	}
	if q.closeEntered {
		if q.phase != windowsProductQuiescenceClosed {
			err := errors.New("quiescence closure still entered")
			q.causes = append(q.causes, err)
			q.mu.Unlock()
			return err
		}
		err := errors.Join(q.closeErr, errors.Join(q.causes...))
		q.mu.Unlock()
		return err
	}
	q.closeEntered = true
	q.mu.Unlock()
	var causes []error
	if q.intentLease != nil {
		closed, err := q.intentLease.CloseObserved()
		q.intentClose = closed
		if err == nil && q.intentLease.AcquisitionObservation().Held {
			err = closed.Validate(q.intentLease)
		}
		causes = append(causes, err)
	}
	for _, read := range q.guiReads {
		causes = append(causes, read.OperationError, read.ReceivingError)
		if read.Lease != nil {
			closed, err := read.Lease.CloseObserved()
			if err == nil && read.Acquisition.Held {
				err = closed.Validate(read.Lease)
			}
			causes = append(causes, err)
		}
	}
	if q.supervisor != nil {
		var err error
		q.release, err = q.supervisor.ReleaseQuietObserved()
		causes = append(causes, err)
	}
	if q.guiLease != nil {
		causes = append(causes, q.guiLease.ReleaseErr())
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closeErr = errors.Join(errors.Join(q.causes...), errors.Join(causes...))
	q.phase = windowsProductQuiescenceClosed
	return q.closeErr
}

// The command root resolves the existing per-user paths once. A different
// explicit state namespace cannot borrow ambient GUI control or reservation.
func bindWindowsProductSuccessorQuiescence(req *windowsProductRecoveryRequest, executing *binaryadmission.WindowsCanonicalEntrySetV2) {
	stateDir := req.StateDir
	controlPath := api.SettingsPath()
	namespace, namespaceErr := api.DaemonStateDirReadOnly()
	req.AcquireSuccessorQuiescence = func(ctx context.Context, j windowsProductRecoveryJournalV3, source binaryadmission.WindowsCanonicalRecoverySourceV2, successor *binaryadmission.WindowsCanonicalEntryRecoverySetV2) (*windowsProductSuccessorQuiescence, error) {
		if namespaceErr != nil || !samePath(namespace, stateDir) || !cleanAbsolutePath(controlPath) || !samePath(filepath.Dir(controlPath), namespace) {
			return nil, errors.Join(namespaceErr, errors.New("current per-user GUI/state binding unavailable"))
		}
		return acquireWindowsProductSuccessorQuiescence(ctx, stateDir, controlPath, j, source, successor, executing)
	}
}

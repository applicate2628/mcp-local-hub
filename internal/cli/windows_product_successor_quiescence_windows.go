//go:build windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
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

func acquireWindowsProductSuccessorQuiescence(ctx context.Context, stateDir, guiControlPath string, journal windowsProductRecoveryJournalV3, source binaryadmission.WindowsCanonicalRecoverySourceV2, successor *binaryadmission.WindowsCanonicalEntryRecoverySetV2, executing *binaryadmission.WindowsCanonicalEntrySetV2) (q *windowsProductSuccessorQuiescence, retErr error) {
	q = &windowsProductSuccessorQuiescence{source: source, successor: successor, executing: executing, journal: journal, stateDir: stateDir, guiControlPath: guiControlPath, intentPath: filepath.Join(stateDir, "supervisor-intent.json")}
	defer func() {
		if retErr != nil {
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
	if err := q.verifyNative(limit); err != nil {
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
	if err = q.absent(limit); err != nil {
		return q, err
	}
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
	if err = q.verifyHeld(limit); err != nil {
		return q, err
	}
	q.phase = windowsProductQuiescenceInverseReady
	return q, nil
}

func (q *windowsProductSuccessorQuiescence) verifyNative(ctx context.Context) (retErr error) {
	if ctx == nil {
		return errors.New("quiescence context required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if q.source == nil || q.successor == nil || q.executing == nil {
		return errors.New("original native quiescence lenders required")
	}
	if err := q.source.ValidateOriginalRecovery(ctx); err != nil {
		return err
	}
	if err := q.successor.VerifyUnchanged(ctx); err != nil {
		return err
	}
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

func (q *windowsProductSuccessorQuiescence) absent(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if q.intent == nil {
		return errors.New("complete current intent scope required")
	}
	images, err := q.successor.IssuedImagePaths(ctx)
	if err != nil {
		return err
	}
	frozen := expandWindowsProductFrozenPorts(q.intent.Ports)
	if _, err := observeWindowsProductIntentPorts(ctx, q.journal.Product.Target, frozen, true); err != nil {
		return err
	}
	names := []string{"mcphub.exe", "mcphub-windowless.exe"}
	for _, image := range images {
		names = append(names, filepath.Base(image))
	}
	rows, err := api.NewAPI().ListMatchingProcesses(names)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		observation, err := process.ObservePIDHandleIdentity(ctx, row.PID)
		if err != nil {
			return err
		}
		if observation.ConfirmedAbsent {
			continue
		}
		identity := observation.Identity
		for _, image := range images {
			if samePath(identity.ExecutablePath, image) {
				return fmt.Errorf("%w: current selected image still has a process user", ErrWindowsProductRecoveryPending)
			}
		}
		role := supervisorCommandLineSubcommand(row.Cmdline)
		if role == "gui" || role == "tray" || role == "supervise" {
			// Other namespaces remain untouched; their GUI listener/singleton may
			// nevertheless conflict with this required per-user slot below.
			if samePath(identity.ExecutablePath, q.journal.Product.Target) {
				return errors.New("current runtime actor remains")
			}
		}
	}
	record, err := gui.ReadGUIOwnerRecord(q.guiOwnerPath)
	if err != nil {
		return err
	}
	return proveUpgradeGUIOwnerEndpointAbsent(ctx, record, api.LoopbackPortOwnerPIDContext)
}

func (q *windowsProductSuccessorQuiescence) verifyHeld(ctx context.Context) error {
	if err := q.verifyNative(ctx); err != nil {
		return err
	}
	if q.guiLease == nil || q.supervisor == nil || q.intentLease == nil {
		return errors.New("original quiescence reservations incomplete")
	}
	if err := q.supervisor.ValidateQuietObservedHeld(ctx, filepath.Join(q.stateDir, "supervisor.lock")); err != nil {
		return err
	}
	if err := q.intentLease.ValidateHeld(ctx, q.intentPath); err != nil {
		return err
	}
	return q.intentLease.Borrow(ctx, q.intentPath, func() error {
		observed, err := api.ObserveSupervisorRuntimeIntent(ctx, q.intentPath, q.now())
		if err != nil {
			return err
		}
		if observed == nil || observed.SHA256 != q.intent.SHA256 || observed.StopSHA256 != q.intent.StopSHA256 || observed.Generation != q.intent.Generation || !reflect.DeepEqual(observed.Ports, q.intent.Ports) || !reflect.DeepEqual(observed.Stopped, q.intent.Stopped) {
			return errors.New("current runtime intent/cohort/stops changed")
		}
		return q.absent(ctx)
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
		return err
	}
	defer func() { q.leave(retErr) }()
	return q.verifyHeld(ctx)
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
	retErr = q.verifyHeld(ctx)
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

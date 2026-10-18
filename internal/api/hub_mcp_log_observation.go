package api

import (
	"context"
	"errors"
	"fmt"
	"github.com/gofrs/flock"
	"io"
	"os"
	"path/filepath"
	"time"
)

type HubMcpLogSnapshotV1 struct {
	Schema                 string                   `json:"schema"`
	Version                uint32                   `json:"version"`
	Level                  string                   `json:"level"`
	Event                  string                   `json:"event"`
	Stage                  string                   `json:"stage"`
	InvocationEntered      bool                     `json:"invocation_entered"`
	EnvelopeDefined        bool                     `json:"envelope_defined"`
	ResolverReceived       bool                     `json:"resolver_received"`
	ParentReceived         bool                     `json:"parent_received"`
	MutexAcquired          bool                     `json:"mutex_acquired"`
	LockAcquisitionDefined bool                     `json:"lock_acquisition_defined"`
	RotationSizeDefined    bool                     `json:"rotation_size_defined"`
	RotationSizeError      *StateFileReadFailureV1  `json:"rotation_size_error"`
	RotationAttempted      bool                     `json:"rotation_attempted"`
	RotationRemoveError    *StateFileReadFailureV1  `json:"rotation_remove_error"`
	RotationRenameError    *StateFileReadFailureV1  `json:"rotation_rename_error"`
	FileAcquired           bool                     `json:"file_acquired"`
	ChmodAttempted         bool                     `json:"chmod_attempted"`
	WriteAttempted         bool                     `json:"write_attempted"`
	WriteComplete          bool                     `json:"write_complete"`
	SyncAttempted          bool                     `json:"sync_attempted"`
	SyncError              *StateFileReadFailureV1  `json:"sync_error"`
	AppendError            *StateFileReadFailureV1  `json:"append_error"`
	ResourceCloseErrors    []StateFileReadFailureV1 `json:"resource_close_errors"`
	ResourcesComplete      bool                     `json:"resources_complete"`
	FinishedInBound        bool                     `json:"finished_in_bound"`
}

type hubMcpAuditParent struct {
	resources  *stateFileReadBuilder
	identity   *ConfigParentDirectoryIdentityV1
	finalized  bool
	closureErr error
}
type HubMcpLogObservation struct {
	original                                                                            *HubMcpLogObservation
	policy                                                                              *StateReadPolicyObservation
	resolver                                                                            *StateDirResolutionObservation
	parent                                                                              *hubMcpAuditParent
	view                                                                                HubMcpLogSnapshotV1
	deadline                                                                            time.Time
	deadlineDefined, finalized, policyReceived                                          bool
	acquisition                                                                         flock.AcquisitionObservation
	release                                                                             flock.ReleaseObservation
	lockReturned, releaseReturned, fileCloseAttempted, fileCloseReturned, mutexReleased bool
	primaryErr, resourceErr                                                             error
	requestPayloadSHA256                                                                string
	effects                                                                             hubMcpLogEffects
}

type hubMcpLogAppendFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
}
type hubMcpLogLock interface {
	Lock() error
	Unlock() error
	TryLockContextObserved(context.Context, time.Duration) (flock.AcquisitionObservation, error)
	ReleaseObserved() (flock.ReleaseObservation, error)
}
type hubMcpLogEffects struct {
	now                    func() time.Time
	marshal                func(string, string, map[string]any) ([]byte, error)
	policyValidate         func(context.Context, *StateReadPolicyObservation) error
	parent                 func(context.Context, *StateReadPolicyObservation) (*hubMcpAuditParent, error)
	legacyDir              func() (string, error)
	mutexTryLock           func() bool
	mutexLock, mutexUnlock func()
	lock                   func(string) hubMcpLogLock
	stat                   func(string) (int64, error)
	remove                 func(string) error
	rename                 func(string, string) error
	open                   func(string) (hubMcpLogAppendFile, error)
	chmod                  func(string) error
}

// Invocation-local delivery failures, independent of policy observation. This
// retains acquired outcomes only; it is neither a receipt nor another logger.
type hubMcpLogDelivery struct {
	err error
	// Preserve original uncertain resources and their actual observations in
	// the returned cause. No retry, second owner or completion is fabricated.
	pendingFile hubMcpLogAppendFile
	pendingLock hubMcpLogLock
	acquisition flock.AcquisitionObservation
	release     flock.ReleaseObservation
}

func (d *hubMcpLogDelivery) Error() string { return "required hub-mcp log delivery failed" }
func (d *hubMcpLogDelivery) Unwrap() error { return d.err }

func (d *hubMcpLogDelivery) add(err error) {
	if d != nil && err != nil {
		d.err = errors.Join(d.err, err)
	}
}

func hubMcpLogFailure(err error, stage string) *StateFileReadFailureV1 {
	if err == nil {
		return nil
	}
	return &StateFileReadFailureV1{Code: "HUB_MCP_AUDIT_" + stage, Stage: stage, Slot: "audit"}
}
func (o *HubMcpLogObservation) Snapshot() HubMcpLogSnapshotV1 {
	if o == nil {
		return HubMcpLogSnapshotV1{}
	}
	v := o.view
	v.ResourceCloseErrors = append([]StateFileReadFailureV1{}, v.ResourceCloseErrors...)
	for _, p := range []**StateFileReadFailureV1{&v.RotationSizeError, &v.RotationRemoveError, &v.RotationRenameError, &v.SyncError, &v.AppendError} {
		if *p != nil {
			x := **p
			*p = &x
		}
	}
	return v
}
func (o *HubMcpLogObservation) boundary(ctx context.Context, stage string) error {
	if !o.finalized {
		o.view.Stage = stage
	}
	if ctx == nil {
		return errors.New("original audit context required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if o.deadlineDefined && !o.effects.now().Before(o.deadline) {
		return context.DeadlineExceeded
	}
	return nil
}
func (o *HubMcpLogObservation) resourceFailure(err error, stage string) {
	if err != nil {
		o.resourceErr = errors.Join(o.resourceErr, err)
		o.view.ResourceCloseErrors = append(o.view.ResourceCloseErrors, *hubMcpLogFailure(err, stage))
	}
}
func (o *HubMcpLogObservation) ValidateResourceClosure(ctx context.Context, policy *StateReadPolicyObservation, level, event string) error {
	if level == "" {
		level = "info"
	}
	if o == nil || o.original != o || !o.finalized || !o.policyReceived || o.policy != policy || policy == nil || o.resolver != policy.resolution || o.view.Level != level || o.view.Event != event || !o.view.ResourcesComplete || !o.view.FinishedInBound {
		return errors.New("same finalized audit resource observation required")
	}
	if err := o.boundary(ctx, "receive"); err != nil {
		return err
	}
	if o.deadlineDefined {
		d, ok := ctx.Deadline()
		if !ok || d.After(o.deadline) {
			return errors.New("audit receiving deadline renewed")
		}
	}
	return errors.Join(o.resourceErr, o.effects.policyValidate(ctx, policy))
}

func LogHubMcpEventObserved(ctx context.Context, policy *StateReadPolicyObservation, level, event string, fields map[string]any) (*HubMcpLogObservation, error) {
	return logHubMcpEventObservedWithEffects(ctx, policy, level, event, fields, hubMcpLogProductionEffects())
}
func logHubMcpEventObservedWithEffects(ctx context.Context, policy *StateReadPolicyObservation, level, event string, fields map[string]any, effects hubMcpLogEffects) (observation *HubMcpLogObservation, retErr error) {
	if level == "" {
		level = "info"
	}
	o := &HubMcpLogObservation{policy: policy, effects: effects, view: HubMcpLogSnapshotV1{Schema: "hub-mcp-log-observation-v1", Version: 1, Level: level, Event: event, InvocationEntered: true, ResourceCloseErrors: []StateFileReadFailureV1{}}}
	o.original = o
	observation = o
	if policy != nil {
		o.resolver = policy.resolution
	}
	if ctx != nil {
		o.deadline, o.deadlineDefined = ctx.Deadline()
	}
	defer func() {
		o.primaryErr = retErr
		o.finalized = true
		o.view.AppendError = hubMcpLogFailure(retErr, "APPEND")
		o.view.FinishedInBound = o.boundary(ctx, "finish") == nil
		o.view.ResourcesComplete = o.policyReceived && o.resourceErr == nil && (!o.view.MutexAcquired || o.mutexReleased) && (!o.view.FileAcquired || (o.fileCloseAttempted && o.fileCloseReturned)) && (!o.view.LockAcquisitionDefined || (o.lockReturned && o.releaseReturned))
		o.view.Stage = "finished"
		retErr = errors.Join(retErr, o.resourceErr)
	}()
	if err := o.boundary(ctx, "policy"); err != nil {
		return o, err
	}
	if err := effects.policyValidate(ctx, policy); err != nil {
		return o, err
	}
	o.policyReceived = true
	o.view.ResolverReceived = true
	line, err := effects.marshal(level, event, fields)
	if err != nil {
		return o, err
	}
	o.view.EnvelopeDefined = true
	o.requestPayloadSHA256 = stateFileReadObservationDigest(line)
	return o, appendHubMcpLogLineCore(line, o, effects, ctx, nil)
}

func appendHubMcpLogLineCore(line []byte, o *HubMcpLogObservation, e hubMcpLogEffects, ctx context.Context, delivery *hubMcpLogDelivery) (retErr error) {
	var dir string
	if o != nil {
		if err := o.boundary(ctx, "parent"); err != nil {
			return err
		}
		parent, err := e.parent(ctx, o.policy)
		o.parent = parent
		o.view.ParentReceived = parent != nil
		if parent == nil || !parent.finalized {
			o.resourceFailure(errors.New("original audit parent resources unavailable"), "PARENT_CLOSE")
		} else {
			o.resourceFailure(parent.closureErr, "PARENT_CLOSE")
		}
		if err != nil {
			return err
		}
		if o.resourceErr != nil {
			return o.resourceErr
		}
		dir = o.policy.stateDir
		if err := o.boundary(ctx, "mutex"); err != nil {
			return err
		}
		if !e.mutexTryLock() {
			return errors.New("HUB_MCP_AUDIT_MUTEX_BUSY")
		}
		o.view.MutexAcquired = true
	} else {
		e.mutexLock()
		var err error
		dir, err = e.legacyDir()
		if err != nil {
			e.mutexUnlock()
			return err
		}
	}
	defer func() {
		e.mutexUnlock()
		if o != nil {
			o.mutexReleased = true
		}
	}()
	path := filepath.Join(dir, hubMcpLogFileLeaf)
	lockPath := filepath.Join(dir, hubMcpLogLockFileLeaf)
	if o != nil {
		if err := o.boundary(ctx, "lock"); err != nil {
			return err
		}
	}
	lk := e.lock(lockPath)
	if o == nil && delivery == nil {
		if err := lk.Lock(); err != nil {
			return fmt.Errorf("hub-mcp log flock: %w", err)
		}
		defer func() { _ = lk.Unlock() }()
	} else {
		// Required delivery has no policy operand. It uses the real observed
		// pair under the original logger namespace and no-deadline semantics.
		if o == nil {
			ctx = context.Background()
		}
		resourceFailure := func(err error, stage string) {
			if o != nil {
				o.resourceFailure(err, stage)
			}
			delivery.add(err)
		}
		acquisition, err := lk.TryLockContextObserved(ctx, stateFileLockPollInterval)
		if delivery != nil {
			delivery.acquisition = acquisition
			delivery.add(acquisition.Error)
			if err == nil && acquisition.Error != nil {
				err = acquisition.Error
			}
		}
		if o != nil {
			o.acquisition = acquisition
		}
		var receiptErr error
		if !acquisition.Attempted || !acquisition.Returned || acquisition.Generation == 0 {
			receiptErr = errors.New("original audit acquisition receipt is incomplete")
			resourceFailure(receiptErr, "FLOCK_ACQUISITION")
		}
		if o != nil {
			o.lockReturned = true
			o.view.LockAcquisitionDefined = acquisition.Attempted && acquisition.Returned
		}
		if acquisition.Attempted || delivery != nil {
			defer func() {
				release, closeErr := lk.ReleaseObserved()
				if delivery != nil {
					delivery.release = release
				}
				if o != nil {
					o.release = release
					o.releaseReturned = true
				}
				resourceFailure(closeErr, "FLOCK_CLOSE")
				// Preserve the actual independent outcomes even if a provider's
				// aggregate error omitted one of its acquired failure causes.
				delivery.add(release.Error)
				delivery.add(release.UnlockError)
				delivery.add(release.FileCloseError)
				valid := release.Generation == acquisition.Generation && release.AcquisitionAttempted && release.AcquisitionReturned && release.AllocatedFilesClosed && !release.Pending && release.Error == nil && release.FileCloseError == nil && release.UnlockError == nil && (!acquisition.Held || release.Complete)
				if delivery != nil && (!valid || closeErr != nil || receiptErr != nil) {
					delivery.pendingLock = lk
				}
				if !valid {
					resourceFailure(errors.New("original audit flock closure incomplete"), "FLOCK_CLOSE")
				}
			}()
		} else {
			resourceFailure(errors.New("original audit lock acquisition has no receipt"), "FLOCK_ACQUISITION")
		}
		if err != nil {
			return fmt.Errorf("hub-mcp log flock: %w", err)
		}
		if receiptErr != nil {
			if o != nil {
				return o.resourceErr
			}
			return receiptErr
		}
		if !acquisition.Held {
			return errors.New("original audit log flock was not acquired")
		}
	}
	if o != nil {
		if err := o.boundary(ctx, "rotation"); err != nil {
			return err
		}
	}
	size, sizeErr := e.stat(path)
	if o != nil {
		o.view.RotationSizeDefined = sizeErr == nil
		o.view.RotationSizeError = hubMcpLogFailure(sizeErr, "STAT")
	}
	if sizeErr == nil && size >= HubMcpLogRotateSizeBytes {
		if err := rotateHubMcpLogFileCore(path, o, e, ctx); err != nil {
			return err
		}
	}
	return appendHubMcpLogLineUnlockedCore(path, line, o, e, ctx, delivery)
}
func rotateHubMcpLogFileCore(path string, o *HubMcpLogObservation, e hubMcpLogEffects, ctx context.Context) error {
	if o != nil {
		if err := o.boundary(ctx, "rotation-remove"); err != nil {
			return err
		}
	}
	if o != nil {
		o.view.RotationAttempted = true
	}
	target := path + hubMcpLogRotatedSuffix
	removeErr := e.remove(target)
	if o != nil {
		o.view.RotationRemoveError = hubMcpLogFailure(removeErr, "REMOVE")
	}
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return fmt.Errorf("remove existing rotated %s: %w", target, removeErr)
	}
	if o != nil {
		if err := o.boundary(ctx, "rotation-rename"); err != nil {
			return err
		}
	}
	renameErr := e.rename(path, target)
	if o != nil {
		o.view.RotationRenameError = hubMcpLogFailure(renameErr, "RENAME")
	}
	if renameErr != nil {
		return fmt.Errorf("rotate %s -> %s: %w", path, target, renameErr)
	}
	return nil
}
func appendHubMcpLogLineUnlockedCore(path string, line []byte, o *HubMcpLogObservation, e hubMcpLogEffects, ctx context.Context, delivery *hubMcpLogDelivery) (retErr error) {
	if o != nil {
		if err := o.boundary(ctx, "open"); err != nil {
			return err
		}
	}
	f, err := e.open(path)
	if f != nil {
		if o != nil {
			o.view.FileAcquired = true
		}
		defer func() {
			if o != nil {
				o.fileCloseAttempted = true
			}
			closeErr := f.Close()
			if delivery != nil && closeErr != nil {
				delivery.pendingFile = f
			}
			if o != nil {
				o.fileCloseReturned = true
				o.resourceFailure(closeErr, "FILE_CLOSE")
			}
			delivery.add(closeErr)
		}()
	}
	if err != nil {
		return fmt.Errorf("open hub-mcp log %s: %w", path, err)
	}
	if f == nil {
		if o != nil {
			o.resourceFailure(errors.New("successful audit open returned no file"), "FILE_ACQUISITION")
		}
		return errors.New("successful audit open returned no file")
	}
	if o != nil {
		if err := o.boundary(ctx, "chmod"); err != nil {
			return err
		}
		o.view.ChmodAttempted = true
	}
	if err := e.chmod(path); err != nil {
		return fmt.Errorf("chmod hub-mcp log %s: %w", path, err)
	}
	if o != nil {
		if err := o.boundary(ctx, "write"); err != nil {
			return err
		}
		o.view.WriteAttempted = true
	}
	payload := append(line, '\n')
	n, writeErr := f.Write(payload)
	if (o != nil || delivery != nil) && writeErr == nil && n != len(payload) {
		writeErr = io.ErrShortWrite
	}
	if o != nil {
		o.view.WriteComplete = writeErr == nil
	}
	if writeErr != nil {
		return fmt.Errorf("write hub-mcp log %s: %w", path, writeErr)
	}
	if o != nil {
		if err := o.boundary(ctx, "sync"); err != nil {
			return err
		}
		o.view.SyncAttempted = true
	}
	syncErr := f.Sync()
	delivery.add(syncErr)
	if o != nil {
		o.view.SyncError = hubMcpLogFailure(syncErr, "SYNC")
	}
	return nil
}

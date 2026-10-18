package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mcp-local-hub/internal/clients"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
)

// This input binds one positively requested original operation. Its flags
// cannot issue a read, namespace, writer or lease capability.
type SupervisorFileMutationPrecondition struct {
	NamespaceSource   *SupervisorStateNamespaceObservation `json:"-"`
	Apply             bool                                 `json:"apply"`
	SourceOperationID string                               `json:"source_operation_id"`
	RequestSHA256     string                               `json:"request_sha256"`
	NamespaceSHA256   string                               `json:"namespace_sha256"`
	BeforeDefined     bool                                 `json:"before_defined"`
	BeforeSHA256      *string                              `json:"before_sha256"`
}

type SupervisorFileMutationSnapshot struct {
	Schema               string             `json:"schema"`
	Version              uint32             `json:"version"`
	Kind                 string             `json:"kind"`
	Stage                string             `json:"stage"`
	Apply                bool               `json:"apply"`
	BodyEntered          bool               `json:"body_entered"`
	Changed              bool               `json:"changed"`
	BeforeDefined        bool               `json:"before_defined"`
	BeforeSHA256         *string            `json:"before_sha256"`
	IntendedAfterSHA256  *string            `json:"intended_after_sha256"`
	ReceivedAfterDefined bool               `json:"received_after_defined"`
	ReceivedAfterSHA256  *string            `json:"received_after_sha256"`
	Publication          string             `json:"publication"`
	ReaderComplete       bool               `json:"reader_complete"`
	WriterComplete       bool               `json:"writer_complete"`
	LockComplete         bool               `json:"lock_complete"`
	Complete             bool               `json:"complete"`
	FirstCause           *StopBatchCauseV2  `json:"first_cause"`
	CleanupFailures      []StopBatchCauseV2 `json:"cleanup_failures"`
}

type supervisorMutationIssuer struct{ marker byte }

// Only this mutation's finalizer constructs the observation. Every positive
// receive depends on the actual opaque reader/writer/lease/namespace owners.
type SupervisorFileMutationObservation struct {
	mu                sync.Mutex
	issuer            *supervisorMutationIssuer
	input             SupervisorFileMutationPrecondition
	ctx               context.Context
	deadline          time.Time
	path              string
	cap               int64
	role              SupervisorStateLeafRole
	view              SupervisorFileMutationSnapshot
	reader            *StateFileReadObservation
	readerValidated   bool
	beforeRaw         []byte
	lease             *StateFileObservedLease
	borrowedLease     bool
	admissionOnly     bool
	leaseClose        *StateFileLeaseCloseObservation
	capture           *clients.ConfigWriteCaptureTransaction
	request           clients.ConfigWriteRequest
	writer            *StateFilePublicationObservation
	namespaceEvidence *StateFileFreshCreationEvidence
	namespaceClose    *StateFileFreshNamespaceCloseObservation
	finalized         bool
	err               error
	closeReceiveErr   error
	restoration       *SupervisorIntentAbsenceRestorationObservation
}

func MutateSupervisorStateObserved(ctx context.Context, path string, input SupervisorFileMutationPrecondition, mutate func(*SupervisorStateFile) (bool, error)) (*SupervisorFileMutationObservation, error) {
	return mutateSupervisorFileObserved(ctx, path, input, "state", SupervisorStateLeaf, func(defined bool, raw []byte) ([]byte, bool, error) {
		file := &SupervisorStateFile{Version: 1}
		if defined {
			if err := json.Unmarshal(raw, file); err != nil {
				return nil, false, fmt.Errorf("decode supervisor state: %w", err)
			}
		}
		return prepareSupervisorStateMutation(file, mutate)
	})
}

func MutateSupervisorIntentObserved(ctx context.Context, path string, input SupervisorFileMutationPrecondition, mutate func(*SupervisorIntentFile) (bool, error)) (*SupervisorFileMutationObservation, error) {
	return mutateSupervisorFileObserved(ctx, path, input, "intent", SupervisorIntentLeaf, prepareObservedSupervisorIntent(path, mutate))
}

func MutateSupervisorIntentLockHeldObserved(ctx context.Context, path string, input SupervisorFileMutationPrecondition, lease *StateFileObservedLease, mutate func(*SupervisorIntentFile) (bool, error)) (*SupervisorFileMutationObservation, error) {
	if lease == nil {
		return nil, errors.New("original held supervisor intent lease required")
	}
	return mutateSupervisorFileObservedWithLease(ctx, path, input, "intent", SupervisorIntentLeaf, prepareObservedSupervisorIntent(path, mutate), lease)
}

func prepareObservedSupervisorIntent(path string, mutate func(*SupervisorIntentFile) (bool, error)) func(bool, []byte) ([]byte, bool, error) {
	return func(defined bool, raw []byte) ([]byte, bool, error) {
		file := &SupervisorIntentFile{Version: 1}
		if defined {
			var err error
			file, _, err = decodeSupervisorIntentFile(path, raw)
			if err != nil {
				return nil, false, err
			}
		}
		file, changed, err := prepareSupervisorIntentMutation(file, mutate)
		if err != nil || !changed {
			return nil, changed, err
		}
		raw, err = marshalNextSupervisorIntent(file)
		return raw, true, err
	}
}

func copySupervisorMutationInput(input SupervisorFileMutationPrecondition) SupervisorFileMutationPrecondition {
	if input.BeforeSHA256 != nil {
		digest := *input.BeforeSHA256
		input.BeforeSHA256 = &digest
	}
	return input
}

func mutateSupervisorFileObserved(ctx context.Context, path string, input SupervisorFileMutationPrecondition, kind string, role SupervisorStateLeafRole, prepare func(bool, []byte) ([]byte, bool, error)) (observation *SupervisorFileMutationObservation, retErr error) {
	return mutateSupervisorFileObservedWithLease(ctx, path, input, kind, role, prepare, nil)
}

func mutateSupervisorFileObservedWithLease(ctx context.Context, path string, input SupervisorFileMutationPrecondition, kind string, role SupervisorStateLeafRole, prepare func(bool, []byte) ([]byte, bool, error), heldLease *StateFileObservedLease) (observation *SupervisorFileMutationObservation, retErr error) {
	return mutateSupervisorFileObservedMode(ctx, path, input, kind, role, prepare, heldLease, false)
}

func mutateSupervisorFileObservedMode(ctx context.Context, path string, input SupervisorFileMutationPrecondition, kind string, role SupervisorStateLeafRole, prepare func(bool, []byte) ([]byte, bool, error), heldLease *StateFileObservedLease, admissionOnly bool) (observation *SupervisorFileMutationObservation, retErr error) {
	observation = &SupervisorFileMutationObservation{issuer: &supervisorMutationIssuer{marker: 1}, input: copySupervisorMutationInput(input), ctx: ctx, path: path, cap: stateFileReadCapBytes(path), role: role, view: SupervisorFileMutationSnapshot{Schema: "supervisor-file-mutation-observation-v1", Version: 1, Kind: kind, Stage: "input", Apply: input.Apply, Publication: "not_attempted", CleanupFailures: []StopBatchCauseV2{}}}
	observation.admissionOnly = admissionOnly
	defer func() {
		observation.err = retErr
		observation.finalized = true
		if retErr != nil && observation.view.FirstCause == nil {
			observation.view.FirstCause = &StopBatchCauseV2{Code: "SUPERVISOR_MUTATION_FAILED", Stage: observation.view.Stage}
		}
	}()
	if err := validateSupervisorFileMutationPrecondition(input); err != nil {
		return observation, err
	}
	if ctx == nil || prepare == nil || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return observation, errors.New("original supervisor mutation context/path required")
	}
	deadline, finite := ctx.Deadline()
	if !finite {
		return observation, errors.New("original supervisor mutation deadline required")
	}
	observation.deadline = deadline
	if err := ctx.Err(); err != nil {
		return observation, err
	}
	observation.view.Stage = "namespace"
	if input.NamespaceSource == nil {
		return observation, errors.New("original supervisor namespace observation required")
	}
	if err := input.NamespaceSource.ValidateOperation(ctx, input.SourceOperationID, input.RequestSHA256); err != nil {
		return observation, err
	}
	if err := input.NamespaceSource.ValidateNamespace(ctx, path, role, input.NamespaceSHA256); err != nil {
		return observation, err
	}
	// This entered namespace borrow protects the same acquisition while the
	// file lease runs. Its owner closes it outside this exclusion afterwards.
	execute := func() (bodyErr error) {
		defer func() {
			if observation.capture != nil {
				proof, closeErr := observation.capture.CloseObserved()
				bodyErr = errors.Join(bodyErr, closeErr)
				if closeErr != nil {
					observation.view.CleanupFailures = append(observation.view.CleanupFailures, StopBatchCauseV2{Code: "CAPTURE_CLOSE_FAILED", Stage: "close"})
				}
				if observation.writer != nil && observation.request.ExpectedSnapshot != nil {
					receiveErr := observation.writer.ReceiveBorrowedParentClose(proof)
					bodyErr = errors.Join(bodyErr, receiveErr)
					if receiveErr != nil {
						observation.view.CleanupFailures = append(observation.view.CleanupFailures, StopBatchCauseV2{Code: "CAPTURE_CLOSE_RECEIVE_FAILED", Stage: "close"})
					}
				}
			}
			if observation.lease != nil && !observation.borrowedLease {
				proof, closeErr := observation.lease.CloseObserved()
				observation.leaseClose = proof
				bodyErr = errors.Join(bodyErr, closeErr)
				if closeErr != nil {
					observation.view.CleanupFailures = append(observation.view.CleanupFailures, StopBatchCauseV2{Code: "LEASE_CLOSE_FAILED", Stage: "close"})
				}
			}
			bodyErr = errors.Join(bodyErr, ctx.Err())
		}()
		observation.view.Stage = "acquire"
		if heldLease != nil {
			observation.lease = heldLease
			observation.borrowedLease = true
		} else {
			var acquireErr error
			observation.lease, acquireErr = AcquireStateFileObservedLease(ctx, path)
			if acquireErr != nil {
				return acquireErr
			}
		}
		if err := observation.lease.ValidateHeld(ctx, path); err != nil {
			return err
		}
		observation.view.Stage = "read"
		raw, reader, readErr := ReadStateFileInodeAnchoredObserved(ctx, path, observation.cap)
		observation.reader = reader
		if reader == nil {
			return errors.Join(readErr, errors.New("original supervisor read observation missing"))
		}
		if input.BeforeDefined {
			if readErr != nil {
				return readErr
			}
			if err := reader.Validate(ctx, path, observation.cap, raw, *input.BeforeSHA256); err != nil {
				return err
			}
			observation.beforeRaw = append([]byte(nil), raw...)
			observation.view.BeforeDefined = true
			digest := *input.BeforeSHA256
			observation.view.BeforeSHA256 = &digest
		} else {
			if err := reader.ValidateAbsence(ctx, path, StateFileReadLeafNotFound); err != nil {
				return errors.Join(readErr, err)
			}
		}
		observation.readerValidated = true
		if err := input.NamespaceSource.ValidateOperation(ctx, input.SourceOperationID, input.RequestSHA256); err != nil {
			return err
		}
		if err := input.NamespaceSource.ValidateNamespace(ctx, path, role, input.NamespaceSHA256); err != nil {
			return err
		}
		observation.view.Stage = "mutate"
		observation.view.BodyEntered = true
		after, changed, mutateErr := prepare(input.BeforeDefined, raw)
		observation.view.Changed = changed
		if mutateErr != nil {
			return mutateErr
		}
		if !changed {
			observation.view.Stage = "noop"
			observation.view.IntendedAfterSHA256 = observation.view.BeforeSHA256
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// Use the existing reader owner's ceiling: publishing a larger image
		// would make the same canonical state unreadable on its next receive.
		if len(after) == 0 || int64(len(after)) > observation.cap {
			return errors.New("supervisor mutation output exceeds its existing state read ceiling")
		}
		afterSHA := preparedSerenaHash(after)
		observation.view.IntendedAfterSHA256 = &afterSHA
		if admissionOnly {
			observation.view.Stage = "canonical-admission"
			return nil
		}
		observation.request = clients.ConfigWriteRequest{Context: ctx, Path: path, Contents: after}
		if input.BeforeDefined {
			observation.view.Stage = "capture"
			var captureErr error
			observation.capture, captureErr = clients.CaptureStateFileWriteSnapshotObserved(ctx, path, observation.cap, *input.BeforeSHA256)
			if captureErr != nil {
				return captureErr
			}
			if observation.capture == nil {
				return errors.New("original supervisor CAS transaction missing")
			}
			observation.request.ExpectedSnapshot = observation.capture.Snapshot()
			if observation.request.ExpectedSnapshot == nil {
				return errors.New("original supervisor CAS snapshot missing")
			}
			if err := observation.request.ExpectedSnapshot.ValidateObservedCapture(ctx, path); err != nil {
				return err
			}
		} else {
			var freshErr error
			observation.namespaceEvidence, freshErr = NewStateFileFreshCreationEvidence(ctx, input.NamespaceSource, path, string(role))
			if freshErr != nil {
				return freshErr
			}
			observation.request.FreshCreationEvidence = observation.namespaceEvidence
		}
		observation.view.Stage = "write"
		// Once entered, any incomplete writer/readback/close is pending, even
		// when bytes may already have committed. No failed-write guess is made.
		observation.view.Publication = "pending"
		var writeErr error
		observation.writer, writeErr = WriteStateFileBytesLockHeldObserved(observation.request, observation.lease)
		if observation.writer == nil {
			return errors.Join(writeErr, errors.New("original supervisor writer observation missing"))
		}
		return writeErr
	}
	retErr = input.NamespaceSource.borrowStateFileFreshNamespace(ctx, func() error {
		if heldLease != nil {
			return heldLease.Borrow(ctx, path, execute)
		}
		return execute()
	})
	return observation, retErr
}

func (observation *SupervisorFileMutationObservation) ReceiveLeaseClose(proof *StateFileLeaseCloseObservation) error {
	if observation == nil {
		return errors.New("original supervisor mutation missing")
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	if !observation.finalized || observation.lease == nil {
		return errors.New("original returned supervisor lease missing")
	}
	if observation.leaseClose != nil && observation.leaseClose != proof {
		return errors.New("supervisor mutation lease close substituted")
	}
	if err := proof.Validate(observation.lease); err != nil {
		observation.closeReceiveErr = errors.Join(observation.closeReceiveErr, err)
		return err
	}
	observation.leaseClose = proof
	observation.closeReceiveErr = errors.Join(observation.closeReceiveErr, observation.ctx.Err())
	return observation.closeReceiveErr
}

// The acquiring composition supplies its one original namespace close after
// every file lease has returned. This receives proof; it never closes/reopens.
func (observation *SupervisorFileMutationObservation) ReceiveNamespaceClose(proof *StateFileFreshNamespaceCloseObservation) error {
	if observation == nil {
		return errors.New("original supervisor mutation observation required")
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	if observation.issuer == nil || !observation.finalized || observation.input.NamespaceSource == nil {
		return errors.New("original finalized supervisor namespace binding required")
	}
	if observation.namespaceClose != nil {
		if observation.namespaceClose != proof {
			return errors.New("supervisor mutation namespace close substituted")
		}
		return observation.closeReceiveErr
	}
	if err := proof.ValidateSource(observation.input.NamespaceSource); err != nil {
		return err
	}
	observation.namespaceClose = proof
	if observation.writer != nil && observation.request.FreshCreationEvidence != nil {
		observation.closeReceiveErr = observation.writer.ReceiveFreshNamespaceClose(proof)
	}
	observation.closeReceiveErr = errors.Join(observation.closeReceiveErr, observation.ctx.Err())
	return observation.closeReceiveErr
}

func (observation *SupervisorFileMutationObservation) validateReaderLocked(ctx context.Context) error {
	if observation.reader == nil || !observation.readerValidated {
		return errors.New("qualified original supervisor reader required")
	}
	if observation.input.BeforeDefined {
		return observation.reader.Validate(ctx, observation.path, observation.cap, observation.beforeRaw, *observation.input.BeforeSHA256)
	}
	return observation.reader.ValidateAbsence(ctx, observation.path, StateFileReadLeafNotFound)
}

func (observation *SupervisorFileMutationObservation) validateLocked(ctx context.Context, input SupervisorFileMutationPrecondition, expectedAfterSHA string) error {
	return observation.validateOutcomeLocked(ctx, input, expectedAfterSHA, nil)
}

func (observation *SupervisorFileMutationObservation) validateOutcomeLocked(ctx context.Context, input SupervisorFileMutationPrecondition, expectedAfterSHA string, heldLease *StateFileObservedLease) error {
	if observation.issuer == nil || !observation.finalized || ctx == nil || observation.err != nil || observation.closeReceiveErr != nil {
		return errors.Join(errors.New("original supervisor mutation incomplete"), observation.err, observation.closeReceiveErr)
	}
	deadline, finite := ctx.Deadline()
	if !finite || deadline.After(observation.deadline) || !time.Now().Before(observation.deadline) {
		return errors.New("original supervisor mutation deadline expired or renewed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateSupervisorFileMutationPrecondition(input); err != nil {
		return err
	}
	if input.NamespaceSource != observation.input.NamespaceSource || input.SourceOperationID != observation.input.SourceOperationID || input.RequestSHA256 != observation.input.RequestSHA256 || input.NamespaceSHA256 != observation.input.NamespaceSHA256 || input.BeforeDefined != observation.input.BeforeDefined || !reflect.DeepEqual(input.BeforeSHA256, observation.input.BeforeSHA256) {
		return errors.New("supervisor mutation original precondition differs")
	}
	if heldLease != nil {
		if !observation.borrowedLease || heldLease != observation.lease {
			return errors.New("original held supervisor lease differs")
		}
		if err := heldLease.ValidateHeld(ctx, observation.path); err != nil {
			return err
		}
		if err := input.NamespaceSource.ValidateOperation(ctx, input.SourceOperationID, input.RequestSHA256); err != nil {
			return err
		}
		if err := input.NamespaceSource.ValidateNamespace(ctx, observation.path, observation.role, input.NamespaceSHA256); err != nil {
			return err
		}
	} else {
		if observation.namespaceClose.ValidateSource(observation.input.NamespaceSource) != nil {
			return errors.New("original supervisor namespace close unreceived")
		}
		if err := observation.leaseClose.Validate(observation.lease); err != nil {
			return err
		}
	}
	if err := observation.validateReaderLocked(ctx); err != nil {
		return err
	}
	if !observation.view.BodyEntered {
		return errors.New("original supervisor mutation body unentered")
	}
	if observation.admissionOnly {
		if heldLease == nil || observation.view.IntendedAfterSHA256 == nil || expectedAfterSHA != *observation.view.IntendedAfterSHA256 {
			return errors.New("canonical admission is not publication")
		}
		return nil
	}
	if !observation.view.Changed {
		if observation.input.BeforeDefined {
			if expectedAfterSHA != *observation.input.BeforeSHA256 {
				return errors.New("supervisor mutation no-op content differs")
			}
		} else if expectedAfterSHA != "" {
			return errors.New("supervisor mutation no-op absence differs")
		}
		return nil
	}
	if observation.writer == nil || observation.view.IntendedAfterSHA256 == nil || expectedAfterSHA != *observation.view.IntendedAfterSHA256 {
		return errors.New("original supervisor writer binding missing or different")
	}
	if heldLease != nil {
		return observation.writer.validateHeldProgress(ctx, observation.request, expectedAfterSHA, heldLease, input.NamespaceSource, string(observation.role))
	}
	return observation.writer.Validate(ctx, observation.request, expectedAfterSHA)
}

func (observation *SupervisorFileMutationObservation) ValidateHeldProgress(ctx context.Context, input SupervisorFileMutationPrecondition, expectedAfterSHA string, lease *StateFileObservedLease) error {
	if observation == nil || lease == nil {
		return errors.New("original held supervisor mutation missing")
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	return observation.validateOutcomeLocked(ctx, input, expectedAfterSHA, lease)
}

func preflightPreparedSupervisorIntentLockHeld(ctx context.Context, path string, input SupervisorFileMutationPrecondition, lease *StateFileObservedLease, desired *SupervisorIntentFile) error {
	if lease == nil || desired == nil {
		return errors.New("prepared canonical intent admission missing")
	}
	prepare := prepareObservedSupervisorIntent(path, func(current *SupervisorIntentFile) (bool, error) {
		replacement := cloneSupervisorIntentFile(desired)
		replacement.IntentGeneration = current.IntentGeneration
		*current = *replacement
		return true, nil
	})
	o, err := mutateSupervisorFileObservedMode(ctx, path, input, "intent", SupervisorIntentLeaf, prepare, lease, true)
	if err != nil {
		return err
	}
	if o == nil || o.view.IntendedAfterSHA256 == nil {
		return errors.New("prepared canonical intent admission undefined")
	}
	return o.ValidateHeldProgress(ctx, input, *o.view.IntendedAfterSHA256, lease)
}

func (observation *SupervisorFileMutationObservation) Validate(ctx context.Context, input SupervisorFileMutationPrecondition, expectedAfterSHA string) error {
	if observation == nil {
		return errors.New("original supervisor mutation observation required")
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	return observation.validateLocked(ctx, input, expectedAfterSHA)
}

func (observation *SupervisorFileMutationObservation) Snapshot() SupervisorFileMutationSnapshot {
	if observation == nil {
		return SupervisorFileMutationSnapshot{}
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	view := observation.view
	view.ReaderComplete = observation.readerValidated
	view.LockComplete = observation.leaseClose.Validate(observation.lease) == nil
	view.WriterComplete = observation.view.BodyEntered && !observation.view.Changed && observation.err == nil
	if observation.writer != nil {
		view.WriterComplete = observation.writer.Complete()
	}
	expected := ""
	if view.IntendedAfterSHA256 != nil {
		expected = *view.IntendedAfterSHA256
	}
	view.Complete = observation.validateLocked(observation.ctx, observation.input, expected) == nil
	if view.Complete {
		view.Stage = "complete"
		view.ReceivedAfterDefined = observation.input.BeforeDefined || observation.view.Changed
		if view.ReceivedAfterDefined {
			view.ReceivedAfterSHA256 = &expected
		}
		if observation.view.Changed {
			view.Publication = "received"
		}
	}
	copyDigest := func(value *string) *string {
		if value == nil {
			return nil
		}
		copyValue := *value
		return &copyValue
	}
	view.BeforeSHA256 = copyDigest(view.BeforeSHA256)
	view.IntendedAfterSHA256 = copyDigest(view.IntendedAfterSHA256)
	view.ReceivedAfterSHA256 = copyDigest(view.ReceivedAfterSHA256)
	view.CleanupFailures = append([]StopBatchCauseV2{}, view.CleanupFailures...)
	if view.FirstCause != nil {
		cause := *view.FirstCause
		view.FirstCause = &cause
	}
	return view
}

func validateSupervisorFileMutationPrecondition(input SupervisorFileMutationPrecondition) error {
	if !input.Apply || strings.TrimSpace(input.SourceOperationID) == "" || !stopBatchV2SHA256(input.RequestSHA256) || !stopBatchV2SHA256(input.NamespaceSHA256) {
		return fmt.Errorf("observed supervisor mutation requires positive original operation bindings")
	}
	if input.BeforeDefined != (input.BeforeSHA256 != nil) || (input.BeforeSHA256 != nil && !stopBatchV2SHA256(*input.BeforeSHA256)) {
		return fmt.Errorf("observed supervisor mutation prior definition/digest differs")
	}
	return nil
}

// Decode preserves explicit false/null presence. An unknown original absence
// still refuses at the actual reader; decoding this input grants no action.
func DecodeSupervisorFileMutationPrecondition(raw []byte) (SupervisorFileMutationPrecondition, error) {
	var input SupervisorFileMutationPrecondition
	if err := rejectStopBatchV2DuplicateKeys(raw); err != nil {
		return input, err
	}
	if err := requireStopBatchV2ExactKeys(raw, reflect.TypeOf(input)); err != nil {
		return input, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return input, err
	}
	if err := requireStopBatchV2Fields(object, "apply", "source_operation_id", "request_sha256", "namespace_sha256", "before_defined"); err != nil {
		return input, err
	}
	if _, known := object["before_sha256"]; !known {
		return input, fmt.Errorf("observed supervisor mutation missing prior digest observation")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, err
	}
	return input, validateSupervisorFileMutationPrecondition(input)
}

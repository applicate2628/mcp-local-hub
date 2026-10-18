//go:build windows

package api

import (
	"errors"
	"golang.org/x/sys/windows"
	"mcp-local-hub/internal/clients"
	"path/filepath"
)

func WriteStateFileBytesAtomicObserved(req clients.ConfigWriteRequest) (observation *StateFilePublicationObservation, retErr error) {
	lease, acquireErr := AcquireStateFileObservedLease(req.Context, req.Path)
	if acquireErr != nil {
		observation = &StateFilePublicationObservation{capture: newStateFileIOCapture(), request: req, path: req.Path, sha256: preparedSerenaHash(req.Contents), operation: "write", lease: lease, err: acquireErr}
		if lease != nil {
			_, closeErr := lease.CloseObserved()
			retErr = errors.Join(acquireErr, closeErr)
		} else {
			retErr = acquireErr
		}
		return observation, retErr
	}
	observation, retErr = WriteStateFileBytesLockHeldObserved(req, lease)
	_, closeErr := lease.CloseObserved()
	retErr = errors.Join(retErr, closeErr)
	if observation != nil {
		observation.err = errors.Join(observation.err, closeErr)
	}
	return observation, retErr
}
func WriteStateFileBytesLockHeldObserved(req clients.ConfigWriteRequest, lease *StateFileObservedLease) (observation *StateFilePublicationObservation, retErr error) {
	capture := newStateFileIOCapture()
	observation = &StateFilePublicationObservation{capture: capture, request: req, path: req.Path, sha256: preparedSerenaHash(req.Contents), operation: "write", lease: lease}
	observation.request.Contents = append([]byte(nil), req.Contents...)
	defer func() {
		observation.err = retErr
		observation.finishedInBound = req.Context != nil && req.Context.Err() == nil
	}()
	if req.Context == nil || !filepath.IsAbs(req.Path) || filepath.Clean(req.Path) != req.Path || len(req.Contents) == 0 || lease == nil {
		return observation, errors.New("STATE_OBSERVED_WRITE_INVALID")
	}
	if _, ok := req.Context.Deadline(); !ok {
		return observation, errors.New("STATE_OBSERVED_DEADLINE_REQUIRED")
	}
	receiveReadback := func() error {
		observation.readbackAttempted = true
		var captureLeaf func(uintptr, []byte) error
		if observation.fresh != nil && observation.fresh.role == string(SupervisorIntentLeaf) {
			if _, ok := observation.fresh.source.(*SupervisorStateNamespaceObservation); !ok {
				return errors.New("STATE_INTENT_FRESH_SOURCE_MISMATCH")
			}
			captureLeaf = func(handle uintptr, raw []byte) error {
				object, err := serenaTreeHandleIdentity(windows.Handle(handle), nil)
				if err != nil {
					return err
				}
				if object.Directory || object.Links != 1 || object.Bytes != int64(len(raw)) || object.Device == "" || object.FileID == "" {
					return errors.New("STATE_INTENT_PUBLISHED_OBJECT_UNQUALIFIED")
				}
				observation.publishedObject = &stateFilePublishedObject{writer: observation, object: object, sha256: preparedSerenaHash(raw)}
				return nil
			}
		}
		raw, readback, readErr := readStateFileInodeAnchoredObservedWithLeafCapture(req.Context, req.Path, int64(len(req.Contents)), captureLeaf)
		observation.readback = readback
		observation.readbackRaw = append([]byte(nil), raw...)
		if readErr != nil {
			return readErr
		}
		if readback == nil {
			return errors.New("STATE_OBSERVED_READBACK_UNDEFINED")
		}
		validateErr := readback.Validate(req.Context, req.Path, int64(len(req.Contents)), raw, observation.sha256)
		observation.readbackQualified = validateErr == nil
		return validateErr
	}
	retErr = lease.Borrow(req.Context, req.Path, func() error {
		if req.ExpectedSnapshot != nil && req.FreshCreationEvidence != nil {
			return errors.New("STATE_OBSERVED_WRITE_MODE_CONFLICT")
		}
		if req.ExpectedSnapshot == nil {
			fresh, ok := req.FreshCreationEvidence.(*StateFileFreshCreationEvidence)
			if !ok || fresh == nil {
				return errors.New("STATE_OBSERVED_FRESH_NAMESPACE_CAPABILITY_UNAVAILABLE")
			}
			return fresh.source.borrowStateFileFreshNamespace(req.Context, func() error {
				if err := fresh.ValidateFreshCreation(req.Context, req.Path); err != nil {
					return err
				}
				_, absence, readErr := ReadStateFileInodeAnchoredObserved(req.Context, req.Path, int64(len(req.Contents)))
				if absence == nil || absence.ValidateAbsence(req.Context, req.Path, StateFileReadLeafNotFound) != nil {
					return errors.Join(readErr, errors.New("STATE_OBSERVED_FRESH_ABSENCE_UNQUALIFIED"))
				}
				fresh.lease = lease
				fresh.absence = absence
				observation.fresh = fresh
				if err := secureWriteClientConfigImplObserved(req.Path, req.Contents, false, capture); err != nil {
					return err
				}
				return receiveReadback()
			})
		}
		if err := req.ExpectedSnapshot.ValidateObservedCapture(req.Context, req.Path); err != nil {
			return err
		}
		capture.acquired("borrowed-parent")
		writeErr := secureWriteClientConfigToResolvedParentObserved(windows.Handle(req.ExpectedSnapshot.BorrowedParentHandle()), filepath.Dir(req.Path), filepath.Base(req.Path), req.Contents, false, capture, nil, &req)
		capture.closed("borrowed-parent", errors.New("STATE_OBSERVED_BORROWED_PARENT_CLOSE_PENDING"))
		if writeErr != nil {
			return writeErr
		}
		return receiveReadback()
	})
	return observation, retErr
}

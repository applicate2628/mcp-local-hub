package api

import (
	"context"
	"errors"
	"mcp-local-hub/internal/clients"
	"path/filepath"
	"sync"
	"time"
)

// PreparedSerenaRecordIO adapts the existing protected state writer/reader and
// one-shot lock ledger. Root is injected by the API assembly, never discovered
// from an install subprocess or a workspace. This adapter does not install.
type PreparedSerenaRecordIO struct {
	operationID, sourceSHA256 string
	Root                      string
	IntentPath                string
	namespace                 *SerenaRecordNamespaceObservation
	packageBinding            *serenaPackageNamespaceBinding
	intent                    *preparedSerenaIntentExclusion
	intentNamespace           *SupervisorStateNamespaceObservation
	intentRequestSHA256       string
}

func (io PreparedSerenaRecordIO) bindReadRecordNamespace(ctx context.Context, original *SerenaRecordNamespaceObservation) (PreparedSerenaRecordIO, error) {
	if original == nil || !original.readOnly || original.io.Root != io.Root || original.io.operationID != io.operationID || original.io.sourceSHA256 != io.sourceSHA256 || io.namespace != nil {
		return PreparedSerenaRecordIO{}, errors.New("SERENA_RECORD_READ_SOURCE_MISMATCH")
	}
	if err := original.platform.validate(ctx, original.path); err != nil {
		return PreparedSerenaRecordIO{}, err
	}
	identity, err := original.platform.packageIdentity(ctx)
	if err != nil {
		return PreparedSerenaRecordIO{}, err
	}
	digest, err := original.NamespaceSHA256(ctx)
	if err != nil {
		return PreparedSerenaRecordIO{}, err
	}
	io.namespace = original
	io.packageBinding = &serenaPackageNamespaceBinding{issuer: original, root: io.Root, key: original.key, operationID: io.operationID, sourceEpoch: io.sourceSHA256, digest: digest, identity: identity}
	return io, nil
}

type preparedSerenaIntentExclusion struct {
	mu           sync.Mutex
	acquiring    bool
	lease        *StateFileObservedLease
	publications []*StateFilePublicationObservation
	readers      []*StateFileReadObservation
	cleanupErr   error
}

func NewPreparedSerenaRecordIO(root, intentPath, operationID, sourceSHA256 string) (PreparedSerenaRecordIO, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || !filepath.IsAbs(intentPath) || filepath.Clean(intentPath) != intentPath || !preparedSerenaID(operationID) || !preparedSerenaDigest(sourceSHA256) {
		return PreparedSerenaRecordIO{}, errors.New("SERENA_RECORD_IO_INPUT_INVALID")
	}
	return PreparedSerenaRecordIO{Root: root, IntentPath: intentPath, operationID: operationID, sourceSHA256: sourceSHA256, intent: &preparedSerenaIntentExclusion{}}, nil
}

// BindRecordNamespace receives the original acquisition made before any
// package/state lock. A different operation, parent, or key cannot be rebound.
func (io PreparedSerenaRecordIO) BindRecordNamespace(ctx context.Context, original *SerenaRecordNamespaceObservation) (PreparedSerenaRecordIO, error) {
	if original == nil || original.io.Root != io.Root || original.io.IntentPath != io.IntentPath || original.io.operationID != io.operationID || original.io.sourceSHA256 != io.sourceSHA256 || io.namespace != nil {
		return PreparedSerenaRecordIO{}, errors.New("SERENA_RECORD_NAMESPACE_SOURCE_MISMATCH")
	}
	if err := original.validateStateFileFreshNamespace(ctx, original.path, "prepared-ready"); err != nil {
		return PreparedSerenaRecordIO{}, err
	}
	identity, err := original.platform.packageIdentity(ctx)
	if err != nil {
		return PreparedSerenaRecordIO{}, err
	}
	digest, err := original.NamespaceSHA256(ctx)
	if err != nil {
		return PreparedSerenaRecordIO{}, err
	}
	io.packageBinding = &serenaPackageNamespaceBinding{issuer: original, root: io.Root, key: original.key, operationID: io.operationID, sourceEpoch: io.sourceSHA256, digest: digest, identity: identity}
	io.namespace = original
	return io, nil
}

func (io PreparedSerenaRecordIO) recordPath(key string) (string, error) {
	if !filepath.IsAbs(io.Root) || filepath.Clean(io.Root) != io.Root || !preparedSerenaDigest(key) {
		return "", errors.New("SERENA_PREPARED_NAMESPACE_INVALID")
	}
	return filepath.Join(io.Root, key, "ready.json"), nil
}
func preparedSerenaBoundedLock(ctx context.Context, path string) (func() error, error) {
	if ctx == nil {
		return nil, errors.New("SERENA_PREPARED_CONTEXT_MISSING")
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("SERENA_PREPARED_DEADLINE_MISSING")
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		release, locked, err := tryLockLeafLedgered(path)
		if err != nil {
			return nil, err
		}
		if locked {
			return release, nil
		}
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
func (io PreparedSerenaRecordIO) LockPackage(ctx context.Context, key string) (func() error, error) {
	path, err := io.recordPath(key)
	if err != nil {
		return nil, err
	}
	if io.namespace == nil || io.namespace.key != key {
		return nil, errors.New("SERENA_RECORD_NAMESPACE_NOT_ACQUIRED_BEFORE_LOCK")
	}
	if err = io.namespace.validateStateFileFreshNamespace(ctx, path, "prepared-ready"); err != nil {
		return nil, err
	}
	parent := filepath.Dir(path)
	return preparedSerenaBoundedLock(ctx, filepath.Join(parent, "package.lock"))
}
func (io PreparedSerenaRecordIO) LockIntentOperation(ctx context.Context) (func() error, error) {
	if !filepath.IsAbs(io.IntentPath) || io.intent == nil {
		return nil, errors.New("SERENA_PREPARED_INTENT_PATH_INVALID")
	}
	exists, err := SecureVerifyExistingParentDirForConfigLock(ctx, filepath.Dir(io.IntentPath))
	if err != nil || !exists {
		return nil, errors.Join(err, errors.New("SERENA_PREPARED_INTENT_PARENT_MISSING"))
	}
	io.intent.mu.Lock()
	if io.intent.acquiring || io.intent.lease != nil {
		io.intent.mu.Unlock()
		return nil, errors.New("SERENA_PREPARED_INTENT_EXCLUSION_ACTIVE")
	}
	io.intent.acquiring = true
	io.intent.mu.Unlock()
	lease, acquireErr := AcquireStateFileObservedLease(ctx, io.IntentPath)
	io.intent.mu.Lock()
	io.intent.acquiring = false
	io.intent.lease = lease
	io.intent.mu.Unlock()
	if acquireErr != nil {
		if lease != nil {
			_, closeErr := lease.CloseObserved()
			acquireErr = errors.Join(acquireErr, closeErr)
		}
		return nil, acquireErr
	}
	return func() error {
		_, closeErr := lease.CloseObserved()
		io.intent.mu.Lock()
		publications := append([]*StateFilePublicationObservation(nil), io.intent.publications...)
		io.intent.mu.Unlock()
		for _, publication := range publications {
			if !publication.Complete() {
				closeErr = errors.Join(closeErr, errors.New("SERENA_PREPARED_INTENT_PUBLICATION_UNSETTLED"))
			}
		}
		if closeErr == nil {
			io.intent.mu.Lock()
			if io.intent.lease == lease {
				io.intent.lease = nil
				io.intent.publications = nil
			}
			io.intent.mu.Unlock()
		}
		return closeErr
	}, nil
}
func (io PreparedSerenaRecordIO) ReadReady(ctx context.Context, key string) ([]byte, bool, error) {
	raw, exists, _, err := io.readReadyObserved(ctx, key)
	return raw, exists, err
}
func (io PreparedSerenaRecordIO) readReadyObserved(ctx context.Context, key string) ([]byte, bool, *StateFileReadObservation, error) {
	if ctx == nil {
		return nil, false, nil, errors.New("SERENA_PREPARED_CONTEXT_MISSING")
	}
	path, err := io.recordPath(key)
	if err != nil {
		return nil, false, nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, false, nil, err
	}
	raw, observation, err := ReadStateFileInodeAnchoredObserved(ctx, path, 1048576)
	if err != nil {
		if observation != nil && observation.ValidateAbsence(ctx, path, StateFileReadLeafNotFound) == nil {
			return nil, false, observation, nil
		}
		return nil, false, observation, err
	}
	if err = observation.Validate(ctx, path, 1048576, raw, preparedSerenaHash(raw)); err != nil {
		return nil, false, observation, errors.Join(err, errors.New("SERENA_PREPARED_READY_READER_UNSETTLED"))
	}
	if err = ctx.Err(); err != nil {
		return nil, false, observation, err
	}
	return raw, true, observation, nil
}
func (io PreparedSerenaRecordIO) WriteReady(ctx context.Context, key string, raw []byte) error {
	_, err := io.writeReadyObserved(ctx, key, raw)
	return err
}
func (io PreparedSerenaRecordIO) writeReadyObserved(ctx context.Context, key string, raw []byte) (publication *StateFilePublicationObservation, err error) {
	if ctx == nil {
		return publication, errors.New("SERENA_PREPARED_CONTEXT_MISSING")
	}
	path, err := io.recordPath(key)
	if err != nil {
		return publication, err
	}
	if len(raw) == 0 || len(raw) > 1048576 {
		return publication, errors.New("SERENA_PREPARED_RECORD_SIZE")
	}
	if _, err = decodePreparedSerenaRecord(raw); err != nil {
		return publication, err
	}
	if err = ctx.Err(); err != nil {
		return publication, err
	}
	if io.namespace == nil || io.namespace.key != key {
		return publication, errors.New("SERENA_RECORD_NAMESPACE_NOT_ACQUIRED_BEFORE_LOCK")
	}
	lease, err := AcquireStateFileObservedLease(ctx, path)
	if err != nil {
		if lease != nil {
			_, closeErr := lease.CloseObserved()
			err = errors.Join(err, closeErr)
		}
		return publication, err
	}
	defer func() {
		_, closeErr := lease.CloseObserved()
		err = errors.Join(err, closeErr)
		if publication != nil {
			io.namespace.retainPublication(publication)
			if publication.err != nil || !publication.capture.complete() || !publication.readbackQualified {
				err = errors.Join(err, errors.New("SERENA_PREPARED_READY_WRITER_UNSETTLED"))
			}
		}
	}()
	prior, read, readErr := ReadStateFileInodeAnchoredObserved(ctx, path, 1048576)
	req := clients.ConfigWriteRequest{Context: ctx, Path: path, Contents: raw}
	if readErr != nil {
		if read == nil || read.ValidateAbsence(ctx, path, StateFileReadLeafNotFound) != nil {
			return publication, errors.Join(readErr, errors.New("SERENA_PREPARED_READY_ABSENCE_UNQUALIFIED"))
		}
		req.FreshCreationEvidence, err = NewStateFileFreshCreationEvidence(ctx, io.namespace, path, "prepared-ready")
		if err != nil {
			return publication, err
		}
		publication, err = WriteStateFileBytesLockHeldObserved(req, lease)
		// Namespace ownership outlives both package and state locks. Its
		// owner must receive this publication's final close after exclusion.
		return publication, err
	}
	priorSHA := preparedSerenaHash(prior)
	if err = read.Validate(ctx, path, 1048576, prior, priorSHA); err != nil {
		return publication, err
	}
	transaction, captureErr := clients.CaptureStateFileWriteSnapshotObserved(ctx, path, 1048576, priorSHA)
	if captureErr != nil {
		return publication, captureErr
	}
	req.ExpectedSnapshot = transaction.Snapshot()
	if req.ExpectedSnapshot == nil {
		_, closeErr := transaction.CloseObserved()
		return publication, errors.Join(closeErr, errors.New("SERENA_PREPARED_READY_SNAPSHOT_UNAVAILABLE"))
	}
	var writeErr error
	publication, writeErr = WriteStateFileBytesLockHeldObserved(req, lease)
	parentClose, closeErr := transaction.CloseObserved()
	if publication != nil {
		closeErr = errors.Join(closeErr, publication.ReceiveBorrowedParentClose(parentClose))
	}
	return publication, errors.Join(writeErr, closeErr, ctx.Err())
}

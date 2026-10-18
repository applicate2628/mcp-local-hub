package api

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"time"
)

// All namespaces/read views are acquired before either exclusion. The returned
// observation is useful only after every locally acquired original slot closes.
func (io PreparedSerenaRecordIO) observeColdPublicationReads(ctx context.Context, record PreparedSerenaRuntimeRecord, limits SerenaTreeLimits) (r *PreparedSerenaColdPublicationReadObservation, retErr error) {
	r = &PreparedSerenaColdPublicationReadObservation{record: record, io: io}
	var readyAttempted, operationAttempted, intentAttempted, publishedAttempted, intentLeaseAttempted, packageLeaseAttempted bool
	var originalDeadline time.Time
	defer func() {
		if packageLeaseAttempted {
			if r.packageLease == nil {
				retErr = errors.Join(retErr, errors.New("SERENA_COLD_PACKAGE_ACQUISITION_UNKNOWN"))
			} else {
				var err error
				r.packageClose, err = r.packageLease.CloseObserved()
				retErr = errors.Join(retErr, err, r.packageClose.Validate(r.packageLease))
			}
		}
		if intentLeaseAttempted {
			if r.intentLease == nil {
				retErr = errors.Join(retErr, errors.New("SERENA_COLD_INTENT_ACQUISITION_UNKNOWN"))
			} else {
				var err error
				r.intentClose, err = r.intentLease.CloseObserved()
				retErr = errors.Join(retErr, err, r.intentClose.Validate(r.intentLease))
			}
		}
		if publishedAttempted {
			if r.published == nil {
				retErr = errors.Join(retErr, errors.New("SERENA_COLD_GENERATION_ACQUISITION_UNKNOWN"))
			} else {
				var err error
				r.publishedClose, err = r.published.CloseObserved()
				retErr = errors.Join(retErr, err, r.publishedClose.Validate(r.published))
			}
		}
		if operationAttempted {
			if r.operationNamespace == nil {
				retErr = errors.Join(retErr, errors.New("SERENA_COLD_OPERATION_ACQUISITION_UNKNOWN"))
			} else {
				var err error
				r.operationClose, err = r.operationNamespace.CloseObserved()
				retErr = errors.Join(retErr, err, r.operationClose.ValidateSource(r.operationNamespace))
			}
		}
		if readyAttempted {
			if r.ready == nil {
				retErr = errors.Join(retErr, errors.New("SERENA_COLD_READY_ACQUISITION_UNKNOWN"))
			} else {
				var err error
				r.readyClose, err = r.ready.CloseObserved()
				retErr = errors.Join(retErr, err, r.readyClose.ValidateSource(r.ready))
			}
		}
		if intentAttempted {
			if r.intentNamespace == nil {
				retErr = errors.Join(retErr, errors.New("SERENA_COLD_INTENT_NAMESPACE_UNKNOWN"))
			} else {
				var err error
				r.intentNamespaceClose, err = r.intentNamespace.CloseObserved()
				retErr = errors.Join(retErr, err, r.intentNamespaceClose.ValidateSource(r.intentNamespace))
			}
		}
		if ctx == nil || ctx.Err() != nil || originalDeadline.IsZero() || !time.Now().Before(originalDeadline) {
			retErr = errors.Join(retErr, errors.New("SERENA_COLD_RECEIVING_LATE_OR_CANCELLED"))
		}
		r.err = retErr
		r.closedInBound = retErr == nil
		if retErr == nil {
			retErr = r.validateClosed(record, r.operation)
			r.err = retErr
			r.closedInBound = retErr == nil
		}
	}()
	if ctx == nil || !limits.valid() {
		return r, errors.New("SERENA_COLD_READ_INPUT_MISSING")
	}
	var finite bool
	originalDeadline, finite = ctx.Deadline()
	if !finite || ctx.Err() != nil {
		return r, errors.New("SERENA_COLD_READ_ORIGINAL_DEADLINE_REQUIRED")
	}
	readyPath, err := io.recordPath(record.RequestIdentitySHA256)
	if err != nil {
		return r, err
	}
	readyAttempted = true
	r.ready, err = acquireSerenaRecordNamespaceReadPlatform(ctx, io, record.RequestIdentitySHA256, readyPath)
	if err != nil {
		return r, err
	}
	r.io, err = io.bindReadRecordNamespace(ctx, r.ready)
	if err != nil {
		return r, err
	}
	r.identity = r.io.packageBinding.identity
	common, err := r.ready.NamespaceSHA256(ctx)
	if err != nil || common != record.NamespaceSHA256 {
		return r, errors.Join(err, errors.New("SERENA_COLD_COMMON_NAMESPACE_DRIFT"))
	}
	readyLeaf, err := r.ready.LeafParentSHA256(ctx)
	if err != nil || readyLeaf != record.LeafParentSHA256 {
		return r, errors.Join(err, errors.New("SERENA_COLD_READY_LEAF_DRIFT"))
	}
	operationAttempted = true
	r.operationNamespace, err = r.io.AcquireOperationNamespaceObserved(ctx, record.RequestIdentitySHA256, record.OperationID, false)
	if err != nil {
		return r, err
	}
	requestSHA := preparedSerenaHash([]byte("serena-cold-publication-v1\n" + record.OperationID + "\n" + record.RequestIdentitySHA256))
	intentAttempted = true
	r.intentNamespace, err = AcquireSupervisorStateNamespaceObserved(ctx, SupervisorStateNamespaceRequest{SourceOperationID: record.OperationID, RequestSHA256: requestSHA, SourceEpoch: io.sourceSHA256, LeafRoles: []SupervisorStateLeafRole{SupervisorIntentLeaf}, CreateMissing: false})
	if err != nil {
		return r, err
	}
	publishedAttempted = true
	r.published, err = AcquireSerenaPublishedGenerationObserved(ctx, record, limits)
	if err != nil {
		return r, err
	}
	intentLeaseAttempted = true
	r.intentLease, err = AcquireStateFileObservedLease(ctx, io.IntentPath)
	if err != nil {
		return r, err
	}
	packageLeaseAttempted = true
	r.packageLease, err = AcquireStateFileObservedLease(ctx, filepath.Join(io.Root, record.RequestIdentitySHA256, "package"))
	if err != nil {
		return r, err
	}
	// Actual current ready/operation/generation receiving occurs under these same
	// original exclusions. No namespace acquisition is performed under a lease.
	retErr = r.intentLease.Borrow(ctx, io.IntentPath, func() error {
		return r.packageLease.Borrow(ctx, filepath.Join(io.Root, record.RequestIdentitySHA256, "package"), func() error {
			var reader *StateFileReadObservation
			raw, exists, reader, readErr := r.io.readReadyObserved(ctx, record.RequestIdentitySHA256)
			r.readers = append(r.readers, reader)
			if readErr != nil || !exists {
				return errors.Join(readErr, errors.New("SERENA_COLD_CURRENT_READY_MISSING"))
			}
			actual, decodeErr := decodePreparedSerenaRecord(raw)
			if decodeErr != nil || !reflect.DeepEqual(actual, record) {
				return errors.Join(decodeErr, errors.New("SERENA_COLD_CURRENT_READY_DRIFT"))
			}
			r.readyRaw = append([]byte(nil), raw...)
			operationRaw, operationReader, readErr := ReadStateFileInodeAnchoredObserved(ctx, r.operationNamespace.path, 64<<20)
			r.readers = append(r.readers, operationReader)
			if readErr != nil {
				return readErr
			}
			if operationReader == nil {
				return errors.New("SERENA_COLD_OPERATION_READER_MISSING")
			}
			if err := operationReader.Validate(ctx, r.operationNamespace.path, 64<<20, operationRaw, preparedSerenaHash(operationRaw)); err != nil {
				return err
			}
			operation, decodeErr := decodePreparedSerenaOperation(operationRaw)
			if decodeErr != nil {
				return decodeErr
			}
			if operation.SourceEpoch != io.sourceSHA256 {
				return errors.New("SERENA_COLD_SOURCE_EPOCH_DRIFT")
			}
			leaf, leafErr := r.operationNamespace.LeafParentSHA256(ctx)
			if leafErr != nil || leaf != operation.LeafParentSHA256 {
				return errors.Join(leafErr, errors.New("SERENA_COLD_OPERATION_LEAF_DRIFT"))
			}
			if err := validateColdPublicationData(record, operation, preparedSerenaHash(raw)); err != nil {
				return err
			}
			if err := r.published.ValidatePublished(ctx, record); err != nil {
				return err
			}
			r.operation = operation
			r.operationRaw = append([]byte(nil), operationRaw...)
			return ctx.Err()
		})
	})
	return r, retErr
}

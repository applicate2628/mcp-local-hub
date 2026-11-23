package api

import (
	"context"
	"errors"
	"path/filepath"
)

// This is canonical metadata admission before any owned probe/UV action. It
// neither issues a native ABI qualification nor publishes an intent record.
func preflightSerenaPreparationIntentMetadata(ctx context.Context, resolution *StateDirResolutionObservation, operation, requestSHA, sourceEpoch string) (err error) {
	if resolution == nil {
		return errors.New("SERENA_PREPARATION_METADATA_NAMESPACE_MISMATCH")
	}
	namespace, acquireErr := acquireSupervisorStateNamespaceObserved(ctx, SupervisorStateNamespaceRequest{SourceOperationID: operation, RequestSHA256: requestSHA, SourceEpoch: sourceEpoch, LeafRoles: []SupervisorStateLeafRole{SupervisorIntentLeaf}, CreateMissing: true}, resolution)
	if namespace != nil {
		defer func() {
			proof, closeErr := namespace.CloseObserved()
			err = errors.Join(err, closeErr, proof.ValidateSource(namespace))
		}()
	}
	if errors.Is(acquireErr, errSupervisorNamespaceResolutionMismatch) {
		return errors.Join(errors.New("SERENA_PREPARATION_METADATA_NAMESPACE_MISMATCH"), acquireErr)
	}
	if acquireErr != nil {
		return acquireErr
	}
	if namespace == nil {
		return errors.New("SUPERVISOR_NAMESPACE_SOURCE_UNAVAILABLE")
	}
	path := filepath.Join(namespace.root, supervisorIntentFileLeaf)
	return namespace.borrowStateFileFreshNamespace(ctx, func() (retErr error) {
		lease, leaseErr := AcquireStateFileObservedLease(ctx, path)
		if lease != nil {
			defer func() {
				proof, closeErr := lease.CloseObserved()
				retErr = errors.Join(retErr, closeErr, proof.Validate(lease))
			}()
		}
		if leaseErr != nil {
			return leaseErr
		}
		if lease == nil {
			return errors.New("SERENA_PREPARATION_METADATA_LEASE_MISSING")
		}
		return lease.Borrow(ctx, path, func() error {
			cap := stateFileReadCapBytes(path)
			raw, reader, readErr := readStateFileInodeAnchoredObservedInNamespace(ctx, path, cap, nil, namespace)
			if reader == nil {
				return errors.Join(readErr, errors.New("SERENA_PREPARATION_METADATA_READER_MISSING"))
			}
			if readErr != nil {
				if absenceErr := reader.ValidateAbsence(ctx, path, StateFileReadLeafNotFound); absenceErr != nil {
					return errors.Join(readErr, absenceErr)
				}
				return nil
			}
			if err := reader.Validate(ctx, path, cap, raw, preparedSerenaHash(raw)); err != nil {
				return err
			}
			_, _, err := decodeSupervisorIntentFile(path, raw)
			return err
		})
	})
}

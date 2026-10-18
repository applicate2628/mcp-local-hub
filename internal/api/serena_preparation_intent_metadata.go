package api

import (
	"context"
	"errors"
	"path/filepath"
)

// This is canonical metadata admission before any owned probe/UV action. It
// neither issues a native ABI qualification nor publishes an intent record.
func preflightSerenaPreparationIntentMetadata(ctx context.Context, root, operation, requestSHA, sourceEpoch string) (err error) {
	namespace, acquireErr := AcquireSupervisorStateNamespaceObserved(ctx, SupervisorStateNamespaceRequest{SourceOperationID: operation, RequestSHA256: requestSHA, SourceEpoch: sourceEpoch, LeafRoles: []SupervisorStateLeafRole{SupervisorIntentLeaf}, CreateMissing: true})
	if namespace != nil {
		defer func() {
			proof, closeErr := namespace.CloseObserved()
			err = errors.Join(err, closeErr, proof.ValidateSource(namespace))
		}()
	}
	if acquireErr != nil {
		return acquireErr
	}
	if namespace == nil || namespace.root != root {
		return errors.New("SERENA_PREPARATION_METADATA_NAMESPACE_MISMATCH")
	}
	path := filepath.Join(root, supervisorIntentFileLeaf)
	lease, leaseErr := AcquireStateFileObservedLease(ctx, path)
	if lease != nil {
		defer func() {
			proof, closeErr := lease.CloseObserved()
			err = errors.Join(err, closeErr, proof.Validate(lease))
		}()
	}
	if leaseErr != nil {
		return leaseErr
	}
	if lease == nil {
		return errors.New("SERENA_PREPARATION_METADATA_LEASE_MISSING")
	}
	return namespace.borrowStateFileFreshNamespace(ctx, func() error {
		return lease.Borrow(ctx, path, func() error {
			cap := stateFileReadCapBytes(path)
			raw, reader, readErr := ReadStateFileInodeAnchoredObserved(ctx, path, cap)
			if reader == nil {
				return errors.Join(readErr, errors.New("SERENA_PREPARATION_METADATA_READER_MISSING"))
			}
			if readErr != nil {
				return errors.Join(reader.ValidateAbsence(ctx, path, StateFileReadLeafNotFound))
			}
			if err := reader.Validate(ctx, path, cap, raw, preparedSerenaHash(raw)); err != nil {
				return err
			}
			_, _, err := decodeSupervisorIntentFile(path, raw)
			return err
		})
	})
}

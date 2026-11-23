package api

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"

	"mcp-local-hub/internal/process"
)

type PrivateSerenaPublisherNamespaceSnapshot struct {
	Schema             string                               `json:"schema"`
	StateRoot          StateDirResolutionSnapshotV1         `json:"state_root"`
	Image              PublisherImageSnapshotV1             `json:"image"`
	Publisher          process.PublisherIncarnationRecordV1 `json:"publisher"`
	NamespaceDirectory string                               `json:"namespace_directory"`
	Bound              bool                                 `json:"bound"`
	NamespaceClosed    bool                                 `json:"namespace_closed"`
	Complete           bool                                 `json:"complete"`
	PrimaryError       string                               `json:"primary_error,omitempty"`
	CloseError         string                               `json:"close_error,omitempty"`
	FinalizationError  string                               `json:"finalization_error,omitempty"`
}

// This is closed namespace/publisher evidence only. It carries neither a
// prepared generation nor a consumer/publication capability. On any failure,
// the originating API retains this exact object and its partial originals.
type PrivateSerenaPublisherNamespaceObservation struct {
	owner     *privateSerenaPublisherOwner
	namespace *SerenaRecordNamespaceObservation
	close     *StateFileFreshNamespaceCloseObservation
	capture   *process.PublisherIncarnation
	image     *PublisherImageObservation
	binding   process.PublisherIncarnationBinding
	view      PrivateSerenaPublisherNamespaceSnapshot
	finished  bool
	err       error
}

func (o *PrivateSerenaPublisherNamespaceObservation) Snapshot() PrivateSerenaPublisherNamespaceSnapshot {
	if o == nil {
		return PrivateSerenaPublisherNamespaceSnapshot{}
	}
	v := o.view
	if o.owner != nil && o.owner.root != nil {
		v.StateRoot = o.owner.root.Snapshot()
	}
	return v
}

func (o *PrivateSerenaPublisherNamespaceObservation) Validate(ctx context.Context, a *API) error {
	if o == nil || a == nil || o.owner == nil || o.owner.api != a ||
		a.serenaPrivatePublisher != o.owner || o.owner.namespaceObservation != o ||
		!o.finished || !o.view.Complete || !o.view.Bound || !o.view.NamespaceClosed || o.err != nil {
		return errors.New("SERENA_PRIVATE_PUBLISHER_NAMESPACE_OBSERVATION_UNAVAILABLE")
	}
	if err := o.owner.validateOrigin(ctx); err != nil {
		return err
	}
	if err := o.close.ValidateSource(o.namespace); err != nil {
		return err
	}
	if err := o.capture.ValidateCapture(ctx, o.binding); err != nil {
		return err
	}
	return o.image.Validate(ctx, o.capture, o.binding.SourceEpoch)
}

// ObservePrivateSerenaPublisherNamespace is a single pre-install qualification.
// It obtains its IDs, key, root and source from original owners. It creates only
// the protected record namespace, captures/binds the actual publisher, then
// closes the namespace. It never calls the preparation assembly, acquires a
// generation/attempt, writes Ready/operation/intent, or starts a process.
func (a *API) ObservePrivateSerenaPublisherNamespace(ctx context.Context) (observation *PrivateSerenaPublisherNamespaceObservation, retErr error) {
	if a == nil || a.serenaPrivatePublisher == nil || a.serenaPrivatePublisher.api != a {
		return nil, errors.New("SERENA_PRIVATE_PUBLISHER_API_MISMATCH")
	}
	owner := a.serenaPrivatePublisher
	if err := owner.claimActivity(a, privateSerenaNamespaceActivity); err != nil {
		return nil, err
	}
	work, cancel, err := owner.boundedContext(ctx, a)
	if err != nil {
		return nil, err
	}
	defer cancel()
	observation = &PrivateSerenaPublisherNamespaceObservation{
		owner: owner,
		view: PrivateSerenaPublisherNamespaceSnapshot{
			Schema:    "private-serena-publisher-namespace-v1",
			StateRoot: owner.root.Snapshot(), Image: owner.image.view,
		},
	}
	owner.namespaceObservation = observation
	defer func() {
		if retErr != nil {
			observation.view.PrimaryError = retErr.Error()
		}
		var closeErr error
		if observation.namespace != nil {
			observation.close, closeErr = observation.namespace.CloseObserved()
			closeErr = errors.Join(closeErr, observation.close.ValidateSource(observation.namespace))
			observation.view.NamespaceClosed = closeErr == nil
		}
		if closeErr != nil {
			observation.view.CloseError = closeErr.Error()
		}
		finalErr := owner.validateOrigin(work)
		if observation.image != nil {
			finalErr = errors.Join(finalErr, observation.image.Validate(work, observation.capture, observation.binding.SourceEpoch))
		}
		if finalErr != nil {
			observation.view.FinalizationError = finalErr.Error()
		}
		observation.err = errors.Join(retErr, closeErr, finalErr)
		observation.finished = true
		observation.view.Complete = observation.err == nil && observation.view.Bound && observation.view.NamespaceClosed
		retErr = observation.err
	}()

	ids, err := NewSerenaPreparationIDs(work)
	if err != nil {
		return observation, err
	}
	publisherID, err := newSerenaPublisherOwnerInstanceID(work)
	if err != nil {
		return observation, err
	}
	// This is a diagnostic namespace selector, not a package request or a
	// fabricated Ready identity. The original namespace owner supplies all
	// physical authority after this data selector is computed.
	keyBytes, err := json.Marshal(struct {
		Purpose, OperationID, SourceEpoch, ImageIdentitySHA256 string
	}{"private-publisher-namespace-qualification-v1", ids.OperationID, owner.image.view.SourceEpoch, owner.image.view.ImageIdentitySHA256})
	if err != nil {
		return observation, err
	}
	key := preparedSerenaHash(keyBytes)
	recordIO, err := NewPreparedSerenaRecordIO(owner.rootPath, joinStateFilePath(owner.rootPath, supervisorIntentFileLeaf), ids.OperationID, owner.image.view.SourceEpoch)
	if err != nil {
		return observation, err
	}
	observation.namespace, err = recordIO.AcquireRecordNamespaceObserved(work, key)
	if err != nil {
		return observation, err
	}
	recordIO, err = recordIO.BindRecordNamespace(work, observation.namespace)
	if err != nil {
		return observation, err
	}
	observation.view.NamespaceDirectory = filepath.Join(recordIO.Root, key)
	common, err := observation.namespace.NamespaceSHA256(work)
	if err != nil {
		return observation, err
	}
	observation.binding = process.PublisherIncarnationBinding{
		OwnerInstanceID: publisherID, OperationID: ids.OperationID,
		NamespaceSHA256: common, SourceEpoch: owner.image.view.SourceEpoch,
		ResourceScope: process.PublisherExclusiveLocalResources,
	}
	observation.capture, err = process.CapturePublisherIncarnation(work, observation.binding)
	if err != nil {
		return observation, err
	}
	observation.image, err = owner.captureImage(work, observation.binding, observation.capture, observation.namespace)
	if err != nil {
		return observation, err
	}
	observation.view.Publisher, err = BindPublisherIncarnation(work, observation.binding, observation.capture, observation.image, observation.namespace)
	if err != nil {
		return observation, err
	}
	observation.view.Bound = true
	return observation, nil
}

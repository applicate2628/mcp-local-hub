package api

import "errors"

type preparedSerenaConsumerCoordinator struct {
	root, intentPath, sourceEpoch string
	publication                   *PreparedSerenaPublicationOwner
	runtimeIdentity               SerenaTreeObject
	limits                        SerenaTreeLimits
	recovered                     *SerenaPublicationObservation
}

func (o *PreparedSerenaRuntimeOwner) consumerCoordinator() (*preparedSerenaConsumerCoordinator, error) {
	if o == nil {
		return nil, errors.New("SERENA_CONSUMER_OWNER_MISSING")
	}
	if o.consumer != nil {
		c := o.consumer
		if c.recovered == nil || c.recovered.owner != c.publication || c.recovered.reads == nil || c.recovered.reads.validateClosed(c.recovered.record, c.recovered.operation) != nil {
			return nil, errors.New("SERENA_COLD_COORDINATOR_UNQUALIFIED")
		}
		return c, nil
	}
	original, ok := o.effects.(*preparedSerenaRuntimeEffects)
	if !ok || original == nil || original.publication == nil || original.packageBinding == nil {
		return nil, errors.New("SERENA_CONSUMER_PUBLICATION_ISSUER_MISSING")
	}
	return &preparedSerenaConsumerCoordinator{root: original.Root, intentPath: original.IntentPath, sourceEpoch: original.sourceSHA256, publication: original.publication, runtimeIdentity: original.packageBinding.identity.runtimeRoot, limits: original.acquisition.inputs.Request.OwnedTreeLimits}, nil
}

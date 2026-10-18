package api

import (
	"context"
	"encoding/json"
	"errors"
	"mcp-local-hub/internal/buildinfo"
)

// Values are data; only the original observed readers and sealed source factory
// qualify capture. No Boolean here can assert resource closure.
type SerenaConsumerEnvironmentSourceBinding struct {
	TaskName, RawManifestSHA256 string
	Reads                       []*StateFileReadObservation
}
type serenaConsumerSourceBinding struct {
	task, rawManifestSHA, epoch string
	source                      *buildinfo.PublisherSourceObservation
	reads                       []*StateFileReadObservation
}
type SerenaConsumerEnvironmentObservation struct {
	source                   *serenaPreparationEnvironmentSource
	binding                  *serenaConsumerSourceBinding
	policySHA, projectionSHA string
	projection               SerenaBuildEnvironmentProjectionV1
}

func validateConsumerSourceReads(reads []*StateFileReadObservation) error {
	if len(reads) == 0 {
		return errors.New("SERENA_CONSUMER_SOURCE_READS_UNAVAILABLE")
	}
	for _, r := range reads {
		if r == nil || r.issuer == nil || !r.finalized || !r.view.OwnedResourcesComplete || !r.view.BorrowedResourcesComplete || (!r.view.Complete && !r.absenceQualified) {
			return errors.New("SERENA_CONSUMER_SOURCE_CLOSE_UNQUALIFIED")
		}
	}
	return nil
}

func NewSerenaConsumerEnvironmentObservation(ctx context.Context, source SerenaPreparationEnvironmentSource, rawSHA, task string, policy SerenaPreparationPolicyV1) (*SerenaConsumerEnvironmentObservation, error) {
	s, ok := source.(*serenaPreparationEnvironmentSource)
	if !ok || s == nil || s.issuer == nil || s.consumer == nil || ctx == nil || ctx.Err() != nil || s.consumer.task != task || s.consumer.rawManifestSHA != rawSHA {
		return nil, errors.New("SERENA_CONSUMER_ENVIRONMENT_SOURCE_UNAVAILABLE")
	}
	if err := validateConsumerSourceReads(s.consumer.reads); err != nil {
		return nil, err
	}
	if err := s.consumer.source.Validate(s.consumer.epoch); err != nil {
		return nil, err
	}
	projection, _, err := BuildSerenaBuildEnvironmentProjection(ctx, s, policy)
	if err != nil {
		return nil, err
	}
	policySHA, err := policy.IdentitySHA256()
	if err != nil {
		return nil, err
	}
	sha, err := projection.SHA256()
	if err != nil {
		return nil, err
	}
	return &SerenaConsumerEnvironmentObservation{source: s, binding: s.consumer, policySHA: policySHA, projectionSHA: sha, projection: projection}, ctx.Err()
}
func (o *SerenaConsumerEnvironmentObservation) Validate(ctx context.Context, rawSHA, task, policySHA, expectedSHA string) error {
	if o == nil || ctx == nil || ctx.Err() != nil || o.source == nil || o.source.consumer != o.binding || o.binding == nil || o.binding.rawManifestSHA != rawSHA || o.binding.task != task || o.policySHA != policySHA || o.projectionSHA != expectedSHA {
		return errors.New("SERENA_CONSUMER_ENVIRONMENT_BINDING_DRIFT")
	}
	if err := o.binding.source.Validate(o.binding.epoch); err != nil {
		return err
	}
	return validateConsumerSourceReads(o.binding.reads)
}
func (o *SerenaConsumerEnvironmentObservation) Projection(ctx context.Context) (SerenaBuildEnvironmentProjectionV1, error) {
	if o == nil || o.binding == nil {
		return SerenaBuildEnvironmentProjectionV1{}, errors.New("SERENA_CONSUMER_ENVIRONMENT_MISSING")
	}
	if err := o.Validate(ctx, o.binding.rawManifestSHA, o.binding.task, o.policySHA, o.projectionSHA); err != nil {
		return SerenaBuildEnvironmentProjectionV1{}, err
	}
	raw, err := json.Marshal(o.projection)
	if err != nil {
		return SerenaBuildEnvironmentProjectionV1{}, err
	}
	var copy SerenaBuildEnvironmentProjectionV1
	err = json.Unmarshal(raw, &copy)
	return copy, err
}

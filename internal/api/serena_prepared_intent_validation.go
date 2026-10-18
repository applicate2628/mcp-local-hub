package api

import (
	"encoding/json"
	"errors"
)

func validatePreparedSerenaBinding(binding PreparedSerenaRuntimeBinding) error {
	if binding.Schema != PreparedSerenaBindingSchema || !preparedSerenaID(binding.GenerationID) {
		return errors.New("SERENA_PREPARED_BINDING_INVALID")
	}
	for _, digest := range []string{binding.RequestIdentitySHA256, binding.RecordSHA256, binding.SourceSHA256, binding.InterpreterSHA256, binding.EntrypointSHA256, binding.ResolutionSHA256} {
		if !preparedSerenaDigest(digest) {
			return errors.New("SERENA_PREPARED_BINDING_INVALID")
		}
	}
	return nil
}
func preparedSerenaDescriptorBinding(d SupervisorDaemon) (*PreparedSerenaRuntimeBinding, error) {
	if d.PreparedRuntime != nil {
		if d.Workspace != "" || d.RuntimeSpec != nil || d.Server != SerenaServerName {
			return nil, errors.New("SERENA_PREPARED_GLOBAL_DESCRIPTOR_INVALID")
		}
		if err := validatePreparedSerenaBinding(*d.PreparedRuntime); err != nil {
			return nil, err
		}
		return d.PreparedRuntime, nil
	}
	if d.RuntimeSpec != nil && (d.RuntimeSpec.PreparedRuntime != nil || d.RuntimeSpec.SpecVersion == SerenaPreparedRuntimeSpecVersion) {
		if d.Workspace == "" || d.Server != SerenaServerName || d.RuntimeSpec.SpecVersion != SerenaPreparedRuntimeSpecVersion || d.RuntimeSpec.PreparedRuntime == nil {
			return nil, errors.New("SERENA_PREPARED_WORKSPACE_DESCRIPTOR_INVALID")
		}
		if err := validatePreparedSerenaBinding(*d.RuntimeSpec.PreparedRuntime); err != nil {
			return nil, err
		}
		return d.RuntimeSpec.PreparedRuntime, nil
	}
	return nil, nil
}
func preparedSerenaDescriptorSHA(d SupervisorDaemon) (string, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	return preparedSerenaHash(raw), nil
}

func normalizePreparedSupervisorIntentMerge(prior, merged *SupervisorIntentFile) error {
	if err := validatePreparedSupervisorIntent(prior); err != nil {
		return err
	}
	if merged == nil {
		return errors.New("SERENA_PREPARED_INTENT_MISSING")
	}
	prepared := false
	for _, d := range merged.Daemons {
		binding, err := preparedSerenaDescriptorBinding(d)
		if err != nil {
			return err
		}
		prepared = prepared || binding != nil
	}
	if prior.Version == 1 && prepared {
		if merged.Version != 1 || merged.SerenaPreparedConsumers != nil {
			return errors.New("SERENA_PREPARED_INTENT_FENCE_MISSING")
		}
		merged.Version = 2
		merged.SerenaPreparedConsumers = &PreparedSerenaConsumersV1{Schema: PreparedSerenaConsumersSchema, Bindings: []PreparedConsumerBindingV1{}}
	} else {
		merged.SerenaPreparedConsumers = clonePreparedSerenaConsumers(merged.SerenaPreparedConsumers)
	}
	return validatePreparedSupervisorIntent(merged)
}

func validatePreparedSupervisorIntent(f *SupervisorIntentFile) error {
	if f == nil {
		return errors.New("SERENA_PREPARED_INTENT_MISSING")
	}
	if f.Version != 1 && f.Version != 2 {
		return errors.New("SERENA_PREPARED_INTENT_VERSION_UNSUPPORTED")
	}
	descriptors := map[string]SupervisorDaemon{}
	for _, d := range f.Daemons {
		binding, err := preparedSerenaDescriptorBinding(d)
		if err != nil {
			return err
		}
		if binding != nil {
			if _, exists := descriptors[d.TaskName]; exists {
				return errors.New("SERENA_PREPARED_DESCRIPTOR_DUPLICATE")
			}
			descriptors[d.TaskName] = d
		}
	}
	if f.Version == 1 {
		if f.SerenaPreparedConsumers != nil || len(descriptors) != 0 {
			return errors.New("SERENA_PREPARED_INTENT_FENCE_MISSING")
		}
		return nil
	}
	if err := f.SerenaPreparedConsumers.Validate(); err != nil {
		return err
	}
	for _, row := range f.SerenaPreparedConsumers.Bindings {
		if row.Disposition == "rollback" {
			continue
		} // exact old generation remains a reference
		d, exists := descriptors[row.TaskName]
		if !exists {
			return errors.New("SERENA_PREPARED_CONSUMER_DESCRIPTOR_MISSING")
		}
		binding, err := preparedSerenaDescriptorBinding(d)
		if err != nil {
			return err
		}
		kind := "global"
		if d.Workspace != "" {
			kind = "workspace"
		}
		descriptorSHA, err := preparedSerenaDescriptorSHA(d)
		if err != nil {
			return err
		}
		if binding == nil || kind != row.ConsumerKind || descriptorSHA != row.DescriptorSHA256 || binding.RequestIdentitySHA256 != row.PackageRequestIdentitySHA256 || binding.GenerationID != row.GenerationID || binding.RecordSHA256 != row.ReadyRecordSHA256 || binding.SourceSHA256 != row.SourceSHA256 || binding.EntrypointSHA256 != row.EntrypointSHA256 {
			return errors.New("SERENA_PREPARED_CONSUMER_DESCRIPTOR_DRIFT")
		}
	}
	return nil
}

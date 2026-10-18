package api

import (
	"context"
	"errors"
	"io"
	"mcp-local-hub/internal/config"
)

type SerenaPreparationAssembly interface {
	PrepareInputs(context.Context, *config.ServerManifest, string, []string, *SerenaPreparationPolicyV1) (SerenaPreparationInputs, error)
	QualificationPort() PreparedSerenaQualificationPort
}

type SerenaConsumerEnvironmentAssembly interface {
	ObserveConsumerEnvironment(context.Context, *config.ServerManifest, string, string, *SerenaPreparationPolicyV1) (*SerenaConsumerEnvironmentObservation, error)
}

func (a *API) prepareSerenaInstallPlan(ctx context.Context, m *config.ServerManifest, plan *Plan, dryRun bool) error {
	if m == nil || plan == nil {
		return errors.New("SERENA_INSTALL_PLAN_MISSING")
	}
	if dryRun || !IsSerenaServer(m.Name) || len(plan.SupervisorIntent) == 0 {
		return nil
	}
	selected := plan.selectedExecutionPhase
	if selected == nil {
		return errors.New("SERENA_PREPARED_PRECOMMIT_PLAN_EFFECTS_REFUSED")
	}
	actual := selected.opts
	actual.DryRun = dryRun
	if err := selected.admitPrepared(m, plan, actual, false); err != nil {
		return err
	}
	tasks := make([]string, 0, len(plan.SupervisorIntent))
	rawSHA := ""
	for _, entry := range plan.SupervisorIntent {
		if entry.Name == "" || !preparedSerenaDigest(entry.manifestHash) {
			return errors.New("SERENA_INSTALL_FROZEN_TASK_SOURCE_MISSING")
		}
		if rawSHA == "" {
			rawSHA = entry.manifestHash
		} else if rawSHA != entry.manifestHash {
			return errors.New("SERENA_INSTALL_MANIFEST_SOURCE_DRIFT")
		}
		tasks = append(tasks, entry.Name)
	}
	binding, record, request, err := a.prepareSerenaInstall(ctx, m, rawSHA, tasks, nil)
	if err != nil {
		return err
	}
	plan.serenaPreparedBinding = &binding
	plan.serenaPreparedRecord = &record
	plan.serenaPreparationRequest = &request
	descriptors, descriptorErr := serenaOrPlanDaemons(m, plan, rawSHA, nil, "", io.Discard)
	if descriptorErr != nil {
		return descriptorErr
	}
	consumerID, idErr := NewSerenaConsumerOperationID(ctx)
	if idErr != nil {
		return idErr
	}
	owner, ownerErr := a.PreparedSerenaConsumerOwner()
	if ownerErr != nil {
		return ownerErr
	}
	bounded, cancel := context.WithDeadline(ctx, request.WorkDeadline)
	defer cancel()
	invocation, invocationErr := owner.AcquireConsumerInvocation(bounded, PreparedSerenaConsumerInvocationRequest{OperationID: consumerID, Binding: binding, ExpectedDescriptors: descriptors})
	if invocationErr != nil {
		if invocation != nil {
			proof, closeErr := invocation.CloseObserved()
			invocationErr = errors.Join(invocationErr, closeErr, proof.Validate(invocation))
		}
		return invocationErr
	}
	plan.serenaPreparedInvocation = invocation
	return nil
}

func NewAPIWithSerenaPreparation(assembly SerenaPreparationAssembly) *API {
	a := NewAPI()
	a.serenaPreparationAssembly = assembly
	a.serenaPreparationPublication = NewPreparedSerenaPublicationOwner()
	return a
}

// Normal install calls this unlocked, with its actual frozen task set.
// Parsed callers already holding exclusion must supply a previously qualified
// binding instead. No API-to-daemon import or default global provider exists.
func (a *API) prepareSerenaInstall(ctx context.Context, m *config.ServerManifest, rawSHA string, tasks []string, policy *SerenaPreparationPolicyV1) (PreparedSerenaRuntimeBinding, PreparedSerenaRuntimeRecord, PreparedSerenaRequest, error) {
	if a == nil || a.serenaPreparationAssembly == nil {
		return PreparedSerenaRuntimeBinding{}, PreparedSerenaRuntimeRecord{}, PreparedSerenaRequest{}, errors.New("SERENA_PREPARATION_ASSEMBLY_UNAVAILABLE")
	}
	inputs, err := a.serenaPreparationAssembly.PrepareInputs(ctx, m, rawSHA, append([]string(nil), tasks...), policy)
	if err != nil {
		return PreparedSerenaRuntimeBinding{}, PreparedSerenaRuntimeRecord{}, PreparedSerenaRequest{}, err
	}
	work, cancel := context.WithDeadline(ctx, inputs.Request.WorkDeadline)
	defer cancel()
	intentPath := joinStateFilePath(inputs.Request.Namespace, supervisorIntentFileLeaf)
	owner, err := NewPreparedSerenaRuntimeComposition(work, inputs, intentPath, a.serenaPreparationAssembly.QualificationPort(), a.serenaPreparationPublication)
	if err != nil {
		return PreparedSerenaRuntimeBinding{}, PreparedSerenaRuntimeRecord{}, PreparedSerenaRequest{}, err
	}
	binding, err := owner.Prepare(work, inputs.Request)
	if err != nil {
		return PreparedSerenaRuntimeBinding{}, PreparedSerenaRuntimeRecord{}, PreparedSerenaRequest{}, err
	}
	if err = a.BindPreparedSerenaRuntimeOwner(owner); err != nil {
		return PreparedSerenaRuntimeBinding{}, PreparedSerenaRuntimeRecord{}, PreparedSerenaRequest{}, err
	}
	if owner.completedRecord == nil {
		return PreparedSerenaRuntimeBinding{}, PreparedSerenaRuntimeRecord{}, PreparedSerenaRequest{}, errors.New("SERENA_INSTALL_COMPLETED_SELECTION_MISSING")
	}
	return binding, *owner.completedRecord, inputs.Request, nil
}

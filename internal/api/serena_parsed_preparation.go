package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"mcp-local-hub/internal/config"
)

// PreparedSerenaParsedInstall keeps the original API publication owner across
// an unlocked package preparation and a later, exact registry selection. It
// cannot be reconstructed from a ready record or a caller-supplied binding.
type PreparedSerenaParsedInstall struct {
	api         *API
	owner       *PreparedSerenaRuntimeOwner
	manifest    *config.ServerManifest
	manifestSHA string
	policy      SerenaPreparationPolicyV1
	request     PreparedSerenaRequest
	binding     PreparedSerenaRuntimeBinding
	record      PreparedSerenaRuntimeRecord
}

// PrepareSerenaParsedInstall performs package preparation before a caller
// reaps the supervisor or acquires its exclusion lock. The final workspace set
// may be re-read later; acquiring that exact consumer invocation performs only
// observation and validation, never package installation or qualification.
func (a *API) PrepareSerenaParsedInstall(ctx context.Context, m *config.ServerManifest, opts InstallParsedManifestOpts) (*PreparedSerenaParsedInstall, error) {
	if ctx == nil {
		return nil, errors.New("SERENA_PARSED_PREPARATION_CONTEXT_INVALID")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a == nil || m == nil || !IsSerenaServer(m.Name) || m.Kind != config.KindWorkspaceScoped || m.DaemonTemplate == nil {
		return nil, errors.New("SERENA_PARSED_PREPARATION_SCOPE_INVALID")
	}
	if opts.DryRun || opts.SupervisorLockBypass.lk != nil || opts.SerenaPreparedBinding != nil || opts.SerenaPreparedInvocation != nil {
		return nil, errors.New("SERENA_PARSED_PREPARATION_REQUIRES_UNLOCKED_CALLER")
	}
	if opts.Workspaces == nil || !preparedSerenaDigest(opts.ManifestHash) {
		return nil, errors.New("SERENA_PARSED_PREPARATION_SOURCE_MISSING")
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("validate manifest: %w", err)
	}
	if err := validateDynamicPoolManifest(m); err != nil {
		return nil, err
	}
	w := opts.Writer
	if w == nil {
		w = io.Discard
	}
	live := filterExistingWorkspaceRows(m.Name, opts.Workspaces, w)
	if len(live) == 0 {
		return nil, nil
	}
	if err := Preflight(m, ""); err != nil {
		return nil, fmt.Errorf("preflight: %w", err)
	}
	plan, err := BuildPlanWithOpts(m, a.installBuildPlanOpts(InstallOpts{ClientsInclude: opts.ClientsInclude, IncludeAllClients: opts.IncludeAllClients}))
	if err != nil {
		return nil, err
	}
	selected, err := selectInstallExecutionPhase(m, plan, installPlanOpts{Writer: w, SkipSchedulerPrune: true}, true, true)
	if err != nil {
		return nil, err
	}
	if err := selected.admitPrepared(m, plan, selected.opts, false); err != nil {
		return nil, err
	}
	policy, err := resolveSerenaPreparationPolicy(opts.SerenaPreparationPolicy)
	if err != nil {
		return nil, err
	}
	// Freeze the parsed input as well as its exact raw-source hash. A caller may
	// not change the manifest while the registry is unlocked and silently widen
	// the descriptor selection attached to the original package owner.
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var frozen config.ServerManifest
	if err = json.Unmarshal(raw, &frozen); err != nil {
		return nil, err
	}
	tasks := make([]string, 0, len(live))
	for _, workspace := range live {
		tasks = append(tasks, SerenaTaskNameForWorkspace(workspace.WorkspacePath))
	}
	binding, record, request, err := a.prepareSerenaInstall(ctx, &frozen, opts.ManifestHash, tasks, &policy)
	if err != nil {
		return nil, err
	}
	policySHA, err := policy.IdentitySHA256()
	if err != nil || request.ManifestSHA256 != opts.ManifestHash || request.PolicySHA256 != policySHA {
		return nil, errors.Join(err, errors.New("SERENA_PARSED_PREPARATION_SOURCE_DRIFT"))
	}
	owner, err := a.PreparedSerenaConsumerOwner()
	if err != nil {
		return nil, err
	}
	return &PreparedSerenaParsedInstall{api: a, owner: owner, manifest: &frozen, manifestSHA: opts.ManifestHash, policy: policy, request: request, binding: binding, record: record}, nil
}

// WithConsumerInvocation retains the exact final workspace selection through
// the install callback and observed closure. Its context stays live until all
// original mutation observations have received their resource closes. A
// cleanup failure is separate from the callback outcome so a committed intent
// cannot be mistaken for a pre-commit failure.
func (prepared *PreparedSerenaParsedInstall) WithConsumerInvocation(ctx context.Context, workspaces []WorkspaceEntry, bypass InstallParsedManifestBypass, run func(context.Context, *PreparedSerenaConsumerInvocation) error) (runErr, cleanupErr error) {
	if prepared == nil || ctx == nil || run == nil {
		return errors.New("SERENA_PARSED_PREPARATION_MISSING"), nil
	}
	work, cancel := context.WithDeadline(ctx, prepared.request.WorkDeadline)
	defer cancel()
	invocation, err := prepared.acquireConsumerInvocation(work, workspaces, bypass)
	if err != nil {
		return err, nil
	}
	if invocation != nil {
		defer func() {
			proof, closeErr := invocation.CloseObserved()
			cleanupErr = errors.Join(closeErr, proof.Validate(invocation))
		}()
	}
	return run(work, invocation), nil
}

func (prepared *PreparedSerenaParsedInstall) acquireConsumerInvocation(work context.Context, workspaces []WorkspaceEntry, bypass InstallParsedManifestBypass) (*PreparedSerenaConsumerInvocation, error) {
	if prepared == nil || prepared.api == nil || prepared.owner == nil || prepared.manifest == nil || work == nil {
		return nil, errors.New("SERENA_PARSED_PREPARATION_MISSING")
	}
	owner, err := prepared.api.PreparedSerenaConsumerOwner()
	if err != nil || owner != prepared.owner {
		return nil, errors.Join(err, errors.New("SERENA_PARSED_PREPARATION_OWNER_DRIFT"))
	}
	if err := work.Err(); err != nil {
		return nil, err
	}
	if workspaces == nil {
		return nil, errors.New("SERENA_PARSED_PREPARATION_SOURCE_MISSING")
	}
	live := filterExistingWorkspaceRows(prepared.manifest.Name, workspaces, io.Discard)
	if len(live) == 0 {
		return nil, nil
	}
	assembly, ok := prepared.api.serenaPreparationAssembly.(SerenaConsumerEnvironmentAssembly)
	if !ok || assembly == nil {
		return nil, errors.New("SERENA_CONSUMER_ENVIRONMENT_ASSEMBLY_UNAVAILABLE")
	}
	for _, workspace := range live {
		task := SerenaTaskNameForWorkspace(workspace.WorkspacePath)
		observation, err := assembly.ObserveConsumerEnvironment(work, prepared.manifest, prepared.manifestSHA, task, &prepared.policy)
		if err != nil {
			return nil, err
		}
		if err = observation.Validate(work, prepared.manifestSHA, task, prepared.request.PolicySHA256, prepared.request.BuildEnvironmentSHA256); err != nil {
			return nil, err
		}
	}
	var runtimeProduct *InstallParsedManifestBypass
	if bypass.lk != nil {
		runtimeProduct = &bypass
	}
	image, err := resolveInstallRuntimeProduct(runtimeProduct)
	if err != nil {
		return nil, err
	}
	plan := &Plan{serenaPreparedBinding: &prepared.binding, serenaPreparedRecord: &prepared.record}
	descriptors, err := materializePreparedSerenaDescriptors(prepared.manifest, BuildSupervisorDaemonsForSerena(prepared.manifest, live, prepared.manifestSHA, image), plan)
	if err != nil {
		return nil, err
	}
	operationID, err := NewSerenaConsumerOperationID(work)
	if err != nil {
		return nil, err
	}
	invocation, err := owner.AcquireConsumerInvocation(work, PreparedSerenaConsumerInvocationRequest{OperationID: operationID, Binding: prepared.binding, ExpectedDescriptors: descriptors})
	if err != nil && invocation != nil {
		proof, closeErr := invocation.CloseObserved()
		return nil, errors.Join(err, closeErr, proof.Validate(invocation))
	}
	return invocation, err
}

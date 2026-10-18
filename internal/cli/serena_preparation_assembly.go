package cli

import (
	"context"
	"errors"
	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/secrets"
	"os"
	"time"
)

type serenaPreparationAssembly struct{}

func newSerenaPreparationAssembly() api.SerenaPreparationAssembly {
	return &serenaPreparationAssembly{}
}
func (*serenaPreparationAssembly) QualificationPort() api.PreparedSerenaQualificationPort {
	return &PreparedSerenaUnpublishedQualifier{EndpointEvidence: observeSerenaOriginalEndpoint}
}
func (*serenaPreparationAssembly) PrepareInputs(ctx context.Context, manifest *config.ServerManifest, rawSHA string, taskNames []string, override *api.SerenaPreparationPolicyV1) (api.SerenaPreparationInputs, error) {
	if ctx == nil || manifest == nil || len(taskNames) == 0 {
		return api.SerenaPreparationInputs{}, errors.New("SERENA_PREPARATION_TASK_SCOPE_MISSING")
	}
	policy := api.DefaultSerenaPreparationPolicy()
	if override != nil {
		policy = *override
	}
	if err := policy.Validate(); err != nil {
		return api.SerenaPreparationInputs{}, err
	}
	end := time.Now().Add(policy.WorkLimit)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(end) {
		end = deadline
	}
	work, cancel := context.WithDeadline(ctx, end)
	defer cancel()
	ids, err := api.NewSerenaPreparationIDs(work)
	if err != nil {
		return api.SerenaPreparationInputs{}, err
	}
	bootstrap := map[string]string{}
	for _, key := range api.SerenaBuildBootstrapDeclarationKeys() {
		value, present := os.LookupEnv(key)
		if !present || value == "" {
			return api.SerenaPreparationInputs{}, errors.New("SERENA_BUILD_BOOTSTRAP_DECLARATION_MISSING")
		}
		bootstrap[key] = value
	}
	vault, vaultErr := secrets.OpenVaultOptional(defaultKeyPath(), defaultVaultPath())
	resolver := secrets.NewResolver(vault, nil)
	seen := map[string]bool{}
	var selected api.SerenaPreparationEnvironmentSource
	projectionSHA := ""
	for _, task := range taskNames {
		if task == "" || seen[task] {
			return api.SerenaPreparationInputs{}, errors.New("SERENA_PREPARATION_TASK_SCOPE_AMBIGUOUS")
		}
		seen[task] = true
		source, sourceErr := buildSerenaPreparationEnvironmentSource(work, manifest, resolver, task, manifest.EnvForwardLocal, bootstrap)
		if sourceErr != nil {
			return api.SerenaPreparationInputs{}, errors.Join(sourceErr, vaultErr)
		}
		projection, _, projectionErr := api.BuildSerenaBuildEnvironmentProjection(work, source, policy)
		if projectionErr != nil {
			return api.SerenaPreparationInputs{}, errors.Join(projectionErr, vaultErr)
		}
		sha, projectionErr := projection.SHA256()
		if projectionErr != nil {
			return api.SerenaPreparationInputs{}, projectionErr
		}
		if selected == nil {
			selected = source
			projectionSHA = sha
		} else if sha != projectionSHA {
			return api.SerenaPreparationInputs{}, errors.New("SERENA_PREPARATION_TASK_BUILD_PROJECTIONS_DIFFER")
		}
	}
	// Unrelated runtime-only secrets do not participate in package preparation.
	// A relevant reference failure above retains the original vault cause.
	return api.BuildSerenaPreparationInputs(work, manifest, rawSHA, api.SerenaPreparationAdmission{OperationID: ids.OperationID, GenerationID: ids.GenerationID, EnvironmentSource: selected}, override)
}

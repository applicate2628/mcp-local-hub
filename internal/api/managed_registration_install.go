package api

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"mcp-local-hub/internal/buildinfo"
	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

type managedRegistrationInstallMaterializer func(context.Context, *API, *config.ServerManifest, *Plan, InstallOpts, ClientRoutingAuthorityRequest, io.Writer) error

type managedRegistrationInstallCompositionDeps struct {
	Core        managedRegistrationInstallCore
	Clients     func() (map[string]clients.Client, []string)
	StateDir    func() (string, error)
	BuildInfo   func() (string, string, string)
	Now         func() time.Time
	Materialize managedRegistrationInstallMaterializer
}

func productionManagedRegistrationInstallDeps(apiHandle *API) managedRegistrationInstallCompositionDeps {
	return managedRegistrationInstallCompositionDeps{
		Core:      managedRegistrationInstallCoreV1{api: apiHandle},
		Clients:   clients.AllClientsWithErrors,
		StateDir:  DaemonStateDir,
		BuildInfo: buildinfo.Get,
		Now:       time.Now,
		Materialize: func(ctx context.Context, apiHandle *API, manifest *config.ServerManifest, plan *Plan, opts InstallOpts, authority ClientRoutingAuthorityRequest, writer io.Writer) error {
			return apiHandle.installPlanCoreWithRoutingTargetLease(ctx, manifest, plan, opts, authority, writer)
		},
	}
}

// InstallCommittedUnverifiedError reports a successful PR-core commit whose
// post-commit managed-registration journal/projection did not settle.
type InstallCommittedUnverifiedError struct{ cause error }

func (e *InstallCommittedUnverifiedError) Error() string {
	if e == nil || e.cause == nil {
		return "install committed but managed registration settlement is unverified"
	}
	return "install committed but managed registration settlement is unverified: " + e.cause.Error()
}

func (e *InstallCommittedUnverifiedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

type managedRegistrationInstallCoreV1 struct{ api *API }

func (core managedRegistrationInstallCoreV1) ReconcileInstall(ctx context.Context, input managedRegistrationFrozenInstallInput) (*ManagedRegistrationReconcileReport, error) {
	if core.api == nil {
		return nil, newManagedRegistrationFailure(ManagedRegistrationFailureRuntimeInvalid, nil)
	}
	runtime, err := newManagedRegistrationRuntime(input)
	if err != nil {
		return nil, err
	}
	clientSet := make(map[string]struct{}, len(input.Targets))
	for _, target := range input.Targets {
		clientSet[target.ClientID] = struct{}{}
	}
	clientIDs := make([]string, 0, len(clientSet))
	for clientID := range clientSet {
		clientIDs = append(clientIDs, clientID)
	}
	sort.Strings(clientIDs)
	return core.api.ReconcileManagedRegistrations(ctx, ManagedRegistrationReconcileOptions{
		Runtime: runtime,
		Request: ManagedRegistrationReconcileRequest{
			Operation:             ManagedRegistrationForward,
			Trigger:               ManagedRegistrationTriggerInstall,
			Servers:               []string{input.Server},
			Clients:               clientIDs,
			RequireAlreadyDesired: true,
		},
	})
}

func (a *API) executeManagedRegistrationInstall(
	ctx context.Context,
	manifest *config.ServerManifest,
	plan *Plan,
	opts InstallOpts,
	authority ClientRoutingAuthorityRequest,
	writer io.Writer,
	allClients map[string]clients.Client,
	clientConstructionFailures []string,
	deps managedRegistrationInstallCompositionDeps,
) (materializationCommitted bool, err error) {
	if deps.Materialize == nil || plan == nil || manifest == nil {
		return false, fmt.Errorf("managed registration install composition is incomplete")
	}
	settleManaged := !opts.DryRun && !opts.SkipClientConfigWrites && len(plan.ClientUpdates) > 0
	var input managedRegistrationFrozenInstallInput
	if settleManaged {
		input, err = freezeManagedRegistrationInstallInput(manifest, plan, allClients, clientConstructionFailures, deps)
		if err != nil {
			return false, err
		}
	}
	if err := deps.Materialize(ctx, a, manifest, plan, opts, authority, writer); err != nil {
		return false, err
	}
	materializationCommitted = !opts.DryRun
	if !settleManaged {
		return materializationCommitted, nil
	}
	if deps.Core == nil {
		return true, &InstallCommittedUnverifiedError{cause: fmt.Errorf("managed registration install core is unavailable")}
	}
	if _, err := deps.Core.ReconcileInstall(ctx, input); err != nil {
		return true, &InstallCommittedUnverifiedError{cause: err}
	}
	return true, nil
}

func freezeManagedRegistrationInstallInput(manifest *config.ServerManifest, plan *Plan, allClients map[string]clients.Client, constructionFailures []string, deps managedRegistrationInstallCompositionDeps) (managedRegistrationFrozenInstallInput, error) {
	if manifest == nil || plan == nil || manifest.Name == "" || plan.Server != manifest.Name || len(plan.ClientUpdates) == 0 || deps.StateDir == nil || deps.BuildInfo == nil || deps.Now == nil {
		return managedRegistrationFrozenInstallInput{}, newManagedRegistrationFailure(ManagedRegistrationFailureRuntimeInvalid, nil)
	}
	stateDir, err := deps.StateDir()
	if err != nil {
		return managedRegistrationFrozenInstallInput{}, err
	}
	version, commit, _ := deps.BuildInfo()
	input := managedRegistrationFrozenInstallInput{
		Server:           plan.Server,
		ReleaseIdentity:  managedRegistrationReleaseIdentity{Version: version, Commit: commit},
		CapturedAtUTC:    deps.Now().UTC(),
		StateRoot:        stateDir,
		JournalRoot:      stateDir,
		BackupIO:         managedRegistrationBackupIO{},
		ClientPopulation: make(map[string]clients.Client),
	}
	failureSet := make(map[string]struct{}, len(constructionFailures))
	for _, clientID := range constructionFailures {
		failureSet[clientID] = struct{}{}
	}
	selected := make(map[string]struct{}, len(plan.ClientUpdates))
	for _, update := range plan.ClientUpdates {
		selected[update.Client] = struct{}{}
	}
	for clientID := range selected {
		if client := allClients[clientID]; client != nil {
			input.ClientPopulation[clientID] = client
			continue
		}
		if _, failed := failureSet[clientID]; failed {
			input.ClientConstructionFailures = append(input.ClientConstructionFailures, clientID)
			continue
		}
		input.ClientConstructionFailures = append(input.ClientConstructionFailures, clientID)
	}
	sort.Strings(input.ClientConstructionFailures)
	for _, update := range plan.ClientUpdates {
		daemon, found := findDaemon(manifest, update.DaemonName)
		if !found {
			return managedRegistrationFrozenInstallInput{}, newManagedRegistrationFailure(ManagedRegistrationFailureRuntimeTargetConflict, nil)
		}
		taskName := supervisorTaskNameForManifestDaemon(manifest.Name, update.DaemonName)
		taskMatches := 0
		for _, intent := range plan.SupervisorIntent {
			if intent.Name == taskName {
				taskMatches++
			}
		}
		if taskMatches != 1 {
			return managedRegistrationFrozenInstallInput{}, newManagedRegistrationFailure(ManagedRegistrationFailureRuntimeTargetConflict, nil)
		}
		urlPath := ""
		bindingMatches := 0
		for _, binding := range manifest.ClientBindings {
			if binding.Client == update.Client && binding.Daemon == update.DaemonName {
				urlPath = binding.URLPath
				if urlPath == "" {
					urlPath = "/mcp"
				}
				bindingMatches++
			}
		}
		if bindingMatches != 1 {
			return managedRegistrationFrozenInstallInput{}, newManagedRegistrationFailure(ManagedRegistrationFailureRuntimeTargetConflict, nil)
		}
		desired := installClientEntryV1(plan.Server, update, plan.installRelayExePath)
		logical := plan.Server
		if plan.projectAttachment != nil {
			desired = plan.projectAttachment.preparation.DesiredEntry()
			desired.Raw = nil
			logical, _, _, _, _ = plan.projectAttachment.preparation.OwnedDelta()
		}
		frozen := managedRegistrationFrozenInstallTarget{
			ClientID: update.Client, LogicalEntryName: logical, PhysicalEntryName: desired.Name, Action: update.Action,
			DaemonName: update.DaemonName, TaskName: taskName, Port: daemon.Port, URLPath: urlPath, Desired: desired,
		}
		frozen.ProjectAttachment = plan.projectAttachment != nil
		if plan.projectAttachment == nil && isCodexAliasInstallUpdate(plan.Server, update) {
			codex := &managedRegistrationFrozenCodexRelocation{SourceEntryName: plan.Server, TargetEntryName: desired.Name, AlreadyConfigured: update.codexAlreadyConfigured}
			if update.codexAlreadyConfigured {
				codex.ExpectedSource = clients.CodexTransportNone
			} else {
				client := input.ClientPopulation[update.Client]
				if client == nil {
					return managedRegistrationFrozenInstallInput{}, newManagedRegistrationFailure(ManagedRegistrationFailureClientPopulationIncomplete, nil)
				}
				prior, readErr := client.GetEntry(plan.Server)
				if readErr != nil {
					return managedRegistrationFrozenInstallInput{}, readErr
				}
				expectedSource, sourceErr := codexInstallSourceTransport(prior)
				if sourceErr != nil {
					return managedRegistrationFrozenInstallInput{}, sourceErr
				}
				codex.ExpectedSource = expectedSource
			}
			frozen.Codex = codex
		}
		input.Targets = append(input.Targets, frozen)
	}
	return input, nil
}

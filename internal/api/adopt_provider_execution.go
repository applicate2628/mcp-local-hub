package api

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/clients"
)

type providerTransactionDeps struct {
	observer providerDirectProcessObserver
	source   clients.ProviderMCPSourceV1
	stop     func(context.Context, SupervisorDaemon) (StoppedSettlement, error)
	close    func(string) error
}

func (d providerTransactionDeps) stopManaged(ctx context.Context, api *API, frozen SupervisorDaemon) (StoppedSettlement, error) {
	if d.stop != nil {
		return d.stop(ctx, frozen)
	}
	return api.stopAdoptOwnedDaemonSettled(ctx, frozen)
}

func (d providerTransactionDeps) closeProvenance(manifestName string) error {
	if d.close != nil {
		return d.close(manifestName)
	}
	return CloseAdoptProvenance(manifestName)
}

func frozenProviderAdoptDaemon(rec *AdoptProvenanceRecord) (SupervisorDaemon, error) {
	if rec == nil {
		return SupervisorDaemon{}, fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	}
	intent, err := loadSupervisorOwnedIntent()
	if err != nil {
		return SupervisorDaemon{}, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	var matches []SupervisorDaemon
	for _, daemon := range intent.Daemons {
		if daemon.Server == rec.ManifestName && daemon.Daemon == adoptDefaultDaemonName && daemon.Port == rec.Port && daemon.ManifestHash == rec.ExpectedManifestHash {
			matches = append(matches, daemon)
		}
	}
	if len(matches) != 1 {
		return SupervisorDaemon{}, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	return matches[0], nil
}

type providerExecutionState struct {
	provider clients.ProviderMCPSourceV1
	entry    clients.ProviderMCPEntryV1
	identity providerDirectProcessIdentityV1
	prior    []providerProcessGenerationV1
}

func (d providerTransactionDeps) observe(ctx context.Context, identity providerDirectProcessIdentityV1, prior []providerProcessGenerationV1) providerDirectProcessObservationV1 {
	observer := d.observer
	if observer == nil {
		observer = observeProviderDirectProcessTree
	}
	return observer(ctx, identity, append([]providerProcessGenerationV1(nil), prior...))
}

func providerProcessIdentityFromEntry(entry clients.ProviderMCPEntryV1) (providerDirectProcessIdentityV1, error) {
	command := entry.Command
	if command == "" || entry.WorkingDir == nil || *entry.WorkingDir == "" || !filepath.IsAbs(*entry.WorkingDir) {
		return providerDirectProcessIdentityV1{}, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	workingDirectoryObject, err := providerWorkingDirectoryObjectIdentityFromPath(*entry.WorkingDir)
	if err != nil {
		return providerDirectProcessIdentityV1{}, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	if filepath.IsAbs(command) {
		// retained as supplied
	} else if strings.ContainsAny(command, `\/`) {
		command = filepath.Join(*entry.WorkingDir, command)
	} else {
		resolved, err := exec.LookPath(command)
		if err != nil {
			return providerDirectProcessIdentityV1{}, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
		}
		command = resolved
	}
	resolved, err := filepath.EvalSymlinks(command)
	if err != nil {
		return providerDirectProcessIdentityV1{}, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || strings.EqualFold(filepath.Ext(resolved), ".cmd") || strings.EqualFold(filepath.Ext(resolved), ".bat") {
		return providerDirectProcessIdentityV1{}, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	if runtime.GOOS == "windows" && binaryadmission.ValidateWindowsNativeImage(resolved) != nil {
		return providerDirectProcessIdentityV1{}, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	return providerDirectProcessIdentityV1{ExecutablePath: resolved, Args: append([]string(nil), entry.Args...), WorkingDirectoryObject: workingDirectoryObject}, nil
}

func observeProviderDirectProcessTwice(deps providerTransactionDeps, identity providerDirectProcessIdentityV1, prior []providerProcessGenerationV1) (providerDirectProcessObservationV1, providerDirectProcessObservationV1) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	before := deps.observe(ctx, identity, prior)
	ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	after := deps.observe(ctx, identity, prior)
	return before, after
}

func resolveProviderExecutionState(plan *AdoptPlan, injected clients.ProviderMCPSourceV1) (providerExecutionState, error) {
	if plan == nil || plan.providerSource == nil {
		return providerExecutionState{}, fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	}
	provider := injected
	if provider == nil {
		client, ok := clients.AllClients()[plan.providerSource.ProviderClient]
		if !ok {
			return providerExecutionState{}, fmt.Errorf("E_PROVIDER_CAPABILITY_UNSUPPORTED")
		}
		var supported bool
		provider, supported = client.(clients.ProviderMCPSourceV1)
		if !supported {
			return providerExecutionState{}, fmt.Errorf("E_PROVIDER_CAPABILITY_UNSUPPORTED")
		}
	}
	entries, err := provider.ListProviderMCPEntries(context.Background())
	if err != nil {
		return providerExecutionState{}, fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	}
	var matches []clients.ProviderMCPEntryV1
	for _, entry := range entries {
		if entry.ProviderClient == plan.providerSource.ProviderClient && entry.PluginRef == plan.providerSource.PluginRef && entry.ServerName == plan.providerSource.ServerName {
			matches = append(matches, entry)
		}
	}
	if len(matches) != 1 {
		return providerExecutionState{}, fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	}
	entry := matches[0]
	expectedActivation := plan.providerSource.ActivationFingerprint
	expectedEnabled := true
	if plan.providerSource.DisablePhase == "disable_applied" {
		expectedActivation = plan.providerSource.ExpectedDisabledFingerprint
		expectedEnabled = false
	}
	if entry.Enabled != expectedEnabled || entry.ReceiptFingerprint != plan.providerSource.ReceiptFingerprint || entry.ActivationFingerprint != expectedActivation || entry.PolicyFingerprint != plan.providerSource.PolicyFingerprint {
		return providerExecutionState{}, fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	}
	identity, err := providerProcessIdentityFromEntry(entry)
	if err != nil {
		return providerExecutionState{}, err
	}
	return providerExecutionState{provider: provider, entry: entry, identity: identity}, nil
}

func providerDisable(ctx context.Context, state providerExecutionState, provenance *ProviderSourceProvenanceV1) (clients.ProviderMCPActivationResultV1, error) {
	result, err := state.provider.CompareAndSetProviderMCPActivation(ctx, clients.ProviderMCPActivationCASV1{
		PluginRef: state.entry.PluginRef, ServerName: state.entry.ServerName,
		ExpectedActivationFingerprint: provenance.ActivationFingerprint,
		ExpectedPolicyFingerprint:     provenance.PolicyFingerprint,
		DesiredEnabledPresent:         true, DesiredEnabled: false,
	})
	if err != nil || result.PriorEnabledPresent != provenance.PriorEnabledPresent || result.PriorEnabled != provenance.PriorEnabled || result.ActivationFingerprint != provenance.ExpectedDisabledFingerprint {
		return clients.ProviderMCPActivationResultV1{}, fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	}
	return result, nil
}

func providerRestore(ctx context.Context, state providerExecutionState, provenance *ProviderSourceProvenanceV1) error {
	result, err := state.provider.CompareAndSetProviderMCPActivation(ctx, clients.ProviderMCPActivationCASV1{
		PluginRef: state.entry.PluginRef, ServerName: state.entry.ServerName,
		ExpectedActivationFingerprint: provenance.ExpectedDisabledFingerprint,
		ExpectedPolicyFingerprint:     provenance.PolicyFingerprint,
		DesiredEnabledPresent:         provenance.PriorEnabledPresent, DesiredEnabled: provenance.PriorEnabled,
	})
	if err != nil || result.ActivationFingerprint != provenance.ActivationFingerprint {
		return fmt.Errorf("E_PROVIDER_RECOVERY_REQUIRED")
	}
	return nil
}

// recoverProviderActivation is explicit de-adopt recovery only. It recognizes
// the recorded prior fingerprint without writing, restores only from the exact
// recorded disabled fingerprint, and refuses every other observed state.
func recoverProviderActivation(ctx context.Context, source clients.ProviderMCPSourceV1, provenance *ProviderSourceProvenanceV1) error {
	if source == nil || provenance == nil {
		return fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	}
	entries, err := source.ListProviderMCPEntries(ctx)
	if err != nil {
		return fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	}
	var matches []clients.ProviderMCPEntryV1
	for _, entry := range entries {
		if entry.ProviderClient == provenance.ProviderClient && entry.PluginRef == provenance.PluginRef && entry.ServerName == provenance.ServerName {
			matches = append(matches, entry)
		}
	}
	if len(matches) != 1 {
		return fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	}
	entry := matches[0]
	if entry.ReceiptFingerprint != provenance.ReceiptFingerprint || entry.PolicyFingerprint != provenance.PolicyFingerprint {
		return fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	}
	if entry.ActivationFingerprint == provenance.ActivationFingerprint && entry.ActivationEnabledPresent == provenance.PriorEnabledPresent && entry.ActivationEnabled == provenance.PriorEnabled {
		return nil
	}
	if entry.ActivationFingerprint != provenance.ExpectedDisabledFingerprint || entry.Enabled {
		return fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	}
	return providerRestore(ctx, providerExecutionState{provider: source, entry: entry}, provenance)
}

func providerRevalidateDisabled(ctx context.Context, state providerExecutionState, provenance *ProviderSourceProvenanceV1) error {
	entries, err := state.provider.ListProviderMCPEntries(ctx)
	if err != nil {
		return fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	}
	var matches []clients.ProviderMCPEntryV1
	for _, entry := range entries {
		if entry.ProviderClient == provenance.ProviderClient && entry.PluginRef == provenance.PluginRef && entry.ServerName == provenance.ServerName {
			matches = append(matches, entry)
		}
	}
	if len(matches) != 1 || matches[0].Enabled || matches[0].ReceiptFingerprint != provenance.ReceiptFingerprint || matches[0].PolicyFingerprint != provenance.PolicyFingerprint || matches[0].ActivationFingerprint != provenance.ExpectedDisabledFingerprint {
		return fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	}
	return nil
}

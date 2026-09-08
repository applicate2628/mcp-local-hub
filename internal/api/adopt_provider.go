package api

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"mcp-local-hub/internal/clients"
)

func (a *API) buildProviderAdoptPlan(opts AdoptOpts, source clients.Client, provider clients.ProviderMCPSourceV1) (*AdoptPlan, error) {
	entries, err := provider.ListProviderMCPEntries(context.Background())
	if err != nil {
		return nil, err
	}
	var selected []clients.ProviderMCPEntryV1
	for _, entry := range entries {
		if entry.ProviderClient == opts.Client && entry.PluginRef == opts.ProviderPluginRef && entry.ServerName == opts.EntryName {
			selected = append(selected, entry)
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("E_PROVIDER_ENTRY_NOT_FOUND")
	}
	if len(selected) != 1 {
		return nil, fmt.Errorf("E_PROVIDER_ENTRY_AMBIGUOUS")
	}
	e := selected[0]
	if !e.Enabled {
		return nil, fmt.Errorf("E_PROVIDER_ENTRY_DISABLED")
	}
	if e.Transport == clients.ProviderMCPTransportHTTP {
		return nil, fmt.Errorf("E_PROVIDER_TRANSPORT_ALREADY_HTTP")
	}
	if e.Transport != clients.ProviderMCPTransportStdio {
		return nil, fmt.Errorf("E_PROVIDER_METADATA_INCOMPLETE")
	}
	if e.Scope != clients.ProviderMCPScopeUser {
		return nil, fmt.Errorf("E_PROVIDER_SCOPE_UNSUPPORTED")
	}
	if e.PolicyState != clients.ProviderMCPPolicyNone {
		return nil, fmt.Errorf("E_PROVIDER_POLICY_UNREPRESENTABLE")
	}
	if e.Command == "" || e.WorkingDir == nil || *e.WorkingDir == "" || !filepath.IsAbs(*e.WorkingDir) || e.ToolTimeoutSec < 0 {
		return nil, fmt.Errorf("E_PROVIDER_METADATA_INCOMPLETE")
	}
	for key, value := range e.Env {
		if key == "" || strings.ContainsAny(key, "\x00=") || strings.Contains(value, "$") || strings.HasPrefix(value, "secret:") || strings.HasPrefix(value, "file:") {
			return nil, fmt.Errorf("E_PROVIDER_STATIC_ENV_UNREPRESENTABLE")
		}
	}
	seenForward := make(map[string]struct{}, len(e.EnvForwardLocal))
	for _, key := range e.EnvForwardLocal {
		if key == "" || strings.ContainsAny(key, "\x00=") {
			return nil, fmt.Errorf("E_PROVIDER_METADATA_INCOMPLETE")
		}
		if _, duplicate := seenForward[key]; duplicate {
			return nil, fmt.Errorf("E_PROVIDER_METADATA_INCOMPLETE")
		}
		if _, collision := e.Env[key]; collision {
			return nil, fmt.Errorf("E_PROVIDER_ENV_AMBIGUOUS")
		}
		seenForward[key] = struct{}{}
	}
	for _, value := range append(append([]string{e.Command, *e.WorkingDir}, e.Args...), mapValues(e.Env)...) {
		if strings.Contains(value, "${") {
			return nil, fmt.Errorf("E_PROVIDER_VARIABLE_EXPANSION_UNSUPPORTED")
		}
	}
	if _, err := providerProcessIdentityFromEntry(e); err != nil {
		return nil, err
	}
	adoptClients, err := normalizeAdoptClientNames(opts.Clients, e.ProviderClient)
	if err != nil {
		return nil, err
	}
	if len(adoptClients) != 1 || adoptClients[0] != e.ProviderClient {
		return nil, fmt.Errorf("E_PROVIDER_CLIENT_FANOUT_UNSUPPORTED")
	}
	bindings := adoptClientBindingsWithToolTimeout(adoptClients, e.ProviderClient, e.ToolTimeoutSec)
	port := opts.Port
	if port == 0 {
		port, err = pickNextFreeAdoptPort()
		if err != nil {
			return nil, err
		}
	} else if err = validateExplicitAdoptPort(port); err != nil {
		return nil, err
	}
	manifest := renderProviderStdioBridgeManifestYAML(opts.EntryName, e.Command, e.Args, e.Env, e.EnvForwardLocal, *e.WorkingDir, port, bindings, opts.MCPProtocolCompatibilityProfile)
	if _, err = a.ManifestValidateMode(manifest, ValidateModeStrict); err != nil {
		return nil, err
	}
	envKeys := make([]string, 0, len(e.Env))
	for key := range e.Env {
		envKeys = append(envKeys, key)
	}
	sort.Strings(envKeys)
	return &AdoptPlan{EntryName: opts.EntryName, SourceClient: e.ProviderClient, ManifestName: opts.EntryName, Port: port, AdoptClients: adoptClients, MCPProtocolCompatibilityProfile: opts.MCPProtocolCompatibilityProfile, ManifestYAML: manifest, TargetEntryNames: map[string]string{e.ProviderClient: opts.EntryName}, toolTimeoutByClient: adoptToolTimeoutByClient(bindings), providerPreview: &ProviderAdoptPreview{ProviderClient: e.ProviderClient, PluginRef: e.PluginRef, ServerName: e.ServerName, ReceiptFingerprint: e.ReceiptFingerprint, ActivationFingerprint: e.ActivationFingerprint, PolicyFingerprint: e.PolicyFingerprint, EnvKeys: envKeys, WorkingDirPresent: e.WorkingDir != nil, ToolTimeoutSec: e.ToolTimeoutSec, Scope: string(e.Scope)}, providerSource: &ProviderSourceProvenanceV1{ProviderClient: e.ProviderClient, PluginRef: e.PluginRef, ServerName: e.ServerName, Scope: string(e.Scope), ReceiptFingerprint: e.ReceiptFingerprint, ActivationFingerprint: e.ActivationFingerprint, PolicyFingerprint: e.PolicyFingerprint, PriorEnabledPresent: e.ActivationEnabledPresent, PriorEnabled: e.ActivationEnabled, ExpectedDisabledFingerprint: e.DisabledActivationFingerprint, DisablePhase: "disable_planned"}}, nil
}

func mapValues(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, v)
	}
	return out
}

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

type fakeProviderMCPSource struct {
	entries  []clients.ProviderMCPEntryV1
	casCalls *int
}

type providerLifecycleFake struct {
	entry                 clients.ProviderMCPEntryV1
	calls                 int
	driftReceiptOnDisable bool
	failRestore           bool
	onCAS                 func()
}

func TestProviderProcessIdentityWindowsRejectsScriptsAndKeepsNativeCommandArgs(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows native-image admission is platform-specific")
	}
	cwd := t.TempDir()
	for _, name := range []string{"extensionless-shell", "text-named-exe.exe", "script.ps1", "script.sh"} {
		path := filepath.Join(cwd, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 42\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := providerProcessIdentityFromEntry(clients.ProviderMCPEntryV1{Command: path, WorkingDir: &cwd}); err == nil || !strings.Contains(err.Error(), "E_PROVIDER_LIFECYCLE_UNSUPPORTED") {
			t.Fatalf("script %q admission error=%v", name, err)
		}
	}

	native, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	extensionless := filepath.Join(cwd, "native-image")
	if err := os.Link(native, extensionless); err != nil {
		t.Fatalf("create extensionless native hard link: %v", err)
	}
	args := []string{"provider-script.py", "--stdio"}
	identity, err := providerProcessIdentityFromEntry(clients.ProviderMCPEntryV1{Command: extensionless, Args: args, WorkingDir: &cwd})
	if err != nil {
		t.Fatalf("extensionless native command rejected: %v", err)
	}
	if identity.ExecutablePath == "" || !reflect.DeepEqual(identity.Args, args) || !identity.WorkingDirectoryObject.valid {
		t.Fatalf("native identity=%+v, want command, unchanged script arguments, and canonical working-directory key", identity)
	}
}

func (f *providerLifecycleFake) ListProviderMCPEntries(context.Context) ([]clients.ProviderMCPEntryV1, error) {
	return []clients.ProviderMCPEntryV1{f.entry}, nil
}

func (f *providerLifecycleFake) CompareAndSetProviderMCPActivation(_ context.Context, req clients.ProviderMCPActivationCASV1) (clients.ProviderMCPActivationResultV1, error) {
	f.calls++
	if f.onCAS != nil {
		f.onCAS()
	}
	if req.ExpectedActivationFingerprint != f.entry.ActivationFingerprint || req.ExpectedPolicyFingerprint != f.entry.PolicyFingerprint {
		return clients.ProviderMCPActivationResultV1{}, errors.New("fingerprint drift")
	}
	result := clients.ProviderMCPActivationResultV1{PriorEnabledPresent: f.entry.ActivationEnabledPresent, PriorEnabled: f.entry.ActivationEnabled}
	if req.DesiredEnabledPresent && !req.DesiredEnabled {
		f.entry.Enabled = false
		f.entry.ActivationEnabledPresent = true
		f.entry.ActivationEnabled = false
		f.entry.ActivationFingerprint = f.entry.DisabledActivationFingerprint
		if f.driftReceiptOnDisable {
			f.entry.ReceiptFingerprint = "drift"
		}
		result.ActivationFingerprint = f.entry.ActivationFingerprint
		return result, nil
	}
	if f.failRestore {
		return clients.ProviderMCPActivationResultV1{}, errors.New("restore failure")
	}
	f.entry.Enabled = req.DesiredEnabled
	f.entry.ActivationEnabledPresent = req.DesiredEnabledPresent
	f.entry.ActivationEnabled = req.DesiredEnabled
	f.entry.ActivationFingerprint = "activation"
	result.ActivationFingerprint = f.entry.ActivationFingerprint
	return result, nil
}

func TestExecuteProviderAdoptRestoresActivationOnPostCASMetadataDrift(t *testing.T) {
	entryName := "provider-post-cas-drift"
	_, manifestRoot, _ := setupAdoptTestEnv(t, entryName, `[mcp_servers.keep]
command = "go"
args = ["version"]
`)
	cwd := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	provider := &providerLifecycleFake{driftReceiptOnDisable: true, entry: clients.ProviderMCPEntryV1{ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: entryName, Transport: clients.ProviderMCPTransportStdio, Command: exe, WorkingDir: &cwd, Scope: clients.ProviderMCPScopeUser, Enabled: true, ReceiptFingerprint: "receipt", ActivationFingerprint: "activation", ActivationEnabledPresent: true, ActivationEnabled: true, DisabledActivationFingerprint: "disabled", PolicyState: clients.ProviderMCPPolicyNone, PolicyFingerprint: "policy"}}
	plan, err := NewAPI().buildProviderAdoptPlan(AdoptOpts{EntryName: entryName, Client: "codex-cli", ManifestName: entryName, ProviderPluginRef: "fixture@catalog", Port: nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())}, nil, provider)
	if err != nil {
		t.Fatal(err)
	}
	observer := func(context.Context, providerDirectProcessIdentityV1, []providerProcessGenerationV1) providerDirectProcessObservationV1 {
		return providerDirectProcessObservationV1{State: providerProcessObservationComplete}
	}
	err = NewAPI().ExecuteAdoptWithOpts(plan, nil, ExecuteAdoptOpts{providerDeps: providerTransactionDeps{source: provider, observer: observer}})
	if err == nil || !strings.Contains(err.Error(), "E_PROVIDER_SOURCE_CHANGED") {
		t.Fatalf("error=%v", err)
	}
	if provider.calls != 2 || !provider.entry.Enabled {
		t.Fatalf("provider calls=%d entry=%+v", provider.calls, provider.entry)
	}
	if _, statErr := os.Stat(filepath.Join(manifestRoot, entryName, "manifest.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("manifest created despite post-CAS drift: %v", statErr)
	}
	if _, found, readErr := ReadAdoptProvenance(entryName); readErr != nil || found {
		t.Fatalf("provenance after restored metadata drift: found=%t err=%v", found, readErr)
	}
}

func (f fakeProviderMCPSource) ListProviderMCPEntries(context.Context) ([]clients.ProviderMCPEntryV1, error) {
	return f.entries, nil
}
func (f fakeProviderMCPSource) CompareAndSetProviderMCPActivation(context.Context, clients.ProviderMCPActivationCASV1) (clients.ProviderMCPActivationResultV1, error) {
	if f.casCalls == nil {
		panic("planning must not mutate provider activation")
	}
	*f.casCalls++
	return clients.ProviderMCPActivationResultV1{}, nil
}

func TestBuildProviderAdoptPlanReadOnlyValidationMatrix(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("provider lifecycle planning is Windows-only; TestBuildProviderAdoptPlanRejectsUnsupportedObservationPlatform covers the unsupported contract")
	}
	cwd := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	valid := clients.ProviderMCPEntryV1{ProviderClient: "codex-cli", PluginRef: "arbitrary@catalog", ServerName: "reader", Transport: clients.ProviderMCPTransportStdio, Command: exe, Args: []string{"--stdio"}, Env: map[string]string{"MODE": "read"}, EnvForwardLocal: []string{"OPTIONAL_TOKEN"}, WorkingDir: &cwd, ToolTimeoutSec: 41, Scope: clients.ProviderMCPScopeUser, Enabled: true, ReceiptFingerprint: "receipt", ActivationFingerprint: "activation", ActivationEnabledPresent: true, ActivationEnabled: true, DisabledActivationFingerprint: "disabled", PolicyState: clients.ProviderMCPPolicyNone, PolicyFingerprint: "policy"}
	base := AdoptOpts{EntryName: "reader", Client: "codex-cli", ManifestName: "reader", ProviderPluginRef: "arbitrary@catalog", Port: nextBindableAdoptPortForTest(t, collectUsedAdoptPorts()), MCPProtocolCompatibilityProfile: "stdio-http-legacy-2024-11-05", MCPProtocolCompatibilityProfileExplicit: true}
	cases := []struct {
		name    string
		entries []clients.ProviderMCPEntryV1
		want    string
	}{
		{"ambiguous", []clients.ProviderMCPEntryV1{valid, valid}, "E_PROVIDER_ENTRY_AMBIGUOUS"},
		{"disabled", []clients.ProviderMCPEntryV1{func() clients.ProviderMCPEntryV1 { x := valid; x.Enabled = false; return x }()}, "E_PROVIDER_ENTRY_DISABLED"},
		{"http", []clients.ProviderMCPEntryV1{func() clients.ProviderMCPEntryV1 {
			x := valid
			x.Transport = clients.ProviderMCPTransportHTTP
			return x
		}()}, "E_PROVIDER_TRANSPORT_ALREADY_HTTP"},
		{"scope", []clients.ProviderMCPEntryV1{func() clients.ProviderMCPEntryV1 { x := valid; x.Scope = "workspace"; return x }()}, "E_PROVIDER_SCOPE_UNSUPPORTED"},
		{"policy", []clients.ProviderMCPEntryV1{func() clients.ProviderMCPEntryV1 {
			x := valid
			x.PolicyState = clients.ProviderMCPPolicyUnrepresentable
			return x
		}()}, "E_PROVIDER_POLICY_UNREPRESENTABLE"},
		{"empty forwarded name", []clients.ProviderMCPEntryV1{func() clients.ProviderMCPEntryV1 { x := valid; x.EnvForwardLocal = []string{""}; return x }()}, "E_PROVIDER_METADATA_INCOMPLETE"},
		{"nul forwarded name", []clients.ProviderMCPEntryV1{func() clients.ProviderMCPEntryV1 {
			x := valid
			x.EnvForwardLocal = []string{"TOKEN\x00VALUE"}
			return x
		}()}, "E_PROVIDER_METADATA_INCOMPLETE"},
		{"equals forwarded name", []clients.ProviderMCPEntryV1{func() clients.ProviderMCPEntryV1 { x := valid; x.EnvForwardLocal = []string{"TOKEN=VALUE"}; return x }()}, "E_PROVIDER_METADATA_INCOMPLETE"},
		{"duplicate forwarded name", []clients.ProviderMCPEntryV1{func() clients.ProviderMCPEntryV1 {
			x := valid
			x.EnvForwardLocal = []string{"TOKEN", "TOKEN"}
			return x
		}()}, "E_PROVIDER_METADATA_INCOMPLETE"},
		{"env collision", []clients.ProviderMCPEntryV1{func() clients.ProviderMCPEntryV1 { x := valid; x.EnvForwardLocal = []string{"MODE"}; return x }()}, "E_PROVIDER_ENV_AMBIGUOUS"},
		{"static expansion", []clients.ProviderMCPEntryV1{func() clients.ProviderMCPEntryV1 { x := valid; x.Env = map[string]string{"MODE": "${HOME}"}; return x }()}, "E_PROVIDER_STATIC_ENV_UNREPRESENTABLE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (&API{}).buildProviderAdoptPlan(base, nil, fakeProviderMCPSource{entries: tc.entries})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v want %s", err, tc.want)
			}
		})
	}
	wrapper := filepath.Join(t.TempDir(), "launcher.cmd")
	if err := os.WriteFile(wrapper, []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrapperEntry := valid
	wrapperEntry.Command = wrapper
	if _, err := (&API{}).buildProviderAdoptPlan(base, nil, fakeProviderMCPSource{entries: []clients.ProviderMCPEntryV1{wrapperEntry}}); err == nil || !strings.Contains(err.Error(), "E_PROVIDER_LIFECYCLE_UNSUPPORTED") {
		t.Fatalf("wrapper plan error=%v, want E_PROVIDER_LIFECYCLE_UNSUPPORTED", err)
	}
	plan, err := (&API{}).buildProviderAdoptPlan(base, nil, fakeProviderMCPSource{entries: []clients.ProviderMCPEntryV1{valid}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.MCPProtocolCompatibilityProfile != "stdio-http-legacy-2024-11-05" {
		t.Fatalf("provider plan profile=%q", plan.MCPProtocolCompatibilityProfile)
	}
	if _, err := (&API{}).buildProviderAdoptPlan(func() AdoptOpts { x := base; x.Clients = []string{"codex-cli", "claude-code"}; return x }(), nil, fakeProviderMCPSource{entries: []clients.ProviderMCPEntryV1{valid}}); err == nil || !strings.Contains(err.Error(), "E_PROVIDER_CLIENT_FANOUT_UNSUPPORTED") {
		t.Fatalf("provider fanout error=%v", err)
	}
	if _, err := (&API{}).buildProviderAdoptPlan(func() AdoptOpts { x := base; x.Clients = []string{"codex-cli"}; return x }(), nil, fakeProviderMCPSource{entries: []clients.ProviderMCPEntryV1{valid}}); err != nil {
		t.Fatalf("explicit source-only provider selection: %v", err)
	}
	manifest, err := config.ParseManifest(strings.NewReader(plan.ManifestYAML))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.EnvForwardLocal[0] != "OPTIONAL_TOKEN" || manifest.Daemons[0].Cwd != cwd || manifest.ClientBindings[0].ToolTimeoutSec != 41 {
		t.Fatalf("manifest lost provider fields: %#v", manifest)
	}
}

func TestBuildProviderAdoptPlanRejectsUnsupportedObservationPlatform(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("provider process observation is available on Windows")
	}
	cwd := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	entry := clients.ProviderMCPEntryV1{
		ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: "linux-provider",
		Transport: clients.ProviderMCPTransportStdio, Command: exe, WorkingDir: &cwd,
		Scope: clients.ProviderMCPScopeUser, Enabled: true, ReceiptFingerprint: "receipt",
		ActivationFingerprint: "activation", ActivationEnabledPresent: true, ActivationEnabled: true,
		DisabledActivationFingerprint: "disabled", PolicyState: clients.ProviderMCPPolicyNone, PolicyFingerprint: "policy",
	}
	_, err = (&API{}).buildProviderAdoptPlan(AdoptOpts{
		EntryName: entry.ServerName, Client: entry.ProviderClient, ManifestName: entry.ServerName,
		ProviderPluginRef: entry.PluginRef, Port: nextBindableAdoptPortForTest(t, collectUsedAdoptPorts()),
	}, nil, fakeProviderMCPSource{entries: []clients.ProviderMCPEntryV1{entry}})
	if err == nil || !strings.Contains(err.Error(), "E_PROVIDER_LIFECYCLE_UNSUPPORTED") {
		t.Fatalf("non-Windows provider plan error=%v, want E_PROVIDER_LIFECYCLE_UNSUPPORTED", err)
	}
}

func TestAdoptPlanProviderPreviewIsNotSerialized(t *testing.T) {
	plan := AdoptPlan{EntryName: "reader", providerPreview: &ProviderAdoptPreview{ProviderClient: "codex-cli", PluginRef: "fixture@catalog", EnvKeys: []string{"TOKEN"}}}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "ProviderPreview") || strings.Contains(string(raw), "fixture@catalog") || strings.Contains(string(raw), "TOKEN") {
		t.Fatalf("provider preview leaked into plan JSON: %s", raw)
	}
}

func TestPrintAdoptPlanProviderPreviewIsRedacted(t *testing.T) {
	plan := AdoptPlan{EntryName: "reader", providerPreview: &ProviderAdoptPreview{ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: "reader", ReceiptFingerprint: "receipt", ActivationFingerprint: "activation", PolicyFingerprint: "policy", EnvKeys: []string{"STATIC_TOKEN", "OPTIONAL_TOKEN"}, WorkingDirPresent: true, ToolTimeoutSec: 41, Scope: "user"}}
	var out bytes.Buffer
	PrintAdoptPlan(&out, &plan)
	printed := out.String()
	for _, want := range []string{"provider plugin: fixture@catalog", "provider env keys: STATIC_TOKEN,OPTIONAL_TOKEN", "provider working_dir_present: true", "provider tool timeout seconds: 41", "provider scope: user"} {
		if !strings.Contains(printed, want) {
			t.Fatalf("preview missing %q: %s", want, printed)
		}
	}
}

func TestExecuteProviderAdoptPreflightFailureSettlesLeaseBeforeProviderMutation(t *testing.T) {
	entry := "provider-execution-refused"
	codexPath, manifestRoot, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.keep]
command = "provider-tool"
args = ["--stdio"]
`)
	cwd := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	casCalls := 0
	provider := fakeProviderMCPSource{
		entries: []clients.ProviderMCPEntryV1{{
			ProviderClient:                "codex-cli",
			PluginRef:                     "arbitrary@catalog",
			ServerName:                    entry,
			Transport:                     clients.ProviderMCPTransportStdio,
			Command:                       exe,
			Args:                          []string{"--stdio"},
			Env:                           map[string]string{"MODE": "read"},
			EnvForwardLocal:               []string{"OPTIONAL_TOKEN"},
			WorkingDir:                    &cwd,
			ToolTimeoutSec:                41,
			Scope:                         clients.ProviderMCPScopeUser,
			Enabled:                       true,
			ReceiptFingerprint:            "receipt",
			ActivationFingerprint:         "activation",
			ActivationEnabledPresent:      true,
			ActivationEnabled:             true,
			DisabledActivationFingerprint: "disabled",
			PolicyState:                   clients.ProviderMCPPolicyNone,
			PolicyFingerprint:             "policy",
		}},
		casCalls: &casCalls,
	}
	plan, err := NewAPI().buildProviderAdoptPlan(AdoptOpts{
		EntryName:         entry,
		Client:            "codex-cli",
		ManifestName:      entry,
		ProviderPluginRef: "arbitrary@catalog",
		Port:              nextBindableAdoptPortForTest(t, collectUsedAdoptPorts()),
	}, nil, provider)
	if err != nil {
		t.Fatalf("buildProviderAdoptPlan: %v", err)
	}
	if plan == nil || plan.providerSource == nil {
		t.Fatalf("provider plan = %#v, want non-nil provider provenance", plan)
	}
	provider.entries[0].PolicyFingerprint = "source-drift"

	clientRoot := filepath.Dir(filepath.Dir(codexPath))
	beforeClient := snapshotRegularFiles(t, clientRoot)
	beforeManifest := snapshotRegularFiles(t, manifestRoot)
	result, err := NewAPI().ExecuteAdoptResultWithOpts(plan, nil, ExecuteAdoptOpts{providerDeps: providerTransactionDeps{source: provider}})
	if err == nil {
		t.Fatal("ExecuteAdoptResultWithOpts accepted provider plan")
	}
	var staged *AdoptStageError
	if !errors.As(err, &staged) || staged.Stage != "provider-revalidate" || staged.CommitState != "uncommitted" {
		t.Fatalf("error = %v, staged = %#v, want provider-revalidate/uncommitted", err, staged)
	}
	if staged.Cause == nil || !strings.Contains(staged.Cause.Error(), "E_PROVIDER_SOURCE_CHANGED") {
		t.Fatalf("cause = %v, want E_PROVIDER_SOURCE_CHANGED", staged.Cause)
	}
	if casCalls != 0 {
		t.Fatalf("provider CAS calls=%d, want zero", casCalls)
	}
	if !reflect.DeepEqual(result, AdoptExecutionResultV1{}) {
		t.Fatalf("execution result mutated before refusal: %#v", result)
	}
	if after := snapshotRegularFiles(t, clientRoot); !reflect.DeepEqual(after, beforeClient) {
		t.Fatalf("client fixture mutated\nbefore=%#v\nafter=%#v", beforeClient, after)
	}
	if after := snapshotRegularFiles(t, manifestRoot); !reflect.DeepEqual(after, beforeManifest) {
		t.Fatalf("manifest fixture mutated\nbefore=%#v\nafter=%#v", beforeManifest, after)
	}
	leasePath, err := adoptManifestLeasePath(entry)
	if err != nil {
		t.Fatalf("adoptManifestLeasePath: %v", err)
	}
	if _, err := os.Lstat(leasePath); !os.IsNotExist(err) {
		t.Fatalf("provider preflight refusal left lease file behind: %v", err)
	}
	lease, acquired, acquireErr := tryAcquireAdoptManifestLease(entry)
	if acquireErr != nil || !acquired {
		t.Fatalf("provider preflight refusal left lease handle held: acquired=%v err=%v", acquired, acquireErr)
	}
	if releaseErr := lease.ReleaseAndRemove(); releaseErr != nil {
		t.Fatalf("release verification lease: %v", releaseErr)
	}
}

func TestExecuteProviderAdoptRollsBackActivationWhenDirectRootSurvives(t *testing.T) {
	entryName := "provider-direct-root"
	_, manifestRoot, _ := setupAdoptTestEnv(t, entryName, `[mcp_servers.keep]
command = "go"
args = ["version"]
`)
	cwd := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	provider := &providerLifecycleFake{entry: clients.ProviderMCPEntryV1{ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: entryName, Transport: clients.ProviderMCPTransportStdio, Command: exe, WorkingDir: &cwd, Scope: clients.ProviderMCPScopeUser, ToolTimeoutSec: 30, Enabled: true, ReceiptFingerprint: "receipt", ActivationFingerprint: "activation", ActivationEnabledPresent: true, ActivationEnabled: true, DisabledActivationFingerprint: "disabled", PolicyState: clients.ProviderMCPPolicyNone, PolicyFingerprint: "policy"}}
	plan, err := NewAPI().buildProviderAdoptPlan(AdoptOpts{EntryName: entryName, Client: "codex-cli", ManifestName: entryName, ProviderPluginRef: "fixture@catalog", Port: nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())}, nil, provider)
	if err != nil {
		t.Fatal(err)
	}
	observations := 0
	observer := func(context.Context, providerDirectProcessIdentityV1, []providerProcessGenerationV1) providerDirectProcessObservationV1 {
		observations++
		if observations == 1 {
			return providerDirectProcessObservationV1{State: providerProcessObservationComplete}
		}
		return providerDirectProcessObservationV1{State: providerProcessObservationComplete, Active: []providerProcessGenerationV1{{PID: 7, RootPID: 7, StartedAt: time.Now(), ExecutablePath: exe, CommandLine: exe}}}
	}
	err = NewAPI().ExecuteAdoptWithOpts(plan, nil, ExecuteAdoptOpts{providerDeps: providerTransactionDeps{source: provider, observer: observer}})
	if err == nil || !strings.Contains(err.Error(), "E_PROVIDER_SOURCE_STILL_RUNNING") {
		t.Fatalf("error=%v", err)
	}
	if provider.calls != 2 || provider.entry.Enabled != true || provider.entry.ActivationFingerprint != "activation" {
		t.Fatalf("provider calls=%d entry=%+v", provider.calls, provider.entry)
	}
	if _, statErr := os.Stat(filepath.Join(manifestRoot, entryName, "manifest.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("manifest created despite surviving direct root: %v", statErr)
	}
	if _, found, readErr := ReadAdoptProvenance(entryName); readErr != nil || found {
		t.Fatalf("provenance after restored pre-install refusal: found=%t err=%v", found, readErr)
	}
}

func TestExecuteProviderAdoptPreservesProvenanceWhenPostDisableRestoreFails(t *testing.T) {
	entryName := "provider-restore-failure"
	_, manifestRoot, stateRoot := setupAdoptTestEnv(t, entryName, `[mcp_servers.keep]
command = "go"
args = ["version"]

[mcp_servers.provider-restore-failure]
command = "provider-tool"
args = ["--stdio"]
`)
	cwd := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	provider := &providerLifecycleFake{failRestore: true, entry: clients.ProviderMCPEntryV1{ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: entryName, Transport: clients.ProviderMCPTransportStdio, Command: exe, WorkingDir: &cwd, Scope: clients.ProviderMCPScopeUser, Enabled: true, ReceiptFingerprint: "receipt", ActivationFingerprint: "activation", ActivationEnabledPresent: true, ActivationEnabled: true, DisabledActivationFingerprint: "disabled", PolicyState: clients.ProviderMCPPolicyNone, PolicyFingerprint: "policy"}}
	plan, err := NewAPI().buildProviderAdoptPlan(AdoptOpts{EntryName: entryName, Client: "codex-cli", ManifestName: entryName, ProviderPluginRef: "fixture@catalog", Port: nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())}, nil, provider)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	observer := func(context.Context, providerDirectProcessIdentityV1, []providerProcessGenerationV1) providerDirectProcessObservationV1 {
		calls++
		if calls == 1 {
			return providerDirectProcessObservationV1{State: providerProcessObservationComplete}
		}
		return providerDirectProcessObservationV1{State: providerProcessObservationComplete, Active: []providerProcessGenerationV1{{PID: 7, RootPID: 7, StartedAt: time.Now(), ExecutablePath: exe, CommandLine: exe}}}
	}
	err = NewAPI().ExecuteAdoptWithOpts(plan, nil, ExecuteAdoptOpts{providerDeps: providerTransactionDeps{source: provider, observer: observer}})
	if err == nil || !strings.Contains(err.Error(), "E_PROVIDER_RECOVERY_REQUIRED") {
		t.Fatalf("error=%v", err)
	}
	if provider.entry.Enabled || provider.entry.ActivationFingerprint != "disabled" {
		t.Fatalf("provider unexpectedly restored: %+v", provider.entry)
	}
	if _, err := os.Stat(filepath.Join(manifestRoot, entryName, "manifest.yaml")); !os.IsNotExist(err) {
		t.Fatalf("manifest created: %v", err)
	}
	rec, found, readErr := ReadAdoptProvenance(entryName)
	if readErr != nil || !found || rec.ProviderSource == nil || rec.ProviderSource.DisablePhase != "disable_applied" {
		t.Fatalf("recovery record=%+v found=%t err=%v", rec, found, readErr)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, adoptProvenanceSnapshotSubdir, entryName)); err != nil {
		t.Fatalf("provider recovery snapshots missing: %v", err)
	}
}

func TestExecuteProviderAdoptTracksPreCASGenerationAcrossReparent(t *testing.T) {
	entryName := "provider-reparented-child"
	_, manifestRoot, _ := setupAdoptTestEnv(t, entryName, `[mcp_servers.keep]
command = "go"
args = ["version"]
`)
	cwd := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	provider := &providerLifecycleFake{entry: clients.ProviderMCPEntryV1{ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: entryName, Transport: clients.ProviderMCPTransportStdio, Command: exe, WorkingDir: &cwd, Scope: clients.ProviderMCPScopeUser, Enabled: true, ReceiptFingerprint: "receipt", ActivationFingerprint: "activation", ActivationEnabledPresent: true, ActivationEnabled: true, DisabledActivationFingerprint: "disabled", PolicyState: clients.ProviderMCPPolicyNone, PolicyFingerprint: "policy"}}
	plan, err := NewAPI().buildProviderAdoptPlan(AdoptOpts{EntryName: entryName, Client: "codex-cli", ManifestName: entryName, ProviderPluginRef: "fixture@catalog", Port: nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())}, nil, provider)
	if err != nil {
		t.Fatal(err)
	}
	pre := providerProcessGenerationV1{PID: 71, ParentPID: 1, RootPID: 71, StartedAt: time.Now(), ExecutablePath: exe, CommandLine: exe}
	calls := 0
	observer := func(_ context.Context, _ providerDirectProcessIdentityV1, prior []providerProcessGenerationV1) providerDirectProcessObservationV1 {
		calls++
		if calls == 1 {
			return providerDirectProcessObservationV1{State: providerProcessObservationComplete, Active: []providerProcessGenerationV1{pre}}
		}
		if len(prior) != 1 || prior[0].PID != pre.PID {
			t.Fatalf("post-CAS prior=%+v, want captured generation", prior)
		}
		return providerDirectProcessObservationV1{State: providerProcessObservationComplete, Active: []providerProcessGenerationV1{{PID: pre.PID, ParentPID: 999, RootPID: pre.RootPID, StartedAt: pre.StartedAt, ExecutablePath: pre.ExecutablePath, CommandLine: pre.CommandLine}}}
	}
	err = NewAPI().ExecuteAdoptWithOpts(plan, nil, ExecuteAdoptOpts{providerDeps: providerTransactionDeps{source: provider, observer: observer}})
	if err == nil || !strings.Contains(err.Error(), "E_PROVIDER_SOURCE_STILL_RUNNING") {
		t.Fatalf("error=%v", err)
	}
	if provider.calls != 2 || !provider.entry.Enabled {
		t.Fatalf("provider=%+v calls=%d", provider.entry, provider.calls)
	}
	if _, err := os.Stat(filepath.Join(manifestRoot, entryName, "manifest.yaml")); !os.IsNotExist(err) {
		t.Fatalf("manifest created: %v", err)
	}
}

func TestExecuteProviderAdoptCommitsDisabledProviderAndRepeatIsNoop(t *testing.T) {
	entryName := "provider-apply-repeat"
	codexPath, manifestRoot, stateRoot := setupAdoptTestEnv(t, entryName, `[mcp_servers.keep]
command = "go"
args = ["version"]
`)
	preparePreflightBinaryChecks(t)
	installFakeScheduler(t, newInstallFakeScheduler())
	installFakeAutostartBackend(t, &fakeInstallAutostartBackend{})
	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(context.Context, bool) (ReconcileResponse, error) { return ReconcileResponse{}, nil }))
	cwd := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	provider := &providerLifecycleFake{entry: clients.ProviderMCPEntryV1{ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: entryName, Transport: clients.ProviderMCPTransportStdio, Command: exe, WorkingDir: &cwd, Scope: clients.ProviderMCPScopeUser, ToolTimeoutSec: 30, Enabled: true, ReceiptFingerprint: "receipt", ActivationFingerprint: "activation", ActivationEnabledPresent: true, ActivationEnabled: true, DisabledActivationFingerprint: "disabled", PolicyState: clients.ProviderMCPPolicyNone, PolicyFingerprint: "policy"}}
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().buildProviderAdoptPlan(AdoptOpts{EntryName: entryName, Client: "codex-cli", ManifestName: entryName, ProviderPluginRef: "fixture@catalog", Port: port}, nil, provider)
	if err != nil {
		t.Fatal(err)
	}
	observer := func(context.Context, providerDirectProcessIdentityV1, []providerProcessGenerationV1) providerDirectProcessObservationV1 {
		return providerDirectProcessObservationV1{State: providerProcessObservationComplete}
	}
	if err := NewAPI().ExecuteAdoptWithOpts(plan, nil, ExecuteAdoptOpts{providerDeps: providerTransactionDeps{source: provider, observer: observer}}); err != nil {
		t.Fatalf("provider apply: %v", err)
	}
	if provider.calls != 1 || provider.entry.Enabled || provider.entry.ActivationFingerprint != "disabled" {
		t.Fatalf("provider after apply=%+v", provider.entry)
	}
	rec, found, err := ReadAdoptProvenance(entryName)
	if err != nil || !found || rec.ProviderSource == nil || rec.ProviderSource.DisablePhase != "disable_applied" {
		t.Fatalf("provider provenance=%+v found=%t err=%v", rec, found, err)
	}
	if len(rec.Clients) != 1 || rec.Clients[0].OriginalState != AdoptOriginalStateAbsent || rec.Clients[0].ToolTimeoutSec != 30 {
		t.Fatalf("absent-source provenance=%+v; want one absent client with expected timeout 30", rec.Clients)
	}
	if _, err := os.Stat(filepath.Join(manifestRoot, entryName, "manifest.yaml")); err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
	repeat, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entryName, Client: "codex-cli", ManifestName: entryName, ProviderPluginRef: "fixture@catalog", Port: port})
	if err != nil || !repeat.alreadyAdopted {
		t.Fatalf("repeat plan=%+v err=%v", repeat, err)
	}
	if _, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entryName, Client: "codex-cli", ManifestName: entryName, ProviderPluginRef: "different@catalog", Port: port}); err == nil || !strings.Contains(err.Error(), "provider plugin") {
		t.Fatalf("provider repeat plugin mismatch error=%v", err)
	}
	if err := NewAPI().ExecuteAdoptWithOpts(repeat, nil, ExecuteAdoptOpts{providerDeps: providerTransactionDeps{source: provider, observer: observer}}); err != nil {
		t.Fatalf("repeat execute: %v", err)
	}
	if provider.calls != 1 {
		t.Fatalf("repeat touched provider activation: calls=%d", provider.calls)
	}
	raw, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	drifted := bytes.Replace(raw, []byte("tool_timeout_sec = 30.0"), []byte("tool_timeout_sec = 31.0"), 1)
	if bytes.Equal(drifted, raw) {
		t.Fatal("provider fixture did not contain the expected timeout 30")
	}
	if err := os.WriteFile(codexPath, drifted, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewAPI().ExecuteAdoptWithOpts(repeat, nil, ExecuteAdoptOpts{providerDeps: providerTransactionDeps{source: provider, observer: observer}}); err == nil || !strings.Contains(err.Error(), "existing adopted receiver state does not match its durable receipt") {
		t.Fatalf("timeout-drift repeat error=%v", err)
	}
	if err := os.WriteFile(codexPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	provider.entry.Enabled = true
	provider.entry.ActivationFingerprint = "activation"
	if err := NewAPI().ExecuteAdoptWithOpts(repeat, nil, ExecuteAdoptOpts{providerDeps: providerTransactionDeps{source: provider, observer: observer}}); err == nil || !strings.Contains(err.Error(), "E_PROVIDER_SOURCE_CHANGED") {
		t.Fatalf("re-enabled provider repeat error=%v", err)
	}
	if provider.calls != 1 {
		t.Fatalf("re-enabled repeat touched provider activation: calls=%d", provider.calls)
	}
	_ = stateRoot
}

func TestExpectedAdoptBindingsRequireExactlyOneRecordedClientBinding(t *testing.T) {
	rec := &AdoptProvenanceRecord{ManifestName: "binding-contract", AdoptClients: []string{"codex-cli"}}
	valid := config.ClientBinding{Client: "codex-cli", Daemon: adoptDefaultDaemonName, URLPath: adoptDefaultURLPath, ToolTimeoutSec: 30}
	if bindings, err := expectedAdoptBindings(rec, []config.ClientBinding{valid}); err != nil || len(bindings) != 1 || bindings[0].ToolTimeoutSec != 30 {
		t.Fatalf("exact binding = %#v, %v", bindings, err)
	}
	if _, err := expectedAdoptBindings(rec, nil); err == nil {
		t.Fatal("missing binding was accepted")
	}
	if _, err := expectedAdoptBindings(rec, []config.ClientBinding{valid, valid}); err == nil {
		t.Fatal("ambiguous binding was accepted")
	}
	legacy := config.ClientBinding{Client: "codex-cli", Daemon: adoptDefaultDaemonName, URLPath: adoptDefaultURLPath}
	if bindings, err := expectedAdoptBindings(rec, []config.ClientBinding{legacy}); err != nil || len(bindings) != 1 || bindings[0].ToolTimeoutSec != 0 {
		t.Fatalf("legacy zero binding = %#v, %v", bindings, err)
	}
}

func TestBuildAdoptPlanExistingGenericRefusesProviderFlag(t *testing.T) {
	entry := "generic-provider-flag"
	_, manifestRoot, _ := setupAdoptTestEnv(t, entry, "[mcp_servers]\n")
	manifest := renderStdioBridgeManifestYAML(entry, "go", []string{"version"}, nil, 9367, adoptClientBindings([]string{"codex-cli"}))
	if err := os.MkdirAll(filepath.Join(manifestRoot, entry), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestRoot, entry, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	hash := ManifestHashContent([]byte(manifest))
	if err := writeAdoptedEntries(&AdoptedEntries{Records: []AdoptProvenanceRecord{{ManifestName: entry, SourceClient: "codex-cli", SourceEntryName: entry, Port: 9367, AdoptClients: []string{"codex-cli"}, AdoptManifestHash: hash, ExpectedManifestHash: hash, OperationState: AdoptOperationStateAdopted, Clients: []AdoptClientProvenance{{Client: "codex-cli", TargetEntryName: entry, OriginalState: AdoptOriginalStateAbsent, RestoreMode: AdoptRestoreModeNA}}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry, ProviderPluginRef: "other@catalog"}); err == nil || !strings.Contains(err.Error(), "provider plugin") {
		t.Fatalf("generic existing provider flag error=%v", err)
	}
}

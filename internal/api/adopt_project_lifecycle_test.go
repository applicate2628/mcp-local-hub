package api

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
)

type adoptLifecycleTestClient struct {
	clients.Client
	entry    *clients.MCPEntry
	getNames []string
}

func cleanupProjectAdoptTestManifest(t *testing.T, manifestName string) string {
	t.Helper()
	if err := CheckManifestName(manifestName); err != nil {
		t.Fatalf("invalid test manifest name %q: %v", manifestName, err)
	}
	root := filepath.Clean(mustCanonicalManifestDir(t))
	if !filepath.IsAbs(root) {
		t.Fatalf("test manifest root is not absolute: %q", root)
	}
	target := filepath.Clean(filepath.Join(root, manifestName))
	relative, err := filepath.Rel(root, target)
	if err != nil || relative != manifestName || filepath.Dir(target) != root {
		t.Fatalf("test manifest target %q escapes private root %q", target, root)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(target); err != nil {
			t.Errorf("remove exact test manifest %q: %v", manifestName, err)
		}
	})
	return target
}

func (c *adoptLifecycleTestClient) GetEntry(name string) (*clients.MCPEntry, error) {
	c.getNames = append(c.getNames, name)
	return c.entry, nil
}

func TestProjectAdoptProvenanceCaptureUsesPinnedBytesAndPersistsTarget(t *testing.T) {
	isolateStateDir(t)
	projectRoot := writeProjectAdoptConfig(t, validProjectAdoptConfig)
	source, err := extractCodexProjectAdoptSource(projectRoot, "demo")
	if err != nil {
		t.Fatal(err)
	}
	pinned := bytes.Clone(source.configBytes)
	plan := &AdoptPlan{
		EntryName:        "demo",
		SourceClient:     "codex-cli",
		ManifestName:     "demo-project-" + source.identity.ProjectIdentitySHA256()[:8],
		AdoptClients:     []string{"codex-cli"},
		TargetEntryNames: map[string]string{"codex-cli": "demo"},
		presentAtBuild:   []string{"codex-cli"},
		clientTargets: map[string]adoptClientTargetRef{
			"codex-cli": {clientName: "codex-cli", targetEntryName: "demo", projectSource: source},
		},
	}
	globalCalls, projectCalls := 0, 0
	resolver := adoptClientTargetResolver{
		globalCatalogFactory: func() map[string]clients.Client {
			globalCalls++
			return nil
		},
		projectClientFactory: func(root, entry string) (clients.Client, error) {
			projectCalls++
			return &adoptTargetTestClient{name: "codex-cli"}, nil
		},
	}

	got, err := captureAdoptClientsProvenanceWithResolver(plan, resolver)
	if err != nil {
		t.Fatalf("capture project provenance: %v", err)
	}
	if globalCalls != 0 || projectCalls != 1 {
		t.Fatalf("resolver calls global=%d project=%d, want 0/1", globalCalls, projectCalls)
	}
	if len(got) != 1 || got[0].ConfigTarget == nil {
		t.Fatalf("project provenance = %#v, want one config_target", got)
	}
	target := got[0].ConfigTarget
	if target.Kind != adoptClientConfigTargetKindCodexProject || target.ProjectRoot != projectRoot ||
		target.ProjectIdentitySHA256 != source.identity.ProjectIdentitySHA256() {
		t.Fatalf("config_target = %#v", target)
	}
	stateDir, err := DaemonStateDir()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := os.ReadFile(filepath.Join(stateDir, filepath.FromSlash(got[0].SnapshotRef)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(snapshot, pinned) {
		t.Fatalf("snapshot bytes = %q, want pinned %q", snapshot, pinned)
	}
}

func TestProjectAdoptRelocationUsesExactProjectTargetWithoutGlobalCatalog(t *testing.T) {
	isolateStateDir(t)
	projectRoot := writeProjectAdoptConfig(t, validProjectAdoptConfig)
	source, err := extractCodexProjectAdoptSource(projectRoot, "demo")
	if err != nil {
		t.Fatal(err)
	}
	plan := &AdoptPlan{
		EntryName:        "demo",
		SourceClient:     "codex-cli",
		Port:             9137,
		AdoptClients:     []string{"codex-cli"},
		TargetEntryNames: map[string]string{"codex-cli": "demo"},
		projectSource:    source,
		clientTargets: map[string]adoptClientTargetRef{
			"codex-cli": {clientName: "codex-cli", targetEntryName: "demo", projectSource: source},
		},
	}
	globalCalls := 0
	resolver := newAdoptClientTargetResolver()
	resolver.globalCatalogFactory = func() map[string]clients.Client {
		globalCalls++
		return nil
	}

	if !plan.hasCodexTransportRelocation() {
		t.Fatal("project plan did not select its dedicated Codex mutation")
	}
	result, err := applyCodexAdoptTargetWithResolver(plan, resolver)
	if err != nil {
		t.Fatalf("apply project target: %v", err)
	}
	if globalCalls != 0 {
		t.Fatalf("global catalog calls = %d, want 0", globalCalls)
	}
	if result.WriteTarget != "codex_project" || result.Action != "replace" || result.CollisionReason != "" ||
		result.Outcome != clients.CodexHTTPRelocationCommitted || result.Readback != clients.CodexHTTPRelocationReadbackExact {
		t.Fatalf("project relocation result = %#v", result)
	}
	client, _, err := clients.NewCodexCLIForProject(projectRoot, "demo", NewCodexProjectConfigMutationOwner)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := client.GetEntry("demo")
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil || entry.URL != "http://127.0.0.1:9137/mcp" || entry.Disabled {
		t.Fatalf("project entry = %#v", entry)
	}
}

func TestProjectAdoptReceivingReadbackUsesExactProjectResolver(t *testing.T) {
	projectRoot := writeProjectAdoptConfig(t, validProjectAdoptConfig)
	source, err := extractCodexProjectAdoptSource(projectRoot, "demo")
	if err != nil {
		t.Fatal(err)
	}
	plan := &AdoptPlan{
		EntryName:        "demo",
		SourceClient:     "codex-cli",
		Port:             9137,
		AdoptClients:     []string{"codex-cli"},
		TargetEntryNames: map[string]string{"codex-cli": "demo"},
		projectSource:    source,
		clientTargets: map[string]adoptClientTargetRef{
			"codex-cli": {clientName: "codex-cli", targetEntryName: "demo", projectSource: source},
		},
	}
	wantURL := "http://127.0.0.1:9137/mcp"
	client := &adoptLifecycleTestClient{entry: &clients.MCPEntry{Name: "demo", URL: wantURL}}
	globalCalls, projectCalls := 0, 0
	resolver := adoptClientTargetResolver{
		globalCatalogFactory: func() map[string]clients.Client {
			globalCalls++
			return nil
		},
		projectClientFactory: func(root, entry string) (clients.Client, error) {
			projectCalls++
			return client, nil
		},
	}

	if err := verifyAdoptClientReadbacksWithResolver(plan, wantURL, resolver); err != nil {
		t.Fatalf("verify project client readback: %v", err)
	}
	if globalCalls != 0 || projectCalls != 1 || len(client.getNames) != 1 || client.getNames[0] != "demo" {
		t.Fatalf("resolver calls global=%d project=%d get=%v", globalCalls, projectCalls, client.getNames)
	}
}

func TestProjectAdoptExactRepeatIsVerificationOnly(t *testing.T) {
	fixture := newProjectAdoptPlanFixture(t, "project-repeat", validProjectAdoptConfig)
	t.Cleanup(SetDaemonStateRootForTest(hardenedTempDir(t)))
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	opts := projectAdoptPlanOpts(fixture, "demo", port)
	opts.Clients = nil
	plan, err := NewAPI().BuildAdoptPlan(opts)
	if err != nil {
		t.Fatalf("BuildAdoptPlan fresh: %v", err)
	}
	cleanupProjectAdoptTestManifest(t, plan.ManifestName)
	if _, err := NewAPI().PreflightAdoptPlan(plan); err != nil {
		t.Fatalf("PreflightAdoptPlan fresh: %v", err)
	}
	installCalls := 0
	var capturedInstall InstallOpts
	result, err := NewAPI().ExecuteAdoptResultWithOpts(plan, nil, ExecuteAdoptOpts{
		ReceivingVerifier: func(context.Context, *API, *AdoptPlan, *AdoptProvenanceRecord) error { return nil },
		installFn: func(_ context.Context, _ *API, got InstallOpts) error {
			installCalls++
			capturedInstall = got
			return nil
		},
	})
	if err != nil {
		t.Fatalf("ExecuteAdopt fresh: %v", err)
	}
	if installCalls != 1 || !capturedInstall.SkipClientConfigWrites || len(capturedInstall.ClientsInclude) != 0 ||
		len(plan.AdoptClients) != 1 || plan.AdoptClients[0] != "codex-cli" {
		t.Fatalf("project default install calls=%d skip=%v clients=%v planned=%v", installCalls, capturedInstall.SkipClientConfigWrites, capturedInstall.ClientsInclude, plan.AdoptClients)
	}
	if len(result.ClientConfigSettlements) != 1 {
		t.Fatalf("project settlements = %#v, want one", result.ClientConfigSettlements)
	}
	settlement := result.ClientConfigSettlements[0]
	if settlement.SchemaVersion != clientConfigSettlementSchemaV2 || settlement.WriteTarget != "codex_project" ||
		settlement.ProjectIdentitySHA256 != plan.projectSource.identity.ProjectIdentitySHA256() {
		t.Fatalf("project settlement = %#v", settlement)
	}

	// Windows repeat must use the same identity despite a different spelling.
	// The rest of this test verifies the repeat stays verification-only.
	if runtime.GOOS == "windows" {
		opts.CodexProjectSourceRoot = strings.ToUpper(fixture.projectRoot)
		if opts.CodexProjectSourceRoot == fixture.projectRoot {
			opts.CodexProjectSourceRoot = strings.ToLower(fixture.projectRoot)
		}
	}
	repeat, err := NewAPI().BuildAdoptPlan(opts)
	if err != nil {
		t.Fatalf("BuildAdoptPlan repeat: %v", err)
	}
	if !repeat.alreadyAdopted || repeat.projectSource != nil || repeat.projectRepeat == nil {
		t.Fatalf("repeat markers already=%v fresh=%v repeat=%v", repeat.alreadyAdopted, repeat.projectSource != nil, repeat.projectRepeat != nil)
	}
	if err := repeat.ValidateProjectSourcePreimageToken(repeat.ProjectSourcePreimageToken()); err != nil {
		t.Fatalf("validate repeat token: %v", err)
	}
	var preview bytes.Buffer
	PrintAdoptPlan(&preview, repeat)
	for _, forbidden := range []string{
		"global write target",
		"project read-only",
		"replace project entry with HTTP relay",
		fixture.projectRoot,
	} {
		if strings.Contains(preview.String(), forbidden) {
			t.Fatalf("repeat dry-run output exposed stale or sensitive text %q:\n%s", forbidden, preview.String())
		}
	}

	stateDir, err := DaemonStateDir()
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{
		fixture.projectConfig,
		filepath.Join(mustCanonicalManifestDir(t), plan.ManifestName, "manifest.yaml"),
		filepath.Join(stateDir, adoptedEntriesFileLeaf),
		filepath.Join(stateDir, SupervisorEventLogFileLeaf),
	}
	before := make(map[string][]byte, len(paths))
	for _, path := range paths {
		before[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatalf("read repeat preimage %s: %v", filepath.Base(path), err)
		}
	}
	var out bytes.Buffer
	if err := NewAPI().ExecuteAdoptWithOpts(repeat, &out, ExecuteAdoptOpts{
		installFn: func(context.Context, *API, InstallOpts) error {
			t.Fatal("repeat invoked Install")
			return nil
		},
		ReceivingVerifier: func(_ context.Context, _ *API, verified *AdoptPlan, _ *AdoptProvenanceRecord) error {
			return verifyAdoptClientReadbacksWithResolver(verified, "http://127.0.0.1:"+strconv.Itoa(verified.Port)+adoptDefaultURLPath, newAdoptClientTargetResolver())
		},
	}); err != nil {
		t.Fatalf("ExecuteAdopt repeat: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("Already adopted")) {
		t.Fatalf("repeat output = %q", out.String())
	}
	for _, path := range paths {
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read repeat postimage %s: %v", filepath.Base(path), err)
		}
		if !bytes.Equal(after, before[path]) {
			t.Fatalf("repeat mutated %s", filepath.Base(path))
		}
	}
	events := before[filepath.Join(stateDir, SupervisorEventLogFileLeaf)]
	if bytes.Contains(events, []byte(fixture.projectRoot)) || !bytes.Contains(events, []byte(`"schema_version":"client-config-settlement-v2"`)) ||
		!bytes.Contains(events, []byte(`"project_identity_sha256":"`+settlement.ProjectIdentitySHA256+`"`)) {
		t.Fatalf("project settlement event is not path-free v2: %s", events)
	}
	if _, err := os.Stat(fixture.globalCodexTrap); !os.IsNotExist(err) {
		t.Fatalf("default project adopt touched global Codex trap: %v", err)
	}

	oldToken := repeat.ProjectSourcePreimageToken()
	if err := os.WriteFile(fixture.projectConfig, append(before[fixture.projectConfig], []byte("\n# unrelated preview drift\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := NewAPI().BuildAdoptPlan(opts)
	if err != nil {
		t.Fatalf("BuildAdoptPlan repeat after unrelated drift: %v", err)
	}
	if rebuilt.ProjectSourcePreimageToken() == oldToken || !errors.Is(rebuilt.ValidateProjectSourcePreimageToken(oldToken), clients.ErrCodexProjectSourceChanged) {
		t.Fatal("repeat preview token did not bind the current full project bytes")
	}
}

func TestProjectAdoptRepeatDistinguishesUnknownCollisionAndOwnedDrift(t *testing.T) {
	t.Run("unknown manifest collision", func(t *testing.T) {
		fixture := newProjectAdoptPlanFixture(t, "project-collision", validProjectAdoptConfig)
		t.Cleanup(SetDaemonStateRootForTest(hardenedTempDir(t)))
		fingerprint, err := clients.CodexProjectIdentitySHA256ForRoot(fixture.projectRoot)
		if err != nil {
			t.Fatal(err)
		}
		manifestName := "demo-project-" + fingerprint[:8]
		dir := cleanupProjectAdoptTestManifest(t, manifestName)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("unknown"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = NewAPI().BuildAdoptPlan(projectAdoptPlanOpts(fixture, "demo", 9137))
		if err == nil || !bytes.Contains([]byte(err.Error()), []byte("PROJECT_ADOPT_MANAGED_NAME_CONFLICT")) || errors.Is(err, ErrProjectAdoptRepeatStateInconsistent) {
			t.Fatalf("collision error = %v", err)
		}
	})

	t.Run("owned current binding drift", func(t *testing.T) {
		fixture := newProjectAdoptPlanFixture(t, "project-owned-drift", validProjectAdoptConfig)
		t.Cleanup(SetDaemonStateRootForTest(hardenedTempDir(t)))
		opts := projectAdoptPlanOpts(fixture, "demo", nextBindableAdoptPortForTest(t, collectUsedAdoptPorts()))
		plan, err := NewAPI().BuildAdoptPlan(opts)
		if err != nil {
			t.Fatal(err)
		}
		cleanupProjectAdoptTestManifest(t, plan.ManifestName)
		if _, err := NewAPI().ExecuteAdoptResultWithOpts(plan, nil, ExecuteAdoptOpts{
			installFn:         func(context.Context, *API, InstallOpts) error { return nil },
			ReceivingVerifier: func(context.Context, *API, *AdoptPlan, *AdoptProvenanceRecord) error { return nil },
		}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixture.projectConfig, []byte("[mcp_servers.demo]\nurl = \"http://127.0.0.1:9999/mcp\"\nstartup_timeout_sec = 10\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewAPI().BuildAdoptPlan(opts); !errors.Is(err, ErrProjectAdoptRepeatStateInconsistent) {
			t.Fatalf("owned binding drift error = %v, want PROJECT_ADOPT_REPEAT_STATE_INCONSISTENT", err)
		}
	})
}

func TestProviderRecoveryClassifierNeverUsesProjectTargetResolver(t *testing.T) {
	previousManifestExists := adoptManifestExistsFn
	adoptManifestExistsFn = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() { adoptManifestExistsFn = previousManifestExists })
	resolver := adoptClientTargetResolver{
		globalCatalogFactory: func() map[string]clients.Client { t.Fatal("provider recovery entered target resolver"); return nil },
		projectClientFactory: func(string, string) (clients.Client, error) {
			t.Fatal("provider recovery entered target resolver")
			return nil, nil
		},
	}
	rec := AdoptProvenanceRecord{ProviderSource: &ProviderSourceProvenanceV1{DisablePhase: "disable_planned"}}
	if got := classifyDeadAdoptingRowWithResolver(rec, resolver); got != adoptRowRecoveryKeep {
		t.Fatalf("provider recovery verdict = %v, want recovery keep", got)
	}
}

func TestProjectAdoptRowFirstAnchorIsSafelyReapableBeforeSnapshot(t *testing.T) {
	fixture := newProjectAdoptPlanFixture(t, "project-anchor", validProjectAdoptConfig)
	t.Cleanup(SetDaemonStateRootForTest(hardenedTempDir(t)))
	source, err := extractCodexProjectAdoptSource(fixture.projectRoot, "demo")
	if err != nil {
		t.Fatal(err)
	}
	plan := &AdoptPlan{
		EntryName: "demo", SourceClient: "codex-cli", ManifestName: "demo-project-" + source.identity.ProjectIdentitySHA256()[:8],
		Port: 9137, AdoptClients: []string{"codex-cli"}, TargetEntryNames: map[string]string{"codex-cli": "demo"}, projectSource: source,
	}
	rec := AdoptProvenanceRecord{
		ManifestName: plan.ManifestName, SourceClient: plan.SourceClient, SourceEntryName: plan.EntryName, Port: plan.Port,
		AdoptClients: plan.AdoptClients, OperationState: AdoptOperationStateAdopting, Clients: projectAdoptAnchorClients(plan),
	}
	previousManifestExists := adoptManifestExistsFn
	adoptManifestExistsFn = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() { adoptManifestExistsFn = previousManifestExists })
	resolver := newAdoptClientTargetResolver()
	resolver.globalCatalogFactory = func() map[string]clients.Client { t.Fatal("project anchor used global catalog"); return nil }
	if got := classifyDeadAdoptingRowWithResolver(rec, resolver); got != adoptRowCrashReap {
		t.Fatalf("project anchor verdict = %v, want crash reap", got)
	}
	if !adoptRowProvablyUnmutatedWithResolver(rec, resolver) {
		t.Fatal("project row-first anchor was not provably safe to reap")
	}
}

func TestProjectAdoptRecoveryClassifiersUsePersistedProjectTarget(t *testing.T) {
	const body = "[mcp_servers.demo]\nurl = \"http://127.0.0.1:9137/mcp\"\nstartup_timeout_sec = 10\n"
	fixture := newProjectAdoptPlanFixture(t, "project-classifiers", body)
	t.Cleanup(SetDaemonStateRootForTest(hardenedTempDir(t)))
	fingerprint, err := clients.CodexProjectIdentitySHA256ForRoot(fixture.projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	target := &AdoptClientConfigTargetV1{
		Kind: adoptClientConfigTargetKindCodexProject, ProjectRoot: fixture.projectRoot, ProjectIdentitySHA256: fingerprint,
	}
	rec := AdoptProvenanceRecord{
		ManifestName: "demo-project-" + fingerprint[:8], SourceClient: "codex-cli", SourceEntryName: "demo", Port: 9137,
		AdoptClients: []string{"codex-cli"}, OperationState: AdoptOperationStateAdopting,
		Clients: []AdoptClientProvenance{{
			Client: "codex-cli", TargetEntryName: "demo", ConfigTarget: target,
			OriginalState: AdoptOriginalStateAbsent, RestoreMode: AdoptRestoreModeNA,
		}},
	}
	globalCalls, projectCalls := 0, 0
	resolver := newAdoptClientTargetResolver()
	resolver.globalCatalogFactory = func() map[string]clients.Client {
		globalCalls++
		return nil
	}
	productionProjectFactory := resolver.projectClientFactory
	resolver.projectClientFactory = func(root, entry string) (clients.Client, error) {
		projectCalls++
		return productionProjectFactory(root, entry)
	}
	previousManifestExists := adoptManifestExistsFn
	adoptManifestExistsFn = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() { adoptManifestExistsFn = previousManifestExists })

	if verdict := classifyDeadAdoptingRowWithResolver(rec, resolver); verdict != adoptRowCommittedKeep {
		t.Fatalf("dead-adopting verdict = %v, want committed keep", verdict)
	}
	if adoptRowProvablyUnmutatedWithResolver(rec, resolver) {
		t.Fatal("orphan classifier marked live project hub binding unmutated")
	}
	if globalCalls != 0 || projectCalls != 2 {
		t.Fatalf("resolver calls global=%d project=%d, want 0/2", globalCalls, projectCalls)
	}
}

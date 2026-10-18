package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
)

type projectDeAdoptFixture struct {
	logicalEntry  string
	manifestName  string
	projectRoot   string
	projectConfig string
	projectHub    []byte
	projectNative []byte
	globalConfig  string
	globalBefore  []byte
	manifestRoot  string
	stateRoot     string
	record        AdoptProvenanceRecord
}

func setupProjectDeAdoptFixture(t *testing.T, logicalEntry string, state AdoptOperationState) projectDeAdoptFixture {
	t.Helper()
	projectRoot := t.TempDir()
	fingerprint, err := clients.CodexProjectIdentitySHA256ForRoot(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifestName := logicalEntry + "-project-" + fingerprint[:8]
	globalBefore := []byte(deAdoptNativeConfig(logicalEntry, "global-foreign-command"))
	globalConfig, manifestRoot, stateRoot := setupAdoptTestEnv(t, manifestName, string(globalBefore))

	projectConfig := filepath.Join(projectRoot, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	projectHub := []byte(deAdoptHubConfig(logicalEntry) + deAdoptNativeConfig("later-sibling", "later-command"))
	if err := os.WriteFile(projectConfig, projectHub, 0o600); err != nil {
		t.Fatal(err)
	}
	projectNative := []byte(deAdoptNativeConfig(logicalEntry, "project-native-command") + deAdoptNativeConfig("snapshot-only-sibling", "snapshot-command"))
	manifestBytes := deAdoptManagedManifest(manifestName, 0)
	manifestDir := filepath.Join(manifestRoot, manifestName)
	if err := os.MkdirAll(manifestDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "manifest.yaml"), manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	clientRecord := AdoptClientProvenance{
		Client:          "codex-cli",
		TargetEntryName: logicalEntry,
		ConfigTarget: &AdoptClientConfigTargetV1{
			Kind:                  adoptClientConfigTargetKindCodexProject,
			ProjectRoot:           projectRoot,
			ProjectIdentitySHA256: fingerprint,
		},
		OriginalState:  AdoptOriginalStatePresent,
		RestoreMode:    AdoptRestoreModeFunctionalEquivalent,
		SnapshotSHA256: ManifestHashContent(projectNative),
	}
	if _, _, err := writeAdoptClientSnapshot(manifestName, clientRecord.Client, projectNative); err != nil {
		t.Fatal(err)
	}
	record := AdoptProvenanceRecord{
		ManifestName:         manifestName,
		SourceClient:         "codex-cli",
		SourceEntryName:      logicalEntry,
		Port:                 deAdoptTestPort,
		AdoptClients:         []string{"codex-cli"},
		AdoptManifestHash:    ManifestHashContent(manifestBytes),
		ExpectedManifestHash: ManifestHashContent(manifestBytes),
		OperationState:       state,
		Clients:              []AdoptClientProvenance{clientRecord},
	}
	writeDeAdoptExecutorRecord(t, record)
	return projectDeAdoptFixture{
		logicalEntry: logicalEntry, manifestName: manifestName,
		projectRoot: projectRoot, projectConfig: projectConfig,
		projectHub: projectHub, projectNative: projectNative,
		globalConfig: globalConfig, globalBefore: globalBefore,
		manifestRoot: manifestRoot, stateRoot: stateRoot, record: record,
	}
}

// Catches preview resolving a version-3 project target through the global
// catalog instead of the persisted target resolver.
func TestBuildDeAdoptPlanUsesPersistedProjectTargetWithoutGlobalMutation(t *testing.T) {
	fixture := setupProjectDeAdoptFixture(t, "project-preview", AdoptOperationStateAdopted)
	plan, err := NewAPI().BuildDeAdoptPlan(fixture.manifestName)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Routing != DeAdoptRoutingFresh || len(plan.Clients) != 1 || plan.Clients[0].Disposition != DeAdoptClientRestorePending {
		t.Fatalf("project preview = routing %q clients %+v, want fresh restore-pending", plan.Routing, plan.Clients)
	}
	assertProjectDeAdoptFilesUnchanged(t, fixture)
}

// Catches fresh or resume execution resolving the leased row's project client
// through the global catalog, using an alias key, or entering legacy backup
// instead of restoring the pinned snapshot through project CAS.
func TestExecuteDeAdoptUsesPersistedProjectTargetForFreshAndResume(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state AdoptOperationState
	}{
		{name: "fresh", state: AdoptOperationStateAdopted},
		{name: "resume", state: AdoptOperationStateDeAdopting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := setupProjectDeAdoptFixture(t, "project-execute-"+tc.name, tc.state)
			seedDeAdoptSupervisorIntent(t, fixture.stateRoot, fixture.record)
			report, err := NewAPI().ExecuteDeAdopt(fixture.manifestName, nil)
			if err != nil {
				t.Fatalf("ExecuteDeAdopt: %v", err)
			}
			if !reflect.DeepEqual(report.Restored, []string{"codex-cli"}) || len(report.Failed) != 0 || len(report.Accepted) != 0 {
				t.Fatalf("report = %+v, want exact project restore", report)
			}
			snapshot, err := clients.ReadCodexProjectConfigSnapshot(fixture.projectRoot)
			if err != nil {
				t.Fatal(err)
			}
			assertProjectDeAdoptStdioEntry(t, snapshot, fixture.logicalEntry, "project-native-command", []any{"version"})
			assertProjectDeAdoptStdioEntry(t, snapshot, "later-sibling", "later-command", []any{"version"})
			if stale, err := snapshot.Entry("snapshot-only-sibling"); err != nil || stale != nil {
				t.Fatalf("snapshot-only sibling = %#v/%v, want absent from live config", stale, err)
			}
			assertProjectDeAdoptGlobalExact(t, fixture)
			assertProjectDeAdoptNoLegacySiblings(t, fixture.projectConfig)
			assertDeAdoptClosed(t, fixture.manifestRoot, fixture.stateRoot, fixture.record)
		})
	}
}

// Catches de-adopt bypassing the resolver's pure identity precondition and
// reaching either the project factory/config or the global adapter.
func TestBuildDeAdoptPlanRejectsWrongProjectFingerprintBeforeConfigAccess(t *testing.T) {
	logicalEntry := "project-wrong-fingerprint"
	projectRoot := t.TempDir()
	manifestName := logicalEntry + "-project-00000000"
	globalBefore := []byte(deAdoptHubConfig(logicalEntry))
	globalConfig, manifestRoot, stateRoot := setupAdoptTestEnv(t, manifestName, string(globalBefore))
	manifestBytes := deAdoptManagedManifest(manifestName, 0)
	manifestDir := filepath.Join(manifestRoot, manifestName)
	if err := os.MkdirAll(manifestDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "manifest.yaml"), manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	record := AdoptProvenanceRecord{
		ManifestName: manifestName, SourceClient: "codex-cli", SourceEntryName: logicalEntry,
		Port: deAdoptTestPort, AdoptClients: []string{"codex-cli"},
		AdoptManifestHash: ManifestHashContent(manifestBytes), ExpectedManifestHash: ManifestHashContent(manifestBytes),
		OperationState: AdoptOperationStateAdopted,
		Clients: []AdoptClientProvenance{{
			Client: "codex-cli", TargetEntryName: logicalEntry, OriginalState: AdoptOriginalStateAbsent, RestoreMode: AdoptRestoreModeNA,
			ConfigTarget: &AdoptClientConfigTargetV1{Kind: adoptClientConfigTargetKindCodexProject, ProjectRoot: projectRoot, ProjectIdentitySHA256: strings.Repeat("0", 64)},
		}},
	}
	raw, err := json.Marshal(&AdoptedEntries{Version: adoptedEntriesSchemaV3, Records: []AdoptProvenanceRecord{record}})
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately bypass the normal writer validator to model persisted
	// corruption. The production reader/resolver must reject it before project
	// factory/config access; weakening writeAdoptedEntries would invalidate the
	// oracle.
	if err := writeHubMcpStateFile(adoptedEntriesFileLeaf, raw); err != nil {
		t.Fatal(err)
	}
	projectEntriesBefore := directoryNamesForProjectDeAdoptTest(t, projectRoot)
	stateEntriesBefore := directoryNamesForProjectDeAdoptTest(t, stateRoot)

	plan, err := NewAPI().BuildDeAdoptPlan(manifestName)
	if plan != nil || !errors.Is(err, errProjectAdoptConfigTargetInvalid) {
		t.Fatalf("BuildDeAdoptPlan = %#v/%v, want nil/PROJECT_ADOPT_CONFIG_TARGET_INVALID", plan, err)
	}
	if got := directoryNamesForProjectDeAdoptTest(t, projectRoot); !reflect.DeepEqual(got, projectEntriesBefore) {
		t.Fatalf("invalid identity accessed project root: before=%v after=%v", projectEntriesBefore, got)
	}
	if got := directoryNamesForProjectDeAdoptTest(t, stateRoot); !reflect.DeepEqual(got, stateEntriesBefore) {
		t.Fatalf("invalid identity created project lock/state: before=%v after=%v", stateEntriesBefore, got)
	}
	assertFileBytesForProjectDeAdoptTest(t, globalConfig, globalBefore)
}

// Catches the provider-recovery early return moving behind ordinary client
// target resolution.
func TestBuildDeAdoptPlanProviderRecoveryDoesNotResolveProjectTarget(t *testing.T) {
	name := "provider-recovery-no-project-resolver"
	manifestRoot, _, _, _ := setupProviderPreInstallDeAdoptFixture(t, name, AdoptOperationStateAdopting)
	manifestPath := filepath.Join(manifestRoot, name, "manifest.yaml")
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Dir(manifestPath)); err != nil {
		t.Fatal(err)
	}
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil || plan == nil || !plan.providerRecovery || len(plan.Clients) != 0 {
		t.Fatalf("provider recovery plan = %#v/%v, want recovery plan without client resolution", plan, err)
	}
}

func assertProjectDeAdoptFilesUnchanged(t *testing.T, fixture projectDeAdoptFixture) {
	t.Helper()
	assertFileBytesForProjectDeAdoptTest(t, fixture.projectConfig, fixture.projectHub)
	assertProjectDeAdoptGlobalExact(t, fixture)
	assertProjectDeAdoptNoLegacySiblings(t, fixture.projectConfig)
}

func assertProjectDeAdoptGlobalExact(t *testing.T, fixture projectDeAdoptFixture) {
	t.Helper()
	assertFileBytesForProjectDeAdoptTest(t, fixture.globalConfig, fixture.globalBefore)
}

func assertProjectDeAdoptNoLegacySiblings(t *testing.T, configPath string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.toml" || !entries[0].Type().IsRegular() {
		t.Fatalf("project config directory has lock/backup residue: %+v", entries)
	}
}

func assertProjectDeAdoptStdioEntry(t *testing.T, snapshot clients.CodexProjectConfigSnapshot, name, command string, args []any) {
	t.Helper()
	entry, err := snapshot.Entry(name)
	if err != nil || entry == nil {
		t.Fatalf("project entry %q = %#v/%v", name, entry, err)
	}
	raw := entry.Raw
	if raw == nil {
		t.Fatalf("project entry %q raw map is nil", name)
	}
	if gotCommand, _ := raw["command"].(string); gotCommand != command {
		t.Fatalf("project entry %q command = %q, want %q", name, gotCommand, command)
	}
	if gotArgs, _ := raw["args"].([]any); !reflect.DeepEqual(gotArgs, args) {
		t.Fatalf("project entry %q args = %#v, want %#v", name, gotArgs, args)
	}
}

func assertFileBytesForProjectDeAdoptTest(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("file %q changed/read: read=%v before=%q after=%q", path, err, want, got)
	}
}

func directoryNamesForProjectDeAdoptTest(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

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

func TestAdoptPlanValidateProjectSourcePreimageTokenMatrix(t *testing.T) {
	validToken := strings.Repeat("a", 64)
	tests := []struct {
		name     string
		plan     *AdoptPlan
		supplied string
		wantErr  error
	}{
		{name: "nil plan", wantErr: clients.ErrCodexProjectSourceChanged},
		{name: "project empty private token", plan: &AdoptPlan{projectSource: &projectAdoptSource{}}, wantErr: clients.ErrCodexProjectSourceChanged},
		{name: "project malformed private token", plan: &AdoptPlan{projectSource: &projectAdoptSource{}, projectSourcePreimageToken: strings.ToUpper(validToken)}, supplied: validToken, wantErr: clients.ErrCodexProjectSourceChanged},
		{name: "project empty supplied token", plan: &AdoptPlan{projectSource: &projectAdoptSource{}, projectSourcePreimageToken: validToken}, wantErr: clients.ErrCodexProjectSourcePreviewRequired},
		{name: "project malformed supplied token", plan: &AdoptPlan{projectSource: &projectAdoptSource{}, projectSourcePreimageToken: validToken}, supplied: " " + validToken, wantErr: clients.ErrCodexProjectSourceChanged},
		{name: "project unequal supplied token", plan: &AdoptPlan{projectSource: &projectAdoptSource{}, projectSourcePreimageToken: validToken}, supplied: strings.Repeat("b", 64), wantErr: clients.ErrCodexProjectSourceChanged},
		{name: "project exact token", plan: &AdoptPlan{projectSource: &projectAdoptSource{}, projectSourcePreimageToken: validToken}, supplied: validToken},
		{name: "repeat exact token", plan: &AdoptPlan{projectRepeat: &projectAdoptRepeat{}, projectSourcePreimageToken: validToken}, supplied: validToken},
		{name: "repeat empty supplied token", plan: &AdoptPlan{projectRepeat: &projectAdoptRepeat{}, projectSourcePreimageToken: validToken}, wantErr: clients.ErrCodexProjectSourcePreviewRequired},
		{name: "dual project markers", plan: &AdoptPlan{projectSource: &projectAdoptSource{}, projectRepeat: &projectAdoptRepeat{}, projectSourcePreimageToken: validToken}, supplied: validToken, wantErr: clients.ErrCodexProjectSourceChanged},
		{name: "global inconsistent private token", plan: &AdoptPlan{projectSourcePreimageToken: validToken}, wantErr: clients.ErrCodexProjectSourceChanged},
		{name: "global unexpected supplied token", plan: &AdoptPlan{}, supplied: validToken, wantErr: ErrProjectAdoptSourcePreimageUnexpected},
		{name: "global empty supplied token", plan: &AdoptPlan{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.plan.ValidateProjectSourcePreimageToken(tc.supplied)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateProjectSourcePreimageToken error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

type projectAdoptPlanFixture struct {
	root            string
	projectRoot     string
	projectConfig   string
	globalCodexTrap string
	manifestRoot    string
	absentStateRoot string
}

func newProjectAdoptPlanFixture(t *testing.T, projectName, body string) projectAdoptPlanFixture {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "localappdata"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "xdg-data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg-state"))
	neutralizeClientConfigPathEnv(t, home)

	canonical := filepath.Join(root, mcphubShortName)
	if err := os.WriteFile(canonical, []byte("test mcphub"), 0o700); err != nil {
		t.Fatal(err)
	}
	previousCanonical := testCanonicalMcphubPathOverride
	testCanonicalMcphubPathOverride = canonical
	t.Cleanup(func() { testCanonicalMcphubPathOverride = previousCanonical })

	manifestRoot := filepath.Join(root, "manifests")
	if err := os.Mkdir(manifestRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestRoot)

	projectRoot := filepath.Join(root, projectName)
	configDir := filepath.Join(projectRoot, ".codex")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	projectConfig := filepath.Join(configDir, "config.toml")
	if err := os.WriteFile(projectConfig, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	absentStateRoot := filepath.Join(root, "absent-state")
	t.Cleanup(SetDaemonStateRootForTest(absentStateRoot))
	return projectAdoptPlanFixture{
		root:            root,
		projectRoot:     projectRoot,
		projectConfig:   projectConfig,
		globalCodexTrap: filepath.Join(root, "global-codex-must-not-be-read.toml"),
		manifestRoot:    manifestRoot,
		absentStateRoot: absentStateRoot,
	}
}

func projectAdoptPlanOpts(fixture projectAdoptPlanFixture, entry string, port int) AdoptOpts {
	return AdoptOpts{
		EntryName:              entry,
		Client:                 "codex-cli",
		Port:                   port,
		Clients:                []string{"codex-cli"},
		CodexProjectSourceRoot: fixture.projectRoot,
		ScanOpts: ScanOpts{ConfigPaths: map[string]string{
			"codex-cli": fixture.globalCodexTrap,
		}},
	}
}

func TestBuildAdoptPlanProjectSourceBindsPreviewWithoutStateOrGlobalMutation(t *testing.T) {
	const body = `[profile.default]
model = "gpt-5"

[mcp_servers.graphify]
command = "graphify"
args = ["--mcp"]
tool_timeout_sec = 19

[mcp_servers.graphify.env]
MODE = "project"
`
	fixture := newProjectAdoptPlanFixture(t, "project-a", body)
	before, err := os.ReadFile(fixture.projectConfig)
	if err != nil {
		t.Fatal(err)
	}
	identitySHA256, err := clients.CodexProjectIdentitySHA256ForRoot(fixture.projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	wantManifestName := "graphify-project-" + identitySHA256[:8]
	usedPorts, err := collectUsedAdoptPortsResolved()
	if err != nil {
		t.Fatal(err)
	}
	port := nextBindableAdoptPortForTest(t, usedPorts)

	plan, err := NewAPI().BuildAdoptPlan(projectAdoptPlanOpts(fixture, "graphify", port))
	if err != nil {
		t.Fatalf("BuildAdoptPlan(project): %v", err)
	}
	if plan.EntryName != "graphify" || plan.SourceClient != "codex-cli" || plan.ManifestName != wantManifestName || plan.Port != port {
		t.Fatalf("plan identity = entry %q source %q manifest %q port %d", plan.EntryName, plan.SourceClient, plan.ManifestName, plan.Port)
	}
	if !reflect.DeepEqual(plan.AdoptClients, []string{"codex-cli"}) || plan.TargetEntryNames["codex-cli"] != "graphify" {
		t.Fatalf("plan clients=%v targets=%v", plan.AdoptClients, plan.TargetEntryNames)
	}
	if plan.projectSource == nil || plan.projectSource.identity.ProjectIdentitySHA256() != identitySHA256 ||
		plan.projectSource.logicalEntryName != "graphify" || plan.projectSource.entry.Command != "graphify" ||
		!reflect.DeepEqual(plan.projectSource.entry.Args, []string{"--mcp"}) ||
		!reflect.DeepEqual(plan.projectSource.entry.Env, map[string]string{"MODE": "project"}) || plan.projectSource.entry.ToolTimeoutSec != 19 {
		t.Fatalf("private project source = %+v", plan.projectSource)
	}
	wantToken, err := codexProjectSourcePreimageToken(identitySHA256, "graphify", plan.projectSource.configSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ProjectSourcePreimageToken() != wantToken || len(wantToken) != 64 || wantToken != strings.ToLower(wantToken) {
		t.Fatalf("preview token = %q, want %q", plan.ProjectSourcePreimageToken(), wantToken)
	}
	target := plan.clientTargets["codex-cli"]
	if target.clientName != "codex-cli" || target.targetEntryName != "graphify" || target.projectSource != plan.projectSource || target.configTarget != nil {
		t.Fatalf("project target = %+v", target)
	}
	if !strings.Contains(plan.ManifestYAML, `command: graphify`) || !strings.Contains(plan.ManifestYAML, `tool_timeout_sec: 19`) {
		t.Fatalf("manifest does not use project source:\n%s", plan.ManifestYAML)
	}

	after, err := os.ReadFile(fixture.projectConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("project config changed during preview\nbefore:\n%s\nafter:\n%s", before, after)
	}
	for _, absent := range []string{
		fixture.absentStateRoot,
		fixture.globalCodexTrap,
		filepath.Join(fixture.manifestRoot, wantManifestName),
	} {
		if _, err := os.Stat(absent); !os.IsNotExist(err) {
			t.Fatalf("preview created or accessed mutation target %q: %v", absent, err)
		}
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(fixture.projectRoot)) || bytes.Contains(encoded, []byte(wantToken)) || bytes.Contains(encoded, []byte("projectSource")) {
		t.Fatalf("private project identity leaked through plan JSON: %s", encoded)
	}
}

func TestPrintAdoptPlanProjectSourceUsesProjectTargetText(t *testing.T) {
	fixture := newProjectAdoptPlanFixture(t, "project-preview", "[mcp_servers.graphify]\ncommand = \"graphify\"\nargs = [\"--mcp\"]\n")
	plan, err := NewAPI().BuildAdoptPlan(projectAdoptPlanOpts(fixture, "graphify", 9308))
	if err != nil {
		t.Fatalf("BuildAdoptPlan(project): %v", err)
	}

	var out bytes.Buffer
	PrintAdoptPlan(&out, plan)
	printed := out.String()
	for _, want := range []string{
		"project write target: selected Codex project config",
		"action: replace project entry with HTTP relay",
	} {
		if !strings.Contains(printed, want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, printed)
		}
	}
	for _, forbidden := range []string{
		"global write target",
		"project read-only",
		fixture.projectRoot,
	} {
		if strings.Contains(printed, forbidden) {
			t.Fatalf("dry-run output exposed stale or sensitive text %q:\n%s", forbidden, printed)
		}
	}
}

func TestBuildAdoptPlanProjectSourceDerivesDistinctSameNameIdentities(t *testing.T) {
	const body = "[mcp_servers.graphify]\ncommand = \"graphify\"\nargs = [\"--mcp\"]\n"
	fixtureA := newProjectAdoptPlanFixture(t, "project-a", body)
	fixtureB := newProjectAdoptPlanFixture(t, "project-b", body)
	planA, err := NewAPI().BuildAdoptPlan(projectAdoptPlanOpts(fixtureA, "graphify", 9305))
	if err != nil {
		t.Fatalf("BuildAdoptPlan(project A): %v", err)
	}
	planB, err := NewAPI().BuildAdoptPlan(projectAdoptPlanOpts(fixtureB, "graphify", 9306))
	if err != nil {
		t.Fatalf("BuildAdoptPlan(project B): %v", err)
	}
	if planA.ManifestName == planB.ManifestName || planA.ProjectSourcePreimageToken() == planB.ProjectSourcePreimageToken() {
		t.Fatalf("same-name projects collapsed: manifests %q/%q tokens %q/%q", planA.ManifestName, planB.ManifestName, planA.ProjectSourcePreimageToken(), planB.ProjectSourcePreimageToken())
	}
	if planA.TargetEntryNames["codex-cli"] != "graphify" || planB.TargetEntryNames["codex-cli"] != "graphify" {
		t.Fatalf("logical project targets changed: %v / %v", planA.TargetEntryNames, planB.TargetEntryNames)
	}
}

func TestBuildAdoptPlanProjectSourceRejectsIncompatibleSelectorsBeforeRead(t *testing.T) {
	missingRoot := filepath.Join(t.TempDir(), "missing-project")
	for _, tc := range []struct {
		name string
		opts AdoptOpts
	}{
		{
			name: "non-Codex source",
			opts: AdoptOpts{EntryName: "demo", Client: "claude-code", CodexProjectSourceRoot: missingRoot},
		},
		{
			name: "provider source",
			opts: AdoptOpts{EntryName: "demo", Client: "codex-cli", ProviderPluginRef: "plugin@example", CodexProjectSourceRoot: missingRoot},
		},
		{
			name: "user supplied manifest name",
			opts: AdoptOpts{EntryName: "demo", Client: "codex-cli", ManifestName: "demo", CodexProjectSourceRoot: missingRoot},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewAPI().BuildAdoptPlan(tc.opts)
			if err == nil || !strings.Contains(err.Error(), "CODEX_PROJECT_SOURCE_MODE_INVALID") {
				t.Fatalf("BuildAdoptPlan error = %v, want CODEX_PROJECT_SOURCE_MODE_INVALID", err)
			}
			if _, statErr := os.Stat(missingRoot); !os.IsNotExist(statErr) {
				t.Fatalf("incompatible selector accessed project root: %v", statErr)
			}
		})
	}
}

func TestBuildAdoptPlanProjectSourceUnavailableUsesPathFreeReason(t *testing.T) {
	fixture := newProjectAdoptPlanFixture(t, "project-missing-entry", "[mcp_servers.other]\ncommand = \"other\"\n")
	_, err := NewAPI().BuildAdoptPlan(projectAdoptPlanOpts(fixture, "demo", 9303))
	if err == nil || !strings.Contains(err.Error(), "CODEX_PROJECT_SOURCE_UNAVAILABLE") ||
		!strings.Contains(err.Error(), "source entry is absent") {
		t.Fatalf("BuildAdoptPlan error = %v, want CODEX_PROJECT_SOURCE_UNAVAILABLE with nonempty absent-entry reason", err)
	}
	if strings.Contains(err.Error(), fixture.projectRoot) {
		t.Fatalf("project source error leaked root %q: %v", fixture.projectRoot, err)
	}
	if _, statErr := os.Stat(fixture.absentStateRoot); !os.IsNotExist(statErr) {
		t.Fatalf("unavailable project source created state root: %v", statErr)
	}
}

func TestBuildAdoptPlanGlobalLegacyBehaviorRemainsUnchanged(t *testing.T) {
	fixture := newProjectAdoptPlanFixture(t, "unused-project", "[mcp_servers.unused]\ncommand = \"unused\"\n")
	globalConfig := filepath.Join(fixture.root, "global-config.toml")
	const body = "[mcp_servers.global-demo]\ncommand = \"global-tool\"\nargs = [\"--stdio\"]\n"
	if err := os.WriteFile(globalConfig, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    "global-demo",
		Client:       "codex-cli",
		ManifestName: "global-demo",
		Port:         9304,
		Clients:      []string{"codex-cli"},
		ScanOpts: ScanOpts{ConfigPaths: map[string]string{
			"codex-cli": globalConfig,
		}},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan(global): %v", err)
	}
	if plan.ManifestName != "global-demo" || plan.ProjectSourcePreimageToken() != "" || plan.projectSource != nil {
		t.Fatalf("global plan gained project identity: manifest=%q token=%q source=%+v", plan.ManifestName, plan.ProjectSourcePreimageToken(), plan.projectSource)
	}
	if !strings.Contains(plan.ManifestYAML, "command: global-tool") || !strings.Contains(plan.ManifestYAML, "- --stdio") {
		t.Fatalf("global manifest changed:\n%s", plan.ManifestYAML)
	}
	target := plan.clientTargets["codex-cli"]
	if target.clientName != "codex-cli" || target.targetEntryName != "global-demo" || target.configTarget != nil || target.projectSource != nil {
		t.Fatalf("global target ref = %+v", target)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("project")) || bytes.Contains(encoded, []byte("preimage")) {
		t.Fatalf("global plan wire changed with project fields: %s", encoded)
	}
}

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/clients"
)

// Catches the CLI dropping the selected project root, requiring a caller-made
// manifest name/port, printing private token/config material, or executing any
// dry-run mutation instead of using the existing API plan renderer.
func TestAdoptCmdProjectSourceDryRunUsesDerivedPlanWithoutMutation(t *testing.T) {
	root, home := adoptTestHome(t)
	stateDir := apitest.HardenedTempDir(t)
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))
	manifestDir := filepath.Join(root, "manifests")
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)

	projectRoot := filepath.Join(root, "selected-project")
	projectConfig := filepath.Join(projectRoot, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	const secretValue = "project-source-secret-value-canary"
	projectBefore := []byte(`[mcp_servers.graphify]
command = "project-command-canary"
args = ["--stdio"]

[mcp_servers.graphify.env]
GRAPHIFY_PRIVATE_TOKEN = "` + secretValue + `"
`)
	if err := os.WriteFile(projectConfig, projectBefore, 0o600); err != nil {
		t.Fatal(err)
	}

	// A malformed same-name global config makes accidental global fallback/read
	// observable while remaining a synthetic temp-root fixture.
	globalConfig := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(globalConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	globalBefore := []byte("global-invalid-canary = [\n")
	if err := os.WriteFile(globalConfig, globalBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	stateBefore := directoryNamesForProjectSourceTest(t, stateDir)

	var capturedOpts api.AdoptOpts
	var capturedPlan *api.AdoptPlan
	builder := func(a *api.API, opts api.AdoptOpts) (*api.AdoptPlan, error) {
		capturedOpts = opts
		plan, err := a.BuildAdoptPlan(opts)
		capturedPlan = plan
		return plan, err
	}
	cmd := newAdoptCmdWithDepsAndPlanBuilder(api.NewAPI, nil, nil, builder)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"graphify", "--client", "codex-cli", "--project-source", projectRoot})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("project-source dry-run: %v\n%s", err, out.String())
	}
	if capturedOpts.CodexProjectSourceRoot != projectRoot || capturedOpts.CodexProjectRoot != "" || capturedOpts.CodexWorkingDir != "" {
		t.Fatalf("project source/legacy opts = %q/%q/%q, want selected root/empty/empty", capturedOpts.CodexProjectSourceRoot, capturedOpts.CodexProjectRoot, capturedOpts.CodexWorkingDir)
	}
	if capturedOpts.ManifestName != "" || capturedOpts.Port != 0 {
		t.Fatalf("CLI supplied manual manifest/port = %q/%d, want empty/0", capturedOpts.ManifestName, capturedOpts.Port)
	}
	if capturedPlan == nil {
		t.Fatal("project-source builder returned nil plan")
	}
	fingerprint, err := clients.CodexProjectIdentitySHA256ForRoot(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	wantManifest := "graphify-project-" + fingerprint[:8]
	if capturedPlan.ManifestName != wantManifest || capturedPlan.Port <= 0 {
		t.Fatalf("derived plan manifest/port = %q/%d, want %q/positive", capturedPlan.ManifestName, capturedPlan.Port, wantManifest)
	}
	token := capturedPlan.ProjectSourcePreimageToken()
	if token == "" {
		t.Fatal("project-source plan has empty preimage token")
	}
	printed := out.String()
	for _, required := range []string{"dry-run", "  manifest: " + wantManifest, "No changes made."} {
		if !strings.Contains(printed, required) {
			t.Fatalf("dry-run output missing %q:\n%s", required, printed)
		}
	}
	for _, forbidden := range []string{secretValue, "project-command-canary", token, projectRoot, string(projectBefore)} {
		if forbidden != "" && strings.Contains(printed, forbidden) {
			t.Fatalf("dry-run output leaked private project material %q:\n%s", forbidden, printed)
		}
	}
	assertProjectSourceFileExact(t, projectConfig, projectBefore)
	assertProjectSourceFileExact(t, globalConfig, globalBefore)
	if _, statErr := os.Stat(projectConfig + ".lock"); !os.IsNotExist(statErr) {
		t.Fatalf("dry-run created adjacent project lock: %v", statErr)
	}
	if got := directoryNamesForProjectSourceTest(t, stateDir); strings.Join(got, "\x00") != strings.Join(stateBefore, "\x00") {
		t.Fatalf("dry-run changed state directory: before=%v after=%v", stateBefore, got)
	}
	if entries, err := os.ReadDir(manifestDir); err == nil && len(entries) != 0 {
		t.Fatalf("dry-run wrote manifest entries: %v", entries)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read manifest directory: %v", err)
	}
}

// Catches the CLI accepting a project source for a non-Codex client or
// combining the positive project selector with any provider-plugin flag.
func TestAdoptCmdProjectSourceRejectsInvalidCombinationsBeforeAPI(t *testing.T) {
	projectRoot := t.TempDir()
	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "non-Codex client", args: []string{"entry", "--client", "cursor", "--project-source", projectRoot}, wantErr: "--project-source requires --client codex-cli"},
		{name: "provider plugin", args: []string{"entry", "--client", "codex-cli", "--project-source", projectRoot, "--provider-plugin", "plugin@catalog"}, wantErr: "--project-source cannot be combined with --provider-plugin"},
		{name: "explicit empty provider plugin", args: []string{"entry", "--client", "codex-cli", "--project-source", projectRoot, "--provider-plugin="}, wantErr: "--project-source cannot be combined with --provider-plugin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newAPICalls := 0
			builderCalls := 0
			cmd := newAdoptCmdWithDepsAndPlanBuilder(func() *api.API {
				newAPICalls++
				return api.NewAPI()
			}, nil, nil, func(*api.API, api.AdoptOpts) (*api.AdoptPlan, error) {
				builderCalls++
				return &api.AdoptPlan{}, nil
			})
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
			if newAPICalls != 0 || builderCalls != 0 {
				t.Fatalf("invalid combination reached API/builder: %d/%d", newAPICalls, builderCalls)
			}
		})
	}
}

// Catches registration of the new flag changing the existing global path when
// the selector is omitted or explicitly empty.
func TestAdoptCmdEmptyProjectSourcePreservesGlobalOptionsAndNarration(t *testing.T) {
	t.Cleanup(api.SetDaemonStateRootForTest(apitest.HardenedTempDir(t)))
	for _, tc := range []struct {
		name  string
		extra []string
	}{
		{name: "omitted"},
		{name: "explicit empty", extra: []string{"--project-source="}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var captured api.AdoptOpts
			builder := func(_ *api.API, opts api.AdoptOpts) (*api.AdoptPlan, error) {
				captured = opts
				return &api.AdoptPlan{
					EntryName: "reader", SourceClient: "codex-cli", ManifestName: "reader", Port: 9344,
					AdoptClients: []string{"codex-cli"}, TargetEntryNames: map[string]string{"codex-cli": "reader"},
				}, nil
			}
			cmd := newAdoptCmdWithDepsAndPlanBuilder(api.NewAPI, nil, nil, builder)
			var out bytes.Buffer
			cmd.SetOut(&out)
			args := []string{"reader", "--client", "codex-cli"}
			args = append(args, tc.extra...)
			cmd.SetArgs(args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("global dry-run: %v", err)
			}
			if captured.CodexProjectSourceRoot != "" || captured.CodexProjectRoot != "" || captured.CodexWorkingDir != "" {
				t.Fatalf("global project opts = %q/%q/%q, want empty", captured.CodexProjectSourceRoot, captured.CodexProjectRoot, captured.CodexWorkingDir)
			}
			printed := out.String()
			if !strings.Contains(printed, "  manifest: reader\n") || !strings.Contains(printed, "dry-run") || strings.Contains(printed, "reader-project-") {
				t.Fatalf("global narration changed:\n%s", printed)
			}
		})
	}
}

func assertProjectSourceFileExact(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("file %q changed/read: read=%v before=%q after=%q", path, err, want, got)
	}
}

func directoryNamesForProjectSourceTest(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

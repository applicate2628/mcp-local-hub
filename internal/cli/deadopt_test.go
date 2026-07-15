package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

type fakeDeAdoptCLIAPI struct {
	plan       *api.DeAdoptPlan
	planErr    error
	report     *api.DeAdoptReport
	executeErr error

	executeServer string
	executeOpts   api.ExecuteDeAdoptOpts
}

func (f *fakeDeAdoptCLIAPI) BuildDeAdoptPlan(string) (*api.DeAdoptPlan, error) {
	return f.plan, f.planErr
}

func (f *fakeDeAdoptCLIAPI) ExecuteDeAdoptWithOpts(server string, _ io.Writer, opts api.ExecuteDeAdoptOpts) (*api.DeAdoptReport, error) {
	f.executeServer = server
	f.executeOpts = opts
	return f.report, f.executeErr
}

func runDeAdoptCommandWithFake(t *testing.T, fake *fakeDeAdoptCLIAPI, args ...string) (string, error) {
	t.Helper()
	prior := newDeAdoptCLIAPI
	newDeAdoptCLIAPI = func() deAdoptCLIAPI { return fake }
	t.Cleanup(func() { newDeAdoptCLIAPI = prior })

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestDeAdoptCmdDryRunByDefaultMutatesNothingAndRedactsSecrets(t *testing.T) {
	const (
		name   = "deadopt-dry-cli"
		secret = "deadopt-cli-secret-must-not-leak"
	)
	root := t.TempDir()
	home := filepath.Join(root, "home")
	stateRoot := filepath.Join(root, "state")
	manifestRoot := filepath.Join(root, "manifests")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "localappdata"))
	t.Setenv("APPDATA", filepath.Join(root, "appdata"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg-config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "xdg-data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg-state"))
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestRoot)
	t.Cleanup(api.SetDaemonStateRootForTest(stateRoot))

	codexPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o700); err != nil {
		t.Fatalf("mkdir codex config parent: %v", err)
	}
	initialConfig := `[mcp_servers.deadopt-dry-cli]
url = "http://127.0.0.1:0/mcp"

[mcp_servers.unrelated]
command = "go"
args = ["version"]

[mcp_servers.unrelated.env]
API_KEY = "` + secret + `"
`
	if err := os.WriteFile(codexPath, []byte(initialConfig), 0o600); err != nil {
		t.Fatalf("seed codex config: %v", err)
	}

	manifestBytes := []byte("name: " + name + "\n")
	manifestPath := filepath.Join(manifestRoot, name, "manifest.yaml")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o700); err != nil {
		t.Fatalf("mkdir manifest dir: %v", err)
	}
	if err := os.WriteFile(manifestPath, manifestBytes, 0o600); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}

	hash := api.ManifestHashContent(manifestBytes)
	store := api.AdoptedEntries{
		Version: 1,
		Records: []api.AdoptProvenanceRecord{{
			ManifestName:         name,
			SourceClient:         "codex-cli",
			SourceEntryName:      name,
			Port:                 0,
			AdoptClients:         []string{"codex-cli"},
			AdoptManifestHash:    hash,
			ExpectedManifestHash: hash,
			OperationState:       api.AdoptOperationStateAdopted,
			Clients: []api.AdoptClientProvenance{{
				Client:        "codex-cli",
				OriginalState: api.AdoptOriginalStateAbsent,
				RestoreMode:   api.AdoptRestoreModeNA,
			}},
		}},
	}
	storeBytes, err := json.MarshalIndent(&store, "", "  ")
	if err != nil {
		t.Fatalf("marshal adopt provenance: %v", err)
	}
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatalf("mkdir state root: %v", err)
	}
	storePath := filepath.Join(stateRoot, "adopted-entries.json")
	if err := api.WriteStateFileBytesAtomic(storePath, storeBytes); err != nil {
		t.Fatalf("seed adopt provenance: %v", err)
	}
	provenanceBefore, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("read seeded provenance: %v", err)
	}

	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"de-adopt", name})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("de-adopt dry-run: %v\n%s", err, out.String())
	}
	for _, marker := range []string{"De-adopt plan", "dry-run", "routing: FRESH", "disposition=remove-pending"} {
		if !strings.Contains(out.String(), marker) {
			t.Fatalf("dry-run output missing %q:\n%s", marker, out.String())
		}
	}
	if strings.Contains(out.String(), secret) {
		t.Fatalf("dry-run output leaked secret value:\n%s", out.String())
	}

	afterConfig, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatalf("read codex config after dry-run: %v", err)
	}
	if string(afterConfig) != initialConfig {
		t.Fatalf("codex config mutated during dry-run\nbefore:\n%s\nafter:\n%s", initialConfig, afterConfig)
	}
	afterManifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest after dry-run: %v", err)
	}
	if !bytes.Equal(afterManifest, manifestBytes) {
		t.Fatalf("manifest mutated during dry-run: got %q want %q", afterManifest, manifestBytes)
	}
	afterProvenance, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("read provenance after dry-run: %v", err)
	}
	if !bytes.Equal(afterProvenance, provenanceBefore) {
		t.Fatalf("provenance mutated during dry-run\nbefore:\n%s\nafter:\n%s", provenanceBefore, afterProvenance)
	}
}

func TestDeAdoptCmdRefusalPrintsPlanAndReturnsError(t *testing.T) {
	fake := &fakeDeAdoptCLIAPI{plan: &api.DeAdoptPlan{
		ManifestName:  "not-adopted",
		Routing:       api.DeAdoptRoutingRefuse,
		RefusalReason: "manifest is not adopt-owned",
	}}
	out, err := runDeAdoptCommandWithFake(t, fake, "de-adopt", "not-adopted")
	if err == nil {
		t.Fatalf("refused de-adopt succeeded:\n%s", out)
	}
	if !strings.Contains(out, "routing: REFUSE") || !strings.Contains(out, "manifest is not adopt-owned") {
		t.Fatalf("refusal output missing routing/reason:\n%s", out)
	}
	if !strings.Contains(err.Error(), "de-adopt refused") {
		t.Fatalf("refusal error = %v, want de-adopt refused", err)
	}
}

func TestDeAdoptCmdAliasExecutesAndPassesRepeatableAcceptConflict(t *testing.T) {
	fake := &fakeDeAdoptCLIAPI{
		plan: &api.DeAdoptPlan{
			ManifestName: "adopted-server",
			Routing:      api.DeAdoptRoutingFresh,
		},
		report: &api.DeAdoptReport{Accepted: []string{"codex-cli", "cursor"}},
	}
	out, err := runDeAdoptCommandWithFake(t, fake,
		"deadopt", "adopted-server", "--yes",
		"--accept-conflict", "codex-cli",
		"--accept-conflict", "cursor",
	)
	if err != nil {
		t.Fatalf("deadopt alias execute: %v\n%s", err, out)
	}
	if fake.executeServer != "adopted-server" {
		t.Fatalf("execute server = %q, want adopted-server", fake.executeServer)
	}
	want := []string{"codex-cli", "cursor"}
	if !reflect.DeepEqual(fake.executeOpts.AcceptConflictClients, want) {
		t.Fatalf("AcceptConflictClients = %#v, want %#v", fake.executeOpts.AcceptConflictClients, want)
	}
}

func TestDeAdoptCmdFailedReportReturnsError(t *testing.T) {
	fake := &fakeDeAdoptCLIAPI{
		plan: &api.DeAdoptPlan{
			ManifestName: "blocked-close",
			Routing:      api.DeAdoptRoutingResume,
		},
		report: &api.DeAdoptReport{Failed: []api.DeAdoptClientFailure{{
			Client: "codex-cli",
			Reason: "live entry is a genuine conflict",
		}}},
	}
	out, err := runDeAdoptCommandWithFake(t, fake, "de-adopt", "blocked-close", "--yes")
	if err == nil {
		t.Fatalf("de-adopt with failed report succeeded:\n%s", out)
	}
	for _, marker := range []string{"de-adopt did not complete: 1 client(s) failed", "codex-cli", "genuine conflict"} {
		if !strings.Contains(err.Error(), marker) {
			t.Fatalf("failed-report error missing %q: %v", marker, err)
		}
	}
}

func TestDeAdoptCmdExecuteErrorReturnsError(t *testing.T) {
	wantErr := errors.New("execute failed")
	fake := &fakeDeAdoptCLIAPI{
		plan: &api.DeAdoptPlan{
			ManifestName: "execute-error",
			Routing:      api.DeAdoptRoutingFresh,
		},
		executeErr: wantErr,
	}
	_, err := runDeAdoptCommandWithFake(t, fake, "de-adopt", "execute-error", "--yes")
	if !errors.Is(err, wantErr) {
		t.Fatalf("execute error = %v, want %v", err, wantErr)
	}
}

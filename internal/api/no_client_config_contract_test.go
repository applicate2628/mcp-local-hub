package api

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

func noClientContractManifest() *config.ServerManifest {
	return &config.ServerManifest{
		Name:      "demo",
		Kind:      config.KindGlobal,
		Transport: "stdio-bridge",
		Command:   "go",
		Daemons: []config.DaemonSpec{
			{Name: "alpha", Port: 51234},
			{Name: "beta", Port: 51235},
		},
		ClientBindings: []config.ClientBinding{
			{Client: "claude-code", Daemon: "alpha", URLPath: "/mcp"},
			{Client: "codex-cli", Daemon: "alpha", URLPath: "/mcp"},
			{Client: "cursor", Daemon: "alpha", URLPath: "/mcp"},
		},
	}
}

func TestDefaultInstallScopeUnchanged(t *testing.T) {
	plan, err := BuildPlanWithOpts(noClientContractManifest(), BuildPlanOpts{})
	if err != nil {
		t.Fatalf("BuildPlanWithOpts(default): %v", err)
	}
	got := make([]string, 0, len(plan.ClientUpdates))
	for _, update := range plan.ClientUpdates {
		got = append(got, update.Client)
	}
	want := clients.DefaultInstallClientNames()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("default client scope = %v, want exact registry-ordered defaults %v", got, want)
	}
	readinessCalls := 0
	readinessDefaults := resolveReadinessDefaultClientScope(AdmissionScope{}, func(string) ([]string, error) {
		readinessCalls++
		return nil, errors.New("no persisted override")
	})
	if readinessCalls != 1 || !reflect.DeepEqual(readinessDefaults, want) {
		t.Fatalf("readiness default scope = %v, calls=%d; want exact defaults %v and one preference read", readinessDefaults, readinessCalls, want)
	}
}

func TestNoClientPartialDaemonPreservesSiblingIntent(t *testing.T) {
	stub := filepath.Join(t.TempDir(), mcphubShortName)
	if err := os.WriteFile(stub, []byte("stub\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	previousCanonical := testCanonicalMcphubPathOverride
	testCanonicalMcphubPathOverride = stub
	t.Cleanup(func() { testCanonicalMcphubPathOverride = previousCanonical })

	m := noClientContractManifest()
	plan, err := BuildPlanWithOpts(m, BuildPlanOpts{DaemonFilter: "alpha", SkipClientConfigWrites: true})
	if err != nil {
		t.Fatalf("BuildPlanWithOpts(partial no-client): %v", err)
	}
	if len(plan.ClientUpdates) != 0 || len(plan.SupervisorIntent) != 1 || plan.SupervisorIntent[0].Name != "mcp-local-hub-demo-alpha" {
		t.Fatalf("partial no-client plan = clients:%d intent:%+v, want only alpha intent", len(plan.ClientUpdates), plan.SupervisorIntent)
	}

	intentPath := filepath.Join(t.TempDir(), "supervisor-intent.json")
	priorAlpha := SupervisorDaemon{TaskName: `\mcp-local-hub-demo-alpha`, Server: "demo", Daemon: "alpha", Command: "old-alpha", Port: 51234, ManifestHash: "old-alpha-hash"}
	priorBeta := SupervisorDaemon{TaskName: `\mcp-local-hub-demo-beta`, Server: "demo", Daemon: "beta", Command: "keep-beta", Args: []string{"--keep"}, Env: map[string]string{"KEEP": "true"}, Port: 51235, ManifestHash: "keep-beta-hash"}
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{priorAlpha, priorBeta}}); err != nil {
		t.Fatalf("seed supervisor intent: %v", err)
	}
	merged, _, _, err := NewAPI().buildMergedSupervisorIntent(m, intentPath, nil, "alpha", &bytes.Buffer{})
	if err != nil {
		t.Fatalf("buildMergedSupervisorIntent(partial): %v", err)
	}
	var gotAlpha, gotBeta *SupervisorDaemon
	for i := range merged.Daemons {
		switch merged.Daemons[i].Daemon {
		case "alpha":
			gotAlpha = &merged.Daemons[i]
		case "beta":
			gotBeta = &merged.Daemons[i]
		}
	}
	if gotAlpha == nil || gotAlpha.Command == priorAlpha.Command {
		t.Fatalf("selected alpha row was not refreshed: %+v", gotAlpha)
	}
	if gotBeta == nil || !reflect.DeepEqual(*gotBeta, priorBeta) {
		t.Fatalf("unselected beta row changed: got %+v, want %+v", gotBeta, priorBeta)
	}
}

func TestNoClientExecutionLeavesClientArtifactsUntouched(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "Local"))
	t.Setenv("APPDATA", filepath.Join(home, "Roaming"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))

	for _, name := range clients.DefaultInstallClientNames() {
		path, err := clients.ConfigPathForName(name)
		if err != nil {
			t.Fatalf("resolve %s config: %v", name, err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("client="+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".bak-existing", []byte("backup="+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshotRegularFiles(t, home)
	stub := filepath.Join(home, ".local", "bin", mcphubShortName)
	if err := os.MkdirAll(filepath.Dir(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stub, []byte("stub\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	previousCanonical := testCanonicalMcphubPathOverride
	testCanonicalMcphubPathOverride = stub
	t.Cleanup(func() { testCanonicalMcphubPathOverride = previousCanonical })
	before = snapshotRegularFiles(t, home)

	m := noClientContractManifest()
	plan, err := BuildPlanWithOpts(m, BuildPlanOpts{DaemonFilter: "alpha", SkipClientConfigWrites: true})
	if err != nil {
		t.Fatalf("build typed skip plan: %v", err)
	}
	intentPath := filepath.Join(t.TempDir(), "supervisor-intent.json")
	intermediateCalls := 0
	err = executeInstallTo(&bytes.Buffer{}, m, plan, 3, false, func() (func(), error) {
		intermediateCalls++
		merged, _, _, err := NewAPI().buildMergedSupervisorIntent(m, intentPath, nil, "alpha", &bytes.Buffer{})
		if err != nil {
			return nil, err
		}
		if err := WriteSupervisorIntent(intentPath, merged); err != nil {
			return nil, err
		}
		return func() { _ = os.Remove(intentPath) }, nil
	}, true, true)
	if err != nil {
		t.Fatalf("execute zero-client plan: %v", err)
	}
	if intermediateCalls != 1 {
		t.Fatalf("supervisor-intent step calls = %d, want 1", intermediateCalls)
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("read committed supervisor intent: %v", err)
	}
	if len(intent.Daemons) != 1 || intent.Daemons[0].Daemon != "alpha" {
		t.Fatalf("committed supervisor intent = %+v, want only selected alpha row", intent.Daemons)
	}
	after := snapshotRegularFiles(t, home)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("zero-client execution changed client artifacts:\nbefore=%v\nafter=%v", before, after)
	}
}

func TestRemoteSkipApplicabilityPrecedesSecrets(t *testing.T) {
	m := &config.ServerManifest{
		Name:            "remote-demo",
		Kind:            config.KindGlobal,
		Transport:       config.TransportRemoteHTTP,
		URL:             "https://example.invalid/${secret:missing_path}/mcp",
		Headers:         map[string]string{"Authorization": "Bearer ${secret:missing_header}"},
		RequiredSecrets: []string{"missing_required"},
	}
	scope := AdmissionScope{SkipClientConfigWrites: true}
	findings := AdmissionCheck(m, scope)
	if len(findings) != 1 || findings[0].ID != "install-scope-applicability" || findings[0].Optional {
		t.Fatalf("AdmissionCheck(remote skip) = %+v, want one terminal applicability blocker", findings)
	}
	if strings.Contains(findings[0].Reason, "secret") {
		t.Fatalf("applicability blocker leaked into secret resolution: %+v", findings[0])
	}
	var admissionErr *AdmissionError
	if err := preflightWithScope(m, scope); !errors.As(err, &admissionErr) || admissionErr.ID != "install-scope-applicability" {
		t.Fatalf("preflightWithScope(remote skip) = %v, want typed applicability error", err)
	}
	if _, err := BuildPlanWithOpts(m, BuildPlanOpts{SkipClientConfigWrites: true}); err == nil || !strings.Contains(err.Error(), "no local daemon to materialize") {
		t.Fatalf("BuildPlanWithOpts(remote skip) = %v, want applicability error", err)
	}
	rep := CheckServerReadinessWithScope(m, scope)
	if rep.Ready || len(rep.Requirements) != 1 || rep.Requirements[0].Name != "install scope" {
		t.Fatalf("readiness(remote skip) = %+v, want one terminal applicability requirement", rep)
	}
}

func TestOrdinaryReadinessStillReadsDefaultPreferenceOnce(t *testing.T) {
	calls := 0
	resolve := func(string) ([]string, error) {
		calls++
		return []string{"cursor", "claude-code"}, nil
	}
	got := resolveReadinessDefaultClientScope(AdmissionScope{}, resolve)
	if calls != 1 || !reflect.DeepEqual(got, []string{"cursor", "claude-code"}) {
		t.Fatalf("ordinary readiness resolution = %v, calls=%d", got, calls)
	}
	got = resolveReadinessDefaultClientScope(AdmissionScope{SkipClientConfigWrites: true}, resolve)
	if calls != 1 || got != nil {
		t.Fatalf("skip readiness resolution = %v, cumulative calls=%d; want nil and no additional read", got, calls)
	}
}

func snapshotRegularFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		got[rel] = string(body)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return got
}

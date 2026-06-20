package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

func TestAdmissionCheckCorpusPreflightReadinessParity(t *testing.T) {
	setupAdmissionParityTest(t)

	// Keep the strict parity corpus to scope-independent admission edges.
	// Caller-dependent client-binding validation (daemon bindings, client config
	// paths, url_path, and remote-http client matrix checks) intentionally lives
	// in the scoped planner. Preflight skips those by design; readiness surfaces
	// them through its effective-scope install-plan dry-run. The invalid-url-path
	// edge is pinned separately below.
	cases := admissionCorpusManifests(t)
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			preflightOK := Preflight(tc.manifest, "") == nil
			ready := CheckServerReadiness(tc.manifest).Ready
			if preflightOK != ready {
				t.Fatalf("Preflight == nil is %t, readiness Ready is %t", preflightOK, ready)
			}
		})
	}
}

func TestAdmissionCheckCallerDependentClientBindingSplit(t *testing.T) {
	setupAdmissionParityTest(t)

	m := syntheticInvalidURLPathAdmissionManifest()
	if err := Preflight(m, ""); err != nil {
		t.Fatalf("Preflight rejected caller-dependent client binding validation: %v", err)
	}

	rep := CheckServerReadiness(m)
	if rep.Ready {
		t.Fatalf("readiness Ready=true, want false for effective-scope install-plan blocker; requirements: %#v", rep.Requirements)
	}
	var installPlan *ReadinessRequirement
	for i := range rep.Requirements {
		if rep.Requirements[i].Name == "install plan" {
			installPlan = &rep.Requirements[i]
			break
		}
	}
	if installPlan == nil {
		t.Fatalf("readiness did not surface install-plan blocker: %#v", rep.Requirements)
	}
	if installPlan.OK {
		t.Fatalf("install-plan requirement OK=true, want false")
	}
	if !strings.Contains(installPlan.Reason, "invalid url_path") {
		t.Fatalf("install-plan reason = %q, want invalid url_path", installPlan.Reason)
	}
}

func TestAdmissionCheckReturnsUnionOfFindings(t *testing.T) {
	setupAdmissionParityTest(t)

	m := &config.ServerManifest{
		Name:             "multi-fail",
		Kind:             config.KindGlobal,
		Transport:        config.TransportNativeHTTP,
		Command:          "definitely-absent-launcher-zzzz",
		RequiredBinaries: []string{"definitely-absent-required-binary-zzzz"},
		Daemons: []config.DaemonSpec{
			{Name: "bad", Port: 70000},
		},
		Env: map[string]string{"BAD_FILE_REF": "file:local-only-key"},
	}

	findings := AdmissionCheck(m, AdmissionScope{})
	for _, want := range []string{
		"command-on-path",
		"required-binary",
		"external-port-range",
		"file-env-ref",
	} {
		if !hasAdmissionFinding(findings, want) {
			t.Fatalf("AdmissionCheck missing finding %q in %#v", want, findings)
		}
	}
}

func TestAdmissionCheckMarksAdvisoryFindingsOptional(t *testing.T) {
	setupAdmissionParityTest(t)

	m := &config.ServerManifest{
		Name:      "workspace-lsp",
		Kind:      config.KindWorkspaceScoped,
		Transport: config.TransportNativeHTTP,
		Command:   "definitely-absent-lsp-launcher-zzzz",
		Languages: []config.LanguageSpec{
			{Name: "rust", RequiredBinaries: []string{"definitely-absent-rust-analyzer-zzzz"}},
		},
	}

	findings := AdmissionCheck(m, AdmissionScope{})
	for _, id := range []string{"command-on-path", "language-required-binary"} {
		f, ok := admissionFindingByID(findings, id)
		if !ok {
			t.Fatalf("AdmissionCheck missing finding %q in %#v", id, findings)
		}
		if !f.Optional {
			t.Fatalf("finding %q Optional=false, want true", id)
		}
	}
}

// TestAdmissionCheckAdmitsCompanion guards the #381<->#382 merge integration: a
// kind=companion daemon (Port==0, no MCP port — it binds its own port) must NOT be
// flagged by AdmissionCheck's daemon port loop, so the merged AdmissionCheck refactor
// keeps the companion installable (the companion-skip was preserved through the refactor).
func TestAdmissionCheckAdmitsCompanion(t *testing.T) {
	setupAdmissionParityTest(t)
	m := &config.ServerManifest{
		Name:      "excalidraw-canvas",
		Kind:      config.KindCompanion,
		Transport: config.TransportProcess,
		Command:   "go",
		Daemons:   []config.DaemonSpec{{Name: "default", Cwd: t.TempDir()}},
	}
	if hasAdmissionFinding(AdmissionCheck(m, AdmissionScope{}), "external-port-range") {
		t.Fatal("AdmissionCheck flagged external-port-range for a companion (Port==0 is valid)")
	}
	if err := Preflight(m, ""); err != nil {
		t.Fatalf("Preflight rejected a companion: %v", err)
	}
}

func TestPreflightSkipsCallerDependentClientBindingValidation(t *testing.T) {
	setupAdmissionParityTest(t)

	m := &config.ServerManifest{
		Name:      "filtered-client-install",
		Kind:      config.KindGlobal,
		Transport: config.TransportNativeHTTP,
		Command:   "go",
		Daemons: []config.DaemonSpec{
			{Name: "vscode", Port: 54324},
		},
		ClientBindings: []config.ClientBinding{
			{Client: "claude-code", Daemon: "missing-default-daemon", URLPath: "/mcp"},
			{Client: "vscode", Daemon: "vscode", URLPath: "/mcp"},
		},
	}

	if err := Preflight(m, ""); err != nil {
		t.Fatalf("Preflight rejected a binding that the scoped install plan skips: %v", err)
	}
	if findings := AdmissionCheck(m, AdmissionScope{}); containsNonOptional(findings) {
		t.Fatalf("AdmissionCheck returned caller-dependent client binding findings: %#v", findings)
	}
	plan, err := BuildPlanWithOpts(m, BuildPlanOpts{ClientsInclude: []string{"vscode"}})
	if err != nil {
		t.Fatalf("BuildPlanWithOpts scoped to vscode: %v", err)
	}
	if got := len(plan.ClientUpdates); got != 1 {
		t.Fatalf("ClientUpdates len = %d, want 1", got)
	}
	if plan.ClientUpdates[0].Client != "vscode" {
		t.Fatalf("ClientUpdates[0].Client = %q, want vscode", plan.ClientUpdates[0].Client)
	}
}

// TestAdmissionCheckPoolExhaustionIsAdvisory is the #382 r3 guard: a dynamic-pool
// SERVER install/reinstall allocates NO pool port (workspaces allocate lazily at
// registration), so an exhausted pool must NOT block the install — it only means a
// NEW workspace cannot register until a port frees, and a reinstall whose own
// existing workspaces hold every port stays installable. The pool-free finding is
// still surfaced (operator sees the pool is full) but Optional, in BOTH gates.
func TestAdmissionCheckPoolExhaustionIsAdvisory(t *testing.T) {
	setupAdmissionParityTest(t)
	// Every pool port appears OS-bound — the exhausted-pool reinstall case.
	portAvailable = func(int) bool { return false }

	m := &config.ServerManifest{
		Name:      "exhausted-pool",
		Kind:      config.KindWorkspaceScoped,
		Transport: config.TransportNativeHTTP,
		Command:   "go",
		DaemonTemplate: &config.DaemonTemplate{
			Context:  "workspace",
			PortPool: &config.PortPool{Start: 55000, End: 55000},
		},
	}

	if err := Preflight(m, ""); err != nil {
		t.Fatalf("Preflight rejected an exhausted-pool reinstall: %v (pool exhaustion must be advisory)", err)
	}
	if !CheckServerReadiness(m).Ready {
		t.Fatal("CheckServerReadiness.Ready=false on pool exhaustion; pool exhaustion must be advisory")
	}
	f, ok := admissionFindingByID(AdmissionCheck(m, AdmissionScope{}), "port-pool-free")
	if !ok {
		t.Fatal("port-pool-free finding missing; the exhausted pool must still be surfaced (advisory)")
	}
	if !f.Optional {
		t.Fatal("port-pool-free finding is non-optional; it must be advisory so install/reinstall is not blocked")
	}
}

func TestAdmissionCheckTopLevelPoolExhaustionIsBlocking(t *testing.T) {
	setupAdmissionParityTest(t)
	// The legacy workspace-scoped LSP shape allocates from m.PortPool during
	// workspace registration, so a fully occupied top-level pool is not ready.
	portAvailable = func(int) bool { return false }

	m := &config.ServerManifest{
		Name:      "legacy-lsp-exhausted-pool",
		Kind:      config.KindWorkspaceScoped,
		Transport: config.TransportNativeHTTP,
		Command:   "go",
		PortPool:  &config.PortPool{Start: 55000, End: 55000},
		Languages: []config.LanguageSpec{
			{Name: "go", RequiredBinaries: []string{"go"}},
		},
	}

	if err := Preflight(m, ""); err == nil {
		t.Fatal("Preflight accepted an exhausted top-level workspace pool; want blocking rejection")
	}
	if CheckServerReadiness(m).Ready {
		t.Fatal("CheckServerReadiness.Ready=true on exhausted top-level workspace pool; want false")
	}
	f, ok := admissionFindingByID(AdmissionCheck(m, AdmissionScope{}), "port-pool-free")
	if !ok {
		t.Fatal("port-pool-free finding missing for exhausted top-level pool")
	}
	if f.Optional {
		t.Fatal("top-level port-pool-free finding is optional; want blocking")
	}
}

func TestAdmissionCheckNativeHTTPPoolOverflowIsBlocking(t *testing.T) {
	setupAdmissionParityTest(t)

	m := &config.ServerManifest{
		Name:      "native-http-overflow-pool",
		Kind:      config.KindWorkspaceScoped,
		Transport: config.TransportNativeHTTP,
		Command:   "go",
		DaemonTemplate: &config.DaemonTemplate{
			Context:  "workspace",
			PortPool: &config.PortPool{Start: 56000, End: 56000},
		},
	}

	if err := Preflight(m, ""); err == nil {
		t.Fatal("Preflight accepted a native-http pool whose upstream port exceeds 65535; want blocking rejection")
	}
	rep := CheckServerReadiness(m)
	if rep.Ready {
		t.Fatalf("CheckServerReadiness.Ready=true on native-http pool overflow; want false; requirements: %#v", rep.Requirements)
	}
	f, ok := admissionFindingByID(AdmissionCheck(m, AdmissionScope{}), "port-pool-native-overflow")
	if !ok {
		t.Fatal("port-pool-native-overflow finding missing for native-http pool overflow")
	}
	if f.Optional {
		t.Fatal("port-pool-native-overflow finding is optional; want blocking")
	}
}

type admissionCorpusCase struct {
	name     string
	manifest *config.ServerManifest
}

func admissionCorpusManifests(t *testing.T) []admissionCorpusCase {
	t.Helper()

	var out []admissionCorpusCase
	root := repoRootForAdmissionTest(t)
	paths, err := filepath.Glob(filepath.Join(root, "servers", "*", "manifest.yaml"))
	if err != nil {
		t.Fatalf("glob server manifests: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("no server manifests found under %s", filepath.Join(root, "servers"))
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		m, err := config.ParseManifest(strings.NewReader(string(data)))
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		out = append(out, admissionCorpusCase{
			name:     "server-" + m.Name,
			manifest: m,
		})
	}

	out = append(out,
		admissionCorpusCase{
			name: "synthetic-missing-binary",
			manifest: &config.ServerManifest{
				Name:             "missing-binary",
				Kind:             config.KindGlobal,
				Transport:        config.TransportNativeHTTP,
				Command:          "go",
				RequiredBinaries: []string{"definitely-absent-required-binary-zzzz"},
				Daemons:          []config.DaemonSpec{{Name: "main", Port: 54321}},
			},
		},
		admissionCorpusCase{
			name: "synthetic-out-of-range-port",
			manifest: &config.ServerManifest{
				Name:      "bad-port",
				Kind:      config.KindGlobal,
				Transport: config.TransportNativeHTTP,
				Command:   "go",
				Daemons:   []config.DaemonSpec{{Name: "main", Port: 70000}},
			},
		},
		admissionCorpusCase{
			name: "synthetic-missing-secret-optional",
			manifest: &config.ServerManifest{
				Name:      "missing-secret",
				Kind:      config.KindGlobal,
				Transport: config.TransportNativeHTTP,
				Command:   "go",
				Daemons:   []config.DaemonSpec{{Name: "main", Port: 54322}},
				Env:       map[string]string{"TOKEN": "secret:admission_missing_token_zzzz"},
			},
		},
		admissionCorpusCase{
			name: "synthetic-remote-secret-blocking",
			manifest: &config.ServerManifest{
				Name:      "remote-secret",
				Kind:      config.KindGlobal,
				Transport: config.TransportRemoteHTTP,
				URL:       "https://example.com/${secret:ADMISSION_REMOTE_TOKEN_ZZZZ}/mcp",
				ClientBindings: []config.ClientBinding{
					{Client: "claude-code"},
				},
			},
		},
		admissionCorpusCase{
			name: "synthetic-dynamic-pool",
			manifest: &config.ServerManifest{
				Name:      "dynamic-pool",
				Kind:      config.KindWorkspaceScoped,
				Transport: config.TransportNativeHTTP,
				Command:   "go",
				DaemonTemplate: &config.DaemonTemplate{
					Context:  "workspace",
					PortPool: &config.PortPool{Start: 55000, End: 55000},
				},
			},
		},
		admissionCorpusCase{
			name: "synthetic-companion-kind-excluded",
			manifest: &config.ServerManifest{
				Name:      "companion-kind",
				Kind:      "companion",
				Transport: config.TransportNativeHTTP,
				Command:   "go",
			},
		},
	)

	return out
}

func syntheticInvalidURLPathAdmissionManifest() *config.ServerManifest {
	return &config.ServerManifest{
		Name:      "invalid-url-path",
		Kind:      config.KindGlobal,
		Transport: config.TransportNativeHTTP,
		Command:   "go",
		Daemons:   []config.DaemonSpec{{Name: "main", Port: 54323}},
		ClientBindings: []config.ClientBinding{
			{Client: "claude-code", Daemon: "main", URLPath: "@evil.example/mcp"},
		},
	}
}

func setupAdmissionParityTest(t *testing.T) {
	t.Helper()
	preparePreflightBinaryChecks(t)

	root := t.TempDir()
	for _, dir := range []string{
		filepath.Join(root, "Local"),
		filepath.Join(root, "Roaming"),
		filepath.Join(root, "Home"),
		filepath.Join(root, "State"),
		filepath.Join(root, "Config"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "Local"))
	t.Setenv("APPDATA", filepath.Join(root, "Roaming"))
	t.Setenv("USERPROFILE", filepath.Join(root, "Home"))
	t.Setenv("HOME", filepath.Join(root, "Home"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "State"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "Config"))

	origPortAvailable := portAvailable
	origPreflightPortInUse := preflightPortInUse
	origRegistryPathFn := defaultRegistryPathFn
	t.Cleanup(func() {
		portAvailable = origPortAvailable
		preflightPortInUse = origPreflightPortInUse
		defaultRegistryPathFn = origRegistryPathFn
	})
	portAvailable = func(int) bool { return true }
	preflightPortInUse = func(int) bool { return false }
	defaultRegistryPathFn = func() (string, error) {
		return filepath.Join(root, "State", "mcp-local-hub", "workspaces.yaml"), nil
	}
}

func repoRootForAdmissionTest(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate repository root from test working directory")
		}
		dir = parent
	}
}

func hasAdmissionFinding(findings []AdmissionFinding, id string) bool {
	_, ok := admissionFindingByID(findings, id)
	return ok
}

func admissionFindingByID(findings []AdmissionFinding, id string) (AdmissionFinding, bool) {
	for _, f := range findings {
		if f.ID == id {
			return f, true
		}
	}
	return AdmissionFinding{}, false
}

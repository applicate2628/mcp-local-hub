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
		admissionCorpusCase{
			name: "synthetic-invalid-url-path",
			manifest: &config.ServerManifest{
				Name:      "invalid-url-path",
				Kind:      config.KindGlobal,
				Transport: config.TransportNativeHTTP,
				Command:   "go",
				Daemons:   []config.DaemonSpec{{Name: "main", Port: 54323}},
				ClientBindings: []config.ClientBinding{
					{Client: "claude-code", Daemon: "main", URLPath: "@evil.example/mcp"},
				},
			},
		},
	)

	return out
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

package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/clients"
)

// adoptTestHome installs the sandbox both adopt CLI tests need and returns
// (root, home).
//
// `mcphub adopt` builds its plan through api.BuildAdoptPlan → adoptScanOpts →
// api.DefaultScanConfigPaths (internal/api/scan.go:2264), which resolves
// clients.ConfigPathForName for EVERY registered client and then scans those
// files — regardless of the `--client` narrowing on the command line. So a home
// redirect alone is not isolation here: before this fixture existed, both tests
// below read the operator's REAL %APPDATA%\Code\User\mcp.json,
// ~/.config/mimocode/mimocode.json and the rest of the non-home adapters. That
// is a READ leak, and a read leak is not harmless — it makes the assertions
// depend on whatever the host happens to have installed.
//
// neutralizeClientConfigPathEnv (client_config_env_isolation_test.go) is the one
// owner of the full non-home env set; never re-type the list here.
func adoptTestHome(t *testing.T) (root, home string) {
	t.Helper()
	root = t.TempDir()
	home = filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "localappdata"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "xdg-data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg-state"))
	neutralizeClientConfigPathEnv(t, home)
	return root, home
}

func cliAdoptDefaultManifestDir(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		return filepath.Join(string(os.PathSeparator), "nonexistent", "mcphub", "servers")
	}
	exeDir := filepath.Dir(exe)
	if sibling := filepath.Join(exeDir, "servers"); isDir(sibling) {
		return sibling
	}
	if parent := filepath.Join(exeDir, "..", "servers"); isDir(parent) {
		return parent
	}
	return filepath.Join(exeDir, "servers")
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func TestAdoptCmdDryRunByDefaultMutatesNothingAndRedactsSecrets(t *testing.T) {
	root, home := adoptTestHome(t)
	manifestDir := filepath.Join(root, "manifests")
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)

	codexPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o700); err != nil {
		t.Fatalf("mkdir codex config parent: %v", err)
	}
	initial := `[mcp_servers.mui-dry-cli]
command = "go"
args = ["version"]

[mcp_servers.mui-dry-cli.env]
API_KEY = "cli-secret-value"
`
	if err := os.WriteFile(codexPath, []byte(initial), 0o600); err != nil {
		t.Fatalf("seed codex config: %v", err)
	}

	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"adopt", "mui-dry-cli", "--client", "codex-cli", "--port", "9313"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("adopt dry-run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "dry-run") {
		t.Fatalf("dry-run output missing dry-run marker:\n%s", out.String())
	}
	if strings.Contains(out.String(), "cli-secret-value") {
		t.Fatalf("dry-run output leaked secret value:\n%s", out.String())
	}
	after, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatalf("read codex config after dry-run: %v", err)
	}
	if string(after) != initial {
		t.Fatalf("codex config mutated during dry-run\nbefore:\n%s\nafter:\n%s", initial, after)
	}
	if entries, err := os.ReadDir(manifestDir); err == nil && len(entries) > 0 {
		t.Fatalf("dry-run wrote manifest entries under override dir: %v", entries)
	}
}

func TestAdoptCmdRequiresNameToMatchEntry(t *testing.T) {
	_, home := adoptTestHome(t)

	codexPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o700); err != nil {
		t.Fatalf("mkdir codex config parent: %v", err)
	}
	if err := os.WriteFile(codexPath, []byte(`[mcp_servers.mui-name-cli]
command = "go"
args = ["version"]
`), 0o600); err != nil {
		t.Fatalf("seed codex config: %v", err)
	}

	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"adopt", "mui-name-cli", "--client", "codex-cli", "--name", "mui-name-custom", "--port", "9314"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("adopt with mismatched --name succeeded")
	}
	if !strings.Contains(err.Error(), "--name") {
		t.Fatalf("error = %v, want --name context", err)
	}
}

// TestAdoptLeaseFailureErrorRedactsCanary is the local-CLI renderer boundary:
// Cobra returns the API error unchanged to the composition root, so the closed
// stage string is the only terminal value it is allowed to render. Raw
// namespace paths, token-like text, controls, and oversized causes stay behind
// the API's typed error boundary.
type cliCleanupFailureLeaseOwner struct {
	inner api.AdoptLeaseOwner
	cause error
}

func (o cliCleanupFailureLeaseOwner) AcquireAdoptLease(manifest string) (api.AdoptLease, bool, error) {
	lease, acquired, err := o.inner.AcquireAdoptLease(manifest)
	if err != nil || !acquired {
		return lease, acquired, err
	}
	return cliCleanupFailureLease{inner: lease, cause: o.cause}, true, nil
}

type cliCleanupFailureLease struct {
	inner api.AdoptLease
	cause error
}

func (l cliCleanupFailureLease) Unlock() error { return l.inner.Unlock() }
func (l cliCleanupFailureLease) ReleaseAndRemove() error {
	if err := l.inner.ReleaseAndRemove(); err != nil {
		return err
	}
	return l.cause
}

func TestAdoptLeaseCleanupFailureKeepsCLIChannelsRedacted(t *testing.T) {
	root, home := adoptTestHome(t)
	entry := "cli-lease-cleanup"
	secret := "cli-source-secret-DO-NOT-LEAK"
	canary := `Z:\\private-user\\adopt-provenance\\cli-lease-cleanup.lease token=cli-unlock-canary` + "\x1b[2J"
	manifestRoot := cliAdoptDefaultManifestDir(t)
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestRoot)
	if err := os.MkdirAll(manifestRoot, 0o700); err != nil {
		t.Fatalf("mkdir manifest root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(manifestRoot, entry)) })
	t.Cleanup(api.SetDaemonStateRootForTest(apitest.HardenedTempDir(t)))
	canonical := filepath.Join(root, "bin", api.MCPHubBinaryName())
	if err := os.MkdirAll(filepath.Dir(canonical), 0o700); err != nil {
		t.Fatalf("mkdir canonical parent: %v", err)
	}
	if err := os.WriteFile(canonical, []byte("test mcphub"), 0o700); err != nil {
		t.Fatalf("write canonical stub: %v", err)
	}
	t.Cleanup(api.SetTestCanonicalMcphubPath(canonical))
	codexPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o700); err != nil {
		t.Fatalf("mkdir codex config parent: %v", err)
	}
	if err := os.WriteFile(codexPath, []byte(`[mcp_servers.cli-lease-cleanup]
command = "go"
args = ["version"]
`), 0o600); err != nil {
		t.Fatalf("seed codex config: %v", err)
	}

	cmd := newAdoptCmdWithDeps(api.NewAPI, cliCleanupFailureLeaseOwner{inner: api.NewAdoptLeaseOwner(), cause: errors.New(canary)}, func(*api.API, *api.AdoptPlan, *api.AdoptProvenanceRecord) error { return nil })
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{entry, "--client", "codex-cli", "--port", "9342", "--yes"})
	err := cmd.Execute()
	if err == nil || err.Error() != "E_ADOPT_LEASE_CLEANUP" {
		t.Fatalf("CLI exit error=%v, want E_ADOPT_LEASE_CLEANUP", err)
	}
	for channel, text := range map[string]string{"stdout": stdout.String(), "stderr": stderr.String(), "exit": err.Error()} {
		if strings.Contains(text, canary) || strings.Contains(text, secret) || strings.Contains(text, `Z:\\private-user`) {
			t.Fatalf("CLI %s leaked canary/path/secret: %q", channel, text)
		}
		if strings.Contains(text, "Adopted ") || strings.Contains(text, "Install complete") {
			t.Fatalf("CLI %s emitted success narration before failed settlement: %q", channel, text)
		}
	}
}

func TestAdoptCLI_GlobalCodexSameNamePathHasNoHSettlement(t *testing.T) {
	root, home := adoptTestHome(t)
	entry := "cli-global-only-codex"
	manifestRoot := cliAdoptDefaultManifestDir(t)
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestRoot)
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(manifestRoot, entry)) })
	stateRoot := apitest.HardenedTempDir(t)
	t.Cleanup(api.SetDaemonStateRootForTest(stateRoot))
	canonical := filepath.Join(root, "bin", api.MCPHubBinaryName())
	if err := os.MkdirAll(filepath.Dir(canonical), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canonical, []byte("test mcphub"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(api.SetTestCanonicalMcphubPath(canonical))
	codexPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexPath, []byte("[mcp_servers."+entry+"]\ncommand = \"go\"\nargs = [\"version\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newAdoptCmdWithDeps(api.NewAPI, nil, func(*api.API, *api.AdoptPlan, *api.AdoptProvenanceRecord) error { return nil })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{entry, "--client", "codex-cli", "--port", "9344", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("global-only adopt: %v\n%s", err, out.String())
	}
	client := clients.AllClients()["codex-cli"]
	settled, err := client.GetEntry(entry)
	if err != nil || settled == nil || settled.URL == "" {
		t.Fatalf("same-name global entry = %#v, err = %v", settled, err)
	}
	alias, err := client.GetEntry(entry + "-mcphub")
	if err != nil || alias != nil {
		t.Fatalf("unexpected global alias = %#v, err = %v", alias, err)
	}
	if strings.Contains(out.String(), "client-config-settled") || cmd.Flags().Lookup("codex-project-root") != nil {
		t.Fatalf("global-only CLI fabricated H evidence or project flag: %q", out.String())
	}
	if raw, readErr := os.ReadFile(filepath.Join(stateRoot, api.SupervisorEventLogFileLeaf)); readErr == nil && strings.Contains(string(raw), `"event":"client-config-settled"`) {
		t.Fatalf("global-only CLI emitted H event: %s", raw)
	}
}

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/secrets"

	toml "github.com/pelletier/go-toml/v2"
)

func setupAdoptTestEnv(t *testing.T, entryName, body string) (codexPath, manifestRoot, stateRoot string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "localappdata"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "xdg-data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg-state"))
	canonical := filepath.Join(root, mcphubShortName)
	if err := os.WriteFile(canonical, []byte("test mcphub"), 0o700); err != nil {
		t.Fatalf("seed canonical mcphub path: %v", err)
	}
	prevCanonical := testCanonicalMcphubPathOverride
	testCanonicalMcphubPathOverride = canonical
	t.Cleanup(func() { testCanonicalMcphubPathOverride = prevCanonical })

	codexPath = filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o700); err != nil {
		t.Fatalf("mkdir codex config parent: %v", err)
	}
	if err := os.WriteFile(codexPath, []byte(body), 0o600); err != nil {
		t.Fatalf("seed codex config: %v", err)
	}

	manifestRoot = defaultManifestDir()
	if err := os.MkdirAll(manifestRoot, 0o700); err != nil {
		t.Fatalf("mkdir default manifest dir: %v", err)
	}
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestRoot)
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(manifestRoot, entryName)) })

	stateRoot = filepath.Join(root, "state")
	t.Cleanup(SetDaemonStateRootForTest(stateRoot))
	return codexPath, manifestRoot, stateRoot
}

func TestBuildAdoptPlanDryRunMutatesNothing(t *testing.T) {
	entry := "mui-adopt-dry"
	codexPath, manifestRoot, stateRoot := setupAdoptTestEnv(t, entry, `[profile.default]
model = "gpt-5"

[mcp_servers.mui-adopt-dry]
command = "go"
args = ["version"]
`)
	before, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatalf("read seeded codex config: %v", err)
	}

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         9307,
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if plan.Port != 9307 {
		t.Fatalf("Port = %d, want 9307", plan.Port)
	}
	if !reflect.DeepEqual(plan.AdoptClients, []string{"codex-cli"}) {
		t.Fatalf("AdoptClients = %#v, want [codex-cli]", plan.AdoptClients)
	}
	if _, err := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); !os.IsNotExist(err) {
		t.Fatalf("manifest dry-run side effect err = %v, want not exist", err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, supervisorIntentFileLeaf)); !os.IsNotExist(err) {
		t.Fatalf("intent dry-run side effect err = %v, want not exist", err)
	}
	after, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatalf("read codex config after plan: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("codex config changed during BuildAdoptPlan\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestExecuteAdoptWritesManifestIntentAndRepointsCodexPreservingForeignTables(t *testing.T) {
	entry := "mui-adopt-full"
	codexPath, manifestRoot, stateRoot := setupAdoptTestEnv(t, entry, `[profile.default]
model = "gpt-5"

[mcp_servers.keep]
url = "http://example.invalid/mcp"

[mcp_servers.mui-adopt-full]
command = "go"
args = ["version"]
`)

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         9308,
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	var out bytes.Buffer
	if err := NewAPI().ExecuteAdopt(plan, &out); err != nil {
		t.Fatalf("ExecuteAdopt: %v\n%s", err, out.String())
	}

	manifestBytes, err := os.ReadFile(filepath.Join(manifestRoot, entry, "manifest.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	m, err := config.ParseManifest(bytes.NewReader(manifestBytes))
	if err != nil {
		t.Fatalf("ParseManifest: %v\n%s", err, manifestBytes)
	}
	if m.Name != entry || m.Transport != config.TransportStdioBridge || m.Daemons[0].Port != 9308 {
		t.Fatalf("manifest = name %q transport %q port %d, want %q stdio-bridge 9308", m.Name, m.Transport, m.Daemons[0].Port, entry)
	}
	if len(m.ClientBindings) != 1 || m.ClientBindings[0].Client != "codex-cli" {
		t.Fatalf("client_bindings = %#v, want only codex-cli", m.ClientBindings)
	}

	intent, err := ReadSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf))
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if len(intent.Daemons) != 1 {
		t.Fatalf("intent daemons = %d, want 1 (%#v)", len(intent.Daemons), intent.Daemons)
	}
	if got := intent.Daemons[0].Server; got != entry {
		t.Fatalf("intent server = %q, want %q", got, entry)
	}

	var root map[string]any
	codexBytes, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatalf("read codex TOML: %v", err)
	}
	if err := toml.Unmarshal(codexBytes, &root); err != nil {
		t.Fatalf("decode codex TOML: %v", err)
	}
	if _, ok := root["profile"].(map[string]any)["default"]; !ok {
		t.Fatalf("foreign [profile.default] table was not preserved: %#v", root["profile"])
	}
	servers := root["mcp_servers"].(map[string]any)
	if _, ok := servers["keep"]; !ok {
		t.Fatalf("foreign mcp_servers.keep table was not preserved: %#v", servers)
	}
	adopted := servers[entry].(map[string]any)
	if adopted["url"] != "http://127.0.0.1:9308/mcp" {
		t.Fatalf("adopted url = %#v, want http://127.0.0.1:9308/mcp", adopted["url"])
	}
	if _, hasCommand := adopted["command"]; hasCommand {
		t.Fatalf("adopted entry still has command: %#v", adopted)
	}

	logBytes, err := os.ReadFile(filepath.Join(stateRoot, SupervisorEventLogFileLeaf))
	if err != nil {
		t.Fatalf("read supervisor-events.log: %v", err)
	}
	if !strings.Contains(string(logBytes), `"source":"adopt"`) || !strings.Contains(string(logBytes), `"entry":"`+entry+`"`) {
		t.Fatalf("adopt audit row missing from supervisor-events.log:\n%s", logBytes)
	}
}

func TestExecuteAdoptRollbackRestoresDirectStdioPriorFromBackupOnSecondClientFailure(t *testing.T) {
	entry := "mui-adopt-rollback"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-rollback]
command = "go"
args = ["version"]
`)
	home := filepath.Dir(filepath.Dir(codexPath))
	claudePath := filepath.Join(home, ".claude.json")
	originalClaudeEntry := map[string]any{
		"type":    "stdio",
		"command": "go",
		"args":    []any{"version"},
		"env":     map[string]any{"KEEP": "yes"},
	}
	claudeRaw, err := json.Marshal(map[string]any{
		"mcpServers": map[string]any{entry: originalClaudeEntry},
	})
	if err != nil {
		t.Fatalf("marshal claude config: %v", err)
	}
	if err := os.WriteFile(claudePath, claudeRaw, 0o600); err != nil {
		t.Fatalf("seed claude config: %v", err)
	}
	failClientConfigWritesForAdoptTest(t, codexPath)

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         9315,
		Clients:      []string{"claude-code", "codex-cli"},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	var out bytes.Buffer
	err = NewAPI().ExecuteAdopt(plan, &out)
	if err == nil {
		t.Fatalf("ExecuteAdopt succeeded; want induced codex write failure\noutput:\n%s", out.String())
	}

	var root map[string]any
	afterBytes, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatalf("read claude config after rollback: %v", err)
	}
	if err := json.Unmarshal(afterBytes, &root); err != nil {
		t.Fatalf("decode claude config after rollback: %v\n%s", err, afterBytes)
	}
	servers, ok := root["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing after rollback: %#v", root)
	}
	got, ok := servers[entry].(map[string]any)
	if !ok {
		t.Fatalf("entry %q missing after rollback: %#v", entry, servers)
	}
	if !reflect.DeepEqual(got, originalClaudeEntry) {
		t.Fatalf("claude entry after rollback:\n got: %#v\nwant: %#v\noutput:\n%s", got, originalClaudeEntry, out.String())
	}
}

func TestAdoptSensitiveLiteralRoutesToVaultAndRedactsPlan(t *testing.T) {
	entry := "mui-adopt-secret"
	_, manifestRoot, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-secret]
command = "go"
args = ["version"]

[mcp_servers.mui-adopt-secret.env]
API_KEY = "literal-secret-value"
VISIBLE = "not-secret"
`)
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         9309,
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if !reflect.DeepEqual(plan.SecretRoutedKeys, []string{"API_KEY"}) {
		t.Fatalf("SecretRoutedKeys = %#v, want [API_KEY]", plan.SecretRoutedKeys)
	}
	var dryRun bytes.Buffer
	PrintAdoptPlan(&dryRun, plan)
	if strings.Contains(dryRun.String(), "literal-secret-value") || strings.Contains(plan.ManifestYAML, "literal-secret-value") {
		t.Fatalf("dry-run/manifest leaked secret value\nplan:\n%s\nmanifest:\n%s", dryRun.String(), plan.ManifestYAML)
	}
	if !strings.Contains(plan.ManifestYAML, "secret:API_KEY") {
		t.Fatalf("manifest did not rewrite sensitive value to secret ref:\n%s", plan.ManifestYAML)
	}

	if err := NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{}); err != nil {
		t.Fatalf("ExecuteAdopt: %v", err)
	}
	vault, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		t.Fatalf("OpenVault: %v", err)
	}
	got, err := vault.Get("API_KEY")
	if err != nil {
		t.Fatalf("vault.Get(API_KEY): %v", err)
	}
	if got != "literal-secret-value" {
		t.Fatalf("vault secret = %q, want literal-secret-value", got)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(manifestRoot, entry, "manifest.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if strings.Contains(string(manifestBytes), "literal-secret-value") {
		t.Fatalf("persisted manifest leaked secret value:\n%s", manifestBytes)
	}
}

func TestBuildAdoptPlanRejectsExplicitPortOutsideAdoptRangeBeforeMutation(t *testing.T) {
	entry := "mui-adopt-port-range"
	codexPath, manifestRoot, stateRoot := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-port-range]
command = "go"
args = ["version"]
`)
	before := mustReadFileForAdoptTest(t, codexPath)

	_, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         9128,
	})
	if err == nil {
		t.Fatal("BuildAdoptPlan accepted explicit port outside adopt range")
	}
	if !strings.Contains(err.Error(), "9300-9399") {
		t.Fatalf("explicit port range error = %v, want adopt range", err)
	}
	assertAdoptPlanMutationFree(t, codexPath, before, manifestRoot, stateRoot, entry)
}

func TestBuildAdoptPlanRejectsExplicitUsedPortBeforeMutation(t *testing.T) {
	entry := "mui-adopt-port-used"
	codexPath, manifestRoot, stateRoot := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-port-used]
command = "go"
args = ["version"]
`)
	usedPort := nextBindableAdoptPortForTest(t, map[int]bool{})
	usedName := "mui-adopt-port-owner"
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(manifestRoot, usedName)) })
	if err := os.MkdirAll(filepath.Join(manifestRoot, usedName), 0o700); err != nil {
		t.Fatalf("mkdir used-port manifest: %v", err)
	}
	usedManifest := "name: " + usedName + "\nkind: global\ntransport: stdio-bridge\ncommand: go\ndaemons:\n  - name: default\n    port: " + strconv.Itoa(usedPort) + "\n"
	if err := os.WriteFile(filepath.Join(manifestRoot, usedName, "manifest.yaml"), []byte(usedManifest), 0o600); err != nil {
		t.Fatalf("write used-port manifest: %v", err)
	}
	before := mustReadFileForAdoptTest(t, codexPath)

	_, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         usedPort,
	})
	if err == nil {
		t.Fatal("BuildAdoptPlan accepted explicit port already present in manifest pool")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("explicit used-port error = %v, want already in use", err)
	}
	assertAdoptPlanMutationFree(t, codexPath, before, manifestRoot, stateRoot, entry)
}

func TestExecuteAdoptInstallFailureRemovesAdoptCreatedManifestAndSaysRerun(t *testing.T) {
	entry := "mui-adopt-install-fail"
	codexPath, manifestRoot, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-install-fail]
command = "go"
args = ["version"]
`)
	failClientConfigWritesForAdoptTest(t, codexPath)

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         9316,
		Clients:      []string{"codex-cli"},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	err = NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{})
	if err == nil {
		t.Fatal("ExecuteAdopt succeeded; want induced install failure")
	}
	if !strings.Contains(err.Error(), "adopt can be re-run") {
		t.Fatalf("ExecuteAdopt error = %v, want re-run guidance", err)
	}
	if _, statErr := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("adopt install failure left orphan manifest: %v", statErr)
	}
	if _, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         9316,
		Clients:      []string{"codex-cli"},
	}); err != nil {
		t.Fatalf("BuildAdoptPlan after failed adopt: %v", err)
	}
}

func TestExecuteAdoptRefusesUnavailableVaultBeforeManifestWrite(t *testing.T) {
	entry := "mui-adopt-no-vault"
	_, manifestRoot, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-no-vault]
command = "go"
args = ["version"]

[mcp_servers.mui-adopt-no-vault.env]
API_TOKEN = "literal-token"
`)
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         9310,
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	err = NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{})
	if err == nil {
		t.Fatal("ExecuteAdopt succeeded without an initialized vault")
	}
	if !strings.Contains(err.Error(), "vault") {
		t.Fatalf("ExecuteAdopt error = %v, want vault context", err)
	}
	if _, statErr := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("manifest written despite vault refusal: %v", statErr)
	}
}

func TestBuildAdoptPlanRefusesNameMismatchAndEmbeddedName(t *testing.T) {
	setupAdoptTestEnv(t, "serena", `[mcp_servers.serena]
command = "go"
args = ["version"]
`)
	if _, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    "serena",
		Client:       "codex-cli",
		ManifestName: "serena-custom",
		Port:         9311,
	}); err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("name mismatch err = %v, want --name refusal", err)
	}
	if _, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: "serena",
		Client:    "codex-cli",
		Port:      9311,
	}); err == nil || !strings.Contains(err.Error(), "shipped") {
		t.Fatalf("embedded collision err = %v, want shipped-name refusal", err)
	}
}

func TestPickNextFreeAdoptPortSkipsDiskEmbedIntentAndBoundPorts(t *testing.T) {
	embedPort := 9300
	prevEmbeddedCollector := collectEmbeddedManifestPortsFn
	collectEmbeddedManifestPortsFn = func(used map[int]bool) {
		prevEmbeddedCollector(used)
		used[embedPort] = true
	}
	t.Cleanup(func() { collectEmbeddedManifestPortsFn = prevEmbeddedCollector })
	manifestRoot := defaultManifestDir()
	if err := os.MkdirAll(manifestRoot, 0o700); err != nil {
		t.Fatalf("mkdir default manifest dir: %v", err)
	}

	used := map[int]bool{embedPort: true}
	diskPort := nextBindableAdoptPortForTest(t, used)
	used[diskPort] = true
	intentPort := nextBindableAdoptPortForTest(t, used)
	used[intentPort] = true
	boundPort := nextBindableAdoptPortForTest(t, used)
	used[boundPort] = true
	expected := nextBindableAdoptPortForTest(t, used)

	diskName := "adopt-port-disk"
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(manifestRoot, diskName)) })
	if err := os.MkdirAll(filepath.Join(manifestRoot, diskName), 0o700); err != nil {
		t.Fatalf("mkdir disk manifest: %v", err)
	}
	diskManifest := "name: adopt-port-disk\nkind: global\ntransport: stdio-bridge\ncommand: go\ndaemons:\n  - name: default\n    port: " + strconv.Itoa(diskPort) + "\n"
	if err := os.WriteFile(filepath.Join(manifestRoot, diskName, "manifest.yaml"), []byte(diskManifest), 0o600); err != nil {
		t.Fatalf("write disk manifest: %v", err)
	}

	stateRoot := t.TempDir()
	t.Cleanup(SetDaemonStateRootForTest(stateRoot))
	if err := WriteSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf), &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: "\\mcp-local-hub-adopt-port-intent-default",
			Server:   "adopt-port-intent",
			Daemon:   "default",
			Port:     intentPort,
		}},
	}); err != nil {
		t.Fatalf("seed supervisor intent: %v", err)
	}

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(boundPort)))
	if err != nil {
		t.Fatalf("bind selected port %d: %v", boundPort, err)
	}
	defer ln.Close()

	got, err := pickNextFreeAdoptPort()
	if err != nil {
		t.Fatalf("pickNextFreeAdoptPort: %v", err)
	}
	if got != expected {
		t.Fatalf("pickNextFreeAdoptPort = %d, want %d (embed=%d disk=%d intent=%d bound=%d)", got, expected, embedPort, diskPort, intentPort, boundPort)
	}
}

type ioDiscardForAdoptTest struct{}

func (ioDiscardForAdoptTest) Write(p []byte) (int, error) { return len(p), nil }

func failClientConfigWritesForAdoptTest(t *testing.T, failPath string) {
	t.Helper()
	cleanFailPath := filepath.Clean(failPath)
	orig := clients.WriteConfigFile
	clients.WriteConfigFile = func(path string, contents []byte) error {
		if filepath.Clean(path) == cleanFailPath {
			return fmt.Errorf("induced client config write failure for %s", filepath.Base(path))
		}
		if dir := filepath.Dir(path); dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
		}
		return os.WriteFile(path, contents, 0o600)
	}
	t.Cleanup(func() { clients.WriteConfigFile = orig })
}

func mustReadFileForAdoptTest(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func assertAdoptPlanMutationFree(t *testing.T, codexPath string, before []byte, manifestRoot, stateRoot, entry string) {
	t.Helper()
	after := mustReadFileForAdoptTest(t, codexPath)
	if !bytes.Equal(before, after) {
		t.Fatalf("codex config changed despite BuildAdoptPlan refusal\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if _, err := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); !os.IsNotExist(err) {
		t.Fatalf("manifest side effect after BuildAdoptPlan refusal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, supervisorIntentFileLeaf)); !os.IsNotExist(err) {
		t.Fatalf("intent side effect after BuildAdoptPlan refusal: %v", err)
	}
}

func nextBindableAdoptPortForTest(t *testing.T, used map[int]bool) int {
	t.Helper()
	for p := adoptPortStart; p <= adoptPortEnd; p++ {
		if used[p] {
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			used[p] = true
			continue
		}
		_ = ln.Close()
		return p
	}
	t.Fatalf("no bindable adopt port on %s", runtime.GOOS)
	return 0
}

func TestAdoptPlanOutputReportsOmittedSameNameClients(t *testing.T) {
	entry := "mui-adopt-report"
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "localappdata"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "xdg-data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg-state"))
	t.Cleanup(SetDaemonStateRootForTest(filepath.Join(root, "state")))

	codexPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexPath, []byte(`[mcp_servers.mui-adopt-report]
command = "go"
args = ["version"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(home, ".claude.json")
	claudeRaw, _ := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			entry: map[string]any{"command": "go", "args": []string{"version"}},
		},
	})
	if err := os.WriteFile(claudePath, claudeRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         9312,
		Clients:      []string{"codex-cli"},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	var out bytes.Buffer
	PrintAdoptPlan(&out, plan)
	if !strings.Contains(out.String(), "also present in claude-code") {
		t.Fatalf("dry-run omitted same-name report:\n%s", out.String())
	}
}

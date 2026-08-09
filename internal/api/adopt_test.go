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
	"sync"
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
	// The four env vars above cover only 4 of the 47 registered adapters. Every
	// test built on this fixture calls BuildAdoptPlan with an EMPTY ScanOpts, and
	// adoptScanOpts (adopt.go:403-405) then falls back to DefaultScanConfigPaths,
	// which resolves clients.ConfigPathForName for EVERY client and READS each
	// resolved file (scan.go:2398) — regardless of the fixture's `--client`
	// narrowing. Before this line the adopt tests read the operator's real
	// %APPDATA%\Code\User\mcp.json et al, and a same-named stdio entry there would
	// have promoted vscode into plan.AdoptClients, at which point a.Install
	// (adopt.go:318) BACKS UP, PRUNES BACKUPS OF, and REWRITES that real file. It
	// held only because the operator's real entries happen not to collide by name
	// and are `type: remote`. See client_config_env_isolation_test.go.
	neutralizeClientConfigPathEnv(t, home)
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

func TestExecuteAdoptScopedConsentDoesNotAuthorizeConcurrentNonAdoptWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Log("attempting Windows symlink test; host may require Developer Mode or elevation")
	}
	entry := "mui-adopt-scoped-concurrent"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-scoped-concurrent]
command = "go"
args = ["version"]
`)
	home := filepath.Dir(filepath.Dir(codexPath))
	realTarget := filepath.Join(home, "dotfiles", ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(realTarget), 0o700); err != nil {
		t.Fatalf("mkdir adopt real target parent: %v", err)
	}
	if err := os.Rename(codexPath, realTarget); err != nil {
		t.Fatalf("move codex config to real target: %v", err)
	}
	if err := os.Symlink(realTarget, codexPath); err != nil {
		t.Skipf("symlink unsupported on this host: %v", err)
	}
	_, pinned, was := ResolveClientConfigSymlink(codexPath)
	if !was {
		t.Fatalf("codex config was not detected as symlink")
	}
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry,
		Client:    "codex-cli",
		Port:      9318,
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}

	otherRoot := hardenedTempDir(t)
	otherLink := filepath.Join(otherRoot, "other-client.json")
	otherTarget := filepath.Join(otherRoot, "dotfiles", "other-client.json")
	if err := os.MkdirAll(filepath.Dir(otherTarget), 0o700); err != nil {
		t.Fatalf("mkdir other real target parent: %v", err)
	}
	if err := os.WriteFile(otherTarget, []byte(`{"before":true}`), 0o600); err != nil {
		t.Fatalf("write other real target: %v", err)
	}
	if err := os.Symlink(otherTarget, otherLink); err != nil {
		t.Skipf("symlink unsupported on this host: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	prevHook := afterResolveBeforePinHook
	afterResolveBeforePinHook = func() {
		once.Do(func() { close(entered) })
		<-release
	}
	t.Cleanup(func() { afterResolveBeforePinHook = prevHook })

	adoptErr := make(chan error, 1)
	go func() {
		adoptErr <- NewAPI().ExecuteAdoptWithOpts(plan, ioDiscardForAdoptTest{}, ExecuteAdoptOpts{
			SymlinkConsents: []ResolvedSymlinkConsent{{
				Client:             "codex-cli",
				OriginalPath:       codexPath,
				PinnedResolvedPath: pinned,
			}},
		})
	}()
	<-entered

	err = SecureWriteClientConfig(otherLink, []byte(`{"after":true}`))
	if err == nil {
		close(release)
		t.Fatal("concurrent non-adopt symlink write succeeded without scoped consent")
	}
	if got := strings.ToLower(err.Error()); !strings.Contains(got, "symlink") && !strings.Contains(got, "reparse") {
		close(release)
		t.Fatalf("concurrent non-adopt error = %v, want symlink/reparse refusal", err)
	}
	otherBytes, err := os.ReadFile(otherTarget)
	if err != nil {
		close(release)
		t.Fatalf("read other target: %v", err)
	}
	if string(otherBytes) != `{"before":true}` {
		close(release)
		t.Fatalf("concurrent non-adopt write changed target: %s", otherBytes)
	}

	close(release)
	if err := <-adoptErr; err != nil {
		t.Fatalf("ExecuteAdoptWithOpts: %v", err)
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
	wantVaultKey := "MUI_ADOPT_SECRET_API_KEY"
	if !reflect.DeepEqual(plan.SecretRoutedKeys, []string{wantVaultKey}) {
		t.Fatalf("SecretRoutedKeys = %#v, want [%s]", plan.SecretRoutedKeys, wantVaultKey)
	}
	var dryRun bytes.Buffer
	PrintAdoptPlan(&dryRun, plan)
	if strings.Contains(dryRun.String(), "literal-secret-value") || strings.Contains(plan.ManifestYAML, "literal-secret-value") {
		t.Fatalf("dry-run/manifest leaked secret value\nplan:\n%s\nmanifest:\n%s", dryRun.String(), plan.ManifestYAML)
	}
	if !strings.Contains(plan.ManifestYAML, "secret:"+wantVaultKey) {
		t.Fatalf("manifest did not rewrite sensitive value to secret ref:\n%s", plan.ManifestYAML)
	}

	if err := NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{}); err != nil {
		t.Fatalf("ExecuteAdopt: %v", err)
	}
	vault, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		t.Fatalf("OpenVault: %v", err)
	}
	got, err := vault.Get(wantVaultKey)
	if err != nil {
		t.Fatalf("vault.Get(%s): %v", wantVaultKey, err)
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

func TestAdoptEnvPlaceholderSurvivesManifestWithoutVaultWrite(t *testing.T) {
	entry := "mui-adopt-env-placeholder"
	_, manifestRoot, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-env-placeholder]
command = "go"
args = ["version"]

[mcp_servers.mui-adopt-env-placeholder.env]
API_KEY = "${env:API_KEY}"
`)
	t.Setenv("API_KEY", "expanded-value-must-not-appear")
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         port,
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if len(plan.SecretRoutedKeys) != 0 {
		t.Fatalf("SecretRoutedKeys = %#v, want none for ${env:} indirection", plan.SecretRoutedKeys)
	}
	if !strings.Contains(plan.ManifestYAML, `${env:API_KEY}`) {
		t.Fatalf("manifest lost ${env:API_KEY} placeholder:\n%s", plan.ManifestYAML)
	}
	if strings.Contains(plan.ManifestYAML, "expanded-value-must-not-appear") {
		t.Fatalf("manifest expanded ${env:API_KEY} at plan time:\n%s", plan.ManifestYAML)
	}

	if err := NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{}); err != nil {
		t.Fatalf("ExecuteAdopt: %v", err)
	}
	vault, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		t.Fatalf("OpenVault: %v", err)
	}
	if keys := vault.List(); len(keys) != 0 {
		t.Fatalf("vault keys = %v, want no adopt write for ${env:} indirection", keys)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(manifestRoot, entry, "manifest.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !strings.Contains(string(manifestBytes), `${env:API_KEY}`) {
		t.Fatalf("persisted manifest lost ${env:API_KEY} placeholder:\n%s", manifestBytes)
	}
	m, err := config.ParseManifest(bytes.NewReader(manifestBytes))
	if err != nil {
		t.Fatalf("ParseManifest: %v\n%s", err, manifestBytes)
	}
	if got := m.Env["API_KEY"]; got != `${env:API_KEY}` {
		t.Fatalf("parsed env API_KEY = %q, want ${env:API_KEY}", got)
	}
}

func TestAdoptSecretPrefixedForeignLiteralRoutesWhenVaultKeyMissing(t *testing.T) {
	entry := "mui-adopt-secret-prefix-literal"
	_, manifestRoot, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-secret-prefix-literal]
command = "go"
args = ["version"]

[mcp_servers.mui-adopt-secret-prefix-literal.env]
API_KEY = "secret:foreign-text"
`)
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         port,
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	wantKey := "MUI_ADOPT_SECRET_PREFIX_LITERAL_API_KEY"
	if !reflect.DeepEqual(plan.SecretRoutedKeys, []string{wantKey}) {
		t.Fatalf("SecretRoutedKeys = %#v, want [%s]", plan.SecretRoutedKeys, wantKey)
	}
	if !strings.Contains(plan.ManifestYAML, "secret:"+wantKey) {
		t.Fatalf("manifest did not route secret-prefixed foreign literal to namespaced ref:\n%s", plan.ManifestYAML)
	}
	if strings.Contains(plan.ManifestYAML, "secret:foreign-text") {
		t.Fatalf("manifest kept foreign secret-prefixed literal as hub ref:\n%s", plan.ManifestYAML)
	}
	if err := NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{}); err != nil {
		t.Fatalf("ExecuteAdopt: %v", err)
	}
	vault, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		t.Fatalf("OpenVault: %v", err)
	}
	got, err := vault.Get(wantKey)
	if err != nil {
		t.Fatalf("vault.Get(%s): %v", wantKey, err)
	}
	if got != "secret:foreign-text" {
		t.Fatalf("vault secret = %q, want original secret-prefixed literal", got)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(manifestRoot, entry, "manifest.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if strings.Contains(string(manifestBytes), "secret:foreign-text") {
		t.Fatalf("persisted manifest kept foreign secret-prefixed literal:\n%s", manifestBytes)
	}
}

func TestAdoptSecretPrefixedExistingVaultKeyRoutesAsForeignLiteral(t *testing.T) {
	entry := "mui-adopt-existing-secret-ref"
	_, _, _ = setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-existing-secret-ref]
command = "go"
args = ["version"]

[mcp_servers.mui-adopt-existing-secret-ref.env]
API_KEY = "secret:EXISTING_API_KEY"
`)
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	vault, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		t.Fatalf("OpenVault: %v", err)
	}
	if err := vault.Set("EXISTING_API_KEY", "hub-managed-value"); err != nil {
		t.Fatalf("seed existing hub ref: %v", err)
	}
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         port,
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	wantKey := "MUI_ADOPT_EXISTING_SECRET_REF_API_KEY"
	if !reflect.DeepEqual(plan.SecretRoutedKeys, []string{wantKey}) {
		t.Fatalf("SecretRoutedKeys = %#v, want [%s]", plan.SecretRoutedKeys, wantKey)
	}
	if !strings.Contains(plan.ManifestYAML, "secret:"+wantKey) {
		t.Fatalf("manifest did not route foreign secret-prefixed value to namespaced ref:\n%s", plan.ManifestYAML)
	}
	if strings.Contains(plan.ManifestYAML, "secret:EXISTING_API_KEY") {
		t.Fatalf("manifest kept existing hub secret ref from foreign config:\n%s", plan.ManifestYAML)
	}

	if err := NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{}); err != nil {
		t.Fatalf("ExecuteAdopt: %v", err)
	}
	vault, err = secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		t.Fatalf("OpenVault after adopt: %v", err)
	}
	got, err := vault.Get(wantKey)
	if err != nil {
		t.Fatalf("vault.Get(%s): %v", wantKey, err)
	}
	if got != "secret:EXISTING_API_KEY" {
		t.Fatalf("routed vault secret = %q, want original foreign literal", got)
	}
	existing, err := vault.Get("EXISTING_API_KEY")
	if err != nil {
		t.Fatalf("vault.Get(EXISTING_API_KEY): %v", err)
	}
	if existing != "hub-managed-value" {
		t.Fatalf("existing hub secret changed: got %q", existing)
	}
}

func TestAdoptSecretRoutingNamespacesVaultKeysByManifest(t *testing.T) {
	first := "mui-adopt-secret-one"
	second := "mui-adopt-secret-two"
	_, manifestRoot, _ := setupAdoptTestEnv(t, first, `[mcp_servers.mui-adopt-secret-one]
command = "go"
args = ["version"]

[mcp_servers.mui-adopt-secret-one.env]
API_KEY = "first-secret"

[mcp_servers.mui-adopt-secret-two]
command = "go"
args = ["env"]

[mcp_servers.mui-adopt-secret-two.env]
API_KEY = "second-secret"
`)
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(manifestRoot, second)) })
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}

	used := collectUsedAdoptPorts()
	firstPort := nextBindableAdoptPortForTest(t, used)
	used[firstPort] = true
	secondPort := nextBindableAdoptPortForTest(t, used)
	for _, tc := range []struct {
		entry  string
		port   int
		secret string
	}{
		{first, firstPort, "first-secret"},
		{second, secondPort, "second-secret"},
	} {
		plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
			EntryName:    tc.entry,
			Client:       "codex-cli",
			ManifestName: tc.entry,
			Port:         tc.port,
		})
		if err != nil {
			t.Fatalf("BuildAdoptPlan(%s): %v", tc.entry, err)
		}
		wantKey := strings.ToUpper(strings.ReplaceAll(tc.entry, "-", "_")) + "_API_KEY"
		if !reflect.DeepEqual(plan.SecretRoutedKeys, []string{wantKey}) {
			t.Fatalf("%s SecretRoutedKeys = %#v, want [%s]", tc.entry, plan.SecretRoutedKeys, wantKey)
		}
		if !strings.Contains(plan.ManifestYAML, "secret:"+wantKey) {
			t.Fatalf("%s manifest does not reference namespaced key %s:\n%s", tc.entry, wantKey, plan.ManifestYAML)
		}
		if err := NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{}); err != nil {
			t.Fatalf("ExecuteAdopt(%s): %v", tc.entry, err)
		}
	}

	vault, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		t.Fatalf("OpenVault: %v", err)
	}
	for _, tc := range []struct {
		key  string
		want string
	}{
		{"MUI_ADOPT_SECRET_ONE_API_KEY", "first-secret"},
		{"MUI_ADOPT_SECRET_TWO_API_KEY", "second-secret"},
	} {
		got, err := vault.Get(tc.key)
		if err != nil {
			t.Fatalf("vault.Get(%s): %v", tc.key, err)
		}
		if got != tc.want {
			t.Fatalf("vault[%s] = %q, want %q", tc.key, got, tc.want)
		}
	}
}

func TestExecuteAdoptRefusesExistingNamespacedVaultKeyBeforeManifestWrite(t *testing.T) {
	entry := "mui-adopt-secret-collision"
	_, manifestRoot, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-secret-collision]
command = "go"
args = ["version"]

[mcp_servers.mui-adopt-secret-collision.env]
API_KEY = "new-adopt-secret"
`)
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	vault, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		t.Fatalf("OpenVault: %v", err)
	}
	collisionKey := "MUI_ADOPT_SECRET_COLLISION_API_KEY"
	if err := vault.Set(collisionKey, "user-managed-secret"); err != nil {
		t.Fatalf("seed collision secret: %v", err)
	}
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         port,
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	err = NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{})
	if err == nil {
		t.Fatal("ExecuteAdopt overwrote an existing namespaced vault key")
	}
	if !strings.Contains(err.Error(), collisionKey) || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("collision error = %v, want key name + already exists", err)
	}
	if _, statErr := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("manifest written despite vault collision refusal: %v", statErr)
	}
	vault, err = secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		t.Fatalf("OpenVault after refusal: %v", err)
	}
	got, err := vault.Get(collisionKey)
	if err != nil {
		t.Fatalf("vault.Get(%s): %v", collisionKey, err)
	}
	if got != "user-managed-secret" {
		t.Fatalf("collision key overwritten: got %q", got)
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

func TestBuildAdoptPlanWithExplicitPortIgnoresExhaustedLegacyDraftRange(t *testing.T) {
	entry := "mui-adopt-legacy-exhausted"
	_, manifestRoot, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-legacy-exhausted]
command = "go"
args = ["version"]
`)
	seedLegacyDraftPortRangeExhaustedForAdoptTest(t, manifestRoot)
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         port,
		ScanOpts:     ScanOpts{ManifestDir: manifestRoot},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan with explicit adopt port must not require legacy 9121-9139 draft port: %v", err)
	}
	if plan.Port != port {
		t.Fatalf("plan.Port = %d, want %d", plan.Port, port)
	}
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

func TestExecuteAdoptVaultSetFailureDeletesEarlierRoutedKeys(t *testing.T) {
	entry := "mui-adopt-partial-vault"
	_, manifestRoot, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-partial-vault]
command = "go"
args = ["version"]

[mcp_servers.mui-adopt-partial-vault.env]
API_KEY = "first-secret"
API_TOKEN = "second-secret"
`)
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         port,
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if !reflect.DeepEqual(plan.SecretRoutedKeys, []string{"MUI_ADOPT_PARTIAL_VAULT_API_KEY", "MUI_ADOPT_PARTIAL_VAULT_API_TOKEN"}) {
		t.Fatalf("SecretRoutedKeys = %#v, want two routed keys", plan.SecretRoutedKeys)
	}

	vaultWriteCount := 0
	restoreWriter := secrets.SetVaultFileWriter(func(path string, data []byte, perm os.FileMode) error {
		if path == secrets.DefaultVaultPath() {
			vaultWriteCount++
			if vaultWriteCount == 2 {
				return fmt.Errorf("synthetic second vault write failure")
			}
		}
		return os.WriteFile(path, data, perm)
	})
	t.Cleanup(restoreWriter)

	err = NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{})
	if err == nil {
		t.Fatal("ExecuteAdopt succeeded despite second vault write failure")
	}
	if !strings.Contains(err.Error(), "synthetic second vault write failure") {
		t.Fatalf("ExecuteAdopt error = %v, want original set failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("manifest written despite vault set failure: %v", statErr)
	}
	vault, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		t.Fatalf("OpenVault after failed ExecuteAdopt: %v", err)
	}
	if keys := vault.List(); len(keys) != 0 {
		t.Fatalf("partial vault set failure left orphaned vault keys: %v", keys)
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

func TestBuildAdoptPlanRejectsExistingDiskManifestBeforeMutation(t *testing.T) {
	entry := "mui-adopt-disk-collision"
	codexPath, manifestRoot, stateRoot := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-disk-collision]
command = "go"
args = ["version"]

[mcp_servers.mui-adopt-disk-collision.env]
API_TOKEN = "literal-token"
`)
	if err := os.MkdirAll(filepath.Join(manifestRoot, entry), 0o700); err != nil {
		t.Fatalf("mkdir existing manifest dir: %v", err)
	}
	existingManifestPath := filepath.Join(manifestRoot, entry, "manifest.yaml")
	beforeManifest := []byte("name: " + entry + "\nkind: global\ntransport: stdio-bridge\ncommand: go\n")
	if err := os.WriteFile(existingManifestPath, beforeManifest, 0o600); err != nil {
		t.Fatalf("write existing manifest: %v", err)
	}
	before := mustReadFileForAdoptTest(t, codexPath)

	_, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         9317,
	})
	if err == nil {
		t.Fatal("BuildAdoptPlan accepted a name that already exists on disk")
	}
	if !strings.Contains(err.Error(), "already exists") || !strings.Contains(err.Error(), "adopt") {
		t.Fatalf("disk-manifest collision error = %v, want adopt refusal guidance", err)
	}
	after := mustReadFileForAdoptTest(t, codexPath)
	if !bytes.Equal(before, after) {
		t.Fatalf("codex config changed despite BuildAdoptPlan refusal\nbefore:\n%s\nafter:\n%s", before, after)
	}
	afterManifest := mustReadFileForAdoptTest(t, existingManifestPath)
	if !bytes.Equal(beforeManifest, afterManifest) {
		t.Fatalf("existing manifest changed despite BuildAdoptPlan refusal\nbefore:\n%s\nafter:\n%s", beforeManifest, afterManifest)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, supervisorIntentFileLeaf)); !os.IsNotExist(err) {
		t.Fatalf("intent side effect after BuildAdoptPlan refusal: %v", err)
	}
}

func TestExecuteAdoptManifestCreateFailureDeletesRoutedVaultKeys(t *testing.T) {
	entry := "mui-adopt-create-fail"
	_, _, _ = setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-create-fail]
command = "go"
args = ["version"]

[mcp_servers.mui-adopt-create-fail.env]
API_TOKEN = "literal-token"
`)
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         port,
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if !reflect.DeepEqual(plan.SecretRoutedKeys, []string{"MUI_ADOPT_CREATE_FAIL_API_TOKEN"}) {
		t.Fatalf("SecretRoutedKeys = %#v, want sanitized API_TOKEN key", plan.SecretRoutedKeys)
	}

	blockingFile := filepath.Join(t.TempDir(), "manifest-root-is-file")
	if err := os.WriteFile(blockingFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("seed blocking manifest root file: %v", err)
	}
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", blockingFile)

	err = NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{})
	if err == nil {
		t.Fatal("ExecuteAdopt succeeded despite manifest-create failure")
	}
	if !strings.Contains(err.Error(), "removed routed vault keys") {
		t.Fatalf("ExecuteAdopt error = %v, want routed-key cleanup note", err)
	}
	vault, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		t.Fatalf("OpenVault after failed ExecuteAdopt: %v", err)
	}
	if keys := vault.List(); len(keys) != 0 {
		t.Fatalf("manifest-create failure left orphaned vault keys: %v", keys)
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

func TestBuildAdoptPlanRejectsStrictInvalidEntryNameBeforeMutation(t *testing.T) {
	entry := "bad__name"
	codexPath, manifestRoot, stateRoot := setupAdoptTestEnv(t, entry, `[mcp_servers.bad__name]
command = "go"
args = ["version"]
`)
	before := mustReadFileForAdoptTest(t, codexPath)

	_, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry,
		Client:    "codex-cli",
		Port:      9311,
	})
	if err == nil {
		t.Fatal("BuildAdoptPlan accepted a strict-invalid '__' manifest name")
	}
	msg := err.Error()
	for _, want := range []string{"entry name", entry, "not a valid manifest name", "__", "valid --name is not supported in v1"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("BuildAdoptPlan error = %q, want substring %q", msg, want)
		}
	}
	assertAdoptPlanMutationFree(t, codexPath, before, manifestRoot, stateRoot, entry)
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

func TestPickNextFreeAdoptPortScrapesUnparseableManifestPortAndPool(t *testing.T) {
	manifestRoot := isolateAdoptPortAllocatorForTest(t)
	used := map[int]bool{}
	daemonPort := nextBindableAdoptPortForTest(t, used)
	used[daemonPort] = true
	poolPort := nextBindableAdoptPortForTest(t, used)
	used[poolPort] = true
	expected := nextBindableAdoptPortForTest(t, used)

	manifestName := "adopt-port-unparseable"
	if err := os.MkdirAll(filepath.Join(manifestRoot, manifestName), 0o700); err != nil {
		t.Fatalf("mkdir manifest: %v", err)
	}
	raw := fmt.Sprintf(`name: %s
kind: global
transport: stdio-bridge
command: go
base_args: ["${ADOPT_PORT_UNSET_FOR_TEST}/tool"]
port_pool: {start: %d, end: %d}
daemons:
  - name: default
    port: %d
`, manifestName, poolPort, poolPort, daemonPort)
	if err := os.WriteFile(filepath.Join(manifestRoot, manifestName, "manifest.yaml"), []byte(raw), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	got, err := pickNextFreeAdoptPort()
	if err != nil {
		t.Fatalf("pickNextFreeAdoptPort: %v", err)
	}
	if got != expected {
		t.Fatalf("pickNextFreeAdoptPort = %d, want %d (unparseable manifest port=%d pool=%d)", got, expected, daemonPort, poolPort)
	}
}

func TestPickNextFreeAdoptPortScrapesFlowPortAndReversedInlinePool(t *testing.T) {
	manifestRoot := isolateAdoptPortAllocatorForTest(t)
	used := map[int]bool{}
	daemonPort := nextBindableAdoptPortForTest(t, used)
	used[daemonPort] = true
	poolPort := nextBindableAdoptPortForTest(t, used)
	used[poolPort] = true
	expected := nextBindableAdoptPortForTest(t, used)

	manifestName := "adopt-port-flow"
	if err := os.MkdirAll(filepath.Join(manifestRoot, manifestName), 0o700); err != nil {
		t.Fatalf("mkdir manifest: %v", err)
	}
	raw := fmt.Sprintf(`name: %s
kind: global
transport: stdio-bridge
command: go
future_unknown_field: true
daemons: [{name: default, port: %d}]
port_pool: {end: %d, start: %d}
`, manifestName, daemonPort, poolPort, poolPort)
	if err := os.WriteFile(filepath.Join(manifestRoot, manifestName, "manifest.yaml"), []byte(raw), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	got, err := pickNextFreeAdoptPort()
	if err != nil {
		t.Fatalf("pickNextFreeAdoptPort: %v", err)
	}
	if got != expected {
		t.Fatalf("pickNextFreeAdoptPort = %d, want %d (flow daemon port=%d reversed pool port=%d)", got, expected, daemonPort, poolPort)
	}
}

func TestPickNextFreeAdoptPortSkipsConfiguredGUIPort(t *testing.T) {
	isolateAdoptPortAllocatorForTest(t)
	used := map[int]bool{}
	guiPort := nextBindableAdoptPortForTest(t, used)
	used[guiPort] = true
	expected := nextBindableAdoptPortForTest(t, used)
	if err := NewAPI().SettingsSet("gui_server.port", strconv.Itoa(guiPort)); err != nil {
		t.Fatalf("SettingsSet(gui_server.port): %v", err)
	}

	got, err := pickNextFreeAdoptPort()
	if err != nil {
		t.Fatalf("pickNextFreeAdoptPort: %v", err)
	}
	if got != expected {
		t.Fatalf("pickNextFreeAdoptPort = %d, want %d (configured GUI port %d must be reserved)", got, expected, guiPort)
	}
}

type ioDiscardForAdoptTest struct{}

func (ioDiscardForAdoptTest) Write(p []byte) (int, error) { return len(p), nil }

// failClientConfigWritesForAdoptTest fails ONLY the FIRST live-config write to
// failPath — the AddEntry write — and lets every subsequent write to it (the
// rollback restore-from-backup write) pass through. This models a transient
// AddEntry failure whose compensating restore SUCCEEDS, so Install's rollback
// completes cleanly and returns a PLAIN error → adopt takes the ABORT path these
// callers assert (manifest removed / no provenance residue).
//
// Failing EVERY write to failPath would, post-fix (restore closure registered
// before AddEntry — bug 2026-07-12), make the client's restore ALSO fail, feeding
// the InstallClientRollbackIncompleteError sentinel → adopt PRESERVES. That
// persistent-write-fault → preserve path is exercised deliberately by the
// per-path seam in adopt_abort_preserve_provenance_test.go; here we want the abort
// path, which requires the restore to succeed.
func failClientConfigWritesForAdoptTest(t *testing.T, failPath string) {
	t.Helper()
	cleanFailPath := filepath.Clean(failPath)
	realWrite := func(path string, contents []byte) error {
		if dir := filepath.Dir(path); dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
		}
		return os.WriteFile(path, contents, 0o600)
	}
	var mu sync.Mutex
	firstWriteFailed := false
	orig := clients.WriteConfigFile
	clients.WriteConfigFile = func(path string, contents []byte) error {
		if filepath.Clean(path) == cleanFailPath {
			mu.Lock()
			isFirst := !firstWriteFailed
			firstWriteFailed = true
			mu.Unlock()
			if isFirst {
				return fmt.Errorf("induced client config write failure for %s", filepath.Base(path))
			}
		}
		return realWrite(path, contents)
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

func isolateAdoptPortAllocatorForTest(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "localappdata"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "xdg-data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg-state"))
	// Same non-home adapter set setupAdoptTestEnv neutralizes — these two
	// sites are inline copies of that block and leaked identically.
	neutralizeClientConfigPathEnv(t, home)
	t.Cleanup(SetDaemonStateRootForTest(filepath.Join(root, "state")))
	manifestRoot := filepath.Join(root, "servers")
	if err := os.MkdirAll(manifestRoot, 0o700); err != nil {
		t.Fatalf("mkdir manifest root: %v", err)
	}
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestRoot)
	prevManifestDir := adoptManifestDirFn
	adoptManifestDirFn = func() string { return manifestRoot }
	t.Cleanup(func() { adoptManifestDirFn = prevManifestDir })
	prevEmbeddedCollector := collectEmbeddedManifestPortsFn
	collectEmbeddedManifestPortsFn = func(map[int]bool) {}
	t.Cleanup(func() { collectEmbeddedManifestPortsFn = prevEmbeddedCollector })
	return manifestRoot
}

func seedLegacyDraftPortRangeExhaustedForAdoptTest(t *testing.T, manifestRoot string) {
	t.Helper()
	for p := 9121; p <= 9139; p++ {
		name := fmt.Sprintf("legacy-port-%d", p)
		dir := filepath.Join(manifestRoot, name)
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir legacy manifest %d: %v", p, err)
		}
		body := fmt.Sprintf("name: %s\nkind: global\ntransport: stdio-bridge\ncommand: go\ndaemons:\n  - name: default\n    port: %d\n", name, p)
		if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(body), 0o600); err != nil {
			t.Fatalf("write legacy manifest %d: %v", p, err)
		}
	}
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
	// Same non-home adapter set setupAdoptTestEnv neutralizes — these two
	// sites are inline copies of that block and leaked identically.
	neutralizeClientConfigPathEnv(t, home)
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

func TestBuildAdoptPlanDefaultClientsExcludeMismatchedSameNameEntry(t *testing.T) {
	entry := "mui-adopt-signature"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-signature]
command = "go"
args = ["version"]

[mcp_servers.mui-adopt-signature.env]
SHARED = "1"
`)
	home := filepath.Dir(filepath.Dir(codexPath))
	claudePath := filepath.Join(home, ".claude.json")
	writeJSONForAdoptTest(t, claudePath, map[string]any{
		"mcpServers": map[string]any{
			entry: map[string]any{
				"command": "node",
				"args":    []any{"server.js"},
				"env":     map[string]any{"SHARED": "1"},
			},
		},
	})
	cursorPath := filepath.Join(home, ".cursor", "mcp.json")
	writeJSONForAdoptTest(t, cursorPath, map[string]any{
		"mcpServers": map[string]any{
			entry: map[string]any{
				"command": "go",
				"args":    []any{"version"},
				"env":     map[string]any{"SHARED": "1"},
			},
		},
	})
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         port,
		ScanOpts: ScanOpts{
			CodexConfigPath:  codexPath,
			ClaudeConfigPath: claudePath,
			CursorConfigPath: cursorPath,
		},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if containsAdoptString(plan.AdoptClients, "claude-code") {
		t.Fatalf("mismatched claude-code entry auto-selected: %#v", plan.AdoptClients)
	}
	if !containsAdoptString(plan.AdoptClients, "cursor") {
		t.Fatalf("matching cursor entry not auto-selected: %#v", plan.AdoptClients)
	}
	var out bytes.Buffer
	PrintAdoptPlan(&out, plan)
	if !strings.Contains(out.String(), entry+" in claude-code differs (command/args)") ||
		!strings.Contains(out.String(), "not adopted") {
		t.Fatalf("dry-run did not report mismatched same-name entry:\n%s", out.String())
	}
}

// TestAdoptExtractionErrorClass pins the leak-safe classification (bug 2026-07-08
// adopt Area-3): only a present-but-unreadable config is "corrupted", and the
// returned reason is ALWAYS path-free — no err.Error() / filesystem path can reach
// the /api wire (the fail-closed path-redaction posture of PR #516).
func TestAdoptExtractionErrorClass(t *testing.T) {
	secretPath := `C:\Users\alice\.cursor\mcp.json`
	cases := []struct {
		name      string
		err       error
		corrupted bool
		reasonHas string // if set, the reason must contain this (reason-class check)
	}{
		{"nil", nil, false, ""},
		{"absent file", &os.PathError{Op: "open", Path: secretPath, Err: os.ErrNotExist}, false, ""},
		{"entry not present (sentinel)", fmt.Errorf("server %q not found in client %q config: %w", "foo", "cursor", ErrClientEntryNotPresent), false, ""},
		{"permission denied", &os.PathError{Op: "open", Path: secretPath, Err: os.ErrPermission}, true, "permission denied"},
		{"parse error", fmt.Errorf("invalid character '}' looking for beginning of value"), true, "parsed"},
		{"http-only/hub-managed (sentinel)", fmt.Errorf("server has no command field: %w", ErrClientEntryNotStdio), true, "hub-managed"},
		{"relay-managed (sentinel)", fmt.Errorf("entry is a mcphub-managed relay stdio: %w", ErrClientEntryHubRelay), true, "relay"},
		{"path-unset (no sentinel) fails closed", fmt.Errorf("CursorConfigPath empty"), true, ""},
		// ADVERSARIAL (codex D2/D3 P1): classification is by TYPED SENTINEL, never by
		// substring on err.Error(). A read/parse failure whose PATH contains a
		// classifier phrase must NOT be fooled into a not-corrupted verdict —
		// reopening this re-enables the silent partial apply.
		{"permission denied at path containing 'not found in client'", &os.PathError{Op: "open", Path: `C:\Users\alice\not found in client\mcp.json`, Err: os.ErrPermission}, true, "permission denied"},
		{"other read failure at path containing 'ConfigPath empty'", &os.PathError{Op: "open", Path: `C:\Users\alice\ConfigPath empty\mcp.json`, Err: os.ErrInvalid}, true, ""},
		// codex D3: a MiMoCode-style parse error WRAPS the layer path (not a
		// *PathError, no sentinel); an adversarial path must still fail closed.
		{"mimocode parse error at path containing 'not found in client'", fmt.Errorf(`parse %s: %w`, `C:\Users\alice\not found in client\mimocode.jsonc`, fmt.Errorf("unexpected end of JSON")), true, ""},
	}
	for _, tc := range cases {
		reason, corrupted := adoptExtractionErrorClass(tc.err)
		if corrupted != tc.corrupted {
			t.Errorf("%s: corrupted=%v, want %v (reason=%q)", tc.name, corrupted, tc.corrupted, reason)
		}
		if strings.Contains(reason, secretPath) || strings.Contains(reason, "alice") {
			t.Errorf("%s: reason leaked a filesystem path: %q", tc.name, reason)
		}
		if tc.reasonHas != "" && !strings.Contains(reason, tc.reasonHas) {
			t.Errorf("%s: reason %q must contain %q (wrong reason class)", tc.name, reason, tc.reasonHas)
		}
	}
}

// TestBuildAdoptPlanFailsLoudOnRequestedUnreadableClient: an EXPLICITLY-requested
// client whose config is present-but-unreadable makes BuildAdoptPlan ERROR before
// any mutation (fail-loud, path-free) — no silent partial adopt (bug Area-3; the
// pre-fix behavior let it survive to AddEntry and roll back the whole adopt).
func TestBuildAdoptPlanFailsLoudOnRequestedUnreadableClient(t *testing.T) {
	entry := "mui-adopt-errored"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-errored]
command = "go"
args = ["version"]
`)
	home := filepath.Dir(filepath.Dir(codexPath))
	cursorPath := filepath.Join(home, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(cursorPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cursorPath, []byte(`{"mcpServers": {`), 0o600); err != nil { // malformed JSON
		t.Fatal(err)
	}
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	_, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port,
		Clients:  []string{"codex-cli", "cursor"},
		ScanOpts: ScanOpts{CodexConfigPath: codexPath, CursorConfigPath: cursorPath},
	})
	if err == nil {
		t.Fatal("expected a fail-loud error for a requested unreadable client; got nil")
	}
	if !strings.Contains(err.Error(), "cursor") || !strings.Contains(strings.ToLower(err.Error()), "adopt") {
		t.Errorf("error must name the client + refuse to adopt: %v", err)
	}
	if strings.Contains(err.Error(), cursorPath) {
		t.Errorf("error leaked the filesystem path (must be path-free): %v", err)
	}
}

// TestBuildAdoptPlanDefaultModeIgnoresUnrequestedUnreadableClient: a corrupted
// client that is NOT explicitly requested must not block a default-mode adopt.
func TestBuildAdoptPlanDefaultModeIgnoresUnrequestedUnreadableClient(t *testing.T) {
	entry := "mui-adopt-errored-default"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-errored-default]
command = "go"
args = ["version"]
`)
	home := filepath.Dir(filepath.Dir(codexPath))
	cursorPath := filepath.Join(home, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(cursorPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cursorPath, []byte(`{"mcpServers": {`), 0o600); err != nil {
		t.Fatal(err)
	}
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port,
		// no --clients (default mode): cursor's broken config must not block adopt.
		ScanOpts: ScanOpts{CodexConfigPath: codexPath, CursorConfigPath: cursorPath},
	})
	if err != nil {
		t.Fatalf("default-mode adopt must not fail on an unrequested broken client: %v", err)
	}
	if containsAdoptString(plan.AdoptClients, "cursor") {
		t.Errorf("broken cursor must not be adopted: %#v", plan.AdoptClients)
	}
	if !containsAdoptString(plan.AdoptClients, "codex-cli") {
		t.Errorf("source codex-cli must be adopted: %#v", plan.AdoptClients)
	}
}

// TestBuildAdoptPlanPreservesFanoutToEntrylessClient: explicitly requesting a
// client with a VALID config that simply lacks the entry is NOT an error — the hub
// entry is fanned out to it. Only a CORRUPTED config blocks (Area-3 scope: the fix
// must not narrow this previously-working fan-out).
func TestBuildAdoptPlanPreservesFanoutToEntrylessClient(t *testing.T) {
	entry := "mui-adopt-fanout"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-fanout]
command = "go"
args = ["version"]
`)
	home := filepath.Dir(filepath.Dir(codexPath))
	cursorPath := filepath.Join(home, ".cursor", "mcp.json")
	writeJSONForAdoptTest(t, cursorPath, map[string]any{
		"mcpServers": map[string]any{"someOther": map[string]any{"command": "x"}},
	})
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName: entry, Client: "codex-cli", ManifestName: entry, Port: port,
		Clients:  []string{"codex-cli", "cursor"},
		ScanOpts: ScanOpts{CodexConfigPath: codexPath, CursorConfigPath: cursorPath},
	})
	if err != nil {
		t.Fatalf("fan-out to an entryless valid-config client must not error: %v", err)
	}
	if !containsAdoptString(plan.AdoptClients, "cursor") {
		t.Errorf("cursor (valid config, entry absent) must be fanned out to: %#v", plan.AdoptClients)
	}
}

func TestBuildAdoptPlanDefaultClientsCompareEnvValuesAndRedactMismatchReport(t *testing.T) {
	entry := "mui-adopt-env-signature"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-env-signature]
command = "go"
args = ["version"]

[mcp_servers.mui-adopt-env-signature.env]
API_KEY = "staging-secret"
`)
	home := filepath.Dir(filepath.Dir(codexPath))
	claudePath := filepath.Join(home, ".claude.json")
	writeJSONForAdoptTest(t, claudePath, map[string]any{
		"mcpServers": map[string]any{
			entry: map[string]any{
				"command": "go",
				"args":    []any{"version"},
				"env":     map[string]any{"API_KEY": "staging-secret"},
			},
		},
	})
	cursorPath := filepath.Join(home, ".cursor", "mcp.json")
	writeJSONForAdoptTest(t, cursorPath, map[string]any{
		"mcpServers": map[string]any{
			entry: map[string]any{
				"command": "go",
				"args":    []any{"version"},
				"env":     map[string]any{"API_KEY": "prod-secret"},
			},
		},
	})
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         port,
		ScanOpts: ScanOpts{
			CodexConfigPath:  codexPath,
			ClaudeConfigPath: claudePath,
			CursorConfigPath: cursorPath,
		},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if !containsAdoptString(plan.AdoptClients, "claude-code") {
		t.Fatalf("same env value claude-code entry not auto-selected: %#v", plan.AdoptClients)
	}
	if containsAdoptString(plan.AdoptClients, "cursor") {
		t.Fatalf("different env value cursor entry auto-selected: %#v", plan.AdoptClients)
	}
	var out bytes.Buffer
	PrintAdoptPlan(&out, plan)
	msg := out.String()
	if !strings.Contains(msg, "env values differ for keys: API_KEY") {
		t.Fatalf("dry-run did not report env-value mismatch by key name only:\n%s", msg)
	}
	for _, leaked := range []string{"staging-secret", "prod-secret"} {
		if strings.Contains(msg, leaked) {
			t.Fatalf("dry-run leaked env value %q in mismatch report:\n%s", leaked, msg)
		}
	}
}

func TestBuildAdoptPlanExplicitClientsReexcludeMismatchedSameNameEntry(t *testing.T) {
	entry := "mui-adopt-signature-explicit"
	codexPath, manifestRoot, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-signature-explicit]
command = "go"
args = ["version"]
`)
	home := filepath.Dir(filepath.Dir(codexPath))
	claudePath := filepath.Join(home, ".claude.json")
	writeJSONForAdoptTest(t, claudePath, map[string]any{
		"mcpServers": map[string]any{
			entry: map[string]any{
				"command": "node",
				"args":    []any{"server.js"},
			},
		},
	})
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         port,
		Clients:      []string{"codex-cli", "claude-code"},
		ScanOpts: ScanOpts{
			CodexConfigPath:  codexPath,
			ClaudeConfigPath: claudePath,
		},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if !reflect.DeepEqual(plan.AdoptClients, []string{"codex-cli"}) {
		t.Fatalf("AdoptClients = %#v, want source only after re-excluding mismatched claude-code", plan.AdoptClients)
	}
	if len(plan.SignatureMismatches) != 1 || plan.SignatureMismatches[0].Client != "claude-code" {
		t.Fatalf("SignatureMismatches = %#v, want claude-code surfaced", plan.SignatureMismatches)
	}
	if err := NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{}); err != nil {
		t.Fatalf("ExecuteAdopt: %v", err)
	}
	manifestBytes := mustReadFileForAdoptTest(t, filepath.Join(manifestRoot, entry, "manifest.yaml"))
	m, err := config.ParseManifest(bytes.NewReader(manifestBytes))
	if err != nil {
		t.Fatalf("ParseManifest: %v\n%s", err, manifestBytes)
	}
	if len(m.ClientBindings) != 1 || m.ClientBindings[0].Client != "codex-cli" {
		t.Fatalf("client_bindings = %#v, want only codex-cli", m.ClientBindings)
	}
	var claudeRoot map[string]any
	afterClaude := mustReadFileForAdoptTest(t, claudePath)
	if err := json.Unmarshal(afterClaude, &claudeRoot); err != nil {
		t.Fatalf("decode claude after ExecuteAdopt: %v\n%s", err, afterClaude)
	}
	claudeEntry := claudeRoot["mcpServers"].(map[string]any)[entry].(map[string]any)
	if claudeEntry["command"] != "node" {
		t.Fatalf("mismatched claude entry was repointed: %#v", claudeEntry)
	}
}

func TestAdoptSanitizesRoutedVaultKeyAndResolverAcceptsIt(t *testing.T) {
	entry := "mui-mcp"
	_, _, _ = setupAdoptTestEnv(t, entry, `[mcp_servers.mui-mcp]
command = "go"
args = ["version"]

[mcp_servers.mui-mcp.env]
API_KEY = "literal-secret-value"
`)
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         port,
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	wantKey := "MUI_MCP_API_KEY"
	if !reflect.DeepEqual(plan.SecretRoutedKeys, []string{wantKey}) {
		t.Fatalf("SecretRoutedKeys = %#v, want [%s]", plan.SecretRoutedKeys, wantKey)
	}
	if err := secrets.ValidateSettableKeyName(wantKey); err != nil {
		t.Fatalf("sanitized adopt key is not SecretsSet-compatible: %v", err)
	}
	if !strings.Contains(plan.ManifestYAML, "secret:"+wantKey) {
		t.Fatalf("manifest does not reference sanitized secret key:\n%s", plan.ManifestYAML)
	}
	if err := NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{}); err != nil {
		t.Fatalf("ExecuteAdopt: %v", err)
	}
	vault, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		t.Fatalf("OpenVault: %v", err)
	}
	got, err := secrets.NewResolver(vault, nil).Resolve("secret:" + wantKey)
	if err != nil {
		t.Fatalf("Resolve(secret:%s): %v", wantKey, err)
	}
	if got != "literal-secret-value" {
		t.Fatalf("resolved secret = %q, want literal-secret-value", got)
	}
}

func TestAdoptShellEnvReferenceBecomesRuntimeEnvPlaceholderWithoutVaultWrite(t *testing.T) {
	entry := "mui-adopt-shell-env"
	_, manifestRoot, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-shell-env]
command = "go"
args = ["version"]

[mcp_servers.mui-adopt-shell-env.env]
API_KEY = "$API_KEY"
`)
	t.Setenv("API_KEY", "runtime-secret-value")
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         port,
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if len(plan.SecretRoutedKeys) != 0 {
		t.Fatalf("SecretRoutedKeys = %#v, want none for shell env indirection", plan.SecretRoutedKeys)
	}
	if !strings.Contains(plan.ManifestYAML, `${env:API_KEY}`) {
		t.Fatalf("manifest did not normalize $API_KEY to runtime ${env:API_KEY} placeholder:\n%s", plan.ManifestYAML)
	}
	if strings.Contains(plan.ManifestYAML, "runtime-secret-value") {
		t.Fatalf("manifest leaked runtime env value:\n%s", plan.ManifestYAML)
	}
	if err := NewAPI().ExecuteAdopt(plan, ioDiscardForAdoptTest{}); err != nil {
		t.Fatalf("ExecuteAdopt: %v", err)
	}
	vault, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		t.Fatalf("OpenVault: %v", err)
	}
	if keys := vault.List(); len(keys) != 0 {
		t.Fatalf("vault keys = %v, want no adopt write for shell env indirection", keys)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(manifestRoot, entry, "manifest.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	m, err := config.ParseManifest(bytes.NewReader(manifestBytes))
	if err != nil {
		t.Fatalf("ParseManifest: %v\n%s", err, manifestBytes)
	}
	if got := m.Env["API_KEY"]; got != `${env:API_KEY}` {
		t.Fatalf("parsed env API_KEY = %q, want ${env:API_KEY}", got)
	}
	resolved, err := secrets.NewResolver(nil, nil).Resolve(m.Env["API_KEY"])
	if err != nil {
		t.Fatalf("Resolve runtime env placeholder: %v", err)
	}
	if resolved != "runtime-secret-value" {
		t.Fatalf("resolved runtime env value = %q, want runtime-secret-value", resolved)
	}
}

func TestBuildAdoptPlanDefaultClientsSkipDisabledSameNameEntryAndReport(t *testing.T) {
	entry := "mui-adopt-disabled-target"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-disabled-target]
command = "go"
args = ["version"]
`)
	home := filepath.Dir(filepath.Dir(codexPath))
	cursorPath := filepath.Join(home, ".cursor", "mcp.json")
	writeJSONForAdoptTest(t, cursorPath, map[string]any{
		"mcpServers": map[string]any{
			entry: map[string]any{
				"command":  "go",
				"args":     []any{"version"},
				"disabled": true,
			},
		},
	})
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         port,
		ScanOpts: ScanOpts{
			CodexConfigPath:  codexPath,
			CursorConfigPath: cursorPath,
		},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if containsAdoptString(plan.AdoptClients, "cursor") {
		t.Fatalf("disabled cursor entry auto-selected by default: %#v", plan.AdoptClients)
	}
	var out bytes.Buffer
	PrintAdoptPlan(&out, plan)
	if !strings.Contains(out.String(), entry+" in cursor is disabled") ||
		!strings.Contains(out.String(), "not adopted") {
		t.Fatalf("dry-run did not report disabled same-name entry:\n%s", out.String())
	}

	explicit, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         port,
		Clients:      []string{"codex-cli", "cursor"},
		ScanOpts: ScanOpts{
			CodexConfigPath:  codexPath,
			CursorConfigPath: cursorPath,
		},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan explicit --clients: %v", err)
	}
	if containsAdoptString(explicit.AdoptClients, "cursor") {
		t.Fatalf("explicit --clients included disabled cursor target: %#v", explicit.AdoptClients)
	}
	if len(explicit.DisabledSameName) != 1 || explicit.DisabledSameName[0].Client != "cursor" {
		t.Fatalf("explicit DisabledSameName = %#v, want cursor surfaced", explicit.DisabledSameName)
	}
}

func TestBuildAdoptPlanExplicitClientsAllCleanStayFrozenAndDoNotSweepInNewSameName(t *testing.T) {
	entry := "mui-adopt-explicit-frozen"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-explicit-frozen]
command = "go"
args = ["version"]
`)
	home := filepath.Dir(filepath.Dir(codexPath))
	claudePath := filepath.Join(home, ".claude.json")
	writeJSONForAdoptTest(t, claudePath, map[string]any{
		"mcpServers": map[string]any{
			entry: map[string]any{
				"command": "go",
				"args":    []any{"version"},
			},
		},
	})
	cursorPath := filepath.Join(home, ".cursor", "mcp.json")
	writeJSONForAdoptTest(t, cursorPath, map[string]any{
		"mcpServers": map[string]any{
			entry: map[string]any{
				"command": "go",
				"args":    []any{"version"},
			},
		},
	})
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())

	plan, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         port,
		Clients:      []string{"codex-cli", "claude-code"},
		ScanOpts: ScanOpts{
			CodexConfigPath:  codexPath,
			ClaudeConfigPath: claudePath,
			CursorConfigPath: cursorPath,
		},
	})
	if err != nil {
		t.Fatalf("BuildAdoptPlan: %v", err)
	}
	if !reflect.DeepEqual(plan.AdoptClients, []string{"codex-cli", "claude-code"}) {
		t.Fatalf("AdoptClients = %#v, want exactly the requested clean clients", plan.AdoptClients)
	}
	if !reflect.DeepEqual(plan.AlsoPresent, []string{"cursor"}) {
		t.Fatalf("AlsoPresent = %#v, want unrequested clean cursor only", plan.AlsoPresent)
	}
	if len(plan.SignatureMismatches) != 0 {
		t.Fatalf("SignatureMismatches = %#v, want none for all-clean requested set", plan.SignatureMismatches)
	}
	if len(plan.DisabledSameName) != 0 {
		t.Fatalf("DisabledSameName = %#v, want none for all-clean requested set", plan.DisabledSameName)
	}
}

func TestBuildAdoptPlanRefusesDisabledSourceEntry(t *testing.T) {
	entry := "mui-adopt-disabled-source"
	setupAdoptTestEnv(t, entry, `[mcp_servers.mui-adopt-disabled-source]
command = "go"
args = ["version"]
disabled = true
`)

	_, err := NewAPI().BuildAdoptPlan(AdoptOpts{
		EntryName:    entry,
		Client:       "codex-cli",
		ManifestName: entry,
		Port:         9318,
	})
	if err == nil {
		t.Fatal("BuildAdoptPlan accepted a disabled source entry")
	}
	if !strings.Contains(err.Error(), "disabled") || !strings.Contains(err.Error(), "enable it first") {
		t.Fatalf("disabled source error = %v, want enable-it-first guidance", err)
	}
}

func writeJSONForAdoptTest(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

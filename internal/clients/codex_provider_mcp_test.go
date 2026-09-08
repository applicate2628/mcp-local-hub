package clients

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCodexListProviderMCPEntriesNormalizesSortsAndCopies(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	writeProviderFixture(t, root, "quartz-tools@catalog-a", "quartz-tools", "catalog-a", "7.4.2", "orbit-reader", false, false)
	writeProviderFixture(t, root, "nebula-suite@catalog-b", "nebula-suite", "catalog-b", "3.1.5", "delta-lens", true, false)
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("[plugins.\"quartz-tools@catalog-a\".mcp_servers.\"orbit-reader\"]\nenabled = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withProviderInventory(t, providerInventoryJSON(`[
{"pluginId":"nebula-suite@catalog-b","name":"nebula-suite","marketplaceName":"catalog-b","version":"3.1.5","installed":true,"enabled":true,"source":{"source":"marketplace","id":"catalog-b"},"installPolicy":"user","authPolicy":"none"},
{"pluginId":"quartz-tools@catalog-a","name":"quartz-tools","marketplaceName":"catalog-a","version":"7.4.2","installed":true,"enabled":true,"source":{"source":"marketplace","id":"catalog-a"},"installPolicy":"user","authPolicy":"none"}
]`))
	entries, err := (&codexCLI{path: filepath.Join(root, "config.toml")}).ListProviderMCPEntries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].PluginRef != "nebula-suite@catalog-b" || entries[1].PluginRef != "quartz-tools@catalog-a" {
		t.Fatalf("sorted entries=%+v", entries)
	}
	quartz := entries[1]
	if quartz.ServerName != "orbit-reader" || quartz.Transport != ProviderMCPTransportStdio || quartz.Command != "./bin/launch" || quartz.ToolTimeoutSec != 41 || quartz.WorkingDir == nil || !filepath.IsAbs(*quartz.WorkingDir) || quartz.PolicyState != ProviderMCPPolicyNone || !quartz.Enabled {
		t.Fatalf("quartz normalized=%+v", quartz)
	}
	if len(quartz.EnvForwardLocal) != 1 || quartz.EnvForwardLocal[0] != "TOKEN_A" || quartz.Env["MODE"] != "read" || quartz.ReceiptFingerprint == "" || quartz.ActivationFingerprint == "" || quartz.PolicyFingerprint == "" {
		t.Fatalf("quartz metadata=%+v", quartz)
	}
	entries[1].Args[0] = "mutated"
	entries[1].Env["MODE"] = "mutated"
	again, err := (&codexCLI{path: filepath.Join(root, "config.toml")}).ListProviderMCPEntries(context.Background())
	if err != nil || again[1].Args[0] != "--stdio" || again[1].Env["MODE"] != "read" {
		t.Fatalf("defensive copy result=%+v err=%v", again, err)
	}
}

func TestCodexListProviderMCPEntriesSupportsLocalAndRemoteReceiptSchemas(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)

	localReceipt := filepath.Join(root, "local-receipts", "local-tools")
	writeProviderReceiptForSchemaTest(t, localReceipt, "local-tools", "0.2.0", `{"mcpServers":{"local-worker":{"command":"./bin/launch","args":["--stdio","--local"],"cwd":".","env":{"MODE":"local"},"env_vars":["LOCAL_TOKEN"],"tool_timeout_sec":17}}}`)

	remoteReceipt := filepath.Join(root, "plugins", "cache", "catalog-remote", "remote-tools", "4.0.1")
	writeProviderReceiptForSchemaTest(t, remoteReceipt, "remote-tools", "4.0.1", `{"mcp_servers":{"remote-worker":{"command":"./bin/launch","args":["--stdio","--remote"],"cwd":".","env":{"MODE":"remote"},"env_vars":[{"name":"LEGACY_LOCAL_TOKEN","source":"local"}],"tool_timeout_sec":23}}}`)

	withProviderInventory(t, providerInventoryJSON(fmt.Sprintf(`[
{"pluginId":"remote-tools@catalog-remote","name":"remote-tools","marketplaceName":"catalog-remote","version":"4.0.1","installed":true,"enabled":true,"source":{"source":"marketplace","id":"catalog-remote"},"installPolicy":"user","authPolicy":"none"},
{"pluginId":"local-tools@dev","name":"local-tools","marketplaceName":"dev","version":"0.2.0","installed":true,"enabled":true,"source":{"source":"local","path":%q},"installPolicy":"user","authPolicy":"none"}
]`, localReceipt)))

	entries, err := (&codexCLI{path: filepath.Join(root, "config.toml")}).ListProviderMCPEntries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].PluginRef != "local-tools@dev" || entries[1].PluginRef != "remote-tools@catalog-remote" {
		t.Fatalf("entries=%+v", entries)
	}
	if entries[0].WorkingDir == nil || *entries[0].WorkingDir != localReceipt || len(entries[0].EnvForwardLocal) != 1 || entries[0].EnvForwardLocal[0] != "LOCAL_TOKEN" {
		t.Fatalf("local entry=%+v", entries[0])
	}
	if entries[1].WorkingDir == nil || *entries[1].WorkingDir != remoteReceipt || len(entries[1].EnvForwardLocal) != 1 || entries[1].EnvForwardLocal[0] != "LEGACY_LOCAL_TOKEN" {
		t.Fatalf("remote entry=%+v", entries[1])
	}
}

func TestCodexListProviderMCPEntriesClassifiesDisabledHTTPAndPolicy(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	writeProviderFixture(t, root, "quartz-tools@catalog-a", "quartz-tools", "catalog-a", "7.4.2", "orbit-reader", false, true)
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("[plugins.\"quartz-tools@catalog-a\".mcp_servers.\"orbit-reader\"]\nenabled = false\nenabled_tools = [\"x\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withProviderInventory(t, providerInventoryJSON(`[{"pluginId":"quartz-tools@catalog-a","name":"quartz-tools","marketplaceName":"catalog-a","version":"7.4.2","installed":true,"enabled":true,"source":{"source":"marketplace","id":"catalog-a"},"installPolicy":"user","authPolicy":"none"}]`))
	entries, err := (&codexCLI{path: filepath.Join(root, "config.toml")}).ListProviderMCPEntries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Transport != ProviderMCPTransportHTTP || entries[0].Enabled || entries[0].PolicyState != ProviderMCPPolicyUnrepresentable {
		t.Fatalf("classified=%+v", entries)
	}
}

func TestCodexListProviderMCPEntriesCapturesPriorActivationAndDisabledFingerprint(t *testing.T) {
	for _, tc := range []struct {
		name        string
		config      string
		wantPresent bool
		wantEnabled bool
	}{
		{"absent enabled", "[plugins.\"quartz-tools@catalog-a\".mcp_servers.\"orbit-reader\"]\n", false, false},
		{"enabled true", "[plugins.\"quartz-tools@catalog-a\".mcp_servers.\"orbit-reader\"]\nenabled = true\n", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("CODEX_HOME", root)
			writeProviderFixture(t, root, "quartz-tools@catalog-a", "quartz-tools", "catalog-a", "7.4.2", "orbit-reader", false, false)
			configPath := filepath.Join(root, "config.toml")
			if err := os.WriteFile(configPath, []byte(tc.config), 0o600); err != nil {
				t.Fatal(err)
			}
			withProviderInventory(t, providerInventoryJSON(`[{"pluginId":"quartz-tools@catalog-a","name":"quartz-tools","marketplaceName":"catalog-a","version":"7.4.2","installed":true,"enabled":true,"source":{"source":"marketplace","id":"catalog-a"},"installPolicy":"user","authPolicy":"none"}]`))

			entries, err := (&codexCLI{path: configPath}).ListProviderMCPEntries(context.Background())
			if err != nil || len(entries) != 1 {
				t.Fatalf("entries=%+v err=%v", entries, err)
			}
			entry := entries[0]
			if entry.ActivationEnabledPresent != tc.wantPresent || entry.ActivationEnabled != tc.wantEnabled {
				t.Fatalf("prior activation present/enabled=%v/%v, want %v/%v", entry.ActivationEnabledPresent, entry.ActivationEnabled, tc.wantPresent, tc.wantEnabled)
			}
			config, err := codexProviderConfig(configPath)
			if err != nil {
				t.Fatal(err)
			}
			activation, _ := codexProviderServerConfig(config, entry.PluginRef, entry.ServerName)
			activation["enabled"] = false
			wantDisabled, err := providerActivationFingerprint(activation)
			if err != nil {
				t.Fatal(err)
			}
			if entry.DisabledActivationFingerprint != wantDisabled {
				t.Fatalf("disabled fingerprint=%q, want %q", entry.DisabledActivationFingerprint, wantDisabled)
			}
		})
	}
}

func TestCodexListProviderMCPEntriesFailsClosedOnInventoryAndReceiptErrors(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
		body  string
	}{
		{"malformed-inventory", func(t *testing.T) {}, "{"},
		{"missing-required-row", func(t *testing.T) {}, `[{"pluginId":"p","name":"n","installed":true}]`},
		{"receipt-path-escape", func(t *testing.T) {
			writeProviderFixture(t, root, "quartz-tools@catalog-a", "quartz-tools", "catalog-a", "7.4.2", "orbit-reader", false, false)
			plugin := filepath.Join(root, "plugins", "cache", "catalog-a", "quartz-tools", "7.4.2", ".codex-plugin", "plugin.json")
			if err := os.WriteFile(plugin, []byte(`{"name":"quartz-tools","version":"7.4.2","mcpServers":"../escape.json"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}, `[{"pluginId":"quartz-tools@catalog-a","name":"quartz-tools","marketplaceName":"catalog-a","version":"7.4.2","installed":true,"enabled":true,"source":{"source":"marketplace","id":"catalog-a"},"installPolicy":"user","authPolicy":"none"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)
			withProviderInventory(t, providerInventoryJSON(tc.body))
			_, err := (&codexCLI{path: filepath.Join(root, "config.toml")}).ListProviderMCPEntries(context.Background())
			if !errors.Is(err, errCodexProviderInventoryUnavailable) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestCodexListProviderMCPEntriesInventoryUnlockedConfigSnapshotLocked(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	writeProviderFixture(t, root, "quartz-tools@catalog-a", "quartz-tools", "catalog-a", "7.4.2", "orbit-reader", false, false)
	configPath := filepath.Join(root, "config.toml")
	if err := os.WriteFile(configPath, []byte("[plugins.\"quartz-tools@catalog-a\".mcp_servers.\"orbit-reader\"]\nenabled = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writerEntered := make(chan struct{})
	releaseWriter := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- withConfigLock(configPath, func() error {
			close(writerEntered)
			<-releaseWriter
			return nil
		})
	}()
	<-writerEntered
	runnerCalled := make(chan struct{})
	previous := codexProviderMCPInventoryRun
	codexProviderMCPInventoryRun = func(context.Context) ([]byte, error) {
		close(runnerCalled)
		return providerInventoryJSON(`[{"pluginId":"quartz-tools@catalog-a","name":"quartz-tools","marketplaceName":"catalog-a","version":"7.4.2","installed":true,"enabled":true,"source":{"source":"marketplace","id":"catalog-a"},"installPolicy":"user","authPolicy":"none"}]`), nil
	}
	t.Cleanup(func() { codexProviderMCPInventoryRun = previous })
	result := make(chan error, 1)
	go func() {
		_, err := (&codexCLI{path: configPath}).ListProviderMCPEntries(context.Background())
		result <- err
	}()
	select {
	case <-runnerCalled:
	case <-time.After(time.Second):
		t.Fatal("inventory did not run while writer held config lock")
	}
	completedEarly := false
	var earlyErr error
	select {
	case earlyErr = <-result:
		completedEarly = true
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseWriter)
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	if completedEarly {
		t.Fatalf("config snapshot completed while writer held lock: %v", earlyErr)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func withProviderInventory(t *testing.T, body []byte) {
	t.Helper()
	previous := codexProviderMCPInventoryRun
	codexProviderMCPInventoryRun = func(context.Context) ([]byte, error) { return append([]byte(nil), body...), nil }
	t.Cleanup(func() { codexProviderMCPInventoryRun = previous })
}

func providerInventoryJSON(body string) []byte {
	return []byte(`{"installed":` + body + `,"available":[]}`)
}

func TestResolveCodexProviderInventoryCommandNPMShim(t *testing.T) {
	root := filepath.Join(t.TempDir(), "npm tools")
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "@openai", "codex", "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(root, "codex.cmd")
	node := filepath.Join(root, "node.exe")
	script := filepath.Join(root, "node_modules", "@openai", "codex", "bin", "codex.js")
	if err := os.WriteFile(node, []byte("node"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("entry"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := "@ECHO off\r\nGOTO start\r\n:find_dp0\r\nSET dp0=%~dp0\r\nEXIT /b\r\n:start\r\nSETLOCAL\r\nCALL :find_dp0\r\nIF EXIST \"%dp0%\\node.exe\" SET _prog=\"%dp0%\\node.exe\"\r\n\"%_prog%\"  \"%dp0%\\node_modules\\@openai\\codex\\bin\\codex.js\" %*\r\n"
	if err := os.WriteFile(shim, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd, err := resolveCodexProviderInventoryCommand(shim)
	if err != nil {
		t.Fatalf("resolve npm shim: %v", err)
	}
	if cmd.Path != node || !reflect.DeepEqual(cmd.Args, []string{node, script, "plugin", "list", "--json"}) {
		t.Fatalf("command path=%q args=%q", cmd.Path, cmd.Args)
	}
}

func TestResolveCodexProviderInventoryCommandRefusesUnknownShim(t *testing.T) {
	root := t.TempDir()
	shim := filepath.Join(root, "codex.cmd")
	if err := os.WriteFile(shim, []byte("@echo off\r\ncmd /c arbitrary\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveCodexProviderInventoryCommand(shim); err == nil || !strings.Contains(err.Error(), "unsupported codex cmd shim") {
		t.Fatalf("unknown shim error=%v", err)
	}
}

func TestResolveCodexProviderInventoryCommandRejectsBatchAndAlternateScript(t *testing.T) {
	root := t.TempDir()
	bat := filepath.Join(root, "codex.bat")
	if err := os.WriteFile(bat, []byte("@echo off\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveCodexProviderInventoryCommand(bat); err == nil {
		t.Fatal("accepted .bat shim")
	}
	native := filepath.Join(root, "codex.exe")
	cmd, err := resolveCodexProviderInventoryCommand(native)
	if err != nil || !reflect.DeepEqual(cmd.Args, []string{native, "plugin", "list", "--json"}) {
		t.Fatalf("native command=%+v err=%v", cmd, err)
	}
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "other", "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "node.exe"), []byte("node"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "node_modules", "other", "bin", "entry.js"), []byte("entry"), 0o600); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(root, "codex.cmd")
	body := "@ECHO off\r\nSETLOCAL\r\nCALL :find_dp0\r\nIF EXIST \"%dp0%\\node.exe\" SET _prog=\"%dp0%\\node.exe\"\r\n\"%_prog%\" \"%dp0%\\node_modules\\other\\bin\\entry.js\" %*\r\n"
	if err := os.WriteFile(shim, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveCodexProviderInventoryCommand(shim); err == nil {
		t.Fatal("accepted alternate package script")
	}
}

func TestResolveCodexProviderInventoryCommandUsesNativeNodePathFallback(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "@openai", "codex", "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(root, "codex.cmd")
	script := filepath.Join(root, "node_modules", "@openai", "codex", "bin", "codex.js")
	fallback := filepath.Join(t.TempDir(), "node.com")
	if err := os.WriteFile(script, []byte("entry"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fallback, []byte("node"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := "@ECHO off\r\nSETLOCAL\r\nCALL :find_dp0\r\nIF EXIST \"%dp0%\\node.exe\" SET _prog=\"%dp0%\\node.exe\"\r\n\"%_prog%\" \"%dp0%\\node_modules\\@openai\\codex\\bin\\codex.js\" %*\r\n"
	if err := os.WriteFile(shim, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	lookup := func(name string) (string, error) {
		if name == "node" {
			return fallback, nil
		}
		return "", errors.New("unexpected lookup")
	}
	cmd, err := resolveCodexProviderInventoryCommandWithLookup(shim, lookup)
	if err != nil || cmd.Path != fallback || !reflect.DeepEqual(cmd.Args, []string{fallback, script, "plugin", "list", "--json"}) {
		t.Fatalf("fallback command=%+v err=%v", cmd, err)
	}
	bad := filepath.Join(t.TempDir(), "node.cmd")
	if err := os.WriteFile(bad, []byte("batch"), 0o600); err != nil {
		t.Fatal(err)
	}
	badLookup := func(name string) (string, error) {
		if name == "node" {
			return bad, nil
		}
		return "", errors.New("unexpected lookup")
	}
	if _, err := resolveCodexProviderInventoryCommandWithLookup(shim, badLookup); err == nil {
		t.Fatal("accepted batch node fallback")
	}
}

func TestResolveCodexProviderInventoryCommandRejectsMultipleMissingAndOversizedShim(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "@openai", "codex", "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "node.exe"), []byte("node"), 0o600); err != nil {
		t.Fatal(err)
	}
	exact := filepath.Join(root, "node_modules", "@openai", "codex", "bin", "codex.js")
	shim := filepath.Join(root, "codex.cmd")
	standard := func(extra string) string {
		return "@ECHO off\r\nSETLOCAL\r\nCALL :find_dp0\r\nIF EXIST \"%dp0%\\node.exe\" SET _prog=\"%dp0%\\node.exe\"\r\n\"%_prog%\" \"%dp0%\\node_modules\\@openai\\codex\\bin\\codex.js\" %*\r\n" + extra
	}
	if err := os.WriteFile(shim, []byte(standard("")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveCodexProviderInventoryCommand(shim); err == nil {
		t.Fatal("accepted missing exact script")
	}
	if err := os.WriteFile(exact, []byte("entry"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shim, []byte(standard("\"%_prog%\" \"%dp0%\\node_modules\\@openai\\codex\\bin\\codex.js\" %*\r\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveCodexProviderInventoryCommand(shim); err == nil {
		t.Fatal("accepted multiple script references")
	}
	if err := os.WriteFile(shim, []byte(strings.Repeat("x", (64<<10)+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveCodexProviderInventoryCommand(shim); err == nil {
		t.Fatal("accepted oversized shim")
	}
}

func writeProviderFixture(t *testing.T, root, pluginRef, name, marketplace, version, server string, wrapped, http bool) {
	t.Helper()
	receipt := filepath.Join(root, "plugins", "cache", marketplace, name, version)
	if err := os.MkdirAll(filepath.Join(receipt, ".codex-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(receipt, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(receipt, ".codex-plugin", "plugin.json"), []byte(`{"name":"`+name+`","version":"`+version+`","mcpServers":"./mcp.json"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := `{"command":"./bin/launch","args":["--stdio","--profile","blue"],"cwd":".","env":{"MODE":"read"},"env_vars":[{"name":"TOKEN_A","source":"local"}],"tool_timeout_sec":41}`
	if http {
		entry = `{"url":"http://127.0.0.1:9999/mcp","cwd":".","tool_timeout_sec":41}`
	}
	body := `{"` + server + `":` + entry + `}`
	if wrapped {
		body = `{"mcp_servers":` + body + `}`
	}
	if err := os.WriteFile(filepath.Join(receipt, "mcp.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeProviderReceiptForSchemaTest(t *testing.T, receipt, name, version, mcpBody string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(receipt, ".codex-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(receipt, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(receipt, ".codex-plugin", "plugin.json"), []byte(`{"name":"`+name+`","version":"`+version+`","mcpServers":"./mcp.json"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(receipt, "mcp.json"), []byte(mcpBody), 0o600); err != nil {
		t.Fatal(err)
	}
}

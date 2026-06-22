package clients

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestMimoCode_ManagedPrefsShadow_FailsLoud pins bot PR #420 finding 1: the macOS
// Managed Preferences (MDM) layer is read-only and "overrides everything"
// (config.ts:876-885). When it defines the server being installed, AddEntry must
// FAIL LOUD with ErrMimoCodeManagedShadowsServer rather than report a successful
// write to mimocode.json (MiMoCode would keep resolving the MDM entry). The
// managed reader is injected via the mimoCodeManagedPrefsReader func-var seam so
// the path is exercised on any OS without a real /Library plist (mirrors
// clients.go copyFileTornWindowHook).
func TestMimoCode_ManagedPrefsShadow_FailsLoud(t *testing.T) {
	t.Run("MDM defines the server then AddEntry fails loud", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		globalPath := filepath.Join(dir, "mimocode.json")
		o := &mimoCodeClient{path: globalPath}

		prev := mimoCodeManagedPrefsReader
		t.Cleanup(func() { mimoCodeManagedPrefsReader = prev })
		mimoCodeManagedPrefsReader = func(name string) (mimoCodeShadowSource, error) {
			if name == "serena" {
				return mimoCodeShadowSource{Kind: "managed", Label: "macOS Managed Preferences", PlistFile: "/Library/Managed Preferences/ai.opencode.managed.plist"}, nil
			}
			return mimoCodeShadowSource{}, nil
		}

		err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"})
		var managedErr *ErrMimoCodeManagedShadowsServer
		if !errors.As(err, &managedErr) {
			t.Fatalf("AddEntry over an MDM-defined server must return ErrMimoCodeManagedShadowsServer, got %v", err)
		}
		if managedErr.Server != "serena" || managedErr.WriteTarget != globalPath {
			t.Errorf("managed error fields wrong: %+v", managedErr)
		}
		// The write target must NOT have been written (no silent success).
		if _, statErr := os.Stat(globalPath); statErr == nil {
			t.Errorf("AddEntry must not write mimocode.json when MDM shadows the server")
		}
	})

	t.Run("MDM defines a different server then install proceeds", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		globalPath := filepath.Join(dir, "mimocode.json")
		o := &mimoCodeClient{path: globalPath}

		prev := mimoCodeManagedPrefsReader
		t.Cleanup(func() { mimoCodeManagedPrefsReader = prev })
		mimoCodeManagedPrefsReader = func(name string) (mimoCodeShadowSource, error) {
			return mimoCodeShadowSource{}, nil // no shadow for any name
		}
		if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
			t.Fatalf("AddEntry must succeed when MDM does not define the server: %v", err)
		}
		if e, _ := o.GetEntry("serena"); e == nil || e.URL != "http://localhost:9121/mcp" {
			t.Errorf("server must be installed to the write target: %+v", e)
		}
	})

	t.Run("managed reader error propagates not silently no-shadow", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		o := &mimoCodeClient{path: filepath.Join(dir, "mimocode.json")}
		prev := mimoCodeManagedPrefsReader
		t.Cleanup(func() { mimoCodeManagedPrefsReader = prev })
		boom := errors.New("plutil failed")
		mimoCodeManagedPrefsReader = func(string) (mimoCodeShadowSource, error) { return mimoCodeShadowSource{}, boom }
		if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://x/mcp"}); !errors.Is(err, boom) {
			t.Errorf("a managed-reader error must propagate, got %v", err)
		}
	})
}

// TestMimoCode_ManagedReader_NonDarwinNoop confirms the production reader is a
// no-op off darwin (managed.ts:47), so non-macOS hosts never raise a managed
// shadow.
func TestMimoCode_ManagedReader_NonDarwinNoop(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("non-darwin no-op assertion; on darwin the reader actually probes /Library")
	}
	src, err := mimoCodeReadManagedPrefs("serena")
	if err != nil {
		t.Fatalf("non-darwin managed reader must not error: %v", err)
	}
	if src.Kind != "" {
		t.Errorf("non-darwin managed reader must report no shadow, got %+v", src)
	}
}

// TestMimoCode_ConfigDirEqualsGlobal_NotAShadow pins bot PR #420 finding 2: when
// MIMOCODE_CONFIG_DIR resolves to the global config dir (directly, or via a
// redundant path spelling), the overlay shadow check reads the SAME mimocode.json
// o.path writes; editing o.path IS what takes effect, so a re-install must NOT be
// refused as a shadow.
func TestMimoCode_ConfigDirEqualsGlobal_NotAShadow(t *testing.T) {
	t.Run("MIMOCODE_CONFIG_DIR equals global dir then re-install succeeds", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		globalDir := t.TempDir()
		globalPath := filepath.Join(globalDir, "mimocode.json")
		// The hub already wrote serena to the write target.
		if err := os.WriteFile(globalPath, []byte(`{"mcp":{"serena":{"type":"remote","url":"http://old/mcp","enabled":true}}}`), 0600); err != nil {
			t.Fatal(err)
		}
		// Overlay dir == the global dir (the overlay mimocode.json IS the write target).
		o := &mimoCodeClient{path: globalPath, overlayDir: globalDir}
		if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://new/mcp"}); err != nil {
			t.Fatalf("re-install must succeed when MIMOCODE_CONFIG_DIR equals global dir, got %v", err)
		}
		if e, _ := o.GetEntry("serena"); e == nil || e.URL != "http://new/mcp" {
			t.Errorf("re-install must update the write-target entry: %+v", e)
		}
	})

	t.Run("MIMOCODE_CONFIG_DIR equals global dir via redundant spelling then not a shadow", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		globalDir := t.TempDir()
		globalPath := filepath.Join(globalDir, "mimocode.json")
		if err := os.WriteFile(globalPath, []byte(`{"mcp":{"serena":{"type":"remote","url":"http://old/mcp","enabled":true}}}`), 0600); err != nil {
			t.Fatal(err)
		}
		spelledDir := filepath.Join(globalDir, "sub", "..")
		o := &mimoCodeClient{path: globalPath, overlayDir: spelledDir}
		if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://new/mcp"}); err != nil {
			t.Fatalf("re-install must succeed when the overlay spelling resolves to the write-target dir, got %v", err)
		}
	})

	t.Run("a genuinely different overlay still shadows (no over-exemption)", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		globalDir := t.TempDir()
		globalPath := filepath.Join(globalDir, "mimocode.json")
		overlayDir := t.TempDir() // a DIFFERENT physical dir
		if err := os.WriteFile(filepath.Join(overlayDir, "mimocode.json"),
			[]byte(`{"mcp":{"serena":{"type":"remote","url":"http://overlay/mcp","enabled":true}}}`), 0600); err != nil {
			t.Fatal(err)
		}
		o := &mimoCodeClient{path: globalPath, overlayDir: overlayDir}
		err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://new/mcp"})
		var shadowErr *ErrMimoCodeOverlayShadowsServer
		if !errors.As(err, &shadowErr) {
			t.Fatalf("a genuine different-path overlay must still shadow, got %v", err)
		}
	})
}

// TestMimoCode_ClaudeImport pins bot PR #420 finding 3: ~/.claude.json mcpServers
// are imported into the effective mcp view (READ-ONLY), skip-if-name-exists,
// gated by MIMOCODE_DISABLE_CLAUDE_CODE_MCP.
func TestMimoCode_ClaudeImport(t *testing.T) {
	writeClaudeJSON := func(t *testing.T, home, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	newGlobalDir := func(t *testing.T, home string) string {
		t.Helper()
		globalDir := filepath.Join(home, ".config", "mimocode")
		if err := os.MkdirAll(globalDir, 0755); err != nil {
			t.Fatal(err)
		}
		return globalDir
	}

	t.Run("a Claude-imported server appears in the merged read", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		t.Setenv(MimoCodeDisableClaudeImportEnv, "") // re-enable the import for this test
		home := t.TempDir()
		globalDir := newGlobalDir(t, home)
		writeClaudeJSON(t, home, `{"mcpServers":{"ctx7":{"command":"npx","args":["-y","ctx7"]},"remote-srv":{"url":"http://r/mcp"}}}`)
		o := &mimoCodeClient{path: filepath.Join(globalDir, "mimocode.json"), claudeHome: home}

		merged, err := o.readMergedLayers()
		if err != nil {
			t.Fatalf("readMergedLayers: %v", err)
		}
		servers, _ := merged[mimoCodeMCPKey].(map[string]any)
		local, ok := servers["ctx7"].(map[string]any)
		if !ok {
			t.Fatalf("Claude-imported local server must appear in the merge: %+v", servers)
		}
		if local["type"] != "local" {
			t.Errorf("imported command entry must be type:local, got %v", local["type"])
		}
		cmd, _ := local["command"].([]any)
		if len(cmd) != 3 || cmd[0] != "npx" || cmd[1] != "-y" || cmd[2] != "ctx7" {
			t.Errorf("imported command must be [command, ...args]: %v", cmd)
		}
		remote, ok := servers["remote-srv"].(map[string]any)
		if !ok || remote["type"] != "remote" || remote["url"] != "http://r/mcp" {
			t.Errorf("imported url entry must be type:remote: %+v", remote)
		}
	})

	t.Run("an explicit mimo entry of the same name wins (skip-if-name-exists)", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		t.Setenv(MimoCodeDisableClaudeImportEnv, "") // re-enable the import for this test
		home := t.TempDir()
		globalDir := newGlobalDir(t, home)
		globalPath := filepath.Join(globalDir, "mimocode.json")
		// Explicit mimo entry "shared" (remote) in the write target.
		if err := os.WriteFile(globalPath, []byte(`{"mcp":{"shared":{"type":"remote","url":"http://explicit/mcp","enabled":true}}}`), 0600); err != nil {
			t.Fatal(err)
		}
		// Claude tries to import a same-name "shared" (local) which must be skipped.
		writeClaudeJSON(t, home, `{"mcpServers":{"shared":{"command":"claude-cmd"}}}`)
		o := &mimoCodeClient{path: globalPath, claudeHome: home}

		merged, err := o.readMergedLayers()
		if err != nil {
			t.Fatalf("readMergedLayers: %v", err)
		}
		servers, _ := merged[mimoCodeMCPKey].(map[string]any)
		shared, _ := servers["shared"].(map[string]any)
		if shared["type"] != "remote" || shared["url"] != "http://explicit/mcp" {
			t.Errorf("explicit mimo entry must win skip-if-name-exists, got %+v", shared)
		}
	})

	t.Run("MIMOCODE_DISABLE_CLAUDE_CODE_MCP suppresses the import", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		t.Setenv("MIMOCODE_DISABLE_CLAUDE_CODE_MCP", "1")
		home := t.TempDir()
		globalDir := newGlobalDir(t, home)
		writeClaudeJSON(t, home, `{"mcpServers":{"ctx7":{"command":"npx"}}}`)
		o := &mimoCodeClient{path: filepath.Join(globalDir, "mimocode.json"), claudeHome: home}
		merged, err := o.readMergedLayers()
		if err != nil {
			t.Fatalf("readMergedLayers: %v", err)
		}
		servers, _ := merged[mimoCodeMCPKey].(map[string]any)
		if _, ok := servers["ctx7"]; ok {
			t.Errorf("MIMOCODE_DISABLE_CLAUDE_CODE_MCP must suppress the import, got %+v", servers)
		}
	})

	t.Run("a same-name claude import does NOT shadow the hub write (skip-if-name-exists)", func(t *testing.T) {
		// The claude import is SKIP-IF-NAME-EXISTS: once the hub writes the name to
		// the write target, the import skips it (config.ts:694-698). So a
		// ~/.claude.json entry of the same name must NOT block the install — the hub
		// write wins the merge. (An earlier revision wrongly refused this as a shadow.)
		isolateMimoCodeEnv(t)
		t.Setenv(MimoCodeDisableClaudeImportEnv, "") // re-enable the import for this test
		home := t.TempDir()
		globalDir := newGlobalDir(t, home)
		writeClaudeJSON(t, home, `{"mcpServers":{"serena":{"url":"http://claude-serena/mcp"}}}`)
		o := &mimoCodeClient{path: filepath.Join(globalDir, "mimocode.json"), claudeHome: home}
		if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://new/mcp"}); err != nil {
			t.Fatalf("a claude same-name import must NOT block the install, got %v", err)
		}
		// After the hub write, the write-target entry wins the merge (claude skips it).
		if e, _ := o.GetEntry("serena"); e == nil || e.URL != "http://new/mcp" {
			t.Errorf("hub write must win over the claude import (skip-if-name-exists): %+v", e)
		}
	})

	t.Run("malformed claude json propagates a loud parse error", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		t.Setenv(MimoCodeDisableClaudeImportEnv, "") // re-enable the import for this test
		home := t.TempDir()
		globalDir := newGlobalDir(t, home)
		writeClaudeJSON(t, home, `{"mcpServers": { not json`)
		o := &mimoCodeClient{path: filepath.Join(globalDir, "mimocode.json"), claudeHome: home}
		if _, err := o.readMergedLayers(); err == nil {
			t.Errorf("a malformed claude json must propagate a parse error, got nil")
		}
	})

	t.Run("state-safe: a temp override path never imports claude json", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		// mimoCodeClientForScanPath with a NON-global-layer-name path then no claudeHome.
		c := mimoCodeClientForScanPath(filepath.Join(t.TempDir(), "explicit-override.json"))
		if c.claudeHome != "" {
			t.Errorf("an explicit/temp override must not capture claudeHome (state-safe), got %q", c.claudeHome)
		}
	})

	t.Run("state-safe BY DEFAULT: a global-layer-name temp path under isolation never reads real claude json", func(t *testing.T) {
		// The critical state-safety guarantee (architect claim 2/3): a scan path
		// whose basename IS a global layer name (mimocode.json) passes the layer
		// gate, but under isolateMimoCodeEnv the disable flag is set, so the scan
		// resolver must NOT resolve os.UserHomeDir() → claudeHome stays "".
		isolateMimoCodeEnv(t)
		c := mimoCodeClientForScanPath(filepath.Join(t.TempDir(), "mimocode.json"))
		if c.claudeHome != "" {
			t.Errorf("a global-layer temp path under isolation must NOT resolve claudeHome (no real ~/.claude.json read), got %q", c.claudeHome)
		}
		// And the merged read must contain no claude-imported servers.
		merged, err := c.readMergedLayers()
		if err != nil {
			t.Fatalf("readMergedLayers: %v", err)
		}
		if servers, ok := merged[mimoCodeMCPKey].(map[string]any); ok && len(servers) != 0 {
			t.Errorf("isolated scan must surface no claude-imported servers, got %+v", servers)
		}
	})

	t.Run("scan path imports a FIXTURE claude json when re-enabled with a temp home", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		home := mimoCodeEnableClaudeImportForTest(t)
		globalDir := newGlobalDir(t, home)
		writeClaudeJSON(t, home, `{"mcpServers":{"fixture-srv":{"url":"http://fixture/mcp"}}}`)
		// The scan resolver now resolves claudeHome=home (temp) → imports the fixture.
		merged, err := MimoCodeMergedConfig(filepath.Join(globalDir, "mimocode.json"))
		if err != nil {
			t.Fatalf("MimoCodeMergedConfig: %v", err)
		}
		servers, _ := merged[mimoCodeMCPKey].(map[string]any)
		if _, ok := servers["fixture-srv"]; !ok {
			t.Errorf("scan path must import the fixture ~/.claude.json server when re-enabled, got %+v", servers)
		}
	})

	t.Run("sse and unconvertible claude entries are skipped", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		t.Setenv(MimoCodeDisableClaudeImportEnv, "") // re-enable the import for this test
		home := t.TempDir()
		globalDir := newGlobalDir(t, home)
		writeClaudeJSON(t, home, `{"mcpServers":{"sse-srv":{"type":"sse","url":"http://sse/mcp"},"bad-cmd":{"command":123},"empty":{}}}`)
		o := &mimoCodeClient{path: filepath.Join(globalDir, "mimocode.json"), claudeHome: home}
		merged, err := o.readMergedLayers()
		if err != nil {
			t.Fatalf("readMergedLayers: %v", err)
		}
		servers, _ := merged[mimoCodeMCPKey].(map[string]any)
		for _, bad := range []string{"sse-srv", "bad-cmd", "empty"} {
			if _, ok := servers[bad]; ok {
				t.Errorf("unconvertible claude entry %q must be skipped, got %+v", bad, servers[bad])
			}
		}
	})
}

// TestMimoCode_MalformedInlineOnly_IsConfigError pins bot PR #420 finding 4: a
// present-but-unparseable MIMOCODE_CONFIG_CONTENT (no file layers) is an
// active-but-broken profile and must surface a config-error tri-state, NOT
// "absent".
func TestMimoCode_MalformedInlineOnly_IsConfigError(t *testing.T) {
	isolateMimoCodeEnv(t)
	// Use a known global layer name so the inline env is honored.
	globalDir := t.TempDir()
	globalPath := filepath.Join(globalDir, "mimocode.json")

	t.Run("malformed inline yields state error", func(t *testing.T) {
		t.Setenv("MIMOCODE_CONFIG_CONTENT", `{ "mcp": { broken`)
		state, err := MimoCodeInlineContentState(globalPath)
		if state != "error" {
			t.Errorf("malformed inline must yield state error, got %q", state)
		}
		if err == nil {
			t.Errorf("malformed inline must carry the parse error")
		}
		if MimoCodeHasInlineContent(globalPath) {
			t.Errorf("MimoCodeHasInlineContent must be false for malformed inline (only parseable promotes)")
		}
	})

	t.Run("parseable inline yields state ok", func(t *testing.T) {
		t.Setenv("MIMOCODE_CONFIG_CONTENT", `{"mcp":{"x":{"type":"remote","url":"http://x/mcp","enabled":true}}}`)
		state, err := MimoCodeInlineContentState(globalPath)
		if state != "ok" || err != nil {
			t.Errorf("parseable inline must yield (ok, nil), got (%q, %v)", state, err)
		}
		if !MimoCodeHasInlineContent(globalPath) {
			t.Errorf("MimoCodeHasInlineContent must be true for parseable inline")
		}
	})

	t.Run("no inline yields empty state", func(t *testing.T) {
		t.Setenv("MIMOCODE_CONFIG_CONTENT", "")
		state, err := MimoCodeInlineContentState(globalPath)
		if state != "" || err != nil {
			t.Errorf("no inline must yield (empty, nil), got (%q, %v)", state, err)
		}
	})

	t.Run("state-safe: a temp override path never reads inline content", func(t *testing.T) {
		t.Setenv("MIMOCODE_CONFIG_CONTENT", `{ broken`)
		state, err := MimoCodeInlineContentState(filepath.Join(t.TempDir(), "explicit-override.json"))
		if state != "" || err != nil {
			t.Errorf("an explicit/temp override must not read inline content (state-safe), got (%q, %v)", state, err)
		}
	})
}

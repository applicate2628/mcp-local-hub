package clients

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMimoCode_Exists_ClaudeImportOnly pins bot PR #420 finding 2 (r16):
// Exists() must return true for a CLAUDE-IMPORT-ONLY profile — one whose only
// active mimo MCP source is the ~/.claude.json mcpServers import (no mimo file
// layer, no inline content, no config dir). The scan path already promotes such
// a profile to "ok" via MimoCodeHasClaudeImport; without the Exists() branch the
// method falls through to the parent-dir stat → false → install/register/Apply
// gate on Exists() and SKIP mimo even though the imported servers are present.
func TestMimoCode_Exists_ClaudeImportOnly(t *testing.T) {
	isolateMimoCodeEnv(t)
	home := mimoCodeEnableClaudeImportForTest(t)
	// NO mimo config dir, NO file layer, NO inline content — the only source is
	// ~/.claude.json. The write target's parent dir does NOT exist.
	writeTarget := filepath.Join(home, ".config", "mimocode", "mimocode.json")
	claudeBody := `{"mcpServers":{"ctx7":{"url":"http://claude-ctx7/mcp"}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(claudeBody), 0600); err != nil {
		t.Fatal(err)
	}
	o := &mimoCodeClient{path: writeTarget, claudeHome: home}

	// Sanity: the parent-dir stat branch would be false (dir absent), and there
	// is no file/inline layer — so without the claude-import branch Exists()
	// would be false.
	if _, err := os.Stat(filepath.Dir(writeTarget)); err == nil {
		t.Fatalf("precondition: config dir %s must be absent", filepath.Dir(writeTarget))
	}
	if !o.Exists() {
		t.Fatal("a claude-import-only profile must report Exists()==true (finding 2)")
	}
}

// TestMimoCode_Exists_ClaudeImportDisabledFlag pins the state-safety gate: with
// MIMOCODE_DISABLE_CLAUDE_CODE_MCP set, the claude import contributes nothing, so
// a claude-import-only profile (no other layer, no dir) reports Exists()==false —
// the disable flag short-circuits the import exactly as the read path does.
func TestMimoCode_Exists_ClaudeImportDisabledFlag(t *testing.T) {
	isolateMimoCodeEnv(t) // sets MIMOCODE_DISABLE_CLAUDE_CODE_MCP=1
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	writeTarget := filepath.Join(home, ".config", "mimocode", "mimocode.json")
	claudeBody := `{"mcpServers":{"ctx7":{"url":"http://claude-ctx7/mcp"}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(claudeBody), 0600); err != nil {
		t.Fatal(err)
	}
	// claudeHome empty AND the disable flag set → no import.
	o := &mimoCodeClient{path: writeTarget}
	if o.Exists() {
		t.Fatal("with the claude import disabled and no other layer, Exists() must be false (state-safe)")
	}
}

// TestMimoCode_GetEntry_WriteTargetStub_NotMergedSynthesis pins bot PR #420
// finding 3: when a lower-layer (config.json) LOCAL server is overridden by a
// bare {enabled:false} stub IN the write target (mimocode.json), GetEntry's
// rollback Raw must be the write-target's OWN stub, NOT the merged synthesis
// (which carries the lower layer's `command`). Otherwise a rollback
// AddEntry(*prior) copies the full lower server UP into mimocode.json, shadowing
// the layer the hub never owned.
func TestMimoCode_GetEntry_WriteTargetStub_NotMergedSynthesis(t *testing.T) {
	isolateMimoCodeEnv(t)
	dir := t.TempDir()
	writeTarget := filepath.Join(dir, "mimocode.json")
	// Lower layer (config.json): a LOCAL server with a command array.
	writeMimoFile(t, filepath.Join(dir, "config.json"),
		`{"mcp":{"srv":{"type":"local","command":["lower-cmd","--flag"],"enabled":true}}}`)
	// Write target (mimocode.json): an enabled-only override stub for the SAME name.
	writeMimoFile(t, writeTarget, `{"mcp":{"srv":{"enabled":false}}}`)
	o := &mimoCodeClient{path: writeTarget}

	prior, err := o.GetEntry("srv")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if prior == nil {
		t.Fatal("GetEntry must return a non-nil entry (read membership)")
	}
	// The name IS in the write target → at/above → SourceBelowWriteTarget=false.
	if prior.SourceBelowWriteTarget {
		t.Errorf("a write-target-resident entry must carry SourceBelowWriteTarget=false, got %+v", prior)
	}
	// CRITICAL: Raw must be the write-target's OWN value {enabled:false}, NOT the
	// merged {type:local,command:[lower-cmd...],enabled:false}.
	if prior.Raw == nil {
		t.Fatal("Raw must carry the write-target stub for an enabled:false entry")
	}
	if _, hasCmd := prior.Raw["command"]; hasCmd {
		t.Errorf("BUG (finding 3): Raw carries the lower layer's `command` (merged synthesis), must be the write-target's own {enabled:false} stub: %+v", prior.Raw)
	}
	if _, hasType := prior.Raw["type"]; hasType {
		t.Errorf("BUG (finding 3): Raw carries the lower layer's `type` (merged synthesis): %+v", prior.Raw)
	}
	if en, ok := prior.Raw["enabled"].(bool); !ok || en {
		t.Errorf("Raw must preserve enabled:false from the write-target stub, got %+v", prior.Raw)
	}
	// Finding 5: the disabled entry must be flagged disabled.
	if !prior.Disabled {
		t.Errorf("an enabled:false entry must carry Disabled=true (finding 5), got %+v", prior)
	}

	// ROLLBACK simulation: AddEntry(*prior) writes Raw verbatim. Assert the
	// write target on disk ends up with only the {enabled:false} stub — no
	// `command` copied up — and config.json is untouched.
	if err := o.AddEntry(*prior); err != nil {
		t.Fatalf("rollback AddEntry: %v", err)
	}
	wtData, _ := os.ReadFile(writeTarget)
	wtM, _ := parseJSONCBytes(wtData)
	wtServers, _ := wtM[mimoCodeMCPKey].(map[string]any)
	srv, _ := wtServers["srv"].(map[string]any)
	if srv == nil {
		t.Fatalf("write target lost the srv entry after rollback: %s", wtData)
	}
	if _, hasCmd := srv["command"]; hasCmd {
		t.Errorf("COPY-UP BUG (finding 3): rollback copied the lower layer's `command` into mimocode.json: %s", wtData)
	}
	// config.json untouched.
	cfgData, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	cfgM, _ := parseJSONCBytes(cfgData)
	cfgServers, _ := cfgM[mimoCodeMCPKey].(map[string]any)
	cfgSrv, _ := cfgServers["srv"].(map[string]any)
	if cmd, _ := cfgSrv["command"].([]any); len(cmd) == 0 {
		t.Errorf("config.json lower layer was disturbed by rollback: %s", cfgData)
	}
}

// TestMimoCode_GetEntry_DisabledFlag_OnDisabledRemote pins finding 5 at the
// adapter level: a DISABLED remote entry (enabled:false) projects with
// Disabled=true so the gate consumer (api.GatedOnClients) can exclude it, while
// an ENABLED entry leaves Disabled=false.
func TestMimoCode_GetEntry_DisabledFlag_OnDisabledRemote(t *testing.T) {
	isolateMimoCodeEnv(t)

	t.Run("disabled remote -> Disabled=true", func(t *testing.T) {
		o := newMimoCodeForTest(t, `{"mcp":{"mcphub-hub":{"type":"remote","url":"http://127.0.0.1:9200/clients/mimocode/mcp","enabled":false}}}`)
		e, err := o.GetEntry("mcphub-hub")
		if err != nil || e == nil {
			t.Fatalf("GetEntry: e=%+v err=%v", e, err)
		}
		if !e.Disabled {
			t.Errorf("a disabled remote entry must carry Disabled=true, got %+v", e)
		}
	})

	t.Run("enabled remote -> Disabled=false", func(t *testing.T) {
		o := newMimoCodeForTest(t, `{"mcp":{"mcphub-hub":{"type":"remote","url":"http://127.0.0.1:9200/clients/mimocode/mcp","enabled":true}}}`)
		e, err := o.GetEntry("mcphub-hub")
		if err != nil || e == nil {
			t.Fatalf("GetEntry: e=%+v err=%v", e, err)
		}
		if e.Disabled {
			t.Errorf("an enabled remote entry must carry Disabled=false, got %+v", e)
		}
	})
}

// TestMimoCode_NonRegularActiveLayer pins bot PR #420 finding 4's clients-side
// classifier: a non-regular EXISTING active layer is reported; an all-regular /
// partly-absent layer set is not.
func TestMimoCode_NonRegularActiveLayer(t *testing.T) {
	t.Run("regular write target + absent siblings -> not a fault", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		writeMimoFile(t, filepath.Join(dir, "mimocode.json"), `{"mcp":{}}`)
		if bad, isBad := MimoCodeNonRegularActiveLayer(filepath.Join(dir, "mimocode.json")); isBad {
			t.Errorf("an all-regular/absent layer set must not be a fault, got bad=%q", bad)
		}
	})

	t.Run("a directory at an active layer -> fault", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		writeMimoFile(t, filepath.Join(dir, "mimocode.json"), `{"mcp":{}}`)
		// config.json is a DIRECTORY (active lower layer, non-regular).
		if err := os.Mkdir(filepath.Join(dir, "config.json"), 0o755); err != nil {
			t.Fatal(err)
		}
		bad, isBad := MimoCodeNonRegularActiveLayer(filepath.Join(dir, "mimocode.json"))
		if !isBad {
			t.Fatal("a directory at the config.json active layer must be reported as a fault (finding 4)")
		}
		if filepath.Base(bad) != "config.json" {
			t.Errorf("expected config.json reported as the bad layer, got %q", bad)
		}
	})
}

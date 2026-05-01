package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// withTempHome redirects SettingsPath to a tempdir for the test duration.
func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	t.Setenv("XDG_DATA_HOME", dir)
	return filepath.Join(dir, "mcp-local-hub", "gui-preferences.yaml")
}

func runCLI(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newSettingsCmdReal()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errb.String(), err
}

func TestCLI_List_GroupedBySection(t *testing.T) {
	withTempHome(t)
	out, _, err := runCLI(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"appearance:", "gui_server:", "daemons:", "backups:", "advanced:"} {
		if !strings.Contains(out, section) {
			t.Errorf("expected section %q in list output:\n%s", section, out)
		}
	}
}

func TestCLI_List_AnnotatesDeferred(t *testing.T) {
	withTempHome(t)
	out, _, err := runCLI(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[deferred") {
		t.Errorf("expected at least one [deferred] annotation:\n%s", out)
	}
	if !strings.Contains(out, "[restart required]") {
		t.Errorf("expected gui_server.port [restart required] annotation:\n%s", out)
	}
}

func TestCLI_List_PrintsCanonicalKeys_NotStripped(t *testing.T) {
	// Codex PR #20 r5 P2: list output must print canonical keys
	// (appearance.theme), not section-stripped (theme), so users can
	// copy directly into `mcp settings get/set`.
	withTempHome(t)
	out, _, err := runCLI(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "appearance.theme = ") {
		t.Errorf("expected canonical 'appearance.theme = ' in list output, got:\n%s", out)
	}
	if !strings.Contains(out, "gui_server.port = ") {
		t.Errorf("expected canonical 'gui_server.port = ' in list output, got:\n%s", out)
	}
	// The OLD (stripped) form must be absent. We use a tighter pattern
	// to avoid matching the section header line "appearance:" which
	// contains "appearance".
	// Old format had a 2-space indent + bare local name + " = "; e.g. "  theme = ".
	// Canonical now: "  appearance.theme = ". So a line starting with
	// "  theme = " would indicate regression.
	for _, badPrefix := range []string{"  theme =", "  density =", "  shell =", "  port =", "  keep_n ="} {
		if strings.Contains(out, badPrefix) {
			t.Errorf("unexpected stripped form %q in list output (Codex r5 P2 regression)", badPrefix)
		}
	}
}

func TestCLI_Get_UnknownKey_Exit1(t *testing.T) {
	withTempHome(t)
	_, _, err := runCLI(t, "get", "no.such.key")
	if err == nil || !strings.Contains(err.Error(), "unknown setting") {
		t.Fatalf("expected unknown-setting error, got %v", err)
	}
}

func TestCLI_Get_ActionKey_Exit1(t *testing.T) {
	withTempHome(t)
	_, _, err := runCLI(t, "get", "advanced.open_app_data_folder")
	if err == nil || !strings.Contains(err.Error(), "is an action") {
		t.Fatalf("expected is-action error, got %v", err)
	}
}

func TestCLI_Get_Deferred_PrintsValueAndStderrWarning(t *testing.T) {
	// gui_server.tray is the canonical Deferred:true TypeBool key that
	// Task 1 did NOT flip (PR #2 will flip it). daemons.weekly_schedule was
	// flipped to Deferred:false by Task 1 so it no longer emits the warning.
	withTempHome(t)
	stdout, stderr, err := runCLI(t, "get", "gui_server.tray")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout) == "" {
		t.Error("expected a value on stdout")
	}
	if !strings.Contains(stderr, "[deferred") {
		t.Errorf("expected stderr deferred warning, got %q", stderr)
	}
}

func TestCLI_Set_UnknownKey_Exit1(t *testing.T) {
	withTempHome(t)
	_, _, err := runCLI(t, "set", "no.such.key", "x")
	if err == nil || !strings.Contains(err.Error(), "unknown setting") {
		t.Fatalf("expected unknown-setting error, got %v", err)
	}
}

func TestCLI_Set_ActionKey_Exit1(t *testing.T) {
	withTempHome(t)
	_, _, err := runCLI(t, "set", "advanced.open_app_data_folder", "x")
	if err == nil || !strings.Contains(err.Error(), "cannot set action key") {
		t.Fatalf("expected cannot-set-action error, got %v", err)
	}
}

func TestCLI_Set_Validation_RejectsBadValue(t *testing.T) {
	withTempHome(t)
	_, _, err := runCLI(t, "set", "appearance.theme", "puce")
	if err == nil || !strings.Contains(err.Error(), "invalid value") {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestCLI_Set_DeferredNonAction_SucceedsWithStderrWarning(t *testing.T) {
	// gui_server.tray is the canonical Deferred:true TypeBool key that
	// Task 1 did NOT flip (PR #2 will flip it). daemons.retry_policy was
	// flipped to Deferred:false by Task 1 so it no longer emits the warning.
	withTempHome(t)
	_, stderr, err := runCLI(t, "set", "gui_server.tray", "false")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "deferred to A4-b") {
		t.Errorf("expected stderr deferred warning, got %q", stderr)
	}
	// Confirm value persisted.
	a := api.NewAPI()
	v, err := a.SettingsGet("gui_server.tray")
	if err != nil || v != "false" {
		t.Errorf("expected false persisted, got %q err=%v", v, err)
	}
}

// TestCLI_Get_LegacyKeyAlias verifies that pre-A4 unqualified names accepted
// by `mcp settings get theme` resolve to the canonical appearance.theme value.
// Codex PR #20 r13 P2: disk-side legacyKeyMap migrates YAML; this tests the
// mirror at the CLI lookup layer so existing scripts need no update.
func TestCLI_Get_LegacyKeyAlias(t *testing.T) {
	withTempHome(t)
	// Write via the canonical key so the value is definitely on disk.
	if _, _, err := runCLI(t, "set", "appearance.theme", "dark"); err != nil {
		t.Fatal(err)
	}
	// Read via the legacy alias — must succeed and return the written value.
	out, _, err := runCLI(t, "get", "theme")
	if err != nil {
		t.Fatalf("legacy alias 'theme' must resolve: %v", err)
	}
	if !strings.Contains(out, "dark") {
		t.Errorf("expected 'dark' from legacy 'theme' alias, got: %q", out)
	}
}

// TestCLI_Set_LegacyKeyAlias verifies that writing via a legacy alias lands
// at the canonical key, so a follow-up `mcp settings get appearance.shell`
// returns the value that was written as `mcp settings set shell bash`.
func TestCLI_Set_LegacyKeyAlias(t *testing.T) {
	withTempHome(t)
	if _, _, err := runCLI(t, "set", "shell", "bash"); err != nil {
		t.Fatalf("legacy alias 'shell' must resolve on set: %v", err)
	}
	// Confirm the write landed at the canonical key.
	out, _, err := runCLI(t, "get", "appearance.shell")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "bash") {
		t.Errorf("expected canonical write to carry 'bash', got: %q", out)
	}
}

// TestCLI_LegacyAlias_DoesNotShadowNonLegacyKey guards the pass-through
// path: a canonical key that is NOT a legacy alias must still round-trip
// normally through lookupRegistry (regression guard for ResolveLegacyKey).
func TestCLI_LegacyAlias_DoesNotShadowNonLegacyKey(t *testing.T) {
	withTempHome(t)
	if _, _, err := runCLI(t, "set", "gui_server.port", "9300"); err != nil {
		t.Fatal(err)
	}
	out, _, err := runCLI(t, "get", "gui_server.port")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "9300") {
		t.Errorf("non-legacy canonical key must round-trip unchanged, got: %q", out)
	}
}

// TestMain ensures any leftover env state from prior tests doesn't leak.
// The withTempHome helper sets envs per-test via t.Setenv, which is
// auto-restored. No extra setup needed.
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

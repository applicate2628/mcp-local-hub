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
	path := withTempHome(t)
	_ = os.MkdirAll(filepath.Dir(path), 0700)
	stdout, stderr, err := runCLI(t, "get", "daemons.weekly_schedule")
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
	withTempHome(t)
	_, stderr, err := runCLI(t, "set", "daemons.retry_policy", "linear")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "deferred to A4-b") {
		t.Errorf("expected stderr deferred warning, got %q", stderr)
	}
	// Confirm value persisted.
	a := api.NewAPI()
	v, err := a.SettingsGet("daemons.retry_policy")
	if err != nil || v != "linear" {
		t.Errorf("expected linear persisted, got %q err=%v", v, err)
	}
}

// TestMain ensures any leftover env state from prior tests doesn't leak.
// The withTempHome helper sets envs per-test via t.Setenv, which is
// auto-restored. No extra setup needed.
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

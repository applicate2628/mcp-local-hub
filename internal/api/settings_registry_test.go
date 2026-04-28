package api

import (
	"strconv"
	"testing"
)

func TestRegistry_AllSectionsCanonical(t *testing.T) {
	allowed := map[string]bool{
		"appearance": true, "gui_server": true, "daemons": true,
		"backups": true, "advanced": true,
	}
	for _, d := range SettingsRegistry {
		if !allowed[d.Section] {
			t.Fatalf("registry entry %q has unknown section %q", d.Key, d.Section)
		}
	}
}

func TestRegistry_NoDuplicateKeys(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range SettingsRegistry {
		if seen[d.Key] {
			t.Fatalf("duplicate registry key %q", d.Key)
		}
		seen[d.Key] = true
	}
}

func TestRegistry_DefaultsValidate(t *testing.T) {
	for _, d := range SettingsRegistry {
		if d.Type == TypeAction {
			if d.Default != "" {
				t.Errorf("action key %q must have empty Default, got %q", d.Key, d.Default)
			}
			continue
		}
		if err := validate(&d, d.Default); err != nil {
			t.Errorf("default for %q (%q) fails its own validator: %v", d.Key, d.Default, err)
		}
	}
}

func TestRegistry_EnumNonEmpty(t *testing.T) {
	for _, d := range SettingsRegistry {
		if d.Type == TypeEnum && len(d.Enum) == 0 {
			t.Errorf("enum entry %q has empty Enum", d.Key)
		}
	}
}

func TestRegistry_IntBoundsConsistent(t *testing.T) {
	for _, d := range SettingsRegistry {
		if d.Type != TypeInt {
			continue
		}
		n, _ := strconv.Atoi(d.Default)
		if d.Min != nil && n < *d.Min {
			t.Errorf("int %q default %d below Min %d", d.Key, n, *d.Min)
		}
		if d.Max != nil && n > *d.Max {
			t.Errorf("int %q default %d above Max %d", d.Key, n, *d.Max)
		}
	}
}

func TestValidate_Enum(t *testing.T) {
	def := findDef("appearance.theme")
	if err := validate(def, "puce"); err == nil {
		t.Fatal("expected enum validation to reject 'puce'")
	}
	if err := validate(def, "dark"); err != nil {
		t.Fatalf("expected 'dark' to validate, got %v", err)
	}
}

func TestValidate_Int_Bounds(t *testing.T) {
	def := findDef("gui_server.port")
	if err := validate(def, "99"); err == nil {
		t.Fatal("expected 99 (below 1024) to fail")
	}
	if err := validate(def, "70000"); err == nil {
		t.Fatal("expected 70000 (above 65535) to fail")
	}
	if err := validate(def, "9125"); err != nil {
		t.Fatalf("expected 9125 to validate, got %v", err)
	}
}

func TestValidate_Bool(t *testing.T) {
	def := findDef("gui_server.browser_on_launch")
	for _, ok := range []string{"true", "false"} {
		if err := validate(def, ok); err != nil {
			t.Errorf("bool: %q should validate, got %v", ok, err)
		}
	}
	for _, bad := range []string{"yes", "1", "True", ""} {
		if err := validate(def, bad); err == nil {
			t.Errorf("bool: %q should fail, got nil", bad)
		}
	}
}

func TestValidate_Path_OptionalEmpty(t *testing.T) {
	def := findDef("appearance.default_home")
	if !def.Optional {
		t.Fatal("appearance.default_home must be Optional=true (memo §4.1, Codex r1 P1.3)")
	}
	if err := validate(def, ""); err != nil {
		t.Errorf("Optional path empty value should validate, got %v", err)
	}
	if err := validate(def, " /tmp"); err == nil {
		t.Error("path with leading whitespace should fail")
	}
	if err := validate(def, "/ok/path"); err != nil {
		t.Errorf("normal path should validate, got %v", err)
	}
}

func TestValidate_Path_RejectsControlChars(t *testing.T) {
	// Codex PR #20 r14 P2: TypePath must reject embedded control chars
	// (newline, tab, etc.) — same guard as TypeString. Without this,
	// a path like "/tmp/foo\nbar" reaches the CLI output and downstream
	// launch-path consumers, breaking both.
	def := findDef("appearance.default_home")
	if def.Type != TypePath {
		t.Fatal("test setup: appearance.default_home must be TypePath")
	}
	cases := []struct {
		name  string
		value string
	}{
		{"newline in middle", "/tmp/foo\nbar"},
		{"tab in middle", "/tmp/foo\tbar"},
		{"carriage return", "/tmp/foo\rbar"},
		{"DEL (0x7F)", "/tmp/foo\x7Fbar"},
		{"vertical tab", "/tmp/foo\vbar"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validate(def, tc.value); err == nil {
				t.Errorf("%s: expected control-char rejection, got nil", tc.name)
			}
		})
	}
}

func TestValidate_Path_AcceptsValidNonEmpty(t *testing.T) {
	// Sanity: ensure the new control-char guard doesn't reject normal paths.
	def := findDef("appearance.default_home")
	cases := []string{
		"/home/user",
		"C:\\Users\\dima",
		"/tmp/foo bar with spaces in middle",
		"relative/path",
		"./dot-prefix",
		"../parent",
	}
	for _, p := range cases {
		if err := validate(def, p); err != nil {
			t.Errorf("normal path %q must validate, got %v", p, err)
		}
	}
}

func TestValidate_Action_AlwaysRejects(t *testing.T) {
	def := findDef("advanced.open_app_data_folder")
	if err := validate(def, "anything"); err == nil {
		t.Fatal("action keys must always reject set")
	}
}

func TestValidate_String_Pattern(t *testing.T) {
	def := findDef("daemons.weekly_schedule")
	if err := validate(def, "weekly Sun 03:00"); err != nil {
		t.Errorf("registry default should validate, got %v", err)
	}
	if err := validate(def, "garbage"); err == nil {
		t.Error("non-matching string should fail")
	}
}

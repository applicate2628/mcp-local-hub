package api

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// SettingType is the discriminator for SettingDef.Type. It controls
// validation behavior and (on the wire) the shape of the SettingDTO.
type SettingType string

const (
	TypeEnum   SettingType = "enum"
	TypeBool   SettingType = "bool"
	TypeInt    SettingType = "int"
	TypeString SettingType = "string"
	TypePath   SettingType = "path"
	TypeAction SettingType = "action"
)

// SettingDef is one entry in the authoritative settings schema. The
// persisted gui-preferences.yaml stores values as a flat map[string]string;
// the registry overlays meaning (type, default, validation, deferred
// flag) on top of that flat map. Memo §4.1.
type SettingDef struct {
	Key      string
	Section  string
	Type     SettingType
	Default  string
	Enum     []string
	Min      *int
	Max      *int
	Pattern  string
	Optional bool // for TypeString/TypePath: empty value allowed (memo §4.1, Codex r1 P1.3)
	Deferred bool
	Help     string
}

// intPtr returns &n. Used to keep registry literals compact for
// Min/Max int bounds.
func intPtr(n int) *int { return &n }

// SettingsRegistry is the canonical list of all known settings keys. Order
// matches §5.7 reading order: appearance, gui_server, daemons, backups,
// advanced. CLI list and GUI snapshot both render in this order.
var SettingsRegistry = []SettingDef{
	// ----- appearance -----
	{Key: "appearance.theme", Section: "appearance", Type: TypeEnum,
		Default: "system", Enum: []string{"light", "dark", "system"},
		Help: "Color theme. 'system' follows OS dark-mode."},
	{Key: "appearance.density", Section: "appearance", Type: TypeEnum,
		Default: "comfortable", Enum: []string{"compact", "comfortable", "spacious"},
		Help: "UI spacing density."},
	{Key: "appearance.shell", Section: "appearance", Type: TypeEnum,
		Default: "pwsh", Enum: []string{"pwsh", "cmd", "bash", "zsh", "git-bash"},
		Help: "Default shell for shell-out actions. Used by future launches."},
	{Key: "appearance.default_home", Section: "appearance", Type: TypePath,
		Default: "", Optional: true,
		Help: "Default home directory for new servers. Used by future launches."},

	// ----- gui_server -----
	{Key: "gui_server.browser_on_launch", Section: "gui_server", Type: TypeBool,
		Default: "true", Help: "Open GUI in browser on launch."},
	{Key: "gui_server.port", Section: "gui_server", Type: TypeInt,
		Default: "9125", Min: intPtr(1024), Max: intPtr(65535),
		Help: "GUI server port. Restart required to take effect."},
	{Key: "gui_server.tray", Section: "gui_server", Type: TypeBool,
		Default: "true", Deferred: true,
		Help: "Show tray icon (Windows). Edit coming in A4-b."},

	// ----- daemons -----
	{Key: "daemons.weekly_schedule", Section: "daemons", Type: TypeString,
		Default: "weekly Sun 03:00", Pattern: `^(daily|weekly)\s+\S+(\s+\d{2}:\d{2})?$`,
		Deferred: true,
		Help: "Weekly refresh schedule. Edit coming in A4-b."},
	{Key: "daemons.retry_policy", Section: "daemons", Type: TypeEnum,
		Default: "exponential", Enum: []string{"none", "linear", "exponential"},
		Deferred: true,
		Help: "Retry policy on daemon failure. Edit coming in A4-b."},

	// ----- backups -----
	{Key: "backups.keep_n", Section: "backups", Type: TypeInt,
		Default: "5", Min: intPtr(0), Max: intPtr(50),
		Help: "Keep timestamped backups per client. Originals are never cleaned."},
	{Key: "backups.clean_now", Section: "backups", Type: TypeAction,
		Deferred: true,
		Help: "Delete eligible timestamped backups. Coming in A4-b."},

	// ----- advanced -----
	{Key: "advanced.open_app_data_folder", Section: "advanced", Type: TypeAction,
		Help: "Open mcp-local-hub data folder in OS file manager."},
	{Key: "advanced.export_config_bundle", Section: "advanced", Type: TypeAction,
		Deferred: true,
		Help: "Export all manifests + secrets ciphertext as a tarball. Coming in A4-b."},
}

// findDef returns the SettingDef for the given key, or nil if unknown.
func findDef(key string) *SettingDef {
	for i := range SettingsRegistry {
		if SettingsRegistry[i].Key == key {
			return &SettingsRegistry[i]
		}
	}
	return nil
}

// stringHasControlChars returns true if s contains any byte < 0x20 or
// the DEL byte 0x7F. Used by the TypeString and TypePath syntactic
// validators to reject paths/strings with embedded control characters
// (newlines, tabs, etc.) that break CLI output and downstream consumers.
func stringHasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7F {
			return true
		}
	}
	return false
}

// validate runs the per-type validator for def against value. Returns
// nil if valid, or an error whose message is suitable for surfacing in
// CLI stderr / HTTP 400 reason. Memo §4.2.
func validate(def *SettingDef, value string) error {
	switch def.Type {
	case TypeEnum:
		for _, v := range def.Enum {
			if value == v {
				return nil
			}
		}
		return fmt.Errorf("not in enum %v", def.Enum)
	case TypeBool:
		if value != "true" && value != "false" {
			return fmt.Errorf("must be 'true' or 'false'")
		}
		return nil
	case TypeInt:
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("not an integer: %v", err)
		}
		if def.Min != nil && n < *def.Min {
			return fmt.Errorf("below min %d", *def.Min)
		}
		if def.Max != nil && n > *def.Max {
			return fmt.Errorf("above max %d", *def.Max)
		}
		return nil
	case TypeString:
		if value == "" {
			if def.Optional {
				return nil
			}
			return fmt.Errorf("must not be empty")
		}
		if stringHasControlChars(value) {
			return fmt.Errorf("contains control characters")
		}
		if def.Pattern != "" {
			re, err := regexp.Compile(def.Pattern)
			if err != nil {
				return fmt.Errorf("internal: registry pattern compile failed: %v", err)
			}
			if !re.MatchString(value) {
				return fmt.Errorf("does not match pattern %s", def.Pattern)
			}
		}
		return nil
	case TypePath:
		if value == "" {
			if def.Optional {
				return nil
			}
			return fmt.Errorf("must not be empty")
		}
		if strings.ContainsRune(value, 0) {
			return fmt.Errorf("contains null byte")
		}
		if stringHasControlChars(value) {
			return fmt.Errorf("contains control characters")
		}
		if value != strings.TrimSpace(value) {
			return fmt.Errorf("has leading or trailing whitespace")
		}
		return nil
	case TypeAction:
		return fmt.Errorf("cannot set action key")
	}
	return fmt.Errorf("unknown type %q", def.Type)
}

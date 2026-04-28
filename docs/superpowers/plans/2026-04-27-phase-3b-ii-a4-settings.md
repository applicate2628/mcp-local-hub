# Phase 3B-II A4 — Settings screen Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship A4-a Settings screen — registry-driven 5-section config UI with per-section Save, BackupsList passive prune preview, and Open-app-data-folder action — fully covering the §5.7 master-design contract while deferring lifecycle mutation to A4-b.

**Architecture:** A package-level Go `Registry` in `internal/api/settings_registry.go` is the authoritative schema (`SettingDef` + 5 type validators). HTTP routes in `internal/gui/settings.go` and `internal/gui/backups.go` (NOT `internal/api`) call into `*api.API` and are wrapped with `requireSameOrigin`. CLI in `internal/cli/settings.go` consults the same registry. Frontend uses a single `Settings.tsx` screen with a sticky secondary inner-nav, registry-driven `FieldRenderer`, per-section dirty + Save state, and a passive `BackupsList` preview.

**Tech Stack:** Go 1.22 + Preact + TypeScript + Vitest + Playwright (Chromium, Windows-only E2E) + yaml.v3.

**Source memo:** [`docs/superpowers/specs/2026-04-27-phase-3b-ii-a4-settings-design.md`](../specs/2026-04-27-phase-3b-ii-a4-settings-design.md) (rev 2, Codex r1–r5 PASS at commit `103eb4a`).

**Branch:** `feat/phase-3b-ii-a4-settings`. Master tip is `103eb4a` (memo commit).

**DO NOT push or open PR without explicit user approval** (precedent A3-a/A3-b).

**Execution order (Codex r4 P1 — Task 4 must precede Task 2):** Task 0 → 1 → **4** → 2 → 3 → 5 → 6 → 7 → 8 → 9 → 10 → 11. Reason: Task 2's `internal/gui/settings.go` has `realSettingsAPI.OpenPath(...) { return OpenPath(path) }` which requires the `OpenPath` symbol that Task 4 introduces in `internal/gui/openpath.go`. The numbering preserves the original logical grouping (Go API → HTTP routes → CLI → helpers → frontend); the dispatch-order swap only re-orders Task 4 ahead of Task 2 to satisfy the symbol dependency.

---

## File Map

**New files (Go):**

- `internal/api/settings_registry.go` — `SettingDef`, `SettingType` constants, `Registry []SettingDef`, validators
- `internal/api/settings_registry_test.go` — registry shape + per-key validator tests
- `internal/api/settings_test.go` — `SettingsSet` round-trip, unknown-key preservation, concurrent-write
- `internal/gui/settings.go` — `registerSettingsRoutes`, `GET /api/settings`, `PUT /api/settings/<key>`, `POST /api/settings/<action>`
- `internal/gui/settings_test.go` — HTTP route tests + same-origin
- `internal/gui/backups.go` — `registerBackupsRoutes`, `GET /api/backups`, `GET /api/backups/clean-preview`
- `internal/gui/backups_test.go` — HTTP route tests + same-origin
- `internal/gui/openpath.go` — `OpenPath(path string) error` (cross-platform file-manager opener)
- `internal/gui/openpath_test.go` — `spawnProcess` seam tests
- `internal/cli/settings_registry_test.go` — CLI behavior over registry
- `internal/gui/frontend/src/lib/settings-types.ts` — `SettingDTO` (discriminated union), `SettingsEnvelope`, `SettingsSnapshot`
- `internal/gui/frontend/src/lib/use-settings-snapshot.ts` — hook
- `internal/gui/frontend/src/lib/settings-api.ts` — `getSettings`, `putSetting`, `postAction`, `getBackups`, `getBackupsPreview`
- `internal/gui/frontend/src/screens/Settings.tsx`
- `internal/gui/frontend/src/components/settings/FieldRenderer.tsx`
- `internal/gui/frontend/src/components/settings/SectionNav.tsx`
- `internal/gui/frontend/src/components/settings/SectionAppearance.tsx`
- `internal/gui/frontend/src/components/settings/SectionGuiServer.tsx`
- `internal/gui/frontend/src/components/settings/SectionDaemons.tsx`
- `internal/gui/frontend/src/components/settings/SectionBackups.tsx`
- `internal/gui/frontend/src/components/settings/SectionAdvanced.tsx`
- `internal/gui/frontend/src/components/settings/BackupsList.tsx`
- Vitest companions: each component gets `.test.tsx`. Plus `backups-copy.test.ts` (literal equality lock for the 6 verbatim Codex copy strings, Codex r1 P2.3).
- `internal/gui/e2e/tests/settings.spec.ts`

**Modified files:**

- `internal/api/settings.go` — add `settingsMu sync.Mutex`; rewrite `SettingsSet` to preserve unknown keys + validate; resolve registry defaults in `SettingsList`/`SettingsGet`
- `internal/cli/settings.go` — `list/get/set` consult registry
- `internal/gui/server.go` — wire `registerSettingsRoutes(s)` + `registerBackupsRoutes(s)` in `NewServer`
- `internal/gui/frontend/src/app.tsx` — 7th sidebar link "Settings" + `case "settings"` branch + `settingsDirty` in dirty-guard
- `internal/gui/frontend/src/styles/style.css` — Settings layout + theme/density CSS variables wiring (verify existing) + Backups dimmed/striped rows
- `CLAUDE.md` — E2E count update (76 → 92)
- `docs/superpowers/plans/phase-3b-ii-backlog.md` — mark A4 done at PR merge

---

## Task 0: Branch setup + baseline smoke

**Files:** none (workspace operation only).

- [ ] **Step 0.1: Verify clean working tree**

```bash
cd d:/dev/mcp-local-hub
git status
```

Expected: only `.playwright-mcp/` untracked (or completely clean). HEAD = `103eb4a`.

- [ ] **Step 0.2: Create feature branch**

```bash
git checkout -b feat/phase-3b-ii-a4-settings
```

Expected: `Switched to a new branch 'feat/phase-3b-ii-a4-settings'`.

- [ ] **Step 0.3: Smoke baseline Go**

```bash
go test ./internal/api/... ./internal/gui/... ./internal/cli/...
```

Expected: PASS. Capture baseline for regression comparison.

- [ ] **Step 0.4: Smoke baseline frontend**

```bash
cd internal/gui/frontend && npm run test && npm run typecheck && cd ../../..
```

Expected: 224 Vitest pass + 0 type errors.

- [ ] **Step 0.5: Smoke baseline E2E (skip if slow)**

```bash
cd internal/gui/e2e && npm test -- --grep "shell" && cd ../../..
```

Expected: 3 shell scenarios pass (~5s). Confirms harness alive. Full E2E re-run happens at Task 11.

**No commit at Task 0** — branch exists, baseline confirmed, ready for Task 1.

---

## Task 1: Settings registry + validators + concurrent-write safety

**Memo refs:** §4.1, §4.2, §5, §6.2.

**Files:**

- Create: `internal/api/settings_registry.go`
- Create: `internal/api/settings_registry_test.go`
- Replace: `internal/api/settings_test.go` (file already exists with old unqualified-key tests; delete obsolete tests, keep only the new registry-aware ones below — Codex r1 P1.2)
- Modify: `internal/api/settings.go`

### Step 1.1 — Create the registry skeleton

- [ ] **Step 1.1a: Write `internal/api/settings_registry.go`**

```go
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

// Registry is the canonical list of all known settings keys. Order
// matches §5.7 reading order: appearance, gui_server, daemons, backups,
// advanced. CLI list and GUI snapshot both render in this order.
var Registry = []SettingDef{
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
	for i := range Registry {
		if Registry[i].Key == key {
			return &Registry[i]
		}
	}
	return nil
}

// stringHasControlChars returns true if s contains any byte < 0x20 or
// the DEL byte 0x7F. Used by TypeString/TypePath syntactic validators.
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
		if value != strings.TrimSpace(value) {
			return fmt.Errorf("has leading or trailing whitespace")
		}
		return nil
	case TypeAction:
		return fmt.Errorf("cannot set action key")
	}
	return fmt.Errorf("unknown type %q", def.Type)
}
```

- [ ] **Step 1.1b: Write `internal/api/settings_registry_test.go`**

```go
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
	for _, d := range Registry {
		if !allowed[d.Section] {
			t.Fatalf("registry entry %q has unknown section %q", d.Key, d.Section)
		}
	}
}

func TestRegistry_NoDuplicateKeys(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range Registry {
		if seen[d.Key] {
			t.Fatalf("duplicate registry key %q", d.Key)
		}
		seen[d.Key] = true
	}
}

func TestRegistry_DefaultsValidate(t *testing.T) {
	for _, d := range Registry {
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
	for _, d := range Registry {
		if d.Type == TypeEnum && len(d.Enum) == 0 {
			t.Errorf("enum entry %q has empty Enum", d.Key)
		}
	}
}

func TestRegistry_IntBoundsConsistent(t *testing.T) {
	for _, d := range Registry {
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
```

- [ ] **Step 1.1c: Run registry tests — expect PASS** (Codex r1 P2.2)

```bash
go test ./internal/api/... -run TestRegistry -v 2>&1 | tail -20
```

Expected: all `TestRegistry_*` and `TestValidate_*` pass. (No red-then-green TDD cycle here because the registry definitions and validators are tightly coupled — we cannot stub one without the other. Step 1.2 reintroduces TDD-style failing-then-passing for `SettingsSet`.)

- [ ] **Step 1.1d: Commit (registry only, no consumer changes)**

```bash
git add internal/api/settings_registry.go internal/api/settings_registry_test.go
git commit -m "feat(api): settings registry + per-type validators (A4-a §4.1, §4.2, §5)"
```

### Step 1.2 — Add `settingsMu` and rewrite `SettingsSet` to preserve unknown keys + validate

- [ ] **Step 1.2a: Replace `internal/api/settings.go`**

```go
package api

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"
)

// settingsMu serializes read-validate-write over gui-preferences.yaml.
// Mirrors vaultMutex in secrets.go. Memo §6.2 (Codex r1 P1.5): without
// this, concurrent PUT /api/settings/<a> and PUT /api/settings/<b> can
// each read {x:1, y:2}, modify their own key, and the slower writer
// silently drops the faster writer's change.
var settingsMu sync.Mutex

// SettingsPath returns the canonical preferences file location (in the
// per-user data dir — same as secrets).
func SettingsPath() string {
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		return filepath.Join(v, "mcp-local-hub", "gui-preferences.yaml")
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "mcp-local-hub", "gui-preferences.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "gui-preferences.yaml"
	}
	return filepath.Join(home, ".local", "share", "mcp-local-hub", "gui-preferences.yaml")
}

// SettingsList returns the schema-resolved snapshot: every registry key
// (settable + action) mapped to either the persisted value or its
// registry default. Unknown keys in the YAML file are NOT included in
// the returned map (they are preserved on disk via SettingsSet, but not
// exposed through the schema-resolved API).
func (a *API) SettingsList() (map[string]string, error) {
	return a.SettingsListIn(SettingsPath())
}

// SettingsListIn is the tempdir-capable form.
func (a *API) SettingsListIn(path string) (map[string]string, error) {
	raw, err := readRawSettingsMap(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, def := range Registry {
		if def.Type == TypeAction {
			continue
		}
		if v, ok := raw[def.Key]; ok {
			out[def.Key] = v
			continue
		}
		out[def.Key] = def.Default
	}
	return out, nil
}

// SettingsGet returns the value for a key (registry default if not
// persisted). Returns an error if the key is unknown or is an action.
func (a *API) SettingsGet(key string) (string, error) {
	return a.SettingsGetIn(SettingsPath(), key)
}

// SettingsGetIn is the tempdir-capable form.
func (a *API) SettingsGetIn(path, key string) (string, error) {
	def := findDef(key)
	if def == nil {
		return "", fmt.Errorf("unknown setting %q", key)
	}
	if def.Type == TypeAction {
		return "", fmt.Errorf("%q is an action; use 'mcp settings invoke' (coming in A4-b)", key)
	}
	all, err := a.SettingsListIn(path)
	if err != nil {
		return "", err
	}
	return all[key], nil
}

// SettingsSet writes a key=value pair, creating the file if needed.
// Validates against the registry, preserves unknown keys on the way
// through (memo §2.2 Codex r1 P2.1), and serializes the read-modify-write
// via settingsMu (memo §6.2 Codex r1 P1.5).
func (a *API) SettingsSet(key, value string) error {
	return a.SettingsSetIn(SettingsPath(), key, value)
}

// SettingsSetIn is the tempdir-capable form.
func (a *API) SettingsSetIn(path, key, value string) error {
	def := findDef(key)
	if def == nil {
		return fmt.Errorf("unknown setting %q", key)
	}
	if err := validate(def, value); err != nil {
		return fmt.Errorf("invalid value for %s: %v", key, err)
	}
	settingsMu.Lock()
	defer settingsMu.Unlock()
	raw, err := readRawSettingsMap(path)
	if err != nil {
		return err
	}
	raw[key] = value
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// readRawSettingsMap reads the file as a flat map[string]string. Unknown
// keys (e.g., a typo or a future-deferred entry written by CLI ahead of
// A4-b's GUI editor) are preserved verbatim. Returns an empty map if
// the file does not exist.
func readRawSettingsMap(path string) (map[string]string, error) {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}
```

- [ ] **Step 1.2b: REPLACE `internal/api/settings_test.go`** (Codex r1 P1.2)

The existing file contains `TestSettingsRoundTrip` and `TestSettingsGetMissingKey` that use unqualified keys (`theme`, `shell`). Those will fail validation against the registry (registry has `appearance.theme`, `appearance.shell`, not bare `theme`/`shell`). Delete the obsolete file contents entirely and write the replacement below — do not append.

```go
package api

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func tmpSettings(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "gui-preferences.yaml")
}

func TestSettings_DefaultsResolve(t *testing.T) {
	a := &API{}
	all, err := a.SettingsListIn(tmpSettings(t))
	if err != nil {
		t.Fatal(err)
	}
	if all["appearance.theme"] != "system" {
		t.Errorf("expected default 'system', got %q", all["appearance.theme"])
	}
	if _, has := all["advanced.open_app_data_folder"]; has {
		t.Error("action keys must not appear in SettingsList output")
	}
}

func TestSettings_SetAndGet(t *testing.T) {
	a := &API{}
	path := tmpSettings(t)
	if err := a.SettingsSetIn(path, "appearance.theme", "dark"); err != nil {
		t.Fatal(err)
	}
	got, err := a.SettingsGetIn(path, "appearance.theme")
	if err != nil {
		t.Fatal(err)
	}
	if got != "dark" {
		t.Errorf("expected 'dark', got %q", got)
	}
}

func TestSettings_Set_RejectsUnknownKey(t *testing.T) {
	a := &API{}
	err := a.SettingsSetIn(tmpSettings(t), "no.such.key", "x")
	if err == nil || !contains(err.Error(), "unknown setting") {
		t.Fatalf("expected unknown-setting error, got %v", err)
	}
}

func TestSettings_Set_RejectsBadValue(t *testing.T) {
	a := &API{}
	err := a.SettingsSetIn(tmpSettings(t), "appearance.theme", "puce")
	if err == nil || !contains(err.Error(), "invalid value") {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestSettings_Set_RejectsAction(t *testing.T) {
	a := &API{}
	err := a.SettingsSetIn(tmpSettings(t), "advanced.open_app_data_folder", "anything")
	if err == nil {
		t.Fatal("expected action-set rejection")
	}
}

func TestSettings_Set_PreservesUnknownKeys(t *testing.T) {
	// Codex r1 P2.1: a stale or future-unknown key must round-trip.
	a := &API{}
	path := tmpSettings(t)
	// Seed a file with a known + an unknown key.
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	seeded := []byte("appearance.theme: dark\nfuture_unknown.key: hello\n")
	if err := os.WriteFile(path, seeded, 0600); err != nil {
		t.Fatal(err)
	}
	// Mutate a known key.
	if err := a.SettingsSetIn(path, "appearance.theme", "light"); err != nil {
		t.Fatal(err)
	}
	// Reload raw and assert the unknown key survived.
	raw, err := readRawSettingsMap(path)
	if err != nil {
		t.Fatal(err)
	}
	if raw["appearance.theme"] != "light" {
		t.Errorf("known-key write lost: got %q", raw["appearance.theme"])
	}
	if raw["future_unknown.key"] != "hello" {
		t.Errorf("unknown-key NOT preserved on rewrite (Codex r1 P2.1): got %q", raw["future_unknown.key"])
	}
	// And ensure SettingsList still doesn't expose the unknown.
	all, err := a.SettingsListIn(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := all["future_unknown.key"]; has {
		t.Error("SettingsList must not expose unknown keys")
	}
}

func TestSettings_Concurrent_DistinctKeys(t *testing.T) {
	// Codex r1 P1.5 + r3 P2.2: 10 settable registry keys, 10 goroutines,
	// each writing one distinct key concurrently. After Wait, every key
	// must still be present in the file.
	a := &API{}
	path := tmpSettings(t)

	type kv struct{ k, v string }
	pairs := []kv{
		{"appearance.theme", "dark"},
		{"appearance.density", "compact"},
		{"appearance.shell", "bash"},
		{"appearance.default_home", "/home/x"},
		{"gui_server.browser_on_launch", "false"},
		{"gui_server.port", "9999"},
		{"gui_server.tray", "false"},
		{"daemons.weekly_schedule", "daily Mon 04:00"},
		{"daemons.retry_policy", "linear"},
		{"backups.keep_n", "12"},
	}
	var wg sync.WaitGroup
	for _, p := range pairs {
		wg.Add(1)
		go func(p kv) {
			defer wg.Done()
			if err := a.SettingsSetIn(path, p.k, p.v); err != nil {
				t.Errorf("set %q: %v", p.k, err)
			}
		}(p)
	}
	wg.Wait()
	raw, err := readRawSettingsMap(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pairs {
		if raw[p.k] != p.v {
			t.Errorf("lost write: %q expected %q got %q", p.k, p.v, raw[p.k])
		}
	}
}

func TestSettings_Concurrent_SameKey(t *testing.T) {
	// Codex r3 P2.2: 20 goroutines writing the same key; round-robin
	// through 3 valid enum values. File must always parse cleanly and
	// final value must be one of the 3.
	a := &API{}
	path := tmpSettings(t)
	values := []string{"light", "dark", "system"}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := a.SettingsSetIn(path, "appearance.theme", values[i%3]); err != nil {
				t.Errorf("set %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	raw, err := readRawSettingsMap(path)
	if err != nil {
		t.Fatalf("file did not parse cleanly after concurrent writes: %v", err)
	}
	got := raw["appearance.theme"]
	ok := false
	for _, v := range values {
		if got == v {
			ok = true
			break
		}
	}
	if !ok {
		t.Errorf("final value %q not in %v (torn write?)", got, values)
	}
}

// contains is a tiny substring helper (avoids importing strings just here).
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		(len(haystack) > len(needle) && (haystack[:len(needle)] == needle ||
			haystack[len(haystack)-len(needle):] == needle ||
			indexOf(haystack, needle) >= 0)))
}
func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 1.2c: Run all api tests — expect PASS**

```bash
go test ./internal/api/... -count=1 -v 2>&1 | tail -40
```

Expected: all pass, including new `TestSettings_*` and `TestRegistry_*`.

- [ ] **Step 1.2d: Commit**

```bash
git add internal/api/settings.go internal/api/settings_test.go
git commit -m "feat(api): settings validation + unknown-key preservation + concurrent-write safety (A4-a §2.2, §6.2)"
```

---

## Task 2: HTTP routes — `internal/gui/settings.go`

**Memo refs:** §4.3, §4.5, §6.1, §6.2, §6.3.

**Pre-requisite (Codex r4 P1):** Task 4 (`OpenPath` helper) must be completed **before** Task 2. The `realSettingsAPI` adapter in this task references the `OpenPath` symbol introduced by Task 4. Subagent execution order: Task 0 → 1 → 4 → 2 → ...

**Files:**

- Create: `internal/gui/settings.go`
- Create: `internal/gui/settings_test.go`
- Modify: `internal/gui/server.go` (wire `registerSettingsRoutes(s)`)

### Step 2.1 — Wire the narrow API interface + router

- [ ] **Step 2.1a: Write `internal/gui/settings.go`**

```go
// internal/gui/settings.go
package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"mcp-local-hub/internal/api"
)

// settingsAPI is the narrow surface used by /api/settings handlers.
// realSettingsAPI delegates to *api.API; tests inject their own.
type settingsAPI interface {
	List() (map[string]string, error)
	Set(key, value string) error
	OpenPath(path string) error
	SettingsPath() string
}

type realSettingsAPI struct{}

func (realSettingsAPI) List() (map[string]string, error)   { return api.NewAPI().SettingsList() }
func (realSettingsAPI) Set(key, value string) error        { return api.NewAPI().SettingsSet(key, value) }
func (realSettingsAPI) SettingsPath() string               { return api.SettingsPath() }
func (realSettingsAPI) OpenPath(path string) error         { return OpenPath(path) }

// configSettingDTO is the JSON shape for non-action settings entries.
// `default` and `value` are ALWAYS emitted (no omitempty) so legitimate
// empty values — most importantly `appearance.default_home` whose
// default is "" with Optional:true — round-trip correctly. Memo §6.1.
//
// Codex r6 P2: rev-1 used a single settingDTO with `omitempty` on
// Default/Value, which dropped those keys for any non-action setting
// whose value happened to be empty. Splitting into two DTO types is
// the only way to guarantee the wire contract: actions omit, configs
// always include.
type configSettingDTO struct {
	Key      string   `json:"key"`
	Section  string   `json:"section"`
	Type     string   `json:"type"`
	Default  string   `json:"default"`         // ALWAYS emitted
	Value    string   `json:"value"`           // ALWAYS emitted
	Enum     []string `json:"enum,omitempty"`
	Min      *int     `json:"min,omitempty"`
	Max      *int     `json:"max,omitempty"`
	Pattern  string   `json:"pattern,omitempty"`
	Optional bool     `json:"optional,omitempty"`
	Deferred bool     `json:"deferred"`
	Help     string   `json:"help"`
}

// actionSettingDTO is the JSON shape for action settings entries.
// `default` and `value` are deliberately absent — actions have no
// stored value. Codex r1 P2.2 + r2 P1.2.
type actionSettingDTO struct {
	Key      string `json:"key"`
	Section  string `json:"section"`
	Type     string `json:"type"` // always "action"
	Deferred bool   `json:"deferred"`
	Help     string `json:"help"`
}

func registerSettingsRoutes(s *Server) {
	s.mux.HandleFunc("/api/settings", s.requireSameOrigin(s.settingsListHandler))
	s.mux.HandleFunc("/api/settings/", s.requireSameOrigin(s.settingsByKeyHandler))
}

// settingsListHandler handles GET /api/settings. Returns a snapshot of
// every registry entry (action + non-action) plus the live actual_port
// at the top level. Memo §6.1.
func (s *Server) settingsListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	values, err := s.settings.List()
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "SETTINGS_LIST_FAILED")
		return
	}
	// Heterogeneous slice: configSettingDTO or actionSettingDTO per entry.
	settings := make([]any, 0, len(api.Registry))
	for _, def := range api.Registry {
		if def.Type == api.TypeAction {
			settings = append(settings, actionSettingDTO{
				Key:      def.Key,
				Section:  def.Section,
				Type:     string(def.Type),
				Deferred: def.Deferred,
				Help:     def.Help,
			})
			continue
		}
		v, has := values[def.Key]
		if !has {
			v = def.Default
		}
		settings = append(settings, configSettingDTO{
			Key:      def.Key,
			Section:  def.Section,
			Type:     string(def.Type),
			Default:  def.Default,
			Value:    v,
			Enum:     def.Enum,
			Min:      def.Min,
			Max:      def.Max,
			Pattern:  def.Pattern,
			Optional: def.Optional,
			Deferred: def.Deferred,
			Help:     def.Help,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":    settings,
		"actual_port": s.Port(),
	})
}

// settingsByKeyHandler handles PUT /api/settings/<key> and POST
// /api/settings/<action>. Memo §6.2 + §6.3.
func (s *Server) settingsByKeyHandler(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/api/settings/")
	if key == "" {
		http.NotFound(w, r)
		return
	}
	def := findRegistryDef(key)
	if def == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "unknown setting",
			"key":   key,
		})
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.settingsPut(w, r, def)
	case http.MethodPost:
		s.settingsPost(w, r, def)
	default:
		if def.Type == api.TypeAction {
			w.Header().Set("Allow", "POST")
		} else {
			w.Header().Set("Allow", "PUT")
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) settingsPut(w http.ResponseWriter, r *http.Request, def *api.SettingDef) {
	if def.Type == api.TypeAction {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error": "is_action",
			"key":   def.Key,
		})
		return
	}
	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "SETTINGS_INVALID_JSON")
		return
	}
	if err := s.settings.Set(def.Key, body.Value); err != nil {
		// Validation failures bubble up from api.SettingsSet with prefix
		// "invalid value for ...:". Map them to 400. Other errors are 500.
		if strings.HasPrefix(err.Error(), "invalid value") {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":  "validation_failed",
				"key":    def.Key,
				"reason": err.Error(),
			})
			return
		}
		writeAPIError(w, err, http.StatusInternalServerError, "SETTINGS_SET_FAILED")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"saved": true,
		"key":   def.Key,
		"value": body.Value,
	})
}

func (s *Server) settingsPost(w http.ResponseWriter, r *http.Request, def *api.SettingDef) {
	if def.Type != api.TypeAction {
		w.Header().Set("Allow", "PUT")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error": "not_action",
			"key":   def.Key,
		})
		return
	}
	if def.Deferred {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "deferred_action_not_implemented",
			"key":   def.Key,
		})
		return
	}
	switch def.Key {
	case "advanced.open_app_data_folder":
		target := s.settings.SettingsPath()
		// Open the directory containing the file, not the file itself.
		dir := target
		if i := strings.LastIndexAny(dir, "\\/"); i >= 0 {
			dir = dir[:i]
		}
		if err := s.settings.OpenPath(dir); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error":  "spawn_failed",
				"reason": err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"opened": dir})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "unknown_action",
			"key":   def.Key,
		})
	}
}

// findRegistryDef returns a pointer into api.Registry for key, or nil.
// Local helper so this file does not depend on an unexported api function.
func findRegistryDef(key string) *api.SettingDef {
	for i := range api.Registry {
		if api.Registry[i].Key == key {
			return &api.Registry[i]
		}
	}
	return nil
}
```

- [ ] **Step 2.1b: Wire the route in `internal/gui/server.go`**

In `NewServer`, add the field and the call. Find the line `s.secrets = realSecretsAPI{}` and add immediately below:

```go
s.settings = realSettingsAPI{}
```

Find the `registerSecretsRoutes(s)` line and add immediately below:

```go
registerSettingsRoutes(s)
```

Find the `Server` struct definition (search for `secrets          secretsAPI`) and add a new field above or below it:

```go
settings settingsAPI
```

- [ ] **Step 2.1c: Write `internal/gui/settings_test.go`**

```go
package gui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// fakeSettings is the test seam.
type fakeSettings struct {
	values     map[string]string
	setErr     error
	openErr    error
	openCalled string
}

func (f *fakeSettings) List() (map[string]string, error) {
	if f.values == nil {
		f.values = map[string]string{}
	}
	out := map[string]string{}
	for _, def := range api.Registry {
		if def.Type == api.TypeAction {
			continue
		}
		if v, ok := f.values[def.Key]; ok {
			out[def.Key] = v
		} else {
			out[def.Key] = def.Default
		}
	}
	return out, nil
}
func (f *fakeSettings) Set(key, value string) error {
	if f.setErr != nil {
		return f.setErr
	}
	if f.values == nil {
		f.values = map[string]string{}
	}
	f.values[key] = value
	return nil
}
func (f *fakeSettings) SettingsPath() string { return "/tmp/test/gui-preferences.yaml" }
func (f *fakeSettings) OpenPath(path string) error {
	f.openCalled = path
	return f.openErr
}

func newTestServer(t *testing.T) (*Server, *fakeSettings) {
	t.Helper()
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	fake := &fakeSettings{}
	s.settings = fake
	return s, fake
}

func sameOriginHeaders() http.Header {
	h := http.Header{}
	h.Set("Sec-Fetch-Site", "same-origin")
	return h
}

func TestSettings_GET_Snapshot(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/settings", nil)
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Settings   []map[string]any `json:"settings"`
		ActualPort int              `json:"actual_port"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ActualPort == 0 {
		t.Error("actual_port must be set (memo §6.1, Codex r1 P2.4)")
	}
	if len(resp.Settings) != len(api.Registry) {
		t.Errorf("expected %d entries, got %d", len(api.Registry), len(resp.Settings))
	}
}

func TestSettings_GET_ConfigEntriesAlwaysIncludeDefaultAndValue(t *testing.T) {
	// Codex r7 P2: regression for r6 — config entries with empty default/value
	// (most importantly appearance.default_home with Default:"", Optional:true)
	// MUST include both `default` and `value` keys in the JSON, otherwise the
	// frontend ConfigSettingDTO contract breaks.
	s, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/settings", nil)
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	var resp struct {
		Settings []map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var entry map[string]any
	for _, e := range resp.Settings {
		if e["key"] == "appearance.default_home" {
			entry = e
			break
		}
	}
	if entry == nil {
		t.Fatal("appearance.default_home missing from snapshot")
	}
	if entry["type"] != "path" {
		t.Errorf("expected type=path, got %v", entry["type"])
	}
	if _, has := entry["default"]; !has {
		t.Error("default_home must include 'default' key in JSON (Codex r7 P2)")
	}
	if _, has := entry["value"]; !has {
		t.Error("default_home must include 'value' key in JSON (Codex r7 P2)")
	}
	// And both should be empty strings (since Optional:true and no user value).
	if entry["default"] != "" {
		t.Errorf("expected default=\"\", got %v", entry["default"])
	}
	if entry["value"] != "" {
		t.Errorf("expected value=\"\", got %v", entry["value"])
	}
}

func TestSettings_GET_ActionEntriesOmitValueDefault(t *testing.T) {
	// Codex r1 P2.2: action entries MUST not include value or default
	// in the JSON wire shape.
	s, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/settings", nil)
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	var resp struct {
		Settings []map[string]any `json:"settings"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, e := range resp.Settings {
		if e["type"] == "action" {
			if _, has := e["value"]; has {
				t.Errorf("action %q must not have 'value' in JSON", e["key"])
			}
			if _, has := e["default"]; has {
				t.Errorf("action %q must not have 'default' in JSON", e["key"])
			}
		}
	}
}

func TestSettings_PUT_ValidWrite(t *testing.T) {
	s, fake := newTestServer(t)
	body := strings.NewReader(`{"value":"dark"}`)
	req := httptest.NewRequest("PUT", "/api/settings/appearance.theme", body)
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	if fake.values["appearance.theme"] != "dark" {
		t.Errorf("expected dark stored, got %q", fake.values["appearance.theme"])
	}
}

func TestSettings_PUT_InvalidEnum(t *testing.T) {
	s, fake := newTestServer(t)
	// Make the fake call into the registry validator path: simulate the
	// real api.SettingsSet error message format.
	fake.setErr = nil // we want the route to call into Set
	// Use a real validator path: fake.Set returns nil here, so the route
	// returns 200. To simulate real behavior, swap the fake to error.
	fake.setErr = errString("invalid value for appearance.theme: not in enum [light dark system]")
	body := strings.NewReader(`{"value":"puce"}`)
	req := httptest.NewRequest("PUT", "/api/settings/appearance.theme", body)
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["error"] != "validation_failed" {
		t.Errorf("expected validation_failed, got %v", resp["error"])
	}
}

func TestSettings_PUT_UnknownKey(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("PUT", "/api/settings/no.such.key", strings.NewReader(`{"value":"x"}`))
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 404 {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestSettings_PUT_ToActionKey_Returns405(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("PUT", "/api/settings/advanced.open_app_data_folder", strings.NewReader(`{"value":"x"}`))
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 405 {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
	if rr.Header().Get("Allow") != "POST" {
		t.Errorf("expected Allow: POST, got %q", rr.Header().Get("Allow"))
	}
}

func TestSettings_POST_ToConfigKey_Returns405(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("POST", "/api/settings/appearance.theme", bytes.NewReader(nil))
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 405 {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
	if rr.Header().Get("Allow") != "PUT" {
		t.Errorf("expected Allow: PUT, got %q", rr.Header().Get("Allow"))
	}
}

func TestSettings_POST_OpenAppDataFolder_Success(t *testing.T) {
	s, fake := newTestServer(t)
	req := httptest.NewRequest("POST", "/api/settings/advanced.open_app_data_folder", nil)
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.HasSuffix(fake.openCalled, "test") && fake.openCalled == "" {
		t.Errorf("OpenPath not called, got %q", fake.openCalled)
	}
}

func TestSettings_POST_DeferredAction_Returns404(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("POST", "/api/settings/backups.clean_now", nil)
	req.Header = sameOriginHeaders()
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 404 {
		t.Fatalf("expected 404 deferred, got %d", rr.Code)
	}
	var resp map[string]any
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["error"] != "deferred_action_not_implemented" {
		t.Errorf("expected deferred_action_not_implemented, got %v", resp["error"])
	}
}

func TestSettings_AllRoutes_RejectCrossOrigin(t *testing.T) {
	// Codex r1 P2.5 + r2 P2.2: every settings route (read AND mutating)
	// must reject cross-origin browser requests.
	s, _ := newTestServer(t)
	cases := []struct{ method, path, body string }{
		{"GET", "/api/settings", ""},
		{"PUT", "/api/settings/appearance.theme", `{"value":"dark"}`},
		{"POST", "/api/settings/advanced.open_app_data_folder", ""},
	}
	for _, c := range cases {
		var body *strings.Reader
		if c.body != "" {
			body = strings.NewReader(c.body)
		} else {
			body = strings.NewReader("")
		}
		req := httptest.NewRequest(c.method, c.path, body)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rr := httptest.NewRecorder()
		s.mux.ServeHTTP(rr, req)
		if rr.Code != 403 {
			t.Errorf("%s %s: expected 403 cross-origin, got %d", c.method, c.path, rr.Code)
		}
	}
}

// errString turns a string into an error for fake.setErr.
type errString string

func (e errString) Error() string { return string(e) }
```

- [ ] **Step 2.1d: Run gui tests — expect PASS**

```bash
go test ./internal/gui/... -count=1 -run TestSettings -v 2>&1 | tail -30
```

Expected: all `TestSettings_*` pass.

- [ ] **Step 2.1e: Commit**

```bash
git add internal/gui/settings.go internal/gui/settings_test.go internal/gui/server.go
git commit -m "feat(gui): /api/settings GET/PUT/POST routes + same-origin (A4-a §4.3, §6)"
```

---

## Task 3: CLI updates — registry-aware `mcp settings list/get/set`

**Memo refs:** §7.

**Files:**

- Modify: `internal/cli/settings.go`
- Create: `internal/cli/settings_registry_test.go`

### Step 3.1 — Rewrite CLI to consult registry

- [ ] **Step 3.1a: Replace `internal/cli/settings.go`**

```go
package cli

import (
	"fmt"
	"sort"
	"strings"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newSettingsCmdReal() *cobra.Command {
	root := &cobra.Command{
		Use:   "settings",
		Short: "Read/write GUI preferences (theme, shell, default-home, etc.)",
		Long: `Manage persistent key/value preferences under
%LOCALAPPDATA%\mcp-local-hub\gui-preferences.yaml (or equivalent XDG path).

Schema is authoritative in the Go registry (internal/api/settings_registry.go).
Keys, types, defaults, and validation rules come from there.

Subcommands:
  settings list      # all known settings, grouped by section
  settings get <k>   # print one value (registry default if unset)
  settings set <k> <v>  # write one validated value`,
	}
	root.AddCommand(newSettingsListCmd())
	root.AddCommand(newSettingsGetCmd())
	root.AddCommand(newSettingsSetCmd())
	return root
}

func newSettingsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Print all known settings, grouped by section",
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			values, err := a.SettingsList()
			if err != nil {
				return err
			}
			// Group by section, in registry order.
			currentSection := ""
			for _, def := range api.Registry {
				if def.Section != currentSection {
					if currentSection != "" {
						fmt.Fprintln(cmd.OutOrStdout())
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%s:\n", def.Section)
					currentSection = def.Section
				}
				keyShort := strings.TrimPrefix(def.Key, def.Section+".")
				if def.Type == api.TypeAction {
					marker := ""
					if def.Deferred {
						marker = "  [deferred — coming in A4-b]"
					}
					fmt.Fprintf(cmd.OutOrStdout(), "  %s = <action>%s\n", keyShort, marker)
					continue
				}
				v, has := values[def.Key]
				if !has {
					v = def.Default
				}
				marker := ""
				if def.Deferred {
					marker = "  [deferred]"
				}
				if def.Key == "gui_server.port" {
					marker = "  [restart required]"
				}
				if v == "" {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s = <empty>  (default: %q)%s\n", keyShort, def.Default, marker)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s = %s  (default: %s)%s\n", keyShort, v, def.Default, marker)
				}
			}
			return nil
		},
	}
}

func newSettingsGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Print the value for a setting",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := args[0]
			def := lookupRegistry(key)
			if def == nil {
				return fmt.Errorf("unknown setting %q", key)
			}
			if def.Type == api.TypeAction {
				return fmt.Errorf("%s is an action; use 'mcp settings invoke' (coming in A4-b)", key)
			}
			a := api.NewAPI()
			val, err := a.SettingsGet(key)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), val)
			if def.Deferred {
				fmt.Fprintf(cmd.ErrOrStderr(), "[deferred — this field is reserved for A4-b]\n")
			}
			return nil
		},
	}
}

func newSettingsSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Write a setting value (validated against registry)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, value := args[0], args[1]
			def := lookupRegistry(key)
			if def == nil {
				return fmt.Errorf("unknown setting %q", key)
			}
			if def.Type == api.TypeAction {
				return fmt.Errorf("cannot set action key %s", key)
			}
			a := api.NewAPI()
			if err := a.SettingsSet(key, value); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ %s=%s\n", key, value)
			if def.Deferred {
				fmt.Fprintf(cmd.ErrOrStderr(), "setting accepted; this field is deferred to A4-b and has no effect yet\n")
			}
			return nil
		},
	}
}

// lookupRegistry returns a pointer into api.Registry for key, or nil.
func lookupRegistry(key string) *api.SettingDef {
	for i := range api.Registry {
		if api.Registry[i].Key == key {
			return &api.Registry[i]
		}
	}
	return nil
}

// sortedRegistryKeys returns registry keys in canonical (registry) order.
// Currently unused but kept for future "settings reset" subcommand.
var _ = func() []string {
	keys := make([]string, 0, len(api.Registry))
	for _, d := range api.Registry {
		keys = append(keys, d.Key)
	}
	sort.Strings(keys) // stable order if some caller wants alpha
	return keys
}
```

- [ ] **Step 3.1b: Write `internal/cli/settings_registry_test.go`**

```go
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

// settle ensures any leftover env state from prior tests doesn't leak.
func TestMain(m *testing.M) {
	// The withTempHome helper sets envs per-test via t.Setenv, which is
	// auto-restored. No extra setup needed.
	os.Exit(m.Run())
}
```

- [ ] **Step 3.1c: Run CLI tests — expect PASS**

```bash
go test ./internal/cli/... -count=1 -run TestCLI_ -v 2>&1 | tail -30
```

Expected: all `TestCLI_*` pass.

- [ ] **Step 3.1d: Commit**

```bash
git add internal/cli/settings.go internal/cli/settings_registry_test.go
git commit -m "feat(cli): mcp settings consults registry (validation, deferred warnings, action rejection) (A4-a §7)"
```

---

## Task 4: `OpenPath` helper

**Memo refs:** §2.4, §9.5.

**Execution-order note (Codex r4 P1):** despite the numerical Task ID being 4, this task **must execute before Task 2** because Task 2's `realSettingsAPI.OpenPath` references the `OpenPath` symbol introduced here. Subagent dispatch order: Task 0 → 1 → 4 → 2 → 3 → 5 → 6 → 7 → 8 → 9 → 10 → 11. The numerical ID is preserved for traceability with the memo §13 commit decomposition.

**Files:**

- Create: `internal/gui/openpath.go`
- Create: `internal/gui/openpath_test.go`

### Step 4.1 — Write the cross-platform helper

- [ ] **Step 4.1a: Write `internal/gui/openpath.go`**

```go
// internal/gui/openpath.go
//
// OpenPath opens a filesystem directory in the OS file manager. Unlike
// LaunchBrowser (which is browser-oriented and tries Chrome --app first),
// OpenPath goes straight to the file-manager:
//   - Windows: explorer.exe <path>
//   - macOS:   open <path>
//   - Linux:   xdg-open <path>
//
// Reuses the spawnProcess seam already defined in browser.go so tests
// can intercept the spawn without touching the OS file manager.
//
// Memo §2.4 (Codex r1 P1.6): originally we proposed reusing LaunchBrowser
// for the "Open app-data folder" action, but LaunchBrowser opens browsers,
// not file managers — Chrome would happily load the path in a tab. This
// dedicated helper avoids that confusion.

package gui

import "runtime"

func OpenPath(path string) error {
	switch runtime.GOOS {
	case "windows":
		return spawnProcess("explorer.exe", path)
	case "darwin":
		return spawnProcess("open", path)
	default:
		return spawnProcess("xdg-open", path)
	}
}
```

- [ ] **Step 4.1b: Write `internal/gui/openpath_test.go`**

```go
package gui

import (
	"errors"
	"runtime"
	"testing"
)

func TestOpenPath_UsesPlatformCommand(t *testing.T) {
	// Capture spawn invocations.
	type invocation struct {
		name string
		args []string
	}
	var captured invocation
	orig := spawnProcess
	spawnProcess = func(name string, args ...string) error {
		captured = invocation{name, args}
		return nil
	}
	defer func() { spawnProcess = orig }()

	err := OpenPath("/some/dir")
	if err != nil {
		t.Fatalf("OpenPath returned: %v", err)
	}

	var wantName string
	switch runtime.GOOS {
	case "windows":
		wantName = "explorer.exe"
	case "darwin":
		wantName = "open"
	default:
		wantName = "xdg-open"
	}
	if captured.name != wantName {
		t.Errorf("expected %q, got %q", wantName, captured.name)
	}
	if len(captured.args) != 1 || captured.args[0] != "/some/dir" {
		t.Errorf("expected args=[/some/dir], got %v", captured.args)
	}
}

func TestOpenPath_PropagatesError(t *testing.T) {
	orig := spawnProcess
	spawnProcess = func(name string, args ...string) error {
		return errors.New("boom")
	}
	defer func() { spawnProcess = orig }()
	err := OpenPath("/x")
	if err == nil || err.Error() != "boom" {
		t.Errorf("expected error 'boom', got %v", err)
	}
}
```

- [ ] **Step 4.1c: Run — expect PASS**

```bash
go test ./internal/gui/... -count=1 -run TestOpenPath -v 2>&1 | tail -20
```

- [ ] **Step 4.1d: Commit**

```bash
git add internal/gui/openpath.go internal/gui/openpath_test.go
git commit -m "feat(gui): OpenPath helper for OS file-manager (A4-a §2.4, Codex r1 P1.6)"
```

---

## Task 5: Backups HTTP routes — `internal/gui/backups.go`

**Memo refs:** §3.6, §10.3.

**Files:**

- Create: `internal/gui/backups.go`
- Create: `internal/gui/backups_test.go`
- Modify: `internal/gui/server.go` (wire `registerBackupsRoutes(s)`)

### Step 5.1 — Routes that wrap `BackupsList` and `BackupsCleanPreview`

- [ ] **Step 5.1a: Write `internal/gui/backups.go`**

```go
// internal/gui/backups.go
package gui

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"mcp-local-hub/internal/api"
)

// backupsAPI is the narrow surface used by /api/backups handlers.
type backupsAPI interface {
	List() ([]api.BackupInfo, error)
	CleanPreview(keepN int) ([]string, error)
}

type realBackupsAPI struct{}

func (realBackupsAPI) List() ([]api.BackupInfo, error)         { return api.NewAPI().BackupsList() }
func (realBackupsAPI) CleanPreview(n int) ([]string, error)    { return api.NewAPI().BackupsCleanPreview(n) }

// backupDTO is the JSON shape of one entry in GET /api/backups.
// ModTime is serialized as RFC3339 for predictable wire format.
type backupDTO struct {
	Client   string `json:"client"`
	Path     string `json:"path"`
	Kind     string `json:"kind"` // "original" | "timestamped"
	ModTime  string `json:"mod_time"`
	SizeByte int64  `json:"size_byte"`
}

func registerBackupsRoutes(s *Server) {
	s.mux.HandleFunc("/api/backups", s.requireSameOrigin(s.backupsListHandler))
	s.mux.HandleFunc("/api/backups/clean-preview", s.requireSameOrigin(s.backupsCleanPreviewHandler))
}

func (s *Server) backupsListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rows, err := s.backups.List()
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "BACKUPS_LIST_FAILED")
		return
	}
	dtos := make([]backupDTO, 0, len(rows))
	for _, b := range rows {
		dtos = append(dtos, backupDTO{
			Client:   b.Client,
			Path:     b.Path,
			Kind:     b.Kind,
			ModTime:  b.ModTime.Format(time.RFC3339),
			SizeByte: b.SizeByte,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": dtos})
}

func (s *Server) backupsCleanPreviewHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query().Get("keep_n")
	if q == "" {
		writeAPIError(w, fmt.Errorf("missing keep_n"), http.StatusBadRequest, "BACKUPS_PREVIEW_BAD_PARAM")
		return
	}
	n, err := strconv.Atoi(q)
	if err != nil || n < 0 {
		writeAPIError(w, fmt.Errorf("keep_n must be a non-negative integer"), http.StatusBadRequest, "BACKUPS_PREVIEW_BAD_PARAM")
		return
	}
	paths, err := s.backups.CleanPreview(n)
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "BACKUPS_PREVIEW_FAILED")
		return
	}
	if paths == nil {
		paths = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"would_remove": paths})
}
```

- [ ] **Step 5.1b: Wire in `internal/gui/server.go`**

In the `Server` struct, alongside `settings settingsAPI`, add:

```go
backups  backupsAPI
```

In `NewServer`, after `s.settings = realSettingsAPI{}`, add:

```go
s.backups = realBackupsAPI{}
```

After `registerSettingsRoutes(s)`, add:

```go
registerBackupsRoutes(s)
```

- [ ] **Step 5.1c: Write `internal/gui/backups_test.go`**

```go
package gui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

type fakeBackups struct {
	list       []api.BackupInfo
	listErr    error
	preview    []string
	previewErr error
	previewN   int
}

func (f *fakeBackups) List() ([]api.BackupInfo, error) { return f.list, f.listErr }
func (f *fakeBackups) CleanPreview(n int) ([]string, error) {
	f.previewN = n
	return f.preview, f.previewErr
}

func newBackupsTestServer(t *testing.T) (*Server, *fakeBackups) {
	t.Helper()
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	fb := &fakeBackups{}
	s.backups = fb
	return s, fb
}

func TestBackups_GET_List(t *testing.T) {
	s, fb := newBackupsTestServer(t)
	fb.list = []api.BackupInfo{
		{Client: "claude-code", Path: "/x.bak", Kind: "timestamped",
			ModTime: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC), SizeByte: 1234},
	}
	req := httptest.NewRequest("GET", "/api/backups", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Backups []map[string]any `json:"backups"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Backups) != 1 {
		t.Fatalf("expected 1, got %d", len(resp.Backups))
	}
	row := resp.Backups[0]
	if row["client"] != "claude-code" {
		t.Errorf("client mismatch: %v", row["client"])
	}
	if row["mod_time"] != "2026-04-01T12:00:00Z" {
		t.Errorf("mod_time RFC3339 mismatch: %v", row["mod_time"])
	}
}

func TestBackups_GET_CleanPreview(t *testing.T) {
	s, fb := newBackupsTestServer(t)
	fb.preview = []string{"/old.bak", "/older.bak"}
	req := httptest.NewRequest("GET", "/api/backups/clean-preview?keep_n=3", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	if fb.previewN != 3 {
		t.Errorf("expected keep_n=3 forwarded, got %d", fb.previewN)
	}
	var resp struct {
		WouldRemove []string `json:"would_remove"`
	}
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.WouldRemove) != 2 {
		t.Errorf("expected 2 paths, got %v", resp.WouldRemove)
	}
}

func TestBackups_GET_CleanPreview_MissingParam_400(t *testing.T) {
	s, _ := newBackupsTestServer(t)
	req := httptest.NewRequest("GET", "/api/backups/clean-preview", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestBackups_GET_CleanPreview_NegativeKeepN_400(t *testing.T) {
	s, _ := newBackupsTestServer(t)
	req := httptest.NewRequest("GET", "/api/backups/clean-preview?keep_n=-1", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestBackups_GET_CleanPreview_NilPathsEmittedAsEmptyArray(t *testing.T) {
	s, fb := newBackupsTestServer(t)
	fb.preview = nil
	req := httptest.NewRequest("GET", "/api/backups/clean-preview?keep_n=99", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), `"would_remove":[]`) {
		t.Errorf("expected empty array, got %s", rr.Body.String())
	}
}

func TestBackups_AllRoutes_RejectCrossOrigin(t *testing.T) {
	// Codex r2 P2.2: read-only routes also wrapped with requireSameOrigin.
	s, _ := newBackupsTestServer(t)
	cases := []struct{ method, path string }{
		{"GET", "/api/backups"},
		{"GET", "/api/backups/clean-preview?keep_n=5"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rr := httptest.NewRecorder()
		s.mux.ServeHTTP(rr, req)
		if rr.Code != 403 {
			t.Errorf("%s %s: expected 403, got %d", c.method, c.path, rr.Code)
		}
	}
}

func TestBackups_GET_List_PropagatesError(t *testing.T) {
	s, fb := newBackupsTestServer(t)
	fb.listErr = errors.New("disk full")
	req := httptest.NewRequest("GET", "/api/backups", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != 500 {
		t.Fatalf("expected 500, got %d", rr.Code)
	}
}
```

- [ ] **Step 5.1d: Run — expect PASS**

```bash
go test ./internal/gui/... -count=1 -run TestBackups -v 2>&1 | tail -30
```

- [ ] **Step 5.1e: Commit**

```bash
git add internal/gui/backups.go internal/gui/backups_test.go internal/gui/server.go
git commit -m "feat(gui): /api/backups + /api/backups/clean-preview routes (A4-a §3.6, §10.3)"
```

---

## Task 6: Frontend hook + types — `useSettingsSnapshot`

**Memo refs:** §8.2.

**Files:**

- Create: `internal/gui/frontend/src/lib/settings-types.ts`
- Create: `internal/gui/frontend/src/lib/settings-api.ts`
- Create: `internal/gui/frontend/src/lib/use-settings-snapshot.ts`
- Create: `internal/gui/frontend/src/lib/use-settings-snapshot.test.ts`

### Step 6.1 — Types + API client + hook

- [ ] **Step 6.1a: Write `internal/gui/frontend/src/lib/settings-types.ts`**

```ts
// Discriminated union per memo §8.2 (Codex r1 P2.2 + r2 P1.2). Action
// entries omit `value` and `default` to match the wire shape from
// internal/gui/settings.go::settingDTO.MarshalJSON.

export type Section = "appearance" | "gui_server" | "daemons" | "backups" | "advanced";

type BaseSettingDTO = {
  key: string;
  section: Section;
  deferred: boolean;
  help: string;
};

export type ConfigSettingDTO = BaseSettingDTO & {
  type: "enum" | "bool" | "int" | "string" | "path";
  default: string;
  value: string;
  enum?: string[];
  min?: number;
  max?: number;
  pattern?: string;
  optional?: boolean;
};

export type ActionSettingDTO = BaseSettingDTO & {
  type: "action";
};

export type SettingDTO = ConfigSettingDTO | ActionSettingDTO;

export type SettingsEnvelope = {
  settings: SettingDTO[];
  actual_port: number;
};

export type APIError = { code?: string; message: string };

export type SettingsSnapshotState =
  | { status: "loading"; data: null; error: null }
  | { status: "ok"; data: SettingsEnvelope; error: null }
  | { status: "error"; data: null; error: APIError | Error };

export type SettingsSnapshot = SettingsSnapshotState & {
  refresh: () => Promise<void>;
};

export function isAction(s: SettingDTO): s is ActionSettingDTO {
  return s.type === "action";
}

export function isConfig(s: SettingDTO): s is ConfigSettingDTO {
  return s.type !== "action";
}

export type BackupInfo = {
  client: string;
  path: string;
  kind: "original" | "timestamped";
  mod_time: string;
  size_byte: number;
};
```

- [ ] **Step 6.1b: Write `internal/gui/frontend/src/lib/settings-api.ts`**

```ts
import type { SettingsEnvelope, BackupInfo } from "./settings-types";

async function jsonOrThrow(res: Response): Promise<any> {
  const ct = res.headers.get("content-type") || "";
  let body: any = null;
  if (ct.includes("application/json")) {
    try {
      body = await res.json();
    } catch { /* fall through */ }
  }
  if (!res.ok) {
    const msg = body?.error || body?.reason || res.statusText || `HTTP ${res.status}`;
    const err: any = new Error(String(msg));
    err.status = res.status;
    err.body = body;
    throw err;
  }
  return body;
}

export async function getSettings(): Promise<SettingsEnvelope> {
  const res = await fetch("/api/settings", { credentials: "same-origin" });
  return await jsonOrThrow(res);
}

export async function putSetting(key: string, value: string): Promise<void> {
  const res = await fetch(`/api/settings/${encodeURIComponent(key)}`, {
    method: "PUT",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ value }),
  });
  await jsonOrThrow(res);
}

export async function postAction(key: string): Promise<any> {
  const res = await fetch(`/api/settings/${encodeURIComponent(key)}`, {
    method: "POST",
    credentials: "same-origin",
  });
  return await jsonOrThrow(res);
}

export async function getBackups(): Promise<BackupInfo[]> {
  const res = await fetch("/api/backups", { credentials: "same-origin" });
  const body = await jsonOrThrow(res);
  return body.backups as BackupInfo[];
}

export async function getBackupsCleanPreview(keepN: number): Promise<string[]> {
  const res = await fetch(`/api/backups/clean-preview?keep_n=${keepN}`, {
    credentials: "same-origin",
  });
  const body = await jsonOrThrow(res);
  return (body.would_remove ?? []) as string[];
}
```

- [ ] **Step 6.1c: Write `internal/gui/frontend/src/lib/use-settings-snapshot.ts`**

```ts
import { useEffect, useState, useCallback, useRef } from "preact/hooks";
import { getSettings } from "./settings-api";
import type { SettingsSnapshot, SettingsSnapshotState } from "./settings-types";

export function useSettingsSnapshot(): SettingsSnapshot {
  const [state, setState] = useState<SettingsSnapshotState>({
    status: "loading",
    data: null,
    error: null,
  });
  const generation = useRef(0);

  const refresh = useCallback(async () => {
    const myGen = ++generation.current;
    // Stale-while-revalidate: keep previous data if we already have ok.
    setState((prev) => (prev.status === "ok" ? prev : { status: "loading", data: null, error: null }));
    try {
      const data = await getSettings();
      if (myGen !== generation.current) return;
      setState({ status: "ok", data, error: null });
    } catch (e) {
      if (myGen !== generation.current) return;
      setState({ status: "error", data: null, error: e as Error });
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  return { ...state, refresh };
}
```

- [ ] **Step 6.1d: Write `internal/gui/frontend/src/lib/use-settings-snapshot.test.ts`**

```ts
import { describe, expect, it, vi, beforeEach } from "vitest";
import { renderHook, act, waitFor } from "@testing-library/preact";
import { useSettingsSnapshot } from "./use-settings-snapshot";
import * as api from "./settings-api";
import type { SettingsEnvelope } from "./settings-types";

const goodEnvelope: SettingsEnvelope = {
  actual_port: 9125,
  settings: [
    { key: "appearance.theme", section: "appearance", type: "enum",
      default: "system", value: "system", enum: ["light", "dark", "system"],
      deferred: false, help: "" },
    { key: "advanced.open_app_data_folder", section: "advanced", type: "action",
      deferred: false, help: "" },
  ],
};

describe("useSettingsSnapshot", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("loads on mount", async () => {
    vi.spyOn(api, "getSettings").mockResolvedValue(goodEnvelope);
    const { result } = renderHook(() => useSettingsSnapshot());
    expect(result.current.status).toBe("loading");
    await waitFor(() => expect(result.current.status).toBe("ok"));
    expect(result.current.data?.actual_port).toBe(9125);
    expect(result.current.data?.settings).toHaveLength(2);
  });

  it("transitions ok → ok via refresh, preserving previous data on stale-while-revalidate", async () => {
    const spy = vi.spyOn(api, "getSettings").mockResolvedValue(goodEnvelope);
    const { result } = renderHook(() => useSettingsSnapshot());
    await waitFor(() => expect(result.current.status).toBe("ok"));
    await act(async () => { await result.current.refresh(); });
    expect(spy).toHaveBeenCalledTimes(2);
    expect(result.current.status).toBe("ok");
  });

  it("transitions to error on fetch failure", async () => {
    vi.spyOn(api, "getSettings").mockRejectedValue(new Error("network"));
    const { result } = renderHook(() => useSettingsSnapshot());
    await waitFor(() => expect(result.current.status).toBe("error"));
    expect((result.current.error as Error).message).toBe("network");
  });

  it("retains discriminated-union shape (action entries have no value/default)", async () => {
    vi.spyOn(api, "getSettings").mockResolvedValue(goodEnvelope);
    const { result } = renderHook(() => useSettingsSnapshot());
    await waitFor(() => expect(result.current.status).toBe("ok"));
    const action = result.current.data!.settings.find((s) => s.key === "advanced.open_app_data_folder")!;
    expect(action.type).toBe("action");
    expect("value" in action).toBe(false);
    expect("default" in action).toBe(false);
  });
});
```

- [ ] **Step 6.1e: Run vitest — expect PASS**

```bash
cd internal/gui/frontend && npm run test -- --run --reporter=basic 2>&1 | tail -20 && cd ../../..
```

- [ ] **Step 6.1f: Commit**

```bash
git add internal/gui/frontend/src/lib/settings-types.ts internal/gui/frontend/src/lib/settings-api.ts internal/gui/frontend/src/lib/use-settings-snapshot.ts internal/gui/frontend/src/lib/use-settings-snapshot.test.ts
git commit -m "feat(gui/frontend): useSettingsSnapshot hook + types (discriminated union per Codex r2 P1.2) (A4-a §8.2)"
```

---

## Task 7: `FieldRenderer` — registry-driven control picker

**Memo refs:** §8.3.

**Files:**

- Create: `internal/gui/frontend/src/components/settings/FieldRenderer.tsx`
- Create: `internal/gui/frontend/src/components/settings/FieldRenderer.test.tsx`

### Step 7.1 — Generic control picker

- [ ] **Step 7.1a: Write `internal/gui/frontend/src/components/settings/FieldRenderer.tsx`**

```tsx
import type { ConfigSettingDTO } from "../../lib/settings-types";

export type FieldRendererProps = {
  def: ConfigSettingDTO;
  value: string;
  onChange: (next: string) => void;
  disabled?: boolean;
  error?: string;
};

// FieldRenderer maps registry def types to native HTML controls.
// Memo §8.3.
export function FieldRenderer({ def, value, onChange, disabled, error }: FieldRendererProps): preact.JSX.Element {
  const ariaProps = error
    ? { "aria-invalid": true as const, "aria-describedby": `${def.key}-error` }
    : {};
  let control: preact.JSX.Element;
  switch (def.type) {
    case "enum":
      control = (
        <select
          id={def.key}
          value={value}
          disabled={disabled}
          onChange={(e) => onChange((e.target as HTMLSelectElement).value)}
          {...ariaProps}
        >
          {(def.enum ?? []).map((opt) => (
            <option key={opt} value={opt}>{opt}</option>
          ))}
        </select>
      );
      break;
    case "bool":
      control = (
        <input
          id={def.key}
          type="checkbox"
          checked={value === "true"}
          disabled={disabled}
          onChange={(e) => onChange((e.target as HTMLInputElement).checked ? "true" : "false")}
          {...ariaProps}
        />
      );
      break;
    case "int":
      control = (
        <input
          id={def.key}
          type="number"
          value={value}
          disabled={disabled}
          min={def.min}
          max={def.max}
          onInput={(e) => onChange((e.target as HTMLInputElement).value)}
          {...ariaProps}
        />
      );
      break;
    case "string":
    case "path":
      control = (
        <input
          id={def.key}
          type="text"
          value={value}
          disabled={disabled}
          onInput={(e) => onChange((e.target as HTMLInputElement).value)}
          {...ariaProps}
        />
      );
      break;
  }
  return (
    <div class={`settings-field${error ? " has-error" : ""}${disabled ? " disabled" : ""}`}>
      <label for={def.key} class="settings-field-label">
        {labelFromKey(def.key)}
        {disabled && def.deferred ? <span class="deferred-badge"> (coming in A4-b)</span> : null}
      </label>
      {control}
      {def.help ? <small class="settings-field-help">{def.help}</small> : null}
      {error ? <small id={`${def.key}-error`} class="settings-field-error" role="alert">{error}</small> : null}
    </div>
  );
}

function labelFromKey(key: string): string {
  // "appearance.theme" → "theme"; "gui_server.browser_on_launch" → "browser on launch"
  const last = key.split(".").pop() || key;
  return last.replace(/_/g, " ");
}
```

- [ ] **Step 7.1b: Write `internal/gui/frontend/src/components/settings/FieldRenderer.test.tsx`**

```tsx
import { describe, expect, it, vi } from "vitest";
import { render, fireEvent } from "@testing-library/preact";
import { FieldRenderer } from "./FieldRenderer";
import type { ConfigSettingDTO } from "../../lib/settings-types";

const enumDef: ConfigSettingDTO = {
  key: "appearance.theme", section: "appearance", type: "enum",
  default: "system", value: "system", enum: ["light", "dark", "system"],
  deferred: false, help: "Color theme.",
};
const boolDef: ConfigSettingDTO = {
  key: "gui_server.browser_on_launch", section: "gui_server", type: "bool",
  default: "true", value: "true", deferred: false, help: "",
};
const intDef: ConfigSettingDTO = {
  key: "gui_server.port", section: "gui_server", type: "int",
  default: "9125", value: "9125", min: 1024, max: 65535, deferred: false, help: "",
};
const pathDef: ConfigSettingDTO = {
  key: "appearance.default_home", section: "appearance", type: "path",
  default: "", value: "", optional: true, deferred: false, help: "",
};

describe("FieldRenderer", () => {
  it("renders <select> for enum with all options", () => {
    const onChange = vi.fn();
    const { container } = render(<FieldRenderer def={enumDef} value="system" onChange={onChange} />);
    const select = container.querySelector("select")!;
    expect(select).toBeTruthy();
    expect(select.querySelectorAll("option")).toHaveLength(3);
  });

  it("enum onChange fires with selected option value", () => {
    const onChange = vi.fn();
    const { container } = render(<FieldRenderer def={enumDef} value="system" onChange={onChange} />);
    const select = container.querySelector("select")! as HTMLSelectElement;
    fireEvent.change(select, { target: { value: "dark" } });
    expect(onChange).toHaveBeenCalledWith("dark");
  });

  it("bool checkbox checked iff value === 'true'", () => {
    const { container } = render(<FieldRenderer def={boolDef} value="true" onChange={() => {}} />);
    expect((container.querySelector("input[type=checkbox]") as HTMLInputElement).checked).toBe(true);
    const { container: c2 } = render(<FieldRenderer def={boolDef} value="false" onChange={() => {}} />);
    expect((c2.querySelector("input[type=checkbox]") as HTMLInputElement).checked).toBe(false);
  });

  it("bool onChange emits 'true' / 'false' (string)", () => {
    const onChange = vi.fn();
    const { container } = render(<FieldRenderer def={boolDef} value="false" onChange={onChange} />);
    const cb = container.querySelector("input[type=checkbox]")! as HTMLInputElement;
    fireEvent.click(cb);
    expect(onChange).toHaveBeenLastCalledWith("true");
  });

  it("int control respects min/max attributes from def", () => {
    const { container } = render(<FieldRenderer def={intDef} value="9125" onChange={() => {}} />);
    const input = container.querySelector("input[type=number]")! as HTMLInputElement;
    expect(input.min).toBe("1024");
    expect(input.max).toBe("65535");
  });

  it("path renders text input", () => {
    const { container } = render(<FieldRenderer def={pathDef} value="" onChange={() => {}} />);
    expect(container.querySelector("input[type=text]")).toBeTruthy();
  });

  it("disabled propagates to control + shows '(coming in A4-b)' for deferred", () => {
    const def = { ...enumDef, deferred: true };
    const { container, getByText } = render(<FieldRenderer def={def} value="system" onChange={() => {}} disabled />);
    expect((container.querySelector("select") as HTMLSelectElement).disabled).toBe(true);
    expect(getByText(/coming in A4-b/)).toBeTruthy();
  });

  it("inline error renders with role=alert and aria-describedby", () => {
    const { container } = render(<FieldRenderer def={enumDef} value="system" onChange={() => {}} error="bad value" />);
    const select = container.querySelector("select") as HTMLSelectElement;
    expect(select.getAttribute("aria-invalid")).toBe("true");
    expect(select.getAttribute("aria-describedby")).toBe("appearance.theme-error");
    const err = container.querySelector("[role=alert]");
    expect(err?.textContent).toBe("bad value");
  });
});
```

- [ ] **Step 7.1c: Run vitest — expect PASS**

```bash
cd internal/gui/frontend && npm run test -- --run FieldRenderer 2>&1 | tail -15 && cd ../../..
```

- [ ] **Step 7.1d: Commit**

```bash
git add internal/gui/frontend/src/components/settings/FieldRenderer.tsx internal/gui/frontend/src/components/settings/FieldRenderer.test.tsx
git commit -m "feat(gui/frontend): FieldRenderer registry-driven control picker (A4-a §8.3)"
```

---

## Task 8: Settings shell — `Settings.tsx` + `SectionNav` + sidebar link + dirty-guard

**Memo refs:** §8.1, §8.4, §8.5, §10.6.

**Files:**

- Create: `internal/gui/frontend/src/screens/Settings.tsx`
- Create: `internal/gui/frontend/src/screens/Settings.test.tsx`
- Create: `internal/gui/frontend/src/components/settings/SectionNav.tsx`
- Create: `internal/gui/frontend/src/components/settings/SectionNav.test.tsx`
- Modify: `internal/gui/frontend/src/app.tsx` — add 7th sidebar link + `case "settings"` + `settingsDirty`

### Step 8.1 — `SectionNav` (sticky secondary)

- [ ] **Step 8.1a: Write `internal/gui/frontend/src/components/settings/SectionNav.tsx`**

```tsx
import type { Section } from "../../lib/settings-types";

const SECTION_ORDER: { id: Section; label: string }[] = [
  { id: "appearance", label: "Appearance" },
  { id: "gui_server", label: "GUI server" },
  { id: "daemons", label: "Daemons" },
  { id: "backups", label: "Backups" },
  { id: "advanced", label: "Advanced" },
];

export type SectionNavProps = {
  active: Section | null;
};

// SectionNav is the sticky, secondary in-screen nav. Each link uses
// `#/settings?section=<id>` which the existing useRouter parses as
// `route.query = "section=<id>"` (memo §8.5, Codex r1 P1.1).
export function SectionNav({ active }: SectionNavProps): preact.JSX.Element {
  return (
    <nav class="settings-section-nav" aria-label="Settings sections">
      {SECTION_ORDER.map((s) => (
        <a
          key={s.id}
          href={`#/settings?section=${s.id}`}
          class={active === s.id ? "active" : ""}
          aria-current={active === s.id ? "true" : undefined}
        >
          {s.label}
        </a>
      ))}
    </nav>
  );
}
```

- [ ] **Step 8.1b: Write `internal/gui/frontend/src/components/settings/SectionNav.test.tsx`**

```tsx
import { describe, expect, it } from "vitest";
import { render } from "@testing-library/preact";
import { SectionNav } from "./SectionNav";

describe("SectionNav", () => {
  it("renders 5 section links in order", () => {
    const { container } = render(<SectionNav active={null} />);
    const links = container.querySelectorAll("a");
    expect(links).toHaveLength(5);
    expect(links[0].textContent).toBe("Appearance");
    expect(links[4].textContent).toBe("Advanced");
  });

  it("uses query-string deep-link syntax (Codex r1 P1.1)", () => {
    const { container } = render(<SectionNav active={null} />);
    const links = Array.from(container.querySelectorAll("a"));
    for (const a of links) {
      expect(a.getAttribute("href")).toMatch(/^#\/settings\?section=[a-z_]+$/);
    }
  });

  it("highlights active section with class + aria-current", () => {
    const { container } = render(<SectionNav active="backups" />);
    const link = container.querySelector('a[href="#/settings?section=backups"]') as HTMLAnchorElement;
    expect(link.className).toContain("active");
    expect(link.getAttribute("aria-current")).toBe("true");
  });
});
```

### Step 8.2 — `Settings.tsx` shell

- [ ] **Step 8.2a: Write `internal/gui/frontend/src/screens/Settings.tsx`**

```tsx
import { useState, useEffect } from "preact/hooks";
import type { RouterState } from "../hooks/useRouter";
import type { Section } from "../lib/settings-types";
import { useSettingsSnapshot } from "../lib/use-settings-snapshot";
import { SectionNav } from "../components/settings/SectionNav";
import { SectionAppearance } from "../components/settings/SectionAppearance";
import { SectionGuiServer } from "../components/settings/SectionGuiServer";
import { SectionDaemons } from "../components/settings/SectionDaemons";
import { SectionBackups } from "../components/settings/SectionBackups";
import { SectionAdvanced } from "../components/settings/SectionAdvanced";

export type SettingsScreenProps = {
  route: RouterState;
  onDirtyChange: (b: boolean) => void;
};

const SECTION_IDS: Section[] = ["appearance", "gui_server", "daemons", "backups", "advanced"];

// Codex r1 P3.1 — destructure both props.
export function SettingsScreen({ route, onDirtyChange }: SettingsScreenProps): preact.JSX.Element {
  const snapshot = useSettingsSnapshot();
  const [appearanceDirty, setAppearanceDirty] = useState(false);
  const [guiServerDirty, setGuiServerDirty] = useState(false);
  const [backupsDirty, setBackupsDirty] = useState(false);
  const anyDirty = appearanceDirty || guiServerDirty || backupsDirty;

  useEffect(() => {
    onDirtyChange(anyDirty);
  }, [anyDirty, onDirtyChange]);

  const [activeSection, setActiveSection] = useState<Section | null>(null);

  // Scroll-spy via IntersectionObserver — flag the deepest in-viewport
  // section. This is registry-driven (no hardcoded selectors per section).
  useEffect(() => {
    const sections = SECTION_IDS
      .map((id) => document.querySelector<HTMLElement>(`section[data-section="${id}"]`))
      .filter((el): el is HTMLElement => el !== null);
    if (sections.length === 0) return;
    const observer = new IntersectionObserver(
      (entries) => {
        const visible = entries
          .filter((e) => e.isIntersecting)
          .sort((a, b) => b.intersectionRatio - a.intersectionRatio);
        if (visible.length > 0) {
          const id = visible[0].target.getAttribute("data-section") as Section | null;
          if (id) setActiveSection(id);
        }
      },
      { rootMargin: "-10% 0px -70% 0px", threshold: [0.1, 0.5, 0.9] },
    );
    for (const s of sections) observer.observe(s);
    return () => observer.disconnect();
  }, [snapshot.status]);

  // Deep-link on mount and on hash change. Memo §8.5 (Codex r1 P1.1).
  useEffect(() => {
    const params = new URLSearchParams(route.query ?? "");
    const target = params.get("section");
    if (target && SECTION_IDS.includes(target as Section)) {
      // Wait one tick so sections have mounted + measured.
      const id = setTimeout(() => {
        const el = document.querySelector<HTMLElement>(`section[data-section="${target}"]`);
        el?.scrollIntoView({ behavior: "smooth", block: "start" });
      }, 0);
      return () => clearTimeout(id);
    }
  }, [route.query, snapshot.status]);

  if (snapshot.status === "loading") {
    return (
      <div class="settings-screen loading">
        <h1>Settings</h1>
        <p>Loading…</p>
      </div>
    );
  }
  if (snapshot.status === "error") {
    return (
      <div class="settings-screen error">
        <h1>Settings</h1>
        <p class="error-banner">Could not load settings: {(snapshot.error as Error).message}</p>
        <button type="button" onClick={() => void snapshot.refresh()}>Retry</button>
      </div>
    );
  }

  return (
    <div class="settings-screen settings-layout">
      <SectionNav active={activeSection} />
      <div class="settings-body">
        <h1>Settings</h1>
        <SectionAppearance snapshot={snapshot} onDirtyChange={setAppearanceDirty} />
        <SectionGuiServer  snapshot={snapshot} onDirtyChange={setGuiServerDirty}  />
        <SectionDaemons    snapshot={snapshot} />
        <SectionBackups    snapshot={snapshot} onDirtyChange={setBackupsDirty}    />
        <SectionAdvanced   snapshot={snapshot} />
      </div>
    </div>
  );
}
```

- [ ] **Step 8.2b: Write `internal/gui/frontend/src/screens/Settings.test.tsx`**

```tsx
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, waitFor } from "@testing-library/preact";
import { SettingsScreen } from "./Settings";
import * as api from "../lib/settings-api";
import type { SettingsEnvelope } from "../lib/settings-types";
import type { RouterState } from "../hooks/useRouter";

const fakeEnv: SettingsEnvelope = {
  actual_port: 9125,
  settings: [
    { key: "appearance.theme", section: "appearance", type: "enum",
      default: "system", value: "system", enum: ["light", "dark", "system"], deferred: false, help: "" },
    { key: "appearance.density", section: "appearance", type: "enum",
      default: "comfortable", value: "comfortable", enum: ["compact","comfortable","spacious"], deferred: false, help: "" },
    { key: "appearance.shell", section: "appearance", type: "enum",
      default: "pwsh", value: "pwsh", enum: ["pwsh","cmd","bash","zsh","git-bash"], deferred: false, help: "" },
    { key: "appearance.default_home", section: "appearance", type: "path",
      default: "", value: "", optional: true, deferred: false, help: "" },
    { key: "gui_server.browser_on_launch", section: "gui_server", type: "bool",
      default: "true", value: "true", deferred: false, help: "" },
    { key: "gui_server.port", section: "gui_server", type: "int",
      default: "9125", value: "9125", min: 1024, max: 65535, deferred: false, help: "" },
    { key: "gui_server.tray", section: "gui_server", type: "bool",
      default: "true", value: "true", deferred: true, help: "" },
    { key: "daemons.weekly_schedule", section: "daemons", type: "string",
      default: "weekly Sun 03:00", value: "weekly Sun 03:00", deferred: true, help: "" },
    { key: "daemons.retry_policy", section: "daemons", type: "enum",
      default: "exponential", value: "exponential", enum: ["none","linear","exponential"], deferred: true, help: "" },
    { key: "backups.keep_n", section: "backups", type: "int",
      default: "5", value: "5", min: 0, max: 50, deferred: false, help: "" },
    { key: "backups.clean_now", section: "backups", type: "action", deferred: true, help: "" },
    { key: "advanced.open_app_data_folder", section: "advanced", type: "action", deferred: false, help: "" },
    { key: "advanced.export_config_bundle", section: "advanced", type: "action", deferred: true, help: "" },
  ],
};

const stubRoute = (query: string): RouterState => ({ screen: "settings", query });

describe("SettingsScreen", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.spyOn(api, "getSettings").mockResolvedValue(fakeEnv);
    vi.spyOn(api, "getBackups").mockResolvedValue([]);
    vi.spyOn(api, "getBackupsCleanPreview").mockResolvedValue([]);
  });

  it("renders all 5 section <h2>s on success", async () => {
    const { findByText } = render(<SettingsScreen route={stubRoute("")} onDirtyChange={() => {}} />);
    expect(await findByText("Appearance")).toBeTruthy();
    expect(await findByText("GUI server")).toBeTruthy();
    expect(await findByText("Daemons")).toBeTruthy();
    expect(await findByText("Backups")).toBeTruthy();
    expect(await findByText("Advanced")).toBeTruthy();
  });

  it("renders Loading then Settings header", async () => {
    const { container, findByText } = render(<SettingsScreen route={stubRoute("")} onDirtyChange={() => {}} />);
    // Initial loading state may or may not render; final state must show Settings.
    await findByText("Settings");
    expect(container.querySelector("h1")?.textContent).toBe("Settings");
  });

  it("calls onDirtyChange(false) initially", async () => {
    const onDirty = vi.fn();
    render(<SettingsScreen route={stubRoute("")} onDirtyChange={onDirty} />);
    await waitFor(() => expect(onDirty).toHaveBeenCalled());
    expect(onDirty).toHaveBeenLastCalledWith(false);
  });

  it("error state renders Retry button", async () => {
    vi.spyOn(api, "getSettings").mockRejectedValue(new Error("boom"));
    const { findByText } = render(<SettingsScreen route={stubRoute("")} onDirtyChange={() => {}} />);
    expect(await findByText(/Could not load settings/)).toBeTruthy();
    expect(await findByText("Retry")).toBeTruthy();
  });
});
```

### Step 8.3 — Wire sidebar + dirty-guard in `app.tsx`

- [ ] **Step 8.3a: Modify `internal/gui/frontend/src/app.tsx`**

Replace the top of the file (imports + state) with the version below, and replace the body. Keep the file structure (single default export, existing patterns).

```tsx
import type { JSX } from "preact";
import { useState } from "preact/hooks";
import { useRouter, type RouterState } from "./hooks/useRouter";
import { useUnsavedChangesGuard } from "./hooks/useUnsavedChangesGuard";
import { AddServerScreen } from "./screens/AddServer";
import { DashboardScreen } from "./screens/Dashboard";
import { LogsScreen } from "./screens/Logs";
import { MigrationScreen } from "./screens/Migration";
import { SecretsScreen } from "./screens/Secrets";
import { ServersScreen } from "./screens/Servers";
import { SettingsScreen } from "./screens/Settings";

export function App() {
  const [addServerDirty, setAddServerDirty] = useState(false);
  const [settingsDirty, setSettingsDirty] = useState(false);
  const dirtyAny = addServerDirty || settingsDirty;

  // Codex r2 P1: discard signal for in-screen navigation. Section-local
  // edit state in useSectionSaveFlow / SectionBackups stays mounted across
  // intra-Settings hash changes. Bumping this counter on confirmed discard
  // forces SettingsScreen to remount via React `key` prop, resetting every
  // section's local draft state in one go. Memo §10.4.
  const [discardKey, setDiscardKey] = useState(0);

  const guard = (target: RouterState): boolean => {
    if (!dirtyAny) return true;
    if (target.screen === route.screen && target.query === route.query) return true;
    // eslint-disable-next-line no-alert
    const ok = window.confirm("Discard unsaved changes?");
    if (ok) {
      setAddServerDirty(false);
      setSettingsDirty(false);
      setDiscardKey((n) => n + 1);
    }
    return ok;
  };

  const route = useRouter("servers", guard);
  useUnsavedChangesGuard(dirtyAny);

  function guardClick(targetScreen: string): (e: MouseEvent) => void {
    return (e) => {
      if (!dirtyAny) return;
      // Only prompt if leaving a dirty-guarded screen for a different one.
      const onGuardedScreen =
        route.screen === "add-server" || route.screen === "edit-server" || route.screen === "settings";
      if (!onGuardedScreen) return;
      if (targetScreen === route.screen) return;
      // eslint-disable-next-line no-alert
      const ok = window.confirm("Discard unsaved changes?");
      if (!ok) {
        e.preventDefault();
      } else {
        setAddServerDirty(false);
        setSettingsDirty(false);
        setDiscardKey((n) => n + 1);
      }
    };
  }

  let body: JSX.Element;
  switch (route.screen) {
    case "servers":
      body = <ServersScreen />;
      break;
    case "migration":
      body = <MigrationScreen />;
      break;
    case "add-server":
      body = <AddServerScreen mode="create" route={route} onDirtyChange={setAddServerDirty} />;
      break;
    case "edit-server":
      body = <AddServerScreen mode="edit" route={route} onDirtyChange={setAddServerDirty} />;
      break;
    case "secrets":
      body = <SecretsScreen />;
      break;
    case "dashboard":
      body = <DashboardScreen />;
      break;
    case "logs":
      body = <LogsScreen />;
      break;
    case "settings":
      // `key={discardKey}` forces a full remount on confirmed discard so
      // every section's local draft state resets cleanly (Codex r2 P1).
      body = <SettingsScreen key={discardKey} route={route} onDirtyChange={setSettingsDirty} />;
      break;
    default:
      body = <p>Unknown screen: {route.screen}</p>;
  }

  return (
    <>
      <aside class="sidebar">
        <div class="brand">mcp-local-hub</div>
        <nav>
          <a href="#/servers"    class={route.screen === "servers"    ? "active" : ""} onClick={guardClick("servers")}>Servers</a>
          <a href="#/migration"  class={route.screen === "migration"  ? "active" : ""} onClick={guardClick("migration")}>Migration</a>
          <a href="#/add-server" class={route.screen === "add-server" ? "active" : ""} onClick={guardClick("add-server")}>Add server</a>
          <a href="#/secrets"    class={route.screen === "secrets"    ? "active" : ""} onClick={guardClick("secrets")}>Secrets</a>
          <a href="#/dashboard"  class={route.screen === "dashboard"  ? "active" : ""} onClick={guardClick("dashboard")}>Dashboard</a>
          <a href="#/logs"       class={route.screen === "logs"       ? "active" : ""} onClick={guardClick("logs")}>Logs</a>
          <a href="#/settings"   class={route.screen === "settings"   ? "active" : ""} onClick={guardClick("settings")}>Settings</a>
        </nav>
      </aside>
      <main id="screen-root">
        {body}
      </main>
    </>
  );
}
```

**Build order (Codex r1 P2.1):** execute strictly **8 → 9 → 10 → 11**. Task 8 ships the placeholder shim (`SectionPlaceholder.tsx`) so `Settings.tsx` compiles. Task 9 edits the shim to retain only `SectionBackups` + `SectionAdvanced`. Task 10 deletes the shim entirely and switches `Settings.tsx`'s last two imports to the real per-section files. Task 11 only adds CSS, E2E, and docs — it does NOT do component wiring.

- [ ] **Step 8.3b: For now, scaffold minimal placeholders** — create `internal/gui/frontend/src/components/settings/SectionPlaceholder.tsx` that Tasks 9+10 will overwrite with real implementations. This unblocks `Settings.tsx` build:

```tsx
// Placeholder so Settings.tsx compiles before Tasks 9+10 land. Tasks 9+10
// replace these with real per-section components.
import type { SettingsSnapshot } from "../../lib/settings-types";
type Props = { snapshot: SettingsSnapshot; onDirtyChange?: (b: boolean) => void };

export function SectionAppearance(_: Props)  { return <section data-section="appearance"><h2>Appearance</h2></section>; }
export function SectionGuiServer(_: Props)   { return <section data-section="gui_server"><h2>GUI server</h2></section>; }
export function SectionDaemons(_: Props)     { return <section data-section="daemons"><h2>Daemons</h2></section>; }
export function SectionBackups(_: Props)     { return <section data-section="backups"><h2>Backups</h2></section>; }
export function SectionAdvanced(_: Props)    { return <section data-section="advanced"><h2>Advanced</h2></section>; }
```

And in `Settings.tsx`, change the imports to:

```tsx
import { SectionAppearance, SectionGuiServer, SectionDaemons, SectionBackups, SectionAdvanced } from "../components/settings/SectionPlaceholder";
```

Tasks 9 + 10 split this file into per-section files and update the imports in `Settings.tsx` accordingly.

- [ ] **Step 8.3c: Run typecheck + tests — expect PASS**

```bash
cd internal/gui/frontend && npm run typecheck && npm run test -- --run Settings 2>&1 | tail -20 && cd ../../..
```

- [ ] **Step 8.3d: Commit**

```bash
git add internal/gui/frontend/src/screens/Settings.tsx internal/gui/frontend/src/screens/Settings.test.tsx internal/gui/frontend/src/components/settings/SectionNav.tsx internal/gui/frontend/src/components/settings/SectionNav.test.tsx internal/gui/frontend/src/components/settings/SectionPlaceholder.tsx internal/gui/frontend/src/app.tsx
git commit -m "feat(gui/frontend): Settings screen shell + sidebar link + dirty-guard wiring (A4-a §8.1, §8.4, §8.5, §10.6)"
```

---

## Task 9: SectionAppearance + SectionGuiServer + SectionDaemons

**Memo refs:** §3.5, §4.4, §9.1, §9.2, §9.3.

**Files:**

- Create: `internal/gui/frontend/src/components/settings/SectionAppearance.tsx` (+ test)
- Create: `internal/gui/frontend/src/components/settings/SectionGuiServer.tsx` (+ test)
- Create: `internal/gui/frontend/src/components/settings/SectionDaemons.tsx` (+ test)
- Modify: `internal/gui/frontend/src/screens/Settings.tsx` — switch imports from `SectionPlaceholder` to the per-section files
- Delete: `internal/gui/frontend/src/components/settings/SectionPlaceholder.tsx` (only the 3 sections it stubbed; SectionBackups/SectionAdvanced come in Task 10)

### Step 9.1 — Shared per-section save helper

Create a tiny shared helper to encode the per-section Save flow (memo §4.4 partial-save merge rule).

- [ ] **Step 9.1a: Write `internal/gui/frontend/src/components/settings/useSectionSaveFlow.ts`**

```ts
import { useState, useEffect, useMemo } from "preact/hooks";
import { putSetting } from "../../lib/settings-api";
import type { SettingsSnapshot, ConfigSettingDTO } from "../../lib/settings-types";

export type SaveOutcome = {
  // Map of dirty-key → outcome: success or error message.
  failures: Record<string, string>;
  successes: string[];
};

export function useSectionSaveFlow(
  snapshot: SettingsSnapshot,
  sectionKeys: string[],
  onDirtyChange: (b: boolean) => void,
) {
  // local edited values, keyed by registry key. Empty = clean.
  const [edits, setEdits] = useState<Record<string, string>>({});
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [banner, setBanner] = useState<{ kind: "ok" | "partial"; text: string } | null>(null);

  const persisted = useMemo(() => {
    const out: Record<string, string> = {};
    if (snapshot.status === "ok") {
      for (const k of sectionKeys) {
        const dto = snapshot.data.settings.find((s) => s.key === k) as ConfigSettingDTO | undefined;
        if (dto) out[k] = dto.value;
      }
    }
    return out;
  }, [snapshot, sectionKeys]);

  const dirty = Object.keys(edits).length > 0;
  useEffect(() => onDirtyChange(dirty), [dirty, onDirtyChange]);

  function effective(key: string): string {
    return edits[key] ?? persisted[key] ?? "";
  }

  function setLocal(key: string, value: string) {
    setEdits((prev) => {
      const next = { ...prev };
      // If matches persisted, drop from edits (clean).
      if ((persisted[key] ?? "") === value) {
        delete next[key];
      } else {
        next[key] = value;
      }
      return next;
    });
    // Clear that key's error on edit.
    setErrors((prev) => {
      if (!(key in prev)) return prev;
      const next = { ...prev };
      delete next[key];
      return next;
    });
  }

  function reset() {
    setEdits({});
    setErrors({});
    setBanner(null);
  }

  async function save(): Promise<void> {
    if (!dirty) return;
    setBusy(true);
    setBanner(null);
    const dirtyKeys = Object.keys(edits);
    const failures: Record<string, string> = {};
    const successes: string[] = [];
    // Sequential PUTs — deterministic ordering, avoids server-side write races.
    for (const k of dirtyKeys) {
      try {
        await putSetting(k, edits[k]);
        successes.push(k);
      } catch (e: any) {
        const reason = e?.body?.reason ?? e?.message ?? "save failed";
        failures[k] = String(reason);
      }
    }
    // Memo §4.4 merge rule: drop successes from edits + errors; keep failures dirty.
    // Codex r7 P2: errors map must clear successes BEFORE merging new failures —
    // otherwise a key that failed previously and now saved successfully would
    // keep its stale inline error message while becoming clean.
    setEdits((prev) => {
      const next = { ...prev };
      for (const k of successes) delete next[k];
      return next;
    });
    setErrors((prev) => {
      const next = { ...prev };
      for (const k of successes) delete next[k]; // clear stale errors on retry-success
      for (const [k, v] of Object.entries(failures)) next[k] = v;
      return next;
    });
    setBusy(false);
    if (Object.keys(failures).length === 0) {
      setBanner({ kind: "ok", text: "Saved." });
      setTimeout(() => setBanner(null), 2000);
    } else {
      setBanner({
        kind: "partial",
        text: `Saved ${successes.length} of ${dirtyKeys.length} settings. Fix errors below and try again.`,
      });
    }
    // Refresh the snapshot AFTER the merge so successful keys re-anchor cleanly.
    await snapshot.refresh();
  }

  return { effective, setLocal, reset, save, dirty, busy, errors, banner };
}
```

### Step 9.2 — `SectionAppearance`

- [ ] **Step 9.2a: Write `internal/gui/frontend/src/components/settings/SectionAppearance.tsx`**

```tsx
import { FieldRenderer } from "./FieldRenderer";
import { useSectionSaveFlow } from "./useSectionSaveFlow";
import type { SettingsSnapshot, ConfigSettingDTO } from "../../lib/settings-types";

export type SectionAppearanceProps = {
  snapshot: SettingsSnapshot;
  onDirtyChange: (b: boolean) => void;
};

const SECTION_KEYS = [
  "appearance.theme",
  "appearance.density",
  "appearance.shell",
  "appearance.default_home",
];

export function SectionAppearance({ snapshot, onDirtyChange }: SectionAppearanceProps): preact.JSX.Element {
  const flow = useSectionSaveFlow(snapshot, SECTION_KEYS, onDirtyChange);
  if (snapshot.status !== "ok") return <section data-section="appearance"><h2>Appearance</h2></section>;
  const defs = SECTION_KEYS
    .map((k) => snapshot.data.settings.find((s) => s.key === k))
    .filter((s): s is ConfigSettingDTO => !!s && s.type !== "action");

  return (
    <section data-section="appearance" class="settings-section">
      <h2>Appearance</h2>
      <p class="settings-section-help">Visual appearance of the GUI.</p>
      {defs.map((d) => (
        <FieldRenderer
          key={d.key}
          def={d}
          value={flow.effective(d.key)}
          onChange={(v) => flow.setLocal(d.key, v)}
          error={flow.errors[d.key]}
        />
      ))}
      <SectionFooter flow={flow} />
    </section>
  );
}

export function SectionFooter({ flow }: { flow: ReturnType<typeof useSectionSaveFlow> }): preact.JSX.Element {
  return (
    <div class="settings-section-footer">
      {flow.banner ? <span class={`save-banner ${flow.banner.kind}`}>{flow.banner.text}</span> : null}
      <button type="button" disabled={!flow.dirty || flow.busy} onClick={() => void flow.save()}>
        {flow.busy ? "Saving…" : "Save"}
      </button>
      <button type="button" disabled={!flow.dirty || flow.busy} onClick={flow.reset}>Reset</button>
    </div>
  );
}
```

- [ ] **Step 9.2b: Write `internal/gui/frontend/src/components/settings/SectionAppearance.test.tsx`**

```tsx
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, fireEvent, waitFor } from "@testing-library/preact";
import { SectionAppearance } from "./SectionAppearance";
import * as api from "../../lib/settings-api";
import type { SettingsSnapshot, SettingsEnvelope } from "../../lib/settings-types";

const env: SettingsEnvelope = {
  actual_port: 9125,
  settings: [
    { key: "appearance.theme", section: "appearance", type: "enum",
      default: "system", value: "system", enum: ["light","dark","system"], deferred: false, help: "" },
    { key: "appearance.density", section: "appearance", type: "enum",
      default: "comfortable", value: "comfortable", enum: ["compact","comfortable","spacious"], deferred: false, help: "" },
    { key: "appearance.shell", section: "appearance", type: "enum",
      default: "pwsh", value: "pwsh", enum: ["pwsh","cmd","bash","zsh","git-bash"], deferred: false, help: "" },
    { key: "appearance.default_home", section: "appearance", type: "path",
      default: "", value: "", optional: true, deferred: false, help: "" },
  ],
};

function makeSnapshot(refresh = vi.fn(async () => {})): SettingsSnapshot {
  return { status: "ok", data: env, error: null, refresh };
}

describe("SectionAppearance", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("renders 4 fields in registry order", () => {
    const { container } = render(<SectionAppearance snapshot={makeSnapshot()} onDirtyChange={() => {}} />);
    expect(container.querySelectorAll(".settings-field")).toHaveLength(4);
  });

  it("editing theme dirties the section + Save enables", async () => {
    const onDirty = vi.fn();
    const { container } = render(<SectionAppearance snapshot={makeSnapshot()} onDirtyChange={onDirty} />);
    const select = container.querySelector("#appearance\\.theme") as HTMLSelectElement;
    fireEvent.change(select, { target: { value: "dark" } });
    await waitFor(() => expect(onDirty).toHaveBeenCalledWith(true));
    const saveBtn = Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Save")!;
    expect(saveBtn.disabled).toBe(false);
  });

  it("Reset reverts edits", async () => {
    const onDirty = vi.fn();
    const { container } = render(<SectionAppearance snapshot={makeSnapshot()} onDirtyChange={onDirty} />);
    const select = container.querySelector("#appearance\\.theme") as HTMLSelectElement;
    fireEvent.change(select, { target: { value: "dark" } });
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(true));
    const resetBtn = Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Reset")!;
    fireEvent.click(resetBtn);
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(false));
    expect(select.value).toBe("system");
  });

  it("Save calls putSetting for each dirty key + clears dirty on success", async () => {
    const putSpy = vi.spyOn(api, "putSetting").mockResolvedValue(undefined);
    const refresh = vi.fn(async () => {});
    const onDirty = vi.fn();
    const { container } = render(<SectionAppearance snapshot={makeSnapshot(refresh)} onDirtyChange={onDirty} />);
    const sel = container.querySelector("#appearance\\.theme") as HTMLSelectElement;
    fireEvent.change(sel, { target: { value: "dark" } });
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(true));
    const saveBtn = Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Save")!;
    fireEvent.click(saveBtn);
    await waitFor(() => expect(putSpy).toHaveBeenCalledWith("appearance.theme", "dark"));
    await waitFor(() => expect(refresh).toHaveBeenCalled());
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(false));
  });

  it("partial save: failed key stays dirty + error inline (Codex r1 P2.3)", async () => {
    vi.spyOn(api, "putSetting").mockImplementation(async (key) => {
      if (key === "appearance.density") {
        const err: any = new Error("invalid value");
        err.body = { reason: "not in enum" };
        throw err;
      }
    });
    const refresh = vi.fn(async () => {});
    const onDirty = vi.fn();
    const { container, findByText } = render(<SectionAppearance snapshot={makeSnapshot(refresh)} onDirtyChange={onDirty} />);
    fireEvent.change(container.querySelector("#appearance\\.theme") as HTMLSelectElement, { target: { value: "dark" } });
    fireEvent.change(container.querySelector("#appearance\\.density") as HTMLSelectElement, { target: { value: "compact" } });
    fireEvent.click(Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Save")!);
    expect(await findByText(/Saved 1 of 2 settings/)).toBeTruthy();
    // Density still dirty (failed) → onDirty(true) at end; theme cleaned.
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(true));
    expect((container.querySelector("[role=alert]") as HTMLElement).textContent).toMatch(/not in enum/);
  });

  it("retry-success: save() clears stale error WITHOUT intervening edit (Codex r7 P2 + r8 P2)", async () => {
    // Two dirty keys. First Save: BOTH fail (transient backend error).
    // Second Save WITHOUT any intervening edit: density's mock now
    // succeeds (attempt 2), shell's mock still fails. The test verifies
    // that save() itself clears errors[density] on success — without an
    // intervening setLocal() that would clear it via the edit path.
    let densityAttempt = 0;
    vi.spyOn(api, "putSetting").mockImplementation(async (key) => {
      if (key === "appearance.density") {
        densityAttempt++;
        if (densityAttempt === 1) {
          const err: any = new Error("invalid value");
          err.body = { reason: "transient" };
          throw err;
        }
        return; // attempt 2 succeeds
      }
      if (key === "appearance.shell") {
        const err: any = new Error("invalid value");
        err.body = { reason: "still bad" };
        throw err;
      }
    });
    const refresh = vi.fn(async () => {});
    const { container, findByText } = render(
      <SectionAppearance snapshot={makeSnapshot(refresh)} onDirtyChange={() => {}} />,
    );
    fireEvent.change(container.querySelector("#appearance\\.density") as HTMLSelectElement, { target: { value: "compact" } });
    fireEvent.change(container.querySelector("#appearance\\.shell") as HTMLSelectElement, { target: { value: "bash" } });
    // First Save: BOTH fail → 2 alerts.
    fireEvent.click(Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Save")!);
    expect(await findByText(/Saved 0 of 2 settings/)).toBeTruthy();
    await waitFor(() => expect(container.querySelectorAll("[role=alert]").length).toBe(2));
    // Second Save WITHOUT editing — density succeeds, shell still fails.
    fireEvent.click(Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Save")!);
    // After save: only ONE alert remains (shell). Density's stale alert MUST
    // be cleared by save() itself, not by setLocal-on-edit.
    await waitFor(() => expect(container.querySelectorAll("[role=alert]").length).toBe(1));
    // Density's field has no error binding any more.
    const densityField = container.querySelector("#appearance\\.density");
    expect(densityField?.getAttribute("aria-describedby")).toBeNull();
    // Shell still has the error binding.
    const shellField = container.querySelector("#appearance\\.shell");
    expect(shellField?.getAttribute("aria-describedby")).toBe("appearance.shell-error");
  });
});
```

### Step 9.3 — `SectionGuiServer` (with port pending-restart badge)

- [ ] **Step 9.3a: Write `internal/gui/frontend/src/components/settings/SectionGuiServer.tsx`**

```tsx
import { FieldRenderer } from "./FieldRenderer";
import { SectionFooter } from "./SectionAppearance";
import { useSectionSaveFlow } from "./useSectionSaveFlow";
import type { SettingsSnapshot, ConfigSettingDTO, SettingDTO } from "../../lib/settings-types";

export type SectionGuiServerProps = {
  snapshot: SettingsSnapshot;
  onDirtyChange: (b: boolean) => void;
};

const SECTION_KEYS = ["gui_server.browser_on_launch", "gui_server.port", "gui_server.tray"];
const EDITABLE_KEYS = ["gui_server.browser_on_launch", "gui_server.port"];

export function SectionGuiServer({ snapshot, onDirtyChange }: SectionGuiServerProps): preact.JSX.Element {
  const flow = useSectionSaveFlow(snapshot, EDITABLE_KEYS, onDirtyChange);
  if (snapshot.status !== "ok") return <section data-section="gui_server"><h2>GUI server</h2></section>;

  const portDef = snapshot.data.settings.find((s) => s.key === "gui_server.port") as ConfigSettingDTO;
  const persistedPort = Number(portDef.value);
  const actualPort = snapshot.data.actual_port;
  // Codex r3 P2.1 + r4 P2.1: badge anchored to PERSISTED port, NOT local draft.
  const showPortBadge = !Number.isNaN(persistedPort) && actualPort !== persistedPort;

  return (
    <section data-section="gui_server" class="settings-section">
      <h2>GUI server</h2>
      <p class="settings-section-help">How the GUI server runs.</p>
      {SECTION_KEYS.map((k) => {
        const def = snapshot.data.settings.find((s: SettingDTO) => s.key === k) as ConfigSettingDTO | undefined;
        if (!def) return null;
        const editable = EDITABLE_KEYS.includes(k);
        return (
          <div key={k} class="settings-field-row">
            <FieldRenderer
              def={def}
              value={editable ? flow.effective(k) : def.value}
              onChange={(v) => editable && flow.setLocal(k, v)}
              disabled={!editable || def.deferred}
              error={flow.errors[k]}
            />
            {k === "gui_server.port" && showPortBadge ? (
              <span class="settings-restart-badge" data-test-id="port-restart-badge" role="status">
                ⚠ Restart required — port {persistedPort} will take effect after restart
              </span>
            ) : null}
          </div>
        );
      })}
      <SectionFooter flow={flow} />
    </section>
  );
}
```

- [ ] **Step 9.3b: Write `internal/gui/frontend/src/components/settings/SectionGuiServer.test.tsx`**

```tsx
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, fireEvent, waitFor } from "@testing-library/preact";
import { SectionGuiServer } from "./SectionGuiServer";
import * as api from "../../lib/settings-api";
import type { SettingsSnapshot, SettingsEnvelope } from "../../lib/settings-types";

function envWithPort(value: string, actualPort: number): SettingsEnvelope {
  return {
    actual_port: actualPort,
    settings: [
      { key: "gui_server.browser_on_launch", section: "gui_server", type: "bool",
        default: "true", value: "true", deferred: false, help: "" },
      { key: "gui_server.port", section: "gui_server", type: "int",
        default: "9125", value, min: 1024, max: 65535, deferred: false, help: "" },
      { key: "gui_server.tray", section: "gui_server", type: "bool",
        default: "true", value: "true", deferred: true, help: "" },
    ],
  };
}

function snap(env: SettingsEnvelope, refresh = vi.fn(async () => {})): SettingsSnapshot {
  return { status: "ok", data: env, error: null, refresh };
}

describe("SectionGuiServer", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("renders all 3 fields with tray disabled (deferred)", () => {
    const { container, getByText } = render(<SectionGuiServer snapshot={snap(envWithPort("9125", 9125))} onDirtyChange={() => {}} />);
    const tray = container.querySelector("#gui_server\\.tray") as HTMLInputElement;
    expect(tray.disabled).toBe(true);
    expect(getByText(/coming in A4-b/)).toBeTruthy();
  });

  it("port-pending-restart badge HIDDEN when persisted == actual_port", () => {
    const { container } = render(<SectionGuiServer snapshot={snap(envWithPort("9125", 9125))} onDirtyChange={() => {}} />);
    expect(container.querySelector('[data-test-id="port-restart-badge"]')).toBeNull();
  });

  it("port-pending-restart badge VISIBLE when persisted != actual_port", () => {
    const { container } = render(<SectionGuiServer snapshot={snap(envWithPort("9200", 9125))} onDirtyChange={() => {}} />);
    const badge = container.querySelector('[data-test-id="port-restart-badge"]');
    expect(badge).toBeTruthy();
    expect(badge!.textContent).toMatch(/9200/);
  });

  it("Codex r4 P2.1: dirty draft does NOT flip badge", async () => {
    // Both persisted and actual are 9125 → no badge. Type a different
    // value into the field but DO NOT save. Badge must stay hidden.
    const onDirty = vi.fn();
    const { container } = render(
      <SectionGuiServer snapshot={snap(envWithPort("9125", 9125))} onDirtyChange={onDirty} />,
    );
    const portInput = container.querySelector("#gui_server\\.port") as HTMLInputElement;
    fireEvent.input(portInput, { target: { value: "9200" } });
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(true));
    // Badge must still be hidden — local draft is dirty, not persisted.
    expect(container.querySelector('[data-test-id="port-restart-badge"]')).toBeNull();
  });

  it("Codex r4 P2.1: badge appears AFTER Save", async () => {
    let env = envWithPort("9125", 9125);
    const refresh = vi.fn(async () => {
      // Simulate refresh: persisted now reflects the saved 9200 value.
      env = envWithPort("9200", 9125);
    });
    vi.spyOn(api, "putSetting").mockResolvedValue(undefined);
    const { container, rerender } = render(
      <SectionGuiServer snapshot={snap(env, refresh)} onDirtyChange={() => {}} />,
    );
    const portInput = container.querySelector("#gui_server\\.port") as HTMLInputElement;
    fireEvent.input(portInput, { target: { value: "9200" } });
    fireEvent.click(Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Save")!);
    await waitFor(() => expect(refresh).toHaveBeenCalled());
    // Re-render with the post-save snapshot.
    rerender(<SectionGuiServer snapshot={snap(env)} onDirtyChange={() => {}} />);
    await waitFor(() => expect(container.querySelector('[data-test-id="port-restart-badge"]')).toBeTruthy());
  });
});
```

### Step 9.4 — `SectionDaemons` (read-only, "Configured … (effective in A4-b)")

- [ ] **Step 9.4a: Write `internal/gui/frontend/src/components/settings/SectionDaemons.tsx`**

```tsx
import type { SettingsSnapshot, ConfigSettingDTO } from "../../lib/settings-types";

export type SectionDaemonsProps = {
  snapshot: SettingsSnapshot;
};

// Memo §9.3 (Codex r1 P1.7): the labels MUST distinguish *configured*
// from *currently active*. A user-written-via-CLI deferred value must
// not be mis-read as runtime state.
export function SectionDaemons({ snapshot }: SectionDaemonsProps): preact.JSX.Element {
  if (snapshot.status === "loading") {
    return <section data-section="daemons" class="settings-section"><h2>Daemons</h2><p>Loading…</p></section>;
  }
  if (snapshot.status === "error") {
    return (
      <section data-section="daemons" class="settings-section">
        <h2>Daemons</h2>
        <p class="error-banner">Schedule unavailable.</p>
      </section>
    );
  }
  const sched = snapshot.data.settings.find((s) => s.key === "daemons.weekly_schedule") as ConfigSettingDTO | undefined;
  const retry = snapshot.data.settings.find((s) => s.key === "daemons.retry_policy") as ConfigSettingDTO | undefined;
  return (
    <section data-section="daemons" class="settings-section settings-section-readonly">
      <h2>Daemons</h2>
      <p class="settings-section-help">Background daemon settings.</p>
      <div class="readonly-row">
        <span class="readonly-label">Configured schedule:</span>
        <span class="readonly-value">{sched?.value ?? sched?.default ?? ""}</span>
        <span class="readonly-suffix">(effective in A4-b)</span>
      </div>
      <div class="readonly-row">
        <span class="readonly-label">Configured retry policy:</span>
        <span class="readonly-value">{retry?.value ?? retry?.default ?? ""}</span>
        <span class="readonly-suffix">(effective in A4-b)</span>
      </div>
      <p class="deferred-affordance">edit coming in A4-b</p>
    </section>
  );
}
```

- [ ] **Step 9.4b: Write `internal/gui/frontend/src/components/settings/SectionDaemons.test.tsx`**

```tsx
import { describe, expect, it, vi } from "vitest";
import { render } from "@testing-library/preact";
import { SectionDaemons } from "./SectionDaemons";
import type { SettingsSnapshot, SettingsEnvelope } from "../../lib/settings-types";

const env: SettingsEnvelope = {
  actual_port: 9125,
  settings: [
    { key: "daemons.weekly_schedule", section: "daemons", type: "string",
      default: "weekly Sun 03:00", value: "weekly Sun 03:00", deferred: true, help: "" },
    { key: "daemons.retry_policy", section: "daemons", type: "enum",
      default: "exponential", value: "exponential", enum: ["none","linear","exponential"], deferred: true, help: "" },
  ],
};
const snap: SettingsSnapshot = { status: "ok", data: env, error: null, refresh: vi.fn(async () => {}) };

describe("SectionDaemons", () => {
  it("renders 'Configured schedule' label (NOT 'Current schedule' — Codex r1 P1.7)", () => {
    const { getByText, queryByText } = render(<SectionDaemons snapshot={snap} />);
    expect(getByText(/Configured schedule:/)).toBeTruthy();
    expect(queryByText(/^Current schedule:/)).toBeNull();
  });

  it("renders '(effective in A4-b)' suffix on each row", () => {
    const { getAllByText } = render(<SectionDaemons snapshot={snap} />);
    expect(getAllByText("(effective in A4-b)").length).toBeGreaterThanOrEqual(2);
  });

  it("renders 'edit coming in A4-b' affordance", () => {
    const { getByText } = render(<SectionDaemons snapshot={snap} />);
    expect(getByText("edit coming in A4-b")).toBeTruthy();
  });

  it("has no Save button (read-only)", () => {
    const { container } = render(<SectionDaemons snapshot={snap} />);
    expect(container.querySelectorAll("button")).toHaveLength(0);
  });

  it("shows 'Schedule unavailable' on snapshot error", () => {
    const errSnap: SettingsSnapshot = { status: "error", data: null, error: new Error("boom"), refresh: vi.fn(async () => {}) };
    const { getByText } = render(<SectionDaemons snapshot={errSnap} />);
    expect(getByText(/Schedule unavailable/)).toBeTruthy();
  });
});
```

### Step 9.5 — Replace placeholder imports + delete it

- [ ] **Step 9.5a: Update `Settings.tsx` imports** to use the per-section files for the 3 sections this task ships:

```tsx
import { SectionAppearance } from "../components/settings/SectionAppearance";
import { SectionGuiServer } from "../components/settings/SectionGuiServer";
import { SectionDaemons } from "../components/settings/SectionDaemons";
import { SectionBackups, SectionAdvanced } from "../components/settings/SectionPlaceholder";
```

- [ ] **Step 9.5b: Edit `internal/gui/frontend/src/components/settings/SectionPlaceholder.tsx`** to keep only `SectionBackups` + `SectionAdvanced` exports (Task 10 will delete the file entirely):

```tsx
import type { SettingsSnapshot } from "../../lib/settings-types";
type Props = { snapshot: SettingsSnapshot; onDirtyChange?: (b: boolean) => void };
export function SectionBackups(_: Props)  { return <section data-section="backups"><h2>Backups</h2></section>; }
export function SectionAdvanced(_: Props) { return <section data-section="advanced"><h2>Advanced</h2></section>; }
```

- [ ] **Step 9.5c: Run vitest + typecheck — expect PASS**

```bash
cd internal/gui/frontend && npm run typecheck && npm run test -- --run "Section(Appearance|GuiServer|Daemons)" 2>&1 | tail -25 && cd ../../..
```

- [ ] **Step 9.5d: Commit**

```bash
git add internal/gui/frontend/src/components/settings/SectionAppearance.tsx internal/gui/frontend/src/components/settings/SectionAppearance.test.tsx internal/gui/frontend/src/components/settings/SectionGuiServer.tsx internal/gui/frontend/src/components/settings/SectionGuiServer.test.tsx internal/gui/frontend/src/components/settings/SectionDaemons.tsx internal/gui/frontend/src/components/settings/SectionDaemons.test.tsx internal/gui/frontend/src/components/settings/useSectionSaveFlow.ts internal/gui/frontend/src/components/settings/SectionPlaceholder.tsx internal/gui/frontend/src/screens/Settings.tsx
git commit -m "feat(gui/frontend): SectionAppearance + SectionGuiServer + SectionDaemons + per-section save flow (A4-a §9.1, §9.2, §9.3)"
```

---

## Task 10: SectionBackups + BackupsList + SectionAdvanced

**Memo refs:** §3.6, §9.4, §9.5.

**Files:**

- Create: `internal/gui/frontend/src/components/settings/BackupsList.tsx` (+ test)
- Create: `internal/gui/frontend/src/components/settings/SectionBackups.tsx` (+ test)
- Create: `internal/gui/frontend/src/components/settings/SectionAdvanced.tsx` (+ test)
- Delete: `internal/gui/frontend/src/components/settings/SectionPlaceholder.tsx`
- Modify: `internal/gui/frontend/src/screens/Settings.tsx` — switch final imports to per-section files

### Step 10.1 — Locked-copy constants + literal-equality test

- [ ] **Step 10.1a: Add `internal/gui/frontend/src/components/settings/backups-copy.ts`**

```ts
// Codex-locked copy strings (memo §9.4). DO NOT paraphrase. The Vitest
// test in backups-copy.test.ts asserts exact equality against the memo
// literals — paraphrasing breaks those tests independent of any
// component test that only checks rendering.
export const BACKUPS_COPY = {
  sliderLabel: "Keep timestamped backups per client",
  helperText:  "Preview only. No files are deleted from this screen.",
  rowBadge:    "Would be eligible for cleanup",
  cleanTooltip:
    "Cleanup arrives in A4-b. This view only previews which timestamped backups cleanup would target.",
  groupNote:
    "Original backups are never cleaned. Retention is calculated separately for each client.",
  previewFailureInline: "Preview unavailable",
} as const;
```

- [ ] **Step 10.1b: Add `internal/gui/frontend/src/components/settings/backups-copy.test.ts`** (Codex r1 P2.3)

```ts
// Lock the verbatim Codex copy from memo §9.4. If a future implementer
// rewords any of these constants the test fails immediately, regardless
// of whether component tests still pass against the (paraphrased) constant.
import { describe, expect, it } from "vitest";
import { BACKUPS_COPY } from "./backups-copy";

describe("BACKUPS_COPY (memo §9.4 verbatim Codex copy)", () => {
  it("sliderLabel matches memo exactly", () => {
    expect(BACKUPS_COPY.sliderLabel).toBe("Keep timestamped backups per client");
  });
  it("helperText matches memo exactly", () => {
    expect(BACKUPS_COPY.helperText).toBe("Preview only. No files are deleted from this screen.");
  });
  it("rowBadge matches memo exactly", () => {
    expect(BACKUPS_COPY.rowBadge).toBe("Would be eligible for cleanup");
  });
  it("cleanTooltip matches memo exactly", () => {
    expect(BACKUPS_COPY.cleanTooltip).toBe(
      "Cleanup arrives in A4-b. This view only previews which timestamped backups cleanup would target.",
    );
  });
  it("groupNote matches memo exactly", () => {
    expect(BACKUPS_COPY.groupNote).toBe(
      "Original backups are never cleaned. Retention is calculated separately for each client.",
    );
  });
  it("previewFailureInline matches memo exactly", () => {
    expect(BACKUPS_COPY.previewFailureInline).toBe("Preview unavailable");
  });
});
```

### Step 10.2 — `BackupsList` component

- [ ] **Step 10.2a: Write `internal/gui/frontend/src/components/settings/BackupsList.tsx`**

```tsx
import { useEffect, useMemo, useState } from "preact/hooks";
import { getBackups, getBackupsCleanPreview } from "../../lib/settings-api";
import type { BackupInfo } from "../../lib/settings-types";
import { BACKUPS_COPY } from "./backups-copy";

export type BackupsListProps = {
  // The keep_n value to preview against. -1 means "no preview yet".
  keepN: number;
};

const CLIENT_ORDER = ["claude-code", "codex-cli", "gemini-cli", "antigravity"];

export function BackupsList({ keepN }: BackupsListProps): preact.JSX.Element {
  const [backups, setBackups] = useState<BackupInfo[] | null>(null);
  const [loadErr, setLoadErr] = useState<string | null>(null);
  const [wouldRemove, setWouldRemove] = useState<Set<string>>(new Set());
  const [previewFailed, setPreviewFailed] = useState(false);

  useEffect(() => {
    let cancelled = false;
    getBackups()
      .then((rows) => { if (!cancelled) setBackups(rows); })
      .catch((e) => { if (!cancelled) setLoadErr(String(e?.message ?? e)); });
    return () => { cancelled = true; };
  }, []);

  // Debounced preview refetch on keepN change.
  useEffect(() => {
    if (keepN < 0) return;
    let cancelled = false;
    const id = setTimeout(async () => {
      try {
        const paths = await getBackupsCleanPreview(keepN);
        if (cancelled) return;
        setWouldRemove(new Set(paths));
        setPreviewFailed(false);
      } catch {
        if (cancelled) return;
        setPreviewFailed(true);
      }
    }, 250);
    return () => { cancelled = true; clearTimeout(id); };
  }, [keepN]);

  const groups = useMemo(() => {
    const m = new Map<string, BackupInfo[]>();
    for (const c of CLIENT_ORDER) m.set(c, []);
    for (const b of backups ?? []) {
      if (!m.has(b.client)) m.set(b.client, []);
      m.get(b.client)!.push(b);
    }
    // Sort each client's backups: originals last, timestamped newest-first.
    for (const arr of m.values()) {
      arr.sort((a, b) => {
        if (a.kind === b.kind) return b.mod_time.localeCompare(a.mod_time);
        return a.kind === "original" ? 1 : -1;
      });
    }
    return m;
  }, [backups]);

  if (loadErr) {
    return <p class="error-banner">Could not load backups: {loadErr}</p>;
  }
  if (backups === null) {
    return <p>Loading backups…</p>;
  }

  return (
    <div class="backups-list">
      <p class="backups-group-note">{BACKUPS_COPY.groupNote}</p>
      {previewFailed ? (
        <p class="backups-preview-unavailable" data-test-id="preview-unavailable">{BACKUPS_COPY.previewFailureInline}</p>
      ) : null}
      {Array.from(groups.entries()).map(([client, rows]) => (
        <details key={client} class="backups-client-group" open>
          <summary>{client} ({rows.length} backup{rows.length === 1 ? "" : "s"})</summary>
          <ul>
            {rows.map((b) => {
              const eligible = b.kind === "timestamped" && wouldRemove.has(b.path);
              return (
                <li
                  key={b.path}
                  class={`backups-row ${b.kind} ${eligible ? "eligible" : ""}`}
                  data-eligible={eligible ? "true" : "false"}
                >
                  <span class="backups-row-when">{relTime(b.mod_time)}</span>
                  <span class={`backups-row-kind kind-${b.kind}`}>{b.kind}</span>
                  <span class="backups-row-size">{formatBytes(b.size_byte)}</span>
                  {eligible ? (
                    <span class="backups-eligible-badge" data-test-id="eligible-badge">
                      {BACKUPS_COPY.rowBadge}
                    </span>
                  ) : null}
                </li>
              );
            })}
            {rows.length === 0 ? <li class="backups-row empty"><span>No backups for this client.</span></li> : null}
          </ul>
        </details>
      ))}
    </div>
  );
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
  return `${(n / 1024 / 1024).toFixed(1)} MiB`;
}

function relTime(rfc3339: string): string {
  const t = Date.parse(rfc3339);
  if (Number.isNaN(t)) return rfc3339;
  return new Date(t).toISOString().replace("T", " ").slice(0, 16);
}
```

- [ ] **Step 10.2b: Write `internal/gui/frontend/src/components/settings/BackupsList.test.tsx`**

```tsx
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, waitFor } from "@testing-library/preact";
import { BackupsList } from "./BackupsList";
import * as api from "../../lib/settings-api";
import { BACKUPS_COPY } from "./backups-copy";

const fixture = [
  { client: "claude-code", path: "/cc/orig.bak", kind: "original" as const,
    mod_time: "2025-12-01T00:00:00Z", size_byte: 1000 },
  { client: "claude-code", path: "/cc/2026-04-25.bak", kind: "timestamped" as const,
    mod_time: "2026-04-25T14:00:00Z", size_byte: 1234 },
  { client: "claude-code", path: "/cc/2026-04-24.bak", kind: "timestamped" as const,
    mod_time: "2026-04-24T14:00:00Z", size_byte: 1100 },
];

describe("BackupsList", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.spyOn(api, "getBackups").mockResolvedValue(fixture);
    vi.spyOn(api, "getBackupsCleanPreview").mockResolvedValue([]);
  });

  it("renders 4 client groups", async () => {
    const { findAllByText } = render(<BackupsList keepN={5} />);
    // Wait for load.
    await findAllByText(/claude-code/);
    // Each client has its own <details><summary>.
    const summaries = document.querySelectorAll(".backups-client-group summary");
    expect(summaries.length).toBe(4);
  });

  it("renders the locked group note (Codex copy §9.4)", async () => {
    const { findByText } = render(<BackupsList keepN={5} />);
    expect(await findByText(BACKUPS_COPY.groupNote)).toBeTruthy();
  });

  it("would-prune rows tagged with eligible badge", async () => {
    vi.spyOn(api, "getBackupsCleanPreview").mockResolvedValue(["/cc/2026-04-24.bak"]);
    const { findByTestId, container } = render(<BackupsList keepN={1} />);
    await findByTestId("eligible-badge");
    expect(container.querySelector('[data-test-id="eligible-badge"]')!.textContent).toBe(BACKUPS_COPY.rowBadge);
  });

  it("originals NEVER get the eligible badge even if path matches", async () => {
    // Defensive: simulate backend mistakenly including an original path.
    vi.spyOn(api, "getBackupsCleanPreview").mockResolvedValue(["/cc/orig.bak"]);
    const { container } = render(<BackupsList keepN={0} />);
    await waitFor(() => expect(container.querySelectorAll(".backups-row.original").length).toBeGreaterThan(0));
    const orig = Array.from(container.querySelectorAll(".backups-row.original"))[0];
    expect(orig.querySelector('[data-test-id="eligible-badge"]')).toBeNull();
  });

  it("preview failure shows 'Preview unavailable' inline + base list still visible", async () => {
    vi.spyOn(api, "getBackupsCleanPreview").mockRejectedValue(new Error("boom"));
    const { findByTestId, findAllByText } = render(<BackupsList keepN={2} />);
    expect(await findByTestId("preview-unavailable")).toBeTruthy();
    // Base list still rendered.
    await findAllByText(/claude-code/);
  });
});
```

### Step 10.3 — `SectionBackups` (slider + list + locked copy)

- [ ] **Step 10.3a: Write `internal/gui/frontend/src/components/settings/SectionBackups.tsx`**

```tsx
import { useState, useEffect } from "preact/hooks";
import { putSetting } from "../../lib/settings-api";
import { BackupsList } from "./BackupsList";
import { BACKUPS_COPY } from "./backups-copy";
import type { SettingsSnapshot, ConfigSettingDTO } from "../../lib/settings-types";

export type SectionBackupsProps = {
  snapshot: SettingsSnapshot;
  onDirtyChange: (b: boolean) => void;
};

export function SectionBackups({ snapshot, onDirtyChange }: SectionBackupsProps): preact.JSX.Element {
  if (snapshot.status !== "ok") return <section data-section="backups"><h2>Backups</h2></section>;
  const def = snapshot.data.settings.find((s) => s.key === "backups.keep_n") as ConfigSettingDTO;
  const persisted = Number(def.value);

  const [draft, setDraft] = useState<number>(persisted);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [banner, setBanner] = useState<string | null>(null);

  // Re-anchor when snapshot persisted value changes (e.g. after refresh).
  useEffect(() => { setDraft(persisted); }, [persisted]);

  const dirty = draft !== persisted;
  useEffect(() => onDirtyChange(dirty), [dirty, onDirtyChange]);

  async function save() {
    setBusy(true);
    setErr(null);
    try {
      await putSetting("backups.keep_n", String(draft));
      setBanner("Saved.");
      setTimeout(() => setBanner(null), 2000);
      await snapshot.refresh();
    } catch (e: any) {
      setErr(String(e?.body?.reason ?? e?.message ?? "save failed"));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section data-section="backups" class="settings-section">
      <h2>Backups</h2>
      <p class="settings-section-help">Manage backup retention for managed client configs.</p>

      <div class="backups-slider-row">
        <label for="backups-keep-n-slider" class="backups-slider-label">
          {BACKUPS_COPY.sliderLabel}: <strong>{draft}</strong>
        </label>
        <input
          id="backups-keep-n-slider"
          type="range"
          min={def.min ?? 0}
          max={def.max ?? 50}
          value={draft}
          disabled={busy}
          onInput={(e) => setDraft(Number((e.target as HTMLInputElement).value))}
        />
        <small class="backups-helper-text">{BACKUPS_COPY.helperText}</small>
        {err ? <small class="settings-field-error" role="alert">{err}</small> : null}
      </div>

      <BackupsList keepN={draft} />

      <div class="backups-clean-row">
        <button
          type="button"
          disabled
          title={BACKUPS_COPY.cleanTooltip}
          aria-label={BACKUPS_COPY.cleanTooltip}
          data-test-id="clean-now-disabled"
        >
          Clean now
        </button>
        <span class="deferred-badge">(coming in A4-b)</span>
      </div>

      <div class="settings-section-footer">
        {banner ? <span class="save-banner ok">{banner}</span> : null}
        <button type="button" disabled={!dirty || busy} onClick={() => void save()}>
          {busy ? "Saving…" : "Save"}
        </button>
        <button type="button" disabled={!dirty || busy} onClick={() => setDraft(persisted)}>Reset</button>
      </div>
    </section>
  );
}
```

- [ ] **Step 10.3b: Write `internal/gui/frontend/src/components/settings/SectionBackups.test.tsx`**

```tsx
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, fireEvent, waitFor } from "@testing-library/preact";
import { SectionBackups } from "./SectionBackups";
import * as api from "../../lib/settings-api";
import { BACKUPS_COPY } from "./backups-copy";
import type { SettingsSnapshot, SettingsEnvelope } from "../../lib/settings-types";

const env: SettingsEnvelope = {
  actual_port: 9125,
  settings: [
    { key: "backups.keep_n", section: "backups", type: "int",
      default: "5", value: "5", min: 0, max: 50, deferred: false, help: "" },
    { key: "backups.clean_now", section: "backups", type: "action", deferred: true, help: "" },
  ],
};
const snap = (refresh = vi.fn(async () => {})): SettingsSnapshot =>
  ({ status: "ok", data: env, error: null, refresh });

describe("SectionBackups", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.spyOn(api, "getBackups").mockResolvedValue([]);
    vi.spyOn(api, "getBackupsCleanPreview").mockResolvedValue([]);
  });

  it("renders all 6 verbatim Codex copy strings (memo §9.4)", async () => {
    const { findByText } = render(<SectionBackups snapshot={snap()} onDirtyChange={() => {}} />);
    expect(await findByText(new RegExp(BACKUPS_COPY.sliderLabel))).toBeTruthy();
    expect(await findByText(BACKUPS_COPY.helperText)).toBeTruthy();
    expect(await findByText(BACKUPS_COPY.groupNote)).toBeTruthy();
    // Tooltip: title attribute on the disabled Clean button.
    const btn = document.querySelector('[data-test-id="clean-now-disabled"]') as HTMLButtonElement;
    expect(btn.title).toBe(BACKUPS_COPY.cleanTooltip);
    expect(btn.disabled).toBe(true);
    // The eligible-badge + preview-unavailable copy come from BackupsList
    // and are tested in BackupsList.test.tsx; this test asserts the
    // section-level surface only.
  });

  it("slider drag dirties the section", async () => {
    const onDirty = vi.fn();
    const { container } = render(<SectionBackups snapshot={snap()} onDirtyChange={onDirty} />);
    const slider = container.querySelector("input[type=range]") as HTMLInputElement;
    fireEvent.input(slider, { target: { value: "10" } });
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(true));
  });

  it("Save calls putSetting + refreshes snapshot", async () => {
    const putSpy = vi.spyOn(api, "putSetting").mockResolvedValue(undefined);
    const refresh = vi.fn(async () => {});
    const { container } = render(<SectionBackups snapshot={snap(refresh)} onDirtyChange={() => {}} />);
    const slider = container.querySelector("input[type=range]") as HTMLInputElement;
    fireEvent.input(slider, { target: { value: "12" } });
    fireEvent.click(Array.from(container.querySelectorAll("button")).find((b) => b.textContent === "Save")!);
    await waitFor(() => expect(putSpy).toHaveBeenCalledWith("backups.keep_n", "12"));
    await waitFor(() => expect(refresh).toHaveBeenCalled());
  });

  it("disabled Clean now button has the locked tooltip", () => {
    const { container } = render(<SectionBackups snapshot={snap()} onDirtyChange={() => {}} />);
    const btn = container.querySelector('[data-test-id="clean-now-disabled"]') as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    expect(btn.title).toBe(BACKUPS_COPY.cleanTooltip);
  });
});
```

### Step 10.4 — `SectionAdvanced`

- [ ] **Step 10.4a: Write `internal/gui/frontend/src/components/settings/SectionAdvanced.tsx`**

```tsx
import { useState } from "preact/hooks";
import { postAction } from "../../lib/settings-api";
import type { SettingsSnapshot } from "../../lib/settings-types";

export type SectionAdvancedProps = {
  snapshot: SettingsSnapshot;
};

export function SectionAdvanced({ snapshot: _ }: SectionAdvancedProps): preact.JSX.Element {
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function openFolder() {
    setBusy(true);
    setErr(null);
    try {
      await postAction("advanced.open_app_data_folder");
    } catch (e: any) {
      setErr(String(e?.body?.reason ?? e?.message ?? "spawn failed"));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section data-section="advanced" class="settings-section">
      <h2>Advanced</h2>
      <p class="settings-section-help">Power-user actions.</p>
      <div class="advanced-actions">
        <button type="button" onClick={() => void openFolder()} disabled={busy} data-test-id="open-folder">
          Open app-data folder
        </button>
        <button type="button" disabled data-test-id="export-bundle-disabled">
          Export bundle
          <span class="deferred-badge"> (coming in A4-b)</span>
        </button>
      </div>
      {err ? <p class="error-banner" role="alert">Could not open folder: {err}</p> : null}
    </section>
  );
}
```

- [ ] **Step 10.4b: Write `internal/gui/frontend/src/components/settings/SectionAdvanced.test.tsx`**

```tsx
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, fireEvent, waitFor } from "@testing-library/preact";
import { SectionAdvanced } from "./SectionAdvanced";
import * as api from "../../lib/settings-api";
import type { SettingsSnapshot } from "../../lib/settings-types";

const snap: SettingsSnapshot = {
  status: "ok",
  data: { actual_port: 9125, settings: [] },
  error: null,
  refresh: vi.fn(async () => {}),
};

describe("SectionAdvanced", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("Open folder button calls postAction", async () => {
    const spy = vi.spyOn(api, "postAction").mockResolvedValue({ opened: "/x" });
    const { container } = render(<SectionAdvanced snapshot={snap} />);
    const btn = container.querySelector('[data-test-id="open-folder"]') as HTMLButtonElement;
    fireEvent.click(btn);
    await waitFor(() => expect(spy).toHaveBeenCalledWith("advanced.open_app_data_folder"));
  });

  it("error from postAction surfaces inline", async () => {
    vi.spyOn(api, "postAction").mockRejectedValue(Object.assign(new Error("nope"), { body: { reason: "not found" } }));
    const { container, findByText } = render(<SectionAdvanced snapshot={snap} />);
    const btn = container.querySelector('[data-test-id="open-folder"]') as HTMLButtonElement;
    fireEvent.click(btn);
    expect(await findByText(/Could not open folder: not found/)).toBeTruthy();
  });

  it("Export bundle button is disabled with (coming in A4-b)", () => {
    const { container, getByText } = render(<SectionAdvanced snapshot={snap} />);
    const btn = container.querySelector('[data-test-id="export-bundle-disabled"]') as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
    expect(getByText(/coming in A4-b/)).toBeTruthy();
  });
});
```

### Step 10.5 — Final import wiring + delete placeholder

- [ ] **Step 10.5a: Update `internal/gui/frontend/src/screens/Settings.tsx`** so all 5 imports point to per-section files:

```tsx
import { SectionAppearance } from "../components/settings/SectionAppearance";
import { SectionGuiServer } from "../components/settings/SectionGuiServer";
import { SectionDaemons } from "../components/settings/SectionDaemons";
import { SectionBackups } from "../components/settings/SectionBackups";
import { SectionAdvanced } from "../components/settings/SectionAdvanced";
```

- [ ] **Step 10.5b: Delete `internal/gui/frontend/src/components/settings/SectionPlaceholder.tsx`**

```bash
rm internal/gui/frontend/src/components/settings/SectionPlaceholder.tsx
```

- [ ] **Step 10.5c: Run vitest + typecheck — expect PASS**

```bash
cd internal/gui/frontend && npm run typecheck && npm run test -- --run "Section(Backups|Advanced)|BackupsList" 2>&1 | tail -25 && cd ../../..
```

- [ ] **Step 10.5d: Commit**

```bash
git add internal/gui/frontend/src/components/settings/backups-copy.ts internal/gui/frontend/src/components/settings/backups-copy.test.ts internal/gui/frontend/src/components/settings/BackupsList.tsx internal/gui/frontend/src/components/settings/BackupsList.test.tsx internal/gui/frontend/src/components/settings/SectionBackups.tsx internal/gui/frontend/src/components/settings/SectionBackups.test.tsx internal/gui/frontend/src/components/settings/SectionAdvanced.tsx internal/gui/frontend/src/components/settings/SectionAdvanced.test.tsx internal/gui/frontend/src/screens/Settings.tsx
git rm internal/gui/frontend/src/components/settings/SectionPlaceholder.tsx
git commit -m "feat(gui/frontend): SectionBackups + BackupsList + SectionAdvanced (locked Codex copy, A4-a §9.4, §9.5)"
```

---

## Task 11: CSS + theme/density wiring + E2E + go generate + docs

**Memo refs:** §10.2, §11.3, §12.

**Files:**

- Modify: `internal/gui/frontend/src/styles/style.css` (Settings layout + theme/density vars + Backups styles)
- Create: `internal/gui/e2e/tests/settings.spec.ts`
- Modify: `CLAUDE.md` (E2E count + Settings paragraph)
- Modify: `docs/superpowers/plans/phase-3b-ii-backlog.md` (mark A4 done)
- Regenerate: `internal/gui/assets/{index.html,app.js,style.css}` via `go generate`

### Step 11.1 — Verify theme/density CSS variable scaffolding

The memo §10.2 flagged this as an open question: do CSS variables `--bg-color` etc. and `<html data-theme>` hookups already exist? Verify, and add only if missing.

- [ ] **Step 11.1a: Inspect existing style.css for theme variables**

```bash
grep -n "data-theme\|--bg-color\|--text-color\|prefers-color-scheme\|data-density" internal/gui/frontend/src/styles/style.css | head -30
```

If output shows existing `:root[data-theme="dark"]` / `:root[data-theme="light"]` / `@media (prefers-color-scheme: dark)` blocks, scaffolding exists — skip Step 11.1b.

If output is empty, scaffolding does not exist — add it in Step 11.1b.

- [ ] **Step 11.1b: Add theme/density variables to `style.css` (only if missing)**

Append to the end of `internal/gui/frontend/src/styles/style.css`:

```css
/* ============================================================
 * A4-a — theme + density CSS variables (memo §10.2)
 * Only add this block if the file did not already contain a
 * data-theme/data-density scaffold.
 * ============================================================ */
:root {
  --bg-color: #ffffff;
  --text-color: #1f2937;
  --muted-color: #6b7280;
  --border-color: #e5e7eb;
  --accent-color: #3b82f6;
  --error-color: #dc2626;
  --warning-color: #92400e;
  --row-height: 32px;
  --section-gap: 24px;
}
:root[data-theme="dark"] {
  --bg-color: #0f172a;
  --text-color: #e5e7eb;
  --muted-color: #9ca3af;
  --border-color: #334155;
}
@media (prefers-color-scheme: dark) {
  :root[data-theme="system"] {
    --bg-color: #0f172a;
    --text-color: #e5e7eb;
    --muted-color: #9ca3af;
    --border-color: #334155;
  }
}
:root[data-density="compact"]    { --row-height: 24px; --section-gap: 12px; }
:root[data-density="comfortable"] { --row-height: 32px; --section-gap: 24px; }
:root[data-density="spacious"]   { --row-height: 40px; --section-gap: 36px; }
```

Then ensure `internal/gui/frontend/src/main.tsx` (or wherever the app boots) reads the persisted theme/density on mount. Verify with:

```bash
grep -n "data-theme\|data-density" internal/gui/frontend/src/main.tsx 2>/dev/null
```

If absent, add to the top of the app's mount logic — but since A4-a's section-save flow already triggers `snapshot.refresh()`, plumbing live theme application via a `useEffect` inside `Settings.tsx` after each ok-snapshot is sufficient for A4-a:

In `Settings.tsx`, after the existing scroll-spy `useEffect`, add:

```tsx
// Apply theme + density CSS variables on every snapshot change.
useEffect(() => {
  if (snapshot.status !== "ok") return;
  const theme = snapshot.data.settings.find((s) => s.key === "appearance.theme");
  const density = snapshot.data.settings.find((s) => s.key === "appearance.density");
  if (theme && "value" in theme) document.documentElement.setAttribute("data-theme", theme.value);
  if (density && "value" in density) document.documentElement.setAttribute("data-density", density.value);
}, [snapshot.status, snapshot.status === "ok" ? snapshot.data : null]);
```

### Step 11.2 — Settings layout CSS

- [ ] **Step 11.2a: Append Settings + Backups styles to `internal/gui/frontend/src/styles/style.css`**

```css
/* ============================================================
 * A4-a — Settings screen layout (memo §3.4, §8.4)
 * ============================================================ */
.settings-layout {
  display: grid;
  grid-template-columns: 110px 1fr;
  gap: 24px;
}
.settings-section-nav {
  position: sticky;
  top: 16px;
  align-self: start;
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: 13px;
  color: var(--muted-color);
  border-right: 1px solid var(--border-color);
  padding-right: 12px;
}
.settings-section-nav a {
  color: inherit;
  text-decoration: none;
  padding: 4px 6px;
  border-left: 2px solid transparent;
  margin-left: -8px;
}
.settings-section-nav a.active {
  color: var(--accent-color);
  border-left-color: var(--accent-color);
}
.settings-body { min-width: 0; }
.settings-section {
  margin-bottom: var(--section-gap);
  border-bottom: 1px solid var(--border-color);
  padding-bottom: var(--section-gap);
}
.settings-section h2 {
  margin: 0 0 4px 0;
  font-size: 18px;
}
.settings-section-help {
  color: var(--muted-color);
  margin: 0 0 12px 0;
}
.settings-field {
  display: flex;
  flex-direction: column;
  gap: 4px;
  margin-bottom: 12px;
  min-height: var(--row-height);
}
.settings-field-label {
  font-weight: 600;
}
.settings-field-help {
  color: var(--muted-color);
}
.settings-field-error {
  color: var(--error-color);
}
.settings-field.has-error input,
.settings-field.has-error select {
  border-color: var(--error-color);
}
.settings-field.disabled {
  opacity: 0.6;
}
.settings-section-footer {
  display: flex;
  gap: 8px;
  align-items: center;
  margin-top: 12px;
}
.save-banner.ok {
  color: #059669;
}
.save-banner.partial {
  color: var(--warning-color);
}
.deferred-badge {
  color: var(--muted-color);
  font-size: 12px;
  margin-left: 6px;
}
.settings-restart-badge {
  color: var(--warning-color);
  margin-left: 8px;
  font-size: 13px;
}
.settings-section-readonly .readonly-row {
  display: flex;
  gap: 12px;
  align-items: baseline;
}
.settings-section-readonly .readonly-label {
  font-weight: 600;
}
.settings-section-readonly .readonly-suffix {
  color: var(--muted-color);
  font-size: 12px;
}
.deferred-affordance {
  color: var(--muted-color);
  font-style: italic;
  margin-top: 8px;
}

/* ============================================================
 * A4-a — Backups list (memo §9.4 — NO red destructive styling)
 * ============================================================ */
.backups-slider-row {
  display: flex;
  flex-direction: column;
  gap: 4px;
  margin-bottom: 12px;
}
.backups-helper-text {
  color: var(--muted-color);
  font-size: 12px;
}
.backups-list {
  margin: 12px 0;
}
.backups-group-note {
  color: var(--muted-color);
  font-size: 13px;
  margin-bottom: 8px;
}
.backups-preview-unavailable {
  color: var(--warning-color);
  font-size: 13px;
}
.backups-client-group {
  border: 1px solid var(--border-color);
  border-radius: 4px;
  padding: 6px 10px;
  margin-bottom: 8px;
}
.backups-client-group summary {
  cursor: pointer;
  font-weight: 600;
}
.backups-client-group ul {
  list-style: none;
  padding: 0;
  margin: 8px 0 0 0;
}
.backups-row {
  display: grid;
  grid-template-columns: 1fr 100px 80px auto;
  gap: 8px;
  padding: 4px 0;
  align-items: center;
  font-size: 13px;
}
.backups-row.eligible {
  /* DELIBERATELY NEUTRAL — Codex r1 §9.4: NO red destructive styling.
     Use a faint diagonal-stripe pattern on a neutral gray. */
  background: repeating-linear-gradient(
    45deg,
    transparent 0 6px,
    rgba(107, 114, 128, 0.06) 6px 12px
  );
  opacity: 0.75;
}
.backups-eligible-badge {
  background: rgba(107, 114, 128, 0.15);
  color: var(--muted-color);
  border-radius: 3px;
  padding: 1px 6px;
  font-size: 11px;
}
.backups-row-kind.kind-original { color: var(--muted-color); font-weight: 600; }
.backups-clean-row {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 8px 0;
}
```

### Step 11.3 — E2E tests (Codex r1 P1.1 — uses existing repo fixture)

The repo's existing E2E pattern (verified at `internal/gui/e2e/fixtures/hub.ts`) exports a Playwright `test` extended with a `hub: HubHandle` fixture. Tests destructure `{ page, hub }` per-test; there is **no** `spawnHub` factory and **no** `helpers/hub` directory. `HubHandle` exposes `{ url: string; port: number; home: string }` only — `seedBackups` and `readSettingsYaml` live as standalone helper functions in this spec file (or in a sibling file under `tests/`) and operate on `hub.home`.

- [ ] **Step 11.3a: Write `internal/gui/e2e/tests/settings.spec.ts`**

```ts
// Phase 3B-II A4-a — Settings screen E2E. Memo §11.3 (16 scenarios).
// Uses the repo's existing fixture API: `import { test, expect } from "../fixtures/hub";`
// and the per-test `{ page, hub }` destructure pattern. Codex r1 P1.1.
import { test, expect } from "../fixtures/hub";
import * as fs from "node:fs/promises";
import * as path from "node:path";

const LIVE_BY_CLIENT: Record<string, string> = {
  "claude-code": ".claude.json",
  "codex-cli": ".codex/config.toml",
  "gemini-cli": ".gemini/settings.json",
  "antigravity": ".gemini/antigravity/mcp_config.json",
};

// Seed N timestamped backups for `client` under hub.home. Backups land
// next to the live config and use the canonical filename pattern from
// internal/api/backups.go: `<liveBase>.bak-mcp-local-hub-<timestamp>`.
async function seedBackups(home: string, client: keyof typeof LIVE_BY_CLIENT, count: number): Promise<void> {
  const live = LIVE_BY_CLIENT[client];
  if (!live) throw new Error(`unknown client ${client}`);
  const fullLive = path.join(home, live);
  await fs.mkdir(path.dirname(fullLive), { recursive: true });
  // Touch the live file so BackupsList's clientFiles(home) lookup includes it.
  try { await fs.access(fullLive); } catch { await fs.writeFile(fullLive, "{}"); }
  const baseName = path.basename(live);
  const dir = path.dirname(fullLive);
  for (let i = 0; i < count; i++) {
    const ts = new Date(Date.now() - i * 86400_000).toISOString().replace(/[:.]/g, "-");
    const bak = path.join(dir, `${baseName}.bak-mcp-local-hub-${ts}`);
    await fs.writeFile(bak, "{}");
  }
}

// Read the persisted gui-preferences.yaml using the same path resolution
// rules as internal/api/settings.go::SettingsPath. Hub fixture sets
// LOCALAPPDATA + XDG_DATA_HOME to `home`, so on Windows the file lands
// at <home>/mcp-local-hub/gui-preferences.yaml.
async function readSettingsYaml(home: string): Promise<string> {
  const candidates = [
    path.join(home, "mcp-local-hub", "gui-preferences.yaml"),                 // LOCALAPPDATA / XDG_DATA_HOME
    path.join(home, ".local", "share", "mcp-local-hub", "gui-preferences.yaml"), // POSIX fallback
  ];
  for (const p of candidates) {
    try { return await fs.readFile(p, "utf8"); } catch { /* try next */ }
  }
  throw new Error("gui-preferences.yaml not found under any known path under " + home);
}

test("Settings sidebar link navigates to settings screen", async ({ page, hub }) => {
  await page.goto(hub.url);
  await page.click('a[href="#/servers"]'); // start somewhere
  await page.click('a[href="#/settings"]');
  await expect(page.locator("h1", { hasText: "Settings" })).toBeVisible();
  expect(page.url()).toContain("#/settings");
});

test("All 5 section headers render", async ({ page, hub }) => {
  await page.goto(hub.url + "#/settings");
  for (const name of ["Appearance", "GUI server", "Daemons", "Backups", "Advanced"]) {
    await expect(page.locator("h2", { hasText: new RegExp(`^${name}$`) })).toBeVisible();
  }
});

test("Deep-link query-string scrolls Backups into view (Codex r1 P1.1)", async ({ page, hub }) => {
  await page.goto(hub.url + "#/settings?section=backups");
  const target = page.locator('section[data-section="backups"]');
  await expect(target).toBeInViewport();
});

test("Sticky inner-nav active state changes on scroll", async ({ page, hub }) => {
  await page.goto(hub.url + "#/settings");
  await page.evaluate(() => {
    document.querySelector('section[data-section="gui_server"]')?.scrollIntoView({ block: "start" });
  });
  await page.waitForFunction(() => {
    const a = document.querySelector('.settings-section-nav a[href="#/settings?section=gui_server"]');
    return a?.classList.contains("active");
  }, null, { timeout: 5000 });
});

test("Save Appearance round-trips to gui-preferences.yaml", async ({ page, hub }) => {
  await page.goto(hub.url + "#/settings");
  await page.locator("#appearance\\.theme").selectOption("dark");
  await page.locator('section[data-section="appearance"] button:has-text("Save")').click();
  await expect(page.locator(".save-banner.ok")).toBeVisible();
  await page.reload();
  await page.click('a[href="#/settings"]');
  await expect(page.locator("#appearance\\.theme")).toHaveValue("dark");
  const yaml = await readSettingsYaml(hub.home);
  expect(yaml).toMatch(/appearance\.theme:\s*dark/);
});

test("Save validation failure shows inline error + keeps key dirty", async ({ page, hub }) => {
  await page.goto(hub.url + "#/settings?section=gui_server");
  await page.locator("#gui_server\\.port").fill("99");
  await page.locator('section[data-section="gui_server"] button:has-text("Save")').click();
  await expect(page.locator(".save-banner.partial")).toBeVisible();
  await expect(page.locator('#gui_server\\.port-error[role="alert"]')).toBeVisible();
});

test("Port pending-restart badge appears after Save (Codex r3 P2.1 + r4 P2.1)", async ({ page, hub }) => {
  await page.goto(hub.url + "#/settings?section=gui_server");
  const actual = await page.evaluate(async () => {
    const r = await fetch("/api/settings", { credentials: "same-origin" });
    return (await r.json()).actual_port;
  });
  const newPort = actual + 100;
  await page.locator("#gui_server\\.port").fill(String(newPort));
  // Codex r4 P2.1: dirty draft does NOT flip badge yet.
  await expect(page.locator('[data-test-id="port-restart-badge"]')).toBeHidden();
  await page.locator('section[data-section="gui_server"] button:has-text("Save")').click();
  await expect(page.locator(".save-banner.ok")).toBeVisible();
  await expect(page.locator('[data-test-id="port-restart-badge"]')).toBeVisible();
});

test("Daemons read-only with 'Configured ... (effective in A4-b)' wording (Codex r1 P1.7)", async ({ page, hub }) => {
  await page.goto(hub.url + "#/settings?section=daemons");
  await expect(page.locator('section[data-section="daemons"]')).toContainText("Configured schedule:");
  await expect(page.locator('section[data-section="daemons"]')).toContainText("(effective in A4-b)");
  await expect(page.locator('section[data-section="daemons"] button')).toHaveCount(0);
  await expect(page.locator('section[data-section="daemons"]')).not.toContainText(/^Current schedule:/);
});

test("Backups list renders 4 client groups", async ({ page, hub }) => {
  await page.goto(hub.url + "#/settings?section=backups");
  await expect(page.locator(".backups-client-group")).toHaveCount(4);
  for (const c of ["claude-code", "codex-cli", "gemini-cli", "antigravity"]) {
    await expect(page.locator(".backups-client-group summary", { hasText: c })).toBeVisible();
  }
});

test("Backups preview marks would-prune rows", async ({ page, hub }) => {
  await seedBackups(hub.home, "claude-code", 7);
  await page.goto(hub.url + "#/settings?section=backups");
  // Set keep_n=3 → expect 4 rows (oldest) tagged eligible.
  await page.locator("#backups-keep-n-slider").fill("3");
  // Wait for debounced preview (250ms debounce + RTT margin).
  await page.waitForTimeout(500);
  const eligible = page.locator('[data-test-id="eligible-badge"]');
  await expect(eligible.first()).toBeVisible();
  expect(await eligible.count()).toBeGreaterThanOrEqual(4);
});

test("Disabled Clean now button has the locked tooltip (memo §9.4)", async ({ page, hub }) => {
  await page.goto(hub.url + "#/settings?section=backups");
  const btn = page.locator('[data-test-id="clean-now-disabled"]');
  await expect(btn).toBeDisabled();
  await expect(btn).toHaveAttribute(
    "title",
    "Cleanup arrives in A4-b. This view only previews which timestamped backups cleanup would target.",
  );
});

test("Open app-data folder action triggers POST (mocked, no real spawn)", async ({ page, hub }) => {
  // Codex r2 P2: intercept the POST so the real backend never actually
  // shells out to explorer.exe / open / xdg-open during the E2E run.
  // The test asserts only that the GUI issues the right POST.
  await page.route("**/api/settings/advanced.open_app_data_folder", async (route, req) => {
    if (req.method() !== "POST") {
      await route.fallback();
      return;
    }
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ opened: "/mocked/path" }),
    });
  });
  await page.goto(hub.url + "#/settings?section=advanced");
  const postPromise = page.waitForRequest((req) =>
    req.method() === "POST" && req.url().endsWith("/api/settings/advanced.open_app_data_folder"),
  );
  await page.locator('[data-test-id="open-folder"]').click();
  await postPromise;
});

test("Discard-key remount: confirmed in-screen discard resets section state (Codex r2 P1, memo §10.4)", async ({ page, hub }) => {
  // Edit Appearance, navigate intra-Settings, confirm discard, verify
  // that local draft is gone (the saved snapshot value is restored).
  await page.goto(hub.url + "#/settings?section=appearance");
  await page.locator("#appearance\\.theme").selectOption("dark");
  // Intra-Settings hash navigation triggers the dirty-guard. Confirm.
  page.once("dialog", (d) => d.accept());
  await page.locator('a[href="#/settings?section=backups"]').click();
  // Hop back to Appearance and assert the draft is gone.
  await page.locator('a[href="#/settings?section=appearance"]').click();
  await expect(page.locator("#appearance\\.theme")).toHaveValue("system");
});

test("Dirty-guard prompts when navigating away from dirty Settings", async ({ page, hub }) => {
  await page.goto(hub.url + "#/settings");
  await page.locator("#appearance\\.theme").selectOption("dark");
  page.once("dialog", (d) => d.dismiss());
  await page.locator('a[href="#/servers"]').click();
  expect(page.url()).toContain("#/settings");
});

test("Per-section Save isolation", async ({ page, hub }) => {
  await page.goto(hub.url + "#/settings");
  await page.locator("#appearance\\.theme").selectOption("dark");
  await page.locator("#gui_server\\.browser_on_launch").click();
  await page.locator('section[data-section="appearance"] button:has-text("Save")').click();
  await expect(page.locator('section[data-section="appearance"] .save-banner.ok')).toBeVisible();
  const guiSaveBtn = page.locator('section[data-section="gui_server"] button:has-text("Save")');
  await expect(guiSaveBtn).toBeEnabled();
});

test("Deferred field 'tray' rendered disabled with (coming in A4-b)", async ({ page, hub }) => {
  await page.goto(hub.url + "#/settings?section=gui_server");
  await expect(page.locator("#gui_server\\.tray")).toBeDisabled();
  await expect(page.locator('section[data-section="gui_server"]')).toContainText("coming in A4-b");
});
```

- [ ] **Step 11.3b: No fixture changes needed** (Codex r1 P1.1)

The two helper functions (`seedBackups`, `readSettingsYaml`) live inside the spec file itself and operate on `hub.home`. The existing `internal/gui/e2e/fixtures/hub.ts` is unchanged. This avoids cross-spec pollution and keeps the helpers close to their only caller.

### Step 11.4 — Regenerate embedded assets

- [ ] **Step 11.4a: Run `go generate`**

```bash
go generate ./internal/gui/...
```

Expected: `internal/gui/assets/{index.html,app.js,style.css}` updated. The bundle now contains the Settings screen + per-section components.

- [ ] **Step 11.4b: Run Go embed smoke**

```bash
go test ./internal/gui/ -run TestEmbed -count=1 -v 2>&1 | tail -10
```

Expected: PASS (assets bundle is non-empty, all expected files present).

### Step 11.5 — Update CLAUDE.md + backlog

- [ ] **Step 11.5a: Update `CLAUDE.md`**

Find the existing E2E coverage paragraph (`### What's covered`) and:

1. Update the count line at the bottom: `76 smoke tests total (...)` → `92 smoke tests total (3 shell + 8 servers + 6 migration + 13 add-server + 17 edit-server + 2 dashboard + 3 logs + 14 secrets + 10 secret-picker + 16 settings)`
2. Add a new bullet after the secret-picker bullet:

```
- Settings: sidebar link + 5 section headers + deep-link query-string (#/settings?section=backups) + sticky inner-nav active-on-scroll + Save Appearance round-trip to gui-preferences.yaml + port save validation (below min) + port pending-restart badge after Save (anchored to persisted, not draft — Codex r3+r4 P2.1) + Daemons read-only "Configured schedule (effective in A4-b)" wording + Backups 4-client groups + would-prune badge with locked Codex copy + disabled Clean-now tooltip + Open app-data folder POST (mocked, no real spawn — Codex r2 P2) + dirty-guard navigation prompt + per-section Save isolation + deferred tray field disabled + discard-key remount on intra-Settings confirmed-discard navigation (Codex r2 P1, memo §10.4).
```

- [ ] **Step 11.5b: Update `docs/superpowers/plans/phase-3b-ii-backlog.md`**

Find the line `9. **A4** — Settings screen` and replace with:

```
9. **A4-a** — Settings screen ✅ — see [docs/superpowers/plans/2026-04-27-phase-3b-ii-a4-settings.md](2026-04-27-phase-3b-ii-a4-settings.md). Memo: [docs/superpowers/specs/2026-04-27-phase-3b-ii-a4-settings-design.md](../specs/2026-04-27-phase-3b-ii-a4-settings-design.md). Merge SHA: <FILL-AT-MERGE>.
9b. **A4-b** — Settings lifecycle: tray, port live-rebind, weekly schedule edit, retry policy, Clean now confirm, export bundle.
```

(`<FILL-AT-MERGE>` is replaced with the actual squash-merge SHA after PR is merged. The plan's executor leaves it unfilled; the user updates after merge.)

### Step 11.6 — Run the full test suite + final smoke

- [ ] **Step 11.6a: Full Go test suite**

```bash
go test ./... -count=1 2>&1 | tail -20
```

Expected: ALL pass. Capture totals.

- [ ] **Step 11.6b: Full Vitest**

```bash
cd internal/gui/frontend && npm run test -- --run 2>&1 | tail -10 && cd ../../..
```

Expected: ~260+ tests pass (224 baseline + ~36 new from A4-a).

- [ ] **Step 11.6c: Full TypeScript typecheck**

```bash
cd internal/gui/frontend && npm run typecheck 2>&1 | tail -10 && cd ../../..
```

Expected: 0 errors.

- [ ] **Step 11.6d: Full E2E suite (Windows runner)**

```bash
cd internal/gui/e2e && npm test 2>&1 | tail -20 && cd ../../..
```

Expected: ALL 92 tests pass (~60s wall-time on warm machine).

- [ ] **Step 11.6e: Manual UI smoke**

```bash
go run ./cmd/mcphub gui --no-browser --no-tray --port 9125
```

Then in a browser: open `http://127.0.0.1:9125`, click Settings in sidebar, verify all 5 sections render, change theme, save, refresh, confirm persistence.

- [ ] **Step 11.6f: Commit**

```bash
git add internal/gui/frontend/src/styles/style.css internal/gui/frontend/src/screens/Settings.tsx internal/gui/assets/index.html internal/gui/assets/app.js internal/gui/assets/style.css internal/gui/e2e/tests/settings.spec.ts CLAUDE.md docs/superpowers/plans/phase-3b-ii-backlog.md
git commit -m "feat(gui): A4-a Settings screen — CSS + theme/density wiring + 16 E2E + asset regen + docs"
```

---

## Closeout

After all 11 task commits are clean:

- [ ] **Closeout 1: Verify branch state**

```bash
git log --oneline master..HEAD
```

Expected: 11 commits ahead of master, all on `feat/phase-3b-ii-a4-settings`.

- [ ] **Closeout 2: Final dry-run**

```bash
go test ./... -count=1 && cd internal/gui/frontend && npm run test -- --run && npm run typecheck && cd ../../.. && cd internal/gui/e2e && npm test && cd ../../..
```

Expected: ALL pass.

- [ ] **Closeout 3: STOP — DO NOT push or open PR**

Per project standing instruction (precedent A3-a/A3-b at PR #18 and #19): the user reviews locally first. The implementer subagent reports DONE; the user runs the smoke and approves before any push happens.

If the user approves a push, the workflow is:

```bash
git push -u origin feat/phase-3b-ii-a4-settings
gh pr create --title "Phase 3B-II A4-a — Settings screen" --body "$(cat <<'EOF'
## Summary
- Settings screen contract: registry-driven schema + per-section Save + BackupsList passive prune preview
- Memo: docs/superpowers/specs/2026-04-27-phase-3b-ii-a4-settings-design.md (rev 2, Codex r1–r5 PASS)
- Plan: docs/superpowers/plans/2026-04-27-phase-3b-ii-a4-settings.md
- Deferred to A4-b: tray, port live rebind, weekly schedule edit, retry policy, Clean now, export bundle

## Test plan
- [x] go test ./... — all pass
- [x] Vitest (260+) — all pass
- [x] Playwright E2E (92 scenarios) — all pass on Windows
- [x] Manual UI smoke against embedded bundle
EOF
)"
```

After PR creation: trigger Codex bot review with `@codex review` comment; iterate to PASS; squash merge per A3-a/A3-b precedent.

---

## Acceptance criteria checklist (mirrors memo §12)

- [ ] AC-1: `internal/api/settings_registry.go` defines `SettingDef` (with `Optional`), `SettingType` consts, `Registry` covers all §5.7 fields including deferred ones.
- [ ] AC-2: `internal/api/settings.go` validates via registry; preserves unknown keys; `settingsMu` guards concurrent writes; `SettingsList`/`SettingsGet` resolve registry defaults.
- [ ] AC-3: `internal/gui/settings.go` exposes `GET /api/settings` (with top-level `actual_port`), `PUT /api/settings/<key>`, `POST /api/settings/<action>`, all `requireSameOrigin`-wrapped.
- [ ] AC-4: `internal/gui/backups.go` exposes `GET /api/backups` and `GET /api/backups/clean-preview?keep_n=N`.
- [ ] AC-5: `internal/gui/openpath.go` defines `OpenPath` reusing `spawnProcess` seam (NOT `LaunchBrowser`).
- [ ] AC-6: `internal/cli/settings.go` consults registry for `list` (annotated), `get` (exit-1 unknown), `set` (validation + deferred warning + action rejection).
- [ ] AC-7: Settings sidebar link in `app.tsx`; `#/settings` route works; active-link class.
- [ ] AC-8: `Settings.tsx` renders single-page layout with sticky `SectionNav`; deep-link `#/settings?section=<name>` scrolls via `URLSearchParams`.
- [ ] AC-9: Per-section Save/Reset on Appearance, GUI server, Backups. Daemons read-only. Advanced action button.
- [ ] AC-10: Per-section Save merge rule (success keys clean + refreshed; failed keys retain local + dirty + error).
- [ ] AC-11: Daemons read-only "Configured schedule (effective in A4-b)" labels rendered.
- [ ] AC-12: Port pending-restart badge anchored to `persistedPort` from snapshot (NOT local draft); survives reload.
- [ ] AC-13: `BackupsList` per-client groups + passive prune preview via `GET /api/backups/clean-preview`; all 6 verbatim Codex copy strings present.
- [ ] AC-14: "Open app-data folder" action calls `POST /api/settings/advanced.open_app_data_folder` → `OpenPath`.
- [ ] AC-15: App-level dirty-guard covers `settingsDirty`; sidebar nav prompts on dirty state. **Discard-key remount (Codex r2 P1, memo §10.4):** confirmed discard increments `discardKey`; `<SettingsScreen key={discardKey}>` forces full remount so every section's local draft state resets. E2E asserts intra-Settings hash navigation with confirm-discard reverts edits to persisted values.
- [ ] AC-16: `go generate ./internal/gui/...` regenerated bundle committed.
- [ ] AC-17: All Go unit, Vitest, and Playwright E2E pass on Windows.
- [ ] AC-18: CLAUDE.md updated with A4 surface description + new E2E count (92).
- [ ] AC-19: backlog row 9 marked done with memo + plan + merge SHA.

---

## Plan revision history

- **rev 1 (2026-04-27)** — first draft post-memo (Codex r1–r5 PASS). 11 tasks + closeout.

(Codex review iteration of this plan happens before subagent-driven execution; rev numbers update as findings land.)


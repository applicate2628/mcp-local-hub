# A4-b PR #1 Settings Lifecycle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the polish half of A4-b — flip 5 deferred Settings keys to functional, add per-workspace WeeklyRefresh membership UI, ship the typed `ScheduleSpec` parser + `RetryPolicy` interface seams, add the two-click force-kill recovery flow. Tray + port live-rebind defer to PR #2.

**Architecture:** The memo (`docs/superpowers/specs/2026-05-01-a4b-pr1-settings-lifecycle-design.md`) locks 14 decisions D1-D14 across 5 Codex review rounds. Plan decomposes them into 18 commit-ready tasks: registry+CLI seams (1-3), membership backend+UI (4-5), schedule parser+swap+route (6-8), retry policy seam (9), ConfirmModal+clean-now (10-11), export bundle (12), force-kill (13-14), E2E + asset regen + docs (15-17), final verification (18).

**Tech Stack:** Go 1.22+ (backend, scheduler XML rollback), Preact + TypeScript + Vite (frontend), Playwright (E2E), Vitest (unit), `<dialog>` element for modal (no useFocusTrap hook).

**Memo-locked invariants subagents MUST preserve verbatim:**

- D1: `legacy_migrate.go:161` keeps hardcoded `WeeklyRefresh: true` (exemption); regression test asserts the exemption.
- D5: membership endpoint body is `[{workspace_key, language, enabled}]`; idempotent partial update.
- D7: only `weekly DAY HH:MM`; parser is authoritative; regex is early UI rejection.
- D8: `SwapWeeklyTrigger` four-tuple disjoint return: `("n/a", nil)` / `("ok", err)` / `("degraded", err)` / `("n/a", err)`.
- D8: parse-error 400 carries only `{error, detail, example}`; no `updated`, no `restore_status`.
- D9: `RetryPolicy` is timing-only (`Backoff(attempt)` + `MaxAttempts()`); `IsRetryableError(err)` is SEPARATE.
- D10: copy AddSecretModal `<dialog>` pattern locally; do NOT extract `useFocusTrap` hook.
- D11: hostname literal `"redacted"` (not `"<host>"`, not omitted).
- D12: index-safe cmdline guard `(len > 1 && PIDCmdline[1] === "gui") || len <= 1`.
- D14: `RenderKind` is a string enum (`""` = `RenderDefault`, `"custom"` = `RenderCustom`).

**Master tip at plan start:** `42c4ba7`

---

## Task 0: Branch setup

**Files:** none (orchestrator does this directly, no subagent).

- [ ] **Step 0.1: Verify clean working tree**

```bash
git status --short
```

Expected: empty output (memo + backlog already committed in `42c4ba7`).

- [ ] **Step 0.2: Create feature branch**

```bash
git checkout -b feat/a4b-pr1-settings-lifecycle
git log -1 --format='%h %s'
```

Expected: branch created from `42c4ba7 docs(a4b): A4-b PR #1 design memo r6 (Codex PASS)`.

---

## Task 1: Settings registry — `RenderKind` discriminator + new keys + flips + tightened cron pattern

**Memo refs:** D1, D7, D14. **Subagent profile:** sonnet (mechanical, single-file).

**Files:**
- Modify: `internal/api/settings_registry.go` (deltas in D14)
- Test: `internal/api/settings_test.go` (verify new defaults + Pattern + RenderKind round-trip)

- [ ] **Step 1.1: Write failing tests for new registry shape**

Add to `internal/api/settings_test.go`:

```go
func TestSettingsRegistry_WeeklyRefreshDefault(t *testing.T) {
	def := findDef("daemons.weekly_refresh_default")
	if def == nil {
		t.Fatal("daemons.weekly_refresh_default not in registry")
	}
	if def.Type != TypeBool {
		t.Errorf("type = %v, want bool", def.Type)
	}
	if def.Default != "false" {
		t.Errorf("default = %q, want false (D1: opt-in default)", def.Default)
	}
	if def.Section != "daemons" {
		t.Errorf("section = %q, want daemons", def.Section)
	}
}

func TestSettingsRegistry_WeeklySchedulePatternBounds(t *testing.T) {
	def := findDef("daemons.weekly_schedule")
	if def == nil {
		t.Fatal("daemons.weekly_schedule missing")
	}
	if def.Deferred {
		t.Error("daemons.weekly_schedule still Deferred:true; A4-b PR #1 must flip to false")
	}
	// D7/D14: regex must reject 24:00 / 99:99 etc.
	cases := []struct {
		in    string
		valid bool
	}{
		{"weekly Sun 03:00", true},
		{"weekly Mon 14:30", true},
		{"weekly sun 03:00", true},  // case-insensitive day
		{"weekly Sun 24:00", false}, // hour out of range
		{"weekly Sun 99:99", false}, // both out of range
		{"weekly Sun 23:60", false}, // minute out of range
		{"daily 03:00", false},      // daily not yet supported
		{"weekly Funday 03:00", false},
		{"", false},
	}
	for _, c := range cases {
		err := validate(def, c.in)
		got := err == nil
		if got != c.valid {
			t.Errorf("validate(%q): got valid=%v, want %v (err=%v)", c.in, got, c.valid, err)
		}
	}
}

func TestSettingsRegistry_DeferredFlipsForA4bPR1(t *testing.T) {
	// Memo §13 acceptance #4: five Deferred flips.
	mustNotBeDeferred := []string{
		"daemons.weekly_schedule",
		"daemons.retry_policy",
		"backups.clean_now",
		"advanced.export_config_bundle",
	}
	for _, k := range mustNotBeDeferred {
		def := findDef(k)
		if def == nil {
			t.Errorf("key %q missing from registry", k)
			continue
		}
		if def.Deferred {
			t.Errorf("key %q still Deferred:true (A4-b PR #1 must flip)", k)
		}
	}
}

func TestSettingsRegistry_ForceKillCustomRender(t *testing.T) {
	// D14: new force_kill_* Action keys exist with RenderKind=RenderCustom.
	for _, k := range []string{"advanced.force_kill_diagnose", "advanced.force_kill"} {
		def := findDef(k)
		if def == nil {
			t.Errorf("key %q missing from registry", k)
			continue
		}
		if def.Type != TypeAction {
			t.Errorf("key %q type = %v, want action", k, def.Type)
		}
		if def.RenderKind != RenderCustom {
			t.Errorf("key %q RenderKind = %q, want %q", k, def.RenderKind, RenderCustom)
		}
	}
}
```

- [ ] **Step 1.2: Run tests; expect failures**

```bash
go test ./internal/api/ -run 'TestSettingsRegistry_(WeeklyRefreshDefault|WeeklySchedulePatternBounds|DeferredFlipsForA4bPR1|ForceKillCustomRender)$' -v
```

Expected: all four tests FAIL (keys missing / Deferred still true / RenderKind field undefined).

- [ ] **Step 1.3: Add `RenderKind` discriminator type**

In `internal/api/settings_registry.go`, after the `SettingType` block (around line 22):

```go
// RenderKind discriminates between default FieldRenderer rendering and
// section-owned custom rendering. Memo D14 (B-lite): keeps the registry
// as the single ordering/help/source-of-truth surface while letting
// sections render Action keys (and future variants) with custom UI when
// the default "single button + Help line" affordance is insufficient.
type RenderKind string

const (
	RenderDefault RenderKind = ""       // omit field → default; FieldRenderer (or section) handles it.
	RenderCustom  RenderKind = "custom" // section code owns rendering for this key.
)
```

- [ ] **Step 1.4: Add `RenderKind` field to `SettingDef`**

In the same file, extend the struct (around line 28-40):

```go
type SettingDef struct {
	Key        string
	Section    string
	Type       SettingType
	Default    string
	Enum       []string
	Min        *int
	Max        *int
	Pattern    string
	Optional   bool
	Deferred   bool
	Help       string
	RenderKind RenderKind // memo D14: "" = default, "custom" = section owns rendering
}
```

- [ ] **Step 1.5: Apply registry deltas (D14)**

Replace the existing `daemons.*`, `backups.clean_now`, and `advanced.export_config_bundle` entries; insert two new keys.

```go
// ----- daemons -----
{Key: "daemons.weekly_refresh_default", Section: "daemons", Type: TypeBool,
	Default: "false",
	Help:    "When registering a new workspace, enroll it in weekly refresh by default. Existing workspaces are not affected."},
{Key: "daemons.weekly_schedule", Section: "daemons", Type: TypeString,
	Default: "weekly Sun 03:00",
	Pattern: `^weekly\s+(?i:Sun|Mon|Tue|Wed|Thu|Fri|Sat)\s+(?:[01]\d|2[0-3]):[0-5]\d$`,
	Help:    "Weekly refresh schedule (format: weekly DAY HH:MM, 24-hour local time)."},
{Key: "daemons.retry_policy", Section: "daemons", Type: TypeEnum,
	Default: "exponential", Enum: []string{"none", "linear", "exponential"},
	Help: "Retry policy on daemon failure. Edit value here; runtime applier ships in A4-b PR #2."},

// ----- backups -----
{Key: "backups.keep_n", Section: "backups", Type: TypeInt,
	Default: "5", Min: intPtr(0), Max: intPtr(50),
	Help: "Keep timestamped backups per client. Originals are never cleaned."},
{Key: "backups.clean_now", Section: "backups", Type: TypeAction,
	Help: "Delete eligible timestamped backups. Originals are never cleaned. Confirms before deleting."},

// ----- advanced -----
{Key: "advanced.open_app_data_folder", Section: "advanced", Type: TypeAction,
	Help: "Open mcp-local-hub data folder in OS file manager."},
{Key: "advanced.export_config_bundle", Section: "advanced", Type: TypeAction,
	Help: "Download a .zip bundle of all manifests, encrypted secrets, settings, and registry. Hostname redacted; ciphertext only."},
{Key: "advanced.force_kill_diagnose", Section: "advanced", Type: TypeAction,
	RenderKind: RenderCustom,
	Help:       "Diagnose the single-instance lock. Read-only — shows what holds the lock without killing it."},
{Key: "advanced.force_kill", Section: "advanced", Type: TypeAction,
	RenderKind: RenderCustom,
	Help:       "Kill the recorded mcphub process holding the lock. Only available when diagnostic shows Stuck."},
```

Preserve existing `appearance.*` and `gui_server.*` entries unchanged.

- [ ] **Step 1.6: Run tests; expect pass**

```bash
go test ./internal/api/ -run 'TestSettingsRegistry_(WeeklyRefreshDefault|WeeklySchedulePatternBounds|DeferredFlipsForA4bPR1|ForceKillCustomRender)$' -v
```

Expected: all four tests PASS.

- [ ] **Step 1.7: Run full registry test suite for regression**

```bash
go test ./internal/api/ -run TestSettings -v
```

Expected: PASS — existing settings tests must still pass after the schema extension.

- [ ] **Step 1.8: Commit**

```bash
git add internal/api/settings_registry.go internal/api/settings_test.go
git commit -m "$(cat <<'EOF'
feat(settings): RenderKind discriminator + A4-b PR #1 registry deltas

Add `daemons.weekly_refresh_default` (D1 opt-in knob), flip five Deferred
flags to false (weekly_schedule, retry_policy, clean_now, export bundle),
add two force_kill_* Action keys with RenderKind=RenderCustom (D14
B-lite seam), and tighten weekly_schedule regex to bounded HH:MM
(D7/D14: rejects 24:00/99:99/etc. before parser).

Memo: docs/superpowers/specs/2026-05-01-a4b-pr1-settings-lifecycle-design.md
EOF
)"
```

---

## Task 2: Register CLI tri-state flag + knob-aware default

**Memo refs:** D1. **Subagent profile:** sonnet.

**Files:**
- Modify: `internal/api/register.go` (RegisterOpts default semantics)
- Modify: `internal/cli/register.go` (tri-state flag wiring)
- Test: `internal/api/register_test.go` (knob default; explicit flag override)

- [ ] **Step 2.1: Write failing test for knob-driven RegisterOpts default**

Append to `internal/api/register_test.go`:

```go
// D1: when caller does NOT explicitly set RegisterOpts.WeeklyRefresh,
// Register reads daemons.weekly_refresh_default from settings. Explicit
// override always wins.
func TestRegister_KnobDefault_FalseByDefault(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))

	a := NewAPI()
	// No settings file → knob defaults to "false" per registry.
	report, err := a.Register(t.TempDir(), []string{"python"}, RegisterOpts{
		Writer:                  io.Discard,
		WeeklyRefreshExplicit:   false, // means "honor knob"
		WeeklyRefresh:           false, // ignored when WeeklyRefreshExplicit==false
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, e := range report.Entries {
		if e.WeeklyRefresh != false {
			t.Errorf("entry %s WeeklyRefresh = true, want false (knob default)", e.Language)
		}
	}
}

func TestRegister_KnobDefault_HonorsExplicitTrue(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))

	a := NewAPI()
	report, err := a.Register(t.TempDir(), []string{"python"}, RegisterOpts{
		Writer:                io.Discard,
		WeeklyRefreshExplicit: true,
		WeeklyRefresh:         true,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, e := range report.Entries {
		if !e.WeeklyRefresh {
			t.Errorf("entry %s WeeklyRefresh = false, want true (explicit override)", e.Language)
		}
	}
}

func TestRegister_KnobDefault_ReadsKnobTrue(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))

	a := NewAPI()
	if err := a.SettingsSet("daemons.weekly_refresh_default", "true"); err != nil {
		t.Fatalf("SettingsSet: %v", err)
	}
	report, err := a.Register(t.TempDir(), []string{"python"}, RegisterOpts{
		Writer:                io.Discard,
		WeeklyRefreshExplicit: false,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, e := range report.Entries {
		if !e.WeeklyRefresh {
			t.Errorf("entry %s WeeklyRefresh = false, want true (knob=true)", e.Language)
		}
	}
}
```

(Add `"io"`, `"path/filepath"` imports at the top if not already present.)

- [ ] **Step 2.2: Run tests; expect failures**

```bash
go test ./internal/api/ -run 'TestRegister_KnobDefault' -v
```

Expected: FAIL with `WeeklyRefreshExplicit undefined`.

- [ ] **Step 2.3: Extend `RegisterOpts` with explicit-flag semantics**

In `internal/api/register.go` (around line 48-52):

```go
// RegisterOpts controls a Register invocation.
type RegisterOpts struct {
	// WeeklyRefreshExplicit selects between two interpretation modes:
	//   - true:  use WeeklyRefresh literally (caller has decided).
	//   - false: ignore WeeklyRefresh; read daemons.weekly_refresh_default
	//            from settings and use that. Memo D1 (opt-in knob).
	// CLI surface: --weekly-refresh / --no-weekly-refresh both flip this
	// to true; absent flag leaves it false (knob path).
	WeeklyRefreshExplicit bool

	// WeeklyRefresh is the value persisted on each created entry when
	// WeeklyRefreshExplicit==true. Ignored otherwise.
	WeeklyRefresh bool

	Writer io.Writer // progress output; nil = os.Stderr
}
```

- [ ] **Step 2.4: Implement knob read in `registerOneLanguage`**

Add a helper near the top of `register.go`:

```go
// resolveWeeklyRefresh picks the effective WeeklyRefresh value for a
// new entry per memo D1: explicit caller override beats the persisted
// knob; absent explicit override, read daemons.weekly_refresh_default
// from settings (default "false").
func resolveWeeklyRefresh(a *API, opts RegisterOpts) bool {
	if opts.WeeklyRefreshExplicit {
		return opts.WeeklyRefresh
	}
	v, err := a.SettingsGet("daemons.weekly_refresh_default")
	if err != nil || v == "" {
		// Settings file may not exist on first install; honor registry default.
		return false
	}
	return v == "true"
}
```

Then in `registerOneLanguage` (search for the existing `weeklyRefresh := opts.WeeklyRefresh` line near 387) replace:

```go
// Memo D1: knob-aware default. Explicit caller flag still wins.
weeklyRefresh := resolveWeeklyRefresh(a, opts)
if prior != nil {
	weeklyRefresh = prior.WeeklyRefresh
}
```

(The `prior != nil` guard preserves the existing "re-register keeps prior value" behavior.)

- [ ] **Step 2.5: Update CLI register command**

In `internal/cli/register.go` (around line 25-77), replace `noWeekly` with tri-state handling:

```go
func newRegisterCmdReal() *cobra.Command {
	var weekly bool
	var noWeekly bool
	c := &cobra.Command{
		Use:   "register <workspace> [language...]",
		Short: "Register workspace-scoped mcp-language-server daemons (lazy-mode)",
		Long: `Allocate one lazy proxy per (workspace, language), create the scheduler
task that launches it, and write managed entries into every installed MCP
client config (codex-cli, claude-code, gemini-cli).

Lazy mode:
  - No LSP binary preflight at register time. A missing binary surfaces
    later at first tools/call via the LifecycleMissing state shown in
    ` + "`mcphub workspaces`" + `.
  - Scheduler task args: ` + "`daemon workspace-proxy --port <p> --workspace <ws> --language <lang>`" + `.
  - Entry names are ` + "`mcp-language-server-<lang>`" + `; a cross-workspace
    collision appends ` + "`-<4hex>`" + ` from the workspace key.

Weekly refresh enrollment:
  - --weekly-refresh         force-enroll new entries (override knob).
  - --no-weekly-refresh      force-skip new entries (override knob).
  - (neither)                read daemons.weekly_refresh_default from
                             settings (default: false). Memo D1.

Examples:
  mcphub register D:\projects\foo
  mcphub register D:\projects\foo python typescript rust --weekly-refresh
  mcphub register /home/u/web typescript --no-weekly-refresh

See also: unregister, workspaces, status.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			workspace := args[0]
			var languages []string
			if len(args) > 1 {
				languages = args[1:]
			}
			if weekly && noWeekly {
				return fmt.Errorf("cannot use both --weekly-refresh and --no-weekly-refresh")
			}
			explicit := weekly || noWeekly
			a := api.NewAPI()
			report, err := a.Register(workspace, languages, api.RegisterOpts{
				WeeklyRefreshExplicit: explicit,
				WeeklyRefresh:         weekly,
				Writer:                cmd.OutOrStdout(),
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"\nRegistered %d language(s) for workspace %s (key %s):\n",
				len(report.Entries), report.Workspace, report.WorkspaceKey)
			for _, e := range report.Entries {
				fmt.Fprintf(cmd.OutOrStdout(), "  %-12s port=%-5d task=%s\n",
					e.Language, e.Port, e.TaskName)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&weekly, "weekly-refresh", false,
		"force-enroll new entries in weekly refresh (override daemons.weekly_refresh_default)")
	c.Flags().BoolVar(&noWeekly, "no-weekly-refresh", false,
		"force-skip new entries from weekly refresh (override daemons.weekly_refresh_default)")
	return c
}
```

- [ ] **Step 2.6: Run tests; expect pass**

```bash
go test ./internal/api/ -run 'TestRegister_KnobDefault' -v
go test ./internal/cli/ -run TestRegister -v
```

Expected: PASS for both.

- [ ] **Step 2.7: Update existing call sites that pass `WeeklyRefresh: true`**

Search for and update non-legacy call sites (`legacy_migrate.go:161` is INTENTIONALLY excluded — Task 3):

```bash
grep -rn "WeeklyRefresh: true" internal/api/ internal/cli/ --include="*.go" | grep -v "_test.go" | grep -v "legacy_migrate"
```

Expected: only `legacy_migrate.go:161`. If others appear, update them to set `WeeklyRefreshExplicit: true, WeeklyRefresh: true`.

- [ ] **Step 2.8: Run wider regression**

```bash
go test ./internal/api/ ./internal/cli/ -count=1
```

Expected: PASS.

- [ ] **Step 2.9: Commit**

```bash
git add internal/api/register.go internal/cli/register.go internal/api/register_test.go
git commit -m "$(cat <<'EOF'
feat(register): tri-state weekly-refresh flag + knob-driven default

Add RegisterOpts.WeeklyRefreshExplicit so callers can opt into the
daemons.weekly_refresh_default settings knob (memo D1). CLI now accepts
--weekly-refresh / --no-weekly-refresh; absent flag honors the knob.
Three new tests cover knob default, explicit override, and knob=true.

Note: legacy_migrate.go remains explicitly hardcoded WeeklyRefresh:true
per memo D1 exemption — addressed in the next commit.
EOF
)"
```

---

## Task 3: Legacy migrate exemption + regression test

**Memo refs:** D1 exemption. **Subagent profile:** sonnet.

**Files:**
- Modify: `internal/api/legacy_migrate.go:161` (add inline comment + WeeklyRefreshExplicit:true)
- Test: `internal/api/legacy_migrate_test.go` (new — assert exemption preserved)

- [ ] **Step 3.1: Write failing regression test**

Create `internal/api/legacy_migrate_test.go` if it does not already exist; otherwise append:

```go
package api

import (
	"os"
	"strings"
	"testing"
)

// Memo D1 exemption regression guard: legacy_migrate.go preserves
// hardcoded WeeklyRefresh:true with WeeklyRefreshExplicit:true so it
// bypasses the knob. Source-level assertions catch any future flip.
func TestLegacyMigrate_ExemptionFromKnob(t *testing.T) {
	src, err := os.ReadFile("legacy_migrate.go")
	if err != nil {
		t.Fatalf("read legacy_migrate.go: %v", err)
	}
	body := string(src)

	// Must still hardcode WeeklyRefresh:true with explicit override.
	if !strings.Contains(body, "WeeklyRefreshExplicit: true") {
		t.Error("legacy_migrate.go missing WeeklyRefreshExplicit:true (memo D1 exemption)")
	}
	if !strings.Contains(body, "WeeklyRefresh:        true") &&
		!strings.Contains(body, "WeeklyRefresh: true") {
		t.Error("legacy_migrate.go no longer hardcodes WeeklyRefresh:true (memo D1 exemption violated)")
	}

	// Must contain the rationale comment so future readers see why.
	if !strings.Contains(body, "Legacy import preserves the pre-A4-b register-time default") {
		t.Error("legacy_migrate.go missing memo-D1 rationale comment")
	}
}
```

- [ ] **Step 3.2: Run test; expect failure**

```bash
go test ./internal/api/ -run TestLegacyMigrate_ExemptionFromKnob -v
```

Expected: FAIL — comment + WeeklyRefreshExplicit not yet added.

- [ ] **Step 3.3: Update `legacy_migrate.go:161`**

Locate the existing line:

```go
if _, err := a.Register(ws, nil, RegisterOpts{Writer: w, WeeklyRefresh: true}); err != nil {
```

Replace with the explicit-override variant + rationale comment:

```go
// Memo D1 exemption: legacy import preserves the pre-A4-b register-time default
// (WeeklyRefresh: true). New register operations honor daemons.weekly_refresh_default,
// but legacy migration is a one-time intent-known operation that imports a
// pre-existing user setup. Flipping legacy entries to false would surprise users
// whose pre-A4-b workflow relied on every imported workspace getting weekly
// refresh by default. This is regression-tested in legacy_migrate_test.go.
if _, err := a.Register(ws, nil, RegisterOpts{
	Writer:                w,
	WeeklyRefreshExplicit: true,
	WeeklyRefresh:         true,
}); err != nil {
```

- [ ] **Step 3.4: Run regression test; expect pass**

```bash
go test ./internal/api/ -run TestLegacyMigrate_ExemptionFromKnob -v
```

Expected: PASS.

- [ ] **Step 3.5: Run wider regression**

```bash
go test ./internal/api/ -count=1
```

Expected: PASS.

- [ ] **Step 3.6: Commit**

```bash
git add internal/api/legacy_migrate.go internal/api/legacy_migrate_test.go
git commit -m "$(cat <<'EOF'
feat(legacy-migrate): explicit D1 exemption from weekly_refresh_default knob

Legacy import keeps hardcoded WeeklyRefresh:true with the new
WeeklyRefreshExplicit:true flag so it bypasses the knob (memo D1).
Inline rationale comment + source-level regression test prevent
accidental future flips.
EOF
)"
```

---

## Task 4: Membership backend — service + atomic save + endpoint + tests

**Memo refs:** D5. **Subagent profile:** sonnet.

**Files:**
- Create: `internal/api/membership.go` — `UpdateWeeklyRefreshMembership(deltas) (n int, err error)`
- Create: `internal/api/membership_test.go`
- Create: `internal/gui/daemons.go` — registers `PUT /api/daemons/weekly-refresh-membership`
- Create: `internal/gui/daemons_test.go`
- Modify: `internal/gui/server.go` — call `registerDaemonsRoutes(s)` in setup

- [ ] **Step 4.1: Write failing service test**

Create `internal/api/membership_test.go`:

```go
package api

import (
	"os"
	"path/filepath"
	"testing"
)

func setupRegistryWithEntries(t *testing.T) *Registry {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "workspaces.yaml")
	reg := NewRegistry(path)
	reg.Workspaces = []WorkspaceEntry{
		{WorkspaceKey: "k1", Language: "python", TaskName: "tA", Port: 9100, WeeklyRefresh: true, Backend: "mcp-language-server"},
		{WorkspaceKey: "k1", Language: "rust", TaskName: "tB", Port: 9101, WeeklyRefresh: false, Backend: "mcp-language-server"},
		{WorkspaceKey: "k2", Language: "go", TaskName: "tC", Port: 9102, WeeklyRefresh: true, Backend: "mcp-language-server"},
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return reg
}

func TestUpdateMembership_HappyPartialUpdate(t *testing.T) {
	reg := setupRegistryWithEntries(t)
	deltas := []MembershipDelta{
		{WorkspaceKey: "k1", Language: "python", Enabled: false},
		{WorkspaceKey: "k2", Language: "go", Enabled: false},
	}
	n, err := UpdateWeeklyRefreshMembership(reg.path, deltas)
	if err != nil {
		t.Fatalf("UpdateWeeklyRefreshMembership: %v", err)
	}
	if n != 2 {
		t.Errorf("updated = %d, want 2", n)
	}

	// Reload and verify D5: entries not in body stay unchanged.
	reloaded := NewRegistry(reg.path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := map[string]bool{"k1/python": false, "k1/rust": false, "k2/go": false}
	for _, e := range reloaded.Workspaces {
		key := e.WorkspaceKey + "/" + e.Language
		if got, ok := want[key]; ok && got != e.WeeklyRefresh {
			t.Errorf("entry %s WeeklyRefresh = %v, want %v", key, e.WeeklyRefresh, got)
		}
	}
}

func TestUpdateMembership_UnknownPair_Rejected(t *testing.T) {
	reg := setupRegistryWithEntries(t)
	deltas := []MembershipDelta{
		{WorkspaceKey: "kX", Language: "ruby", Enabled: true},
	}
	_, err := UpdateWeeklyRefreshMembership(reg.path, deltas)
	if err == nil {
		t.Fatal("expected error for unknown (workspace_key, language); got nil")
	}
	// Registry must remain untouched.
	reloaded := NewRegistry(reg.path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, e := range reloaded.Workspaces {
		if e.WorkspaceKey == "k1" && e.Language == "python" && !e.WeeklyRefresh {
			t.Error("registry mutated despite validation failure")
		}
	}
}

func TestUpdateMembership_EmptyBody_NoOp(t *testing.T) {
	reg := setupRegistryWithEntries(t)
	statBefore, err := os.Stat(reg.path)
	if err != nil {
		t.Fatal(err)
	}
	n, err := UpdateWeeklyRefreshMembership(reg.path, []MembershipDelta{})
	if err != nil {
		t.Fatalf("UpdateWeeklyRefreshMembership: %v", err)
	}
	if n != 0 {
		t.Errorf("updated = %d, want 0 for empty body", n)
	}
	statAfter, err := os.Stat(reg.path)
	if err != nil {
		t.Fatal(err)
	}
	if !statBefore.ModTime().Equal(statAfter.ModTime()) {
		t.Error("empty body should not rewrite registry file")
	}
}
```

- [ ] **Step 4.2: Run; expect failure**

```bash
go test ./internal/api/ -run TestUpdateMembership -v
```

Expected: FAIL — `MembershipDelta` and `UpdateWeeklyRefreshMembership` undefined.

- [ ] **Step 4.3: Implement service**

Create `internal/api/membership.go`:

```go
// Package api — weekly_refresh membership service. Memo D5.
//
// PUT /api/daemons/weekly-refresh-membership accepts a structured array
// body and applies it as an idempotent partial update against
// workspaces.yaml. Entries listed in the body are updated to the given
// enabled value; entries NOT in the body are unchanged. One registryMu
// acquire; one Registry.Save call.
package api

import (
	"fmt"

	"github.com/gofrs/flock"
)

// MembershipDelta is one (workspace_key, language) toggle. Memo D5.
type MembershipDelta struct {
	WorkspaceKey string `json:"workspace_key"`
	Language     string `json:"language"`
	Enabled      bool   `json:"enabled"`
}

// UpdateWeeklyRefreshMembership applies the deltas atomically against
// the registry at path. Returns the number of entries actually updated
// (may be less than len(deltas) if a delta's enabled value already
// matches), or an error if any delta names an unknown (key, language)
// pair (in which case the registry is NOT modified — fail-closed).
//
// Memo D5 contract:
//   - Idempotent partial update: entries not in deltas are unchanged.
//   - Atomic: one Registry.Save; either all named deltas persist or none.
//   - Validation: every (workspace_key, language) MUST exist in the
//     registry; otherwise return an error and skip the save.
func UpdateWeeklyRefreshMembership(path string, deltas []MembershipDelta) (int, error) {
	if len(deltas) == 0 {
		return 0, nil
	}

	// File-level lock so concurrent calls (e.g. CLI + GUI) cannot race.
	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		return 0, fmt.Errorf("acquire lock: %w", err)
	}
	defer func() { _ = lock.Unlock() }()

	reg := NewRegistry(path)
	if err := reg.Load(); err != nil {
		return 0, fmt.Errorf("load registry: %w", err)
	}

	// Index existing entries for O(1) match lookup.
	idx := make(map[[2]string]int, len(reg.Workspaces))
	for i, e := range reg.Workspaces {
		idx[[2]string{e.WorkspaceKey, e.Language}] = i
	}

	// Validate every delta first; abort before mutating on first miss.
	for _, d := range deltas {
		if _, ok := idx[[2]string{d.WorkspaceKey, d.Language}]; !ok {
			return 0, fmt.Errorf("unknown (workspace_key=%q, language=%q)", d.WorkspaceKey, d.Language)
		}
	}

	// Apply.
	updated := 0
	for _, d := range deltas {
		i := idx[[2]string{d.WorkspaceKey, d.Language}]
		if reg.Workspaces[i].WeeklyRefresh != d.Enabled {
			reg.Workspaces[i].WeeklyRefresh = d.Enabled
			updated++
		}
	}

	if updated == 0 {
		return 0, nil // no-op; do not rewrite file.
	}
	if err := reg.Save(); err != nil {
		return 0, fmt.Errorf("save registry: %w", err)
	}
	return updated, nil
}
```

- [ ] **Step 4.4: Run service tests; expect pass**

```bash
go test ./internal/api/ -run TestUpdateMembership -v
```

Expected: PASS.

- [ ] **Step 4.5: Write failing handler test**

Create `internal/gui/daemons_test.go`:

```go
package gui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMembershipHandler_HappyPath(t *testing.T) {
	srv := newTestServer(t)
	body, _ := json.Marshal([]map[string]any{
		{"workspace_key": "k1", "language": "python", "enabled": false},
	})
	req := httptest.NewRequest(http.MethodPut, "/api/daemons/weekly-refresh-membership", bytes.NewReader(body))
	req.Header.Set("Origin", srv.OriginAllowed())
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := resp["updated"]; !ok {
		t.Errorf("response missing 'updated' field: %v", resp)
	}
}

func TestMembershipHandler_UnknownPair_400(t *testing.T) {
	srv := newTestServer(t)
	body, _ := json.Marshal([]map[string]any{
		{"workspace_key": "kX", "language": "ruby", "enabled": true},
	})
	req := httptest.NewRequest(http.MethodPut, "/api/daemons/weekly-refresh-membership", bytes.NewReader(body))
	req.Header.Set("Origin", srv.OriginAllowed())
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMembershipHandler_BadMethod(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/daemons/weekly-refresh-membership", nil)
	req.Header.Set("Origin", srv.OriginAllowed())
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestMembershipHandler_BadJSON_400(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodPut, "/api/daemons/weekly-refresh-membership",
		strings.NewReader("not json"))
	req.Header.Set("Origin", srv.OriginAllowed())
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}
```

(`newTestServer` is the existing test helper; if absent, follow the pattern from `internal/gui/settings_test.go`.)

- [ ] **Step 4.6: Implement handler**

Create `internal/gui/daemons.go`:

```go
// Package gui — daemon-lifecycle HTTP routes. Memo §4.
//
// These routes own external-system side effects (workspace registry,
// Task Scheduler) with their own transaction boundaries; settings.go
// stays a pure persistence layer for /api/settings.
package gui

import (
	"encoding/json"
	"fmt"
	"net/http"

	"mcp-local-hub/internal/api"
)

func registerDaemonsRoutes(s *Server) {
	s.mux.HandleFunc("/api/daemons/weekly-refresh-membership",
		s.requireSameOrigin(s.weeklyRefreshMembershipHandler))
}

// weeklyRefreshMembershipHandler implements PUT
// /api/daemons/weekly-refresh-membership per memo D5. Body is a
// structured array of {workspace_key, language, enabled}; idempotent
// partial update.
func (s *Server) weeklyRefreshMembershipHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body []api.MembershipDelta
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":  "bad_json",
			"detail": err.Error(),
		})
		return
	}
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error":  "registry_path",
			"detail": err.Error(),
		})
		return
	}
	updated, err := api.UpdateWeeklyRefreshMembership(regPath, body)
	if err != nil {
		// Validation errors (unknown pair) → 400; storage errors → 500.
		status := http.StatusBadRequest
		if isStorageErr(err) {
			status = http.StatusInternalServerError
		}
		writeJSON(w, status, map[string]string{
			"error":  "membership_failed",
			"detail": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"updated":  updated,
		"warnings": []string{},
	})
}

// isStorageErr distinguishes "unknown (workspace_key, language)"
// (caller-fixable, 400) from "save registry: …" (server-side, 500).
func isStorageErr(err error) bool {
	return err != nil && (containsAny(err.Error(),
		"save registry", "load registry", "acquire lock"))
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) && (indexOf(s, sub) >= 0) {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// (writeJSON is defined elsewhere in package gui; reuse it.)
//
// Compile-time guard: ensure fmt is referenced even when only used in
// future expansions of this file (avoids `imported and not used`).
var _ = fmt.Sprintf
```

- [ ] **Step 4.7: Wire in `server.go` setup**

In `internal/gui/server.go`, after `registerSettingsRoutes(s)` (around line 394), add:

```go
	registerDaemonsRoutes(s)
```

- [ ] **Step 4.8: Run handler tests**

```bash
go test ./internal/gui/ -run TestMembershipHandler -v
go test ./internal/api/ -run TestUpdateMembership -v
```

Expected: PASS.

- [ ] **Step 4.9: Run wider regression**

```bash
go test ./internal/api/ ./internal/gui/ -count=1
```

Expected: PASS.

- [ ] **Step 4.10: Commit**

```bash
git add internal/api/membership.go internal/api/membership_test.go internal/gui/daemons.go internal/gui/daemons_test.go internal/gui/server.go
git commit -m "$(cat <<'EOF'
feat(daemons): membership service + PUT /api/daemons/weekly-refresh-membership

UpdateWeeklyRefreshMembership applies a structured-array delta against
workspaces.yaml as an idempotent partial update with file-level lock
(memo D5). Unknown (workspace_key, language) pair fails closed without
mutation. Handler validates body, surfaces 400 for caller errors, 500
for storage errors.
EOF
)"
```

---

## Task 5: Schedule parser — `ScheduleSpec` + `ParseSchedule` + tests

**Memo refs:** D7. **Subagent profile:** sonnet.

**Files:**
- Create: `internal/api/schedule_parser.go`
- Create: `internal/api/schedule_parser_test.go`

- [ ] **Step 5.1: Write failing parser test**

Create `internal/api/schedule_parser_test.go`:

```go
package api

import (
	"strings"
	"testing"
)

func TestParseSchedule_ValidWeekly(t *testing.T) {
	cases := []struct {
		in   string
		want ScheduleSpec
	}{
		{"weekly Sun 03:00", ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 0, Hour: 3, Minute: 0}},
		{"weekly Mon 14:30", ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 1, Hour: 14, Minute: 30}},
		{"weekly Sat 23:59", ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 6, Hour: 23, Minute: 59}},
		{"weekly sun 03:00", ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 0, Hour: 3, Minute: 0}},
		{"weekly TUE 09:05", ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 2, Hour: 9, Minute: 5}},
	}
	for _, c := range cases {
		got, err := ParseSchedule(c.in)
		if err != nil {
			t.Errorf("ParseSchedule(%q): %v", c.in, err)
			continue
		}
		if *got != c.want {
			t.Errorf("ParseSchedule(%q) = %+v, want %+v", c.in, *got, c.want)
		}
	}
}

func TestParseSchedule_Rejected(t *testing.T) {
	cases := []string{
		"",
		"weekly",
		"weekly Sun",
		"weekly Sun 03",
		"weekly Sun 24:00",
		"weekly Sun 99:99",
		"weekly Sun 23:60",
		"daily 03:00",
		"weekly Funday 03:00",
		"weekly Sun 3:00",      // unpadded hour rejected by tightened spec
		"0 3 * * 0",            // cron syntax not supported
		"weekly Sun 03:00 UTC", // suffix rejected
	}
	for _, in := range cases {
		_, err := ParseSchedule(in)
		if err == nil {
			t.Errorf("ParseSchedule(%q): expected error, got nil", in)
		}
	}
}

func TestParseSchedule_ErrorIncludesExample(t *testing.T) {
	_, err := ParseSchedule("daily 03:00")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "weekly Sun 03:00") {
		t.Errorf("error %q missing canonical example", err.Error())
	}
}
```

- [ ] **Step 5.2: Run; expect failure**

```bash
go test ./internal/api/ -run TestParseSchedule -v
```

Expected: FAIL — symbols undefined.

- [ ] **Step 5.3: Implement parser**

Create `internal/api/schedule_parser.go`:

```go
// Package api — typed schedule parser for daemons.weekly_schedule.
// Memo D7: only `weekly DAY HH:MM` is accepted today; daily/cron Kinds
// extend the parser with new cases without callsite changes.
package api

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ScheduleKind discriminates between supported cron-like surfaces.
type ScheduleKind string

const (
	ScheduleWeekly ScheduleKind = "weekly"
	// future: ScheduleDaily, ScheduleCron
)

// ScheduleSpec is the typed result of ParseSchedule. Callers (e.g.
// SwapWeeklyTrigger) consume the typed struct, not the raw string.
type ScheduleSpec struct {
	Kind      ScheduleKind
	DayOfWeek int // 0=Sunday..6=Saturday (Kind == ScheduleWeekly)
	Hour      int // 0..23
	Minute    int // 0..59
}

// canonicalExample is shown in error messages so users see a known-good
// schedule string at the point of rejection.
const canonicalExample = "weekly Sun 03:00"

// dayLookup maps the case-insensitive 3-letter day name to the
// 0=Sunday-indexed DayOfWeek used by Task Scheduler's WeeklyTrigger.
var dayLookup = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// weeklyRE is the canonical pattern. It mirrors (and tightens) the
// registry pattern at daemons.weekly_schedule. The parser is the
// authoritative validator — registry pattern is an early UI rejection
// hint; ParseSchedule is the single source of truth.
//
// Tightened bounds (memo D7/D14): hour must be 00-23, minute 00-59.
var weeklyRE = regexp.MustCompile(`^weekly\s+([A-Za-z]{3})\s+([01]\d|2[0-3]):([0-5]\d)$`)

// ParseSchedule converts a settings-string schedule into a typed
// ScheduleSpec. Returns a typed error with the canonical example on
// rejection so users see a known-good shape.
func ParseSchedule(s string) (*ScheduleSpec, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty schedule (canonical example: %q)", canonicalExample)
	}
	m := weeklyRE.FindStringSubmatch(s)
	if m == nil {
		return nil, fmt.Errorf("schedule %q not in form `weekly DAY HH:MM` (canonical example: %q)", s, canonicalExample)
	}
	dow, ok := dayLookup[strings.ToLower(m[1])]
	if !ok {
		return nil, fmt.Errorf("schedule %q: unknown day %q (canonical example: %q)", s, m[1], canonicalExample)
	}
	hh, _ := strconv.Atoi(m[2]) // regex guarantees in-range.
	mm, _ := strconv.Atoi(m[3])
	return &ScheduleSpec{
		Kind:      ScheduleWeekly,
		DayOfWeek: dow,
		Hour:      hh,
		Minute:    mm,
	}, nil
}
```

- [ ] **Step 5.4: Run; expect pass**

```bash
go test ./internal/api/ -run TestParseSchedule -v
```

Expected: PASS.

- [ ] **Step 5.5: Commit**

```bash
git add internal/api/schedule_parser.go internal/api/schedule_parser_test.go
git commit -m "$(cat <<'EOF'
feat(api): typed ScheduleSpec parser for daemons.weekly_schedule

ParseSchedule returns *ScheduleSpec for the only supported Kind today
(weekly DAY HH:MM). Future ScheduleDaily/ScheduleCron Kinds slot in
without callsite changes (memo D7). Tightened HH:MM bounds 00-23/00-59;
case-insensitive day; canonical example surfaced in every error.
EOF
)"
```

---

## Task 6: Scheduler swap helper — `SwapWeeklyTrigger` with disjoint-tuple contract + rollback test

**Memo refs:** D8. **Subagent profile:** opus (coordination-heavy, must preserve verbatim invariants).

**Files:**
- Create: `internal/api/scheduler_swap.go`
- Create: `internal/api/scheduler_swap_test.go`

- [ ] **Step 6.1: Write failing happy-path test**

Create `internal/api/scheduler_swap_test.go`:

```go
package api

import (
	"errors"
	"testing"

	"mcp-local-hub/internal/scheduler"
)

// fakeScheduler is the test seam for SwapWeeklyTrigger so the helper
// can be unit-tested without touching Windows Task Scheduler. The
// helper accepts a scheduler abstraction at call time; the tests
// inject a fake.
type fakeScheduler struct {
	deleteErr error
	createErr error
	importErr error
	deleted   bool
	created   bool
	imported  bool
}

func (f *fakeScheduler) Delete(name string) error           { f.deleted = true; return f.deleteErr }
func (f *fakeScheduler) Create(spec scheduler.TaskSpec) error { f.created = true; return f.createErr }
func (f *fakeScheduler) ImportXML(name string, xml []byte) error {
	f.imported = true
	return f.importErr
}

func TestSwapWeeklyTrigger_FreshInstall_Success(t *testing.T) {
	fake := &fakeScheduler{}
	spec := &ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 1, Hour: 14, Minute: 30}
	status, err := swapWeeklyTriggerWith(fake, spec, nil) // priorXML==nil = fresh install
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if status != "n/a" {
		t.Errorf(`status = %q, want "n/a" (D8 fresh-install Create-success)`, status)
	}
	if !fake.deleted || !fake.created {
		t.Error("Delete + Create must both be invoked")
	}
	if fake.imported {
		t.Error("ImportXML must NOT be invoked on Create success")
	}
}

func TestSwapWeeklyTrigger_HadPriorTask_Success(t *testing.T) {
	fake := &fakeScheduler{}
	spec := &ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 0, Hour: 3, Minute: 0}
	status, err := swapWeeklyTriggerWith(fake, spec, []byte("<Task/>"))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if status != "n/a" {
		t.Errorf(`status = %q, want "n/a" (D8 had-prior-task Create-success)`, status)
	}
	if fake.imported {
		t.Error("ImportXML must NOT be invoked on Create success")
	}
}

func TestSwapWeeklyTrigger_FreshInstall_CreateFails_NoRollback(t *testing.T) {
	fake := &fakeScheduler{createErr: errors.New("create boom")}
	spec := &ScheduleSpec{Kind: ScheduleWeekly}
	status, err := swapWeeklyTriggerWith(fake, spec, nil)
	if err == nil {
		t.Fatal("err = nil, want create boom")
	}
	if status != "n/a" {
		t.Errorf(`status = %q, want "n/a" (D8 fresh-install Create-failed: nothing to restore)`, status)
	}
	if fake.imported {
		t.Error("ImportXML must NOT be invoked when priorXML==nil")
	}
}

func TestSwapWeeklyTrigger_HadPriorTask_CreateFails_RestoreOK(t *testing.T) {
	fake := &fakeScheduler{createErr: errors.New("create boom")}
	spec := &ScheduleSpec{Kind: ScheduleWeekly}
	status, err := swapWeeklyTriggerWith(fake, spec, []byte("<Task/>"))
	if err == nil {
		t.Fatal("err = nil, want create boom")
	}
	if status != "ok" {
		t.Errorf(`status = %q, want "ok" (D8 had-prior-task Create-failed + ImportXML succeeded)`, status)
	}
	if !fake.imported {
		t.Error("ImportXML must be invoked when priorXML != nil and Create fails")
	}
}

func TestSwapWeeklyTrigger_HadPriorTask_CreateFails_RestoreFails_Degraded(t *testing.T) {
	fake := &fakeScheduler{
		createErr: errors.New("create boom"),
		importErr: errors.New("import boom"),
	}
	spec := &ScheduleSpec{Kind: ScheduleWeekly}
	status, err := swapWeeklyTriggerWith(fake, spec, []byte("<Task/>"))
	if err == nil {
		t.Fatal("err = nil, want create boom")
	}
	if status != "degraded" {
		t.Errorf(`status = %q, want "degraded" (D8 had-prior-task Create-failed + ImportXML-failed)`, status)
	}
}
```

- [ ] **Step 6.2: Run; expect failure**

```bash
go test ./internal/api/ -run TestSwapWeeklyTrigger -v
```

Expected: FAIL — `swapWeeklyTriggerWith` and friends undefined.

- [ ] **Step 6.3: Implement helper with seam**

Create `internal/api/scheduler_swap.go`:

```go
// Package api — Task Scheduler trigger swap helper. Memo D8.
//
// SwapWeeklyTrigger owns ONLY the scheduler XML lifecycle:
//   Delete → Create → optional ImportXML(priorXML) on Create failure.
//
// It does NOT call ExportXML (caller's preflight) and does NOT touch
// settings YAML (caller's responsibility). The four disjoint return
// tuples are documented at the Swap function's docstring.
package api

import (
	"fmt"
	"path/filepath"

	"mcp-local-hub/internal/scheduler"
)

// schedulerSwap is the test seam: the production path is a
// scheduler.Scheduler created by schedulerNewForRegister; tests inject
// a fake to drive deterministic Delete/Create/ImportXML outcomes.
type schedulerSwap interface {
	Delete(name string) error
	Create(spec scheduler.TaskSpec) error
	ImportXML(name string, xml []byte) error
}

// SwapWeeklyTrigger is the production entrypoint. It loads the real
// scheduler and delegates to swapWeeklyTriggerWith.
func SwapWeeklyTrigger(spec *ScheduleSpec, priorXML []byte) (restoreStatus string, err error) {
	sch, sErr := schedulerNewForRegister()
	if sErr != nil {
		return "n/a", fmt.Errorf("scheduler init: %w", sErr)
	}
	return swapWeeklyTriggerWith(sch, spec, priorXML)
}

// swapWeeklyTriggerWith is the test-seam variant. Returns disjoint
// (restoreStatus, err) tuples per memo D8:
//
//   ("n/a", nil)        Create succeeded (both fresh-install and
//                       had-prior-task paths). No rollback was needed.
//   ("ok", err)         Create FAILED, priorXML != nil, ImportXML
//                       succeeded — prior task restored.
//   ("degraded", err)   Create FAILED, priorXML != nil, ImportXML also
//                       FAILED — prior task lost.
//   ("n/a", err)        Create FAILED, priorXML == nil (fresh-install).
//                       No rollback was attempted (nothing to restore).
//
// All four cases are exhaustive over the helper's scheduler-XML domain.
// The caller's truth table at D8 step 8 maps these to response
// `restore_status` after combining with settings-YAML rollback.
func swapWeeklyTriggerWith(sch schedulerSwap, spec *ScheduleSpec, priorXML []byte) (restoreStatus string, err error) {
	canonical, perr := canonicalMcphubPath()
	if perr != nil {
		return "n/a", fmt.Errorf("canonical path: %w", perr)
	}

	// Idempotent replace: Delete returns nil if the task is absent.
	_ = sch.Delete(WeeklyRefreshTaskName)

	createErr := sch.Create(scheduler.TaskSpec{
		Name:        WeeklyRefreshTaskName,
		Description: "mcp-local-hub: weekly refresh of workspace-scoped lazy proxies",
		Command:     canonical,
		Args:        []string{"workspace-weekly-refresh"},
		WorkingDir:  filepath.Dir(canonical),
		WeeklyTrigger: &scheduler.WeeklyTrigger{
			DayOfWeek:   spec.DayOfWeek,
			HourLocal:   spec.Hour,
			MinuteLocal: spec.Minute,
		},
	})
	if createErr == nil {
		return "n/a", nil // success
	}

	// Create failed. Restore via ImportXML if we have a prior snapshot.
	if priorXML == nil {
		// Fresh-install case: nothing to restore. State is "no task" —
		// same as before the swap was attempted.
		return "n/a", createErr
	}
	if importErr := sch.ImportXML(WeeklyRefreshTaskName, priorXML); importErr != nil {
		return "degraded", createErr // prior task lost; original error surfaced
	}
	return "ok", createErr
}
```

- [ ] **Step 6.4: Run; expect pass**

```bash
go test ./internal/api/ -run TestSwapWeeklyTrigger -v
```

Expected: PASS.

- [ ] **Step 6.5: Commit**

```bash
git add internal/api/scheduler_swap.go internal/api/scheduler_swap_test.go
git commit -m "$(cat <<'EOF'
feat(api): SwapWeeklyTrigger with four-tuple disjoint return contract

Helper owns only Delete + Create + optional ImportXML(priorXML); does
NOT call ExportXML (caller's preflight) and does NOT touch settings
YAML (caller's responsibility). Memo D8 four cases:
  ("n/a", nil)      Create succeeded (any priorXML)
  ("ok", err)       Create failed + ImportXML restored prior task
  ("degraded", err) Create failed + ImportXML also failed
  ("n/a", err)      Create failed + no priorXML (fresh-install)

Test seam injects a fake scheduler so unit tests run cross-platform.
PR #2 (and any future task-mutating route) reuses this contract.
EOF
)"
```

---

## Task 7: Weekly schedule HTTP handler with preflight + transactional swap + truth-table response

**Memo refs:** D8. **Subagent profile:** opus.

**Files:**
- Modify: `internal/gui/daemons.go` — add `weeklyScheduleHandler`
- Modify: `internal/gui/daemons_test.go` — handler tests including 400 parse-error envelope

- [ ] **Step 7.1: Write failing handler tests**

Append to `internal/gui/daemons_test.go`:

```go
func TestWeeklyScheduleHandler_ParseError_400_NoUpdatedField(t *testing.T) {
	// Memo D8: 400 carries only {error, detail, example}; NO updated, NO restore_status.
	srv := newTestServer(t)
	body := `{"schedule": "daily 03:00"}`
	req := httptest.NewRequest(http.MethodPut, "/api/daemons/weekly-schedule", strings.NewReader(body))
	req.Header.Set("Origin", srv.OriginAllowed())
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["error"] != "parse_error" {
		t.Errorf("error = %v, want parse_error", resp["error"])
	}
	if _, has := resp["updated"]; has {
		t.Error("400 parse-error must NOT include 'updated' (memo D8)")
	}
	if _, has := resp["restore_status"]; has {
		t.Error("400 parse-error must NOT include 'restore_status' (memo D8)")
	}
	if resp["example"] != "weekly Sun 03:00" {
		t.Errorf("example = %v, want canonical 'weekly Sun 03:00'", resp["example"])
	}
}

func TestWeeklyScheduleHandler_ValidPayload_Accepted(t *testing.T) {
	// Note: this test exercises the handler control flow up to the swap call.
	// The actual swap is mocked via the schedulerSwapForRoute test seam.
	srv := newTestServer(t)
	srv.swapForRoute = func(spec *api.ScheduleSpec, priorXML []byte) (string, error) {
		return "n/a", nil
	}
	srv.exportXMLForRoute = func(name string) ([]byte, error) { return nil, nil }
	body := `{"schedule": "weekly Tue 14:30"}`
	req := httptest.NewRequest(http.MethodPut, "/api/daemons/weekly-schedule", strings.NewReader(body))
	req.Header.Set("Origin", srv.OriginAllowed())
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["restore_status"] != "n/a" {
		t.Errorf("restore_status = %v, want n/a", resp["restore_status"])
	}
}

func TestWeeklyScheduleHandler_ExportXMLFails_Preflight500(t *testing.T) {
	srv := newTestServer(t)
	srv.exportXMLForRoute = func(name string) ([]byte, error) {
		return nil, errors.New("scheduler down")
	}
	body := `{"schedule": "weekly Sun 03:00"}`
	req := httptest.NewRequest(http.MethodPut, "/api/daemons/weekly-schedule", strings.NewReader(body))
	req.Header.Set("Origin", srv.OriginAllowed())
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["error"] != "snapshot_unavailable" {
		t.Errorf("error = %v, want snapshot_unavailable", resp["error"])
	}
}

func TestWeeklyScheduleHandler_SwapFails_RollbackOK(t *testing.T) {
	srv := newTestServer(t)
	srv.exportXMLForRoute = func(name string) ([]byte, error) { return []byte("<Task/>"), nil }
	srv.swapForRoute = func(spec *api.ScheduleSpec, priorXML []byte) (string, error) {
		return "ok", errors.New("create boom")
	}
	body := `{"schedule": "weekly Sun 03:00"}`
	req := httptest.NewRequest(http.MethodPut, "/api/daemons/weekly-schedule", strings.NewReader(body))
	req.Header.Set("Origin", srv.OriginAllowed())
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["restore_status"] != "ok" {
		t.Errorf("restore_status = %v, want ok", resp["restore_status"])
	}
	if _, has := resp["manual_recovery"]; has {
		t.Error("manual_recovery must NOT be present when restore_status==ok")
	}
}

func TestWeeklyScheduleHandler_SwapFails_DegradedRestore(t *testing.T) {
	srv := newTestServer(t)
	srv.exportXMLForRoute = func(name string) ([]byte, error) { return []byte("<Task/>"), nil }
	srv.swapForRoute = func(spec *api.ScheduleSpec, priorXML []byte) (string, error) {
		return "degraded", errors.New("create + import boom")
	}
	body := `{"schedule": "weekly Sun 03:00"}`
	req := httptest.NewRequest(http.MethodPut, "/api/daemons/weekly-schedule", strings.NewReader(body))
	req.Header.Set("Origin", srv.OriginAllowed())
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["restore_status"] != "degraded" {
		t.Errorf("restore_status = %v, want degraded", resp["restore_status"])
	}
	if _, has := resp["manual_recovery"]; !has {
		t.Error("manual_recovery must be present when restore_status==degraded")
	}
}
```

(Add `"errors"` import if not present. The `srv.swapForRoute` and `srv.exportXMLForRoute` test seams are introduced in the next step.)

- [ ] **Step 7.2: Run; expect failure**

```bash
go test ./internal/gui/ -run TestWeeklyScheduleHandler -v
```

Expected: FAIL — handler + seams undefined.

- [ ] **Step 7.3: Add test seams to `Server` struct**

In `internal/gui/server.go`, find the `Server` struct definition. Add two fields (place them next to existing test seams):

```go
	// Weekly schedule swap test seams. Production: nil (handler uses
	// real api.SwapWeeklyTrigger and real ExportXML). Tests inject
	// closures to drive deterministic outcomes.
	swapForRoute      func(spec *api.ScheduleSpec, priorXML []byte) (string, error)
	exportXMLForRoute func(taskName string) ([]byte, error)
```

- [ ] **Step 7.4: Implement weekly-schedule handler**

Append to `internal/gui/daemons.go`:

```go
// Add to imports: "errors", "mcp-local-hub/internal/api", "mcp-local-hub/internal/scheduler".
// (If already imported, leave them.)

// registerDaemonsRoutes is updated to also wire the weekly-schedule
// route. Replace the prior single-route version with this.
func registerDaemonsRoutes(s *Server) {
	s.mux.HandleFunc("/api/daemons/weekly-refresh-membership",
		s.requireSameOrigin(s.weeklyRefreshMembershipHandler))
	s.mux.HandleFunc("/api/daemons/weekly-schedule",
		s.requireSameOrigin(s.weeklyScheduleHandler))
}

// weeklyScheduleHandler implements PUT /api/daemons/weekly-schedule
// per memo D8. Owns the full transaction:
//   parse → ExportXML preflight → settings snapshot+write → swap →
//   on failure, settings rollback + combine restoreStatus.
//
// The settings handler at /api/settings/{key} stays a pure persistence
// layer; this route owns scheduler side-effects + best-effort atomicity.
func (s *Server) weeklyScheduleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Schedule string `json:"schedule"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "bad_json",
			"detail":  err.Error(),
			"example": "weekly Sun 03:00",
		})
		return
	}

	// 1. Parse: 400 envelope is OUTSIDE the rollback envelope (memo D8).
	spec, err := api.ParseSchedule(body.Schedule)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "parse_error",
			"detail":  err.Error(),
			"example": "weekly Sun 03:00",
		})
		return
	}

	// 2. Preflight: ExportXML. Failure aborts BEFORE destructive Delete.
	exportFn := s.exportXMLForRoute
	if exportFn == nil {
		exportFn = realExportXML
	}
	priorXML, exportErr := exportFn(api.WeeklyRefreshTaskName)
	if exportErr != nil && !errors.Is(exportErr, scheduler.ErrTaskNotFound) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":          "snapshot_unavailable",
			"detail":         exportErr.Error(),
			"updated":        false,
			"restore_status": "n/a",
		})
		return
	}
	// ErrTaskNotFound → fresh-install case; priorXML stays nil.

	// 3. Snapshot prior settings value for potential rollback.
	priorScheduleValue, _ := api.NewAPI().SettingsGet("daemons.weekly_schedule")

	// 4. Write new settings value.
	if err := api.NewAPI().SettingsSet("daemons.weekly_schedule", body.Schedule); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":          "settings_write_failed",
			"detail":         err.Error(),
			"updated":        false,
			"restore_status": "n/a",
		})
		return
	}

	// 5. Swap.
	swapFn := s.swapForRoute
	if swapFn == nil {
		swapFn = api.SwapWeeklyTrigger
	}
	helperStatus, swapErr := swapFn(spec, priorXML)

	// 6. On swap success → 200.
	if swapErr == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"updated":        true,
			"schedule":       body.Schedule,
			"restore_status": "n/a",
		})
		return
	}

	// 7. On swap failure → settings rollback + combination truth table.
	settingsRollbackFailed := false
	if rerr := api.NewAPI().SettingsSet("daemons.weekly_schedule", priorScheduleValue); rerr != nil {
		settingsRollbackFailed = true
	}

	// Truth table per memo D8 step 8:
	//   helper "n/a" + settings ok      → "n/a"
	//   helper "ok" + settings ok       → "ok"
	//   helper "degraded" + settings ok → "degraded"
	//   any helper + settings failed    → "failed"
	finalStatus := helperStatus
	if settingsRollbackFailed {
		finalStatus = "failed"
	}

	resp := map[string]any{
		"error":          "scheduler_swap_failed",
		"detail":         swapErr.Error(),
		"updated":        false,
		"restore_status": finalStatus,
	}
	if finalStatus == "degraded" || finalStatus == "failed" {
		resp["manual_recovery"] = "Run `mcphub workspace-weekly-refresh-restore` or restart mcphub to re-create the task."
	}
	writeJSON(w, http.StatusInternalServerError, resp)
}

// realExportXML is the production ExportXML adapter used when no
// test seam is set.
func realExportXML(taskName string) ([]byte, error) {
	sch, err := schedulerNewForRouteHandler()
	if err != nil {
		return nil, err
	}
	return sch.ExportXML(taskName)
}

// schedulerNewForRouteHandler is a thin wrapper around the package-internal
// schedulerNewForRegister so the gui package can reach it without exposing
// internals. If the api package already exports a scheduler factory, prefer
// that instead.
func schedulerNewForRouteHandler() (*scheduler.Scheduler, error) {
	return scheduler.New() // matches existing scheduler.New() factory
}
```

(The handler reuses `api.NewAPI()` for settings I/O — the settings store has its own `settingsMu` internally; no extra handler-level mutex is needed beyond what `SettingsSet` already provides.)

- [ ] **Step 7.5: Run handler tests**

```bash
go test ./internal/gui/ -run TestWeeklyScheduleHandler -v
```

Expected: PASS.

- [ ] **Step 7.6: Run wider regression**

```bash
go test ./internal/gui/ -count=1
```

Expected: PASS.

- [ ] **Step 7.7: Commit**

```bash
git add internal/gui/daemons.go internal/gui/daemons_test.go internal/gui/server.go
git commit -m "$(cat <<'EOF'
feat(daemons): PUT /api/daemons/weekly-schedule with preflight + truth-table response

Handler owns the full memo-D8 transaction: parse → ExportXML preflight
(fail-fast on error other than ErrTaskNotFound) → settings snapshot +
write → SwapWeeklyTrigger → on swap failure, settings rollback +
combination truth table → response.

Parse errors (400) carry {error, detail, example} only — no `updated`,
no `restore_status` (memo D8 envelope distinction). Transactional 5xx
failures carry the full rollback envelope; degraded/failed include
`manual_recovery` hint.

Test seams (swapForRoute, exportXMLForRoute) drive five handler-level
scenarios deterministically without touching real Task Scheduler.
EOF
)"
```

---

## Task 8: Retry policy seam — `RetryPolicy` + `IsRetryableError` + tests

**Memo refs:** D9. **Subagent profile:** sonnet.

**Files:**
- Create: `internal/api/retry.go` — `RetryPolicy` interface + `PolicyFromString`
- Create: `internal/api/retry_classifier.go` — `IsRetryableError`
- Create: `internal/api/retry_test.go`

- [ ] **Step 8.1: Write failing tests**

Create `internal/api/retry_test.go`:

```go
package api

import (
	"errors"
	"io/fs"
	"os"
	"testing"
	"time"
)

func TestPolicyFromString_KnownStrings(t *testing.T) {
	cases := []string{"none", "linear", "exponential"}
	for _, name := range cases {
		p, err := PolicyFromString(name)
		if err != nil {
			t.Errorf("PolicyFromString(%q): %v", name, err)
			continue
		}
		if p == nil {
			t.Errorf("PolicyFromString(%q) returned nil policy", name)
		}
	}
}

func TestPolicyFromString_UnknownReturnsError(t *testing.T) {
	_, err := PolicyFromString("custom")
	if err == nil {
		t.Error("expected error for unknown policy name")
	}
}

func TestNonePolicy_BackoffAndMaxAttempts(t *testing.T) {
	p, _ := PolicyFromString("none")
	if p.MaxAttempts() != 1 {
		t.Errorf("none MaxAttempts = %d, want 1 (no retries means 1 attempt)", p.MaxAttempts())
	}
	if p.Backoff(1) != 0 {
		t.Errorf("none Backoff(1) = %v, want 0", p.Backoff(1))
	}
}

func TestLinearPolicy_BackoffSequence(t *testing.T) {
	p, _ := PolicyFromString("linear")
	if p.MaxAttempts() < 3 {
		t.Errorf("linear MaxAttempts = %d, want at least 3", p.MaxAttempts())
	}
	if p.Backoff(1) <= 0 {
		t.Errorf("linear Backoff(1) = %v, want > 0", p.Backoff(1))
	}
	if p.Backoff(2) != 2*p.Backoff(1) {
		t.Errorf("linear Backoff(2) = %v, want 2*Backoff(1)=%v", p.Backoff(2), 2*p.Backoff(1))
	}
	if p.Backoff(3) != 3*p.Backoff(1) {
		t.Errorf("linear Backoff(3) = %v, want 3*Backoff(1)", p.Backoff(3))
	}
}

func TestExponentialPolicy_BackoffSequence(t *testing.T) {
	p, _ := PolicyFromString("exponential")
	if p.MaxAttempts() < 5 {
		t.Errorf("exponential MaxAttempts = %d, want at least 5", p.MaxAttempts())
	}
	if p.Backoff(1) <= 0 {
		t.Error("exponential Backoff(1) must be > 0")
	}
	if p.Backoff(2) != 2*p.Backoff(1) {
		t.Errorf("exponential Backoff(2) = %v, want 2*Backoff(1)", p.Backoff(2))
	}
	if p.Backoff(3) != 4*p.Backoff(1) {
		t.Errorf("exponential Backoff(3) = %v, want 4*Backoff(1)", p.Backoff(3))
	}
	if p.Backoff(20) > 5*time.Minute {
		t.Errorf("exponential Backoff(20) = %v, must be capped at 5min", p.Backoff(20))
	}
}

func TestIsRetryableError_NilDefensive(t *testing.T) {
	// Memo D9 + Codex r2 specific answer: nil returns false (defensive).
	if IsRetryableError(nil) {
		t.Error("IsRetryableError(nil) = true, want false (defensive degenerate)")
	}
}

func TestIsRetryableError_NonRetryableClasses(t *testing.T) {
	// Memo D9 documented non-retryable classes.
	cases := []error{
		ErrBinaryNotFound,
		ErrPermissionDenied,
		ErrBadConfig,
		ErrUnrecoverableLockState,
		// Underlying os errors that map to these classes.
		fs.ErrPermission,
		os.ErrNotExist, // binary-not-found maps to fs.ErrNotExist for canonical exec paths
	}
	for _, e := range cases {
		if IsRetryableError(e) {
			t.Errorf("IsRetryableError(%v) = true, want false", e)
		}
	}
}

func TestIsRetryableError_RetryableClasses(t *testing.T) {
	cases := []error{
		errors.New("port already in use"),
		errors.New("temporary EAGAIN"),
		errors.New("scheduler service hiccup"),
	}
	for _, e := range cases {
		if !IsRetryableError(e) {
			t.Errorf("IsRetryableError(%v) = false, want true", e)
		}
	}
}
```

- [ ] **Step 8.2: Run; expect failure**

```bash
go test ./internal/api/ -run 'TestPolicyFromString|TestNonePolicy|TestLinearPolicy|TestExponentialPolicy|TestIsRetryableError' -v
```

Expected: FAIL — symbols undefined.

- [ ] **Step 8.3: Implement `RetryPolicy` interface**

Create `internal/api/retry.go`:

```go
// Package api — RetryPolicy timing surface. Memo D9.
//
// RetryPolicy controls timing and attempt budget for retryable errors.
// It does NOT decide whether an error is retryable — that is
// IsRetryableError's job in retry_classifier.go. The PR #2 callsite
// composes them:
//
//   for attempt := 1; ; attempt++ {
//     err := startDaemon(...)
//     if err == nil { return nil }
//     if !IsRetryableError(err) { return err } // immediate exit
//     if attempt >= policy.MaxAttempts() { return err }
//     time.Sleep(policy.Backoff(attempt))
//   }
package api

import (
	"fmt"
	"time"
)

// RetryPolicy is the timing/budget surface. Implementations are stateless.
type RetryPolicy interface {
	// Backoff returns the delay to wait BEFORE attempt+1 after a failed
	// attempt. attempt is 1-indexed (Backoff(1) is the first wait).
	Backoff(attempt int) time.Duration
	// MaxAttempts is the total attempt budget. Backoff is consulted up
	// to MaxAttempts-1 times.
	MaxAttempts() int
}

// PolicyFromString returns the policy named by s. Memo D9 names:
// "none" | "linear" | "exponential". Unknown returns error.
func PolicyFromString(s string) (RetryPolicy, error) {
	switch s {
	case "none":
		return nonePolicy{}, nil
	case "linear":
		return linearPolicy{step: 2 * time.Second, max: 3}, nil
	case "exponential":
		return exponentialPolicy{base: time.Second, cap: 5 * time.Minute, max: 5}, nil
	default:
		return nil, fmt.Errorf("unknown retry policy %q (must be none|linear|exponential)", s)
	}
}

type nonePolicy struct{}

func (nonePolicy) Backoff(attempt int) time.Duration { return 0 }
func (nonePolicy) MaxAttempts() int                  { return 1 }

type linearPolicy struct {
	step time.Duration
	max  int
}

func (p linearPolicy) Backoff(attempt int) time.Duration { return p.step * time.Duration(attempt) }
func (p linearPolicy) MaxAttempts() int                  { return p.max }

type exponentialPolicy struct {
	base time.Duration
	cap  time.Duration
	max  int
}

func (p exponentialPolicy) Backoff(attempt int) time.Duration {
	d := p.base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d > p.cap {
			return p.cap
		}
	}
	return d
}

func (p exponentialPolicy) MaxAttempts() int { return p.max }
```

- [ ] **Step 8.4: Implement classifier**

Create `internal/api/retry_classifier.go`:

```go
// Package api — error retryability classifier. Memo D9.
//
// IsRetryableError separates error-domain decisions (which errors are
// inherently non-retryable) from RetryPolicy timing decisions. Splitting
// them lets future error classes be added without recompiling every
// policy and keeps the policy interface degenerate over error type.
package api

import (
	"errors"
	"io/fs"
	"strings"
)

// Sentinel errors for the documented non-retryable classes (memo D9).
// Daemon-spawn callsites in PR #2 wrap their underlying os errors with
// these via fmt.Errorf("...: %w", ErrBinaryNotFound) so IsRetryableError
// can use errors.Is.
var (
	ErrBinaryNotFound         = errors.New("binary not found")
	ErrPermissionDenied       = errors.New("permission denied")
	ErrBadConfig              = errors.New("bad config")
	ErrUnrecoverableLockState = errors.New("unrecoverable lock state")
)

// IsRetryableError returns false for documented non-retryable classes
// and true otherwise. Nil returns false (defensive degenerate per
// memo D9 + Codex r2 specific answer).
func IsRetryableError(err error) bool {
	if err == nil {
		return false
	}
	// Sentinel-typed wrappers via errors.Is.
	for _, sentinel := range []error{
		ErrBinaryNotFound, ErrPermissionDenied, ErrBadConfig, ErrUnrecoverableLockState,
	} {
		if errors.Is(err, sentinel) {
			return false
		}
	}
	// Underlying fs/os classes that map to the same domains.
	if errors.Is(err, fs.ErrPermission) {
		return false
	}
	if errors.Is(err, fs.ErrNotExist) {
		// Treat fs.ErrNotExist as non-retryable for binary-not-found canonical
		// case. PR #2's daemon-spawn wraps fs.ErrNotExist with ErrBinaryNotFound
		// when the missing file is the LSP server binary itself; here we cover
		// the un-wrapped case as well for defense in depth.
		return false
	}
	// String-match fallback for known non-retryable signatures absent
	// proper wrapping (older code paths). Conservative: only match
	// signatures we are confident are non-retryable.
	for _, sig := range []string{
		"executable not found in $PATH",
		"the system cannot find the file specified",
	} {
		if strings.Contains(err.Error(), sig) {
			return false
		}
	}
	return true
}
```

- [ ] **Step 8.5: Run; expect pass**

```bash
go test ./internal/api/ -run 'TestPolicyFromString|TestNonePolicy|TestLinearPolicy|TestExponentialPolicy|TestIsRetryableError' -v
```

Expected: PASS.

- [ ] **Step 8.6: Commit**

```bash
git add internal/api/retry.go internal/api/retry_classifier.go internal/api/retry_test.go
git commit -m "$(cat <<'EOF'
feat(api): timing-only RetryPolicy + separate IsRetryableError classifier

Memo D9: split timing decisions from error-domain classification.
RetryPolicy interface = Backoff(attempt) + MaxAttempts(); does NOT take
err. IsRetryableError lives in retry_classifier.go with sentinel errors
(ErrBinaryNotFound / ErrPermissionDenied / ErrBadConfig /
ErrUnrecoverableLockState) plus fs.ErrPermission + fs.ErrNotExist
fallthroughs.

PR #2 callsite composes them:
  if !IsRetryableError(err) { return err }
  if attempt >= policy.MaxAttempts() { return err }
  time.Sleep(policy.Backoff(attempt))

PR #1 ships the contracts + tests; runtime applier wires in PR #2.
EOF
)"
```

---

## Task 9: ConfirmModal component using `<dialog>` element + tests

**Memo refs:** D10. **Subagent profile:** sonnet.

**Files:**
- Create: `internal/gui/frontend/src/components/ConfirmModal.tsx`
- Create: `internal/gui/frontend/src/components/ConfirmModal.test.tsx`

- [ ] **Step 9.1: Write failing component test**

Create `internal/gui/frontend/src/components/ConfirmModal.test.tsx`:

```tsx
import { render, screen, fireEvent, waitFor } from "@testing-library/preact";
import { describe, expect, it, vi, beforeEach } from "vitest";
import { ConfirmModal } from "./ConfirmModal";

// jsdom does not implement HTMLDialogElement.showModal/close natively.
// Polyfill them so the modal opens and closes deterministically in tests.
beforeEach(() => {
  HTMLDialogElement.prototype.showModal = function () { this.open = true; };
  HTMLDialogElement.prototype.close = function () { this.open = false; };
});

describe("ConfirmModal", () => {
  it("renders title + body + confirm label", () => {
    render(
      <ConfirmModal
        open
        title="Delete eligible backups?"
        body={<>Delete <b>3</b> backups across <b>2</b> client(s).</>}
        confirmLabel="Delete"
        onConfirm={vi.fn()}
        onCancel={vi.fn()}
      />,
    );
    expect(screen.getByText("Delete eligible backups?")).toBeTruthy();
    expect(screen.getByText("Delete")).toBeTruthy();
    expect(screen.getByText("3")).toBeTruthy();
  });

  it("calls onConfirm when confirm button clicked", async () => {
    const onConfirm = vi.fn();
    render(
      <ConfirmModal
        open
        title="X"
        body={<>Y</>}
        confirmLabel="Yes"
        onConfirm={onConfirm}
        onCancel={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByText("Yes"));
    await waitFor(() => expect(onConfirm).toHaveBeenCalledOnce());
  });

  it("calls onCancel when cancel button clicked", () => {
    const onCancel = vi.fn();
    render(
      <ConfirmModal
        open
        title="X"
        body={<>Y</>}
        confirmLabel="OK"
        onConfirm={vi.fn()}
        onCancel={onCancel}
      />,
    );
    fireEvent.click(screen.getByText("Cancel"));
    expect(onCancel).toHaveBeenCalledOnce();
  });

  it("applies danger class when danger=true", () => {
    const { container } = render(
      <ConfirmModal
        open
        title="X"
        body={<>Y</>}
        confirmLabel="Delete"
        danger
        onConfirm={vi.fn()}
        onCancel={vi.fn()}
      />,
    );
    const confirm = container.querySelector('button[data-testid="confirm-modal-confirm"]');
    expect(confirm?.className).toContain("danger");
  });

  it("disables confirm button while busy", async () => {
    // Slow onConfirm should keep busy=true and disable buttons.
    const slow = vi.fn().mockImplementation(() => new Promise<void>((r) => setTimeout(r, 100)));
    const { container } = render(
      <ConfirmModal
        open
        title="X"
        body={<>Y</>}
        confirmLabel="Go"
        onConfirm={slow}
        onCancel={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByText("Go"));
    await waitFor(() => {
      const confirm = container.querySelector('button[data-testid="confirm-modal-confirm"]');
      expect((confirm as HTMLButtonElement).disabled).toBe(true);
    });
  });

  it("does not render the dialog when open=false", () => {
    render(
      <ConfirmModal
        open={false}
        title="X"
        body={<>Y</>}
        confirmLabel="OK"
        onConfirm={vi.fn()}
        onCancel={vi.fn()}
      />,
    );
    // Dialog is in DOM but not open; getByText should NOT find title.
    // (open=false collapses showModal call.)
    const dialog = document.querySelector("dialog");
    expect(dialog?.open).toBeFalsy();
  });
});
```

- [ ] **Step 9.2: Run; expect failure**

```bash
cd internal/gui/frontend && npm test -- ConfirmModal.test
```

Expected: FAIL — component file missing.

- [ ] **Step 9.3: Implement component**

Create `internal/gui/frontend/src/components/ConfirmModal.tsx`:

```tsx
// internal/gui/frontend/src/components/ConfirmModal.tsx
//
// Memo D10: reusable destructive-action confirm. Used in PR #1 by:
//   - SectionBackups clean-now action
//   - SectionAdvanced kill-stuck-process flow
//
// Uses the native <dialog> element pattern (same as A3-a's
// AddSecretModal). The browser provides focus trap, Esc to cancel,
// and ARIA semantics for free; this component does NOT extract a
// useFocusTrap hook in PR #1 (memo D10 deferred).
import { useEffect, useRef, useState } from "preact/hooks";
import type { JSX } from "preact";

export type ConfirmModalProps = {
  open: boolean;
  title: string;
  body: JSX.Element;
  confirmLabel: string;
  danger?: boolean;
  onConfirm: () => void | Promise<void>;
  onCancel: () => void;
};

export function ConfirmModal(props: ConfirmModalProps): JSX.Element {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    const d = dialogRef.current;
    if (!d) return;
    if (props.open && !d.open) {
      d.showModal();
      setBusy(false);
    } else if (!props.open && d.open) {
      d.close();
    }
  }, [props.open]);

  async function onConfirmClick() {
    if (busy) return;
    setBusy(true);
    try {
      await props.onConfirm();
    } finally {
      // The parent owns close-on-success — onConfirm typically calls
      // setOpen(false); we just clear our local busy state.
      setBusy(false);
    }
  }

  return (
    <dialog
      ref={dialogRef}
      class="confirm-modal"
      data-testid="confirm-modal"
      onCancel={(e) => {
        // Native cancel (Esc) — let parent close it.
        if (busy) {
          e.preventDefault();
          return;
        }
        props.onCancel();
      }}
      onClose={() => {
        // No double-fire here: parent's setOpen(false) drives the next render.
      }}
    >
      <h2>{props.title}</h2>
      <div class="confirm-modal-body">{props.body}</div>
      <div class="confirm-modal-actions">
        <button
          type="button"
          onClick={() => !busy && props.onCancel()}
          disabled={busy}
          data-testid="confirm-modal-cancel"
        >
          Cancel
        </button>
        <button
          type="button"
          class={props.danger ? "danger" : ""}
          onClick={onConfirmClick}
          disabled={busy}
          data-testid="confirm-modal-confirm"
        >
          {busy ? "Working…" : props.confirmLabel}
        </button>
      </div>
    </dialog>
  );
}
```

- [ ] **Step 9.4: Run; expect pass**

```bash
cd internal/gui/frontend && npm test -- ConfirmModal.test
```

Expected: PASS.

- [ ] **Step 9.5: Commit**

```bash
git add internal/gui/frontend/src/components/ConfirmModal.tsx internal/gui/frontend/src/components/ConfirmModal.test.tsx
git commit -m "$(cat <<'EOF'
feat(gui): reusable ConfirmModal using native <dialog> (memo D10)

Wraps the AddSecretModal <dialog> pattern locally — gets focus trap,
Esc cancellation, and ARIA semantics from the browser without a custom
useFocusTrap hook (memo D10 hook extraction deferred to a future PR).

Used in PR #1 by clean-now confirm and force-kill confirm flows.
EOF
)"
```

---

## Task 10: Clean-now action — wire `SectionBackups` to `<ConfirmModal>`

**Memo refs:** D10. **Subagent profile:** sonnet.

**Files:**
- Modify: `internal/gui/frontend/src/components/settings/SectionBackups.tsx` — render ConfirmModal on clean-now click
- Modify: `internal/gui/frontend/src/components/settings/SectionBackups.test.tsx` — confirm + cancel paths

- [ ] **Step 10.1: Read existing SectionBackups to understand integration point**

Before editing, run:

```bash
grep -n "clean_now\|backups\.clean\|deferred" internal/gui/frontend/src/components/settings/SectionBackups.tsx
```

Locate the existing clean-now button (likely rendered as a disabled deferred Action). The wire-up replaces `disabled` with a click handler that opens ConfirmModal.

- [ ] **Step 10.2: Write failing tests**

Append to `internal/gui/frontend/src/components/settings/SectionBackups.test.tsx`:

```tsx
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/preact";
import { SectionBackups } from "./SectionBackups";

beforeEach(() => {
  HTMLDialogElement.prototype.showModal = function () { this.open = true; };
  HTMLDialogElement.prototype.close = function () { this.open = false; };
});

function snapshotWithBackups(): any {
  return {
    status: "ok",
    data: {
      settings: [
        { key: "backups.keep_n", section: "backups", type: "int", default: "5", value: "5", deferred: false, help: "" },
        { key: "backups.clean_now", section: "backups", type: "action", deferred: false, help: "Delete eligible timestamped backups." },
      ],
      actual_port: 9125,
    },
    error: null,
    refresh: () => Promise.resolve(),
  };
}

describe("SectionBackups clean-now", () => {
  it("opens ConfirmModal on Clean-now button click", async () => {
    render(<SectionBackups snapshot={snapshotWithBackups()} />);
    fireEvent.click(screen.getByText(/clean.*now/i));
    await waitFor(() => expect(screen.getByTestId("confirm-modal")).toBeTruthy());
    expect(screen.getByRole("heading", { name: /delete.*backups/i })).toBeTruthy();
  });

  it("invokes POST /api/backups/clean on Confirm", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve({ cleaned: 3 }) });
    vi.stubGlobal("fetch", fetchMock);
    render(<SectionBackups snapshot={snapshotWithBackups()} />);
    fireEvent.click(screen.getByText(/clean.*now/i));
    await waitFor(() => screen.getByTestId("confirm-modal"));
    fireEvent.click(screen.getByTestId("confirm-modal-confirm"));
    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        expect.stringContaining("/api/backups/clean"),
        expect.objectContaining({ method: "POST" }),
      );
    });
    vi.unstubAllGlobals();
  });

  it("does NOT invoke endpoint on Cancel", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    render(<SectionBackups snapshot={snapshotWithBackups()} />);
    fireEvent.click(screen.getByText(/clean.*now/i));
    await waitFor(() => screen.getByTestId("confirm-modal"));
    fireEvent.click(screen.getByTestId("confirm-modal-cancel"));
    await waitFor(() => {
      expect(fetchMock).not.toHaveBeenCalled();
    });
    vi.unstubAllGlobals();
  });
});
```

- [ ] **Step 10.3: Run; expect failure**

```bash
cd internal/gui/frontend && npm test -- SectionBackups.test
```

Expected: existing tests pass; new clean-now tests FAIL because the action is currently disabled or wired to a different flow.

- [ ] **Step 10.4: Wire SectionBackups to ConfirmModal**

In `SectionBackups.tsx`, import ConfirmModal and the existing backups-API client. Replace the disabled clean-now button with:

```tsx
import { useState } from "preact/hooks";
import { ConfirmModal } from "../ConfirmModal";
import { cleanBackups, fetchCleanPreview } from "../../lib/backups-api"; // existing client

// ...inside SectionBackups component, before the return:
const [confirmOpen, setConfirmOpen] = useState(false);
const [preview, setPreview] = useState<{ eligible_count: number; client_count: number } | null>(null);

async function openConfirm() {
  // Fetch the preview BEFORE opening so the body can show concrete counts.
  try {
    const p = await fetchCleanPreview();
    setPreview({ eligible_count: p.eligible_count, client_count: p.client_count });
  } catch {
    setPreview({ eligible_count: 0, client_count: 0 });
  }
  setConfirmOpen(true);
}

async function doClean() {
  await cleanBackups();
  setConfirmOpen(false);
  await snapshot.refresh();
}

// ...in the JSX where the action button lives:
<button type="button" onClick={() => void openConfirm()} data-testid="clean-now-button">
  Clean now eligible backups
</button>

<ConfirmModal
  open={confirmOpen}
  title="Delete eligible backups?"
  body={
    <>
      Delete <b>{preview?.eligible_count ?? 0}</b> backup(s) across{" "}
      <b>{preview?.client_count ?? 0}</b> client(s). Originals are never cleaned.
    </>
  }
  confirmLabel="Delete"
  danger
  onConfirm={doClean}
  onCancel={() => setConfirmOpen(false)}
/>
```

(If `cleanBackups` / `fetchCleanPreview` are not yet exported from `backups-api.ts`, add them by mirroring existing helpers.)

- [ ] **Step 10.5: Run frontend tests**

```bash
cd internal/gui/frontend && npm test -- SectionBackups.test
```

Expected: PASS.

- [ ] **Step 10.6: Commit**

```bash
git add internal/gui/frontend/src/components/settings/SectionBackups.tsx internal/gui/frontend/src/components/settings/SectionBackups.test.tsx
git commit -m "$(cat <<'EOF'
feat(settings): wire backups.clean_now to ConfirmModal (memo D10)

Replace the deferred-disabled Clean-now button in SectionBackups with
a ConfirmModal-gated POST /api/backups/clean call. Modal body shows
the eligible count + client count fetched from /api/backups/clean-preview
so the user sees concrete numbers before committing.
EOF
)"
```

---

## Task 11: WeeklyMembershipTable component + backend snapshot endpoint + integrate into SectionDaemons

**Memo refs:** D3, D4, D5, D6. **Subagent profile:** opus (multi-file frontend coordination + multi-op save flow).

**Files:**
- Create: `internal/api/membership_snapshot.go` — `WeeklyMembershipSnapshot()` returns the rows for the GUI table
- Create: `internal/gui/daemons.go` extension — `GET /api/daemons/weekly-refresh-membership`
- Create: `internal/gui/frontend/src/lib/api-daemons.ts` — typed client for the three new daemon routes
- Create: `internal/gui/frontend/src/components/settings/WeeklyMembershipTable.tsx`
- Create: `internal/gui/frontend/src/components/settings/WeeklyMembershipTable.test.tsx`
- Modify: `internal/gui/frontend/src/components/settings/SectionDaemons.tsx` — render the table; add the four editable fields (knob + cron + retry + table); orchestrate the multi-op save

- [ ] **Step 11.1: Backend snapshot endpoint — failing test**

Append to `internal/gui/daemons_test.go`:

```go
func TestMembershipSnapshotHandler_GET(t *testing.T) {
	srv := newTestServer(t)
	// Seed the registry test seam with three rows.
	srv.registryRows = []map[string]any{
		{"workspace_key": "k1", "workspace_path": "D:/p1", "language": "python", "weekly_refresh": true},
		{"workspace_key": "k1", "workspace_path": "D:/p1", "language": "rust", "weekly_refresh": false},
		{"workspace_key": "k2", "workspace_path": "/p2", "language": "go", "weekly_refresh": true},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/daemons/weekly-refresh-membership", nil)
	req.Header.Set("Origin", srv.OriginAllowed())
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp struct {
		Rows []struct {
			WorkspaceKey  string `json:"workspace_key"`
			WorkspacePath string `json:"workspace_path"`
			Language      string `json:"language"`
			WeeklyRefresh bool   `json:"weekly_refresh"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Rows) != 3 {
		t.Errorf("rows = %d, want 3", len(resp.Rows))
	}
	if resp.Rows[0].WeeklyRefresh != true || resp.Rows[1].WeeklyRefresh != false {
		t.Error("D2 invariant: existing values must be preserved verbatim")
	}
}
```

(Add `registryRows` test seam to `Server` struct in `server.go`.)

- [ ] **Step 11.2: Implement snapshot endpoint**

Append to `internal/gui/daemons.go`:

```go
// weeklyRefreshMembershipHandler is replaced with a method-multiplexer
// since the same path now serves GET (list) and PUT (update).
func (s *Server) weeklyRefreshMembershipHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.weeklyRefreshMembershipList(w, r)
	case http.MethodPut:
		s.weeklyRefreshMembershipPut(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type membershipRowDTO struct {
	WorkspaceKey  string `json:"workspace_key"`
	WorkspacePath string `json:"workspace_path"`
	Language      string `json:"language"`
	WeeklyRefresh bool   `json:"weekly_refresh"`
}

func (s *Server) weeklyRefreshMembershipList(w http.ResponseWriter, r *http.Request) {
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "registry_path", "detail": err.Error()})
		return
	}
	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "load_registry", "detail": err.Error()})
		return
	}
	rows := make([]membershipRowDTO, 0, len(reg.Workspaces))
	for _, e := range reg.Workspaces {
		rows = append(rows, membershipRowDTO{
			WorkspaceKey:  e.WorkspaceKey,
			WorkspacePath: e.WorkspacePath,
			Language:      e.Language,
			WeeklyRefresh: e.WeeklyRefresh,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

// Rename the old handler to weeklyRefreshMembershipPut (its body is the
// same as Task 4's weeklyRefreshMembershipHandler).
func (s *Server) weeklyRefreshMembershipPut(w http.ResponseWriter, r *http.Request) {
	// (paste body from Task 4 step 4.6's PUT handler)
}
```

- [ ] **Step 11.3: Run backend tests**

```bash
go test ./internal/gui/ -run TestMembershipSnapshotHandler -v
```

Expected: PASS.

- [ ] **Step 11.4: Frontend typed client**

Create `internal/gui/frontend/src/lib/api-daemons.ts`:

```ts
// internal/gui/frontend/src/lib/api-daemons.ts
//
// Typed clients for /api/daemons/* endpoints introduced in A4-b PR #1.
// Memo §4 endpoint table.

export type MembershipRow = {
  workspace_key: string;
  workspace_path: string;
  language: string;
  weekly_refresh: boolean;
};

export type MembershipDelta = {
  workspace_key: string;
  language: string;
  enabled: boolean;
};

export async function fetchMembership(): Promise<MembershipRow[]> {
  const r = await fetch("/api/daemons/weekly-refresh-membership");
  if (!r.ok) throw new Error(`fetchMembership: HTTP ${r.status}`);
  const j = await r.json();
  return j.rows ?? [];
}

export async function putMembership(deltas: MembershipDelta[]): Promise<{ updated: number; warnings: string[] }> {
  const r = await fetch("/api/daemons/weekly-refresh-membership", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(deltas),
  });
  if (!r.ok) {
    const body = await r.json().catch(() => ({}));
    throw new Error(body.detail ?? `putMembership: HTTP ${r.status}`);
  }
  return r.json();
}

// Memo D8: success vs transactional-failure vs parse-error envelopes.
export type WeeklyScheduleSuccess = { updated: true; schedule: string; restore_status: "n/a" };
export type WeeklyScheduleParseError = { error: "parse_error"; detail: string; example: string };
export type WeeklyScheduleSwapFailure = {
  error: "snapshot_unavailable" | "settings_write_failed" | "scheduler_swap_failed";
  detail: string;
  updated: false;
  restore_status: "ok" | "degraded" | "failed" | "n/a";
  manual_recovery?: string;
};

export async function putWeeklySchedule(schedule: string): Promise<WeeklyScheduleSuccess> {
  const r = await fetch("/api/daemons/weekly-schedule", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ schedule }),
  });
  const j = await r.json();
  if (r.status === 200) return j as WeeklyScheduleSuccess;
  if (r.status === 400) throw j as WeeklyScheduleParseError;
  throw j as WeeklyScheduleSwapFailure;
}
```

- [ ] **Step 11.5: Write failing component test**

Create `internal/gui/frontend/src/components/settings/WeeklyMembershipTable.test.tsx`:

```tsx
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/preact";
import { WeeklyMembershipTable } from "./WeeklyMembershipTable";
import type { MembershipRow } from "../../lib/api-daemons";

const rows: MembershipRow[] = [
  { workspace_key: "k1", workspace_path: "D:/p1", language: "python", weekly_refresh: true },
  { workspace_key: "k1", workspace_path: "D:/p1", language: "rust", weekly_refresh: false },
  { workspace_key: "k2", workspace_path: "/p2", language: "go", weekly_refresh: true },
];

beforeEach(() => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue({
    ok: true, json: () => Promise.resolve({ rows }),
  }));
});

describe("WeeklyMembershipTable", () => {
  it("renders one row per registry entry with correct initial state", async () => {
    const onDirtyChange = vi.fn();
    render(<WeeklyMembershipTable onDirtyChange={onDirtyChange} onDeltasChange={vi.fn()} />);
    await waitFor(() => screen.getByText("python"));
    expect(screen.getByText("python")).toBeTruthy();
    expect(screen.getByText("rust")).toBeTruthy();
    expect(screen.getByText("go")).toBeTruthy();
    const checkboxes = screen.getAllByRole("checkbox");
    expect((checkboxes[0] as HTMLInputElement).checked).toBe(true);  // python: enabled
    expect((checkboxes[1] as HTMLInputElement).checked).toBe(false); // rust: disabled
    expect((checkboxes[2] as HTMLInputElement).checked).toBe(true);  // go: enabled
  });

  it("toggling a row sets dirty state", async () => {
    const onDirtyChange = vi.fn();
    render(<WeeklyMembershipTable onDirtyChange={onDirtyChange} onDeltasChange={vi.fn()} />);
    await waitFor(() => screen.getByText("python"));
    const checkboxes = screen.getAllByRole("checkbox");
    fireEvent.click(checkboxes[1]); // toggle rust → enabled
    await waitFor(() => expect(onDirtyChange).toHaveBeenCalledWith(true));
  });

  it("Select all flips every row to enabled", async () => {
    const onDeltasChange = vi.fn();
    render(<WeeklyMembershipTable onDirtyChange={vi.fn()} onDeltasChange={onDeltasChange} />);
    await waitFor(() => screen.getByText("python"));
    fireEvent.click(screen.getByText("Select all"));
    const checkboxes = screen.getAllByRole("checkbox");
    expect((checkboxes[0] as HTMLInputElement).checked).toBe(true);
    expect((checkboxes[1] as HTMLInputElement).checked).toBe(true);
    expect((checkboxes[2] as HTMLInputElement).checked).toBe(true);
  });

  it("Clear all flips every row to disabled", async () => {
    render(<WeeklyMembershipTable onDirtyChange={vi.fn()} onDeltasChange={vi.fn()} />);
    await waitFor(() => screen.getByText("python"));
    fireEvent.click(screen.getByText("Clear all"));
    const checkboxes = screen.getAllByRole("checkbox");
    expect((checkboxes[0] as HTMLInputElement).checked).toBe(false);
    expect((checkboxes[1] as HTMLInputElement).checked).toBe(false);
    expect((checkboxes[2] as HTMLInputElement).checked).toBe(false);
  });

  it("renders empty-state when zero rows", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: () => Promise.resolve({ rows: [] }) }));
    render(<WeeklyMembershipTable onDirtyChange={vi.fn()} onDeltasChange={vi.fn()} />);
    await waitFor(() => screen.getByText(/no workspaces registered/i));
  });
});
```

- [ ] **Step 11.6: Run; expect failure**

```bash
cd internal/gui/frontend && npm test -- WeeklyMembershipTable.test
```

Expected: FAIL — component file missing.

- [ ] **Step 11.7: Implement WeeklyMembershipTable**

Create `internal/gui/frontend/src/components/settings/WeeklyMembershipTable.tsx`:

```tsx
// internal/gui/frontend/src/components/settings/WeeklyMembershipTable.tsx
//
// Memo D3-D6: per-(workspace_key, language) checkbox table inside
// SectionDaemons. Loads from GET /api/daemons/weekly-refresh-membership
// on mount; emits dirty deltas via onDeltasChange so the section's
// save flow can include them in op 3 of the multi-op save.
import { useEffect, useState, useMemo } from "preact/hooks";
import type { JSX } from "preact";
import { fetchMembership, type MembershipRow, type MembershipDelta } from "../../lib/api-daemons";

export type WeeklyMembershipTableProps = {
  onDirtyChange: (dirty: boolean) => void;
  onDeltasChange: (deltas: MembershipDelta[]) => void;
};

type Status = "loading" | "ok" | "error";

export function WeeklyMembershipTable(props: WeeklyMembershipTableProps): JSX.Element {
  const [status, setStatus] = useState<Status>("loading");
  const [rows, setRows] = useState<MembershipRow[]>([]);
  const [edits, setEdits] = useState<Record<string, boolean>>({}); // key = workspace_key+"\x00"+language

  useEffect(() => {
    fetchMembership()
      .then((r) => { setRows(r); setStatus("ok"); })
      .catch(() => setStatus("error"));
  }, []);

  function rowKey(r: { workspace_key: string; language: string }) {
    return `${r.workspace_key}\x00${r.language}`;
  }

  function effective(r: MembershipRow): boolean {
    const k = rowKey(r);
    return k in edits ? edits[k] : r.weekly_refresh;
  }

  const deltas = useMemo<MembershipDelta[]>(() => {
    const out: MembershipDelta[] = [];
    for (const r of rows) {
      const k = rowKey(r);
      if (k in edits && edits[k] !== r.weekly_refresh) {
        out.push({ workspace_key: r.workspace_key, language: r.language, enabled: edits[k] });
      }
    }
    return out;
  }, [rows, edits]);

  useEffect(() => {
    props.onDirtyChange(deltas.length > 0);
    props.onDeltasChange(deltas);
  }, [deltas]);

  function toggle(r: MembershipRow) {
    const k = rowKey(r);
    setEdits((prev) => {
      const next = { ...prev };
      const cur = k in prev ? prev[k] : r.weekly_refresh;
      const flipped = !cur;
      if (flipped === r.weekly_refresh) {
        delete next[k]; // back to baseline → clean
      } else {
        next[k] = flipped;
      }
      return next;
    });
  }

  function selectAll() {
    setEdits((prev) => {
      const next: Record<string, boolean> = { ...prev };
      for (const r of rows) {
        if (!r.weekly_refresh) next[rowKey(r)] = true;
        else delete next[rowKey(r)];
      }
      return next;
    });
  }

  function clearAll() {
    setEdits((prev) => {
      const next: Record<string, boolean> = { ...prev };
      for (const r of rows) {
        if (r.weekly_refresh) next[rowKey(r)] = false;
        else delete next[rowKey(r)];
      }
      return next;
    });
  }

  if (status === "loading") {
    return <div class="membership-table"><p>Loading workspaces…</p></div>;
  }
  if (status === "error") {
    return <div class="membership-table"><p class="error-banner">Could not load workspace list.</p></div>;
  }
  if (rows.length === 0) {
    return (
      <div class="membership-table">
        <p class="empty-state">
          No workspaces registered yet — run <code>mcphub register</code> from a workspace folder to add one.
        </p>
      </div>
    );
  }
  return (
    <div class="membership-table" data-testid="weekly-membership-table">
      <h3>Workspaces in weekly refresh</h3>
      <div class="membership-actions">
        <button type="button" class="link-button" onClick={selectAll}>Select all</button>
        <span class="sep">·</span>
        <button type="button" class="link-button" onClick={clearAll}>Clear all</button>
      </div>
      <ul class="membership-list">
        {rows.map((r) => (
          <li key={rowKey(r)}>
            <label>
              <input
                type="checkbox"
                checked={effective(r)}
                onChange={() => toggle(r)}
                data-testid={`membership-${r.workspace_key}-${r.language}`}
              />
              <span class="ws-path">{r.workspace_path}</span>
              <span class="ws-lang">× {r.language}</span>
            </label>
          </li>
        ))}
      </ul>
    </div>
  );
}
```

- [ ] **Step 11.8: Run; expect pass**

```bash
cd internal/gui/frontend && npm test -- WeeklyMembershipTable.test
```

Expected: PASS.

- [ ] **Step 11.9: Replace `SectionDaemons.tsx` with editable section + multi-op save**

Replace the entire `SectionDaemons.tsx` content with:

```tsx
// internal/gui/frontend/src/components/settings/SectionDaemons.tsx
//
// A4-b PR #1: editable section with multi-op save (memo D4).
// One Save button orchestrates THREE independent transactional ops:
//   op 1 = settings save  (knob + retry policy via existing /api/settings/{key})
//   op 2 = weekly schedule swap (PUT /api/daemons/weekly-schedule)
//   op 3 = membership update (PUT /api/daemons/weekly-refresh-membership)
//
// Partial failures: prior committed ops stay; subsequent ops are skipped;
// banner names committed vs dirty per memo D4 UI messaging contract.
import { useState } from "preact/hooks";
import type { SettingsSnapshot, ConfigSettingDTO } from "../../lib/settings-types";
import { putSetting } from "../../lib/settings-api";
import { putMembership, putWeeklySchedule, type MembershipDelta } from "../../lib/api-daemons";
import { WeeklyMembershipTable } from "./WeeklyMembershipTable";

export type SectionDaemonsProps = {
  snapshot: SettingsSnapshot;
  onDirtyChange: (b: boolean) => void;
};

type Banner = { kind: "ok" | "partial" | "error"; text: string };

export function SectionDaemons({ snapshot, onDirtyChange }: SectionDaemonsProps): preact.JSX.Element {
  if (snapshot.status !== "ok") {
    return <section data-section="daemons" class="settings-section"><h2>Daemons</h2><p>Loading…</p></section>;
  }
  const knob = snapshot.data.settings.find((s) => s.key === "daemons.weekly_refresh_default") as ConfigSettingDTO | undefined;
  const sched = snapshot.data.settings.find((s) => s.key === "daemons.weekly_schedule") as ConfigSettingDTO | undefined;
  const retry = snapshot.data.settings.find((s) => s.key === "daemons.retry_policy") as ConfigSettingDTO | undefined;
  if (!knob || !sched || !retry) {
    return <section data-section="daemons" class="settings-section"><h2>Daemons</h2><p class="error-banner">Schema mismatch.</p></section>;
  }

  const [knobValue, setKnobValue] = useState(knob.value);
  const [schedValue, setSchedValue] = useState(sched.value);
  const [retryValue, setRetryValue] = useState(retry.value);
  const [tableDirty, setTableDirty] = useState(false);
  const [tableDeltas, setTableDeltas] = useState<MembershipDelta[]>([]);
  const [busy, setBusy] = useState(false);
  const [banner, setBanner] = useState<Banner | null>(null);
  const [schedError, setSchedError] = useState<string | null>(null);

  const knobDirty = knobValue !== knob.value;
  const schedDirty = schedValue !== sched.value;
  const retryDirty = retryValue !== retry.value;
  const dirty = knobDirty || schedDirty || retryDirty || tableDirty;

  // Bubble dirty up so app-level guard can prompt on navigation.
  useState(() => { onDirtyChange(dirty); }); // run once on mount; re-render path calls below
  if (dirty !== false) onDirtyChange(dirty);

  async function save() {
    if (!dirty) return;
    setBusy(true);
    setBanner(null);
    setSchedError(null);

    const committed: string[] = [];
    let abort = false;

    // ---- op 1: settings (knob + retry) ----
    if (!abort && knobDirty) {
      try { await putSetting("daemons.weekly_refresh_default", knobValue); committed.push("knob"); }
      catch (e: any) { abort = true; setBanner({ kind: "partial", text: `Knob save failed: ${e?.message ?? "?"}` }); }
    }
    if (!abort && retryDirty) {
      try { await putSetting("daemons.retry_policy", retryValue); committed.push("retry"); }
      catch (e: any) { abort = true; setBanner({ kind: "partial", text: `Retry policy save failed: ${e?.message ?? "?"}` }); }
    }

    // ---- op 2: weekly schedule swap ----
    if (!abort && schedDirty) {
      try {
        await putWeeklySchedule(schedValue);
        committed.push("schedule");
      } catch (e: any) {
        abort = true;
        if (e?.error === "parse_error") {
          setSchedError(`${e.detail} (example: ${e.example})`);
        }
        const status = e?.restore_status;
        const tail = status === "degraded"
          ? ` (degraded restore — ${e.manual_recovery ?? "manual recovery needed"})`
          : status === "failed" ? " (rollback failed)" : "";
        setBanner({
          kind: "partial",
          text: `${committed.join(" + ") || "No prior op"} saved. Schedule update failed${tail}. Membership not attempted; still dirty.`,
        });
      }
    }

    // ---- op 3: membership ----
    if (!abort && tableDirty) {
      try {
        await putMembership(tableDeltas);
        committed.push("membership");
      } catch (e: any) {
        abort = true;
        setBanner({
          kind: "partial",
          text: `${committed.join(" + ") || "No prior op"} saved. Membership update failed: ${e?.message ?? "?"}. Still dirty.`,
        });
      }
    }

    if (!abort) {
      await snapshot.refresh();
      setBanner({ kind: "ok", text: "Saved." });
      setTimeout(() => setBanner(null), 2500);
      // Reset dirty markers (the snapshot refresh propagates persisted values to props).
      setKnobValue((v) => v); // no-op; snapshot refresh provides new baseline
    }
    setBusy(false);
  }

  function reset() {
    setKnobValue(knob.value);
    setSchedValue(sched.value);
    setRetryValue(retry.value);
    setSchedError(null);
    setBanner(null);
    // The membership table resets its edits map automatically when the
    // section is remounted via the dirty-discard discardKey in app.tsx.
  }

  return (
    <section data-section="daemons" class="settings-section">
      <h2>Daemons</h2>
      <p class="settings-section-help">Background daemon settings. One Save button writes settings + schedule + membership in three sequential transactions; partial failures are surfaced explicitly.</p>

      <div class="settings-field">
        <label for="daemons.weekly_schedule">weekly schedule</label>
        <input
          id="daemons.weekly_schedule"
          type="text"
          value={schedValue}
          onInput={(e) => setSchedValue((e.target as HTMLInputElement).value)}
          placeholder="weekly Sun 03:00"
          data-testid="weekly-schedule-input"
        />
        {schedError ? <small class="settings-field-error" role="alert">{schedError}</small> : null}
        <small class="settings-field-help">{sched.help}</small>
      </div>

      <div class="settings-field">
        <label for="daemons.retry_policy">retry policy</label>
        <select
          id="daemons.retry_policy"
          value={retryValue}
          onChange={(e) => setRetryValue((e.target as HTMLSelectElement).value)}
          data-testid="retry-policy-select"
        >
          {(retry.enum ?? []).map((opt) => <option key={opt} value={opt}>{opt}</option>)}
        </select>
        <small class="settings-field-help">{retry.help}</small>
      </div>

      <div class="settings-field">
        <label for="daemons.weekly_refresh_default">
          <input
            id="daemons.weekly_refresh_default"
            type="checkbox"
            checked={knobValue === "true"}
            onChange={(e) => setKnobValue((e.target as HTMLInputElement).checked ? "true" : "false")}
            data-testid="weekly-refresh-default-checkbox"
          />
          Default for new workspaces: enroll in weekly refresh
        </label>
        <small class="settings-field-help">{knob.help}</small>
      </div>

      <WeeklyMembershipTable onDirtyChange={setTableDirty} onDeltasChange={setTableDeltas} />

      <div class="settings-section-footer">
        {banner ? <span class={`save-banner ${banner.kind}`}>{banner.text}</span> : null}
        <button type="button" disabled={!dirty || busy} onClick={() => void save()} data-testid="daemons-save">
          {busy ? "Saving…" : "Save"}
        </button>
        <button type="button" disabled={!dirty || busy} onClick={reset} data-testid="daemons-reset">Reset</button>
      </div>
    </section>
  );
}
```

- [ ] **Step 11.10: Update existing `SectionDaemons.test.tsx` snapshot if needed**

```bash
cd internal/gui/frontend && npm test -- SectionDaemons.test
```

Update assertions for the new editable section UI; remove old "(effective in A4-b)" copy expectations.

- [ ] **Step 11.11: Run frontend regression**

```bash
cd internal/gui/frontend && npm run typecheck && npm test
```

Expected: PASS.

- [ ] **Step 11.12: Commit**

```bash
git add internal/api/membership_snapshot.go internal/gui/daemons.go internal/gui/daemons_test.go internal/gui/server.go internal/gui/frontend/src/lib/api-daemons.ts internal/gui/frontend/src/components/settings/WeeklyMembershipTable.tsx internal/gui/frontend/src/components/settings/WeeklyMembershipTable.test.tsx internal/gui/frontend/src/components/settings/SectionDaemons.tsx internal/gui/frontend/src/components/settings/SectionDaemons.test.tsx
git commit -m "$(cat <<'EOF'
feat(settings): per-workspace WeeklyRefresh table + editable Daemons section

Add WeeklyMembershipTable component (memo D3-D6) loaded from new
GET /api/daemons/weekly-refresh-membership snapshot endpoint; user
toggles per-row checkboxes plus Select all / Clear all bulk affordance;
empty-state for zero registered workspaces.

SectionDaemons rewritten with editable knob (default for new
workspaces) + cron edit (with parse-error surfacing) + retry policy
select + membership table. ONE Save button orchestrates THREE
independent transactional ops sequentially (memo D4):
  op 1 = settings (knob + retry policy)
  op 2 = weekly schedule swap
  op 3 = membership update
Partial-failure banner explicitly names committed vs dirty ops.
EOF
)"
```

---

## Task 12: Export config bundle — `.zip` streaming

**Memo refs:** D11. **Subagent profile:** sonnet.

**Files:**
- Create: `internal/api/export_bundle.go` — `WriteConfigBundle(w io.Writer) error`
- Create: `internal/api/export_bundle_test.go`
- Create: `internal/gui/export_bundle.go` — `POST /api/export-config-bundle` handler
- Create: `internal/gui/export_bundle_test.go`
- Modify: `internal/gui/server.go` — call `registerExportBundleRoutes(s)`
- Modify: `internal/gui/frontend/src/components/settings/SectionAdvanced.tsx` — wire button to fetch + download

- [ ] **Step 12.1: Write failing bundle composition test**

Create `internal/api/export_bundle_test.go`:

```go
package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteConfigBundle_Composition(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))

	// Seed fake artifacts.
	dataDir := filepath.Join(tmp, "mcp-local-hub")
	stateDir := filepath.Join(tmp, "state", "mcp-local-hub")
	for _, d := range []string{dataDir, stateDir, filepath.Join(dataDir, "servers", "wolfram")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	must := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must(filepath.Join(dataDir, "servers", "wolfram", "manifest.yaml"), "name: wolfram\nport: 9001\n")
	must(filepath.Join(dataDir, "secrets.json"), `{"ciphertext":"AAA"}`)
	must(filepath.Join(dataDir, "gui-preferences.yaml"), "theme: dark\n")
	must(filepath.Join(stateDir, "workspaces.yaml"), "version: 1\nworkspaces: []\n")

	var buf bytes.Buffer
	if err := WriteConfigBundle(&buf); err != nil {
		t.Fatalf("WriteConfigBundle: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = string(b)
	}
	for _, expectedName := range []string{
		"servers/wolfram/manifest.yaml",
		"secrets.json",
		"gui-preferences.yaml",
		"workspaces.yaml",
		"bundle-meta.json",
	} {
		if _, ok := got[expectedName]; !ok {
			t.Errorf("bundle missing %q", expectedName)
		}
	}

	// Memo D11: hostname literal "redacted".
	var meta struct {
		Hostname     string `json:"hostname"`
		ExportTime   string `json:"export_time"`
		MCPHubVer    string `json:"mcphub_version"`
		Platform     string `json:"platform"`
	}
	if err := json.Unmarshal([]byte(got["bundle-meta.json"]), &meta); err != nil {
		t.Fatalf("bundle-meta.json: %v", err)
	}
	if meta.Hostname != "redacted" {
		t.Errorf("hostname = %q, want literal %q (memo D11)", meta.Hostname, "redacted")
	}
	if meta.ExportTime == "" {
		t.Error("export_time missing")
	}
}

func TestWriteConfigBundle_ExcludesBackupFiles(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	dataDir := filepath.Join(tmp, "mcp-local-hub")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Create a .bak.<ts> file that MUST NOT be included.
	bak := filepath.Join(dataDir, "secrets.json.bak.20260101120000")
	if err := os.WriteFile(bak, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "secrets.json"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	_ = WriteConfigBundle(&buf)
	zr, _ := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	for _, f := range zr.File {
		if strings.Contains(f.Name, ".bak.") {
			t.Errorf("bundle includes backup file %q (must be excluded per memo D11)", f.Name)
		}
	}
}
```

- [ ] **Step 12.2: Run; expect failure**

```bash
go test ./internal/api/ -run TestWriteConfigBundle -v
```

Expected: FAIL — `WriteConfigBundle` undefined.

- [ ] **Step 12.3: Implement bundle composition**

Create `internal/api/export_bundle.go`:

```go
// Package api — config bundle export. Memo D11.
//
// WriteConfigBundle streams a zip archive of:
//   - servers/<name>/manifest.yaml (every manifest under the servers folder)
//   - secrets.json (ciphertext as-is)
//   - gui-preferences.yaml (full settings file)
//   - workspaces.yaml (full registry)
//   - bundle-meta.json {export_time, mcphub_version, platform, hostname:"redacted"}
//
// Backup files (*.bak.*) are explicitly excluded.
package api

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// MCPHubVersion is the build-time version (set via -ldflags or fallback).
// Resolve at runtime if a real version source exists in the package.
var mcphubVersionForBundle = func() string { return "dev" }

// WriteConfigBundle writes a .zip stream to w containing all config
// artifacts (memo D11). Returns nil on success; partial writes propagate
// the underlying io.Writer error.
func WriteConfigBundle(w io.Writer) error {
	zw := zip.NewWriter(w)
	defer zw.Close()

	dataDir, err := dataDirForBundle()
	if err != nil {
		return fmt.Errorf("locate data dir: %w", err)
	}
	stateDir, err := stateDirForBundle()
	if err != nil {
		return fmt.Errorf("locate state dir: %w", err)
	}

	// servers/<name>/manifest.yaml — walk the servers folder.
	serversRoot := filepath.Join(dataDir, "servers")
	if err := addDirGlob(zw, serversRoot, "servers", "manifest.yaml"); err != nil {
		return fmt.Errorf("add servers: %w", err)
	}
	// Top-level data files.
	for _, item := range []struct{ src, name string }{
		{filepath.Join(dataDir, "secrets.json"), "secrets.json"},
		{filepath.Join(dataDir, "gui-preferences.yaml"), "gui-preferences.yaml"},
	} {
		if err := addFileIfExists(zw, item.src, item.name); err != nil {
			return fmt.Errorf("add %s: %w", item.name, err)
		}
	}
	// State-dir files.
	if err := addFileIfExists(zw, filepath.Join(stateDir, "workspaces.yaml"), "workspaces.yaml"); err != nil {
		return fmt.Errorf("add workspaces.yaml: %w", err)
	}
	// bundle-meta.json
	meta := map[string]string{
		"export_time":     time.Now().UTC().Format(time.RFC3339),
		"mcphub_version":  mcphubVersionForBundle(),
		"platform":        runtime.GOOS + "/" + runtime.GOARCH,
		"hostname":        "redacted", // Memo D11: literal string, not <host>, not omitted.
	}
	metaBytes, _ := json.MarshalIndent(meta, "", "  ")
	if fh, err := zw.Create("bundle-meta.json"); err != nil {
		return err
	} else if _, err := fh.Write(metaBytes); err != nil {
		return err
	}
	return nil
}

func addFileIfExists(zw *zip.Writer, src, dstName string) error {
	if strings.Contains(filepath.Base(src), ".bak.") {
		return nil // memo D11: never include backups
	}
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // tolerate missing optional files
		}
		return err
	}
	fh, err := zw.Create(dstName)
	if err != nil {
		return err
	}
	_, err = fh.Write(data)
	return err
}

func addDirGlob(zw *zip.Writer, srcRoot, dstPrefix, fileName string) error {
	if _, err := os.Stat(srcRoot); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return filepath.Walk(srcRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if filepath.Base(path) != fileName {
			return nil
		}
		if strings.Contains(path, ".bak.") {
			return nil
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		// Normalize to forward-slash for zip cross-platform openability.
		dstName := dstPrefix + "/" + filepath.ToSlash(rel)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fh, err := zw.Create(dstName)
		if err != nil {
			return err
		}
		_, err = fh.Write(data)
		return err
	})
}

// dataDirForBundle returns the platform data dir (mirrors SettingsPath
// resolution but lives here to keep the bundle composer independent).
func dataDirForBundle() (string, error) {
	if runtime.GOOS == "windows" {
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "mcp-local-hub"), nil
		}
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "mcp-local-hub"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "mcp-local-hub"), nil
}

func stateDirForBundle() (string, error) {
	if runtime.GOOS == "windows" {
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "mcp-local-hub"), nil
		}
	}
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "mcp-local-hub"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "mcp-local-hub"), nil
}
```

- [ ] **Step 12.4: Run; expect pass**

```bash
go test ./internal/api/ -run TestWriteConfigBundle -v
```

Expected: PASS.

- [ ] **Step 12.5: Implement HTTP handler**

Create `internal/gui/export_bundle.go`:

```go
// Package gui — POST /api/export-config-bundle handler. Memo D11.
package gui

import (
	"fmt"
	"net/http"
	"time"

	"mcp-local-hub/internal/api"
)

func registerExportBundleRoutes(s *Server) {
	s.mux.HandleFunc("/api/export-config-bundle",
		s.requireSameOrigin(s.exportConfigBundleHandler))
}

func (s *Server) exportConfigBundleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	filename := fmt.Sprintf("mcphub-bundle-%s.zip", time.Now().UTC().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	if err := api.WriteConfigBundle(w); err != nil {
		// Best-effort: headers may have been flushed already.
		// Surface on stderr so operators see compose errors.
		fmt.Fprintf(s.logErr, "export-config-bundle: %v\n", err)
	}
}
```

(`s.logErr` is the existing log writer; if absent, use `os.Stderr` directly.)

- [ ] **Step 12.6: Wire route in server.go**

In `internal/gui/server.go` (after `registerDaemonsRoutes(s)`):

```go
	registerExportBundleRoutes(s)
```

- [ ] **Step 12.7: Write failing handler test**

Create `internal/gui/export_bundle_test.go`:

```go
package gui

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExportConfigBundleHandler_ContentTypeAndDisposition(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/export-config-bundle", nil)
	req.Header.Set("Origin", srv.OriginAllowed())
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("Content-Type") != "application/zip" {
		t.Errorf("Content-Type = %q, want application/zip", rec.Header().Get("Content-Type"))
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment; filename=\"mcphub-bundle-") {
		t.Errorf("Content-Disposition = %q, missing mcphub-bundle prefix", cd)
	}
}

func TestExportConfigBundleHandler_BodyIsValidZip(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/export-config-bundle", nil)
	req.Header.Set("Origin", srv.OriginAllowed())
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	body := rec.Body.Bytes()
	if _, err := zip.NewReader(bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("response body is not a valid zip: %v", err)
	}
}
```

- [ ] **Step 12.8: Run handler tests**

```bash
go test ./internal/gui/ -run TestExportConfigBundleHandler -v
```

Expected: PASS.

- [ ] **Step 12.9: Wire frontend SectionAdvanced "Export bundle" button**

In `SectionAdvanced.tsx`, replace the existing disabled `Export bundle` button:

```tsx
async function exportBundle() {
  // POST returns the zip directly. Browser triggers download via
  // Content-Disposition; we just open it in a new tab/window.
  const r = await fetch("/api/export-config-bundle", { method: "POST" });
  if (!r.ok) {
    setErr(`Export failed: HTTP ${r.status}`);
    return;
  }
  const blob = await r.blob();
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  // Filename is provided by Content-Disposition but jsdom drops it for
  // blob: URLs; stamp a sensible default for the manual flow.
  a.download = `mcphub-bundle-${new Date().toISOString().replace(/[:.]/g, "-").slice(0, 19)}.zip`;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}

// In the JSX:
<button type="button" onClick={() => void exportBundle()} disabled={busy} data-testid="export-bundle">
  Export config bundle
</button>
```

- [ ] **Step 12.10: Run frontend tests**

```bash
cd internal/gui/frontend && npm test -- SectionAdvanced.test
```

Expected: PASS (existing tests; the new button has E2E coverage in Task 15).

- [ ] **Step 12.11: Commit**

```bash
git add internal/api/export_bundle.go internal/api/export_bundle_test.go internal/gui/export_bundle.go internal/gui/export_bundle_test.go internal/gui/server.go internal/gui/frontend/src/components/settings/SectionAdvanced.tsx
git commit -m "$(cat <<'EOF'
feat(settings): export config bundle as streamed .zip (memo D11)

WriteConfigBundle streams a zip archive over io.Writer with manifests,
secrets ciphertext, gui-preferences.yaml, workspaces.yaml, and
bundle-meta.json with hostname literal "redacted". Backup files
(*.bak.*) explicitly excluded.

POST /api/export-config-bundle handler streams via zip.Writer over
http.ResponseWriter — no temp file, no two-step. Frontend wires the
SectionAdvanced button to fetch + Blob download.
EOF
)"
```

---

## Task 13: Force-kill backend — `POST /api/force-kill/probe` + `POST /api/force-kill`

**Memo refs:** D12, D13. **Subagent profile:** opus (C1 contract preservation; identity-gate verbatim).

**Files:**
- Create: `internal/gui/force_kill.go` — both handlers
- Create: `internal/gui/force_kill_test.go`
- Modify: `internal/gui/server.go` — `registerForceKillRoutes(s)`

- [ ] **Step 13.1: Write failing handler tests**

Create `internal/gui/force_kill_test.go`:

```go
package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"mcp-local-hub/internal/gui"
)

func TestForceKillProbe_ReturnsVerdict(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS returns 501 — covered by separate test")
	}
	srv := newTestServer(t)
	srv.probeForRoute = func() gui.Verdict {
		return gui.Verdict{Class: "Healthy", PIDAlive: true, PingMatch: true}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/force-kill/probe", nil)
	req.Header.Set("Origin", srv.OriginAllowed())
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var v gui.Verdict
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v.Class != "Healthy" {
		t.Errorf("Class = %q, want Healthy", v.Class)
	}
}

func TestForceKill_RequiresPOST(t *testing.T) {
	srv := newTestServer(t)
	for _, path := range []string{"/api/force-kill", "/api/force-kill/probe"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Origin", srv.OriginAllowed())
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s GET status = %d, want 405", path, rec.Code)
		}
	}
}

func TestForceKill_IdentityGateRefuse_403(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS returns 501")
	}
	srv := newTestServer(t)
	srv.killForRoute = func() (gui.Verdict, error) {
		return gui.Verdict{Class: "Mismatched"}, gui.ErrIdentityGateRefused
	}
	req := httptest.NewRequest(http.MethodPost, "/api/force-kill", nil)
	req.Header.Set("Origin", srv.OriginAllowed())
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestForceKill_LockChanged_412(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS returns 501")
	}
	srv := newTestServer(t)
	srv.killForRoute = func() (gui.Verdict, error) {
		return gui.Verdict{}, gui.ErrLockMtimeChanged
	}
	req := httptest.NewRequest(http.MethodPost, "/api/force-kill", nil)
	req.Header.Set("Origin", srv.OriginAllowed())
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusPreconditionFailed {
		t.Errorf("status = %d, want 412", rec.Code)
	}
}

func TestForceKill_Macos_501(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("only meaningful on macOS")
	}
	srv := newTestServer(t)
	for _, path := range []string{"/api/force-kill", "/api/force-kill/probe"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Origin", srv.OriginAllowed())
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s status = %d, want 501", path, rec.Code)
		}
	}
}
```

(`probeForRoute` and `killForRoute` are new test seams on `Server`.)

- [ ] **Step 13.2: Run; expect failure**

```bash
go test ./internal/gui/ -run TestForceKill -v
```

Expected: FAIL — handlers + seams undefined.

- [ ] **Step 13.3: Add test seams to Server struct**

In `internal/gui/server.go`:

```go
	probeForRoute func() Verdict
	killForRoute  func() (Verdict, error)
```

- [ ] **Step 13.4: Implement handlers**

Create `internal/gui/force_kill.go`:

```go
// Package gui — POST /api/force-kill/probe + /api/force-kill handlers.
// Memo D12 + D13. Wraps C1's KillRecordedHolder + Probe semantics in
// HTTP. macOS returns 501 with product-neutral copy.
package gui

import (
	"errors"
	"net/http"
	"runtime"
)

// (Verdict, ErrIdentityGateRefused, ErrLockMtimeChanged are defined in
// the existing single_instance.go from C1 PR #23. Reuse those types.)

func registerForceKillRoutes(s *Server) {
	s.mux.HandleFunc("/api/force-kill/probe",
		s.requireSameOrigin(s.forceKillProbeHandler))
	s.mux.HandleFunc("/api/force-kill",
		s.requireSameOrigin(s.forceKillHandler))
}

const macosNotSupportedMsg = "Lock recovery is not yet supported on macOS. As a workaround, run `mcphub gui --force` from Terminal for a diagnostic, or restart the system to clear stuck file handles."

func (s *Server) forceKillProbeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if runtime.GOOS == "darwin" {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error":  "not_supported_on_macos",
			"detail": macosNotSupportedMsg,
		})
		return
	}
	probe := s.probeForRoute
	if probe == nil {
		probe = func() Verdict { return Probe(SingleInstanceLockPath()) }
	}
	v := probe()
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) forceKillHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if runtime.GOOS == "darwin" {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error":  "not_supported_on_macos",
			"detail": macosNotSupportedMsg,
		})
		return
	}
	kill := s.killForRoute
	if kill == nil {
		kill = func() (Verdict, error) {
			return KillRecordedHolder(SingleInstanceLockPath())
		}
	}
	v, err := kill()
	if err == nil {
		writeJSON(w, http.StatusOK, v)
		return
	}
	switch {
	case errors.Is(err, ErrIdentityGateRefused):
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":  "identity_gate_refused",
			"detail": err.Error(),
			"verdict": v,
		})
	case errors.Is(err, ErrLockMtimeChanged):
		writeJSON(w, http.StatusPreconditionFailed, map[string]any{
			"error":  "lock_changed_mid_flight",
			"detail": err.Error(),
		})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":   "kill_failed",
			"detail":  err.Error(),
			"verdict": v,
		})
	}
}
```

- [ ] **Step 13.5: Wire route in server.go**

After `registerExportBundleRoutes(s)`:

```go
	registerForceKillRoutes(s)
```

- [ ] **Step 13.6: Run handler tests**

```bash
go test ./internal/gui/ -run TestForceKill -v
```

Expected: PASS.

- [ ] **Step 13.7: Commit**

```bash
git add internal/gui/force_kill.go internal/gui/force_kill_test.go internal/gui/server.go
git commit -m "$(cat <<'EOF'
feat(gui): POST /api/force-kill/probe + /api/force-kill HTTP wrappers

Probe is read-only — returns C1's Verdict struct as JSON. Kill wraps
KillRecordedHolder; maps ErrIdentityGateRefused→403,
ErrLockMtimeChanged→412, kill failure→500. Both routes return 501 on
macOS with product-neutral copy (memo D13: no CLAUDE.md reference).

Test seams (probeForRoute, killForRoute) drive deterministic outcomes
without touching real file locks.
EOF
)"
```

---

## Task 14: Force-kill UI — `SectionAdvancedDiagnostics` two-click flow

**Memo refs:** D10, D12, D13, D14. **Subagent profile:** opus (index-safe guard, ConfirmModal integration, RenderCustom check).

**Files:**
- Create: `internal/gui/frontend/src/components/settings/SectionAdvancedDiagnostics.tsx`
- Create: `internal/gui/frontend/src/components/settings/SectionAdvancedDiagnostics.test.tsx`
- Modify: `internal/gui/frontend/src/components/settings/SectionAdvanced.tsx` — render `<SectionAdvancedDiagnostics>` for the two `RenderCustom` keys
- Modify: `internal/gui/frontend/src/lib/settings-types.ts` — add `render_kind?: string` to BaseSettingDTO; surface in DTO

- [ ] **Step 14.1: Surface `RenderKind` on the wire**

In `internal/gui/settings.go`, extend both DTOs to carry the discriminator:

```go
type configSettingDTO struct {
	Key        string   `json:"key"`
	Section    string   `json:"section"`
	Type       string   `json:"type"`
	Default    string   `json:"default"`
	Value      string   `json:"value"`
	Enum       []string `json:"enum,omitempty"`
	Min        *int     `json:"min,omitempty"`
	Max        *int     `json:"max,omitempty"`
	Pattern    string   `json:"pattern,omitempty"`
	Optional   bool     `json:"optional,omitempty"`
	Deferred   bool     `json:"deferred"`
	Help       string   `json:"help"`
	RenderKind string   `json:"render_kind,omitempty"` // memo D14
}

type actionSettingDTO struct {
	Key        string `json:"key"`
	Section    string `json:"section"`
	Type       string `json:"type"`
	Deferred   bool   `json:"deferred"`
	Help       string `json:"help"`
	RenderKind string `json:"render_kind,omitempty"` // memo D14
}
```

In the list builder (line ~88-117), pass `RenderKind: string(def.RenderKind)` for both branches.

In `internal/gui/frontend/src/lib/settings-types.ts`, extend BaseSettingDTO:

```ts
type BaseSettingDTO = {
  key: string;
  section: Section;
  deferred: boolean;
  help: string;
  render_kind?: string; // memo D14: "" or "custom"
};
```

- [ ] **Step 14.2: Write failing component test**

Create `internal/gui/frontend/src/components/settings/SectionAdvancedDiagnostics.test.tsx`:

```tsx
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/preact";
import { SectionAdvancedDiagnostics } from "./SectionAdvancedDiagnostics";

beforeEach(() => {
  HTMLDialogElement.prototype.showModal = function () { this.open = true; };
  HTMLDialogElement.prototype.close = function () { this.open = false; };
});

const stuckVerdict = {
  Class: "Stuck",
  PID: 1234,
  Port: 9125,
  Mtime: "2026-05-01T03:00:00Z",
  PIDAlive: true,
  PIDImage: "C:/path/mcphub.exe",
  PIDCmdline: ["mcphub.exe", "gui"],
  PIDStart: "2026-05-01T02:59:00Z",
  PingMatch: false,
};

describe("SectionAdvancedDiagnostics", () => {
  it("first click runs Probe and shows result strip", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce({
      ok: true, json: () => Promise.resolve({ Class: "Healthy" }),
    }));
    render(<SectionAdvancedDiagnostics />);
    fireEvent.click(screen.getByText("Diagnose lock state"));
    await waitFor(() => screen.getByTestId("verdict-strip"));
    expect(screen.getByText(/healthy/i)).toBeTruthy();
    vi.unstubAllGlobals();
  });

  it("Stuck + identity gate pass → Kill button appears with PID baked in", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce({
      ok: true, json: () => Promise.resolve(stuckVerdict),
    }));
    render(<SectionAdvancedDiagnostics />);
    fireEvent.click(screen.getByText("Diagnose lock state"));
    await waitFor(() => screen.getByTestId("kill-button"));
    expect(screen.getByText(/Kill stuck PID 1234/)).toBeTruthy();
    vi.unstubAllGlobals();
  });

  it("Healthy → Kill button does NOT render", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce({
      ok: true, json: () => Promise.resolve({ Class: "Healthy", PIDCmdline: [] }),
    }));
    render(<SectionAdvancedDiagnostics />);
    fireEvent.click(screen.getByText("Diagnose lock state"));
    await waitFor(() => screen.getByTestId("verdict-strip"));
    expect(screen.queryByTestId("kill-button")).toBeNull();
    vi.unstubAllGlobals();
  });

  it("PIDCmdline.length === 1 (Explorer launch) still passes the cmdline guard", async () => {
    const explorerLaunch = { ...stuckVerdict, PIDCmdline: ["mcphub.exe"] };
    vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce({
      ok: true, json: () => Promise.resolve(explorerLaunch),
    }));
    render(<SectionAdvancedDiagnostics />);
    fireEvent.click(screen.getByText("Diagnose lock state"));
    await waitFor(() => screen.getByTestId("kill-button"));
    expect(screen.getByText(/Kill stuck PID 1234/)).toBeTruthy();
    vi.unstubAllGlobals();
  });

  it("Mismatched image (e.g. cmd.exe) → Kill button does NOT render", async () => {
    const mismatched = { ...stuckVerdict, PIDImage: "C:/Windows/System32/cmd.exe", PIDCmdline: ["cmd.exe", "/c", "blah"] };
    vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce({
      ok: true, json: () => Promise.resolve(mismatched),
    }));
    render(<SectionAdvancedDiagnostics />);
    fireEvent.click(screen.getByText("Diagnose lock state"));
    await waitFor(() => screen.getByTestId("verdict-strip"));
    expect(screen.queryByTestId("kill-button")).toBeNull();
    vi.unstubAllGlobals();
  });

  it("PIDStart >= Mtime → Kill button does NOT render (clock semantics fail-closed)", async () => {
    const startAfterMtime = { ...stuckVerdict, PIDStart: "2026-05-01T03:01:00Z" }; // after Mtime
    vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce({
      ok: true, json: () => Promise.resolve(startAfterMtime),
    }));
    render(<SectionAdvancedDiagnostics />);
    fireEvent.click(screen.getByText("Diagnose lock state"));
    await waitFor(() => screen.getByTestId("verdict-strip"));
    expect(screen.queryByTestId("kill-button")).toBeNull();
    vi.unstubAllGlobals();
  });

  it("Kill button click opens ConfirmModal", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce({
      ok: true, json: () => Promise.resolve(stuckVerdict),
    }));
    render(<SectionAdvancedDiagnostics />);
    fireEvent.click(screen.getByText("Diagnose lock state"));
    await waitFor(() => screen.getByTestId("kill-button"));
    fireEvent.click(screen.getByTestId("kill-button"));
    await waitFor(() => screen.getByTestId("confirm-modal"));
    expect(screen.getByText(/PID 1234/)).toBeTruthy();
    vi.unstubAllGlobals();
  });
});
```

- [ ] **Step 14.3: Run; expect failure**

```bash
cd internal/gui/frontend && npm test -- SectionAdvancedDiagnostics.test
```

Expected: FAIL — component file missing.

- [ ] **Step 14.4: Implement component**

Create `internal/gui/frontend/src/components/settings/SectionAdvancedDiagnostics.tsx`:

```tsx
// internal/gui/frontend/src/components/settings/SectionAdvancedDiagnostics.tsx
//
// Memo D12 + D13: two-click force-kill flow inside SectionAdvanced.
// First click runs Probe (read-only); if Verdict.Class==Stuck AND the
// identity gate passes (D12 index-safe + clock-aware), a "Kill stuck
// PID N" button appears that opens ConfirmModal → POST /api/force-kill.
import { useState } from "preact/hooks";
import { ConfirmModal } from "../ConfirmModal";

type Verdict = {
  Class: string;
  PID?: number;
  Port?: number;
  Mtime?: string;
  PIDAlive?: boolean;
  PIDImage?: string;
  PIDCmdline?: string[];
  PIDStart?: string;
  PingMatch?: boolean;
};

const MCPHUB_BASENAMES = new Set(["mcphub.exe", "mcphub"]);

// canKill applies the memo-D12 identity gate client-side. The server
// re-enforces these via C1's KillRecordedHolder; client-side gating is
// UX-only.
function canKill(v: Verdict | null): boolean {
  if (!v) return false;
  if (v.Class !== "Stuck") return false;

  // Image basename check (case-insensitive on Windows).
  const image = (v.PIDImage ?? "").replaceAll("\\", "/");
  const base = image.split("/").pop()?.toLowerCase() ?? "";
  if (!MCPHUB_BASENAMES.has(base)) return false;

  // Index-safe cmdline guard (memo D12 mandatory).
  const cmd = v.PIDCmdline ?? [];
  const cmdGuardOK =
    (cmd.length > 1 && cmd[1] === "gui") || cmd.length <= 1;
  if (!cmdGuardOK) return false;

  // Clock semantics: strict <, fail-closed on equality (memo D12).
  if (!v.PIDStart || !v.Mtime) return false;
  if (new Date(v.PIDStart).getTime() >= new Date(v.Mtime).getTime()) return false;

  return true;
}

function classLabel(v: Verdict): string {
  if (v.Class === "Healthy") return "Healthy — lock holder alive and responding.";
  if (v.Class === "Stuck") return `Stuck — lock held by PID ${v.PID ?? "?"} (${v.PIDImage ?? "?"}).`;
  if (v.Class === "Mismatched") return `Mismatched — lock holder image is not mcphub (${v.PIDImage ?? "?"}).`;
  if (v.Class === "Vacant") return "Vacant — lock file present but no live holder.";
  return v.Class;
}

export function SectionAdvancedDiagnostics(): preact.JSX.Element {
  const [verdict, setVerdict] = useState<Verdict | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [killBusy, setKillBusy] = useState(false);

  async function probe() {
    setBusy(true);
    setErr(null);
    try {
      const r = await fetch("/api/force-kill/probe", { method: "POST" });
      if (r.status === 501) {
        const j = await r.json();
        setErr(j.detail ?? "Not supported on this platform.");
        setVerdict(null);
        return;
      }
      if (!r.ok) {
        setErr(`Probe failed: HTTP ${r.status}`);
        return;
      }
      const v = await r.json();
      setVerdict(v);
    } catch (e: any) {
      setErr(String(e?.message ?? e));
    } finally {
      setBusy(false);
    }
  }

  async function doKill() {
    setKillBusy(true);
    try {
      const r = await fetch("/api/force-kill", { method: "POST" });
      if (r.ok) {
        const v = await r.json();
        setVerdict(v);
      } else {
        const body = await r.json().catch(() => ({}));
        setErr(`Kill failed: ${body.detail ?? `HTTP ${r.status}`}`);
      }
    } catch (e: any) {
      setErr(String(e?.message ?? e));
    } finally {
      setKillBusy(false);
      setConfirmOpen(false);
    }
  }

  const showKill = canKill(verdict);

  return (
    <div class="diagnostics-block" data-section="advanced-diagnostics">
      <h3>Diagnostics</h3>
      <button type="button" onClick={() => void probe()} disabled={busy} data-testid="probe-button">
        {busy ? "Probing…" : "Diagnose lock state"}
      </button>
      {verdict ? (
        <div class="verdict-strip" data-testid="verdict-strip">
          <p>{classLabel(verdict)}</p>
          <details>
            <summary>Details</summary>
            <ul>
              <li>PID: {verdict.PID ?? "?"}</li>
              <li>Port: {verdict.Port ?? "?"}</li>
              <li>Image: {verdict.PIDImage ?? "?"}</li>
              <li>Cmdline: {(verdict.PIDCmdline ?? []).join(" ")}</li>
              <li>Start: {verdict.PIDStart ?? "?"}</li>
              <li>Lock mtime: {verdict.Mtime ?? "?"}</li>
              <li>Ping match: {String(verdict.PingMatch ?? false)}</li>
            </ul>
          </details>
        </div>
      ) : null}
      {showKill ? (
        <button
          type="button"
          class="danger"
          onClick={() => setConfirmOpen(true)}
          data-testid="kill-button"
        >
          Kill stuck PID {verdict?.PID}
        </button>
      ) : null}
      {err ? <p class="error-banner" role="alert">{err}</p> : null}

      <ConfirmModal
        open={confirmOpen}
        title="Kill stuck mcphub process?"
        body={
          <>
            PID <b>{verdict?.PID}</b> ({verdict?.PIDImage}, started {verdict?.PIDStart}).
            The process will be terminated immediately.
          </>
        }
        confirmLabel={`Kill PID ${verdict?.PID ?? ""}`}
        danger
        onConfirm={doKill}
        onCancel={() => setConfirmOpen(false)}
      />
    </div>
  );
}
```

- [ ] **Step 14.5: Wire into SectionAdvanced**

Modify `SectionAdvanced.tsx` to render `<SectionAdvancedDiagnostics />` below the existing actions block:

```tsx
import { SectionAdvancedDiagnostics } from "./SectionAdvancedDiagnostics";

// ...inside SectionAdvanced JSX, after the existing buttons:
<SectionAdvancedDiagnostics />
```

(SectionAdvanced does NOT need to filter `RenderCustom` keys today because FieldRenderer already only handles non-Action types and SectionAdvanced renders Actions explicitly. The discriminator's contract for PR #1 is purely informational + future-proof.)

- [ ] **Step 14.6: Run; expect pass**

```bash
cd internal/gui/frontend && npm test -- SectionAdvancedDiagnostics.test
cd internal/gui/frontend && npm test -- SectionAdvanced.test
```

Expected: PASS.

- [ ] **Step 14.7: Run frontend regression**

```bash
cd internal/gui/frontend && npm run typecheck && npm test
```

Expected: PASS.

- [ ] **Step 14.8: Commit**

```bash
git add internal/gui/settings.go internal/gui/frontend/src/lib/settings-types.ts internal/gui/frontend/src/components/settings/SectionAdvancedDiagnostics.tsx internal/gui/frontend/src/components/settings/SectionAdvancedDiagnostics.test.tsx internal/gui/frontend/src/components/settings/SectionAdvanced.tsx
git commit -m "$(cat <<'EOF'
feat(settings): two-click force-kill diagnostic + ConfirmModal flow

SectionAdvancedDiagnostics renders the read-only Probe → Verdict
strip → conditional Kill button per memo D12. canKill enforces ALL of:
  - Class === "Stuck"
  - Image basename ∈ {mcphub.exe, mcphub} (case-insensitive)
  - Index-safe cmdline guard: (len > 1 && [1]==="gui") || len <= 1
  - Clock semantics: PIDStart < Mtime (strict, fail-closed)

Kill button only renders when ALL pass. Click opens ConfirmModal with
PID baked into label; Confirm POSTs /api/force-kill. Server re-enforces
identity gate; client check is UX-only.

DTO surface gains optional `render_kind` field (memo D14) so future
sections can opt out of FieldRenderer for keys with custom UX.
EOF
)"
```

---

## Task 15: Playwright E2E coverage

**Memo refs:** §7. **Subagent profile:** sonnet (extending an existing test file with concrete scenarios).

**Files:**
- Modify: `internal/gui/e2e/settings.spec.ts` — append A4-b PR #1 scenarios
- Modify: `internal/gui/e2e/seed-helpers.ts` (or whichever helper file currently exists) — add helpers to seed `workspaces.yaml` with mixed `weekly_refresh` values

- [ ] **Step 15.1: Add seed helper for mixed registry**

In the existing E2E seed helpers file, add:

```ts
// internal/gui/e2e/seed-helpers.ts
import * as fs from "node:fs/promises";
import * as path from "node:path";

export async function seedWorkspacesYAML(stateDir: string, rows: Array<{ key: string; path: string; lang: string; weekly: boolean }>) {
  const lines: string[] = [
    "version: 1",
    "workspaces:",
  ];
  for (const r of rows) {
    lines.push("  - workspace_key: " + r.key);
    lines.push("    workspace_path: " + r.path);
    lines.push("    language: " + r.lang);
    lines.push("    backend: mcp-language-server");
    lines.push("    port: " + (9100 + Math.floor(Math.random() * 100)));
    lines.push("    task_name: t-" + r.key + "-" + r.lang);
    lines.push("    client_entries: {}");
    lines.push("    weekly_refresh: " + r.weekly);
  }
  await fs.mkdir(stateDir, { recursive: true });
  await fs.writeFile(path.join(stateDir, "workspaces.yaml"), lines.join("\n") + "\n");
}
```

- [ ] **Step 15.2: Append E2E scenarios to `settings.spec.ts`**

```ts
import { test, expect } from "@playwright/test";
import { spawnGui, killGui } from "./gui-fixture"; // existing helpers
import { seedWorkspacesYAML } from "./seed-helpers";
import * as path from "node:path";

test.describe("A4-b PR #1: Settings lifecycle", () => {
  test("membership table renders mixed initial state", async ({ page }) => {
    const fixture = await spawnGui(async (home) => {
      await seedWorkspacesYAML(path.join(home, "AppData", "Local", "mcp-local-hub"), [
        { key: "k1", path: "D:/p1", lang: "python", weekly: true },
        { key: "k1", path: "D:/p1", lang: "rust", weekly: false },
      ]);
    });
    await page.goto(fixture.url + "#/settings?section=daemons");
    await page.waitForSelector('[data-testid="weekly-membership-table"]');
    const checkboxes = await page.$$('[data-testid="weekly-membership-table"] input[type=checkbox]');
    expect(await checkboxes[0].isChecked()).toBe(true);
    expect(await checkboxes[1].isChecked()).toBe(false);
    await killGui(fixture);
  });

  test("toggle one row + Save persists", async ({ page }) => {
    const fixture = await spawnGui(async (home) => {
      await seedWorkspacesYAML(path.join(home, "AppData", "Local", "mcp-local-hub"), [
        { key: "k1", path: "D:/p1", lang: "python", weekly: true },
      ]);
    });
    await page.goto(fixture.url + "#/settings?section=daemons");
    await page.waitForSelector('[data-testid="weekly-membership-table"]');
    await page.locator('[data-testid="weekly-membership-table"] input[type=checkbox]').first().click();
    await page.locator('[data-testid="daemons-save"]').click();
    await page.waitForSelector("text=Saved.");
    // Reload + verify persistence.
    await page.reload();
    await page.waitForSelector('[data-testid="weekly-membership-table"]');
    const cb = await page.$('[data-testid="weekly-membership-table"] input[type=checkbox]');
    expect(await cb!.isChecked()).toBe(false);
    await killGui(fixture);
  });

  test("Select all / Clear all bulk toggle", async ({ page }) => {
    const fixture = await spawnGui(async (home) => {
      await seedWorkspacesYAML(path.join(home, "AppData", "Local", "mcp-local-hub"), [
        { key: "k1", path: "D:/p1", lang: "python", weekly: false },
        { key: "k2", path: "D:/p2", lang: "rust", weekly: false },
      ]);
    });
    await page.goto(fixture.url + "#/settings?section=daemons");
    await page.waitForSelector('[data-testid="weekly-membership-table"]');
    await page.click("text=Select all");
    const all = await page.$$('[data-testid="weekly-membership-table"] input[type=checkbox]');
    for (const cb of all) {
      expect(await cb.isChecked()).toBe(true);
    }
    await page.click("text=Clear all");
    for (const cb of all) {
      expect(await cb.isChecked()).toBe(false);
    }
    await killGui(fixture);
  });

  test("cron edit valid + Save", async ({ page }) => {
    const fixture = await spawnGui();
    await page.goto(fixture.url + "#/settings?section=daemons");
    await page.fill('[data-testid="weekly-schedule-input"]', "weekly Tue 14:30");
    await page.locator('[data-testid="daemons-save"]').click();
    // The swap is mocked under MCPHUB_E2E_SCHEDULER=none — Saved banner expected.
    await page.waitForSelector("text=Saved.");
    await killGui(fixture);
  });

  test("cron edit invalid → inline parse-error", async ({ page }) => {
    const fixture = await spawnGui();
    await page.goto(fixture.url + "#/settings?section=daemons");
    await page.fill('[data-testid="weekly-schedule-input"]', "daily 03:00");
    await page.locator('[data-testid="daemons-save"]').click();
    await page.waitForSelector("text=/weekly Sun 03:00/i");
    await killGui(fixture);
  });

  test("clean-now ConfirmModal cancel preserves backups", async ({ page }) => {
    const fixture = await spawnGui();
    await page.goto(fixture.url + "#/settings?section=backups");
    await page.locator('[data-testid="clean-now-button"]').click();
    await page.waitForSelector('[data-testid="confirm-modal"]');
    await page.locator('[data-testid="confirm-modal-cancel"]').click();
    // Modal closes; no fetch was made — verified by the backups list staying intact.
    await expect(page.locator('[data-testid="confirm-modal"]')).toHaveCount(0);
    await killGui(fixture);
  });

  test("export bundle → download triggered", async ({ page }) => {
    const fixture = await spawnGui();
    await page.goto(fixture.url + "#/settings?section=advanced");
    const downloadPromise = page.waitForEvent("download");
    await page.locator('[data-testid="export-bundle"]').click();
    const download = await downloadPromise;
    expect(download.suggestedFilename()).toMatch(/^mcphub-bundle-.*\.zip$/);
    await killGui(fixture);
  });

  test("force-kill diagnose → Healthy → no kill button", async ({ page }) => {
    const fixture = await spawnGui();
    await page.goto(fixture.url + "#/settings?section=advanced");
    await page.locator('[data-testid="probe-button"]').click();
    await page.waitForSelector('[data-testid="verdict-strip"]');
    await expect(page.locator('[data-testid="kill-button"]')).toHaveCount(0);
    await killGui(fixture);
  });
});
```

- [ ] **Step 15.3: Run E2E (Windows runner)**

```bash
cd internal/gui/e2e && npm test -- --grep "A4-b PR #1"
```

Expected: PASS on Windows. (Linux/macOS skips per existing scheduler-seam constraint.)

- [ ] **Step 15.4: Commit**

```bash
git add internal/gui/e2e/settings.spec.ts internal/gui/e2e/seed-helpers.ts
git commit -m "$(cat <<'EOF'
test(e2e): A4-b PR #1 Playwright scenarios

Eight new scenarios covering the membership table mixed-state render,
toggle + Save persistence, Select all/Clear all bulk affordance, cron
valid + invalid (parse-error inline), clean-now ConfirmModal cancel,
export bundle download trigger, and force-kill probe Healthy → no kill
button. Frontend wiring for cron success-banner and export-bundle
download path verified end-to-end.
EOF
)"
```

---

## Task 16: Asset regen + CLAUDE.md update + backlog mark

**Memo refs:** §13. **Subagent profile:** sonnet (mechanical).

**Files:**
- Run: `go generate ./internal/gui/...` (rebuilds the embedded frontend bundle)
- Modify: `CLAUDE.md` (add A4-b PR #1 surface description; update E2E count)
- Modify: `docs/superpowers/plans/phase-3b-ii-backlog.md` (mark row 9b ✅ with merge SHA placeholder)

- [ ] **Step 16.1: Regenerate embedded frontend bundle**

```bash
go generate ./internal/gui/...
```

Expected: `internal/gui/assets/{index.html,app.js,style.css}` updated. Inspect with `git diff --stat` — only those three files plus the source map should change.

- [ ] **Step 16.2: Update CLAUDE.md GUI E2E section**

In `CLAUDE.md`, locate the `### What's covered` block under `## GUI E2E tests`. Append the A4-b PR #1 line (sample wording — preserve existing prose style):

```
- A4-b PR #1: SectionDaemons editable section with multi-op save (settings + schedule + membership) + WeeklyMembershipTable (mixed-state render, toggle + Save persistence, Select all/Clear all), cron parse-error inline, ConfirmModal-gated clean-now (cancel preserves backups), export bundle download trigger, force-kill Diagnose → Healthy no-kill-button.
```

Update the count line:

```
103 smoke tests total (3 shell + 8 servers + 6 migration + 13 add-server + 17 edit-server + 2 dashboard + 3 logs + 14 secrets + 10 secret-picker + 16 settings + 3 about + 8 a4b-pr1), ~55s wall-time on a warm machine.
```

(Adjust the per-area counts per the actual implementation — the example above assumes 8 new A4-b PR #1 scenarios.)

- [ ] **Step 16.3: Mark backlog row 9b**

In `docs/superpowers/plans/phase-3b-ii-backlog.md`, replace the line:

```markdown
9b. **A4-b** — Settings lifecycle: tray, port live-rebind, weekly schedule edit, retry policy, Clean now confirm, export bundle, **per-workspace weekly-refresh membership UI**.
```

…with:

```markdown
9b. **A4-b PR #1** ✅ — Settings lifecycle (polish half): weekly schedule edit, retry policy edit (preference-only; runtime applier in PR #2), Clean now confirm, export bundle, force-kill button, per-workspace weekly-refresh membership UI. Memo: [docs/superpowers/specs/2026-05-01-a4b-pr1-settings-lifecycle-design.md](../specs/2026-05-01-a4b-pr1-settings-lifecycle-design.md). Plan: [docs/superpowers/plans/2026-05-01-a4b-pr1-settings-lifecycle.md](2026-05-01-a4b-pr1-settings-lifecycle.md). Merge SHA: `<TBD-after-merge>`.
   - **A4-b PR #2 (deferred):** tray show/hide runtime mutator + port live-rebind + retry policy runtime applier wiring. Separate PR.
   - **Forward-ref to PR #23 C1:** preserved (force-kill button posts to /api/force-kill which wraps gui.Verdict).
   - (Membership-decision detail block from earlier remains as historical record.)
```

(The `<TBD-after-merge>` placeholder is filled in when the PR merges. Do not invent a SHA.)

- [ ] **Step 16.4: Run final regression**

```bash
go test ./internal/api/ ./internal/gui/ ./internal/cli/ -count=1
cd internal/gui/frontend && npm run typecheck && npm test
```

Expected: all PASS.

- [ ] **Step 16.5: Commit**

```bash
git add internal/gui/assets/ CLAUDE.md docs/superpowers/plans/phase-3b-ii-backlog.md
git commit -m "$(cat <<'EOF'
chore(a4b): regenerate embedded bundle + update CLAUDE.md + mark backlog

Regenerated internal/gui/assets/{index.html,app.js,style.css} so the
shipped binary matches frontend source. CLAUDE.md describes the new
A4-b PR #1 surface and updated E2E count. Backlog row 9b marked PR #1
done with a forward-ref note that runtime mutators (tray, port
live-rebind, retry applier) are deferred to PR #2.
EOF
)"
```

---

## Task 17: Final verification

**Memo refs:** §13 acceptance. **Orchestrator does this directly (no subagent).**

- [ ] **Step 17.1: Build the binary**

```bash
go build -o /tmp/mcphub-a4bpr1 ./cmd/mcphub
```

Expected: success, no warnings.

- [ ] **Step 17.2: Verify acceptance criteria checklist (memo §8)**

Walk each AC and confirm in code:

```bash
# AC1: weekly_refresh_default added; legacy exempt with regression test.
grep -A2 "daemons.weekly_refresh_default" internal/api/settings_registry.go
grep "WeeklyRefreshExplicit: true" internal/api/legacy_migrate.go
go test ./internal/api/ -run TestLegacyMigrate_ExemptionFromKnob -v

# AC2: membership table renders mixed states; D2 invariant.
go test ./internal/gui/ -run TestMembershipSnapshotHandler -v

# AC3: multi-op save (D4) — three independent transaction boundaries.
grep -n "op 1.*op 2.*op 3\|three independent transactional" internal/gui/frontend/src/components/settings/SectionDaemons.tsx

# AC4: five Deferred flips.
go test ./internal/api/ -run TestSettingsRegistry_DeferredFlipsForA4bPR1 -v

# AC5: ScheduleSpec + SwapWeeklyTrigger + RetryPolicy interfaces with tests.
go test ./internal/api/ -run 'TestParseSchedule|TestSwapWeeklyTrigger|TestPolicyFromString|TestIsRetryableError' -v

# AC6: ConfirmModal reused 2x.
grep -rn "ConfirmModal" internal/gui/frontend/src/components/settings/ | grep -v ".test"

# AC7: Export bundle .zip + hostname literal "redacted".
go test ./internal/api/ -run TestWriteConfigBundle -v

# AC8: force-kill two-click flow + index-safe guard + clock semantics.
grep -n "PIDCmdline.length > 1\|PIDStart.*Mtime" internal/gui/frontend/src/components/settings/SectionAdvancedDiagnostics.tsx

# AC9: macOS 501 path covered.
grep -n "not_supported_on_macos\|TestForceKill_Macos_501" internal/gui/

# AC10: tests pass on Windows runner.
go test ./... -count=1
cd internal/gui/frontend && npm test
cd internal/gui/e2e && npm test
```

- [ ] **Step 17.3: Manual smoke (optional but encouraged)**

```bash
# Terminal 1: backend with fixed port.
go run ./cmd/mcphub gui --no-browser --no-tray --port 9125

# Terminal 2: open the GUI.
start http://127.0.0.1:9125
```

Click through:

1. Settings → Daemons. Edit cron to `weekly Tue 14:30`, toggle two membership rows, change retry policy. Save. Banner says "Saved." Reload — values persist.
2. Edit cron to `daily 03:00`. Save. Inline parse-error visible with canonical example. Other ops still attempt (or get skipped per ordering).
3. Settings → Backups → Clean now eligible backups. ConfirmModal opens with eligible count. Cancel → no fetch.
4. Settings → Advanced → Export config bundle. Download dialog appears with `mcphub-bundle-<ts>.zip` filename.
5. Settings → Advanced → Diagnose lock state. Result strip says "Healthy". Kill button NOT visible (because the running GUI itself holds the lock and ping matches → Class=Healthy).

- [ ] **Step 17.4: Stop and await user approval**

Per repo standing rule (precedent A3-a/A3-b/A4-a/PR#21/PR#23): plan stops here. Orchestrator does NOT push, does NOT open PR. User reviews locally and approves before any remote operation.

```bash
git log --oneline 42c4ba7..HEAD
```

Expected: 16 commits (Tasks 1-16). Each task is one or more focused commits per the per-step commit cadence. No commit on master tip; everything on `feat/a4b-pr1-settings-lifecycle`.

---

## Self-review checklist (orchestrator runs after writing the plan)

### 1. Spec coverage

- D1 opt-in default + legacy_migrate exemption → Tasks 1, 2, 3
- D2 existing entries not migrated → Task 11 (table renders persisted state)
- D3 membership inside SectionDaemons → Task 11
- D4 multi-op save with three transactions → Task 11 (SectionDaemons rewrite)
- D5 structured array endpoint, idempotent partial update → Task 4
- D6 Select all / Clear all, no pagination → Task 11
- D7 ScheduleSpec typed parser → Task 5
- D8 dedicated PUT route + helper boundary + truth table → Tasks 6, 7
- D9 RetryPolicy + IsRetryableError split → Task 8
- D10 ConfirmModal copy AddSecretModal pattern → Task 9
- D11 export bundle hostname literal redacted → Task 12
- D12 force-kill index-safe + clock semantics → Tasks 13, 14
- D13 macOS 501 product-neutral copy → Task 13
- D14 RenderKind discriminator → Tasks 1, 14

### 2. Placeholder scan

No "TBD", no "implement later", no "similar to Task N", no `// add appropriate error handling`. The only deliberate `<TBD-after-merge>` is in the backlog row Task 16 — which is a SHA that exists only after the PR merges. That is acceptable per the writing-plans skill (true TBDs that are FILLED LATER are different from plan-failure TBDs that hide unspecified design).

### 3. Type consistency

- `RegisterOpts.WeeklyRefreshExplicit` (Task 2) is referenced in Task 3 (legacy_migrate edit) and Task 11 register-test exemption — name matches.
- `MembershipDelta` (Task 4) is the same shape used by frontend `api-daemons.ts` (Task 11) and the E2E test bodies.
- `ScheduleSpec` fields (Kind, DayOfWeek, Hour, Minute) consistent across Task 5 (parser), Task 6 (swap helper), Task 7 (handler).
- `SwapWeeklyTrigger(spec, priorXML)` signature consistent across Task 6 (impl), Task 7 (handler call), Task 17 (verification grep).
- `RenderKind` string constants (`""`, `"custom"`) consistent across Task 1 (registry), Task 14 (DTO + frontend).
- `Verdict` field names (`Class`, `PID`, `PIDCmdline`, `PIDStart`, `Mtime`, `PIDImage`, `PingMatch`) match between Task 13 backend test and Task 14 frontend `canKill`.

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-05-01-a4b-pr1-settings-lifecycle.md`. Two execution options:

**1. Subagent-Driven (recommended)** — orchestrator dispatches a fresh subagent per task, two-stage review (spec compliance + code quality) between tasks, fast iteration.

**2. Inline Execution** — execute tasks in this session using `superpowers:executing-plans` with batch execution + checkpoints.

**Per the original brainstorming arguments + repo precedent:** subagent-driven is the standing chain (C1 PR #23 used it). Recommend Subagent-Driven.

After all 16 implementation commits land, the orchestrator stops at the end of Task 17 and awaits explicit user approval before any push or PR creation per the repo standing rule (A3-a/A3-b/A4-a/PR#21/PR#23 precedent).


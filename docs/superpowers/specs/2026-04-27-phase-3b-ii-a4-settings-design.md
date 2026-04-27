# Phase 3B-II A4 — Settings screen (design memo)

**Status:** design memo (pre-plan, rev 2 — Codex r1 applied: 7 P1 + 5 P2 + 2 P3)
**Author:** Claude Code (brainstorm + Codex Q1–Q5 consensus, 2026-04-27)
**Predecessor:** PR #19 (Phase 3B-II A3-b — env.secret picker, merged at `04cc250`)
**Backlog entry:** [`docs/superpowers/plans/phase-3b-ii-backlog.md`](../plans/phase-3b-ii-backlog.md) §A row A4
**Master design reference:** [`docs/superpowers/specs/2026-04-17-phase-3-gui-installer-design.md`](2026-04-17-phase-3-gui-installer-design.md) §5.7
**Sibling memos:**
- [`2026-04-25-phase-3b-ii-a3a-secrets-screen-design.md`](2026-04-25-phase-3b-ii-a3a-secrets-screen-design.md) (Save flow precedent)
- [`2026-04-26-phase-3b-ii-a3b-env-secret-picker-design.md`](2026-04-26-phase-3b-ii-a3b-env-secret-picker-design.md) (single-close-path precedent)

## 1. Summary

A4-a delivers the **Settings screen contract**: a `gui-preferences.yaml`-backed schema with a Go-registry as the authoritative source of truth, a `/api/settings` HTTP surface for the GUI consumer, registry-aware `mcp settings list/get/set` CLI updates, and a single-page Settings screen with **per-section Save** semantics. The screen ships a 7th sidebar link and renders all 5 §5.7 sections inline:

- **Appearance** (4 fields): theme, density, shell, default home — fully editable
- **GUI server** (2 fields): browser-on-launch, port (save-pending-restart, no live rebind) — fully editable
- **Daemons**: read-only "Configured schedule" display (effective in A4-b)
- **Backups** (1 field + list): keep-N slider with passive prune-preview, `BackupsList` view grouped per client
- **Advanced** (1 action): "Open app-data folder" button

A4-a deliberately ships only **local, deterministic, non-lifecycle** writes. Anything that mutates daemon state, restarts a process, deletes files, or rebinds the GUI server is **deferred to A4-b**, with placeholder-safe registry entries so the schema and CLI cover all 5 sections from day one.

This memo encodes:
- The Q1–Q5 brainstorming consensus as locked design decisions D1–D5 (§3).
- Six implementation-structure decisions D6–D11 not covered by Q1–Q5 (§4).
- The `SettingDef` registry shape and per-section field inventory (§5).
- HTTP wire format and CLI behavior (§6 and §7).
- Component prop surfaces and the per-section Save state machine (§8).
- The Backups section behavior with verbatim Codex copy (§9).
- Risks (§10), test surface (§11), and acceptance criteria (§12).

A4-a does **not** ship the deferred items listed above. Each is recorded in §3.1.D1 and is delivered by **A4-b**, a follow-up PR with a fixed scope: port live rebind, tray, weekly schedule editing, retry policy, `Clean now` confirm flow, export config bundle. A4-b is admitted scope, not "later" — it is enumerated here so users and reviewers can audit the plan honestly.

## 2. Context recon

### 2.1 Existing settings backend — `internal/api/settings.go`

[`internal/api/settings.go`](../../../internal/api/settings.go) provides a flat `map[string]string` YAML at `%LOCALAPPDATA%\mcp-local-hub\gui-preferences.yaml` (Windows) / `$XDG_DATA_HOME/mcp-local-hub/gui-preferences.yaml` (POSIX) with three operations:

```go
func SettingsPath() string                              // canonical file path
func (a *API) SettingsList() (map[string]string, error) // read all
func (a *API) SettingsGet(key string) (string, error)
func (a *API) SettingsSet(key, value string) error
```

Each has a `…In(path, …)` tempdir-capable variant for testing. The store is **stringly-typed**, has **no schema**, **no validation**, **no defaults**, and **no known-keys registry**. There are no HTTP routes for it yet — A4-a adds them.

The file format today: a flat YAML map (e.g., `theme: dark` at the top level), no sections, no nesting. **A4-a does not change this format** (D2.M1 below). The Go registry overlays meaning on top of the flat map.

### 2.2 Existing settings CLI — `internal/cli/settings.go`

`mcp settings list/get/set` already exists and writes through `SettingsList/Get/Set`. It is currently stringly-typed and unaware of any schema. A4-a updates it to consult the registry: `list` includes registry keys with current/default values, `get` exits 1 with `unknown setting <key>` for unknown keys, `set` validates against the registry and warns on `Deferred: true`.

The existing CLI does not have stable consumers yet (Phase 3B-II is the first GUI surface for settings; CLI parity is forward-compatibility scaffolding per the master design). Backwards compatibility for existing users is not a hard constraint — `mcp settings list` output gains structure but does not lose information.

**Unknown-key preservation (Codex r1 P2.1):** because users may pre-populate keys ahead of A4-b (or hand-edit the YAML with a typo), `SettingsSet` must not silently drop unknown keys. The implementation reads the raw YAML map first, validates only the **target key** against the registry, and writes back the full map (target key updated, unknown keys passed through unchanged). `SettingsList`/`SettingsGet` ignore unknown keys when *exposing* the schema-resolved snapshot (so the GUI does not see them), but the file always preserves them. This is enforced by a unit test that round-trips a file containing a non-registry key.

### 2.3 Existing backups backend — `internal/api/backups.go`

[`internal/api/backups.go`](../../../internal/api/backups.go) provides a complete backups surface. A4-a reuses it as-is:

```go
func (a *API) BackupsList() ([]BackupInfo, error)                   // all 4 clients
func (a *API) BackupsClean(keepN int) ([]string, error)             // mutate (deferred to A4-b)
func (a *API) BackupsCleanPreview(keepN int) ([]string, error)      // pure read — used by A4-a
func (a *API) BackupShow(path string) (string, error)               // (not used by A4-a)
func (a *API) RollbackOriginal() ([]RollbackResult, error)          // (not used by A4-a)
```

`BackupInfo` carries `{Client, Path, Kind, ModTime, SizeByte}` where `Kind ∈ {"original", "timestamped"}`. The 4 clients are claude-code, codex-cli, gemini-cli, antigravity (`clientFiles(home)` enumerates them). `BackupsClean` is **per-client**: `keepN` retains the most recent `keepN` timestamped backups *per client*, never touches `-original` sentinels.

### 2.4 Existing process-spawn seam — `internal/gui/browser.go`

[`internal/gui/browser.go`](../../../internal/gui/browser.go) defines `spawnProcess` (the injectable test seam) and `LaunchBrowser`. `LaunchBrowser` is **browser-specific**: it tries Chrome/Chromium/Edge `--app=<url>` first and only falls back to `rundll32 url.dll,FileProtocolHandler` (Windows), `open` (macOS), `xdg-open` (Linux). It is **not** suitable for opening a file-manager — Chrome would happily load the path in a browser tab.

A4-a therefore adds a **dedicated** helper `OpenPath(path string) error` in a new file `internal/gui/openpath.go` (Codex r1 P1.6 fix). `OpenPath` reuses the same `spawnProcess` seam so tests can intercept the spawn, but goes straight to the OS file manager:

```go
// internal/gui/openpath.go
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

The target path for "Open app-data folder" is `filepath.Dir(SettingsPath())` (i.e., `%LOCALAPPDATA%\mcp-local-hub` on Windows).

### 2.5 Outer sidebar nav — `internal/gui/frontend/src/app.tsx`

[`internal/gui/frontend/src/app.tsx`](../../../internal/gui/frontend/src/app.tsx) hosts a fixed 6-link sidebar: Servers, Migration, Add server, Secrets, Dashboard, Logs. A4-a adds a 7th link, "Settings", and the corresponding `case "settings"` branch. The existing `useRouter` parses URLs of shape `#/<screen>?<query>` — a second `#` after the screen name (e.g. `#/settings#backups`) does **not** parse cleanly because everything after the first `#` is the URL fragment. A4-a therefore uses **query-string syntax** for the in-screen anchor: `#/settings?section=backups`, which the existing parser splits as `screen="settings"`, `query="section=backups"`. The Settings screen reads `route.query` to scroll to the matching section. (Codex r1 P1.1 fix — original draft used `#/settings#backups` which would not have routed.)

The existing `useUnsavedChangesGuard(addServerDirty)` machinery is the proven dirty-guard pattern. A4-a generalizes it: the App-level guard tracks `settingsDirty` (boolean OR across all section-local dirty flags) the same way it tracks `addServerDirty`.

## 3. Brainstorm consensus — Q1–Q5

### 3.1 D1 — Scope: H2' (Codex-refined hybrid; user confirmed)

A4-a includes:

- **Contract:** full schema + CLI parity (`mcp settings list/get/set`) for all 5 sections, including placeholder-safe entries for deferred fields
- **Appearance:** `theme`, `density`, `shell`, `default_home` — fully editable
- **GUI server:** `browser_on_launch` — fully editable; `port` — save-pending-restart only (no live rebind)
- **Daemons:** read-only "Configured schedule (effective in A4-b)" display + "edit coming in A4-b" affordance — no edit controls. **Important (Codex r1 P1.7):** the display shows the *configured* (registry / persisted-but-deferred) value, not the actual scheduler runtime state. A4-a labels this honestly so a user-written-via-CLI deferred value is not misread as "what the scheduler is currently doing." Until A4-b wires the editor and the scheduler-state read, "Configured" === "default" for fresh installs.
- **Backups:** `keep_n` slider (with passive prune preview) + `BackupsList` per-client view
- **Advanced:** `open_app_data_folder` action button
- Settings sidebar link in the outer nav

A4-a explicitly **defers** to A4-b:

- `gui_server.tray` (Windows-only restart concerns)
- `gui_server.port` live rebind (A4-a only persists; user manually restarts)
- `daemons.weekly_schedule` editing (touches scheduler state, needs migration/reconciliation semantics)
- `daemons.retry_policy` editing
- `backups.clean_now` confirm flow + per-client partial-failure handling
- `advanced.export_config_bundle` (tar/zip + file-save dialog)

The **rationale** for the split is the *runtime/lifecycle line*: A4-a only writes config; A4-b mutates lifecycle. This makes A4-a's review focus the contract (schema, validation, persistence, CLI parity), and A4-b's review focus mutation correctness (confirm UX, partial-failure reporting, restart sequencing).

**Constraint on deferred-section UI:** partial sections must be **visibly scoped**, with unavailable actions disabled or absent — no clickable dead ends. Daemons therefore renders a status-only line, not a half-built form. The Settings sidebar link stays unconditional (PR1 already exposes a real settings surface; hiding it would worsen discoverability).

### 3.2 D2 — Persistence model: Model 1 (flat YAML + Go registry as authoritative)

The persisted file `gui-preferences.yaml` stays a **flat `map[string]string`**, identical to today's format. The **Go registry** (§5) is the single source of truth for the schema: per-key `{Section, Type, Default, Enum, Min, Max, Pattern, Deferred, Help}`. All validation, defaulting, and metadata flow through the registry.

**Why not nested YAML (Model 2):** mostly storage-shape migration without strong type benefits — values stay strings, structure for UI comes from the registry regardless. Defers a file-format migration that brings no user-visible benefit.

**Why not typed Go struct + TS codegen (Model 3):** correct long-term endpoint after the schema stabilizes, but premature now. The registry will churn as A4-b through A4-d add fields and as deferred entries flip `Deferred: false`. Typed-struct migration becomes safer once the schema is stable.

### 3.3 D3 — Naming convention: dot-notation, lowercase + underscore

CLI keys and HTTP path segments use **dot-notation** matching §5.7 sections:

- `appearance.theme`, `appearance.density`, `appearance.shell`, `appearance.default_home`
- `gui_server.browser_on_launch`, `gui_server.port`, `gui_server.tray` (deferred)
- `daemons.weekly_schedule` (deferred), `daemons.retry_policy` (deferred)
- `backups.keep_n`, `backups.clean_now` (deferred — action, not setting)
- `advanced.open_app_data_folder` (action), `advanced.export_config_bundle` (deferred — action)

Section names are lowercase with `_` separators (`gui_server`, not `gui-server` or `guiServer`). This matches Go field-name idioms and YAML conventions; kebab-case keys lose hierarchy and become awkward once placeholders or subgroups appear.

### 3.4 D4 — Layout: A (single-page scroll + secondary sticky inner nav)

The Settings screen is a single scrollable `<main>` with all 5 sections rendered inline as `<section data-section="appearance">…`. A **sticky, secondary** mini-nav on the left (compact, ~110px wide; subordinate to the outer sidebar) lists section names with active-section highlight (scroll-spy). Click a nav item → smooth-scroll to that section. Deep-link `#/settings?section=backups` scrolls to the Backups section on load (query-string syntax — see §8.5).

**Why A (single-page) over B (inner sidebar) or C (tabs):**

- Matches the existing repo pattern (Servers, Secrets, etc. are all single-page).
- `Ctrl+F` finds any setting on the page — important for a config screen.
- Deferred sections (Daemons read-only) and disabled deferred fields stay visible inline, so users discover that A4-b is coming.
- Scales to A4-b additions without restructuring.

Inner-nav refinement (Codex): keep the mini-nav **compact and secondary** — not visually doming. Use small typography and faint borders so it reads as supportive rather than the primary navigation.

### 3.5 D5 — Apply strategy: P2 (per-section Save)

Each editable section has its own `[Save] [Reset]` pair, both disabled when that section is clean. Saving a section issues sequential `PUT /api/settings/<key>` calls for that section's dirty keys only. **No page-level "Save All"** button.

**Why P2 over P1 (single page-level Save):** the backend's `SettingsSet` is per-key. A page-level Save would either be a frontend-only illusion of atomicity (sequential PUTs that may partially succeed) or require a new bulk endpoint. P2 mirrors the actual contract: per-key writes succeed or fail independently, per-section UI reports per-key status honestly.

**Why P2 over P3/P4 (live-apply for some fields):** dual-mode persistence creates a split-brain UX (some fields silently write, some require Save) and complicates error recovery for already-toggled controls. P2 keeps the contract simple and uniform.

**Per-section Save semantics:**

- Save submits dirty keys via sequential `PUT /api/settings/<key>`. If any key fails (validation 400 / write error 500), the section reports per-key failures and **keeps failed keys dirty** (the user can retry the section without re-typing).
- Reset reverts the section's local state to the last-saved snapshot.
- The dirty-guard at app level (`useUnsavedChangesGuard`) fires if **any** section is dirty (logical OR across all sections).
- For `gui_server.port`: Save persists normally, then renders a "⚠ pending restart" badge next to the port field. No live rebind happens. (Spelled out in §9.2.)

**A4-a actually has 3 Save buttons total:** Appearance, GUI server, Backups. Daemons is read-only. Advanced is an action button (no Save needed).

### 3.6 D6 — Backups section: B2 (passive prune preview)

(This is Q5, but kept here for D-series ordering.)

The Backups section combines a `keep_n` slider with a passive `BackupsCleanPreview`-driven preview of which timestamped backups *would* be cleaned at the current `keep_n` value. **Nothing is deleted** in A4-a; the Clean-now mutation is deferred to A4-b. The preview is for calibration only.

**Locked: server-side preview only.** The preview is computed server-side via `BackupsCleanPreview(keepN)` — A4-a adds a thin HTTP wrapper `GET /api/backups/clean-preview?keep_n=N` (Codex r1 P1.2 fix; the rev-1 draft mentioned a client-side computation alternative, which is now removed). The wrapper exists for two reasons: (1) it keeps `keepN` semantics consistent with what `BackupsClean(keepN)` will eventually delete, so the "preview" never lies; (2) it lets the GUI stay free of file-mtime sorting logic that already lives in `internal/api/backups.go`. The full backups list comes from a separate `GET /api/backups` route also added by A4-a (no current GUI consumer for it; the slider's preview reads it once on mount and re-uses).

The backups list groups rows by client (4 collapsible groups). Rows that *would* be eligible for cleanup at the current slider value are visually dimmed/striped and tagged with a neutral "Would be eligible for cleanup" badge — **no red destructive styling**. If the preview API call fails, the base `BackupsList` view stays visible with an inline "Preview unavailable" message; the base view does not depend on preview success.

Verbatim Codex-locked copy is captured in §9.4.

## 4. Implementation-structure decisions

### 4.1 D7 — Registry shape and authority

The Go registry is a package-level `[]SettingDef` slice in a new file `internal/api/settings_registry.go`. Each entry:

```go
type SettingType string

const (
    TypeEnum   SettingType = "enum"
    TypeBool   SettingType = "bool"
    TypeInt    SettingType = "int"
    TypeString SettingType = "string"
    TypePath   SettingType = "path"
    TypeAction SettingType = "action"
)

type SettingDef struct {
    Key      string      // "appearance.theme"
    Section  string      // "appearance"
    Type     SettingType // see constants above
    Default  string      // string-form default; coerced per Type. Empty allowed when Optional=true.
    Enum     []string    // for TypeEnum: allowed values, ordered for UI
    Min      *int        // for TypeInt: optional lower bound (inclusive)
    Max      *int        // for TypeInt: optional upper bound (inclusive)
    Pattern  string      // for TypeString/TypePath: optional regex (anchored)
    Optional bool        // for TypeString/TypePath: true allows empty value (Codex r1 P1.3)
    Deferred bool        // true → A4-a accepts read/write but warns/disables
    Help     string      // human-readable label/hint (≤120 chars)
}
```

The full registry contents are in §5. Key invariants:

- Every key in the YAML store **must** be in the registry. Unknown keys in the YAML are warning-logged and ignored on read; CLI/HTTP returns `404 unknown setting`.
- The registry is the **only** place that defines defaults. The persisted YAML stores only user-explicit values; missing keys fall back to registry defaults at read time.
- `Deferred: true` means "the schema slot is reserved, but A4-a does not implement the action/effect of writing this field." CLI `set` succeeds (validation runs), HTTP PUT succeeds (persists), but the GUI renders the field disabled with a "(coming in A4-b)" affordance, and CLI prints a warning: `setting accepted; this field is deferred to A4-b and has no effect yet`. **Daemons display caveat (Codex r1 P1.7):** even though the deferred entries persist a value, the Daemons section in A4-a labels them as "Configured (effective in A4-b)" rather than "Current". A user who writes `daemons.weekly_schedule` via CLI sees the value reflected in the GUI as configured/pending, never as "currently active." A4-b will replace the read-only display with an editor and a separate "current scheduler state" line, at which point the distinction becomes load-bearing.
- **Action** type (`open_app_data_folder`, `clean_now`, etc.) is a special slot: not a stored value, but a registry entry that the GUI uses to render an action button. CLI `get` exits 1 with `<key> is an action; use 'mcp settings invoke' (coming in A4-b)`, CLI `set` is rejected with `cannot set action key <key>`, HTTP PUT returns 405 (not 400 — the key exists, the method is wrong). Actions are invoked through dedicated endpoints (e.g., `POST /api/settings/advanced.open_app_data_folder`).
- **Action DTO shape (Codex r1 P2.2):** action entries omit `value` and `default` from the JSON DTO (those fields don't apply). The TypeScript type uses a discriminated union: `type SettingDTO = ConfigSettingDTO | ActionSettingDTO`, where `ConfigSettingDTO` has required `value`/`default` and `ActionSettingDTO` does not. Tests assert the wire shape for both.

### 4.2 D8 — Validation rules

Per-type validation, applied in `SettingsSet` before write and again in `PUT /api/settings/<key>` (defense in depth):

| Type | Validation |
|---|---|
| `TypeEnum` | value ∈ `Enum` |
| `TypeBool` | value ∈ `{"true", "false"}` (lowercase) |
| `TypeInt` | parses as int; if `Min/Max` set, value ∈ [Min, Max] |
| `TypeString` | empty allowed iff `Optional`; if `Pattern` set and value non-empty, regex matches; rejects control chars |
| `TypePath` | empty allowed iff `Optional` (Codex r1 P1.3); if non-empty, **syntactic only**: rejects null bytes, leading/trailing whitespace; does **not** require existence (per H2' "validation can be syntactic/existence-light") |
| `TypeAction` | `set` always rejected with `cannot set action key`; HTTP PUT returns 405 |

Validation errors return a structured shape: `{"error": "validation_failed", "key": "<key>", "reason": "<message>"}` (HTTP 400). The CLI maps this to `error: invalid value for <key>: <reason>` on stderr.

### 4.3 D9 — HTTP API surface

Five endpoints (three settings + two backups), all wrapped with the existing `requireSameOrigin` middleware (Codex r1 P2.5 — matches the precedent in [`internal/gui/csrf.go`](../../../internal/gui/csrf.go) and [`internal/gui/secrets.go`](../../../internal/gui/secrets.go)):

| Method | Path | Behavior |
|---|---|---|
| `GET` | `/api/settings` | Snapshot. Returns `{settings: SettingDTO[], actual_port: <int>}`. See wire format in §6. |
| `PUT` | `/api/settings/<key>` | Validated single-key write. Body: `{"value": "<string>"}`. 400 on validation, 404 on unknown key, 405 on action key, 200 on success. |
| `POST` | `/api/settings/<action_key>` | Invoke action. 404 on unknown key, 405 if key is not `TypeAction` or for unsupported method, 200 on success. Body and response are action-specific (see §9.5 for `open_app_data_folder`). |
| `GET` | `/api/backups` | Returns `{backups: BackupInfo[]}` for all 4 clients. Used by the Backups section. |
| `GET` | `/api/backups/clean-preview` | Query: `?keep_n=<int>`. Returns `{would_remove: ["<path>", ...]}` — the would-prune set computed by `BackupsCleanPreview`. |

All mutating routes (`PUT`, `POST`) return `405` with an `Allow:` header for wrong methods (matches the pattern in `secretsListOrAddHandler`). All routes (mutating and read-only on the settings/backups surface) are wrapped with `s.requireSameOrigin(...)` so cross-origin browser callers receive `403 CROSS_ORIGIN`; non-browser callers (curl with no `Origin`, Sec-Fetch-Site `none`) pass through.

`GET /api/settings/<key>` (single-key read) is **not** added — `GET /api/settings` is cheap (registry is in-memory, file read is one stat) and the GUI fetches the snapshot on screen load. CLI `get <key>` calls `SettingsGet` directly via the in-process API; no HTTP roundtrip. **No DELETE** — reset-to-default is local-only in A4-a (the section's Reset button reverts unsaved edits; persisted defaults are returned by GET when no value is set). An explicit "reset to default" action could come later as a follow-up if needed.

### 4.4 D10 — Section dirty-state tracking and Save flow

Each editable section component owns its local edit state and a `dirty: boolean` flag. The flag is `true` when the local state differs from the last-saved snapshot. The section bubbles `dirty` up to the screen, which OR-aggregates and feeds the result into `useUnsavedChangesGuard` so navigation prompts the standard "Discard unsaved changes?" confirm.

Save flow per section:

1. User clicks `[Save]`. Button enters busy state (disabled + spinner).
2. Section iterates dirty keys, issues `PUT /api/settings/<key>` for each (sequential, not parallel — keeps error reporting deterministic and avoids server-side write races).
3. For each key: success → update **last-saved snapshot** for that key, clear dirty flag, clear inline error; failure → keep local value, keep dirty flag, attach error message inline next to the field.
4. After all keys processed: if any failed, section banner shows `Saved N of M settings. Fix errors below and try again.` If all succeeded, banner shows `Saved.` for ~2s then fades.
5. After step 4, `snapshot.refresh()` is called to re-fetch the canonical state from the backend.
6. Restart-required keys (`gui_server.port`): badge logic moved to a snapshot-derived computation — see §9.2.

**Partial-save + refresh merge rule (Codex r1 P2.3).** After Save completes (regardless of partial failure), `snapshot.refresh()` runs. The section's local edit state must merge with the refreshed snapshot without overwriting the user's still-failed-and-edited keys:

| Key state going into Save | Outcome | Local value after refresh |
| --- | --- | --- |
| Dirty + Save success | Server persisted; local clean | Refreshed value (matches server) |
| Dirty + Save failed | Server unchanged; local still dirty | **User's local value preserved**, dirty + error retained |
| Clean (not edited) | Untouched | Refreshed value |

A unit test asserts this: edit two keys A and B, mock A succeeds and B fails; after refresh, A clean with new value, B still dirty with user-typed value and error message intact.

### 4.5 D11 — Code organization

**Layering rule (Codex r1 P1.4):** `internal/api` owns operations and the registry; **HTTP route registration lives in `internal/gui`** alongside `secrets.go`, `servers.go`, `migrate.go`, etc. Routes call into `*api.API` methods through the existing `Server` struct's narrow injected interface. Tests for HTTP wire format live next to the route file.

New files (A4-a):

```
internal/api/settings_registry.go            # SettingDef, registry definitions, validators
internal/api/settings_registry_test.go       # registry shape + per-key validator tests
internal/api/settings.go (extended)          # SettingsSet validates via registry, preserves unknown keys
internal/api/settings_test.go (new or ext.)  # round-trip + unknown-key preservation + concurrent-write
internal/gui/openpath.go                     # OpenPath helper (Codex r1 P1.6)
internal/gui/openpath_test.go                # spawnProcess seam test
internal/gui/settings.go                     # /api/settings GET/PUT/POST handlers + requireSameOrigin
internal/gui/settings_test.go                # routes + same-origin + Allow header + error wire format
internal/gui/backups.go                      # /api/backups + /api/backups/clean-preview
internal/gui/backups_test.go                 # routes + same-origin + preview correctness
internal/cli/settings_registry_test.go       # CLI behavior over registry (extends existing settings_test.go style)
internal/gui/frontend/src/screens/Settings.tsx                      # screen
internal/gui/frontend/src/components/settings/SectionAppearance.tsx
internal/gui/frontend/src/components/settings/SectionGuiServer.tsx
internal/gui/frontend/src/components/settings/SectionDaemons.tsx
internal/gui/frontend/src/components/settings/SectionBackups.tsx
internal/gui/frontend/src/components/settings/SectionAdvanced.tsx
internal/gui/frontend/src/components/settings/SectionNav.tsx        # sticky inner nav
internal/gui/frontend/src/components/settings/FieldRenderer.tsx     # registry-driven control picker
internal/gui/frontend/src/components/settings/BackupsList.tsx       # per-client groups, preview
internal/gui/frontend/src/lib/use-settings-snapshot.ts              # GET /api/settings hook
internal/gui/frontend/src/lib/settings-types.ts                     # SettingDTO, snapshot types
internal/gui/frontend/src/styles/settings.css                       # screen-specific styles (or appended to style.css)
internal/gui/e2e/tests/settings.spec.ts                             # E2E scenarios
```

Modified files:

```
internal/api/settings.go              # SettingsSet validates against registry; registry-resolved defaults in Get/List; preserves unknown keys
internal/cli/settings.go              # list/get/set consult registry
internal/gui/server.go                # registerSettingsRoutes(s) + registerBackupsRoutes(s) wired in routes init
internal/gui/frontend/src/app.tsx     # 7th sidebar link + case "settings"; settingsDirty in App-level guard
CLAUDE.md                             # E2E count update
docs/superpowers/plans/phase-3b-ii-backlog.md  # mark A4 row done at PR merge
```

`internal/gui/frontend/src/hooks/useRouter.ts` is **not** modified — query-string syntax `#/settings?section=backups` works with the existing parser.

Component decomposition rationale: each section is its own file because each has its own dirty-state, Save handler, and (in the case of Backups) extra subcomponents. `FieldRenderer` is the registry-driven control picker — given a `SettingDef` plus the current value and an `onChange` callback, it renders the appropriate control (`<select>` / `<input type="number">` / `<input type="text">` / etc.). Sections compose `FieldRenderer` for editable keys and inline custom JSX for sliders, action buttons, and `BackupsList`.

## 5. Settings registry

Full A4-a registry (§5.7 fields, with `Deferred: true` for A4-b items):

```go
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
```

`intPtr(n int) *int { return &n }` is a tiny helper used to keep the literal compact.

`Pattern` for `daemons.weekly_schedule` is intentionally permissive — A4-a only persists; A4-b's editor will tighten validation when it ships the actual scheduler integration.

**Section ordering** matches §5.7 reading order: Appearance, GUI server, Daemons, Backups, Advanced. The GUI nav and screen render in this order. CLI `list` defaults to this order grouped by section, with deferred keys marked `[deferred]`.

## 6. HTTP wire format

### 6.1 `GET /api/settings`

Response (200):

```json
{
  "actual_port": 9125,
  "settings": [
    {
      "key": "appearance.theme",
      "section": "appearance",
      "type": "enum",
      "default": "system",
      "value": "system",
      "enum": ["light", "dark", "system"],
      "deferred": false,
      "help": "Color theme. 'system' follows OS dark-mode."
    },
    {
      "key": "appearance.default_home",
      "section": "appearance",
      "type": "path",
      "default": "",
      "value": "",
      "optional": true,
      "deferred": false,
      "help": "Default home directory for new servers. Used by future launches."
    },
    {
      "key": "gui_server.port",
      "section": "gui_server",
      "type": "int",
      "default": "9125",
      "value": "9125",
      "min": 1024,
      "max": 65535,
      "deferred": false,
      "help": "GUI server port. Restart required to take effect."
    },
    {
      "key": "gui_server.tray",
      "section": "gui_server",
      "type": "bool",
      "default": "true",
      "value": "true",
      "deferred": true,
      "help": "Show tray icon (Windows). Edit coming in A4-b."
    },
    {
      "key": "advanced.open_app_data_folder",
      "section": "advanced",
      "type": "action",
      "deferred": false,
      "help": "Open mcp-local-hub data folder in OS file manager."
    }
    // ... all other registry entries
  ]
}
```

**Action entries omit `value` and `default`** (Codex r1 P2.2). The TS consumer uses a discriminated union:

```ts
type ConfigSettingDTO = {
  type: "enum" | "bool" | "int" | "string" | "path";
  default: string;
  value: string;
  // ... other shared fields
};
type ActionSettingDTO = {
  type: "action";
  // value/default absent
  // ... other shared fields
};
type SettingDTO = ConfigSettingDTO | ActionSettingDTO;
```

**Top-level `actual_port`** (Codex r1 P2.4) is the port the GUI server is currently bound to (read from `s.Port()` at request time). The GUI uses this to render a sticky `pending restart` badge whenever the persisted `gui_server.port` value differs from `actual_port` — the badge survives page reloads automatically without separate state.

`value` is the string-form value (registry default if no user value set); it has the same shape as `default`. Optional fields (`enum`, `min`, `max`, `pattern`, `optional`) are emitted only when set on the registry entry. The order of `settings[]` matches registry order (which matches §5.7 reading order).

### 6.2 `PUT /api/settings/<key>`

Request body: `{"value": "<string>"}`. Success: 200, response `{"saved": true, "key": "<key>", "value": "<string>"}`. Validation failure: 400, response `{"error": "validation_failed", "key": "<key>", "reason": "<message>"}`. Unknown key: 404, `{"error": "unknown_setting", "key": "<key>"}`. Action key (cannot PUT): 405 with `Allow: POST` header, body `{"error": "is_action", "key": "<key>"}`. Wrong method (e.g., DELETE): 405 with `Allow: PUT`.

**Concurrency safety (Codex r1 P1.5).** `*api.API` does not currently have a settings mutex. A4-a adds a package-level `var settingsMu sync.Mutex` in `internal/api/settings.go` (mirroring `vaultMutex` in `internal/api/secrets.go`). Every `SettingsSet` and `SettingsListIn`-followed-by-write path acquires the lock for the full read-validate-write sequence. The lock guards against torn writes from concurrent `PUT /api/settings/<a>` and `PUT /api/settings/<b>` racing on the same YAML file: without it, both calls might read `{x:1, y:2}`, modify their own key, and the slower writer's write would silently drop the faster writer's change. A regression test starts N goroutines each writing a distinct key, then asserts the final file contains all N values.

Idempotency: PUT is idempotent over identical bodies. A4-a does not add cross-key transactions — the per-section sequential PUT pattern from §4.4 is sufficient.

### 6.3 `POST /api/settings/<action_key>`

Action endpoints. A4-a only ships `advanced.open_app_data_folder`:

```
POST /api/settings/advanced.open_app_data_folder
→ 200 {"opened": "<path>"}    on success (path is filepath.Dir(SettingsPath()))
→ 500 {"error": "spawn_failed", "reason": "<message>"}    on shell-out error
→ 404 {"error": "unknown_setting", "key": "<key>"}        for unknown action
→ 405 {"error": "not_action", "key": "<key>"}             for non-action keys
```

Other action keys (`backups.clean_now`, `advanced.export_config_bundle`) are present in the registry as `Deferred: true` but have no POST handler in A4-a — calling `POST /api/settings/backups.clean_now` returns `404 deferred_action_not_implemented` (special-case 404 with explanatory body so callers and tests can distinguish from "unknown key").

## 7. CLI updates

`mcp settings list`: prints all registry keys grouped by section, in registry order. Format:

```
appearance:
  theme = system  (default: system)
  density = comfortable  (default: comfortable)
  shell = pwsh  (default: pwsh)
  default_home =   (default: <empty>)
gui_server:
  browser_on_launch = true  (default: true)
  port = 9125  (default: 9125)  [restart required]
  tray = true  (default: true)  [deferred — coming in A4-b]
daemons:
  weekly_schedule = weekly Sun 03:00  (default)  [deferred]
  retry_policy = exponential  (default)  [deferred]
backups:
  keep_n = 5  (default: 5)
  clean_now = <action>  [deferred]
advanced:
  open_app_data_folder = <action>
  export_config_bundle = <action>  [deferred]
```

Annotations: `[deferred]`, `[restart required]`, `<action>`, `<empty>`. Default-equal values can elide the `(default: …)` redundancy or keep it for clarity — pick "always show default" for predictability (chose this for tests being deterministic).

`mcp settings get <key>`:
- Unknown key → exit 1, stderr `error: unknown setting <key>` (CLI surface; the equivalent HTTP status is 404)
- Action key → exit 1, stderr `error: <key> is an action; use 'mcp settings invoke' (coming in A4-b)`
- Deferred non-action → prints value + `[deferred]` annotation on stderr
- Otherwise → prints raw value on stdout

`mcp settings set <key> <value>`:
- Unknown key → exit 1, stderr `error: unknown setting <key>`
- Action key → exit 1, stderr `error: cannot set action key <key>`
- Validation failure → exit 1, stderr `error: invalid value for <key>: <reason>`
- Deferred → still writes, but stderr warns: `setting accepted; this field is deferred to A4-b and has no effect yet`
- Otherwise → writes, exit 0, no stdout

The CLI does not block deferred-key writes because doing so would prevent users (and tests) from pre-populating the file ahead of A4-b. The warning is informational.

## 8. Frontend

### 8.1 Settings screen — `Settings.tsx`

```tsx
type Props = { route: RouterState; onDirtyChange: (b: boolean) => void };

export function SettingsScreen({ route, onDirtyChange }: Props) {  // Codex r1 P3.1
  const snapshot = useSettingsSnapshot();
  const [appearanceDirty, setAppearanceDirty] = useState(false);
  const [guiServerDirty, setGuiServerDirty] = useState(false);
  const [backupsDirty, setBackupsDirty] = useState(false);
  const anyDirty = appearanceDirty || guiServerDirty || backupsDirty;
  useEffect(() => onDirtyChange(anyDirty), [anyDirty]);

  // Scroll-spy for sticky nav active section.
  const activeSection = useScrollSpy(["appearance","gui_server","daemons","backups","advanced"]);

  // Deep-link on mount: parse `route.query` for `section=<name>` and scrollIntoView.
  // (Codex r1 P1.1: route.query is "section=backups" for `#/settings?section=backups`.)
  useEffect(() => {
    const params = new URLSearchParams(route.query ?? "");
    const target = params.get("section");
    if (target) {
      document.querySelector(`[data-section="${target}"]`)?.scrollIntoView({behavior: "smooth"});
    }
  }, [route.query]);

  return (
    <div class="settings-layout">
      <SectionNav active={activeSection} />
      <div class="settings-body">
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

Each editable section receives `snapshot` (typed `SettingsSnapshot`) and an `onDirtyChange` callback. Read-only sections (Daemons, Advanced) only consume `snapshot`. The screen aggregates dirty state and bubbles up to App-level via the existing `onDirtyChange` prop pattern (mirroring `AddServerScreen`).

### 8.2 `useSettingsSnapshot`

Mirrors `useSecretsSnapshot` (A3-a). Stale-while-revalidate semantics; status union of `loading | ok | error`. After Save success, the section calls `snapshot.refresh()` to reload the snapshot so next render uses persisted values.

Snapshot type (Codex r2 P1.2 — discriminated union for action vs config; top-level `actual_port`):

```ts
type Section = "appearance" | "gui_server" | "daemons" | "backups" | "advanced";

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
  // value/default deliberately absent — actions have no stored value (§6.1).
};

export type SettingDTO = ConfigSettingDTO | ActionSettingDTO;

export type SettingsEnvelope = {
  settings: SettingDTO[];
  actual_port: number;          // §6.1 — used by §9.2 port restart badge
};

export type SettingsSnapshot =
  | { status: "loading"; data: null; error: null }
  | { status: "ok"; data: SettingsEnvelope; error: null }
  | { status: "error"; data: null; error: APIError | Error };
```

### 8.3 `FieldRenderer`

Generic registry-driven control picker:

```tsx
type Props = {
  def: SettingDTO;
  value: string;
  onChange: (next: string) => void;
  disabled?: boolean;     // true for deferred; renders disabled control
  error?: string;         // inline per-field error message
};
```

Mapping:

- `type === "enum"` → `<select>` with `<option>` for each `enum[]` entry; helper text from `help` rendered as `<small>` below
- `type === "bool"` → `<input type="checkbox" checked={value === "true"}>`; toggling sends `"true"` / `"false"`
- `type === "int"` → `<input type="number" min={min} max={max}>`; on blur, validates string-parses; on invalid, surfaces `error` prop content
- `type === "string"` → `<input type="text">`; if `pattern` set, validates on blur
- `type === "path"` → `<input type="text">` (A4-a does not add a file-browse picker — that's a future polish)
- `type === "action"` → not handled by `FieldRenderer`; sections render action buttons inline directly

For deferred fields the control is rendered with `disabled` and a "(coming in A4-b)" inline label.

### 8.4 `SectionNav`

A sticky-positioned `<nav>` listing the 5 sections in registry order. Each entry: `<a href="#/settings?section=<section>" class={active ? "active" : ""}>`. Click triggers `scrollIntoView({behavior:"smooth"})` on the matching `<section data-section="...">`. Scroll-spy updates `active` class on `IntersectionObserver` entries.

### 8.5 Routing wiring

`useRouter` parses `#/<screen>?<query>`. For `#/settings?section=backups`, it produces `route.screen === "settings"` and `route.query === "section=backups"`. The Settings screen `useEffect` parses `route.query` via `URLSearchParams`, reads `section`, and scrolls into the matching section (see §8.1). No changes to `useRouter` are required; this is the same query-string mechanism `add-server` and `edit-server` already use for `?name=`. (Codex r1 P1.1.)

## 9. Section-by-section spec

### 9.1 Appearance

Header `<h2>Appearance</h2>`. Helper paragraph: "Visual appearance of the GUI." Four `FieldRenderer` rows in registry order (theme, density, shell, default_home). Save/Reset pair in a section footer.

- `theme` — applies via CSS variable on `<html data-theme>` after Save (D7 of master design §7). On `system`, the GUI listens to `prefers-color-scheme`. **Note:** for A4-a, Save persists; the CSS variable hookup may already exist in `style.css` from earlier design — if not, it is added as part of A4-a (test: changing theme and saving updates `<html data-theme>`).
- `density` — applies via CSS variable on `<html data-density>` after Save.
- `shell` and `default_home` — pure persistence in A4-a; consumers (existing or future) read the persisted value when launching shells / suggesting home dirs. A4-a does not add new consumers.

### 9.2 GUI server

Header `<h2>GUI server</h2>`. Helper paragraph: "How the GUI server runs."

- `browser_on_launch` — bool checkbox.
- `port` — int input with min=1024, max=65535. **Pending-restart badge derived from the persisted snapshot, not the local draft.** The badge `⚠ Restart required — port <persistedPort> will take effect after restart` renders whenever `snapshot.actual_port !== Number(persistedPort)`, where `persistedPort = snapshot.settings.find(s => s.key === "gui_server.port").value` (the saved/server value, NOT the local editable `value` of the dirty input). Because `actual_port` comes from the live server (the port mcphub is currently bound to), the badge automatically appears after Save (saved port differs from running port) and disappears once the user restarts mcphub on the new port (running port matches saved port). This survives page reloads with no extra state. **Important:** while the port field is dirty (user is editing but has not saved), the badge state stays anchored to the last persisted port — typing in the field never flips the badge. (Codex r1 P2.4 + r3 P2.1.)
- `tray` — rendered disabled (`Deferred: true`) with "(coming in A4-b)" inline label.

Save/Reset pair in section footer.

### 9.3 Daemons

Header `<h2>Daemons</h2>`. Helper paragraph: "Background daemon settings."

**Read-only display only.** A single block (Codex r1 P1.7 — labels distinguish *configured* from *currently active*):

```
Configured schedule: weekly Sun 03:00     (effective in A4-b)
Configured retry policy: exponential      (effective in A4-b)
                                              [edit coming in A4-b]
```

The block uses neutral styling (no Save button, no inputs). The "edit coming in A4-b" badge is right-aligned and uses the same style as deferred-field badges elsewhere.

Values shown are the persisted/default values from `snapshot` (same source as any other registry entry). The "(effective in A4-b)" suffix prevents misreading a CLI-written deferred value as the actual scheduler runtime state — A4-b will replace this read-only block with an editor *and* a separate "current scheduler state" line read from the running scheduler. If the snapshot is loading, show a small loader; if it errors, show "Schedule unavailable" inline (does not block other sections).

### 9.4 Backups

Header `<h2>Backups</h2>`. Helper paragraph: "Manage backup retention for managed client configs."

Layout:

```
Backups
Manage backup retention for managed client configs.

[ Keep timestamped backups per client: ████████░░░░░  5 ]
   Preview only. No files are deleted from this screen.

   Original backups are never cleaned. Retention is calculated separately for each client.

   ▼ claude-code (3 backups)
     2026-04-25 14:32   timestamped   1.2 KiB
     2026-04-20 09:15   timestamped   1.1 KiB   [Would be eligible for cleanup]
     2026-04-01 12:00   original      1.0 KiB

   ▼ codex-cli (2 backups)
     ...

   [ Clean now ]   ← disabled
   tooltip on hover: "Cleanup arrives in A4-b. This view only previews
                      which timestamped backups cleanup would target."

   [Save]   [Reset]
```

**Behavior:**

- Slider shows the current `keep_n` value (live, debounced 250ms while dragging).
- On slider drag-end (or with debounce), call `BackupsCleanPreview(currentKeepN)` and mark matching rows with the `Would be eligible for cleanup` badge + dimmed/striped row styling.
- **Original backups are never marked.** (Backend already ensures this; UI mirrors the contract.)
- **No red destructive styling.** Use neutral gray for the badge background and a subtle stripe pattern (`linear-gradient(45deg, ...)` at 5% opacity).
- Save persists `backups.keep_n` via `PUT /api/settings/backups.keep_n`.
- Reset reverts the slider to the last-saved value.
- Disabled `[Clean now]` button: tooltip on hover (text above, verbatim Codex copy).
- Preview API failure: keep base list visible, show inline `Preview unavailable` next to the slider. Slider still works (just no row marking).
- Per-client groups are collapsible with an expand-by-default state. Group header shows count: `▼ claude-code (3 backups)`.

**Locked Codex copy** (do not paraphrase):

| Element | String (verbatim) |
|---|---|
| Slider label | `Keep timestamped backups per client` |
| Helper text below slider | `Preview only. No files are deleted from this screen.` |
| Row badge | `Would be eligible for cleanup` |
| Disabled Clean tooltip | `Cleanup arrives in A4-b. This view only previews which timestamped backups cleanup would target.` |
| Group note (above per-client groups) | `Original backups are never cleaned. Retention is calculated separately for each client.` |
| Preview-failure inline | `Preview unavailable` |

### 9.5 Advanced

Header `<h2>Advanced</h2>`. Helper paragraph: "Power-user actions."

- **Open app-data folder.** A `[Open folder]` button. On click, calls `POST /api/settings/advanced.open_app_data_folder`. The route handler invokes `OpenPath(filepath.Dir(SettingsPath()))` — the dedicated file-manager helper from §2.4 (NOT `LaunchBrowser`, which is browser-specific). Success → 200 `{"opened": "<path>"}`, no UI feedback (the OS file manager opening is the feedback). Failure → inline error toast: `Could not open folder: <reason>`.
- **Export config bundle.** Disabled button labeled `[Export bundle]` with "(coming in A4-b)" inline label.

No Save button (no editable fields).

## 10. Risks

### 10.1 ~~Hash-anchor parsing~~ (resolved by Codex r1 P1.1)

Original concern: `#/settings#backups` second-`#` ambiguity. **Resolved** by switching to query-string syntax `#/settings?section=backups`, which the existing `useRouter` already parses. The Settings screen reads `route.query` and uses `URLSearchParams` to extract the target section. No router changes; mirrors the existing `#/edit-server?name=` pattern. (Risk now closed; left as a stub for traceability.)

### 10.2 Theme/density CSS variable wiring may not exist yet

The master design says theme switches via `<html data-theme>` and CSS variables. Whether the CSS variables and theme-color rules are already wired in `style.css` from earlier phases needs verification. If not, A4-a adds them (one-time scaffolding). Tests assert that setting `data-theme="dark"` changes a known CSS variable (e.g., `--bg-color`).

### 10.3 BackupsCleanPreview HTTP wrapping (locked: server-side)

A4-a adds two backups HTTP routes (Codex r1 P1.2 — the rev-1 client-side computation alternative is removed):

- `GET /api/backups` → returns `{backups: BackupInfo[]}`
- `GET /api/backups/clean-preview?keep_n=N` → returns `{would_remove: ["<path>", ...]}` from `BackupsCleanPreview(N)`

Both wrap the existing `*api.API` methods 1:1. Server-side preview ensures the "would-prune" set is identical to what `BackupsClean(N)` will eventually delete, so the preview never lies. Tests assert: matching `would_remove` between the API and the HTTP route, requestor-side rendering of badges from `would_remove` paths, preview-failure fallback (base list still visible).

### 10.4 Per-section dirty-state desync

Each section keeps a local snapshot copy + edit state + dirty flag. If the user opens Settings, edits Appearance (now dirty), then navigates away and confirms discard, the GUI must reset all section state. The dirty-guard discard path already handles this for AddServer (resets `addServerDirty` to false after confirm); A4 mirrors with `setSettingsDirty(false)` and a section-state reset trigger.

**Specifically:** when the App-level guard's `setAddServerDirty(false)` (or its A4 equivalent) fires, the Settings screen's section state should also reset to last-saved snapshot values. This is not automatic — sections track local state. The implementation plan must wire a "discard signal" (e.g., a `key` prop forced re-mount, or an explicit `discardKey` counter that sections watch via `useEffect`) so confirming "discard" actually discards.

### 10.5 Concurrent edits via CLI vs GUI

If a user runs `mcp settings set appearance.theme dark` in a terminal while the GUI Settings screen is open, the GUI snapshot becomes stale. A4-a does not implement live sync (no SSE for settings, no file watcher). The user sees stale state until they reload the page or trigger `snapshot.refresh()` (which happens after each section Save, so it self-heals after one Save cycle).

**Documented limitation:** the master design does not require live cross-channel sync for Settings; A4-b/c can add a refresh-on-focus or SSE update if needed.

### 10.6 Settings sidebar link in the dirty-guard chain

The existing `guardClick(targetScreen)` in `app.tsx` checks `route.screen !== "add-server" && route.screen !== "edit-server"` to skip the prompt when leaving non-dirty screens. A4-a generalizes this: the prompt fires whenever any tracked dirty flag is true (`addServerDirty || settingsDirty`).

**Implementation note:** the simplest path is a single `dirtyAny` boolean tracked in `App` state, fed from both `AddServerScreen` and `SettingsScreen` via their respective `onDirtyChange` props. `guardClick` uses `dirtyAny` instead of a hardcoded screen check.

## 11. Test surface

### 11.1 Go unit tests

`internal/api/settings_registry_test.go`:
- Every registry entry has a valid `Type`.
- Defaults parse cleanly per Type (e.g., `Default: "5"` for `TypeInt` parses as 5).
- Per-type validators accept the default (no entry has a default that its own validator would reject) — including `appearance.default_home` empty default with `Optional: true` (Codex r1 P1.3).
- Enum entries have non-empty `Enum`.
- Int entries with `Min`/`Max` have `Min ≤ Default ≤ Max`.
- No duplicate keys.
- All section names are in the canonical set.
- `TypeAction` entries have empty `Default` and the registry never tries to read `value` for them.

`internal/api/settings_test.go` (new tests on `SettingsSet/SettingsList`):
- **Unknown-key preservation (Codex r1 P2.1):** seed YAML with `{appearance.theme: dark, future_unknown.key: hello}`; call `SettingsSet("appearance.theme", "light")`; reload file; assert `future_unknown.key: hello` still present. Variant: also test that `SettingsList` does NOT return `future_unknown.key` in the schema-resolved snapshot but does preserve it on disk.
- **Concurrent-write safety (Codex r1 P1.5 + r3 P2.2)** — two test variants:
  - *Distinct keys, no lost updates:* the registry has 10 settable keys (4 appearance + 3 gui_server + 2 daemons + 1 backups; the 3 action keys are not PUT-able). Start exactly that many goroutines, each writing one distinct key concurrently; await all; assert the final YAML file parses cleanly AND contains every written value. Asserts `settingsMu` serializes the read-validate-write properly so no goroutine's write is silently dropped.
  - *Same-key contention, no torn writes:* start N=20 goroutines all writing the **same** key (`appearance.theme`) with one of the 3 valid enum values picked round-robin; await all; assert the file parses cleanly (no half-written YAML) and the persisted value is one of the 3 valid ones. Last-write-wins is acceptable; torn writes or non-parseable YAML is not.
- **Validation rejects bad value:** `SettingsSet("appearance.theme", "puce")` → returns validation error matching pattern `not in enum`; file unchanged.

`internal/gui/settings_test.go` (HTTP routes):
- `GET /api/settings` returns 200 with `{actual_port, settings[]}`. `actual_port` matches `s.Port()`.
- `GET /api/settings` includes action entries with no `value` / `default` keys in the JSON (Codex r1 P2.2 — wire shape).
- `PUT /api/settings/appearance.theme` with valid value → 200 + file persists.
- `PUT` with invalid enum → 400 with `{error:"validation_failed", key, reason}`.
- `PUT` with int out of range → 400.
- `PUT` to deferred non-action → 200 (write succeeds).
- `PUT` to action key (`advanced.open_app_data_folder`) → 405 with `Allow: POST` header.
- `PUT` to unknown key → 404.
- `POST /api/settings/advanced.open_app_data_folder` → 200; the `OpenPath` test seam captured the `(name, args)` call (assert `name="explorer.exe"` on Windows runner; the test verifies command construction, not actual folder opening).
- `POST` to deferred action (`backups.clean_now`) → 404 `{error:"deferred_action_not_implemented"}`.
- `POST` to unsupported method (e.g. `GET /api/settings/appearance.theme`) → 405 with `Allow: PUT` header.
- **Same-origin/CSRF (Codex r1 P2.5 + r2 P2.2):** **all** settings/backups routes (mutating PUT/POST and read-only GETs alike) reject cross-origin. The contract in §4.3 wraps every route with `requireSameOrigin`, so tests cover the full set:
  - `GET /api/settings` with `Sec-Fetch-Site: cross-site` → 403 `CROSS_ORIGIN`
  - `PUT /api/settings/<key>` with cross-origin → 403
  - `POST /api/settings/<action>` with cross-origin → 403
  - `GET /api/backups` with cross-origin → 403
  - `GET /api/backups/clean-preview?keep_n=N` with cross-origin → 403
  - All routes pass with `Origin: http://localhost:<port>` → 200
  - All routes pass with no Origin and no `Sec-Fetch-Site` (curl-style) → 200

`internal/gui/backups_test.go`:
- `GET /api/backups` returns 200 with `{backups: [...]}` for seeded fixture.
- `GET /api/backups/clean-preview?keep_n=3` returns 200 with `would_remove` matching `BackupsCleanPreview(3)`.
- `keep_n` missing → 400.
- `keep_n` invalid (negative, non-numeric) → 400.
- Same-origin checks per the precedent.

`internal/gui/openpath_test.go`:
- `OpenPath("/some/path")` calls `spawnProcess` with the OS-correct command and args (asserts via injectable seam).

`internal/cli/settings_registry_test.go`:
- `mcp settings list` includes deferred annotations and section grouping.
- `mcp settings get <unknown>` → exit 1 with `unknown setting`.
- `mcp settings set <deferred-non-action> <val>` → succeeds with stderr warning.
- `mcp settings set <action> <val>` → exit 1 with `cannot set action key`.

### 11.2 Vitest unit tests

`FieldRenderer.test.tsx`:
- Renders `<select>` for enum, `<input type="checkbox">` for bool, `<input type="number">` for int, `<input type="text">` for string/path.
- Bool checkbox checked iff value === "true"; toggling fires onChange("true"|"false").
- Int control respects min/max attributes from def.
- Disabled state propagates to control.
- Inline error rendered when error prop is set.

`SectionAppearance.test.tsx`:
- Renders 4 fields in registry order.
- Editing theme dirties the section; Reset reverts; Save calls the right PUTs.
- Save with one failed key reports per-key error and keeps that key dirty.

`SectionGuiServer.test.tsx`:
- Editing port + Save → triggers PUT then renders `pending restart` badge (asserting comparison between `actual_port` and persisted snapshot value).
- **Dirty draft does not flip the badge (Codex r4 P2.1):** mock snapshot with `actual_port: 9125, gui_server.port: 9125`; type a different value (e.g. `9200`) into the field but do NOT save; assert the badge is **not** rendered. Then save; assert the badge appears.
- `tray` field rendered disabled with "(coming in A4-b)".

`SectionDaemons.test.tsx`:
- Renders read-only schedule/retry display from snapshot.
- Renders "edit coming in A4-b" badge.
- No Save button rendered.

`SectionBackups.test.tsx`:
- Slider drag updates value (debounced).
- Preview API call triggered after debounce; matching rows get `Would be eligible for cleanup` badge.
- Original rows never get the badge.
- Disabled Clean button has correct tooltip text (verbatim Codex copy).
- Slider Save triggers PUT for `backups.keep_n`.
- All 6 verbatim copy strings match §9.4 table.
- Preview API failure → base list still visible + `Preview unavailable` inline.

`SectionAdvanced.test.tsx`:
- Open folder button click triggers POST.
- Open folder failure → error toast.
- Export bundle button rendered disabled with "(coming in A4-b)".

`SectionNav.test.tsx`:
- Active class on the section matching scroll-spy.
- Click triggers smooth scroll (mock).

### 11.3 Playwright E2E (`internal/gui/e2e/tests/settings.spec.ts`)

Target: ~12-15 scenarios, bringing CLAUDE.md count from 76 → ~88-90.

1. **Sidebar Settings link** — navigate from Servers → Settings via sidebar; URL hash becomes `#/settings`; Settings header rendered.
2. **All 5 section headers visible** — Appearance, GUI server, Daemons, Backups, Advanced rendered as `<h2>`.
3. **Deep-link query-string** — navigate to `#/settings?section=backups` directly; Backups section scrolled into view.
4. **Sticky inner nav active state** — scroll past Appearance into GUI server; nav shows GUI server as active.
5. **Save Appearance round-trip** — toggle theme to "dark"; Save; reload page; theme persists; gui-preferences.yaml on disk has `appearance.theme: dark`.
6. **Save validation failure** — set port to 99 (below min); Save; section banner shows error; field highlighted; key remains dirty.
7. **Port save-pending-restart badge** — change port; Save; `pending restart` badge appears next to field.
8. **Daemons read-only** — section renders "Configured schedule: ..." text with "(effective in A4-b)" suffix; no input controls; "edit coming in A4-b" badge present. (Codex r2 P2.1 — wording must match §9.3, not the rev-1 "Current schedule" misnomer.)
9. **Backups list — 4 client groups** — seed 4 client configs with mock backups; list shows 4 collapsible groups with counts.
10. **Backups preview marks would-prune rows** — seed 7 timestamped backups for one client; set keep_n=3; rows 4-7 (oldest) marked with `Would be eligible for cleanup` badge; `original` rows never marked.
11. **Backups disabled Clean tooltip** — hover Clean now button; tooltip text is verbatim Codex copy.
12. **Open app-data folder action** — click button; mock POST returns success; no UI error; backend invocation recorded (test asserts the POST URL was hit, not actual folder opening).
13. **Dirty-guard on navigation** — edit Appearance (dirty); click Servers in sidebar; "Discard unsaved changes?" prompt appears; cancel keeps user on Settings; confirm resets dirty state and navigates.
14. **Per-section Save isolation** — edit both Appearance and GUI server; click Save in Appearance only; Appearance's keys persist; GUI server stays dirty.
15. **Deferred field disabled** — `gui_server.tray` checkbox is disabled; "(coming in A4-b)" label visible.

### 11.4 Codex review iteration

The memo and plan go through Codex review cycles before commit (precedent A3-a/A3-b). Any findings flagged P1/P2 are closed before plan write begins. Plan goes through its own Codex review iteration before subagent-driven execution starts.

## 12. Acceptance criteria

A4-a ships when:

1. ✅ `internal/api/settings_registry.go` defines `SettingDef` (with `Optional bool`), `SettingType` constants, and `Registry []SettingDef` covering all §5.7 fields including deferred ones.
2. ✅ `internal/api/settings.go` validates via registry on `SettingsSet`; preserves unknown keys on rewrite; `settingsMu` guards concurrent writes; `SettingsList`/`SettingsGet` resolve registry defaults for keys with no user value.
3. ✅ `internal/gui/settings.go` exposes `GET /api/settings`, `PUT /api/settings/<key>`, `POST /api/settings/<action>` wrapped with `requireSameOrigin`, with the wire format in §6 (including top-level `actual_port`).
4. ✅ `internal/gui/backups.go` exposes `GET /api/backups` and `GET /api/backups/clean-preview?keep_n=N`.
5. ✅ `internal/gui/openpath.go` defines `OpenPath` reusing `spawnProcess` seam (NOT `LaunchBrowser`).
6. ✅ `internal/cli/settings.go` consults registry for `list` (annotated output), `get` (exit-1 unknown), `set` (validation + deferred warning + action rejection).
7. ✅ Settings sidebar link in `app.tsx`; `#/settings` route works; active-link class on the link.
8. ✅ `Settings.tsx` renders single-page layout with sticky `SectionNav`; deep-link `#/settings?section=<name>` scrolls to section via `URLSearchParams`.
9. ✅ Per-section Save/Reset (3 sections: Appearance, GUI server, Backups). Daemons read-only. Advanced action button.
10. ✅ Per-section Save merge rule respects partial failures (failed keys stay dirty + error after refresh) per §4.4.
11. ✅ Daemons read-only display rendered with "Configured … (effective in A4-b)" labels.
12. ✅ Port pending-restart badge rendered iff `snapshot.actual_port !== Number(persistedPort)`, where `persistedPort = snapshot.settings.find(s => s.key === "gui_server.port").value` (the saved/server value from the snapshot, NOT the local editable draft). Badge state stays anchored to the persisted port while the field is dirty — typing in the input never flips the badge. Survives page reload. (Codex r3 P2.1 + r4 P2.1.)
13. ✅ `BackupsList` per-client groups; passive prune preview via `GET /api/backups/clean-preview`; all 6 Codex-locked copy strings present verbatim.
14. ✅ "Open app-data folder" action functional (POST → backend invokes `OpenPath`).
15. ✅ App-level `useUnsavedChangesGuard` extended to cover `settingsDirty`; sidebar nav prompts on dirty state.
16. ✅ `go generate ./internal/gui/...` regenerated bundle committed (production-grade precedent A3-a/A3-b).
17. ✅ All Go unit tests, Vitest tests, and Playwright E2E in §11 pass on Windows runner.
18. ✅ CLAUDE.md updated with A4 surface description + new E2E count.
19. ✅ `docs/superpowers/plans/phase-3b-ii-backlog.md` row 9 marked `✅ A4 — Settings screen` with link to this memo and the merge SHA.

A4-b is **explicitly admitted scope** (not "later"): tray, port live rebind, weekly schedule edit, retry policy, Clean now confirm flow, export config bundle. A4-b's memo and plan are written separately after A4-a merges.

## 13. Commit decomposition (preview)

The plan will refine this; rough shape:

1. Registry + validators + Go-side tests (no HTTP, no GUI).
2. HTTP routes + tests.
3. CLI updates + tests.
4. `useSettingsSnapshot` hook + types.
5. `FieldRenderer` + tests.
6. `Settings.tsx` shell + `SectionNav` + hash routing + sidebar link in `app.tsx` + dirty-guard wiring.
7. `SectionAppearance` + `SectionGuiServer` + tests.
8. `SectionDaemons` + `SectionAdvanced` + tests.
9. `SectionBackups` + `BackupsList` + preview wiring + tests.
10. CSS + theme/density wiring (verify existing scaffolding first).
11. E2E suite + asset regen + CLAUDE.md + backlog mark.

Granularity is the planner's call; this list captures the natural seams.

## 14. Open questions for plan / Codex review

- **§10.2:** confirm theme/density CSS variables already exist in `style.css`. If yes, A4-a wires them up; if no, A4-a adds the scaffolding.
- **§9.1:** confirm whether `appearance.shell` and `appearance.default_home` have downstream consumers in this repo today, or if they are purely forward-compatibility scaffolding for a future feature. (Codex Q1 said "used by future launches" copy is appropriate; this confirms the pattern.)

These are answerable by reading code in the implementation plan's Task 0; none gate the design.

**Resolved by Codex r1:**

- ~~Hash-anchor parsing~~ → query-string syntax (§10.1, §8.5)
- ~~Backups preview client-side option~~ → server-side only via `GET /api/backups/clean-preview` (§10.3)
- ~~HTTP route location~~ → `internal/gui/settings.go`, not `internal/api` (§4.5)
- ~~Existing mutex assumption~~ → explicit `settingsMu` added (§6.2)
- ~~LaunchBrowser reuse~~ → dedicated `OpenPath` helper (§2.4)
- ~~Daemons "Current" mislabel~~ → "Configured (effective in A4-b)" (§9.3)
- ~~TypePath empty default~~ → `Optional bool` field (§4.1)

---

**End of memo. Rev 2 ready for Codex re-review.**

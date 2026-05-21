# Servers matrix revamp: LSP-bridge integration + per-daemon env overlay

**Status:** Spec, draft v4 (third post-review revision), pending user review.

**Tracking PR(s):** to be filled by writing-plans phase. Likely 1 PR with 4 phases (auto-discovery, overlay+merge, LSP recognition, GUI surface), or split if any phase grows too large.

**Closes / unblocks:**

- Operator UX gap: 9 LSP language entries (clangd, fortran, go, javascript, python, rust, typescript, vscode-css, vscode-html) currently show under "Other MCP entries (N)" because the matrix does not recognize them as language rows of the `mcp-language-server` workspace-scoped manifest.
- Operator UX gap: `mcphub` daemons spawned by the supervisor task inherit a Task-Scheduler-logon PATH that lacks common Windows tool install locations (`C:\msys64\ucrt64\bin`, `C:\Program Files\LLVM\bin`, etc.). `gdb` MCP daemon answers but reports "GDB/LLDB not available"; `mcp-language-server` would have the same problem for `clangd`, `gopls`, `pyright`, `rust-analyzer`, etc.

## v4 revision notes (changes from v3)

v3 went through a third dual review (codex xhigh + sonnet architect). Verdict: NEEDS_REVISION (both). The recurring failure mode v3 STILL exhibited: claiming Windows API behavior + existing GUI auth posture without grep-verifying. v4's stricter rule: **every concrete Windows API, env var, function name, auth header, and existing-code "same posture as" claim is grep-verified against the actual codebase before inclusion.**

**3 BLOCKERS resolved:**

- **B-V4-1 — Windows reparse-point polarity inverted.** v3 said `FILE_FLAG_OPEN_REPARSE_POINT` *cleared* refuses symlinks. That is the OPPOSITE of Win32 semantics — cleared = follow the link. The correct posture (already used elsewhere in this repo) is `FILE_FLAG_OPEN_REPARSE_POINT | FILE_FLAG_BACKUP_SEMANTICS` **SET** at `CreateFileW`, then refuse if the opened handle's `BY_HANDLE_FILE_INFORMATION.dwFileAttributes & FILE_ATTRIBUTE_REPARSE_POINT != 0`. The pattern is at [internal/api/hub_mcp_state_dacl_windows.go:85-99](../../../internal/api/hub_mcp_state_dacl_windows.go) for the parent-dir open, and at line 192 of the same file for the reparse-attribute refusal. v4 cites this pattern verbatim instead of re-inventing.
- **B-V4-2 — CSRF posture fabricated.** v3 claimed `X-Mcphub-CSRF` header with per-session tokens — grep-verified ZERO hits in the repo. The actual posture is `requireSameOrigin` middleware ([internal/gui/csrf.go:81-99](../../../internal/gui/csrf.go)) — origin allow-listing + `Sec-Fetch-Site` check; empty `Origin` passes (so curl/native callers work), and the outer `requireAllowedHost` at line 17 enforces DNS-rebinding defense via Host header. v4 describes the actual posture and drops the fabricated token language.
- **B-V4-3 — `ResolveEntryName` suffix-as-key incorrect.** v3 claimed the algorithm "uses the suffix as a registry key to disambiguate workspace ownership". Wrong: [register.go:730-746](../../../internal/api/register.go) shows the suffix is `workspaceKey[:4]` (first 4 hex chars of the FULL workspace key), with fallback to the full `workspaceKey` on prefix collision. The suffix is NOT a registry key; reverse disambiguation requires walking the registry and matching `WorkspaceEntry.ClientEntries[client] == entryName` ([workspace_registry.go:38](../../../internal/api/workspace_registry.go)). v4 rewrites the recognition algorithm to use this reverse-lookup against the registry.

**6 IMPORTANT resolved:**

- **I-V4-1 — LSP task-name canonicalization claim corrected.** v3 said the scheduler API canonicalizes before storage. register.go writes bare form throughout (lines 292, 375, 401, 445). The canonicalization happens at the `WorkspaceEntry.TaskName` → `SupervisorDaemon.TaskName` transcription step (install/migration code path, not register.go). v4 documents this transition explicitly and drops the over-claim.
- **I-V4-2 — `ScanEntry` schema change for dual-badge cells.** v3 said cells render two badges on hub+legacy coexistence, but `ScanEntry.ClientPresence` is `map[string]ClientEntry` ([types.go:102](../../../internal/api/types.go)) — one `ClientEntry` per client. The schema cannot hold two transports for one (client, server) cell as-is. v4 specifies: add `ScanEntry.LegacyConflict map[string]ClientEntry` (omitempty side-channel) holding the stdio entry when a hub entry occupies `ClientPresence` for the same (client, server). Backwards compatible; rendering layer reads both fields.
- **I-V4-3 — `LookupOverlay` normalization symmetry.** v3 spec specified normalization at spawn-time only. v4 requires that ALL call sites (spawn lookup, GUI `/api/daemon/env` write, auto-discovery `WriteOverlay` mutator, `mcphub unregister` orphan detection, hand-edit detection on load) route through the same `normalizeOverlayKey(taskName string) string` helper that prepends `\` if absent. Storage is always canonical form regardless of how the writer presented the key.
- **I-V4-4 — Read-side env var named explicitly.** v3 said "operatorAllowsUnhardenedStateRead (new env var)" without naming. v4 names it `MCPHUB_ALLOW_UNHARDENED_STATE_READ` symmetric with existing `MCPHUB_ALLOW_UNHARDENED_STATE_WRITE` ([client_write_init.go:105](../../../internal/api/client_write_init.go)). Also clarifies that `MCPHUB_REQUIRE_SINGLE_USER_HOME=0` is just "unset/false" (strict gate disabled, normal default-relax flow), NOT a separate opt-out — v3 wording confusingly suggested it was a third state.
- **I-V4-5 — `mcphub config overlay-quarantine` is an offline CLI.** v3 implied this but didn't say it. v4: the command runs without supervisor IPC; rename + flock-release happen even when the supervisor is refusing to spawn. This is the documented recovery path for the "fail-LOUD blocks all daemons" state.
- **I-V4-6 — `workspaces.yaml` missing/corrupt during recognition surfaced as an open question.** v3 left this hidden. v4 adds an explicit open question and a proposed degraded-mode behavior (recognition falls through to single-workspace branch when registry is empty; ResolveEntryName suffix is preserved as label but not used for ownership lookup).

**3 MINOR resolved:**

- **M-V4-1 — `DefaultHints` Python paths use glob, not version-locked literals.** v3 hardcoded `Python311` through `Python314`. v4 uses `%LOCALAPPDATA%\Programs\Python\Python3*` and walks for each match.
- **M-V4-2 — `.corrupt-<ts>` retention.** Adopt the 5-newest-retained policy from the v0.4.x watchdog quarantine pattern (CLAUDE.md "All under `<state-dir>`" section). Quarantine command retains 5 newest after rename; oldest pruned.
- **M-V4-3 — Code citation drift fixed.** `mergeDaemonEnv` gate is lines 1504-1506 (not 1504-1505); `ClientEntries` map definition is at [workspace_registry.go:38](../../../internal/api/workspace_registry.go) (not register.go:539-563). v4 cites verified line numbers throughout.

**Audit posture (new for v4):** I read each file/symbol cited in v4 before commit. Verification artifacts: `internal/gui/csrf.go`, `internal/api/workspace_registry.go:30-46`, `internal/api/types.go:90-114`, `internal/api/client_write_init.go:98-105`, `internal/api/hub_mcp_state_dacl_windows.go:85-220`, `internal/cli/supervise.go:1450-1520`. Reading these BEFORE writing claims is the only way to avoid the v1/v2/v3 fabricated-claim pattern; v4 makes this an explicit gate.

**Carried forward from v2 + v3 revision notes** (still valid): every B-V3-1 through I-V3-9 fix from v3, every B/I fix from v2, every B/I fix from v1. The cumulative spec quality reflects three rounds of dual review.

## Goal

Make the Servers matrix the single place where an operator sees and manages every MCP server mcphub knows about, including the 9 LSP-bridge languages. Make each daemon's effective env (especially PATH) visible and editable from the GUI, with auto-discovery filling in sensible defaults at install time.

## Scope

In scope:

- Matrix recognition of LSP language entries (currently classified as "Other MCP entries") as language rows under the existing `mcp-language-server` manifest. Multi-workspace registrations supported via reverse-lookup against the registry (NOT via suffix-as-key — see B-V4-3 above).
- Exactly 9 LSP language rows in the Servers matrix (one per manifest language). When hub-managed and direct-stdio entries coexist on the same (client, language) cell, the cell holds the hub entry in `ClientPresence` plus the stdio entry in a new `LegacyConflict` side-channel field (see Schema Change section).
- Single `Active workspace` selector at the top of the Servers screen, scoping the check/uncheck and Register actions.
- Per-daemon `env` overlay file separate from shipped manifests. Supervisor merges manifest `env` with overlay `env` at spawn time (overlay wins on collisions, Windows case-insensitive key normalize).
- Auto-discovery of common binary install locations at `mcphub install/setup` time. Populates initial overlay file. Manual "Refresh discovery" button in the GUI re-runs at any time.
- Per-row drawer in the GUI showing effective env (post-merge) with edit affordance and `Apply` button. Apply persists to overlay; operator clicks "Restart daemon to apply" to trigger respawn.
- New read-side `daemon_env_overlay` package owning hardened YAML round-trip with comments preserved, flock-protected RMW, and the new internal `checkStateDirParentReadSafe` helper symmetric to the existing write-side.
- New `respawn` IPC command on the supervisor (replaces the v0.5.0 `restart`/`reload` UNKNOWN_COMMAND stub at [supervise.go:921](../../../internal/cli/supervise.go)). Accepts `{taskName, force: bool}`; quarantined daemons require `force: true`.
- New `ScanEntry.LegacyConflict` side-channel field for coexistence anomalies (additive schema change; existing consumers unaffected).
- New env var `MCPHUB_ALLOW_UNHARDENED_STATE_READ` for explicit operator opt-in on read-side relax lane (mirrors existing `MCPHUB_ALLOW_UNHARDENED_STATE_WRITE`).

Out of scope (deferred):

- Backend rewrite of `mcp-language-server` to serve multiple workspaces from one daemon.
- Cross-workspace router that picks a proxy by caller cwd. The active-workspace selector is operator-driven, not auto-routed.
- Linux/macOS systemd/launchd PATH inheritance fixes — out of scope for this design.

## Architecture summary

Four pieces, mostly independent, glued through existing seams:

1. **Manifest schema additions (with Go struct extension).** Server manifest YAML gains optional `required_binaries: [name, ...]` at server level AND per-language level. Same commit adds the field to `ServerManifest` and `LanguageSpec` Go structs in `internal/config/manifest.go` ([line 128 KnownFields(true)](../../../internal/config/manifest.go) is strict; the new field must be in the struct). The field is free-form metadata only; `Validate()` does NOT enforce binary existence.
2. **Auto-discovery engine.** New `internal/api/binary_discovery/` package: `Discover(ctx, requiredBinaries, hints) (map[binary]absolutePath, error)`. `hints` is a `[]string` parameter for test injection. Production callers use `binary_discovery.DefaultHints()` (per-OS lists; Windows uses `%LOCALAPPDATA%\Programs\Python\Python3*` glob to survive Python version churn).
3. **Env overlay file (new package).** New `internal/api/daemon_env_overlay/` package owns: hardened YAML load + parse + flock-protected RMW writer + the new `checkStateDirParentReadSafe` helper + `WriteOverlay(path, mutator)` transactional API + `normalizeOverlayKey(taskName)` helper used at every call site. Writes route through the existing exported `SecureWriteClientConfig(path string, payload []byte) error` ([state_file_helper.go:127](../../../internal/api/state_file_helper.go)) with YAML-marshaled bytes.
4. **Matrix LSP recognition + workspace scope.** Three changes in `internal/api/scan.go` + frontend:
   - Three-rule recognition algorithm parsing `ClientEntry.Raw map[string]any` directly, with registry reverse-lookup for multi-workspace ownership (see "Matrix LSP recognition" below).
   - Top-of-screen `Active workspace` selector. Default = most-recent registered workspace from `workspaces.yaml`, OR a literal "(none — register a workspace first)" placeholder when empty.
   - Per-cell semantics: hub entry in `ClientPresence`, optional stdio entry in `LegacyConflict` side-channel; the matrix renderer reads both fields and emits 1 or 2 badges accordingly.

## Schema change: `ScanEntry.LegacyConflict`

Current schema at [types.go:99-106](../../../internal/api/types.go):

```go
type ScanEntry struct {
    Name           string                 `json:"name"`
    Status         string                 `json:"status"`
    ClientPresence map[string]ClientEntry `json:"client_presence"`
    ManifestExists bool                   `json:"manifest_exists"`
    CanMigrate     bool                   `json:"can_migrate"`
    ProcessCount   int                    `json:"process_count,omitempty"`
}
```

v4 adds one field:

```go
type ScanEntry struct {
    Name              string                 `json:"name"`
    Status            string                 `json:"status"`
    ClientPresence    map[string]ClientEntry `json:"client_presence"`
    LegacyConflict    map[string]ClientEntry `json:"legacy_conflict,omitempty"` // NEW: stdio entry when hub entry coexists for same (client, server)
    ManifestExists    bool                   `json:"manifest_exists"`
    CanMigrate        bool                   `json:"can_migrate"`
    ProcessCount      int                    `json:"process_count,omitempty"`
}
```

`LegacyConflict` is populated only when scan recognition produces BOTH a hub-managed entry AND a direct-stdio entry for the same (client, server) tuple. `ClientPresence[client]` always holds the canonical (hub-preferred) entry; `LegacyConflict[client]` holds the secondary. Existing consumers of `ClientPresence` are unaffected (the field is `omitempty` and absent in the common case).

Renderer logic: emit `[via-hub]` badge when `ClientPresence[client].Transport == "http"` AND endpoint is hub URL; emit `[legacy]` badge when `LegacyConflict[client]` is present OR when `ClientPresence[client].Transport == "stdio"`. Both badges visible iff both conditions hold simultaneously.

## Canonical daemon key namespace

Overlay file keys are `SupervisorDaemon.TaskName` strings as they appear in `supervisor-intent.json`, **WITH** the leading backslash (per [supervisor_intent.go:25 comment](../../../internal/api/supervisor_intent.go): `// canonical, e.g. "\\mcp-local-hub-memory-default"`). Reconcile indexes by this canonical form ([supervise_reconcile.go:107](../../../internal/cli/supervise_reconcile.go)).

`register.go` writes the BARE form (no leading `\`) into `WorkspaceEntry.TaskName` ([workspace_registry.go:37](../../../internal/api/workspace_registry.go)) for the registry record. The canonical leading-backslash form is materialized later at the install/migration code path that transcribes from `WorkspaceEntry` to `SupervisorDaemon` for `supervisor-intent.json`. v4 does NOT claim register.go itself canonicalizes (v3 over-claim corrected).

Three concrete task-name shapes apply:

| Daemon kind | TaskName in supervisor-intent.json | Source registry field |
|---|---|---|
| Global | `\mcp-local-hub-<server>-<daemon>` | derived at install from manifest; default daemon name is `default` |
| LSP workspace-scoped | `\mcp-local-hub-lsp-<wsKey>-<lang>` | `WorkspaceEntry.TaskName` (BARE form `mcp-local-hub-lsp-<wsKey>-<lang>` per [register.go:292](../../../internal/api/register.go)) gets `\` prefixed at install-time transcription |
| Hub-managed entry name (matrix recognition target) | `mcp-language-server-<lang>` plus optional collision suffix | `WorkspaceEntry.ClientEntries[client]` — see [workspace_registry.go:38](../../../internal/api/workspace_registry.go); resolved by [register.go:722-747 ResolveEntryName](../../../internal/api/register.go); suffix is `workspaceKey[:4]` or full `workspaceKey` on prefix collision |

The new `normalizeOverlayKey(taskName string) string` helper prepends `\` if absent. v4 requires every overlay call site (spawn lookup, GUI write, auto-discovery mutator, orphan detection, hand-edit on load) to route through this helper. Storage on disk is always canonical-prefixed; reads tolerate either form for operator-edited YAML.

## Matrix LSP recognition

### Three-rule recognition algorithm (parses `ClientEntry.Raw`)

```text
Inputs:
  ENTRIES   = scanned client-config entries keyed by entry-name
              (each value: ClientEntry{Transport, Endpoint, Raw})
  LANGS     = the 9 manifest language names {clangd, fortran, go, javascript,
              python, rust, typescript, vscode-css, vscode-html}
  REGISTRY  = workspaces.yaml — for each workspace, ClientEntries map of
              client → entry-name (per workspace_registry.go:38)

For each (entryName, entryByClient) in ENTRIES:
  For each (clientName, entry) in entryByClient:

    // STEP A — language extraction by entry-name (NOT by --lsp arg).
    // Strip ResolveEntryName collision suffix if present:
    //   mcp-language-server-<lang>            → base form
    //   mcp-language-server-<lang>-<4hex>     → short-suffix form
    //   mcp-language-server-<lang>-<8hex>     → full-suffix form
    // where <lang> is one of LANGS and <4hex>/<8hex> is workspaceKey
    // prefix/full. Helper: parseEntryName(entryName, LANGS) returns
    // (baseLang, suffixMaybe).
    baseLang, suffix = parseEntryName(entryName, LANGS)
    if baseLang == "" { continue }  // not an LSP entry

    // STEP B — ownership disambiguation by REVERSE-LOOKUP against REGISTRY.
    // The suffix is NOT a key — it's workspaceKey[:4] or the full key.
    // Correct disambiguation: walk REGISTRY and find the workspace whose
    // ClientEntries[clientName] == entryName.
    owningWorkspace = nil
    for ws in REGISTRY.Workspaces:
      if ws.ClientEntries[clientName] == entryName:
        owningWorkspace = ws
        break

    // STEP C — apply one of three rules:

    // Rule 1 — Hub-managed lazy proxy:
    if entry.Transport == "http" AND owningWorkspace != nil AND
       entry.Endpoint matches lazy-proxy URL pattern:
      categorize as LSP entry, badge="via-hub", owner=owningWorkspace
      → Output: place in ScanEntry.ClientPresence[client]

    // Rule 2 — Direct-stdio mcp-language-server invocation:
    elif entry.Transport == "stdio" AND
         filepath.Base(entry.Endpoint) == "mcp-language-server" (or .exe):
      // The --lsp arg cannot distinguish JS from TS (both use
      // typescript-language-server per manifest.yaml:42, 59). Language
      // is determined from entryName (Step A), not from args. The --lsp
      // arg is checked only as defense-in-depth: it must match the
      // baseLang's manifest.lsp_command (consistent-with-name gate).
      categorize as LSP entry, badge="legacy", owner=owningWorkspace OR (registry-empty fallback)
      → Output: place in ScanEntry.LegacyConflict[client] if hub entry
        already present in ClientPresence[client]; else ClientPresence[client]

    // Rule 3 — Direct-stdio gopls invocation (Go special case):
    elif entry.Transport == "stdio" AND
         filepath.Base(entry.Endpoint) == "gopls" AND
         args (from entry.Raw["args"]) contains "mcp" (per manifest.yaml:33-37
         where Go has backend: gopls-mcp, lsp_command: gopls, extra_flags: [mcp]):
      categorize as Go LSP entry, badge="legacy"
      → Same placement logic as Rule 2

After categorization:
  - Always exactly 9 LSP language rows (one per manifest language).
  - For each (client, language) cell, the cell carries 0, 1, or 2 badges
    based on ClientPresence[client] + LegacyConflict[client] (both may be
    populated for coexistence anomaly).
```

**JS/TS disambiguation grounding**: both languages use `typescript-language-server` as `lsp_command` ([manifest.yaml:42, 59](../../../servers/mcp-language-server/manifest.yaml)). Language is determined by the entry NAME extracted in Step A, NOT by parsing the `--lsp` arg.

**Multi-workspace correctness grounding**: [register.go:722-747 ResolveEntryName](../../../internal/api/register.go) appends `workspaceKey[:4]` (first 4 hex of the 8-char workspace key) on the first multi-workspace collision per language; falls back to the FULL workspaceKey when the 4-char prefix also collides. The suffix is NOT itself a registry key — disambiguation walks `REGISTRY.Workspaces` and matches `ws.ClientEntries[clientName] == entryName` exactly. v4 corrects v3's wrong "suffix-as-key" claim.

**Registry-empty fallback**: if `workspaces.yaml` is missing or empty, Step B's `owningWorkspace` is `nil` for every entry. The algorithm still labels the entry's language correctly (Step A); ownership lookup is just absent. Status renders as "via-hub (workspace unknown)" or "legacy" without per-workspace attribution. No crash.

## Data flow

### Spawn-time env merge (modified)

```text
1. Supervisor reads supervisor-intent.json     → SupervisorDaemon{Env: M, TaskName: T}
2. Supervisor reads daemon-env-overrides.yaml  → overlay map (hardened read)
3. key = normalizeOverlayKey(T)
4. O = overlay[key].env
5. mergeDaemonEnv(parent=os.Environ(), manifestEnv=M, overlayEnv=O)
     → expand ${parent_path} in O using parent's PATH
     → merge with Windows case-insensitive normalize
     → precedence: parent < manifest < overlay
6. cmd.Env = merged
7. Daemon spawns with the merged env
```

`mergeDaemonEnv` at [supervise.go:1456](../../../internal/cli/supervise.go) is extended to take a third `overlayEnv map[string]string` parameter. The gate at lines 1504-1506 (`if len(d.Env) > 0 { cmd.Env = mergeDaemonEnv(os.Environ(), d.Env) }`) is REMOVED — the merge fires whenever EITHER `d.Env` OR overlay has any keys. When both empty, cmd.Env stays nil so child inherits `os.Environ()` directly.

### Auto-discovery at install (new)

```text
mcphub install / mcphub setup
  ↓
For each server manifest with required_binaries:
  For each binary in required_binaries:
    Walk DefaultHints() in OS-specific order until a hit
      (Windows Python: glob %LOCALAPPDATA%\Programs\Python\Python3*)
  Compose Path = first hit's parent dir per binary, joined with ${parent_path}
  WriteOverlay(mutator) — mutator routes key through normalizeOverlayKey
  Inside mutator:
    For each daemon row to write:
      If existing row has source: operator → skip (preserve operator override)
      Else → write source: auto-discovery, discovered_at: <RFC3339Nano>
```

Compare-and-swap on `source: operator` happens under the same flock as the WriteOverlay, so a concurrent GUI Apply cannot lose its operator-tagged row.

### Manual discovery refresh from GUI (new)

```text
GUI Settings → Daemons / per-row drawer → "Refresh discovery"
  ↓
POST /api/discovery/refresh {server, daemon | 'all'}
  ↓
GUI handler invokes binary_discovery.Discover(ctx, requiredBinaries, DefaultHints())
  ↓
WriteOverlay(...) — flock-protected RMW with source: operator preservation
  ↓
Handler returns 200 + new effective env to GUI for display
  ↓
GUI shows "Restart daemon to apply" affordance; operator click → POST /api/daemon/respawn
```

### Apply env edit from GUI (new)

```text
GUI per-row drawer → Edit Path field → Apply
  ↓
POST /api/daemon/env {taskName, env: {KEY: value, ...}}
  ↓
GUI handler validates:
  - taskName (normalized via normalizeOverlayKey) is in current supervisor-intent.json
  - keys/values contain no newlines / NUL / control chars
  - origin/host posture via existing requireAllowedHost + requireSameOrigin
    middleware (see Security section)
  ↓
WriteOverlay(path, mutator) — flock-protected RMW
  Inside mutator: write row with source: operator, modified_at: <ts>
  Mutator-error contract: if mutator returns err, NO write to disk;
    flock released; error propagated verbatim
  ↓
Handler returns 200 + new effective env
  ↓
GUI shows "Restart daemon to apply" affordance; operator click → POST /api/daemon/respawn
```

### Respawn from GUI (new)

```text
GUI per-row drawer → "Restart daemon to apply" button (optional force checkbox)
  ↓
POST /api/daemon/respawn {taskName, force: bool}
  ↓
GUI handler validates: normalized taskName in supervisor-intent.json; same-origin posture
  ↓
If daemon is in Quarantined state AND force == false:
  Return HTTP 409 {state: "quarantined", remedy: "force or unquarantine"}
  ↓
Issue supervisor IPC: respawn {taskName, force}
  ↓
Supervisor: graceful 5s shutdown → SIGKILL/Job-Object kill → spawn-from-intent+overlay
  ↓
GUI polls /api/status, surfaces new PID + Port + State
```

## Components

| Component | New / Modified | Purpose | Owns | Depends on |
|---|---|---|---|---|
| `internal/api/binary_discovery/` | NEW | Auto-discover common binary paths per OS | shipped per-OS hints with version-agnostic globs, hint-injection seam for tests, CAS on source:operator | (none) |
| `internal/api/daemon_env_overlay/` | NEW | YAML overlay file owner | overlay parse, flock-protected RMW writer, hardened read (new `checkStateDirParentReadSafe`), comment-preserving YAML round-trip, `WriteOverlay(path, mutator func) error` transactional API with no-write-on-mutator-error contract, `normalizeOverlayKey(taskName) string` helper used at all call sites | `gopkg.in/yaml.v3` Node API, existing `SecureWriteClientConfig`, flock helper |
| `internal/cli/supervise.go` `mergeDaemonEnv` (lines 1456-1488) | MODIFY | Apply overlay at spawn-time; remove `len(d.Env) > 0` gate at lines 1504-1506; Windows case-insensitive key normalize | env merge precedence, `${parent_path}` expansion | daemon_env_overlay |
| `internal/cli/supervise.go` IPC dispatcher | MODIFY | Add `respawn` IPC at the case-switch around line 921; accepts `{taskName, force}` | IPC frame parse, graceful 5s shutdown, spawn-from-intent-overlay, lifecycle event emit | reconcile loop, daemon_env_overlay |
| `internal/api/scan.go` | MODIFY | Recognize mcp-language-server entries via three-rule algorithm + registry reverse-lookup; populate `LegacyConflict` on coexistence | per-language entry classification, badge metadata, registry walk for ownership | new `manifest_lsp_lookup.go`, workspace registry reader |
| `internal/api/manifest_lsp_lookup.go` | NEW | Reverse-lookup helpers; `parseEntryName(name, langs) (lang string, suffix string)` | manifest reading, language set lookup, suffix-stripping regex | config package |
| `internal/api/types.go` | MODIFY | Add `ScanEntry.LegacyConflict map[string]ClientEntry` field (omitempty) | schema extension | (none) |
| `internal/gui/server.go` | NEW handlers | `/api/daemon/env`, `/api/discovery/refresh`, `/api/daemon/respawn` | route registration; uses existing `requireSameOrigin` + `requireAllowedHost` middleware | supervisor IPC, daemon_env_overlay |
| `internal/gui/frontend/src/screens/Servers.tsx` | MODIFY | Active-workspace selector; 9 LSP rows; per-row drawer with env editor + restart button + force checkbox; `${parent_path}` warning chip; dual-badge rendering reading `ClientPresence` + `LegacyConflict` | per-cell action mapping, drawer state, env edit form | new API endpoints, schema change |
| `servers/mcp-language-server/manifest.yaml` | MODIFY | Add `required_binaries` per language | manifest schema | (config schema) |
| `internal/config/manifest.go` | MODIFY | Add `RequiredBinaries []string` field to `ServerManifest` and `LanguageSpec` structs; no `Validate()` logic added | YAML deserialization (preserve `KnownFields(true)` strictness at line 128) | (none) |
| `servers/gdb/manifest.yaml` | MODIFY | Add `required_binaries: [gdb]` at server level | manifest schema | (config schema) |
| `servers/lldb/manifest.yaml` | MODIFY | Add `required_binaries: [lldb]` at server level (or empty since `command: mcphub` is internal bridge — see open Q2) | manifest schema | (config schema) |
| `internal/api/state_file_helper_read.go` | NEW | `checkStateDirParentReadSafe(dir string) error` — package-internal symmetric helper to existing `checkStateDirParentWriteSafe` at [state_file_helper.go:155](../../../internal/api/state_file_helper.go); reuses parent-DACL logic | parent-dir DACL/mode check, default-relax + strict-mode semantics with new env var `MCPHUB_ALLOW_UNHARDENED_STATE_READ` | (existing) operatorRequiresSingleUserHome |

## Read-side hardening

The supervisor's spawn-time overlay READ is on the trust boundary for child process PATH. v4 specifies the read-side helper:

### Step 1 — Open with reparse-point refusal

**POSIX**: `os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)`. The Go runtime honors `O_NOFOLLOW` on POSIX; the open itself refuses to follow symlinks at kernel level.

**Windows**: cannot use `os.OpenFile` with `O_NOFOLLOW` because the Go runtime returns 0 for that flag on Windows ([internal/clients/write_nofollow_windows.go:20-22](../../../internal/clients/write_nofollow_windows.go) `createNoFollowFlag` returns 0). The correct pattern, already used in this repo for hub-mcp state DACL verification at [internal/api/hub_mcp_state_dacl_windows.go:85-99](../../../internal/api/hub_mcp_state_dacl_windows.go):

```go
h, err := windows.CreateFile(
    pathW,
    windows.GENERIC_READ,
    windows.FILE_SHARE_READ,
    nil,
    windows.OPEN_EXISTING,
    windows.FILE_FLAG_OPEN_REPARSE_POINT | windows.FILE_FLAG_BACKUP_SEMANTICS,
    0,
)
```

The `FILE_FLAG_OPEN_REPARSE_POINT` flag **SET** means "open the reparse point itself, do NOT follow the link" (Win32 semantics). After the open succeeds, query `BY_HANDLE_FILE_INFORMATION` via `GetFileInformationByHandle` and refuse if `dwFileAttributes & FILE_ATTRIBUTE_REPARSE_POINT != 0` (pattern at [hub_mcp_state_dacl_windows.go:192](../../../internal/api/hub_mcp_state_dacl_windows.go)).

This corrects v3's polarity inversion. The flag's name is misleading — "OPEN_REPARSE_POINT" means "I want to open the reparse point itself, not what it points to". Setting the flag is the symlink-refusal posture.

### Step 2 — Stat the open handle

`fi := f.Stat()` (POSIX) or `GetFileInformationByHandle` (Windows) on the open handle. Defeats TOCTOU between any earlier check and the actual read because the handle was bound to the kernel inode/file-object at open time.

### Step 3 — Reject non-regular files

`if !fi.Mode().IsRegular() { return ErrOverlayNotRegular }`. Use `Mode().IsRegular()`, NOT bit math on `os.ModeIrregular` (v2 had wrong bit math; v3 fixed it; v4 keeps the fix).

### Step 4 — Parent-dir DACL check via new `checkStateDirParentReadSafe`

Symmetric with existing `checkStateDirParentWriteSafe` at [state_file_helper.go:155](../../../internal/api/state_file_helper.go). Same default-relax / strict-mode semantics:

- If `MCPHUB_REQUIRE_SINGLE_USER_HOME=1` is set, parent-DACL rejection is a hard error.
- Else, if parent grants write/delete to non-allowlisted principal, run the existing parent-write-bits check (symmetric defense — co-resident attacker can swap the file regardless of read/write).
- Else, if `MCPHUB_ALLOW_UNHARDENED_STATE_READ=1` is set, opt into the relax lane explicitly.
- Else (default), log warn `daemon-env-overlay-read-unhardened-fallback` and proceed (default-relax for legitimate corp hosts; symmetric with existing write side at [state_file_helper.go:139-157](../../../internal/api/state_file_helper.go)).

Env var naming verified consistent with existing surface: `MCPHUB_ALLOW_UNHARDENED_CLIENT_WRITE` ([client_write_init.go:98](../../../internal/api/client_write_init.go)) and `MCPHUB_ALLOW_UNHARDENED_STATE_WRITE` ([client_write_init.go:105](../../../internal/api/client_write_init.go)) follow the same naming pattern.

### Step 5 — Owner check + size cap + UTF-8 validation

- POSIX: stat uid == os.Getuid(); Windows: file owner SID matches process token user SID.
- 64 KiB size cap via `io.LimitReader(f, 65536+1)`.
- Reject non-printable / non-UTF-8 bytes (defense-in-depth before YAML parse).

If any hardened-read failure occurs, the supervisor refuses to spawn ANY daemon (whole-overlay fail-LOUD; see Error Handling).

## Error handling

| Failure mode | Behavior |
|---|---|
| Overlay file missing | Treat as empty overlay. No error. Manifest env applies. |
| Overlay file unreadable (permission denied / symlink at path / reparse point on Windows) | **Fail-LOUD.** Supervisor refuses to spawn ANY daemon (overlay parse failure means "affected daemons" is unknowable). GUI shows red banner: `daemon-env-overlay-read-rejected`. Audit event with reason. Operator runs `mcphub config overlay-quarantine` (offline CLI — does NOT require supervisor IPC) to rename + restart with empty overlay. |
| Overlay file corrupt YAML / size > 64 KiB / non-UTF-8 | **Fail-LOUD.** Same as above; parse error includes line/col for inline editor jump. |
| Overlay declares an unknown taskName | Log warn `daemon-env-overlay-orphan-row` at supervisor startup. Ignore that row at spawn. GUI surfaces orphan rows in a dedicated section with "delete this row" affordance. Triggered automatically after `mcphub unregister <workspace>` removes the workspace from registry. Orphan detection uses `normalizeOverlayKey` to match canonical form. |
| Overlay row exists but `${parent_path}` resolution fails | Per-row failure only; refuse to spawn that one daemon; emit `daemon-env-overlay-parent-path-resolve-failed`. |
| Auto-discovery cannot find a required binary | Discovery returns `{binary: ""}` for the missing one. Overlay row written with comment `auto-detected: BINARY_NAME not found in any common location`. GUI shows red flag on the daemon's row. |
| Apply IPC respawn fails | GUI surfaces the supervisor error; overlay change stays on disk; operator can retry. |
| Respawn requested for Quarantined daemon without `force: true` | HTTP 409 from `/api/daemon/respawn`; body explains remedy. |
| `mcphub register <ws> <lang>` fails | Existing register error handling unchanged. |
| Concurrent GUI Apply + auto-discovery refresh | Both go through `WriteOverlay(path, mutator)`'s flock; second caller waits. |
| `WriteOverlay` mutator returns error | NO write to disk; flock released; error propagated verbatim. Mutator MUST not perform external side effects before returning error (the rollback contract covers YAML persistence only — external side effects cannot be undone). |
| `workspaces.yaml` missing/corrupt during recognition | Step B owningWorkspace stays nil. Recognition still labels language correctly (Step A); status renders "workspace unknown". No crash. |

## `mcphub config overlay-quarantine` (offline CLI)

**Standalone CLI, does NOT require supervisor IPC** (this is the documented recovery path for the fail-LOUD blocked-supervisor state):

1. Acquire the same per-file flock as `WriteOverlay` (path: `daemon-env-overrides.yaml.lock`).
2. If the overlay file does not exist → exit 0 with message "no overlay to quarantine".
3. `os.Rename(<overlay>, <overlay>.corrupt-<RFC3339-ts>)` — atomic on POSIX, atomic-by-rename on Windows.
4. After rename, scan for existing `<overlay>.corrupt-*` files; keep the **5 newest** by mtime; delete older (per-file delete failures are non-fatal, logged warn). Mirrors v0.4.x watchdog quarantine retention pattern documented in CLAUDE.md.
5. Release flock.
6. Print operator guidance: "renamed to `<new-path>`. Run `mcphub restart` (or wait for next supervisor cold start) to apply."

Does NOT signal or restart the supervisor; operator restarts manually.

## Manifest schema additions

Add optional `required_binaries` field at server OR language level. **Same commit** adds the field to the Go structs (`ServerManifest`, `LanguageSpec` in [internal/config/manifest.go](../../../internal/config/manifest.go)) so `ParseManifest`'s `KnownFields(true)` strictness at line 128 still rejects unknown keys. Free-form metadata; `Validate()` does NOT enforce binary existence.

```yaml
# Server-level (e.g. gdb)
required_binaries:
  - gdb

# Per-language (mcp-language-server)
languages:
  - name: clangd
    backend: mcp-language-server
    transport: stdio
    lsp_command: clangd
    required_binaries: [clangd]
  - name: rust
    backend: mcp-language-server
    transport: stdio
    lsp_command: rust-analyzer
    required_binaries: [rust-analyzer]
  - name: go
    backend: gopls-mcp
    transport: stdio
    lsp_command: gopls
    extra_flags: [mcp]
    required_binaries: [gopls]
```

## DefaultHints — shipped per-OS list (version-agnostic)

`binary_discovery.DefaultHints()` returns a `[]string` for the current OS. Windows hints:

- `C:\msys64\ucrt64\bin`, `C:\msys64\mingw64\bin`, `C:\msys64\clang64\bin`
- `C:\Program Files\LLVM\bin`
- `C:\Program Files\Go\bin`
- `C:\Program Files\Microsoft Visual Studio\2022\BuildTools\VC\Tools\Llvm\x64\bin`
- `%LOCALAPPDATA%\Programs\Python\Python3*` — **glob, not version-locked literal** (v4 M-V4-1 fix; v3 hardcoded `Python311`-`Python314`)
- `%USERPROFILE%\.cargo\bin`, `%USERPROFILE%\go\bin`, `%USERPROFILE%\.local\bin`
- `%LOCALAPPDATA%\fnm_multishells`, `%LOCALAPPDATA%\Programs\fnm`, `%LOCALAPPDATA%\nvm`
- `%APPDATA%\npm`

Linux: `/usr/local/bin`, `/usr/bin`, `~/.local/bin`, `~/.cargo/bin`, `~/go/bin`.

macOS: `/opt/homebrew/bin`, `/usr/local/bin`, `/opt/local/bin` + Linux list.

Hints are best-effort seed data only. **The operator's override channel is the env overlay file, NOT hints expansion.** Tests pass synthetic temp-dir roots; production uses `DefaultHints()`.

## Security

### Write-side hardening (existing posture, no new code)

`WriteOverlay` writes through exported `SecureWriteClientConfig(path string, payload []byte) error` ([state_file_helper.go:127](../../../internal/api/state_file_helper.go)) with YAML-marshaled bytes. Per-file DACL at temp-create is handle-bound (POSIX) / NtCreateFile-bound (Windows) per the existing pipeline.

### Read-side hardening (new — see "Read-side hardening" section above)

- New package-internal `checkStateDirParentReadSafe` helper.
- CreateFileW with `FILE_FLAG_OPEN_REPARSE_POINT | FILE_FLAG_BACKUP_SEMANTICS` SET on Windows + post-open `FILE_ATTRIBUTE_REPARSE_POINT` refusal (pattern from [hub_mcp_state_dacl_windows.go:85-192](../../../internal/api/hub_mcp_state_dacl_windows.go)).
- POSIX `O_NOFOLLOW`.
- `Mode().IsRegular()` for non-regular refusal.
- Default-relax / strict-mode symmetric with write side via new env var `MCPHUB_ALLOW_UNHARDENED_STATE_READ`.

### `/api/daemon/env`, `/api/discovery/refresh`, `/api/daemon/respawn` auth posture (existing middleware, no new tokens)

All three endpoints reuse the existing mutation-route posture in [internal/gui/csrf.go](../../../internal/gui/csrf.go):

- **Outer `requireAllowedHost`** (applied at `s.httpHandler()` to ALL routes; [csrf.go:17, 20-29](../../../internal/gui/csrf.go)) — rejects DNS-rebinding by validating Host header is `localhost:<port>` or `127.0.0.1:<port>` matching `s.effectivePort()`.
- **`requireSameOrigin` middleware** for mutating routes ([csrf.go:81-99](../../../internal/gui/csrf.go)): when `Origin` header is present, it must match `allowedOrigin` (scheme=http, allowed host, no userinfo/query/fragment, root path); when `Sec-Fetch-Site` is present, it must NOT be cross-site. Empty `Origin` AND empty `Sec-Fetch-Site` both pass (so curl/native callers work). 403 on failure.
- **No CSRF token.** v3 incorrectly claimed `X-Mcphub-CSRF` header existed; it does not. Loopback-only bind + Host/Origin/Sec-Fetch-Site checks ARE the CSRF defense in this codebase.
- **Known-task validation.** Endpoint handlers reject with HTTP 400 if `normalizeOverlayKey(taskName)` is not present in current `supervisor-intent.json`.
- **Per-key value validation** (for `/api/daemon/env`): Keys must match `[A-Za-z_][A-Za-z0-9_]*`; values reject newline, NUL, control chars.

### `${parent_path}` token semantics

- The ONLY supported template token. Operator's overlay value is treated as opaque bytes after expansion; values go to `exec.Command` env block, not a shell.
- Token resolution: at spawn time, supervisor reads `os.Environ()` and finds the PATH key (case-insensitive on Windows: `Path`, `PATH`, `path` all match the same logical key). Single-pass, non-recursive expansion.
- If `${parent_path}` is omitted from the operator's PATH value, parent PATH is **dropped** for that daemon. GUI shows warning chip; supervisor emits `daemon-env-overlay-path-no-parent-token` info event at spawn (visible via `mcphub status` / event log) even when the operator hand-edited the YAML and bypassed the GUI.

## Observability

New events on the hub-mcp event log:

- `daemon-env-overlay-loaded` (info): row count, `sha256(${parent_path})[:12]` of resolved parent (full PATH only at debug level)
- `daemon-env-overlay-load-failed` (warn): path, error class (symlink-refused, size-exceeded, parse-failed, parent-dir-unsafe, non-owner), line/col for parse-failed
- `daemon-env-overlay-read-rejected` (error): path, hardened-read failure mode — supervisor refuses to spawn ANY daemon
- `daemon-env-overlay-read-unhardened-fallback` (warn): default-relax path on legitimate corp host
- `daemon-env-overlay-applied-via-gui` (info): taskName, changed keys (values redacted)
- `daemon-env-overlay-orphan-row` (warn): taskName not in current intent
- `daemon-env-overlay-skipped-operator-override` (info): taskName, binary — auto-discovery refused to overwrite `source: operator`
- `daemon-env-overlay-parent-path-resolve-failed` (warn): per-row resolution failure
- `daemon-env-overlay-path-no-parent-token` (info): daemon spawned with PATH that omits `${parent_path}`
- `binary-discovery-ran` (info): server, scan duration, hits per binary
- `binary-discovery-missing` (warn): server, binary, scanned hints summary
- `supervisor-respawn-via-gui` (info): taskName, requesting client (loopback PID), respawn outcome, `force` flag value
- `supervisor-respawn-graceful-timeout` (warn): taskName, soft-shutdown deadline exceeded
- `supervisor-respawn-refused-quarantined` (info): taskName, request lacked `force: true`

## Testing strategy

### Unit

- `binary_discovery/discover_test.go`: pass `hints=[<tempdir>]` with synthesized fake binaries; assert correct paths. Inject via parameter — production uses `DefaultHints()`.
- `binary_discovery/source_override_test.go`: pre-write overlay row with `source: operator`; run discovery's mutator; assert row preserved unchanged.
- `daemon_env_overlay/parse_test.go`: round-trip YAML preserving comments; ordered keys; reject open-time symlink (POSIX) / reparse point (Windows via `FILE_ATTRIBUTE_REPARSE_POINT` check); reject 64 KiB+1; reject non-UTF-8.
- `daemon_env_overlay/write_test.go`: `WriteOverlay(path, mutator)` flock-protects concurrent writes; atomic temp+rename; mutator-error path produces NO disk modification + flock release + error propagation; mutator side-effect-before-error scenario.
- `daemon_env_overlay/read_hardening_test.go`: new `checkStateDirParentReadSafe` symmetric with `checkStateDirParentWriteSafe`; `Mode().IsRegular()` rejects directories + pipes + sockets; default-relax fallback on parent-DACL rejection with both `MCPHUB_REQUIRE_SINGLE_USER_HOME` and `MCPHUB_ALLOW_UNHARDENED_STATE_READ` paths exercised.
- `daemon_env_overlay/normalize_test.go`: `normalizeOverlayKey` idempotency; called from spawn lookup, GUI write, mutator, orphan detection.
- `mergeDaemonEnv` extended tests: overlay arg non-nil; precedence parent < manifest < overlay; Windows case-insensitive `PATH`/`Path` merge; both-empty fallback to nil cmd.Env.
- `scan.go` three-rule recognition: test fixtures for (a) hub URL entries with collision suffix (4hex and 8hex forms), (b) direct-stdio `mcp-language-server --lsp X` for JS+TS sharing `typescript-language-server`, (c) direct-stdio `gopls mcp`, (d) coexistence: hub + stdio for same (client, language) → `LegacyConflict` populated; (e) registry-empty fallback.
- `manifest_lsp_lookup_test.go`: `parseEntryName(name, langs)` for all 9 languages with/without suffix, with full 8-hex and short 4-hex forms.
- `types_legacy_conflict_test.go`: `ScanEntry.LegacyConflict` JSON round-trip; omitempty when empty; populated case.

### Integration

- `internal/cli/supervise_lsp_e2e_test.go`: register one language via `mcphub register`; verify supervisor-intent.json gains entry with canonical leading-backslash TaskName; verify supervisor spawns lazy proxy on materialization; verify env from overlay applies.
- `internal/cli/supervise_respawn_test.go`: IPC `respawn` with valid TaskName + `force=false` → graceful 5s → spawn; invalid TaskName → IPC error `UNKNOWN_TASK`; daemon in Backoff → reset to Spawning + spawn; daemon in Quarantined + `force=false` → refuse; daemon in Quarantined + `force=true` → spawn.
- `internal/gui/daemon_env_handler_test.go`: POST /api/daemon/env writes overlay (no IPC); assert event log entry; assert `requireSameOrigin` rejects cross-origin Origin header (matches existing csrf_test.go pattern); assert unknown taskName rejected with 400.
- `internal/api/daemon_env_overlay/integration_test.go`: WriteOverlay + Load round-trip across multiple goroutines under flock.

### Multi-workspace LSP recognition test

`internal/gui/e2e/tests/servers-lsp-multi-workspace.spec.ts`:

1. Seed 2 workspaces in `workspaces.yaml`, both registered for clangd. Second registration triggers `ResolveEntryName` → entry name `mcp-language-server-clangd-<4hex>` in ws2's clients.
2. Active-workspace selector defaults to ws1; matrix clangd row shows URL for ws1 proxy.
3. Switch selector to ws2; assert per-row drawer's active-workspace badge updates; assert reverse-lookup against `ws2.ClientEntries[client] == "mcp-language-server-clangd-<4hex>"` resolves correctly.
4. Per-row drawer surfaces per-workspace history with both proxies.
5. Uncheck clangd cell for codex client while selector is ws2 → `mcphub register --unset` runs against ws2 only; ws1's clangd row unchanged.

### E2E (Playwright)

- `internal/gui/e2e/tests/servers-lsp.spec.ts`: matrix shows exactly 9 LSP rows; check/uncheck cycles register/unregister per active workspace; per-row drawer with env editor + Restart button + force checkbox.
- `internal/gui/e2e/tests/servers-env-overlay.spec.ts`: per-row drawer reveals effective env; edit Path WITHOUT `${parent_path}` → warning chip; Apply writes overlay; click Restart triggers respawn.
- `internal/gui/e2e/tests/servers-coexistence-anomaly.spec.ts`: seed both hub-managed AND direct-stdio for same client+language → assert `LegacyConflict[client]` populated in `/api/scan` response; assert cell renders both `[via-hub]` and `[legacy]` badges; per-row drawer surfaces remediation guidance.

## Migration

For existing installs:

- **Overlay file does not exist** → supervisor merges only `os.Environ()` + `SupervisorDaemon.Env` (same as today). No behavior change.
- **LSP-recognition addition** → rows previously in "Other MCP entries" now appear as proper matrix rows. Clients keep existing direct-stdio entries; matrix offers explicit migrate-to-hub action.
- **`mcphub install/setup` auto-discovery** → runs once (writes initial overlay with `source: auto-discovery`). Existing operators get a one-time `binary-discovery-ran` event.
- **`mcphub unregister <workspace>`** → emits `daemon-env-overlay-orphan-row` for stale `\mcp-local-hub-lsp-<wsKey>-*` taskNames. `mcphub config prune-orphan-overlay-rows` removes them.
- **`ScanEntry.LegacyConflict` addition** → existing JSON consumers ignore the field (omitempty); new GUI consumes it.

## Open questions parked for plan / implementation phase

1. **GUI UX shape for `Active workspace` selector**: top-of-screen vs per-row dropdown? Recognition is correct either way (registry reverse-lookup is the source of truth); the question is purely about discoverability.
2. **Auto-discovery scope for non-LSP servers**: `gdb`, `godbolt`, `perftools` need external binaries. Server-level `required_binaries` is added. Open: should `lldb` manifest's `command: mcphub` (internal bridge) declare empty `required_binaries: []`? Natural answer: yes — internal bridges have no external deps.
3. **GUI "Active workspace" persistence**: per-machine OR per-GUI-session? Proposed: per-machine in `gui-preferences.yaml`.
4. **`workspaces.yaml` missing/corrupt at recognition time** (added in v4): degraded-mode behavior is specified (Step B owningWorkspace stays nil, recognition still labels language). Open: should the GUI surface a warning banner when the file is missing but entries are detected?

## Terms and Abbreviations

- **LSP**: Language Server Protocol; a JSON-RPC protocol exposing IDE-grade code intelligence over a stdio/socket channel.
- **mcp-language-server**: Go binary installed via `go install` to `$GOPATH/bin/mcp-language-server`; bridges an LSP server to the MCP protocol.
- **gopls-mcp**: alternative backend used by the Go language. The `gopls` binary supports an `mcp` subcommand exposing MCP directly without the mcp-language-server bridge ([manifest.yaml:33-37](../../../servers/mcp-language-server/manifest.yaml)).
- **Hub-proxy / lazy proxy**: a workspace-scoped mcphub daemon listening on a port from the 9200-9299 pool and forwarding MCP traffic to the heavy LSP backend.
- **Direct-stdio / legacy**: an MCP client config entry that invokes `mcp-language-server` or `gopls` directly via stdio, bypassing mcphub. The matrix surfaces these with a "legacy" badge.
- **Workspace-scoped**: a server kind where each (workspace, language) pair gets its own daemon. Contrast with `kind: global`.
- **Overlay**: a user-editable layer that augments shipped manifests without modifying them.
- **Active workspace**: the workspace whose `mcphub register`-ed lazy proxies are addressed by Servers matrix actions in the GUI.
- **`${parent_path}` token**: a placeholder in the overlay's env values that expands at spawn time to the supervisor process's PATH.
- **wsKey**: the 8-character hex hash of a workspace path. Short form is the first 4 hex chars ([register.go:730-732](../../../internal/api/register.go)).
- **ResolveEntryName**: at [register.go:722-747](../../../internal/api/register.go); computes the client-config entry name with collision suffix when same-language multi-workspace forces it. Suffix is workspaceKey[:4] (short) or full workspaceKey (on prefix collision).
- **SupervisorDaemon.TaskName**: canonical identifier in supervisor-intent.json + IPC commands + overlay keys. STORED WITH leading backslash per [supervisor_intent.go:25](../../../internal/api/supervisor_intent.go). `register.go` writes the BARE form into `WorkspaceEntry.TaskName`; the leading `\` is added at the install-time transcription to supervisor-intent.json.
- **`normalizeOverlayKey`**: new helper in `daemon_env_overlay` package; prepends `\` if absent. Used at every overlay call site (spawn lookup, GUI write, mutator, orphan detection).
- **flock**: POSIX `flock(2)` / Windows `LockFileEx` advisory lock; serializes RMW on the overlay file across processes.
- **`requireSameOrigin` / `requireAllowedHost`**: existing GUI middleware at [internal/gui/csrf.go](../../../internal/gui/csrf.go); CSRF defense in this codebase is origin/host checking + loopback-only bind, NOT token-based.
- **TOCTOU**: Time-Of-Check-To-Time-Of-Use race; v4's read-side hardening uses open-with-correct-flags + stat-handle to close this window.
- **`FILE_FLAG_OPEN_REPARSE_POINT`**: Win32 flag; **SET** means "open the reparse point itself, do NOT follow the link" (counter-intuitive naming). v4 sets the flag and refuses by checking `FILE_ATTRIBUTE_REPARSE_POINT` on the opened handle.
- **`MCPHUB_ALLOW_UNHARDENED_STATE_READ`** (new v4): operator opt-in env var for read-side relax lane; mirrors existing `MCPHUB_ALLOW_UNHARDENED_STATE_WRITE` ([client_write_init.go:105](../../../internal/api/client_write_init.go)).
- **`ScanEntry.LegacyConflict`** (new v4): omitempty side-channel field on `ScanEntry` holding the stdio entry when a hub entry coexists for the same (client, server) tuple.

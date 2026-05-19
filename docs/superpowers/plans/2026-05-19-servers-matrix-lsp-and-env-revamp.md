# Servers matrix revamp: LSP-bridge integration + per-daemon env overlay — Implementation Plan (v2 thin)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Per memory rule "Subagents always opus + max", dispatch every subagent with `model: opus`.
>
> **Thin-plan posture (v2 revision):** v1 of this plan inlined concrete Go code blocks for each task. Dual review caught **15+ BLOCKERS** of fabricated API symbols (wrong field names, nonexistent functions, fabricated test helpers). v2 removes all concrete code blocks. Each task is **scope + acceptance criteria + test contract + spec reference + implementer notes with verified real symbol names**. The implementer subagent uses `Read`/`Grep`/`Edit` to inspect live source before writing code.
>
> **Plan v1 cumulative findings (NOT all addressed in v2 — listed for transparency):** P1-P15 fabricated symbols caught by dual review. v2 closes all of them by removing the offending code, but the implementer must verify each Go symbol against the actual source. See "Implementer notes" per task for the verified symbol catalog.

**Goal:** Surface the 9 LSP-bridge languages as proper Servers-matrix rows, add a per-daemon env overlay file editable from the GUI, auto-discover binary locations at install time, expand `${parent_path}` at spawn time, emit the spec's observability events, and replace the supervisor's UNKNOWN_COMMAND `restart` stub with a working `respawn` IPC command.

**Architecture:** Four sequential phases on a single branch, single PR. Phase 1 adds `required_binaries` manifest field + `binary_discovery` package + wires it into the install path. Phase 2 adds the `daemon_env_overlay` package + extends `mergeDaemonEnv` (including `${parent_path}` expansion) + removes the spawn gate + emits overlay events. Phase 3 adds `ScanEntry.LegacyConflict` + three-rule LSP recognition via reverse-lookup + frontend type wiring. Phase 4 adds the `respawn` IPC command + three GUI endpoints + frontend Servers.tsx changes + Playwright e2e.

**Tech Stack:** Go 1.22 (backend), Preact + TypeScript + Vite (frontend), Playwright (E2E), `gopkg.in/yaml.v3` (overlay YAML), `github.com/gofrs/flock` (overlay RMW lock), `golang.org/x/sys/windows` (Windows reparse-point handling via existing pattern at `internal/api/hub_mcp_state_dacl_windows.go`).

**Spec:** [docs/superpowers/specs/2026-05-19-servers-matrix-lsp-and-env-revamp-design.md](../specs/2026-05-19-servers-matrix-lsp-and-env-revamp-design.md) (v4 at HEAD `5295c90`, three dual-review cycles + audit verification posture).

---

## Verified API symbol catalog (the implementer's grounding sheet)

Every Go symbol below was grep-verified against the current codebase. Plan v1's BLOCKERS came from fabricating these — v2 lists the real names so the implementer doesn't fall into the same trap.

| Concept | Real symbol | Location |
|---|---|---|
| IPC request fields | `IPCRequest.Cmd`, `IPCRequest.Args` (NOT `.Body`) | `internal/api/supervisor_ipc.go:12-17` |
| IPC response fields | `IPCResponse.ID`, `IPCResponse.OK`, `IPCResponse.Result` (NOT `.Body`), `IPCResponse.Error`, `IPCResponse.Final` | `internal/api/supervisor_ipc.go:49-54` |
| IPC dispatcher deps | `ipcDispatchDeps` struct (fields: stateDir, events, runtimeTracker, reconcileReady, intentFilesLoaded, gracefulInProgress, triggerGracefulExit) | `internal/cli/supervise.go:257-270` |
| IPC handler pattern | `func handleX(conn net.Conn, req api.IPCRequest, deps ipcDispatchDeps) error` | `internal/cli/supervise.go:1009,1092` (handleQuiesceTimers + handleExit are the templates) |
| Event logging | `api.LogHubMcpEvent(level, event string, fields map[string]any) error` | `internal/api/hub_mcp_log.go:98` |
| State dir | `var stateDirFunc = func() (string, error)` — call as `dir, err := stateDirFunc()` | `internal/cli/supervise.go:132` |
| Workspace registry load | Two-step: `path, err := api.DefaultRegistryPath(); reg, err := api.NewRegistry(path).Load()`. NOT `LoadWorkspaceRegistry` (fabricated). `DefaultRegistryPath` returns `(string, error)` — cannot inline. | `internal/api/workspace_registry.go:60,75` |
| Default registry path | `func DefaultRegistryPath() (string, error)` — returns 2 values, NOT 1 | `internal/api/workspace_registry.go:75` |
| Workspace entry fields | `WorkspaceEntry.WorkspaceKey`, `.Language`, `.Port`, `.TaskName`, `.ClientEntries map[string]string` | `internal/api/workspace_registry.go:30-46` |
| Scan API | `(*api.API).ScanFrom(opts api.ScanOpts) (*api.ScanResult, error)` — returns POINTER to ScanResult (NOT value); NOT bare `ScanFrom(home)` | `internal/api/scan.go:17-29,240` |
| ClientEntry shape | `{Transport, Endpoint, Raw map[string]any}` only — NO URL/Command/Args fields | `internal/api/types.go:110-114` |
| ScanEntry shape | `{Name, Status, ClientPresence map[string]ClientEntry, ManifestExists, CanMigrate, ProcessCount}` | `internal/api/types.go:99-106` |
| Codex client key | `"codex-cli"` (NOT `"codex"`) | `internal/api/scan.go:476` |
| shapeClaudeEntry pattern | parses `raw["url"]` → http; `raw["command"]` → stdio (Endpoint=cmd). NOTE: args remain inside `Raw map[string]any` — callers needing args read `raw["args"]` themselves. Three-rule recognition (Task 3.3) needs both. | `internal/api/scan.go:448-454` |
| Windows reparse-point pattern | **Required minimum (Task 2.4):** direct `windows.CreateFile(pathW, GENERIC_READ, FILE_SHARE_READ, nil, OPEN_EXISTING, FILE_FLAG_OPEN_REPARSE_POINT\|FILE_FLAG_BACKUP_SEMANTICS, 0)` on the OVERLAY FILE — flag SET — then `GetFileInformationByHandle` then refuse if `bhfi.FileAttributes & windows.FILE_ATTRIBUTE_REPARSE_POINT != 0`. Pattern at `hub_mcp_state_dacl_windows.go:85-99` (parent dir uses this exact form) + attribute refusal idiom at `:187-193`. **Defense-in-depth upgrade (optional):** open parent dir with the same flags, then open the child file relative to the parent handle via `ntOpenRelative` (`:157-165`). Use the simpler form for v0.5.x; upgrade later if the threat model tightens. Returns `windows.Handle`, NOT `*os.File` — the implementer writes a thin reader. | `internal/api/hub_mcp_state_dacl_windows.go:85-99,157-165,187-193` |
| Parent-DACL write check | `checkStateDirParentWriteSafe(dir) error` (unexported; either rename to exported or add exported shim per Task 2.4) | `internal/api/state_file_helper.go:155` |
| State helper write byte API | `func SecureWriteClientConfig(path string, contents []byte) error` (exported, raw bytes — usable for YAML). NOTE: parameter is `contents`, not `payload`. Definition lives at `secure_write_client_config.go:76`; `state_file_helper.go:127` is just a call site. | `internal/api/secure_write_client_config.go:76` |
| Existing env var names | `MCPHUB_ALLOW_UNHARDENED_CLIENT_WRITE`, `MCPHUB_ALLOW_UNHARDENED_STATE_WRITE`, `MCPHUB_REQUIRE_SINGLE_USER_HOME` | `internal/api/client_write_init.go:98,105` |
| Default-relax / strict-mode pipeline | Read `state_file_helper.go:139-157`; new read-side mirror per spec v4 §"Read-side hardening" | — |
| LSP task-name generator | `fmt.Sprintf("mcp-local-hub-lsp-%s-%s", wsKey, lang)` (BARE form, no leading backslash) | `internal/api/register.go:292` |
| Client-entry collision resolver | `ResolveEntryName(reg, server, lang, wsKey)`; suffix is `workspaceKey[:4]` or full `workspaceKey` on prefix collision | `internal/api/register.go:722-747` |
| SupervisorDaemon canonical form | `TaskName` is stored WITH leading backslash, e.g. `"\\mcp-local-hub-memory-default"` | `internal/api/supervisor_intent.go:25` |
| Reconcile daemon index | `daemonIntent.Tasks[d.TaskName]` (canonical form) | `internal/cli/supervise_reconcile.go:107` |
| Spawn gate to remove | `if len(d.Env) > 0 { cmd.Env = mergeDaemonEnv(os.Environ(), d.Env) }` at lines 1504-1506 | `internal/cli/supervise.go:1504-1506` |
| mergeDaemonEnv current sig | `func mergeDaemonEnv(parent []string, overrides map[string]string) []string` (2 args today, plan extends to 3) | `internal/cli/supervise.go:1456-1471` |
| IPC restart stub to replace | `case "restart", "reload":` returns UNKNOWN_COMMAND | `internal/cli/supervise.go:916-934` |
| GUI auth middleware | `(*Server).requireSameOrigin(next http.HandlerFunc) http.HandlerFunc` + outer `requireAllowedHost`; NO token-based CSRF | `internal/gui/csrf.go:17,81-99` |
| Manifest strict-parse | `dec.KnownFields(true)` — every new YAML key MUST have a matching Go struct field with yaml tag | `internal/config/manifest.go:128` |
| Existing test seed helpers | `seedClaudeLS`, `seedCodexLS`, `seedMembershipRegistry` (use these as patterns when creating new helpers). Note: file is under `internal/cli/`, not `internal/api/`. | `internal/cli/language_server_test.go:62,88`; `internal/gui/daemons_test.go:29` |
| E2E fixture | exports `test` and `expect` from `internal/gui/e2e/fixtures/hub.ts:29,201`; new helpers like `seedCoexistence` must be added there |

**Discipline rule for the implementer:** before writing any code, grep the relevant symbol from the table above (or read the cited file:line). If a symbol you want to use is NOT in the table, grep first; if it doesn't exist, design it explicitly rather than guessing.

**Event-emission test pattern:** there is NO `api.SetHubMcpEventSink` (don't try to use one — fabricated). The existing pattern reads the hub-mcp log file directly after the action. See `internal/api/hub_mcp_log_redaction_test.go:97-103` for the canonical test pattern: trigger the action, then `os.ReadFile` the hub-mcp.log path and assert the expected `event` string appears.

**Execution order (read this before dispatching tasks):** 1.1 → 1.2 → 1.3 → 2.1 → 2.2 → 2.3 → 1.4 (needs Task 2.3's WriteOverlay) → 2.4 → 2.5 → 2.6.0 → 2.6 → 2.7 → 2.8 → 3.1 → 3.2 → 3.3 → 3.4 (cross-cutting; no own commit) → 3.5 → 4.1 → 4.2 → 4.3 → 4.4 → 5.1 → 5.2. **Task 1.4 is filed under Phase 1 for thematic grouping but EXECUTES after Task 2.3 lands.**

**Pre-commit checklist (every task):** before `git commit`, grep the diff for any new symbol you introduced or any external symbol you called. If grep finds it in the codebase OR it appears in this catalog, OK. If not, you fabricated it — stop and verify or design explicitly. Run (heuristic — catches package-qualified exported calls but NOT same-package calls or new types/fields):

```bash
git diff HEAD -- '*.go' '*.ts' '*.tsx' | grep -E '^\+.*\b(api|cli|gui)\.[A-Z][a-zA-Z]+[(]' | sort -u
```

Notes: scope to source globs to avoid plan-doc false positives; use `[(]` (literal character class) instead of `\(` because BSD grep with `-E` rejects backslash-escaped parens. Then grep each match in the codebase to confirm it exists. The check is heuristic — it misses fabricated fields/types and same-package calls; manual review of new symbols is still required.

---

## File Structure

### NEW files

| Path | Responsibility |
|---|---|
| `internal/api/binary_discovery/discover.go` | `Discover(ctx, requiredBinaries, hints)` hint-walking resolver |
| `internal/api/binary_discovery/hints_windows.go` | `DefaultHints()` for Windows (glob-aware Python3*) |
| `internal/api/binary_discovery/hints_linux.go` | `DefaultHints()` for Linux |
| `internal/api/binary_discovery/hints_darwin.go` | `DefaultHints()` for macOS |
| `internal/api/binary_discovery/discover_test.go` | Unit tests with injected synthetic hints |
| `internal/api/daemon_env_overlay/normalize.go` | `NormalizeOverlayKey(taskName) string` |
| `internal/api/daemon_env_overlay/overlay.go` | `Overlay` struct + `Load(path)` |
| `internal/api/daemon_env_overlay/write.go` | `WriteOverlay(path, mutator)` flock RMW |
| `internal/api/daemon_env_overlay/lookup.go` | `LookupOverlay(ov, taskName) map[string]string` |
| `internal/api/daemon_env_overlay/read_hardening.go` | Platform-neutral read pipeline orchestration |
| `internal/api/daemon_env_overlay/read_hardening_windows.go` | Windows `CreateFileW` + `FILE_FLAG_OPEN_REPARSE_POINT` + attribute check; thin reader over `windows.Handle` |
| `internal/api/daemon_env_overlay/read_hardening_posix.go` | POSIX `O_NOFOLLOW` open |
| `internal/api/daemon_env_overlay/parent_check.go` | `checkStateDirParentReadSafe(dir)` + new env var `MCPHUB_ALLOW_UNHARDENED_STATE_READ` |
| `internal/api/daemon_env_overlay/parent_path_expand.go` | `${parent_path}` token expansion in overlay env values |
| `internal/api/daemon_env_overlay/*_test.go` | Per-file unit tests |
| `internal/api/manifest_lsp_lookup.go` | `ParseEntryName(name, langs) (lang, suffix)` |
| `internal/api/manifest_lsp_lookup_test.go` | parseEntryName tests |
| `internal/cli/overlay_quarantine.go` | `mcphub config overlay-quarantine` (offline CLI) |
| `internal/cli/overlay_quarantine_test.go` | Quarantine command tests |
| `internal/cli/config_cmd.go` | New `mcphub config` parent cobra command (NEW — see Task 2.6.0) |
| `internal/gui/daemon_env_handler.go` | POST `/api/daemon/env` |
| `internal/gui/discovery_refresh_handler.go` | POST `/api/discovery/refresh` |
| `internal/gui/daemon_respawn_handler.go` | POST `/api/daemon/respawn` |
| `internal/gui/daemon_env_handler_test.go` | GUI handler tests |
| `internal/gui/frontend/src/components/EnvDrawer.tsx` | Per-row drawer with env editor + restart button + force checkbox |
| `internal/gui/frontend/src/components/WorkspaceSelector.tsx` | Active-workspace dropdown |
| `internal/gui/e2e/tests/servers-lsp.spec.ts` | 9 LSP rows always present |
| `internal/gui/e2e/tests/servers-env-overlay.spec.ts` | `${parent_path}` warning chip |
| `internal/gui/e2e/tests/servers-coexistence-anomaly.spec.ts` | Dual-badge rendering |
| `internal/gui/e2e/fixtures/lsp-helpers.ts` | `seedCoexistence` + LSP-specific test fixtures |

### MODIFIED files

| Path | Change scope |
|---|---|
| `internal/config/manifest.go:48-91` | Add `RequiredBinaries []string` field to `ServerManifest` AND `LanguageSpec` |
| `servers/mcp-language-server/manifest.yaml` | Add `required_binaries` per language |
| `servers/gdb/manifest.yaml` | Add `required_binaries: [gdb]` at server level |
| `internal/cli/supervise.go:1456-1506` | Extend `mergeDaemonEnv` signature with `overlayEnv`; remove `len(d.Env) > 0` gate; integrate `${parent_path}` expansion |
| `internal/cli/supervise.go:916-934` | Replace `restart`/`reload` UNKNOWN_COMMAND stub with `respawn` handler in `dispatchIPCRequest` switch |
| `internal/cli/supervise.go:512` | Wire `ipcDispatchDeps` to carry overlay + intent + spawn references that respawn needs (extend the struct + populate in supervisor startup) |
| `internal/cli/supervise.go:1498` | Thread overlay-file path into `makeProductionSpawnFnWithStatePath` so the spawn func can do per-daemon overlay lookup |
| `internal/cli/install.go` (or wherever install runs) | Invoke `binary_discovery.Discover` once at install time and seed overlay rows with `source: auto-discovery` |
| `internal/api/types.go:99-106` | Add `LegacyConflict map[string]ClientEntry` field (omitempty) to `ScanEntry` |
| `internal/api/scan.go:240-310` | Wire three-rule LSP recognition pass after existing entry assembly |
| `internal/api/scan.go` (state-file helper exports) | Export `checkStateDirParentWriteSafe` + `operatorRequiresSingleUserHome` (via uppercase rename OR exported shim — implementer chooses; ensure existing callers still compile) |
| `internal/cli/root.go` | Register new `mcphub config` parent command |
| `internal/cli/manifest.go` | (No change; the smoke command in Task 1.2 must use the correct existing `manifest validate <name>` form) |
| `internal/gui/server.go` | Register the three new POST routes under `requireSameOrigin` middleware |
| `internal/gui/frontend/src/types.ts` | Add `legacy_conflict?: Record<string, ClientEntry>` to TS `ScanEntry` type |
| `internal/gui/frontend/src/lib/routing.ts` | Propagate `legacy_conflict` through `collectServers` so the Servers screen receives it |
| `internal/gui/frontend/src/screens/Servers.tsx` | Always render 9 LSP rows (placeholder when empty); integrate `WorkspaceSelector` + `EnvDrawer`; dual-badge cell rendering reading `ClientPresence` + `LegacyConflict` |
| `internal/gui/frontend/src/api.ts` | Add `applyDaemonEnv`, `refreshDiscovery`, `respawnDaemon` client functions |
| `internal/gui/assets/{index.html,app.js,style.css}` | Rebuilt via `npm run build` (committed) |

---

## Pre-flight: Verify baseline

- [ ] **Step 0.1: Verify clean tree on master at v4 spec commit**

Run: `git status && git log --oneline -3`. Expected: clean tree; HEAD is `0b13956` (plan commit) on top of `5295c90` (spec v4).

- [ ] **Step 0.2: Verify backend builds green**

Run: `go build ./... && go vet ./...`. Expected: exit 0, no output.

- [ ] **Step 0.3: Verify the load-bearing citations still hold**

Run:
```bash
grep -n "if len(d.Env) > 0" internal/cli/supervise.go
grep -n "case \"restart\", \"reload\":" internal/cli/supervise.go
grep -n "type SupervisorDaemon struct" internal/api/supervisor_intent.go
grep -n "type ClientEntry struct" internal/api/types.go
```

Expected: respectively at lines 1504, 921, 24, 110 (within a few lines of the plan's claim). If any drifted, the implementer updates the line citation in this plan before the relevant task lands.

---

## Phase 1: Manifest schema + binary_discovery package + install-time wire

### Task 1.1: Add `RequiredBinaries` field to manifest structs

**Files:** `internal/config/manifest.go:48-91` (ServerManifest + LanguageSpec); `internal/config/manifest_test.go`.

**Acceptance criteria:**
1. `ServerManifest` gains an optional `RequiredBinaries []string` field with yaml tag `required_binaries,omitempty`.
2. `LanguageSpec` gains the same optional field.
3. `ParseManifest`'s `KnownFields(true)` strictness (line 128) still rejects truly unknown keys.
4. No `Validate()` logic is added — the field is free-form metadata.

**Test contract:** Two unit tests in `manifest_test.go` exercise YAML round-trip:
- Server-level: a manifest with `required_binaries: [gdb]` parses successfully and the slice contains `"gdb"`.
- Language-level: a manifest with one language declaring `required_binaries: [clangd]` parses successfully and `Languages[0].RequiredBinaries[0] == "clangd"`.

**Done check:** `go test ./internal/config/ -count=1` PASS; `go vet ./...` clean.

**Spec reference:** v4 §"Manifest schema additions"; I-V4-2 (binding manifest YAML to Go struct in same commit per `KnownFields(true)`).

**Implementer notes:** field placement at the END of each struct is fine; preserve existing tags. No mass refactor.

**Commit:** `feat(manifest): add RequiredBinaries field to ServerManifest + LanguageSpec`

---

### Task 1.2: Populate `required_binaries` in shipped manifests

**Files:** `servers/mcp-language-server/manifest.yaml`; `servers/gdb/manifest.yaml`. The `servers/lldb/manifest.yaml` decision is parked per spec v4 open question 2 ("should `lldb` manifest's `command: mcphub` internal bridge declare empty `required_binaries: []`?") — implementer adds an empty array if spec is updated to require it; otherwise leave the file untouched and surface the open question in the commit message.

**Acceptance criteria:**
1. Each of the 9 LSP languages declares `required_binaries: [<the lsp_command value>]` — clangd, fortls, gopls, typescript-language-server (twice — js+ts), pyright-langserver, rust-analyzer, vscode-css-language-server, vscode-html-language-server.
2. `servers/gdb/manifest.yaml` declares `required_binaries: [gdb]` at server level.
3. All existing manifest fields preserved.

**Test contract:** Existing `go test ./internal/config/` still passes — proves YAML still parses cleanly with new fields under `KnownFields(true)`.

**Done check:** `go test ./internal/config/ -count=1` PASS; `go build ./...` clean.

**Spec reference:** v4 §"Manifest schema additions" sample YAML.

**Implementer notes:** YAML loaders are sensitive to indentation; check that the new key sits at the same level as `lsp_command` for language entries.

**Commit:** `feat(manifests): declare required_binaries for LSP daemons + gdb`

---

### Task 1.3: Create `internal/api/binary_discovery/` package

**Files:** `internal/api/binary_discovery/{discover.go, hints_windows.go, hints_linux.go, hints_darwin.go, discover_test.go}`.

**Acceptance criteria:**
1. `Discover(ctx context.Context, requiredBinaries []string, hints []string) (map[string]string, error)` walks hints in order; first hit per binary wins.
2. Missing binaries map to empty string (NOT error — discovery is best-effort).
3. Windows search appends `.exe` to each candidate binary name.
4. `DefaultHints() []string` is per-OS via build tags; Windows uses a glob for Python (`%LOCALAPPDATA%\Programs\Python\Python3*` enumerated via `os.ReadDir` — must guard against directory names shorter than `"Python3"` to avoid slice panic).
5. Env vars (`%USERPROFILE%`, `%LOCALAPPDATA%`, `$HOME`) are expanded inside the function (use `os.ExpandEnv`).
6. `ctx.Err()` checked between binaries to support cancellation.

**Test contract:** Four unit tests with `t.TempDir()` synthetic hint dirs:
- Found-in-first-hint: binary exists in hints[0] → returns abs path.
- Missing: binary nowhere → returns empty string + no error.
- Walks-in-order: only seeded in hints[1] → returns hints[1] path.
- `DefaultHints()` returns non-empty for the current OS.

**Done check:** `go test ./internal/api/binary_discovery/ -count=1 -v` PASS (4 tests); `go vet` clean on all platforms (build tags must resolve).

**Spec reference:** v4 §"DefaultHints — shipped per-OS list"; M-V4-1 (glob, not version-locked literals); E1 from v2 review (hint injection).

**Implementer notes:** Python glob — when ReadDir returns directory names, check `len(name) >= len("Python3")` before slicing. Use `strings.HasPrefix(name, "Python3")` instead of slice comparison for safety.

**Commit:** `feat(binary_discovery): add hint-walking binary resolver`

---

### Task 1.4: Wire `binary_discovery` into `mcphub install` (install-time auto-discovery)

**Files:** Locate the install code path — likely `internal/cli/install.go` or `internal/cli/setup.go`. Grep for `mcphub install` cobra registration to find it.

**Acceptance criteria:**
1. At install/setup time, iterate the loaded manifests and collect every manifest's `RequiredBinaries` (server-level + per-language).
2. Call `binary_discovery.Discover(ctx, allBinaries, binary_discovery.DefaultHints())`.
3. For each discovered binary, write/update an overlay row keyed by the relevant `SupervisorDaemon.TaskName` with `Env["Path"] = <binDir> + string(os.PathListSeparator) + "${parent_path}"` and `Source = "auto-discovery"`, `DiscoveredAt = <RFC3339Nano>`. Use `os.PathListSeparator` so Linux/macOS use `:` and Windows uses `;` — hardcoded `;` would corrupt POSIX paths. (Depends on Task 2.3 `WriteOverlay`.)
4. **Source-preservation:** if an overlay row already has `Source: "operator"`, skip it (compare-and-swap inside the mutator transaction).
5. Emit `binary-discovery-ran` event (info) via `api.LogHubMcpEvent` with fields: `{server, scan_duration_ms, hits_per_binary}`.
6. Emit `binary-discovery-missing` event (warn) for each binary that came back empty.

**Test contract:** Integration test (or smoke if test infrastructure too heavy) that sets up a temp install dir with seeded fake binaries in hints, runs the install function, and asserts the overlay file contains expected `source: auto-discovery` rows.

**Done check:** `go test ./internal/cli/ -count=1 -timeout 5m` PASS including new test. Manual smoke: `go run ./cmd/mcphub install` on a Windows host with MSYS2 → overlay file at `%LOCALAPPDATA%\mcp-local-hub\daemon-env-overrides.yaml` contains gdb path including `C:\msys64\ucrt64\bin`.

**Spec reference:** v4 §"Auto-discovery at install" + §"Observability".

**Implementer notes:** This task **depends on Task 2.3 (WriteOverlay)** — it must land AFTER Phase 2 has the overlay package. Plan-execution order: do Task 1.4 as the LAST task in Phase 1 OR move it to end of Phase 2. Recommendation: defer to right after Task 2.3 commit lands.

**Commit:** `feat(install): run binary_discovery at install + seed overlay`

---

## Phase 2: daemon_env_overlay package + mergeDaemonEnv extension

### Task 2.1: `NormalizeOverlayKey` helper

**Files:** `internal/api/daemon_env_overlay/normalize.go`; `internal/api/daemon_env_overlay/normalize_test.go`.

**Acceptance criteria:**
1. `NormalizeOverlayKey(taskName string) string` prepends `\` if absent. Idempotent. Empty string preserved as empty.
2. Package is named `daemon_env_overlay`.

**Test contract:** Three unit tests:
- Bare form → leading backslash prepended.
- Already canonical → returned unchanged.
- Empty → empty.

**Done check:** `go test ./internal/api/daemon_env_overlay/ -count=1 -v` PASS.

**Spec reference:** v4 §"Canonical daemon key namespace" + I-V4-3.

**Implementer notes:** matches `SupervisorDaemon.TaskName` canonical form per `supervisor_intent.go:25`. Helper is called by every other overlay call site (spawn lookup, GUI write, mutator, orphan detection).

**Commit:** `feat(daemon_env_overlay): add NormalizeOverlayKey canonical-key helper`

---

### Task 2.2: `Overlay` struct + `Load(path)` parser

**Files:** `internal/api/daemon_env_overlay/overlay.go`; `internal/api/daemon_env_overlay/overlay_test.go`. Temporary stub for `hardenedOpen` lives in `read_hardening.go` (Task 2.4 replaces it).

**Acceptance criteria:**
1. `Overlay` struct holds `Version int` and `Daemons map[string]DaemonRow`.
2. `DaemonRow` carries `Env map[string]string`, optional `Source string`, optional `DiscoveredAt string` (RFC3339Nano), optional `ModifiedAt string`.
3. `Load(path string) (*Overlay, error)` reads + parses YAML. Missing file returns empty `Overlay{Version:1, Daemons:{}}` and nil error.
4. 64 KiB size cap enforced; `Mode().IsRegular()` check; reject non-UTF-8.
5. **Comment preservation is required** per spec §"Read-side hardening" + §"`daemon_env_overlay` package". This means the package CANNOT use plain `yaml.Marshal` for writes — it must use the yaml.v3 `Node` API or equivalent to round-trip operator-authored comments. Task 2.3 owns the writer; this task's Load just decodes via struct unmarshal (comments are read-only metadata; preservation matters at WRITE time).
6. `Load` uses `hardenedOpen` (stubbed here to `os.Open`; replaced in Task 2.4 with full reparse-point refusal + parent check).

**Test contract:** Two unit tests:
- Missing file → empty `Overlay`, nil error.
- YAML with one row containing `Env, Source, DiscoveredAt` round-trips into the struct.

**Done check:** `go test ./internal/api/daemon_env_overlay/ -count=1 -v` PASS (Tasks 2.1 + 2.2 combined).

**Spec reference:** v4 §"Env overlay file"; F-V4 codex finding "Comment-preserving YAML claim".

**Implementer notes:** struct-based YAML decode is sufficient for Load (no comment tracking on read). Task 2.3 must use Node-based writes if it intends to preserve operator-edited comments. If true round-trip preservation is too complex, an acceptable middle ground is documented in the spec — but the spec text says comments are preserved; implementer must either deliver or escalate.

**Commit:** `feat(daemon_env_overlay): add Overlay struct + Load() YAML parser`

---

### Task 2.3: `WriteOverlay(path, mutator)` flock-protected RMW

**Files:** `internal/api/daemon_env_overlay/write.go`; `internal/api/daemon_env_overlay/write_test.go`.

**Acceptance criteria:**
1. `WriteOverlay(path string, mutator func(*Overlay) error) error` performs: lock → load → mutate → marshal → atomic write → unlock.
2. **Mutator-error rollback:** if mutator returns non-nil error, NO write to disk; flock released; error propagated verbatim.
3. Routes the write through the existing exported `api.SecureWriteClientConfig(path, payload []byte)` — bytes are YAML output (with comment-preserving Node API per Task 2.2 acceptance criteria 5).
4. Uses `github.com/gofrs/flock` for the lockfile at `<path>.lock`.
5. Concurrent writers serialize on the flock — no lost-update window.

**Test contract:** Three unit tests:
- Atomic create new file: WriteOverlay on missing path → mutator adds row → Load returns the row.
- Mutator-error rollback: seed existing file → mutator returns sentinel error → existing file content unchanged; mutator's in-memory changes NOT persisted; sentinel error propagated.
- Concurrent serialization: 5 goroutines each adding a distinct env key to the same row; after all complete, all 5 keys present (no lost updates).

**Done check:** `go test ./internal/api/daemon_env_overlay/ -count=1 -v` PASS including the 3 new tests; full `go vet ./...` clean.

**Spec reference:** v4 §"Apply env edit from GUI" + I-V4-4 (mutator-error contract).

**Implementer notes:** flock is per-file; lock the path's lockfile, never the data file itself. Atomic rename + handle-bound DACL come from `SecureWriteClientConfig`. The plan v1 commit notes claimed "no external side effects in mutator" — keep that guarantee implicit in the API contract via documentation.

**Commit:** `feat(daemon_env_overlay): add WriteOverlay flock-protected RMW`

---

### Task 2.4: Read-side hardening — Windows reparse-point refusal + parent DACL

**Files:** `internal/api/daemon_env_overlay/{read_hardening.go, read_hardening_windows.go, read_hardening_posix.go, parent_check.go, read_hardening_test.go}`. Also exports symbols from `internal/api/state_file_helper.go`.

**Acceptance criteria:**
1. POSIX `hardenedOpen` uses `os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)`.
2. Windows `hardenedOpen` uses `windows.CreateFile` with `FILE_FLAG_OPEN_REPARSE_POINT | FILE_FLAG_BACKUP_SEMANTICS` SET, then `GetFileInformationByHandle`, then refuses if `bhfi.FileAttributes & windows.FILE_ATTRIBUTE_REPARSE_POINT != 0`. Pattern lives at `hub_mcp_state_dacl_windows.go:85-99,192`.
3. **The Windows path cannot return `*os.File` from a `windows.Handle` via `os.NewFile(uintptr(unsafe.Pointer(&h)), …)` — that ceremony does NOT work.** Two acceptable approaches: (a) `os.NewFile(uintptr(h), path)` IF the Handle's underlying fd is compatible (verify by reading what `hub_mcp_state_dacl_windows.go` does — it uses the Handle directly without converting), OR (b) the function returns a small wrapper type with `Read([]byte) (int, error)` + `Stat() (os.FileInfo, error)` + `Close() error` so the rest of the package doesn't depend on `*os.File`. **The implementer chooses based on what compiles cleanly with the live Windows handle.**
4. New package-private `checkStateDirParentReadSafe(dir string) error` mirrors `checkStateDirParentWriteSafe` semantics (defauilt-relax + strict-mode):
   - If `MCPHUB_REQUIRE_SINGLE_USER_HOME=1` → strict gate, reject non-allowlisted parent → error.
   - Else if `MCPHUB_ALLOW_UNHARDENED_STATE_READ=1` → opt out, skip checks.
   - Else (default) → run parent-write-bits check; reject if write/delete granted to non-allowlisted principal; else log warn `daemon-env-overlay-read-unhardened-fallback` and proceed.
5. `Load` (Task 2.2) wired through: `hardenedOpen` → stat handle → IsRegular check → `checkStateDirParentReadSafe(parentDir)` → size cap → UTF-8 → YAML decode.
6. `internal/api/state_file_helper.go` exports the helpers daemon_env_overlay needs: `OperatorRequiresSingleUserHome()` and `CheckStateDirParentWriteSafe(dir) error`. Either rename the lowercase originals (verify all internal callers compile) or add uppercase shims that delegate. Implementer chooses the lower-blast-radius option.
7. New env var `MCPHUB_ALLOW_UNHARDENED_STATE_READ` registered as a `const` near the existing `AllowUnhardenedStateWriteEnv` in `client_write_init.go:105`.

**Test contract:** Tests in `read_hardening_test.go`:
- Directory at overlay path → `Load` returns error mentioning `not a regular file`.
- POSIX-only test: symlink at overlay path → `Load` returns error mentioning symlink refusal (skipped on Windows).
- `Mode().IsRegular()` rejects directories + named pipes + sockets (POSIX only).
- Default-relax fallback: contrived parent-DACL rejection + no env vars set → succeeds + emits `daemon-env-overlay-read-unhardened-fallback` (assert via reading hub-mcp.log after the Load call per pattern at `internal/api/hub_mcp_log_redaction_test.go:97-103`).
- Strict mode: `MCPHUB_REQUIRE_SINGLE_USER_HOME=1` + same contrived parent → rejects.

**Done check:** `go test ./internal/api/daemon_env_overlay/ -count=1 -timeout 2m` PASS on Windows; `go build ./...` cross-platform clean.

**Spec reference:** v4 §"Read-side hardening" + B-V4-1, B-V4-4.

**Implementer notes:** Windows `os.NewFile(uintptr(h), path)` is the canonical idiom in the Go stdlib for wrapping a raw handle; if it doesn't produce a usable `*os.File` here (the runtime may treat overlapped vs synchronous handles differently), use the wrapper type. Reference `hub_mcp_state_dacl_windows.go` to see how the codebase currently uses raw Handles without converting.

**Commit:** `feat(daemon_env_overlay): add Windows reparse-point refusal + parent DACL`

---

### Task 2.5: `${parent_path}` token expansion

**Files:** `internal/api/daemon_env_overlay/parent_path_expand.go`; `internal/api/daemon_env_overlay/parent_path_expand_test.go`.

**Acceptance criteria:**
1. `ExpandParentPath(env map[string]string, parentEnv []string) (map[string]string, error)` returns a new map where every value's `${parent_path}` literal is replaced by the parent's PATH value (Windows: case-insensitive lookup of PATH/Path/path).
2. Expansion is single-pass, non-recursive (the expanded value is NOT re-scanned for tokens).
3. When the parent has no PATH key, the token expands to empty string (NOT error; the overlay row becomes the only PATH source).
4. Returns error if value contains an unknown `${…}` token (defensive — only `${parent_path}` is supported).
5. Emits `daemon-env-overlay-path-no-parent-token` (info) via `api.LogHubMcpEvent` when an env value's logical PATH key (case-insensitive) does NOT contain `${parent_path}` literal — operator deliberately drops parent.

**Test contract:** Four unit tests:
- Single substitution: `Path = "C:/foo;${parent_path}"` → `"C:/foo;<parent's PATH value>"`.
- No-token case: `Path = "C:/foo"` → unchanged, AND `daemon-env-overlay-path-no-parent-token` event present in hub-mcp.log after the call (read the log per pattern at `hub_mcp_log_redaction_test.go:97-103`).
- Unknown token: `Path = "C:/foo;${unknown}"` → returns error.
- Empty parent PATH: parent has no PATH → expansion returns the original value with the token replaced by empty string.

**Done check:** `go test ./internal/api/daemon_env_overlay/ -count=1 -v` PASS.

**Spec reference:** v4 §"${parent_path} token semantics" + §"Observability".

**Implementer notes:** This is the spec-mandated behavior plan v1 omitted entirely. Without this task, daemon spawns with literal `${parent_path}` in PATH — broken. **Event-emit assertion uses hub-mcp.log read pattern** at `internal/api/hub_mcp_log_redaction_test.go:97-103` (NOT a fabricated event-sink — see catalog discipline rule at top of plan).

**Commit:** `feat(daemon_env_overlay): add ${parent_path} token expansion + no-token event`

---

### Task 2.6.0: `mcphub config` parent cobra command

**Files:** `internal/cli/config_cmd.go`; modify `internal/cli/root.go` to register the new parent.

**Acceptance criteria:**
1. New `newConfigCmd() *cobra.Command` returns a cobra command with `Use: "config"`, `Short` description.
2. The parent is registered on the root cobra command alongside existing subcommands.
3. Empty body — child commands added by Task 2.6.

**Test contract:** Smoke: `go run ./cmd/mcphub config --help` exits 0 and lists subcommands (initially empty until Task 2.6 adds quarantine).

**Done check:** `go build ./... && go vet ./...` clean.

**Spec reference:** v4 §"`mcphub config overlay-quarantine` (offline CLI)" implicitly assumes the parent exists.

**Implementer notes:** Grep `newSomeCmd` patterns in `internal/cli/` to find the existing cobra conventions (registration on root, help text style, error returns).

**Commit:** `feat(cli): add 'mcphub config' parent cobra command`

---

### Task 2.6: `mcphub config overlay-quarantine` offline CLI

**Files:** `internal/cli/overlay_quarantine.go`; `internal/cli/overlay_quarantine_test.go`. Wires to `newConfigCmd()` from Task 2.6.0.

**Acceptance criteria:**
1. Subcommand `overlay-quarantine` registered under `mcphub config`.
2. Acquires the same `<path>.lock` flock as `WriteOverlay`.
3. Missing overlay file → exit 0 with message "no overlay to quarantine".
4. Otherwise `os.Rename(path, path+".corrupt-"+RFC3339-UTC)`.
5. After rename, scan parent dir for files matching `<base>.corrupt-*`; retain 5 newest by mtime; delete older (failures logged warn, non-fatal).
6. Prints operator guidance pointing at `mcphub restart` / cold start.
7. **Does NOT signal or restart the supervisor** — purely file operations + flock. Operator restarts manually.
8. Resolves overlay path via `stateDirFunc()` (real symbol, not `stateDirPath()`).

**Test contract:** Two unit tests (table from Task 2.6 v1 + retention test):
- Renames file: seed overlay, run command, assert original gone + `.corrupt-<ts>` exists.
- Missing file is no-op: run command on empty dir, exit code 0, no panic.
- 5-newest retention: seed 7 `.corrupt-*` files with sequential timestamps, run command, assert 5 newest survive + 2 oldest gone.

**Done check:** `go test ./internal/cli/ -run TestOverlayQuarantine -count=1 -v` PASS; smoke `go run ./cmd/mcphub config overlay-quarantine --help` shows usage.

**Spec reference:** v4 §"`mcphub config overlay-quarantine` (offline CLI)" + M-V4-2 (5-newest retention).

**Implementer notes:** retention uses `os.ReadDir` + mtime sort; ignore non-`.corrupt-*` files.

**Commit:** `feat(cli): add 'mcphub config overlay-quarantine' offline command`

---

### Task 2.7: Extend `mergeDaemonEnv` + remove spawn gate

**Files:** `internal/cli/supervise.go:1456-1506`; `internal/cli/supervise_test.go`.

**Acceptance criteria:**
1. `mergeDaemonEnv` signature becomes `func mergeDaemonEnv(parent []string, manifest, overlay map[string]string) []string` (3 args).
2. Precedence: parent < manifest < overlay (later overrides earlier).
3. Windows case-insensitive PATH normalization: `PATH`/`Path`/`path` collide; the highest-precedence one wins; output preserves the casing of the highest-precedence source.
4. Spawn gate at lines 1504-1506 (`if len(d.Env) > 0 { … }`) REMOVED. The new code unconditionally calls `mergeDaemonEnv(os.Environ(), d.Env, overlay)` — but **`mergeDaemonEnv` returns nil when BOTH `manifest` and `overlay` are nil/empty** (the parent-only case). The spawn code then leaves `cmd.Env` nil → child inherits `os.Environ()` directly. This preserves existing behavior for env-less daemons. Concretely: the function's first check is `if len(manifest) == 0 && len(overlay) == 0 { return nil }`; only when at least one is non-empty does the merge produce a slice.
5. ALL existing callers of the old 2-arg `mergeDaemonEnv` are updated — including any tests (e.g., `supervise_reconcile_wiring_test.go:766`). Implementer greps `mergeDaemonEnv(` to find every call site.
6. Overlay argument is passed nil for now; Task 2.8 wires the real overlay load. The 3-arg signature is the upgrade scaffold.

**Test contract:** Three unit tests:
- Overlay wins over manifest: parent `PATH=/system`, manifest `Path=/manifest`, overlay `Path=/overlay` → result contains `Path=/overlay` (case from overlay).
- Both empty manifest+overlay (regardless of parent): `mergeDaemonEnv(parent, nil, nil)` returns nil — caller leaves cmd.Env nil so child inherits parent env via the standard `os/exec` default. **NOT** "returns parent unchanged".
- Windows case-insensitive (skipped on POSIX): parent `PATH=/parent`, manifest `path=/manifest`, overlay `Path=/overlay` → exactly ONE Path-family entry in result with overlay's value.

**Done check:** `go test ./internal/cli/ -count=1 -timeout 5m` PASS (no broken callers); `go build ./...` clean.

**Spec reference:** v4 §"Spawn-time env merge" + B-V4-1 (gate removal); I1 (Win case-insensitive merge).

**Implementer notes:** the existing function uses `sort.Strings(keys)` for determinism — preserve that. For Windows case-folding, normalize keys via `strings.ToUpper` for the lookup map but emit the original casing on output. Compile error from missing test caller updates is the discipline — fix ALL of them, don't comment out tests.

**Commit:** `refactor(supervise): extend mergeDaemonEnv with overlay arg; remove spawn gate`

---

### Task 2.8: Wire `daemon_env_overlay.Load` into supervisor spawn

**Files:** `internal/cli/supervise.go` (around `makeProductionSpawnFnWithStatePath` at line 1498 + supervisor startup); `internal/cli/supervise_test.go`.

**Acceptance criteria:**
1. Supervisor startup calls `daemon_env_overlay.Load(<state-dir>/daemon-env-overrides.yaml)` ONCE; if it returns non-nil error, supervisor aborts startup with a message including `mcphub config overlay-quarantine` (fail-LOUD per spec).
2. Loaded overlay is cached in `ipcDispatchDeps` (extend the struct — add `overlay *daemon_env_overlay.Overlay`) AND passed to `makeProductionSpawnFnWithStatePath` via a new parameter.
3. The spawn function (around line 1499) looks up the per-task env via `daemon_env_overlay.LookupOverlay(overlay, d.TaskName)` (new helper — see below) and passes the result as the third arg to `mergeDaemonEnv`.
4. Apply `${parent_path}` expansion (Task 2.5) on the overlay env BEFORE the merge.
5. Emit `daemon-env-overlay-loaded` (info) at startup with row count + `sha256(parent_path)[:12]` of resolved parent PATH (debug level emits full PATH).
6. **Orphan detection at startup:** after Load succeeds, iterate the overlay's `Daemons` map and for every key NOT present (post-normalize) in `supervisor-intent.json`'s daemon list, emit `daemon-env-overlay-orphan-row` (warn) with `{task_name}`. Spec §"Error handling" mandates this signal so operators can run `mcphub config prune-orphan-overlay-rows` (Task 5.1).

**Test contract:** Three tests:
- Supervisor startup fails-LOUD on corrupt overlay: seed `daemon-env-overrides.yaml` with invalid YAML → startup returns error → error message contains "overlay-quarantine".
- Successful spawn merges overlay env: seed overlay with row for taskName T + Path → mock spawn captures cmd.Env → assert Path includes overlay value.
- Empty overlay: missing file → startup succeeds → spawn uses manifest-only env.

**Done check:** `go test ./internal/cli/ -count=1 -timeout 5m` PASS; build clean.

**Spec reference:** v4 §"Spawn-time env merge" + §"Error handling" (fail-LOUD whole-overlay) + I-V4-5.

**Implementer notes:** `LookupOverlay(ov *Overlay, taskName string) map[string]string` is a thin helper — add to `daemon_env_overlay/lookup.go`. Normalize `taskName` via `NormalizeOverlayKey` before lookup. **Per-spawn re-read is NOT the model** — the supervisor loads overlay once at startup; the `respawn` IPC command (Task 4.1) re-loads it before spawning the affected daemon. Document this explicitly.

**Commit:** `feat(supervise): wire daemon_env_overlay into spawn + fail-LOUD on parse error`

---

## Phase 3: scan.go three-rule recognition + ScanEntry.LegacyConflict

### Task 3.1: `ScanEntry.LegacyConflict` field + JSON round-trip

**Files:** `internal/api/types.go:99-106`; `internal/api/types_test.go` (create if absent).

**Acceptance criteria:**
1. New field `LegacyConflict map[string]ClientEntry json:"legacy_conflict,omitempty"` on `ScanEntry`.
2. Other field ordering / tag formats unchanged.
3. `omitempty` ensures backwards compat — empty/nil field absent from JSON.

**Test contract:** Two unit tests:
- Omitempty: marshal empty `ScanEntry{ClientPresence: {}}` → output does NOT contain `legacy_conflict`.
- Round-trip populated: marshal `ScanEntry{LegacyConflict: {codex-cli: stdio entry}}` → unmarshal → field preserved.

**Done check:** `go test ./internal/api/ -count=1` PASS including new tests; build clean.

**Spec reference:** v4 §"Schema change: ScanEntry.LegacyConflict" + I-V4-2.

**Implementer notes:** test client key is `codex-cli` (real key from scan.go), NOT `codex`.

**Commit:** `feat(scan): add ScanEntry.LegacyConflict side-channel field`

---

### Task 3.2: `ParseEntryName` helper

**Files:** `internal/api/manifest_lsp_lookup.go`; `internal/api/manifest_lsp_lookup_test.go`.

**Acceptance criteria:**
1. `ParseEntryName(entryName string, langs []string) (lang, suffix string)` matches `mcp-language-server-<lang>(-<suffix>)?` where `<lang>` is the LONGEST-matching language prefix from `langs` (handles `vscode-css` and `vscode-html` correctly).
2. Returns empty strings for non-LSP entries OR entries whose suffix doesn't match a known language.

**Test contract:** Six unit tests:
- Plain base: `mcp-language-server-clangd` → `(clangd, "")`.
- Short suffix: `mcp-language-server-rust-a1b2` → `(rust, "a1b2")`.
- Full suffix: `mcp-language-server-typescript-deadbeef` → `(typescript, "deadbeef")`.
- Non-LSP: `some-other-server` → `("", "")`.
- Hyphenated language exact: `mcp-language-server-vscode-html` → `(vscode-html, "")`.
- Hyphenated with suffix: `mcp-language-server-vscode-css-abcd` → `(vscode-css, "abcd")`.

**Done check:** `go test ./internal/api/ -run TestParseEntryName -count=1 -v` PASS.

**Spec reference:** v4 §"Matrix LSP recognition" Step A.

**Implementer notes:** longest-prefix match prevents `vscode-css` being parsed as `(vscode, css)`. Sort `langs` by length descending before matching, or iterate twice.

**Commit:** `feat(api): add ParseEntryName helper for LSP entry-name parsing`

---

### Task 3.3: Three-rule LSP recognition in `scan.go`

**Files:** `internal/api/scan.go` (extend entry-assembly with a classification pass); `internal/api/scan_test.go` (recognition fixtures + helpers).

**Acceptance criteria:**
1. New `classifyLSPEntries(entries map[string]*ScanEntry, reg *Registry)` function runs after the existing entry-assembly loop.
2. For each (entryName, entry) and each (clientName, clientEntry) in ClientPresence:
   - Run `ParseEntryName` — skip non-LSP.
   - **Rule 1 (hub-managed):** `clientEntry.Transport == "http"` → mark `Status = "via-hub"` on the row.
   - **Rule 2 (direct-stdio mcp-language-server):** `clientEntry.Transport == "stdio"` AND `clientEntry.Raw["command"]` basename (post-`.exe`-strip) is `mcp-language-server` → recognize as legacy.
   - **Rule 3 (gopls):** stdio + `gopls` command + `Raw["args"]` contains `"mcp"` → recognize as Go legacy.
3. When a legacy stdio entry exists AND a separate hub row exists for the SAME (clientName, language) pair, MOVE the stdio entry from its own row's ClientPresence to the hub row's `LegacyConflict[clientName]` and remove the dangling row.
4. Ownership disambiguation via registry: when multiple workspaces register the same language, the suffix in the entry name (`-<4hex>` or `-<8hex>`) is **NOT a registry key** — instead, walk `reg.Workspaces` and match `ws.ClientEntries[clientName] == entryName` exactly (reverse lookup).
5. Wired into `(*API).ScanFrom` — runs after existing entry assembly. **Inside `internal/api/scan.go`, drop the `api.` package prefix** (same-package call), so the registry-load pattern is: `regPath, err := DefaultRegistryPath()` (returns `(string, error)`); if err is nil, `reg, err := NewRegistry(regPath).Load()`. **Cannot inline as `NewRegistry(DefaultRegistryPath()).Load()` — that's a Go compile error because `DefaultRegistryPath` returns two values.** Nil-registry (any step errored or no registrations) → fallback degrades gracefully: recognition still labels language, just no ownership attribution.
6. Existing scan tests still PASS — recognition is additive (Status field is populated; ClientPresence is preserved unless coexistence collapses two rows into one).

**Test contract:** Two new tests in `scan_test.go` (plus helpers as needed):
- Hub-managed clangd recognized: seed workspace registry with one clangd registration in codex-cli + seed codex config with `mcp-language-server-clangd` → URL entry → scan returns entry with `Status == "via-hub"`, `ClientPresence["codex-cli"].Transport == "http"`, `LegacyConflict == nil`.
- Coexistence anomaly: seed both `mcp-language-server-rust` → URL hub entry AND `rust-langserver-direct` → stdio with `command=mcp-language-server, args=["--lsp", "rust-analyzer", "--workspace", "/proj/main"]` → after scan, hub row has `LegacyConflict["codex-cli"].Transport == "stdio"`.

**Helpers added:** `seedRegistry(t, tmpHome, []WorkspaceEntry)` writes a `workspaces.yaml`; `seedCodexConfig(t, tmpHome, mcpServers)` writes a `~/.codex/config.toml`. Use the existing `seedClaudeLS` / `seedCodexLS` patterns at `language_server_test.go:62,88` as models. Test client key is `codex-cli` per `scan.go:476`.

**Done check:** `go test ./internal/api/ -count=1 -timeout 5m` PASS; build clean; `go vet ./...` clean.

**Spec reference:** v4 §"Matrix LSP recognition" (full three-rule algorithm).

**Implementer notes:** existing `scan_test.go` already loads codex configs — read it as the testing pattern before designing new helpers. `(*API).ScanFrom(api.ScanOpts)` is the public entry; the new classify pass runs from inside the existing ScanFrom code path.

**Commit:** `feat(scan): three-rule LSP recognition + LegacyConflict population`

---

### Task 3.4: Observability events for overlay lifecycle

**Files:** `internal/api/daemon_env_overlay/overlay.go` (Load); `internal/api/daemon_env_overlay/write.go` (mutator outcomes); `internal/cli/supervise.go` (startup + spawn).

**Acceptance criteria:**
Emit the following events via `api.LogHubMcpEvent(level, event, fields)`:

| Event name | Level | Where | Fields |
|---|---|---|---|
| `daemon-env-overlay-loaded` | info | supervisor startup post-Load | `{row_count, parent_path_hash}` |
| `daemon-env-overlay-load-failed` | warn | Load failure path | `{path, error_class, line, col}` |
| `daemon-env-overlay-read-rejected` | error | hardened-read refusal (symlink/reparse/size/owner) | `{path, reason}` |
| `daemon-env-overlay-read-unhardened-fallback` | warn | parent-DACL default-relax | `{path}` |
| `daemon-env-overlay-applied-via-gui` | info | `/api/daemon/env` handler success | `{task_name, changed_keys}` (values redacted) |
| `daemon-env-overlay-orphan-row` | warn | startup load detects taskName not in intent | `{task_name}` |
| `daemon-env-overlay-skipped-operator-override` | info | auto-discovery skipped due to source:operator | `{task_name, binary}` |
| `daemon-env-overlay-parent-path-resolve-failed` | warn | per-row `${parent_path}` resolve error | `{task_name, key, error}` |
| `daemon-env-overlay-path-no-parent-token` | info | spawn observed PATH lacks token | `{task_name}` |
| `binary-discovery-ran` | info | install auto-discovery completion | `{server, scan_duration_ms, hits_per_binary}` |
| `binary-discovery-missing` | warn | discovery returned empty for a binary | `{server, binary, scanned_hints}` |
| `supervisor-respawn-via-gui` | info | respawn IPC success | `{task_name, force, requesting_client, outcome}` (per spec §"Observability") |
| `supervisor-respawn-graceful-timeout` | warn | soft-shutdown deadline exceeded | `{task_name}` |
| `supervisor-respawn-refused-quarantined` | info | respawn refused without force | `{task_name}` |

(Per spec v4 §"Observability".)

**Test contract:** Each task above that emits an event MUST add a unit test verifying the event fires (use whatever event-sink mock pattern exists; grep for `LogHubMcpEvent` in existing tests to find the pattern). Total: ~7 new event-fire assertions woven into the relevant tasks' tests.

**Done check:** `go test ./... -count=1 -timeout 5m` PASS. Manual: `mcphub status` (or however hub-mcp log is viewed) shows new events.

**Spec reference:** v4 §"Observability" exhaustive list.

**Implementer notes:** This task is **distributed across the implementing tasks** — each task that performs an action MUST emit its spec-defined event(s). The table here is the contract for what must exist by end of Phase 3. The implementer adds the `LogHubMcpEvent` call inline in each task's code change.

**Commit:** This task's check is verifying the events landed during Tasks 2.2, 2.3, 2.4, 2.5, 2.8, 3.3, 4.1. If any are missing after those tasks, a small follow-up commit `feat(observability): backfill missing overlay events` closes the gap.

---

### Task 3.5: Frontend TS type + routing update for `LegacyConflict`

**Files:** `internal/gui/frontend/src/types.ts` — TWO changes here: (a) add `legacy_conflict?: Record<string, ClientEntry>` to the `ScanEntry` TS interface; (b) extend the `ServerRow` interface (lives at `types.ts:98-105`, NOT `routing.ts`) with `legacyConflict?: Record<string, ClientEntry>` camelCase. PLUS `internal/gui/frontend/src/lib/routing.ts:130-137` for the propagation in `collectServers` (NO type definition there — only the helper that fills the new field on rows).

**Acceptance criteria:**
1. `ScanEntry` TypeScript type matches the Go shape including the new optional field.
2. **`ServerRow` type extended** in `routing.ts` with `legacyConflict?: Record<string, ClientEntry>` (camelCase per TS conventions; matches the snake_case JSON via the propagation helper).
3. `collectServers` propagates `e.legacy_conflict` → `row.legacyConflict` on the produced `ServerRow`.
4. No frontend behavior change yet — Task 4.3 consumes the field for dual-badge rendering. This task is the type-system bridge.

**Test contract:** Existing frontend type-check (`npm run typecheck`) PASS; if there's a `routing.test.ts`, add an assertion that `legacy_conflict` survives a round-trip through `collectServers`.

**Done check:** `cd internal/gui/frontend && npm run typecheck && npm run test` clean.

**Spec reference:** v4 §"Schema change" — both backend and frontend halves needed.

**Implementer notes:** read the existing `ScanEntry` TS type before editing; preserve all other fields exactly. `routing.ts` is the routing-lib helper used by `Servers.tsx`.

**Commit:** `feat(frontend): add legacy_conflict to ScanEntry TS type + routing propagation`

---

## Phase 4: GUI surface — IPC + endpoints + Servers.tsx + e2e

### Task 4.1: `respawn` IPC command

**Files:** `internal/cli/supervise.go` (replace UNKNOWN_COMMAND case at line 916-934 with `handleRespawn`; extend `ipcDispatchDeps` if needed for intent/spawn access); `internal/cli/supervise_respawn_test.go`.

**Acceptance criteria:**
1. New case `"respawn":` in `dispatchIPCRequest` switch (line 856).
2. New `handleRespawn(conn net.Conn, req api.IPCRequest, deps ipcDispatchDeps) error` follows the pattern of existing `handleQuiesceTimers` (line 1009) and `handleExit` (line 1092).
3. Request args: `req.Args` map containing `task_name` (string) and `force` (bool). **Note: it's `.Args`, not `.Body`** — verify against `internal/api/supervisor_ipc.go:12-17`.
4. Response: `req.ID` echoed, `Result` map set on success, `Error` set on failure, `Final: true`.
5. **Behavior:** look up `task_name` (normalized via `NormalizeOverlayKey`) in current `supervisor-intent.json` — if absent, error `UNKNOWN_TASK`. If daemon is in "quarantine" state AND `force == false`, error `QUARANTINED`. Otherwise perform graceful 5s shutdown → if timeout, force-kill via Job-Object — then spawn with current intent+overlay.
6. Emits events: `supervisor-respawn-via-gui` (info), `supervisor-respawn-graceful-timeout` (warn on soft-timeout), `supervisor-respawn-refused-quarantined` (info on Quarantine refusal).
7. **`ipcDispatchDeps` MUST be extended** (prescriptive — not optional). The struct at `internal/cli/supervise.go:257-270` currently has `{stateDir, events, runtimeTracker, reconcileReady, intentFilesLoaded, gracefulInProgress, triggerGracefulExit}`. Add these new fields in a small commit BEFORE this task lands (or fold into this task as its first step):
   - `intent *api.SupervisorIntentFile` — current parsed intent (re-loaded on each respawn or held by reference; choose based on how mutation works in the existing reconcile loop).
   - `respawnDaemon func(taskName string, force bool) error` — closure that the supervisor startup wires to invoke the production spawn pipeline (graceful kill + spawn-with-overlay). Lives next to existing `triggerGracefulExit` closure pattern.
   - `daemonState func(taskName string) string` — reader-only helper returning the current state ("running"/"quarantine"/etc.) from `runtimeTracker`. Avoids exporting the tracker's internals.
   Wire location: `ipcDispatchDeps` is constructed at `supervise.go:512`; the production `spawnFn`/`terminateFn` closures are created later at `supervise.go:578-584`. The implementer's choices: (a) defer `ipcDispatchDeps` construction past line 584 so the closures can be captured by reference at deps-build time, OR (b) keep deps construction at :512 but use a late-binding sync.Once / closure-set-via-method pattern. (a) is simpler; (b) preserves the existing line ordering. Either works; pick (a) unless line-order preservation is required.

**Test contract:** Three integration tests (use existing IPC dial pattern from `supervisor_ipc_status_client.go` as template):
- Valid task: dial supervisor, send `respawn` with valid `task_name` and `force=false` → no error response.
- Unknown task: dial, send respawn with bogus task → error code `UNKNOWN_TASK`.
- Quarantined refusal: pre-mark daemon as quarantined in tracker, send respawn force=false → error code `QUARANTINED`.

**Done check:** `go test ./internal/cli/ -run TestRespawn -count=1 -v -timeout 5m` PASS.

**Spec reference:** v4 §"Respawn from GUI" + §"Observability".

**Implementer notes:** the dispatcher switch is at `dispatchIPCRequest` around line 856; the `case "restart", "reload":` UNKNOWN_COMMAND block at line 916 is what's being replaced (drop those case labels — they're stubs). For graceful shutdown, look at how `handleExit` (line 1092) performs its termination — likely there's a `terminator` or `gracefulInProgress` flag in `ipcDispatchDeps`. The respawn handler reuses that pattern for the per-daemon kill.

**Commit:** `feat(supervise): add respawn IPC command (replaces UNKNOWN_COMMAND stub)`

---

### Task 4.2: GUI handlers `/api/daemon/env`, `/api/discovery/refresh`, `/api/daemon/respawn`

**Files:** `internal/gui/{daemon_env_handler,discovery_refresh_handler,daemon_respawn_handler}.go`; `internal/gui/daemon_env_handler_test.go`; `internal/gui/server.go` (route registration).

**Acceptance criteria:**
1. Three POST routes registered on the existing mux, wrapped in `(*Server).requireSameOrigin` middleware.
2. **No CSRF token** — the codebase's CSRF defense is `requireAllowedHost` (outer, all routes) + `requireSameOrigin` (mutating routes). Match the existing posture; do NOT invent token headers.
3. **`/api/daemon/env`:**
   - Body: `{task_name: string, env: {KEY: value, ...}}`.
   - Validates: keys match `[A-Za-z_][A-Za-z0-9_]*`; values reject newline/NUL/control chars.
   - Known-task validation: `NormalizeOverlayKey(task_name)` MUST be in current supervisor-intent.json — else HTTP 400 with code `UNKNOWN_TASK`.
   - Calls `daemon_env_overlay.WriteOverlay` with mutator that sets `Env`, `Source = "operator"`, `ModifiedAt = now`.
   - Emits `daemon-env-overlay-applied-via-gui` event with redacted values.
   - Returns 200 + effective env (after merge with manifest).
4. **`/api/discovery/refresh`:**
   - Body: `{server: string, daemon: string}` — Phase 4 implements `daemon: "all"` only; per-daemon is a follow-up.
   - Gathers required_binaries from manifests, runs `binary_discovery.Discover`, writes results to overlay via `WriteOverlay` (CAS-preserves operator rows).
   - Returns 200 + found-paths map.
5. **`/api/daemon/respawn`:**
   - Body: `{task_name: string, force: bool}`.
   - Dials supervisor IPC (use the existing pipe-dial pattern from `DialSupervisorIPCStatus`).
   - Maps IPC `QUARANTINED` error → HTTP 409 with body `{state: "quarantined", remedy: "force or unquarantine"}`.
   - Maps `UNKNOWN_TASK` → HTTP 400.
   - Returns 200 on success.
6. Handlers use real codebase symbols:
   - State dir: `stateDirFunc()` if accessible from gui package, else accept it via `Server` field — verify what's already wired.
   - Event log: `api.LogHubMcpEvent(level, event, fields)`.
   - Known-task lookup: read current `supervisor-intent.json` via `api.ReadSupervisorIntent` (verify the function name; grep).
   - IPC dial: existing `Dial*IPC*` clients — NO `api.CallSupervisorIPC` (doesn't exist); the implementer either uses an existing dialer or adds a new `DialSupervisorIPCRespawn` next to its siblings in `internal/api/`.

**Test contract:** Tests in `daemon_env_handler_test.go` (also covers patterns for the other two handlers):
- `requireSameOrigin` rejection: POST with `Origin: https://evil.com` → 403.
- Unknown task: POST with bogus task_name → 400 `UNKNOWN_TASK`.
- Successful write: seed supervisor-intent.json with one daemon, POST valid request → 200 + overlay file contains the row.
- Invalid env key: POST with key `bad-key` (hyphen) → 400 `INVALID_KEY`.
- Invalid env value: POST with value containing `\n` → 400 `INVALID_VALUE`.

**Helpers to add (in test file):** `newTestGUIServer(t)` creates a GUI server with a temp state dir; `seedTestIntent(t, server, taskNames)` writes a supervisor-intent.json. Pattern: read existing GUI tests in `internal/gui/*_test.go` to find the helper conventions.

**Done check:** `go test ./internal/gui/ -count=1 -timeout 5m` PASS; build clean.

**Spec reference:** v4 §"Apply env edit from GUI", §"Manual discovery refresh", §"Respawn from GUI", §"`/api/daemon/env`, ... auth posture".

**Implementer notes:** the `requireSameOrigin` returns `http.HandlerFunc` from `http.HandlerFunc` — wrap each handler the same way the existing migrate/secrets routes do. Grep `requireSameOrigin` callers for the registration pattern. The IPC dial pattern: each of the existing `Dial*IPC*` clients opens the pipe, writes a JSON-framed request, reads response — copy that pattern for respawn.

**Commit:** `feat(gui): add daemon-env + discovery-refresh + respawn POST endpoints`

---

### Task 4.3: Frontend `WorkspaceSelector` + `EnvDrawer` components + Servers.tsx integration

**Files:** `internal/gui/frontend/src/components/WorkspaceSelector.tsx` (new); `internal/gui/frontend/src/components/EnvDrawer.tsx` (new); `internal/gui/frontend/src/api.ts` (add 3 client functions); `internal/gui/frontend/src/screens/Servers.tsx` (integrate selector + drawer + 9-row scaffold + dual-badge rendering); `internal/gui/frontend/src/screens/Servers.css` (or wherever existing styles live).

**Acceptance criteria:**
1. `api.ts` adds `applyDaemonEnv(taskName, env)`, `refreshDiscovery(server?, daemon?)`, `respawnDaemon(taskName, force?)` — all POST with proper Content-Type, error-throwing on non-2xx, 409 returns a typed `QuarantinedError` rather than throwing.
2. `WorkspaceSelector.tsx` renders a `<select>` of registered workspaces (from `/api/registry` — implementer reads the existing registry endpoint or adds it). Empty registry → renders a "(none — register a workspace first)" placeholder span.
3. `EnvDrawer.tsx` per-row panel:
   - Path textarea with current effective value.
   - Warning chip "PATH does not include `${parent_path}` — parent PATH will be DROPPED for this daemon" when textarea content does NOT include the literal `${parent_path}` substring.
   - Apply button → `applyDaemonEnv`.
   - Force restart checkbox + Restart button → `respawnDaemon`.
   - Surfaces errors inline (especially the Quarantined-error case).
4. `Servers.tsx` integration:
   - Renders the workspace selector above the existing matrix table.
   - Always renders exactly 9 LSP language rows (one per manifest language). When no entries match, render placeholder cells with the "Initialize" / "Register" affordance the existing matrix uses for manifest-only rows.
   - Per-cell rendering: when `legacy_conflict[client]` is also set, render BOTH `[via-hub]` and `[legacy]` chips stacked.
   - Clicking a row opens the `EnvDrawer` for that taskName.
5. Existing matrix functionality preserved: existing global-server rows still work; dirty-state semantics unchanged; Apply path for existing servers unchanged.

**Test contract:**
- Frontend unit tests in `internal/gui/frontend/src/components/*.test.ts` for `WorkspaceSelector` (renders options from workspaces array; renders placeholder on empty) and `EnvDrawer` (warning chip toggles based on textarea content; Apply triggers fetch).
- `npm run typecheck` clean.
- Existing Servers screen e2e tests still pass.

**Done check:** `cd internal/gui/frontend && npm run typecheck && npm run test && npm run build` clean; `go generate ./internal/gui/...` (which runs the build) updates `internal/gui/assets/*` and the changes are committed.

**Spec reference:** v4 §"Matrix LSP recognition", §"${parent_path} token semantics" (warning chip), §"Apply env edit from GUI", §"Respawn from GUI".

**Implementer notes:** the existing `Servers.tsx` is ~760 lines with `DirtyMap`, `Direction`, `aggregateStatus`, etc. **Do NOT bolt new components in by reaching into private state**. Read the existing file's component structure first; identify the per-row render hook; integrate `EnvDrawer` at that level. The LSP-row scaffold may need a new helper that synthesizes 9 placeholder `ScanEntry` values when corresponding `ScanResult.Entries` are absent — that helper goes in `lib/routing.ts` or similar. Per CLAUDE.md "Surgical-edit consistency" memory: after editing `Servers.tsx`, grep for `Servers` references across the codebase to make sure props / types match.

**Commit:** `feat(frontend): add WorkspaceSelector + EnvDrawer + 9-LSP-row scaffold to Servers.tsx`

---

### Task 4.4: Playwright e2e specs

**Files:** `internal/gui/e2e/tests/{servers-lsp,servers-env-overlay,servers-coexistence-anomaly}.spec.ts`; `internal/gui/e2e/fixtures/lsp-helpers.ts` (new — `seedCoexistence` helper).

**Acceptance criteria:**
1. `servers-lsp.spec.ts`: spawns GUI via existing `startHub` fixture (verify exact export — likely `test, expect` re-exported), navigates to `#/servers`, asserts all 9 LSP rows are visible (use stable `[data-testid="lsp-row-<lang>"]` selectors added in Task 4.3).
2. `servers-env-overlay.spec.ts`: opens a row's drawer, fills PATH WITHOUT `${parent_path}` token, asserts warning chip visible; fills WITH token, asserts chip hidden.
3. `servers-coexistence-anomaly.spec.ts`: uses `seedCoexistence` to write both hub URL entry AND direct-stdio entry to the test codex config; asserts the cell renders both badges.
4. `lsp-helpers.ts` exports `seedCoexistence(hub, client, language)` — writes the dual-entry fixture into the hub's test home dir (mirrors existing fixture functions in `e2e/fixtures/hub.ts`).

**Test contract:** All three specs PASS on a Windows runner (per CLAUDE.md `windows-latest` requirement).

**Done check:** `cd internal/gui/e2e && npm test` PASS.

**Spec reference:** v4 §"Testing strategy" → "Multi-workspace LSP recognition test" + "E2E (Playwright)".

**Implementer notes:** Playwright fixtures live in `internal/gui/e2e/fixtures/hub.ts`. Read the existing exports (`startHub` may not be the exact name) before importing. Add `seedCoexistence` next to the existing seed helpers using the same file-write conventions.

**Commit:** `test(e2e): add Playwright specs for LSP rows + env drawer + coexistence`

---

## Phase 5: Cleanup & final verification

### Task 5.1: `mcphub config prune-orphan-overlay-rows` command

**Files:** `internal/cli/overlay_prune_orphans.go`; `internal/cli/overlay_prune_orphans_test.go`. Wires to `newConfigCmd` from Task 2.6.0.

**Acceptance criteria:**
1. New subcommand `prune-orphan-overlay-rows` under `mcphub config`.
2. Loads current supervisor-intent.json + overlay file.
3. For each overlay row whose taskName is NOT in intent.Daemons (after `NormalizeOverlayKey` on both sides), removes the row.
4. Uses `daemon_env_overlay.WriteOverlay` (flock-protected RMW).
5. Reports the number of rows removed.

**Test contract:** Two unit tests:
- Removes orphans: seed intent with 1 daemon, overlay with 3 rows (1 matching, 2 orphan) → run → overlay has 1 row remaining.
- No-op when clean: all rows match intent → no removal.

**Done check:** `go test ./internal/cli/ -run TestPruneOrphan -count=1 -v` PASS.

**Spec reference:** v4 §"Migration" — "`mcphub config prune-orphan-overlay-rows` removes them".

**Implementer notes:** triggered manually by the operator; not automatic. The GUI surfaces "Clean up orphan overlay rows" affordance in a future iteration (out of scope here).

**Commit:** `feat(cli): add 'mcphub config prune-orphan-overlay-rows'`

---

### Task 5.2: Final whole-branch verification

**Bot-review economics note:** push at PHASE BOUNDARIES (after each phase commits, NOT after each task), so the bot sees ~5 batched pushes instead of 20+ per-task pushes. Per CLAUDE.md PR workflow Step 4, every push triggers fresh bot review; per-task pushes blow through the bot quota. Run `go build ./... && go vet ./... && go test ./...` locally per CLAUDE.md Step 1 before each phase boundary push.

- [ ] **Step 5.2.1: Full backend test suite + build**

Run: `go build ./... && go vet ./... && go test ./... -count=1 -timeout 5m`. Expected: clean.

- [ ] **Step 5.2.1a: Fabrication grep (final scan — heuristic)**

Run, scoped to source globs only:

```bash
git diff origin/master..HEAD -- '*.go' '*.ts' '*.tsx' | grep -E '^\+.*\b(api|cli|gui)\.[A-Z][a-zA-Z]+[(]' | sort -u
```

For each match, grep the symbol in the codebase. If absent, the implementer fabricated it — block the PR until cleared. **This check is heuristic** — it catches package-qualified exported calls but misses fabricated types, fields, and same-package calls. Manual review of every new identifier still required.

Also: `git diff origin/master..HEAD -- '*.go' '*.ts' '*.tsx' | grep -E '^\+.*\b(TODO|FIXME|TBD|XXX)\b'`. Should be zero hits in source files — these are plan failures.

- [ ] **Step 5.2.2: State-path env-tag test suite** (per CLAUDE.md MANDATORY workflow)

Run: `go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/`. Expected: clean.

- [ ] **Step 5.2.3: Sweep test mcphub processes** (per CLAUDE.md memory)

PowerShell: `Get-Process -Name 'mcphub' -ErrorAction SilentlyContinue | Stop-Process -Force`.

- [ ] **Step 5.2.4: Frontend build + tests + e2e**

Run: `cd internal/gui/frontend && npm run typecheck && npm run test && npm run build && cd ../e2e && npm test`. Expected: clean.

- [ ] **Step 5.2.5: Push to feature branch + open PR**

**IMPORTANT:** per CLAUDE.md PR workflow, this work is NOT pushed directly to master. Create a feature branch FIRST, push, then `gh pr create`:

```bash
git checkout -b feat/v0.5.x-servers-matrix-revamp
git push -u origin feat/v0.5.x-servers-matrix-revamp
gh pr create --base master --title "feat(servers-matrix): LSP recognition + per-daemon env overlay" --body "$(cat <<'EOF'
## Summary
- Auto-discover binary install locations at install + 'Refresh discovery' GUI button
- Per-daemon env overlay file with `${parent_path}` expansion at spawn time
- 9 LSP-bridge languages render as proper matrix rows (clangd, fortran, go, javascript, python, rust, typescript, vscode-css, vscode-html)
- New `respawn` IPC command replaces UNKNOWN_COMMAND stub at supervise.go:921
- ScanEntry.LegacyConflict surfaces hub+legacy coexistence with dual badges
- mcphub config {overlay-quarantine, prune-orphan-overlay-rows} offline CLIs
- Spec §"Observability" events emitted across overlay + discovery + respawn lifecycle

## Test plan
- [ ] go build / vet / test all pass
- [ ] Codex bot review on HEAD commit (per CLAUDE.md PR workflow)
- [ ] Manual smoke: edit PATH via GUI + click Restart → daemon spawns with new env
- [ ] Manual smoke: register clangd in 2 workspaces → matrix rows correctly disambiguate
- [ ] Manual smoke: coexistence anomaly (hub + direct-stdio) renders both badges
EOF
)"
```

- [ ] **Step 5.2.6: Bot review loop per CLAUDE.md**

After PR opens, follow CLAUDE.md "PR review + merge workflow" Steps 4-7 to completion — bot must give 👍 on HEAD commit before merge. Do NOT auto-trigger CI; do NOT use `--admin` bypass.

---

## Self-review

**Spec coverage table.** Every spec v4 section maps to a task above:

| Spec section | Tasks |
|---|---|
| Goal / Scope | All phases |
| Architecture summary 4 pieces | Phase 1, 2, 3, 4 |
| Schema change `ScanEntry.LegacyConflict` | 3.1, 3.5 |
| Canonical key namespace + `NormalizeOverlayKey` | 2.1 + every overlay caller |
| Spawn-time env merge | 2.7, 2.8 |
| Auto-discovery at install | 1.3, 1.4 |
| Apply env edit from GUI | 4.2 daemon_env_handler |
| Manual discovery refresh | 4.2 discovery_refresh_handler |
| Respawn from GUI | 4.1 IPC + 4.2 daemon_respawn_handler |
| Matrix LSP recognition three-rule | 3.2, 3.3 |
| Components | distributed |
| Manifest schema additions | 1.1, 1.2 |
| DefaultHints | 1.3 |
| Read-side hardening Windows reparse-point | 2.4 |
| `/api/daemon/...` auth posture | 4.2 (uses `requireSameOrigin`) |
| `${parent_path}` token semantics | 2.5 expansion + 4.3 warning chip |
| Error handling fail-LOUD | 2.8 startup check |
| `mcphub config overlay-quarantine` | 2.6.0 + 2.6 |
| Observability | 3.4 (distributed across emitting tasks) |
| Testing strategy multi-workspace | 3.3 (helpers) + 4.4 (e2e) |
| Migration `prune-orphan-overlay-rows` | 5.1 |

**Placeholder scan.** No "TBD" / "TODO" / "implement later". Each task has acceptance criteria + test contract + done-check command, with verified real symbol names cited inline.

**Type / symbol consistency.** Every Go symbol mentioned in the task descriptions appears in the "Verified API symbol catalog" at top OR is a NEW symbol introduced in a specific task. No fabricated cross-references.

**Cross-task stub chain.** Task 2.2's `hardenedOpen` stub → Task 2.4 replaces. Task 2.7's nil-overlay passthrough → Task 2.8 wires real overlay. Both transitions are explicit.

**Discipline rule for implementer-subagents (repeated for emphasis):** before writing any code, grep / Read the live source for the symbols you intend to call. Plan v1's failure was inventing symbol names from training-data patterns; v2 prevents this by giving you the verified catalog AND requiring you to verify anything outside it.

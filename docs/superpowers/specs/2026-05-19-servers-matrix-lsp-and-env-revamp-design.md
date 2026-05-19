# Servers matrix revamp: LSP-bridge integration + per-daemon env overlay

**Status:** Spec, draft v2 (post-review revision), pending user review.

**Tracking PR(s):** to be filled by writing-plans phase. Likely 1 PR with 4 phases (auto-discovery, overlay+merge, LSP recognition, GUI surface), or split if any phase grows too large.

**Closes / unblocks:**

- Operator UX gap: 9 LSP language entries (clangd, fortran, go, javascript, python, rust, typescript, vscode-css, vscode-html) currently show under "Other MCP entries (N)" because the matrix does not recognize them as language rows of the `mcp-language-server` workspace-scoped manifest.
- Operator UX gap: `mcphub` daemons spawned by the supervisor task inherit a Task-Scheduler-logon PATH that lacks common Windows tool install locations (`C:\msys64\ucrt64\bin`, `C:\Program Files\LLVM\bin`, etc.). `gdb` MCP daemon answers but reports "GDB/LLDB not available"; `mcp-language-server` would have the same problem for `clangd`, `gopls`, `pyright`, `rust-analyzer`, etc.

## v2 revision notes (changes from v1)

v1 went through a dual independent review (codex xhigh + sonnet architect). Verdict: NEEDS_REVISION. The list below summarizes every change v2 makes to address the 6 BLOCKERS + 11 IMPORTANT findings (plus 3 MINOR polish items).

**6 BLOCKERS resolved:**

- **B1 — overlay key format.** v1 example used `\\mcp-language-server-clangd-<workspace-hash>`. Actual task-name format is `mcp-local-hub-lsp-<wsKey>-<lang>` (see [internal/api/register.go:292](../../../internal/api/register.go)). v2 uses canonical `SupervisorDaemon.TaskName` as the overlay key namespace and concretizes the new "Canonical daemon key namespace" section.
- **B2 — LSP recognition misses Go.** v1 algorithm matched on `command == "mcp-language-server"`. Go uses `backend: gopls-mcp` in the manifest (see [servers/mcp-language-server/manifest.yaml:33-37](../../../servers/mcp-language-server/manifest.yaml)). v2 specifies a three-rule recognition algorithm (entry-name prefix + `--lsp` arg sniff + gopls special-case) and distinguishes hub-proxy URL entries from direct-stdio entries.
- **B3 — overlay-only spawn skipped today.** `mergeDaemonEnv` is currently gated by `if len(d.Env) > 0` (see [internal/cli/supervise.go:1504-1505](../../../internal/cli/supervise.go)). For LSP daemons whose manifest declares no `env:`, an overlay-only PATH row would never reach the spawn. v2 explicitly removes that guard and the merge fires whenever overlay OR manifest has any keys.
- **B4 — read-side hardening missing.** Write-side `SecureWriteClientConfig` is cited in v1, but supervisor's spawn-time overlay READ is on the trust boundary for child PATH. v2 requires symlink/reparse refusal, parent-dir DACL verify (same posture as state-file helper read), owner check, and a 64 KiB hard size cap before the YAML is parsed.
- **B5 — YAML overlay vs JSON `WriteStateFileAtomic`.** `WriteStateFileAtomic` marshals JSON (see [internal/api/state_file_helper.go:50-71](../../../internal/api/state_file_helper.go)); piping YAML through it would lose comments and require YAML-as-JSON. v2 introduces a new `internal/api/daemon_env_overlay/` package that owns YAML round-trip (comments preserved through `gopkg.in/yaml.v3` Node API), flock-protected RMW, and an atomic temp+rename writer that reuses the hardened DACL/mode pipeline at temp-create time.
- **B6 — missing parked Q on canonical key namespace.** v2 answers the question directly (key = `SupervisorDaemon.TaskName` without leading backslash) and explicitly enumerates the three task-name shapes (global, LSP workspace-scoped, future `kind: workspace-scoped`).

**11 IMPORTANT findings resolved:**

- **I1 — Env merge key normalization.** Windows treats `PATH`/`Path`/`path` as the same key but `os/exec` does not normalize. v2 specifies case-insensitive merge precedence on Windows (last write wins per normalized key, preserving the original casing of the highest-precedence source).
- **I2 — `required_binaries` requires Go struct extension in same commit.** `ParseManifest` uses `KnownFields(true)` (see [internal/config/manifest.go:128](../../../internal/config/manifest.go)) which rejects unknown YAML keys. v2 explicitly couples the manifest YAML additions to corresponding Go struct field additions on `ServerManifest` and `LanguageSpec`.
- **I3 — `/api/daemon/env` auth posture concrete.** v2 names the existing CSRF and same-origin discipline endpoints (`/api/migrate`, `/api/dismiss`, `/api/secrets`) follow; `/api/daemon/env` reuses the same. Plus: known-task validation against `supervisor-intent.json` before write; reject `taskName` not present in current intent with 400.
- **I4 — Concurrent GUI Apply + auto-discovery refresh lost-update.** v2's `WriteOverlay(path, mutator func)` is a flock-protected RMW: lock → read+parse → mutator transforms → marshal → atomic temp+rename → release. Apply and refresh serialize on the same lock.
- **I5 — Corrupt overlay fail-LOUD.** v1's "warn + proceed with manifest env only" silently spawns daemons missing operator-configured PATH (foot-gun: operator fixes YAML typo, daemon still broken, no signal). v2: supervisor refuses to spawn affected daemons; GUI shows red banner with parse error including line/column; operator either fixes the file or runs `mcphub config overlay-quarantine` to rename to `daemon-env-overrides.yaml.corrupt-<ts>` and restart with empty overlay.
- **I6 — Discovery test fixture problem.** Production code uses absolute shipped hints; tests pin to a real filesystem. v2: `binary_discovery.Discover(ctx, requiredBinaries, hints)` takes `hints []string` as a parameter so unit tests pass synthetic temp-dir roots; the production caller uses `binary_discovery.DefaultHints()` for the shipped per-OS list.
- **I7 — Multi-workspace LSP test fixture missing.** v2 adds an explicit test scenario: 2 registered workspaces for clangd; matrix cell URL switches on active-workspace selector change; per-workspace history visible in row drawer (open questions remaining for the GUI shape).
- **I8 — `respawn` IPC integration test missing.** v2 adds an explicit test under `internal/cli/supervise_respawn_test.go` covering: valid taskName → graceful shutdown 5s → spawn with intent+overlay; invalid taskName → IPC error; daemon in Backoff state → reset to Spawning + spawn.
- **I9 — Missing parked Q: respawn semantics.** v2 specifies graceful 5s SIGTERM/CtrlBreak then SIGKILL/Job-Object kill, then spawn-from-intent+overlay, plus state-machine transitions (Running → Spawning → Running; Backoff → Spawning; Quarantined refuses without operator override).
- **I10 — Missing parked Q: YAML editability vs hardened writer + lock discipline.** v2 specifies: YAML writer is `gopkg.in/yaml.v3` Node API (preserves comments) + flock-protected RMW + atomic temp+rename + handle-bound DACL/mode at temp create.
- **I11 — Migration internally inconsistent.** v1 said "overlay only ADDS to PATH, never REMOVES" but also "overlay wins on collisions" and "`${parent_path}` is optional". v2 drops the misleading "never removes" claim; the truthful statement is "default install-time templates always include `${parent_path}` so existing PATH-dependent daemons keep their inherited PATH; operators can drop the token to deliberately override".

**3 MINOR polish:**

- **M1 — searchHints framing.** v2 states hints are best-effort seed data only; operator's override channel is the env overlay file (not hints expansion).
- **M2 — shell metachar already correct.** v1 framing kept; env values go to `exec.Command` env block, not a shell.
- **M3 — PATH redaction in audit events.** v2: `daemon-env-overlay-loaded` event logs row count + `sha256(${parent_path})` truncated to 12 hex chars at info level; full PATH only at debug level.

## Goal

Make the Servers matrix the single place where an operator sees and manages every MCP server mcphub knows about, including the 9 LSP-bridge languages. Make each daemon's effective env (especially PATH) visible and editable from the GUI, with auto-discovery filling in sensible defaults at install time.

## Scope

In scope:

- Matrix recognition of LSP language entries (currently classified as "Other MCP entries") as language rows under the existing `mcp-language-server` manifest.
- Per-language rows in the Servers matrix with the same check/uncheck affordances existing global-server rows already have.
- Single `Active workspace` selector at the top of the Servers screen (or near the LSP rows section) that scopes the check/uncheck and Register actions.
- Per-daemon `env` overlay file separate from shipped manifests. Supervisor merges manifest `env` with overlay `env` at spawn time (overlay wins on collisions, with Windows case-insensitive key normalization).
- Auto-discovery of common binary install locations (Windows, Linux, macOS) at `mcphub install/setup` time. Populates initial overlay file. Manual "Refresh discovery" button in the GUI re-runs at any time.
- Per-row drawer in the GUI showing effective env (post-merge) with edit affordance and `Apply` button. Apply persists to overlay + IPC-signals supervisor to respawn the affected daemon.
- New `respawn` IPC command on the supervisor (replaces the v0.5.0 `restart`/`reload` UNKNOWN_COMMAND stub at [supervise.go:921](../../../internal/cli/supervise.go) for the single-daemon respawn path).

Out of scope (deferred):

- Backend rewrite of `mcp-language-server` to serve multiple workspaces from one daemon. The current per-(workspace, language) proxy model stays.
- Cross-workspace router that picks a proxy by caller cwd. The active-workspace selector is operator-driven, not auto-routed.
- Linux/macOS systemd/launchd PATH inheritance fixes — out of scope for this design (POSIX paths inherit from the user's shell which usually has the needed tools). The overlay surface is portable; only the install-time auto-discovery code is Windows-heavy.

## Architecture summary

Four pieces, mostly independent, glued through existing seams:

1. **Manifest schema additions (with Go struct extension).** Server manifest YAML gains optional `required_binaries: [name, ...]` at server level AND per-language level. Same commit adds the field to `ServerManifest` and `LanguageSpec` Go structs in [internal/config/manifest.go](../../../internal/config/manifest.go) so `ParseManifest`'s `KnownFields(true)` does not reject the new keys.
2. **Auto-discovery engine.** New `internal/api/binary_discovery/` package: `Discover(ctx, requiredBinaries, hints) (map[binary]absolutePath, error)`. `hints` is a `[]string` parameter so unit tests inject synthetic roots; production callers use `binary_discovery.DefaultHints()` for the shipped per-OS list. Called by `mcphub install/setup` and by the GUI's `/api/discovery/refresh`.
3. **Env overlay file (new package).** New `internal/api/daemon_env_overlay/` package owns: load + parse (with hardened read — see Security), flock-protected RMW writer, YAML round-trip with comments preserved (`gopkg.in/yaml.v3` Node API), and the `WriteOverlay(path, mutator func(*Overlay) error)` transactional API. Supervisor `SupervisorDaemon.Env` is merged with the overlay at spawn time (overlay wins, Windows case-insensitive normalize). Storage path: `~/.config/mcp-local-hub/daemon-env-overrides.yaml` (POSIX) / `%LOCALAPPDATA%\mcp-local-hub\daemon-env-overrides.yaml` (Windows).

   ```yaml
   # daemon-env-overrides.yaml — managed by mcphub; operator-editable.
   version: 1
   daemons:
     mcp-local-hub-gdb-default:
       env:
         Path: "C:/msys64/ucrt64/bin;${parent_path}"
       source: auto-discovery  # auto-discovery | operator
       discovered_at: 2026-05-19T14:00:00Z
     mcp-local-hub-lsp-a1b2c3d4-clangd:
       env:
         Path: "C:/Program Files/LLVM/bin;${parent_path}"
       source: operator
   ```

   `${parent_path}` resolves at spawn time to the supervisor process's current PATH (`os.Environ()["PATH"]` lookup with case-insensitive match on Windows). Operators MUST include the token to preserve the parent PATH.

4. **Matrix LSP recognition + workspace scope.** Three changes in [internal/api/scan.go](../../../internal/api/scan.go) + frontend:
   - Three-rule recognition algorithm (see "Matrix LSP recognition" below) distinguishes hub-proxy URL entries from direct-stdio entries; both forms render as the same language row with different badges.
   - Top-of-screen `Active workspace` selector. Default value = most-recent registered workspace (per registry order in `~/.config/mcp-local-hub/workspaces.json`), OR a literal "(none — register a workspace first)" placeholder when empty.
   - Per-cell semantics: ☐ unchecked = no entry in this client; ☑ checked-direct (legacy direct-stdio entry, badge "legacy"); ☑ checked-hub (entry points at mcphub lazy proxy URL, badge implicit/default). Operator unchecks → removes entry; checks an unchecked cell → `mcphub register <active-workspace> <language> --clients <this>` runs (replacing any direct-stdio entry with the lazy-proxy HTTP URL).

## Canonical daemon key namespace

Overlay file keys are `SupervisorDaemon.TaskName` strings as they appear in `supervisor-intent.json`, **without** a leading backslash. Three concrete shapes apply:

| Daemon kind | TaskName format | Source |
|---|---|---|
| Global | `mcp-local-hub-<server>-<daemon>` | derived at install from manifest; default daemon name is `default`. Examples: `mcp-local-hub-gdb-default`, `mcp-local-hub-memory-default`. |
| LSP workspace-scoped | `mcp-local-hub-lsp-<wsKey>-<lang>` | [internal/api/register.go:292](../../../internal/api/register.go). `wsKey` is the 8-char workspace-key hash. Examples: `mcp-local-hub-lsp-a1b2c3d4-clangd`, `mcp-local-hub-lsp-a1b2c3d4-rust`. |
| Future workspace-scoped non-LSP | `mcp-local-hub-<server>-<wsKey>` (proposed) | not yet implemented in any manifest; reserved. Documented here so the overlay key format does not need to change later. |

Overlay lookup at spawn time: `LookupOverlay(taskName string)` strips any leading backslash (some legacy Task Scheduler paths carry it), then exact-matches the rest. The supervisor's in-memory `SupervisorDaemon.TaskName` is already in bare form (see [internal/cli/supervise_status.go:30-32](../../../internal/cli/supervise_status.go) where `canonicalSupervisorTaskName` is called explicitly to add the prefix for scheduler API calls only).

## Data flow

### Spawn-time env merge (modified)

```text
1. Supervisor reads supervisor-intent.json   → SupervisorDaemon{Env: M}
2. Supervisor reads daemon-env-overrides.yaml → overlay[taskName].env = O
   (hardened read — see Security)
3. mergeDaemonEnv(parent=os.Environ(), manifestEnv=M, overlayEnv=O)
     → expand ${parent_path} in O values using parent's PATH
     → merge with Windows case-insensitive normalize
     → precedence: parent < manifest < overlay
4. cmd.Env = merged
5. Daemon spawns with the merged env
```

The existing `mergeDaemonEnv` helper at [internal/cli/supervise.go:1457](../../../internal/cli/supervise.go) is extended to take a third `overlayEnv` parameter; the gate at line 1504-1505 (`if len(d.Env) > 0`) is **removed** so the merge fires whenever EITHER `d.Env` OR overlay has any keys. When both are empty, fall back to `os.Environ()` directly (parent inheritance unchanged).

### Auto-discovery at install (new)

```text
mcphub install / mcphub setup
  ↓
For each server manifest with required_binaries:
  For each binary in required_binaries:
    Walk DefaultHints() in OS-specific order until a hit
  Compose PATH = first hit's parent dir per binary, joined with ${parent_path}
  WriteOverlay(...) under SupervisorDaemon.TaskName key
  Mark source: auto-discovery, discovered_at: <ts>
```

Auto-discovery NEVER overwrites a row with `source: operator`. If a row already exists with that source, auto-discovery skips it (logged as `binary-discovery-skipped-operator-override`).

### Manual discovery refresh from GUI (new)

```text
GUI Settings → Daemons / per-row drawer → "Refresh discovery"
  ↓
POST /api/discovery/refresh {server, daemon | 'all'}
  ↓
GUI handler invokes binary_discovery.Discover(ctx, requiredBinaries, DefaultHints())
  ↓
WriteOverlay(...) under the per-daemon flock
  ↓
Returns new effective env to GUI for display
  ↓
GUI shows "Restart daemon to apply" affordance; on click → POST /api/daemon/respawn
```

### Apply env edit from GUI (new)

```text
GUI per-row drawer → Edit PATH field → Apply
  ↓
POST /api/daemon/env {taskName, env: {KEY: value, ...}}
  ↓
GUI handler validates:
  - taskName is in current supervisor-intent.json
  - keys/values contain no newlines / NUL / control chars
  - same CSRF + same-origin posture as /api/migrate, /api/dismiss
  ↓
WriteOverlay(path, mutator) — flock-protected RMW
  ↓
GUI handler issues supervisor IPC: respawn {taskName}
  ↓
Supervisor merges manifest env + overlay env, respawns daemon
  ↓
GUI polls /api/status, surfaces new PID + Port + State
```

### Matrix LSP recognition (modified)

`scan.go`'s per-client scan functions emit raw entries keyed by entry-name. After scanning, a new pass categorizes LSP entries using a three-rule algorithm:

```text
For each raw entry e in scanned clients:

  Rule 1 — Hub-managed lazy proxy:
    e.Name matches /^mcp-language-server-([a-z0-9-]+)$/
    AND extracted suffix matches one of mcp-language-server manifest's
        9 declared languages (clangd, fortran, go, javascript, python,
        rust, typescript, vscode-css, vscode-html)
    AND e.URL is set (HTTP entry, not stdio)
    AND e.URL matches the lazy-proxy port pool 9200-9299 OR the
        explicit register registry's per-(workspace,lang) port
    → categorize as LSP language entry e.Language=<suffix>, badge="hub"

  Rule 2 — Direct-stdio mcp-language-server invocation:
    e.Command basename == "mcp-language-server"
    AND e.Args contains a "--lsp <X>" pair where X matches one
        of the 9 languages' lsp_command field
    → categorize as LSP language entry, badge="legacy"

  Rule 3 — Direct-stdio gopls invocation (Go special case):
    e.Command basename == "gopls"
    AND e.Args contains "mcp" (per manifest extra_flags for Go)
    → categorize as Go LSP entry, badge="legacy"

After categorization:
  Build matrix rows for entries; non-LSP entries unchanged; each
  LSP language renders as one INDIVIDUAL row (9 rows total — one per
  manifest language). Per user decision: per-language axis, not
  grouped under a "mcp-language-server" parent.
```

Recognition rules 1 + 2 are intentionally orthogonal: a cell can be EITHER `hub` OR `legacy` for a given (client, language) pair. If both shapes coexist (e.g., operator manually added `mcp-language-server` direct entry AND ran `mcphub register`), both rows appear; this is a configuration anomaly surfaced as a yellow warning chip in the matrix.

## Components

| Component | New / Modified | Purpose | Owns | Depends on |
|---|---|---|---|---|
| `internal/api/binary_discovery/` | NEW | Auto-discover common binary paths per OS | shipped per-OS hints, hint-injection seam for tests | (none) |
| `internal/api/daemon_env_overlay/` | NEW | YAML overlay file owner | overlay parse, flock-protected RMW writer, hardened read, comment-preserving YAML round-trip | `gopkg.in/yaml.v3`, state-file helper (DACL/mode at temp-create), flock helper |
| `internal/cli/supervise.go` `mergeDaemonEnv` | MODIFY | Apply overlay at spawn-time; remove `len(d.Env) > 0` gate; Windows case-insensitive key normalize | env merge precedence, `${parent_path}` expansion | daemon_env_overlay |
| `internal/cli/supervise.go` IPC dispatcher | MODIFY | Add `respawn` IPC command (replaces UNKNOWN_COMMAND stub at line 921 for single-daemon respawn) | IPC frame parse, graceful 5s shutdown, spawn-from-intent-overlay, lifecycle event emit | reconcile loop, daemon_env_overlay |
| `internal/api/scan.go` | MODIFY | Recognize mcp-language-server entries via three-rule algorithm; emit LSP language rows | per-language entry classification, badge metadata | mcp-language-server manifest reader |
| `internal/api/manifest_lsp_lookup.go` | NEW | Reverse-lookup helpers for the three-rule recognition | manifest reading, language-to-binding map, lsp_command index | config package |
| `internal/gui/server.go` | NEW handlers | `/api/daemon/env` (write), `/api/discovery/refresh` (rescan), `/api/daemon/respawn` (IPC) | route registration, validation, IPC dispatch | supervisor IPC, daemon_env_overlay |
| `internal/gui/frontend/src/screens/Servers.tsx` | MODIFY | Active-workspace selector; 9 LSP rows; per-row drawer with env editor; `${parent_path}` warning chip when token absent | per-cell action mapping, drawer state, env edit form | new API endpoints |
| `servers/mcp-language-server/manifest.yaml` | MODIFY | Add `required_binaries` per language | manifest schema | (config schema) |
| `internal/config/manifest.go` | MODIFY | Add `RequiredBinaries []string` field to `ServerManifest` and `LanguageSpec` structs | YAML deserialization (preserve `KnownFields(true)` strictness) | (none) |
| `servers/gdb/manifest.yaml` | MODIFY | Add `required_binaries: [gdb]` at server level | manifest schema | (config schema) |
| `servers/lldb/manifest.yaml` | MODIFY | Add `required_binaries: [lldb]` at server level | manifest schema | (config schema) |

## Manifest schema additions

Add optional `required_binaries` field at server OR language level. **Same commit** adds the field to the Go structs (`ServerManifest`, `LanguageSpec` in [internal/config/manifest.go](../../../internal/config/manifest.go)) so `ParseManifest`'s `KnownFields(true)` strictness still rejects truly unknown keys.

```yaml
# Server-level (applies to whole server, e.g. gdb)
required_binaries:
  - gdb

# Per-language (workspace-scoped servers like mcp-language-server)
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

`required_binaries` is metadata only — runtime spawn never reads it. Auto-discovery uses it to know what to look for. Manifest is parsed once at startup; the field is exposed through the existing manifest reader API.

## DefaultHints — shipped per-OS list

```go
// internal/api/binary_discovery/hints_windows.go
func DefaultHintsWindows() []string {
    return []string{
        `C:\msys64\ucrt64\bin`,
        `C:\msys64\mingw64\bin`,
        `C:\msys64\clang64\bin`,
        `C:\Program Files\LLVM\bin`,
        `C:\Program Files\Go\bin`,
        `C:\Program Files\Microsoft Visual Studio\2022\BuildTools\VC\Tools\Llvm\x64\bin`,
        `%LOCALAPPDATA%\Programs\Python\Python311`,
        `%LOCALAPPDATA%\Programs\Python\Python312`,
        `%LOCALAPPDATA%\Programs\Python\Python313`,
        `%LOCALAPPDATA%\Programs\Python\Python314`,
        `%USERPROFILE%\.cargo\bin`,
        `%USERPROFILE%\go\bin`,
        `%USERPROFILE%\.local\bin`,
        `%LOCALAPPDATA%\fnm_multishells`,
        `%LOCALAPPDATA%\Programs\fnm`,
        `%LOCALAPPDATA%\nvm`,
        `%APPDATA%\npm`,
    }
}

// hints_linux.go: /usr/local/bin, /usr/bin, ~/.local/bin, ~/.cargo/bin, ~/go/bin
// hints_darwin.go: /opt/homebrew/bin, /usr/local/bin, /opt/local/bin + the Linux list
```

Hints are best-effort seed data only. **The operator's override channel is the env overlay file, NOT hints expansion.** Adding a hint requires a code change + PR.

For unit tests, `Discover(ctx, requiredBinaries, hints []string)` takes `hints` as a parameter — tests pass synthetic temp-dir roots; production callers use `DefaultHints()`.

## Error handling

| Failure mode | Behavior |
|---|---|
| Overlay file missing | Treat as empty overlay. No error. Manifest env applies. |
| Overlay file unreadable (permission denied / symlink at path) | **Fail-LOUD.** Supervisor refuses to spawn affected daemons. GUI shows red banner: `daemon-env-overlay-read-rejected`. Audit event with reason. Operator can `mcphub config overlay-quarantine` to rename to `.corrupt-<ts>` and restart with empty overlay. |
| Overlay file corrupt YAML / size > 64 KiB | **Fail-LOUD.** Same as above; parse error includes line/column for inline editor jump. |
| Overlay declares an unknown taskName | Log warn `daemon-env-overlay-orphan-row` at supervisor startup. Ignore that row at spawn. GUI surfaces orphan rows in a dedicated section with "delete this row" affordance. Triggered automatically after `mcphub unregister <workspace>` removes the workspace from registry. |
| Auto-discovery cannot find a required binary | Discovery returns `{binary: ""}` for the missing one. Overlay row written with comment `auto-detected: BINARY_NAME not found in any common location`. GUI shows red flag on the daemon's row with a search hint and a "set PATH manually" link. |
| Apply IPC respawn fails | GUI surfaces the supervisor error; overlay change stays on disk; operator can retry. No state mutation rollback. |
| `mcphub register <ws> <lang>` fails | Existing register error handling unchanged. GUI displays the per-cell failed row, retry affordance via second Apply. |
| Concurrent GUI Apply + auto-discovery refresh | Both go through `WriteOverlay(path, mutator)`'s flock; second caller waits. No lost-update window. |

## Security

### Read-side hardening (new — addresses v1 BLOCKER C1)

The supervisor's spawn-time overlay READ is on the trust boundary for child process PATH. The daemon_env_overlay package hardens the read:

1. **Refuse symlink / reparse point at overlay path.** `os.Lstat` pre-check; if `(fi.Mode() & os.ModeSymlink) != 0` or (Windows) reparse-point attribute set, refuse with `ErrOverlaySymlinkRefused`.
2. **Refuse non-regular file.** `(fi.Mode() & os.ModeIrregular) == 0` required.
3. **Verify parent dir DACL / mode** (same posture as state-file helper read; reuses `checkStateDirParentReadSafe`).
4. **Refuse non-owner file** (POSIX: stat uid == os.Getuid(); Windows: file owner SID matches process token user SID).
5. **64 KiB hard size cap.** `io.LimitReader(file, 65536+1)` then check; reject if cap exceeded.
6. **Reject non-printable / non-UTF-8 bytes.** Defense in depth before YAML parse.

If any check fails, supervisor refuses to spawn affected daemons; GUI shows the failure reason.

### Write-side hardening (existing posture extended)

- Overlay file lives in user-scope dir (per-machine, per-user). Same DACL/mode posture as other state files (handle-bound 0600 / Admin+SYSTEM+CurrentUser ACL).
- `WriteOverlay` uses the hardened temp+rename pipeline at temp-create (per [internal/api/state_file_helper.go](../../../internal/api/state_file_helper.go)), but the writer is custom (YAML, not JSON) — the DACL/mode at temp-create comes from the same hardened pipeline; the encoding is YAML.

### `/api/daemon/env` auth posture (new — addresses v1 IMPORTANT I3)

`/api/daemon/env` follows the same auth posture as `/api/migrate`, `/api/dismiss`, `/api/secrets`:

- Loopback-only listener (already enforced at server bind).
- Same-origin policy with strict Origin header check.
- CSRF token discipline: `X-Mcphub-CSRF` header matched against the per-session token issued at GUI bootstrap.
- **Known-task validation.** Server rejects the request with HTTP 400 if `taskName` is not present in current `supervisor-intent.json`. This prevents drive-by writes from a compromised browser tab against a non-existent taskName.
- **Per-key value validation.** Keys must match `[A-Za-z_][A-Za-z0-9_]*`; values reject newline, NUL, control chars (defense-in-depth against env-injection into log lines or proc-environ snapshots).

### `${parent_path}` token semantics

- The ONLY supported template token. Operator's overlay value is treated as opaque bytes after expansion; values go to `exec.Command` env block, not a shell.
- Token resolution: at spawn time, supervisor reads `os.Environ()` and finds the PATH key (case-insensitive on Windows: `Path`, `PATH`, `path` all match). Expansion is single-pass, non-recursive — the expanded value is not re-scanned for tokens.
- If `${parent_path}` is omitted from the operator's PATH value, the parent PATH is **dropped** for that daemon. The GUI shows a warning chip "PATH does not include `${parent_path}` — parent PATH will be DROPPED for this daemon" when the operator types or pastes a PATH value missing the token. Auto-discovery templates always include the token at the tail to preserve parent.

## Observability

New events on the hub-mcp event log:

- `daemon-env-overlay-loaded` (info): row count, `sha256(${parent_path})[:12]` of resolved parent (full PATH only at debug level)
- `daemon-env-overlay-load-failed` (warn): path, error class (symlink-refused, size-exceeded, parse-failed, parent-dir-unsafe, non-owner), line/col for parse-failed
- `daemon-env-overlay-read-rejected` (error): path, hardened-read failure mode — supervisor refuses to spawn affected daemons
- `daemon-env-overlay-applied-via-gui` (info): taskName, changed keys (values redacted, only key names + before/after hash at debug)
- `daemon-env-overlay-orphan-row` (warn): taskName not in current intent; emitted on startup load
- `daemon-env-overlay-skipped-operator-override` (info): taskName, binary — auto-discovery refused to overwrite an `source: operator` row
- `binary-discovery-ran` (info): server, scan duration, hits per binary
- `binary-discovery-missing` (warn): server, binary, scanned hints summary, "set PATH manually in GUI" guidance
- `supervisor-respawn-via-gui` (info): taskName, requesting client (loopback PID), respawn outcome
- `supervisor-respawn-graceful-timeout` (warn): taskName, soft-shutdown deadline exceeded, force-kill path taken

## Testing strategy

### Unit

- `binary_discovery/discover_test.go`: pass `hints=[<tempdir>]` with synthesized fake binaries at known paths; assert discovery returns the right absolute paths; assert missing binaries return empty. **Test-injected hints** decouples test from real machine layout.
- `daemon_env_overlay/parse_test.go`: round-trip overlay YAML preserving comments; ordered keys; reject symlink at path; reject 64 KiB+1 size; reject non-UTF-8.
- `daemon_env_overlay/write_test.go`: `WriteOverlay(path, mutator)` flock-protects concurrent writes; assert atomic temp+rename; assert DACL/mode at temp-create matches state-file helper.
- `mergeDaemonEnv` extended tests: overlay arg now non-nil; assert precedence parent < manifest < overlay; Windows case-insensitive key collision (`PATH` vs `Path` merge correctly); POSIX case-sensitive.
- `scan.go` three-rule recognition: test fixtures for (a) hub URL entries via `mcphub register`, (b) direct-stdio `mcp-language-server --lsp X`, (c) direct-stdio `gopls mcp` for Go; assert all three map to the right LSP language row with the right badge.
- `manifest_lsp_lookup_test.go`: lookup all 9 manifest languages; assert gopls-mcp backend is surfaced for Go.

### Integration

- `internal/cli/supervise_lsp_e2e_test.go` (new): synthesize a workspace, register one language via `mcphub register`, verify supervisor-intent.json gains the entry, verify supervisor spawns a lazy proxy on materialization, verify env from overlay applies.
- `internal/cli/supervise_respawn_test.go` (new — addresses v1 IMPORTANT I8): IPC `respawn` command with valid taskName → graceful 5s shutdown → spawn with intent+overlay; invalid taskName → IPC error `UNKNOWN_TASK`; daemon in Backoff → state reset to Spawning + spawn; daemon in Quarantined → respawn refused unless `force: true`.
- `internal/gui/daemon_env_handler_test.go` (new): POST /api/daemon/env writes overlay + triggers respawn; assert event log entry + new effective env via /api/status; assert CSRF rejected without header; assert unknown taskName rejected with 400.
- `internal/api/daemon_env_overlay/integration_test.go`: WriteOverlay + Load round-trip across multiple goroutines under flock.

### Multi-workspace LSP recognition test (addresses v1 IMPORTANT I7)

`internal/gui/e2e/tests/servers-lsp-multi-workspace.spec.ts`:

1. Seed 2 workspaces in registry (`ws1` + `ws2`), both registered for clangd.
2. Active-workspace selector defaults to `ws1`; matrix clangd row shows URL for `ws1` proxy.
3. Switch selector to `ws2`; assert matrix re-renders with `ws2` URL on the same row.
4. Per-row drawer shows per-workspace history (both proxies, with badges).
5. Uncheck clangd cell for codex client while selector is `ws2` → `mcphub register --unset` runs against `ws2` only; `ws1` row unchanged.

### E2E (Playwright)

- `internal/gui/e2e/tests/servers-lsp.spec.ts`: matrix shows 9 LSP rows; check/uncheck cycles register/unregister per active workspace; per-row drawer opens with env editor; Apply respawns daemon and Dashboard reflects new PID.
- `internal/gui/e2e/tests/servers-env-overlay.spec.ts`: per-row drawer reveals effective env; edit Path field WITHOUT `${parent_path}` token → warning chip appears; Apply writes overlay + GUI shows new effective env post-respawn.

## Migration

For existing installs:

- **Overlay file does not exist** → supervisor merges only `os.Environ()` + `SupervisorDaemon.Env` (same as today; the removed `len(d.Env) > 0` gate doesn't materially change behavior here because the merge with empty overlay produces the same env as the gated path). No behavior change.
- **LSP-recognition addition** → rows that previously appeared in "Other MCP entries" now appear as proper matrix rows. Backwards-compatible: clients keep their existing direct-stdio entries; matrix offers explicit migrate-to-hub action with a "legacy" badge so operator chooses when to switch.
- **`mcphub install/setup` auto-discovery** → runs once (writes initial overlay file with the binaries it finds under `source: auto-discovery`). Existing operators get a one-time `binary-discovery-ran` event. Default templates always include `${parent_path}` at the tail, so existing PATH-dependent daemons keep their inherited PATH. Operators can drop the token to deliberately override.
- **`mcphub unregister <workspace>`** → emits `daemon-env-overlay-orphan-row` for any overlay rows now keyed under stale `mcp-local-hub-lsp-<wsKey>-*` taskNames. `mcphub config prune-orphan-overlay-rows` removes them; GUI surfaces a "Clean up orphan overlay rows" affordance.

## Open questions parked for plan / implementation phase

1. **Multi-workspace UX for LSP**: when an operator has 2+ workspaces registered for the same language, the matrix shows the active workspace's URL — but the per-row drawer needs to expose per-workspace history. Proposed: drawer shows a sub-table of (workspace, port, last-used) rows under the active-workspace's row. Open: should the active selector be at top-of-screen or per-row?
2. **Auto-discovery scope for non-LSP servers**: `gdb`, `godbolt`, `perftools` also need external binaries (gdb, lldb, clang-tidy). Proposed answer (carried from v1): yes, server-level `required_binaries` added to each manifest with external deps; same discovery engine handles both. Open: should `lldb` manifest's `command: mcphub` (internal bridge) bypass discovery entirely?
3. **GUI "Active workspace" persistence**: per-machine OR per-GUI-session? Proposed answer (carried from v1): per-machine setting in the existing GUI preferences file (`gui-preferences.yaml`), persisted across GUI restarts.
4. **Force-respawn semantics for Quarantined state**: should the GUI expose a `force: true` checkbox in the per-row drawer, or require a separate `mcphub daemon unquarantine` CLI step first? Open — affects API shape of `/api/daemon/respawn`.

## Terms and Abbreviations

- **LSP**: Language Server Protocol; a JSON-RPC protocol exposing IDE-grade code intelligence (find symbol, hover, references, completion, diagnostics) over a stdio/socket channel. Each language has its own implementation (clangd for C/C++, rust-analyzer for Rust, gopls for Go, etc.).
- **mcp-language-server**: a Go binary (`/c/Users/dima_/go/bin/mcp-language-server`) that bridges an LSP server to the MCP protocol; mcphub spawns it with `--workspace <dir> --lsp <command>`.
- **gopls-mcp**: alternative backend used by the Go language (the `gopls` binary supports an `mcp` subcommand that exposes MCP directly without the mcp-language-server bridge). See manifest line 33-37.
- **Hub-proxy / lazy proxy**: a workspace-scoped mcphub daemon that listens on a port from the 9200-9299 pool and forwards MCP traffic to the heavy LSP backend (the actual `mcp-language-server` or `gopls` binary), spawning the backend lazily on the first tools/call.
- **Direct-stdio / legacy**: an MCP client config entry that invokes `mcp-language-server` or `gopls` directly via stdio, bypassing mcphub. The matrix surfaces these with a "legacy" badge.
- **Workspace-scoped**: in mcphub manifest schema, a server kind where each (workspace, language) pair gets its own daemon. Contrast with `kind: global` where one daemon serves all callers.
- **Overlay**: a user-editable layer that augments shipped manifests without modifying them. mcphub install/upgrade preserves overlays.
- **Active workspace**: the workspace whose `mcphub register`-ed lazy proxies are addressed by Servers matrix actions in the GUI. Operator picks one at a time.
- **`${parent_path}` token**: a placeholder in the overlay file's env values that expands at spawn time to the supervisor process's PATH. Lets the operator declare "prepend this to PATH" without manually concatenating the inherited PATH each time.
- **wsKey**: the 8-character hex hash of a workspace path; used as the workspace component of LSP task names (`mcp-local-hub-lsp-<wsKey>-<lang>`).
- **SupervisorDaemon.TaskName**: the canonical string identifier for a daemon in supervisor-intent.json, IPC commands, and overlay keys. Bare form (no leading backslash); canonicalSupervisorTaskName adds the `\` prefix only when calling Task Scheduler APIs.
- **flock**: file-lock; POSIX `flock(2)` / Windows `LockFileEx` advisory lock used to serialize RMW on the overlay file across processes.
- **CSRF**: Cross-Site Request Forgery; the per-session token issued at GUI bootstrap that mutation endpoints require in a header to prevent drive-by writes from a malicious browser tab.

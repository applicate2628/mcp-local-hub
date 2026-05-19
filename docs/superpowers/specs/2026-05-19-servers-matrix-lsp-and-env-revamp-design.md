# Servers matrix revamp: LSP-bridge integration + per-daemon env overlay

**Status:** Spec, draft v3 (second post-review revision), pending user review.

**Tracking PR(s):** to be filled by writing-plans phase. Likely 1 PR with 4 phases (auto-discovery, overlay+merge, LSP recognition, GUI surface), or split if any phase grows too large.

**Closes / unblocks:**

- Operator UX gap: 9 LSP language entries (clangd, fortran, go, javascript, python, rust, typescript, vscode-css, vscode-html) currently show under "Other MCP entries (N)" because the matrix does not recognize them as language rows of the `mcp-language-server` workspace-scoped manifest.
- Operator UX gap: `mcphub` daemons spawned by the supervisor task inherit a Task-Scheduler-logon PATH that lacks common Windows tool install locations (`C:\msys64\ucrt64\bin`, `C:\Program Files\LLVM\bin`, etc.). `gdb` MCP daemon answers but reports "GDB/LLDB not available"; `mcp-language-server` would have the same problem for `clangd`, `gopls`, `pyright`, `rust-analyzer`, etc.

## v3 revision notes (changes from v2)

v2 went through a second dual review (codex xhigh + sonnet architect). Verdict: NEEDS_REVISION (both). v3 fixes 5 BLOCKERS + 9 IMPORTANT findings that v2 introduced or left unaddressed. **Every code reference cited in v3 has been empirically `grep`-verified before inclusion** (axiomatic fix for the v2 failure mode of citing fabricated function names and wrong struct fields).

**5 BLOCKERS resolved:**

- **B-V3-1 — overlay key namespace direction reversed.** v2 claimed keys are stored "without leading backslash"; actual code stores `SupervisorDaemon.TaskName` WITH the leading backslash (`internal/api/supervisor_intent.go:25` comment: `// canonical, e.g. "\\mcp-local-hub-memory-default"`) and reconcile indexes by the canonical form (`internal/cli/supervise_reconcile.go:107`). v3: overlay keys are stored in canonical leading-backslash form, matching the field's serialization contract. Lookup helper accepts either form and normalizes by prepending `\` if absent.
- **B-V3-2 — three-rule recognition references nonexistent struct fields.** v2 used `e.URL`, `e.Command`, `e.Args` — but `ClientEntry` only has `{Transport, Endpoint, Raw map[string]any}` (`internal/api/types.go:110-114`). v3 rewrites recognition to parse the existing `Raw map[string]any` shape, mirroring `shapeClaudeEntry` at `internal/api/scan.go:448-454` (`raw["url"]` for HTTP, `raw["command"]` + `raw["args"]` for stdio).
- **B-V3-3 — three-rule recognition misses ResolveEntryName collision suffixes.** v2 regex `/^mcp-language-server-([a-z0-9-]+)$/` failed when same-language multi-workspace forced `ResolveEntryName` (`internal/api/register.go:722-747`) to append `-<4hex>` or `-<8hex>` suffix. v3 algorithm anchors on the exact 9-language set + optional collision suffix, with registry-side disambiguation for which workspace owns each suffixed entry. Multi-workspace recognition becomes a core requirement, not a parked UX question.
- **B-V3-4 — read-side hardening cites fabricated function.** v2 cited `checkStateDirParentReadSafe`; only `checkStateDirParentWriteSafe` exists (`internal/api/state_file_helper.go:155`). v3 admits the read-side helper must be NEW work: section "Read-side hardening" names the new package-internal `checkStateDirParentReadSafe` as scope-of-this-design, with explicit symmetry against the existing write-side function. v3 also fixes the `fi.Mode().IsRegular()` semantic (v2 had wrong `& os.ModeIrregular` bit math) and preserves default-relax / strict-mode semantics from the existing write side (`internal/api/state_file_helper.go:139-157`).
- **B-V3-5 — YAML helper reuse claim impossible without new exported primitive.** `WriteStateFileAtomic` marshals JSON (`internal/api/state_file_helper.go:50-93`); `secureWriteStateFileWithOperatorOpt` is unexported. `SecureWriteClientConfig(path string, payload []byte) error` IS exported and DOES take raw bytes (`internal/api/state_file_helper.go:127`), so v3 routes `daemon_env_overlay` writes through `SecureWriteClientConfig` directly with YAML-marshaled bytes. No duplication; no new exported primitive needed for the write side. (Read side still needs a new helper per B-V3-4.)

**9 IMPORTANT resolved:**

- **I-V3-1 — JS/TS recognition ambiguity.** Both javascript and typescript use `lsp_command: typescript-language-server` (`servers/mcp-language-server/manifest.yaml:42, 59`). v3 recognition does NOT look up by `lsp_command`; it matches by entry-name suffix against the 9-language set. Direct-stdio entries are distinguished by `--workspace <path>` argument parsing for which language they target (the `register` flow emits `--workspace W --lsp typescript-language-server` for typescript and `--workspace W --lsp typescript-language-server` for javascript with a different `--language` flag — verified in the writing-plans phase via fresh grep).
- **I-V3-2 — 9 rows vs anomaly internal contradiction.** v2 said "exactly 9 LSP rows" (line 207) AND "both rows appear" on hub+legacy coexistence (line 214). v3 resolves: matrix renders exactly 9 rows total (one per language). When hub-managed AND direct-stdio entries coexist on the same (client, language) cell, the cell renders with BOTH state badges visible (e.g., `[via-hub][legacy]` chip stack), and the per-row drawer surfaces the anomaly with operator-resolution affordance.
- **I-V3-3 — endpoint flow contradiction.** v2 had `/api/daemon/respawn` AND env-handler-directly-triggers-IPC. v3 unifies: `/api/daemon/env` writes overlay and returns; GUI shows "Restart daemon to apply" button; click → `/api/daemon/respawn`. Same flow for Refresh discovery → button → respawn endpoint. No implicit IPC from env handler.
- **I-V3-4 — `WriteOverlay` mutator-error rollback contract.** Explicit: if mutator returns error, the on-disk file is NOT modified; flock is released; error propagates verbatim. Atomic temp+rename never happens unless mutator returned nil.
- **I-V3-5 — fail-LOUD scope ambiguity.** When YAML whole-file parse fails, supervisor cannot know which rows would have applied — so supervisor refuses to spawn ANY daemon and prints actionable guidance ("delete or fix the overlay; run `mcphub config overlay-quarantine` to rename and restart"). This is the conservative correct choice. Per-daemon failures (e.g. row syntactically valid but `${parent_path}` resolution fails) refuse only that daemon.
- **I-V3-6 — `mcphub config overlay-quarantine` is now precisely specified.** Acquires the same per-file flock as `WriteOverlay`; renames `daemon-env-overrides.yaml` → `daemon-env-overrides.yaml.corrupt-<RFC3339-ts>` via `os.Rename`; does NOT signal or restart the supervisor (operator restarts manually after fixing or accepting the quarantine).
- **I-V3-7 — Quarantined force-respawn shape resolved.** `/api/daemon/respawn` body accepts `{taskName, force: bool}`. Quarantined daemons require `force: true`; non-force requests return HTTP 409 with body `{state: "quarantined", remedy: "force or unquarantine"}`. GUI per-row drawer exposes the force flag as an explicit checkbox (default unchecked) so the operator confirms intent. Open Q4 from v2 is resolved.
- **I-V3-8 — auto-discovery `source: operator` compare-and-swap.** Explicit: discovery's `mutator` reads `source` field for each row under the flock; if `source == "operator"`, skip the binary's hint walk and preserve the row unchanged. The compare-and-swap is inside the same mutator transaction that writes the result, so concurrent operator Apply during discovery cannot lose the operator's row.
- **I-V3-9 — terms portability leak.** v2 hardcoded `/c/Users/dima_/go/bin/mcp-language-server`. v3 uses `$GOPATH/bin/mcp-language-server` and references the `go install` mechanism.

**Carried forward from v2 revision notes (all still valid):**

v2 fixed B1 (key format), B2 (Go gopls-mcp + recognition), B3 (`len(d.Env) > 0` gate removal — confirmed correct by codex), B4 (read-side hardening introduced — though v3 had to correct the references), B5 (YAML vs JSON — v3 sharpens), B6 (key namespace parked Q answered — v3 corrected direction), I1-I11 (most still valid; v3 specifically sharpens I3, I5, I7, I10).

## Goal

Make the Servers matrix the single place where an operator sees and manages every MCP server mcphub knows about, including the 9 LSP-bridge languages. Make each daemon's effective env (especially PATH) visible and editable from the GUI, with auto-discovery filling in sensible defaults at install time.

## Scope

In scope:

- Matrix recognition of LSP language entries (currently classified as "Other MCP entries") as language rows under the existing `mcp-language-server` manifest. Multi-workspace registrations supported via `ResolveEntryName` suffix-aware recognition + registry disambiguation.
- Exactly 9 LSP language rows in the Servers matrix (one per manifest language). When hub-managed and direct-stdio entries coexist on the same (client, language) cell, the cell renders both badges; no extra rows.
- Single `Active workspace` selector at the top of the Servers screen, scoping the check/uncheck and Register actions.
- Per-daemon `env` overlay file separate from shipped manifests. Supervisor merges manifest `env` with overlay `env` at spawn time (overlay wins on collisions, Windows case-insensitive key normalize).
- Auto-discovery of common binary install locations (Windows-focused) at `mcphub install/setup` time. Populates initial overlay file. Manual "Refresh discovery" button in the GUI re-runs at any time.
- Per-row drawer in the GUI showing effective env (post-merge) with edit affordance and `Apply` button. Apply persists to overlay; operator clicks "Restart daemon to apply" to trigger respawn.
- New read-side `daemon_env_overlay` package owning hardened YAML round-trip with comments preserved, flock-protected RMW, and the new internal `checkStateDirParentReadSafe` helper symmetric to the existing write-side.
- New `respawn` IPC command on the supervisor (replaces the v0.5.0 `restart`/`reload` UNKNOWN_COMMAND stub at `internal/cli/supervise.go:921`). Accepts `{taskName, force: bool}`; quarantined daemons require `force: true`.

Out of scope (deferred):

- Backend rewrite of `mcp-language-server` to serve multiple workspaces from one daemon. The current per-(workspace, language) proxy model stays.
- Cross-workspace router that picks a proxy by caller cwd. The active-workspace selector is operator-driven, not auto-routed.
- Linux/macOS systemd/launchd PATH inheritance fixes — out of scope for this design (POSIX paths inherit from the user's shell which usually has the needed tools).

## Architecture summary

Four pieces, mostly independent, glued through existing seams:

1. **Manifest schema additions (with Go struct extension).** Server manifest YAML gains optional `required_binaries: [name, ...]` at server level AND per-language level. Same commit adds the field to `ServerManifest` and `LanguageSpec` Go structs in `internal/config/manifest.go` so `ParseManifest`'s `KnownFields(true)` strictness does not reject the new keys. The field is free-form metadata only; `Validate()` does NOT enforce any binary-existence check.
2. **Auto-discovery engine.** New `internal/api/binary_discovery/` package: `Discover(ctx, requiredBinaries, hints) (map[binary]absolutePath, error)`. `hints` is a `[]string` parameter so unit tests inject synthetic roots; production callers use `binary_discovery.DefaultHints()` for the shipped per-OS list. Called by `mcphub install/setup` and by the GUI's `/api/discovery/refresh`. Discovery's mutator reads each row's `source` field under the overlay flock and skips rows tagged `source: operator` (compare-and-swap inside the same RMW transaction).
3. **Env overlay file (new package).** New `internal/api/daemon_env_overlay/` package owns: hardened YAML load (symlink refusal + parent DACL verify + 64 KiB size cap + owner check + UTF-8 validation), flock-protected RMW writer, and the `WriteOverlay(path, mutator func(*Overlay) error)` transactional API. Writes route through the existing exported `SecureWriteClientConfig(path string, payload []byte) error` (`internal/api/state_file_helper.go:127`) with YAML-marshaled bytes; reads use the new package-internal `checkStateDirParentReadSafe` helper. Supervisor `SupervisorDaemon.Env` is merged with the overlay at spawn time (overlay wins, Windows case-insensitive normalize). Storage path: `~/.config/mcp-local-hub/daemon-env-overrides.yaml` (POSIX) / `%LOCALAPPDATA%\mcp-local-hub\daemon-env-overrides.yaml` (Windows).
4. **Matrix LSP recognition + workspace scope.** Three changes in `internal/api/scan.go` + frontend:
   - Three-rule recognition algorithm (see "Matrix LSP recognition" below) parsing `ClientEntry.Raw map[string]any` directly, with ResolveEntryName suffix handling and registry disambiguation for multi-workspace.
   - Top-of-screen `Active workspace` selector. Default value = most-recent registered workspace from `~/.config/mcp-local-hub/workspaces.yaml`, OR a literal "(none — register a workspace first)" placeholder when empty.
   - Per-cell semantics: ☐ unchecked = no entry in this client; ☑ checked-direct (legacy direct-stdio entry, badge "legacy"); ☑ checked-hub (entry points at mcphub lazy proxy URL, badge "via-hub"). Cells with BOTH hub and legacy entries render both badges with operator-resolution affordance.

## Canonical daemon key namespace

Overlay file keys are `SupervisorDaemon.TaskName` strings as they appear in `supervisor-intent.json`, **WITH** the leading backslash. The `SupervisorDaemon.TaskName` field comment at `internal/api/supervisor_intent.go:25` documents this: `// canonical, e.g. "\\mcp-local-hub-memory-default"`. Reconcile indexes daemons by this canonical form (`internal/cli/supervise_reconcile.go:107`).

Three concrete task-name shapes apply:

| Daemon kind | TaskName format (canonical, stored) | Source |
|---|---|---|
| Global | `\mcp-local-hub-<server>-<daemon>` | derived at install from manifest; default daemon name is `default`. Examples: `\mcp-local-hub-gdb-default`, `\mcp-local-hub-memory-default`. |
| LSP workspace-scoped | `\mcp-local-hub-lsp-<wsKey>-<lang>` | `internal/api/register.go:292` writes `mcp-local-hub-lsp-%s-%s` (no leading `\`); the scheduler API call at `register.go:401` canonicalizes via the install code path before storage. |
| Hub-managed entry name (matrix recognition) | `mcp-language-server-<lang>` plus optional collision suffix `-<4hex>` or `-<8hex>` | this is the entry NAME in client configs, NOT the TaskName. `internal/api/register.go:722-747 ResolveEntryName` appends collision suffix when multiple workspaces register the same language. |

Overlay lookup at spawn time: `LookupOverlay(taskName string)` normalizes by prepending `\` if absent (idempotent — handles both forms), then exact-matches. This means an operator who hand-edits the YAML can use either form; the canonical storage form remains leading-backslash.

## Data flow

### Spawn-time env merge (modified)

```text
1. Supervisor reads supervisor-intent.json   → SupervisorDaemon{Env: M, TaskName: T}
2. Supervisor reads daemon-env-overrides.yaml → overlay[T].env = O  (hardened read)
3. mergeDaemonEnv(parent=os.Environ(), manifestEnv=M, overlayEnv=O)
     → expand ${parent_path} in O values using parent's PATH
     → merge with Windows case-insensitive normalize
     → precedence: parent < manifest < overlay
4. cmd.Env = merged
5. Daemon spawns with the merged env
```

`mergeDaemonEnv` at `internal/cli/supervise.go:1457` is extended to take a third `overlayEnv map[string]string` parameter. The gate at `internal/cli/supervise.go:1504-1505` (`if len(d.Env) > 0`) is REMOVED — the merge fires whenever EITHER `d.Env` OR overlay has any keys. When both are empty, cmd.Env is left nil so child inherits `os.Environ()` directly (unchanged from current behavior; verified by codex review of v2 B3).

### Auto-discovery at install (new)

```text
mcphub install / mcphub setup
  ↓
For each server manifest with required_binaries:
  For each binary in required_binaries:
    Walk DefaultHints() in OS-specific order until a hit
  Compose Path = first hit's parent dir per binary, joined with ${parent_path}
  WriteOverlay(mutator) under canonical TaskName key
  Inside mutator:
    For each daemon row to write:
      If existing row has source: operator → skip (preserve operator override)
      Else → write source: auto-discovery, discovered_at: <RFC3339Nano>
```

The mutator's compare-and-swap on `source: operator` happens under the same flock that protects the write, so a concurrent GUI Apply cannot lose its operator-tagged row.

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
  - taskName is in current supervisor-intent.json
  - keys/values contain no newlines / NUL / control chars
  - same CSRF + same-origin posture as /api/migrate, /api/dismiss
  ↓
WriteOverlay(path, mutator) — flock-protected RMW
  Inside mutator: write row with source: operator, modified_at: <ts>
  Mutator-error contract: if mutator returns err, NO write, flock released, err propagated
  ↓
Handler returns 200 + new effective env
  ↓
GUI shows "Restart daemon to apply" affordance; operator click → POST /api/daemon/respawn
```

### Respawn from GUI (new)

```text
GUI per-row drawer → "Restart daemon to apply" button (optionally with force checkbox)
  ↓
POST /api/daemon/respawn {taskName, force: bool}
  ↓
GUI handler validates:
  - taskName is in current supervisor-intent.json
  - CSRF + same-origin
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

### Matrix LSP recognition (modified)

`scan.go`'s per-client scan functions emit raw entries via `shapeClaudeEntry` / `shapeCodexEntry` / etc., all returning `ClientEntry{Transport, Endpoint, Raw}`. After scanning, a new pass categorizes entries using three rules:

```text
Inputs:
  ENTRIES   = scanned client-config entries keyed by entry-name
  LANGS     = the 9 manifest language names {clangd, fortran, go, javascript,
              python, rust, typescript, vscode-css, vscode-html}
  REGISTRY  = workspaces.yaml — for each workspace, ClientEntries map of
              client → entry-name (per internal/api/register.go:539-563)

For each (entryName, entry) in ENTRIES:

  // Strip ResolveEntryName collision suffix if present.
  // Match pattern: mcp-language-server-<lang>(-<4hex|8hex>)?
  // where <lang> is one of LANGS and the optional hex suffix is the
  // workspace key short or full form.
  baseLang, wsKeyMaybe = parseEntryName(entryName, LANGS)

  Rule 1 — Hub-managed lazy proxy:
    entry.Transport == "http"
    AND baseLang ∈ LANGS
    AND entry.Endpoint matches lazy-proxy URL pattern (typically
        http://localhost:<port>/mcp where port ∈ [9200, 9299] OR matches
        a port registered in REGISTRY for some (workspace, baseLang))
    → categorize as LSP entry for baseLang, badge="via-hub"
    → resolve owning workspace via REGISTRY lookup using wsKeyMaybe (or
      single-workspace fallback if no suffix)

  Rule 2 — Direct-stdio mcp-language-server invocation:
    entry.Transport == "stdio"
    AND filepath.Base(entry.Endpoint) == "mcp-language-server"  // or with .exe on Windows
    AND args slice (from entry.Raw["args"] []any → []string) contains
        a "--lsp" flag whose value matches one of LANGS' lsp_command fields
    → categorize as LSP entry, badge="legacy"
    → owning workspace inferred from "--workspace" arg if present

  Rule 3 — Direct-stdio gopls invocation (Go special case):
    entry.Transport == "stdio"
    AND filepath.Base(entry.Endpoint) == "gopls"
    AND args slice contains "mcp" (per manifest extra_flags for Go)
    → categorize as Go LSP entry, badge="legacy"

After categorization:
  - Always produce exactly 9 LSP language rows (one per manifest language).
  - For each (client, language) cell, the cell carries 0, 1, or 2 badges
    representing which of {via-hub, legacy} are present.
  - When BOTH via-hub and legacy are present for the same (client,
    language), the cell renders both badges stacked, and the per-row
    drawer surfaces the anomaly with operator-resolution affordance
    (recommend operator un-check legacy + Apply to migrate fully to hub).
```

**JS/TS disambiguation note** (codex v2 review A3): Rule 2's `lsp_command` match cannot distinguish javascript from typescript because both use `typescript-language-server`. Direct-stdio recognition therefore relies on the entry NAME (`mcp-language-server-javascript` vs `mcp-language-server-typescript`) for the language label, NOT the `--lsp` arg value. Rule 2's `--lsp` arg check is a defense-in-depth sanity gate (the args must be consistent with the entry name) but the language is determined by the entry-name prefix.

**Multi-workspace recognition correctness** (codex v2 review A2/E2 BLOCKING): when two workspaces both register clangd, the second workspace's entry name carries a `-<4hex>` or `-<8hex>` collision suffix (`internal/api/register.go:722-747`). The recognition algorithm strips the suffix to identify the language, then uses the suffix as a registry key to disambiguate workspace ownership. This makes multi-workspace recognition correct by construction; the active-workspace selector becomes a UX choice about which workspace's row is highlighted, NOT a correctness-determining gate.

## Components

| Component | New / Modified | Purpose | Owns | Depends on |
|---|---|---|---|---|
| `internal/api/binary_discovery/` | NEW | Auto-discover common binary paths per OS | shipped per-OS hints (`DefaultHints()`), hint-injection seam for tests, compare-and-swap on `source: operator` | (none) |
| `internal/api/daemon_env_overlay/` | NEW | YAML overlay file owner | overlay parse, flock-protected RMW writer, hardened read (with NEW `checkStateDirParentReadSafe`), comment-preserving YAML round-trip, `WriteOverlay(path, mutator func) error` transactional API with explicit "no-write-on-mutator-error" contract | `gopkg.in/yaml.v3` Node API, existing `SecureWriteClientConfig(path, payload []byte) error`, flock helper |
| `internal/cli/supervise.go` `mergeDaemonEnv` | MODIFY | Apply overlay at spawn-time; remove `len(d.Env) > 0` gate; Windows case-insensitive key normalize | env merge precedence, `${parent_path}` expansion | daemon_env_overlay |
| `internal/cli/supervise.go` IPC dispatcher | MODIFY | Add `respawn` IPC command at the case-switch around line 921; accepts `{taskName, force}`; replaces UNKNOWN_COMMAND stub | IPC frame parse, graceful 5s shutdown, spawn-from-intent-overlay, lifecycle event emit | reconcile loop, daemon_env_overlay |
| `internal/api/scan.go` | MODIFY | Recognize mcp-language-server entries via three-rule algorithm; emit LSP language rows | per-language entry classification, badge metadata | new `manifest_lsp_lookup.go`, registry reader |
| `internal/api/manifest_lsp_lookup.go` | NEW | Reverse-lookup helpers for the three-rule recognition; `parseEntryName(name, langs) (lang string, wsSuffix string)` | manifest reading, language set lookup, suffix-stripping regex | config package |
| `internal/gui/server.go` | NEW handlers | `/api/daemon/env` (write overlay, no IPC), `/api/discovery/refresh` (rescan + write overlay, no IPC), `/api/daemon/respawn` (IPC) | route registration, validation, IPC dispatch | supervisor IPC, daemon_env_overlay |
| `internal/gui/frontend/src/screens/Servers.tsx` | MODIFY | Active-workspace selector; 9 LSP rows; per-row drawer with env editor + restart button + force checkbox; `${parent_path}` warning chip when token absent | per-cell action mapping, drawer state, env edit form | new API endpoints |
| `servers/mcp-language-server/manifest.yaml` | MODIFY | Add `required_binaries` per language | manifest schema | (config schema) |
| `internal/config/manifest.go` | MODIFY | Add `RequiredBinaries []string` field to `ServerManifest` and `LanguageSpec` structs; no `Validate()` logic added | YAML deserialization (preserve `KnownFields(true)` strictness) | (none) |
| `servers/gdb/manifest.yaml` | MODIFY | Add `required_binaries: [gdb]` at server level | manifest schema | (config schema) |
| `servers/lldb/manifest.yaml` | MODIFY | Add `required_binaries: [lldb]` at server level | manifest schema | (config schema) |
| `internal/api/state_file_helper_read.go` | NEW | `checkStateDirParentReadSafe(dir string) error` — package-internal symmetric helper to existing `checkStateDirParentWriteSafe` | parent-dir DACL/mode check, default-relax + strict-mode semantics | (existing) operatorRequiresSingleUserHome, operatorAllowsUnhardenedStateRead (new env var) |

## Read-side hardening

The supervisor's spawn-time overlay READ is on the trust boundary for child process PATH. v3 introduces a NEW package-internal `checkStateDirParentReadSafe` helper symmetric to the existing `checkStateDirParentWriteSafe` (`internal/api/state_file_helper.go:155`). Read-side semantics:

1. **Open file first, then stat the handle.** Use `os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)` (POSIX) or equivalent `windows.CreateFile` with `FILE_FLAG_OPEN_REPARSE_POINT` cleared (Windows). The open itself refuses symlinks/reparse points. After open, call `fi := f.Stat()` on the open handle — defeats TOCTOU between any earlier check and the read. (Codex v2 review B1 BLOCKING: v2's `os.Lstat` pre-check was racy.)
2. **Reject non-regular files.** `if !fi.Mode().IsRegular() { return ErrOverlayNotRegular }`. Use `Mode().IsRegular()`, NOT bit math on `os.ModeIrregular` (codex v2 review B2 corrected v2's wrong bit math).
3. **Verify parent dir via new `checkStateDirParentReadSafe`**. Same default-relax + strict-mode semantics as the existing write side (`internal/api/state_file_helper.go:139-157`):
   - If `MCPHUB_REQUIRE_SINGLE_USER_HOME=1` is set, refuse with verbose error including the env var name and remedy.
   - Else, run the parent-write-bits check (`checkStateDirParentWriteSafe` is re-used internally — the read side rejects parents that grant write/delete to non-allowlisted principals because a co-resident attacker could swap the file).
   - Else, log warn `daemon-env-overlay-read-unhardened-fallback` and proceed (default-relax for legitimate corp hosts).
4. **Refuse non-owner file.** POSIX: stat uid == os.Getuid(); Windows: file owner SID matches process token user SID.
5. **64 KiB hard size cap.** `io.LimitReader(f, 65536+1)` then check the byte count.
6. **Reject non-printable / non-UTF-8 bytes.** Defense-in-depth before YAML parse.

If any hardened-read failure occurs AND the operator has not set `MCPHUB_REQUIRE_SINGLE_USER_HOME=0` to opt out of the strict gate, supervisor refuses to spawn (per "Error handling" below).

## Error handling

| Failure mode | Behavior |
|---|---|
| Overlay file missing | Treat as empty overlay. No error. Manifest env applies. |
| Overlay file unreadable (permission denied / symlink at path / reparse point on Windows) | **Fail-LOUD.** Supervisor refuses to spawn ANY daemon (overlay parse failure means "affected daemons" is unknowable). GUI shows red banner: `daemon-env-overlay-read-rejected`. Audit event with reason. Operator runs `mcphub config overlay-quarantine` to rename + restart with empty overlay. |
| Overlay file corrupt YAML / size > 64 KiB / non-UTF-8 | **Fail-LOUD.** Same as above; parse error includes line/col for inline editor jump. |
| Overlay declares an unknown taskName | Log warn `daemon-env-overlay-orphan-row` at supervisor startup. Ignore that row at spawn. GUI surfaces orphan rows in a dedicated section with "delete this row" affordance. Triggered automatically after `mcphub unregister <workspace>` removes the workspace from registry. |
| Overlay row exists but `${parent_path}` resolution fails | Per-row failure only; refuse to spawn that one daemon; emit `daemon-env-overlay-parent-path-resolve-failed` with daemon TaskName + missing key. |
| Auto-discovery cannot find a required binary | Discovery returns `{binary: ""}` for the missing one. Overlay row written with comment `auto-detected: BINARY_NAME not found in any common location`. GUI shows red flag on the daemon's row with a search hint and a "set PATH manually" link. |
| Apply IPC respawn fails | GUI surfaces the supervisor error; overlay change stays on disk; operator can retry. No state mutation rollback. |
| Respawn requested for Quarantined daemon without `force: true` | HTTP 409 from `/api/daemon/respawn`; body explains remedy. |
| `mcphub register <ws> <lang>` fails | Existing register error handling unchanged. GUI displays the per-cell failed row, retry affordance via second Apply. |
| Concurrent GUI Apply + auto-discovery refresh | Both go through `WriteOverlay(path, mutator)`'s flock; second caller waits. No lost-update window. |
| `WriteOverlay` mutator returns error | NO write to disk; flock released; error propagated verbatim. The on-disk file is unchanged. |

## `mcphub config overlay-quarantine`

Rename the overlay file aside so the supervisor can boot with empty overlay:

1. Acquire the same per-file flock as `WriteOverlay` (path: `daemon-env-overrides.yaml.lock`).
2. If the overlay file does not exist → exit 0 with message "no overlay to quarantine".
3. `os.Rename(<overlay>, <overlay>.corrupt-<RFC3339-ts>)` — atomic on POSIX, atomic-by-rename on Windows.
4. Release flock.
5. Print operator guidance: "renamed to `<new-path>`. Run `mcphub restart` (or wait for next supervisor cold start) to apply."

Does NOT signal or restart the supervisor; operator restarts manually after fixing or accepting the quarantine. The `.corrupt-<ts>` files accumulate; cleanup is operator's responsibility (or a future `mcphub config gc-overlay-quarantines` follow-up).

## Manifest schema additions

Add optional `required_binaries` field at server OR language level. **Same commit** adds the field to the Go structs (`ServerManifest`, `LanguageSpec` in `internal/config/manifest.go`) so `ParseManifest`'s `KnownFields(true)` strictness still rejects truly unknown keys. The new field is free-form metadata; `Validate()` does NOT enforce binary existence (deferred — discovery does best-effort find at install time).

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

## DefaultHints — shipped per-OS list

`binary_discovery.DefaultHints()` returns a `[]string` for the current OS. Windows hints (verified-by-author install locations):

- `C:\msys64\ucrt64\bin`, `C:\msys64\mingw64\bin`, `C:\msys64\clang64\bin`
- `C:\Program Files\LLVM\bin`
- `C:\Program Files\Go\bin`
- `C:\Program Files\Microsoft Visual Studio\2022\BuildTools\VC\Tools\Llvm\x64\bin`
- `%LOCALAPPDATA%\Programs\Python\Python311` through `Python314`
- `%USERPROFILE%\.cargo\bin`, `%USERPROFILE%\go\bin`, `%USERPROFILE%\.local\bin`
- `%LOCALAPPDATA%\fnm_multishells`, `%LOCALAPPDATA%\Programs\fnm`, `%LOCALAPPDATA%\nvm`
- `%APPDATA%\npm`

Linux hints: `/usr/local/bin`, `/usr/bin`, `~/.local/bin`, `~/.cargo/bin`, `~/go/bin`.

macOS hints: `/opt/homebrew/bin`, `/usr/local/bin`, `/opt/local/bin` + the Linux list.

Hints are best-effort seed data only. **The operator's override channel is the env overlay file, NOT hints expansion.** Adding a hint requires a code change + PR.

For unit tests, `Discover(ctx, requiredBinaries, hints []string)` takes `hints` as a parameter — tests pass synthetic temp-dir roots; production callers use `DefaultHints()`.

## Security

### Write-side hardening (existing posture extended)

- Overlay file lives in user-scope dir (per-machine, per-user). Same DACL/mode posture as other state files.
- `WriteOverlay` writes through the existing exported `SecureWriteClientConfig(path string, payload []byte) error` (`internal/api/state_file_helper.go:127`) with YAML-marshaled bytes. Per-file DACL at temp-create is handle-bound (POSIX) / NtCreateFile-bound (Windows) per the existing pipeline.

### Read-side hardening (new — see "Read-side hardening" section above)

- New package-internal `checkStateDirParentReadSafe` helper.
- Open-first-then-stat-handle pattern defeats TOCTOU between Lstat pre-check and Open.
- `Mode().IsRegular()` (not bit math) for non-regular refusal.
- Default-relax / strict-mode semantics symmetric with write side.
- 64 KiB size cap + UTF-8 validation.

### `/api/daemon/env`, `/api/discovery/refresh`, `/api/daemon/respawn` auth posture

All three endpoints follow the same auth posture as `/api/migrate`, `/api/dismiss`, `/api/secrets` (existing canonical mutation surface):

- Loopback-only listener (already enforced at server bind).
- Same-origin policy with strict Origin header check.
- CSRF token discipline: `X-Mcphub-CSRF` header matched against the per-session token issued at GUI bootstrap.
- **Known-task validation.** Server rejects the request with HTTP 400 if `taskName` is not present in current `supervisor-intent.json`.
- **Per-key value validation** (for `/api/daemon/env`): Keys must match `[A-Za-z_][A-Za-z0-9_]*`; values reject newline, NUL, control chars.

Verification gate: the writing-plans phase MUST verify that `/api/migrate`, `/api/dismiss`, `/api/secrets` actually enforce CSRF + Origin checks in the live code before this spec's "same posture as" claim becomes binding. If any of them is laxer than claimed, the implementation phase tightens the lax endpoint to match this spec's posture, not the other direction.

### `${parent_path}` token semantics

- The ONLY supported template token. Operator's overlay value is treated as opaque bytes after expansion; values go to `exec.Command` env block, not a shell.
- Token resolution: at spawn time, supervisor reads `os.Environ()` and finds the PATH key (case-insensitive on Windows: `Path`, `PATH`, `path` all match the same logical key). Expansion is single-pass, non-recursive — the expanded value is not re-scanned for tokens.
- If `${parent_path}` is omitted from the operator's PATH value, the parent PATH is **dropped** for that daemon. The GUI shows a warning chip "PATH does not include `${parent_path}` — parent PATH will be DROPPED for this daemon" when the operator types or pastes a PATH value missing the token. Auto-discovery templates always include the token at the tail to preserve parent.
- **YAML-editor bypass**: an operator who hand-edits the overlay YAML and omits the token bypasses the GUI warning. The supervisor emits a `daemon-env-overlay-path-no-parent-token` info event at spawn for any daemon whose overlay PATH lacks the token — visible in `mcphub watchdog status` / event log. The token is not auto-injected server-side (operator might intentionally want to drop parent PATH for a hermetic daemon).

## Observability

New events on the hub-mcp event log:

- `daemon-env-overlay-loaded` (info): row count, `sha256(${parent_path})[:12]` of resolved parent (full PATH only at debug level)
- `daemon-env-overlay-load-failed` (warn): path, error class (symlink-refused, size-exceeded, parse-failed, parent-dir-unsafe, non-owner), line/col for parse-failed
- `daemon-env-overlay-read-rejected` (error): path, hardened-read failure mode — supervisor refuses to spawn ANY daemon (whole-file fail-LOUD)
- `daemon-env-overlay-read-unhardened-fallback` (warn): default-relax path on legitimate corp host; same posture as existing write-side warn
- `daemon-env-overlay-applied-via-gui` (info): taskName, changed keys (values redacted; key names + before/after hash at debug only)
- `daemon-env-overlay-orphan-row` (warn): taskName not in current intent
- `daemon-env-overlay-skipped-operator-override` (info): taskName, binary — auto-discovery refused to overwrite `source: operator`
- `daemon-env-overlay-parent-path-resolve-failed` (warn): per-row resolution failure; refuse to spawn that one daemon
- `daemon-env-overlay-path-no-parent-token` (info): daemon spawned with PATH that omits `${parent_path}` — parent PATH dropped intentionally or by operator hand-edit
- `binary-discovery-ran` (info): server, scan duration, hits per binary
- `binary-discovery-missing` (warn): server, binary, scanned hints summary
- `supervisor-respawn-via-gui` (info): taskName, requesting client (loopback PID), respawn outcome, `force` flag value
- `supervisor-respawn-graceful-timeout` (warn): taskName, soft-shutdown deadline exceeded, force-kill path taken
- `supervisor-respawn-refused-quarantined` (info): taskName, request lacked `force: true`

## Testing strategy

### Unit

- `binary_discovery/discover_test.go`: pass `hints=[<tempdir>]` with synthesized fake binaries; assert correct paths and missing-binary handling. **Test-injected hints** decouples test from real machine.
- `binary_discovery/source_override_test.go`: pre-write overlay row with `source: operator`; run discovery's mutator; assert row preserved unchanged AND no `discovered_at` overwrite.
- `daemon_env_overlay/parse_test.go`: round-trip overlay YAML preserving comments; ordered keys; reject open-time symlink; reject 64 KiB+1; reject non-UTF-8.
- `daemon_env_overlay/write_test.go`: `WriteOverlay(path, mutator)` flock-protects concurrent writes; atomic temp+rename; DACL/mode at temp-create matches existing write helper; mutator-error path produces NO disk modification + flock release + error propagation.
- `daemon_env_overlay/read_hardening_test.go`: new `checkStateDirParentReadSafe` symmetric with `checkStateDirParentWriteSafe`; `Mode().IsRegular()` rejects directories + pipes + sockets; default-relax fallback on parent-DACL rejection with `MCPHUB_REQUIRE_SINGLE_USER_HOME` unset; strict-mode rejection with env var set.
- `mergeDaemonEnv` extended tests: overlay arg non-nil; assert precedence parent < manifest < overlay; Windows case-insensitive key collision (`PATH` vs `Path` merge); both-empty fallback to nil cmd.Env.
- `scan.go` three-rule recognition: test fixtures for (a) hub URL entries via `mcphub register` with and without collision suffix, (b) direct-stdio `mcp-language-server --lsp X` for JS+TS sharing `typescript-language-server`, (c) direct-stdio `gopls mcp` for Go; assert all map to right LSP language row with right badge; multi-workspace test asserts both rows render against same language with workspace disambiguation.
- `manifest_lsp_lookup_test.go`: `parseEntryName(name, langs)` for all 9 languages, with and without suffix, with full 8-hex and short 4-hex suffix forms; reject `mcp-language-server-unknown-lang`.

### Integration

- `internal/cli/supervise_lsp_e2e_test.go` (new): synthesize a workspace, register one language via `mcphub register`, verify supervisor-intent.json gains the entry with canonical leading-backslash TaskName, verify supervisor spawns a lazy proxy on materialization, verify env from overlay applies.
- `internal/cli/supervise_respawn_test.go` (new): IPC `respawn` command with valid TaskName + `force=false` → graceful 5s shutdown → spawn with intent+overlay; invalid TaskName → IPC error `UNKNOWN_TASK`; daemon in Backoff → state reset to Spawning + spawn; daemon in Quarantined + `force=false` → IPC refuse; daemon in Quarantined + `force=true` → spawn proceeds.
- `internal/gui/daemon_env_handler_test.go` (new): POST /api/daemon/env writes overlay (no IPC); assert event log entry + new effective env via /api/status after operator clicks /api/daemon/respawn; assert CSRF rejected without header; assert unknown taskName rejected with 400.
- `internal/api/daemon_env_overlay/integration_test.go`: WriteOverlay + Load round-trip across multiple goroutines under flock.

### Multi-workspace LSP recognition test

`internal/gui/e2e/tests/servers-lsp-multi-workspace.spec.ts`:

1. Seed 2 workspaces in registry (`ws1` + `ws2`), both registered for clangd. Second registration triggers ResolveEntryName → entry name `mcp-language-server-clangd-<short-hex>` in ws2's clients.
2. Active-workspace selector defaults to ws1; matrix clangd row shows URL for ws1 proxy; ws2's clangd row is the SAME row (one row per language total) with the ws2 cells visible for ws2-bound clients.
3. Switch selector to ws2; assert per-row drawer's active-workspace badge updates.
4. Per-row drawer surfaces per-workspace history: both proxies listed with badges + register/unregister timestamps from the registry.
5. Uncheck clangd cell for codex client while selector is ws2 → `mcphub register --unset` runs against ws2 only; ws1's clangd row unchanged.

### E2E (Playwright)

- `internal/gui/e2e/tests/servers-lsp.spec.ts`: matrix shows exactly 9 LSP rows; check/uncheck cycles register/unregister per active workspace; per-row drawer opens with env editor + Restart button + force checkbox; click Restart respawns daemon and Dashboard reflects new PID.
- `internal/gui/e2e/tests/servers-env-overlay.spec.ts`: per-row drawer reveals effective env; edit Path field WITHOUT `${parent_path}` token → warning chip appears; Apply writes overlay; click Restart triggers respawn; GUI shows new effective env post-respawn.
- `internal/gui/e2e/tests/servers-coexistence-anomaly.spec.ts`: seed both hub-managed clangd AND direct-stdio mcp-language-server entry for same client+language; assert cell renders both `[via-hub]` and `[legacy]` badges; per-row drawer surfaces remediation guidance.

## Migration

For existing installs:

- **Overlay file does not exist** → supervisor merges only `os.Environ()` + `SupervisorDaemon.Env` (same as today). No behavior change.
- **LSP-recognition addition** → rows that previously appeared in "Other MCP entries" now appear as proper matrix rows. Clients keep their existing direct-stdio entries; matrix offers explicit migrate-to-hub action with a "legacy" badge.
- **`mcphub install/setup` auto-discovery** → runs once (writes initial overlay file with `source: auto-discovery`). Existing operators get a one-time `binary-discovery-ran` event. Default templates always include `${parent_path}` at the tail.
- **`mcphub unregister <workspace>`** → emits `daemon-env-overlay-orphan-row` for stale `\mcp-local-hub-lsp-<wsKey>-*` taskNames. `mcphub config prune-orphan-overlay-rows` removes them; GUI surfaces "Clean up orphan overlay rows" affordance.

## Open questions parked for plan / implementation phase

1. **GUI UX shape for `Active workspace` selector**: top-of-screen vs per-row dropdown? The recognition algorithm is correct either way (workspace is determined by registry lookup against the matched entry); the question is purely about discoverability. Proposed: top-of-screen with per-row badge showing "active: ws1" when current selection has a row for the language.
2. **Auto-discovery scope for non-LSP servers**: `gdb`, `godbolt`, `perftools` need external binaries. Server-level `required_binaries` is added to each manifest with external deps; same discovery engine handles both. Open: should `lldb` manifest's `command: mcphub` (internal bridge) declare no required_binaries (natural — internal bridges have no external deps)?
3. **GUI "Active workspace" persistence**: per-machine OR per-GUI-session? Proposed: per-machine setting in `gui-preferences.yaml`, persisted across GUI restarts.

(v2's parked question about Quarantined-force semantics is now resolved in the body; v3 carries forward only the genuinely-open three.)

## Terms and Abbreviations

- **LSP**: Language Server Protocol; a JSON-RPC protocol exposing IDE-grade code intelligence over a stdio/socket channel.
- **mcp-language-server**: Go binary installed via `go install` to `$GOPATH/bin/mcp-language-server`; bridges an LSP server to the MCP protocol.
- **gopls-mcp**: alternative backend used by the Go language. The `gopls` binary supports an `mcp` subcommand exposing MCP directly without the mcp-language-server bridge. See manifest line 33-37.
- **Hub-proxy / lazy proxy**: a workspace-scoped mcphub daemon listening on a port from the 9200-9299 pool and forwarding MCP traffic to the heavy LSP backend.
- **Direct-stdio / legacy**: an MCP client config entry that invokes `mcp-language-server` or `gopls` directly via stdio, bypassing mcphub. The matrix surfaces these with a "legacy" badge.
- **Workspace-scoped**: a server kind where each (workspace, language) pair gets its own daemon. Contrast with `kind: global`.
- **Overlay**: a user-editable layer that augments shipped manifests without modifying them.
- **Active workspace**: the workspace whose `mcphub register`-ed lazy proxies are addressed by Servers matrix actions in the GUI.
- **`${parent_path}` token**: a placeholder in the overlay's env values that expands at spawn time to the supervisor process's PATH.
- **wsKey**: the 8-character hex hash of a workspace path. Short form is the first 4 hex chars; ResolveEntryName uses short by default and falls back to full 8 on first-4 collision.
- **ResolveEntryName**: `internal/api/register.go:722-747`; computes the client-config entry name with collision suffix when same-language multi-workspace forces it.
- **SupervisorDaemon.TaskName**: the canonical identifier in supervisor-intent.json, IPC commands, and overlay keys. STORED WITH leading backslash per `internal/api/supervisor_intent.go:25`; `LookupOverlay` accepts either form and normalizes.
- **flock**: POSIX `flock(2)` / Windows `LockFileEx` advisory lock; here serializes RMW on the overlay file across processes.
- **CSRF**: Cross-Site Request Forgery; per-session token issued at GUI bootstrap; required header for mutation endpoints to prevent drive-by writes from a malicious browser tab.
- **TOCTOU**: Time-Of-Check-To-Time-Of-Use race; the gap between a check (e.g., `Lstat`) and a subsequent use (e.g., `Open`) that an attacker can exploit. v3's read-side hardening uses open-first-then-stat-handle to close this window.

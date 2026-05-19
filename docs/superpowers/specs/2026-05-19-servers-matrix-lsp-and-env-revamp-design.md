# Servers matrix revamp: LSP-bridge integration + per-daemon env overlay

**Status:** Spec, draft v1, pending user review.

**Tracking PR(s):** will be filled by writing-plans phase. Likely 1 PR with 4 phases (auto-discovery, overlay+merge, LSP recognition, GUI surface), or split if any phase grows too large.

**Closes / unblocks:**

- [work-items/bugs/2026-05-19-...] (will be filed during implementation if discovered)
- Operator UX gap: 9 LSP language entries (clangd, fortran, go, javascript, python, rust, typescript, vscode-css, vscode-html) currently show under "Other MCP entries (N)" because matrix does not recognize them as languages of the `mcp-language-server` workspace-scoped manifest. They cannot be managed from the GUI matrix today.
- Operator UX gap: `mcphub` daemons spawned by the supervisor task inherit a Task-Scheduler-logon PATH that lacks common Windows tool install locations (`C:\msys64\ucrt64\bin`, `C:\Program Files\LLVM\bin`, etc.). `gdb` MCP daemon answers but reports "GDB/LLDB not available"; `mcp-language-server` would have the same problem for `clangd`, `gopls`, `pyright`, `rust-analyzer`, etc.

## Goal

Make the Servers matrix the single place where an operator sees and manages every MCP server mcphub knows about, including the 9 LSP-bridge languages. Make each daemon's effective env (especially PATH) visible and editable from the GUI, with auto-discovery filling in sensible defaults at install time.

## Scope

In scope:

- Matrix recognition of LSP language entries (currently classified as "Other MCP entries") as language rows under the existing `mcp-language-server` manifest.
- Per-language rows in the Servers matrix with the same check/uncheck affordances existing global-server rows already have.
- Single `Active workspace` selector at the top of the Servers screen (or near the LSP rows section) that scopes the check/uncheck and Register actions.
- Per-daemon `env` overlay file separate from shipped manifests. Supervisor merges manifest `env` with overlay `env` at spawn time (overlay wins on collisions).
- Auto-discovery of common binary install locations (Windows, Linux, macOS) at `mcphub install/setup` time. Populates initial overlay file. Manual "Refresh discovery" button in the GUI re-runs at any time.
- Per-row drawer in the GUI showing effective env (post-merge) with edit affordance and `Apply` button. Apply persists to overlay + IPC-signals supervisor to respawn the affected daemons.

Out of scope (deferred):

- Backend rewrite of `mcp-language-server` to serve multiple workspaces from one daemon. The current per-(workspace, language) proxy model stays.
- Cross-workspace router that picks a proxy by caller cwd. The active-workspace selector is operator-driven, not auto-routed.
- Linux/macOS systemd/launchd PATH inheritance fixes — out of scope (this design targets Windows Task Scheduler primarily; POSIX paths inherit from the user's shell which usually has the needed tools).

## Architecture summary

Four pieces, mostly independent, glued through existing seams:

1. **Manifest schema additions.** Server manifest YAML gains optional `required_binaries: [name, ...]` per server (or per language for workspace-scoped servers). Used by auto-discovery only — runtime spawn is unchanged when omitted.
2. **Auto-discovery engine.** New `internal/api/binary_discovery/` package: `Discover(server, requiredBinaries)` returns `map[binary]absolutePath` by scanning a shipped `searchHints` list per OS. Called by `mcphub install/setup` and by GUI `/api/discovery/refresh`.
3. **Env overlay file.** New `internal/api/daemon_env_overlay.go`: read `~/.config/mcp-local-hub/daemon-env-overrides.yaml` (POSIX) / `%LOCALAPPDATA%\mcp-local-hub\daemon-env-overrides.yaml` (Windows). Supervisor `SupervisorDaemon` descriptor's `Env` map is merged with the overlay at spawn time (overlay wins). Shape:

   ```yaml
   version: 1
   daemons:
     "\\mcp-local-hub-gdb-default":
       env:
         Path: "C:/msys64/ucrt64/bin;${parent_path}"
     "\\mcp-local-hub-mcp-language-server-clangd-<workspace-hash>":
       env:
         Path: "C:/Program Files/LLVM/bin;${parent_path}"
   ```

   `${parent_path}` token resolves at spawn time to the supervisor process's current PATH; this avoids losing inherited PATH when the overlay sets a new one.

4. **Matrix LSP recognition + workspace scope.** Three changes in `internal/api/scan.go` + frontend:
   - When a client config has an entry whose `command == "mcp-language-server"`, recognize it as an LSP language entry. Group under one row per language (9 rows added).
   - Top-of-screen `Active workspace` selector. Default value = result of "best-effort cwd resolution" (most-recent registered workspace, OR the GUI process's cwd, OR a literal "(none — register a workspace first)" placeholder).
   - Per-cell semantics: ☐ unchecked = no entry in this client; ☑ checked-direct (legacy direct-stdio entry, badge "legacy"); ☑ checked-hub (entry points at mcphub lazy proxy URL). Operator unchecks a "direct" cell + Apply → removes entry; checks an unchecked cell + Apply → `mcphub register <active-workspace> <language> --clients <this>` runs (replacing any direct-stdio entry with the lazy-proxy HTTP URL).

## Data flow

### Spawn-time env merge (new)

```text
1. Supervisor reads supervisor-intent.json   → SupervisorDaemon{Env: M}
2. Supervisor reads daemon-env-overrides.yaml → overlay[taskName].env  = O
3. spawnFn merges:  cmd.Env = os.Environ() + (M overridden by O, with ${parent_path} expanded)
4. Daemon spawns with the merged env
```

The existing `mergeDaemonEnv` helper (`internal/cli/supervise.go:1457`) is extended; the spawn function reads overlay once per spawn (best-effort; missing/corrupt overlay logs warn + proceeds with manifest env only).

### Auto-discovery at install (new)

```text
mcphub install / mcphub setup
  ↓
For each server manifest with required_binaries:
  For each binary in required_binaries:
    Walk searchHints in OS-specific order until a hit
  Compose PATH = first hit per binary, joined with ${parent_path}
  Write to overlay file under taskName key
```

### Manual discovery refresh from GUI (new)

```text
GUI Settings → Daemons / per-row drawer → "Refresh discovery"
  ↓
POST /api/discovery/refresh {server, daemon (or 'all')}
  ↓
GUI handler invokes binary_discovery.Discover for the row
  ↓
Updates overlay file
  ↓
Returns new effective env to GUI for display
  ↓
GUI shows "Restart daemon to apply" affordance OR auto-restarts (operator setting)
```

### Apply env edit from GUI (new)

```text
GUI per-row drawer → Edit PATH field → Apply
  ↓
POST /api/daemon/env {taskName, env: {KEY: value, ...}}
  ↓
GUI handler validates + writes overlay file (atomic temp+rename per state-file helper)
  ↓
GUI handler issues supervisor IPC: respawn {taskName}
  ↓
Supervisor merges manifest env + overlay env, spawns daemon with new PATH
  ↓
GUI polls /api/status, surfaces new PID + Port + State
```

### Matrix LSP recognition (new)

```text
scan.go's per-client scan functions emit entries keyed by entry-name.
After scanning:
  For each entry whose underlying command == "mcp-language-server":
    Look up entry name in mcp-language-server manifest's languages list
    If matched:
      Re-key the entry under "mcp-language-server.LANG"   # e.g. "mcp-language-server.clangd"
      Annotate with: language, lsp_command, backend type (mcp-language-server / gopls-mcp)
      Add to a new entry-type "lsp-language"
After:
  Build matrix rows for entries; non-LSP entries unchanged; each LSP language renders as one INDIVIDUAL row (9 rows total — one per manifest language). Per user decision: per-language axis, not grouped under a "mcp-language-server" parent.
```

## Components

| Component | New / Modified | Purpose | Owns | Depends on |
|---|---|---|---|---|
| `internal/api/binary_discovery/` | NEW | Auto-discover common binary paths per OS | shipped `searchHints` (Windows / Linux / macOS arrays) | (none) |
| `internal/api/daemon_env_overlay.go` | NEW | Load + merge per-daemon env overlay from disk | overlay YAML schema, read + atomic write | state-file helper |
| `internal/cli/supervise.go` `mergeDaemonEnv` | MODIFY | Apply overlay at spawn-time | env merge precedence, `${parent_path}` expansion | daemon_env_overlay |
| `internal/api/scan.go` | MODIFY | Recognize mcp-language-server entries; group as LSP rows | per-language entry classification | mcp-language-server manifest |
| `internal/api/manifest_lsp_lookup.go` | NEW | Reverse-lookup: given a stdio entry, is it one of mcp-language-server's 9 languages? | manifest reading, language-to-binding map | config package |
| `internal/gui/install.go` (or new `daemon_env.go`) | NEW handlers | `/api/daemon/env` (write), `/api/discovery/refresh` (rescan), `/api/respawn-daemon` (IPC) | route registration, validation, IPC dispatch | supervisor IPC, overlay writer |
| `internal/cli/supervise.go` IPC handler | MODIFY | Add `respawn` IPC command — restart one daemon by taskName, applying env from current intent+overlay | IPC frame parse, reconcile signal | (existing) reconcile loop |
| `internal/gui/frontend/src/screens/Servers.tsx` | MODIFY | Active-workspace selector; LSP rows; per-row drawer with env editor | per-cell action mapping, drawer state | new API endpoints |
| `servers/mcp-language-server/manifest.yaml` | MODIFY | Add `required_binaries` per language (clangd, fortran-language-server, etc.) | manifest schema | (config schema) |

## Manifest schema additions

Add optional `required_binaries` field at server OR language level:

```yaml
# Server-level (applies to whole server)
required_binaries:
  - gdb
  - lldb

# Per-language (workspace-scoped servers)
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
```

`required_binaries` is metadata only — runtime spawn never reads it. Auto-discovery uses it to know what to look for.

## searchHints (shipped per-OS list)

```go
// internal/api/binary_discovery/hints_windows.go
var searchHintsWindows = []string{
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
    `%LOCALAPPDATA%\fnm_multishells`,                  // node version manager
    `%LOCALAPPDATA%\Programs\fnm`,
    `%LOCALAPPDATA%\nvm`,
    `%APPDATA%\npm`,
}

// hints_linux.go: /usr/local/bin, /usr/bin, ~/.local/bin, ~/.cargo/bin, ~/go/bin
// hints_darwin.go: /opt/homebrew/bin, /usr/local/bin, /opt/local/bin + the Linux list
```

The hints list is intentionally finite and committed to mcphub. The operator's override channel is the env overlay file, not the hints list (avoids one more user-config drift surface). Adding a hint requires a code change + PR.

## Error handling

| Failure mode | Behavior |
|---|---|
| Overlay file missing | Treat as empty overlay. No error. Manifest env applies. |
| Overlay file unreadable / corrupt YAML | Log warn `daemon-env-overlay-load-failed`. Manifest env applies. GUI shows the load error banner so operator can fix. |
| Overlay declares an unknown taskName | Log warn `daemon-env-overlay-orphan-row`. Ignore that row at spawn. GUI surfaces orphan rows with "delete this row" affordance. |
| Auto-discovery cannot find a required binary | Discovery returns `{binary: ""}` for the missing one. Overlay row written with comment `auto-detected: BINARY_NAME not found in any common location`. GUI shows red flag on the daemon's row with a search hint. |
| Apply IPC respawn fails | GUI surfaces the supervisor error; overlay change stays on disk; operator can retry. No state mutation rollback. |
| `mcphub register <ws> <lang>` fails | Existing register error handling unchanged. GUI displays the per-cell failed row, retry affordance via second Apply. |

## Security

- Overlay file lives in user-scope dir (per-machine, per-user). Same DACL/mode posture as other state files (handle-bound 0600 / Admin+SYSTEM+CurrentUser ACL).
- `Apply env` GUI path validates input: forbid newlines / NUL / control chars; PATH separator policy per-OS; values are not interpreted as shell. The value is passed verbatim to spawn's env block.
- The `${parent_path}` expansion is the only template; expanded value comes from `os.Environ()` of the supervisor — same trust level as any spawn env inheritance.
- No new auth surface — `/api/daemon/env` follows the same loopback-only + same-origin policy as other GUI mutation endpoints.

## Observability

New events on the hub-mcp event log:

- `daemon-env-overlay-loaded` (info, on each supervisor startup): paths, row count, ${parent_path} resolution result
- `daemon-env-overlay-load-failed` (warn): path, err
- `daemon-env-overlay-applied-via-gui` (info): taskName, changed keys (values redacted)
- `binary-discovery-ran` (info): server, scan duration, hits per binary
- `binary-discovery-missing` (warn): server, binary, scanned hints, "set PATH manually in GUI" guidance
- `supervisor-respawn-via-gui` (info): taskName, requesting client (IP/PID from loopback)

## Testing strategy

Unit:

- `binary_discovery/discover_test.go`: synthesize a temp dir with fake binaries at known hint paths; assert discovery returns the right absolute paths; assert missing binaries return empty.
- `daemon_env_overlay_test.go`: round-trip overlay YAML; load + merge with manifest env; precedence (overlay wins); `${parent_path}` expansion.
- `mergeDaemonEnv` extended tests: overlay arg now non-nil; ensure os.Environ() + manifest + overlay precedence is correct on Windows (case-insensitive key collision) and POSIX (case-sensitive).
- `scan.go` LSP recognition: test fixtures with mcp-language-server entries in claude/codex/cursor; assert they group under "mcp-language-server.<lang>" not "Other".

Integration:

- `internal/cli/supervise_lsp_e2e_test.go` (new): synthesize a workspace, register one language via `mcphub register`, verify supervisor-intent.json gains the entry, verify supervisor spawns a lazy proxy on materialization, verify env from overlay applies.
- `internal/gui/install.go` test: POST /api/daemon/env writes overlay + triggers respawn; assert event log entry + new effective env via /api/status.

E2E (Playwright):

- `internal/gui/e2e/tests/servers-lsp.spec.ts`: matrix shows 9 LSP rows; check/uncheck cycles register/unregister per active workspace; per-row drawer opens with env editor; Apply respawns daemon.

## Migration

For existing installs:

- Overlay file does not exist → supervisor spawns daemons with manifest env only (same as today). No behavior change.
- LSP-recognition addition: rows that previously appeared in "Other MCP entries" now appear as proper matrix rows. Backwards-compatible: clients keep their existing direct-stdio entries; matrix offers explicit migrate-to-hub action with a "legacy" badge so operator chooses when to switch.
- `mcphub install/setup` auto-discovery runs once (writes initial overlay file with the binaries it finds). Existing operators get a one-time `binary-discovery-ran` event. No surprise behavior change — overlay only ADDS to PATH, never REMOVES.

## Open questions parked for plan / implementation phase

1. **Multi-workspace UX for LSP**: when user has 2+ workspaces registered for the same language, what does the matrix cell look like? Proposed answer: cell shows "via-hub (active workspace)" with the active workspace's URL. Switching active workspace dropdown re-keys cells. Per-workspace history visible in row drawer.
2. **Auto-discovery scope**: should non-LSP servers (gdb, godbolt, perftools) also get discovery hits, since gdb needs gdb/lldb and perftools needs clang-tidy? Proposed answer: yes, server-level `required_binaries` added to each manifest that has external dependencies; same discovery engine handles both kinds.
3. **`${parent_path}` semantics on PATH collision**: if overlay declares Path: "C:/x" AND parent inherits Path: "C:/y", the operator probably wants the union, not the override. Proposed answer: when overlay value contains the `${parent_path}` token, the supervisor expands it inline at spawn time; the operator MUST include the token if they want to preserve parent. The hints generator always includes it as the last segment.
4. **GUI "Active workspace" persistence**: per-machine OR per-GUI-session? Proposed answer: per-machine setting in the existing GUI preferences file (`gui-preferences.yaml`), persisted across GUI restarts.

## Terms and Abbreviations

- **LSP**: Language Server Protocol; a JSON-RPC protocol exposing IDE-grade code intelligence (find symbol, hover, references, completion, diagnostics) over a stdio/socket channel. Each language has its own implementation (clangd for C/C++, rust-analyzer for Rust, gopls for Go, etc.).
- **mcp-language-server**: a Go binary (`/c/Users/dima_/go/bin/mcp-language-server`) that bridges an LSP server to the MCP protocol; mcphub spawns it with `--workspace <dir> --lsp <command>`.
- **workspace-scoped**: in mcphub manifest schema, a server kind where each (workspace, language) pair gets its own daemon. Contrast with `kind: global` where one daemon serves all callers.
- **Overlay**: a user-editable layer that augments shipped manifests without modifying them. mcphub install/upgrade preserves overlays.
- **Active workspace**: the workspace whose `mcphub register`-ed lazy proxies are addressed by Servers matrix actions in the GUI. Operator picks one at a time.
- **Lazy proxy**: a workspace-scoped daemon that doesn't spawn the heavy LSP backend (the actual `mcp-language-server` Go binary) until the first MCP tools/call on it. Reduces startup cost when no client uses the language right after `register`.
- **`${parent_path}` token**: a placeholder in the overlay file's env values that expands at spawn time to the supervisor process's PATH. Lets the operator declare "prepend this to PATH" without manually concatenating the inherited PATH each time.

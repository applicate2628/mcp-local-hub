# Phase 3 Verification — 2026-04-22

Closes Phase 3 — **Workspace-Scoped `mcp-language-server` Daemons with Lazy
Materialization** across the implementation plan:

- `docs/superpowers/plans/2026-04-20-mcp-language-server-workspace-scoped.md`
  (18 tasks across M1–M5)

Plus the post-merge security + docs arc that landed on top of Phase 3 proper:

- PR #4 — hyperfine RCE gate + serena supply-chain pin + weekly-refresh flip
- PR #4 follow-ups — discovery metadata gate, docs drift, task reconciliation,
  INSTALL.md CRLF→LF normalization

## Goal (from plan)

Add a workspace-scoped daemon kind alongside Phase 2's global daemons:

- `mcphub register <workspace> [language...]` allocates ports from **9200–9299**,
  creates one scheduler task per `(workspace, language)` pair whose payload is
  a **lazy proxy** — a lightweight Go process that is **not** the heavy LSP
  backend. Heavy backends do not start at register time.
- The lazy proxy answers `initialize` + `tools/list` **synthetically** from an
  embedded static tool catalog. No backend contacted.
- First `tools/call` for a given `(workspace, language)` **materializes** the
  backend: spawn `mcp-language-server --workspace <ws> --lsp <backend>`
  (8 languages) or `gopls mcp` (Go), MCP-handshake it, then proxy the call and
  all subsequent calls.
- Materialization is guarded by a per-pair `singleflight` gate + retry throttle
  so concurrent first-calls don't race and failures don't spin.
- `mcphub unregister <workspace>` (full) or `unregister <workspace> <lang>`
  (per-language) stops the proxy; the materialized child backend is swept by
  Phase 2's `treekill.go`.

Nine languages in the shipped manifest: `clangd`, `fortran`, `go`, `javascript`,
`python`, `rust`, `typescript`, `vscode-css`, `vscode-html`.

## Servers — **11** (was 10)

| Server | Port | Transport | Notes |
|---|---:|---|---|
| serena (claude) | 9121 | native-http (uvx) | Phase 1 flagship |
| serena (codex) | 9122 | native-http (uvx) | Separate daemon per-client |
| memory | 9123 | stdio-bridge (Go host) | Single writer to jsonl |
| sequential-thinking | 9124 | stdio-bridge (npx) | Stateless |
| wolfram | 9125 | stdio-bridge (node) | `wolfram_app_id` secret |
| godbolt | 9126 | **embedded Go** | Compiler Explorer + llvm-mca + pahole |
| paper-search-mcp | 9127 | stdio-bridge (uvx) | `unpaywall_email` secret |
| time | 9128 | stdio-bridge (npx) | Stateless |
| gdb | 9129 | stdio-bridge (uv run) | Multi-debugger |
| lldb | 9130 | **embedded Go bridge** | HTTP-multiplex to LLDB TCP |
| perftools | 9131 | **embedded Go** | 3 always-on tools + hyperfine opt-in |
| **mcp-language-server** | **9200–9299 pool** | **embedded Go lazy proxy** | **Phase 3 — per (workspace, language) pair** |

Plus `context7` as a direct HTTPS entry.

## Architecture additions

### Workspace-scoped manifest kind (`Kind: workspace-scoped`)

New manifest shape in [internal/config/manifest.go](internal/config/manifest.go):

- `PortPool {start, end}` — range the allocator picks from per-pair
- `Languages[]` — each has `Name`, `Backend` (`mcp-language-server` | `gopls-mcp`),
  `Transport` (defaults `stdio`), `LSPCommand`, `ExtraFlags[]`

`mcphub install --server mcp-language-server` is explicitly rejected with a
clear `use mcphub register` error — no implicit "install once for all
workspaces" semantic, because the (workspace, language) tuples are not
inferable from the manifest alone.

### Workspace registry — atomic + flock-guarded

New file `%LOCALAPPDATA%\mcp-local-hub\workspaces.yaml` (Linux/macOS:
`$XDG_STATE_HOME/mcp-local-hub/` or `~/.local/state/mcp-local-hub/`):

- Key: `sha256[:8]` of `filepath.EvalSymlinks(absPath)` on the workspace root
- Entries: `{workspace_key, language, port, task_name, client_entry_names,
  lifecycle_state, last_used}`
- Writes: temp-file + `os.Rename` atomicity, rolling `.bak` backup, cross-process
  file lock via `github.com/gofrs/flock`

### Lazy proxy ([internal/daemon/lazy_proxy.go](internal/daemon/lazy_proxy.go))

A brand-new Go process started by scheduler task `mcphub daemon workspace-proxy
--port <p> --workspace <ws> --language <lang>`:

- Binds the allocated port, accepts MCP over streamable HTTP
- `initialize` → synthetic response (static capabilities + serverInfo)
- `tools/list` → embedded catalog per backend + language
- First `tools/call`:
  1. Acquires `singleflight.Do("<ws>:<lang>")`
  2. Checks retry throttle (rate-limits re-materialize after backend death)
  3. Spawns backend subprocess, performs MCP handshake
     (`initialize` + `notifications/initialized`)
  4. Forwards the call; subsequent calls proxy directly
- Subsequent `tools/list` after materialization → proxied (so clients see
  real tools registered by the backend, not just the static catalog)
- Backend crash → next `tools/call` re-materializes under the same throttle
- `context.WithoutCancel` + `WithDeadline` decouples backend lifecycle from
  per-request context; client cancel doesn't teardown the backend

### Lifecycle states (5)

| State | Meaning |
|---|---|
| `Dormant` | Proxy running, backend never started |
| `Active` | Backend materialized and healthy |
| `Missing` | Backend binary not on PATH (reported by `tools/list`) |
| `Failed` | Last materialize attempt failed; under throttle |
| `Orphan` | Registry entry exists but no scheduler task (raw state preserved) |

Surfaced by `mcphub workspaces` and by the integrated `mcphub status` when
run with `--workspace-scoped`.

## Commit trail — Phase 3 proper (M1 → M5 → review rounds)

### M1 — Manifest schema + registry + port allocator (Tasks 1–5)

| Commit | Task | Description |
|---|---|---|
| 8fa3790 | 1 | `LanguageSpec.Backend` + `Transport` + workspace-scoped validation |
| 0a1d32b | 2 | `servers/mcp-language-server/manifest.yaml` (9 languages, stdio) |
| e1197d9 | 3 | Canonical workspace path + deterministic `sha256[:8]` key |
| 072ea91 | 4 | Workspace registry — lazy-aware schema + atomic-write + flock |
| 18c3e10 | 5 | First-free port allocator within [start, end] range |

### M2 — Tool catalog + backend lifecycle + inflight gate (Tasks 6–8)

| Commit | Task | Description |
|---|---|---|
| d27c10e | 6 | Embedded tool catalog + synthetic initialize/tools-list |
| f8be850 | 7 | `BackendLifecycle` + `MCPEndpoint` interfaces + two stdio impls |
| 8918ab7 | 8 | Singleflight gate + retry throttle |
| 98242e6 | M2-polish | Consolidate `PutLifecycle` + wrap init err + test nits |

### M3 — Lazy proxy + Register orchestrator + CLI (Tasks 9–12)

| Commit | Task | Description |
|---|---|---|
| 87c25a1 | 9 | Lazy proxy + `mcphub daemon workspace-proxy` subcommand |
| 181fbef | 9.fix | `onSendFailure` ordering + crash test assertions |
| 533ef23 | 10 | `Register` + `Unregister` per-language rollback stack |
| 1697a1b | 12 | Reject workspace-scoped manifests from `Install` / `InstallAll` |
| 7697b3f | 11 | CLI `register` / `unregister` / `workspaces` with 5-state output |

### M4 — Migration + weekly refresh + status enrichment (Tasks 13–15)

| Commit | Task | Description |
|---|---|---|
| c30ec16 | 13 | Shared weekly-refresh task + `WeeklyRefreshAll` |
| edc4c05 | 14 | Legacy `mcp-language-server` migration (per-workspace, not per-language) |
| 49d69ff | 13.fix | `Register` ensures shared weekly-refresh task idempotently |
| 3cde473 | 15 | Status enrichment with 5-state lifecycle + `last_used` |

### M5 — Health probe + weekly-refresh CLI + E2E (Tasks 16–18)

| Commit | Task | Description |
|---|---|---|
| 5584a51 | 16 | `--force-materialize` health probe flag |
| ca4044d | 17 | `workspace-weekly-refresh` hidden subcommand |
| 06784d8 | 18 | E2E: register → handshake → materialize → unregister |
| d05b96a | 8.polish | Bump `ConcurrentFirstCall` materializeDelay to 200ms |

### Correctness fixes that landed mid-stream

| Commit | Description |
|---|---|
| ac4eeff | MCP handshake on `Materialize` (`initialize` + `notifications/initialized`) |
| ca07e60 | `Register` starts proxy immediately after task create |
| 79e35cb | `Unregister` + weekly refresh kill stale proxy by port |
| 960b17d | Quote Windows task args with spaces |
| 83c133b | `--force-materialize` requires `--health` + health output distinguishes proxy from backend |

### Review rounds — Codex bot + manual

**26 Codex review rounds** on PR #1 (`5078657` through `2d51320`) plus
**1 manual review round** (`819d0c6`) addressed findings including:

- Ghost-resurrection guard + Windows task-name normalization (#1)
- Ping id echo + structural filter (#2)
- Rollback kill + wrapped-LSP missing (#3)
- Rollback restart + notification forwarding (#4)
- Pre-Delete rollback + JSON stdout (#5)
- Persist-before-Run + cleanup tolerates missing path (#6)
- Preserve JSON-RPC id + restore prior entry (#7)
- Migrate-legacy explicit `enabled=false` (#8)
- Resolve symlinks + de-collide short suffixes (#9)
- Skip in-place replaced entries (#10)
- Export errors + scoped in-place check (#11)
- EOF-declines + cleanup reads symlink-target (#12)
- Detach ctx + kill-before-replace (#13)
- OS port check + preserve raw state for orphans (#14)
- Client-cancel no teardown + preserve re-register flag (#15)
- Hold registry lock across proxy bind (#16)
- Avoid flock deadlock in Bind path (#17)
- Port validation + structural `Source` tag (#18)
- Resolve broken parent symlinks in cleanup (#19)
- Preserve weekly task on Create failure (#20)
- Stop/Restart discover workspace-scoped tasks (#21)
- `--all` paths + scheduler-upgrade skip (#22)
- Preserve deadline + Stop on Serve err (#23)
- Rewire workspace tasks on scheduler upgrade (#24)
- Resolve symlink ancestors on partial deletes (#25)
- Preflight canonical mcphub + propagate list errs (#26 Codex, #27 manual):
  readiness probe + weekly-task order + force-materialize tool err

All 27 rounds resolved. PR #1 merged via fast-forward.

## Security + docs arc (PR #4) — landed after Phase 3 merge

| Commit | Description |
|---|---|
| e3adacc | **hyperfine RCE gate** (`MCP_LOCAL_HUB_ENABLE_UNSAFE_HYPERFINE=1`), **serena SHA pin** (`f0a3a279…`), drop `--refresh`, flip `weekly_refresh: false` |
| 410dd49 | Discovery metadata hides `hyperfine` when gate closed (round-1 Codex) |
| c122150 | Docs drift fix + `pruneObsoleteServerTasks` reconciliation (round-2 manual review) |
| c81810e | INSTALL.md CRLF→LF + `resource://tools` wording fix (round-3 manual review) |

### Security hotfix rationale

1. **hyperfine RCE surface**: `perftools.hyperfine` executes client-supplied
   shell commands. Tool registration now gated behind literal `"1"` in
   `MCP_LOCAL_HUB_ENABLE_UNSAFE_HYPERFINE` (any other value — `"true"`, `"0"`,
   whitespace, quoted — leaves the tool unregistered). Both `resource://tools`
   and `list_tools` filter hyperfine from discovery metadata when gate closed,
   so the "check tools first, then call" contract stays "advertised ⇒ callable".

2. **serena supply-chain**: `base_args` no longer includes `--refresh`; upstream
   pinned to commit `f0a3a279b7c48d28b9e7e4aea1ed9caed846906b`; `weekly_refresh:
   false`. Every serena launch now pulls the same audited revision. Weekly
   task no longer re-runs uvx against HEAD.

3. **Task reconciliation on reinstall**: `Install` on a full install now
   enumerates scheduler tasks with the `mcp-local-hub-<server>-*` prefix and
   prunes any that are not in the current plan. Closes the gap where the
   `weekly_refresh: true → false` flip stopped *creating* the weekly-refresh
   task but existing installs silently kept running the old one. Rollback-safe
   via ExportXML snapshots in the shared install rollback stack.

## CLI additions this phase

### `mcphub register <workspace> [language...] [flags]`

```
Allocate one lazy proxy per (workspace, language), create the scheduler
task that launches it, and write managed entries into every installed MCP
client config (codex-cli, claude-code, gemini-cli).

Lazy mode:
  - No LSP binary preflight at register time. A missing binary surfaces
    later at first tools/call via the LifecycleMissing state shown in
    `mcphub workspaces`.
  - Scheduler task args: `daemon workspace-proxy --port <p> --workspace <ws> --language <lang>`.
  - Entry names are `mcp-language-server-<lang>`; a cross-workspace
    collision appends `-<4hex>` from the workspace key.

Examples:
  mcphub register D:\projects\foo                         # every language
  mcphub register D:\projects\foo python typescript rust  # three languages
  mcphub register /home/u/web typescript --no-weekly-refresh
```

### `mcphub unregister <workspace> [language...]`

Stops the lazy proxy, kills the materialized backend via `treekill`, removes
client entries, deletes the scheduler task. Per-language scope leaves sibling
languages untouched.

### `mcphub workspaces`

Renders the registry:

```
WORKSPACE    LANG         PORT   BACKEND              LIFECYCLE   LAST_USED  PATH
```

Empty output when no workspaces registered (verified on this machine below).

### `mcphub --force-materialize --server mcp-language-server --workspace <ws> --language <lang> --health`

Health probe that actually triggers materialization (doesn't just check proxy
binding). Returns separate status for proxy layer vs backend layer so
diagnostics can tell "proxy up, LSP binary missing" apart from "proxy down".

## Live verification

```
$ mcphub manifest list
gdb
godbolt
lldb
mcp-language-server          ← new Phase 3 manifest
memory
paper-search-mcp
perftools
sequential-thinking
serena
time
wolfram
```

11 manifests — matches the 11 servers including the workspace-scoped
`mcp-language-server`.

```
$ mcphub workspaces
WORKSPACE    LANG         PORT   BACKEND              LIFECYCLE   LAST_USED  PATH
```

Clean empty registry on this machine — expected state, no workspace has been
registered here post-merge. End-to-end register → synthetic handshake →
tools/call → materialize → status → unregister is exercised by
[internal/e2e/lazy_register_test.go](internal/e2e/lazy_register_test.go)
(Task 18, commit `06784d8`) against real ports with a stubbed backend.

```
$ mcphub --help | grep -E 'register|workspace'
  register                 Register workspace-scoped mcp-language-server daemons (lazy-mode)
  unregister               Remove workspace-scoped daemons (full or per-language)
  workspaces               List registered workspaces and their languages
```

## Test suite

```
ok  	mcp-local-hub/internal/api                  7.400s
ok  	mcp-local-hub/internal/cli                  (cached)
ok  	mcp-local-hub/internal/clients              (cached)
ok  	mcp-local-hub/internal/config               0.022s
ok  	mcp-local-hub/internal/daemon               27.150s
ok  	mcp-local-hub/internal/e2e                  3.790s
ok  	mcp-local-hub/internal/godbolt              (cached)
ok  	mcp-local-hub/internal/perftools            6.012s
ok  	mcp-local-hub/internal/scheduler            (cached)
ok  	mcp-local-hub/internal/secrets              (cached)
```

- **36 test files** across `internal/{api,daemon,e2e}` after Phase 3
- **~254 test functions** in the Phase-3-affected packages
- `go vet ./...` clean
- `staticcheck ./...` clean
- `gofmt -l .` clean
- `git diff --check origin/master...HEAD` clean

New test files added this phase include:

- `internal/daemon/lazy_proxy_test.go` — handshake synthesis, singleflight,
  retry throttle, materialize races, concurrent first-calls, client-cancel
  vs backend-failure distinction
- `internal/api/register_test.go` — rollback stack (LIFO), canonical preflight,
  readiness probe via test seam, atomic port allocation
- `internal/api/workspace_path_test.go` — symlink resolution, path
  canonicalization, deterministic key
- `internal/api/workspace_registry_test.go` — atomic write, flock, schema
  migration, backup rotation
- `internal/api/force_materialize_test.go` — JSON-RPC envelope + SSE framing
  parsing, per-row tool-err propagation
- `internal/api/status_workspace_test.go` — enrichment through 5 states,
  orphan raw-state preservation
- `internal/e2e/lazy_register_test.go` — full register → handshake →
  tools/call → materialize → status → unregister round-trip
- `internal/api/prune_tasks_test.go` (PR #4) — 7 tests: keep-planned/prune-
  unplanned, rollback restore, rollback noop without XML, List failure fatal,
  per-task Delete continue, serena regression, Windows backslash prefix
- `internal/perftools/hyperfine_gate_test.go` (PR #4) — 9 subcases guarding
  the exact `"1"` contract against ParseBool-style loosening
- `internal/config/manifest_test.go` — 5 new tests for workspace-scoped shape
  (`Kind`, `PortPool`, `Languages[]`, transport default, transport enum)

## Known gaps / follow-ups

### ⚪ End-to-end smoke against real LSP binaries not run on this machine

E2E test `lazy_register_test.go` uses a stubbed backend; real-world first-call
materialization against `pyright-langserver`, `gopls mcp`, `clangd`, etc.
hasn't been exercised on this host. Not a functional risk per se (backend
handshake is tested in `internal/daemon/lazy_proxy_test.go`) but a real-LSP
smoke run would close the loop for the `LifecycleMissing` detection path.

### ⚪ `mcp status` `--workspace-scoped` filter is commit `3cde473`-era

Shipped in Task 15 but not live-verified on this machine because no workspace
is registered. Unit-tested via `status_workspace_test.go`.

### ⚪ Weekly-refresh for workspace-scoped uses one shared task

`mcp-local-hub-mcp-language-server-weekly-refresh` restarts every workspace's
proxies Sun 03:00. If future work wants per-workspace cadence, that's a
Task-17 extension — not a current gap.

### ⚪ INSTALL.md `resource://tools` wording says "three always-on"

INSTALL.md:358 describes the catalog as "three always-on + hyperfine when
enabled". Accurate for perftools, but the number ("three") is hard-coded in
prose — if a fifth tool is added later, INSTALL.md must be updated with the
code. Low priority.

### ⚪ No GUI yet

Phase 3 GUI installer spec at
`docs/superpowers/specs/2026-04-17-phase-3-gui-installer-design.md` predates
this workspace-scoped work. Still deferred — picks up next.

## Phase status

- ✅ **Phase 1** — Serena flagship daemon (docs/phase-1-verification.md)
- ✅ **Phase 2** — Global daemons consolidation (docs/phase-2-verification.md)
- ✅ **Phase 3A** — CLI parity + foundations (docs/phase-3a-verification.md)
- ✅ **Phase 3** — Workspace-scoped `mcp-language-server` with lazy materialization + security hotfix arc (**this document**)
- ⏳ **Phase 3B** — GUI installer (deferred; spec ready, no plan yet)

## Merge trail (master)

```
c81810e chore(docs): normalize INSTALL.md CRLF→LF + clarify resource://tools wording
c122150 fix(api,docs): address REVISE — prune obsolete tasks + docs reflect hyperfine gate
410dd49 fix(perftools): discovery hides hyperfine when gate is closed
e3adacc fix(security): gate hyperfine tool + pin serena source + drop auto-refresh
819d0c6 fix(api): manual review #26 — readiness probe + weekly-task order + force-materialize tool err
(…25 prior Codex re-reviews…)
5078657 fix(api): Codex review — ghost-resurrection guard + Windows task-name normalization
06784d8 test(api,e2e): E2E lazy register -> handshake -> materialize -> unregister
(…17 M1–M5 feature commits…)
8fa3790 feat(config): manifest LanguageSpec gains Backend + Transport
```

All on master. All tests green. PR #1 + PR #4 closed-merged via fast-forward.

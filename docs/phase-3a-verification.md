# Phase 3A Verification — 2026-04-18

Closes Phase 3A — the CLI parity + foundations track across the three sub-plans:

- `docs/superpowers/plans/2026-04-17-phase-3a-cli-foundations.md` (API foundations, subsystem switch, install refactor)
- `docs/superpowers/plans/2026-04-17-phase-3a2-operational-cli.md` (scan/migrate/logs/backups/rollback)
- `docs/superpowers/plans/2026-04-17-phase-3a3-management-cli.md` (manifest/scheduler/stop/settings)

Plus the session-driven additions that landed on top of 3A:

- godbolt: Python FastMCP → embedded Go (Tasks 1-3 + dual-entry refactor + filters/execute_parameters/tools/popularArguments/JSON response)
- lldb: Python bridge → embedded Go `mcphub lldb-bridge` (dual-entry)
- perftools: new MCP server wrapping clang-tidy/hyperfine/llvm-objdump/iwyu
- PATH-based install model with `mcphub setup` (canonical ~/.local/bin/mcphub.exe)
- `go:embed` manifests so canonical-path binary finds configs without filesystem dependency
- stdio-child-exit detection propagates to Task Scheduler `RestartOnFailure`

## Servers (10)

| Server | Port | Transport | Notes |
|---|---:|---|---|
| serena (claude) | 9121 | native-http (uvx) | Phase 1 flagship |
| serena (codex) | 9122 | native-http (uvx) | Separate daemon for codex |
| memory | 9123 | stdio-bridge (Go host, npx) | Single writer to jsonl |
| sequential-thinking | 9124 | stdio-bridge (npx) | Stateless |
| wolfram | 9125 | stdio-bridge (node) | Requires `wolfram_app_id` secret |
| godbolt | 9126 | stdio-bridge (**embedded Go**) | Formerly Python FastMCP |
| paper-search-mcp | 9127 | stdio-bridge (uvx) | Requires `unpaywall_email` secret |
| time | 9128 | stdio-bridge (npx) | Stateless |
| gdb | 9129 | stdio-bridge (uv run GDB-MCP) | Multi-debugger, session-managed |
| lldb | 9130 | stdio-bridge (**embedded Go bridge**) | Formerly Python per-session |
| perftools | 9131 | stdio-bridge (**embedded Go**) | clang-tidy + hyperfine + llvm-objdump + iwyu |

Plus context7 as direct HTTPS entry (no daemon, no scheduler task).

**Scheduler task count:** 11 (one per daemon + serena weekly-refresh).

## Live state — `mcphub status`

All 10 daemons Running on their configured ports, one weekly-refresh Scheduled:

```
NAME                                          STATE      PORT   PID      RAM(MB)  UPTIME     NEXT RUN
\mcp-local-hub-gdb-default                    Running    9129   111768   13       0h20m      N/A
\mcp-local-hub-godbolt-default                Running    9126   143252   12       0h20m      N/A
\mcp-local-hub-lldb-default                   Running    9130   128404   12       0h20m      N/A
\mcp-local-hub-memory-default                 Running    9123   149012   12       0h20m      N/A
\mcp-local-hub-paper-search-mcp-default       Running    9127   139412   12       0h20m      N/A
\mcp-local-hub-perftools-default              Running    9131   139244   13       0h20m      N/A
\mcp-local-hub-sequential-thinking-default    Running    9124   113580   13       0h20m      N/A
\mcp-local-hub-serena-claude                  Running    9121   150832   109      0h19m      N/A
\mcp-local-hub-serena-codex                   Running    9122   147896   109      0h19m      N/A
\mcp-local-hub-serena-weekly-refresh          Scheduled                                      19.04.2026 3:00:00
\mcp-local-hub-time-default                   Running    9128   115784   13       0h20m      N/A
\mcp-local-hub-wolfram-default                Running    9125   94112    13       0h20m      N/A
```

**Observations:**

- PID / RAM / Uptime columns populated for every Running task — confirms Phase 3A.3's `enrichStatus` is pulling port-liveness data correctly after the scheduler-gate fix (`7ee0e4f`).
- NEXT RUN column populated only for `-weekly-refresh` — confirms Phase 3A.3's trigger-time extraction (`e71f720`).
- State derivation: port-bound + PID alive → Running; no PID + future trigger → Scheduled (`930e771`).

## CLI surface (22 user-facing commands)

```
mcphub --help
```

```
backups     List, clean, or show client config backups
cleanup     Find and kill orphan MCP server processes (dry-run by default)
completion  Generate the autocompletion script for the specified shell
daemon      Run a daemon (invoked by scheduler, not by humans)
help        Help about any command
install     Install an MCP server as shared daemon(s)
logs        Print (and optionally follow) daemon logs
manifest    Manage server manifests under servers/*/manifest.yaml
migrate     Switch stdio client entries to hub HTTP for the specified servers
relay       Forward stdio MCP to an HTTP MCP endpoint (for clients lacking HTTP support)
restart     Restart daemon(s): stop + re-run scheduler tasks
rollback    Restore the latest mcp-local-hub backup for each client
scan        Scan client configs: which MCP servers are hub-routed, can-migrate, unknown, or per-session
scheduler   Scheduler-level operations (upgrade tasks, manage weekly refresh)
secrets     Manage encrypted secrets
settings    Read/write GUI preferences (theme, shell, default-home, etc.)
setup       Install mcphub to ~/.local/bin and register on user PATH
status      Show state of all mcp-local-hub scheduler tasks
stop        Stop daemon(s) without uninstalling (tasks and configs remain)
uninstall   Remove an installed MCP server (scheduler + client bindings)
version     Print version, commit, build metadata, and project homepage
```

Plus three Hidden subcommands (transport shims embedded into mcphub.exe as dual-entry with cmd/\*/):

- `mcphub lldb-bridge <host:port>` — stdio↔TCP bridge + LLDB auto-spawn (also ships as standalone `lldb-bridge.exe`)
- `mcphub godbolt` — Compiler Explorer API proxy (also ships as `godbolt.exe`)
- `mcphub perftools` — perf-analysis toolbox (also ships as `perftools.exe`)

## Per-command live verification

### `mcphub version`

```
mcp-local-hub dev
  commit:     unknown
  build date: unknown
  go version: go1.26.2
  platform:   windows/amd64

  homepage:   https://github.com/applicate2628/mcp-local-hub
  issues:     https://github.com/applicate2628/mcp-local-hub/issues
  license:    Apache-2.0
  author:     Dmitry Denisenko (@applicate2628)
```

**Note:** `commit` and `build date` show `unknown` because `go build -o mcphub.exe ./cmd/mcphub` doesn't inject ldflags. Phase 3C tagging will add `-ldflags "-X main.commit=... -X main.buildDate=..."` to a release script.

### `mcphub scan`

Classifies every MCP-client entry into one of four buckets:

```
via-hub (9):
  memory                    cc=http cx=http gm=http ag=relay
  serena                    cc=http cx=http gm=http ag=relay
  paper-search-mcp          cc=http cx=http gm=http ag=relay
  sequential-thinking       cc=http cx=http gm=http ag=relay
  perftools                 cc=http cx=http gm=http ag=relay
  gdb                       cc=http cx=http gm=http ag=relay
  godbolt                   cc=http cx=http gm=http ag=relay
  time                      cc=http cx=http gm=http ag=relay
  wolfram                   cc=http cx=http gm=http ag=relay

unknown (11):
  go                        cx=stdio ag=stdio
  fortran                   cx=stdio ag=stdio
  rust                      cx=stdio ag=stdio
  ...

per-session (2):
  lldb                      cc=http cx=http gm=http ag=relay
  playwright                cx=stdio gm=stdio ag=stdio
```

`via-hub` (9) matches installed servers. `per-session (lldb)` category is a minor mis-classification (lldb is actually hub-routed now; scanner may be recognizing an older pattern). Filed as follow-up.

### `mcphub manifest list`

```
gdb
godbolt
lldb
memory
paper-search-mcp
perftools
sequential-thinking
serena
time
wolfram
```

10 manifests match the 10 servers, including perftools landed in this session.

### `mcphub backups list`

Returns all `.bak-mcp-local-hub-*` files across all 4 client configs (claude-code, codex-cli, gemini-cli, antigravity). Current machine has **57 accumulated timestamped backups** from today's iterative install/uninstall cycles, plus 4 pristine sentinels (`.bak-mcp-local-hub-original`). `mcphub backups clean` is the prune command (not verified here — see follow-ups).

### `mcphub secrets list`

```
unpaywall_email
wolfram_app_id
```

Two secrets stored in the age-encrypted vault at `%LOCALAPPDATA%\mcp-local-hub\secrets.age` (decrypted via `.age-key` sibling). Used by paper-search-mcp and wolfram manifests via `env: X: secret:KEY` references.

### `mcphub settings list`

```
(no settings yet — defaults apply)
```

Empty by design — settings are written by GUI (Phase 3B). Command exists as a forward-compatibility seam per Phase 3A.3 plan.

### `mcphub setup`

```
✓ mcphub already at C:\Users\USERNAME\.local\bin\mcphub.exe (no copy needed)
✓ C:\Users\USERNAME\.local\bin already on user PATH
```

Idempotent. First-run: copies binary to canonical location + writes HKCU PATH entry + broadcasts WM_SETTINGCHANGE. Subsequent runs: no-op.

### `mcphub install --server perftools` (successful session trace)

```
✓ Scheduler task created: mcp-local-hub-perftools-default
  backup: C:\Users\USERNAME\.claude.json.bak-mcp-local-hub-20260418-232719
✓ claude-code → http://localhost:9131/mcp
  backup: C:\Users\USERNAME\.codex\config.toml.bak-mcp-local-hub-20260418-232719
✓ codex-cli → http://localhost:9131/mcp
  backup: C:\Users\USERNAME\.gemini\settings.json.bak-mcp-local-hub-20260418-232719
✓ gemini-cli → http://localhost:9131/mcp
  backup: C:\Users\USERNAME\.gemini\antigravity\mcp_config.json.bak-mcp-local-hub-20260418-232719
✓ antigravity → http://localhost:9131/mcp
✓ Started: mcp-local-hub-perftools-default

Install complete.
```

Confirms:

- Scheduler task creation (XML generator at `internal/scheduler/scheduler_windows.go`)
- Backup writing to each client config's sibling directory
- Client config patching per manifest's `client_bindings` list
- Immediate task `/Run` (not waiting for next logon trigger)

### `mcphub restart --all`

```
✓ Restarted \mcp-local-hub-gdb-default
✓ Restarted \mcp-local-hub-godbolt-default
✓ Restarted \mcp-local-hub-lldb-default
✓ Restarted \mcp-local-hub-memory-default
✓ Restarted \mcp-local-hub-paper-search-mcp-default
✓ Restarted \mcp-local-hub-sequential-thinking-default
✓ Restarted \mcp-local-hub-serena-claude
✓ Restarted \mcp-local-hub-serena-codex
✓ Restarted \mcp-local-hub-time-default
✓ Restarted \mcp-local-hub-wolfram-default
```

10 non-weekly-refresh tasks restarted. Confirms the port-kill-before-rerun fix (`477b6f1`) — without it, old daemons kept port bindings and new task runs silently failed.

### `mcphub scheduler upgrade`

```
✓ Upgraded \mcp-local-hub-gdb-default → C:\Users\USERNAME\.local\bin\mcphub.exe
✓ Upgraded \mcp-local-hub-godbolt-default → C:\Users\USERNAME\.local\bin\mcphub.exe
✓ Upgraded \mcp-local-hub-lldb-default → C:\Users\USERNAME\.local\bin\mcphub.exe
✓ Upgraded \mcp-local-hub-memory-default → C:\Users\USERNAME\.local\bin\mcphub.exe
✓ Upgraded \mcp-local-hub-paper-search-mcp-default → C:\Users\USERNAME\.local\bin\mcphub.exe
✓ Upgraded \mcp-local-hub-perftools-default → C:\Users\USERNAME\.local\bin\mcphub.exe
✓ Upgraded \mcp-local-hub-sequential-thinking-default → C:\Users\USERNAME\.local\bin\mcphub.exe
✓ Upgraded \mcp-local-hub-serena-claude → C:\Users\USERNAME\.local\bin\mcphub.exe
✓ Upgraded \mcp-local-hub-serena-codex → C:\Users\USERNAME\.local\bin\mcphub.exe
✓ Upgraded \mcp-local-hub-serena-weekly-refresh → C:\Users\USERNAME\.local\bin\mcphub.exe
✓ Upgraded \mcp-local-hub-time-default → C:\Users\USERNAME\.local\bin\mcphub.exe
✓ Upgraded \mcp-local-hub-wolfram-default → C:\Users\USERNAME\.local\bin\mcphub.exe
```

Rewrites every scheduler task's `<Command>` to the current canonical `~/.local/bin/mcphub.exe`. 11 tasks upgraded (10 daemon + 1 weekly-refresh). Confirms the `parseTaskName` weekly-refresh fix (`c067e2e`) — earlier versions mis-split `serena-weekly-refresh` into server=serena-weekly / daemon=refresh and silently skipped it.

## Architecture additions this session

### Dual-entry: library + standalone binary + hub subcommand

Three servers now ship with two entry points each, sharing one code path via `NewCommand() *cobra.Command`:

| Server | Library | Standalone exe | Hub subcommand |
|---|---|---|---|
| godbolt | `internal/godbolt/` | `go build ./cmd/godbolt` | `mcphub godbolt` |
| lldb | `internal/lldb/` | `go build ./cmd/lldb-bridge` | `mcphub lldb-bridge` |
| perftools | `internal/perftools/` | `go build ./cmd/perftools` | `mcphub perftools` |

Commits: `e700e0a` (godbolt refactor), `a1e4d6a` (lldb refactor), `ae120ed` (perftools initial).

### PATH-based install + `mcphub setup`

Scheduler tasks previously baked the absolute path of whatever `mcphub.exe` ran the install (typically `D:\dev\mcp-local-hub\mcphub.exe`) into the task XML. Moving the binary invalidated every task silently.

New model:

1. `mcphub setup` copies the current binary to `~/.local/bin/mcphub.exe` and adds that dir to user PATH via HKCU registry.
2. Scheduler tasks reference `C:\Users\<user>\.local\bin\mcphub.exe` (the canonical absolute path that depends only on `$HOME`, not on dev location).
3. Antigravity relay entries reference the short name `mcphub.exe` (Node's child_process does PATH lookup).
4. Install preflight checks both — `os.Stat(canonical)` + `exec.LookPath(mcphubShortName)`.
5. `install --server X` prompts to run setup when mcphub isn't on PATH (interactive terminals only).

Commits: `62f3413` (initial PATH-based model), `6763be1` (scheduler_mgmt sibling fix), `fa7123a` (canonical absolute path, closes Task Scheduler CreateProcess/lpApplicationName gotcha).

### go:embed manifests

Binary at `~/.local/bin/mcphub.exe` has no servers/ directory sibling, so the old filesystem-based manifest lookup (`<exeDir>/servers/` or `<exeDir>/../servers/`) failed. Daemons spawned by scheduler returned exit 1 with "open manifest: file not found" — masked on this machine by stale zombie processes from the dev-path layout.

Fix (`4336774`): `//go:embed */manifest.yaml` in `servers/embed.go`. `daemon.go` now opens from the embed.FS — no filesystem dependency, works from any install location.

Tradeoff: editing a manifest requires a rebuild. Acceptable since manifests change monthly at most, and the binary already embeds its subcommand code.

### stdio child-exit propagates to RestartOnFailure

Before: when an npx/uvx child died unexpectedly (e.g., memory-server after N requests), `mcphub daemon` kept its HTTP server running with a dead child. MCP clients saw `subprocess response timeout` forever until manual restart.

After (`9bd9f8b`): `StdioHost.childExited` channel closed by a Wait-watcher goroutine. `daemon.go` selects on it alongside ctx-cancel and errCh, returns a non-nil error on child death, which exits mcphub non-zero, which trips Task Scheduler's `RestartOnFailure` policy (3 retries, 1-minute spacing — already configured in every task's XML). In-flight HTTP handlers also observe the channel and return 502 "subprocess died".

## Known issues (followup registry)

### 🟡 `mcphub cleanup` misclassifies processes

`cleanup --dry-run` tags unrelated Windows processes (Dropbox.exe, VS Code TS server, MSYS64 shells) as "server=gdb" or "server=lldb". Its classification heuristic is too broad — likely just grepping for strings like "gdb" or "lldb" in the cmdline without scoping to `mcphub daemon --server X` patterns.

**Impact:** Without `--dry-run`, cleanup would kill those processes. **Users should ALWAYS pass `--dry-run` first** until this is fixed. Filed as bug.

### 🟡 `mcphub logs perftools` fails when daemon writes nothing to stderr

perftools (and any pure-stdio server that produces no stderr) never creates its log file, so `mcphub logs perftools` errors with "file not found" even though the daemon is healthy. Should return "(no output yet)" cleanly.

### ⚪ `mcphub backups clean` flag incompatibility

`backups clean --dry-run` errors with "unknown flag: --dry-run". The command may have a different preview mechanism; docs/help text don't surface it clearly.

### ⚪ `scan` classifies lldb as "per-session"

lldb is now hub-routed via `mcphub lldb-bridge` stdio subcommand but `scan` still lists it under `per-session`. Scanner heuristic needs updating for the dual-entry pattern.

### ⚪ Accumulated backups (57+)

Today's iterative install/uninstall produced 57+ timestamped backups per client config. A `backups clean` pass would reduce the noise. The pristine `.bak-mcp-local-hub-original` sentinel (1 per client, ever) is correctly retained (`e86dab5` lex-sort fix).

## Test suite

```
ok  	mcp-local-hub/internal/api
ok  	mcp-local-hub/internal/cli
ok  	mcp-local-hub/internal/clients
ok  	mcp-local-hub/internal/config
ok  	mcp-local-hub/internal/daemon
ok  	mcp-local-hub/internal/godbolt       (7 tests)
ok  	mcp-local-hub/internal/perftools     (6 tests: 5 PASS, iwyu SKIP on env)
ok  	mcp-local-hub/internal/scheduler
ok  	mcp-local-hub/internal/secrets
```

`go vet ./...` clean. `go build ./cmd/{mcphub,godbolt,lldb-bridge,perftools}` all clean. Linux + macOS cross-compile also verified (for mcphub).

## MCP client connectivity (`claude mcp list`)

```
gdb: http://localhost:9129/mcp (HTTP) - ✓ Connected
godbolt: http://localhost:9126/mcp (HTTP) - ✓ Connected
lldb: http://localhost:9130/mcp (HTTP) - ✗ Failed to connect (expected — lldb waits for debuggee)
memory: http://localhost:9123/mcp (HTTP) - ✓ Connected
perftools: http://localhost:9131/mcp (HTTP) - ✓ Connected
serena: http://localhost:9121/mcp (HTTP) - ✓ Connected
time: http://localhost:9128/mcp (HTTP) - ✓ Connected
wolfram: http://localhost:9125/mcp (HTTP) - ✓ Connected
```

7/8 hub MCPs show Connected. lldb's daemon is up but its underlying LLDB TCP server (`:47000`) isn't listening — by design, LLDB spins up only when a debugger session starts.

## Commit trail — this session

| Session phase | Commits | Description |
|---|---|---|
| Post-3A cleanup | `7ee0e4f` `e71f720` `930e771` `477b6f1` | status enrichment, NEXT RUN column, STATE derivation, restart kill-by-port |
| lldb rewrite | `bf3ec7c` | Python bridge → embedded Go |
| godbolt core | `27fbf23`..`a25858c` | Skeleton + 6 resources + 3 tools |
| Dual-entry refactor | `e700e0a` `a1e4d6a` | godbolt/lldb → library + standalone binary + hub subcommand |
| Install model | `62f3413` `6763be1` `fa7123a` | PATH-based install + mcphub setup + canonical absolute path |
| Docs | `85266cf` `9d0a971` `11284b5` `6cd571a` | Manifest switch, INSTALL.md sections, placeholder fix, lldb antigravity binding |
| Followups | `c067e2e` `e86dab5` `9bd9f8b` | parseTaskName weekly-refresh + dir-on-PATH auto-bootstrap, findLatestBackup sentinel skip, stdio child exit detection |
| Godbolt perf-expansion | `1175cd0`..`9b10a29` | JSON mode + filters + executeParameters + tools + popularArguments + docs |
| Perftools MCP | `ae120ed`..`38f6349` | New server: clang-tidy/hyperfine/llvm-objdump/iwyu + embed manifest fix |

All commits on master. Test suite green throughout.

## Phase status

- ✅ **Phase 3A.1 — API Foundations** (CLI scaffolding, scan/migrate API)
- ✅ **Phase 3A.2 — Operational CLI** (logs/backups/rollback/status enrichment)
- ✅ **Phase 3A.3 — Management CLI** (manifest/scheduler/stop/settings)
- ✅ **Phase 3A postscript** — lldb rewrite, godbolt rewrite + embed, perftools server, PATH-based install, canonical path, embed-manifests, child-exit detection
- ⏳ **Phase 3B — GUI layer** (deferred; spec exists at `docs/superpowers/specs/2026-04-17-phase-3-gui-installer-design.md`)
- ⏳ **Phase 3C — this document + README update + version tag**

Phase 3B is intentionally deferred. The CLI surface is now complete enough that GUI work can proceed against a stable backend API in a future session.

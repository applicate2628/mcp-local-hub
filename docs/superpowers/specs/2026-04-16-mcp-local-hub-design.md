# MCP Local Hub — Platform Design Spec

**Date:** 2026-04-16
**Author:** Dmitry (with Claude Code)
**Status:** Design — pending implementation plan
**Repo name:** `mcp-local-hub` (verified free on GitHub as of 2026-04-16 scan)

## 1. Problem

This workstation runs four MCP clients (Claude Code, Codex CLI, Gemini CLI, Antigravity) in a multi-agent setup that includes worktrees, `$external-worker` / `$external-reviewer` adapters, and parallel sessions. Each MCP client session spawns its own stdio subprocess for each MCP server it needs. This produces two concrete pain points:

- **Global-state servers spawned N times** — Serena in particular reinitializes LSP servers and symbol graphs on every session, costing ~3-5 s per startup and hundreds of MB per process. `memory` (JSONL persistence) is worse: concurrent writes to one file from multiple stdio instances race and can corrupt state.
- **Workspace-bound servers spawned N × M times** — `mcp-language-server` wrappers (pyright, tsserver, rust-analyzer, etc.) take `--workspace .` at startup, so every subagent / external adapter working on the same project respawns a fresh LSP with the same 20-60 s index cost.

Existing workarounds (stdio per session) cannot solve this — the transport itself forces per-client subprocesses. A local platform that owns the lifecycle of these servers and exposes them over HTTP/Streamable HTTP to all clients is required.

## 2. Goal

Ship a local platform — **MCP Local Hub** — that:

1. Runs MCP servers as shared long-lived daemons, owned by user session, not by client sessions.
2. Supports two daemon kinds: **global** (one process per server-instance, serves all workspaces internally) and **workspace-scoped** (one process per (server, workspace, language), bound to that workspace at startup).
3. Works across the four active MCP clients on this machine today, and is extensible to new clients/servers via YAML manifests without code changes.
4. Cross-platform by construction — Windows is the primary target but Linux and macOS implementations follow from the same codebase via Go build tags.
5. Manages secrets (API keys) through a portable encrypted store, not plaintext in client configs.

### Success criteria

1. After install, at most one process per global-daemon context runs at any time, regardless of how many client sessions are active.
2. Multiple clients opening the same workspace share one workspace-scoped LSP process per language.
3. All four existing clients continue to function for their Serena tool usage without protocol changes.
4. Adding a new MCP server requires only a new YAML manifest file and a single `mcp install --server <name>` command.
5. Rollback is single-command and restores the pre-hub state of each client's MCP configuration.
6. The binary runs on Windows 11, Ubuntu 22.04+, macOS 13+ with no runtime dependencies other than the MCP servers themselves.

### Non-goals

- **Not an MCP server itself.** The hub launches and routes to existing MCP servers; it does not implement tool logic.
- **No remote access.** Daemons bind to loopback only. Cross-machine sharing is out of scope.
- **No multi-user orchestration.** One user per machine; multi-user systems need separate hubs per user.
- **No web dashboard replacement.** Serena's built-in dashboard (port 24282) stays independent.
- **No automatic MCP client discovery.** Client config files are updated via explicit `mcp install` invocations.

## 3. Architecture

### 3.1 High-level component diagram

```
┌─ mcp (single Go binary) ────────────────────────────────────────────────┐
│                                                                         │
│  Subcommands: install, uninstall, daemon, register, unregister,         │
│               secrets, status, rollback, restart                        │
│                                                                         │
│  ┌─ internal/cli ─┐ ┌─ internal/config ─┐ ┌─ internal/secrets ─┐        │
│  │ install logic  │ │ manifest parsing  │ │ age encryption     │        │
│  │ CLI surface    │ │ client adapters   │ │ CLI integration    │        │
│  └────────────────┘ └───────────────────┘ └────────────────────┘        │
│                                                                         │
│  ┌─ internal/scheduler ────────────┐ ┌─ internal/daemon ───────┐        │
│  │ scheduler_windows.go            │ │ launcher.go             │        │
│  │ scheduler_linux.go   (systemd)  │ │ log_rotate.go           │        │
│  │ scheduler_darwin.go  (launchd)  │ │ bridge.go (stdio→http)  │        │
│  └─────────────────────────────────┘ └─────────────────────────┘        │
│                                                                         │
│  ┌─ internal/workspace ───┐ ┌─ internal/clients ──────────────┐         │
│  │ registry.go            │ │ claude_code.go                  │         │
│  │ ports.go (deterministic│ │ codex_cli.go                    │         │
│  │   hash allocation)     │ │ gemini_cli.go                   │         │
│  │ cleanup.go (idle TTL)  │ │ antigravity.go                  │         │
│  └────────────────────────┘ └─────────────────────────────────┘         │
└─────────────────────────────────────────────────────────────────────────┘
                │                                      │
                │ creates                              │ reads/writes (with .bak)
                ▼                                      ▼
┌─ OS Scheduler ──────────────┐      ┌─ MCP client configs ────────────┐
│ Windows Task Scheduler      │      │ ~/.claude.json                  │
│ systemd --user units        │      │ ~/.codex/config.toml            │
│ launchd agents              │      │ ~/.gemini/settings.json         │
└─────────────────────────────┘      │ ~/.gemini/antigravity/mcp_*.json│
                │                    │ <workspace>/.mcp.json           │
                │ launches           └─────────────────────────────────┘
                ▼                                      │
┌─ Daemons (per server manifest) ──────────┐          │
│ global: serena-claude (9121)  shared     │          │
│         ├─ Claude Code                   │          │
│         ├─ Gemini CLI                    │ serves   │
│         └─ Antigravity                   │◀─────────┘
│ global: serena-codex  (9122)             │          │
│ global: memory (9140)                    │ MCP requests
│ workspace: pyright@d:/dev/Orch (9214)    │
│ workspace: rust@d:/dev/foo    (9215)     │
└──────────────────────────────────────────┘
```

### 3.2 Implementation language — Go

Chosen after comparing PowerShell / Python / Go / Rust / TypeScript (details below, in section 10).

- **Cross-platform code via build tags.** OS-specific implementations of scheduler, secret-store, path resolution live in `*_windows.go` / `*_linux.go` / `*_darwin.go`. The top-level interface is portable.
- **Single static binary.** `go build` produces `mcp.exe` or `mcp`. No runtime, no installer, no pip/npm.
- **Fast daemon launcher.** Process start <10 ms vs ~500 ms for a PowerShell host. Matters for workspace-scoped daemons that cycle on idle-timeout.
- **Goroutines for watchdog/cleanup.** Idle-TTL enforcement, health checks, log rotation run as concurrent goroutines inside the daemon subcommand.
- **Ecosystem:** `filippo.io/age` for encryption, `zalando/go-keyring` available if OS-native secret enhancement is added later, `mark3labs/mcp-go` available if a custom MCP gateway becomes necessary in later phases.

### 3.3 Daemon kinds

#### 3.3.1 Global daemons

One process per (server, context). The context distinction matters when one server has per-client-context variants — e.g., Serena accepts one `--context` flag per process. For the Phase 1 Serena deployment we run **two** daemons: `claude-code` context (port 9121, shared by Claude Code, Gemini CLI, and Antigravity — all three accept this context's tool preset) and `codex` context (port 9122, required by Codex CLI's own preset). The three non-Codex clients share a single Serena process and thus share the gopls LSP cache.

| Property | Value |
|---|---|
| Lifecycle trigger | At log on (OS scheduler) |
| Termination | At log off, manual, or crash |
| Restart policy | 60 s × 3 attempts on failure |
| Refresh | Weekly task restarts all global daemons at Sun 03:00 (covers sessions that never log out) |
| Port | Static, from manifest |
| Binding | Referenced from global client configs (`~/.claude.json`, `~/.codex/config.toml`, etc.) |

Examples: Serena (3 contexts × 1 instance each), memory (1), sequential-thinking (1), fetch (1), wolfram (1), paper-search-mcp (1).

#### 3.3.2 Workspace-scoped daemons

One process per (server, workspace, language). LSP-backed servers such as `mcp-language-server` are fundamentally workspace-bound: `--workspace <path>` at startup cannot change without restart.

| Property | Value |
|---|---|
| Lifecycle trigger | `mcp register <workspace> <lang...>` explicit command |
| Termination | Idle timeout (default 2 h), manual `mcp unregister`, or workspace directory deleted |
| Restart policy | 60 s × 3 on failure during active window |
| Port | Deterministic: `PortPool.start + hash(workspace) * langCount + langIndex` |
| Binding | Referenced from **per-workspace** `<workspace>/.mcp.json` |

Examples: `mcp-language-server` for pyright, tsserver, rust-analyzer, clangd.

### 3.4 Transport modes

Two modes. A server's manifest declares which it is.

#### 3.4.1 `native-http`
Server implements MCP Streamable HTTP (spec rev 2025-06-18+) natively. Daemon starts the server with `--transport streamable-http --port N`. Clients connect to `http://localhost:N/mcp` directly.

Current servers: Serena (`--transport streamable-http`). Future: any MCP server that exposes HTTP endpoint.

#### 3.4.2 `stdio-bridge`
Server is stdio-only. Daemon wraps it with a stdio-to-HTTP bridge so multiple clients can connect. Primary bridge: `supergateway` (well-maintained community tool, Node-based, `npx -y supergateway --stdio '<cmd>' --port N`). A Go-native bridge may be implemented later using `mark3labs/mcp-go` if supergateway's dependency footprint or performance becomes a concern.

Current servers via bridge: `memory`, `sequential-thinking`, `mcp-server-fetch`, `wolfram`, `paper-search-mcp`, `mcp-language-server` (all stdio-only).

### 3.5 Port allocation

| Range | Purpose | Phase 1-3 usage |
|---|---|---|
| 9121-9139 | Global daemons, assigned per server manifest (19 ports) | Serena 9121-9122 |
| 9140-9199 | Global daemons for later servers (60 ports) | memory 9140 (Phase 2) |
| 9200-9299 | Workspace-scoped daemons (100 slots) | mcp-language-server instances (Phase 3) |

Central allocation recorded in `configs/ports.yaml` at repo root. `mcp install` validates no conflicts with that registry.

Workspace-scoped slot allocation is deterministic from the workspace path hash. Given a server's `port_pool: {start: 9200, end: 9299}` (100-port span) and `language_count` per workspace (typically 1-4), the allocator reserves `language_count` consecutive ports per workspace, yielding `floor(100 / max_langs_per_workspace)` addressable workspaces. With max 4 languages per workspace: 25 unique workspaces before re-use / collision handling (planned: open-addressing probe on conflict). Exhaustion is far above any realistic developer workload.

### 3.6 Client configuration

Hub manages four client config files on this machine. Abstraction: each client has its own Go module (`internal/clients/*.go`) that knows:

- Config file path(s) per-OS
- Config format (JSON, TOML)
- The MCP server entry schema (`"command"` vs `"url"` vs `"httpUrl"` — varies per client)
- Backup strategy (`.bak-mcp-local-hub-<timestamp>` sidecar file)
- Merge semantics (override vs replace)

Client modules registered today:

| Client | Config path | Format | URL field |
|---|---|---|---|
| Claude Code | `~/.claude.json` | JSON | `url` + `type: "http"` (verified against code.claude.com/docs/en/mcp + empirical `~/.claude.json`) |
| Codex CLI | `~/.codex/config.toml` | TOML | `url` (verified, Codex 0.121+) |
| Gemini CLI | `~/.gemini/settings.json` | JSON | `httpUrl` (TBD, verified at Phase 0) |
| Antigravity | `~/.gemini/antigravity/mcp_config.json` | JSON | `httpUrl` (TBD, same as Gemini) |

Per-workspace `.mcp.json` files: all four clients already understand this convention (project-local MCP overrides). Hub writes this file during `mcp register`.

### 3.7 Manifest schema

Declarative per-server YAML in `servers/<name>/manifest.yaml`.

Serena manifest:

```yaml
name: serena
kind: global
transport: native-http
command: uvx
base_args:
  - --refresh
  - --from
  - git+https://github.com/oraios/serena
  - serena
  - start-mcp-server
  - --transport
  - streamable-http
daemons:
  - name: claude
    context: claude-code
    port: 9121
    extra_args:
      - --context
      - claude-code
  - name: codex
    context: codex
    port: 9122
    extra_args: [--context, codex]
client_bindings:
  - client: claude-code
    daemon: claude
    url_path: /mcp
  - client: codex
    daemon: codex
    url_path: /mcp
  - client: antigravity
    daemon: claude           # shared: antigravity accepts claude-code context
    url_path: /mcp
  - client: gemini-cli
    daemon: claude           # shared: same as antigravity
    url_path: /mcp
weekly_refresh: true
```

Memory manifest:

```yaml
name: memory
kind: global
transport: stdio-bridge
command: npx
base_args:
  - -y
  - '@modelcontextprotocol/server-memory'
env:
  MEMORY_FILE_PATH: $HOME/OneDrive/Documents/env/Agents/memory.jsonl
daemons:
  - name: shared
    port: 9140
client_bindings:
  - client: claude-code
    daemon: shared
  - client: codex
    daemon: shared
  - client: antigravity
    daemon: shared
  - client: gemini-cli
    daemon: shared
weekly_refresh: false
```

mcp-language-server (workspace-scoped) manifest:

```yaml
name: mcp-language-server
kind: workspace-scoped
transport: stdio-bridge
command: mcp-language-server
base_args_template:
  - --workspace
  - '{{.Workspace}}'
  - --lsp
  - '{{.Lsp}}'
languages:
  - name: pyright
    lsp_command: pyright-langserver
    extra_flags: [--stdio]
  - name: typescript
    lsp_command: typescript-language-server
    extra_flags: [--stdio]
  - name: rust
    lsp_command: rust-analyzer
  - name: clangd
    lsp_command: clangd
port_pool:
  start: 9200
  end: 9299
idle_timeout_min: 120
# `daemon: workspace` is a marker: for workspace-scoped servers, the binding's
# actual daemon is materialized per-workspace at `mcp register` time, not at install.
client_bindings:
  - client: claude-code
    daemon: workspace
  - client: codex
    daemon: workspace
  - client: antigravity
    daemon: workspace
  - client: gemini-cli
    daemon: workspace
```

### 3.8 Secrets management

**Primary store:** age-encrypted YAML file.

| File | Purpose | Git-tracked? |
|---|---|---|
| `secrets.age` | Encrypted key/value pairs | Yes (encrypted at rest, safe in git) |
| `.age-key` | Identity file (decryption key) | **No** (`.gitignore`; copy between machines via password manager) |

**Manifest syntax:**

```yaml
env:
  WOLFRAM_LLM_APP_ID: secret:WOLFRAM_LLM_APP_ID   # resolved from secrets.age at launch
  PAPER_SEARCH_MCP_UNPAYWALL_EMAIL: file:email     # resolved from config.local.yaml
  CUSTOM_HEADER: $CUSTOM_HEADER_ENV                # resolved from process env
  LOG_LEVEL: info                                  # plain literal
```

**Resolution order:**
1. `secret:<key>` → decrypt from `secrets.age` using `.age-key`
2. `file:<key>` → read from `config.local.yaml` (non-secret overrides, gitignored)
3. `$VAR` → read from OS env at launch time
4. literal → pass through

**CLI surface (full):**

| Command | Purpose |
|---|---|
| `mcp secrets init` | Generate identity, create empty vault |
| `mcp secrets list [--with-usage]` | List keys; optionally show which manifests use each |
| `mcp secrets get <key> [--show]` | Copy to clipboard by default; `--show` prints to stdout |
| `mcp secrets set <key> [--value V \| --from-stdin]` | Create or update |
| `mcp secrets rotate <key> [--restart-dependents]` | Replace + optionally restart dependent daemons |
| `mcp secrets delete <key>` | Remove |
| `mcp secrets rename <old> <new>` | Rename with optional manifest reference update |
| `mcp secrets edit` | Open decrypted vault in `$EDITOR`, re-encrypt on save |
| `mcp secrets migrate --from-client <name>` | Scan existing client config for hardcoded secrets, interactive import |
| `mcp secrets export <file>` | Write to different age-file (e.g., for backup with new identity) |
| `mcp secrets import <file>` | Load from age-file |
| `mcp secrets verify` | Check all manifest-referenced secrets exist |
| `mcp secrets where <key>` | Reverse-lookup: which manifests use this key |

**Cross-machine portability:** copy `.age-key` content via password manager to new machine; same `secrets.age` (from git) decrypts identically on Windows/Linux/macOS.

### 3.9 Repo structure

```
mcp-local-hub/
├── README.md
├── LICENSE
├── .gitignore                         # secrets.age allowed, .age-key excluded
├── go.mod
├── go.sum
│
├── cmd/
│   └── mcp/
│       └── main.go                    # CLI entry, subcommand routing
│
├── internal/
│   ├── config/
│   │   ├── manifest.go                # YAML manifest parser + validation
│   │   ├── ports.go                   # port-registry reader, conflict check
│   │   └── local.go                   # config.local.yaml reader
│   │
│   ├── clients/                       # one file per client
│   │   ├── clients.go                 # Client interface
│   │   ├── claude_code.go
│   │   ├── codex_cli.go
│   │   ├── gemini_cli.go
│   │   └── antigravity.go
│   │
│   ├── scheduler/                     # OS-abstracted task scheduling
│   │   ├── scheduler.go               # interface
│   │   ├── scheduler_windows.go       # Task Scheduler via schtasks + COM
│   │   ├── scheduler_linux.go         # systemd --user units
│   │   └── scheduler_darwin.go        # launchd user agents
│   │
│   ├── secrets/
│   │   ├── vault.go                   # age file wrapper, CRUD
│   │   ├── resolver.go                # secret:/file:/env: prefix resolution
│   │   └── migrate.go                 # scan client configs
│   │
│   ├── daemon/
│   │   ├── launcher.go                # spawn child process, log tee, exit code
│   │   ├── log_rotate.go
│   │   └── bridge.go                  # supergateway wrapper (fallback: native Go bridge)
│   │
│   ├── workspace/
│   │   ├── registry.go                # mcp register/unregister logic
│   │   ├── ports.go                   # deterministic allocation from hash
│   │   └── cleanup.go                 # idle timeout + orphan watchdog
│   │
│   └── cli/
│       ├── install.go
│       ├── uninstall.go
│       ├── daemon.go
│       ├── register.go
│       ├── unregister.go
│       ├── secrets.go                 # all mcp secrets * subcommands
│       ├── status.go
│       ├── restart.go
│       └── rollback.go
│
├── servers/                           # declarative server manifests
│   ├── _template/
│   │   ├── manifest.yaml
│   │   └── README.md
│   ├── serena/
│   │   ├── manifest.yaml
│   │   └── README.md
│   ├── memory/
│   │   ├── manifest.yaml
│   │   └── README.md
│   └── mcp-language-server/
│       ├── manifest.yaml
│       └── README.md
│
├── configs/
│   ├── config.example.yaml            # template for user's config.local.yaml
│   └── ports.yaml                     # central port registry
│
├── secrets.age                        # committed encrypted vault (empty at repo init)
│
└── docs/
    ├── architecture.md                # this spec
    ├── adding-a-server.md             # walkthrough
    ├── cross-platform.md              # OS-specific notes
    ├── troubleshooting.md
    └── servers/
        ├── serena.md
        ├── memory.md
        └── mcp-language-server.md
```

## 4. Data flow

### 4.1 Install a server

```
user> mcp install --server serena
  │
  ├─ read servers/serena/manifest.yaml
  ├─ resolve env entries (secret:/file:/env:/literal)
  ├─ check ports.yaml for conflicts
  ├─ register scheduler tasks for each daemon (3 for Serena)
  ├─ update client configs (backup → write url entry)
  │    ├─ ~/.claude.json                 ← Claude Code binding
  │    ├─ ~/.codex/config.toml           ← Codex binding
  │    ├─ ~/.gemini/antigravity/mcp_config.json  ← Antigravity
  │    └─ ~/.gemini/settings.json       ← Gemini (shares Antigravity port)
  ├─ start daemons immediately (without waiting for next logon)
  └─ report status
```

### 4.2 Register a workspace

```
user> cd d:/dev/Orchestrator
user> mcp register pyright typescript
  │
  ├─ read servers/mcp-language-server/manifest.yaml
  ├─ compute workspace-hash → allocate ports (e.g., 9214, 9215)
  ├─ register scheduler tasks with --workspace d:/dev/Orchestrator
  ├─ start daemons
  ├─ write d:/dev/Orchestrator/.mcp.json with pyright/typescript URLs
  └─ report status; existing clients in this workspace pick up config on next session start
```

### 4.3 Daemon launch (scheduler → daemon process)

```
OS scheduler fires → `mcp daemon --server serena --daemon claude`
  │
  ├─ read manifest
  ├─ resolve env (decrypt secrets via .age-key)
  ├─ rotate log file if > 10 MB
  ├─ exec child process: uvx --refresh --from ... serena start-mcp-server ...
  ├─ tee stdout/stderr to log file
  ├─ on child exit: propagate exit code (scheduler retries on non-zero)
  └─ on SIGTERM (logoff): propagate to child, wait 10 s, force-kill if still alive
```

### 4.4 Client MCP handshake (unchanged from MCP protocol)

Client reads its config → finds `url` entry → POSTs MCP `initialize` to `http://localhost:<port>/mcp` → receives tool list → subsequent calls flow over same endpoint.

### 4.5 Secret resolution at daemon launch

```
manifest: env.WOLFRAM_LLM_APP_ID: secret:WOLFRAM_LLM_APP_ID
  │
  ├─ launcher sees `secret:` prefix
  ├─ vault.Open(".age-key", "secrets.age")
  ├─ vault.Get("WOLFRAM_LLM_APP_ID") → plaintext
  ├─ set as env var in child process
  └─ child process (wolfram server) reads os.Getenv normally
```

Plaintext value exists only in memory for the launcher → child process duration. `.age-key` file read requires filesystem permissions; password-protected identity adds prompt at launcher start (MVP: plain identity + filesystem permissions).

## 5. Error handling

### 5.1 Daemon fails to start

**Causes:** port occupied, uvx/npx unavailable in task context, manifest references missing secret, scheduler task misconfigured, child process exits immediately.

**Behavior:** scheduler retries 60 s × 3, then rests. MCP clients get connection-refused; all four clients gracefully handle this (show warning, continue without Serena tools).

**Diagnosis:** `mcp status` lists daemon state (running/stopped/failed/unknown) and recent log tail. Full logs at `%LOCALAPPDATA%\mcp-local-hub\logs\` (Windows) or `~/.local/state/mcp-local-hub/logs/` (Linux/Mac via XDG).

### 5.2 Workspace-scoped daemon idle

Daemon self-terminates after `idle_timeout_min` (default 120). On next client connection, client gets connection-refused once; `mcp register` re-triggered on `.mcp.json` read. Alternative: a watchdog in `mcp daemon` can auto-restart on client attempt — MVP uses explicit re-register.

### 5.3 Port collision

Detected at `mcp install` time via ports.yaml validation. If runtime collision occurs (external process grabbed port between install and start), daemon exits with specific code; `mcp status` shows the conflict and suggests `mcp install --reallocate-ports`.

### 5.4 Secret missing

`mcp secrets verify` at install time catches this. At runtime, daemon launcher exits with specific code, log records the missing key. `mcp status` flags "missing secrets: WOLFRAM_LLM_APP_ID".

### 5.5 Client config corruption

Each install creates `.bak-mcp-local-hub-<timestamp>`. `mcp rollback --client <name>` restores the most recent backup. Full `mcp rollback` restores all four clients.

### 5.6 Scheduler task management failure

Access denied, task-service down, manifest ref stale. Each scheduler adapter surfaces OS error verbatim; CLI translates to actionable message.

## 6. Testing strategy

### 6.1 Unit tests (`go test ./...`)

- Manifest parser (valid/invalid YAML, template substitution)
- Port allocator (hash determinism, range exhaustion)
- Secret vault (encrypt/decrypt round-trip, wrong identity, corrupted file)
- Client config adapters (round-trip read/write, merge semantics, backup creation)
- Port registry conflict detection

### 6.2 Integration tests

- `mcp install --server serena --dry-run` produces expected scheduler tasks and config deltas without side effects
- `mcp install` on a sandbox user profile produces working daemons
- `mcp register <temp-workspace>` produces working `.mcp.json` and daemon
- `mcp rollback` restores exact pre-install state (bitwise diff)

### 6.3 End-to-end verification (manual, per phase)

- Phase 1 (Serena): Claude Code → `find_symbol` via daemon → matches pre-migration result
- Phase 2 (memory): Claude Code + Codex concurrent writes → no JSONL corruption
- Phase 3 (LSP): two Claude Code sessions on same workspace → only one pyright daemon running

### 6.4 Cross-platform smoke (stretch)

- `GOOS=linux go build` and basic CLI operations in a Linux VM
- `GOOS=darwin go build` (no live test, but must compile)

## 7. Implementation phases

### Phase 0 — Platform foundation
**Scope:** Go project skeleton, core libraries, CLI structure, four client modules, install/uninstall framework, scheduler adapters (Windows first; Linux/macOS scaffolded but not fully tested), secrets subsystem, manifest parser, port registry.

**Deliverable:** `mcp install --server _template` creates no-op scheduler tasks; `mcp secrets init/set/list` works; `mcp uninstall` cleans up; `mcp rollback` restores client configs.

**Exit criteria:** unit tests green, dry-run install produces correct artifacts, empty repo ships.

### Phase 1 — Serena (global daemon, native-http)
**Scope:** Serena manifest (2 daemons — shared `claude-code` context for Claude Code + Gemini CLI + Antigravity, separate `codex` context for Codex CLI), install path through all 4 clients, verify native-http transport end-to-end.

**Validates:** global-daemon pattern, multi-daemon-per-server, native-http path, all 4 client adapters.

**Deliverable:** Serena runs as 3 shared daemons; all 4 clients use them; `mcp rollback --server serena` works.

### Phase 2 — memory (global daemon, stdio-bridge)
**Scope:** memory manifest, stdio-bridge integration via supergateway, concurrent-write verification.

**Validates:** stdio-bridge path, supergateway integration, solves real correctness issue (JSONL race).

**Deliverable:** single memory daemon serves all 4 clients; concurrent writes serialized.

### Phase 3 — mcp-language-server (workspace-scoped)
**Scope:** workspace registry, deterministic port allocation, per-project `.mcp.json` generation, idle-timeout watchdog, `mcp register` / `unregister` commands.

**Validates:** workspace-scoped pattern, the main goal of the project (prevent LSP proliferation across subagents).

**Deliverable:** `mcp register <workspace> pyright typescript` produces working per-workspace LSPs; parallel client sessions on same workspace share one LSP; idle-timeout reclaims ports.

### Phase 4+ — Additional servers (future, not in this spec)
**Candidates:** sequential-thinking, wolfram, paper-search-mcp. Each = one new manifest + `mcp install`, no core code changes if platform is designed correctly.

**Explicitly excluded:** `mcp-server-fetch` — redundant because all four clients (Claude Code, Codex CLI, Gemini CLI, Antigravity) have native web fetch/search tools built in (`WebFetch`/`WebSearch`, `web_search`, `web_fetch`/`google_web_search`). Remove from client configs during Phase 0 cleanup.

**Not in this spec.** Each future server gets a brief per-server design note (`docs/servers/<name>.md`) but platform code freezes at Phase 3.

## 8. Migration from current state

1. **Inventory current stdio configs** in all 4 clients (completed in this design phase).
2. **Install Phase 0 foundation** — no-op, repo present.
3. **Phase 1 migrate Serena** — per-client, one at a time, backup/restore tested at each step.
4. **Stability window 1-2 days** after Phase 1.
5. **Phase 2 migrate memory** — same flow.
6. **Stability window 1-2 days** after Phase 2.
7. **Phase 3 install mcp-language-server platform** + register first active workspace.
8. **Operational steady-state** — Phase 4+ additions on demand.

At any point, `mcp rollback --all` restores pre-hub state for every migrated server.

## 9. Rollback

Single command per scope:

- `mcp rollback --server <name>` — restore that server's client config entries and delete its scheduler tasks
- `mcp rollback --client <name>` — restore that client's config file to latest backup
- `mcp rollback --all` — full revert across all servers and clients, scheduler tasks deleted, daemons stopped

Each operation leaves `secrets.age` and `.age-key` untouched (secrets survive rollback for future re-install).

## 10. Open questions for implementation

Deferred to implementation phase, not design-blocking:

1. **Gemini/Antigravity URL field exact name** — `url` vs `httpUrl` vs `serverUrl`. Verify against installed CLI version during Phase 1.
2. **uvx/npx PATH under scheduler context** — may differ from interactive shell. Each OS adapter resolves fallback paths explicitly (`$HOME/.local/bin`, `%USERPROFILE%\.local\bin`).
3. **supergateway fork-or-adopt decision** — if supergateway is unmaintained or has issues at Phase 2, decide fork vs replace with native Go bridge using `mark3labs/mcp-go`.
4. **Identity password protection UX** — Phase 0 ships with plain `.age-key`. If users want password-protected identity, UX pattern for entering password at scheduler-launched daemon (no stdin available) needs design: keystore-backed, systemd-askpass-like helper, or startup-time prompt that blocks until provided.
5. **`.mcp.json` in version control** — per-workspace `.mcp.json` generated by hub may or may not be committed. Document recommendation at Phase 3.

## 11. Language choice — full comparison

Final decision: Go. Alternatives considered:

| Criterion | Weight | PS | Python | **Go** | Rust | TS/Node |
|---|---|---|---|---|---|---|
| Cross-platform | ★★★★★ | 2 | 4 | 5 | 5 | 4 |
| Runtime speed (startup) | ★★★★★ | 2 | 3 | 5 | 5 | 3 |
| Runtime speed (concurrency) | ★★★★☆ | 2 | 2 | 5 | 5 | 4 |
| Single binary | ★★★★☆ | N/A | 1 | 5 | 5 | 2 |
| OS abstraction ergonomics | ★★★★☆ | 2 | 3 | 5 | 5 | 3 |
| Time to MVP | ★★★☆☆ | 5 | 4 | 4 | 2 | 4 |
| MCP ecosystem fit | ★★☆☆☆ | 1 | 3 | 2 | 2 | 5 |
| Long-term maintenance | ★★★☆☆ | 3 | 3 | 5 | 5 | 3 |
| **Weighted** | | 2.6 | 3.2 | **4.6** | 4.2 | 3.4 |

Go wins on cross-platform + speed + single-binary deployment + long-term maintenance. Rust close but worse time-to-MVP. TS/Node loses on runtime speed and single-binary deployment. PowerShell fails cross-platform. Python fails single-binary and runtime speed.

## 12. Secrets storage choice — full comparison

Final decision: age-encrypted file. Alternatives:

| Option | Cross-platform UX identical | Secret portable between machines | Encrypted at rest | Setup complexity |
|---|---|---|---|---|
| plain `config.local.yaml` | ☑ | ☑ (plaintext) | ☒ | low |
| OS-native (wincred/libsecret/keychain) | ☒ (different GUIs) | ☒ (tied to user account) | ☑ | low |
| **age-encrypted file** | ☑ | ☑ (identity portable) | ☑ | low |
| sops-YAML | ☑ | ☑ | ☑ | medium |
| pass / GPG | partial | partial | ☑ | high |

age wins on portability + encryption + UX consistency. OS-native is a reasonable enhancement layer for later (optional, identity held in keystore rather than on disk) but not primary.

## 13. Glossary

- **MCP** — Model Context Protocol, the tool/resource-exposure protocol between AI agents and external services.
- **Streamable HTTP** — MCP transport spec revision 2025-06-18, single-endpoint HTTP with optional SSE streaming.
- **Legacy SSE** — earlier MCP HTTP transport, two-endpoint; only Serena's `--transport sse` uses this today; not used in this platform.
- **Global daemon** — a hub-managed long-lived process serving one MCP server-instance to all clients.
- **Workspace-scoped daemon** — a hub-managed process serving one (server, workspace, language) tuple.
- **stdio-bridge** — transport wrapper that adapts a stdio-only MCP server to HTTP for multi-client access.
- **Manifest** — YAML file declaring one server's daemons, transport, command, bindings.
- **Identity** — age secret-key file (`.age-key`) that decrypts the vault.
- **Vault** — age-encrypted `secrets.age` containing key/value pairs.

## 14. Related work — reference implementations

No existing tool covers ≥50% of this spec's requirements (verified via 2026-04-16 GitHub scan of 15+ candidates). Building from scratch is justified. These five projects cover specific slices and should be **studied as references** (not forked):

| Project | Overlap with this spec | What to study |
|---|---|---|
| [Daichi-Kudo/mcp-session-manager](https://github.com/Daichi-Kudo/mcp-session-manager) | Architectural twin: daemon-per-server + stdio-proxy + per-port allocation. TypeScript, 0 stars, 3-server hardcoded. | SIGINT-shielding across client sessions; stdio-proxy lifecycle; per-port allocation table |
| [regression-io/coder-config](https://github.com/regression-io/coder-config) | Only tool that writes ALL 4 of our target client configs (Claude/Codex/Gemini/Antigravity). JS, 43 stars. | Multi-client config writer patterns; hierarchical global→workspace→project merge logic |
| [mozilla-ai/mcpd](https://github.com/mozilla-ai/mcpd) | TOML manifest schema, CLI surface (`mcpd daemon`), secrets-file layout. Go, active. | Manifest schema design; secrets-file structure at `~/.config/mcpd/`; CLI subcommand decomposition |
| [vlazic/mcp-server-manager](https://github.com/vlazic/mcp-server-manager) | Closest Go single-binary architecture with systemd installer and YAML manifests. 15 stars, Oct 2025. | Go project layout for this problem space; systemd-user unit installer |
| [supercorp-ai/supergateway](https://github.com/supercorp-ai/supergateway) | Stdio↔HTTP bridge primitive — the exact building block needed for `transport: stdio-bridge` daemons. Active. | **Embed or vendor directly** as the stdio-bridge implementation (req 4 of §2 goals). Fallback: native Go implementation using `mark3labs/mcp-go`. |

**Architectural anti-pattern to avoid:** single-endpoint aggregators that own the MCP URL seen by clients (samanhappy/mcphub, ravitemer/mcp-hub, docker/mcp-gateway, IBM mcp-context-forge, MCPJungle, MetaMCP, agentgateway, etc.). These solve multi-tenant team deployments, not local per-user daemon sharing with workspace scoping. Our design keeps each daemon independently addressable on its own port so clients' native configs point directly at it — no router in between.

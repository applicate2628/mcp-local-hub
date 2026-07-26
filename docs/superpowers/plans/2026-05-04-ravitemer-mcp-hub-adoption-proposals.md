# ravitemer/mcp-hub Adoption Proposals

This is an intake note for future Phase 3/Phase 3B follow-up planning. It is
not an implementation plan and does not change the accepted Phase 3B-II backlog
by itself.

## Evidence Snapshot

| Source | Evidence |
|---|---|
| `ravitemer/mcp-hub` | Local read-only clone under ignored `.scratch/` at commit `9c7670a4c341ed3cf738a6242c0fde1cea40bccf`; live GitHub README described version `4.2.1`, MIT license, unified `/mcp`, REST `/api/*`, marketplace, remote transports, OAuth, workspace cache, SSE events, JSON5, and VS Code compatibility. |
| Local project | `README.md`, `docs/superpowers/plans/phase-3b-ii-backlog.md`, `docs/phase-3b-verification.md`, `docs/phase-3b-ii-verification.md`, `internal/api`, `internal/gui`, `internal/daemon`, and `internal/config` show a Windows-first local install/migration/scheduler/tray architecture with embedded manifests and per-daemon MCP endpoints. |

## Strategic Fit

`ravitemer/mcp-hub` optimizes for one gateway process that exposes all managed
servers through one MCP endpoint plus REST/Web management. `mcp-local-hub`
optimizes for local Windows client-config migration, scheduler-backed shared
daemons, backups, secrets, tray, and safety-first install flows.

The safest adoption path is additive: keep the existing per-daemon endpoints
and client-specific migration model, then add selected gateway, discovery, and
compatibility surfaces as optional Phase 3C+ work.

## Candidate Backlog

| Priority | Candidate | Proposed Fit |
|---|---|---|
| P1 | Feature-support/readiness matrix | Add a README or docs matrix for transports, auth, capabilities, GUI/CLI/API surfaces, tested platforms, and known untested paths. This supports the preview warning and prevents overclaiming. |
| P1 | Unified health endpoint | Add one JSON endpoint that combines GUI state, version/build info, daemon status, client routing, ports, process info, workspace registry, and probe summaries. It can sit beside existing `/api/ping`, `/api/status`, and `/api/version`. |
| P1 | Optional unified MCP endpoint | Add an opt-in `Hub` endpoint that lists tools/resources/prompts from all selected daemons with stable namespacing such as `memory__...` and `godbolt__...`. This must not replace existing per-server endpoints. |
| P2 | Remote MCP manifests | Extend manifest handling for remote `url + headers + secrets` entries so direct HTTPS servers like `context7` are first-class instead of special-case wiring. |
| P2 | Capability browser | In GUI, show probed tools/resources/prompts per server with timestamps and probe errors. Tool execution should stay disabled or explicitly gated because some tools execute local commands. |
| P2 | Marketplace/import flow | Add browse/import from an MCP registry as a draft-manifest flow: inspect metadata and README, generate YAML, validate, dry-run, then install. Avoid automatic install side effects. |
| P2 | VS Code workspace/JSON5 import compatibility | VS Code user-profile install support is now in tree. Still add import for workspace `.vscode/mcp.json`, accept both `servers` and `mcpServers`, and support common placeholders such as `${env:VAR}`, `${workspaceFolder}`, `${userHome}`, and `${pathSeparator}`. |
| P2 | Config watch/dev reload | For development manifests, watch selected files and restart only affected daemons while publishing SSE lifecycle events. |
| P2 | Structured logs/events | Add consistent JSON event envelopes for GUI/API lifecycle events and daemon failures while preserving the existing human-readable logs. |
| P2 | Public contribution/security docs | Add `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`, and `CHANGELOG.md` before broader publication or commercial outreach. |

## Client Expansion Note (2026-05-04)

The install/read surfaces now distinguish default and opt-in clients:

| Group | Client ids | Planning note |
|---|---|---|
| Default | `claude-code`, `codex-cli` | Safe first-run write set for `install` and workspace `register`. |
| Opt-in | `cursor`, `vscode`, `gemini-cli`, `qwen-cli`, `antigravity` | User must pass `--clients ...` or `--all-clients`; keep live smoke and import compatibility in the release-hardening backlog. |

## Do Not Copy Directly

| Pattern | Reason |
|---|---|
| Mandatory single `/mcp` endpoint | It would weaken the current client-specific daemon isolation and migration model. Make it opt-in. |
| Node/JavaScript runtime architecture | The local project is already built around Go, embedded servers, Windows Task Scheduler, and a static embedded GUI. |
| Unrestricted `${cmd:...}` placeholders | This is a command-execution surface. If adopted, it needs an explicit unsafe gate, dry-run display, and clear audit output. |
| Automatic marketplace install | The local project should preserve inspect -> validate -> dry-run -> backup -> apply semantics. |
| Remote access as a default | Current GUI and daemon surfaces intentionally bind loopback only and have no auth/TLS layer. |

## Suggested Phase Placement

| Phase | Work |
|---|---|
| Phase 3B-II hardening | README/support matrix, unified health endpoint, capability-status display if low-risk, backlog cross-links. |
| Phase 3C | Optional unified MCP endpoint and namespaced capability routing. |
| Phase 3C/3D | Remote manifests, VS Code/JSON5 import, marketplace draft-manifest flow. |
| Phase 4+ | Cross-platform scheduler and server/headless readiness, keeping remote access out of default scope unless separately designed. |

## Acceptance Notes For Future Planning

- Any unified endpoint must preserve existing daemon isolation, per-client
  routing, backups, dry-runs, and rollback paths.
- Any remote-server support must route secrets through the existing encrypted
  vault model where possible.
- Any config-import feature must treat external config as untrusted input and
  show the generated manifest before writing client configs or scheduler tasks.
- Any marketplace feature must cache metadata with clear freshness and fallback
  behavior, but stale data must not silently auto-install.
- Any capability execution from GUI must be separately threat-modeled because
  tools such as shell, benchmark, debugger, and file-edit operations can have
  local side effects.

## Terms and Abbreviations

- `API`: Application Programming Interface; here, local HTTP routes used by the GUI and CLI wrappers.
- `CLI`: Command-Line Interface; shell commands such as `mcphub install`.
- `Cursor`: Cursor editor/agent client; a supported explicit opt-in install target.
- `GUI`: Graphical User Interface; the embedded local web interface and tray surface.
- `JSON5`: JSON-compatible config format that supports comments and trailing commas.
- `MCP`: Model Context Protocol; the protocol used by managed clients and servers.
- `OAuth`: authorization flow commonly used by remote services.
- `P1` / `P2`: rough priority labels; `P1` is higher priority than `P2`.
- `Qwen CLI`: Qwen Code command-line client; opt-in install target.
- `RCE`: Remote Code Execution; a security risk where a caller can cause command execution.
- `SSE`: Server-Sent Events; HTTP event streams used for live updates.
- `VS Code`: Visual Studio Code; editor whose `.vscode/mcp.json` format is useful as an import target.

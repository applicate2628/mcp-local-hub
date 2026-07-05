---
status: proposed
date: 2026-06-19
owner: architect (design) / planner (phase-1 plan)
supersedes: none
relates-to:
  - excalidraw native install management (user ask)
  - work-items/bugs/2026-06-19-context7-remote-mcp-mishandling.md (sibling non-stdio install surface)
---

# Decision: `kind: companion` + `transport: process` — hub-managed non-MCP companion process

## Context

The user runs a native (NOT hub-routed) MCP server `excalidraw` plus a SEPARATE
non-MCP "canvas" process — a Node Express server (`dist/server.js`, listens on
:3000) that MUST run with cwd = its package dir (it writes `excalidraw.log` +
serves cwd-relative web assets + stores drawing elements). The MCP server (stdio,
run by the client) syncs to the canvas via `EXPRESS_SERVER_URL`.

Today the canvas is kept alive by a fragile user-level stopgap: a Windows
Startup-folder `.vbs` that sets `CurrentDirectory` then launches node. The user's
explicit ask: the **hub** should MANAGE this companion process lifecycle (correct
cwd, persistence across reboots, restart on crash) **WITHOUT** routing its traffic
through the hub's MCP aggregation. Verbatim: «пусть excalidraw работает нативно,
не надо его маршрутизировать через хаб», «хаб только install management», «вот это
и есть причина, почему я попросил его устанавливать через хаб» — the
process-management fragility is *why* hub involvement was requested.

## Decision

Add a new manifest **`kind: companion`** paired with a new **`transport: process`**
on the existing `config.ServerManifest`, reusing the `daemons[]` list and its
already-present `DaemonSpec.Cwd` field.

**Why this shape (architect, file:line-anchored):** supervised lifecycle
(Job-Object orphan-protection, restart policy, backoff/quarantine, autostart, GUI
status) is ALL driven by `m.Daemons` → `SupervisorDaemon` → reconcile → spawn,
while MCP routing is ALL driven by a SEPARATE field, `m.ClientBindings`. A
companion is therefore "a manifest with `daemons[]` but no `client_bindings`": it
slots into supervision and falls out of routing **by construction**. Critically,
`DaemonSpec.Cwd` (the bug's root cause) ALREADY exists, is validated absolute, and
is consumed as `cmd.Dir` (manifest.go:106-120; daemon.go:178-182,247-251;
readiness.go:308-309) — the cwd-injection mechanism is shipped, not a gap.

### Rejected alternatives
- **`companions[]` in `supervisor-intent.json`** — forks the supervision pipeline
  (a second spawn-desired set, GUI source, build path). A companion IS a
  supervised process; it belongs in the daemon stream, flagged, not paralleled.
- **`sidecar:` on an MCP server manifest** — couples lifetimes; the excalidraw MCP
  server is run by the *client* (stdio), so there is no hub-side MCP manifest to
  attach to.
- **bare `companion: bool`** — bypasses the validator's `kind`/`transport` single
  dispatch (manifest.go:385-390) for scattered `if m.Companion` checks.

## Exclusion model (the safety-critical part)

A companion must never appear as an MCP server. Exclusion is **structural** —
routing is `ClientBindings`-keyed (companion has none → inert) — reinforced by a
single source-filter and three sink-guards:

- **Source filter:** `readManifestNames` (scan.go:1306) skips `kind: companion`
  → excludes it from classify, the Servers matrix, and via-hub detection at once.
- **Sink guards (defense-in-depth):** `BuildResolverSnapshot…` (hub_mcp_resolver.go:221),
  install client-update loop (install.go:1499), migrate (migrate.go:187).
- **Deliberately NOT excluded:** port enumeration (`manifestDaemonPorts`
  scan.go:1344 — :3000 SHOULD be a known-taken port for collision avoidance) and
  readiness (readiness.go:284-322 already resolves the entry script under
  `d.Cwd` — a *desired* surface that verifies `dist/server.js` exists).

## Autostart (the `.vbs` replacement)

Free: a companion in `supervisor-intent.json` auto-starts because the supervisor
auto-starts on logon and reconciles every `intent.Daemons` row
(supervise_reconcile.go:139). **No extra Task Scheduler entry needed.**

## Phase 1 scope (smallest correct PR)

1. `internal/config/manifest.go` — `KindCompanion`/`TransportProcess` consts +
   `Validate()` companion branch (require `command`; allow `daemons[]`+`cwd`;
   REJECT `client_bindings`/`languages`/`port_pool`/`daemon_template`/`url`/`headers`).
2. `internal/cli/daemon.go:331` — new `transport == process` branch: run
   `cmdPath + childArgs` with `cmd.Dir = spec.Cwd`, NoConsole, log to `logPath`;
   on child exit return error so the supervisor respawns. NO HTTP host, NO port bind.
3. `internal/api/scan.go:1306` — `readManifestNames` filters `kind: companion`.
4. Three one-line `kind == companion` sink guards (hub_mcp_resolver.go:221,
   install.go:1499, migrate.go entry).
5. `servers/excalidraw-canvas/manifest.yaml` — `kind: companion`,
   `transport: process`, `command: node` (use absolute node path — supervisor's
   reduced PATH may not resolve `node`, same hazard as the gdb/lldb PATH injection),
   `base_args: ["dist/server.js"]`, `daemons: [{name: default, cwd: "<package dir>"}]`.

**Deferred to phase 2:** a dedicated "Companion processes" GUI card with
start/stop/uninstall (phase 1 shows it on the Dashboard via supervisor IPC
immediately); companion-specific readiness panel; `mcphub companion add/remove`
ergonomics.

## Risks
- **Highest: exclusion-miss** → a non-MCP HTTP process routed as MCP. Mitigated by
  the dual structure (ClientBindings-keyed routing + source-filter + sink-guards);
  the planner's claims (1-3) carry enforcement probes.
- **`command: node` PATH** under the supervisor's reduced logon-trigger PATH — use
  an absolute command or `${ENV}` expansion; verify with a live spawn.
- cwd-injection gap: **NONE** (`DaemonSpec.Cwd` shipped).

## Status / next
`proposed`. Blocked on PR #379 (serena router) merge — phase-1 touches
scan.go/install.go/migrate.go, the same files #379 edits; sequence after #379
to avoid conflicting edits. Then hand to `$planner` (+ `$ux-designer` for the
phase-2 companion GUI section).

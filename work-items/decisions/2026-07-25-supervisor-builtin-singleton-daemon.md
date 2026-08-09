---
title: The supervisor auto-spawns `mcphub route` as a supervisor-owned built-in singleton daemon, seeded on disk by the supervisor itself
status: proposed
date: 2026-07-25
owner: backend-engineer (Increment 1b implementer)
supersedes: none
relates-to:
  - work-items/decisions/2026-07-25-mcp-data-plane-off-gui-onto-supervised-front-daemon.md
  - work-items/active/2026-07-25-mcp-front-daemon/
---

## The question

Increment 1 built `mcphub route` (`internal/cli/route.go`) — a standalone,
read-only serena+LSP front daemon that survives GUI death — but stopped short
of making the supervisor actually spawn and manage it. `mcphub route` has to
run *somewhere* continuously for Increment 1's whole premise (MCP survives GUI
death) to hold. How does the supervisor come to own it, given supervisor-intent
rows are otherwise always either (a) manifest-derived (one row per catalog
server's daemon, or per registered workspace) or (b) legacy/operator-authored?
`mcphub route` is neither — it has no catalog manifest and no per-workspace
registry entry backing it.

## Decision

**Reuse the existing `SupervisorDaemon` descriptor shape verbatim and add
exactly one new seeder that persists a single reserved row on disk at
supervisor startup — the "supervisor-owned built-in singleton daemon"
pattern.** No new descriptor kind, no new spawn path, no new reconcile branch,
no new IPC command, no schema change.

Concretely (see `internal/api/builtin_route_daemon.go` +
`internal/cli/supervise.go`'s post-`loadIntentFiles` call site):

1. `BuildBuiltinRouteDaemon(command, port) SupervisorDaemon` returns an
   ordinary global-shaped descriptor: `TaskName: "\mcp-local-hub-route-front"`
   (reserved, canonical), `Server: "route"` (reserved, not a catalog name),
   `Daemon: "front"`, `Command`/`Args`/`Port` matching exactly what `mcphub
   route --port <N>` needs, `RuntimeSpec: nil`.
2. `EnsureBuiltinRouteDaemon(f, command, port) bool` UPSERTs that row into an
   in-memory `*SupervisorIntentFile` by the reserved task name: add if absent,
   replace if drifted (binary relocated, port constant bumped), no-op if
   already canonical.
3. `ensureBuiltinRouteDaemonAtStartup` (internal/cli, unexported) calls both of
   the above right after `runSupervise`'s `loadIntentFiles`, projects the
   canonical routing epoch under the API routing lease, mutates the in-memory
   `intent` this cold start's own first reconcile pass will use, and — only
   when the row changed — persists it through the SAME flocked read-modify-write
   every other supervisor-intent writer uses
   (`api.MutateSupervisorIntentIfChanged`). Stable GUI uses its requested port;
   stable or recovering front epochs use the already admitted port. The routing
   lease spans epoch projection through this descriptor persist, so an explicit
   epoch transition cannot interleave an A/B descriptor split.

Everything downstream (reconcile's spawn-desired set, the production
`SpawnFunc`, the restart-policy state machine, the liveness sweep, `mcphub
status`) treats this row exactly like a manifest-derived global daemon row,
because it IS one, structurally. Nothing had to change to make that true.

## Why this pattern, not a new mechanism

- **The descriptor shape was already general enough.** `SupervisorDaemon` is
  `{TaskName, Server, Daemon, Command, Args, Port, ...}` — the fields a global
  daemon needs, not the fields a *manifest-derived* daemon needs. `mcphub
  route --port 9137` fits it exactly with zero new fields.
- **The reconcile spawn-exclusion predicates are argv-shaped, and `route`'s
  argv doesn't match either shape.** `IsSerenaProxyDescriptor` and
  `IsWorkspaceLSPProxyDescriptor` (`internal/api/supervisor_port_owner.go`)
  both require `Args[0] == "daemon"`; the route descriptor's `Args[0] ==
  "route"`. This is not a coincidence to preserve carefully — it is simply the
  correct outcome of the route descriptor being a DIFFERENT top-level
  subcommand (`mcphub route`, not `mcphub daemon ...`), so the two
  proxy-specific exclusions were never going to apply. No new predicate,
  no new "skip route" branch.
- **The only genuinely missing capability was durability, not spawn logic.**
  Reconcile already spawns anything present in its intent snapshot; the gap
  was that nothing put the route row INTO that snapshot durably. An
  in-memory-only add is dropped by the next 60s `IntentWatcher` re-read
  (`supervisor_controller.go`'s `intentCache` swap only ever keeps what
  `refreshSupervisorIntent` reads back off disk) — so the seeder's entire job
  is "make sure the row is on disk before anything reads it back", which is
  exactly what `MutateSupervisorIntentIfChanged`'s flocked read-modify-write
  is for. This is the ONE new capability; everything else is reuse.
- **Reserved-name isolation, not new ownership logic.** `buildMergedSupervisorIntent`'s
  per-server ownership scan (install/uninstall/reinstall) only ever claims a
  row whose `Server` field matches the manifest name being installed, or a
  blank-`Server` legacy row whose task-name prefix matches. Because
  `BuiltinRouteServer = "route"` is reserved and mechanically verified to
  never collide with a real catalog manifest name
  (`TestBuiltinRouteDaemon_ReservedServerNameNotClaimedByAnyShippedManifest`),
  the existing ownership scan simply never touches this row for ANY other
  server's install or uninstall. No new "skip this reserved row" branch was
  needed in the merge logic — the existing scan's own matching rule already
  excludes it, by construction, as long as the name stays reserved.

## What this pattern generalizes to (for a future second built-in daemon)

If mcphub ever needs a second supervisor-owned singleton with no catalog
manifest and no per-workspace registry backing (the group-aggregate hub
listener is a candidate, though it currently runs in-process inside the GUI,
not supervisor-managed), the same three-piece shape applies: a
`BuildBuiltinXDaemon` pure constructor with a reserved `TaskName`/`Server`
pair, an `EnsureBuiltinXDaemon` upsert, and one startup call site that
persists it. The reserved `Server` name must be added to the mechanical
catalog-collision test (or a shared reserved-names registry, if a THIRD such
daemon ever appears — two reserved names each with their own bespoke
mechanical test is fine; a third would be the trigger to consolidate into one
table-driven test, not before).

## Non-goals (explicitly out of scope for this decision)

- This decision does NOT change reconcile's spawn/terminate decision logic,
  the restart-policy state machine, `internal/daemon/host.go`'s
  `composeChildEnv`, or any merge ownership predicate.
- This decision does NOT make the route daemon's own console-attach behavior
  special-cased: it inherits `MCPHUB_NO_CONSOLE_ATTACH` through the existing
  `mergeDaemonEnv(os.Environ(), d.Env, overlayEnv)` composition every daemon
  gets, mechanically confirmed by
  `TestProductionSpawnFn_RouteDescriptorInheritsConsoleAttachSuppression`
  (internal/cli).
- This decision does NOT address Increment 2 (flipping port ownership so the
  front daemon binds the GUI's own port `P`) — that stays a separate,
  contract-gated, security-sensitive-shaped item.

## Verification

- `internal/api/builtin_route_daemon_test.go`:
  `TestBuiltinRouteDaemon_SurvivesUnrelatedServerInstallThenUninstall`,
  `TestBuiltinRouteDaemon_ReservedServerNameNotClaimedByAnyShippedManifest`.
- `internal/cli/builtin_route_daemon_test.go`:
  `TestEnsureBuiltinRouteDaemonAtStartup_PersistsAndSurvivesReread`,
  `TestProductionSpawnFn_RouteDescriptorInheritsConsoleAttachSuppression`,
  `TestBuildBuiltinRouteDaemon_PortMatchesArgsPortFlag`.
- All five are mutation-proven (see the implementer's report in this
  work-item's session log / status.md for each mutation + observed failure).

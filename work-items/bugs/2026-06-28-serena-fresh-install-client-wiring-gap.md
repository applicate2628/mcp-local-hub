# Bug: router-native serena flip leaves a fresh-install client-wiring gap — install/install-all/matrix-Apply no longer write the /serena/mcp client entry

- id: 2026-06-28-serena-fresh-install-client-wiring-gap
- context: feat/serena-router-native-catalog (area-4 REVISE, commit 3c2bfc2a)
- status: open
- severity: high
- area: internal/api/install.go, internal/api/migrate.go, internal/gui/install.go, servers/serena/manifest.yaml
- found-by: qa-engineer

## Reproduction (static trace — no runtime repro possible without a fresh host)

The area-4 flip removed `client_bindings` (and `daemons[unified@9121]`) from
`servers/serena/manifest.yaml` and set `kind: workspace-scoped`. The
client-wiring-to-router override lives at `internal/api/install.go:1591-1603`,
INSIDE the `for _, b := range m.ClientBindings` loop (`install.go:1566`). With
`client_bindings` now empty, that loop iterates zero times → `BuildPlanWithOpts`
emits ZERO `ClientUpdates` for serena. The override `url =
SerenaRouterClientURL(opts.GUIPort)` (install.go:1599) is now unreachable for the
shipped serena catalog.

Three fresh-install client-wiring surfaces that wrote `/serena/mcp` pre-flip are
now broken or skipped:

1. `mcphub install serena` / GUI `POST /api/install` (→ `(*API).Install`,
   install.go:253) → REFUSED by `refuseWorkspaceScopedInstall` (serena is now
   workspace-scoped). Pre-flip (serena `kind: global`) this wired all 7 clients
   to `/serena/mcp` via the binding loop + override (the #400 fix).
2. GUI "Install all" / `InstallAll` (install.go:287, 298-348) → SILENTLY SKIPS
   workspace-scoped serena.
3. GUI Servers-matrix "check serena cell + Apply" (`/api/migrate` →
   `MigrateFrom` → `migrateOneBinding`): the manifest-binding loop
   (migrate.go:128) is empty; the synthesized-binding pass (migrate.go:148) calls
   `primaryDaemonName(m)` which fails for a no-daemon manifest →
   **Failed row**: `"manifest serena: no daemons declared; cannot migrate
   non-binding client"` (migrate.go:163-172).

## Expected vs actual

- Expected (epic `install-and-it-works-ux` area-4, lines 52-58, 139-142): a
  fresh operator points a client at `/serena/mcp` and the happy path works via
  auto-register-on-miss "no manual migrate on the happy path". For that,
  SOMETHING must write `/serena/mcp` into the client config at install/setup
  time.
- Actual: the ONLY automatic path that writes `/serena/mcp` into a client config
  post-flip is `mcphub migrate serena legacy-to-dynamic-pool` (its
  `ReconcileSerenaClientsToRouter` at serena_client_reconcile.go:298 wires every
  in-scope client unconditionally — it works even on a fresh zero-legacy host).
  Auto-register-on-miss (`attemptSerenaAutoRegister`, serena_router.go:666) only
  fires once a request has ALREADY reached `/serena/mcp` — it cannot bootstrap
  the client→router URL itself. So a brand-new operator who never had legacy
  serena has no install/GUI affordance that wires the client; they must discover
  and run the migrate command the epic explicitly says should NOT be needed on
  the happy path.

## Why this is the load-bearing gap

The agent's flip tests (serena_router_native_flip_test.go,
migrate_serena_router_native_test.go) cover synthesizer identity, §7.1 gate
inertness, the classifier, and the legacy-intent-host cutover — but NOT the
end-to-end question "does a fresh operator's client config get `/serena/mcp`
after a fresh install". No test asserts a fresh-install `ClientUpdate`/client
entry for the flipped serena.

## Suggested resolution (for the implementer / architect)

Either (a) route a fresh GUI/CLI serena install through
`ReconcileSerenaClientsToRouter` (the existing single-owner wiring path) when
`IsSerenaServer(name)` and the manifest is the dynamic-pool shape — instead of
refusing/skipping; or (b) explicitly accept the migrate command as the fresh
serena onboarding path and make it discoverable (GUI affordance + docs), and add
a test pinning the chosen fresh-install wiring contract end-to-end. This is a
scope/route decision for the architect, not a QA fix.

# Bug: GUI Servers-matrix serena cell is structurally unreachable for the dynamic-pool catalog — Apply returns "no daemons declared" instead of wiring the router

- id: 2026-06-28-serena-matrix-migrate-dead-for-dynamic-pool
- context: adjacent-finding
- status: open
- severity: medium
- area: internal/api/migrate.go
- found-by: backend-engineer (while fixing 2026-06-28-serena-fresh-install-client-wiring-gap)

## Reproduction (static trace)

The serena `/serena/mcp` router override in the matrix-migrate path lives at
`internal/api/migrate.go:233-234`:

```go
if IsSerenaServer(server) && guiPort > 0 {
    url = SerenaRouterClientURL(guiPort)
}
```

That override is INSIDE `migrateOneBinding`, which is reached from the
synthesized-binding pass for a targeted (matrix-toggled) client. But for the
post-flip dynamic-pool serena catalog (`servers/serena/manifest.yaml`, no
`daemons[]`, only a `daemon_template`), the synthesized-binding pass at
`migrate.go:148-180` first calls `primaryDaemonName(m)` (`migrate.go:163`):

```go
primaryDaemon, ok := primaryDaemonName(m)
if !ok {
    report.Failed = append(report.Failed, FailedMigration{
        Server: server, Client: client,
        Err: fmt.Sprintf("manifest %s: no daemons declared; cannot migrate non-binding client %q", server, client),
    })
    continue
}
```

`primaryDaemonName` returns `("", false)` for a zero-daemon manifest
(`migrate.go:319-321`):

```go
func primaryDaemonName(m *config.ServerManifest) (string, bool) {
    if len(m.Daemons) == 0 {
        return "", false
    }
    ...
}
```

So the flow short-circuits to a **Failed row** at `migrate.go:168-172` and
`continue`s — it NEVER reaches `migrateOneBinding`, and therefore never reaches
the serena `/serena/mcp` override at `migrate.go:233-234`. The serena override is
structurally unreachable for the dynamic-pool shape.

## Expected vs actual

- Expected: a serena cell toggled ON in the GUI Servers matrix + Apply wires the
  client config to the `/serena/mcp` router (the same single-owner outcome the
  fresh `mcphub workspace register` hook now produces).
- Actual: the cell returns a **Failed row**
  `"manifest serena: no daemons declared; cannot migrate non-binding client
  <client>"`. The override that would have written the router URL is dead code for
  the shipped serena catalog.

## Why this is adjacent (not in the fresh-install-wiring-gap fix scope)

The fresh-install wiring gap (2026-06-28-serena-fresh-install-client-wiring-gap)
was fixed by routing `mcphub workspace register` through
`api.ReconcileSerenaClientsToRouter` (the single owner). The GUI Servers-matrix
`/api/migrate` → `MigrateFrom` → `migrateOneBinding` path is a DIFFERENT entry
surface with its own zero-daemon short-circuit; consolidating it onto the same
reconcile owner is an architecture/scope decision, not part of the register-hook
fix.

## Suggested resolution (for the architect / orchestrator)

Either (a) special-case `IsSerenaServer(server)` in `MigrateFrom`'s
synthesized-binding pass BEFORE the `primaryDaemonName` daemon check so a
dynamic-pool serena cell routes through `ReconcileSerenaClientsToRouter` (the
single-owner consolidation) instead of failing on the absent daemon; or (b)
explicitly mark the serena matrix cell as register-driven in the GUI and remove
the now-dead synthesized-binding path for serena. Deferred — orchestrator's scope
call.

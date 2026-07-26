---
title: HEAD 4d2d8295 registers route/front 9137 in configs/ports.yaml, which its own guard test asserts must NOT be there — TestDefaultRouteDaemonPort_NotInPortsYAMLOrGUIOrSerenaPool is RED on the branch
severity: medium
found-by: backend-engineer, codex bot PR #588 finding-fix round (pre-existing failure encountered during the gate run)
affected-surface: configs/ports.yaml (route/front row), internal/cli/route_port_test.go (TestDefaultRouteDaemonPort_NotInPortsYAMLOrGUIOrSerenaPool)
context: adjacent-finding
status: open
branch: feat/mcp-front-daemon
---

## Symptom

On `feat/mcp-front-daemon` at 4d2d8295 (before the PR #588 finding-fix commit),
`go test ./internal/cli/` fails:

```
--- FAIL: TestDefaultRouteDaemonPort_NotInPortsYAMLOrGUIOrSerenaPool (0.00s)
    route_port_test.go:40: DefaultRouteDaemonPort (9137) collides with configs/ports.yaml entry route/front
```

## Cause — two commits assert opposite things

- `f262df68` ("fix(mcphub-route): retarget default port off the godbolt
  collision (F2)") added `internal/cli/route_port_test.go`, whose contract is
  "`DefaultRouteDaemonPort` must NOT appear in `configs/ports.yaml`" — a
  reasonable guard when 9137 was an unregistered hand-picked constant.
- `4d2d8295` ("fix(route): register the route daemon's 9137 in the port
  ledger") then ADDED `route/front: 9137` to `configs/ports.yaml`, with an
  explicit and good rationale (a port claim that lives only in a Go constant is
  invisible to the ledger, and a sibling branch had already taken 9137 while
  the route daemon was answering on it).

Both changes are individually correct. Together they contradict: the guard
reads every ledger row as a FOREIGN claim, so registering the route daemon's
OWN claim trips it.

## Provenance (not caused by the PR #588 fix round)

The PR #588 finding-fix commit touches neither file:

- `git diff --name-only` on that commit lists no `configs/ports.yaml` and no
  `internal/cli/route_port_test.go`;
- the failing assertion cites the `route/front` ledger row, which
  `git show 4d2d8295 -- configs/ports.yaml` shows was added by that earlier
  commit;
- the test reads only `configs/ports.yaml`, `config.ReservedGUIPort`,
  `DefaultRouteDaemonPort`, and `api.EffectiveSerenaPortPool` — none of which
  that commit modifies.

## Suggested fix (NOT applied — outside the approved change surface)

Preferred: keep the ledger row (4d2d8295's rationale stands — the ledger must
be authoritative) and narrow the guard to FOREIGN rows only, e.g. skip the row
whose `(server, daemon)` is `(api.BuiltinRouteServer,
api.BuiltinRouteDaemonName)`. The guard's real intent is "9137 is not claimed
by SOMEONE ELSE", and a self-claim is exactly what registration is supposed to
produce. Update the test's doc comment in the same change so the contract it
states matches the contract it enforces.

The alternative — dropping the ledger row — should be rejected: it re-opens the
cross-branch collision 4d2d8295 was filed to close.

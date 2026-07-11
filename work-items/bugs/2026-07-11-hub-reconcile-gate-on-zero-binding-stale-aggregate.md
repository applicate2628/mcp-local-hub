# `BuildHubReconcilePlan` leaves a stale `mcphub-hub` for a zero-binding client under gate-ON

- **status:** open
- **severity:** medium (stale aggregate URL; connection-refused surface after last-server removal)
- **filed:** 2026-07-11
- **context:** adjacent-finding (surfaced by the de-adopt multi-model design review)
- **owner:** unassigned (pre-existing; NOT de-adopt v1 scope)

## Symptom

Under gate-ON, when a client's LAST manifest binding is removed (e.g. the operator uninstalls
their last server bound to that client, then runs `mcphub install --reconcile-hub-mode`), the
client's `mcphub-hub` aggregate entry is NOT pruned — it lingers pointing at a hub route that
now aggregates nothing for that client.

## Root cause

`BuildHubReconcilePlan` (`internal/api/install_hub_reconcile.go:103-324`) removes the
`mcphub-hub` aggregate from EVERY supported client ONLY on the gate-OFF path (the sweep at
`:164-180`). The gate-ON path iterates only clients WITH at least one binding
(`if len(refs) == 0 { continue }`, `:181-185`) — a client that has dropped to zero bindings
is skipped entirely, so no `Remove mcphub-hub` op is ever emitted for it under gate-ON.

## Impact

A stale `mcphub-hub` entry remains in the client config. It is inert (the resolver routes
nothing for that client), but it is misleading and can present as a connection surface. Bounded
to the "last binding for a client removed under gate-ON" case.

## Fix direction (not scoped here)

Extend the gate-ON path to sweep supported clients and emit `Remove mcphub-hub` for those with
ZERO remaining bindings (mirroring the gate-OFF all-client sweep at `:164-180`). The sole
caller (`internal/cli/install.go` reconcile-hub-mode) then also prunes stale aggregates — a
correctness improvement to DECLARE behavioral. This is the shared-owner change the de-adopt
multi-model memo called F-B/#6; the de-adopt-side use folds into the gate-ON de-adopt follow-up
(`work-items/backlog/2026-07-11-deadopt-subset-and-gate-on-followup.md`).

## Why not fixed in de-adopt v1

De-adopt v1 refuses gate-ON (decision
`work-items/decisions/2026-07-11-deadopt-v1-all-clients-only-scope.md`), so v1 never needs the
prune. The gap is pre-existing and independent of de-adopt; filed for the reconcile owner.

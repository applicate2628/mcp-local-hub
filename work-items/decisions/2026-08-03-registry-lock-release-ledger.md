---
status: proposed
date: 2026-08-03
slug: registry-lock-release-ledger
related-pr: PR #590
---

# Use one process-local Registry and Serena lock-release coordinator

## Context

Registry operations, Serena removal fences, and the Serena supervisor-intent repair leaf use reportable flock releases. An unlock failure can retain the operating-system handle, so a later same-process acquire must distinguish that ghost from ordinary foreign contention and must never enter a blocking acquire on its own retained handle.

## Proposed decision

1. Registry blocking/non-blocking locks, Serena removal-fence acquire/observe, and the Serena supervisor-intent repair leaf route through one lock-release coordinator keyed by the exact lock-leaf path.
2. `lockReleaseLedgerEntry` is the single ghost-check and acquire-reservation owner. `beginBlockingAcquire` and `beginTryAcquire` are its sole blocking and non-blocking transitions. Its mutex protects ghost, acquiring, held, and state-change publication; callers do not duplicate those predicates.
3. Each acquired leaf returns a concurrency-safe, one-shot `func() error` release. `recordUnconfirmedLockRelease` records the first failure and preserves the original cause.
4. Coordinator identity is stable for the process lifetime, including healthy paths. Healthy entries retain only mutex/channel/boolean state and no flock handle. Stable identity prevents an ABA split in which an old waiter observes one entry while a new same-path acquirer reserves through a replacement entry.
5. Ghost state is process-lifetime and recovery is process exit only. It is never serialized or cleared in-process.
6. No caller may discard a release error or adapt the callback back to `func()`.
7. `ReleaseAndJoin` is the shared spelling for preserving a primary error and joining release failure.
8. Reconciliation snapshots fail open when already loaded: the usable snapshot is returned together with the release error so consumers can continue safely and report uncertainty.
9. Operations whose mutation can commit before release structurally separate primary/mutation and release outcomes. Release-only failure does not roll back a committed registration or Serena deletion.
10. No persisted-state migration is introduced.

## Consequences

- A same-process Registry or Serena ghost fails loudly instead of appearing as ordinary contention or entering a later blocking acquire.
- Original unlock causes remain available through `errors.Is` together with `ErrLockReleaseUnconfirmed`; Serena fence callers additionally retain `ErrSerenaRemovalFenceReleaseFailed`.
- Blocking acquisition still waits on foreign-process contention. `TryLock` paths remain non-blocking and report healthy contention without an error.
- Healthy coordinator entries are intentionally retained for stable process-lifetime identity. Safe reclamation would require explicit waiter/reference ownership and is outside this decision.
- Only leaves routed through `lockLeafLedgered*` or `tryLockLeafLedgered*` participate. The Serena removal fence is explicitly routed through those helpers; other flock families retain their existing independent ownership contracts.

## Evidence and falsifiers

- Shared coordinator and one-shot release: `internal/api/lock_release_ledger.go`.
- Registry routes: `internal/api/workspace_registry.go`.
- Serena fence routes: `internal/api/serena_removal_fence.go`.
- Serena supervisor-intent repair route: `internal/api/serena_intent_repair.go`.
- Shared join: `internal/api/lock_release_ledger.go`.
- Fail-open snapshot: `internal/api/register_supervisor.go`.
- Falsifier: a routed release typed as `func()`, a discarded release result, a second ghost/reservation predicate outside `lockReleaseLedgerEntry`, replacement of a live coordinator entry, or serialized ledger state.

## Status

Proposed pending implementation verification, review, and operator acceptance.

## Terms and Abbreviations

- **ABA split** — replacing a coordinator so an old waiter and a new same-path acquirer observe different owner identities.
- **Registry** — the `workspaces.yaml` state owner.
- **Serena** — the workspace-scoped language-service integration using removal fences and supervisor intent.
- **Ledger ghost** — a routed lock leaf whose release this process could not confirm.
- **One-shot release** — a callback that invokes its underlying unlock at most once and returns the same result on every call.
- **IPC** — inter-process communication.

---
status: proposed
date: 2026-08-03
slug: registry-lock-release-ledger
related-pr: PR #590
---

# Use a process-local Registry lock-release ledger

## Context

Registry operations previously received an errorless release callback. That contract could not report `flock.Unlock` failure, preserve a primary operation error alongside it, or distinguish ordinary contention from a lock handle this process may still retain.

## Proposed decision

1. `Registry.Lock` and `Registry.TryLock` return reportable, concurrency-safe, one-shot `func() error` releases.
2. `recordUnconfirmedLockRelease` owns one process-local, first-failure-wins ledger keyed by the exact lock leaf.
3. Ledger entries are monotonic for the process lifetime. Recovery is process exit only; state is never serialized or cleared in-process.
4. No caller may discard a release error or adapt the callback back to `func()`.
5. `ReleaseAndJoin` is the shared spelling for preserving a primary error and joining release failure.
6. Reconciliation snapshots fail open when already loaded: the usable snapshot is returned together with the release error so consumers can continue safely and report uncertainty.
7. Operations whose mutation can commit before release structurally separate primary/mutation and release outcomes. Release-only failure does not roll back a committed registration or Serena deletion.
8. No persisted-state migration is introduced.

## Consequences

- A same-process Registry ghost fails loud instead of appearing as ordinary contention or entering a later blocking acquire.
- Original unlock causes remain available through `errors.Is` together with `ErrLockReleaseUnconfirmed`.
- Callers with early returns, caches, retries, rollback callbacks, or committed results require explicit error-bearing paths.
- Unrelated flock families remain separately owned and separately tracked.

## Evidence and falsifiers

- Owner contract: `internal/api/workspace_registry.go:218-247`.
- Ledger and one-shot release: `internal/api/lock_release_ledger.go:17-90`.
- Shared join: `internal/api/lock_release_ledger.go:92-98`.
- Fail-open snapshot: `internal/api/register_supervisor.go:102-125`.
- Falsifier: any Registry release typed as `func()`, any discarded release result, any second writer of stranded-release state, or any serialized ledger reference.

## Status

Proposed pending implementation verification, review, and operator acceptance.

## Terms and Abbreviations

- Registry: the `workspaces.yaml` state owner.
- Ledger ghost: a Registry lock leaf whose release this process could not confirm.
- One-shot release: a callback that invokes its underlying unlock at most once and returns the same result on every call.
- IPC: Inter-Process Communication.

---
status: open
date: 2026-07-26
slug: retained-removal-fence-wedges-the-next-teardown
severity: high
affected-surface: Serena removal fence lifecycle
related-pr: PR #590
---

# Retained removal fence wedges the next teardown

## Summary

A failed Serena removal-fence release deliberately leaves the process appearing live, but the fence owner has no process-local stranded-release ledger. A later same-process blocking acquisition can therefore wait on a handle the process may still retain.

## Current source evidence

- `internal/api/serena_removal_fence.go:161-176` documents that failed release blocks a later teardown and performs a blocking acquisition.
- `internal/api/serena_removal_fence.go:181-190` returns the raw release closure without one-shot memoization or a same-process ghost check.
- `internal/api/serena_removal_fence.go:82-90` owns the existing release sentinel and injectable unlock seam.

## Expected vs actual

- Expected: later acquisition fails loud with the first retained-fence diagnosis or follows another explicitly designed recovery policy.
- Actual: callers observe the first release failure, but the next blocking acquire has no process-local discriminator.

## Scope

Keep this record open. The Registry ledger introduced by PR #590 does not become a removal-fence ledger.

## Terms and Abbreviations

- Removal fence: the per-workspace lock spanning Serena teardown marking through row deletion or rollback.
- Ghost: a lock leaf whose release this process could not confirm.

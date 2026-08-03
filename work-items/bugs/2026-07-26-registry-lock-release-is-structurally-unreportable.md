---
status: open
date: 2026-07-26
slug: registry-lock-release-is-structurally-unreportable
severity: high
affected-surface: internal/api Registry lock contract and every Registry caller
related-pr: PR #590
---

# Registry lock release is structurally unreportable

## Summary

The accepted r20 contract returned an errorless Registry release callback, so an operating-system unlock failure could neither reach the caller nor prevent the same process from blocking on its retained handle. The r24 candidate changes the owner contract to a one-shot `func() error` and records unconfirmed leaves for the process lifetime, but this record remains open until verification and user approval.

## Current source evidence

- `internal/api/workspace_registry.go:218-247` owns `LockPath`, `Lock`, and `TryLock` and now exposes error-bearing releases.
- `internal/api/lock_release_ledger.go:31-63` owns the first-wins ledger and memoized one-shot release.
- `internal/api/lock_release_ledger.go:92-98` owns primary-plus-release error joining.

## Expected vs actual

- Expected: every Registry release failure is returned, recorded before return, and makes later same-process acquisition fail immediately with its original cause.
- Actual at accepted r20: the callback type erased the release result; the r24 candidate implements the expected behavior but is not yet approved.

## Closure condition

Close only after the 43-path migration, static legacy-shape oracle, normal and race tests, full differential, and user review all pass.

## Terms and Abbreviations

- Registry: the process-shared `workspaces.yaml` state owner.
- One-shot release: a callback that invokes its underlying unlock at most once and memoizes the result.
- r20/r24: the accepted frozen base and this implementation candidate.

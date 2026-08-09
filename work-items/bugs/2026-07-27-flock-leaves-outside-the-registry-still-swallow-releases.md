---
status: open
date: 2026-07-27
slug: flock-leaves-outside-the-registry-still-swallow-releases
severity: high
affected-surface: non-Registry flock owners
related-pr: PR #590
---

# Flock leaves outside the Registry still swallow releases

## Summary

PR #590 deliberately migrates the Registry family and the Serena repair's raw supervisor-intent leaf only. Other flock owners still discard unlock results and therefore remain a separate defect class.

## Current source evidence

- `internal/api/default_workspace_marker.go:101-107` discards the default-marker lock release.
- `internal/api/daemon_intent.go:406` and `internal/api/daemon_intent.go:839` discard daemon-intent releases.
- `internal/api/hub_mcp_control.go:158` and `internal/api/hub_mcp_control.go:329` discard hub-control releases.
- `internal/api/register_supervisor.go:507` and `internal/api/register_supervisor.go:562` discard independent supervisor-intent releases outside the admitted repair leaf.

## Expected vs actual

- Expected: each owning family defines an error-bearing release contract and explicit recovery policy.
- Actual: many unrelated owners still use `_ = lock.Unlock()`; the Registry ledger must not be generalized into those families without their own lifecycle analysis.

## Scope

Keep this record open. PR #590 must neither claim these leaves fixed nor silently route them through the Registry ledger.

## Terms and Abbreviations

- Flock: an operating-system advisory file lock exposed by `github.com/gofrs/flock`.
- Registry: the `workspaces.yaml` state owner; only this family is migrated by PR #590.

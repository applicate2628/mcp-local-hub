---
id: 2026-07-08-aggressive-cleanup-token-omits-started-at
status: fixed
severity: low
area: internal/api/cleanup.go (AggressiveConfirmToken) + internal/gui/cleanup_aggressive.go
found-by: architect (A2 PR5 r5 consent-binding review)
context: adjacent-finding
fixed-by: fix/aggressive-cleanup-identity-binding (PR pending)
---

## Status — FIXED (branch fix/aggressive-cleanup-identity-binding)

Both gaps closed: (1) `AggressiveConfirmToken` now includes StartedAt in the hash tuple, so
a same-basename PID reuse changes the token → recompute-and-compare refuses the kill; (2) the
kill binds by {PID, StartedAt} identity via `api.IdentitiesOf(fresh)` →
`CleanupOpts.Expect` → `filterToExpectedIdentities` on BOTH surfaces (the GUI apply was
PID-only; the CLI aggressive kill was entirely UNBOUND). The dead PID-only path
(`CleanupOpts.ExpectPIDs`, `filterToExpectedPIDs`, GUI `pidsOf`) is removed — both reaper
paths now share the single identity-keyed kill-binding owner. Tests + review pending merge.

## Summary

`AggressiveCleanup`'s confirm-token + kill binding shares the SAME PID-reuse class the
default reaper's identity binding just closed (bot PR #520 P2), because the aggressive path
binds by PID, not by `{pid, started_at}` identity:

- `AggressiveConfirmToken` hashes `{pid, cmdline_display(basename), match_source}` — NOT
  `started_at` ([internal/api/cleanup.go](../../internal/api/cleanup.go), the
  `AggressiveConfirmToken` region). A same-basename PID reuse produces an IDENTICAL token,
  so the token is a set-drift gate, not an identity binding.
- The aggressive kill is PID-bound: the GUI apply passes `pidsOf(fresh)`
  ([internal/gui/cleanup_aggressive.go](../../internal/gui/cleanup_aggressive.go) ~L248) →
  `CleanupOpts.ExpectPIDs` → `filterToExpectedPIDs` (PID only).

So a confirmed aggressive candidate whose PID is recycled onto a different same-basename
process between confirm and apply could be killed unacknowledged — the same mechanism the
default reaper now excludes via `filterToExpectedIdentities`.

## Why lower severity than the default-path fix

- The aggressive path is operator-CONFIRMED + scoped (`--client` / `--root-pid`), not the
  unattended 5-min ticker. An operator is watching.
- The token's set-drift detection narrows the deliberation window (a changed candidate set
  invalidates the token → re-preview), so the window is tighter than the default reaper's
  was before its fix.
- Every aggressive candidate is still identity-re-verified at kill time by
  `TerminatePIDWithIdentity` against the FRESH row — so a recycled PID whose fresh identity
  self-consistently verifies is the only leak (same tautology as the default path had).

## Fix (converge both paths onto the identity binding)

Route the aggressive kill through `filterToExpectedIdentities` too. The aggressive `fresh`
rows already carry `StartedAt`, so the aggressive path can pass `identitiesOf(fresh)`
**server-side** — NO aggressive wire/GUI change needed — which also retires the
two-helper (`filterToExpectedPIDs` vs `filterToExpectedIdentities`) asymmetry. Optionally
fold `started_at` into `AggressiveConfirmToken` for defence-in-depth on the set-drift gate.

## Why not in A2 PR5

PR #520 owns the DEFAULT reaper's config-absence gate + its consent binding. The aggressive
path's token/GUI/409 state machine is out of that PR's scope (touching it would drag the
aggressive card's re-preview flow into a config-gate PR). Filed as an adjacent finding per
the architect verdict; convergence is a small standalone follow-up.

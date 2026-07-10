# Closure — test-leftover reaper (v1 preview-only)

Closed: 2026-07-10

## Outcome

Version 1 of `mcphub cleanup test-leftovers` shipped as a **preview/diagnostics-only** command in PR #527, merged to master as `436e4f58` (Codex Cloud bot PASS across 2 rounds). It is non-destructive: it enumerates and classifies candidate test-leftover processes, reports per-candidate evidence (PID, `StartedAt`, executable basename under local-output policy, argv shape, pattern class, age diagnostics, parent-liveness, on-disk buildinfo tag, environment status) and the refusal label a hypothetical v2 apply would emit. It has no apply flag, confirm token, kill owner, tree-reap coordinator, or process-termination step.

The implementation is `internal/api/test_leftover_preview.go` with vocabulary tests in `internal/api/test_leftover_preview_test.go`. Existing `CleanupOrphans` / `AggressiveCleanup` paths, their own-binary protection, and all current termination owners are unchanged.

As part of this closure the `design.md` enum/vocabulary tables were reconciled to the merged code const set (the vocabulary had grown past the original design across implementation + two commissions + two bot rounds). The reconciled tokens: `ambiguous-family-classification` and `protected-scope-unverified` (refusal reasons), `snapshot-unsupported-platform` (snapshot verdict), `live-supervise` (pattern class); and the stale, never-shipped `unverified-ppid-chain` token was removed in favor of the actual `live-supervise` / `standalone-supervise` split. The deferred-v2-only labels (`basename-not-in-branch`, `e2e-markers-absent`, `env-read-error`, `env-override-absent`, `unsupported-arch`, `command-line-mismatch`) were moved to an explicit "reserved for v2, not emitted by baseline v1" row.

## Residual risk

- **The destructive apply is deferred to v2 and remains open** (see Open follow-ups). V1 does not kill anything; the operator still performs removal manually after out-of-band identity verification (image path, argv, `StartedAt`), then uses an OS process tool outside this command.
- **The standalone-`supervise` leftover class — the principal real field population (the 2026-07-09 incident) — is still operator-manual-reap.** V1 lists it as `standalone-supervise` with `would-refuse=supervise-not-tree-reachable` and the note `manual-reap-only: verify identity out-of-band before killing`, but never auto-authorizes it, because a live adopted supervisor is byte-equivalent to a genuine leftover at the available evidence surface (dead recorded PPID by design).
- V2 destructive apply is blocked on the round-3 P2 respawn-loop ordering defect and the P3 recyclable-PPID ancestry defect (both in `security-review.md`), plus a demonstrated value case for the safely automatable subset.

## Archive location

`work-items/archive/2026-07/2026-07-09-test-leftover-reaper/` (design.md, security-review.md, status.md, closure.md). The accepted decision record stays at `work-items/decisions/2026-07-10-test-leftover-reaper-preview-only-v1.md` (`status: accepted` — the decision stands). The deferred PEB decision `work-items/decisions/2026-07-09-test-leftover-reaper-peb-env-proof-preview-only.md` is not superseded; it remains a v2 constraint.

## Open follow-ups (NOT closed by this item)

- **v2 destructive apply** — deferred; blocked on P2/P3 + value case per the decision record. The full deferred contract (PEB reader, `envProofGate`, confirm token, `{PID, StartedAt}` binding, 600s apply floor, strict path guards, pre-kill audit, tree-reap, single kill owner) is preserved under the `Deferred: Destructive Apply (v2)` section of `design.md`.
- **adopt-side durable pre-adopt provenance** — `work-items/active/2026-07-09-adopt-side-durable-pre-adopt-provenance/` (research complete; awaiting PM admission + architect design). Unrelated to the reaper except that both are queued backlog; stays open.

## Retrospective

The load-bearing lesson: **a destructive auto-reaper could not safely handle its own main target class.** Three consecutive adversarial security re-gates (`9 (3×P1) → 1×P1 → 0×P1`) drove the design from a full destructive apply toward preview-only, because the only way to reach zero P1 live-kill paths was to refuse standalone `supervise` — which is exactly the population the work-item was created to reap. A live adopted supervisor is indistinguishable from a leftover at the available evidence surface, so any automated kill risked terminating a live supervisor and its `KILL_ON_JOB_CLOSE` Job-Object fleet. Shipping the read-only evidence command first delivers the real incident-workflow value (faster, reproducible manual investigation) with zero automated live-kill surface, and preserves the full v2 apply contract for when P2/P3 are resolved and the safe subset justifies the coordination cost.

Secondary lesson (hygiene): the diagnostic/refusal vocabulary drifted from the design across implementation and review rounds; the merged code const block plus the vocabulary tests — not any summary — were the authoritative source for this closure's design reconciliation.

# External quality-assurance review

## Provenance

- Execution role: external-reviewer
- Assigned / replaced internal role: qa-engineer
- Requested provider: codex
- Resolved provider: Codex CLI
- Actual execution path: direct external CLI
- Model / profile used: `gpt-5.6-sol` / `xhigh`
- Launch flags: `--model gpt-5.6-sol -c model_reasoning_effort=xhigh --sandbox read-only --ephemeral`
- Run window: 2026-07-26 04:09:58–04:18:45 +03:00
- Raw report: `.scratch/external-reviews/qa-r10.out`
- Deviation reason: none

## Reviewed revision

The reviewer independently reproduced the raw bytes from:

`git diff --binary origin/fix/cursor-not-default-install -- <15 product paths>`

Result: 48,364 bytes, plain SHA-1 `8568f0bbf84adabb3c21266819e78c49552ae9d6`.

## Findings

| Claim | Verdict | Evidence summary |
| --- | --- | --- |
| Complete product surface | VERIFIED | Exactly 15 product files relative to the branch baseline. |
| Runtime confinement | VERIFIED | Register fallback and cleanup both consume the effective binding policy; no unrelated runtime or persistence change. |
| Regression coverage | VERIFIED | Both API guards, the clients/GUI/CLI scoped tests, build, vet, generator, asset no-diff, and diff check were reconciled against the current revision. |
| Stale-text closure | VERIFIED | Revision-9 sweep coverage and the reviewer’s independent probes found no unclassified live stale derivative. |
| Frontend derivative | VERIFIED | All three frontend files are comment-only; generated assets remain unchanged after a successful generator run. |

## Residual risk

- No live-fleet runtime smoke was performed, by explicit safety constraint.
- The no-`client_bindings` cleanup path has a named regression test. Explicit manifest bindings use the same selector but do not have a separately named cleanup test in this handoff.
- `implementation.md` is phase-scoped; final revision authority is `verification.md`, `sweep.md`, and this review.

Gate: PASS

## Terms and Abbreviations

- `API`: Application Programming Interface.
- `CLI`: Command-Line Interface.
- `QA`: Quality Assurance.
- `SHA-1`: Secure Hash Algorithm 1, used here only as a reproducible diff identity.

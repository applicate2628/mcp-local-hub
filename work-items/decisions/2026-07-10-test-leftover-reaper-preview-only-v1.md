---
status: accepted
date: 2026-07-10
slug: test-leftover-reaper-preview-only-v1
deciders: $lead (directive), $architect (design revision)
context: work-items/active/2026-07-09-test-leftover-reaper/design.md
supersedes: none
superseded-by: none
---

# Decision: test-leftover reaper v1 is preview/diagnostics only

## Decision

Version 1 of `mcphub cleanup test-leftovers` is a preview/diagnostics command. It enumerates candidate leftovers and reports, per candidate, PID, `StartedAt`, executable path under the local redaction/full-path policy, argv shape, branch/pattern class, age, parent-liveness verdict, cheaply available buildinfo-tag and environment-override findings, and the refusal label a hypothetical apply would emit.

Version 1 never terminates a process. It exposes no apply flag, confirm token, kill owner, tree-reap action, or hidden destructive mode. Standalone `supervise` candidates are listed because they are the main observed leftover class, with the exact note `manual-reap-only: verify identity out-of-band before killing`.

Destructive apply is deferred to a future v2. The deferred package retains the Process Environment Block (PEB) reader, `envProofGate` kill authorization, confirm-token binding, `{PID, StartedAt}` kill binding, 600-second apply floor, strict path guards, pre-kill audit, tree-reap, and single kill owner contracts. V2 remains blocked on the round-3 P2 respawn-ordering defect, the P3 recyclable-PPID ancestry defect, and a demonstrated value case for the safely automatable subset.

## Rationale

The three adversarial security rounds are recorded in `work-items/active/2026-07-09-test-leftover-reaper/security-review.md:14-106`, `work-items/active/2026-07-09-test-leftover-reaper/security-review.md:108-132`, and `work-items/active/2026-07-09-test-leftover-reaper/security-review.md:134-174`. The trajectory was `9 (3×P1) → 1×P1 → 0×P1`: destructive safety converged, but only after every standalone `supervise` row was refused.

That refusal excludes the actual population that motivated the work item. The 2026-07-09 incident was a standalone `mcphub-reliability-*.exe supervise` process, and a live adopted supervisor can be byte-equivalent at the available evidence surface. Automatically killing that class would risk a live supervisor and its managed fleet.

The remaining GUI-tree subset does not justify shipping apply in v1. The round-3 P2 finding shows that descendant-before-GUI reaping races the GUI's armed one-second respawn loop and can manufacture a replacement orphan. The round-3 P3 finding shows that a snapshot PPID chain is not ancestry proof unless every edge is identity- and time-bound. Both add recurring coordination and validation complexity to a lane that still cannot handle the main field class.

Manual reaping already works: the operator verifies image path, argv, and `StartedAt` out of band, then uses an operating-system process tool outside the preview command. A read-only command makes that procedure faster and more reproducible without introducing an automated live-kill surface.

## Consequences

- The accepted v1 implementation reuses `runProcessSnapshot` plus `parseProcessRows`, performs read-only classification, and stops after rendering evidence.
- Strict path canonicalization is used for classification display and protected-path evidence, not as permission to act.
- On-disk `debug/buildinfo.ReadFile` evidence may be collected cheaply. Baseline v1 does not require or implement cross-process memory reads; the environment field is `not-collected-v1` unless a separately accepted cheap, bounded, read-only diagnostic provider already exists.
- Missing, unreadable, ambiguous, or adverse evidence keeps the candidate visible and supplies a diagnostic label. It never becomes implicit permission.
- Existing default and aggressive cleanup paths, their own-binary protection, and all current termination owners remain unchanged.
- The accepted PEB decision in `work-items/decisions/2026-07-09-test-leftover-reaper-peb-env-proof-preview-only.md` is not superseded; it remains a deferred v2 constraint.
- V2 is not admitted by this decision. Resolving P2/P3 without demonstrating value for the safe subset is insufficient to restart apply work.

## Alternatives

| Alternative | Decision | Reason |
|---|---|---|
| Ship destructive apply for the safe subset in v1. | Rejected. | It omits the main real class and retains disproportionate coordination risk. |
| Auto-kill standalone `supervise` from path/argv/tag/environment resemblance. | Rejected. | The available evidence cannot distinguish a true leftover from a live adopted supervisor. |
| Do nothing and keep manual investigation ad hoc. | Rejected. | Candidate enumeration and consistent evidence output materially improve the proven manual procedure. |
| Ship preview/diagnostics v1 and defer apply. | Accepted. | It serves the real incident workflow with no automated process action. |

Gate decision: **PASS**.

## Terms and Abbreviations

- **Apply:** the deferred process-termination mode; no apply mode exists in v1.
- **PEB:** Process Environment Block.
- **PID / PPID:** process identifier / parent process identifier.
- **P1 / P2 / P3:** security-review severity levels.
- **V1 / V2:** preview/diagnostics-only version 1 / deferred destructive version 2.

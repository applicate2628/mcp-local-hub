---
status: accepted
date: 2026-07-27
decided-by: $lead (main conversation), on the PR #591 re-gate REVISE closure
context: feat/vcpkg-mcp — two wire-contract changes to `vcpkg_last_failure` output
supersedes: none
superseded-by: none
---

# `vcpkg_last_failure` may bound its response and normalizes terminal display bytes

> **CORRECTION (2026-07-27, same day, by the narrow final gate). The bound below does NOT yet
> bound.** I signed this off after verifying that the budget cannot corrupt a verdict — which is
> true, and was re-proved rigorously. I did not verify that the budget actually caps the response,
> and it does not: `Diagnostic.File`, `first_error.file` and `Result.BuildCommand` sit outside every
> constant here. Three of the five matched diagnostic shapes capture rest-of-line or head-of-line
> into `File` (`diagnostics.go:94` `ninjaFailedRE`, `:60` `msvcCompileDiagRE`, and
> `lastfailure.go:668` `findRunBuildCommandLine`), so a single pathological line still returns
> **6.30 MB / ~1.575M tokens** — the exact magnitude the "before" column below records.
>
> Worse than a plain miss: the response carries `diagnostic_text_truncated_to_line_budget` and
> `diagnostics_dropped: 0`, so it LOOKS bounded.
>
> The enforcement test passed because its fixture is the one shape whose payload lives entirely in
> `Text` (`response_budget_test.go:167-206`). The guard exists; it covers one participant of the
> class. This is the same "fix landed on one instance of a class" shape the PR exists to close,
> recurring in the fix itself.
>
> **The `one 3 MiB line → 14 KB` row below is therefore true only for the MSVC-text shape.** The
> row stays as written rather than being quietly edited, so the error is visible; the table is
> superseded by whatever bound the follow-up lands. Tracked as the blocking finding of the narrow
> gate on `537ea802`.
>
> A second, smaller correction: the claim that *"a single-log phase is never truncated"* is false.
> The COUNT budget cannot bind on a single log (200 = the per-log ceiling, verified for every phase
> classification), but the 64 KiB BYTE budget still can and does — measured 83 of 200 dropped on a
> single-log phase at ~330 bytes mean diagnostic text, which MSVC template-instantiation errors
> routinely exceed. No operator harm (it is reported), but the justification was wrong.

Two changes to the tool's wire output, signed off together because both were surfaced by the
same re-gate and both trade a stated contract line for a failure mode that is worse.

## Decision

**1. The response is bounded.** `MaxResponseDiagnostics = 200`, `MaxDiagnosticTextBytes = 4 KiB`,
`MaxResponseDiagnosticBytes = 64 KiB` (`internal/vcpkgmcp/lastfailure/diagnostics.go:743-753`).

This CHANGES the contract at `types.go` that read *"Warnings are never dropped… Aggregates are
never dropped either."* Affected surfaces: `diagnostics[]`, `notes[]`, and a new
`diagnostics_dropped` count.

**2. Terminal display bytes are normalized** before matching, in one owner `normalizeLogLine`
(`diagnostics.go:332`) used by all three phase-log scanners (`:450`, `:682`,
`lastfailure.go:704`). This CHANGES the contract that `Diagnostic.Text` / `Diagnostic.File` are
verbatim log bytes.

## Rationale

**On the budget.** The line "warnings are never dropped" was written when the volume was assumed
small. It was never a promise the tool could keep at scale, and the measured worst case is not a
degradation — it is a tool that cannot be called:

| scenario | before | after |
|---|---|---|
| release+debug install, 4 logs | 800 diagnostics, 204 KB, ~52k tokens | 200, 57 KB, ~14k |
| 8 configs, 16 logs | 3200, 813 KB, **~208k tokens** | 200, 60 KB, ~15k |
| one 3 MiB line | **6.00 MB, ~1.57M tokens** | 14 KB |
| every real in-tree fixture | 0–3 diagnostics, 0.7–4.8 KB | byte-identical |

A single tool call returning 1.57M tokens does not degrade the caller's context, it destroys it.
Against that, "a warning may be dropped from the tail of a 200-deep ranked list" is a small,
reported cost — and on every real fixture measured the output is byte-identical, so the budget is
invisible in ordinary use.

Three properties make it safe, and I verified each rather than accepting them:

- **The budget cannot change a verdict.** `FirstError = headlineErrorDiagnostic(chosenDiags)` and
  the verdict switch both read the COMPLETE `chosenDiags` (`lastfailure.go:493-499`); the budget is
  applied afterwards at `:510`. A truncated response never reports a different conclusion than an
  untruncated one would have.
- **Truncation is by rank, from the tail only.** Nothing higher-ranked is ever dropped to keep
  something lower-ranked, and the headline is never dropped.
- **It is never silent.** `Result.DiagnosticsDropped` carries the count and
  `NoteDiagnosticsTruncatedToBudget` carries the note (`diagnostics.go:779`, `types.go:300,522`).
  A caller can always tell a bounded response from a complete one.

`MaxResponseDiagnostics = 200` equals one log's own per-log ceiling, so a single-log phase is never
truncated — the budget only engages where the response was multiplied across logs, which is exactly
the case it exists for.

**On normalization.** Leaving the bytes verbatim produces a CONFIDENT WRONG VERDICT, which is the
defect class this whole PR exists to close. A colourized clang diagnostic matched nothing, so the
tool returned `unknown(no_diagnostic_found)` for a build that plainly failed. The reachability is
ordinary, not exotic: `g++ -fdiagnostics-color=always` emits ANSI **to a redirected pipe with no
TTY and no `CLICOLOR_FORCE`**, and that is a normal `CXXFLAGS` setting. GCC also emits `ESC[K`, so
stripping SGR sequences alone would have left residue.

Normalization is additionally a terminal-injection fix, consistent with the posture already taken
for the marketplace catalog (C0/C1/ESC stripped before anything reaches stdout).

## Consequences

- A caller that relied on `diagnostics[]` being exhaustive must now read `diagnostics_dropped`.
- A caller that relied on `Diagnostic.Text` being byte-identical to the log gets normalized text.
  `ParseWrapperContent` is deliberately EXCLUDED from normalization — different producer, and its
  `command:` line becomes `ExactCommand`, whose whole purpose is verbatim reproducibility.
- C1 bytes are deliberately left untouched: they are UTF-8 continuation bytes.
- The pre-existing `ASSUMPTION (UNVERIFIED)` on `maxDiagnosticsPerLog` is resolved — both of its
  named resolving steps (a measured worst case, and a stated total budget) are now done.

## Alternatives rejected

- **Contract-preserving no-cap.** Keeps the promise and keeps the 1.57M-token failure. The promise
  is not worth a tool that cannot be called.
- **Errors-only cap.** Would drop the cause while keeping aggregate noise — the exact starvation the
  per-cell budget was added to prevent.
- **A single byte cap, or a single count cap.** Either alone leaves the other axis unbounded; one
  3 MiB line defeats a count cap, and 3200 small diagnostics defeat a text cap.
- **Subdividing the per-log budget.** Would weaken the 2026-07-26 per-cell guarantee, i.e. trade a
  regression for a ceiling. A larger bounded total is not a regression.
- **For normalization: local strip at each regex, or document-only.** Three scanners disagreeing on
  what a line is, is how this class recurs; per-regex escapes multiply the disagreement.

## Residual

The three scanners still disagree on whether CR is a line SEPARATOR — `DetectInterrupted` says yes
and documents why, the other two say no. Changing that alters line boundaries and needs its own
measured pass. Filed as `work-items/bugs/2026-07-27-lastfailure-two-notions-of-a-log-line.md`, not
folded in here.

The frequency of `-fdiagnostics-color=always` in real vcpkg triplets is not measured, and the
decision does not rest on it: the failure mode is a confident wrong verdict and the mechanism is
proven reachable, which is sufficient.

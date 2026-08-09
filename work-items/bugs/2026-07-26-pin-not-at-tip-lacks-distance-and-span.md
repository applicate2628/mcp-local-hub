# `pin_not_at_tip` is a boolean; the actionable question is distance and span

- **Status:** open
- **Kind:** enhancement (NOT a defect — the current output is honest, just thin)
- **Context:** adjacent-finding — raised by the operator driving `vcpkg-mcp 0.1.0`
  against a real vcpkg tree, 2026-07-26. Filed at the Lead's instruction during
  the pre-submission review round on `feat/vcpkg-mcp`; **explicitly NOT
  implemented in that round, which was defect-closing only.**
- **Component:** `internal/vcpkgmcp/pinstatus`
- **Tool:** `vcpkg_pin_status`

## Observed

Across one real tree the tool flagged six ports as
`unknown(pin_not_at_tip)`: `sleipnir`, `hpx`, `ngspice`, `python3`, `skia`,
`libb2`.

The result carries `pinned_sha`, `tip_sha` and a `compare_url`, but the
verdict itself is a boolean: the pin either equals the tracked tip or it does
not. Every one of those six rows looks identical in severity.

## Why it matters

A pin three days behind and a pin two years behind are completely different
maintenance decisions, and the current output cannot distinguish them. The
operator has to open each `compare_url` by hand to triage, which is exactly
the manual work the tool exists to remove.

## Why it looks cheap

The remote round-trip that would answer it has **already been performed** —
`git ls-remote` is issued for every commit-shaped pin today. The missing data
is distance (commits between pin and tip) and span (wall-clock age of the
gap).

## The hard constraint any design must respect

`git ls-remote` **cannot** supply this. It proves only what a named ref points
at NOW. The package doc's "hard limit" section is explicit and measured (live
61-remote probe): ls-remote cannot establish that the pinned commit still
exists, that it is an ancestor of the tip rather than diverged or rebased
away, or how far behind it is. That is precisely why this package has no
`behind` value anywhere in its vocabulary
(`TestNoCodePathProducesBehind` pins that invariant statically and at runtime).

So distance/span requires a genuinely new capability, not a richer read of the
existing response. Sketch of the options, cheapest first:

1. **Forge REST APIs** (GitHub `/compare/{base}...{head}`, GitLab equivalent)
   — returns `ahead_by`/`behind_by` and commit dates directly. Costs: a second
   network dependency, authentication for rate limits, and it only covers
   recognized forges (the generic `vcpkg_from_git` shape gets nothing).
2. **A bounded shallow fetch** into a temp object store. Costs: real disk and
   time, and it is a MUTATION-shaped operation in a tool that is currently
   strictly read-only.
3. **Do nothing; improve presentation.** Sort the `pin_not_at_tip` rows and
   make `compare_url` more prominent.

## Acceptance criteria if it is ever admitted

- The `ok | failed | unknown(<closed reason enum>)` contract is preserved, and
  any new reason value is added to the closed enum plus the tool's declared
  schema in the same change.
- Distance/span are **omitted** rather than guessed when the capability is
  unavailable for that remote (unknown forge, auth failure, rate limit). A
  missing number must never render as `0`.
- The `behind` prohibition is not weakened: a *measured* ancestor distance
  from a real comparison API is a different claim from an *inferred* direction,
  and the distinction must be visible in the field names and documented.
- No new unbounded network work: the existing per-child deadline, bounded
  retry decision, and resource ceilings apply to any added call site.

## Not doing now

Defect-closing round only. No code was written for this item.

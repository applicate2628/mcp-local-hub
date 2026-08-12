# Bug: routing refresh tests use a stale source-text anchor

- id: 2026-08-12-routing-refresh-tests-use-stale-source-anchor
- context: 2026-08-11-cst-saved-field-sampler
- status: open
- severity: low
- area: routing invariant tests
- found-by: backend-engineer
- fix-class: test-owner

## Reproduction

Run `TestRefreshCapturesEntriesBeforeRegistryRelease` in both
`internal/api/lsp_routing` and `internal/api/serena_routing`.

Both tests search sibling source for the literal assignment
`entries = r.reg.LSPEntries()`, while the current immutable source uses the
short declaration `entries := r.reg.LSPEntries()`. The capture anchor is
therefore `-1` even though the ordering under test remains visible in source.

## Expected versus actual

- Expected: the test proves the semantic capture-before-release ordering.
- Actual: the test depends on one formatting-level assignment spelling and
  fails after the equivalent short declaration.

## Required correction

Move the invariant to a behavioral seam or a syntax-aware assertion shared by
both routing owners. Do not weaken the ordering assertion to a broad substring.

## Falsifying probe

Mutate the capture to occur after Registry release and prove both tests fail;
restore capture-before-release and prove both pass regardless of `=` versus
`:=` spelling.

## Provenance

Found during the T00 full-Go baseline. Exact focused reproduction failed in
both routing packages on immutable HEAD; the adjacent supervisor cleanup test
passed on focused rerun.

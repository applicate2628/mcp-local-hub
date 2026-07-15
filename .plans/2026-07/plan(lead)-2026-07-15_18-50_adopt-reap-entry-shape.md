# Adopt Reap Entry-Shape Predicate Plan

1. Verify the existing whole-file predicate, both callers, and the merged de-adopt classification seam.
2. Add regression tests for unrelated churn, no-snapshot states, committed relays, and fail-safe uncertainty; observe the intended failures.
3. Replace only `adoptRowProvablyUnmutated` with the existing anchored-snapshot and locked physical-entry classification path.
4. Run focused tests, the requested build/vet/test/race commands, and constrained-diff checks.
5. Report the unified diff, evidence, scope confirmations, and review ambiguity.

Status: completed.

## Terms and Abbreviations

- CAS: compare-and-swap capability used here for locked entry classification.
- GC: garbage collection of orphaned adopt provenance.
- TOCTOU: time-of-check to time-of-use race.

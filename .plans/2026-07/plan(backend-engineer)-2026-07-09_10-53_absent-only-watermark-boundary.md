Plan snapshot for absent-only legacy stop watermark boundary work.

1. Verify audited holes and persistence funnel from real code. Status: completed. Evidence: `rg` showed non-test production supervisor-intent commits route through `writeSupervisorIntentLockHeld`; `WriteSupervisorIntent` is exported but had no non-test callers.
2. Add regression tests and capture fail-before evidence. Status: completed. Focused RED run failed on redundant active-task watermarks, retained install watermarks, bare-key collapse `Changed=false`, and public writer normalization.
3. Implement single boundary normalizer and remove redundant per-site checks. Status: completed. Added `normalizeAbsentOnlyStopWatermarks`; invoked via supervisor-intent marshal/write paths; removed same-task delete-on-present in `mutateStopSubBlock` and redundant restore guard/prune in `restoreSupervisorStopArtifacts`.
4. Run scoped build/vet/test gates and inspect diff. Status: completed. Gates passed.
5. Write required session report and final implementation summary. Status: completed.

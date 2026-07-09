# brief - intent collapse stop resurrection

## Problem
`daemon-intent.json` can contain a legacy active stop that has already been
collapsed into `supervisor-intent.json` and later cleared by a new binary. A later
collapse must not replay that stale legacy stop and suppress a deliberately
re-enabled daemon.

## Accepted scope
- Preserve fail-toward-stop behavior for missing or mismatching watermark evidence.
- Prevent stale legacy stops from resurrecting after a deliberate clear.
- Replace the rejected eager watermark representation with the accepted absent-only
  representation: `legacy_stop_watermarks` contains only tasks absent from `Stops`.
- Keep watermark birth/prune in the single Stops-mutation owner and fix rollback
  through a structural rollback-artifact abstraction.

## Out of scope
- Deleting the legacy `daemon-intent.json` surface entirely.
- New sidecar files or new read caps.
- Product-code changes in this recovery-state patch.

## Resume point
PR #525 is open at `016896418f321c0923f57465b4487ff79e51ed5d`. A linked worktree
contains uncommitted absent-only rewrite changes. The next session should complete
that rewrite, close the P3 invariant race, rerun the independent audit, and push
the fixed branch for re-review.

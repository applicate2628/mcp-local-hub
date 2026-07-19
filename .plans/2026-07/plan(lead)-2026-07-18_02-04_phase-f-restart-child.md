# Phase F restart child implementation plan snapshot

Outcome: PASS — implementation and required local verification completed without a commit.

1. Completed — restore the active work-item resume state and re-verify the accepted Phase F design,
   plan, gate, listener, marker, and reservation seams.
2. Completed — add acceptance tests first and preserve the compile/test failures that identified each
   missing Phase F surface.
3. Completed — implement challenged standby readiness, nonce-file consumption, reservation-aware child
   acquisition, one activation barrier, shared server continuation, Commit retry, and child events.
4. Completed — self-falsify lease/listener cleanup, late-marker rejection, parent-death/no-reservation,
   standby expiry, ordinary ping bytes, and gate-off/legacy path selection.
5. Completed — run formatting, build, vet, and the mandatory tagged API/CLI/GUI test suite; hand the
   uncommitted package to the orchestrator.

Canonical work item: `work-items/active/2026-07-16-productization-gui-solidify/`.

## Terms and Abbreviations

- API: Application Programming Interface.
- CLI: Command-Line Interface.
- MAC: Message Authentication Code.

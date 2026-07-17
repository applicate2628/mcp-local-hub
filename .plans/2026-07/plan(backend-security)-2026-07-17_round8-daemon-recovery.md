# Round 8 daemon recovery plan

Scope: the approved `feat/gui-daemon-recovery` recovery ordering, timing,
operator-contract, focused-test, frontend wire, design, and follow-up surfaces.

Roles: `$backend-engineer` and `$security-engineer`.

1. Inspect the complete committed-termination to respawn path and verify every
   potentially blocking operation.
2. Queue committed-termination audit events until after the force-respawn call,
   separate the pre-kill probe clock from the post-kill budget anchor, and give
   insufficient respawn budget a dedicated failure kind.
3. Add non-vacuous recovery, sweep, and automatic-path regression tests.
4. Reconcile CLI/GUI/frontend contracts, both exit tables, the design document,
   and the deferred automatic-path follow-up.
5. Regenerate embedded frontend assets and run every user-required check.

No commit, push, external review, model helper, untagged CLI/API test, or GUI
spawn-test opt-in is in scope.

# status - test-leftover reaper

Template: security-sensitive design revision. Orchestrator: lead.
State: DESIGN REVISED (round 3) after re-gate.

## Active agents / lanes

- None. Implementation remains blocked by the adversarial security gate.

## Completed agents / lanes

- The first fable-headed pre-implementation security gate confirmed nine findings: three P1, three P2, and three P3. The round-2 re-gate closed all nine findings plus the A/B/C code contracts, then confirmed one new P1 standalone-supervise admission finding and one new P3 snapshot-owner finding. The complete record is security-review.md.
- design.md revision 3 removes `supervise` argv from every positive branch, permits supervisor reaping only as a live tree descendant before its confirmed test GUI, defines supervise-not-tree-reachable, leaves already-orphaned standalone supervisors to manual out-of-band verification, and names parseProcessRows / `snapErr` as Positive Common Gate 1's single owner.
- The lane remains explicit and operator-invoked only; it is not an unattended ticker, default cleanup, aggressive cleanup change, or GUI cleanup endpoint.

## Next action

RE-RUN the adversarial security gate against design revision 3. Implementation remains blocked until that gate is clean.

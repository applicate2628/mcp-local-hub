# Productization GUI solidify — current delivery brief

Admission source: direct human decision recorded in `status.md` and Phase-0 item 3 in `roadmap.md`.

Primary task: Item 3, Unit B, Phase F — implement only the restart child half from the accepted
`item3-restart-design.md` and `item3-unitB-plan.md`.

Scope: additive challenged standby readiness; one-shot owner-only nonce-file consumption; the gated
restart-child standby/acquire/activate/Commit flow; and the authorized minimal `Server` continuation
that reuses the existing post-listener-bind lifecycle. Directly enforcing tests are in scope.

Out of scope: the Phase-G parent coordinator, frontend work, ensure-alive changes, gate flip, deployment,
commit, push, and any change to the gate-off normal launch or legacy bounded-acquire handoff path.

Acceptance: AC-F1 through AC-F5 in `item3-unitB-plan.md`, normal `/api/ping` byte compatibility,
normal `Server.Start` behavior preservation, and the repository build/vet/tagged-test commands named by
the human handoff.

Current stage: Phase F correction implementation is complete and locally verified after the human selected
Option B pathname transport and accepted the same-user rename residual. The strict consume now authorizes
only the canonical state-directory nonce leaf, hard-fails broadened access, unlinks before use, verifies
post-unlink identity state where the platform exposes it, and zeroizes failed buffers. Commit publication is
serialized with runtime stop/cancellation and exhausts to terminal `interrupted`; the child bind deadline and
normal 10-second header timeout are separately owned. No commit is authorized. Integration owner: main
conversation holding `$lead`; implementation owner: explicitly assigned `$backend-engineer`; next verification
owners: `$qa-engineer` and `$security-reviewer`.

Critical risks and owners: strict one-shot nonce consumption and challenged-readiness authorization
(`$backend-engineer`, independently checked by `$security-reviewer`); child/marker liveness settlement and
parent-death resource lifetime (`$backend-engineer`, independently checked by `$qa-engineer`); continuation
seam, gate-off compatibility, and owner-layer blast radius (`$backend-engineer`, mechanically reconciled by
`$lead`).

Next action: repeat the independent QA and security gates over the corrected uncommitted Phase-F diff. Keep
Phase I and the gate flip out of scope; do not commit until the orchestrator admits the next gate.

## Terms and Abbreviations

- DACL: Discretionary Access Control List.
- FLOCK: The operating-system-backed GUI single-instance file lock.
- MAC: Message Authentication Code.

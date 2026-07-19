# Productization GUI solidify — current delivery brief

Admission source: direct human Phase J decision on 2026-07-18, grounded in Phase-0 item 3 in `roadmap.md`
and the accepted `item3-unitB-plan.md`.

Primary task: Item 3, Unit B, Phase J — complete the final atomic release group by enabling RestartV3 by
default, making the disabled gate fully inert, deleting only the retained v1 unsafe spawn-and-exit body,
adding AC-J1, and closing the approved documentation drift.

Scope: `internal/gui/gui_restart_gate.go`; the gate-off endpoint branch and retained v1 body in
`internal/gui/gui_self_restart.go`; one adjacent inert-matrix test; `docs/phase-3b-ii-verification.md`;
`CLAUDE.md`; and the required C6 supersession note in
`work-items/decisions/2026-07-17-item3-unitB-recovery-simplify.md`.

Approved seams: preserve the v3 coordinator's spawn and exit seams and the composition-root discipline that
successful restart exits without calling `manager.Stop`. Preserve all Phase D-I/G/H behavior and contracts.

Out of scope: deployment, commit, push, real GUI spawn, Graphify, the `claude` CLI,
`MCPHUB_GUI_SPAWN_TESTS`, any `mcphub.exe` sweep/kill, frontend implementation changes, and changes outside
the Phase J surface.

Acceptance: AC-J1 through AC-J4 in `item3-unitB-plan.md`, as narrowed by the direct human constraints. Gate
OFF must return 503, write no marker, spawn no child, skip the ensure-alive predicate, and retain frontend
manual guidance. Gate ON must activate the RestartV3 contract suite. The two degrade-to-recovery end-to-end
tests must pass if seam-drivable. Documentation must add the two CLI-primary discriminator rows, the real
self-restart-and-reconnect smoke, the unreapable wedged-holder runbook row, the current bounded listener
restart/exhaustion behavior, and the v3.1 C6 supersession note.

Required verification: `go build ./...`; `go vet ./...`; `go test -count=1 -timeout 5m ./...`; and
`go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/`. Run frontend
build/test/typecheck and `go generate ./internal/gui/...` only if a frontend or bundle input is touched.

Current stage: implementation. Branch `feat/gui-restart-unitb-gated` at starting HEAD `3b42b1e7` contains
Phases D, E, F, I, G, and H plus the browser-test follow-up; all remain gated and undeployed before Phase J.
Integration owner: the main conversation holding `$lead`; implementation owner: explicitly assigned
`$backend-engineer`; verification owner: the same session under the exact user-required local gate because
subagent dispatch was not authorized.

Critical risks and owners: gate-off inertness and endpoint wire behavior (`$backend-engineer`); spawn/exit
seam preservation and post-release `manager.Stop` discipline (`$backend-engineer`); documentation C6
coherence (`$knowledge-archivist`); fresh full-gate evidence (`$lead`).

Next action: write AC-J1 first, prove the expected red state, make the smallest production edits, prove green,
finish the documentation pass, then run the exact full gate. Do not commit.

## Terms and Abbreviations

- CLI: Command-Line Interface.
- FLOCK: The operating-system-backed GUI single-instance file lock.
- GUI: Graphical User Interface.
- SSE: Server-Sent Events.

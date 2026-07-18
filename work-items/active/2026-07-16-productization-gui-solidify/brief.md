# Productization GUI solidify — current delivery brief

Admission source: direct human decision recorded in `status.md` and Phase-0 item 3 in `roadmap.md`.

Primary task: Item 3, Unit B, Phase G — implement only the parent restart coordinator, pre-release rollback,
parent hub-close ordering, and gated 202/2xx endpoint from the accepted `item3-restart-design.md` and
`item3-unitB-plan.md` (including the 2026-07-18 cli-layer change-surface amendment).

Scope: `RestartCoordinator` in `internal/gui/gui_restart_protocol.go`; the gated endpoint in
`internal/gui/gui_self_restart.go`; the owner-side hub-close immediately before the parent's flock release in
`internal/gui/server.go`; cli-layer parent composition in `internal/cli/gui.go`; and adjacent `_test.go` files.
The coordinator must use the existing marker, listener-owner, lease, child-retained-handle, authenticated
readiness, hub-owner, parser-aware argv, and self-restart exit seams. Directly enforcing AC-G1 through AC-G8
tests are in scope.

Out of scope: Phase H frontend changes, Phase J gate flip or v1 deletion, deployment, commit, push, real GUI
spawn, Graphify, the `claude` CLI, `MCPHUB_GUI_SPAWN_TESTS`, and changes outside the approved Phase G surface.
The gate-OFF endpoint remains the retained v1 implementation.

Acceptance: AC-G1 through AC-G8 in `item3-unitB-plan.md`; the concrete pre-release rollback gate is
`parentLeaseReleased == false`; proved rollback retains the lease and rebinds without reacquire; failed rollback
releases exactly once and requests the composition-root exit; post-release behavior performs no protocol writes,
waits, termination, claim/reclaim, recovery bind, or activation signaling; successful handoff skips
`manager.Stop`; spawn failure remains 2xx. Required verification is `go build ./...`, `go vet ./...`, clean
`gofmt -l` on touched files, and `go test -tags=test_state_path_env -count=1 -timeout 15m ./internal/gui/
./internal/cli/`, followed by an `mcphub.exe` process sweep.

Current stage: implementation. The branch is `feat/gui-restart-unitb-gated` at `beadf474`; this is the requested
`58340fd5` baseline plus one documentation-only commit that authorizes `internal/cli/gui.go` and its adjacent
tests for Phase G. Phases D, E, F, and I are landed default-OFF and not deployed. Integration owner: the main
conversation holding `$lead`; implementation owner: explicitly assigned `$backend-engineer`; next verification
owner: `$qa-engineer`.

Critical risks and owners: lease/hub/child handle ordering and post-release no-op fencing (`$backend-engineer`,
independently checked by `$qa-engineer`); pre-release rollback proof and marker cleanup (`$backend-engineer`,
independently checked by `$qa-engineer`); endpoint wire compatibility and gate-OFF v1 preservation
(`$backend-engineer`, mechanically reconciled by `$lead`).

Next action: execute the Phase G backend implementation with test-first red/green cycles, build after each
production file, then run the independent QA gate and the exact required verification commands. Do not commit.

## Terms and Abbreviations

- FLOCK: The operating-system-backed GUI single-instance file lock.
- GRACE(P): Old-port handler mode that serves only the approved restart allowlist while rejecting new work.
- MAC: Message Authentication Code.
- P: The parent GUI's currently owned port.

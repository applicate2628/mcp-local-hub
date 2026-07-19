# Phase G Parent Restart Coordinator — Analyst Evidence

REPOSITORY ORIENTATION: scope=`internal/gui` parent restart and direct CLI/frontend consumers; status=mutable preview feature; workflow=`POST /api/gui/restart` -> `RestartCoordinator.Start` -> `continueHandoff`; protected=default-OFF v1 behavior and adopted supervisor/daemon fleet; evidence=`README.md:11`, `README.md:82`, `work-items/active/2026-07-16-productization-gui-solidify/item3-restart-plan.md:187`.

## Files & symbols

| Surface | Evidence |
| --- | --- |
| Parent state machine | `RestartCoordinator.Start`, `continueHandoff`, `rollbackBeforeRelease`, `ConfirmAuthenticatedStandby` in `internal/gui/gui_restart_protocol.go:131`, `internal/gui/gui_restart_protocol.go:235`, `internal/gui/gui_restart_protocol.go:327`, `internal/gui/gui_restart_protocol.go:495` |
| Listener state | `GUIListenerOwner.bind`, `EnterGrace`, `CloseListener` in `internal/gui/gui_listener_lifecycle.go:82`, `internal/gui/gui_listener_lifecycle.go:152`, `internal/gui/gui_listener_lifecycle.go:214` |
| HTTP/process boundary | `guiRestartV3Handler`, `SpawnRestartV3GUI` in `internal/gui/gui_self_restart.go:163`, `internal/gui/gui_self_restart.go:292` |
| Consumer | `restartGui` and `doRestart` in `internal/gui/frontend/src/api.ts:932`, `internal/gui/frontend/src/components/settings/SectionGuiServer.tsx:65` |

## Flows

1. **[P1] One-shot readiness race — confirmed (`static-read`).** Spawn returns after process start (`internal/gui/gui_self_restart.go:315-319`); the continuation immediately calls confirmation (`internal/gui/gui_restart_protocol.go:244-271`). Confirmation performs one `client.Do` and returns the first connection error (`internal/gui/gui_restart_protocol.go:529-531`). Concrete ordering: parent starts child -> continuation dials before child binds -> `ECONNREFUSED` -> rollback, although the child could bind within the two-second deadline required by `item3-restart-design.md:315-316`. The context supplies a deadline, not retries. Production scheduling is `static-inference`; **ASSUMPTION (UNVERIFIED):** reproduce with a deterministic authenticated listener that opens after the first dial but before deadline.

2. **[P1] Same-port quiesce/close rollback can destroy a recoverable parent — confirmed (`static-read`).** `EnterGrace` changes mode before waiting and leaves grace installed on timeout (`internal/gui/gui_listener_lifecycle.go:149-162`). `CloseListener` returns on close/wait error before clearing `current` (`internal/gui/gui_listener_lifecycle.go:229-240`). Either error routes to rollback (`internal/gui/gui_restart_protocol.go:258-281`), whose same-port branch unconditionally calls `BindForRecovery` (`internal/gui/gui_restart_protocol.go:338-343`); binding rejects while `current != nil` (`internal/gui/gui_listener_lifecycle.go:89-93`). The resulting restore failure releases the lease and exits (`internal/gui/gui_restart_protocol.go:363-371`) instead of the mandated “restore full; no release” behavior (`item3-restart-design.md:808`).

3. **[P1] Accepted response is not ordered before process exit — confirmed (`static-read`).** `Start` launches `continueHandoff` before returning (`internal/gui/gui_restart_protocol.go:219-225`). That goroutine may reach `Exit` (`internal/gui/gui_restart_protocol.go:299-317`) before the handler writes the 202 body (`internal/gui/gui_self_restart.go:171-200`). Concrete ordering: scheduler runs the new goroutine through fast confirm/reserve/release/exit before rescheduling the handler; the client receives no accepted response. No response-written/flush barrier connects the handler to the continuation. Production timing is `static-inference`; **ASSUMPTION (UNVERIFIED):** falsify with a blocked response writer and instant coordinator dependencies.

4. **[P2] Post-spawn rollback can leave/reuse the nonce file — confirmed (`static-read`).** The fixed path is written at `internal/gui/gui_restart_protocol.go:183-184`; removal exists only for spawn error or invalid returned child (`internal/gui/gui_restart_protocol.go:200-214`). Prepare/reserve failures merely zero the parent buffer (`internal/gui/gui_restart_protocol.go:278-295`), and successful rollback resets the coordinator without removing the file (`internal/gui/gui_restart_protocol.go:353-357`). A delayed unauthenticated child is detach-only (`internal/gui/gui_restart_protocol.go:347-352`) and consumes that same fixed leaf (`internal/gui/gui_restart_protocol.go:634-650`). Concrete ordering: child A is delayed before consume -> parent rolls back and starts generation B -> B overwrites fixed leaf -> A consumes/unlinks B's nonce. Cross-process timing is `static-inference`; **ASSUMPTION (UNVERIFIED):** deterministic two-generation delayed-consume test.

5. **[P2] Duplicate request is misreported as spawn failure — confirmed (`static-read`).** The mutex prevents interleaving but returns “already in progress” (`internal/gui/gui_restart_protocol.go:131-138`). The endpoint maps every `Start` error to 200 with `spawn_error` (`internal/gui/gui_self_restart.go:171-180`), so a second tab displays “Restart incomplete” (`internal/gui/frontend/src/components/settings/SectionGuiServer.tsx:70-78`) while the first restart continues. This violates the requested 202 in-progress contract.

## Contracts

The 2xx requirement is otherwise preserved: the frontend throws before body parsing only for non-2xx (`internal/gui/frontend/src/api.ts:933-948`), and v3 pre-accept failures remain 200. Added response fields are ignored safely by the structural TypeScript cast. Gate-OFF still branches to the existing v1 handler before v3 (`internal/gui/gui_self_restart.go:112-120`).

## Tests & coverage

Existing tests cover happy ordering, reserve-failure rebind, post-release no-op, 202, spawn-failure 2xx, and child bind retry (`internal/gui/gui_restart_protocol_test.go:197`, `internal/gui/gui_restart_protocol_test.go:277`, `internal/gui/gui_restart_protocol_test.go:393`, `internal/gui/gui_self_restart_test.go:130`, `internal/gui/gui_self_restart_test.go:161`, `internal/cli/gui_self_restart_handoff_test.go:92`). They do not exercise delayed readiness, `EnterGrace`/`CloseListener` errors, response-vs-exit ordering, delayed nonce consumption across generations, or duplicate request semantics. No tests/builds were run; review is static by assignment.

## Similar implementations

The retained v1 handler writes its response before launching the delayed exit goroutine (`internal/gui/gui_self_restart.go:120-160`), unlike v3's independently running coordinator. Git history/blame shows the Phase G parent surface is uncommitted; no earlier same mechanism supplies missing ordering.

## Constraints

Default-OFF rollout, exact-child authentication, retained parent lease before release, and no supervisor fleet stop remain fixed. Graphify was not used. Serena was probed but unavailable at the configured local endpoint; shell reads were used.

## Change risks

The three P1 mechanisms can turn a valid restart into rollback or eliminate delivery/parent availability. The P2 nonce collision can poison a later generation; the duplicate response misstates live state. Clean facts (`static-read`): `c.run` serializes generations; `grace.released` uses atomic Store/Load (`internal/gui/gui_restart_protocol.go:306`, `internal/gui/gui_restart_protocol.go:423`); accepted-child handle ownership is single-goroutine and terminal paths terminate/reap authenticated children or detach unproven children; reviewed contexts have cancellation.

## Unresolved questions

Only the three explicitly labeled cross-scheduler/cross-process probes remain runtime-unverified; their missing ordering is directly visible statically.

## Research admission gates

Known limitations: five defects above. Regression risk: GUI loss, false restart failure, nonce generation collision. Bounded falsification probes are named inline. Gate result: REVISE.

## Adjacent findings

None. Searched and excluded: marker/lease/grace atomics, accepted-child handle lifetime, gate-OFF response shape, frontend 2xx parsing, and prior focal-file history. Two widening steps (frontend consumer; HEAD/history) changed no conclusion, so exploration stopped.

REVISE

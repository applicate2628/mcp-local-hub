# Bug: Restart-v3 validates the reservation only after releasing the parent lease

- id: 2026-07-18-restart-v3-validates-reservation-after-release
- context: 2026-07-16-productization-gui-solidify
- status: fixed
- severity: medium
- area: internal/gui/gui_restart_protocol.go
- found-by: qa-engineer

## Reproduction by source trace

`RestartCoordinator.continueHandoff` accepts a nil or non-`reserved` record whenever `MarkerStore.Reserve` returns no error (`internal/gui/gui_restart_protocol.go:284-297`). It then closes the parent hub and releases the single-instance lease (`internal/gui/gui_restart_protocol.go:299-305`). Only after release, local cleanup, and the process-exit seam does it call `reservedResultError(reserved)` (`internal/gui/gui_restart_protocol.go:308-317`); that function rejects nil/non-`reserved` records at `internal/gui/gui_restart_protocol.go:320-324`.

Expected: validate the returned reservation before hub close and lease release. An invalid reservation result is a pre-release preparation failure and must enter the existing rollback path while `parentLeaseReleased == false`.

Actual: the coordinator crosses the irreversible release boundary first and reports the invalid reservation afterward. This contradicts its own no-protocol-decision comment (`internal/gui/gui_restart_protocol.go:299-300`) and the plan's post-release no-op boundary (`work-items/active/2026-07-16-productization-gui-solidify/item3-unitB-plan.md:190`).

## Coverage gap

The current marker-store fakes always return a valid `reserved` record, so `TestRestartV3_ParentPerformsNoProtocolWriteWaitTerminateOrReclaimAfterRelease` remains green while this post-release decision executes (`internal/gui/gui_restart_protocol_test.go:393-450`). Add a fake that returns nil or a non-`reserved` record with nil error and assert zero hub close, zero lease release, and pre-release rollback.


## Resolution (2026-07-18) — MOOT (defensive-only, not a live release-before-validate)
`MarkerStore.Reserve` returns `(record, nil)` ONLY when the in-flock phase transition succeeded
(`record.Phase = HandoffPhaseReserved`, gui_restart_record.go); it returns `(_, error)` otherwise.
`continueHandoff` rolls back BEFORE any hub-close/lease-release on `err != nil`, so a nil/non-reserved record
never reaches the release path. `reservedResultError(reserved)` is a belt-and-suspenders check that cannot
fire when `err == nil`. The round-3 fable commission independently cleared the marker-store semantics + the
rollback matrix (no post-release protocol write, no double-release). No behavior change needed.

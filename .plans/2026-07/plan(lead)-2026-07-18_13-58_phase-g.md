# Phase G execution plan snapshot

Canonical plan: `work-items/active/2026-07-16-productization-gui-solidify/item3-unitB-plan.md`, Phase G.

Admission: direct human decision and the 2026-07-18 `$lead` amendment authorizing `internal/cli/gui.go`
plus adjacent tests. Current baseline: `beadf474` (requested `58340fd5` plus the documentation-only surface
amendment).

1. Refresh `brief.md`, `status.md`, and `work-items/index.md` for Phase G.
2. Delegate one ordering-cohesive `$backend-engineer` implementation package.
3. Require test-first AC-G1 through AC-G8 coverage and a build after each production file.
4. Run an independent `$qa-engineer` claim-verification gate and bounded corrections.
5. Run `go build ./...`, `go vet ./...`, touched-file `gofmt -l`, and
   `go test -tags=test_state_path_env -count=1 -timeout 15m ./internal/gui/ ./internal/cli/`.
6. Sweep `mcphub.exe`, reconcile every acceptance criterion and invariant, and report exact file:line evidence.

Constraints: no Graphify; no `claude` CLI; do not set `MCPHUB_GUI_SPAWN_TESTS`; no real GUI spawn; no commit;
all cli/api/gui tests touching state paths use `-tags=test_state_path_env`; gate-OFF remains v1.

## 2026-07-18 seven-finding correction cycle

The accepted deep review at `item3-unitB-phaseG-deep-review.md` requires seven test-first corrections before
the Phase G gate can be repeated. Each task is RED (run the named tagged regression and observe the specified
failure), GREEN (make the smallest owner-level correction), then rerun the named regression plus the existing
`TestRestartV3_*` set.

1. Add `TestRestartV3_CloseHubBarrierRejectsRacingHubProducerAfterShutdown`; make one Server-owned barrier
   cancel and join the hub initializer/restart driver, reject late publication, and close the taken component
   before the coordinator releases the GUI lease.
2. Add `TestRestartV3_ConfirmRetriesConnectionRefusedUntilChildBinds`; retry only transient loopback dial
   failures on a bounded cadence inside `RestartDeadlines.Bind`, while authentication/status failures remain
   terminal.
3. Add `TestRestartV3_SamePort_EnterGraceFailureRestoresLiveListenerWithoutRebind`; pass a concrete
   listener-closed fact into rollback so a retained same-port listener uses `RestoreFull`, while a successfully
   closed listener keeps the existing `BindForRecovery` plus `ServeFull` path.
4. Add `TestRestartV3_API202FlushesBeforeCoordinatorCanExit`; return an acknowledgement with the accepted
   coordinator start, flush the JSON 202 body, and open the irreversible continuation only after that flush.
5. Add `TestRestartV3_NoncePathIsGenerationBoundAndStaleGenerationCannotConsumeNext`; derive the canonical
   nonce leaf from the generation, validate that exact leaf in the child, retain secret-file hardening for the
   generated leaf, and remove the generation's nonce on every terminal path.
6. Add `TestRestartV3_ConcurrentRestartReturnsActive202`; preserve the accepted start descriptor while the
   run is active and map the typed duplicate-start result to the same 202 `restarting:true` response.
7. Add `TestRestartV3_PostBeginCleanupFailureTerminalizesMarkerBeforeRunReset`; centralize post-Begin
   marker/nonce/retained-handle cleanup, reset in-memory run state only after residue removal is proved, and
   terminalize a nonterminal marker when proved cleanup fails.

Final verification remains exactly the commands and process sweep in steps 5-6 above. No commit is permitted.

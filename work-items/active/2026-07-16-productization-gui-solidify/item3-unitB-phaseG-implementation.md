# Phase G backend implementation package

## Receiving-side echo

- Named regression guards: AC-G1 through AC-G8 are mapped below to fresh passing tests. The rollback, post-release, and grace guards were additionally self-falsified with temporary mutations and restored.
- Diff-invisible invariants: the concrete `parentLeaseReleased` boolean gates rollback; the hub close precedes the one lease release; successful rollback retains the lease and performs no reacquire; terminal rollback releases and exits; successful handoff detaches the retained child handle without terminating it; gate OFF retains the v1 endpoint path.
- Defect-class inventory: marker writes, authenticated child termination, readiness waits, recovery binds, lease reacquire, and recovery claims are forbidden after release. `TestRestartV3_ParentPerformsNoProtocolWriteWaitTerminateOrReclaimAfterRelease` instruments every Phase-G seam in that class.

## Diagnostic and hypothesis record

Verified implementation premises:

1. CLI composition can inject the lease, parser-aware argv/spawn, and process-exit dependencies without a GUI-to-CLI import (`internal/cli/gui.go:95-211`, `internal/cli/gui.go:838-889`).
2. The installed Go process API provides exact-handle `Kill`, `Wait`, and no-wait `Release`; `go doc os.Process.{Kill,Wait,Release}` and `go doc os/exec.Cmd.Wait` were executed before implementation.
3. One `GUIListenerOwner` owns grace/drain/close/recovery/full operations (`internal/gui/gui_listener_lifecycle.go:117-163`, `internal/gui/gui_listener_lifecycle.go:211-241`).
4. Exact standby confirmation reuses the Phase-F challenged identity and message-authentication-code verifier (`internal/gui/ping.go:123-150`, `internal/gui/ping.go:188-196`; parent probe at `internal/gui/gui_restart_protocol.go:499`).
5. Server owns the hub component and atomically removes it through `ShutdownHubListener` immediately before the CLI-owned lease release (`internal/gui/server.go:920-934`, `internal/gui/gui_restart_protocol.go:300-305`).

No implementation-driving assumption remains unverified.

## Changed files and symbols

| File | Phase-G symbols and behavior |
| --- | --- |
| `internal/gui/gui_restart_protocol.go` | `RestartCoordinator`, `RestartCoordinatorDependencies`, `RestartParentChild`, concrete pre/post-release boundary, healthy and terminal rollback, grace handler, `ConfirmAuthenticatedStandby`, and exported validated handoff decode. |
| `internal/gui/gui_self_restart.go` | v3 response fields and handler, `RequestSelfRestartExit`, exact retained-process adapter, `SpawnRestartV3GUI`, structured owner-only nonce-path handoff environment. The v1 spawn/exit branch remains below the gate. |
| `internal/gui/server.go` | `restartCoordinatorStarter`, `Server.ConfigureRestartCoordinator`, and owner-side `closeOwnHubListenerForRestart` using `hubMcpComp.Swap(nil)` plus `ShutdownHubListener`. |
| `internal/cli/gui.go` | `restartV3ParentRuntime`, parser-aware `buildRestartV3ParentDependencies`, idempotent owned lease threading, structured child target-port consumption, same-port bind retry, self-restart exit marker, and manager-stop guard. |
| Adjacent tests | AC-G1 through AC-G8, retained lease/argv composition, same-port bind wait, gate-ON API contract, grace redirect trust, and real-store ensure-alive no-op coverage. No real GUI process was spawned by a Phase-G test. |

## Acceptance-criteria map

| AC | Enforcing tests |
| --- | --- |
| G1 | `TestRestartV3_PortChange_ParentClosesHubBeforeFlockReleaseThenChildActivatesImmediately` (`internal/gui/gui_restart_protocol_test.go:197`). |
| G2 | `TestRestartV3_SamePort_PreReleaseRollbackRetainsLeaseAndRebindsWithoutReacquire` (`internal/gui/gui_restart_protocol_test.go:277`) and `TestRestartV3_SamePortStandbyBindWaitsForParentClose` (`internal/cli/gui_self_restart_handoff_test.go:92`). |
| G3 | `TestRestartV3_PreReleaseRollbackFailureInterruptsReleasesLeaseAndExits` (`internal/gui/gui_restart_protocol_test.go:327`), including interrupted-marker write failure. |
| G4 | `TestRestartV3_ParentPerformsNoProtocolWriteWaitTerminateOrReclaimAfterRelease` (`internal/gui/gui_restart_protocol_test.go:393`). |
| G5 | `TestRestartV3_API202RetainsRestartingField` (`internal/gui/gui_self_restart_test.go:130`) and `TestRestartV3_SpawnFailureReturns2xxNonRestartingBody` (`internal/gui/gui_self_restart_test.go:161`). |
| G6 | `TestRestartV3_GraceAllowlistRedirectAndMutatorRejection` (`internal/gui/gui_restart_protocol_test.go:497`) plus the real-owner drain guard `TestGUIListenerOwner_EnterGraceRejectsNewMutatorsAndDrainsAdmitted` (`internal/gui/gui_listener_lifecycle_test.go:217`). |
| G7 | `TestRestartV3_SuccessfulHandoffExitSkipsManagerStop` (`internal/cli/gui_self_restart_handoff_test.go:138`), the retained v1 exit guard `TestGUISelfRestart_SpawnSuccess` (`internal/gui/gui_self_restart_test.go:58`), and adopted-owner guard `TestArmSupervisorManager_AdoptedOwnerReturnsNilNoLoop` (`internal/cli/gui_supervisor_owner_test.go:835`). |
| G8 | `TestRestartV3_ProvedRollbackClearsMarkerAndEnsureAliveTickDoesNothing` (`internal/cli/supervise_ensure_alive_test.go:466`): real store, proved confirm-timeout rollback, absent marker, one real Phase-I classifier tick, zero output/probe/mutation, unchanged event log, and no `gui-restart-live-holder-wedged`. |

## TDD and self-falsification evidence

- G1 RED: coordinator types absent; GREEN after the first protocol slice.
- G5 RED: Server coordinator seam and v3 response fields absent; GREEN after server/handler wiring.
- Same-port RED: first `EADDRINUSE` ended standby binding; GREEN after bounded address-in-use retry.
- G7 RED: self-restart exit marker and manager-stop guard absent; GREEN after the explicit boundary.
- G8 RED: proved rollback left the unauthenticated child's retained handle open; GREEN after close-release without termination.
- G2 temporary mutation disabled same-port recovery; the test failed because the lease was released. Restored test passed.
- G3 temporary mutation disabled the interrupted write; both marker-success and marker-write-failure subtests failed on zero writes. Restored test passed.
- G4 temporary mutation added one post-release marker write; the test failed with post-release marker count one. Restored test passed.
- G6 temporary mutation widened grace to `/api/health`; the test failed on 204 instead of required 503. Restored test passed.

## Endpoint, ordering, and failure contract

- Gate ON success: HTTP 202 with `handoff_id`, `generation`, `phase:"in-progress"`, `spawned:true`, `spawned_pid`, `restarting:true`, `old_port`, and optional changed `target_port` (`internal/gui/gui_self_restart.go:163-201`).
- Gate ON pre-accept/spawn failure: HTTP 200 with `spawned:false`, `spawned_pid:0`, `restarting:false`, and `spawn_error`; the existing frontend can read the body.
- Gate OFF: the original `selfRestartSpawnFn` v1 path remains selected (`internal/gui/gui_self_restart.go:115-161`).
- Success ordering: bounded parent hub close at `internal/gui/gui_restart_protocol.go:300-303`, lease release at `:304`, then only local detach/grace-listener cleanup/process exit. No child protocol decision occurs after release.
- Healthy rollback: exact authenticated child termination only, owned recovery bind/full restore, marker erase, lease retained, zero reacquire (`internal/gui/gui_restart_protocol.go:327-360`). An unauthenticated child is not killed; only the retained parent handle is close-released.
- Terminal rollback: write `interrupted` when possible, publish the marker-write-failure discriminator otherwise, bounded hub cleanup, exactly one release through `releaseOnceLease`, then self-restart exit (`internal/gui/gui_restart_protocol.go:362-371`, `internal/cli/gui.go:838-842`).
- The self-restart exit marker causes the manager-stop defer to no-op even when a test exit seam returns (`internal/cli/gui.go:111-131`, `internal/cli/gui.go:1159`). Production still crosses `os.Exit` through `RequestSelfRestartExit`.

## Timeouts and resource lifetime

- Child proof/bind, quiesce, reservation, rollback, and grace use injected `RestartDeadlines`.
- Same-port standby retries only address-in-use under the existing bind context, with 25 ms backoff.
- Parent hub close is capped by a five-second context; `ShutdownHubListener` force-closes the HTTP server if graceful shutdown expires.
- Exact child termination starts one bounded wait/reap; successful handoff closes the retained process handle without `Wait` or termination.
- Nonce bytes are zeroed locally; the child consumes the owner-only file; the nonce value never enters argv, environment, marker, or event data.

## Verification

```text
gofmt -l <eight touched Go files>
Exit code: 0
Output: <empty>

go build ./...
Exit code: 0
Output: <empty>

go vet ./...
Exit code: 0
Output: <empty>

go test -tags=test_state_path_env -count=1 -timeout 15m ./internal/gui/ ./internal/cli/
Exit code: 0
ok  	mcp-local-hub/internal/gui	233.507s
ok  	mcp-local-hub/internal/cli	230.588s

mcphub.exe stabilization sweep (after an instantaneous zero did not persist)
Exit code: 0
mcphub.exe stabilization sweep: complete; stopped 35; zero remaining for 2s
```

`git diff --check` also returned exit 0 with empty output. A residue probe confirmed `MCPHUB_GUI_SPAWN_TESTS: unset`. No commit was created. A later read-only process inventory found the installed external GUI had restarted its supervisor/daemon fleet from the user installation, not the workspace/test binary; no scheduler or autostart state was mutated to suppress that live fleet.

## Residual risk

- Per the explicit constraint, Phase-G tests use retained-child seams and do not spawn a real GUI. Exact Windows detached-process handle behavior is backed by the installed Go API surface and production adapter but is not exercised by this unit gate.
- Phase H frontend progress/navigation remains outside this backend phase.

## Gate

PASS — Phase G backend implementation and required local verification are complete; independent lead/QA review remains downstream.

## Seven-finding correction (2026-07-18)

The deep review's seven confirmed defects were corrected without changing the default-OFF v1 route or the successful handoff contract.

| Finding | Owning correction | Regression |
|---|---|---|
| P1-1 | The Server-owned hub producer barrier closes publication admission, cancels and joins both producers, then takes and closes the current component (`internal/gui/server.go:518`, `internal/gui/server.go:570`, `internal/gui/server.go:1012`). | `TestRestartV3_HubLifecycleBarrierJoinsPublishersBeforeRelease` |
| P1-2 | Standby confirmation retries transient network readiness failures at a bounded cadence under the single bind deadline; protocol/authentication failures remain terminal (`internal/gui/gui_restart_protocol.go:27`, `internal/gui/gui_restart_protocol.go:613`). | `TestRestartV3_ConfirmRetriesConnectionRefusedUntilChildBinds` |
| P1-3 | Same-port rollback records listener closure only after a successful close and restores the still-live listener without rebinding when preparation failed earlier (`internal/gui/gui_restart_protocol.go:317`, `internal/gui/gui_restart_protocol.go:441`). | `TestRestartV3_SamePort_EnterGraceFailureRestoresLiveListenerWithoutRebind` |
| P2-4 | The coordinator waits for the HTTP handler's explicit post-flush acknowledgement before hub close, lease release, or exit (`internal/gui/gui_restart_protocol.go:239`, `internal/gui/gui_restart_protocol.go:394`, `internal/gui/gui_self_restart.go:204`). | `TestRestartV3_API202FlushesBeforeCoordinatorCanExit` |
| P2-5 | Nonce leaves are derived from the generation and validated against that exact generation; every coordinator terminal path attempts removal (`internal/gui/gui_restart_protocol.go:277`, `internal/gui/gui_restart_protocol.go:802`, `internal/api/state_read_inode_anchor.go:87`). | `TestRestartV3_NoncePathIsGenerationBoundAndStaleGenerationCannotConsumeNext` |
| P2-6 | A concurrent request waits for and returns the active accepted descriptor as HTTP 202 with `restarting:true`, while true pre-accept failures retain the existing HTTP 200 error body (`internal/gui/gui_restart_protocol.go:147`, `internal/gui/gui_self_restart.go:172`). | `TestRestartV3_ConcurrentRestartReturnsActive202` |
| P2-7 | One post-`Begin` cleanup owner removes nonce and marker before resetting the in-memory run; marker-removal failure is surfaced and terminalized as interrupted (`internal/gui/gui_restart_protocol.go:277`). | `TestRestartV3_PostBeginCleanupFailureTerminalizesMarkerBeforeRunReset` |

Wire contract before/after: ordinary accepted restart remains HTTP 202 and ordinary spawn failure remains HTTP 200; only a duplicate active restart changes from the misleading HTTP 200 `spawn_error` body to the existing active HTTP 202 restart body. No fields were added, removed, or renamed by this correction. Same-origin authorization, the default-OFF gate, and supervisor-survival exit ownership are unchanged.

Fresh correction verification:

```text
MCPHUB_GUI_SPAWN_TESTS_PRESENT=False
go test -tags=test_state_path_env -count=1 -timeout 15m -run '^(TestRestartV3_|TestGUISelfRestart_SpawnSuccess$)' ./internal/gui/ ./internal/cli/
ok  	mcp-local-hub/internal/gui	0.929s
ok  	mcp-local-hub/internal/cli	0.213s

go build ./...
Exit code: 0
Output: <empty>

go vet ./...
Exit code: 0
Output: <empty>

gofmt -l internal/api/state_read_inode_anchor.go internal/cli/gui.go internal/cli/gui_self_restart_handoff_test.go internal/cli/supervise_ensure_alive_test.go internal/gui/gui_restart_protocol.go internal/gui/gui_restart_protocol_test.go internal/gui/gui_self_restart.go internal/gui/gui_self_restart_test.go internal/gui/hub_listener.go internal/gui/server.go
Exit code: 0
Output: <empty>

go test -tags=test_state_path_env -count=1 -timeout 15m ./internal/gui/ ./internal/cli/
ok  	mcp-local-hub/internal/gui	225.414s
ok  	mcp-local-hub/internal/cli	216.027s

WORKSPACE_MCPHUB_BEFORE=0
WORKSPACE_MCPHUB_STOPPED=0
WORKSPACE_MCPHUB_AFTER=0
```

Correction gate: PASS for backend implementation and the required local verification. Independent QA and architecture review remain downstream; no commit was created.

## Terms and Abbreviations

- API: Application Programming Interface.
- FLOCK: operating-system-backed GUI single-instance file lock.
- MAC: Message Authentication Code.
- TDD: Test-Driven Development.

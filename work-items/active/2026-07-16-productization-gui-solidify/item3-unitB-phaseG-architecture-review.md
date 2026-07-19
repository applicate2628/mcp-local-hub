# Phase G architecture review

- Date: 2026-07-18
- Role: `$architecture-reviewer`
- Review strategy: Adversarial
- Target: uncommitted Phase G implementation on `feat/gui-restart-unitb-gated`
- Gate: **REVISE**

## Receiving-side echo

- Approved production surface: `internal/gui/gui_restart_protocol.go`, `internal/gui/gui_self_restart.go`, `internal/gui/server.go`, and `internal/cli/gui.go`; adjacent tests are in scope.
- Diff-invisible invariants reviewed: gate OFF retains v1; same-port hub-toggle rebind remains live; Unit A argv/port behavior remains guarded; spawn failure remains HTTP 200/2xx; successful handoff skips `manager.Stop`.
- Named regression guard reviewed and run: `TestRestartV3_ParentPerformsNoProtocolWriteWaitTerminateOrReclaimAfterRelease`.
- The implementation package contained the required receiving-side echo and six `{guarantee, single owner, probe}` claim triples.
- I did not read `item3-restart-design.md` or any QA report.

## Reviewed surfaces

| File | Contract actually read |
| --- | --- |
| `README.md` | Live repository status and Go build entry point (`README.md:82-94`, `README.md:222-228`). |
| `item3-unitB-phaseG-implementation.md` | Implementation claims, AC mapping, reported checks, and receiving-side echo. |
| `internal/gui/gui_restart_protocol.go` | Parent coordinator, concrete release boundary, rollback, grace handler, readiness confirmation, child descriptor, and retained-child interface (`:34-128`, `:131-399`, `:401-547`, `:550-733`). |
| `internal/gui/gui_self_restart.go` | Gate-ON/OFF endpoint wire shape, v1 exit, v3 process adapter, retained handle cleanup (`:69-214`, `:216-331`, `:334-379`). |
| `internal/gui/server.go` | Server ownership, coordinator composition, hub close, hub initializer/restart driver, and normal shutdown (`:510-527`, `:912-935`, `:1013-1076`, `:1090-1233`). |
| `internal/cli/gui.go` | Idempotent lease wrapper, parent composition, child startup, gate resolution, runtime wiring, and manager-stop defer (`:31-201`, `:204-359`, `:780-895`, `:1142-1162`). |
| `internal/gui/gui_restart_record.go` | Deadlines and marker state owner, including clear-after-proved-rollback (`:29-59`, `:151-177`, `:195-310`, `:350-392`). |
| `internal/gui/single_instance.go` | Lease ownership/Release contract (`:40-61`, `:190-220`). |
| `internal/gui/gui_listener_lifecycle.go` | Sole GUI listener owner, grace/drain, close, recovery bind, full restore, and shutdown (`:38-64`, `:117-163`, `:165-249`, `:252-295`, `:311-354`). |
| `internal/gui/hub_listener.go` | Hub restart producer, component publication, and bounded component shutdown (`:172-275`, `:343-575`, `:1071-1133`). |
| `internal/gui/events.go` | Nonblocking event publication and SSE flush behavior (`:323-392`, `:557-607`). |
| `internal/gui/gui_restart_gate.go` | Default-OFF gate owner (`:9-37`). |
| `internal/cli/supervise_ensure_alive.go` | Stale nonterminal-marker classification and held-owner degrade (`:460-523`, `:607-615`). |
| `internal/api/hub_mcp_session.go` | Hub session-store close/join semantics (`:386-462`). |
| `internal/gui/gui_restart_protocol_test.go` | G1-G4/G6 seam probes and post-release instrumentation (`:21-535`). |
| `internal/gui/gui_self_restart_test.go` | v1 fallback and G5 endpoint probes (`:24-181`). |
| `internal/gui/gui_listener_lifecycle_test.go` | Real GUI owner same-port/hub-survival and mutator-drain guards (`:18-215`, `:217-300`). |
| `internal/cli/gui_self_restart_handoff_test.go` | Gate selection, child bind retry, G7, parent composition, and child continuation (`:23-260`). |
| `internal/cli/supervise_ensure_alive_test.go` | Real-store G8 rollback/ensure-alive guard (`:456-540`). |
| `internal/cli/gui_port_test.go` | Unit A parser-aware port/argv matrix (`:35-90`). |

## Claim verdicts

| # | Verdict | Verification |
| --- | --- | --- |
| 1 | **failed** | `RestartCoordinator` does call `CloseHub` before `Lease.Release` (`internal/gui/gui_restart_protocol.go:299-305`), and G1 passes. |
|  |  | The real Server close only swaps/shuts the current component (`internal/gui/server.go:933-934`) while hub publishers remain live; an in-flight publisher can repopulate the slot after the claimed boundary. |
| 2 | **verified** | Rollback is gated by the concrete local `parentLeaseReleased` value (`internal/gui/gui_restart_protocol.go:235-280`, `:327-371`). |
|  |  | Proved async rollback retains the lease; terminal rollback interrupts, closes/detaches, releases, and exits. G2/G3/G8 pass. This verdict does not cover the separate pre-accept cleanup defect below. |
| 3 | **verified** | After release, the success branch performs only retained-handle detach, optional old-GUI grace cleanup, the injected exit, and local result construction (`internal/gui/gui_restart_protocol.go:304-317`). |
|  |  | No marker mutation, readiness confirmation, termination, recovery bind, claim, or reacquire occurs. G4 passes. |
| 4 | **verified** | Route registration retains the same-origin guard (`internal/gui/gui_self_restart.go:103-107`); gate ON uses the v3 starter and 202/200 bodies (`:109-200`); gate OFF falls through to the retained v1 path (`:119-160`). G5 and the v1 guard pass. |
| 5 | **failed** | The dependency graph remains acyclic (`internal/cli` imports `internal/gui`; `internal/gui` does not import `internal/cli`), and settings/argv policy is composed in CLI (`internal/cli/gui.go:133-201`). |
|  |  | Actual v3 process spawning and process termination remain implemented in the reusable GUI package (`internal/gui/gui_self_restart.go:206-214`, `:216-319`) and are merely selected by CLI (`internal/cli/gui.go:102-119`). That contradicts the approved CLI ownership boundary and D1. |
| 6 | **failed** | The normal success/rollback handle paths are explicit, and production lease release is idempotent through `releaseOnceLease` (`internal/cli/gui.go:79-93`, `:835-842`). |
|  |  | An invalid non-nil spawned child is not detached, several post-`Begin` cleanup branches ignore marker/nonce cleanup failures, successful post-release detach errors are discarded, and the hub producer race can recreate a listener after cleanup. |

## Top three adversarial failure mechanisms

### 1. Hub close can be undone after the flock is released

- Defect class: lifecycle ownership race; C1 single-owner violation; D4 resource-lifetime violation; no-fix-layering violation.
- Exact mechanism:
  1. `runActivatedGUIListener` creates `hubInitCtx` and launches both an asynchronous initializer and `runHubListenerRestartDriver` (`internal/gui/server.go:1121-1127`, `:1196-1197`).
  2. The restart coordinator's Server close calls only `s.hubMcpComp.Swap(nil)` plus `ShutdownHubListener` (`internal/gui/server.go:933-934`). It cannot cancel or join either producer.
  3. An initializer already past bind, or a restart attempt blocked in `startFn`, can resume and `CompareAndSwap(nil, newComp)` after the close (`internal/gui/server.go:1172-1183`; `internal/gui/hub_listener.go:474-544`).
  4. `RestartCoordinator` then releases the GUI flock (`internal/gui/gui_restart_protocol.go:301-305`), so the outgoing parent can again own a hub socket after the mandated hub-close boundary while the child activates.
- Why the current probe misses it: G1 injects `CloseHub: func(context.Context)` and records a list step; it never runs the Server initializer/restart driver (`internal/gui/gui_restart_protocol_test.go:197-275`).
- Narrowest design correction: make Server's hub lifecycle one owner that holds the producer cancel plus completion barrier. Its restart close operation must cancel hub initialization/restart production, join it within the same bounded budget, then atomically take and shut down the last component. Normal Server shutdown and Phase G must call that same owner operation; do not add another `Swap(nil)` variant.
- Deterministic falsifying regression probe:
  1. In a `package gui` test, inject `startHubMcpListenerFn` that signals entry and blocks before returning a live `HubListenerComponents`.
  2. Start the Server and wait for the injected initializer to be in flight while `hubMcpComp` is nil.
  3. Call the restart-specific hub-close operation; then unblock the initializer.
  4. Assert the close does not return until the producer is settled, `hubMcpComp.Load()` remains nil, the returned component is shut down exactly once, and no hub listener is alive when the fake lease release step runs.
  5. Repeat with the restart driver after it has taken the old component and is blocked producing its replacement. This engineers the race window rather than relying on scheduler timing.

### 2. Healthy-parent early failures can leave durable restart residue or a retained child handle

- Defect class: repeated pre-accept cleanup logic; D4 resource leak; C1 cleanup-owner violation; fable P2 false-degrade regression.
- Exact mechanism:
  1. After `Begin`, `Start` has separate cleanup branches for nonce creation, nonce validation, nonce write, spawn error, and invalid child (`internal/gui/gui_restart_protocol.go:160-216`).
  2. Four branches discard `ClearAfterProvedPreReleaseRollback` errors (`:173-186`, `:211-215`); nonce removal errors are discarded (`:200-214`).
  3. If `Spawn` returns a non-nil retained handle whose `PID()` is invalid, the branch returns without `DetachAtRelease` (`:211-216`).
  4. The parent retains the flock and resumes normal operation, but an uncleared in-progress marker later reaches `FreshUntil`; Phase I classifies the healthy holder as wedged and emits `gui-restart-live-holder-wedged` with a kill instruction (`internal/cli/supervise_ensure_alive.go:475-512`, `:607-615`).
- Why G8 misses it: G8 covers a confirmation-timeout rollback where `ClearAfterProvedPreReleaseRollback` succeeds (`internal/cli/supervise_ensure_alive_test.go:466-540`). It does not cover cleanup failure before the continuation starts.
- Narrow correction: consolidate all post-`Begin`, pre-continuation failures into one cleanup owner that tracks marker, nonce file, nonce bytes, and optional retained child. It must close every acquired handle exactly once and map cleanup failure to the accepted terminal rollback-failure policy; an ordinary 200 `spawned:false,restarting:false` return is valid only after proving the marker and nonce residue are gone.
- Required probes: non-nil/PID-zero child is detached; nonce removal failure is surfaced; clear failure cannot leave a healthy parent plus a stale nonterminal marker; an ensure-alive tick after every healthy pre-accept failure emits and mutates nothing.
- `[CROSS-DOMAIN: security]` A failed nonce removal can leave the owner-only one-shot secret file on disk (`internal/gui/gui_restart_protocol.go:183-214`). Security severity is not assigned in this review.

### 3. CLI process ownership is nominal; GUI still owns the terminating primitive

- Defect class: D1 termination-boundary violation; A6 adapter-placement violation; approved ownership deviation.
- Exact mechanism:
  1. CLI injects `Exit`, but its production value wraps `gui.RequestSelfRestartExit` (`internal/cli/gui.go:102-119`).
  2. `RequestSelfRestartExit` calls the GUI package's process-global function whose default is `os.Exit(0)` (`internal/gui/gui_self_restart.go:203-214`).
  3. The new retained `exec.Cmd`, `Kill`, `Wait`, and `Release` adapter is also implemented in GUI (`internal/gui/gui_self_restart.go:216-319`) even though the approved boundary says CLI composition owns argv/process spawn and process exit.
  4. G7 proves only that the CLI wrapper sets an atomic flag and skips a fake manager stopper (`internal/cli/gui_self_restart_handoff_test.go:138-150`); it does not prove that the composition root owns the real terminating primitive.
- Narrow correction: inject a CLI-owned v3 exit primitive directly from the composition root and keep the GUI coordinator return/status based. Move the v3 retained-process adapter to the CLI composition layer, or place only reusable OS process mechanics on a neutral lower surface while CLI retains the spawn/termination decision. Do not expand this correction into the retained gate-OFF v1 branch.

## Blocking deviations and routing

| ID | Level | Required owner | Required correction |
| --- | --- | --- | --- |
| AR-G-1 | Design seam | `$architect` | Define one Server-owned cancel/join/take hub lifecycle operation usable by normal shutdown and restart close. |
| AR-G-1 |  | `$backend-engineer` after design acceptance | Implement the owner and deterministic in-flight initializer/restart-driver probes. |
| AR-G-2 | Code, with failure-policy confirmation | `$backend-engineer` | Consolidate pre-accept cleanup and close every acquired resource; add residue/ensure-alive probes. |
| AR-G-2 |  | `$architect` if the 200 body conflicts with terminal cleanup | Resolve the cleanup-failure response/exit contract without weakening AC-G8. |
| AR-G-3 | Code/boundary | `$backend-engineer` | Make the CLI the actual v3 exit owner. |
| AR-G-3 |  | `$architect` only if a neutral retained-process adapter is needed | Select the stable lower process seam without duplicating detached-spawn machinery. |

## No-fix-layering and architecture laws

| Defect class | Verdict | Evidence |
| --- | --- | --- |
| Hub close/shutdown | **PILED** | Normal shutdown cancels/join-waits the producer before `Swap(nil)` (`internal/gui/server.go:1200-1233`); the new restart path copies only the final swap/shutdown (`:933-934`). The partial second path does not preserve the original lifecycle invariant. |
| Pre-accept cleanup | **PILED** | Five inline cleanup branches after `Begin` have different error/resource handling (`internal/gui/gui_restart_protocol.go:171-216`). Consolidation into one state-aware cleanup owner is required. |
| Lease release | **CLEAN-SINGLE-OWNER** | Coordinator owns the two mutually exclusive release points; CLI supplies one `sync.Once` lease (`internal/gui/gui_restart_protocol.go:304`, `:368`; `internal/cli/gui.go:79-93`). |
| Async rollback restore | **CLEAN-SINGLE-OWNER** | Coordinator sequences termination/restore/marker decision; GUIListenerOwner and HandoffMarkerStore retain their resource/state ownership (`internal/gui/gui_restart_protocol.go:327-381`). |
| Endpoint gate/wire selection | **CLEAN-SINGLE-OWNER** | `guiSelfRestartHandler` alone selects v3 vs v1 from injected `Server.Config`; same-origin remains at route registration (`internal/gui/gui_self_restart.go:103-200`). |
| V3 process termination | **PILED** | CLI adds an exit wrapper but delegates the terminating primitive to a GUI process-global (`internal/cli/gui.go:102-119`; `internal/gui/gui_self_restart.go:203-214`). |

### D1 failure-idiom check

**Failed.** The reusable GUI package both returns errors/status through coordinator contracts and directly owns `os.Exit` for the new v3 path. The CLI composition root does not own the real terminal primitive. The retained v1 behavior is grandfathered by the gate-OFF requirement; Phase G must not expand that lower-layer idiom into v3.

### A6, C1, C2, D4, and abstraction-level summary

- A6 dependency direction: import direction is acyclic and GUI does not import CLI; however, v3 process adapter placement does not match the approved ownership boundary.
- C1 single owners: rollback, marker transitions, GUI listener mode, and lease release are coherent. Hub shutdown and pre-accept cleanup are split/duplicated.
- C2 config injection: passed for Phase G. Persisted port resolution and argv policy stay in CLI (`internal/cli/gui.go:133-201`); `Server.Config` receives the resolved gate (`:873-891`).
- D4 resources: failed for the hub-producer race, invalid retained-child path, ignored nonce/marker cleanup failures, and ignored successful-path detach error (`internal/gui/gui_restart_protocol.go:200-216`, `:304-308`).
- Right abstraction level: the coordinator is appropriately protocol-shaped, but Server lacks a reusable hub-lifecycle stop seam and the GUI package owns concrete v3 process policy that belongs at composition.

## Blast radius

- The diff is confined to the approved four production files and four adjacent test files.
- No GUI-to-CLI import was introduced; `go list` confirmed the existing downward CLI-to-GUI dependency.
- The Phase G production diff is large but matches the admitted phase. The blocking findings arise inside the admitted seams, not from unrelated-module churn.
- Same-port GUI-listener-only close and Unit A argv/port behavior remain passing. No frontend, deployment, gate-flip, commit, or publication change was reviewed.

## Verification evidence

```text
go test -tags=test_state_path_env -count=1 -timeout 10m -run '<G1-G7 and named guards>' ./internal/gui/
ok  	mcp-local-hub/internal/gui	0.294s

go test -tags=test_state_path_env -count=1 -timeout 10m -run '<G7/G8/gate-off/manager guards>' ./internal/cli/
ok  	mcp-local-hub/internal/cli	0.087s

go test -tags=test_state_path_env -count=1 -timeout 10m -run '^TestRestartV3_SamePort_ClosesOnlyGUIListenerAndKeepsHubEventsAlive$' ./internal/gui/
ok  	mcp-local-hub/internal/gui	0.033s

go test -tags=test_state_path_env -count=1 -timeout 10m -run '^TestRestartV3_PortArgvMatrix$' ./internal/cli/
ok  	mcp-local-hub/internal/cli	0.029s

git diff --check -- <eight changed Go files>
Exit code: 0
Output: <empty>

mcphub.exe sweep after the first test batch
mcphub.exe sweep: stopped=46 remaining=0

mcphub.exe sweep after the second test batch
mcphub.exe sweep: stopped=87 remaining=0

final mcphub.exe stabilization sweep (continuous two-second zero window)
mcphub.exe stabilization sweep: stopped=44 zero_ticks=20 remaining=0
```

No real GUI spawn was enabled; `MCPHUB_GUI_SPAWN_TESTS` was not set. No commit was created.

## Residual risk

- The 202 handler and the coordinator continuation have no explicit response-flush acknowledgement. The child startup normally provides elapsed time before exit, and reserved progress publication is nonblocking (`internal/gui/events.go:323-392`), but the current G1/G5 split probes do not engineer a continuation that reaches the exit seam before the HTTP handler returns. Add a deterministic handler/continuation ordering probe while correcting AR-G-1/AR-G-2.
- Windows retained-process behavior is not exercised with a real GUI by this unit gate, per the explicit constraint.
- Full `go build ./...`, `go vet ./...`, and full GUI/CLI suites were reported passing by the implementation package but were not re-run by this independent architecture lane; this lane ran the focused architectural guards above.

## Final gate

**REVISE**

The concrete release boolean, rollback split, exactly-once production lease wrapper, post-release protocol no-op boundary, endpoint wire shape, same-port rebind guard, Unit A guard, and gate-OFF v1 fallback are supported by source plus passing focused tests. The change cannot pass architecture review until the Server hub producer is made impossible to republish after restart close, pre-accept cleanup has one complete owner, and the v3 process-exit boundary is owned by CLI composition.

## Terms and Abbreviations

- AC: Acceptance Criterion.
- CLI: Command-Line Interface.
- D1: Failure-is-returned / composition-root termination law.
- D4: Resource-lifetime ownership law.
- GUI: Graphical User Interface.
- SSE: Server-Sent Events.

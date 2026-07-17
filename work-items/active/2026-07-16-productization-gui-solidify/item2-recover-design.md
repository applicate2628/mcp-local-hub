# Productization Phase-0 item 2 — GUI recovery design

## Evidence basis and current constraint

Dashboard's per-card **Restart** posts to `/api/servers/<server>/restart?daemon=<daemon>` and turns any non-2xx result into a thrown error that is only sent to `console.error`; the card then has only a transient `Failed` button label. `Dashboard.tsx:299-335`, `Dashboard.tsx:659-670`. That route delegates to `api.API.Restart`, which routes supervisor-owned rows through `DialSupervisorIPCRespawn(..., force=false, ...)`; a quarantined row is deliberately returned as an error with no intent write. `internal/gui/server.go:336-348`, `internal/api/restart_supervisor.go:151-190`.

The lower IPC client already carries a `force` argument, and `force=true` bypasses quarantine refusal. `internal/api/supervisor_ipc_respawn_client.go:92-104`, `internal/api/supervisor_ipc_respawn_client.go:161-168`. The present HTTP-only respawn endpoint forwards that boolean, but it does not inspect or reap a port owner. `internal/gui/daemon_env.go:385-420`. The CLI recover command does both: resolve the intent descriptor, inspect the effective port, reap only a verified-own disowned child, then request a force respawn. `internal/cli/daemon_recover.go:90-153`, `internal/cli/daemon_recover.go:156-245`. This matches the documented distinction that `POST /api/daemon/respawn {force:true}` is only the force-respawn step, without squatter reap. `CLAUDE.md:1467-1475`, `CLAUDE.md:1491-1500`.

## Change-Surface Contract

| Field | Contract |
|---|---|
| Intended change surface | New shared recovery operation; a same-origin `POST /api/daemon/recover` adapter; Dashboard card recovery UI; frontend API/status helpers and their tests. |
| Approved extension seams | Supervisor intent/state readers, `EffectiveDaemonPort`, context-aware port-owner probe, `HoldPIDForTermination` plus the held generation's `VerifyIdentity` / `Terminate` / `Close` lifecycle, the existing IPC `respawn` verb, `writeAPIErrorRedacted`, `ConfirmModal`, and Dashboard's per-card callback boundary. |
| Protected / must-not-touch surfaces | Supervisor IPC protocol and its `respawn` verb; automatic liveness sweep policy; legacy `/api/servers/.../restart` semantics; task identity and state-wire vocabulary; Milestone-1 hub-health-banner ownership in Dashboard header. |
| Declared blast radius | Behavioral shared-operation migration spanning the CLI recovery adapter, the held process-generation handle, the classifier/audit surface reused by automatic sweep policy, the same-origin GUI route, and the Dashboard UI. The classifier and proof construction are moved, not copied; automatic sweep retains its `TerminatePIDWithIdentity` adapter over the same process package. No daemon is spawned directly by GUI code. |

## A. `POST /api/daemon/recover`

### Owner and execution path

`internal/daemonrecovery` is the shipped reusable operation and classifier owner. It imports only `internal/api` and `internal/process`; both the CLI command and GUI adapter call it, while the automatic sweep retains thin policy adapters over the shared classifier/audit surfaces. This ownership avoids an import cycle because `internal/cli/gui.go` already imports `internal/gui`. `internal/daemonrecovery/recovery.go`, `internal/daemonrecovery/classifier.go`, `internal/cli/daemon_recover.go`, `internal/gui/daemon_recover.go`.

Move the shared port-owner classifier, fresh identity proof, bounded recover audit, and operator-recovery orchestration into that package. The CLI command remains a thin Cobra/TTY adapter and the sweep keeps its automatic-policy adapters; neither may retain a parallel classifier. The classifier accepts only a live non-self PID, excludes tracked siblings, requires a fresh identity read, checks the configured executable, and exact task argv; ambiguity returns unverified. `internal/daemonrecovery/classifier.go`.

Operator recovery must acquire `HoldPIDForTermination` before classification and retain that same generation through fresh argv classification, the final `VerifyIdentity(KillProof(identity))`, the context-aware port-owner boundary probe, and `Terminate`; `Close` runs on every exit. Reopening by PID through `TerminatePIDWithIdentity` after the argv read is insufficient here: PID reuse can place a different-argv process at the same executable path and within the ±1-second start-time tolerance between classification and termination. Holding the generation closes that PID-reuse/argv authorization hole without changing the automatic sweep adapter. `internal/process/pid_identity_windows.go`, `internal/daemonrecovery/recovery.go`.

`internal/gui/daemon_recover.go` owns only HTTP parsing, mapping, and redacted response writing; `registerDaemonRecoverRoutes(s)` is added beside the other route-registration calls in `NewServer`. `internal/gui/server.go:828-873`. Its production `recoverer` adapter invokes `daemonrecovery.Execute`; a narrow server interface permits handler tests without real process or IPC activity, as the existing restart adapter does. `internal/gui/server.go:320-348`.

### Request and success response

```json
POST /api/daemon/recover
{
  "task_name": "\\mcp-local-hub-memory-default",
  "confirm": true
}

200 OK
{
  "task_name": "\\mcp-local-hub-memory-default",
  "state": "respawn_accepted",
  "reaped": true,
  "port_owner_check": "reaped",
  "port_wait_outcome": "released"
}
```

`confirm` is positive-polarity and required before **any** recovery execution; absent or false returns `412 RECOVER_CONFIRMATION_REQUIRED` with no reap and no force respawn. The browser's confirm modal is the sole interactive confirmation. Unlike the CLI, the route must never read stdin or present a TTY prompt: the CLI's non-affirmative/EOF path returns refusal before both kill and respawn. `internal/cli/daemon_recover.go:226-228`, `internal/cli/daemon_recover.go:292-306`.

`state: "respawn_accepted"` means that the supervisor accepted the force-respawn request, not that the daemon is already Running; the CLI makes the same accepted-by-supervisor distinction. `internal/cli/daemon_recover.go`. `port_owner_check` is a safe enum only: `reaped`, `already_exited`, `termination_unconfirmed`, `unbound`, `tracked_child`, `port_unresolvable`, or `probe_unavailable`. `already_exited` reports that no reap occurred because the held generation was already dead; `termination_unconfirmed` reports that termination committed but its bounded exit wait failed, so no confirmed-reap claim is made. Both still require the mandatory respawn attempt. The enum never returns PID, command line, executable path, state-directory paths, or IPC addresses.

### Ordered operation and safety behavior

1. Normalize and validate `task_name`; reject blank and maintenance rows. The existing control route uses `NormalizeOverlayKey` and rejects maintenance task names before IPC. `internal/gui/daemon_env.go:398-416`.
2. Resolve the state directory, read `supervisor-intent.json`, and find the normalized descriptor. Unknown task stops here; it never reaches port or IPC operations. The CLI performs these checks before recovery work. `internal/cli/daemon_recover.go:94-121`.
3. Resolve the descriptor's effective port with `api.EffectiveDaemonPort`. A missing/renamed manifest or portless row reports `port_unresolvable` and continues to the force-respawn step, matching current CLI behavior; it does not invent a port. `internal/api/supervisor_port_owner.go:43-50`, `internal/cli/daemon_recover.go:164-175`.
4. Probe the port owner. Unbound port proceeds without a reap. A probe error is recorded as `probe_unavailable`, performs no kill, and follows the CLI's existing force-respawn path. `internal/cli/daemon_recover.go:177-183`.
5. Read tracked PIDs from `supervisor-state.json` and require the target task's row to be present before classifying a bound owner. An empty map or a map containing only other rows is not evidence that the observed PID is disowned: it returns `state_read_failed` with no terminate and no respawn. A present row whose current PID owns the port is a tracked child, not a squatter. A foreign or unverifiable owner emits the recover audit event and returns refusal. `internal/daemonrecovery/recovery.go`.
6. A bound port with a nonpositive/unreadable owner PID is unverifiable, never unbound. For a positive owner PID, acquire one held generation, classify exact executable + argv, confirm, verify the identity proof on the held handle, then run the final context-aware port-owner probe. At that same destructive boundary, re-read `supervisor-state.json` and re-apply the target current-child exclusion; a read error, missing target row, or newly tracked target fails closed with no terminate and no respawn. Only the same verified held generation may terminate. `internal/daemonrecovery/recovery.go`, `internal/process/pid_identity_windows.go`.
7. Before the point of no return, use the existing bounded supervisor status IPC call as a reachability signal and reserve a nonzero detached respawn slice. This probe is intentionally time-of-check/time-of-use (TOCTOU): it prevents the common already-dead-supervisor kill but cannot prevent the supervisor dying after the probe. The probe must fit within the configured nonreserved allowance, but elapsed identity/state work and the GUI request's remaining deadline do not reduce the detached reserve. The final request-context check immediately before `Terminate` remains the point of no return: cancellation before it stops with no terminate or respawn; cancellation racing after it can yield a committed kill. The post-kill clock starts immediately before `Terminate`; termination may consume the configured post-kill budget, and the port-release wait receives only `post-kill budget - respawn reserve - termination elapsed`. After that bounded wait, a fresh detached context receives the full configured respawn reserve. Process-already-exited and committed-but-unconfirmed results emit distinct audit events only after the respawn attempt is dispatched and never claim a confirmed reap. `internal/daemonrecovery/recovery.go`.
8. Send exactly one `DialSupervisorIPCRespawn(respawnCtx, task, true, ...)`; after a committed termination, `respawnCtx` is detached from request cancellation and owns a fresh full post-commit reserve regardless of elapsed pre-kill request time. No GUI-side spawn, scheduler restart, intent mutation, or raw PID kill is allowed. Force is what bypasses the quarantine gate and resets its failure window. `internal/daemonrecovery/recovery.go`, `internal/api/supervisor_ipc_respawn_client.go`.

Every refusal and reap remains attributable through the existing bounded supervisor-event shape with `source: "recover"` and actor, including a refusal that killed nothing. `internal/cli/daemon_recover.go:250-262`, `internal/cli/supervise_squatter.go:636-672`.

### HTTP outcome mapping and redaction

All internal/state/probe/IPC errors are logged server-side and serialized through `writeAPIErrorRedacted`, which returns only the fixed `"internal error"` and a stable code. `internal/gui/scan.go:101-124`. The handler must not forward `RespawnResult.Message`, process identity fields, or file/pipe paths: the existing respawn route documents that an unavailable-supervisor message can contain an absolute lock/socket path. `internal/gui/daemon_env.go:477-491`.

| Recovery result | HTTP / stable code | Wire body and UI meaning |
|---|---|---|
| CLI exit 2: unknown descriptor | `400 RECOVER_UNKNOWN_TASK` | Redacted envelope. The existing respawn endpoint also treats `UNKNOWN_TASK` as 400. `internal/gui/daemon_env.go:460-464` |
| CLI exit 3: observed foreign or unverifiable owner | `409 RECOVER_REFUSED_PORT_OWNER` | Redacted envelope; frontend maps the code to “Recovery was refused: the port owner could not be verified as this daemon's child. No process was stopped.” |
| CLI exit 4: supervisor rejects/fails force respawn | `500 RECOVER_RESPAWN_FAILED` | Redacted envelope; frontend retains “The supervisor did not accept recovery. View logs and retry after resolving the failure.” |
| CLI exit 5: no reachable supervisor or IPC transport failure | `503 RECOVER_SUPERVISOR_UNAVAILABLE` | Redacted envelope; frontend retains “The supervisor is unavailable. Restart the hub, then retry recovery.” |
| CLI exit 6: transient local timing refused before termination because the detached respawn reserve could not be preserved | `503 RECOVER_RESPAWN_BUDGET_INSUFFICIENT` | Distinct redacted envelope; frontend states that no process was stopped and asks the operator to retry when the local system is less busy. This is not a state-read failure or a supervisor outage. |
| Request canceled before a termination commit | `408 RECOVER_REQUEST_CANCELED` | Redacted envelope; frontend states that no process was stopped and allows an explicit retry. |
| Final port-owner boundary probe exceeded the request deadline after identity verification | `504 RECOVER_BOUNDARY_PROBE_TIMEOUT` | Redacted envelope; frontend says identity verified but the final owner recheck timed out, so no process was stopped. |
| Intent/state-dir read failure, malformed input, or confirmation absent | `500 RECOVER_STATE_READ_FAILED`, `400 INVALID_ARGS`, or `412 RECOVER_CONFIRMATION_REQUIRED` | Stable redacted/validation envelope; none reaches a kill or force respawn. |

The normal CLI mapping includes intent-read failure in exit 2, but the HTTP contract intentionally distinguishes it from an unknown task: calling a state/permission failure “unknown” would be false and less actionable. A timing refusal uses its own failure kind, HTTP code, and CLI exit 6. The CLI's published exit code meanings are documented in `CLAUDE.md` under Stuck-instance recovery, and the adapter mapping is owned by `internal/cli/daemon_recover.go`.

## B. Dashboard surface

`DaemonStatus.task_name` is the canonical routing identity supplied by `/api/status`; the frontend must use it rather than reconstructing a task name from server/daemon labels. `internal/api/types.go:17-29`, `internal/gui/frontend/src/types.ts:50-57`.

For a current `Quarantined` card, replace ordinary per-card Restart with:

- An inline `role="status"` reason: “Automatic restart is paused because this daemon is quarantined after repeated failures. Recover checks for a lost child on its port, may stop only a verified hub child, then requests a forced respawn.”
- A first-class **Recover** action, visible only for the recovery-eligible state and only when non-empty `task_name` is present. It replaces—not supplements—the known-refused Restart action. This removes the quarantined path from Dashboard's current console-only failure flow. `internal/gui/frontend/src/screens/Dashboard.tsx:299-335`, `internal/api/restart_supervisor.go:161-190`.
- Reuse `ConfirmModal`, whose native dialog already supplies modal semantics, focus trapping, Escape cancellation, and a busy state. `internal/gui/frontend/src/components/ConfirmModal.tsx:3-10`, `internal/gui/frontend/src/components/ConfirmModal.tsx:32-95`.

The modal title is **Recover <daemon display name>?**. Its body says: “Recovery checks the daemon's configured port. If it is occupied, mcphub will stop the process only after it proves that it is this daemon's own lost child. It will never stop a foreign or unverifiable process. Then it asks the supervisor to force a respawn.” The sole positive action is **Recover daemon**; Cancel sends no request. Confirm posts `confirm:true`.

After a 200, keep the card in its reported state, disable Recover while the request is in flight, show “Recovery accepted; waiting for supervisor status,” and trigger an immediate `/api/status` refresh. Do not optimistically paint it Running. On failure, retain a code-mapped inline `role="alert"` message until the next successful recovery or a status change clears it; do not render raw backend errors. This gives the operator feedback that outlives the current 1.5-second action-label flash. `internal/gui/frontend/src/screens/Dashboard.tsx:541-571`, `internal/gui/frontend/src/screens/Dashboard.tsx:659-723`.

There is no distinct `LostChild` value in the current `/api/status` wire shape: the existing status state is a string, while `orphan_pid` is a different Windows post-create diagnostic and not proof of a port squatter. `internal/gui/frontend/src/types.ts:5-60`, `internal/api/types.go:36-50`. Therefore `isRecoveryEligibleState` is exactly `Quarantined` in this item. If a future backend adds a canonical `LostChild` state, the helper may admit that exact value; it must not infer it from `orphan_pid`, PID, or an unknown state. The recovery operation itself detects the lost-child condition at execution time.

Milestone-1 compatibility: apply this item after PR #555 and limit the Dashboard edit to the card callback/card subtree plus imports. Do not move, replace, or own header/banner markup: current header and recovery controls are composed separately from cards. `internal/gui/frontend/src/screens/Dashboard.tsx:437-477`.

## C. Truthful state colors

`status.ts` already groups the canonical presentation vocabulary for shapes: Running; Partial; recovering (`Starting`, `Restarting`, `Backoff`, `Spawning`); terminal (`Failed`, `Quarantined`); idle (`Ready`, `Scheduled`, `Stopped`); and unknown fallback. `internal/gui/frontend/src/lib/status.ts:16-55`. Despite that grouping, Dashboard independently assigns green only to `Running` and red to every other state for both card and badge. `internal/gui/frontend/src/screens/Dashboard.tsx:589-603`.

Add one exported `daemonStateVisual(state)` in `status.ts`, derived from the same bucket classifier used by `stateShape`; Dashboard consumes its card and badge classes rather than testing `d.state === "Running"` itself. The glyph remains supplementary accessibility information, as today. `internal/gui/frontend/src/lib/status.ts:3-31`.

| Exact state bucket | Current visual color | Proposed visual color |
|---|---|---|
| `Running` | Green | Green — healthy and fully up |
| `Partial` | Red | Amber — mixed multi-daemon state needs attention, but is not one daemon's terminal failure |
| `Starting`, `Restarting`, `Backoff`, `Spawning` | Red | Amber — supervisor is actively recovering |
| `Quarantined` | Red | Amber/orange — recovery-needed state with the Recover CTA |
| `Failed` | Red | Red — terminal failure |
| `Ready`, `Scheduled`, `Stopped`, `Idle` | Red or fallback red | Grey — intentionally/not currently running |
| Unknown or blank | Red | Neutral grey — current shape policy treats unknown vocabulary as non-failure rather than borrowing the error glyph. `internal/gui/frontend/src/lib/status.ts:50-54` |

This separates Quarantined from Failed only at the frontend visual/CTA layer; it does not alter supervisor or health semantics. The backend explicitly continues to classify both as failure for `/api/health`, while recovery states are `starting` and idle states `stopped`. `internal/api/daemon_state.go:217-233`.

## D. Test plan

| Layer | Cases and assertions |
|---|---|
| Shared Go recovery operation | Unknown descriptor performs no probe/terminate/respawn; empty or target-missing state maps fail `state_read_failed`; boundary state re-read refuses a newly tracked current child with no terminate/respawn; bound+nonpositive PID refuses; foreign/unverifiable or duplicate-discriminator owners perform no terminate or respawn and audit refusal; supervisor-unreachable and runtime/static insufficient-reservation cases refuse before terminate; permitted GUI identity-plus-probe latency still reaps and receives a full detached reserve; already-exited and committed-but-unconfirmed outcomes emit no reaped event; a held audit flock cannot preempt respawn dispatch after a committed termination; cancellation after the point of no return uses the detached reserved respawn context; the port wait cannot consume that reservation; and adapter notification cannot preempt respawn. `internal/daemonrecovery/recovery_test.go` |
| Windows held-generation primitive | Focused injected-OS tests cover exactly-once handle close on success, verification failure, terminate failure, already-exited, wait error, wait timeout, and unexpected wait result; identity mismatch and already-exited sentinels; ACCESS_DENIED live-vs-exited behavior; and sentinel preservation by `TerminatePIDWithIdentity` orchestration without spawning a helper process. `internal/process/pid_identity_windows_test.go` |
| GUI route | Same-origin POST only; missing confirmation is 412 with no recovery invocation; unknown is 400; fake foreign/refused result is 409 with `RECOVER_REFUSED_PORT_OWNER`; success is 200 with safe fields and confirms `confirm:true`; supervisor unavailable and respawn-budget-insufficient are distinct 503 codes; path-bearing errors go through `writeAPIErrorRedacted` and never appear in JSON. Existing respawn tests establish the handler/503 and no-path test pattern. `internal/gui/daemon_env_test.go:194-247`, `internal/gui/scan_test.go:20-47` |
| Dashboard unit/e2e | Quarantined status renders the inline explanation and Recover rather than Restart; Cancel sends no recover request; Confirm uses the exact `task_name` and `confirm:true`; a 200 retains “waiting for supervisor status” until refreshed; 409/500/503/504 outcomes retain code-mapped inline alerts without raw server text, including distinct local-budget and supervisor-unavailable copy; non-quarantined cards keep existing Restart/Stop behavior. Preserve per-daemon targeting tests for multi-daemon cards. `internal/gui/frontend/src/screens/Dashboard.test.tsx`, `internal/gui/frontend/src/screens/Dashboard.tsx` |
| Status visual unit tests | Add table-driven expectations for every bucket above, including `Idle` and unknown fallback; assert Dashboard receives visual classes only from `daemonStateVisual`; retain existing shape-bucket assertions. `internal/gui/frontend/src/lib/status.test.ts:78-125` |

## E. Scope, alternatives, and risks

The chosen design extracts the existing recovery authority into a shared internal operation, adds an HTTP adapter, and gives the Dashboard an explicit state-driven recovery flow. It is smaller and safer than copying CLI code into GUI, which would diverge at the classifier, fresh proof, audit, or port-free boundary. It is also safer than making Dashboard call `/api/daemon/respawn {force:true}` directly: that endpoint intentionally omits the verified-own squatter reap. `internal/cli/daemon_recover.go:56-60`, `CLAUDE.md:1499-1500`.

A per-task recovery lease remains a follow-up. The identity gate constrains a possible victim to our binary with our exact argv naming the recovered task. The bound is never a stranger, and there is no loss beyond what hard-restarting the target daemon itself entails: that daemon's in-flight requests and writes can be severed. The destructive-boundary state re-read reduces the stale tracked-child verdict from an operator-confirmation interval to the controller event loop's measured approximately 1.5-second persistence lag; it does not claim that `CurrentPID` is persisted before bind. The follow-up remains necessary to serialize duplicate recovery requests completely. `internal/daemonrecovery/recovery.go`, `internal/cli/supervise.go`.

Rejected alternatives:

- **Force the existing `/api/servers/.../restart` path.** Rejected because its supervisor branch intentionally uses `force=false` and preserves the quarantine force gate. `internal/api/restart_supervisor.go:161-190`.
- **Expose a raw PID/process description to the browser for approval.** Rejected because executable path and command line are attacker-controlled in the foreign-owner case; the CLI sanitizes them only for TTY output. The GUI needs only a stable no-kill reason. `internal/cli/daemon_recover.go:192-211`, `internal/cli/daemon_recover_test.go:334-382`.
- **Infer “lost child” from a card PID/orphan PID.** Rejected because those fields do not prove port ownership or verified identity. The recovery owner performs that proof at the destructive boundary. `internal/api/types.go:36-50`, `internal/cli/supervise_squatter.go:102-173`.

Expected file set: new `internal/daemonrecovery/` operation plus tests; thin edits to `internal/cli/daemon_recover.go` and its shared squatter helpers without changing automatic restart/reap policy; new `internal/gui/daemon_recover.go` plus route tests; one registration call in `internal/gui/server.go`; `frontend/src/api.ts`, `lib/status.ts`, `Dashboard.tsx`, and focused tests. Surfacing committed-but-unconfirmed termination from the shared automatic-path primitive remains a separately reviewed follow-up. Out of scope: new supervisor IPC verbs, changing automatic sweep restart/reap policy, changing legacy restart semantics, creating a new status-wire `LostChild` value, killing foreign processes, and any Milestone-1 banner changes.

## Claims

1. `{ guarantee: Only the shared recovery operation can authorize a port-owner reap for CLI or GUI recovery, and it requires a present target state row at both classification and the destructive boundary plus one held process generation across argv classification and termination; single-owner: internal/daemonrecovery; enforcement-probe: shared-operation tests prove empty/missing target rows, a newly tracked current child, bound nonpositive PID, foreign/unverified owner, and pre-boundary cancellation each cause zero terminate and zero respawn calls. }`
2. `{ guarantee: GUI recovery never authorizes a kill without confirmed operator intent and held-generation identity, and after any committed termination the respawn attempt is dispatched before blocking audit emission or adapter notification; a termination whose exit wait fails is reported as unconfirmed rather than reaped; single-owner: POST /api/daemon/recover handler plus HoldPIDForTermination lifecycle and post-commit queue in internal/daemonrecovery; enforcement-probe: route confirm:false test, held-generation close/sentinel tests, different-argv and start-proof mismatch tests, cancellation-during-boundary-probe test, committed-termination wait-error test, and held-audit-flock test. }`
3. `{ guarantee: A 200 means force-respawn accepted, not daemon Running; single-owner: recover response contract; enforcement-probe: Dashboard test retains pending status until /api/status returns a new state. }`
4. `{ guarantee: Every Dashboard status color derives from one status.ts bucket classifier; single-owner: daemonStateVisual; enforcement-probe: status.ts table test and Dashboard source test remove the local Running-vs-other color ternary. }`
5. `{ guarantee: Dashboard does not offer the known-refused normal Restart action for a quarantined daemon; single-owner: Card recovery eligibility helper; enforcement-probe: Quarantined-card test finds Recover and no Restart. }`

## Open question — RESOLVED 2026-07-16 ($lead)

> Should Productization define and emit a separate canonical `LostChild` status, or is the current
> `Quarantined` state the only intended GUI entry point for the lost-child recovery class?

**Decision: NO separate `LostChild` wire state in this item. `Quarantined` is the GUI entry point.**

Rationale:

1. **The condition is proven at execution time, not at classification time.** The recovery operation
   establishes port ownership + verified identity at the destructive boundary; that proof cannot be
   hoisted into a status field the poller writes seconds earlier. A pre-classified `LostChild` would be
   a *prediction*, and the operation would still have to re-prove it.
2. **The only available inference sources are lies.** `orphan_pid` is a Windows post-create diagnostic,
   not evidence of a port squatter; PID/state do not prove ownership either. Inferring `LostChild` from
   them is exactly the dishonest-status class this Phase-0 exists to eliminate — the same mistake as
   painting a dead hub green.
3. **A new wire enum is a cross-cutting change with no user-facing gain here.** It would ripple through
   the Go status type, the TS union, every consumer switch, and the embedded bundle, for a value the
   operator does not act on differently: a quarantined daemon is what they see and what they want
   recovered, whatever the underlying cause.
4. **Nothing is foreclosed.** `isRecoveryEligibleState` admits `Quarantined` today and is written so a
   future canonical backend state can be added by EXACT value. It must never widen by inference from
   `orphan_pid`, PID, or an unknown/unmodelled state — the same "present-but-unknown ⇒ generic, never
   invent a story" rule the hub-health reason classifier landed on in item 1.

If a later item makes the supervisor emit a canonical lost-child state (i.e. the backend itself proves
it), admitting it is a one-line widening of the helper plus its table test.

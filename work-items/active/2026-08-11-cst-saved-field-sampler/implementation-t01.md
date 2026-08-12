# T01 Backend Implementation — Supervisor CST Identity Status

Gate: **PASS** — strict RED/GREEN, the post-T01 full-suite receipt, and a bounded affected-test differential prove T01 is a non-regression. The only reproducible full-suite failures are the same pre-existing stale routing anchors classified in T00; no T01-affected test fails.

Execution role: `$backend-engineer` under the main-conversation `$lead`.
Scope: T01 only. Baseline: `5ff268dc13b2be9ca9500b5441634f0594538b94`.

## Receiving-side echo

Accepted immutable inputs were re-read and matched: design `AFABC3C001169D5C571D7319EA2C751CDD228E46B335C9630C0516F6EBAE6DC9`, decision `18307E933D393BBD0C6B0396F47FE6AAFB0C5AE94CE39E395F8EE948371BE92A`, architecture review `18499E40CC82236F9EA256F988BB7F48342806240A1EC710E7739978BCF7601E`, security constraints `A0F0D2CEF3BA016D4E4E607755D643F5415F2D56F2C9BF99E848481498A81A12`, security design review `BFC9A0F36F7FF0E07ADE7E4DC79D507FBB1BBBDCD548713B263F7BC3FF14B84A`, and plan `8DD78E5B6EC48ED7671403C98695B4F364C8E13430C0FE49396E426B76CAE3EA`. T00 was accepted PASS. CodeGraph returned current source without a stale/disabled banner. Unrelated dirty paths and the adjacent routing bug record were preserved.

## Owner invariant and change surface

The existing supervisor IPC owns one pre-generic-dispatch capability: only an exact kernel-observed client whose PID equals the fixed `McpLocalHubCstDaemon` Service Control Manager PID, token user equals its runtime-resolved numeric service SID, session is 0, integrity is High or System, and process image equals the service-configured image may invoke `GET_CURRENT_CST_TASK_IDENTITY_V1`. The request has no task selector and returns only `{task,pid,pid_generation,creation_time}` for the unique current `cst` tracker row. The same service identity is denied every generic opcode before generic dispatch. Request IDs are consumed on success; task state is read-only.

| Owner | Implemented surface |
|---|---|
| Closed contract/authorizer | `internal/api/supervisor_cst_identity.go`; exact request, peer/policy, response and replay owner. |
| Kernel-bound client | `internal/api/supervisor_cst_identity_client.go`, `supervisor_status_server_identity_{windows,other}.go`; server pipe PID, creation time, token user/session and canonical image are checked before the request. |
| Lock generation | `internal/api/supervisor_lock.go`; owner sidecar now records kernel process creation time when available. |
| Server peer proof/pre-dispatch | `internal/cli/supervise_cst_identity.go`, platform adapters, and `internal/cli/supervise.go`; Windows derives client PID, fixed SCM PID/configured image and token facts before the status-only branch. |
| Descriptor | `internal/api/dacl_shared_windows.go` and `internal/cli/supervise_ipc_windows.go`; runtime-resolved numeric daemon SID is the sole added client ACE after provisioning, with High no-write-up label; absent service retains the prior default-off descriptor and the opcode fails closed. Live readback includes owner/DACL/label. |

No CST, SCM registration, service, hub, fleet, deployment, Git index, commit, push, or live process state was mutated.

## RED/GREEN receipts

| Stage | Command | Receipt |
|---|---|---|
| RED | `go test ./internal/api -run 'TestSupervisorCstIdentity|TestSupervisorStatusAuthorization' -count=1` | Exit 1, build failed on the absent policy, peer proof, response, opcode, authorizer and kernel validator symbols. |
| GREEN focused | same command | Exit 0; `ok mcp-local-hub/internal/api 0.059s`. |
| GREEN actual dispatch | `go test ./internal/cli -run 'TestSupervisorCstIdentity|TestDispatchIPC|TestSuperviseIPC' -count=1` | Exit 0; `ok mcp-local-hub/internal/cli 0.319s`. Deny matrix, replay and before/after tracker JSON equality passed. |
| Windows static | `GOOS=windows GOARCH=amd64 go test ./internal/api ./internal/cli -run '^$' -count=1` | Exit 0 for both packages; Windows production adapters compile. |
| Static | `go vet ./internal/api ./internal/cli` | Exit 0, no output. |
| Diff hygiene | `git diff --check` | Exit 0. |
| Broad | `go test ./... -count=1` | Post-T01 run completed exit 1 in 423s. `internal/api` passed in 270.807s. The same `internal/api/lsp_routing` and `internal/api/serena_routing` stale literal-anchor tests failed as classified against immutable HEAD in T00. `internal/cli` package reported FAIL in truncated aggregate output, so its exact T01-affected surface was rerun rather than inferring cleanliness. |
| Differential acceptance | `go test ./internal/api ./internal/cli -run '^(...)$' -count=1 -timeout 4m`, with the exact anchored alternation listed below | Exit 0 in 6.3s: `internal/api` passed in 0.055s; `internal/cli` passed in 2.790s. The set covers all new T01 tests, every directly affected dispatcher/status/readiness/control/version test, lock/startup ownership, Windows listener DACL/handshake, and the previously broad-flaky `TestSuperviseCommand_SweepsOldBinariesOnStartup`. |

Exact differential set: `TestSupervisorCstIdentityAuthorizationMatrix`, `TestSupervisorCstIdentityReplayDenied`, `TestSupervisorStatusAuthorizationKernelBinding`, `TestSupervisorCstIdentityPreDispatchAndStateImmutability`, `TestIPCStatusReadsRuntimeTracker`, `TestSuperviseCommand_AcquiresLockAndExitsOnSignal`, `TestSuperviseCommand_RefusesSecondInstance`, `TestSuperviseCommand_LockLoserSkipsOldBinarySweep`, `TestSuperviseCommand_StatusIPC_ReconcileReady`, `TestSuperviseCommand_StatusIPC_UnknownCommand`, `TestSuperviseCommand_SweepsOldBinariesOnStartup`, `TestSupervisorIPCRefusesMutatingCommandsBeforeReady`, `TestSupervise_IPC_QuiesceTimersTwoFrames`, `TestSupervise_IPC_QuiesceTimersDrainTimeout`, `TestSupervise_IPC_ExitGracefulInitiates`, `TestSupervise_IPC_VersionPinning`, `TestSuperviseIPC_ListenerDACL`, `TestSuperviseIPC_HandshakeSent`, and `TestSupervisorLockOwnerHelloConsistency`.

## AC state and rollback

| Criterion | State |
|---|---|
| T01-AC01 | GREEN in focused allow/deny matrix and production pre-dispatch branch. |
| T01-AC02 | GREEN by Windows compile plus pure wrong PID/time/SID/session/token/image falsifiers; live provisioned-service descriptor smoke remains a later target/provisioning gate. |
| T01-AC03 | GREEN in actual pre-dispatch test; denied generic/explicit-target/replay cases preserve byte-identical tracker snapshot. Malformed JSON remains rejected before dispatch by the existing decoder. |
| T01-AC04 | GREEN for exact closed response and missing/ambiguous/stale-generation/mismatch matrix. |
| T01-AC05 | PASS by post-T01 full receipt plus exact affected-test differential. Existing opcode/status/listener/lock contracts pass; full-suite failures match the independently classified T00 stale-anchor baseline and are outside T01. |

Rollback is one coherent T01 group: remove the new supervisor CST identity API/client/platform files and CLI authorizer/platform/test files, then revert only the T01 hunks in `supervisor_lock.go`, `dacl_shared_windows.go`, `supervise.go`, and `supervise_ipc_windows.go`. Do not touch unrelated dirty paths.

## Terms and Abbreviations

- CST: Computer Simulation Technology.
- IPC: inter-process communication.
- SCM: Windows Service Control Manager.
- SID: Windows security identifier.
- DACL/SACL: discretionary/system access-control list.
- RED/GREEN: failing test before implementation / passing test after implementation.

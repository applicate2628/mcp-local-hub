# T02 Backend Implementation — StdioHost Launch Capability

Gate: **PASS** — strict RED/GREEN and the bounded affected surface prove the T02 Go launch owner, enrollment client and CLI composition.

Execution role: `$backend-engineer` under `$lead`. Scope: T02 only. Baseline: `5ff268dc13b2be9ca9500b5441634f0594538b94`.

## Owner invariant

Only the exact Windows `cst/default` supervisor-tracked host receives a launch-capability requirement. `StdioHost.Start` generates 32 bytes with `BCryptGenRandom(..., BCRYPT_USE_SYSTEM_PREFERRED_RNG)`, enrolls only their SHA-256 digest before spawn, writes exact bytes then EOF to an anonymous pipe, and exposes only its decimal read-handle locator. The read handle is the sole additional inheritable handle; the write handle is non-inheritable. Every local buffer is cleared through `RtlSecureZeroMemory`; enrollment is cancelled on failed start and on child exit. An unavailable enrollment endpoint degrades to no capability, preserving the existing six tools.

| Owner | Surface |
|---|---|
| Closed enrollment contract | `internal/api/hub_enrollment_client.go` plus Windows/non-Windows clients. Closed 4096-byte frames, challenge, correlation, digest, state and terminal receipt gates. |
| Handle/capability lifecycle | `internal/daemon/launch_capability.go` plus platform adapters. CNG generation, exact pipe write/EOF, explicit inherited read handle, decimal locator, close/zero/cancel. |
| Actual spawn owner | `internal/daemon/host.go`. Enrollment occurs after stdio pipe creation and before `cmd.Start`; locator is stripped from ambient/manifest environment and appended only after enrollment. Cancellation is owned by the process watcher, not a stream reader. |
| Composition | `internal/cli/daemon.go`. Exact Windows `cst/default` row must match current PID, positive generation and start identity before enabling the client; all other launches receive nil configuration. |

The T02 client owns `ISSUED -> ENROLLED` and cancellation. Server-observed `CONSUMED` after child exact-32-plus-EOF intake and frontend challenge belongs to serial T04/T05 and is not self-attested by this phase.

## RED/GREEN receipts

| Stage | Command | Receipt |
|---|---|---|
| RED | `go test ./internal/daemon ./internal/api -run 'Test.*LaunchCapability|Test.*EnrollmentClient|Test.*HandleList' -count=1` | Exit 1: build failed on absent enrollment-frame/client and launch-capability lifecycle symbols. |
| GREEN focused | same command | Exit 0: `internal/daemon` 0.053s; `internal/api` 0.087s. |
| GREEN CLI differential | `go test ./internal/cli -run 'TestSupervisorCstIdentity|TestSuperviseCommand_SweepsOldBinariesOnStartup' -count=1 -timeout 4m` | Exit 0: `internal/cli` 0.217s. |
| Diff hygiene | `git diff --check -- <T02 production paths>` | Exit 0. |

No full repository run was started: Lead bounded final acceptance to focused/static evidence and the known T00/T01 differential baseline.

## AC and rollback

| Criterion | State |
|---|---|
| T02-AC01 | PASS: actual `StdioHost` owner calls injected/tested production CNG and enroll-before-start path. |
| T02-AC02 | PASS for T02-owned issue/enroll/cancel paths; consume remains explicitly owned by T04/T05 server/frontend phases. |
| T02-AC03 | PASS: Go Windows `AdditionalInheritedHandles` contains only capability-read; os/exec owns its existing stdin/stdout/stderr handle list. Write is explicitly non-inheritable. |
| T02-AC04 | PASS for Go producer: decimal locator only, exact write+EOF, parent closes, and secure zero. Child intake is the T05 consumer boundary. |
| T02-AC05 | PASS in focused host/API and CLI differential; nil configuration preserves every non-CST path. |

Rollback: remove the new T02 enrollment/capability files and revert only T02 hunks in `internal/daemon/host.go` and `internal/cli/daemon.go`.

## Terms and Abbreviations

- CNG: Windows Cryptography Next Generation.
- CST: Computer Simulation Technology.
- EOF: end of file.
- SCM: Windows Service Control Manager.

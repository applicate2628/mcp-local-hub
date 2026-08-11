# Implementation F — deterministic GUI test lifecycle owners

## Summary

This phase changes only two GUI test files. Eight daemon-recovery handler fixtures now use the existing `newEphemeralServer` lifecycle owner, so disk-persistence workers are disabled before route exposure and joined by test cleanup. The named owner guard parses the Go abstract syntax tree (AST) of the eight actual participant functions and requires each to call that owner exactly once with no direct `NewServer` bypass. The audit-lock containment test now cancels only after its helper marker proves that the child owns the occurrence flock; deadline classification is tested separately by an in-process runner.

No production source, route behavior, event schema, console composition, installer, or command-line grammar changed. The accepted Windows console baseline was therefore not rerun.

## Receiving-side echo

| Contract field | Received and applied |
| --- | --- |
| Role / goal | Backend implementer for exactly two test-lifecycle owners: daemon-recovery broadcaster ownership and deterministic audit-lock synchronization. |
| Approved inputs | Active R2 status/brief/roadmap; accepted `reliability.md` SHA-256 `8ED29F7B7B0F98F8769F8F7C37C19BDEF5596E8DD3AFBFF60726A3E25B1852AC`; archived 6/6 top-level and 63/63 Windows console baseline. |
| Scope | Test-only edits in `internal/gui/daemon_recover_test.go` and `internal/gui/audit_lock_terminal_worker_test.go`; RED first; exact, route-family, race, broad GUI, vet, format, and diff gates. |
| Out of scope | Production files; CLI/Reconcile/EventLoop; process production; console, installer, npm, live fleet; Git commit/push; pull requests; lifecycle/status/ledger mutation. |
| Must not break | Route response/status/event assertions, broadcaster production semantics and package leak oracle, audit protocol/failure IDs/receipt classification/flock/store mutex/containment cleanup, exact leading `--debug-console`, and user-owned dirty files. |
| Terminal handoff | PASS returns to Lead for independent mechanical verification. CLI adjacent failures, native Linux/macOS, final QA, publication, install, and live hub proof remain open. |

## TDD break statements

| Guard | Concrete break caught |
| --- | --- |
| `TestDaemonRecoverHandlers_OwnBroadcasterLifecycle` | Bypassing `newEphemeralServer` in any of the eight named participant functions makes the AST inventory fail even when the route currently publishes no event. A regression in the helper itself remains behaviorally covered by the four publishing participants and package worker oracle. |
| `TestAuditLockTerminalWorkerCancellationAfterAcquisitionReapsBeforeReturn` | Returning from containment before cancellation has killed and joined the acquired-lock helper leaves a live PID, held flock, or held store mutex. Cancelling before the marker makes acquisition unproved. |
| `TestAuditLockTerminalizationDeadlineClassifiesWithoutProcessStartup` | Skipping the injected runner, not waiting for `ctx.Done()`, or no longer mapping `StrictRunTimeout` changes the stable failure ID, receipt uncertainty, or mutex release. |

## Exact RED receipts

| Guard | Command | Expected and observed RED | Receipt |
| --- | --- | --- | --- |
| Broadcaster owner | `go test -count=1 -timeout 2m -run '^TestDaemonRecoverHandlers_OwnBroadcasterLifecycle$' -v ./internal/gui` | Exit 1. `unowned_sites=8 drainPersist=9 runDropReporter=0`; package oracle repeated `TEST_BROADCASTER_LIFECYCLE_LEAK drainPersist=9 runDropReporter=0`. | `.scratch/windows-console-contract/r2-backend/red-daemon-handler-lifecycle.txt`; SHA-256 `E54F691F8DD2A05790A4FEB06A48D97B0C376EEEE1BFEFCA63719072A4E49240`. |
| Acquired-lock cancellation | `go test -count=1 -timeout 2m -run '^TestAuditLockTerminalWorkerCancellationAfterAcquisitionReapsBeforeReturn$' -v ./internal/gui` | Exit 1. `contained helper never acquired occurrence flock`; the old common deadline expired before acquisition. | `.scratch/windows-console-contract/r2-backend/red-audit-acquisition-reap.txt`; SHA-256 `AC13D7EB711A61548A6F8B3D11FFD397C20FF93C08C9849EEE34ECEAD33EA587`. |
| In-process deadline classification | `go test -count=1 -timeout 2m -run '^TestAuditLockTerminalizationDeadlineClassifiesWithoutProcessStartup$' -v ./internal/gui` | Exit 1. Observed `DAEMON_RECOVERY_TERMINAL_WORKER_PROTOCOL_INVALID`; expected `DAEMON_RECOVERY_TERMINAL_WORKER_TIMEOUT`. | `.scratch/windows-console-contract/r2-backend/red-audit-deadline-classification.txt`; SHA-256 `A4193937F588FB4018985240E5405741F00A1E1A1CED57F57611A8269BDF314D`. |

No named guard passed on its first RED run.

### REVISE iteration 1 — guard-quality falsifier and mutation proof

The first runtime guard constructed a detached synthetic server set. It proved the `9 -> 0` worker count but did not enforce ownership in the eight actual participant functions. The review falsifier was reproduced before correction and the source was restored by exact hash, not by a broad Git operation.

| Probe | Expected / observed | Receipt |
| --- | --- | --- |
| Old guard with actual same-origin participant temporarily changed from `newEphemeralServer` to `NewServer` | A valid causal guard would fail. Observed exit 0: old guard and package oracle stayed GREEN, confirming the guard-quality defect. | `.scratch/windows-console-contract/r2-backend/revise1-old-guard-latent-bypass-pass.txt`; SHA-256 `6A2107B85E86E9EDD8CC4B18A2D86D6BE2A11F4BF8784EC360F3D14ECE4F265A`. |
| Restore after old-guard falsifier | `daemon_recover_test.go` SHA-256 before/after both `1182017DFFD177BD62443EF2AFA677D3C5DF57B5BE591184BFE86F5E220320AA`. | Current-session hash receipt in the implementation transcript. |
| New AST guard with the same actual participant bypass | Exit 1. Exact causal failure: `TestDaemonRecoverRouteRequiresSameOriginPOST: owner_calls=0 bypass_calls=1, want owner_calls=1 bypass_calls=0`. | `.scratch/windows-console-contract/r2-backend/revise1-new-guard-latent-bypass-red.txt`; SHA-256 `7D61C7D5CA76E095EE7028B92CA3655765F53B6FEBA0FAB81FC563574D2633FA`. |
| Restore after new-guard mutation | Structural candidate SHA-256 before/after both `69FD19747EF47F8755CD63F384E0109736AACB4C8241B2A4501ED27947B91AE3`. | Current-session hash receipt in the implementation transcript. |

## Change inventory

### Daemon-recovery handler fixtures

The eight accepted unowned constructors now call `newEphemeralServer` at current `daemon_recover_test.go:319,345,372,475,504,611,641,698`. The three already-owned direct constructors remain direct at `:152,181,557` with their explicit event cleanup unchanged.

`TestDaemonRecoverHandlers_OwnBroadcasterLifecycle` owns the structural participant registry at `daemon_recover_test.go:54-63`. It parses Go syntax rather than raw text or line offsets, finds each named function declaration, and requires exactly one `newEphemeralServer` call plus zero direct `NewServer` calls in its complete body. This structural test is a narrow, reviewer-approved exception to the normal preference for behavior-only tests because the invariant being enforced is test-fixture ownership at eight named construction sites, including four routes that currently do not publish.

| Participant | Instances / prior workers | Classification |
| --- | ---: | --- |
| Committed error matrix | 5 / 5 | Fixed: existing HTTP/status/receipt assertions unchanged; constructor now lifecycle-owned. |
| Success safe fields | 1 / 1 | Fixed: safe response-field assertions unchanged. |
| Release unconfirmed | 1 / 1 | Fixed: warning semantics unchanged. |
| Respawn failure matrix | 2 / 2 | Fixed: redacted failure and committed-termination assertions unchanged. |
| Same-origin rejection | 1 / 0 | Fixed latent owner: still no current persistence worker; future publication cannot leak. |
| Explicit confirmation | 2 / 0 | Fixed latent owner: precondition response/call-count behavior unchanged. |
| Known-contract matrix | 11 / 0 in accepted targeted probe | Fixed latent owner: status/code/redaction matrix unchanged. |
| Durable handoff | 1 / 0 | Fixed latent owner: no-warning behavior unchanged. |
| Pre/post-commit panic and reserve-post-rename | 3 / 0 after explicit cleanup | Not affected; already owned and deliberately left unchanged. |
| Drop reporter / `Broadcaster.Close` / package oracle | 0 / 0 | Not affected; no production or oracle edit. |

Observed owner correction is exactly `drainPersist 9 -> 0`, `runDropReporter 0 -> 0`.

### Audit-lock synchronization

| Class participant | Implementation / classification |
| --- | --- |
| Precheck, store-mutex timer, remaining allowance | Not affected. Production `terminalizeBounded` remains byte-identical; guard still asserts a positive bounded `AllowanceMS`. |
| Injected runner | Fixed test ordering. It derives `childCtx`, owns a buffered completion channel, and starts the real strictly-contained helper. |
| Helper marker / flock | Fixed. A bounded ticker observes the marker; cancellation happens only after marker presence and a non-nil child process prove acquisition. |
| Contained job, process, streams, parent wait | Fixed test ownership. Every marker/error/context branch cancels and consumes `runDone`; the normal post-marker branch has a one-second reap watchdog and fail-closed kill+join fallback. |
| Timeout/error classification | Split from process startup. The in-process runner enters, waits for `ctx.Done()`, returns `StrictRunTimeout`, and is asserted to enter and return. |
| Protocol branches, stable failure IDs, uncertain receipt | Not changed. Existing production mapping is exercised; exactly one timeout failure event and an uncertain receipt are required. |
| Store mutex and fresh flock | Verified after terminal return. `storeMu.TryLock` succeeds and an independent flock can be acquired immediately. |
| Retry / re-entry | Per-call contexts, channels, timers, process, PID, and marker remain local; existing real protocol and second-run coverage remains unchanged. |

## All-return-path resource inventory

| Resource | Success / expected cancellation | Error / early exit | Timeout / cleanup / repeated call |
| --- | --- | --- | --- |
| Handler broadcaster | Persistence is disabled before route exposure; in-memory publication remains available. | Route error branches retain the same response/event logic. | `t.Cleanup(Broadcaster.Close)` is idempotent; every constructed server has exactly one owner. |
| Acquisition ticker | Stops by `defer` after marker. | Stops after child exit, marker stat error, or parent context cancellation. | Outer five-second context bounds acquisition. |
| Helper child/job/streams | Marker -> cancel child context -> consume `runDone`; strict runner joins child and three stream workers before return. | Child-before-marker is consumed from `runDone`; stat/context errors cancel then consume it. | One-second post-cancel watchdog records failure, kills the child as fail-closed cleanup, then consumes `runDone`. |
| Helper PID/flock/marker | PID captured only after marker; dead-PID and fresh-flock probes run after terminal return. | No unowned PID is accepted as success. | Temporary marker/root are test-owned; the helper process releases flock on termination. |
| In-process deadline runner | Enters, blocks on adapter context, returns `StrictRunTimeout`, then releases `storeMu` through the unchanged production defer. | Protocol/failure-ID mismatch fails the guard. | No process, watcher, stream, or retry exists in this guard; broadcaster subscription and adapter are cleaned once. |

## Fresh GREEN and static receipts

| Gate | Result | Receipt |
| --- | --- | --- |
| Revised structural owner guard | Exit 0; package `0.030s`. | `.scratch/windows-console-contract/r2-backend/revise1-structural-guard-green.txt`; SHA-256 `0DA2C0807102CC00DEF1CDB1A98ED5CCE63BF5FB9FBC9A4ADCB2DE5277331FE3`. |
| Exact audit guards | Exit 0; package `0.368s`. | `.scratch/windows-console-contract/r2-backend/revise1-audit-guards-green.txt`; SHA-256 `5680FC22C9A290A76209AF778D95EC1437DAE953F67A6ADD02B6F3707E37BABF`. |
| Revised named guards with race detector | Exit 0; package `1.410s`. | `.scratch/windows-console-contract/r2-backend/revise1-named-guards-race.txt`; SHA-256 `A11B5D2510FAB2A05E97B555C805AC6BA0CB8553F78D4E5CF488FC1261E6C9B1`. |
| Existing daemon-recovery route family | Exit 0; package `0.270s`; all prior response, status, body, receipt, redaction, warning, and call-count assertions passed unchanged. | `.scratch/windows-console-contract/r2-backend/revise1-daemon-route-family-green.txt`; SHA-256 `50DE69112EF733551E9D0064C08DFE198D39DB3DB6D748F43610EA374C376548`. |
| Existing audit worker family | Exit 0. Existing hidden-child harness remains an explicit pre-existing skip without `-tags=test_state_path_env`. | `.scratch/windows-console-contract/r2-backend/green-audit-worker-family.txt`; SHA-256 `83C5E9503C538F7A34C6E586742ED4EDA0A2C26F2A8C084097DF32B364114A72`. |
| Revised full GUI package | `go test -count=1 -timeout 12m ./internal/gui`; exit 0, `59.593s`; package leak oracle emitted no failure, proving `drainPersist=0 runDropReporter=0` after all cleanups. | `.scratch/windows-console-contract/r2-backend/revise1-internal-gui-broad-12m.txt`; SHA-256 `8A4D8D6DCCDD8C439602FE8FBA9FEFF60A4DD51CFC4C67494DAC4EAE9985EF3D`. |
| Revised vet | `go vet ./internal/gui`; exit 0. | `.scratch/windows-console-contract/r2-backend/revise1-go-vet-internal-gui.txt`; SHA-256 `CE48B67003E2B0FF2671B6FF427E7CA3CA4EB27B21DF1070481CE61D278DB1A2`. |
| Revised format / diff / allowlist | `gofmt -d` empty; scoped `git diff --check` exit 0; exact code allowlist contains only the two approved test files. | `.scratch/windows-console-contract/r2-backend/revise1-format-diff-allowlist.txt`; SHA-256 `964AB4EB67F31315DF3B483F696618179B76C2C52BB337057747461B7C1BBF21`. |

## Diff-invisible invariant echo

| Invariant | Verdict and falsifying probe |
| --- | --- |
| Every handler-test server has one lifecycle owner before any route can publish. | VERIFIED. AST inventory is causally bound to all eight actual participant functions and fails on a latent non-publishing bypass; full package oracle independently proves real publishing paths settle at `drainPersist=0 runDropReporter=0`. |
| Route HTTP results and in-memory events remain semantically identical. | VERIFIED. Existing route family passed without assertion changes; only unrelated disk persistence is disabled in these fixtures. |
| Post-acquisition cancellation happens only after marker-proved flock ownership. | VERIFIED. PID is captured only after marker; then cancel, join, dead-PID, mutex, and fresh-flock probes pass. |
| Deadline classification is independent of process startup. | VERIFIED. The asserted in-process runner enters, waits for context completion, returns timeout, and spawns no child. |
| Production/console bytes do not change. | VERIFIED by scoped mutation/diff surface: only the two approved `_test.go` files changed. The accepted console contract remained protected. |

## Backend contract statement

- Wire-level before/after: none; no endpoint, status code, field, ordering, pagination, or consumer contract changed.
- Authorization: not applicable; no route or handler authorization rule changed.
- Outbound calls: no production call site added or modified. The test-only child process is bounded by a five-second parent context and a one-second post-cancel reap watchdog; it is not retried and maps failure to test failure/uncertainty.
- Database/query cardinality: not applicable; no query or storage path changed.

## Risks / unknowns

| Item | Status |
| --- | --- |
| Existing audit real receiver harness | Explicitly skips without `-tags=test_state_path_env`; unchanged and not a failure of this lane. The full accepted command does not require that tag. |
| CLI quiesce and temporary-directory failures | Separate open diagnosis; intentionally not touched or hidden. |
| Native Linux/macOS, final QA, publication, install, and live hub | Still open downstream gates. |
| Production behavior | No residual implementation risk introduced here because production bytes are unchanged; remaining risk is limited to test fixture coverage and is guarded by exact/race/broad package evidence. |

## Adjacent findings

No new adjacent product defect was discovered. Existing CLI lifecycle failures remain under their separately admitted owner.

## Recommended next role

Lead should independently verify the two-file diff and receipt hashes, then continue with the separately admitted CLI quiesce/temporary-directory lifecycle diagnosis. This PASS does not authorize publication or deployment.

## Gate

**PASS** — REVISE iteration 1 replaced the detached synthetic guard with a mutation-proved AST inventory of the eight actual participants. Both accepted GUI test-lifecycle corrections remain within the two-file boundary with true RED/falsifier evidence, fresh GREEN, race, full package, vet, format, and scoped diff evidence. Broader release obligations remain open.

## Terms and Abbreviations

- **GUI** — graphical user interface.
- **PID** — process identifier.
- **TDD** — test-driven development.
- **RED / GREEN** — observed failing guard before correction / passing guard after correction.
- **PASS** — this bounded implementation is ready for the next gate.

# Item 3 Unit B — PR #563 round-1 correction QA

Outcome: **PASS**. The three round-1 defect classes meet their absolute criteria, the final generated bundle contains the frontend change, the focused QA checks pass, and the integration owner's final exact tagged GUI/CLI package run passes after both test-harness issues were corrected. No commit, push, deployment, real GUI spawn, process sweep/kill, or `MCPHUB_GUI_SPAWN_TESTS` use occurred in this QA lane.

## Scope and evidence provenance

- Accepted plan: `item3-unitB-plan.md:338-374`.
- Frontend implementation handoff: `item3-unitB-round1-frontend-implementation.md`.
- Backend implementation handoff: `item3-unitB-round1-backend-implementation.md`.
- QA independently inspected the final diff and executed only focused frontend, tagged CLI/GUI, bundle-reconciliation, formatting, and whitespace checks.
- The broader frontend build/test and exact final tagged package results were executed by the main integration owner and accepted as upstream evidence. Their supplied results are preserved as a provenance-labelled handoff capture at `.scratch/pr563-round1-qa-integration-owner-handoff.txt`; it is not represented as a QA-created subprocess transcript.

## Pre-run criterion tightening

| Criterion | What would this criterion let pass? | Tightened falsifying condition |
| --- | --- | --- |
| C1 port-change navigation | An early, repeated, exhaustion, same-port, or standby-triggered assignment | For one matching `reserved` event: zero assignments while readiness is pending, exactly one after success, zero after exhaustion, and zero for same-port/non-reserved; standby must 404 `/favicon.ico`, full GUI must serve it |
| C2 same-port handoff | A synthetic retry that never holds a real port, never spans the old two-second budget, or never commits | Default same-port budget is `Quiesce + Bind`, greater than `Quiesce` and below `Proof`/`Reservation`; a real listener stays held for 3.2 seconds, is released, the child binds, commits, and exits without rollback failure |
| C3 privileged actual port | A direct validator passes while spawn is skipped, argv rewrites port 80, invalid ports spawn, or persisted policy widens | Actual port 80 reaches `Spawn` exactly once with `--port 80`; 0 and 65536 reject before another spawn; persisted `[1024,65535]` policy remains unchanged |
| Adjacent surfaces | Focused tests pass while the bundle is stale or the broader packages fail | Generated bundle markers reconcile to source; frontend build/test/typecheck, Go build/vet/format/diff, named-pipe stress, and the exact final tagged GUI/CLI packages all pass |

## Acceptance-to-evidence map

| Acceptance criterion | Source/test evidence | Observed result | Status |
| --- | --- | --- | --- |
| C1: matching `reserved/new_port` waits for activated full GUI | Single consumer and bounded poll at `internal/gui/frontend/src/api.ts:960-1037,1110-1149`; pending/success/exhaustion/same-port assertions at `SectionGuiServer.test.tsx:293-360`; standby path gate at `internal/gui/ping.go:123-149`; full icon route at `internal/gui/assets.go:31-48` | RED reproduced 2 failures/19 before the owner change. QA GREEN passed 19/19. Assignment stayed absent while the injected readiness promise was pending, occurred once after success, stayed zero after exhaustion, and stayed zero for same-port/committed. Production poll is bounded to 20 attempts, 500 ms each, plus at most 19 delays of 250 ms: 14.75 seconds worst-case timer budget. | PASS |
| C1: wording does not imply commit | `internal/gui/frontend/src/components/settings/SectionGuiServer.tsx:95-103` | Message says the tab makes a best-effort attempt to follow and gives manual new-port guidance; it does not claim commit. | PASS |
| C2: same-port budget spans quiesce and bind | Defaults at `internal/gui/gui_restart_record.go:47-59`; positive deadline validation at `internal/gui/gui_restart_protocol.go:553-570`; composed child budget at `internal/cli/gui.go:232-236`; ordering guard at `gui_restart_record_test.go:22-58` | Defaults remain Bind 2 s, Quiesce 5 s, Proof 10 s, Reservation 10 s. Same-port budget is 7 s, greater than Quiesce and less than Proof/Reservation; port-change remains Bind-only. | PASS |
| C2: real 3.2-second listener handoff commits | `internal/cli/gui_self_restart_handoff_test.go:94-206` | QA focused run passed in 3.32 s: the OS-assigned listener stayed held beyond the former 2 s budget, was released before the 5 s quiesce duration, the child bound the same port, the marker was observed `committed`, and child shutdown returned nil. | PASS |
| C3: privileged actual port reaches spawn | Explicit flag precedence at `internal/cli/gui_port.go:64-75`; actual-port callback at `internal/cli/gui.go:154-178`; regression at `internal/cli/gui_self_restart_handoff_test.go:284-337` | Actual 80 resolved to 80, invoked `Spawn` once, and preserved `gui --port 80 --no-tray`. Actual 0 and 65536 rejected without increasing the spawn count. | PASS |
| C3: persisted implicit policy unchanged | `validPersistedGUIPort` remains `[1024,65535]` at `internal/cli/gui_port.go:43-61`; classification/argv matrix tests | QA passed all 9 persisted-port cases and all 49 argv-matrix subtests, including explicit-port precedence and invalid persisted fallback. | PASS |
| Adjacent generated/frontend surface | `internal/gui/assets/app.js`; frontend package scripts at `internal/gui/frontend/package.json:6-10` | Bundle contains one occurrence each of `/favicon.ico`, `gui-restart-ready`, and the loopback target origin marker; size 581,204 bytes. Integration owner: build 268 modules in 1.78 s, full Vitest 69 files/1123 tests in 8.71 s, and typecheck all exited 0. The expected greater-than-500-kB Vite warning is informational. | PASS |
| Adjacent Go surface | Final diff plus integration-owner exact package run | Build and vet exited 0; final scoped `gofmt -d` and `git diff --check` were empty. Final exact tagged run passed both packages in 224.4 s: GUI 222.962 s, CLI 219.672 s. | PASS |

## Executed checks and preserved output

| Executor | Verbatim command/check | Result and counts | Wall/package time | Evidence |
| --- | --- | --- | --- | --- |
| Frontend implementation lane, RED | `npm test -- src/components/settings/SectionGuiServer.test.tsx` | Expected RED: 1 file failed; 17 passed, 2 failed of 19 | Vitest 14.57 s | `.scratch/pr563-round1-p1-frontend-red.txt` |
| Backend implementation lane, RED | `go test -tags=test_state_path_env -count=1 -timeout 20s ./internal/cli -run 'TestRestartV3_(SamePortStandbyBindWaitsForParentClose|ParentCompositionPrivilegedActualPortReachesSpawn)$' -v` | Expected RED: 2 failed of 2 | Package 2.124 s | `.scratch/pr563-p2-backend-red.txt` |
| QA | `npm test -- src/components/settings/SectionGuiServer.test.tsx` | 1/1 file, 19/19 tests passed; 0 failed/skipped | Vitest 1.00 s; command 1.934 s | `.scratch/pr563-round1-qa-frontend-focused.txt` |
| QA | `npm run typecheck` | TypeScript compiler exit 0; no test count applicable | 5.015 s | `.scratch/pr563-round1-qa-frontend-typecheck.txt` |
| QA | `go test -tags=test_state_path_env -count=1 -timeout 30s ./internal/cli/ -run '^(TestClassifyPersistedGUIPort|TestRestartV3_PortArgvMatrix|TestRestartV3ChildStandbyBindUsesDedicatedBindDeadline|TestRestartV3_SamePortStandbyBindWaitsForParentClose|TestRestartV3_ParentCompositionPrivilegedActualPortReachesSpawn|TestRestartV3_ChildStartupUsesStandbyContinuationAndCommits)$' -v` | 6/6 selected top-level tests and 58/58 enumerated subtests passed | Package 3.466 s; command 4.532 s | `.scratch/pr563-round1-qa-cli-focused.txt` |
| QA | `go test -tags=test_state_path_env -count=1 -timeout 30s ./internal/gui/ -run '^(TestRestartDeadlines_DefaultPolicy|TestRestartV3_ChallengedStandbyPingBindsExactChild|TestStaticAsset_AppJS|TestStaticAsset_NoCacheHeader)$' -v` | 4/4 passed | Package 0.114 s; command 1.036 s | `.scratch/pr563-round1-qa-gui-focused.txt` |
| QA | Read-only source/bundle marker reconciliation | 3/3 required markers present in source and bundle | Not timing-sensitive | `.scratch/pr563-round1-qa-bundle-reconcile.txt` |
| QA | `gofmt -d <five touched Go files>` and `git diff --check` | Both exit 0; both outputs empty | 0.092 s | `.scratch/pr563-round1-qa-final-format-diff.txt` |
| Integration owner | `npm run build`; `npm run test`; `npm run typecheck`; `go generate ./internal/gui/...`; `go build ./...`; `go vet ./...` | All exit 0; build 268 modules; tests 69/69 files and 1123/1123 tests | Build 1.78 s; tests 8.71 s | `.scratch/pr563-round1-qa-integration-owner-handoff.txt` |
| Integration owner | `go test -tags=test_state_path_env -count=100 -timeout 2m ./internal/cli/ -run '^TestSECF1_UpgradeDialReachesListenerBoundOnSupervisorIPCAddress$'` | 100/100 repetitions passed | Package 1.159 s | `.scratch/pr563-round1-qa-integration-owner-handoff.txt` |
| Integration owner | `go test -tags=test_state_path_env -count=1 -timeout 15m ./internal/gui/ ./internal/cli/` | 2/2 packages passed; no current failing package check | Command 224.4 s; GUI 222.962 s; CLI 219.672 s | `.scratch/pr563-round1-qa-integration-owner-handoff.txt` |

## Defect-class participant classification

| Defect class | Participant | Classification | Evidence/rationale |
| --- | --- | --- | --- |
| C1 | Section action/event subscription | not-affected | `SectionGuiServer.tsx:71-88` still delegates to the single API owner; no parallel assignment path was added |
| C1 | `consumeGuiRestartProgressEvent` and readiness helpers | fixed | `api.ts:960-1037,1110-1149` gates the sole restart-origin assignment on bounded readiness |
| C1 | Standby readiness route | not-affected | `ping.go:123-149` already serves only challenged `/api/ping` and 404s `/favicon.ico` |
| C1 | Full GUI asset route | not-affected | `assets.go:31-48` already owns `/favicon.ico`; focused asset tests pass |
| C1 | `SectionGuiServer` navigation tests | fixed | Tests now pin pending, success-once, exhaustion-zero, mismatched-stream, same-port-zero, and committed-zero behavior |
| C1 | Generated `assets/app.js` | fixed | Regenerated bundle contains the readiness probe markers |
| C2 | `RestartDeadlines` defaults and positive validation | not-affected | Default durations and validation owner are unchanged; ordering coverage was added |
| C2 | `runRestartV3ChildStartup` budget composition | fixed | Same-port adds Quiesce to Bind; port-change retains Bind alone |
| C2 | `bindRestartV3ChildStandby` retry owner | not-affected | Retry remains address-in-use-only and consumes the caller's bounded context |
| C2 | Parent quiesce/listener release | not-affected | `EnterGrace` and `CloseListener` remain owned at `gui_restart_protocol.go:351-357`; the child budget now spans them |
| C2 | Child readiness/commit marker path | not-affected | Existing path commits after the retained listener is activated; real-listener regression observed `committed` |
| C2 | Same-port regression test | fixed | Uses an OS-assigned listener held for 3.2 s instead of a synthetic two-attempt retry |
| C3 | `resolveGuiPort` explicit-flag policy | not-affected | Explicit flag still wins, including port 80 |
| C3 | Parent actual-bound-port callback | fixed | Validation now uses TCP `[1,65535]` rather than persisted-setting policy |
| C3 | Spawn argv reconstruction | not-affected | Exact explicit `--port 80` argv is retained |
| C3 | Persisted implicit-port classification | not-affected | Remains `[1024,65535]`; 9 classification and 49 argv cases pass |
| Adjacent | Adopt-plan fixed-port test setup | fixed | Test-only environment assumption removed; production allocator selects a free port |
| Adjacent | Named-pipe upgrade test lifecycle | fixed | Client close is deferred until after listener `Accept`; 100/100 stress and final tagged packages pass |

## Historical failure classification

| Incident | Classification | Resolution and current status |
| --- | --- | --- |
| AdGuardVpnSvc occupied explicit test port 9323 during the initial full run | **test-rot / deterministic environment collision**, not a product regression | `TestAdoptPlanRouteNeverSerializesSecretValues` no longer assumes a globally free fixed port and instead exercises the production allocator. Focused verification and the final exact package run pass. No unresolved defect remains. |
| Named-pipe upgrade test failed because the client could close before the harness observed `Accept` | **flaky test-harness lifecycle race**, not a product regression | The lifecycle owner now defers close until after `Accept`; the formerly failing exact test passed 100/100 and the final exact package run passed. No skip, xfail, or quarantine was introduced. |
| Frontend and backend RED runs | **expected regression-test RED**, not final-check failures | Each reproduced its pre-fix defect and is paired with focused GREEN plus final integration evidence. |

There is no unexplained current failing check.

## Ambient-input and basic-performance assessment

| Ambient input | Applicability and control |
| --- | --- |
| Clock/timers | Applicable. Frontend production uses `Date.now` only for cache busting and bounded timers for readiness; focused tests inject a controlled promise. The same-port test intentionally uses a real 3.2 s hold, creating a 1.2 s margin beyond the old 2 s failure and remaining below the 5 s quiesce duration. |
| Randomness/ports | Cryptographic/test nonce bytes are fixed. Listener values use OS-assigned ephemeral ports and assert behavior, not a hard-coded value; this removed the observed 9323 collision. |
| Filesystem ordering/state | Ordering is inapplicable. Tagged Go tests use isolated state paths/temp directories; every QA and integration-owner `internal/gui` or `internal/cli` Go test command carried `-tags=test_state_path_env`. |
| Scheduling | Frontend readiness ordering is promise-controlled. The same-port race window is deliberately large. The named-pipe close/accept order is now structural and passed 100 repetitions. |
| Locale/timezone | Inapplicable to the changed decisions and assertions. |

Basic performance is accepted: the readiness probe is bounded to at most 14.75 seconds; the real same-port handoff passed in 3.32 seconds; no render-loop or throughput hot path changed. Deep profiling is not required for this correction gate.

## Residual risk and non-goals

- **ASSUMPTION (UNVERIFIED):** a production browser reports the embedded full-GUI favicon as `Image.onload` and the standby 404 as `Image.onerror`. The no-GUI-spawn constraint prohibited the resolving manual smoke; route ownership, bundle content, and the injected readiness contract were verified.
- A full-GUI activation taking longer than 14.75 seconds intentionally exhausts the probe and leaves the old page in place; the UI retains manual new-port guidance.
- Real administrator-bound port-80 process restart was not spawned. Validation, target resolution, spawn seam count, and argv were verified without a GUI process.
- The integration owner's broad outputs were accepted through the explicit handoff; QA independently reran the defect-focused checks and final formatting/diff checks, not the four-minute package suite.

## Gate

**PASS**. No bug-registry entry is required because both historical harness defects were corrected and reverified, and there is no unresolved current defect. Recommended next role: the main `$lead` integration owner should record this gate and advance the PR #563 review loop; no further round-1 implementation is indicated.

## Terms and Abbreviations

- CLI — Command-Line Interface.
- GUI — Graphical User Interface.
- QA — Quality Assurance.
- TCP — Transmission Control Protocol.
- xfail — An expected-failure test classification.

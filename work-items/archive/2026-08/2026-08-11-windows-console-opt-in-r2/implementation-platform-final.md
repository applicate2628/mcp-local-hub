# Platform final gate — Windows console opt-in R2

## Summary

The focused Windows console/process/command-line interface (CLI)/graphical user interface (GUI) guards, both Windows release-shaped builds, Portable Executable (PE) subsystem admission, npm tests, version synchronization, generated-package freshness, six-target cross-build, package payload validation, and exact candidate-patch publication scan all passed.

The release platform gate is **REVISE**:

- native Ubuntu vet and build passed, and the focused CLI gate passed, but the full non-CLI set failed in unchanged `internal/process` and `internal/api` owners;
- no configured native macOS execution target is currently available;
- Policy B waives only the already-characterized broad `internal/cli` lifecycle baseline. It does not waive these native-platform gaps.

No source correction was attempted. No package was installed or published, no hub process or fleet state was changed, no Sandbox was launched, and the two pull-request worktrees were not read, written, executed, removed, or rebased.

## Receiving-side echo

| Contract field | Received and applied |
| --- | --- |
| Role / goal | Platform engineer for one bounded final platform and release-readiness gate on the existing dirty console candidate. |
| Accepted policy | Policy B from `status.md` and `reliability-cli-prerequisites.md` SHA-256 `C52BF8ADFEA00E1BB90A7A217BF2CA42A04C172E9CE29C915C13456726BFACC8`. |
| Waiver boundary | Park only the pre-existing broad `internal/cli` lifecycle defects; do not redesign or add fatal-exit/restart behavior. |
| Approved checks | Focused console/process/CLI/GUI guards; Windows builds and PE admission; npm tests and package shapes; available native Linux; direct native macOS availability; publication/diff inventory. |
| Forbidden side effects | Source mutation; stage/commit/push; install/restart/deploy; live hub or fleet access; visible console launches; Sandbox; PR #598/#600 worktree access. |
| Failure behavior | If a source correction is required, return REVISE with the exact conflict instead of expanding scope. |
| Handoff | Lead sends native Linux reds and macOS availability to independent QA; this artifact does not authorize publication or installation. |

## Repository orientation and candidate boundary

`README.md:11-16` identifies a live preview with Windows 11 as the primary tested path. `build.ps1:33-48` is the canonical Windows build/admission shape; `npm/package.json:24-35` owns npm tests and the meta-package payload; `.github/workflows/npm-publish.yml:102-164` owns the six-target release build and pack shape. The active status keeps the release in Phase F under Policy B.

The existing Windows target evidence was not repeated: `brief.md:7-12` accepts the 6/6 top-level and 63/63 subtest result and says not to rerun it unless a relevant source byte changes. The subsequent accepted implementation changed only two GUI test files and had already passed its full GUI gate. This lane reran the named focused guards but did not launch Windows Sandbox or the live target harness.

All commands below were invoked from `<repo>` unless a platform column says otherwise. Build/package output was confined to `/.scratch/windows-console-contract/r2-platform-final/`.

## Windows focused gates

| Gate | Exact command | Result |
| --- | --- | --- |
| Phase A console policy | `go test -count=1 -timeout 5m ./cmd/mcphub ./internal/cli ./internal/process -run '^TestWindows(ConsolePrefixGrammar|ConsolePolicyApplication|RootHelpDebugConsolePrefix|GUIConsoleState|ReleaseDebugConsole)$'` | Exit 0. `cmd/mcphub` and `internal/cli` passed; `internal/process` correctly reported no matching test. |
| Phase B child owners | `go test -count=1 -timeout 5m ./internal/process ./internal/cli ./internal/gui ./internal/api -run '^TestWindows(ChildSpawnContract|StartWithJobNoConsole|SupervisorSpawnNoConsole|SupervisorRetryNoConsole|RestartNoConsole|HiddenWorkerNoConsole|NetshNoConsole|EditorRequiresGUI)$'` | Exit 0 in all four packages; `internal/process` 38.583 s. |
| Release-parent behavior | `go test -count=1 -timeout 5m ./internal/process -run '^Test(R35NoConsole|ReleaseParentConsole)'` | Exit 0; 15.888 s. |
| Route/state/static owner | `go test -count=1 -timeout 5m ./cmd/mcphub ./internal/cli -run '^(TestRouteBareInvocationProducesTrayEnabledGUIArgs|TestShouldAutoLaunchGUIIsConsoleIndependent|TestResolveReleaseConsole|TestSetDebugConsoleAcquiredRoundTrips|TestWindowsSingleConsoleControlStaticGate)$'` | Exit 0 in both packages. |
| PE admission/canonicalize | `go test -count=1 -timeout 5m ./internal/binaryadmission ./internal/cli -run '^(TestWindowsPE|TestCanonicalizeBinaryToTarget)'` | Exit 0 in both packages. |
| R2 GUI lifecycle owners | `go test -count=1 -timeout 3m ./internal/gui -run '^(TestDaemonRecoverHandlers_OwnBroadcasterLifecycle|TestAuditLockTerminalWorkerCancellationAfterAcquisitionReapsBeforeReturn|TestAuditLockTerminalizationDeadlineClassifiesWithoutProcessStartup)$'` | Exit 0; 0.540 s. |

The test commands were invoked directly through the existing shell; no visible `Start-Process` path was used. Child-oriented guards exercised the repository's existing hidden/no-window construction seams.

## Windows release-shaped builds and PE admission

The build used the canonical `-trimpath` plus `-H windowsgui` shape and npm authority version `0.4.28`, but wrote to scratch rather than running `go generate` or overwriting a source/release path. A native amd64 GUI-subsystem admission adapter checked both target binaries.

| Target | Build/admission | Size | SHA-256 |
| --- | --- | ---: | --- |
| Windows amd64 hub | Build exit 0; admission exit 0; subsystem `2 (WINDOWS_GUI)` | 23,214,080 | `BA8E7AE000972B6E1F254A8357C50487FFA4DA5357FFA27D7F401F7E1F6443E5` |
| Windows arm64 hub | Build exit 0; admission exit 0; subsystem `2 (WINDOWS_GUI)` | 20,884,992 | `BFD48EB756C5EE93F2F04A20497E890F996A7F4FFC6E77BA0EA7B27CE147A817` |
| Windows amd64 admission adapter | Build exit 0 | 1,810,944 | `04211A699E8D5762451FBC9EA6D955F5ECD1E19D200600AC54A79483A628EBD4` |
| Windows arm64 admission adapter | Build exit 0 | 1,711,616 | `903FEEE508D9AFD335F7543070CDD59B690759915B3020264227C821DAAA0277` |

## npm and six-target package gates

| Gate | Result |
| --- | --- |
| `npm --prefix npm test` | Exit 0; 29 tests passed, 0 failed/skipped. |
| `node npm/sync-version.js --check` | Exit 0; `build.sh`, `build.ps1`, and `versioninfo.json` match npm version `0.4.28`. |
| Generator freshness | The generator ran against a scratch copy: 6 targets, 0 files written; all 12 generated `package.json`/`README.md` hashes matched the candidate. |
| Meta `npm pack --dry-run --json` | Exit 0; exactly `README.md`, `bin/cli.js`, `lib/platform-binary.js`, `package.json`, `scripts/postinstall.js`. |
| Cross-build | Windows amd64/arm64, Darwin amd64/arm64, and Linux amd64/arm64 all exited 0. |
| Windows package payloads | Each contains exactly `README.md`, `bin/mcphub.exe`, `bin/mcphub-pe-admit.exe`, `package.json`; both pack checks exited 0. |
| Darwin/Linux package payloads | Each contains exactly `README.md`, `bin/mcphub`, `package.json`, with no `.exe` or PE adapter; all four pack checks exited 0. |
| Native staged Linux x64 binary | Exit 0 under Ubuntu; reported version `0.4.28`, commit `dcc41eb8`, build date `platform-final-gate`, platform `linux/amd64`. |

One initial `npm pack --prefix npm` wrapper was invalid: npm searched for `<repo>/package.json` and exited `-4058`. It executed no pack oracle and is excluded. The corrected command ran from `<repo>/npm` with npm cache inside scratch and passed.

## Native Linux gate

### Availability and tools

- Direct probe: `wsl.exe` present; WSL2 default Ubuntu and an additional Debian distribution are registered.
- Native Ubuntu identity: Linux `6.18.33.2-microsoft-standard-WSL2`, x86_64.
- Native Go: `go1.26.2 linux/amd64`.
- `node --version` returned `v24.18.1`, but it resolves through Windows interoperability. `npm --version` failed because that Windows installation referenced a non-existent translated module path. Native Linux npm is therefore **unavailable**, not passed; no tool was installed.

The valid WSL wrapper used literal command delivery, scratch-local `GOCACHE`/`GOMODCACHE`, named `GATE_EXIT` markers, and one terminal wrapper exit. An earlier quoting attempt expanded bash variables at the PowerShell boundary, ran zero gates, and is explicitly excluded despite wrapper exit 0.

| Gate | Exact command (inside native Ubuntu) | Result |
| --- | --- | --- |
| Vet | `go vet ./cmd/mcphub ./internal/cli ./internal/process ./internal/gui ./internal/api ./internal/binaryadmission` | Exit 0. |
| Build | `go build ./cmd/mcphub ./internal/cli ./internal/process ./internal/gui ./internal/api ./internal/binaryadmission` | Exit 0. Linker warned that a generated object lacks `.note.GNU-stack`; build still passed. |
| Non-CLI tests | `go test -count=1 -timeout 12m ./cmd/mcphub ./internal/process ./internal/gui ./internal/api ./internal/binaryadmission` | Exit 1. `cmd/mcphub` PASS 9.282 s; `internal/gui` PASS 99.464 s; `internal/binaryadmission` PASS 0.102 s; `internal/process` and `internal/api` failed as inventoried below. |
| Focused CLI | `go test -count=1 -timeout 5m ./internal/cli -run '^(TestResolveReleaseConsole|TestSetDebugConsoleAcquiredRoundTrips|TestCanonicalizeBinaryToTarget|TestGuiCmd_TrayRuntimePolicyFromRoutedInvocation)$'` | Exit 0; 0.889 s. This is not a broad CLI substitute. |

### Native Linux red inventory

| Package | Exact failure |
| --- | --- |
| `internal/process` | `TestRunContainedStreamPOSIX_LiveGroupStillTimesOut`, `TestRunContainedStreamPOSIX_CleanupTimeoutIsTyped`, and `TestRunContainedStreamPOSIX_ReadyCompletionsBeatJoinDeadline`: child start returned `context deadline exceeded`. |
|  | `TestRunContainedStream_SuccessStreamsBeforeExit`: stdout was not observed before child exit. |
|  | `TestR21CleanupDeadlineBoundsAStuckWait`: cleanup held the request for `2.23400735s` after its deadline. |
| `internal/api` | `TestWriteHubMcpStateFile_HonorsPersistedStrictModeWhenParentInsecure`: persisted strict mode did not reject the write. |
|  | `TestMigrateSetsRelayExePathForZed`: expected `(memory, zed)` in `Applied`, observed `Applied=[]`. |
|  | `TestDialSupervisorIPCReconcile_RoundtripWithFakeListener`, both `dry-run` and `apply`: Unix socket bind returned `invalid argument`. |

Read-only diff classification found the failing `internal/process` implementation/test owner files byte-identical to `HEAD`, the failing `internal/api` test files byte-identical to `HEAD`, and the only candidate `internal/api` diff is Windows build-tagged. This supports a baseline/environment hypothesis, but source-byte equality does not prove behavioral non-involvement. Per the no-rerun/no-A-B boundary, no immutable-HEAD runtime control was launched and the native Linux gate remains **REVISE**, not PASS.

## Native macOS availability

Fresh direct probes found:

- GitHub CLI available and authenticated, but `repos/applicate2628/mcp-local-hub/actions/runners` returned `TOTAL=0`;
- no macOS/Darwin runner environment binding is declared in this session;
- an SSH executable exists, but no authorized/configured macOS target was supplied;
- the repository's current continuous-integration workflow cross-compiles Darwin and does not provide native execution.

Therefore native macOS is **BLOCKED:dependency** for this gate. Darwin amd64/arm64 cross-build success is compilation evidence only and is not represented as native execution.

## Publication and diff readiness

| Check | Result |
| --- | --- |
| `git diff --check` | Exit 0. |
| Staged paths | 0; this lane did not stage. |
| Tracked `/.scratch/` paths | 0. |
| `go.mod` / `go.sum` delta | 0 files. |
| Working-tree status | 85 rows before this artifact; the dirty tree includes hub candidate files, two out-of-scope PR work-item directories, and an unrelated Windows API diff. No whole-tree staging set is authorized. |
| Publication scanner availability | The documented PowerShell wrapper is absent. Direct probe found the installed Python owner and verified its CLI. |
| Path-level candidate scan | 103 candidate files examined. Two whole-file findings were pre-existing synthetic fixture/comment paths in `internal/cli/install_upgrade_test.go` and `internal/cli/setup_ephemeral_range_windows.go`; none is in an added hunk. All untracked candidate files passed. |
| Exact tracked candidate patch scan | Clean; patch SHA-256 `D6F07EF6E454BDE02248FA0D502C2AA60CB58F5A99772F452BB5C656AA1B8110`, size 152,256 bytes. |

The readiness patch intentionally excludes the two PR work-items and the unrelated `internal/api/port_alloc_excluded_windows.go` change. The final integration owner must construct and rescan the exact staged set; this lane does not claim a stage-level publication PASS.

## Changed-file and side-effect inventory

This platform lane makes exactly one canonical repository change:

- `work-items/active/2026-08-11-windows-console-opt-in-r2/implementation-platform-final.md` — this handoff.

Intended non-canonical evidence/build output was created under `/.scratch/windows-console-contract/r2-platform-final/`: four Windows binaries/adapters, four Darwin/Linux hub binaries in the npm stage, scratch npm metadata/cache, WSL Go caches, and the exact publication patch. The invalid initial npm wrapper also caused npm to write its ordinary diagnostic log under `%LOCALAPPDATA%/npm-cache/_logs/`; two exact-file cleanup attempts were rejected by the execution policy, so that one standard cache log remains outside the workspace. No product source, manifest, workflow, package metadata, installed binary, scheduler state, supervisor state, or fleet data was changed.

The PR #598 and #600 scratch worktrees and their source/test surfaces were untouched. The live hub fleet was untouched.

## Rollback and cleanup

- Source rollback: not applicable; there is no source patch from this lane. Removing this report is the exact inverse of the canonical write if Lead rejects the handoff.
- Scratch cleanup: after downstream QA no longer needs the binaries/patch, remove only the resolved `/.scratch/windows-console-contract/r2-platform-final/` directory. Cleanup was not executed because those outputs are the current evidence and package inputs.
- Installed rollback: not exercised because installation/restart was forbidden. The prior installed hub remains unchanged. Any later installation must preserve the prior artifact and use the accepted 10-minute availability/no-visible-console rollback rule from `reliability-cli-prerequisites.md`.
- No rollback claim is represented as exercised in a lower environment.

## Required next gate

Independent QA should own the next action:

1. decide whether the native Linux failures reproduce on its canonical Linux runner or are WSL/cold-mounted-cache baseline artifacts;
2. obtain a configured native macOS runner or return an explicit operator-approved platform narrowing;
3. consume the green Windows/npm evidence without repeating Sandbox or touching the live fleet;
4. only after QA PASS, build the exact staged set and run a fresh stage-level publication scan before commit/push.

## Gate

**REVISE** — Windows console/process/CLI/GUI, PE admission, npm, six-target cross-build/package shape, and exact candidate-patch publication checks passed. Native Linux full execution is red and native macOS execution is unavailable. Policy B does not waive either gap. Publication, install, restart, and live fleet verification remain unauthorized and unperformed.

## Terms and Abbreviations

- **CLI** — command-line interface.
- **GUI** — graphical user interface.
- **PE** — Portable Executable, the Windows binary format.
- **WSL** — Windows Subsystem for Linux.
- **PASS / REVISE / BLOCKED** — accepted gate / bounded correction or evidence required / external dependency required.
- **Policy B** — explicit parking of the pre-existing broad CLI lifecycle baseline for this console-only release.

# PR 591 R35 consolidated QA

## Scope and execution boundary

- Candidate: `.scratch/pr591-bot-r9`, reviewed against base `51ac193f7e2c1d49404a4609b31de20fd06566da`, published head `c363e5845ebda802932d57b23757d68a98cddcf8`, plus its 14-path R35 worktree delta.
- Mode: read-only QA. No candidate source, tests, documentation, status record, Git index, commit, push, GitHub comment, or unrelated process was modified.
- Environment: Windows `go1.26.5 windows/amd64`; Linux target probe through Ubuntu WSL with Go's test-binary `-exec` namespace wrapper.
- CodeGraph was queried before source-led reasoning but resolved through the parent index rather than stable candidate bytes; it is non-load-bearing. All findings below use exact candidate files and command output.

## Criterion guard before execution

| Criterion | What a too-weak check could let pass |
|---|---|
| Exact CMake keyword | Lowercase `result_variable` treated as CMake's exact `RESULT_VARIABLE`. |
| Pin-status remote shape | A valid user-optional `host:path` remote rejected, or a local/drive-relative path granted remote authority. |
| POSIX lifecycle | A test skipped outside PID 1, reaped before group signal, or never reached the zombie fixture. |
| Windows containment | `CREATE_NO_WINDOW` present while stdout/stderr redirection or adjacent cleanup breaks. |
| Discovery root | A regular file reported as a synthetic child-path failure instead of terminal invalid explicit input. |
| Documentation/catalogue | The 11-row showcase mistaken for the number actually installed by `install --all`. |

## Executed checks

| Check | Result and count | Wall time | Raw output |
|---|---|---:|---|
| `git diff --check` | PASS; 0 diagnostics | 0.4 s | `.scratch/pr591-bot-r9/.scratch/review-r35-qa/00-diff-check.txt` |
| `go test -json ./internal/cmakegraph ./internal/vcpkgmcp/discovery ./internal/vcpkgmcp/pinstatus ./internal/process -count=1 -timeout=120s` | PASS; 567 passed, 0 failed, 0 skipped, 0 xfail. Counts: cmakegraph 131; discovery 53; pinstatus 248; process 135. | 25.315 s | `.scratch/pr591-bot-r9/.scratch/review-r35-qa/01-focused-go-test.json` |
| `wsl.exe ... unshare --user --map-root-user --pid --fork --mount-proc go test -v ...TestRunContainedStreamPOSIX_ZombieOnlyGroupDoesNotFalseTimeout...` | SKIP; test binary was not PID 1, therefore non-evidence. | 0.976 s | `.scratch/pr591-bot-r9/.scratch/review-r35-qa/03-wsl-linux-pid1-zombie-verbose.txt` |
| `wsl.exe ... go test -v -exec "unshare --user --map-root-user --pid --fork --mount-proc" ./internal/process -run "^TestRunContainedStreamPOSIX_ZombieOnlyGroupDoesNotFalseTimeout$" -count=1 -timeout=35s` | FAIL; 0 passed, 1 failed, 0 skipped, 0 xfail. Test binary was PID 1 and failed at `zombie fixture start: context deadline exceeded`. | 11.002 s | `.scratch/pr591-bot-r9/.scratch/review-r35-qa/04-wsl-linux-pid1-zombie-exec.txt` |
| `go test -json ./... -count=1 -timeout=240s` | UNVERIFIED incident. The operator stopped it because unscoped CLI/API tests caused Scheduler-side-effect risk while the hub was disrupted. The exact self-started tree `pwsh 56556 -> go 62032 -> cli.test 71540` was terminated; the immediate direct probe confirmed all three PIDs absent. No terminal suite result was recorded. | 153 s before stop | `.scratch/pr591-bot-r9/.scratch/review-r35-qa/05-full-go-test.json` |

## Acceptance mapping and receiving-side echo

| R35 participant | Classification | Evidence |
|---|---|---|
| `cmakegraph` exact `RESULT_VARIABLE` | fixed | Focused package PASS, including `TestR35IncludeResultVariableRequiresExactCase`; candidate uses exact equality at `internal/cmakegraph/cmakegraph.go:2026`. |
| Pin-status user-optional SCP and local guards | fixed | Focused package PASS, including `TestR35SCPLikeRemoteWithoutUserIsAdmitted` and `TestR35SCPLikeAdmissionPreservesLocalPathGuards`; candidate guard is `internal/vcpkgmcp/pinstatus/redact.go:263-287`. |
| POSIX signal-before-reap | not fixed | The true Linux PID-1 run fails. The fixture constructs `&posixContainedChild{cmd: command}` at `internal/process/contained_stream_linux_test.go:317`, but the new deferred lifecycle requires `initializePlatformContainedWait` to create `waitObserved` and `reapDone` (`internal/process/contained_stream_wait_deferred_posix.go:10-17`). It cannot reach its zombie-start handshake at line 344. Classification: regression/test-contract change; the same implementer must repair the fixture and re-run it under PID 1. |
| Windows `CREATE_NO_WINDOW` with redirected streams | fixed | Focused Windows process package PASS, including `TestR35ContainedWindowsUsesNoWindowAndPreservesRedirectedStreams`; `containedWindowsCreationFlags` includes `windows.CREATE_NO_WINDOW` at `internal/process/contained_stream_windows.go:157-161`. |
| Explicit regular-file discovery root | fixed | Focused discovery package PASS, including `TestR35ExplicitRegularFileIsInvalidRoot`; direct root-object classification is at `internal/vcpkgmcp/discovery/discovery.go:263-276`. |
| Documentation/catalogue behavior | not fixed | README says `mcphub install --all # all 11 servers` at `README.md:106`. Candidate tracks 16 manifests; one is workspace-scoped. `InstallAllWithOpts` enumerates all names and skips only `KindWorkspaceScoped` (`internal/api/install.go:299-327`), leaving 15 global install targets. `configs/ports.yaml` has 15 `global` server records. The 11-row README table does not make the command cardinality true. |

## Defects and classifications

1. **Regression / contract-change test:** the Linux non-reaping-PID1 zombie integration fixture no longer creates the deferred-wait state required by R35. Reproduction is the failed namespace command above. Expected: the fixture reaches `zombie-launched`, performs the signal-before-reap lifecycle, and completes. Actual: it times out waiting for the initial fixture handshake. This invalidates POSIX acceptance evidence.
2. **Documentation defect:** `README.md:106` claims `install --all` installs 11 servers, while the owner installs 15 global manifests after skipping one workspace-scoped manifest. Expected: prose distinguishes the 11-row showcase from the actual `install --all` target cardinality. Actual: it conflates them.

## Residual risk and limits

- Darwin/BSD runtime exit observation and signal-before-reap remain UNVERIFIED; the Linux failure is not evidence for those target runtimes.
- The operator-stopped unscoped suite is an UNVERIFIED Scheduler-side-effect incident. Partial output must not be treated as a passing suite.
- No performance acceptance criterion was supplied for this correctness-only delta. Focused runtime was 25.315 s; this is a test duration, not a performance pass.

## Gate decision

**REVISE.** Four R35 criteria have focused passing evidence, but the Linux PID-1 integration test fails under the required execution shape and the `install --all` documentation cardinality is false. Repair both, then re-run the Linux PID-1 fixture and the focused affected packages; provide Darwin/BSD target evidence or retain that platform gap explicitly.

## Terms and Abbreviations

- PID 1: the namespace init process identifier; it has special child-reaping behavior on Linux.
- R35: the thirty-fifth bot-review correction round for PR #591.
- WSL: Windows Subsystem for Linux.
- PASS: sufficient evidence for the stated criterion.
- REVISE: a confirmed defect requires correction before acceptance.
- UNVERIFIED: no valid terminal evidence was obtained.

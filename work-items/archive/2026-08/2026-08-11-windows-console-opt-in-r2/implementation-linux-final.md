# Native Linux correction — Windows console opt-in R2

Recorded: 2026-08-11T13:30:41Z

## Gate

**PASS for the assigned native-Linux lane.** The three deterministic `internal/api` failures and the five unengineered `internal/process` timing participants were reproduced and diagnosed on native Ubuntu under Windows Subsystem for Linux (WSL), corrected at their owning test/fixture seams, and passed the required native-Linux normal, race, common non-command-line-interface, vet, and build gates.

This lane changes no production source, wire shape, application programming interface, command-line interface, configuration, schema, dependency, or runtime behavior. Native macOS remains parked by the operator because no target exists. Policy B continues to park the pre-existing broad `internal/cli` lifecycle baseline.

## Receiving-side echo

- **Execution role:** backend engineer using the bug-hunting diagnostic discipline.
- **Assigned scope:** close the native WSL Linux release gate only at the owning `internal/api` and `internal/process` test/fixture seams.
- **Accepted evidence:** `qa-final-r2.md`, verified SHA-256 `7C76D591EA1207F7F11FEBB1E314986E99197DC3AB77EA5E2F2AA0E9347223DD`; `implementation-platform-final.md`; `work-items/bugs/2026-08-11-native-linux-release-gate-red.md`.
- **Policy boundary:** Policy B remains unchanged; broad `internal/cli` work is outside this lane.
- **Operator narrowing:** native macOS execution is parked because no target machine exists; Darwin compilation is evidence only and is not called native execution.
- **Forbidden side effects honored:** no PR worktree access; no live-fleet mutation; no install, restart, stage, commit, push, publication, Sandbox launch, or visible console.

## Repository orientation

The live mutable owners are `internal/api` and `internal/process`; the normal validation entry points are the repository's Go test, race, vet, and build commands. `README.md:82-100`, `status.md:12-24`, and `qa-final-r2.md:82-98` establish the live workflow, active Phase-F gate, protected PR worktrees, Policy B boundary, and exact native-Linux participants.

CodeGraph existed and reported a fresh index before diagnosis and after the edits. It was scoped to tracked source; scratch snapshots and diagnostics were not indexed.

## Target environment

| Field | Observed target value |
| --- | --- |
| Runtime | WSL2 Ubuntu, Linux `6.18.33.2-microsoft-standard-WSL2`, `x86_64` |
| Go | Directly probed native Go 1.26.2 binary; `go1.26.2 linux/amd64`; `GOTOOLCHAIN=local` |
| Controlled ambient input | `GOMAXPROCS=28`, `TZ=UTC`, `LANG=C`, `LC_ALL=C` |
| Source/cache class | Disposable source plus Go caches on WSL ext4 under exact task-owned `/tmp` roots |
| Evidence | Small raw receipts under `/.scratch/windows-console-contract/linux-final/` |

## Pre-fix diagnostics and verified roots

No product or test edit was made until the exact participants were reproduced and the owner mechanism was runtime-proven.

| Failure class | Native RED / diagnostic evidence | Verified cause chain | Owner-level correction |
| --- | --- | --- | --- |
| Persisted strict-mode state write | Exact API shard failed because the write unexpectedly succeeded. The diagnostic recorded `strict-before mode=0755` and `strict-after mode=0700 writeErr=<nil>`. | On Portable Operating System Interface (POSIX) systems, `DaemonStateDir()` owns and hardens the state directory to `0700` before `writeHubMcpStateFile` applies the strict policy. The cross-platform test incorrectly required the Windows reject outcome on POSIX despite the owner having repaired the directory. | Keep one shared scenario and dispatch the expected result through platform test helpers: Windows preserves rejection/no-output; POSIX requires successful hardening plus directory `0700` and output `0600`. Production logic is unchanged. |
| Zed migration | Exact API shard returned `Applied=[]`. Diagnostic seeded `.../Zed/settings.json`; live resolution returned lowercase `.../zed/settings.json`, which did not exist. | The fixture duplicated Windows casing instead of asking the existing client path owner. Linux is case-sensitive. | Seed through `clients.ConfigPathForName("zed")`, the same owner the live adapter uses. |
| Supervisor reconcile socket | Exact dry-run and apply cases failed to bind. The observed derived pathname was 111–114 bytes; the production validator reported the 107-byte usable bound. | Go folds the long test identity into `t.TempDir()`. The roundtrip test name, not the production socket derivation, overflowed Linux `sockaddr_un.sun_path`. | Shorten the roundtrip test identity. The production path-limit validator and its separate contract coverage remain unchanged. |
| Five process timing participants | The first exact rerun passed 5/5, so rerun-to-green was rejected. Diagnostics then engineered delayed healthy starts/streams: local 2-second and 1-second observer clocks expired before the runner later completed. The R21 probe measured 784.289 ms total versus 20.648 ms post-start cleanup. | Test-local wall clocks measured scheduler/start setup as if it were the lifecycle contract. They did not create a deterministic contract window. | Run the fixture-owned goroutine/time contracts under Go `testing/synctest`; do not lengthen sleeps or production timeouts. Keep real subprocess join behavior separate from virtual budget accounting. |
| Linux settlement sibling exposed by the full gate | `TestLinuxGroupSettlementDeadlineJoinsCloseFailure` reported a negative remaining reserve although the helper was joined. | One assertion mixed the virtual settlement reserve with real subprocess join overhead. | Split it into a deterministic fake-classifier reserve contract and a real Linux helper join/close-failure contract. |

Raw root receipts:

- `20260811-api-red.txt`
- `20260811-api-root-diagnostics.txt`
- `20260811-socket-root-diagnostics.txt`
- `20260811-process-baseline.txt`
- `20260811-process-root-diagnostics.txt`

All are under `/.scratch/windows-console-contract/linux-final/`.

## Changed files

Exactly nine test files changed in this lane:

| Owner | File | Change |
| --- | --- | --- |
| API | `internal/api/hub_mcp_state_test.go` | Make the persisted strict-mode scenario platform-policy aware. |
|  | `internal/api/state_file_helper_windows_test.go` | Pin the Windows reject/no-output outcome. |
|  | `internal/api/state_file_helper_posix_test.go` | Pin POSIX hardening and `0700`/`0600` modes. |
|  | `internal/api/migrate_test.go` | Resolve the Zed fixture path through the client owner. |
|  | `internal/api/supervisor_ipc_reconcile_client_test.go` | Keep the roundtrip fixture below the Unix-socket pathname bound. |
| Process | `internal/process/contained_stream_test.go` | Add the standard-library `testing/synctest` fixture helper and deterministic stream-order coverage. |
|  | `internal/process/contained_stream_posix_test.go` | Move POSIX lifecycle timing assertions into deterministic virtual time. |
|  | `internal/process/contained_stream_linux_test.go` | Move Linux timing assertions into virtual time and separate reserve accounting from real helper joining. |
|  | `internal/process/r21_bot_regressions_test.go` | Make the cleanup-deadline participant deterministic under virtual time. |

There is no new dependency: `testing/synctest` is from the Go 1.26 standard library.

## RED to GREEN

| Class | RED evidence | GREEN evidence |
| --- | --- | --- |
| Exact API participants | 3 top-level failures plus both reconcile subtests in `20260811-api-red.txt`. | All three corrected top-level tests and both reconcile subtests passed in 0.070 s; `20260811-api-green.txt`. |
| Original process participants | First rerun passed 5/5 in 1.577 s but remained unverified until the timing mechanism was proven. | Five repetitions, 25 participant executions, passed in 27.983 s; `20260811-process-green-count5.txt`. |
| Settlement sibling | Full common gate exposed the mixed-clock reserve assertion. | Both split owner contracts passed in 0.166 s; `20260811-process-sibling-green.txt`. |

An attempted 25-repetition process run is excluded: its test binary printed `PASS`, but the outer command was terminated before a terminal wrapper result. It is not used as acceptance evidence.

## Native WSL verification

All commands below used the target Go 1.26.2 runtime and controlled environment described above.

| Gate | Exact scope | Result |
| --- | --- | --- |
| Affected normal packages | `go test -count=1 -timeout 12m` over `./internal/api` and `./internal/process` | Both packages PASS: API 96.788 s; process 11.193 s. The process result is preserved in the initial combined receipt; API was rerun after completing the initially incomplete ext4 snapshot. |
| Full affected race | `go test -race -count=1 -timeout 15m ./internal/api ./internal/process` | Exit 0: API 178.856 s; process 60.083 s; `20260811-wsl-affected-race.txt`. |
| Platform common non-CLI | `go test -count=1 -timeout 12m ./cmd/mcphub ./internal/process ./internal/gui ./internal/api ./internal/binaryadmission` | Exit 0: command 1.076 s; process 8.560 s; GUI 43.727 s; API 95.779 s; binary admission 0.013 s; `20260811-wsl-common-non-cli-final.txt`. |
| Linux vet | `go vet ./cmd/mcphub ./internal/cli ./internal/process ./internal/gui ./internal/api ./internal/binaryadmission` | Exit 0. |
| Linux build | `go build ./cmd/mcphub ./internal/cli ./internal/process ./internal/gui ./internal/api ./internal/binaryadmission` | Exit 0; `20260811-wsl-vet-build.txt`. |

The linker emitted the already-observed warning that one generated object lacks `.note.GNU-stack`; the build and tests completed successfully. Earlier source-snapshot attempts missing repository fixtures are excluded as setup failures and are not product evidence.

## Windows and compile-only regression checks

| Gate | Result |
| --- | --- |
| Windows affected normal | `go test -count=1 -timeout 12m ./internal/api ./internal/process`: exit 0; API 253.448 s, process 37.729 s. |
| Windows changed-process race | Five repetitions of the two Windows-buildable changed process participants: exit 0; 1.375 s. |
| Windows vet | `go vet ./internal/api ./internal/process`: exit 0. |
| Darwin compile-only | `internal/api` and `internal/process` compiled with `CGO_ENABLED=0` for `darwin/amd64` and `darwin/arm64`: 4/4 exit 0. |
| Linux arm64 compile-only | Both affected packages compiled with `CGO_ENABLED=0`: 2/2 exit 0. |

Compile-only evidence used `go1.26.5 windows/amd64` and is recorded in `20260811-cross-compile.txt`. It is portability evidence, not native macOS execution.

The full Windows affected race command passed `internal/api` in 277.058 s, then the unchanged `internal/process/start_with_job_windows_test.go:136` baseline crashed under `checkptr` in `TestPrepareWindowsCommand_EnvironmentParity` after 126.536 s. This lane does not edit that owner. The changed process slice passes the Windows race detector, so the adjacent baseline is recorded as a residual rather than hidden or relaxed.

## All-return-path and contract review

- No production return path changed.
- Persisted strict-mode coverage now follows both real platform paths: Windows rejects and emits no file; POSIX owner hardens then writes with the required modes.
- Zed test setup now follows the same single client-path owner as production instead of duplicating platform casing.
- Supervisor socket production validation, error behavior, and path-limit coverage are untouched; only the unrelated roundtrip fixture identity changed.
- Process start, stream, cancellation, cleanup, settlement, classifier, and join implementations are untouched. Virtual time applies only to fixture-owned lifecycle windows; the real Linux helper join remains exercised separately.
- No sleep, timeout, retry, skip, expected-failure marker, fallback, or swallowed error was added.

## Diff, publication, and cleanup

| Check | Result |
| --- | --- |
| Scoped `git diff --check` | PASS for all nine changed files. |
| Stale executable test references | The three replaced test names are absent from `internal/`, `cmd/`, scripts, build entry points, and workflows. Historical QA/bug prose remains valid provenance. |
| Stage / commit / push / publication | Not performed; forbidden in this lane. The integration owner must form and scan the exact stage. |
| Temporary diagnostic Go files | Removed from both the workspace scratch area and WSL source snapshot before final verification. |
| Cross-compile binaries | Removed after recording sizes and exit results; only the small receipt remains. |
| WSL ext4 source/cache roots | Exact resolved task-owned roots removed. The Go module cache required restoring owner-write permission before deletion because downloaded module directories are read-only by design. |
| Live resources | No listener, hub process, installed artifact, scheduled task, fleet state, or visible console was created or changed. |

## Residuals and next gate

| Residual | Disposition |
| --- | --- |
| Native macOS execution | Parked by explicit operator decision because no machine exists; not claimed. |
| Broad `internal/cli` lifecycle | Parked under Policy B; outside this lane. |
| Windows full `internal/process -race` `checkptr` crash | Adjacent unchanged baseline at `start_with_job_windows_test.go:136`; changed-slice race is PASS. Track/reproduce in its owning lane if the release gate requires the entire Windows package race run. |
| `.note.GNU-stack` linker warning | Pre-existing warning; no observed gate failure. |
| Publication/live installation | Still owned by Lead after independent QA and a fresh exact-stage publication scan. |

The next gate is independent QA of this Linux correction. The assigned implementation lane itself is **PASS**.

## Terms and Abbreviations

- **API** — application programming interface.
- **CLI** — command-line interface.
- **GUI** — graphical user interface.
- **POSIX** — Portable Operating System Interface family used by Linux and macOS.
- **WSL** — Windows Subsystem for Linux.
- **RED / GREEN** — failing pre-fix evidence / passing post-fix evidence.
- **PASS / REVISE / BLOCKED** — accepted gate / correction required / external dependency required.
- **Policy B** — the operator-approved parking of the pre-existing broad CLI lifecycle baseline for this console-only release.

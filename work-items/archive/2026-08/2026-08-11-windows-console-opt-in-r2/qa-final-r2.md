# Final independent QA — Windows console opt-in R2

## Gate

**PASS** — this report supersedes the earlier `BLOCKED` conclusion in this same canonical file. The approved nine-file native-Linux test correction has independent causal RED-to-GREEN proof on native Ubuntu under Windows Subsystem for Linux 2 (WSL2), including fresh normal and race checks. Fresh Windows exact, affected-normal, changed-slice race, and vet checks pass. The broad Linux Serena race failure and broad Windows process-race `checkptr` crash are both classified against immutable `HEAD` evidence as unchanged adjacent baseline defects, not failures of this correction.

This PASS advances the candidate to integration-owned exact staging and publication-safety review. It does **not** authorize or claim staging, commit, push, publication, installation, restart, deployment, Windows Sandbox execution, or live-fleet verification.

## Receiving-side echo

- **Role:** independent quality assurance (QA), re-verifying the native-Linux correction and binding the existing Windows/platform evidence to current bytes.
- **Policy:** Policy B waives only the characterized pre-existing broad `internal/cli` lifecycle baseline.
- **Operator decision:** native macOS is explicitly parked; native Linux through WSL2 remains required and is verified here.
- **Forbidden side effects:** product/test/fixture edits; staging, commit, push, install, restart, deploy, Sandbox launch, visible-console launch, pull-request worktree access, or live-fleet mutation.
- **Evidence root:** `.scratch/windows-console-contract/qa-linux-reverify-20260811-1638/`.

## Criterion preconditions

These were written before the re-verification commands ran.

| Criterion | What would this criterion let pass? | Required evidence |
| --- | --- | --- |
| Input binding | A substituted handoff or hidden production change. | Accepted artifact hashes match; the correction is exactly nine test files and has no production, wire, application programming interface (API), configuration, or dependency delta. |
| Causal RED | A green-only rewrite or rerun-to-green. | Every original API/process failure class reproduces from immutable `HEAD` or a targeted mutation that preserves the actual failing condition. |
| Native Linux | Cross-compilation or partial package coverage. | WSL2 Ubuntu Go 1.26.2 exact, affected normal/race, common non-command-line-interface packages, vet, and build are explicit PASS/FAIL/UNVERIFIED results. |
| Timing determinism | A naturally fast run that misses the original window. | The process start delay is engineered; any new timing red is also reproduced under the real condition or classified flaky/UNVERIFIED. |
| Test integrity | Skips, expected failures, longer sleeps, or virtual time hiding a real operating-system helper. | No new skip/expected-failure marker or sleep inflation; `testing/synctest` stays on in-memory fixtures; a real Linux helper join remains exercised. |
| Windows adjacency | Linux-only green while Windows behavior regresses. | Fresh exact/normal/changed-slice race/vet checks on Windows; full race residual classified from immutable baseline evidence. |
| Publication inventory | A clean partial patch that omits intended files or includes unrelated work. | Exact correction and product-slice inventories, diff hygiene, and publication scan are clean; no index mutation. |
| Isolation | Validation that changes a pull-request worktree, installed hub, live fleet, or console state. | Scratch-only disposable sources/caches and an explicit side-effect audit. |

## Input integrity and correction boundary

| Input | Expected SHA-256 | Fresh result |
| --- | --- | --- |
| `implementation-linux-final.md` | `9577F6EE65D6B85756D62A06D73C84C56797E2A332BDCB7D842594C7D6B2C0BF` | MATCH |
| `implementation-platform-final.md` | `DE486A7205E552ADB61430FEED7FF8CF072ECFDCDEAC58815F9683C019DCFB6E` | MATCH |
| `reliability-cli-prerequisites.md` | `C52BF8ADFEA00E1BB90A7A217BF2CA42A04C172E9CE29C915C13456726BFACC8` | MATCH |
| Prior `qa-final-r2.md`, before this superseding update | `7C76D591EA1207F7F11FEBB1E314986E99197DC3AB77EA5E2F2AA0E9347223DD` | MATCH at re-verification start |

The correction is exactly 9 files, 250 insertions, and 193 deletions:

1. `internal/api/hub_mcp_state_test.go`
2. `internal/api/state_file_helper_windows_test.go`
3. `internal/api/state_file_helper_posix_test.go`
4. `internal/api/migrate_test.go`
5. `internal/api/supervisor_ipc_reconcile_client_test.go`
6. `internal/process/contained_stream_test.go`
7. `internal/process/contained_stream_posix_test.go`
8. `internal/process/contained_stream_linux_test.go`
9. `internal/process/r21_bot_regressions_test.go`

Fresh `git diff --check HEAD -- <nine files>` passed. All nine are `_test.go`; no production file, module manifest, generated wire shape, command-line contract, or package metadata changed in this correction.

## Native WSL2 environment and snapshot construction

- Native runtime: Ubuntu on WSL2, Linux `6.18.33.2-microsoft-standard-WSL2`, amd64.
- Toolchain: Go `1.26.2 linux/amd64`, `GOTOOLCHAIN=local`, `GOMAXPROCS=28`, `TZ=UTC`, `LANG=C`, `LC_ALL=C`.
- Source/cache path class: WSL2 ext4 below `/tmp/mcphub-qa-linux-r2-20260811-1638`; repository scratch contains only scripts, patches, and redacted receipts.
- Candidate arm: immutable copy of current product bytes.
- RED arm: the same copy with all nine correction files restored from immutable `HEAD`; the process class additionally has one targeted 2.1-second pre-start delay.
- Binding checks before execution: candidate equals current worktree for 9/9 files; RED equals immutable `HEAD` for 9/9 files before the targeted process mutation.
- The first excluded setup attempt copied gitignored runtime material, ran no tests, was terminated by exact process group, and its exact ext4 root was removed before the admitted run.

Setup and binding receipts: `prepare-wsl-sources.sh`, `verify-and-mutate-red.sh`, `red-environment.txt`, and `process-delay-red.patch` in the evidence root.

## Causal RED controls

The exact commands are preserved in `run-red.sh`; their substantive forms were:

```text
go test -count=1 -timeout 5m -p=1 ./internal/api -run '<three original API tests>' -v
go test -count=1 -timeout 5m -p=1 ./internal/process -run '<five original process tests>' -v
```

| Class | RED result | Exact symptom | Wall time | Receipt |
| --- | --- | --- | ---: | --- |
| API, immutable `HEAD` tests | 3/3 top-level FAIL; 2/2 reconcile subtests FAIL | strict write unexpectedly succeeded; Zed `Applied=[]`; Unix socket bind `invalid argument` | 69 s, including cold compile | `red-api.txt` |
| Process, immutable `HEAD` tests plus targeted pre-start delay | 5/5 FAIL | three start waits hit 2.00-second deadline; stream not observed; R21 wait exceeded bound at 2.270497899 s | 10 s | `red-process.txt` |

This is causal RED evidence. No acceptance result below depends on rerunning a naturally flaky test until green.

## Native Linux GREEN verification

Exact command text, environment, outer timeout, and ordering are preserved in `run-green.sh` and `run-green-exact-race.sh`.

| Check | Result | Count / package detail | Wall time | Receipt |
| --- | --- | --- | ---: | --- |
| Exact corrected API, normal | PASS | 3/3 top-level plus 2/2 reconcile subtests | 58 s | `green-exact-api.txt` |
| Exact corrected process, normal `-count=5` | PASS | 25/25 participant executions | 11 s | `green-exact-process-count5.txt` |
| Virtual-clock reserve plus real Linux helper join | PASS | 2/2 | 1 s | `green-exact-process-sibling.txt` |
| Affected normal | PASS | `internal/api`, `internal/process` | 97 s | `green-affected-normal.txt` |
| Exact corrected API, race | PASS | 3/3 top-level plus 2/2 subtests | 3 s | `green-exact-api-race.txt` |
| Exact corrected process, race `-count=5` | PASS | 25/25 participant executions | 8 s | `green-exact-process-race-count5.txt` |
| Virtual-clock reserve plus real Linux helper join, race | PASS | 2/2 | 2 s | `green-sibling-race.txt` |
| Common non-CLI normal | PASS | `cmd/mcphub`, process, GUI, API, binary admission: 5/5 packages | 159 s | `green-common-non-cli.txt` |
| Vet | PASS | `cmd/mcphub`, CLI, process, GUI, API, binary admission: 6/6 packages | 10 s | `green-vet.txt` |
| Build | PASS | same 6/6 packages | 8 s | `green-build.txt` |

The broad command

```text
go test -race -count=1 -timeout 15m ./internal/api ./internal/process
```

finished in 252 seconds with `internal/process` PASS and one API failure: unchanged `TestWakeIdleSerenaDaemon_StatusPIDNotLiveFailsAfterPolling` reported `calls=0`. Every corrected class was clean. Receipt: `green-affected-race.txt`.

### Serena race residual classification

The Serena production owner and test are byte-identical to immutable `HEAD`:

| File | Current blob | `HEAD` blob | Result |
| --- | --- | --- | --- |
| `internal/api/serena_idle_shutdown.go` | `a33bc787835449e49cd5770c68b857ef75babc38` | same | identical |
| `internal/api/serena_idle_shutdown_test.go` | `d11899f5b225520dea4a593d9d7b966f5d1e039e` | same | identical |

Source-byte equality was not used as the sole oracle. The real failure condition is that the test starts a 140-millisecond caller deadline before the wake's state-read/clear/reconcile work; if that pre-readiness work consumes the budget, `runSerenaWakeReadiness` returns before the status seam is invoked, producing `calls=0`.

QA preserved that condition by adding the same 220-millisecond delay to the test's reconcile seam in disposable candidate and full immutable-`HEAD` ext4 snapshots, then ran the exact test with `-race` under the same runtime, path class, cache class, ordering, and load policy:

| Arm | Result | Symptom | Test time | Wall time | Receipt |
| --- | --- | --- | ---: | ---: | --- |
| Current candidate | FAIL | `calls=0` | 0.24 s | 49 s, cold compile | `serena-current-engineered.txt` |
| Immutable `HEAD` | FAIL | `calls=0` | 0.24 s | 31 s, cold compile | `serena-head-engineered.txt` |

Patch and runner: `serena-pre-readiness-delay.patch`, `run-serena-ab.sh`. Classification: **pre-existing deterministic test-budget fragility / adjacent baseline**, not a regression from the nine-file correction. The broad race command remains honestly red; the corrected Linux slice is independently race-green.

## Test-integrity review

- No added `t.Skip`, expected-failure marker, or sleep-based retry exists in the nine-file diff.
- `testing/synctest` appears only in `internal/process/contained_stream_test.go` and wraps in-memory `scriptedContainedChild`/pipe fixtures.
- Existing assertion deadlines remain fail-fast guards. Virtualized timers drive fixture clocks; they do not extend real wall-clock sleeps.
- `TestLinuxClassifierDeadlineJoinsCloseFailure` stays outside `synctest`, launches the real Linux classifier helper, and freshly passed normal and race checks with a non-nil `ProcessState`, proving the helper was joined.
- No production behavior, wire shape, API surface, command-line behavior, environment contract, or dependency changed in the correction.

## Fresh Windows verification

Native Windows toolchain: `go version go1.26.5 windows/amd64`. Exact commands are preserved in `run-windows-gates.ps1`.

| Check | Result | Count / package detail | Wall time | Receipt |
| --- | --- | --- | ---: | --- |
| Exact corrected API, normal | PASS | 3/3 top-level plus 2/2 subtests | 1.477 s | `windows-exact-api.txt` |
| Exact corrected process, normal `-count=5` | PASS | 10/10 participant executions | 0.594 s | `windows-exact-process-count5.txt` |
| Affected normal | PASS | API and process, 2/2 packages | 161.368 s | `windows-affected-normal.txt` |
| Exact corrected API, race | PASS | 3/3 top-level plus 2/2 subtests | 20.218 s | `windows-exact-api-race.txt` |
| Exact corrected process, race `-count=5` | PASS | 10/10 participant executions | 5.351 s | `windows-exact-process-race-count5.txt` |
| Vet | PASS | API and process, 2/2 packages | 1.368 s | `windows-vet.txt` |

The accepted full Windows affected-race receipt remains red only because `TestPrepareWindowsCommand_EnvironmentParity` reaches a `checkptr` crash at `internal/process/start_with_job_windows_test.go:136`. Its owner blob is identical to `HEAD` (`831fa5d0b9b92ac111696cd6ef53edfaed0a03b4` current and `HEAD`), the API package passed in 277.058 seconds, and the changed process slice is freshly race-green. Receipt: `.scratch/windows-console-contract/linux-final/20260811-windows-affected-race.txt`. Classification: unchanged adjacent baseline residual; it was not widened into this correction.

## Reused platform evidence

The nine-file correction changes tests only, so it does not invalidate the accepted platform artifact's production/package bytes. Bound by the matching platform artifact hash, QA reuses its passed evidence for:

- focused Windows console/process/CLI/GUI guards;
- Windows amd64 and arm64 release-shaped builds plus Portable Executable subsystem admission;
- npm tests, version synchronization, generated-package freshness, and package payload shape;
- six-target cross-build/package checks;
- accepted Windows Sandbox target evidence.

No Sandbox was launched in this lane.

## Native macOS operator park

Fresh direct read-only probe, 2.785 seconds:

| Surface | Result |
| --- | --- |
| GitHub Actions self-hosted runners | authenticated query succeeded; total `0` |
| Local secure shell (SSH) target | executable present; no SSH config |
| Session environment bindings | 0 names matching macOS/Darwin/runner |
| Workflow | 0 native macOS jobs; 2 Darwin cross-build lines |

Receipt: `macos-availability.txt`. Native macOS remains unavailable, but the operator explicitly parked it for this delivery; it is recorded as **PARKED**, not `BLOCKED`, and cross-builds are not misrepresented as native execution.

## Publication and inventory readiness

The exact correction inventory is the nine files listed above. The current intended product slice is mechanically defined as:

1. all tracked product changes relative to `HEAD`, excluding `work-items/**` and the unrelated `internal/api/port_alloc_excluded_windows.go` change: 63 tracked paths;
2. all untracked non-`work-items/**` product paths: 20 paths;
3. total product inventory: 83 paths.

Every path is enumerated in `product-inventory.txt`. The product patch is `current-product.patch`. A fresh scan with the installed publication-safety scanner covered that exact patch plus all 20 untracked product files: **21/21 targets PASS, 0 FAIL, 28.311 seconds**. Receipts: `publication-scan-current-product.txt` and `publication-scan-summary.txt`.

This is a product-slice publication-safety PASS, not a stage-level PASS. The work-item/report/bug/read-model selection is integration-owned and shares `work-items/README.md` with unrelated lifecycle changes. No safe whole-file stage was inferred or created. The integration owner must select the exact canonical work-item files, stage once, and run a fresh scan over the exact staged bytes before commit or push.

## Acceptance matrix

| Criterion | Verdict | Evidence |
| --- | --- | --- |
| Input hashes and nine-file boundary | PASS | All accepted hashes match; 9 files, 250 insertions, 193 deletions; test-only. |
| API causal RED-to-GREEN | PASS | Original 3/3 plus 2/2 subtest reds reproduced; current normal/race exact checks pass. |
| Process causal RED-to-GREEN | PASS | Engineered delay reproduces 5/5 originals; current normal and race are each 25/25. |
| Native Linux affected normal | PASS | API and process pass in 97 seconds. |
| Native Linux corrected race slice | PASS | API 3/3 plus subtests, process 25/25, sibling joins 2/2. |
| Native Linux broad race | PASS with baseline residual | Corrected classes clean; only unchanged Serena deadline-budget test red, deterministically symmetric on candidate/HEAD. |
| Native Linux common/vet/build | PASS | 5/5 common packages; 6/6 vet; 6/6 build. |
| Test determinism/integrity | PASS | No skips/expected failures/sleep inflation; virtual time limited to fixtures; real helper join exercised. |
| Windows current-byte adjacency | PASS | Exact, affected normal, changed-slice race, and vet all green. |
| Windows full process race | PASS with baseline residual | Unchanged owner reaches accepted `checkptr` baseline; changed slice fresh PASS. |
| Packaging/PE/npm/cross-build | PASS, reused | Production/package bytes unchanged from accepted platform artifact. |
| Native macOS | PARKED | Direct probe confirms no target; operator explicitly parked it. |
| Product-slice publication safety | PASS | Exact 83-path inventory; 21/21 scanner targets clean. |
| Exact final stage | PENDING INTEGRATION | No stage mutation; exact canonical work-item selection and fresh staged-byte scan still required. |
| Side-effect isolation | PASS | Scratch-only evidence; no forbidden operational mutation. |

## Residual risk and next gate

| Residual | Classification | Required owner action |
| --- | --- | --- |
| Serena 140-millisecond race-test budget can expire before readiness polling | Pre-existing adjacent test fragility; not introduced here | Track in its test-owner lane if broad Linux race must be globally green. Do not treat a rerun-to-green as closure. |
| Windows `start_with_job_windows_test.go:136` checkptr crash | Accepted unchanged baseline | Track in its process-test owner lane if full Windows process race must be globally green. |
| Native macOS execution | Operator-parked availability gap | Restore only when a native target is supplied or policy changes. |
| Exact stage/publication/install/live verification | Deliberately unperformed | Integration owner selects/stages exact files, scans staged bytes, obtains human publication approval, then installs/restarts and verifies the live no-visible-console contract. |

The existing Linux release-gate bug record is now `fixed` because the user approved the correction and this independent gate verified it. The earlier CodeGraph scratch-index restart record remains separate; this run queried tracked source only, created no in-repository source snapshot, and did not reproduce or exercise that trigger.

## Side-effect statement

PR #598 and PR #600 worktrees were untouched and unaccessed. The live hub fleet, installed binaries, scheduler/supervisor state, npm installation, git index, branches, remotes, Windows Sandbox, and visible console state were not mutated. No stage, commit, push, publication, install, restart, deploy, or interactive console launch occurred.

After all receipts were copied, the validated exact ext4 root `/tmp/mcphub-qa-linux-r2-20260811-1638` was made owner-writable only to remove Go's read-only module-cache entries, deleted recursively, and verified absent. Repository-local scratch receipts were retained for the integration owner.

## Terms and Abbreviations

- **API** — Application Programming Interface.
- **CLI** — Command-Line Interface.
- **GUI** — Graphical User Interface.
- **HEAD** — the checked-out Git commit used as immutable baseline.
- **PASS / FAIL / PARKED / PENDING** — accepted, failed, explicitly deferred by the operator, or awaiting the named next owner.
- **PE** — Portable Executable, the Windows binary format.
- **QA** — Quality Assurance.
- **WSL2** — Windows Subsystem for Linux 2.

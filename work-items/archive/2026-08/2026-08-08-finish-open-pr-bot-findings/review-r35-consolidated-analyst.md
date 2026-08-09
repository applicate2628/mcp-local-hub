# PR 591 R35 consolidated factual analysis

Baseline: PR #591, base `51ac193f7e2c1d49404a4609b31de20fd06566da`, published head `c363e5845ebda802932d57b23757d68a98cddcf8`, plus the current uncommitted R35 delta. Candidate paths below are relative to `.scratch/pr591-bot-r9`.

## Files & symbols

| Surface | Current owner and evidence |
|---|---|
| Embedded entry | `[static-read]` `vcpkgmcp.NewCommand` owns the hidden `mcphub vcpkg` command and delegates to `vcpkgserver.Run` (`internal/vcpkgmcp/cmd.go:26-35`). The root command registers it with the other embedded commands (`internal/cli/root.go:141-153`). |
| MCP server and wire registration | `[static-read]` `vcpkgserver.Run` creates a registered server and serves it over standard input/output (`internal/vcpkgmcp/vcpkgserver/server.go:61-71`). `registerProjectableTool` owns schema validation and callback registration (`internal/vcpkgmcp/vcpkgserver/tools.go:98-120`); the seven public tool names are declared at `internal/vcpkgmcp/vcpkgserver/tools.go:172,202,318,349,371,440,478`. |
| CMake include graph | `[static-read]` `cmakegraph.Walk` / `WalkTree` are the graph owners (`internal/cmakegraph/cmakegraph.go:615,673`). R35 changes `includeArgumentOptional` so only exact `RESULT_VARIABLE` is recognized (`internal/cmakegraph/cmakegraph.go:2019-2029`). |
| Process containment | `[static-read]` `RunContainedStream` owns the lifecycle (`internal/process/contained_stream.go:235-249`); `posixContainedChild.terminateBy` owns group signal, settlement, and adopted-descendant reap order (`internal/process/contained_stream_other.go:99-124`); Windows process creation is owned by `windowsContainedChild.start` (`internal/process/contained_stream_windows.go:54-155`). |
| Root discovery | `[static-read]` `DiscoverRoot` owns ordered root selection and makes the explicit tier terminal (`internal/vcpkgmcp/discovery/discovery.go:248-304`). |
| Pin remote admission | `[static-read]` `approveRemoteURL` is the sole constructor of the `approvedRemoteURL` execution capability (`internal/vcpkgmcp/pinstatus/redact.go:143-195`); `defaultRemoteRefs` rechecks that capability before constructing `git ls-remote` (`internal/vcpkgmcp/pinstatus/remote.go:61-86`). |
| Installation catalogue | `[static-read]` `InstallAllWithOpts` enumerates embedded manifest names and skips only workspace-scoped manifests (`internal/api/install.go:286-330`). The candidate contains 16 manifest files, while `configs/ports.yaml:1-46` enumerates 15 global server rows. |
| Review delta | `[runtime-verified]` Fresh `git status --short` showed 7 modified tracked paths and 7 untracked R35 paths. Fresh `git diff --stat 51ac193...` showed 290 changed paths across the cumulative PR; fresh `git diff --check` returned no diagnostics. No test or build command was run in this analyst lane. |

## Flows

1. `[static-read]` `servers/vcpkg/manifest.yaml:1-8,32-58` maps the global `vcpkg` server to `mcphub vcpkg` on port 9138 and client bindings. `internal/cli/root.go:141-153` registers the command, and `internal/vcpkgmcp/cmd.go:26-35` delegates to the MCP server.
2. `[static-inference; ASSUMPTION (UNVERIFIED)]` `vcpkgserver.Run` installs MCP callbacks through `mcp.Server.AddTool`, after which the external SDK dispatches requests to package handlers (`internal/vcpkgmcp/vcpkgserver/server.go:61-71`; `internal/vcpkgmcp/vcpkgserver/tools.go:106-120`). Resolving probe: one target-environment MCP initialize plus one call to each registered tool, with received method names and result envelopes captured.
3. `[static-read]` A pin-status remote passes from parsed port metadata through `approveRemoteURL`, the private proof-bearing value, `transportArgument`, `defaultRemoteRefs`, and `RunContainedStream` (`internal/vcpkgmcp/pinstatus/redact.go:143-195`; `internal/vcpkgmcp/pinstatus/remote.go:61-109`).
4. `[static-read]` On Linux and BSD-family targets, `child.wait` first observes exit without calling `cmd.Wait`, then `terminateBy` sends `SIGKILL` to the process group, starts the single reaper, settles descendants, and `reapBy` returns the final wait result (`internal/process/contained_stream_wait_deferred_posix.go:15-75`; `internal/process/contained_stream_other.go:99-124`; `internal/process/contained_stream.go:395-418`). Linux observation uses `waitid(..., WNOWAIT)` (`internal/process/contained_stream_wait_linux.go:12-23`); Darwin/BSD observation uses `kqueue` `EVFILT_PROC/NOTE_EXIT` (`internal/process/contained_stream_wait_kqueue.go:12-44`).
5. `[static-read]` A regular-file explicit root is now rejected by statting the root object before a child executable path is constructed (`internal/vcpkgmcp/discovery/discovery.go:258-276`).

## Contracts

- `[static-read]` The accepted product contract is read-only diagnostics, evidence-bearing results, and tri-state `ok | failed | unknown(reason)` (`work-items/decisions/2026-07-25-vcpkg-mcp-tool-contracts.md:26-36`; `servers/vcpkg/README.md:3-20`).
- `[static-read]` The explicit-root tier is terminal and distinguishes invalid, unreadable, and relative inputs (`internal/vcpkgmcp/discovery/discovery.go:73-83,248-304`; `servers/vcpkg/README.md:53-68`).
- `[static-read]` R35's six reported contracts are represented in the current delta: exact CMake keyword case (`internal/cmakegraph/cmakegraph.go:2019-2029`), user-optional SCP syntax (`internal/vcpkgmcp/pinstatus/redact.go:263-287`), signal-before-reap on macOS/BSD (`internal/process/contained_stream_wait_deferred_posix.go:15-75`), `CREATE_NO_WINDOW` with redirected handles (`internal/process/contained_stream_windows.go:117-160`), regular-file explicit-root rejection (`internal/vcpkgmcp/discovery/discovery.go:258-276`), and catalogue prose edits (`README.md:75-137,206-211`).
- `[runtime-verified + static-read]` The revised installation guidance is still factually inconsistent. `README.md:103-117` says `mcphub install --all` covers 11 servers. The current candidate has 16 embedded manifests; `InstallAllWithOpts` skips only workspace-scoped manifests (`internal/api/install.go:299-325`), and `configs/ports.yaml:1-46` lists the 15 global rows. Thus 11 is the visible table-row count, not the `install --all` target count.
- `[static-read]` Two dependent current surfaces retain the prior count/pattern: `docs/architecture-highlights.md:15-21` says 10 manifests and three dual-entry embedded servers, and `internal/api/install.go:246-250` says the embedded install source contains 10 servers.

## Tests & coverage

- `[static-read]` The exact-case CMake regression is pinned at `internal/cmakegraph/r35_bot_regressions_test.go:8-17`; explicit-root file classification at `internal/vcpkgmcp/discovery/r35_bot_regressions_test.go:12-34`; user-optional SCP admission and local-path separation at `internal/vcpkgmcp/pinstatus/r35_bot_regressions_test.go:9-77`; and Windows no-console plus redirected stream behavior at `internal/process/r35_no_console_windows_test.go:15-41`.
- `[static-read]` The deferred-reap unit test verifies injected observation followed by an exact-one reaper start (`internal/process/r35_deferred_wait_posix_test.go:11-43`). It does not invoke the real Linux `waitid`, BSD `kqueue`, or the complete `RunContainedStream` lifecycle.
- `[static-read]` Confirmed test-contract regression: the existing PID-1 Linux zombie integration test directly constructs `&posixContainedChild{cmd: command}` without calling `initializePlatformContainedWait` (`internal/process/contained_stream_linux_test.go:303-323`). Production construction initializes `waitObserved` and `reapDone` (`internal/process/contained_stream_other.go:46-49`; `internal/process/contained_stream_wait_deferred_posix.go:10-13`), while the new wait rejects nil `waitObserved` (`internal/process/contained_stream_wait_deferred_posix.go:15-24`) and `reapBy` rejects nil `reapDone` (`internal/process/contained_stream_wait_deferred_posix.go:54-57`). When its PID-1 precondition is met, that test no longer exercises the production deferred-wait state it claims to cover.
- `[ASSUMPTION (UNVERIFIED)]` Real Darwin/BSD `kqueue` exit observation, signal-before-reap ordering, and final exit-code propagation have no target-runtime evidence in this analyst lane. Resolving probe: run the real descendant-survival/cancellation scenario on one Darwin target and one BSD target and require the group signal to precede `cmd.Wait`, both process identities to disappear, and the final lifecycle result to retain the direct child's exit state.
- `[runtime-verified]` The analyst lane intentionally ran no tests/builds, per its read-only assignment; therefore repository test success is not claimed here.

## Similar implementations

- `[static-read]` Linux and Darwin/BSD share the deferred-reap state machine (`internal/process/contained_stream_wait_deferred_posix.go:10-75`) but differ only in the non-reaping exit observer (`internal/process/contained_stream_wait_linux.go:12-23`; `internal/process/contained_stream_wait_kqueue.go:12-44`).
- `[static-read]` AIX, illumos, and Solaris retain direct `cmd.Wait` plus a no-op deferred-reap capability (`internal/process/contained_stream_wait_posix.go:1-17`). The R35 GitHub finding names macOS and BSD, so those fallback targets are outside that finding's stated platform set.
- `[static-read]` Windows keeps process identity stable through a Job Object and explicit process handle, and R35 changes only the creation flags helper (`internal/process/contained_stream_windows.go:15-27,117-185`).
- `[static-read]` The shipped vcpkg command follows the same root registration seam as other embedded Go commands but intentionally has no standalone `cmd/vcpkg` executable (`internal/vcpkgmcp/cmd.go:1-17`; `README.md:135-137`).

## Constraints

- `[runtime-verified]` CodeGraph was invoked first, but the candidate lookup resolved through a branch-unsafe parent index and returned unrelated GUI/API symbols; it also reported several candidate files pending synchronization. No CodeGraph fact is load-bearing in this memo. Exact candidate Git bytes are authoritative.
- `[runtime-verified]` The lane was restricted to read-only inspection: no edits to candidate code, tests, builds, commits, pushes, process operations, or GitHub comments occurred. The only write is this required memo.
- `[static-read]` The cumulative PR is 290 paths / roughly 49k added lines, while R35 is a 14-path delta. Conclusions are bounded to the affected owners, callers, contracts, tests, config, and current documentation named above.

### Searched and excluded

- `[runtime-verified]` Historical plans under `docs/superpowers/plans` and old verification snapshots were searched for count/entry wording but excluded as non-current provenance surfaces.
- `[runtime-verified]` `INSTALL.md` matches only an unrelated statement that perftools wraps three tools; it does not drive the vcpkg catalogue conclusion.
- `[runtime-verified]` Root-index CodeGraph GUI/API hits were excluded because they did not describe the candidate branch.
- `[runtime-verified]` A second widening pass covered current install enumeration, manifest/config counts, and the PID-1 integration fixture. It changed the conclusion by confirming the 11-versus-15 contract mismatch and the fixture regression. A final platform-branch pass changed no further conclusion; investigation stopped at saturation.

## Change risks

- `[runtime-verified]` `git log` shows repeated same-surface correction churn: `internal/cmakegraph/cmakegraph.go` has at least 20 PR-lineage edits, including `108b323f`, `b02f9dde`, `b2edfa70`, `20c5f59b`, `67ca8731`, and `3826669d`; `internal/vcpkgmcp/pinstatus/redact.go` has at least 11, including `37320feb`, `0da48820`, `daaccb10`, `eb765f4a`, `852d5706`, and `cdc84926`.
- `[runtime-verified]` Process lifecycle corrections span `cead6b5e`, `b2edfa70`, `cdc84926`, and `49adc902`; the current R35 delta changes the same wait/reap boundary again. The confirmed PID-1 fixture regression is a direct fix-over-fix residue on that boundary.
- `[static-read]` The documentation count has three simultaneous truths: 11 rows in the short README table (`README.md:117-131`), 15 global install targets (`configs/ports.yaml:1-46`), and 16 embedded manifests including the workspace-scoped entry. Wording that treats one count as all three is internally inconsistent.

## Unresolved questions

- `[ASSUMPTION (UNVERIFIED)]` Whether the real BSD-family `kqueue` observer behaves correctly when the child exits before filter registration is not established by candidate tests. Resolving probe: deterministic target-runtime early-exit fixture with observer registration delayed until after child exit.
- `[ASSUMPTION (UNVERIFIED)]` Whether every current CI target includes Darwin and all named BSD cross-compiles was not inspected because this lane stopped after the platform-owner saturation point. Resolving probe: inspect the current PR check matrix and artifact logs for each build tag.
- `[static-read]` The intended user-facing taxonomy for the README's 11-row showcase versus all 15 global install targets is not defined in the current prose; the present command comment states the smaller number as the install-all cardinality (`README.md:103-117`).

## Research admission gates

| Gate | Evidence verdict |
|---|---|
| Regression risk | `REVISE` — `[static-read]` the PID-1 Linux integration fixture omits the newly required deferred-wait initialization and therefore cannot validate its stated production path (`internal/process/contained_stream_linux_test.go:303-323`; `internal/process/contained_stream_wait_deferred_posix.go:10-24,54-57`). |
| Metric alignment | `PASS` — `[static-read]` the reviewed changes target the six exact R35 functional/contract statements, and each has a directly named owner/test surface. |
| Known limits | `REVISE` — `[ASSUMPTION (UNVERIFIED)]` Darwin/BSD correctness is supported by static code and cross-platform build shape only in this lane, not by a target-runtime process-tree oracle. |
| Bounded falsification | `PASS` — the two bounded probes are the PID-1 Linux zombie-owner test after production-equivalent initialization and a deterministic Darwin/BSD early-exit descendant fixture; PASS requires signal-before-reap, no surviving identities, and preserved final wait state. |

## Adjacent findings

- `[static-read]` `README.md:232-233` separately says the project has 15 built-in servers, which agrees with the global config count but conflicts with the earlier phrase that `install --all` means 11. This is the same documentation-count inconsistency, not a separate code mechanism.
- `[static-read]` No additional out-of-scope functional defect was confirmed after the saturation stop.

## Terms and Abbreviations

- MCP: Model Context Protocol.
- PID/PGID: process identifier / process-group identifier.
- R35: the thirty-fifth bot-review correction round for PR #591.
- `PASS`: evidence is sufficient for the stated gate.
- `REVISE`: a confirmed defect or evidence gap must be closed before the gate passes.
- `ASSUMPTION (UNVERIFIED)`: a claim that requires the named empirical probe.

## Gate decision

`REVISE` — the current R35 delta still states a false `install --all` cardinality (11 versus 15 global manifests), and its process-lifecycle change leaves the existing PID-1 Linux integration fixture outside the production initialization path. Darwin/BSD runtime correctness also remains explicitly unverified; no other confirmed functional gap was found within the saturated affected surface.

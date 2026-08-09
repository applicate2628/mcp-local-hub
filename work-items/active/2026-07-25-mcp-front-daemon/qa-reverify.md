# QA re-verification — seven-finding fix round

Target: detached `d6c0501f5866644849423d12583ad4e8f4b0c696`, containing fix
commit `3f72365d534251051190e84e84d7ab6bc0f01cea`.

QA gate: **PASS**. Every required control was run on the baseline and
falsified by its controlled mutation, or was directly observed by a bounded
scratch-only probe. No required mutation survived or was left unrun.

Product closure: **REVISE**. Two runtime-observed product defects remain for
the architecture gate: fresh nil-input failures return a caller-visible
non-nil empty intent, and a broadened-parent read-only route request writes
`hub-mcp.log` plus `hub-mcp.log.lock`.

## Safety and scope

- Every `go test` command targeted exactly one approved package, included an
  anchored `-run`, `-tags=test_state_path_env`, and `-count=1`.
- No whole-package suite and no `go test ./...` command was run.
- Named tests were read before execution. They use `t.TempDir`, the API test
  state-path override, or hardened temporary roots; none starts `mcphub`.
- The loopback fixtures only reserve and immediately close an ephemeral port
  to create a deterministic connection refusal
  (`internal/gui/route_readonly_test.go:155-203`,
  `internal/gui/lsp_router_test.go:1457-1476`,
  `internal/cli/route_i6_readonly_test.go:154-209`).
- Product and transient test mutations were confined to one detached
  worktree under `.scratch/qa-accept-inc1/`. The worktree was returned to
  clean `d6c0501f`, removed, and pruned; evidence:
  `.scratch/qa-accept-inc1/reverify/cleanup-worktree.log`.
- The operator's real state root, live fleet, `supervisor-events.log`, and
  existing `.scratch/qa-accept-inc1/gui-wiring.diff` were not touched.

## Criterion adequacy

| Criterion | What would this criterion let pass? | Tightened acceptance |
|---|---|---|
| Audit-sink wiring | A status-only request test would allow `hub-mcp.log` writes. | Require a complete state-tree snapshot or direct injected-audit observation, then replace the sink and require failure. |
| LSP constructor wiring | Testing only the detached helper would allow `SetLSPRouterReadOnly` to leave `AuditFn` nil. | Test the helper and the constructor as separate controls. |
| Read-only resolver locking | An unchanged registry modification time would bypass the lock/no-lock branch. | Advance the registry modification time, hold the exclusive lock, and bound the command with `-timeout=12s`. |
| Trusted-roots read | Checking only the returned empty store would allow directory creation. | Also assert the redirected state directory remains absent. |
| Seeder persistence ordering | Checking disk only would allow caller-visible intent mutation or a premature success event. | Existing-row failure guard plus fresh nil-input probes must inspect return shape, row, persisted file, failure event, and success event. |
| Reserved identity | A canonical-task-name-only match would allow foreign-row clobber. | Require a loud error and an unchanged row when `Server`/`Daemon` identity is foreign. |
| Descriptor equality | Command/Args/Port-only comparison would allow `Env` or `RuntimeSpec` drift and repeated normalization churn. | Table cases cover nil/empty `Args`, `Env`, and `RuntimeSpec`, then ensure → serialize → reload → ensure must converge. |
| Strict port | An existing row already on the default port would not distinguish strict failure from fallback. | Use an existing custom-port row and a fresh nil host; the fallback mutation must rewrite/add and therefore fail both controls. |
| Whitespace | An uncommitted mutation is invisible to `origin/master..HEAD`. | Commit the one-line mutation only on detached scratch HEAD, run the exact range check, then return the detached worktree to `d6c0501f`. |
| Broadened-parent read | Hardened-parent request tests exclude the known relax-audit branch. | Synthesize a safe temporary broadened Windows directory access-control list, isolate the state root, issue a real read-only Serena request, and require the exact log event. |
| Final cleanliness | Passing mutations alone would not prove the restored tree still builds or has no residue. | Re-run all exact guards on clean HEAD, then `go build ./...`, `go vet ./...`, diff check, clean status, and worktree removal. |

## Baseline and mutation matrix

Counts include Go subtests where present. `xfail=0` for every run; Go has no
xfail result in these commands.

| ID | Baseline | Controlled mutation | Classification | Source and raw evidence |
|---|---|---|---|---|
| M1 Serena route audit sink | 1 pass, 0 fail, 0 skip; 13.249 s; exit 0 | 0 pass, 1 fail, 0 skip; 2.901 s; exit 1; state tree gained `hub-mcp.log` and lock | **HELD** | Control: `internal/gui/serena_router.go:280-298`; guard: `internal/gui/route_readonly_test.go:140-223`; raw: `baseline-01-serena-audit-sink.log`, `mutation-01-serena-shared-audit.log` |
| M2 detached LSP supplied audit | 1 pass, 0 fail, 0 skip; 1.023 s; exit 0 | 0 pass, 1 fail, 0 skip; 3.146 s; exit 1; supplied callback was never called | **HELD** | Control: `internal/gui/lsp_router.go:854-867,873-917`; guard: `internal/gui/lsp_router_test.go:1457-1480`; raw: `baseline-02-lsp-provided-audit.log`, `mutation-02-lsp-hardcoded-audit.log` |
| M3 LSP read-only constructor | 1 pass, 0 fail, 0 skip; 1.031 s; exit 0 | 0 pass, 1 fail, 0 skip; 3.223 s; exit 1; `AuditFn` became nil | **HELD** | Control: `internal/gui/lsp_router.go:155-182`; guard: `internal/gui/lsp_router_test.go:1496-1523`; raw: `baseline-03-lsp-readonly-wiring.log`, `mutation-03-lsp-readonly-wiring-removed.log` |
| M4 Serena unlocked refresh | 1 pass, 0 fail, 0 skip; 0.743 s; exit 0 | 0 pass, 1 fail, 0 skip; 3.893 s; exit 1; deterministic 3 s lock wait | **HELD** | Control: `internal/api/serena_routing/resolver.go:125-188`; guard: `internal/api/serena_routing/resolver_test.go:582-630`; raw: `baseline-04-serena-readonly-lock.log`, `mutation-04-serena-readonly-relocks.log` |
| M5 LSP unlocked refresh | 1 pass, 0 fail, 0 skip; 1.030 s; exit 0 | 0 pass, 1 fail, 0 skip; 3.959 s; exit 1; deterministic 3 s lock wait | **HELD** | Control: `internal/api/lsp_routing/resolver.go:319-368`; guard: `internal/api/lsp_routing/resolver_test.go:316-358`; raw: `baseline-05-lsp-readonly-lock.log`, `mutation-05-lsp-readonly-relocks.log` |
| M6 trusted-roots read accessor | 1 pass, 0 fail, 0 skip; 5.186 s; exit 0 | 0 pass, 1 fail, 0 skip; 6.392 s; exit 1; redirected state directory appeared | **HELD** | Control: `internal/api/lsp_trusted_roots.go:90-121,205-220`; guard: `internal/api/lsp_trusted_roots_test.go:73-90`; raw: `baseline-06-trusted-roots-readonly.log`, `mutation-06-trusted-roots-creates-state.log` |
| M7 persist-before-adopt | Permanent guard: 1 pass; 8.760 s; exit 0. Fresh nil probe: 3 passes including subtests; 3.530 s; exit 0. | 1 pass, 3 fails including subtests; 5.009 s; exit 1; old row changed and fresh persist failure returned a route row | **HELD** | Ordering: `internal/cli/supervise.go:2636-2671,2674-2714`; guard: `internal/cli/builtin_route_daemon_test.go:96-132`; raw: `baseline-07-seeder-persist-failure.log`, `scratch-baseline-fresh-nil-seeder.log`, `mutation-07-seeder-adopts-before-persist.log` |
| M8 foreign reserved identity | 1 pass, 0 fail, 0 skip; 1.367 s; exit 0 | 0 pass, 1 fail, 0 skip; 7.125 s; exit 1; foreign row was accepted/replaced with `err=nil` | **HELD** | Control: `internal/api/builtin_route_daemon.go:128-147`; guard: `internal/api/builtin_route_daemon_test.go:114-139`; raw: `baseline-08-foreign-reserved-name.log`, `mutation-08-foreign-identity-guard-removed.log` |
| M9 whole descriptor equality | Permanent controls: 1+1 passes; 0.898 s + 0.912 s. Scratch table: 5 passes including subtests; 4.645 s; exit 0; all four forms converged after one serialization. | 2 passes, 3 fails including subtests; 6.009 s; exit 1; empty `Env` and allocated empty `RuntimeSpec` were silently accepted | **HELD** | Equality owner: `internal/api/builtin_route_daemon.go:154-158`; fields: `internal/api/supervisor_intent.go:66-110`; raw: `baseline-09-own-row-full-compare.log`, `baseline-09b-server-daemon-drift.log`, `scratch-baseline-descriptor-convergence.log`, `mutation-09-partial-descriptor-compare.log` |
| M10 strict startup port | Existing-row guard: 1 pass; 1.122 s; exit 0. Fresh nil guard: 1 pass; 8.485 s; exit 0. | 0 pass, 2 fails; 4.856 s; exit 1; existing custom port rewrote to 9137 and fresh host persisted a new row with success event | **HELD** | Strict seam: `internal/cli/supervise.go:2592-2625`; guard: `internal/cli/builtin_route_daemon_test.go:146-199`; raw: `baseline-10-strict-port-existing-row.log`, `scratch-baseline-fresh-nil-port.log`, `mutation-10-lenient-port-fallback.log` |
| M11 final blank line | Diff check: no test counts; 0.036 s; exit 0 | Exact `git diff --check origin/master..HEAD`: 0.039 s; exit 2; reported trailing whitespace and new blank line at EOF | **HELD** | Current EOF: `internal/gui/route_readonly_test.go:223`; raw: `baseline-13-diff-check.log`, `mutation-11-trailing-blank-line.log` |
| P12 broadened-parent adjacent path | Scratch request: 1 pass, 0 fail, 0 skip; 4.540 s; exit 0; HTTP 502 and exact low-level event written to isolated `hub-mcp.log` | No product mutation needed; the probe directly requires the defect to occur | **PRODUCT DEFECT PROVED** | Hardcoded writer: `internal/api/hub_mcp_state_read_inode_windows.go:116-143`; raw: `scratch-adjacent-broadened-parent-request.log` |

All raw paths above are under `.scratch/qa-accept-inc1/reverify/`.

## Exact mutation commands

M1:

`go test -tags=test_state_path_env -count=1 -timeout=2m -run '^TestSetSerenaRouterReadOnly_RegisteredWorkspaceUnreachableBackend_NoSharedStateFileWrite$' -v ./internal/gui`

M2:

`go test -tags=test_state_path_env -count=1 -timeout=2m -run '^TestForwardLSPNotificationDetached_UsesProvidedAuditFnNotHardcodedLogHubMcpEvent$' -v ./internal/gui`

M3:

`go test -tags=test_state_path_env -count=1 -timeout=2m -run '^TestSetLSPRouterReadOnly_WiresRouteReadOnlySinkAsAuditFn$' -v ./internal/gui`

M4:

`go test -tags=test_state_path_env -count=1 -timeout=12s -run '^TestNewReadOnlyWorkspaceResolver_NeverBlocksOnConcurrentExclusiveLock$' -v ./internal/api/serena_routing`

M5:

`go test -tags=test_state_path_env -count=1 -timeout=12s -run '^TestNewReadOnlyWorkspaceResolver_NeverBlocksOnConcurrentExclusiveLock$' -v ./internal/api/lsp_routing`

M6:

`go test -tags=test_state_path_env -count=1 -timeout=2m -run '^TestLoadDefaultLSPTrustedRoots_DoesNotCreateStateDirectory$' -v ./internal/api`

M7 baseline:

`go test -tags=test_state_path_env -count=1 -timeout=2m -run '^TestEnsureBuiltinRouteDaemonAtStartup_PersistFailurePreservesExistingRow$' -v ./internal/cli`

M7 scratch baseline:

`go test -tags=test_state_path_env -count=1 -timeout=2m -run '^TestQAReverifyFreshNilSeederFailureSemantics$' -v ./internal/cli`

M7 mutation:

`go test -tags=test_state_path_env -count=1 -timeout=2m -run '^(TestEnsureBuiltinRouteDaemonAtStartup_PersistFailurePreservesExistingRow|TestQAReverifyFreshNilSeederFailureSemantics)$' -v ./internal/cli`

M8:

`go test -tags=test_state_path_env -count=1 -timeout=2m -run '^TestEnsureBuiltinRouteDaemon_ForeignRowCollisionRejectedLoudly$' -v ./internal/api`

M9:

`go test -tags=test_state_path_env -count=1 -timeout=2m -run '^TestQAReverifyBuiltinRouteDescriptorNilEmptyAndConvergence$' -v ./internal/api`

M10 existing-row baseline:

`go test -tags=test_state_path_env -count=1 -timeout=2m -run '^TestEnsureBuiltinRouteDaemonAtStartup_PortResolutionFailurePreservesExistingRow$' -v ./internal/cli`

M10 fresh-host baseline:

`go test -tags=test_state_path_env -count=1 -timeout=2m -run '^TestQAReverifyFreshNilStrictPortFailureDistinguishesLenientFallback$' -v ./internal/cli`

M10 mutation:

`go test -tags=test_state_path_env -count=1 -timeout=2m -run '^(TestEnsureBuiltinRouteDaemonAtStartup_PortResolutionFailurePreservesExistingRow|TestQAReverifyFreshNilStrictPortFailureDistinguishesLenientFallback)$' -v ./internal/cli`

M11 baseline and detached-mutation check:

`git diff --check origin/master..HEAD`

P12:

`go test -tags=test_state_path_env -count=1 -timeout=2m -run '^TestQAReverifyBroadenedParentReadOnlyRouteWritesHubLog$' -v ./internal/gui`

## Fresh-host observations

| Failure path | Returned nil | Route row | Intent file | Failure event | Success event | Evidence |
|---|---:|---:|---:|---:|---:|---|
| Strict-port failure, nil input | No | No | No | Yes | No | `.scratch/qa-accept-inc1/reverify/scratch-baseline-fresh-nil-seeder.log` |
| Persist failure, nil input | No | No | No | Yes | No | `.scratch/qa-accept-inc1/reverify/scratch-baseline-fresh-nil-seeder.log` |
| Lenient-fallback mutation, nil input | No | Yes | Yes | No | Yes | `.scratch/qa-accept-inc1/reverify/mutation-10-lenient-port-fallback.log` |

The first two rows expose a current implementation defect. The function says
resolution or persistence failure returns the original possibly-nil intent
unchanged (`internal/cli/supervise.go:2561-2568`), but it allocates a new intent
before strict resolution and persistence (`internal/cli/supervise.go:2588-2605`)
and returns that non-nil allocation on both failure paths
(`internal/cli/supervise.go:2625,2656-2671`). No route row, durable intent file,
or success event is produced, but caller-visible nilness changes.

## Descriptor convergence

The transient table covered:

- `Args=nil`;
- `Args=[]string{}`;
- `Env=map[string]string{}`;
- `RuntimeSpec=&DaemonRuntimeSpec{}`.

Baseline `reflect.DeepEqual` canonicalized all four, and every
ensure → JSON serialize → reload → ensure cycle reported `changed=false` on
the second ensure. The prior Command/Args/Port-only comparison mutation missed
the empty `Env` and allocated empty `RuntimeSpec` cases, so the scratch guard
failed exactly those subtests. Evidence:
`.scratch/qa-accept-inc1/reverify/scratch-baseline-descriptor-convergence.log`
and `.scratch/qa-accept-inc1/reverify/mutation-09-partial-descriptor-compare.log`.

## Atomic publication invariant

The unlocked-reader premise remains supported by the single registry writer:
`Registry.Save` delegates to `WriteStateFileBytesLockHeld`
(`internal/api/workspace_registry.go:158-189`), which delegates to the hardened
state writer (`internal/api/state_file_helper.go:107-115`), and the platform
writer publishes by same-directory rename with post-rename verification
(`internal/api/secure_write_client_config.go:68-89`). This QA phase did not run
a broad writer-inventory or concurrent stress suite because both were outside
the admitted exact-test scope. Architecture review must retain the analyst's
writer-inventory premise as a residual static-evidence dependency.

## Final clean-HEAD verification

| Check | Counts | Wall time | Exit | Raw |
|---|---:|---:|---:|---|
| Exact `internal/gui` alternation | 3 pass, 0 fail, 0 skip | 1.532 s | 0 | `.scratch/qa-accept-inc1/reverify/final-01-gui-exact.log` |
| Exact `internal/api` alternation | 5 pass, 0 fail, 0 skip | 1.104 s | 0 | `.scratch/qa-accept-inc1/reverify/final-02-api-exact.log` |
| Exact Serena routing guard | 1 pass, 0 fail, 0 skip | 0.703 s | 0 | `.scratch/qa-accept-inc1/reverify/final-03-serena-routing-exact.log` |
| Exact LSP routing guard | 1 pass, 0 fail, 0 skip | 1.461 s | 0 | `.scratch/qa-accept-inc1/reverify/final-04-lsp-routing-exact.log` |
| Exact `internal/cli` alternation | 4 pass, 0 fail, 0 skip | 1.589 s | 0 | `.scratch/qa-accept-inc1/reverify/final-05-cli-exact.log` |
| `go build ./...` | n/a | 1.554 s | 0 | `.scratch/qa-accept-inc1/reverify/final-06-go-build-all.log` |
| `go vet ./...` | n/a | 1.760 s | 0 | `.scratch/qa-accept-inc1/reverify/final-07-go-vet-all.log` |
| `git diff --check origin/master..HEAD` | n/a | 0.056 s | 0 | `.scratch/qa-accept-inc1/reverify/final-08-diff-check.log` |
| Detached HEAD/status | clean `d6c0501f` | n/a | 0 | `.scratch/qa-accept-inc1/reverify/final-09-detached-clean-status.log` |
| Worktree remove + prune | removed | n/a | 0 | `.scratch/qa-accept-inc1/reverify/cleanup-worktree.log` |
| Main product diff reconciliation | unchanged | n/a | 0 | `.scratch/qa-accept-inc1/reverify/main-worktree-reconciliation.log` |

Final exact test total: **14 pass, 0 fail, 0 skip, 0 xfail**.

## Defects, gaps, and residual risk

1. **Product defect — broadened-parent read writes shared state.** The
   scratch-only Windows request produced HTTP 502 and wrote
   `hub-mcp.log`/`hub-mcp.log.lock` containing
   `hub-mcp-state-read-unhardened-parent-fallback`. The write is hardcoded at
   `internal/api/hub_mcp_state_read_inode_windows.go:135-142`; runtime evidence:
   `.scratch/qa-accept-inc1/reverify/scratch-adjacent-broadened-parent-request.log`.
   This violates the route-daemon zero-shared-write invariant despite all
   route-local audit-sink mutations being held.
2. **Product defect — nil-input failures change caller-visible shape.** Both
   strict-port and persistence failure return a non-nil empty intent from nil
   input. They do not create a row/file or emit success, but the returned
   object is not the original nil value.
3. **No required QA evidence gap.** Every required mutation is classified
   `HELD`; none survived and none was skipped.
4. **Residual static dependency.** Registry-writer completeness and the claim
   that every writer converges on `Registry.Save` were not dynamically
   re-inventoried in this exact-test phase. The architecture reviewer should
   reconcile that premise with `research-reverify.md`.
5. **Basic performance acceptance.** No user-visible performance budget was
   admitted. The two lock-contention mutations failed deterministically near
   the engineered 3 s window, while both clean read-only guards completed in
   0.743-1.461 s including compile/test startup. No performance regression was
   observed in the admitted checks.

## Gate

**PASS for the QA artifact.** The mutation and scratch evidence is complete,
live-fleet-safe, and reproducible from the preserved raw outputs.

**REVISE for product closure.** Architecture review must not close the
read-only contract while the broadened-parent request write remains, and must
decide whether the fresh nil-input return-shape defect is in this fix round or
a separately-owned follow-up.

## Terms and Abbreviations

- **ACL / DACL** — access-control list / discretionary access-control list on Windows.
- **HELD** — the controlled regression made the named guard fail.
- **LSP** — Language Server Protocol.
- **MCP** — Model Context Protocol.
- **PASS** — the assigned QA evidence gate is complete and satisfied.
- **REVISE** — product behavior still needs correction or an explicit ownership decision.
- **xfail** — an expected-failure test result; none was used.

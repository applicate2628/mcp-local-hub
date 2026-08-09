# Plan — seven-finding fix-round re-verification

Gate: operator-direct review scope; product code is read-only.

## Phase 1 — Analyst evidence map

Scope: `git diff origin/master..HEAD` plus unchanged callers/writers needed to
prove the seven claims.

- AC1: Enumerate every request path mounted by `RouteHandler()` and every
  reachable filesystem mutation: registry, supervisor intent, trusted roots,
  `hub-mcp.log`, lock files, and directory creation.
- AC2: Classify the documented low-level
  `hub-mcp-state-read-unhardened-parent-fallback` emit as reachable/not-reachable
  and in-scope/out-of-scope against the READ-ONLY invariant in
  `work-items/decisions/2026-07-25-mcp-data-plane-off-gui-onto-supervised-front-daemon.md:54-57`.
- AC3: Enumerate every writer to the workspace registry and prove its
  publication mode (atomic rename or non-atomic).
- AC4: Enumerate all seeder return paths and order persist, adopt, and event.
- AC5: Audit reserved-row identity and whole-descriptor equality, including
  nil-vs-empty slices/maps and hidden/time fields.
- AC6: Trace strict port-resolution failure with an existing row and on a
  genuinely fresh host with no row.
- AC7: Verify the trailing-blank-line claim with `git diff --check
  origin/master..HEAD`.

Evidence: current source `file:line`, `git show`, and commit history only.
Rollback: none; read-only phase.

## Phase 2 — QA baseline guards

Before each run: the criterion must fail if its named control is removed.
Every command targets one package and uses an anchored `-run` filter.

```powershell
go test ./internal/gui -count=1 -run '^(TestSetSerenaRouterReadOnly_RegisteredWorkspaceUnreachableBackend_NoSharedStateFileWrite|TestForwardLSPNotificationDetached_UsesProvidedAuditFnNotHardcodedLogHubMcpEvent|TestSetLSPRouterReadOnly_WiresRouteReadOnlySinkAsAuditFn)$'
go test ./internal/api/serena_routing -count=1 -run '^(TestNewReadOnlyWorkspaceResolver_NeverBlocksOnConcurrentExclusiveLock|TestNewWorkspaceResolver_BlocksOnConcurrentExclusiveLock)$'
go test ./internal/api/lsp_routing -count=1 -run '^(TestNewReadOnlyWorkspaceResolver_NeverBlocksOnConcurrentExclusiveLock|TestNewWorkspaceResolver_BlocksOnConcurrentExclusiveLock)$'
go test ./internal/api -count=1 -run '^(TestLoadDefaultLSPTrustedRoots_DoesNotCreateStateDirectory|TestEnsureBuiltinRouteDaemon_ForeignRowCollisionRejectedLoudly|TestEnsureBuiltinRouteDaemon_ServerDaemonDriftIsNotSilentlyAcceptedAsCanonical|TestEnsureBuiltinRouteDaemon_OwnRowFullCanonicalCompare|TestResolveMCPFrontPort_ErrorsOnCorruptSettingsFile)$'
go test ./internal/cli -count=1 -run '^(TestEnsureBuiltinRouteDaemonAtStartup_PersistsAndSurvivesReread|TestEnsureBuiltinRouteDaemonAtStartup_PersistFailurePreservesExistingRow|TestEnsureBuiltinRouteDaemonAtStartup_PortResolutionFailurePreservesExistingRow|TestBuildRouteServer_RegisteredWorkspaceUnreachableBackend_NoSharedStateFileWrite)$'
```

- AC8: Each named baseline test passes with exact pass/fail count and raw output
  preserved under `.scratch/qa-accept-inc1/reverify/`.
- AC9: `go build ./...` and `go vet ./...` pass; these are the only broad Go
  commands admitted by the operator.

Rollback: none; tests must use their own temporary state and must not launch
`mcphub`.

## Phase 3 — Controlled mutation proof

Isolation: create a temporary Git worktree under `.scratch/` with the exact
marker required by repository governance; never mutate the operator worktree.
Apply one mutation, run only its exact Phase-2 filter, record expected failure,
then remove that worktree path after verifying the resolved target is inside
this repository's `.scratch/`.

- AC10: Replace the Serena read-only audit sink with `api.LogHubMcpEvent`;
  the registered-workspace state-snapshot test fails.
- AC11: Ignore the supplied LSP `auditFn` or wire the hardcoded shared logger;
  the two LSP audit-wiring tests fail.
- AC12: Restore exclusive-lock acquisition in each read-only resolver; the
  corresponding non-blocking lock-contention test fails.
- AC13: Restore directory-creating trusted-roots path resolution; the
  no-directory-creation test fails.
- AC14: Adopt the returned intent before persistence, or emit success before
  persist; the persist-failure guard fails. If no current test distinguishes
  event ordering, record the mutation as surviving and finding 2 cannot close.
- AC15: Remove the reserved-row identity guard; the foreign-collision test
  fails. Weaken equality to the former under-compare; the drift/canonical
  tests fail. Add a nil/empty equivalent-form mutation/probe if the production
  descriptor contains a slice or map.
- AC16: Restore lenient default-port fallback; the corrupt-settings startup
  guard fails. Add/run a fresh-host corrupt-settings case; if absent or green
  under the mutation, finding 5 cannot fully close.
- AC17: Reintroduce the trailing blank line; `git diff --check` fails.

Every mutation result must state `held` (expected failure) or `survived`
(test remained green), with exact command and raw-output path.

## Phase 4 — Independent architecture re-review

Adversarial strategy. Inputs: analyst memo, QA report, implementation diff.

- AC18: Reconcile all seven original findings explicitly.
- AC19: Anti-layering verdict per defect class:
  `CLEAN-SINGLE-OWNER`, `JUSTIFIED-DEPTH`, or `PILED`.
- AC20: Judge the adjacent hardcoded low-level audit write against the
  route-daemon READ-ONLY contract; prior “out of approved implementer surface”
  does not by itself make the defect out of review scope.
- AC21: Reject unlocked reload if any registry writer is non-atomic.

## Role sequence and final gate

`$analyst` -> `$qa-engineer` -> `$architecture-reviewer` -> lead reconciliation.

Final `PASS` requires AC1-AC21, seven `CLOSED` dispositions, no surviving
read-only-control mutation, and no request-reachable shared-state write.
Otherwise verdict is `REVISE` with blocking findings.

Planner gate: **PASS**.

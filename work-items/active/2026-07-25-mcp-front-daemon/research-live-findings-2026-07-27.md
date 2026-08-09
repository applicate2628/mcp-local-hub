# Research — PR #588 live finding classification

Date: 2026-07-27  
Role: `$analyst`  
Branch: `feat/mcp-front-daemon`  
Remote head: `6c02018b7e58f262c42cba8c577346178d4abe8a`  
Current head: `3872ee1609afba53649ec1b00be09200015c0268`

## Evidence posture

- `runtime-verified` (read-only command output): `git status --short --branch`,
  `git rev-parse`, and `git log` show the branch one commit ahead of
  `origin/feat/mcp-front-daemon`, with `3872ee16` as the only local code
  commit. The only working-tree changes observed before this memo were the
  lead-owned `brief.md`, `roadmap.md`, and `status.md` updates.
- `runtime-verified` (tool output): CodeGraph was invoked first and reported
  that this worktree has no `.codegraph/` index. The Go language-server MCP
  backend then timed out while initializing from a file-scoped diagnostics
  probe. No semantic-MCP result is used as evidence below.
- `static-read`: all behavioral classifications below come from current source,
  tests, `git blame`, `git log -S`, and `origin/feat/mcp-front-daemon..HEAD`.
  Commit messages, including `3872ee16`'s completion and mutation claims, were
  treated as hypotheses rather than evidence.
- No Go test, build, vet, GUI, tray, supervisor, or process was run in this
  analyst stage. Existing test source is a static regression anchor, not
  fresh execution evidence.
- The operator message contains **14 verbatim finding rows**, although
  `brief.md:5` says thirteen. The fourteen rows deduplicate to the expected ten
  defect classes.

## Row-by-row classification

| Row | Reported location and title | Class | Current state | Current evidence |
| --- | --- | --- | --- | --- |
| 1 | `internal/cli/install_reconcile_mcp_front.go:343` — Preserve legacy LSP entries in the rollback snapshot | C1 | **REAL, open** | `static-read`: `3872ee16` makes the API snapshot emit canonical and legacy rows (`internal/api/lsp_client_router_snapshot.go:137-178`), but the CLI artifact merge still keys rows only by client and language (`internal/cli/install_reconcile_mcp_front.go:1245-1249`). |
|  |  |  |  | The canonical row is appended first (`internal/api/lsp_client_router_snapshot.go:137-151`), so every same-language legacy row collides and is dropped by the merge (`internal/cli/install_reconcile_mcp_front.go:1225-1230`). |
| 2 | `internal/cli/install_reconcile_mcp_front.go:823` — Track the latest port written by forward retries | C2 | **REAL, open** | `static-read`: the record has one global `Port` (`internal/cli/install_reconcile_mcp_front.go:237-245`), and rollback supplies it to every LSP row (`internal/cli/install_reconcile_mcp_front.go:573-584`). |
|  |  |  |  | A retry publishes that global port before LSP mutation (`internal/cli/install_reconcile_mcp_front.go:450-486`), while LSP writes can fail per client (`internal/api/lsp_client_router.go:1033-1055`). A client left on port A is therefore judged against port B. |
| 3 | `internal/cli/install.go:174` — Reject `--check` before dispatching reconcile mode | C3 | **ALREADY FIXED** | `static-read`: commit `3872ee16`; the top-of-`RunE` gate rejects `--check` with every mutating mode at `internal/cli/install.go:107-121`, before dispatches at `internal/cli/install.go:130-195`. |
| 4 | `internal/cli/install_reconcile_mcp_front.go:404` — Refuse to overwrite changed Serena entries during rollback | C4 | **REAL, open** | `static-read`: `3872ee16` added a fingerprint CAS, but a fingerprint failure records `Recorded=false` (`internal/cli/install_reconcile_mcp_front.go:998-1013`). |
|  |  |  |  | Rollback explicitly treats missing/error baselines as “unjudgeable” and continues (`internal/cli/install_reconcile_mcp_front.go:1111-1131`) into the unconditional restore (`internal/cli/install_reconcile_mcp_front.go:545-550`; `internal/api/serena_client_reconcile.go:561-584`). The active generation therefore does not prove ownership on every restore path. |
| 5 | `internal/cli/install_reconcile_mcp_front.go:290` — Serialize the complete reconcile recovery transaction | C5 | **ALREADY FIXED** | `static-read`: commit `3872ee16`; `runReconcileMCPFront` acquires the dedicated operation lock before either forward or rollback dispatch and releases it after the selected path returns (`internal/cli/install_reconcile_mcp_front.go:315-369`). |
| 6 | `internal/cli/install_reconcile_mcp_front.go:343` — Verify an LSP route before rewriting LSP clients | C6 | **ALREADY FIXED** | `static-read`: commit `3872ee16`; the total preflight checks the LSP lifecycle (`internal/cli/install_reconcile_mcp_front.go:395-412`) before snapshot or mutation (`internal/cli/install_reconcile_mcp_front.go:424-465`). |
| 7 | `internal/cli/route.go:201` — Run session expiration inside the route daemon | C7 | **ALREADY FIXED** | `static-read`: commit `3872ee16`; route construction returns its Serena and LSP stores (`internal/cli/route.go:183-254`), and `runRoute` starts context-bound cleanup for those stores (`internal/cli/route.go:273-301`). |
| 8 | `internal/cli/install_reconcile_mcp_front.go:684` — Mark Serena rows applied only after the rewrite succeeds | C8 | **REAL, open** | `static-read` plus `static-inference`: the callback assignment is `journal.recordSerenaBackup` (`internal/cli/install_reconcile_mcp_front.go:461-469`), and the callback persists the row under `MigrateReport.Applied` before the client write (`internal/cli/install_reconcile_mcp_front.go:880-925`). |
|  |  |  |  | `ReconcileSerenaClientsToRouter` invokes the callback before `AddEntry` and appends its own Applied row only after success (`internal/api/serena_client_reconcile.go:463-487`, `internal/api/serena_client_reconcile.go:525-527`). `commit` checks successful rows but never removes write-ahead-only rows (`internal/cli/install_reconcile_mcp_front.go:975-987`). |
| 9 | `internal/cli/install.go:176` — Reject `--check` before running front reconciliation | C3 | **ALREADY FIXED** | Duplicate of row 3; same commit, production gate, and test anchor. |
| 10 | `internal/cli/install_reconcile_mcp_front.go:822` — Refuse to merge reports across different front ports | C2 | **REAL, open** | Duplicate class of row 2. `3872ee16` chose a global latest port rather than refusal or per-row applied ownership (`internal/cli/install_reconcile_mcp_front.go:1177-1197`); partial cross-port retries remain representable in live state but not in the artifact. |
| 11 | `internal/cli/install_reconcile_mcp_front.go:298` — Serialize the complete front-reconcile transaction | C5 | **ALREADY FIXED** | Duplicate of row 5; the one wrapper encloses both forward and rollback (`internal/cli/install_reconcile_mcp_front.go:352-369`). |
| 12 | `internal/api/lsp_client_router_snapshot.go:200` — Keep absent LSP rows pending while their client is unreachable | C9 | **REAL, open** | `static-read`: an unreachable client reports Pending only for `restorable()` rows (`internal/api/lsp_client_router_snapshot.go:236-253`), while `restorable()` excludes every `Present=false` row (`internal/api/lsp_client_router_snapshot.go:72-83`). |
|  |  |  |  | Rollback blocks retirement only on Pending or Failed (`internal/cli/install_reconcile_mcp_front.go:595-612`). The existing test explicitly requires the unsafe absent row to be non-pending (`internal/api/lsp_client_router_snapshot_review_test.go:170-193`). |
| 13 | `internal/api/lsp_client_router_snapshot.go:124` — Capture legacy LSP entries removed by the forward pass | C1 | **REAL, open** | Duplicate class of row 1. The new direct API tests bypass the CLI journal/merge (`internal/api/lsp_client_router_snapshot_legacy_test.go:106-132`), so they do not detect the artifact dropping the captured legacy row. |
| 14 | `internal/api/lsp_client_router_snapshot.go:116` — Prevent newly appearing clients from bypassing the snapshot | C10 | **REAL, open** | `static-read`: snapshot independently calls `clients.AllClients()` and skips `!Exists()` clients (`internal/api/lsp_client_router_snapshot.go:127-136`); the later mutation independently calls `clients.AllClients()` and rechecks `Exists()` (`internal/api/lsp_client_router.go:194-202`). |
|  |  |  |  | The command passes no captured client map to either call (`internal/cli/install_reconcile_mcp_front.go:442`, `internal/cli/install_reconcile_mcp_front.go:486-489`). `AllClients()` reconstructs adapters on each call (`internal/clients/clients.go:1011-1040`). |

No row is classified `WRONG`.

## Files & symbols

- `static-read` — command dispatch and read-only flag contract:
  `newInstallCmdReal` at `internal/cli/install.go:33-219`.
- `static-read` — durable reconcile report, operation lock, forward/rollback
  lifecycle, Serena journal/CAS, and report merge:
  `internal/cli/install_reconcile_mcp_front.go:220-1249`.
- `static-read` — canonical and legacy LSP snapshot/restore:
  `LSPRouterEntrySnapshot`, `SnapshotLSPRouterClientEntries`,
  `RestoreLSPRouterClientEntriesSnapshot`, and `restorable` at
  `internal/api/lsp_client_router_snapshot.go:61-342`.
- `static-read` — LSP mutation participants and report result classes:
  `EnsureLSPRouterClientEntries`, `ensureLSPRouterClientEntriesWithLoaded`,
  `collectLegacyLSPEntriesToMigrate`, `applyLSPRouterOps`, and
  `lspRouterReportError` at `internal/api/lsp_client_router.go:75-96`,
  `internal/api/lsp_client_router.go:125-259`,
  `internal/api/lsp_client_router.go:754-816`, and
  `internal/api/lsp_client_router.go:1023-1102`.
- `static-read` — Serena write ordering and unconditional restore:
  `SerenaReconcileOpts.OnBackupCaptured`,
  `ReconcileSerenaClientsToRouter`,
  `RestoreSerenaReconcileApplied`, and `SerenaClientEntryFingerprint` at
  `internal/api/serena_client_reconcile.go:308-331`,
  `internal/api/serena_client_reconcile.go:451-530`, and
  `internal/api/serena_client_reconcile.go:561-632`.
- `static-read` — LSP readiness probe:
  `AssertLSPRouterRouteLive`, `routerRouteShapeProbe`, and
  `routerInitializeLifecycleProbe` at
  `internal/api/lsp_router_readiness.go:45-163`.
- `static-read` — route-owned session-store handoff and cleanup:
  `routeSessionStores`, `buildRouteServer`, `runRouteSessionExpiry`, and
  `runRoute` at `internal/cli/route.go:167-301`.
- `static-read` — shared cleanup owners:
  `sessionCleanupInterval`, `runSessionCleanupTicker`, and
  `runLSPSessionCleanupTicker` at `internal/cli/gui.go:1484-1567`;
  Serena and LSP `CleanupWithTTL` implementations at
  `internal/api/serena_routing/session_router.go:133-158` and
  `internal/api/lsp_routing/session_router.go:112-130`.

### Searched and excluded

- `runtime-verified` search control: `rg` found every current caller of
  `SnapshotLSPRouterClientEntries`, `RestoreLSPRouterClientEntriesSnapshot`,
  `EnsureLSPRouterClientEntries`, `collectLegacyLSPEntriesToMigrate`,
  `mergeMCPFrontReconcileReport`, `lspSnapshotKey`,
  `RestoreSerenaReconcileApplied`, and both cleanup tickers.
- `static-read`: `api.RollbackLSPRouterClientEntries` is the separate
  router-to-legacy demotion operation. Current front rollback calls
  `RestoreLSPRouterClientEntriesSnapshot` instead
  (`internal/cli/install_reconcile_mcp_front.go:554-584`), so the demotion
  implementation is not a participant in these live findings.
- `static-read`: `internal/cli/setup.go:78` also calls
  `EnsureLSPRouterClientEntries`, but it has no pre-state recovery artifact and
  is not the front-reconcile forward/rollback transaction.
- `static-read`: `internal/cli/migrate_serena.go:208` also calls
  `RestoreSerenaReconcileApplied`, but it consumes the report from the same
  in-process migrate attempt; the long-lived, cross-command CAS defect is owned
  by the persisted front-reconcile caller.
- `runtime-verified` negative-search control: searches for cross-port partial
  failure, newly appearing client between snapshot/mutation, and a persisted
  Serena AddEntry-failure row found no matching front-reconcile regression
  test. The same searches found the known successful cross-port test, snapshot
  tests, and API-level AddEntry-failure test, proving the test tree and patterns
  were reachable.
- Widening step 1 added direct API/adapter callees and found C9/C10 but no
  eleventh class. Widening step 2 added all symbol callers plus focal
  history/blame and changed no classification. The investigation stopped at
  that saturation point.

## Flows

### Forward reconcile

1. `static-read`: `runReconcileMCPFront` acquires the dedicated lock before
   selecting forward or rollback (`internal/cli/install_reconcile_mcp_front.go:352-369`).
2. `static-read`: forward resolves the port and completes ownership plus Serena
   and LSP route preflight before any recovery artifact or client write
   (`internal/cli/install_reconcile_mcp_front.go:415-445`).
3. `static-read`: the LSP snapshot is captured with a fresh default client map,
   then merged into the in-memory journal (`internal/cli/install_reconcile_mcp_front.go:439-450`).
4. `static-inference`; **ASSUMPTION (UNVERIFIED)**: the interface callback edge
   `SerenaReconcileOpts.OnBackupCaptured -> journal.recordSerenaBackup` follows
   the assignment at `internal/cli/install_reconcile_mcp_front.go:465-469` and
   invocation at `internal/api/serena_client_reconcile.go:469-476`.
   A safe tagged run of
   `TestMCPFrontReview_ClientIsNotMutatedWhenItsRecoveryRowCannotBeDurable`
   would runtime-confirm the edge.
5. `static-read`: `recordSerenaBackup` writes a pinned row under
   `Serena.Applied` before `adapter.AddEntry` executes
   (`internal/cli/install_reconcile_mcp_front.go:889-925`;
   `internal/api/serena_client_reconcile.go:479-487`).
6. `static-read`: `journal.commit` publishes the global latest port and the
   current fingerprint set before `EnsureLSPRouterClientEntries`; per-client
   LSP failures return with that report left active
   (`internal/cli/install_reconcile_mcp_front.go:477-496`).
7. `static-inference`; **ASSUMPTION (UNVERIFIED)**: adapter `Exists`,
   `GetEntry`, `AddEntry`, and `RemoveEntry` calls are interface dispatches.
   The named scoped package tests in “Tests & coverage” would confirm their
   concrete fake-adapter edges.

### Rollback

1. `static-read`: report schema and pinned inputs are checked before writes
   (`internal/cli/install_reconcile_mcp_front.go:519-542`).
2. `static-read`: the Serena CAS checks recorded fingerprints, but
   missing/error baselines are allowed through; restore then replays every
   persisted `Serena.Applied` backup (`internal/cli/install_reconcile_mcp_front.go:543-550`,
   `internal/cli/install_reconcile_mcp_front.go:1100-1137`).
3. `static-read`: all LSP rows receive one global ownership port
   (`internal/cli/install_reconcile_mcp_front.go:573-584`).
4. `static-read`: retirement is blocked by Pending and Failed, but not Skipped
   (`internal/cli/install_reconcile_mcp_front.go:595-623`;
   `internal/api/lsp_client_router.go:1098-1102`).

### Route session lifetime

1. `static-read`: `buildRouteServer` constructs and returns the route process's
   own Serena and optional LSP session routers
   (`internal/cli/route.go:183-254`).
2. `static-inference`; **ASSUMPTION (UNVERIFIED)**: `runRoute` starts the two
   cleanup goroutines through `runRouteSessionExpiry`
   (`internal/cli/route.go:273-301`), and their context-cancellation branches
   are at `internal/cli/gui.go:1511-1524` and
   `internal/cli/gui.go:1557-1567`. A safe tagged run of the three
   `TestRouteDaemon_SessionExpiry*` tests would runtime-confirm expiry and
   cancellation.

## Contracts

| Contract | Current state | Evidence |
| --- | --- | --- |
| Original rollback baseline survives forward retries | **VIOLATED for legacy entry shapes** | Legacy rows are captured at `internal/api/lsp_client_router_snapshot.go:157-178` but collide in `lspSnapshotKey` at `internal/cli/install_reconcile_mcp_front.go:1245-1249`. |
|  |  | Canonical/legacy/multiple-legacy rows share `(client, language)` and the merge drops later rows at `internal/cli/install_reconcile_mcp_front.go:1225-1230`. |
| Rollback changes only state owned by the active generation | **VIOLATED** | Serena unjudgeable rows proceed at `internal/cli/install_reconcile_mcp_front.go:1111-1131`. |
|  |  | Cross-port absent LSP rows can become Skipped at `internal/api/lsp_client_router_snapshot.go:264-281`, and Skipped does not block record retirement at `internal/cli/install_reconcile_mcp_front.go:595-623`. |
| Snapshot and mutation client populations cannot diverge | **VIOLATED** | Independent default maps and independent `Exists()` checks at `internal/api/lsp_client_router_snapshot.go:127-136` and `internal/api/lsp_client_router.go:194-202`. |
| One reconcile transaction owns the report/client lifecycle at a time | **SATISFIED in current code** | Dedicated lock encloses both dispatch paths at `internal/cli/install_reconcile_mcp_front.go:352-369`. |
| `--check` remains read-only | **SATISFIED for the reported combinations** | Top gate at `internal/cli/install.go:107-121` precedes all three mutating mode dispatches. |
| Route process owns cleanup of its in-memory sessions | **SATISFIED in current code** | Store handoff and cleanup start at `internal/cli/route.go:183-301`. |
| Persisted Serena Applied means the rewrite succeeded | **VIOLATED** | Write-ahead journal stores an Applied row at `internal/cli/install_reconcile_mcp_front.go:897-924`; the actual AddEntry succeeds or fails later at `internal/api/serena_client_reconcile.go:479-487`. |
| Unreachable recovery rows keep the record active when work may remain | **VIOLATED for absent pre-state rows** | `Present=false` is excluded by `restorable()` at `internal/api/lsp_client_router_snapshot.go:72-83` and silently omitted from Pending at `internal/api/lsp_client_router_snapshot.go:239-253`. |

## Tests & coverage

No test below was executed in this analyst run. “Anchor” means current test
source that directly falsifies a production branch when run.

### Commit-to-code/test closure map

| Class | Commit claim | Production closure status | Exact current test / evidence gap |
| --- | --- | --- | --- |
| C1 legacy LSP | `3872ee16` | **Not closed.** API capture/restore was added, but CLI persistence collapses rows by `(client, language)` (`internal/cli/install_reconcile_mcp_front.go:1225-1249`). | `TestSnapshotLSPRouterClientEntries_CapturesLegacyPerWorkspaceEntries` and `TestMCPFrontLegacyLSP_ForwardThenRollbackRestoresTheLegacyEntry` at `internal/api/lsp_client_router_snapshot_legacy_test.go:45-145` bypass `mergeMCPFrontReconcileReport`. |
|  |  |  | Existing merge test treats a second same-language entry as an illegal duplicate (`internal/cli/install_reconcile_mcp_front_pr588_test.go:487-500`). No CLI artifact round-trip guard exists. |
| C2 latest port | `3872ee16` | **Not closed.** One global latest port cannot describe a partial A-to-B retry (`internal/cli/install_reconcile_mcp_front.go:237-245`, `internal/cli/install_reconcile_mcp_front.go:573-584`). | `TestMCPFrontR2_RerunAtANewPortRecordsTheLatestPort` requires the entire B forward run to succeed (`internal/cli/install_reconcile_mcp_front_pr588_r2_test.go:155-201`). No partial-failure cross-port case exists. |
| C3 check dispatch | `3872ee16` | **Closed in current code** by the top gate (`internal/cli/install.go:107-121`). | `TestMCPFrontR2_CheckWithReconcileMutatesNothing` (`internal/cli/install_reconcile_mcp_front_pr588_r2_test.go:102-149`). Static falsifier: remove/relocate the top gate and the test reaches the real dispatch, changing config bytes or creating the report. |
| C4 Serena CAS | `3872ee16` | **Partial, not closed.** Recorded fingerprints are checked, but absent/error baselines fail open (`internal/cli/install_reconcile_mcp_front.go:998-1013`, `internal/cli/install_reconcile_mcp_front.go:1111-1131`). | `TestMCPFrontR2_RollbackRefusesAnOperatorEditedSerenaEntry` covers only a successfully recorded baseline (`internal/cli/install_reconcile_mcp_front_pr588_r2_test.go:203-245`). No unjudgeable-baseline conflict case exists. |
| C5 operation lock | `3872ee16` | **Closed in current code** by one wrapper lock around both paths (`internal/cli/install_reconcile_mcp_front.go:352-369`). | `TestMCPFrontR2_SecondInvocationRefusesWhileTheTransactionLockIsHeld` (`internal/cli/install_reconcile_mcp_front_pr588_r2_test.go:248-281`). Static falsifier: bypass the acquire and the held flock no longer prevents the second invocation. |
| C6 LSP readiness | `3872ee16` | **Closed in current code** by total preflight (`internal/cli/install_reconcile_mcp_front.go:395-412`). | `TestMCPFrontR2_ForwardRefusesWhenOnlyTheSerenaRouteIsLive` (`internal/cli/install_reconcile_mcp_front_pr588_r2_test.go:63-100`). Static falsifier: remove `AssertLSPRouterRouteLive`; the Serena-only server passes the remaining gate and a recovery artifact can be created. |
| C7 route expiry | `3872ee16` | **Closed in current code** by store handoff and the route-owned cleanup start (`internal/cli/route.go:183-301`). | `TestRouteDaemon_SessionStoresAreReachableForExpiry`, `TestRouteDaemon_SessionExpiryActuallyReclaimsBoundSessions`, and `TestRouteDaemon_SessionExpiryStopsWithContext` (`internal/cli/route_session_expiry_test.go:24-117`). |
|  |  |  | Static falsifier: stop returning either store or remove the call at `internal/cli/route.go:301`; the structure or actual-expiry guard fails. |
| C8 Serena promotion | `6c02018b` write-ahead claim; not claimed separately by `3872ee16` | **Not closed.** The durable row is still placed in `Serena.Applied` before AddEntry (`internal/cli/install_reconcile_mcp_front.go:897-925`). | `TestSerenaClientReconcile_LegacyEndpointRemovedOnlyAfterRewriteSuccess` proves only the API report excludes failed AddEntry (`internal/api/serena_client_reconcile_test.go:790-833`). No persisted-journal AddEntry-failure test exists. |
| C9 absent/unreachable | `6c02018b` Pending claim | **Not closed for absent rows.** | `TestSnapshotRestore_UnreachableClientWithAbsentPreStateIsNotPending` explicitly asserts the reported unsafe behavior (`internal/api/lsp_client_router_snapshot_review_test.go:170-193`). |
| C10 population race | none | **Open.** Snapshot and mutation independently derive availability (`internal/api/lsp_client_router_snapshot.go:127-136`; `internal/api/lsp_client_router.go:194-202`). | No snapshot-to-mutation appearance guard exists. Existing snapshot tests prove the search control found the family, but none makes `Exists()` transition false to true between the two command calls. |

## Similar implementations

- `static-read`: the reachable LSP restore path already uses exact live-entry
  checks before absent-row removal and ownership checks before present-row
  overwrite (`internal/api/lsp_client_router_snapshot.go:264-308`).
- `static-read`: the ordinary LSP mutation report appends Applied only after
  `adapter.AddEntry` succeeds (`internal/api/lsp_client_router.go:1033-1044`).
- `static-read`: the ordinary Serena API report likewise appends Applied only
  after AddEntry succeeds (`internal/api/serena_client_reconcile.go:479-487`,
  `internal/api/serena_client_reconcile.go:525-527`).
- `static-read`: the operation lock reuses the existing `flock` owner,
  `api.AcquireSupervisorLock`, whose `TryLock` and release behavior is at
  `internal/api/supervisor_lock.go:60-99` and
  `internal/api/supervisor_lock.go:170-190`.
- `static-read`: the GUI composition root already drives the same Serena and
  LSP cleanup functions for its own routers
  (`internal/cli/gui.go:1061`, `internal/cli/gui.go:1089`); the route commit
  adds the third owner without creating a second cleanup policy.

## Constraints

- `static-read`: stay inside this worktree; protected API/CLI tests require
  `-tags=test_state_path_env` plus a fresh `MCPHUB_STATE_DIR_OVERRIDE`;
  unscoped `go test ./...`, GUI/tray/supervisor launch, process killing by
  image name, checkout/reset/stash/push are forbidden
  (`work-items/active/2026-07-25-mcp-front-daemon/roadmap.md:14-24`).
- `static-read`: this stage is Research only and must finish classification and
  class inventory before design or mutation
  (`work-items/active/2026-07-25-mcp-front-daemon/status.md:8-19`,
  `work-items/active/2026-07-25-mcp-front-daemon/status.md:42-46`).
- This memo makes no correction recommendation and changes no production or
  test code.

## Change risks

- `runtime-verified` history: the focal surface has fix-over-fix churn across
  `65098291`, `6c02018b`, and `3872ee16`. `git log -S` attributes
  `lspSnapshotKey` to `65098291`, the absent-row `restorable` policy to
  `6c02018b`, and the lock/session-expiry additions to `3872ee16`.
- `static-read`: two tests encode incomplete rules rather than merely missing
  coverage: the merge test rejects multiple entry names for one
  client/language (`internal/cli/install_reconcile_mcp_front_pr588_test.go:487-500`),
  and the absent-row test requires non-pending consumption
  (`internal/api/lsp_client_router_snapshot_review_test.go:170-193`).
- `static-read`: the comment above `mergeMCPFrontReconcileReport` still says
  “Port keeps the first generation's value” at
  `internal/cli/install_reconcile_mcp_front.go:1164-1169`, while current code
  and the newer comment explicitly use the latest global value at
  `internal/cli/install_reconcile_mcp_front.go:1177-1197`. This is a
  documentation contradiction on a load-bearing polarity.
- Dynamic callback, adapter-interface, HTTP, and goroutine behavior was not
  executed in this stage; those edges remain explicitly
  `ASSUMPTION (UNVERIFIED)` until the named safe tagged tests run.

## Unresolved questions

- No classification question remains unresolved.
- The artifact does not currently encode enough information to distinguish
  which absent LSP rows were actually created, or which port each row's latest
  successful forward write used (`internal/api/lsp_client_router_snapshot.go:61-70`;
  `internal/cli/install_reconcile_mcp_front.go:237-245`). Which representation
  owns those facts is a downstream design decision, outside this analyst memo.
- The current source does not distinguish “client deliberately uninstalled”
  from “config path temporarily unreachable”: representative adapters define
  `Exists()` as `os.Stat(path) == nil`
  (`internal/clients/claude_code.go:36-38`,
  `internal/clients/codex_cli.go:30-32`). Both states are the same boolean at
  the recovery boundary.

## Research admission gates

| Gate | Result | Evidence |
| --- | --- | --- |
| Regression risk | **PASS for research admission** | Every closed class has a named production branch and test falsifier; every open class names the current conflicting participant/test gap under “Tests & coverage”. |
| Metric alignment | **PASS** | The investigation classifies all 14 rows, deduplicates them into ten classes, and inventories current participants; this matches `brief.md:20-31`. |
| Known limits | **PASS with declared limit** | No runtime verification was authorized for the analyst. CodeGraph had no index and Go LSP initialization timed out; dynamic edges are not presented as confirmed runtime behavior. |
| Bounded falsification | **PASS for handoff** | Exact existing safe tagged guards are named for C3/C5/C6/C7. C1/C2/C4/C8/C9/C10 include the missing or contradictory regression surface, so QA need not reopen broad discovery. |

## Adjacent findings

No out-of-scope functional finding was discovered. The stale port-polarity
comment is recorded under Change risks because it sits directly on C2's owning
function and affects interpretation of the admitted class.

## Gate

**PASS** — research artifact complete. The current classification is:

- **ALREADY FIXED:** C3 (`--check` dispatch), C5 (whole-operation lock),
  C6 (LSP route preflight), C7 (route-owned session cleanup).
- **REAL, open:** C1 (legacy rows lost in CLI merge), C2 (global port cannot
  represent partial cross-port retries), C4 (Serena CAS fails open without a
  baseline), C8 (write-ahead rows persisted as Applied before AddEntry),
  C9 (absent rows silently non-pending while unreachable), C10
  (snapshot/mutation client-population race).
- **WRONG:** none.


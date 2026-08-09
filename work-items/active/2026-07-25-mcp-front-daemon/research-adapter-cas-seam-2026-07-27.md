# Research — client-adapter CAS seam for PR #588 rollback

Date: 2026-07-27  
Role: `$analyst`  
Branch: `feat/mcp-front-daemon`  
Scope: Serena and LSP client-entry rollback mutation primitives only

## Summary

`static-read`: Serena rollback calls
`RestoreEntryFromBackupForRollback` directly for every recorded Applied row
(`internal/api/serena_client_reconcile.go:561-584`), while LSP snapshot rollback
reads with `GetEntry`, decides outside a mutation call, and later executes
ordinary `AddEntry` or `RemoveEntry`
(`internal/api/lsp_client_router_snapshot.go:255-340`;
`internal/api/lsp_client_router.go:1023-1055`). Every production client factory
returns a `lockingClient`, so each individual mutation is serialized by one
per-config in-process mutex plus advisory file lock and publishes through the
shared atomic secure-writer pipeline
(`internal/clients/config_lock.go:32-50`,
`internal/clients/config_lock.go:160-182`,
`internal/api/client_write_init.go:46-50`,
`internal/api/secure_write_client_config.go:68-89`). Those ordinary methods do
not accept an expected-live predicate, so the earlier Serena fingerprint and
LSP ownership reads are not part of the same adapter critical section. A true
entry-level compare-and-set (CAS) does already exist:
`clients.CASEntryMutator`, resolved through `AsCASEntryMutator`, re-reads,
compares, and restores/removes under one `withConfigLock` hold
(`internal/clients/cas_mutator.go:33-99`,
`internal/clients/cas_mutator.go:133-162`,
`internal/clients/cas_mutator.go:685-702`). It is implemented for nine concrete
adapters—the seven Serena adapters plus `opencode` and `mimocode`—and is already
used by de-adopt (`internal/api/deadopt.go:445-523`), but neither PR #588
rollback path calls it. The narrowest existing owner/seam that currently
expresses “do not overwrite an externally changed entry” is therefore
`clients.CASEntryMutator` plus the lock-holding `lockingClient` forwarder; the
ordinary `clients.Client` interface cannot express that condition
(`internal/clients/clients.go:201-210`,
`internal/clients/cas_mutator.go:70-110`).

## Files & symbols

- `static-read` — ordinary adapter contract:
  `Client.GetEntry`, `Client.AddEntry`, `Client.RemoveEntry`, and
  `Client.RestoreEntryFromBackupForRollback` at
  `internal/clients/clients.go:198-210` and
  `internal/clients/clients.go:238-253`.
- `static-read` — per-config lock owner:
  `withConfigLock`, `lockingClient`, and the Add/Remove/Restore forwarders at
  `internal/clients/config_lock.go:32-50`,
  `internal/clients/config_lock.go:160-182`, and
  `internal/clients/config_lock.go:217-256`.
- `static-read` — shared JSON-family behavior:
  `jsonMCPClient.AddEntry`, `RemoveEntry`, `GetEntry`, and rollback restore at
  `internal/clients/json_mcp.go:133-170` and
  `internal/clients/json_mcp.go:192-241`.
- `static-read` — representative standalone behavior:
  `claudeCode.AddEntry`, `RemoveEntry`, `GetEntry`, and rollback restore at
  `internal/clients/claude_code.go:111-151` and
  `internal/clients/claude_code.go:173-219`.
- `static-read` — production publication:
  the `clients.WriteConfigFile` hook and production initialization at
  `internal/clients/write.go:38-59` and
  `internal/api/client_write_init.go:46-50`; atomic publication contracts at
  `internal/api/secure_write_client_config.go:68-89`,
  `internal/api/secure_write_posix.go:162-173`, and
  `internal/api/secure_write_windows.go:285-301`.
- `static-read` — true CAS owner:
  `CASEntryMutator`, `AsCASEntryMutator`, `casRestoreFromBytes`,
  `casGuardedRemove`, and the wrapper forwarders at
  `internal/clients/cas_mutator.go:33-162`,
  `internal/clients/cas_mutator.go:200-303`, and
  `internal/clients/cas_mutator.go:652-702`.
- `static-read` — current consumers:
  Serena restore at `internal/api/serena_client_reconcile.go:561-584`,
  LSP snapshot planning at
  `internal/api/lsp_client_router_snapshot.go:255-340`, LSP mutation at
  `internal/api/lsp_client_router.go:1023-1058`, and the existing de-adopt CAS
  consumer at `internal/api/deadopt.go:445-523`.

### Searched and excluded

- `runtime-verified`: CodeGraph was queried first for this worktree and returned
  “no `.codegraph/` directory”; per its tool contract, no index was created and
  CodeGraph was not called again.
- `runtime-verified`: a production-only constructor search found 47
  `New*() (Client, error)` factories and 47 files containing
  `return newLockingClient`; `Compare-Object` over those two file sets returned
  `fileSetDifference=none`. The owning registry is
  `internal/clients/clients.go:824-934`, and the wrapper contract is
  `internal/clients/config_lock.go:178-182`.
- `runtime-verified`: a production-only search for `os.WriteFile(` under
  `internal/clients` found only the test-default fallback at
  `internal/clients/write.go:120-126`. The positive production wiring control is
  `internal/api/client_write_init.go:46-50`.
- `runtime-verified`: searches for `AsCASEntryMutator`,
  `CASRestoreEntryFromBytes`, `CASGuardedRemoveEntry`, and `ErrCASConflict`
  found no occurrence in
  `internal/api/serena_client_reconcile.go`,
  `internal/api/lsp_client_router_snapshot.go`,
  `internal/api/lsp_client_router.go`, or
  `internal/cli/install_reconcile_mcp_front.go`. The same search found the
  positive control in `internal/api/deadopt.go:445-523` and the owner in
  `internal/clients/cas_mutator.go:9-162`.
- `runtime-verified`: a production search for client-entry mutation capability
  interfaces found `Client`, `ScopedConfigWriterClient`, and
  `CASEntryMutator`; only `CASEntryMutator` carries a live-entry compare
  predicate (`internal/clients/clients.go:117-210`,
  `internal/clients/clients.go:335-362`,
  `internal/clients/cas_mutator.go:70-110`).
- `static-read`: `wholeFileRestoreIfWriteTargetGone` uses an atomic
  create-if-absent publish when the entire config vanished
  (`internal/clients/clients.go:387-430`). It is not an entry-level
  compare-and-set and is excluded from the CAS verdict.
- `static-read`: the command-level front-reconcile lock serializes two hub
  reconcile commands (`internal/cli/install_reconcile_mcp_front.go:315-369`).
  It is a different lock from the per-client `ConfigPath()+".lock"` owner
  (`internal/clients/config_lock.go:127-134`) and does not fold a client-entry
  compare into an adapter mutation.
- Widening step 1 covered all registry constructors and both rollback callers.
  Widening step 2 covered all client mutation capabilities and production
  writer paths. The factual verdict did not change after either widening step,
  so the investigation stopped at saturation.

## Adapter coverage

Evidence common to every row:

- `static-read` — `R`: canonical registry and factory membership at
  `internal/clients/clients.go:824-934`; `AllClients` constructs that registry
  at `internal/clients/clients.go:1011-1040`.
- `static-read` — `S`: Serena's fixed seven-client surface at
  `internal/api/serena_client_reconcile.go:164-188`.
- `static-read` — `L`: LSP iterates every constructed client and then applies
  existence/disable/enablement gates at
  `internal/api/lsp_client_router.go:194-213`; snapshot rollback later uses
  rows recorded for those clients
  (`internal/api/lsp_client_router_snapshot.go:231-254`).
- `runtime-verified` plus `static-read` — `M`: the constructor/wrapper file-set
  comparison was exactly 47/47 with no difference; the wrapper locks ordinary
  Add/Remove/Restore calls at `internal/clients/config_lock.go:217-256`, and
  production publication is atomic at
  `internal/api/secure_write_client_config.go:68-89`.
- `static-read` — `C+`: the exhaustive concrete CAS allowlist is
  `internal/clients/cas_mutator.go:112-130`, and the exact capability gate is
  `internal/clients/cas_mutator.go:133-162`. `C-` means the concrete adapter is
  outside that allowlist, so `AsCASEntryMutator` returns `(nil, false)`.

| Client | Registry evidence | Serena rollback | LSP rollback | Ordinary mutation path | True CAS |
| --- | --- | --- | --- | --- | --- |
| `claude-code` | `internal/clients/clients.go:867` | Yes (`S`) | Eligible (`L`) | `M` | `C+`; methods at `internal/clients/cas_mutator.go:452-464` |
| `codex-cli` | `internal/clients/clients.go:868` | Yes (`S`) | Eligible (`L`) | `M` | `C+`; methods at `internal/clients/cas_mutator.go:467-479` |
| `cursor` | `internal/clients/clients.go:869` | Yes (`S`) | Eligible (`L`) | `M` | `C+`; methods at `internal/clients/cas_mutator.go:594-606` |
| `vscode` | `internal/clients/clients.go:870` | Yes (`S`) | Eligible (`L`) | `M` | `C+`; methods at `internal/clients/cas_mutator.go:482-494` |
| `gemini-cli` | `internal/clients/clients.go:871` | Yes (`S`) | Eligible (`L`) | `M` | `C+`; methods at `internal/clients/cas_mutator.go:609-621` |
| `qwen-cli` | `internal/clients/clients.go:872` | Yes (`S`) | Eligible (`L`) | `M` | `C+`; methods at `internal/clients/cas_mutator.go:624-636` |
| `antigravity` | `internal/clients/clients.go:873` | Yes (`S`) | Eligible (`L`) | `M` | `C+`; methods at `internal/clients/cas_mutator.go:639-649` |
| `zed` | `internal/clients/clients.go:875` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `kiro` | `internal/clients/clients.go:876` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `windsurf` | `internal/clients/clients.go:877` | No (`S`) | Eligible (`L`) | `M` | `C-`; explicit exclusion at `internal/clients/cas_mutator.go:54-64` |
| `cline` | `internal/clients/clients.go:878` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `kilocode` | `internal/clients/clients.go:879` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `opencode` | `internal/clients/clients.go:880` | No (`S`) | Eligible (`L`) | `M` | `C+`; methods at `internal/clients/cas_mutator.go:497-507` |
| `mimocode` | `internal/clients/clients.go:885` | No (`S`) | Eligible (`L`) | `M` | `C+`; write-target-aware methods at `internal/clients/cas_mutator.go:510-540` |
| `hermes` | `internal/clients/clients.go:886` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `openclaw` | `internal/clients/clients.go:887` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `copilot-cli` | `internal/clients/clients.go:892` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `amazon-q` | `internal/clients/clients.go:893` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `openhands` | `internal/clients/clients.go:894` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `aider` | `internal/clients/clients.go:895` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `bob` | `internal/clients/clients.go:898` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `codebuddy` | `internal/clients/clients.go:899` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `command-code` | `internal/clients/clients.go:900` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `cortex` | `internal/clients/clients.go:901` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `deepagents` | `internal/clients/clients.go:902` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `devin` | `internal/clients/clients.go:903` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `droid` | `internal/clients/clients.go:904` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `firebender` | `internal/clients/clients.go:905` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `iflow-cli` | `internal/clients/clients.go:906` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `junie` | `internal/clients/clients.go:907` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `kimi-code-cli` | `internal/clients/clients.go:908` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `kode` | `internal/clients/clients.go:909` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `ona` | `internal/clients/clients.go:910` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `pi` | `internal/clients/clients.go:911` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `qoder` | `internal/clients/clients.go:912` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `qoder-cn` | `internal/clients/clients.go:913` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `roo` | `internal/clients/clients.go:914` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `rovodev` | `internal/clients/clients.go:915` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `tabnine-cli` | `internal/clients/clients.go:916` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `warp` | `internal/clients/clients.go:919` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `continue` | `internal/clients/clients.go:920` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `goose` | `internal/clients/clients.go:921` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `neovate` | `internal/clients/clients.go:929` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `crush` | `internal/clients/clients.go:930` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `pochi` | `internal/clients/clients.go:931` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `amp` | `internal/clients/clients.go:932` | No (`S`) | Eligible (`L`) | `M` | `C-` |
| `zencoder` | `internal/clients/clients.go:933` | No (`S`) | Eligible (`L`) | `M` | `C-` |

## Flows

### Serena rollback

1. `static-read`: the command performs a fingerprint comparison before the first
   restore (`internal/cli/install_reconcile_mcp_front.go:543-550`,
   `internal/cli/install_reconcile_mcp_front.go:1100-1137`).
2. `static-inference`; **ASSUMPTION (UNVERIFIED)**: the interface call
   `adapter.RestoreEntryFromBackupForRollback` at
   `internal/api/serena_client_reconcile.go:575-582` dispatches to each
   production `lockingClient` returned by `AllClients`. A safe tagged run of
   `TestRestoreSerenaReconcileApplied_BypassesHubEntryGuard_RestoresLegacyHubBackup`
   (`internal/api/serena_client_reconcile_test.go:921-1030`) would confirm the
   production-adapter edge.
3. `static-read`: the wrapper acquires the per-config lock and then calls the
   concrete restore body (`internal/clients/config_lock.go:253-256`). The
   concrete body compares live state only with the desired backup state for a
   no-op and otherwise restores it
   (`internal/clients/clients.go:365-384`,
   `internal/clients/claude_code.go:225-270`); it does not receive the
   forward-written entry as an expected-live CAS operand.

### LSP snapshot rollback

1. `static-read`: the rollback reads each live entry and decides whether to
   append an add/remove operation
   (`internal/api/lsp_client_router_snapshot.go:255-338`).
2. `static-inference`; **ASSUMPTION (UNVERIFIED)**: later interface calls in
   `applyLSPRouterOps` dispatch through the production wrappers. A safe tagged
   production-adapter test that pauses between the snapshot read and
   `applyLSPRouterOps` would confirm this exact edge; current snapshot tests use
   fakes (`internal/api/lsp_client_router_snapshot_review_test.go:322-373`).
3. `static-read`: `applyLSPRouterOps` invokes ordinary `AddEntry` or
   `RemoveEntry` (`internal/api/lsp_client_router.go:1023-1055`). Each call
   obtains its own per-config lock (`internal/clients/config_lock.go:223-245`),
   but the earlier ownership comparison is outside that call and therefore
   outside that specific lock hold.

### Existing CAS flow

1. `static-read`: de-adopt resolves `AsCASEntryMutator`, derives the match
   function, and invokes CAS restore/remove
   (`internal/api/deadopt.go:445-523`).
2. `static-inference`; **ASSUMPTION (UNVERIFIED)**: the capability interface
   dispatch reaches the matching concrete adapter while the wrapper holds
   `withConfigLock`; source wiring is at
   `internal/clients/cas_mutator.go:150-162` and
   `internal/clients/cas_mutator.go:685-702`. A safe run of
   `TestCASLockingClientForwarderNoDeadlock`
   (`internal/clients/cas_mutator_test.go:435-466`) would runtime-confirm the
   edge.
3. `static-read`: under that one hold, `casRestoreFromBytes` re-reads, invokes
   the expected-live matcher, refuses mismatch, then restores
   (`internal/clients/cas_mutator.go:200-258`);
   `casGuardedRemove` applies the analogous read/check/remove sequence
   (`internal/clients/cas_mutator.go:261-303`).

## Contracts

| Contract | Factual state | Evidence |
| --- | --- | --- |
| `GetEntry` returns the named current entry or nil | Owned by `Client`; wrapper does not override read-only `GetEntry` | `internal/clients/clients.go:209-210`; `internal/clients/config_lock.go:160-167` |
| `AddEntry` adds or replaces one named entry | Ordinary mutation, independently lock-wrapped | `internal/clients/clients.go:201-203`; `internal/clients/config_lock.go:223-227` |
| `RemoveEntry` is idempotent for absence | Ordinary mutation, independently lock-wrapped | `internal/clients/clients.go:205-207`; `internal/clients/config_lock.go:241-245` |
| Rollback restore writes the backup's entry or removes when backup lacks it | Ordinary restore bypasses the hub-entry guard and is independently lock-wrapped | `internal/clients/clients.go:238-253`; `internal/clients/config_lock.go:253-256` |
| Adapter read-modify-write serialization | Per-path process mutex plus cross-process advisory `<config>.lock` | `internal/clients/config_lock.go:12-36`; `internal/clients/config_lock.go:127-134` |
| File publication | Whole replacement is atomic and handle-relative in production | `internal/api/secure_write_client_config.go:68-89`; `internal/api/secure_write_posix.go:162-173`; `internal/api/secure_write_windows.go:285-301` |
| Atomic publication is an entry CAS | **No.** The writer receives only path and complete contents; it has no expected-live operand | `internal/clients/write.go:51-59`; `internal/api/secure_write_client_config.go:68-89` |
| True entry CAS exists anywhere | **Yes, for nine concrete adapters.** Compare and mutate occur under one wrapper-held config lock | `internal/clients/cas_mutator.go:112-162`; `internal/clients/cas_mutator.go:200-303`; `internal/clients/cas_mutator.go:685-702` |
| PR #588 Serena/LSP rollback uses true entry CAS | **No.** Both use ordinary `Client` methods; the CAS-symbol search had a positive control in de-adopt and no hit in either rollback surface | `internal/api/serena_client_reconcile.go:561-584`; `internal/api/lsp_client_router_snapshot.go:255-340`; `internal/api/lsp_client_router.go:1023-1058`; `internal/api/deadopt.go:445-523` |
| Narrowest existing ownership seam | `clients.CASEntryMutator` / `AsCASEntryMutator` with `lockingClient` forwarders is the only current client-entry interface combining expected-live comparison and mutation under one lock | `internal/clients/cas_mutator.go:33-110`; `internal/clients/cas_mutator.go:133-162`; `internal/clients/cas_mutator.go:652-702` |

## Tests & coverage

No test, build, or vet command was run in this addendum.

- `static-read`: `TestAllClientsAreLockWrapped` checks every `AllClients` value
  is a `*lockingClient` (`internal/clients/config_lock_wrapped_test.go:8-36`).
- `static-read`: per-adapter CAS restore/remove coverage and the nine-adapter
  admission gate are at `internal/clients/cas_mutator_test.go:379-432` and
  `internal/clients/cas_mutator_test.go:477-518`.
- `static-read`: lock-forwarder coverage is
  `TestCASLockingClientForwarderNoDeadlock`
  (`internal/clients/cas_mutator_test.go:435-466`).
- `static-read`: ordinary rollback restore has barrier tests proving its
  backup-equality comparison and restore execute under one config lock
  (`internal/clients/rollback_restore_skip_test.go:160-270`). Those tests do
  not supply an expected forward-written value and therefore do not establish
  the stronger “refuse external edit” contract.
- `static-read`: the current LSP operator-edit tests use a fake adapter and
  check only the point-in-time decision before mutation
  (`internal/api/lsp_client_router_snapshot_review_test.go:308-373`); they do
  not create a deterministic edit between read and ordinary RemoveEntry.
- `static-read`: Serena's real-adapter rollback test establishes that the
  guard-bypassing restore runs for Claude Code and Antigravity
  (`internal/api/serena_client_reconcile_test.go:921-1030`), but it does not
  change the live entry between the CLI fingerprint gate and restore.

## Similar implementations

- `static-read`: de-adopt is the only production client-entry flow found that
  resolves `AsCASEntryMutator` and handles `ErrCASConflict`
  (`internal/api/deadopt.go:445-523`).
- `static-read`: ordinary rollback restore's folded compare is a
  desired-state no-op test, not an ownership CAS: exact live==backup returns
  without writing, while divergence proceeds to restore
  (`internal/clients/clients.go:365-384`,
  `internal/clients/claude_code.go:225-270`).
- `static-read`: MiMoCode's CAS implementation already distinguishes the
  physical write target from the merged read view and refuses a winning higher
  layer before mutation (`internal/clients/cas_mutator.go:510-583`). This is an
  existing specialized implementation inside the same capability owner.
- `static-read`: `SecureWriteClientConfig` atomically publishes bytes but does
  not classify the old entry (`internal/api/secure_write_client_config.go:68-89`);
  it is publication atomicity, not compare-and-set semantics.

## Constraints

- `runtime-verified`: no source/test code, test, build, vet, GUI, tray,
  supervisor, or process command was used in this addendum.
- `static-read`: protected API/CLI test execution requires
  `-tags=test_state_path_env` and a fresh `MCPHUB_STATE_DIR_OVERRIDE`;
  unscoped `go test ./...` is forbidden
  (`work-items/active/2026-07-25-mcp-front-daemon/roadmap.md:14-24`).
- `static-read`: this artifact is a factual addendum only. It does not select a
  representation, design a compatibility policy for the 38 non-CAS adapters,
  or edit the accepted implementation plan.

## Change risks

- `static-read`: `CASEntryMutator` is deliberately a concrete-adapter
  allowlist, not a method on `Client` or `jsonMCPClient`; promotion onto an
  adapter with different physical-entry semantics is documented as unsafe
  (`internal/clients/cas_mutator.go:54-66`,
  `internal/clients/cas_mutator.go:112-161`). Nine of 47 registry adapters
  currently pass the gate.
- `static-read`: all seven Serena adapters are in that existing nine-adapter
  set (`internal/api/serena_client_reconcile.go:179-188`;
  `internal/clients/cas_mutator.go:121-129`). LSP can iterate all 47 registered
  adapters, leaving 38 outside the current CAS capability
  (`internal/clients/clients.go:865-934`;
  `internal/api/lsp_client_router.go:194-213`).
- `static-read`: `withConfigLock` is expressly an advisory file lock
  (`internal/clients/config_lock.go:32-36`). Its CAS is atomic among
  lock-honoring hub participants; atomic rename prevents torn publication but
  does not force an unrelated editor/client process to honor the advisory
  lock (`internal/api/secure_write_client_config.go:68-89`).
- `runtime-verified` history: CAS support predates the PR #588 recovery work
  (`f4623355`, then `43675926`), while the focal rollback surface accumulated
  fixes in `65098291`, `6c02018b`, and `3872ee16`. The Serena restore line
  remains attributed to `0dd7287e`, and the LSP read/plan path to
  `65098291`/`6c02018b`.

## Unresolved questions

- `static-read`: the source answers capability availability but not which of
  the 47 LSP adapters are enabled on the operator's host; enablement is decided
  at runtime by existence, disable settings, explicit enablement, or evidence
  (`internal/api/lsp_client_router.go:194-213`). No live host enumeration was
  performed.
- `static-read`: the current CAS interface consumes snapshot bytes and a match
  callback (`internal/clients/cas_mutator.go:70-99`), while Serena's current
  persisted row exposes a backup path and fingerprint
  (`internal/api/serena_client_reconcile.go:569-582`;
  `internal/cli/install_reconcile_mcp_front.go:1100-1126`). How a future caller
  maps its evidence into the existing capability is a design decision and is
  not resolved here.
- `static-read`: no native kernel-level conditional replace of “only if the
  current entry still equals X” was found. The available true CAS is the
  repository's lock-scoped adapter capability; non-lock-honoring editors remain
  outside the flock's serialization contract
  (`internal/clients/config_lock.go:32-36`,
  `internal/clients/cas_mutator.go:39-52`).

## Research admission gates

| Gate | Result | Evidence |
| --- | --- | --- |
| Regression risk | **PASS for factual return** | The inventory separates ordinary lock wrapping, atomic file publication, and entry CAS instead of conflating them (`internal/clients/config_lock.go:217-256`; `internal/api/secure_write_client_config.go:68-89`; `internal/clients/cas_mutator.go:200-303`). |
| Metric alignment | **PASS** | The requested metric was actual compare+mutation under one lock; the CAS forwarders and helpers expose exactly that boundary (`internal/clients/cas_mutator.go:685-702`; `internal/clients/cas_mutator.go:200-303`). |
| Known limits | **PASS with declared limits** | Only nine concrete adapters are admitted, and the lock is advisory (`internal/clients/cas_mutator.go:112-161`; `internal/clients/config_lock.go:32-36`). |
| Bounded falsification | **PASS for handoff** | Existing static test anchors cover wrapper admission, CAS branches, and lock ownership (`internal/clients/config_lock_wrapped_test.go:8-36`; `internal/clients/cas_mutator_test.go:379-518`). No test was run in this read-only stage. |

## Adjacent findings

No adjacent functional issue was admitted. The only material scope boundary is
the deliberate 9-of-47 CAS capability set, recorded under Change risks.

## Gate

**PASS — RETURN(lead); not planner-eligible.** The factual verdict is complete:
a true adapter-level CAS exists and is the narrowest current ownership seam,
but the PR #588 Serena/LSP rollback paths use ordinary independently locked
methods instead. The lead must route any design or implementation decision.

# Bug: the frozen LSP plan's per-row pre-state is invalidated by the SAME run's write to a different client

- id: 2026-07-27-lsp-plan-prestate-invalidated-by-sibling-client-write
- context: adjacent-finding
- status: fixed (2026-07-27, `internal/api/lsp_client_router_snapshot.go` +
  `internal/api/lsp_client_router.go`; awaiting archive to `bugs/closed/`)
- severity: high
- area: internal/api/lsp_client_router.go:236-256 (plan capture), internal/api/lsp_client_router.go:316-322 (ExpectedLive), internal/clients/mimocode.go:2023-2032 (claude import), internal/clients/mimocode.go:195,201 (claudeHome wiring)
- found-by: backend-engineer (PR #588 branch repair lane)

## Summary

`PlanLSPRouterClientEntries` freezes one population and one exact pre-state per
row (invariant I3 / class C10). `ApplyLSPRouterClientPlan` then mutates the rows
in client order under a compare-and-swap whose predicate is that frozen
pre-state.

Two clients in that population can read the SAME underlying file. When they do,
the earlier client's mutation changes the later client's observable state, and
every one of the later client's rows fails its own precondition. The plan
invalidates itself.

The concrete instance is `mimocode`, whose merged read imports
`~/.claude.json` — the exact file the `claude-code` adapter writes.
`claude-code` sorts before `mimocode`, so on any host with both clients the
forward reconcile migrates `claude-code`, and all nine `mimocode` rows then
conflict.

## Why this is not a test artifact

`clients.AllClients()` builds the LIVE mimocode client, and `NewMimoCode` sets
`claudeHome` from `$HOME` / `$USERPROFILE`
(`internal/clients/mimocode.go:195,201`); `claudeImportEntries` reads
`~/.claude.json` from it (`internal/clients/mimocode.go:2023-2032`). Both the
enablement evidence (`clientHasLSPRouterEnablementEvidence`,
`internal/api/lsp_client_router.go:547-584`) and the captured pre-state come
through that import. On the dev host `~/.config/mimocode/mimocode.json` exists
and claude-code is installed, so `mcphub install --reconcile-mcp-front` reaches
this on a real machine, not only under test.

## Reproduction

`go test -tags=test_state_path_env -run '^TestMCPFrontPR588_RollbackRestoresPriorLSPRouterURL$' ./internal/cli/`

Fails at `install_reconcile_mcp_front_pr588_test.go:247` with
`lsp client router reconcile: lsp client router plan failed for 9 operation(s)`,
every one `Op:"precondition"`.

Captured at the conflict site (temporary instrumentation, since removed; front
port 59810):

```
mimocode/clangd/mcp-language-server-clangd op=add
  planPre  = {Client:"mimocode" Language:"clangd" Present:false URL:""}
  liveNow  = {Client:"mimocode" Language:"clangd" Present:true  URL:"http://127.0.0.1:59810/lsp/clangd/mcp"}
mimocode/go/mcp-language-server-go op=add
  planPre  = {... Present:true URL:"http://127.0.0.1:9125/lsp/go/mcp"}
  liveNow  = {... Present:true URL:"http://127.0.0.1:59810/lsp/go/mcp"}
```

`59810` is the front port this run wrote into `~/.claude.json` moments earlier.
No mimocode write happened: a precondition conflict returns before `BackupKeep`
and before the adapter call, so `Invoked=false` on all nine. mimocode's own
config file was never touched — only its view of claude's changed.

`TestMCPFrontR2_RerunAtANewPortRecordsTheLatestPort` fails the same way, one
generation later: run A gives claude-code its nine entries, which is what makes
mimocode look enabled on run B.

## Why it surfaced now

It was masked. At `31b9ca94` the forward run died earlier, at
`claude-code/clangd`, with `forward-ownership-unknown` — the LSP plan's
`IntendedState` was a projection of the write REQUEST rather than the expected
readback, so every successful add compared unequal to its own readback. Fixing
that (`intendedEntryReadbackProjection`) let the run reach the second client for
the first time.

## Fix candidates considered when this was filed

1. **Do not plan a client whose only enablement evidence and pre-state come from
   another client's file.** Recorded as blocked on "a new capability on a shared
   interface".
2. **Treat a precondition conflict whose live state already equals the intended
   state as a satisfied no-write row.** Rejected: contradicts the accepted F2
   acceptance row in
   `work-items/decisions/2026-07-27-mcp-front-reconcile-v3-row-journal.md`.
3. **Re-read each client's pre-state immediately before mutating it.** Rejected:
   deletes invariant I3 and class C10, both protected.

## Resolution (2026-07-27)

Fixed along the shape of candidate (1), and **the blocker recorded against it
did not hold**: the capability already exists and no shared interface changed.

`clients.MCPEntry.SourceBelowWriteTarget` (`internal/clients/clients.go:69-89`)
is exactly the missing predicate. mimocode's `GetEntry` STAMPS it from
`mimoCodeDefinedAtOrAboveWriteTarget` (`internal/clients/mimocode.go:3880-3884`),
which deliberately excludes both the operator's lower `config.json` and the
`~/.claude.json` import (`internal/clients/mimocode.go:3159-3168`) precisely
because those are layers "the hub never wrote and never clobbered". The field's
own doc already enumerated its consumers as the three install/register rollback
sites and noted that "every OTHER GetEntry caller ignores the field" — the LSP
router plan/apply family was one of those callers, and it is a family that
MUTATES under a compare-and-swap, so ignoring it was the defect.

The fix is two edits, both in the api layer:

- `lspSnapshotFromEntry` (`internal/api/lsp_client_router_snapshot.go`) — the
  single owner of "what is THIS adapter's own state for this entry" — projects a
  `SourceBelowWriteTarget` entry as ABSENT. Every pre-state capture, CAS
  precondition, applied receipt and rollback compare in the family funnels
  through it, so all of them become consistent at once.
  `SnapshotLSPRouterClientEntries` re-typed the same field copy inline and now
  routes through the owner too.
- `collectLegacyLSPEntriesToMigrate` (`internal/api/lsp_client_router.go`) skips
  a legacy candidate that is below the write target: `RemoveEntry` only deletes
  the write target's own key, so such an entry is not this reconcile's to
  migrate away. That keeps the function's stated invariant (captured surface ==
  mutated surface, by construction) intact.

**Invariant I3 is restored, not deleted.** I3 assumes a row's pre-state is
independent of every other row's mutation. After the fix a row's pre-state IS
the write-target object that only that client mutates, so the assumption holds
by construction rather than by luck. The F2 acceptance row is untouched: there
is no precondition conflict left to make an equality exception for.

The behavioural outcome is also better than "skip mimocode": mimocode is still
planned and gets its OWN front-port entry in its OWN config, instead of silently
depending on another client's file, and its recorded baseline (absent) has the
honest inverse — remove the hub's key and let the import layer re-emerge, which
is the same polarity the install/register rollback sites already use.

Mutation-proven both ways:

- dropping the `SourceBelowWriteTarget` clause reproduces
  `lsp client router plan failed for 9 operation(s)` on both
  `TestMCPFrontPR588_RollbackRestoresPriorLSPRouterURL` and
  `TestMCPFrontR2_RerunAtANewPortRecordsTheLatestPort`;
- disabling the legacy-candidate skip fails
  `TestLSPRouterLegacyCollection_SkipsCandidatesBelowTheWriteTarget`.

Regression coverage: `internal/api/lsp_client_router_multilayer_test.go`.

## Residual — the general class is NOT fully closed

This closes the layered-read instance. The file's own generalization still
stands for a DIFFERENT shape: two clients whose config paths are **symlinked or
otherwise aliased to one physical file**. There both entries are genuine write
targets, `SourceBelowWriteTarget` is false on both, and the earlier client's
mutation still invalidates the later one's frozen pre-state. No adapter pair in
this repo resolves to one path today, so it is latent, not live. Closing it
needs a plan-time identity check (same resolved config path admitted twice) and
remains a contract decision.

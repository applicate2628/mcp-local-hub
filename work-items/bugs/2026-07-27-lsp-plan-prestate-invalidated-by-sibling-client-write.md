# Bug: the frozen LSP plan's per-row pre-state is invalidated by the SAME run's write to a different client

- id: 2026-07-27-lsp-plan-prestate-invalidated-by-sibling-client-write
- context: adjacent-finding
- status: open
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

## Why it is not fixed in this lane

The fix is a contract decision, not an implementation choice, and each candidate
lands outside what an implementer may decide:

1. **Do not plan a client whose only enablement evidence and pre-state come from
   another client's file.** Needs a way to ask a `clients.Client` whether an
   entry resolves from its OWN writable layer — a new capability on a shared
   interface. `internal/api/scan.go:633` already names this presence class
   (`isPromotableAbsentPresenceState(cur) && clients.MimoCodeHasClaudeImport(path)`),
   so the concept exists but is not reachable from the plan owner.
2. **Treat a precondition conflict whose live state already equals the intended
   state as a satisfied no-write row** (no receipt, so rollback gains no
   authority and correctly never writes mimocode). Coherent, and narrow enough
   to live in the plan owner — but it directly contradicts the accepted F2
   acceptance row in
   `work-items/decisions/2026-07-27-mcp-front-reconcile-v3-row-journal.md`
   ("wrapper rejects precondition; prior receipt absent -> `precondition-conflict`
   -> settled no-write conflict"), which admits no equality exception.
3. **Re-read each client's pre-state immediately before mutating it.** Deletes
   invariant I3 and class C10, both marked protected.

The general statement the decision does not currently model: **I3 assumes each
row's pre-state is independent of every other row's mutation.** Two clients
sharing a read source break that assumption, and mimocode/claude-import is one
instance of it, not the whole class — two clients whose config paths are
symlinked to one file behave identically.

## Requested decision

Which of (1) or (2) is the contract, and whether the frozen-population invariant
should state explicitly that a plan may not contain two clients that resolve one
entry from a shared source.

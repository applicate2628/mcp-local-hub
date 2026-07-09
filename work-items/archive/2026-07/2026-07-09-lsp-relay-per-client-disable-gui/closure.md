# Closure - LSP relay per-client disable GUI

Closed: 2026-07-09

## Outcome

DELIVERED. PR #524 (`feat(gui): per-client LSP-relay enable/disable in the
Servers matrix`) merged as squash commit `22c91cab`. The final reviewed branch
head was `e4058989`: the bot PASS summary named `Reviewed commit: e405898965`,
no bot inline comment was originally anchored to that head, and GitHub GraphQL
reported zero unresolved review threads authored by
`chatgpt-codex-connector`.

The canonical installed binary reports commit `22c91cab`, replacing the
previously recorded `7898c148` installation. Live verification on that binary
found 21 supervisor daemon rows Running and no quarantined row; GUI
`/api/status` returned 200; the unauthenticated hub aggregate probes for both
`/clients/claude-code/mcp` and `/clients/cursor/mcp` returned 401 (listener
alive, authentication required); and an LSP router `/lsp/go/mcp` initialize
returned 200. `GET /api/lsp-router/status` also returned 200 with a real
`clients` array: all 15 entries carried `client`, `config_path`, and `disabled`,
and `missing_entries` was present when non-empty (the merged response type
omits that field when empty).

## What shipped

The Servers matrix now exposes per-client shared LSP-router enable/disable
controls. The API owners `DisableLSPRouterClient` and `EnableLSPRouterClient`
persist the client preference and run the matching client-config rollback or
ensure inside one serialized operation, so the settings write and config
mutation cannot interleave with another settings writer. The GUI reads the
persisted preference through the same-origin-gated
`GET /api/lsp-router/status` endpoint.

Checkbox state describes the current client-config entry; opt-out state is a
separate client-level preference. Their rendered decisions are computed once
by `lspCellState` and consumed by `Servers.tsx`. This preserves the distinction
between whether an entry is present and whether the cell can express a
disable.

GitHub records eight finding-bearing bot reviews before the final PASS summary.
The early reviews found defects in the original change. Later reviews found
regressions introduced by successive fixes; the final repeated availability
finding was root-caused at the owning cell-state abstraction instead of being
patched as another entry-shape special case.

## Review findings

1. **P1 self-deadlock introduced by the serialization fix itself.** An
   adversarial lock review caught this before merge; the bot did not.
   `mcphub lsp-router disable|enable` passes a zero GUI port, so the first
   serialization implementation resolved `gui_server.port` from inside the
   locked `after(raw)` hook. That re-entered the non-reentrant `settingsMu`
   while the settings file lock was also held, deterministically hanging the
   command and blocking every other settings writer. The shipped fix hoists
   every settings read reachable from ensure/rollback out of the lock, derives
   the client sets from the hook's in-hand raw map, and documents that the hook
   must not access settings directly or transitively. The regression
   `TestLSPRouterClientToggleZeroGUIPortCompletes` covers both disable and
   enable and reproduces the timeout against the pre-fix implementation.

2. **A three-round availability cascade was finally root-caused.** Opt-out
   availability had been derived from entry presence instead of from whether
   the cell could express a disable. `clients.lsp_router_disabled` is a
   client-level preference; entry presence is client-config state and belongs
   to the checkbox. Gating one on the other made the control disappear in each
   entry shape not yet enumerated. `lspCellState` now owns that decision once,
   and `Servers.tsx` renders its result.

3. **An optional status fetch temporarily became a whole-screen failure.** The
   change that added `GET /api/lsp-router/status` put it inside the Servers
   screen's fail-fast `Promise.all`, so one optional status failure blanked the
   matrix. The fix made that member degrade locally and audited the whole batch:
   only `/api/scan` and `/api/status` are required; workspace and LSP-router
   status fetches are optional.

4. **The 15-thread triage checkpoint contained two live defects.** At that
   point 13 threads had commit plus file-and-line evidence that their findings
   were fixed. Two replies explicitly kept their threads open: fallback-named
   legacy entries were not yet enableable, and the long-lived `Servers.tsx`
   finding "let users opt out when router entries are absent" was still live
   after five review rounds. Resolving everything that merely looked stale
   would have shipped the latter defect. Three later regression threads brought
   the final PR total to 18; all 18 were resolved before merge, and the final
   GraphQL gate reported zero unresolved bot threads.

## Residual risk

- The `after(raw)` no-settings-access contract is enforced by documentation
  plus one regression test, not by a structural guard. Any future direct or
  transitive settings access reachable from ensure/rollback can recreate the
  same deterministic deadlock.
- Opting out while an entry is inherited records the preference but cannot
  remove the inherited source because the hub never wrote it. The read-only
  checkbox therefore remains checked, which can make the separate **Off**
  action look ineffective even though future hub-owned ensures are disabled.
- The LSP-router opt-out is a preference, not an access-control boundary. The
  separate `tools_hidden` caveat does not apply to this feature.

## Retrospective

- **Verify bot PASS with GraphQL review threads, not only current-head inline
  comments.** On PR #525 the bot posted a current-head "no major issues"
  summary at 09:18Z with zero bot inline comments originally anchored to that
  head, while two earlier threads were still live. Their fix-evidence replies
  were posted later at 09:34Z; one thread was P1.
- **Never resolve a review thread without proving the finding fixed in the code
  at HEAD.** A wrongly resolved thread hides a live defect more effectively
  than an open one.
- **When the bot names one instance, fix the class.** The `Promise.all` audit
  classified every member as required or optional, preventing the same failure
  from moving to a neighbouring fetch in the next round.
- **When a fix widens a contract, test its failure path as well as its success
  path.** The new endpoint was registered, same-origin gated, and tested, yet
  its failure still blanked the whole Servers screen until the consumer path
  was exercised.
- **Deployment checks must probe the owning process.** The gate-ON aggregate
  and LSP router live in the GUI process, not the supervisor, so 21 Running
  daemon rows do not prove either listener is healthy. Probe both ports.
  `install --upgrade` correctly refuses while a GUI holds the binary lock. The
  operator record for this deployment says a fixed two-second wait after
  force-killing the GUI was insufficient; confirm that the process has
  actually exited before retrying.

## Artifacts

- PR #524: reviewed head `e4058989`, merged as squash `22c91cab`.
- Deployment: installed `22c91cab`; previous installation `7898c148`.
- Preserved delivery history: `status.md`.
- Accepted scope and original problem: `brief.md`.

## Terms and Abbreviations

- **API** - Application Programming Interface.
- **GUI** - Graphical User Interface.
- **LSP** - Language Server Protocol.
- **MCP** - Model Context Protocol.
- **P1** - Priority 1, a high-severity review finding.
- **PR** - Pull request.
- **GraphQL** - GitHub's structured API used here to query review-thread state.

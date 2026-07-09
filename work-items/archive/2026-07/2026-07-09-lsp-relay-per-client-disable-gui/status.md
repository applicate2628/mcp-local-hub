# status - LSP relay per-client disable GUI

Template: full-delivery (behavioral GUI/API fix). Orchestrator: `$lead`.
State: ACTIVE - PR #524 open, MERGEABLE, all review threads resolved (0 open).
Codex bot re-review requested on the current head; PASS not yet verified.

PR: #524 `feat(gui): per-client LSP-relay enable/disable in the Servers matrix`
Branch: `fix/lsp-relay-per-client-disable-gui`
Base: `master`
Current HEAD: `e405898965db527a889aca77c27335cf17642e1e`
Bot re-review triggered: 2026-07-09 17:40Z on `e4058989`

## Where this stands

Eight bot rounds. Rounds 1-4 were findings in the original change; rounds 5-7
were regressions introduced by the fixes themselves, which is why the last one
was root-caused rather than patched.

Landed on the branch, in order:
- `669d6019` original feature; `31085bb3` merge of master (regenerated bundle).
- `41750ade` toggle state derives from presence, not transport (3 bot P2:
  legacy-HTTP rendered checked; unusable client config left the toggle enabled;
  inherited cells were not read-only). Also two commission P3: nondeterministic
  checked-flicker in a router+legacy coexistence (fixed by deterministic
  router-preferring row aggregation) and an `isLspRouterURL` vs Go-parser
  divergence (fixed by percent-decoding the pathname).
- `6d651246` serialized the full per-client toggle, and fixed the **P1
  self-deadlock that serialization introduced**: `mcphub lsp-router
  disable|enable` passed a zero GUIPort, so ensure/rollback resolved
  `gui_server.port` from inside the locked `after(raw)` hook, re-entering the
  non-reentrant `settingsMu` while holding the settings flock. Every settings
  read reachable from ensure/rollback is now hoisted before the lock.
- `cf332a74` legacy relay entries are replaceable (mirrors backend
  `entryPointsAtLegacyLSPPort`, which checks both `URL` and `RelayURL`).
- `907e43b6` mirrored the backend fallback candidate names; added
  `GET /api/lsp-router/status` so a client with no router entry could persist an
  opt-out (thread `Servers.tsx:2125`, open and unfixed for five rounds).
- `746f9d5f` fixed two regressions from `907e43b6`: the new status fetch sat in
  the Servers `Promise.all` and blanked the whole matrix on any failure; and the
  fallback-name mirroring only took effect in the all-workspaces view.
- `e4058989` root-caused the three-round cascade: opt-out availability was being
  derived from **entry presence** instead of from **whether the cell can express
  a disable**. `clients.lsp_router_disabled` is a client-level preference; entry
  presence is config state, which is what the checkbox reflects. The cell is now
  computed once in `lspCellState` (lsp-rows.ts) and consumed by Servers.tsx.

## Invariants that must not regress (invisible from the diff)

- `internal/api/settings.go`'s `after(raw)` hook runs under the non-reentrant
  `settingsMu` **and** the settings flock. It must not read or write settings,
  directly or transitively. Any settings access there re-creates a deterministic
  self-deadlock that hangs `mcphub lsp-router disable|enable` and blocks every
  other settings writer until the process is killed. Guarded by
  `TestLSPRouterClientToggleZeroGUIPortCompletes` (must pass for BOTH disable and
  enable). `settings.go` and `client_install_prefs.go` should stay untouched.
- In the Servers screen's `Promise.all`, only `/api/scan` and `/api/status` are
  REQUIRED (they own the matrix rows and the status/port columns). Every other
  fetch must catch locally and degrade; none may blank the matrix.
- Replaceability (may the hub rewrite this entry) is a SEPARATE predicate from
  ownership (is the entry already ours; relay ownership requires the current
  mcphub binary). Only the reserved `mcp-language-server-<lang>` name may render
  a cell checked; a suffixed sibling must not.
- Opt-out availability answers one question - can this cell express a disable:
  already-off / busy / checked+interactive checkbox / writable config -> `Off`
  button / otherwise unavailable. The live-checkbox test precedes the
  config-usable test on purpose.
- `GET /api/lsp-router/status` stays same-origin gated with redacted errors.

## Verification recorded on `e4058989` (lead's own run, not a delegate's claim)

`go build ./...`, `go vet ./internal/api/ ./internal/cli/ ./internal/gui/`,
`go test ./internal/api/ -run 'LSPRouterClient|DisableLSP|EnableLSP|ClientInstallPref'`,
`TestLSPRouterClientToggleZeroGUIPortCompletes` (disable + enable),
`go test ./internal/gui/ -run 'LSPRouter'`, `npm run typecheck`,
`npm run test` (1051 tests) - all green. `go generate ./internal/gui/...` ran;
`internal/gui/assets/app.js` re-embedded.

## Review threads

All bot review threads on this PR were triaged against the code at HEAD and are
resolved: 0 open. Thirteen were already fixed by earlier commits and were
answered with the commit + file:line evidence; three were live and were fixed
(`Servers.tsx:2125`, the two `746f9d5f` regressions, and the `e4058989`
opt-out-with-legacy-entry finding).

**A bot PASS must be checked with GraphQL `reviewThreads`, not by filtering
inline comments to HEAD.** On PR #525 the bot's summary said "no major issues"
on the current head with zero HEAD-anchored inline comments while two threads
from earlier commits were still open, one of them a P1.

## Next action

1. Verify bot PASS on `e4058989`: the summary phrase must name that commit as
   `Reviewed commit`, zero inline comments anchored to it, and zero unresolved
   `reviewThreads` authored by `chatgpt-codex-connector` (GraphQL).
2. If NOT PASS: fix every finding (no severity is deferred), push, re-trigger.
3. On PASS: `gh pr merge 524 --squash --delete-branch` (never `--admin`), then
   `git checkout master && git pull --rebase origin master`.
4. Deploy: build, stage the binary INSIDE the target dir as `mcphub-deploy.exe`
   (a D: -> C: `MoveFileEx` fails), stop the GUI with a precise command-line
   match (`mcphub\.exe"?\s+gui`, never `*gui*` - that once matched a typescript
   daemon whose workspace path contained "gui"), run `install --upgrade`, then
   **relaunch the GUI** (`--no-browser`, keep the tray).
5. Live-verify, and do not mistake a healthy process table for a working system:
   `mcphub status` covers the supervisor's daemons, but the gate-ON aggregate
   (`/clients/<client>/mcp`) and the LSP router (`/lsp/<lang>/mcp`) live in the
   **GUI** process. Probe both ports; `401` on the aggregate means alive+auth.
   After the #525 deploy the GUI was found dead and both surfaces were down.
6. Close this work-item: `closure.md` (outcome, residual risk, retrospective,
   `Closed: <date>`), move to `work-items/archive/2026-07/`, move its row in
   `work-items/index.md` from Active to Archived.

## Follow-up outside this item

`npm/package.json` is at `0.4.23`, and the published `v0.4.23` tag is commit
`8944fb30` (2026-07-04) - master is 45+ commits ahead and the tag predates the
`legacy_stop_watermarks` work. Shipping requires a version bump, a tag push, and
the CI publish workflow (never a local `npm publish`). Until then the
npm-global `mcphub` shim on the dev host is an older binary that shadows the
`.local/bin` install in PATH.

# Demigrate fallback when no pre-hub form exists

**Severity:** P2 (operator gap, no data loss; affects v0.3.0+ installs that
upgraded from `mcp-sync` codename in April 2026)

**Found:** 2026-05-15 by user during PR #185 r2 manual UI smoke. User
attempted to uncheck `time/gemini-cli` from the matrix and Apply. Demigrate
failed with:

```text
time/demigrate/gemini-cli: latest backup
  C:\Users\<you>\.gemini\settings.json.bak-mcp-local-hub-20260429-004626
  holds "time" already in hub-managed form, and
  -original sentinel fallback failed: clients: backup copy of entry is
  already in hub-managed shape
```

## Root cause

`Demigrate` in `internal/api/demigrate.go` relies on two backup sources to
reconstruct an entry's pre-hub (stdio command/args/env) form:

1. Latest timestamped backup: `<config>.bak-mcp-local-hub-<TS>`
2. Pristine sentinel: `<config>.bak-mcp-local-hub-original`

The sentinel is meant to be a one-shot pre-hub snapshot:

> The pristine `-original` sentinel (the one-shot pre-hub snapshot
> Client.Backup() writes on first call; never overwritten).

In `internal/clients/clients.go:writeBackup` (around line 285-301), the
sentinel is only created when missing. This is correct **only if the
first ever `Backup()` call happens BEFORE the first `AddEntry()` writes
hub-managed shape to livePath.**

In April 2026 the project codename was renamed `mcp-sync → mcp-local-hub`.
Backup filename prefix changed accordingly. After the rename, the first
time the new code looked for `<config>.bak-mcp-local-hub-original`, it
was absent (only `<config>.bak-...-mcp-sync` existed). `writeBackup`
created the new sentinel from the **current livePath**, which by then
already contained hub-managed entries from the previous codename's
install/migrate operations. The new sentinel sealed that state.

User's gemini case:

- `~/.gemini/settings.json.bak-2026-04-15-mcp-sync` — 1106 bytes, only
  `playwright`, NO `time` entry. True pre-hub state.
- `~/.gemini/settings.json.bak-mcp-local-hub-original` — 2671 bytes,
  created 2026-04-18 03:06:45, contains `time` in hub-managed form
  (`{type:http, url:http://localhost:9128/mcp}`). Sealed permanently.
- `~/.gemini/settings.json.bak-mcp-local-hub-20260418-193121`,
  `-20260418-232719`, `-20260429-004626` — all timestamped backups
  contain hub-managed `time`. None pre-hub.

`ErrBackupEntryAlreadyMigrated` defensive check in
`internal/clients/{claude_code,codex_cli,json_mcp,vscode}.go` correctly
refuses to write hub-managed data through `RestoreEntryFromBackup`. So
demigrate ends in the "latest+sentinel both refuse" error path in
`demigrate.go:155-159`.

## Impact

Affects v0.3.0+ users who installed before April 2026 codename rename
AND have entries that were never in pre-hub stdio form. They cannot
demigrate those entries through the UI matrix. **No data loss** — UI
correctly surfaces a Failed row (B1 fix), disk untouched, retry state
visible (A4 fix). User must manually edit the client config file to
remove the entry.

## Empirical reproduction (2026-05-15 smoke session)

Tested via Playwright UI smoke under PR #185 r2. Three demigrate
attempts on different clients, all produced **identical error class**:

1. `time/gemini-cli` — sentinel `bak-mcp-local-hub-original` from
   2026-04-18 03:06:45 contains hub-managed time. Real pre-hub state
   exists in pre-rename legacy `settings.json.bak-2026-04-15-mcp-sync`
   (only `playwright`, no `time`) but current code never consults
   non-`mcp-local-hub-` prefix backups.
2. `time/claude-code` — sentinel same shape. Plus latest timestamped
   backup `bak-mcp-local-hub-20260515-023124` (created today by an
   earlier Apply when time was already migrated) ALSO hub-managed.
   `bak-mcp-local-hub-20260515-022829` (size 22776, created BEFORE
   today's migrate — does NOT contain time) is older lexicographically
   and `LatestBackupPath` ignores it.
3. `time/codex-cli` — sentinel `bak-mcp-local-hub-original` from
   2026-04-18 02:25 contains `[mcp_servers.time] url='localhost:9128'`.
   Same gap.

**Scope is wider than initially documented**: the bug appears even on
fresh installs after ≥1 migrate→demigrate→migrate cycle, because
`LatestBackupPath` returns the lexicographically latest timestamped
backup — which may have been written when the entry was already
hub-managed.

## Design (simplified per 2026-05-15 user input)

User constraint: "должно работать всегда" — demigrate MUST succeed when
the server is in a mcphub manifest. No new UX friction. UI should
report uniform success on Apply.

### Mandatory: Always-succeed fallback

Add an `RemoveEntry`-fallback path to `Demigrate`. After both
`LatestBackupPath` and sentinel return `ErrBackupEntryAlreadyMigrated`
(and any legacy-prefix fallback also fails to produce pre-hub form),
call `adapter.RemoveEntry(server)`. This succeeds because:

- `RemoveEntry` is a simple key-deletion on the live config; not
  dependent on backup state
- Semantically: the user is rolling back hub-routing for this entry.
  If no pre-hub form was ever captured, the rollback target IS "no
  entry" (since the entry was first written by mcphub itself).
- Report `Restored: true` so UI shows success — operator sees the cell
  in the matrix flip to ☐ as expected.

This is the minimum fix. Three lines of code in `demigrate.go`, no UI
changes, no new types.

### Quality: Iterate timestamped backups newest-first

Change `latestBackup` to skip backups whose entry is hub-managed,
returning the newest timestamped backup where the named entry is
absent OR in pre-hub shape. Handles the "user did multiple migrate
cycles" case where the lexicographic latest is post-migrate but an
older timestamped backup IS pre-migrate.

Requires a per-adapter predicate: `BackupEntryIsHubManaged(path, name)
(bool, error)`. Each adapter knows its own hub-shape detection (claude/
cursor/vscode use json `url` + no `command`, codex uses TOML
`[mcp_servers.X] url=...`).

### Quality: Legacy-codename prefix fallback

When all `bak-mcp-local-hub-*` backups (timestamped + sentinel) hold
hub-managed forms of the entry, try legacy backup prefixes in order:

1. `bak-mcp-sync-*` — pre-rename codename
2. `bak-YYYYMMDD-HHMMSS` — plain timestamped no-codename
3. `bak-phase2-*` — phase 2 install artifact
4. `bak-YYYY-MM-DD_HH-MM-SS` — older underscore-date format

Recovers true pre-hub state for users who upgraded across the April
2026 codename rename.

### UI

No changes to matrix UI. No new CTA. No new modal. Failed-row state
disappears after Apply succeeds (via either restoration OR
RemoveEntry-fallback path).

### Test coverage

- `internal/api/demigrate_test.go` — new test:
  `TestDemigrate_FallbackToRemoveEntry_WhenNoPreHubFormFound`. Synthesize
  fixture where sentinel + all timestamped backups hold hub-managed
  entry; assert RemoveEntry called, Restored: true reported.
- Per-adapter: `internal/clients/{gemini_cli,claude_code,codex_cli,
  cursor,vscode,antigravity}_test.go` — verify
  `BackupEntryIsHubManaged` returns correct bool for each shape.
- `internal/clients/clients_test.go` — `TestLatestBackup_SkipsHubManaged
  EntriesAndPicksOldestPreHubForm` and `TestLatestBackup_FallsBackToLegacy
  Prefixes`.
- `internal/gui/e2e/tests/servers.spec.ts` — extend the demigrate test
  to seed a tmpHome with hub-managed sentinel, uncheck, Apply, assert
  cell flips to ☐ AND `client-config` no longer has the entry.

## Test surface

- Unit: `internal/api/demigrate_test.go` — synthesize a fixture where
  both sentinel and latest backup contain hub-managed time entry,
  assert new outcome.
- Per-adapter unit: every adapter that implements
  `RestoreEntryFromBackup` must distinguish "hub-managed shape" from
  "absent" so Demigrate can route to the new path.
- E2E: `internal/gui/e2e/tests/servers.spec.ts` — seed a tmpHome with
  hub-managed sentinel, uncheck cell, Apply, assert "Remove entry" CTA
  appears.

## Workaround (current)

User edits the client config manually:

```bash
vim ~/.gemini/settings.json    # delete the mcpServers."<name>" key
```

No backup needed — `<config>.bak-mcp-local-hub-*` already exists.
Restart of the daemon scheduled task NOT required (gemini reads
settings on next CLI invocation).

## Tracking

- v0.4.1 backlog item B4 (or D-prefix if reorganized)
- Depends on: nothing (independent of PR #185 r2)
- Blocks: full operator demigrate experience for early adopters

## Failed attempt: PR #218 (reverted by PR #219)

2026-05-19: extended the marker+backfill+RemoveEntry helper to
Path B (latest hub-managed + sentinel missing entry) and Path C
(sentinel-only + sentinel missing entry). The helper unconditionally
called `adapter.RemoveEntry` once positive ownership evidence was
found via the marker or strict-URL backfill.

**Destructive on live host.** When the entry was originally in
direct/stdio form (user-installed), the marker check would pass
(because the entry IS mcphub-managed NOW, after migrate), but the
rollback target SHOULD have been the pre-migrate direct form, not
deletion. The user's gemini settings.json lost 7 entries
(godbolt, memory, paper-search-mcp, sequential-thinking, serena,
time, wolfram) when the smoke test's Phase 4 demigrate-rollback
ran on cells that started life as direct.

The marker check is necessary but not sufficient — it does not
distinguish "mcphub installed this from scratch" from "mcphub
migrated an existing entry."

## Proper-fix design (deferred)

§"Quality: Iterate timestamped backups newest-first" remains the
correct path. Implementation requirements (consolidated from
PR-218 lessons):

1. New adapter method `BackupEntryIsHubManaged(path, name)
   (bool, error)` for each of the 7 client adapters. Each adapter
   knows its own hub-shape detection.
2. Replace the single `LatestBackupPath()` lookup with an
   iteration helper `IterateBackupsNewestFirst()` that visits
   every timestamped backup in `name.bak-mcp-local-hub-*` order,
   newest first.
3. For each backup, check `BackupContainsEntry`. If contains
   AND NOT `BackupEntryIsHubManaged`, that's the pre-hub form —
   restore from THAT backup.
4. Only if NO backup contains a pre-hub form, AND the marker
   confirms mcphub-managed (or backfill matches strict
   URL+port+path), fall back to `RemoveEntry`. This is the
   "entry never existed pre-hub" case.

   **Explicit policy on sentinel-only + backfill** (per PR #220
   r2 security review): backfill match — live URL exactly equals
   `http://localhost:<daemon.port><url_path>` — IS accepted as
   ownership evidence in this fallback even when the only
   available backup is the empty sentinel (i.e., timestamped
   backups were pruned or never created). Rationale: the URL
   coincidence is vanishingly unlikely for genuine
   user-configured remote MCP servers (they almost always run
   on a non-mcphub port + path), structurally indistinguishable
   from a mcphub install otherwise, and the user constraint
   "должно работать всегда" prioritizes operator unblocking
   over hypothetical user-coincidence preservation. Strict
   mode (MCPHUB_REQUIRE_SINGLE_USER_HOME=1) is unaffected.
   The pre-PR-218 test that asserted fail-closed for this
   sub-case (`TestDemigrate_FailsWhenOnlySentinelExistsAndLacksEntry`)
   was over-conservative and is updated under PR #220 to
   assert the new behavior, with a complementary fail-closed
   test covering the URL-mismatch case.
5. Test coverage: per-adapter unit tests for
   `BackupEntryIsHubManaged` returning the right bool for each
   shape (json url-shape, codex toml url-shape, antigravity
   relay shape).

Net effect: the 5 originally-failing cells (perftools/{claude,
codex,gemini}, lldb/codex, time/cursor) would still demigrate
successfully (no pre-hub form ever existed → RemoveEntry path
fires under marker confirmation). User-direct entries would
restore from the appropriate older backup instead of being
deleted.

## Status

**CLOSED — option (a) accepted 2026-06-16 (operator: "твое рекомендованное").**
The decision below was resolved in favour of **(a): accept the residual
fail-closed case as a safe tradeoff and close.** Demigrate succeeds whenever a
pre-hub form exists OR mcphub ownership is provable; it fails closed ONLY for the
vanishingly-rare pre-marker + all-backups-pruned case, where deleting without
ownership proof is exactly the data-loss that PR #218 caused (7 user-direct
entries deleted, reverted by PR #219). That residual has a manual workaround
(edit the client config). No force-remove UI (option b) is added — it would put a
destructive confirm affordance in the matrix for an edge case, and the safety of
fail-closed outweighs the always-succeed mandate here. The code is unchanged
(PR #257 `f144dea` already shipped the three-tier restore-or-prove-then-remove
logic); this is a status/decision close only.

--- historical (pre-decision) ---

**OPEN (partially fixed)** — the originally-reported case is fixed, but the
broad "должно работать всегда" mandate is NOT fully met.

PR #257 (`f144dea`, "fix(demigrate): legacy-codename backup fallback —
cross-rename entries restore, never delete") fixed the **originally-reported
failure**: the April-2026 codename-rename case where the only pre-hub form
lived in a legacy `bak-mcp-sync-*` backup. What landed in
`internal/api/demigrate.go`:

1. Current-codename backup iteration (`demigrate.go:172-188`) walks every
   `bak-mcp-local-hub-*` backup, skipping `ErrBackupEntryAlreadyMigrated` /
   `errBackupMissingEntry` candidates.
2. `tryLegacyPrefixRestore` (`demigrate.go:202`, defined `demigrate.go:369`
   — the new #257 piece) consults the legacy pre-rename `mcp-sync` / phase2
   / plain-date backups when no current-codename backup holds a pre-hub
   form. Restores the originally-reported gemini case (`time` pre-hub state
   lived only in `settings.json.bak-2026-04-15-mcp-sync`) instead of deleting.
3. `tryMarkerOrBackfillRemove` last-resort (`demigrate.go:219`, defined
   `demigrate.go:282`) fires ONLY when neither current- nor legacy-codename
   backups hold a pre-hub form AND positive ownership evidence (marker, or
   URL-backfill corroborated by ≥1 backup) proves mcphub installed the
   entry. The `restoredFrom == ""` ordering guard (lines 201/218) makes
   restore strictly precede deletion, preserving the PR #218 anti-deletion
   invariant. Supporting: per-adapter `BackupEntryIsHubManaged`
   (`internal/clients/clients.go:212`) + `LegacyBackupsNewestFirst`
   (`clients.go:968`).

**Residual (why this stays OPEN — codex bot flagged it on PR #259):** the
broad "должно работать всегда" mandate (§Design "Mandatory: Always-succeed
fallback") is NOT fully satisfied. `tryMarkerOrBackfillRemove` deliberately
**fails closed** for a *pre-marker hub-managed entry whose backups were all
pruned*: no marker (pre-marker), no backup to corroborate the URL-backfill
(`allowURLBackfill = len(backups)>0 || sawLegacy` — the safety gate added in
PR #257 r3 to stop the data-loss the bot caught there), and no pre-hub backup
to restore → demigrate Apply fails for that entry. This is the **safe**
choice (deleting without ownership proof is exactly the reverted-by-#219
PR #218 regression that deleted 7 user-direct entries), but it IS an
operator-visible failure case the original mandate wanted eliminated.

**Decision needed** (the mandate was the user's "должно работать всегда"):
either **(a)** ACCEPT the residual as a safe tradeoff and close — demigrate
succeeds whenever a pre-hub form exists or ownership is provable, failing
closed only for the vanishingly-rare pre-marker + all-backups-pruned case;
or **(b)** add an operator-confirmed force-remove path (an explicit "yes,
delete this entry" UI affordance) for that case so demigrate can always
succeed without silently risking user-direct deletion.

Verified 2026-06-02: `f144dea` ancestor of HEAD; the three-tier ordering +
`restoredFrom == ""` guard + `tryLegacyPrefixRestore` +
`BackupEntryIsHubManaged` + `LegacyBackupsNewestFirst` all present; the
residual fail-closed confirmed at the `allowURLBackfill` gate. An earlier
"CLOSED — mandate satisfied" status on this doc was inaccurate and is
corrected here.

The "Failed attempt: PR #218 (reverted by PR #219)" and "Proper-fix design
(deferred)" sections above are retained as execution history.

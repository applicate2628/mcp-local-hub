# Demigrate fallback when no pre-hub form exists

**Severity:** P2 (operator gap, no data loss; affects v0.3.0+ installs that
upgraded from `mcp-sync` codename in April 2026)

**Found:** 2026-05-15 by user during PR #185 r2 manual UI smoke. User
attempted to uncheck `time/gemini-cli` from the matrix and Apply. Demigrate
failed with:

```text
time/demigrate/gemini-cli: latest backup
  C:\Users\dima_\.gemini\settings.json.bak-mcp-local-hub-20260429-004626
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

# B1 residual race: backup + live-client-config I/O in internal/clients lacks serialization

**Status:** FIXED (2026-06-15, commits #319 `config_lock.go` + `2c4ba62`) — verified 2026-06-16
**Found during:** B1 design-memo rev 2 / R1 P2 review of 2026-04-24
**Phase:** post-B1

## RESOLUTION (fix candidate 3 — both in-process mutex + flock)

`internal/clients/config_lock.go` `lockingClient` decorates every client so:
- **Write/RMW paths** — `InitEmpty`, `Backup`, `BackupKeep`, `Restore`,
  `AddEntry`, `RemoveEntry`, `RestoreEntryFromBackup`,
  `RestoreEntryFromBackupForRollback` run under `withConfigLock` (per-config-path
  in-process mutex + cross-process advisory flock). [#319]
- **Backup-READ selection paths** — `LatestBackupPath`, `BackupContainsEntry`,
  `BackupEntryIsHubManaged` run under `withConfigReadLock` (same key, tolerates a
  missing backup dir). [`2c4ba62`]
- `AllClients()` returns `*lockingClient` for every client (the concrete adapter's
  internal cross-method calls bypass the decorator, so no self-deadlock).

Covers BOTH exposure classes from this doc: multi-tab same-process (in-process
mutex) AND CLI+GUI interleave across processes (flock).

**Named tests (all PASS under `-race`, verified 2026-06-16):**
- `TestConfigLock_ConcurrentAddRemoveBackup_NeverTearsFile` — the two-goroutine
  unit test this doc asked for.
- `TestConfigLock_FlockSerializesAcrossProcesses` — the cross-process integration
  test this doc asked for.
- `TestConfigLock_BackupReadDuringWrite_NeverSelectsTornFile` — read-race guard.
- `TestAllClientsAreLockWrapped` — regression guard (no adapter bypasses the lock).

---

### Original report (historical)

## Summary

`internal/clients/clients.go` backup-write (`writeBackup`), backup-read
(`latestBackup`), restore, and live client-config read-modify-write paths
all use unguarded `os.ReadFile` / `os.WriteFile` with no `sync.Mutex`,
no `flock`, and no process-level lock.

Concurrent mutations on the same client config can therefore produce:

- **Silent lost updates** — writer B clobbers writer A's pending rotation
  before A's read-modify-write completes.
- **Interleaved backup files** — one writer's JSON overwrites another's
  in the same timestamp window.
- **Demigrate reading a polluted backup** — see B1 design memo §5 R1 for
  the specific multi-tab / CLI+GUI race pattern.

`latestBackup` does NOT validate JSON — it picks a path based on
lexicographic sort of timestamp filenames. So failures are not always
visible parse errors; they can be silent lost updates that surface much
later as stale state in a client config or a successful-but-wrong
restore.

## Exposure at B1 ship time

- **Single GUI tab**: the frontend's sequential per-change apply loop
  keeps requests serialized from the browser side. Within one Apply
  invocation from one tab, no two requests to the same client overlap.
- **Single GUI process**: the `<pidport>.lock` single-instance mutex
  prevents a second `mcphub gui` process from running concurrently.
- **Multiple GUI tabs**: two tabs of the same GUI process CAN issue
  interleaved `POST /api/migrate` and `POST /api/demigrate` to the
  same Go process. The HTTP mux does not serialize same-client
  requests. **This exists pre-B1** for two tabs clicking the Migration
  screen's Demigrate button simultaneously; B1 widens the exposure
  (matrix Apply on one tab + Migration Demigrate on another) but
  introduces no NEW primitive.
- **CLI + GUI interleaving**: `mcphub migrate ...` CLI running while
  the user clicks Apply on the GUI matrix — same risk class.

## Fix candidates (for a future plan)

1. **In-process mutex keyed by client name** (`map[string]*sync.Mutex`
   wrapping backup + live config I/O in `internal/clients`). Covers
   multi-tab case. Does NOT cover CLI-plus-GUI unless the CLI also goes
   through the same in-process state, which it does not in a separate
   process.
2. **OS-level advisory file-lock** on each client config file (`flock`
   on Linux, `LockFileEx` on Windows). Covers CLI+GUI interleave.
   Requires careful platform handling (Windows Git Bash vs PowerShell
   vs cmd quirks).
3. **Both** — in-process mutex as fast-path within one process, file-lock
   as coarse inter-process guard.

## Tests to add when fix lands

- Unit: two goroutines calling `writeBackup` + `latestBackup` for the
  same client in tight loop; assert no lost updates.
- Integration: spawn a second `mcphub` subprocess that calls `mcphub
  migrate` while the GUI tab issues `POST /api/migrate` + `POST
  /api/demigrate` — assert final backup state matches one of the two
  serializations (never a torn JSON blob).

## Related

- Design memo: `docs/superpowers/specs/2026-04-24-phase-3b-ii-b1-servers-matrix-demigrate-design.md`
  §5 R1 (5 Codex revisions, 1 P1 order-of-operations + 1 P1 gate + 1 P1
  retry-prune + 1 P1 always-reload all in this race's orbit)
- A2b backlog follow-ups: `work-items/bugs/a2b-combined-pr-followups.md`
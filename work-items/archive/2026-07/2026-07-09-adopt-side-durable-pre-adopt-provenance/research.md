# Research — adopt-side durable pre-adopt provenance (prereq for de-adopt)

Role: $analyst (read-only). Repo `mcp-local-hub` (repo root), branch `master`.
All citations are on-disk `file:line` as of 2026-07-10.

## Executive fact

De-adopt is **not greenfield in design** but **is greenfield in code**. A design
+ adversarial review already exist under
`work-items/active/2026-07-09-deadopt-hub-to-native/` (`design.md`, `review.md`,
`status.md`); the review verdict is **REVISE / BLOCKED** precisely on this
"adopt-side durable pre-adopt provenance" prereq. No Go de-adopt symbol exists
yet (`de-adopt|deadopt|unadopt` matches only work-items docs). This memo confirms
the exact provenance gap that blocks it.

## Q1 — Adopt flow today (path to the config rewrite)

Entry points funnel into `internal/api/adopt.go`:
- **CLI** — `internal/cli/adopt.go:13-57` (`newAdoptCmdReal`). Builds plan via
  `BuildAdoptPlan` (`:34`), dry-run unless `--yes`, then `ExecuteAdopt` (`:48`).
- **GUI** — routes `internal/gui/server.go:830-839`; handler `adoptHandler`
  `internal/gui/adopt.go:73-123` (POST `/api/adopt`) → `ExecuteAdoptWithOpts`
  (`internal/gui/adopt.go:96`) → `operator-action` audit row (`:112-116`).
- **Frontend** — Discovery/Migration screen (`Migration.tsx`, `postAdopt` in
  `api.ts`).

Backend core:
- `BuildAdoptPlan` — `internal/api/adopt.go:126-202`. Reads source entry
  (`extractStdioEntryFromClient`, `:155`), rewrites sensitive env
  (`rewriteAdoptSensitiveEnv`, `:173`), renders the stdio-bridge manifest,
  returns `AdoptPlan` (`:45-58`). Mutates **no** disk state.
- `ExecuteAdoptWithOpts` — `internal/api/adopt.go:211-253`. Ordered mutations:
  `persistAdoptRoutedSecrets` (`:218`) → `ManifestCreate` (`:221`) →
  **`Install(...)`** (`:230-235`).

**The client-config rewrite is inside `Install`, not `MigrateFrom`.** `Install`
(`internal/api/install.go:234-284`) → `installPlanCoreWithSymlinkConsents` →
`executeInstallToWithSymlinkConsents` (`:2480`). Per-client block `:2632-2710`;
the overwrite is `clients.AddEntryWithConfigWriter(client, entry, configWriter)`
at **`internal/api/install.go:2689`**. Each adapter's `AddEntry` wholesale-
replaces the same-name entry (aider `internal/clients/aider.go:199-246`,
openhands `internal/clients/openhands.go:199-243`).

## Q2 — What is overwritten, what is lost at adopt

**Overwritten:** source same-name entry replaced by a hub entry built at
`install.go:2679-2687`: `MCPEntry{Name, URL: http://127.0.0.1:<port>/mcp,
Headers, RelayServer, RelayDaemon, RelayExePath, RelayURL}`. HTTP-native adapters
write `{name,url}`; relay-stdio adapters write `{name, command: mcphub.exe,
args:[relay,…]}`. Original `command`/`args`/`env`/`disabled` are **dropped** by
the same-name replace.

**Original entry read but not persisted as provenance.** `BuildAdoptPlan`
extracts via `extractStdioEntryFromClient` (`internal/api/scan.go:2303`) into
`extractedStdioEntry{Command,Args,Env,Disabled}` (`scan.go:2278-2283`) — in-memory
only, and **lossy for reconstruction**: sensitive env values rewritten by
`rewriteAdoptSensitiveEnv` (`adopt.go:173`) into `secret:<KEY>` refs, so original
literal-vs-`secret:`-vs-`${env:NAME}` spelling is gone
(`internal/api/adopt_secret_route.go:17-52`).

**Durable preservation exists only via the generic install backup lane — not
adopt-scoped, not pinned.** Before `AddEntry`, the Install block calls
`client.BackupKeep(backupKeepN)` at `install.go:2670`. `BackupKeep`→`writeBackup`
(`internal/clients/clients.go:1021-1051`) writes: a one-shot pristine `-original`
sentinel (`:1032-1037`, written only if missing) + a timestamped full-file copy
`.bak-mcp-local-hub-<ts>` (`:1039-1045`). So the pre-adopt entry survives inside a
**whole-config-file** backup — but **no linkage from adopt to a specific backup
path**, and the timestamped copy is **prunable** (`pruneOldTimestamped`,
`clients.go:1145-1191`; default `keep_n` 5). This is the review's "Backup
retention" blocking gap (`review.md:59-67`).

Two more confirmed gaps at the overwrite seam:
- **`GetEntry` called but discarded** at `install.go:2657` — only a health gate;
  comment states it "can be lossy for direct stdio entries" → not provenance.
- **Adopt's Install path does NOT record `managed-entries.json`.**
  `RecordManagedEntry` callers are `migrate.go:298`, `managed_entries.go:321`
  (backfill), `serena_client_reconcile.go:396` — **not** `install.go`. Adopted
  entry has no ownership marker (design.md:39).

Net: at `AddEntry` (`install.go:2689`) the only durable trace of the pre-adopt
entry is an unlabelled, prunable, whole-file backup that adopt neither pins nor
records. No adopt-scoped per-entry non-prunable snapshot, no "absent" record for
entryless-fanout clients.

## Q3 — Existing provenance mechanisms, ranked by reuse fit

1. **Client-config backup lane** (`BackupKeep`/`writeBackup`/`-original` +
   `RestoreEntryFromBackupForRollback`). Closest to a restore artifact. Captures
   pre-overwrite whole-file state (`install.go:2670`); restore helpers put back
   one named entry or remove if absent (`clients.go:353-362`; body e.g.
   `aider.go:328-412`). **Gaps:** (a) timestamped backup prunable
   (`clients.go:1145-1191`); (b) `-original` sentinel is a single one-shot whole-
   file pre-hub snapshot — if a prior migrate/install already wrote it, it
   predates the adopted entry (not adopt-scoped); (c) no adopt→backup-path pin →
   de-adopt would fall back to "newest backup" heuristic that demigrate already
   treats as unsafe (`demigrate.go:158-307`); (d) no entryless-fanout "absent"
   representation.
2. **`managed-entries.json`** (`internal/api/managed_entries.go`). Right storage
   pattern, insufficient content. Stores only `(client,server,installed_at)`
   (`:120-130`); `IsManagedEntry` (`:249-266`), `ForgetManagedEntry` (`:213-232`);
   persisted via `writeHubMcpStateFile`/`readHubMcpStateFile` + flock + schema
   version (`:99-115,136-167`). **Gaps:** adopt doesn't write it; zero
   reconstruction data. Correct **storage template** for a new adopt provenance
   file, not a sufficient store.
3. **`AdoptPlan.secretValues` + `plan.SecretRoutedKeys`** (`adopt.go:45-58`,
   persisted via `persistAdoptRoutedSecrets` `:218`). In-memory only; mapping of
   which routed keys adopt created for which manifest not durably recorded outside
   manifest env refs — de-adopt needs it to clean up (review F2, `review.md:30-38`).
4. **`_preservedRaw`** (GUI edit-server, `types.ts`). Frontend-only round-trip for
   unknown manifest top-level fields; not a durable server-side per-entry store.
   **Not reusable.**
5. **`dismissed.json`** — dismissed scan rows only; no entry content. **Not
   reusable.**

**Conclusion:** no existing store captures enough to reconstruct the pre-adopt
entry durably + adopt-scoped. Reusable pieces = the **storage pattern** of
`managed_entries.go` (hardened state-file + flock + schema version) + the
**restore mechanics** of the backup lane — but a **new adopt-owned durable record
with a pinned/non-prunable artifact** is required.

## Q4 — De-adopt consumer (what it expects)

Greenfield in code; consumer contract already written as a design
(`work-items/active/2026-07-09-deadopt-hub-to-native/design.md`): proposes
`api.BuildDeAdoptPlan` / `api.ExecuteDeAdoptWithOpts`, CLI `mcphub
de-adopt`/`deadopt`, POST `/api/deadopt[/plan]`; fails closed on missing
provenance (`design.md:151`).

Provenance shape the consumer expects (`design.md:69-77`; `review.md:86`, one row
per adopt-created manifest):
- `manifest_name`, source entry name, source client, selected clients, selected port;
- adopt-generated manifest hash **and** current expected hash (hash-gated manifest
  edit/delete, review F1);
- **per-client original state**: `present` with a **pinned backup ref or
  serialized adapter snapshot**, or **`absent`** for explicit fanout clients with
  no pre-adopt entry;
- per-client original config-shape hash + expected hub-managed live shape;
- **adopt-created routed secret keys** (review F2 — delete keys *before*
  forgetting provenance, or a `cleanup_pending` state);
- **operation state**: `adopting` → `adopted` → `de_adopting` → `closed`, pending
  row written **before the first irreversible adopt mutation**, flipped to
  `adopted` only after install succeeds (`design.md:77`);
- restore artifact must be **non-prunable / genuinely pinned** — "an ordinary
  timestamped backup path is insufficient" (`review.md:87`, prereq 2).

Closest existing reverse op is **`demigrate`** (`demigrate.go:71-307`) but
insufficient: restores/removes a client entry from backups+marker yet **does not
delete the adopt manifest, release supervisor intent, or clean routed secrets**
(`design.md:11,101`). Dependency recorded: de-adopt `status.md:5` declares
`Depends-on: 2026-07-09-adopt-side-durable-pre-adopt-provenance`.

## Q5 — Recommended owner + seam (identification only)

**Owner:** adopt API pipeline `internal/api/adopt.go` (design names `internal/api`
primary, `design.md:53,194`). Durable store = a **new adopt-owned state file**
(design proposes `<state-dir>/adopted-entries.json`, `design.md:68`) on the
existing hardened pattern — `WriteStateFileAtomic`
(`internal/api/state_file_helper.go:72`) / the `writeHubMcpStateFile`+flock+schema
model of `managed_entries.go:99-167`. Note `state_file_helper.go:68-71`: cross-file
consistency must serialize at a higher level — relevant because provenance write +
manifest create + install + secret persist span multiple files, so the adopt owner
must sequence them.

**Seam (fail-closed ordering — capture provenance, then rewrite):** inside
`ExecuteAdoptWithOpts` (`adopt.go:211-253`), first irreversible mutations are
`persistAdoptRoutedSecrets` (`:218`), `ManifestCreate` (`:221`), then `Install`
(`:230`) whose per-client `AddEntry` overwrite is `install.go:2689`. Provenance
capture (per-client raw entry read + present/absent classification + a pinned copy
of each selected client's pre-adopt config) must complete, and a
`pending`/`adopting` row durably written, **before `adopt.go:218`** — then flipped
to `adopted` only after `Install` returns success, folded into the existing adopt
failure-cleanup at `adopt.go:218-248`.

The per-client pre-overwrite backup already physically exists at `install.go:2670`
(immediately before `AddEntry` at `:2689`); the missing durability link is that
this backup path is neither **returned to the adopt owner** nor **pinned against
pruning**. Two candidate seam placements (identification only, not a design
choice): (a) capture in the adopt owner *before* `Install` and pin a copy outside
the prunable set; (b) thread a capture hook through the Install client block so the
`BackupKeep` path is pinned into the adopt provenance row. Option (a) keeps capture
in the adopt owner and does not perturb Install's shared rollback contract
(`install.go:2702-2708`).

## Research admission gates
1. **Regression-risk — PASS (named risk).** Provenance capture is additive (new
   pre-`Install` write step + pinned artifact). Risk: a capture-step failure that
   aborts adopt could regress currently-successful adopts → must have own
   fail-closed handling folded into `adopt.go:218-248`. Not a specialist lane;
   this is the main de-adopt prereq.
2. **Metric-alignment — PASS.** Objective (durable reconstruction of pre-adopt
   entry across a process boundary) matches de-adopt review T1 (adopt → simulate
   reload → de-adopt using only persisted provenance, `review.md:103-104`).
3. **Known-limits — flagged, not blocking.** (a) Byte-equivalent per-adapter
   restore is ASSUMPTION (UNVERIFIED) — design flags it (`design.md:79`);
   interface support exists (`clients.go:353-362`) but byte-equivalence unproven
   per adapter. (b) Secret literal spelling unrecoverable after `secret:` routing.
   (c) Generic backup lane prunable, sentinel not adopt-scoped. Schema must design
   around these (pinned artifact + `functional-equivalent` restore mode).
4. **Bounded-falsification — PASS.** Experiment specified: seed one stdio entry,
   adopt, drop in-memory state (new process/reload), de-adopt using only the
   persisted provenance file, assert client entry equals pre-adopt snapshot
   (review T1). Add T5 (low `keep_n`, churn backups past retention, then restore)
   to falsify the prunability limit.

## Unresolved (for the downstream architect)
- New `adopted-entries.json` vs schema-compatible extension of
  `managed-entries.json` — needs an accepted decision-registry entry
  (`review.md:77-82`; `design.md:196`).
- Whether adopt should also record `managed-entries.json` tuples (`design.md:203`
  recommends yes; de-adopt still relies on the stronger adopt provenance for
  restoration).
- Per-adapter byte-equivalence verification (ASSUMPTION at `design.md:79`) — needs
  an empirical per-adapter probe before user-facing byte-equivalence promises.

## Gate decision
**PASS.** Evidence-backed, internally consistent, no code changes / design
proposals beyond owner+seam identification. Next role (architect designing the
provenance schema) has: the exact overwrite seam (`install.go:2689`), the exact
fail-closed insertion point (`adopt.go:211-253`, before line 218), the reuse
template (`managed_entries.go` storage + backup-lane restore mechanics), and the
already-written consumer contract (`deadopt design.md:69-77`, `review.md:86-91`).
